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
	if err := a.MarkOutcome(ctx, first.ID, PublicationUnknown, json.RawMessage(`{"reason":"lost response"}`)); err != nil {
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
	if err := s.MarkOutcome(ctx, run.ID, Published, json.RawMessage(`{}`)); err == nil {
		t.Error("queued work became published")
	}
	if err := s.Fail(ctx, run.ID, Cancelled, "operator cancelled"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || claimed {
		t.Error("cancelled work restarted as clean review")
	}
}
