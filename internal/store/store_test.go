package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

func TestDurableRunSingleClaimAndBoundPublicationDecision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profile")
	a, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	request := Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 42, ActorLogin: "reviewer", WorkKey: "work-one", State: Queued, Private: true, Configuration: json.RawMessage(`{"schema_version":1}`)}
	first, created, err := a.Create(ctx, request)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	again, created, err := b.Create(ctx, request)
	if err != nil || created || again.ID != first.ID {
		t.Fatal("duplicate observation created independent work")
	}
	var wg sync.WaitGroup
	claims := make(chan bool, 2)
	for _, connection := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			claimed, err := s.Claim(ctx, first.ID)
			if err != nil {
				t.Error(err)
			}
			claims <- claimed
		}(connection)
	}
	wg.Wait()
	close(claims)
	won := 0
	for claimed := range claims {
		if claimed {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("concurrent clients obtained %d review claims", won)
	}
	payload := json.RawMessage(`{"payload_id":"payload-one","body":"**No findings.**"}`)
	if err := a.Prepare(ctx, first.ID, "payload-one", payload, "thread-one", "turn-one"); err != nil {
		t.Fatal(err)
	}
	if err := a.DecidePublication(ctx, first.ID, "payload-old"); err == nil {
		t.Error("old/materially changed preview granted publication")
	}
	if _, claimed, err := a.BeginPublication(ctx, first.ID, "payload-one"); err != nil || claimed {
		t.Error("prepared private result published without an explicit decision")
	}
	if err := a.DecidePublication(ctx, first.ID, "payload-one"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := a.BeginPublication(ctx, first.ID, "payload-one"); err != nil || !claimed {
		t.Fatal("explicit exact-payload decision did not authorize publisher claim")
	}
	if _, claimed, err := b.BeginPublication(ctx, first.ID, "payload-one"); err != nil || claimed {
		t.Error("second client could repost an in-flight publication")
	}
	if err := a.MarkOutcome(ctx, observedRun(t, a, first.ID), PublicationUnknown, json.RawMessage(`{"reason":"lost response"}`)); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err := reopened.Get(ctx, first.ID)
	if err != nil || run.State != PublicationUnknown || run.ThreadID != "thread-one" || string(run.Prepared) != string(payload) {
		t.Fatalf("durable ambiguity/result was lost: %+v %v", run, err)
	}
	if _, claimed, err := reopened.BeginPublication(ctx, first.ID, "payload-one"); err != nil || claimed {
		t.Error("restart blindly retried unknown remote publication")
	}
}

func TestStoreFailureCannotBecomePreparedOrPublished(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 1, PullID: 2, Number: 1, ActorID: 3, ActorLogin: "reviewer", WorkKey: "failure", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err == nil {
		t.Error("unclaimed work became prepared")
	}
	if err := s.MarkOutcome(ctx, observedRun(t, s, run.ID), Published, json.RawMessage(`{}`)); err == nil {
		t.Error("queued work became published")
	}
	if err := s.Fail(ctx, run.ID, Cancelled, "operator cancelled"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || claimed {
		t.Error("cancelled work restarted as clean review")
	}
}

func TestRepairClaimsAreExclusivePayloadBoundAndDurable(t *testing.T) {
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
	run, _, err := a.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 1, PullID: 2, Number: 1, ActorID: 3, ActorLogin: "reviewer", WorkKey: "repair", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Claim(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if err := a.DecidePublication(ctx, run.ID, "payload"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := a.BeginPublication(ctx, run.ID, "payload"); err != nil || !claimed {
		t.Fatal("original publication claim failed")
	}
	if err := a.MarkOutcome(ctx, observedRun(t, a, run.ID), Partial, json.RawMessage(`{"review":{"id":71}}`)); err != nil {
		t.Fatal(err)
	}
	intent := json.RawMessage(`{"review":{"id":71},"pending_repair":{"comment_key":"finding","previous_ids":[72]}}`)
	observed := observedRun(t, a, run.ID)
	if claimed, err := a.BeginRepair(ctx, observed, "old-payload", intent); err != nil || claimed {
		t.Fatal("old decision claimed a new repair")
	}
	var wg sync.WaitGroup
	claims := make(chan bool, 2)
	for _, connection := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			claimed, err := s.BeginRepair(ctx, observed, "payload", intent)
			if err != nil {
				t.Error(err)
			}
			claims <- claimed
		}(connection)
	}
	wg.Wait()
	close(claims)
	winners := 0
	for claimed := range claims {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("repair had %d independent writers", winners)
	}
	a.Close()
	b.Close()
	reopened, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.Get(ctx, run.ID)
	if err != nil || stored.State != Publishing || string(stored.Outcome) != string(intent) {
		t.Fatal("repair intent was not durably recorded before the write")
	}
	if claimed, err := reopened.BeginRepair(ctx, stored, "payload", intent); err != nil || claimed {
		t.Fatal("restart blindly reclaimed an unknown repair")
	}
}

func TestStaleReconciliationCannotEraseActiveRepairIntent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 1, PullID: 2, Number: 1, ActorID: 3, ActorLogin: "reviewer", WorkKey: "stale-reader", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	s.Claim(ctx, run.ID)
	s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn")
	s.DecidePublication(ctx, run.ID, "payload")
	s.BeginPublication(ctx, run.ID, "payload")
	if err := s.MarkOutcome(ctx, observedRun(t, s, run.ID), Partial, json.RawMessage(`{"review":{"id":71}}`)); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	intent := json.RawMessage(`{"review":{"id":71},"pending_repair":{"comment_key":"missing","previous_ids":[72]}}`)
	if claimed, err := s.BeginRepair(ctx, stale, "payload", intent); err != nil || !claimed {
		t.Fatal("first repair not claimed")
	}
	if err := s.MarkOutcome(ctx, stale, Partial, stale.Outcome); err == nil {
		t.Error("stale reconciliation erased active repair intent")
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Publishing || string(current.Outcome) != string(intent) {
		t.Error("active repair ownership was not retained")
	}
	if claimed, err := s.BeginRepair(ctx, stale, "payload", intent); err != nil || claimed {
		t.Error("stale reader authorized another repair writer")
	}
	settled := json.RawMessage(`{"review":{"id":71},"repairs":[{"id":74}],"comments":[{"id":72},{"id":74}]}`)
	if err := s.MarkOutcome(ctx, current, Partial, settled); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.BeginRepair(ctx, stale, "payload", intent); err != nil || claimed {
		t.Error("stale repair reclaimed partial state after another repair settled")
	}
	after := observedRun(t, s, run.ID)
	if after.State != Partial || string(after.Outcome) != string(settled) {
		t.Error("stale repair erased completed repair identity")
	}

}

func observedRun(t testing.TB, s *Store, id string) Run {
	t.Helper()
	run, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
