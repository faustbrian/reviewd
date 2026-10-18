//go:build darwin || linux

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/faustbrian/reviewd/internal/lease"
)

var ErrFeedbackWorkspaceRetained = errors.New("unfinished feedback workspace retained; explicitly resume its existing run")

// A workspace is retained by the existing private owner, not a second lease,
// archive or replay authority. Older clients reject this unknown resource kind
// and conservatively preserve it rather than deleting pending edits.
type ownedFeedbackWorkspace struct {
	Kind    string `json:"kind"`
	OwnerID string `json:"owner_id"`
	RunID   string `json:"run_id"`
}

// ResumeFeedbackOwner reopens the original retained owner only. It does not
// start an agent or replay an effect, and the caller must hold the existing
// cross-profile PR modifier and separately claim the observed run by CAS.
func (s *Store) ResumeFeedbackOwner(ctx context.Context, observed Run) (_ *PrivateOwner, err error) {
	if observed.Purpose != "feedback" || observed.OwnerClosed || (observed.State != FeedbackPending && observed.State != Partial) || !ownerIDPattern.MatchString(observed.OwnerID) {
		return nil, errors.New("explicit resume requires its original retained feedback owner")
	}
	root := filepath.Join(s.directory, "private-work", observed.OwnerID)
	info, err := privateDirectory(root, false)
	if err != nil {
		return nil, err
	}
	lockInfo, err := os.Lstat(filepath.Join(root, "owner.lock"))
	if err != nil || !lockInfo.Mode().IsRegular() {
		return nil, errors.New("retained owner lease identity is unavailable")
	}
	lock, err := lease.Acquire(filepath.Join(root, "owner.lock"), true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	var raw, expectedRoot, expectedLease string
	var closing bool
	if err = s.db.QueryRowContext(ctx, "SELECT processes,closing,root_identity,lease_identity FROM private_owners WHERE id=?", observed.OwnerID).Scan(&raw, &closing, &expectedRoot, &expectedLease); err != nil {
		return nil, err
	}
	currentInfo, err := privateDirectory(root, false)
	if err != nil || !os.SameFile(info, currentInfo) {
		return nil, errors.New("retained feedback storage identity changed")
	}
	rootID, err := rootIdentity(currentInfo)
	if err != nil || rootID != expectedRoot {
		return nil, errors.New("retained feedback storage identity is unproven")
	}
	opened, err := lock.Info()
	if err != nil || !os.SameFile(lockInfo, opened) {
		return nil, errors.New("retained feedback lease was replaced")
	}
	leaseID, err := rootIdentity(opened)
	if err != nil || leaseID != expectedLease || closing {
		return nil, errors.New("retained feedback owner cannot be reopened")
	}
	resources, err := decodeProcesses(raw, observed.OwnerID)
	if err != nil {
		return nil, err
	}
	stopped, err := resourcesStopped(ctx, resources)
	if err != nil || !stopped {
		return nil, errors.New("retained feedback commands have not proven shutdown")
	}
	retained := false
	for _, workspace := range resources.workspaces {
		if workspace.RunID != observed.ID {
			return nil, errors.New("retained workspace belongs to another run")
		}
		retained = true
	}
	current, err := s.Get(ctx, observed.ID)
	if err != nil || !retained || current.Purpose != "feedback" || current.State != observed.State || current.OwnerID != observed.OwnerID || current.OwnerClosed || !bytes.Equal(current.Outcome, observed.Outcome) || current.ThreadID != observed.ThreadID || current.TurnID != observed.TurnID {
		return nil, errors.New("feedback run changed before explicit owner resume")
	}
	return &PrivateOwner{ID: observed.OwnerID, Root: root, store: s, lease: lock, info: currentInfo}, nil
}

func decodeFeedbackWorkspace(raw []byte, owner string) (ownedFeedbackWorkspace, error) {
	var value ownedFeedbackWorkspace
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Kind != "feedback_workspace" || value.OwnerID != owner || !ownerIDPattern.MatchString(value.RunID) {
		return value, ErrFeedbackWorkspaceRetained
	}
	return value, nil
}

// KeepFeedbackWorkspace must be recorded before checkout or any modifying
// command. Both successful shutdown and crash recovery preserve the same work.
func (o *PrivateOwner) KeepFeedbackWorkspace(ctx context.Context, runID string) error {
	if _, err := o.lease.Info(); err != nil {
		return errors.New("feedback workspace owner lease is closed")
	}
	if !ownerIDPattern.MatchString(runID) {
		return errors.New("invalid feedback run identity")
	}
	id, err := newID()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(ownedFeedbackWorkspace{Kind: "feedback_workspace", OwnerID: o.ID, RunID: runID})
	if err != nil {
		return err
	}
	result, err := o.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_set(processes,?,json(?))
 WHERE id=? AND closing=0 AND (SELECT count(*) FROM json_each(processes))<16
 AND NOT EXISTS(SELECT 1 FROM json_each(processes) WHERE json_extract(CASE WHEN json_valid(value) THEN value ELSE '{}' END,'$.kind')='feedback_workspace')
 AND EXISTS(SELECT 1 FROM runs WHERE id=? AND json_extract(record,'$.purpose')='feedback' AND json_extract(record,'$.owner_id')=? AND state='handling_feedback')`, processPath(id), string(raw), o.ID, runID, o.ID)
	return changed(result, err, "cannot retain the existing feedback workspace before execution")
}

// ReleaseFeedbackWorkspace is an application-only terminal-delivery boundary.
// It grants no command or remote effect. Unfinished operations, unknown effects
// and uncommitted work must retain it; an audited completed response may release
// execution while its separately recorded concern awaits human clarification.
func (o *PrivateOwner) ReleaseFeedbackWorkspace(ctx context.Context, runID string) error {
	if _, err := o.lease.Info(); err != nil {
		return errors.New("feedback workspace owner lease is closed")
	}
	var raw string
	if err := o.store.db.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=? AND closing=0", o.ID).Scan(&raw); err != nil {
		return err
	}
	resources, err := decodeProcesses(raw, o.ID)
	if err != nil {
		return err
	}
	for id, workspace := range resources.workspaces {
		if workspace.RunID != runID {
			return errors.New("feedback workspace belongs to another run")
		}
		result, err := o.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_remove(processes,?) WHERE id=? AND closing=0 AND processes=?`, processPath(id), o.ID, raw)
		return changed(result, err, "cannot release the exact delivered feedback workspace")
	}
	return errors.New("feedback workspace retention identity is unavailable")
}
