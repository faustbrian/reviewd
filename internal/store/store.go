// Package store is the authoritative transactional owner of local run state.
// A prepared result, a publication decision and a verified remote outcome are
// different facts; neither process restart nor silence advances that authority.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type State string

const (
	Queued             State = "queued"
	WaitingChecks      State = "waiting_checks"
	Reviewing          State = "reviewing"
	Prepared           State = "prepared"
	Publishing         State = "publishing"
	Published          State = "published"
	PublicationUnknown State = "publication_unknown"
	Partial            State = "partial"
	Failed             State = "failed"
	Cancelled          State = "cancelled"
	Stale              State = "stale"
)

type Run struct {
	ID             string          `json:"id"`
	Repository     string          `json:"repository"`
	RepositoryID   int64           `json:"repository_id"`
	PullID         int64           `json:"pull_id"`
	Number         int             `json:"number"`
	ActorID        int64           `json:"actor_id"`
	ActorLogin     string          `json:"actor_login"`
	WorkKey        string          `json:"work_key"`
	State          State           `json:"state"`
	Private        bool            `json:"private"`
	Configuration  json.RawMessage `json:"configuration"`
	Context        json.RawMessage `json:"context,omitempty"`
	SkillDigest    string          `json:"skill_digest,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	ThreadID       string          `json:"thread_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	PayloadID      string          `json:"payload_id,omitempty"`
	Prepared       json.RawMessage `json:"prepared,omitempty"`
	Outcome        json.RawMessage `json:"outcome,omitempty"`
	Failure        string          `json:"failure,omitempty"`
	RequestedModel string          `json:"requested_model,omitempty"`
	EffectiveModel string          `json:"effective_model,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	Specialists    json.RawMessage `json:"specialists,omitempty"`
}
type Store struct{ db *sql.DB }

func Open(ctx context.Context, directory string) (store *Store, err error) {
	directory, err = filepath.Abs(directory)
	if err != nil {
		return nil, errors.New("invalid local state directory")
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, errors.New("cannot create private local state directory")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state.directory must be a private directory (mode 0700), not a symlink; choose a private profile location")
	}
	path := filepath.Join(directory, "reviewd.sqlite")
	file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if createErr == nil {
		if err = file.Close(); err != nil {
			return nil, errors.New("cannot initialize private state database")
		}
	} else if !errors.Is(createErr, os.ErrExist) {
		return nil, errors.New("cannot create private state database")
	}
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state database must be a private regular file (mode 0600), not a symlink")
	}
	location := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	location.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, errors.New("cannot open local state database")
	}
	db.SetMaxOpenConns(1)
	store = &Store{db: db}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, errors.New("cannot read state database schema")
	}
	if version > 1 {
		return nil, errors.New("state database uses a newer schema; upgrade reviewd without replacing the database")
	}
	if version == 0 {
		tx, beginErr := db.BeginTx(ctx, nil)
		if beginErr != nil {
			return nil, errors.New("cannot initialize state schema")
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS runs (
 id TEXT PRIMARY KEY, repository TEXT NOT NULL, repository_id INTEGER NOT NULL,
 pull_id INTEGER NOT NULL, actor_id INTEGER NOT NULL, work_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','waiting_checks','reviewing','prepared','publishing','published','publication_unknown','partial','failed','cancelled','stale')),
 record TEXT NOT NULL CHECK(json_valid(record)), updated_at TEXT NOT NULL,
 prepared TEXT CHECK(prepared IS NULL OR json_valid(prepared)), payload_id TEXT,
 thread_id TEXT, turn_id TEXT, outcome TEXT CHECK(outcome IS NULL OR json_valid(outcome)), failure TEXT,
 UNIQUE(repository,repository_id,pull_id,actor_id,work_key)
);
CREATE TABLE IF NOT EXISTS publication_decisions (
 run_id TEXT NOT NULL REFERENCES runs(id), payload_id TEXT NOT NULL,
 decided_at TEXT NOT NULL, PRIMARY KEY(run_id,payload_id)
);
CREATE TABLE IF NOT EXISTS publication_attempts (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), payload_id TEXT NOT NULL,
 started_at TEXT NOT NULL
);
PRAGMA user_version=1;`)
		if err != nil {
			return nil, errors.New("cannot create state schema")
		}
		if err = tx.Commit(); err != nil {
			return nil, errors.New("cannot commit state schema")
		}
	}
	return store, nil
}

func (s *Store) WaitForChecks(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='waiting_checks',updated_at=? WHERE id=? AND state='queued'", timestamp(), id)
	return changed(result, err, "could not persist waiting-for-checks review")
}

// RecordWorker preserves partial thread/turn identity on interruption as well
// as completed execution. It never advances the run's lifecycle or opinion.
func (s *Store) RecordWorker(ctx context.Context, id, thread, turn, requested, effective string, usage, specialists json.RawMessage) error {
	if len(usage) == 0 {
		usage = json.RawMessage(`null`)
	}
	if len(specialists) == 0 {
		specialists = json.RawMessage(`[]`)
	}
	if !json.Valid(usage) || !json.Valid(specialists) {
		return errors.New("worker metadata was not valid JSON")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET thread_id=?,turn_id=?,record=json_set(record,'$.requested_model',?,'$.effective_model',?,'$.usage',json(?),'$.specialists',json(?)),updated_at=? WHERE id=? AND state IN ('reviewing','prepared')", thread, turn, requested, effective, string(usage), string(specialists), timestamp(), id)
	return changed(result, err, "could not persist worker execution identity")
}
func (s *Store) Close() error { return s.db.Close() }
func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) Create(ctx context.Context, run Run) (Run, bool, error) {
	if run.Repository == "" || run.RepositoryID <= 0 || run.PullID <= 0 || run.Number < 1 || run.ActorID <= 0 || run.ActorLogin == "" || run.WorkKey == "" || run.State != Queued || !json.Valid(run.Configuration) || (len(run.Context) > 0 && !json.Valid(run.Context)) {
		return Run{}, false, errors.New("run requires complete scope, frozen policy and queued state")
	}
	var err error
	run.ID, err = newID()
	if err != nil {
		return Run{}, false, err
	}
	run.CreatedAt = time.Now().UTC()
	run.UpdatedAt = run.CreatedAt
	data, err := json.Marshal(run)
	if err != nil {
		return Run{}, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,repository,repository_id,pull_id,actor_id,work_key,state,record,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(repository,repository_id,pull_id,actor_id,work_key) DO NOTHING`, run.ID, run.Repository, run.RepositoryID, run.PullID, run.ActorID, run.WorkKey, run.State, string(data), run.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Run{}, false, errors.New("could not persist queued review")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Run{}, false, err
	}
	if count == 1 {
		return run, true, nil
	}
	var existing string
	err = s.db.QueryRowContext(ctx, "SELECT id FROM runs WHERE repository=? AND repository_id=? AND pull_id=? AND actor_id=? AND work_key=?", run.Repository, run.RepositoryID, run.PullID, run.ActorID, run.WorkKey).Scan(&existing)
	if err != nil {
		return Run{}, false, errors.New("could not reconcile existing review work")
	}
	run, err = s.Get(ctx, existing)
	return run, false, err
}

const selectRun = `SELECT record,state,updated_at,prepared,payload_id,thread_id,turn_id,outcome,failure FROM runs`

type rowScanner interface{ Scan(...any) error }

func readRun(row rowScanner) (Run, error) {
	var data, state, updated string
	var prepared, payload, thread, turn, outcome, failure sql.NullString
	if err := row.Scan(&data, &state, &updated, &prepared, &payload, &thread, &turn, &outcome, &failure); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, errors.New("review run was not found in the selected profile")
		}
		return Run{}, errors.New("could not read durable review state")
	}
	var run Run
	if json.Unmarshal([]byte(data), &run) != nil {
		return Run{}, errors.New("durable review record is invalid; preserve the database for recovery")
	}
	run.State = State(state)
	var err error
	run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return Run{}, errors.New("durable review timestamp is invalid")
	}
	run.Prepared = json.RawMessage(prepared.String)
	run.PayloadID = payload.String
	run.ThreadID = thread.String
	run.TurnID = turn.String
	run.Outcome = json.RawMessage(outcome.String)
	run.Failure = failure.String
	return run, nil
}
func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	return readRun(s.db.QueryRowContext(ctx, selectRun+" WHERE id=?", id))
}
func (s *Store) List(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, selectRun+" ORDER BY updated_at DESC,id")
	if err != nil {
		return nil, errors.New("could not read the local review inbox")
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		run, err := readRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("local review inbox read was incomplete")
	}
	return runs, nil
}
func (s *Store) Claim(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='reviewing',updated_at=? WHERE id=? AND state='queued'", timestamp(), id)
	if err != nil {
		return false, errors.New("could not claim queued review work")
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
func changed(result sql.Result, err error, message string) error {
	if err != nil {
		return errors.New(message)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return errors.New(message + "; the run state or decision scope changed")
	}
	return nil
}
func (s *Store) Prepare(ctx context.Context, id, payloadID string, payload json.RawMessage, thread, turn string) error {
	if payloadID == "" || !json.Valid(payload) || thread == "" || turn == "" {
		return errors.New("prepared review requires a valid exact payload and completed thread/turn identity")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='prepared',prepared=?,payload_id=?,thread_id=?,turn_id=?,updated_at=? WHERE id=? AND state='reviewing'", string(payload), payloadID, thread, turn, timestamp(), id)
	return changed(result, err, "could not persist prepared review")
}

// RefreshPrepared atomically changes a preview and revokes its decisions.
// Revocation is permanent even if later CI returns to an earlier payload ID.
func (s *Store) RefreshPrepared(ctx context.Context, id, previous, payloadID string, payload json.RawMessage) error {
	if previous == "" || payloadID == "" || !json.Valid(payload) {
		return errors.New("refresh requires the complete old and new preview identities")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("cannot begin prepared review refresh")
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE runs SET prepared=?,payload_id=?,updated_at=? WHERE id=? AND payload_id=? AND state='prepared'", string(payload), payloadID, timestamp(), id, previous)
	if err = changed(result, err, "could not replace current prepared preview"); err != nil {
		return err
	}
	if previous != payloadID {
		if _, err = tx.ExecContext(ctx, "DELETE FROM publication_decisions WHERE run_id=?", id); err != nil {
			return errors.New("cannot revoke superseded publication decisions")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("cannot commit prepared review refresh")
	}
	return nil
}

func (s *Store) DecidePublication(ctx context.Context, id, payloadID string) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO publication_decisions(run_id,payload_id,decided_at) SELECT id,payload_id,? FROM runs WHERE id=? AND payload_id=? AND state='prepared' ON CONFLICT(run_id,payload_id) DO UPDATE SET decided_at=excluded.decided_at`, timestamp(), id, payloadID)
	return changed(result, err, "publication decision does not match the current prepared preview")
}
func (s *Store) BeginPublication(ctx context.Context, id, payloadID string, initial ...json.RawMessage) (string, bool, error) {
	outcome := "{}"
	if len(initial) > 1 || (len(initial) == 1 && !json.Valid(initial[0])) {
		return "", false, errors.New("invalid initial publication observation")
	}
	if len(initial) == 1 {
		outcome = string(initial[0])
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, errors.New("cannot begin publication claim")
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET state='publishing',outcome=?,updated_at=? WHERE id=? AND payload_id=? AND state='prepared' AND EXISTS(SELECT 1 FROM publication_decisions WHERE run_id=runs.id AND payload_id=runs.payload_id)`, outcome, timestamp(), id, payloadID)
	if err != nil {
		return "", false, errors.New("cannot claim exact-payload publication")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if count != 1 {
		return "", false, nil
	}
	attempt, err := newID()
	if err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO publication_attempts(id,run_id,payload_id,started_at) VALUES(?,?,?,?)", attempt, id, payloadID, timestamp()); err != nil {
		return "", false, errors.New("cannot persist publication attempt before remote submission")
	}
	if err = tx.Commit(); err != nil {
		return "", false, errors.New("cannot commit publication claim")
	}
	return attempt, true, nil
}

var ErrOutcomeChanged = errors.New("publication state changed concurrently; reconcile its current claim")

func (s *Store) MarkOutcome(ctx context.Context, observed Run, state State, outcome json.RawMessage) error {
	if (state != Published && state != PublicationUnknown && state != Partial && state != Stale) || !json.Valid(outcome) {
		return errors.New("invalid publication outcome")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state=?,outcome=?,updated_at=? WHERE id=? AND payload_id=? AND state=? AND COALESCE(outcome,'')=? AND (state IN ('publishing','publication_unknown','partial') OR (state='published' AND ? IN ('published','stale')))", state, string(outcome), timestamp(), observed.ID, observed.PayloadID, observed.State, string(observed.Outcome), state)
	if err != nil {
		return errors.New("could not persist reconciled publication outcome")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrOutcomeChanged
	}
	return nil
}
func (s *Store) Fail(ctx context.Context, id string, state State, message string) error {
	if state != Failed && state != Cancelled && state != Stale {
		return errors.New("invalid review failure state")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state=?,failure=?,updated_at=? WHERE id=? AND state IN ('queued','reviewing','prepared')", state, message, timestamp(), id)
	return changed(result, err, "could not persist interrupted or stale review")
}

// BeginRepair binds an explicit new decision to one missing finding and records
// its recovery intent transactionally before the application performs a write.
func (s *Store) BeginRepair(ctx context.Context, observed Run, payloadID string, outcome json.RawMessage) (bool, error) {
	if !json.Valid(outcome) {
		return false, errors.New("invalid repair intent")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE runs SET state='publishing',outcome=?,updated_at=? WHERE id=? AND payload_id=? AND state='partial' AND COALESCE(outcome,'')=?", string(outcome), timestamp(), observed.ID, payloadID, string(observed.Outcome))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	attempt, err := newID()
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO publication_attempts(id,run_id,payload_id,started_at) VALUES(?,?,?,?)", attempt, observed.ID, payloadID, timestamp()); err != nil {
		return false, err
	}
	err = tx.Commit()
	return err == nil, err
}
