//go:build darwin || linux

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/faustbrian/reviewd/internal/lease"
	"github.com/faustbrian/reviewd/internal/lifetime"
)

// SQLite owns resource identities and launch ambiguity. The kernel lease owns
// liveness; neither timestamps nor saved process numbers can replace it.
const ownerSchema = `CREATE TABLE IF NOT EXISTS private_owners (
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, root_identity TEXT NOT NULL, lease_identity TEXT NOT NULL,
 checked_at TEXT NOT NULL DEFAULT '', closing INTEGER NOT NULL DEFAULT 0 CHECK(closing IN (0,1)),
 processes TEXT NOT NULL CHECK(json_valid(processes) AND json_type(processes)='object')
);`

var ownerIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func rootIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("cannot establish private root identity")
	}
	return fmt.Sprintf("%v:%v", stat.Dev, stat.Ino), nil
}

type PrivateOwner struct {
	ID, Root string
	store    *Store
	lease    *lease.Lease
	info     os.FileInfo
	once     sync.Once
	err      error
}

func privateDirectory(path string, create bool) (os.FileInfo, error) {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private runtime storage must be a mode-0700 directory, not a symlink")
	}
	return info, nil
}

func (s *Store) BeginPrivateOwner(ctx context.Context) (owner *PrivateOwner, err error) {
	parent := filepath.Join(s.directory, "private-work")
	if _, err := privateDirectory(parent, true); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(parent, id)
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	info, err := privateDirectory(root, false)
	if err != nil {
		return nil, err
	}
	owned := &PrivateOwner{ID: id, Root: root, store: s, info: info}
	defer func() {
		if err != nil {
			if owned.lease != nil {
				_ = owned.lease.Close()
			}
			_ = os.RemoveAll(root) // Exact directory created by this call only.
		}
	}()
	owned.lease, err = lease.Acquire(filepath.Join(root, "owner.lock"), true)
	if err != nil {
		return nil, err
	}
	identity, identityErr := rootIdentity(info)
	if identityErr != nil {
		return nil, identityErr
	}
	leaseInfo, err := owned.lease.Info()
	if err != nil {
		return nil, err
	}
	leaseIdentity, identityErr := rootIdentity(leaseInfo)
	if identityErr != nil {
		return nil, identityErr
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO private_owners(id,created_at,root_identity,lease_identity,processes) VALUES(?,?,?,?,'{}')", id, timestamp(), identity, leaseIdentity)
	if err != nil {
		return nil, errors.New("cannot persist private resource ownership; no worker started")
	}
	return owned, nil
}

func (o *PrivateOwner) Context(ctx context.Context) context.Context {
	return lifetime.WithScope(ctx, lifetime.Scope{Root: o.Root, Tracker: o})
}

type privateProcess struct {
	owner *PrivateOwner
	id    string
}

func (o *PrivateOwner) Starting(ctx context.Context) (lifetime.Process, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	result, err := o.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_set(processes,?,0)
	 WHERE id=? AND closing=0 AND (SELECT count(*) FROM json_each(processes))<16`, processPath(id), o.ID)
	// Generated hexadecimal keys need no escaping; use the canonical JSON path.
	if err = changed(result, err, "cannot record private process launch"); err != nil {
		return nil, err
	}
	return &privateProcess{owner: o, id: id}, nil
}

func processPath(id string) string { return `$."` + id + `"` }

func (p *privateProcess) Started(ctx context.Context, group int) error {
	if group <= 0 {
		return errors.New("private process requires a positive group identity")
	}
	result, err := p.owner.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_set(processes,?,?) WHERE id=? AND json_extract(processes,?)=0`, processPath(p.id), group, p.owner.ID, processPath(p.id))
	return changed(result, err, "cannot persist private process group")
}
func (p *privateProcess) Finished(ctx context.Context) error {
	result, err := p.owner.store.db.ExecContext(ctx, `UPDATE private_owners SET processes=json_remove(processes,?) WHERE id=?`, processPath(p.id), p.owner.ID)
	return changed(result, err, "cannot close private process ownership")
}

func decodeProcesses(raw string) (map[string]int, error) {
	var processes map[string]int
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &processes) != nil || processes == nil || len(processes) > 16 {
		return nil, errors.New("invalid private process ownership; resources retained")
	}
	for id, group := range processes {
		if !ownerIDPattern.MatchString(id) || group < 0 {
			return nil, errors.New("invalid private process identity; resources retained")
		}
	}
	return processes, nil
}

func resourcesStopped(ctx context.Context, processes map[string]int) (bool, error) {
	if len(processes) == 0 {
		return true, nil
	}
	for _, group := range processes {
		if group == 0 {
			return false, nil
		}
	}
	for _, group := range processes {
		alive, err := lifetime.GroupAlive(ctx, group)
		if err != nil {
			return false, err
		}
		if alive {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) removePrivateRoot(ctx context.Context, id, root, observedProcesses string, info os.FileInfo) error {
	current, err := privateDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return s.reconcileRemovedPrivateRoot(ctx, id, root)
	}
	if err != nil || !os.SameFile(info, current) {
		return errors.New("private resource root changed; storage retained")
	}
	// This barrier both records proven child shutdown before file deletion and
	// rejects any late launch. The exact process snapshot prevents a concurrent
	// new registration from being mistaken for the already inspected groups.
	result, err := s.db.ExecContext(ctx, `UPDATE private_owners SET closing=1,processes='{}' WHERE id=? AND (processes=? OR closing=1)`, id, observedProcesses)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		// A contender may finish after our identity read but before this CAS.
		// Reconcile only completed deletion, never a changed live snapshot.
		return s.reconcileRemovedPrivateRoot(ctx, id, root)
	}
	if err := os.RemoveAll(root); err != nil {
		return errors.New("cannot remove owned private storage; run reviewd recover")
	}
	return s.finishPrivateOwner(ctx, id)
}

// Missing files alone do not prove shutdown. Another cleaner must have either
// persisted its shutdown barrier or acknowledged it by removing the owner row.
// This path never deletes storage, including a replacement at the same path.
func (s *Store) reconcileRemovedPrivateRoot(ctx context.Context, id, root string) error {
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return errors.New("private resource root changed; storage retained")
	}
	var closing bool
	err := s.db.QueryRowContext(ctx, "SELECT closing FROM private_owners WHERE id=?", id).Scan(&closing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !closing {
		return errors.New("private execution changed before cleanup; storage retained")
	}
	return s.finishPrivateOwner(ctx, id)
}

func (s *Store) finishPrivateOwner(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep a compact shutdown fact on the durable run. If the process dies
	// before its failure defer, recovery need not guess from a missing lease.
	_, err = tx.ExecContext(ctx, `UPDATE runs SET record=json_set(record,'$.owner_closed',json('true')) WHERE json_extract(record,'$.owner_id')=? AND EXISTS(SELECT 1 FROM private_owners WHERE id=? AND closing=1)`, id, id)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM private_owners WHERE id=? AND closing=1", id); err != nil {
		return err
	}
	return tx.Commit()
}

// Close runs after worker teardown and gates all source/skill/runtime storage
// cleanup. An ambiguous child retains the complete shared private scope.
func (o *PrivateOwner) Close() error {
	o.once.Do(func() {
		defer o.lease.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var raw string
		if err := o.store.db.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", o.ID).Scan(&raw); err != nil {
			o.err = errors.New("cannot inspect private resource ownership; storage retained")
			return
		}
		processes, err := decodeProcesses(raw)
		if err != nil {
			o.err = err
			return
		}
		stopped, err := resourcesStopped(ctx, processes)
		if err != nil {
			o.err = err
			return
		}
		if !stopped {
			o.err = errors.New("private child lifetime is unresolved; storage retained for reviewd recover")
			return
		}
		o.err = o.store.removePrivateRoot(ctx, o.ID, o.Root, raw, o.info)
	})
	return o.err
}
