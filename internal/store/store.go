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
	HandlingFeedback   State = "handling_feedback"
	FeedbackPending    State = "feedback_pending"
	FeedbackDelivered  State = "feedback_delivered"
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
	Purpose               string            `json:"purpose,omitempty"`
	PriorExecutions       []json.RawMessage `json:"prior_executions,omitempty"`
	OwnerID               string            `json:"owner_id,omitempty"`
	OwnerClosed           bool              `json:"owner_closed,omitempty"`
	Archive               *Archive          `json:"archive,omitempty"`
	Inputs                []Input           `json:"inputs,omitempty"`
	ID                    string            `json:"id"`
	InboxItemID           string            `json:"inbox_item_id,omitempty"`
	CancellationRequested bool              `json:"cancellation_requested,omitempty"`
	Superseded            bool              `json:"superseded,omitempty"`
	Repository            string            `json:"repository"`
	RepositoryID          int64             `json:"repository_id"`
	PullID                int64             `json:"pull_id"`
	Number                int               `json:"number"`
	ActorID               int64             `json:"actor_id"`
	ActorLogin            string            `json:"actor_login"`
	WorkKey               string            `json:"work_key"`
	State                 State             `json:"state"`
	Private               bool              `json:"private"`
	Configuration         json.RawMessage   `json:"configuration"`
	Context               json.RawMessage   `json:"context,omitempty"`
	SkillDigest           string            `json:"skill_digest,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
	ThreadID              string            `json:"thread_id,omitempty"`
	TurnID                string            `json:"turn_id,omitempty"`
	PayloadID             string            `json:"payload_id,omitempty"`
	Prepared              json.RawMessage   `json:"prepared,omitempty"`
	Outcome               json.RawMessage   `json:"outcome,omitempty"`
	Failure               string            `json:"failure,omitempty"`
	RequestedModel        string            `json:"requested_model,omitempty"`
	EffectiveModel        string            `json:"effective_model,omitempty"`
	Usage                 json.RawMessage   `json:"usage,omitempty"`
	Specialists           json.RawMessage   `json:"specialists,omitempty"`
	Progress              string            `json:"progress,omitempty"`
}
type Store struct {
	db        *sql.DB
	directory string
}

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
	store = &Store{db: db, directory: directory}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	if err = migrate(ctx, db); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) WaitForChecks(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='waiting_checks',updated_at=? WHERE id=? AND state='queued' AND COALESCE(json_extract(record,'$.purpose'),'review')!='feedback'", timestamp(), id)
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
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET thread_id=?,turn_id=?,record=json_set(record,'$.requested_model',?,'$.effective_model',?,'$.usage',json(?),'$.specialists',json(?)),updated_at=? WHERE id=? AND state IN ('reviewing','prepared','handling_feedback')", thread, turn, requested, effective, string(usage), string(specialists), timestamp(), id)
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
	return s.create(ctx, run, nil)
}

// CreateForItem atomically binds a run to the currently authorized inbox event.
// Neither a stale decision nor a superseded snapshot can start new work.
func (s *Store) CreateForItem(ctx context.Context, run Run, item InboxItem) (Run, bool, error) {
	return s.create(ctx, run, &item)
}

func (s *Store) create(ctx context.Context, run Run, item *InboxItem) (Run, bool, error) {
	if run.Repository == "" || run.RepositoryID <= 0 || run.PullID <= 0 || run.Number < 1 || run.ActorID <= 0 || run.ActorLogin == "" || run.WorkKey == "" || (run.Purpose != "" && run.Purpose != "review" && run.Purpose != "feedback") || run.State != Queued || !json.Valid(run.Configuration) || (len(run.Context) > 0 && !json.Valid(run.Context)) {
		return Run{}, false, errors.New("run requires complete scope, frozen policy and queued state")
	}
	var err error
	run.ID, err = newID()
	if err != nil {
		return Run{}, false, err
	}
	run.CreatedAt = time.Now().UTC()
	run.UpdatedAt = run.CreatedAt
	run.InboxItemID = ""
	if item != nil {
		run.InboxItemID = item.ID
	}
	data, err := json.Marshal(run)
	if err != nil {
		return Run{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback()
	var current InboxItem
	if item != nil {
		if hold, err := held(ctx, tx, *item, run.CreatedAt); err != nil {
			return Run{}, false, err
		} else if hold {
			return Run{}, false, ErrHeld
		}
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT i.record FROM inbox_items i JOIN discovery_scopes d ON d.repository=i.repository AND d.actor_id=i.actor_id
 WHERE i.id=? AND json_extract(d.record,'$.state')='ok' AND json_extract(d.record,'$.repository_id')=?`, item.ID, run.RepositoryID).Scan(&raw)
		if err != nil || json.Unmarshal([]byte(raw), &current) != nil {
			return Run{}, false, errors.New("current review item is unavailable")
		}
		ready := current.State == "queued" || (current.State == "retry_wait" && current.Attempts > 0 && current.Attempts < 3 && !current.RetryAt.IsZero() && !run.CreatedAt.Before(current.RetryAt))
		if current.Repository != run.Repository || current.ActorID != run.ActorID || current.PullID != run.PullID || current.EventKey != item.EventKey || current.DecisionID != item.DecisionID || current.HeadSHA != item.HeadSHA || current.TargetBaseSHA != item.TargetBaseSHA || current.ContextKey != item.ContextKey || !ready {
			return Run{}, false, errors.New("review item eligibility or decision changed; synchronize and choose its current event")
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO runs(id,repository,repository_id,pull_id,actor_id,work_key,state,record,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(repository,repository_id,pull_id,actor_id,work_key) DO NOTHING`, run.ID, run.Repository, run.RepositoryID, run.PullID, run.ActorID, run.WorkKey, run.State, string(data), run.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Run{}, false, errors.New("could not persist queued review")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Run{}, false, err
	}
	if count == 1 {
		if item != nil {
			current.RunID = run.ID
			current.State = "reviewing"
			if run.Purpose == "feedback" {
				current.State = "handling_feedback"
			}
			current.UpdatedAt = run.UpdatedAt
			raw, _ := json.Marshal(current)
			if _, err := tx.ExecContext(ctx, "UPDATE inbox_items SET record=? WHERE id=?", string(raw), current.ID); err != nil {
				return Run{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return Run{}, false, err
		}
		return run, true, nil
	}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT id FROM runs WHERE repository=? AND repository_id=? AND pull_id=? AND actor_id=? AND work_key=?", run.Repository, run.RepositoryID, run.PullID, run.ActorID, run.WorkKey).Scan(&existing)
	if err != nil {
		return Run{}, false, errors.New("could not reconcile existing review work")
	}
	run, err = readRun(tx.QueryRowContext(ctx, selectRun+" WHERE id=?", existing))
	if err == nil {
		err = tx.Commit()
	}
	return run, false, err
}

const selectRun = `SELECT record,state,updated_at,prepared,payload_id,thread_id,turn_id,outcome,failure,` + inputAggregate + ` FROM runs`

type rowScanner interface{ Scan(...any) error }

func readRun(row rowScanner) (Run, error) {
	var data, state, updated, inputs string
	var prepared, payload, thread, turn, outcome, failure sql.NullString
	if err := row.Scan(&data, &state, &updated, &prepared, &payload, &thread, &turn, &outcome, &failure, &inputs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, errors.New("review run was not found in the selected profile")
		}
		return Run{}, errors.New("could not read durable review state")
	}
	var run Run
	if json.Unmarshal([]byte(data), &run) != nil {
		return Run{}, errors.New("durable review record is invalid; preserve the database for recovery")
	}
	if json.Unmarshal([]byte(inputs), &run.Inputs) != nil {
		return Run{}, errors.New("invalid clarification history")
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
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='reviewing',updated_at=? WHERE id=? AND state='queued' AND COALESCE(json_extract(record,'$.purpose'),'review')!='feedback'", timestamp(), id)
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
	result, err := s.db.ExecContext(ctx, "UPDATE runs SET state='prepared',prepared=?,payload_id=?,thread_id=?,turn_id=?,updated_at=? WHERE id=? AND state='reviewing' AND COALESCE(json_extract(record,'$.cancellation_requested'),0)=0 AND COALESCE(json_extract(record,'$.superseded'),0)=0", string(payload), payloadID, thread, turn, timestamp(), id)
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
	return s.beginPublication(ctx, id, payloadID, nil, initial...)
}

// BeginItemPublication binds automatic consent and its attempt to the current
// event in the same transaction. No prior eligibility read authorizes a write.
func (s *Store) BeginItemPublication(ctx context.Context, id, payloadID string, item InboxItem, initial json.RawMessage) (string, bool, error) {
	return s.beginPublication(ctx, id, payloadID, &item, initial)
}

func (s *Store) beginPublication(ctx context.Context, id, payloadID string, item *InboxItem, initial ...json.RawMessage) (string, bool, error) {
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
	if item != nil {
		if hold, err := held(ctx, tx, *item, time.Now()); err != nil {
			return "", false, err
		} else if hold {
			return "", false, ErrHeld
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT i.record FROM inbox_items i
 JOIN discovery_scopes d ON d.repository=i.repository AND d.actor_id=i.actor_id
 JOIN runs r ON r.id=? AND r.repository=i.repository AND r.actor_id=i.actor_id AND r.pull_id=i.pull_id
 WHERE i.id=? AND r.state='prepared' AND r.payload_id=? AND json_extract(r.record,'$.private')=0
 AND json_extract(d.record,'$.state')='ok' AND json_extract(d.record,'$.repository_id')=r.repository_id
 AND json_extract(i.record,'$.run_id')=r.id`, id, item.ID, payloadID).Scan(&raw)
		var current InboxItem
		if err != nil || json.Unmarshal([]byte(raw), &current) != nil || current.State != "prepared" || current.EventKey != item.EventKey || current.DecisionID != item.DecisionID || current.Repository != item.Repository || current.ActorID != item.ActorID || current.PullID != item.PullID || current.HeadSHA != item.HeadSHA || current.TargetBaseSHA != item.TargetBaseSHA || current.ContextKey != item.ContextKey {
			return "", false, errors.New("review event or decision changed before publication; no attempt was recorded")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO publication_decisions(run_id,payload_id,decided_at) SELECT id,payload_id,? FROM runs WHERE id=? AND payload_id=? AND state='prepared' ON CONFLICT(run_id,payload_id) DO UPDATE SET decided_at=excluded.decided_at`, timestamp(), id, payloadID)
		if err = changed(result, err, "could not bind automatic publication consent"); err != nil {
			return "", false, err
		}
	}
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
	if item != nil {
		result, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state','publishing','$.updated_at',?) WHERE id=? AND json_extract(record,'$.run_id')=? AND json_extract(record,'$.state')='prepared'`, timestamp(), item.ID, id)
		if err = changed(result, err, "could not record in-flight inbox publication"); err != nil {
			return "", false, err
		}
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
	// Resolve accepted local stops in the terminal write, not an earlier read.
	// A user's explicit cancellation takes precedence over supersession.
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET
 state=CASE WHEN COALESCE(json_extract(record,'$.cancellation_requested'),0)=1 THEN 'cancelled'
 WHEN COALESCE(json_extract(record,'$.superseded'),0)=1 THEN 'stale' ELSE ? END,
 failure=?,updated_at=? WHERE id=? AND (state IN ('queued','reviewing','prepared')
 OR (state='waiting_checks' AND ?='stale') OR (state='cancelled' AND ?='cancelled'))`, state, message, timestamp(), id, state, state)
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
