package store

import (
	"context"
	"errors"
)

// RequestCancellation is local and run-scoped. Queued/waiting work has no
// worker to stop; active work remains reviewing until its owner confirms exit.
func (s *Store) RequestCancellation(ctx context.Context, id string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET
 record=json_set(record,'$.cancellation_requested',json('true')),
 state=CASE WHEN state IN ('queued','waiting_checks') THEN 'cancelled' ELSE state END,
 failure=CASE WHEN state IN ('queued','waiting_checks') THEN 'Cancelled before an agent started.' ELSE failure END,
 updated_at=? WHERE id=? AND state IN ('queued','waiting_checks','reviewing','handling_feedback','running','cancelled')`, timestamp(), id)
	if err != nil {
		return Run{}, errors.New("cannot record scoped cancellation request")
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Run{}, err
	}
	if count != 1 {
		current, readErr := readRun(tx.QueryRowContext(ctx, selectRun+" WHERE id=?", id))
		if readErr != nil {
			return Run{}, readErr
		}
		return Run{}, errors.New("run is " + string(current.State) + "; only queued, waiting or reviewing runs can be cancelled; inspect its existing result with reviewd show")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state','cancelled','$.updated_at',?)
 WHERE json_extract(record,'$.run_id')=? AND json_extract(record,'$.state') NOT IN ('ignored','skipped','closed','ineligible')
 AND EXISTS(SELECT 1 FROM runs WHERE id=? AND state='cancelled')`, timestamp(), id, id); err != nil {
		return Run{}, err
	}
	run, err := readRun(tx.QueryRowContext(ctx, selectRun+" WHERE id=?", id))
	if err != nil {
		return Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) CancellationRequested(ctx context.Context, id string) (bool, error) {
	var requested bool
	err := s.db.QueryRowContext(ctx, `SELECT state='cancelled' OR COALESCE(json_extract(record,'$.cancellation_requested'),0)=1 FROM runs WHERE id=?`, id).Scan(&requested)
	return requested, err
}

// StopRequested is the single durable local-control observation used by private
// workers. Supersession is not a user cancellation and retains its own cause.
func (s *Store) StopRequested(ctx context.Context, id string) (bool, error) {
	var requested bool
	err := s.db.QueryRowContext(ctx, `SELECT state IN ('cancelled','stale')
 OR COALESCE(json_extract(record,'$.cancellation_requested'),0)=1
 OR COALESCE(json_extract(record,'$.superseded'),0)=1 FROM runs WHERE id=?`, id).Scan(&requested)
	return requested, err
}
