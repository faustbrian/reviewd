package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
)

// Input exposes scope and delivery facts. Answers deliberately never appear
// in ambient run/inbox/notification JSON; only the live worker can take them.
type Input struct {
	clarification.Request
	RunID         string `json:"run_id"`
	State         string `json:"state"`
	SelectedAt    string `json:"selected_at,omitempty"`
	WrittenAt     string `json:"written_at,omitempty"`
	ClosedAt      string `json:"closed_at,omitempty"`
	Reason        string `json:"reason,omitempty"`
	TextExpiredAt string `json:"text_expired_at,omitempty"`
}

const inputSchema = `CREATE TABLE input_requests (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id),
 record TEXT NOT NULL CHECK(json_valid(record)), state TEXT NOT NULL
 CHECK(state IN ('pending','selected','sending','sent','closed','delivery_unknown')),
 answers TEXT CHECK(answers IS NULL OR json_valid(answers)),
 selected_at TEXT NOT NULL DEFAULT '',written_at TEXT NOT NULL DEFAULT '',closed_at TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT ''
); CREATE INDEX input_run ON input_requests(run_id);`
const inputView = `json_set(record,'$.state',state,'$.selected_at',selected_at,'$.written_at',written_at,'$.closed_at',closed_at,'$.reason',reason)`
const inputAggregate = `(SELECT COALESCE(json_group_array(json(` + inputView + `)),'[]') FROM input_requests WHERE run_id=runs.id)`

func (s *Store) OpenInput(ctx context.Context, runID string, request clarification.Request) error {
	if err := clarification.ValidateRequest(request); err != nil {
		return err
	}
	raw, _ := json.Marshal(Input{Request: request, RunID: runID, State: "pending"})
	result, err := s.db.ExecContext(ctx, `INSERT INTO input_requests(id,run_id,record,state)
 SELECT ?,?,?,'pending' FROM runs WHERE id=? AND state='reviewing'
 AND COALESCE(json_extract(record,'$.cancellation_requested'),0)=0
 AND COALESCE(json_extract(record,'$.superseded'),0)=0
 AND (?='specialist' OR (thread_id=? AND turn_id=?))`, request.ID, runID, string(raw), runID, request.Participant, request.ThreadID, request.TurnID)
	return changed(result, err, "could not persist input for the active review session")
}
func (s *Store) Inputs(ctx context.Context, runID string) ([]Input, error) {
	query := `SELECT ` + inputView + ` FROM input_requests`
	args := []any{}
	if runID != "" {
		query += " WHERE run_id=?"
		args = append(args, runID)
	}
	query += " ORDER BY rowid"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.New("cannot read private clarification history")
	}
	defer rows.Close()
	inputs := []Input{}
	for rows.Next() {
		var raw string
		var input Input
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &input) != nil {
			return nil, errors.New("invalid durable clarification record")
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}
func (s *Store) SelectInput(ctx context.Context, id string, answers clarification.Answers) (Input, error) {
	// Read immutable questions first; the write is the authoritative state/run CAS.
	var raw string
	if s.db.QueryRowContext(ctx, `SELECT `+inputView+` FROM input_requests WHERE id=?`, id).Scan(&raw) != nil {
		return Input{}, errors.New("clarification target not found")
	}
	var input Input
	if json.Unmarshal([]byte(raw), &input) != nil {
		return input, errors.New("invalid clarification target")
	}
	if err := clarification.ValidateAnswers(input.Questions, answers); err != nil {
		return input, err
	}
	answerJSON, _ := json.Marshal(answers)
	selectedAt := timestamp()
	result, err := s.db.ExecContext(ctx, `UPDATE input_requests SET state='selected',answers=?,selected_at=? WHERE id=? AND state='pending'
 AND EXISTS(SELECT 1 FROM runs WHERE runs.id=input_requests.run_id AND runs.state='reviewing'
 AND COALESCE(json_extract(runs.record,'$.cancellation_requested'),0)=0
 AND COALESCE(json_extract(runs.record,'$.superseded'),0)=0)`, string(answerJSON), selectedAt, id)
	if err = changed(result, err, "clarification is no longer pending; inspect its exact target before answering"); err != nil {
		return input, err
	}
	input.State = "selected"
	input.SelectedAt = selectedAt
	return input, nil
}
func (s *Store) WaitInput(ctx context.Context, id string) (clarification.Answers, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Atomically take a selection once. No owner ever replays sending/sent data.
		var raw string
		err := s.db.QueryRowContext(ctx, `UPDATE input_requests SET state='sending' WHERE id=? AND state='selected'
 AND EXISTS(SELECT 1 FROM runs WHERE runs.id=input_requests.run_id AND runs.state='reviewing'
 AND COALESCE(json_extract(runs.record,'$.cancellation_requested'),0)=0
 AND COALESCE(json_extract(runs.record,'$.superseded'),0)=0) RETURNING answers`, id).Scan(&raw)
		if err == nil {
			var a clarification.Answers
			if clarification.Decode([]byte(raw), &a) != nil {
				return a, errors.New("stored answer is invalid")
			}
			return a, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return clarification.Answers{}, errors.New("cannot read selected clarification answer")
		}
		var state string
		if err := s.db.QueryRowContext(ctx, `SELECT state FROM input_requests WHERE id=?`, id).Scan(&state); err != nil {
			return clarification.Answers{}, errors.New("clarification target disappeared")
		}
		if state != "pending" && state != "selected" {
			return clarification.Answers{}, errors.New("clarification is no longer answerable")
		}
		select {
		case <-ctx.Done():
			return clarification.Answers{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Store) InputEvent(ctx context.Context, id, event, reason string) error {
	var query string
	switch event {
	case "written":
		query = `UPDATE input_requests SET state=CASE WHEN state='sending' THEN 'sent' ELSE state END,written_at=? WHERE id=? AND state IN ('sending','closed','delivery_unknown') AND selected_at!='' AND written_at=''`
	case "closed":
		query = `UPDATE input_requests SET state=CASE WHEN state='sending' THEN 'delivery_unknown' ELSE 'closed' END,closed_at=?,reason=? WHERE id=? AND closed_at=''`
	default:
		return errors.New("unknown clarification lifecycle event")
	}
	if event == "written" {
		result, err := s.db.ExecContext(ctx, query, timestamp(), id)
		return changed(result, err, "cannot record clarification transport write")
	}
	_, err := s.db.ExecContext(ctx, query, timestamp(), reason, id)
	if err != nil {
		return errors.New("cannot close durable clarification")
	}
	return nil
}

// Ending a live owner never means approval or permission to replay an answer.
func (s *Store) CloseInputs(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE input_requests SET state=CASE WHEN state IN ('sending','sent') THEN 'delivery_unknown' ELSE 'closed' END,
 closed_at=?,reason='session_ended' WHERE run_id=? AND closed_at=''`, timestamp(), runID)
	return err
}
