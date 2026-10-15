//go:build darwin || linux

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/faustbrian/reviewd/internal/lease"
)

const RecoveryBatchLimit = 25

type RecoveredOwner struct {
	ID     string   `json:"id"`
	State  string   `json:"state"`
	RunIDs []string `json:"run_ids"`
	// Display only when operator action is required, never credentials/answers.
	Storage string `json:"storage,omitempty"`
}
type RecoveryReport struct {
	DryRun      bool             `json:"dry_run"`
	Owners      []RecoveredOwner `json:"owners"`
	UnknownRuns []string         `json:"unknown_runs"`
	BatchFull   bool             `json:"batch_full"`
}

func (r RecoveryReport) NeedsAttention() bool {
	if len(r.UnknownRuns) > 0 {
		return true
	}
	for _, owner := range r.Owners {
		if owner.State == "resources_pending" || owner.State == "unknown" || owner.State == "recovery_failed" {
			return true
		}
	}
	return false
}

// ReconcilePrivateOwner uses the existing lease/process/closing-barrier owner,
// scoped to one durable identity. Cross-profile admission must not infer child
// death from its separate admission lock being free or from a stored PID.
func (s *Store) ReconcilePrivateOwner(ctx context.Context, id string) (RecoveredOwner, error) {
	unknown := RecoveredOwner{ID: id, State: "unknown", RunIDs: []string{}}
	if !ownerIDPattern.MatchString(id) {
		return unknown, errors.New("invalid private owner identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.recoverPrivateOwner(ctx, id, "", false)
	if err != nil || result.State != "already_recovered" {
		return result, err
	}
	// The row may be absent after another cleaner acknowledged shutdown.
	// Require its durable closure fact; absent metadata alone proves nothing.
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(json_extract(record,'$.owner_closed'),0) FROM runs WHERE json_extract(record,'$.owner_id')=?`, id)
	if err != nil {
		return unknown, err
	}
	closed := true
	for rows.Next() {
		var runID string
		var ownerClosed bool
		if err = rows.Scan(&runID, &ownerClosed); err != nil {
			break
		}
		unknown.RunIDs = append(unknown.RunIDs, runID)
		closed = closed && ownerClosed
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return unknown, err
	}
	if len(unknown.RunIDs) == 0 || !closed {
		return unknown, nil
	}
	if err = s.failLostOwner(ctx, id); err != nil {
		return unknown, err
	}
	unknown.State = "already_recovered"
	return unknown, nil
}

// RecoverPrivate never resumes a thread, starts a review or signals a saved
// PID/group. The owner lease proves death; group metadata can only prevent
// deletion. A reused group number therefore preserves, rather than harms, it.
func (s *Store) RecoverPrivate(ctx context.Context, dry bool) (report RecoveryReport, err error) {
	report = RecoveryReport{DryRun: dry, Owners: []RecoveredOwner{}, UnknownRuns: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id,processes FROM private_owners ORDER BY checked_at,created_at,id LIMIT ?`, RecoveryBatchLimit)
	if err != nil {
		return report, err
	}
	type candidate struct{ id, raw string }
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.raw); err != nil {
			break
		}
		candidates = append(candidates, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return report, err
	}
	report.BatchFull = len(candidates) == RecoveryBatchLimit
	var individualErrors error
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(individualErrors, err)
		}
		owner, recoverErr := s.recoverPrivateOwner(ctx, c.id, c.raw, dry)
		report.Owners = append(report.Owners, owner)
		if recoverErr != nil {
			individualErrors = errors.Join(individualErrors, recoverErr)
		}
		if !dry {
			if _, err := s.db.ExecContext(ctx, "UPDATE private_owners SET checked_at=? WHERE id=?", timestamp(), c.id); err != nil {
				individualErrors = errors.Join(individualErrors, err)
			}
		}
	}
	// Old records have no kernel lease to test. The existence of an inbox link,
	// old timestamp or a PID alone must not turn an unknown owner into dead work.
	rows, err = s.db.QueryContext(ctx, `SELECT id,COALESCE(json_extract(record,'$.owner_id'),''),COALESCE(json_extract(record,'$.owner_closed'),0) FROM runs WHERE state IN ('queued','reviewing','handling_feedback')
 AND (COALESCE(json_extract(record,'$.owner_id'),'')='' OR NOT EXISTS(SELECT 1 FROM private_owners p WHERE p.id=json_extract(runs.record,'$.owner_id')))
 ORDER BY CASE WHEN COALESCE(json_extract(record,'$.owner_closed'),0)=1
 AND length(COALESCE(json_extract(record,'$.owner_id'),''))=32
 AND json_extract(record,'$.owner_id') NOT GLOB '*[^a-f0-9]*'
 THEN 0 ELSE 1 END,COALESCE(json_extract(record,'$.recovery_checked_at'),''),updated_at,id LIMIT ?`, RecoveryBatchLimit)
	if err != nil {
		return report, errors.Join(individualErrors, err)
	}
	type missingOwner struct {
		id, owner string
		closed    bool
	}
	missing := []missingOwner{}
	for rows.Next() {
		var item missingOwner
		if err = rows.Scan(&item.id, &item.owner, &item.closed); err != nil {
			break
		}
		missing = append(missing, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return report, errors.Join(individualErrors, err)
	}
	for _, item := range missing {
		if err := ctx.Err(); err != nil {
			return report, errors.Join(individualErrors, err)
		}
		if item.closed && ownerIDPattern.MatchString(item.owner) {
			result := RecoveredOwner{ID: item.owner, State: "recovered", RunIDs: []string{item.id}}
			if !dry {
				if err := s.failLostOwner(ctx, item.owner); err != nil {
					individualErrors = errors.Join(individualErrors, err)
					result.State = "recovery_failed"
				}
			}
			report.Owners = append(report.Owners, result)
		} else {
			report.UnknownRuns = append(report.UnknownRuns, item.id)
		}
		if !dry {
			// Maintenance order is independent of user-visible run history.
			// Persist it outside the terminal-transition transaction: rollback
			// of one broken run must not strand every later bounded batch.
			if _, err := s.db.ExecContext(ctx, `UPDATE runs SET record=json_set(record,'$.recovery_checked_at',?) WHERE id=? AND state IN ('queued','reviewing','handling_feedback')`, timestamp(), item.id); err != nil {
				individualErrors = errors.Join(individualErrors, err)
			}
		}
	}
	if len(missing) == RecoveryBatchLimit {
		report.BatchFull = true
	}
	return report, individualErrors
}

func (s *Store) recoverPrivateOwner(ctx context.Context, id, raw string, dry bool) (result RecoveredOwner, err error) {
	result = RecoveredOwner{ID: id, State: "unknown", RunIDs: []string{}}
	if !ownerIDPattern.MatchString(id) {
		return result, nil
	}
	var closing bool
	var identity string
	var leaseIdentity string
	load := func() error {
		return s.db.QueryRowContext(ctx, "SELECT processes,closing,root_identity,lease_identity FROM private_owners WHERE id=?", id).Scan(&raw, &closing, &identity, &leaseIdentity)
	}
	if err := load(); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result.State = "already_recovered"
			return result, nil
		}
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE json_extract(record,'$.owner_id')=? ORDER BY id`, id)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var runID string
		if err = rows.Scan(&runID); err != nil {
			break
		}
		result.RunIDs = append(result.RunIDs, runID)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	parent := filepath.Join(s.directory, "private-work")
	root := filepath.Join(parent, id)
	finishMissing := func() (RecoveredOwner, error) {
		if err := load(); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				result.State = "already_recovered"
				return result, nil
			}
			return result, err
		}
		if !closing {
			return result, nil
		}
		result.State = "recovered"
		if dry {
			return result, nil
		}
		if err := s.failLostOwner(ctx, id); err != nil {
			result.State = "recovery_failed"
			return result, err
		}
		if err := s.finishPrivateOwner(ctx, id); err != nil {
			result.State = "resources_pending"
			return result, err
		}
		return result, nil
	}
	if _, err := privateDirectory(parent, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return finishMissing()
		}
		return result, nil
	}
	info, err := privateDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		// A durable closing barrier proves shutdown before deletion. Without
		// it, missing storage alone is not an owner-liveness proof.
		return finishMissing()
	}
	if err != nil {
		if loadErr := load(); errors.Is(loadErr, sql.ErrNoRows) {
			result.State = "already_recovered"
			return result, nil
		} else if loadErr != nil {
			return result, loadErr
		}
		result.Storage = root
		return result, nil
	}
	currentIdentity, identityErr := rootIdentity(info)
	if identityErr != nil || currentIdentity != identity {
		result.Storage = root
		return result, nil
	}
	ownerLease, err := lease.Acquire(filepath.Join(root, "owner.lock"), false)
	if errors.Is(err, lease.ErrBusy) {
		result.State = "live"
		return result, nil
	}
	if err != nil {
		if loadErr := load(); loadErr != nil {
			if errors.Is(loadErr, sql.ErrNoRows) {
				result.State = "already_recovered"
				return result, nil
			}
			return result, loadErr
		}
		if closing && errors.Is(err, os.ErrNotExist) {
			// Recursive deletion may remove owner.lock before the rest of the
			// scope. The persisted shutdown barrier forbids further launches.
			result.State = "recovered"
			if dry {
				return result, nil
			}
			if err := s.failLostOwner(ctx, id); err != nil {
				result.State = "recovery_failed"
				return result, err
			}
			if err := s.removePrivateRoot(ctx, id, root, raw, info); err != nil {
				result.State = "resources_pending"
				result.Storage = root
				return result, err
			}
			return result, nil
		}
		result.Storage = root
		return result, nil
	}
	defer ownerLease.Close()
	// Re-read after the lease: another recovery may have finished while this
	// contender was opening the stable inode. Never act on an earlier snapshot.
	if err = load(); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result.State = "already_recovered"
			return result, nil
		}
		return result, err
	}
	leaseInfo, infoErr := ownerLease.Info()
	if infoErr != nil {
		result.Storage = root
		return result, nil
	}
	currentLeaseIdentity, identityErr := rootIdentity(leaseInfo)
	if identityErr != nil || currentLeaseIdentity != leaseIdentity {
		result.Storage = root
		return result, nil
	}
	processes, decodeErr := decodeProcesses(raw, id)
	stopped := false
	if decodeErr == nil {
		stopped, decodeErr = resourcesStopped(ctx, processes)
	}
	result.State = "recovered"
	if decodeErr != nil || !stopped {
		result.State = "resources_pending"
		result.Storage = root
	}
	if dry {
		return result, nil
	}
	if err = s.failLostOwner(ctx, id); err != nil {
		result.State = "recovery_failed"
		result.Storage = root
		return result, err
	}
	if result.State == "resources_pending" {
		return result, nil
	}
	if err = s.removePrivateRoot(ctx, id, root, raw, info); err != nil {
		result.State = "resources_pending"
		result.Storage = root
		return result, err
	}
	return result, nil
}

func (s *Store) failLostOwner(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE runs SET state=CASE WHEN json_extract(record,'$.purpose')='feedback' THEN 'feedback_pending' WHEN COALESCE(json_extract(record,'$.cancellation_requested'),0)=1 THEN 'cancelled' WHEN COALESCE(json_extract(record,'$.superseded'),0)=1 THEN 'stale' ELSE 'failed' END,
 failure=CASE WHEN json_extract(record,'$.purpose')='feedback' THEN 'Feedback owner stopped; inspect delivered effects and explicitly resume for canonical reconciliation. No operation was blindly replayed.' ELSE 'Private review owner stopped; no session or answer was resumed. Inspect this run and explicitly start a fresh review.' END,updated_at=?
 WHERE json_extract(record,'$.owner_id')=? AND state IN ('queued','reviewing','handling_feedback')`, timestamp(), id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE input_requests SET state=CASE WHEN state IN ('sending','sent') THEN 'delivery_unknown' ELSE 'closed' END,
 closed_at=?,reason='owner_lost' WHERE closed_at='' AND EXISTS(SELECT 1 FROM runs r WHERE r.id=input_requests.run_id AND json_extract(r.record,'$.owner_id')=? AND r.state NOT IN ('reviewing','handling_feedback'))`, timestamp(), id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state',(SELECT r.state FROM runs r WHERE r.id=json_extract(inbox_items.record,'$.run_id')))
 WHERE json_extract(record,'$.state') IN ('reviewing','queued','handling_feedback') AND EXISTS(SELECT 1 FROM runs r WHERE r.id=json_extract(inbox_items.record,'$.run_id') AND json_extract(r.record,'$.owner_id')=? AND r.repository=inbox_items.repository AND r.actor_id=inbox_items.actor_id AND r.pull_id=inbox_items.pull_id AND r.state IN ('failed','cancelled','stale','feedback_pending'))`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
