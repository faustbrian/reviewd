package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackLifecycleCannotAcquirePrivateReviewOrPublicationState(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	run, created, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "feedback-v1", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil || !created {
		t.Fatalf("feedback run creation: %v", err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || claimed {
		t.Fatal("private review claimed modifying work")
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatalf("feedback claim failed: %v", err)
	}
	if err := s.RecordProgress(ctx, run.ID, "feedback-thread", "feedback-turn", "gpt-6.1-sol", "gpt-6.1-sol", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(ctx, run.ID, "not-a-review", json.RawMessage(`{}`), "feedback-thread", "feedback-turn"); err == nil {
		t.Fatal("feedback became a publishable code review")
	}
	if _, err := s.RequestCancellation(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	receipt := json.RawMessage(`{"delivered_head":"bbbb","effects":[{"kind":"reply","state":"verified","id":42}],"remaining":"review requests"}`)
	if err := s.FinishFeedback(ctx, run.ID, Partial, receipt, "cancelled after reply"); err != nil {
		t.Fatal(err)
	}
	actual, err := s.Get(ctx, run.ID)
	if err != nil || actual.State != Partial || actual.Purpose != "feedback" || string(actual.Outcome) != string(receipt) || !actual.CancellationRequested || actual.ThreadID != "feedback-thread" {
		t.Fatalf("partial cancellation lost actual effects or identity: %+v %v", actual, err)
	}
	if err := s.FinishItem(ctx, item, actual); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.BeginPublication(ctx, run.ID, "not-a-review"); err != nil || claimed {
		t.Fatal("feedback acquired code-review publication")
	}
}

func TestFeedbackMigrationPreservesLegacyPreparedPayloadAndDecision(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{runSchema, discoverySchema, notificationSchema, controlsSchema, inputSchema, ownerSchema, cooldownSchema} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	record := `{"id":"legacy","repository":"github.com/owner/repo","repository_id":99,"pull_id":700,"number":17,"actor_id":43,"actor_login":"reviewer","work_key":"legacy","state":"queued","configuration":{}}`
	if _, err := db.ExecContext(ctx, `INSERT INTO runs(id,repository,repository_id,pull_id,actor_id,work_key,state,record,updated_at,prepared,payload_id,thread_id,turn_id) VALUES('legacy','github.com/owner/repo',99,700,43,'legacy','prepared',?,'2026-10-15T00:00:00Z','{"exact":"old payload bytes"}','old-payload','old-thread','old-turn'); INSERT INTO publication_decisions VALUES('legacy','old-payload','2026-10-15T00:00:00Z'); PRAGMA user_version=7;`, record); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	// Open requires private permissions; the fixture owns this exact file.
	if err := os.Chmod(filepath.Join(directory, "reviewd.sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, err := s.Get(ctx, "legacy")
	if err != nil || run.State != Prepared || string(run.Prepared) != `{"exact":"old payload bytes"}` || run.Purpose != "" || run.ThreadID != "old-thread" {
		t.Fatalf("legacy review changed meaning: %+v %v", run, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM publication_decisions WHERE run_id='legacy' AND payload_id='old-payload'").Scan(&count); err != nil || count != 1 {
		t.Fatal("migration lost exact publication consent", err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO publication_decisions VALUES('missing-run','payload','2026-10-15T00:00:00Z')"); err == nil {
		t.Fatal("migration left pooled foreign-key enforcement disabled")
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration broke a referenced legacy run")
	}
}

func TestFeedbackResumeWaitsForProvenOwnerClosureAndRetainsPriorExecution(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	first, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	run, _, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: first.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "resume-feedback", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal("initial feedback claim failed", err)
	}
	if err := s.RecordProgress(ctx, run.ID, "old-thread", "old-turn", "gpt-6.1-sol", "gpt-6.1-sol", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	outcome := json.RawMessage(`{"effects":[{"kind":"push","state":"attempting","commit":"delivered-or-unknown"}]}`)
	if err := s.FinishFeedback(ctx, run.ID, FeedbackPending, outcome, "reconcile before retry"); err != nil {
		t.Fatal(err)
	}
	observed, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ResumeFeedback(ctx, observed, second.ID); err != nil || claimed {
		t.Fatalf("resume stole a live or unproven owner's work: claimed=%t err=%v", claimed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	observed, err = s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ResumeFeedback(ctx, observed, second.ID); err != nil || !claimed {
		t.Fatalf("closed owner's explicit resume failed: %v", err)
	}
	resumed, err := s.Get(ctx, run.ID)
	if err != nil || resumed.State != HandlingFeedback || resumed.OwnerID != second.ID || resumed.ThreadID != "" || resumed.TurnID != "" || string(resumed.Outcome) != string(outcome) || len(resumed.PriorExecutions) != 1 {
		t.Fatalf("fresh agent resume erased effect/identity history: %+v %v", resumed, err)
	}
	if claimed, _ := s.ResumeFeedback(ctx, observed, second.ID); claimed {
		t.Fatal("stale resume claimed a second worker")
	}
}

func TestFeedbackNewDecisionCannotDetachPendingEffectsFromExistingRun(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	run, _, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "pending-effects-choice", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal("claim", err)
	}
	journal := json.RawMessage(`{"effects":[{"kind":"reply","state":"unknown"}]}`)
	if err := s.FinishFeedback(ctx, run.ID, FeedbackPending, journal, "reconcile original reply"); err != nil {
		t.Fatal(err)
	}
	scope, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyDiscovery(ctx, scope, scope, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideFeedbackItem(ctx, item.ID, nil, FeedbackDecision{EventKey: item.EventKey, HeadSHA: item.HeadSHA, TargetBaseSHA: item.TargetBaseSHA, ContextKey: item.ContextKey}); err == nil {
		t.Fatal("new decision detached a pending effect journal instead of requiring explicit reconciliation")
	}
	items, _, err := s.Inbox(ctx)
	actual, getErr := s.Get(ctx, run.ID)
	if err != nil || getErr != nil || len(items) != 1 || items[0].RunID != run.ID || string(actual.Outcome) != string(journal) {
		t.Fatal("rejected decision lost original delivery facts")
	}
}

func TestStaleFeedbackResumeCannotEraseInterveningExecutionAfterABA(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owners := []*PrivateOwner{}
	for range 3 {
		owner, err := s.BeginPrivateOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
		defer owner.Close()
	}
	run, _, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owners[0].ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "resume-aba", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	journal := json.RawMessage(`{"effects":[{"kind":"reply","state":"verified","id":42}]}`)
	finish := func(owner *PrivateOwner, thread string) Run {
		t.Helper()
		if err := s.RecordProgress(ctx, run.ID, thread, "turn-"+thread, "gpt-6.1-sol", "gpt-6.1-sol", "reviewing", nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishFeedback(ctx, run.ID, FeedbackPending, journal, "remaining work"); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		actual, err := s.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return actual
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal("initial claim", err)
	}
	stale := finish(owners[0], "execution-A")
	if claimed, err := s.ResumeFeedback(ctx, stale, owners[1].ID); err != nil || !claimed {
		t.Fatal("B resume", err)
	}
	fresh := finish(owners[1], "execution-B")
	if claimed, err := s.ResumeFeedback(ctx, stale, owners[2].ID); err != nil || claimed {
		t.Fatalf("stale A observation took over after B: claimed=%t err=%v", claimed, err)
	}
	if claimed, err := s.ResumeFeedback(ctx, fresh, owners[2].ID); err != nil || !claimed {
		t.Fatal("fresh C resume", err)
	}
	actual, err := s.Get(ctx, run.ID)
	if err != nil || len(actual.PriorExecutions) != 2 || !strings.Contains(string(actual.PriorExecutions[0]), "execution-A") || !strings.Contains(string(actual.PriorExecutions[1]), "execution-B") || string(actual.Outcome) != string(journal) {
		t.Fatalf("ABA lost execution/effect facts: %+v %v", actual, err)
	}
}

func TestFeedbackClaimCannotReinterpretLegacyReviewDecision(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	forged := item
	forged.Category = "feedback_handling"
	if _, created, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "author", WorkKey: "legacy-escalation", State: Queued, Configuration: json.RawMessage(`{}`)}, forged); err == nil || created {
		t.Fatal("feedback claim converted a stored private-review event into modifying consent")
	}
}

func feedbackSchedulingFixture(t *testing.T) (*Store, InboxItem) {
	t.Helper()
	s, item := schedulingFixture(t)
	ctx := context.Background()
	observed, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	item.Category = "feedback_handling"
	item.ExecutionKind = "feedback_v1"
	item.TargetBaseSHA, item.ContextKey = "base", "human-inventory"
	item.ID = ""
	item.RunID = ""
	item.DecisionID = ""
	if err := s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, err := s.DiscoveryItems(ctx, item.Repository, item.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range items {
		if current.Category == "feedback_handling" {
			return s, current
		}
	}
	t.Fatal("versioned feedback fixture missing")
	return nil, InboxItem{}
}

func TestFeedbackSkillStartRetainsTheSelectedLane(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	selected, err := s.DecideItem(ctx, item.ID, true, false, nil, item.EventKey)
	if err != nil || !selected.Manual || selected.ExecutionKind != "feedback_v1" || selected.DecisionID == "" {
		t.Fatal("normal feedback skill choice was not retained", err)
	}
	if _, _, err := s.CreateForItem(ctx, Run{Executor: NativeExecutor, Purpose: "review", Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "operator", WorkKey: "wrong-lane", State: Queued, Configuration: json.RawMessage(`{}`)}, selected); err == nil {
		t.Fatal("feedback event launched the review skill")
	}
}

func TestFeedbackSkillChoiceSurvivesHeadMovementForTheSameHumanEvent(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	selected, err := s.DecideFeedbackItem(ctx, item.ID, nil, FeedbackDecision{item.EventKey, item.HeadSHA, item.TargetBaseSHA, item.ContextKey})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	next := item
	next.HeadSHA = strings.Repeat("c", 40)
	next.State, next.Baseline = "pending_decision", true
	if err := s.ApplyDiscovery(ctx, observed, observed, nil, []InboxItem{next}); err != nil {
		t.Fatal(err)
	}
	items, err := s.DiscoveryItems(ctx, item.Repository, item.ActorID)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range items {
		if current.ID != selected.ID {
			continue
		}
		if current.HeadSHA != next.HeadSHA || current.DecisionID != selected.DecisionID || !current.Manual || current.State != "queued" || current.Baseline {
			t.Fatalf("same feedback event lost its configured skill choice: %+v", current)
		}
		return
	}
	t.Fatal("feedback item disappeared")
}

func TestFeedbackTypedInboxUpgradePreservesVersionEightBytes(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	var before string
	if err := s.db.QueryRowContext(ctx, "SELECT record FROM inbox_items WHERE id=?", item.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version=8"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	var version int
	var after string
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("older binaries can reinterpret typed fixing items: version=%d err=%v", version, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT record FROM inbox_items WHERE id=?", item.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	var held, prior InboxItem
	if json.Unmarshal([]byte(before), &prior) != nil || json.Unmarshal([]byte(after), &held) != nil || held.ID != prior.ID || held.EventKey != prior.EventKey || held.State != "legacy_pending" || held.LegacyState != prior.State || !held.Baseline {
		t.Fatal("legacy feedback decision was lost or became live skill authority")
	}

}

func TestFeedbackDecisionRejectsChangedDisplayedTarget(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	before, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	next := item
	next.HeadSHA = strings.Repeat("d", 40)
	if err := s.ApplyDiscovery(ctx, before, before, nil, []InboxItem{next}); err != nil {
		t.Fatal(err)
	}
	// The human saw item, then another poll refreshed its target without a new
	// human-feedback event. Its old displayed consent must fail transactionally.
	if _, err := s.DecideFeedbackItem(ctx, item.ID, nil, FeedbackDecision{item.EventKey, item.HeadSHA, item.TargetBaseSHA, item.ContextKey}); err == nil {
		t.Fatal("old displayed feedback event approved an unseen new head")
	}
}

func TestFeedbackExecutionHoldIsNonfatalScopedMetadataClearedByClaim(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	if err := s.SetFeedbackExecutionHold(ctx, item, "Owned unavailable backend"); err != nil {
		t.Fatal(err)
	}
	stale := item
	stale.HeadSHA = "changed"
	if err := s.SetFeedbackExecutionHold(ctx, stale, "Stale diagnostic"); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].ExecutionHold != "Owned unavailable backend" || items[0].Attempts != 0 || items[0].Failure != "" || !items[0].UpdatedAt.Equal(item.UpdatedAt) {
		t.Fatalf("hold changed authority/retries/age or accepted stale target: %+v %v", items, err)
	}
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, created, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "available-after-hold", State: Queued, Configuration: json.RawMessage(`{}`)}, item); err != nil || !created {
		t.Fatal("hold prevented authorized claim", err)
	}
	items, _, err = s.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].ExecutionHold != "" || items[0].State != "handling_feedback" {
		t.Fatalf("real claim retained unavailable diagnostic: %+v %v", items, err)
	}
}
