package store

import (
	"context"
	"database/sql"
)

// Material discovery invalidates the previous linked evidence in the same
// transaction that replaces its event. Active workers acknowledge exit before
// becoming terminal; publication already claimed by its owner is untouched.
func supersedeItemRuns(ctx context.Context, tx *sql.Tx, repository string, actor, pull int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE runs SET
 record=json_set(record,'$.superseded',json('true')),
 state=CASE WHEN state='reviewing' THEN state ELSE 'stale' END,
 failure='Review boundary or controlling requirements changed; this run does not certify the current pull request.',updated_at=?
 WHERE repository=? AND actor_id=? AND pull_id=? AND state IN ('queued','waiting_checks','reviewing','prepared')
 AND id IN (SELECT json_extract(record,'$.run_id') FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=?)`, timestamp(), repository, actor, pull, repository, actor, pull)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM publication_decisions WHERE run_id IN
 (SELECT id FROM runs WHERE repository=? AND actor_id=? AND pull_id=? AND COALESCE(json_extract(record,'$.superseded'),0)=1 AND state IN ('queued','waiting_checks','reviewing','prepared','stale'))`, repository, actor, pull)
	return err
}
