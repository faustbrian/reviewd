package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestActionReloadKeepsFrozenRunAndExplicitDecisionOwnership(t *testing.T) {
	for _, variant := range []struct {
		name            string
		manual, waiting bool
	}{{"automatic-waiting", false, true}, {"manual-waiting", true, true}, {"manual-queued", true, false}} {
		t.Run(variant.name, func(t *testing.T) {
			s, item := schedulingFixture(t)
			ctx := context.Background()
			if variant.manual {
				var err error
				item, err = s.DecideItem(ctx, item.ID, true, false, map[string]any{"reviewer.reasoning_effort": "low"})
				if err != nil {
					t.Fatal(err)
				}
			}
			decision := item.DecisionID
			var run Run
			if variant.waiting {
				var err error
				run, _, err = s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "event", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.WaitForChecks(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				run, err = s.Get(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.FinishItem(ctx, item, run); err != nil {
					t.Fatal(err)
				}
			}
			for _, action := range []string{"ask", "notify", "ignore", "ask"} {
				observed, err := s.DiscoveryScope(ctx, item.Repository)
				if err != nil {
					t.Fatal(err)
				}
				state := map[string]string{"ask": "pending_decision", "notify": "attention", "ignore": "ignored"}[action]
				replacement := InboxItem{Repository: item.Repository, ActorID: item.ActorID, PullID: item.PullID, Number: item.Number, Category: item.Category, Trigger: item.Trigger, Action: action, State: state, Baseline: true, EventKey: item.EventKey, HeadSHA: item.HeadSHA, TargetBaseSHA: item.TargetBaseSHA, ContextKey: item.ContextKey}
				if err = s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{replacement}); err != nil {
					t.Fatal(err)
				}
				items, err := s.DiscoveryItems(ctx, item.Repository, item.ActorID)
				if err != nil || len(items) != 1 {
					t.Fatalf("reload lost work: %v %+v", err, items)
				}
				current := items[0]
				want := "queued"
				if variant.waiting {
					want = "waiting_checks"
				}
				if action == "ignore" {
					want = "ignored"
				}
				if current.State != want || current.RunID != run.ID || current.DecisionID != decision || current.Manual != variant.manual || current.Baseline != item.Baseline || (variant.manual && current.Overrides["reviewer.reasoning_effort"] != "low") {
					t.Fatalf("%s replaced scheduler-owned inputs: %+v", action, current)
				}
				if action == "ignore" {
					visible, _, err := s.Inbox(ctx)
					if err != nil || len(visible) != 0 {
						t.Fatalf("ignore became actionable: %v %+v", err, visible)
					}
				}
			}
			observed, err := s.DiscoveryScope(ctx, item.Repository)
			if err != nil {
				t.Fatal(err)
			}
			replacement := InboxItem{Repository: item.Repository, ActorID: item.ActorID, PullID: item.PullID, Number: item.Number, Category: item.Category, Trigger: item.Trigger, Action: "auto", State: "queued", EventKey: "new-event", HeadSHA: "new-head"}
			if err = s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{replacement}); err != nil {
				t.Fatal(err)
			}
			items, _, err := s.Inbox(ctx)
			if err != nil || len(items) != 1 || items[0].RunID != "" || items[0].DecisionID != "" || items[0].Manual || len(items[0].Overrides) != 0 {
				t.Fatalf("new event inherited old decision: %v %+v", err, items)
			}
		})
	}
}

func schedulingFixture(t *testing.T) (*Store, InboxItem) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	before, err := s.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ActorID, after.RepositoryID, after.State = 43, 99, "ok"
	item := InboxItem{Repository: before.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", Trigger: "review_requested", Action: "auto", State: "queued", EventKey: "request", HeadSHA: "head"}
	if err := s.ApplyDiscovery(ctx, before, after, []Observation{{PullID: 700, Number: 17, Record: json.RawMessage(`{}`)}}, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("fixture: %v %+v", err, items)
	}
	return s, items[0]
}

func TestAcquisitionFailuresHaveDurableBoundedBackoffAndExplicitRestart(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	for attempt := 1; attempt <= 3; attempt++ {
		if err := s.ItemFailure(ctx, item, "GitHub temporarily unavailable", now); err != nil {
			t.Fatal(err)
		}
		items, _, err := s.Inbox(ctx)
		if err != nil || len(items) != 1 {
			t.Fatalf("failure disappeared: %v %+v", err, items)
		}
		item = items[0]
		if item.Attempts != attempt || item.Failure == "" {
			t.Fatalf("attempt lost: %+v", item)
		}
		if attempt < 3 {
			if item.State != "retry_wait" || !item.RetryAt.Equal(now.Add(time.Duration(attempt)*30*time.Second)) {
				t.Fatalf("retry spins instead of waiting: %+v", item)
			}
			now = item.RetryAt
		} else if item.State != "failed" {
			t.Fatalf("retry budget did not terminate: %+v", item)
		}
	}
	started, err := s.DecideItem(ctx, item.ID, true, false, nil)
	if err != nil || started.State != "queued" || started.Attempts != 0 || started.Failure != "" {
		t.Fatalf("explicit new decision did not reset failure: %+v %v", started, err)
	}
}

func TestPreparedFailuresRetainEvidenceAndExhaustAcrossRecovery(t *testing.T) {
	s, item, run := preparedInboxFixture(t)
	ctx := context.Background()
	now := time.Now()
	for attempt := 1; attempt <= 3; attempt++ {
		if err := s.ItemFailure(ctx, item, "prepared verification rate limited", now, time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverScheduled(ctx); err != nil {
			t.Fatal(err)
		}
		observed, err := s.DiscoveryScope(ctx, item.Repository)
		if err != nil {
			t.Fatal(err)
		}
		fresh := item
		fresh.State, fresh.RunID = "queued", ""
		fresh.Baseline = true
		if err = s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{fresh}); err != nil {
			t.Fatal(err)
		}
		items, _, err := s.Inbox(ctx)
		want := "prepared"
		if attempt == 3 {
			want = "failed"
		}
		if err != nil || len(items) != 1 || items[0].State != want || items[0].RunID != run.ID || items[0].Attempts != attempt || !items[0].RetryAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("prepared retry ownership/budget lost: attempt=%d items=%+v err=%v", attempt, items, err)
		}
		current, err := s.Get(ctx, run.ID)
		if err != nil || current.State != Prepared || current.ThreadID != run.ThreadID || current.PayloadID != run.PayloadID {
			t.Fatalf("retry replaced prepared evidence: %+v %v", current, err)
		}
		item = items[0]
	}
	started, err := s.DecideItem(ctx, item.ID, true, false, nil)
	if err != nil || started.Attempts != 0 || !started.RetryAt.IsZero() || started.State != "queued" || !started.Manual {
		t.Fatalf("explicit restart did not reset retry budget: %+v %v", started, err)
	}
}

func TestDueAcquisitionRetryCreatesExactlyOneLinkedRun(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	queued := Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "same-event", State: Queued, Configuration: json.RawMessage(`{}`)}
	if err := s.ItemFailure(ctx, item, "transient source acquisition", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("retry state missing: %v %+v", err, items)
	}
	item = items[0]
	if item.State != "retry_wait" || time.Now().Before(item.RetryAt) {
		t.Fatalf("retry not due: %+v", item)
	}
	run, created, err := s.CreateForItem(ctx, queued, item)
	if err != nil || !created {
		t.Fatalf("recovered acquisition could not enter its first run: created=%v err=%v", created, err)
	}
	items, _, err = s.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].RunID != run.ID || items[0].State != "reviewing" {
		t.Fatalf("retry lost its unique link: %v %+v", err, items)
	}
	if _, created, _ := s.CreateForItem(ctx, queued, item); created {
		t.Fatal("same event created another run")
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("duplicate retry runs: %v %+v", err, runs)
	}
}

func TestAcquisitionRetryCannotBypassFutureServerDeadline(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.ItemFailure(ctx, item, "rate limited", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 || !items[0].RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("server retry guidance lost: %v %+v", err, items)
	}
	item = items[0]
	_, created, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "event", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if created || err == nil {
		t.Fatal("future retry bypassed its deadline")
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("future retry created work: %v %+v", err, runs)
	}
}

func TestRecoveryDoesNotRestartAnInterruptedModelOrDiscardItsIdentity(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := s.CreateForItem(ctx, Run{OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "event", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil || !created {
		t.Fatalf("create: %+v %v", run, err)
	}
	if _, err = s.Claim(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordWorker(ctx, run.ID, "interrupted-thread", "interrupted-turn", "gpt-6.1-sol", "gpt-6.1-sol", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate owner loss without a clean Close. The actual CLI counterpart
	// separately proves kernel release after an owner process exits.
	if err = owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverPrivate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, run.ID)
	items, _, inboxErr := s.Inbox(ctx)
	if err != nil || inboxErr != nil || current.State != Failed || current.ThreadID != "interrupted-thread" || len(items) != 1 || items[0].State != "failed" || items[0].RunID != run.ID {
		t.Fatalf("unsafe recovery: run=%+v items=%+v errors=%v %v", current, items, err, inboxErr)
	}
	if err = s.RecoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("recovery duplicated work: %+v %v", runs, err)
	}
}
