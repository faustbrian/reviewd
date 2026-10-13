package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type DiscoveryScope struct {
	Repository   string    `json:"repository"`
	RepositoryID int64     `json:"repository_id"`
	ActorID      int64     `json:"actor_id"`
	Revision     int64     `json:"revision"`
	Initialized  bool      `json:"initialized"`
	State        string    `json:"state"`
	Failure      string    `json:"failure,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}
type Observation struct {
	PullID int64           `json:"pull_id"`
	Number int             `json:"number"`
	Record json.RawMessage `json:"record"`
}
type InboxItem struct {
	ID            string    `json:"id"`
	Repository    string    `json:"repository"`
	PullID        int64     `json:"pull_id"`
	Number        int       `json:"number"`
	ActorID       int64     `json:"actor_id"`
	Category      string    `json:"category"`
	Trigger       string    `json:"trigger"`
	State         string    `json:"state"`
	Baseline      bool      `json:"baseline"`
	EventKey      string    `json:"event_key"`
	HeadSHA       string    `json:"head_sha"`
	TargetBaseSHA string    `json:"target_base_sha"`
	ContextKey    string    `json:"context_key"`
	Title         string    `json:"title"`
	URL           string    `json:"url"`
	Action        string    `json:"action"`
	Explanation   string    `json:"explanation"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var ErrDiscoveryChanged = errors.New("another synchronization changed this repository; retry its current observation")

func (s *Store) migrateDiscovery(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS discovery_scopes (
 repository TEXT PRIMARY KEY, revision INTEGER NOT NULL, actor_id INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
);
CREATE TABLE IF NOT EXISTS observations (
 repository TEXT NOT NULL, actor_id INTEGER NOT NULL, pull_id INTEGER NOT NULL,
 number INTEGER NOT NULL, record TEXT NOT NULL CHECK(json_valid(record)),
 PRIMARY KEY(repository,actor_id,pull_id)
);
CREATE TABLE IF NOT EXISTS inbox_items (
 id TEXT PRIMARY KEY, repository TEXT NOT NULL, actor_id INTEGER NOT NULL,
 pull_id INTEGER NOT NULL, category TEXT NOT NULL, record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(repository,actor_id,pull_id,category)
);
CREATE TABLE IF NOT EXISTS follows (
 repository TEXT NOT NULL, actor_id INTEGER NOT NULL, pull_id INTEGER NOT NULL,
 number INTEGER NOT NULL, following INTEGER NOT NULL CHECK(following IN (0,1)), ignored_requests TEXT NOT NULL CHECK(json_valid(ignored_requests)),
 PRIMARY KEY(repository,actor_id,pull_id)
);
PRAGMA user_version=2;`)
	if err != nil {
		return errors.New("cannot migrate durable discovery state")
	}
	return tx.Commit()
}

// ConfigureDiscovery reconciles the complete allowlist and captures acquisition
// revisions in one transaction, before authentication or any other external I/O.
// Historical runs, previews, decisions and remote outcomes are not removed.
func (s *Store) ConfigureDiscovery(ctx context.Context, repositories []string) (map[string]DiscoveryScope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	query := `UPDATE discovery_scopes SET revision=revision+1,
 record=json_set(record,'$.revision',revision+1,'$.state','inactive','$.failure','','$.updated_at',?)
 WHERE json_extract(record,'$.state')!='inactive'`
	args := []any{now.Format(time.RFC3339Nano)}
	if len(repositories) > 0 {
		query += " AND repository NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(repositories)), ",") + ")"
		for _, repository := range repositories {
			args = append(args, repository)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, err
	}
	scopes := make(map[string]DiscoveryScope, len(repositories))
	for _, repository := range repositories {
		initial, _ := json.Marshal(DiscoveryScope{Repository: repository, State: "pending", UpdatedAt: now})
		if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,0,0,?)
 ON CONFLICT(repository) DO UPDATE SET revision=revision+1,
 record=json_set(record,'$.revision',revision+1,'$.state','pending','$.failure','','$.updated_at',?)
 WHERE json_extract(record,'$.state')='inactive'`, repository, string(initial), now.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT record FROM discovery_scopes WHERE repository=?", repository).Scan(&raw); err != nil {
			return nil, err
		}
		var scope DiscoveryScope
		if json.Unmarshal([]byte(raw), &scope) != nil {
			return nil, errors.New("invalid durable discovery scope")
		}
		scopes[repository] = scope
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return scopes, nil
}

// DiscoveryScope captures an acquisition revision. Registering a first scope
// before returning it lets concurrent allowlist revocation invalidate that poll
// even before its first success or failure has been persisted.
func (s *Store) DiscoveryScope(ctx context.Context, repository string) (DiscoveryScope, error) {
	initial := DiscoveryScope{Repository: repository, State: "pending", UpdatedAt: time.Now().UTC()}
	record, _ := json.Marshal(initial)
	var raw string
	err := s.db.QueryRowContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,0,0,?)
 ON CONFLICT(repository) DO NOTHING RETURNING record`, repository, string(record)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, "SELECT record FROM discovery_scopes WHERE repository=?", repository).Scan(&raw)
	}
	var scope DiscoveryScope
	if err != nil {
		return scope, err
	}
	if json.Unmarshal([]byte(raw), &scope) != nil {
		return scope, errors.New("invalid durable discovery scope")
	}
	return scope, nil
}
func (s *Store) Observations(ctx context.Context, repository string, actor int64) (map[int64]Observation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pull_id,number,record FROM observations WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]Observation{}
	for rows.Next() {
		var id int64
		var number int
		var raw string
		if err := rows.Scan(&id, &number, &raw); err != nil {
			return nil, err
		}
		result[id] = Observation{PullID: id, Number: number, Record: json.RawMessage(raw)}
	}
	return result, rows.Err()
}

// ApplyDiscovery commits a complete repository snapshot and baseline atomically.
// The observation revision prevents an older concurrent poll from replacing it.
func (s *Store) ApplyDiscovery(ctx context.Context, observed DiscoveryScope, next DiscoveryScope, rows []Observation, items []InboxItem) error {
	if next.Repository != observed.Repository || next.ActorID <= 0 || next.RepositoryID <= 0 || next.State != "ok" {
		return errors.New("invalid discovery boundary")
	}
	next.Revision = observed.Revision + 1
	next.Initialized = true
	next.UpdatedAt = time.Now().UTC()
	record, _ := json.Marshal(next)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,?,?,?)
 ON CONFLICT(repository) DO UPDATE SET revision=excluded.revision,actor_id=excluded.actor_id,record=excluded.record WHERE discovery_scopes.revision=?`, next.Repository, next.Revision, next.ActorID, string(record), observed.Revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrDiscoveryChanged
	}
	for _, row := range rows {
		if row.PullID <= 0 || row.Number <= 0 || !json.Valid(row.Record) {
			return errors.New("invalid discovery observation")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observations(repository,actor_id,pull_id,number,record) VALUES(?,?,?,?,?)
  ON CONFLICT(repository,actor_id,pull_id) DO UPDATE SET number=excluded.number,record=excluded.record`, next.Repository, next.ActorID, row.PullID, row.Number, string(row.Record)); err != nil {
			return err
		}
	}
	// A successful acquisition is a complete category snapshot for every observed
	// pull. Omitted categories must not retain an earlier actionable decision.
	supplied := map[int64]map[string]bool{}
	for _, item := range items {
		if supplied[item.PullID] == nil {
			supplied[item.PullID] = map[string]bool{}
		}
		supplied[item.PullID][item.Category] = true
	}
	for _, row := range rows {
		stored, err := tx.QueryContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=?", next.Repository, next.ActorID, row.PullID)
		if err != nil {
			return err
		}
		retired := []InboxItem{}
		for stored.Next() {
			var raw string
			if err := stored.Scan(&raw); err != nil {
				stored.Close()
				return err
			}
			var item InboxItem
			if json.Unmarshal([]byte(raw), &item) != nil {
				stored.Close()
				return errors.New("invalid stored inbox item")
			}
			if supplied[row.PullID][item.Category] || item.State == "ineligible" || item.State == "closed" {
				continue
			}
			item.State, item.Trigger, item.Action = "ineligible", "ineligible", ""
			item.EventKey = "retired:" + item.EventKey
			item.Explanation = "This category is no longer present in the complete discovery snapshot."
			item.UpdatedAt = next.UpdatedAt
			retired = append(retired, item)
		}
		if err := stored.Err(); err != nil {
			stored.Close()
			return err
		}
		if err := stored.Close(); err != nil {
			return err
		}
		for _, item := range retired {
			raw, _ := json.Marshal(item)
			if _, err := tx.ExecContext(ctx, "UPDATE inbox_items SET record=? WHERE id=?", string(raw), item.ID); err != nil {
				return err
			}
		}
	}
	for _, item := range items {
		if item.Repository != next.Repository || item.ActorID != next.ActorID || item.PullID <= 0 || item.Number <= 0 || item.Category == "" || item.EventKey == "" {
			return errors.New("invalid inbox boundary")
		}
		var previous string
		err := tx.QueryRowContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=? AND category=?", item.Repository, item.ActorID, item.PullID, item.Category).Scan(&previous)
		if err == nil {
			var old InboxItem
			if json.Unmarshal([]byte(previous), &old) != nil {
				return errors.New("invalid stored inbox item")
			}
			item.ID = old.ID
			if old.EventKey == item.EventKey && old.Action == item.Action && old.Trigger == item.Trigger {
				continue
			}
			if old.EventKey == item.EventKey && old.State == "skipped" {
				item.State = old.State
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			item.ID, err = newID()
			if err != nil {
				return err
			}
		} else {
			return err
		}
		item.UpdatedAt = next.UpdatedAt
		raw, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_items(id,repository,actor_id,pull_id,category,record) VALUES(?,?,?,?,?,?)
  ON CONFLICT(repository,actor_id,pull_id,category) DO UPDATE SET record=excluded.record`, item.ID, item.Repository, item.ActorID, item.PullID, item.Category, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) DiscoveryFailure(ctx context.Context, observed DiscoveryScope, message string) error {
	next := observed
	next.State = "failed"
	next.Failure = message
	next.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(next)
	result, err := s.db.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,?,?,?)
 ON CONFLICT(repository) DO UPDATE SET record=excluded.record WHERE discovery_scopes.revision=?`, next.Repository, next.Revision, next.ActorID, string(raw), observed.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDiscoveryChanged
	}
	return nil
}
func (s *Store) Inbox(ctx context.Context) ([]InboxItem, []DiscoveryScope, error) {
	scopes := []DiscoveryScope{}
	items := []InboxItem{}
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM discovery_scopes ORDER BY repository")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var scope DiscoveryScope
		if json.Unmarshal([]byte(raw), &scope) != nil {
			err = errors.New("invalid discovery status")
			break
		}
		scopes = append(scopes, scope)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT i.record FROM inbox_items i JOIN discovery_scopes s ON i.repository=s.repository AND i.actor_id=s.actor_id WHERE json_extract(s.record,'$.state')!='inactive' AND json_extract(i.record,'$.state') NOT IN ('ignored','closed','skipped','following','ineligible') ORDER BY i.repository,i.pull_id,i.category")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, nil, err
		}
		var item InboxItem
		if json.Unmarshal([]byte(raw), &item) != nil {
			return nil, nil, errors.New("invalid inbox record")
		}
		items = append(items, item)
	}
	return items, scopes, rows.Err()
}

type FollowState struct {
	PullID          int64
	Number          int
	Following       bool
	IgnoredRequests []string
}

// DiscoveryItems includes hidden records so current operator policy can be
// reconciled against the original trigger without inventing a new PR event.
func (s *Store) DiscoveryItems(ctx context.Context, repository string, actor int64) ([]InboxItem, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []InboxItem{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item InboxItem
		if json.Unmarshal([]byte(raw), &item) != nil {
			return nil, errors.New("invalid stored inbox item")
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) FollowStates(ctx context.Context, repository string, actor int64) (map[int64]FollowState, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pull_id,number,following,ignored_requests FROM follows WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]FollowState{}
	for rows.Next() {
		var row FollowState
		var following int
		var raw string
		if err := rows.Scan(&row.PullID, &row.Number, &following, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &row.IgnoredRequests) != nil {
			return nil, errors.New("invalid follow request history")
		}
		row.Following = following == 1
		result[row.PullID] = row
	}
	return result, rows.Err()
}
func (s *Store) SetFollow(ctx context.Context, id string, following bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	flag := 0
	if following {
		flag = 1
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO follows(repository,actor_id,pull_id,number,following,ignored_requests)
 SELECT i.repository,i.actor_id,i.pull_id,CAST(json_extract(i.record,'$.number') AS INTEGER),?,
 COALESCE(json_extract(o.record,'$.requests'),'[]')
 FROM inbox_items i JOIN observations o ON i.repository=o.repository AND i.actor_id=o.actor_id AND i.pull_id=o.pull_id
 JOIN discovery_scopes s ON s.repository=i.repository AND s.actor_id=i.actor_id AND json_extract(s.record,'$.state')!='inactive' WHERE i.id=?
 ON CONFLICT(repository,actor_id,pull_id) DO UPDATE SET following=excluded.following,ignored_requests=excluded.ignored_requests`, flag, id)
	if err = changed(result, err, "item is unavailable or belongs to another synchronization identity"); err != nil {
		return err
	}
	if !following {
		if _, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state','skipped')
 WHERE category='reviews_owed' AND (repository,actor_id,pull_id)=
 (SELECT repository,actor_id,pull_id FROM inbox_items WHERE id=?)`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE discovery_scopes SET revision=revision+1,record=json_set(record,'$.revision',revision+1) WHERE repository=(SELECT repository FROM inbox_items WHERE id=?)", id); err != nil {
		return err
	}
	return tx.Commit()
}
