package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) ItemFailure(ctx context.Context, item InboxItem, message string, now time.Time, retryAfter ...time.Duration) error {
	if message == "" {
		return errors.New("failure requires a diagnostic")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT record FROM inbox_items WHERE id=?
 AND json_extract(record,'$.event_key')=? AND COALESCE(json_extract(record,'$.decision_id'),'')=?
 AND COALESCE(json_extract(record,'$.attempts'),0)=?
 AND json_extract(record,'$.state') IN ('queued','retry_wait','waiting_checks','prepared')`, item.ID, item.EventKey, item.DecisionID, item.Attempts).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var current InboxItem
	if json.Unmarshal([]byte(raw), &current) != nil {
		return errors.New("invalid current retry item")
	}
	if item.RunID != "" && current.RunID != item.RunID {
		return nil
	}
	item = current
	item.Attempts++
	item.Failure = message
	if item.State != "prepared" {
		item.State = "retry_wait"
	}
	item.RetryAt = now.Add(time.Duration(item.Attempts) * 30 * time.Second)
	if len(retryAfter) > 0 && retryAfter[0] > item.RetryAt.Sub(now) {
		item.RetryAt = now.Add(retryAfter[0])
	}
	if item.Attempts >= 3 {
		item.State = "failed"
	}
	item.UpdatedAt = now.UTC()
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE inbox_items SET record=? WHERE id=?", string(data), item.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearItemFailure resets consecutive read failures only after the complete
// operation succeeds; a successful refresh alone cannot reset a failed publish.
func (s *Store) ClearItemFailure(ctx context.Context, item InboxItem, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_items SET record=json_remove(record,'$.attempts','$.retry_at','$.failure')
 WHERE id=? AND json_extract(record,'$.event_key')=? AND COALESCE(json_extract(record,'$.decision_id'),'')=?
 AND json_extract(record,'$.run_id')=? AND json_extract(record,'$.state') IN ('waiting_checks','prepared','published')`, item.ID, item.EventKey, item.DecisionID, runID)
	return err
}

func (s *Store) RecoverScheduled(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A scheduler link is not liveness evidence: an independent manual start
	// client may own it. RecoverPrivate classifies owner loss using kernel leases.
	_, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state',
 (SELECT r.state FROM runs r WHERE r.id=json_extract(inbox_items.record,'$.run_id')))
 WHERE json_extract(record,'$.state') IN ('reviewing','waiting_checks','prepared','publishing','published','publication_unknown','partial','failed','cancelled','stale')
 AND EXISTS(SELECT 1 FROM runs r WHERE r.id=json_extract(inbox_items.record,'$.run_id')
 AND r.repository=inbox_items.repository AND r.actor_id=inbox_items.actor_id AND r.pull_id=inbox_items.pull_id
 AND NOT (json_extract(inbox_items.record,'$.state')='failed' AND COALESCE(json_extract(inbox_items.record,'$.attempts'),0)>=3 AND r.state IN ('prepared','waiting_checks')))`)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE input_requests SET state=CASE WHEN state IN ('sending','sent') THEN 'delivery_unknown' ELSE 'closed' END,
 closed_at=?,reason='owner_lost' WHERE closed_at='' AND EXISTS(SELECT 1 FROM runs WHERE runs.id=input_requests.run_id AND runs.state!='reviewing')`, timestamp())
	if err != nil {
		return err
	}
	return tx.Commit()
}
