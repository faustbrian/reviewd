package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Holds are an overlay, never a replacement for an item, decision or run state.
type Snooze struct {
	ItemID  string    `json:"item_id"`
	Until   time.Time `json:"until,omitempty"`
	HeadSHA string    `json:"head_sha,omitempty"`
}
type Controls struct {
	PausedProjects []string `json:"paused_projects"`
	Snoozes        []Snooze `json:"snoozes"`
}

var ErrHeld = errors.New("review item is paused or snoozed")

const deadlineFormat = "2006-01-02T15:04:05.000000000Z"

func (c Controls) Paused(repository string) bool {
	for _, project := range c.PausedProjects {
		if project == repository {
			return true
		}
	}
	return false
}
func (c Controls) Snoozed(id string) bool {
	for _, snooze := range c.Snoozes {
		if snooze.ItemID == id {
			return true
		}
	}
	return false
}
func (c Controls) Holds(item InboxItem) bool { return c.Paused(item.Repository) || c.Snoozed(item.ID) }

func (s *Store) Controls(ctx context.Context, now time.Time) (Controls, error) {
	c := Controls{PausedProjects: []string{}, Snoozes: []Snooze{}}
	rows, err := s.db.QueryContext(ctx, "SELECT repository FROM project_pauses ORDER BY repository")
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var repository string
		if err = rows.Scan(&repository); err != nil {
			break
		}
		c.PausedProjects = append(c.PausedProjects, repository)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return c, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT item_id,until_at,head_sha FROM item_snoozes WHERE until_at='' OR until_at>? ORDER BY item_id", now.UTC().Format(deadlineFormat))
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var value Snooze
		var until string
		if err := rows.Scan(&value.ItemID, &until, &value.HeadSHA); err != nil {
			return c, err
		}
		if until != "" {
			value.Until, err = time.Parse(deadlineFormat, until)
			if err != nil {
				return c, errors.New("invalid item snooze deadline")
			}
		}
		c.Snoozes = append(c.Snoozes, value)
	}
	return c, rows.Err()
}

func (s *Store) PauseProject(ctx context.Context, repository string, paused bool) error {
	// Scope must already be operator-configured; repository preferences cannot
	// create or release a local project pause.
	query := "DELETE FROM project_pauses WHERE repository=?"
	if paused {
		query = "INSERT INTO project_pauses(repository) VALUES(?) ON CONFLICT(repository) DO NOTHING"
	}
	_, err := s.db.ExecContext(ctx, query, repository)
	return err
}

// SnoozeItem accepts exactly one deadline mode and binds head snoozes to the
// current item, not its broader event key. It does not revoke an in-flight POST.
func (s *Store) SnoozeItem(ctx context.Context, id string, until time.Time, untilHead bool, now time.Time) error {
	if untilHead == !until.IsZero() || (!untilHead && !until.After(now)) {
		return errors.New("choose a future snooze deadline or until new commits")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var head, state, runID string
	err = tx.QueryRowContext(ctx, `SELECT json_extract(i.record,'$.head_sha'),json_extract(i.record,'$.state'),COALESCE(json_extract(i.record,'$.run_id'),'') FROM inbox_items i
 JOIN discovery_scopes d ON d.repository=i.repository AND d.actor_id=i.actor_id
 WHERE i.id=? AND json_extract(d.record,'$.state')='ok'
 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.id=json_extract(i.record,'$.run_id') AND r.state='publishing')`, id).Scan(&head, &state, &runID)
	if err != nil || state == "publishing" || state == "closed" || state == "ineligible" {
		return errors.New("current item is unavailable or publication is already in flight; synchronize and inspect its run")
	}
	deadline := ""
	if untilHead {
		if head == "" {
			return errors.New("until new commits requires a known current head")
		}
	} else {
		deadline = until.UTC().Format(deadlineFormat)
		head = ""
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO item_snoozes(item_id,until_at,head_sha) VALUES(?,?,?)
 ON CONFLICT(item_id) DO UPDATE SET until_at=excluded.until_at,head_sha=excluded.head_sha`, id, deadline, head); err != nil {
		return err
	}
	// Older runs may predate the stable category link. Capture the current
	// relationship before discovery can replace this event's RunID.
	if runID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE runs SET record=json_set(record,'$.inbox_item_id',?) WHERE id=?
 AND EXISTS(SELECT 1 FROM inbox_items i WHERE i.id=? AND i.repository=runs.repository AND i.actor_id=runs.actor_id AND i.pull_id=runs.pull_id)`, id, runID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) UnsnoozeItem(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM item_snoozes WHERE item_id=?", id)
	return changed(result, err, "item has no retained snooze; inspect the current inbox")
}

// The scheduler and publisher use this guard inside their existing claims.
// A hold acknowledged before the claim prevents new work or automatic consent.
func held(ctx context.Context, tx *sql.Tx, item InboxItem, now time.Time) (bool, error) {
	var result bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_pauses WHERE repository=?) OR
 EXISTS(SELECT 1 FROM item_snoozes WHERE item_id=? AND (until_at='' OR until_at>?))`, item.Repository, item.ID, now.UTC().Format(deadlineFormat)).Scan(&result)
	return result, err
}
