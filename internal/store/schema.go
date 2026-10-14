package store

import (
	"context"
	"database/sql"
	"errors"
)

// One reserved connection owns the complete upgrade. Read the version only
// after the write lock: another client may have upgraded while we waited.
func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return errors.New("cannot lock state schema for migration")
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return errors.New("cannot read state database schema")
	}
	if version > 7 {
		return errors.New("state database uses a newer schema; upgrade reviewd without replacing the database")
	}
	schemas := []string{runSchema, discoverySchema, notificationSchema, controlsSchema, inputSchema, ownerSchema, cooldownSchema}
	for index := version; index < len(schemas); index++ {
		if _, err := conn.ExecContext(ctx, schemas[index]); err != nil {
			return errors.New("cannot migrate durable state schema")
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA user_version=7"); err != nil {
		return errors.New("cannot record state schema version")
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return errors.New("cannot commit state schema")
	}
	return nil
}

const runSchema = `CREATE TABLE IF NOT EXISTS runs (
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
`

const discoverySchema = `CREATE TABLE IF NOT EXISTS discovery_scopes (
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
`

const notificationSchema = `CREATE TABLE IF NOT EXISTS notification_sources (
 source TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, pending INTEGER NOT NULL CHECK(pending IN (0,1)),
 record TEXT NOT NULL CHECK(json_valid(record))
); `

const controlsSchema = `ALTER TABLE observations ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
CREATE TABLE project_pauses (repository TEXT PRIMARY KEY);
CREATE TABLE item_snoozes (
 item_id TEXT PRIMARY KEY REFERENCES inbox_items(id), until_at TEXT NOT NULL,
 head_sha TEXT NOT NULL, CHECK((until_at='' AND head_sha!='') OR (until_at!='' AND head_sha=''))
);
UPDATE runs SET record=json_set(record,'$.inbox_item_id',
 (SELECT i.id FROM inbox_items i WHERE json_extract(i.record,'$.run_id')=runs.id
 AND i.repository=runs.repository AND i.actor_id=runs.actor_id AND i.pull_id=runs.pull_id))
WHERE EXISTS(SELECT 1 FROM inbox_items i WHERE json_extract(i.record,'$.run_id')=runs.id
 AND i.repository=runs.repository AND i.actor_id=runs.actor_id AND i.pull_id=runs.pull_id);`
