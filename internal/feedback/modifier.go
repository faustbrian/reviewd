//go:build darwin || linux

package feedback

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

var ErrModifierBusy = errors.New("feedback modifier is live or its child shutdown remains unproved")

// ModifierKey deliberately excludes actor, event and profile. GitHub database
// identities survive renames; every local account/profile shares one PR scope.
type ModifierKey struct {
	Host         string `json:"host"`
	RepositoryID int64  `json:"repository_id"`
	PullID       int64  `json:"pull_id"`
}

type ModifierPrior struct {
	Profile string
	Run     store.Run
}

type modifierRecord struct {
	Schema  int         `json:"schema_version"`
	Key     ModifierKey `json:"key"`
	Profile string      `json:"profile"`
	RunID   string      `json:"run_id"`
}

// Modifier is an admission lease, not another execution/recovery owner. Its
// durable pointer delegates all lifetime proof and effects to the existing
// profile Store/PrivateOwner. The pointer remains after Close so another
// profile can reconcile prior pending/partial effects instead of replaying.
type Modifier struct {
	Prior     *ModifierPrior
	key       ModifierKey
	directory string
	lock      *lease.Lease
	once      sync.Once
	closeErr  error
}

var modifierHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

func modifierName(key ModifierKey) string {
	key.Host = strings.ToLower(key.Host)
	data, _ := json.Marshal(key)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// directory is the application's shared operator-state directory, never the
// selected profile, repository config or a model-provided path.
func AcquireModifier(ctx context.Context, directory string, key ModifierKey) (_ *Modifier, err error) {
	key.Host = strings.ToLower(key.Host)
	if directory == "" || !modifierHost.MatchString(key.Host) || key.RepositoryID <= 0 || key.PullID <= 0 {
		return nil, errors.New("feedback admission requires canonical host/repository/PR identity")
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
		return nil, errors.New("shared feedback ownership requires a private directory, not a symlink")
	}
	lock, err := lease.Acquire(filepath.Join(directory, modifierName(key)+".lock"), true)
	if errors.Is(err, lease.ErrBusy) {
		return nil, ErrModifierBusy
	}
	if err != nil {
		return nil, err
	}
	modifier := &Modifier{key: key, directory: directory, lock: lock}
	defer func() {
		if err != nil {
			err = errors.Join(err, modifier.Close())
		}
	}()
	path := filepath.Join(directory, modifierName(key)+".json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return modifier, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("prior feedback owner pointer is unavailable or not a private regular file; preserve it for reconciliation")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, 64<<10+1))
	closeErr := file.Close()
	if statErr != nil || !os.SameFile(info, opened) || readErr != nil || closeErr != nil || len(data) > 64<<10 {
		return nil, errors.New("prior feedback owner pointer changed or could not be completely read")
	}
	var record modifierRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Schema != 1 || record.Key != key || !filepath.IsAbs(record.Profile) || filepath.Clean(record.Profile) != record.Profile || record.RunID == "" {
		return nil, errors.New("prior feedback owner pointer does not match this PR; preserve it for reconciliation")
	}
	profile, err := store.OpenExisting(ctx, record.Profile)
	if err != nil {
		return nil, errors.New("prior feedback profile is unavailable; restore its authoritative state before starting another modifier")
	}
	defer profile.Close()
	run, err := profile.Get(ctx, record.RunID)
	if err != nil || !modifier.matches(run) {
		return nil, errors.New("prior feedback run is unavailable or mismatched; preserve its state before starting another modifier")
	}
	recovered, err := profile.ReconcilePrivateOwner(ctx, run.OwnerID)
	if err != nil {
		return nil, err
	}
	if recovered.State != "recovered" && recovered.State != "already_recovered" {
		return nil, ErrModifierBusy
	}
	run, err = profile.Get(ctx, record.RunID)
	if err != nil || !modifier.matches(run) || !run.OwnerClosed {
		return nil, ErrModifierBusy
	}
	modifier.Prior = &ModifierPrior{Profile: record.Profile, Run: run}
	return modifier, nil
}

func (m *Modifier) matches(run store.Run) bool {
	return run.Purpose == "feedback" && run.RepositoryID == m.key.RepositoryID && run.PullID == m.key.PullID && strings.Split(run.Repository, "/")[0] == m.key.Host && run.OwnerID != ""
}

// Attach is persisted before any source/model/command child starts. A service
// must inspect Prior for pending/ambiguous effects before choosing a new run;
// acquiring this lease alone does not authorize replay or new remote effects.
func (m *Modifier) Attach(ctx context.Context, profile *store.Store, runID string) error {
	run, err := profile.Get(ctx, runID)
	if err != nil || !m.matches(run) || run.OwnerClosed || (run.State != store.Queued && run.State != store.HandlingFeedback) {
		return errors.New("feedback owner pointer requires this PR's current queued/handling run and private owner")
	}
	locked, err := m.lock.Info()
	current, statErr := os.Lstat(filepath.Join(m.directory, modifierName(m.key)+".lock"))
	if err != nil || statErr != nil || !os.SameFile(locked, current) {
		return errors.New("feedback admission lease changed; no worker may start")
	}
	data, err := json.Marshal(modifierRecord{Schema: 1, Key: m.key, Profile: profile.Directory(), RunID: run.ID})
	if err != nil || len(data) > 64<<10 {
		return errors.New("feedback owner pointer exceeds its supported bound")
	}
	file, err := os.CreateTemp(m.directory, ".feedback-owner-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path) // Exact task-created temporary file, never the lock.
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(m.directory, modifierName(m.key)+".json")); err != nil {
		return err
	}
	directory, err := os.Open(m.directory)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (m *Modifier) Close() error {
	m.once.Do(func() { m.closeErr = m.lock.Close() })
	return m.closeErr
}
