package store

import (
	"context"
	"encoding/json"
	"errors"
)

func (s *Store) ClaimFeedback(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET state='handling_feedback',updated_at=? WHERE id=? AND state='queued' AND json_extract(record,'$.purpose')='feedback' AND COALESCE(json_extract(record,'$.cancellation_requested'),0)=0 AND EXISTS(SELECT 1 FROM private_owners WHERE id=json_extract(runs.record,'$.owner_id') AND closing=0)`, timestamp(), id)
	if err != nil {
		return false, errors.New("cannot claim queued feedback work")
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// SaveFeedback records the canonical effect journal before/after each attempt.
// It does not grant an effect or advance the lifecycle. The sole owning broker
// remains responsible for target and current-policy checks before remote I/O.
func (s *Store) SaveFeedback(ctx context.Context, id string, outcome json.RawMessage) error {
	if len(outcome) > 8<<20 || !json.Valid(outcome) {
		return errors.New("feedback outcome is invalid or exceeds 8 MiB")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET outcome=?,updated_at=? WHERE id=? AND state='handling_feedback' AND json_extract(record,'$.purpose')='feedback'`, string(outcome), timestamp(), id)
	return changed(result, err, "cannot persist feedback delivery facts")
}

func (s *Store) FinishFeedback(ctx context.Context, id string, state State, outcome json.RawMessage, diagnostic string) error {
	if state != FeedbackDelivered && state != FeedbackPending && state != Partial && state != Failed && state != Cancelled {
		return errors.New("invalid feedback terminal state")
	}
	if len(outcome) > 8<<20 || !json.Valid(outcome) {
		return errors.New("feedback outcome is invalid or exceeds 8 MiB")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET state=?,outcome=?,failure=?,updated_at=? WHERE id=? AND state IN ('queued','handling_feedback') AND json_extract(record,'$.purpose')='feedback'`, state, string(outcome), diagnostic, timestamp(), id)
	return changed(result, err, "cannot persist final feedback delivery state")
}

// ResumeFeedback requires an explicit action and the observed outcome CAS.
// The caller already holds the cross-profile PR modifier lease. A fresh owner
// and fresh agent replace no prior identity; the execution history is retained.
func (s *Store) ResumeFeedback(ctx context.Context, observed Run, owner string) (bool, error) {
	if observed.Purpose != "feedback" || (observed.State != FeedbackPending && observed.State != Partial) || !ownerIDPattern.MatchString(owner) {
		return false, errors.New("only explicit pending/partial feedback work can resume")
	}
	history := append([]json.RawMessage{}, observed.PriorExecutions...)
	if observed.ThreadID != "" {
		previous, _ := json.Marshal(map[string]any{"thread_id": observed.ThreadID, "turn_id": observed.TurnID, "requested_model": observed.RequestedModel, "effective_model": observed.EffectiveModel, "usage": observed.Usage})
		history = append(history, previous)
	}
	raw, _ := json.Marshal(history)
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET state='handling_feedback',thread_id=NULL,turn_id=NULL,record=json_set(record,'$.owner_id',?,'$.owner_closed',json('false'),'$.cancellation_requested',json('false'),'$.prior_executions',json(?)),updated_at=? WHERE id=? AND state=? AND COALESCE(outcome,'')=? AND json_extract(record,'$.purpose')='feedback' AND COALESCE(json_extract(record,'$.owner_closed'),0)=1 AND json_extract(record,'$.owner_id')=? AND COALESCE(thread_id,'')=? AND COALESCE(turn_id,'')=? AND EXISTS(SELECT 1 FROM private_owners WHERE id=? AND closing=0)`, owner, string(raw), timestamp(), observed.ID, observed.State, string(observed.Outcome), observed.OwnerID, observed.ThreadID, observed.TurnID, owner)
	if err != nil {
		return false, errors.New("cannot claim explicit feedback reconciliation")
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
