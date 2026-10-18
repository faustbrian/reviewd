-- Historical schema 9 fixture exported from 722ec49b5cc02e0c5d96037dd6f98f71133b9f59.
BEGIN TRANSACTION;
CREATE TABLE discovery_scopes (
 repository TEXT PRIMARY KEY, revision INTEGER NOT NULL, actor_id INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
);
INSERT INTO "discovery_scopes" VALUES('github.com/owner/repo',1,43,'{"repository": "github.com/owner/repo", "repository_id": 99, "actor_id": 43, "revision": 1, "initialized": true, "state": "ok", "updated_at": "2026-10-17T00:00:00Z"}');
CREATE TABLE follows (
 repository TEXT NOT NULL, actor_id INTEGER NOT NULL, pull_id INTEGER NOT NULL,
 number INTEGER NOT NULL, following INTEGER NOT NULL CHECK(following IN (0,1)), ignored_requests TEXT NOT NULL CHECK(json_valid(ignored_requests)),
 PRIMARY KEY(repository,actor_id,pull_id)
);
CREATE TABLE github_cooldowns (
 host TEXT PRIMARY KEY, until_at TEXT NOT NULL
);
CREATE TABLE inbox_items (
 id TEXT PRIMARY KEY, repository TEXT NOT NULL, actor_id INTEGER NOT NULL,
 pull_id INTEGER NOT NULL, category TEXT NOT NULL, record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(repository,actor_id,pull_id,category)
);
INSERT INTO "inbox_items" VALUES('cccccccccccccccccccccccccccccccc','github.com/owner/repo',43,900,'reviews_owed','{"id": "cccccccccccccccccccccccccccccccc", "repository": "github.com/owner/repo", "pull_id": 900, "number": 27, "actor_id": 43, "category": "reviews_owed", "trigger": "review_requested", "action": "auto", "state": "queued", "manual": true, "decision_id": "legacy-private-consent", "baseline": false, "event_key": "legacy-event", "head_sha": "dddddddddddddddddddddddddddddddddddddddd", "target_base_sha": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "context_key": "old-context", "updated_at": "2026-10-17T00:00:00Z"}');
CREATE TABLE input_requests (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id),
 record TEXT NOT NULL CHECK(json_valid(record)), state TEXT NOT NULL
 CHECK(state IN ('pending','selected','sending','sent','closed','delivery_unknown')),
 answers TEXT CHECK(answers IS NULL OR json_valid(answers)),
 selected_at TEXT NOT NULL DEFAULT '',written_at TEXT NOT NULL DEFAULT '',closed_at TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE item_snoozes (
 item_id TEXT PRIMARY KEY REFERENCES inbox_items(id), until_at TEXT NOT NULL,
 head_sha TEXT NOT NULL, CHECK((until_at='' AND head_sha!='') OR (until_at!='' AND head_sha=''))
);
CREATE TABLE notification_sources (
 source TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, pending INTEGER NOT NULL CHECK(pending IN (0,1)),
 record TEXT NOT NULL CHECK(json_valid(record))
);
INSERT INTO "notification_sources" VALUES('legacy-notification','already-observed',0,'{"source": "legacy-notification", "fingerprint": "already-observed", "repository": "github.com/owner/repo", "actor_id": 43, "pull_id": 703, "number": 20, "body": "Historical notification", "pending": false, "initial": false, "digest": false, "due_at": "2026-10-17T00:00:00Z"}');
CREATE TABLE observations (
 repository TEXT NOT NULL, actor_id INTEGER NOT NULL, pull_id INTEGER NOT NULL,
 number INTEGER NOT NULL, record TEXT NOT NULL CHECK(json_valid(record)), head_sha TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(repository,actor_id,pull_id)
);
CREATE TABLE private_owners (
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, root_identity TEXT NOT NULL, lease_identity TEXT NOT NULL,
 checked_at TEXT NOT NULL DEFAULT '', closing INTEGER NOT NULL DEFAULT 0 CHECK(closing IN (0,1)),
 processes TEXT NOT NULL CHECK(json_valid(processes) AND json_type(processes)='object')
);
INSERT INTO "private_owners" VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','2026-10-17T00:00:00Z','legacy-root-identity','legacy-lease-identity','',0,'{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": {"kind": "feedback_workspace", "owner_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "run_id": "00000000000000000000000000000005"}}');
CREATE TABLE project_pauses (repository TEXT PRIMARY KEY);
CREATE TABLE publication_attempts (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), payload_id TEXT NOT NULL,
 started_at TEXT NOT NULL
);
CREATE TABLE publication_decisions (
 run_id TEXT NOT NULL REFERENCES runs(id), payload_id TEXT NOT NULL,
 decided_at TEXT NOT NULL, PRIMARY KEY(run_id,payload_id)
);
CREATE TABLE "runs" (
 id TEXT PRIMARY KEY, repository TEXT NOT NULL, repository_id INTEGER NOT NULL,
 pull_id INTEGER NOT NULL, actor_id INTEGER NOT NULL, work_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','waiting_checks','reviewing','prepared','publishing','published','publication_unknown','partial','failed','cancelled','stale','handling_feedback','feedback_pending','feedback_delivered')),
 record TEXT NOT NULL CHECK(json_valid(record)), updated_at TEXT NOT NULL,
 prepared TEXT CHECK(prepared IS NULL OR json_valid(prepared)), payload_id TEXT,
 thread_id TEXT, turn_id TEXT, outcome TEXT CHECK(outcome IS NULL OR json_valid(outcome)), failure TEXT,
 UNIQUE(repository,repository_id,pull_id,actor_id,work_key)
);
INSERT INTO "runs" VALUES('00000000000000000000000000000001','github.com/owner/repo',99,701,43,'legacy-queued','queued','{"id":"00000000000000000000000000000001","purpose":"review","repository":"github.com/owner/repo","repository_id":99,"pull_id":701,"number":18,"actor_id":43,"actor_login":"operator","work_key":"legacy-queued","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}','2026-10-17T00:00:00Z',NULL,NULL,'legacy-thread-1','legacy-turn-1',NULL,NULL);
INSERT INTO "runs" VALUES('00000000000000000000000000000002','github.com/owner/repo',99,702,43,'legacy-prepared','prepared','{"id":"00000000000000000000000000000002","purpose":"review","repository":"github.com/owner/repo","repository_id":99,"pull_id":702,"number":19,"actor_id":43,"actor_login":"operator","work_key":"legacy-prepared","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}','2026-10-17T00:00:00Z','{"historical": "prepared"}','legacy-payload','legacy-thread-2','legacy-turn-2',NULL,NULL);
INSERT INTO "runs" VALUES('00000000000000000000000000000003','github.com/owner/repo',99,703,43,'legacy-published','published','{"id":"00000000000000000000000000000003","purpose":"review","repository":"github.com/owner/repo","repository_id":99,"pull_id":703,"number":20,"actor_id":43,"actor_login":"operator","work_key":"legacy-published","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}','2026-10-17T00:00:00Z',NULL,'legacy-payload','legacy-thread-3','legacy-turn-3','{"historical": "published", "verified": true}',NULL);
INSERT INTO "runs" VALUES('00000000000000000000000000000004','github.com/owner/repo',99,704,43,'legacy-publication_unknown','publication_unknown','{"id":"00000000000000000000000000000004","purpose":"review","repository":"github.com/owner/repo","repository_id":99,"pull_id":704,"number":21,"actor_id":43,"actor_login":"operator","work_key":"legacy-publication_unknown","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}','2026-10-17T00:00:00Z',NULL,'legacy-payload','legacy-thread-4','legacy-turn-4','{"historical": "publication_unknown", "verified": false}',NULL);
INSERT INTO "runs" VALUES('00000000000000000000000000000005','github.com/owner/repo',99,705,43,'legacy-feedback_pending','feedback_pending','{"id":"00000000000000000000000000000005","purpose":"feedback","repository":"github.com/owner/repo","repository_id":99,"pull_id":705,"number":22,"actor_id":43,"actor_login":"operator","work_key":"legacy-feedback_pending","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z","owner_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}','2026-10-17T00:00:00Z',NULL,NULL,'legacy-thread-5','legacy-turn-5','{"historical": "feedback_pending", "verified": false}',NULL);
INSERT INTO "runs" VALUES('00000000000000000000000000000006','github.com/owner/repo',99,706,43,'legacy-handling_feedback','handling_feedback','{"id":"00000000000000000000000000000006","purpose":"feedback","repository":"github.com/owner/repo","repository_id":99,"pull_id":706,"number":23,"actor_id":43,"actor_login":"operator","work_key":"legacy-handling_feedback","state":"queued","private":true,"configuration":{"schema_version":1},"created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z","owner_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}','2026-10-17T00:00:00Z',NULL,NULL,'legacy-thread-6','legacy-turn-6','{"historical": "handling_feedback", "verified": false}',NULL);
CREATE INDEX input_run ON input_requests(run_id);
COMMIT;
PRAGMA user_version=9;
