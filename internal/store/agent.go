package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/faustbrian/reviewd/internal/activity"
)

// ClaimAgent cannot reinterpret a historical private-review or feedback broker
// run. Only ordinary native executions carry these discriminators.
func (s *Store) ClaimAgent(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET state='running',updated_at=? WHERE id=? AND state='queued'
 AND json_extract(record,'$.executor') IN (?,?)
 AND COALESCE(json_extract(record,'$.cancellation_requested'),0)=0
 AND COALESCE(json_extract(record,'$.superseded'),0)=0`, timestamp(), id, NativeExecutor, ClaudeExecutor)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) RecordAgent(ctx context.Context, id, thread, model string, usage json.RawMessage, preview ...string) error {
	if !executionIdentifier(thread) || (model != "" && !executionIdentifier(model)) {
		return errors.New("normal agent progress requires session and requested model identity")
	}
	if len(usage) == 0 {
		usage = json.RawMessage(`null`)
	}
	if !json.Valid(usage) {
		return errors.New("normal agent usage is invalid JSON")
	}
	text := ""
	if len(preview) > 0 {
		text = activity.Clean(preview[0])
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET thread_id=?,
 record=json_set(record,'$.requested_model',?,'$.usage',json(?),
 '$.progress_at',CASE WHEN ?!='' AND ?!=COALESCE(json_extract(record,'$.progress'),'') THEN ? ELSE COALESCE(json_extract(record,'$.progress_at'),'0001-01-01T00:00:00Z') END,
 '$.progress',CASE WHEN ?='' THEN COALESCE(json_extract(record,'$.progress'),'') ELSE ? END),updated_at=?
 WHERE id=? AND state='running' AND json_extract(record,'$.executor') IN (?,?)
 AND (?!='' OR json_extract(record,'$.executor')=?)
 AND (COALESCE(thread_id,'')='' OR thread_id=?)`, thread, model, string(usage), text, text, timestamp(), text, text, timestamp(), id, NativeExecutor, ClaudeExecutor, model, ClaudeExecutor, thread)
	return changed(result, err, "could not record normal agent progress")
}

// A completed process is deliberately not Published or FeedbackDelivered. The
// selected skill owns those operations; reviewd retains its native output only.
func (s *Store) FinishAgent(ctx context.Context, id string, state State, outcome json.RawMessage, diagnostic string) error {
	if state != AgentCompleted && state != AgentInterrupted && state != Failed && state != Cancelled {
		return errors.New("invalid normal agent terminal state")
	}
	if !json.Valid(outcome) {
		return errors.New("normal agent outcome is invalid JSON")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET state=?,outcome=?,failure=?,updated_at=?
 WHERE id=? AND state IN ('queued','running') AND json_extract(record,'$.executor') IN (?,?)`, state, string(outcome), diagnostic, timestamp(), id, NativeExecutor, ClaudeExecutor)
	return changed(result, err, "could not record normal agent outcome")
}
