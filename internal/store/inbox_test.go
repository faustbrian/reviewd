package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestDiscoveryRevisionRejectsOlderPollAndFailureWithoutErasingInbox(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	a, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	older, err := a.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	next := older
	next.ActorID = 43
	next.RepositoryID = 99
	next.State = "ok"
	item := InboxItem{Repository: older.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", Trigger: "review_requested", State: "pending_decision", Baseline: true, EventKey: "newest-request", HeadSHA: "newer"}
	rows := []Observation{{PullID: 700, Number: 17, Record: json.RawMessage(`{"head":"newer"}`)}}
	if err := a.ApplyDiscovery(ctx, older, next, rows, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	stale := item
	stale.EventKey = "older-request"
	stale.HeadSHA = "older"
	if err := b.ApplyDiscovery(ctx, older, next, []Observation{{PullID: 700, Number: 17, Record: json.RawMessage(`{"head":"older"}`)}}, []InboxItem{stale}); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("older poll overwrote newer work: %v", err)
	}
	if err := b.DiscoveryFailure(ctx, older, "old timeout"); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("old failure replaced a successful poll: %v", err)
	}
	items, scopes, err := a.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].EventKey != "newest-request" || len(scopes) != 1 || scopes[0].State != "ok" {
		t.Fatalf("current work was erased: %v %v %v", items, scopes, err)
	}
	originalID := items[0].ID
	observed, err := a.DiscoveryScope(ctx, older.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyDiscovery(ctx, observed, next, rows, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, _, err = b.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].ID != originalID || !items[0].Baseline || items[0].State != "pending_decision" {
		t.Fatal("duplicate poll reset the initial backlog decision or created new work")
	}
}

func TestDiscoveryMigrationPreservesLegacyRunAndPublicationDecision(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	original, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := original.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "reviewer", WorkKey: "legacy", State: Queued, Configuration: json.RawMessage(`{}`), Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := original.Claim(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"payload_id":"legacy-payload","body":"retained preview"}`)
	if err := original.Prepare(ctx, run.ID, "legacy-payload", payload, "legacy-thread", "legacy-turn"); err != nil {
		t.Fatal(err)
	}
	if err := original.DecidePublication(ctx, run.ID, "legacy-payload"); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the predecessor schema in this task-owned temporary database.
	if _, err := original.db.ExecContext(ctx, `DROP TABLE input_requests; DROP TABLE item_snoozes; DROP TABLE project_pauses; ALTER TABLE observations DROP COLUMN head_sha; DROP TABLE inbox_items; DROP TABLE observations; DROP TABLE follows; DROP TABLE discovery_scopes; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	got, err := migrated.Get(ctx, run.ID)
	if err != nil || string(got.Prepared) != string(payload) || got.ThreadID != "legacy-thread" || got.TurnID != "legacy-turn" || got.State != Prepared {
		t.Fatalf("legacy review was lost: %+v %v", got, err)
	}
	if _, claimed, err := migrated.BeginPublication(ctx, run.ID, "legacy-payload"); err != nil || !claimed {
		t.Fatalf("legacy exact publication decision was lost: %v", err)
	}
	items, scopes, err := migrated.Inbox(ctx)
	if err != nil || len(items) != 0 || len(scopes) != 0 {
		t.Fatal("migration invented discovery work")
	}
}

func TestEmptyAllowlistRevokesOutstandingPollWithoutErasingRuns(t *testing.T) {
	ctx := context.Background()
	durable, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	run, _, err := durable.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "reviewer", WorkKey: "retained", State: Queued, Configuration: json.RawMessage(`{}`), Private: true})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := durable.DiscoveryScope(ctx, run.Repository)
	if err != nil {
		t.Fatal(err)
	}
	next := initial
	next.ActorID, next.RepositoryID, next.State = 43, 99, "ok"
	rows := []Observation{{PullID: 700, Number: 17, Record: json.RawMessage(`{}`)}}
	items := []InboxItem{{Repository: run.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", State: "queued", EventKey: "request"}}
	if err := durable.ApplyDiscovery(ctx, initial, next, rows, items); err != nil {
		t.Fatal(err)
	}
	active, _, err := durable.Inbox(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("initial inbox unavailable: %+v %v", active, err)
	}
	outstanding, err := durable.DiscoveryScope(ctx, run.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := durable.ConfigureDiscovery(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := durable.ApplyDiscovery(ctx, outstanding, next, rows, items); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("outstanding poll revived excluded scope: %v", err)
	}
	if err := durable.DiscoveryFailure(ctx, outstanding, "late failure"); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("outstanding failure revived excluded scope: %v", err)
	}
	work, scopes, err := durable.Inbox(ctx)
	if err != nil || len(work) != 0 || len(scopes) != 1 || scopes[0].State != "inactive" {
		t.Fatalf("excluded scope remains actionable: %+v %+v %v", work, scopes, err)
	}
	if got, err := durable.Get(ctx, run.ID); err != nil || got.WorkKey != "retained" {
		t.Fatalf("deactivation erased historical run: %+v %v", got, err)
	}
	if err := durable.SetFollow(ctx, active[0].ID, true); err == nil {
		t.Fatal("local follow revived excluded work")
	}
}

func TestFirstDiscoveryAcquisitionCannotReviveExcludedScope(t *testing.T) {
	ctx := context.Background()
	durable, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	outstanding, err := durable.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if outstanding.Initialized {
		t.Fatal("first acquisition already has a successful baseline")
	}
	if _, err := durable.ConfigureDiscovery(ctx, nil); err != nil {
		t.Fatal(err)
	}
	next := outstanding
	next.ActorID, next.RepositoryID, next.State = 43, 99, "ok"
	item := InboxItem{Repository: outstanding.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", State: "queued", EventKey: "request"}
	if err := durable.ApplyDiscovery(ctx, outstanding, next, nil, []InboxItem{item}); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("first acquisition revived excluded work: %v", err)
	}
	if err := durable.DiscoveryFailure(ctx, outstanding, "late first failure"); !errors.Is(err, ErrDiscoveryChanged) {
		t.Fatalf("first failure revived excluded scope: %v", err)
	}
	items, scopes, err := durable.Inbox(ctx)
	if err != nil || len(items) != 0 || len(scopes) != 1 || scopes[0].State != "inactive" || scopes[0].Initialized {
		t.Fatalf("first acquisition established an excluded baseline: %+v %+v %v", items, scopes, err)
	}
}
