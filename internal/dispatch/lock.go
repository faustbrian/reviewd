//go:build darwin || linux

package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/faustbrian/reviewd/internal/lease"
	"github.com/faustbrian/reviewd/internal/store"
)

var ErrExecutionLeaseBusy = errors.New("agent execution is live or its child shutdown remains unproved")

// ExecutionLeaseKey deliberately excludes actor, event and profile. GitHub database
// identities survive renames; every local account/profile shares one PR scope.
type ExecutionLeaseKey struct {
	Host         string `json:"host"`
	RepositoryID int64  `json:"repository_id"`
	PullID       int64  `json:"pull_id"`
}

type executionLeaseRecord struct {
	Schema  int               `json:"schema_version"`
	Key     ExecutionLeaseKey `json:"key"`
	Profile string            `json:"profile"`
	RunID   string            `json:"run_id"`
}

// ExecutionLease couples a PR admission lease to existing process ownership.
// The durable pointer proves child shutdown across profiles; selected skills,
// not this lease, own external operations and their delivery decisions.
type ExecutionLease struct {
	key       ExecutionLeaseKey
	directory string
	lock      *lease.Lease
	once      sync.Once
	closeErr  error
}

var executionLeaseHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func executionLeaseName(key ExecutionLeaseKey) string {
	key.Host = strings.ToLower(key.Host)
	data, _ := json.Marshal(key)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// directory is the application's shared operator-state directory, never the
// selected profile, repository config or a model-provided path.
func AcquireExecutionLease(ctx context.Context, directory string, key ExecutionLeaseKey) (_ *ExecutionLease, err error) {
	key.Host = strings.ToLower(key.Host)
	if directory == "" || !executionLeaseHost.MatchString(key.Host) || key.RepositoryID <= 0 || key.PullID <= 0 {
		return nil, errors.New("agent admission requires canonical host/repository/PR identity")
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("shared agent ownership requires a private directory, not a symlink")
	}
	lock, err := lease.Acquire(filepath.Join(directory, executionLeaseName(key)+".lock"), true)
	if errors.Is(err, lease.ErrBusy) {
		return nil, ErrExecutionLeaseBusy
	}
	if err != nil {
		return nil, err
	}
	executionLease := &ExecutionLease{key: key, directory: directory, lock: lock}
	defer func() {
		if err != nil {
			err = errors.Join(err, executionLease.Close())
		}
	}()
	path := filepath.Join(directory, executionLeaseName(key)+".json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return executionLease, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("prior agent owner pointer is unavailable or not a private regular file; preserve it for reconciliation")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, 64<<10+1))
	closeErr := file.Close()
	if statErr != nil || !os.SameFile(info, opened) || readErr != nil || closeErr != nil || len(data) > 64<<10 {
		return nil, errors.New("prior agent owner pointer changed or could not be completely read")
	}
	var record executionLeaseRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Schema != 1 || record.Key != key || !filepath.IsAbs(record.Profile) || filepath.Clean(record.Profile) != record.Profile || record.RunID == "" {
		return nil, errors.New("prior agent owner pointer does not match this PR; preserve it for reconciliation")
	}
	profile, err := store.OpenExisting(ctx, record.Profile)
	if err != nil {
		return nil, errors.New("prior agent profile is unavailable; restore its authoritative state before starting another agent execution")
	}
	defer profile.Close()
	run, err := profile.Get(ctx, record.RunID)
	if err != nil || !executionLease.matches(run) {
		return nil, errors.New("prior agent run is unavailable or mismatched; preserve its state before starting another agent execution")
	}
	recovered, err := profile.ReconcilePrivateOwner(ctx, run.OwnerID)
	if err != nil {
		return nil, err
	}
	if recovered.State != "recovered" && recovered.State != "already_recovered" && recovered.State != "feedback_pending" {
		return nil, ErrExecutionLeaseBusy
	}
	run, err = profile.Get(ctx, record.RunID)
	if err != nil || !executionLease.matches(run) || (!run.OwnerClosed && recovered.State != "feedback_pending") {
		return nil, ErrExecutionLeaseBusy
	}
	// Older schema-1 pointers may contain an ancestor alias. OpenExisting
	// validated the exact private profile; compare its admitted identity without
	// rewriting the durable pointer or manufacturing another execution owner.

	return executionLease, nil
}

func (m *ExecutionLease) matches(run store.Run) bool {
	return (run.Purpose == "feedback" || store.IsNativeExecutor(run.Executor)) && run.RepositoryID == m.key.RepositoryID && run.PullID == m.key.PullID && strings.Split(run.Repository, "/")[0] == m.key.Host && run.OwnerID != ""
}

// Attach is persisted before any source/model/command child starts. A service
// persists process ownership, not a skill-workflow or GitHub-effect decision.
func (m *ExecutionLease) Attach(ctx context.Context, profile *store.Store, runID string) error {
	run, err := profile.Get(ctx, runID)
	if err != nil || !store.IsNativeExecutor(run.Executor) || !m.matches(run) || run.OwnerClosed || (run.State != store.Queued && run.State != store.AgentRunning) {
		return errors.New("agent owner pointer requires this PR's current queued/handling run and private owner")
	}
	locked, err := m.lock.Info()
	current, statErr := os.Lstat(filepath.Join(m.directory, executionLeaseName(m.key)+".lock"))
	if err != nil || statErr != nil || !os.SameFile(locked, current) {
		return errors.New("agent admission lease changed; no worker may start")
	}
	data, err := json.Marshal(executionLeaseRecord{Schema: 1, Key: m.key, Profile: profile.Directory(), RunID: run.ID})
	if err != nil || len(data) > 64<<10 {
		return errors.New("agent owner pointer exceeds its supported bound")
	}
	file, err := os.CreateTemp(m.directory, ".agent-owner-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path) // Exact task-created temporary file, never the lock.
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(m.directory, executionLeaseName(m.key)+".json")); err != nil {
		return err
	}
	directory, err := os.Open(m.directory)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (m *ExecutionLease) Close() error {
	m.once.Do(func() { m.closeErr = m.lock.Close() })
	return m.closeErr
}
