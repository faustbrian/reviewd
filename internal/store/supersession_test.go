package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/faustbrian/reviewd/internal/clarification"
)

func supersessionFixture(t *testing.T, state State) (*Store, InboxItem, Run) {
	t.Helper()
	s, item := schedulingFixture(t)
	ctx := context.Background()
	run, won, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "supersession", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil || !won {
		t.Fatalf("create: %t %v", won, err)
	}
	if state == WaitingChecks {
		err = s.WaitForChecks(ctx, run.ID)
	} else if state != Queued {
		_, err = s.Claim(ctx, run.ID)
		if err == nil && state != Reviewing {
			err = s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{"body":"retained"}`), "thread", "turn")
			if err == nil {
				err = s.DecidePublication(ctx, run.ID, "payload")
			}
			if err == nil && state != Prepared {
				_, claimed, claimErr := s.BeginPublication(ctx, run.ID, "payload")
				if claimErr != nil || !claimed {
					t.Fatalf("claim: %t %v", claimed, claimErr)
				}
				if state != Publishing {
					current, _ := s.Get(ctx, run.ID)
					err = s.MarkOutcome(ctx, current, state, json.RawMessage(`{"verified":true,"review":{"id":123}}`))
				}
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	run, err = s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, item, run
}

func applySupersession(t *testing.T, s *Store, item InboxItem, material bool, event string) {
	t.Helper()
	ctx := context.Background()
	observed, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	item.EventKey = event
	item.RunID = ""
	item.DecisionID = ""
	item.State = "queued"
	if err := s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, MaterialChange: material, Record: json.RawMessage(`{}`)}}, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
}

// QA-X05/X08: material invalidation and publication claims share one SQLite
// ordering. Neither winner may erase remote facts or manufacture a new attempt.
func TestMaterialDiscoverySupersessionRespectsPublicationOwnership(t *testing.T) {
	for _, state := range []State{Queued, WaitingChecks, Reviewing, Prepared, Publishing, Published, Partial, PublicationUnknown} {
		t.Run(string(state), func(t *testing.T) {
			s, item, before := supersessionFixture(t, state)
			applySupersession(t, s, item, true, "new-material-event")
			after, err := s.Get(context.Background(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := state
			marked := state == Queued || state == WaitingChecks || state == Reviewing || state == Prepared
			if marked && state != Reviewing {
				want = Stale
			}
			if after.State != want || after.Superseded != marked || after.CancellationRequested || after.PayloadID != before.PayloadID || string(after.Prepared) != string(before.Prepared) || string(after.Outcome) != string(before.Outcome) || after.ThreadID != before.ThreadID || after.TurnID != before.TurnID {
				t.Fatalf("lost lifecycle or retained facts: before=%s after=%s superseded=%t", before.State, after.State, after.Superseded)
			}
			if state == Prepared {
				var decisions int
				if err := s.db.QueryRow("SELECT count(*) FROM publication_decisions WHERE run_id=?", before.ID).Scan(&decisions); err != nil || decisions != 0 {
					t.Fatal("supersession retained publication consent")
				}
				if _, claimed, err := s.BeginPublication(context.Background(), before.ID, before.PayloadID); err != nil || claimed {
					t.Fatal("superseded preview started publication")
				}
			}
			if state == Reviewing {
				if stopped, err := s.StopRequested(context.Background(), before.ID); err != nil || !stopped {
					t.Fatal("owner cannot observe durable supersession")
				}
				if err := s.Prepare(context.Background(), before.ID, "late", json.RawMessage(`{}`), "thread", "turn"); err == nil {
					t.Fatal("late worker prepared obsolete evidence")
				}
				if err := s.Fail(context.Background(), before.ID, Cancelled, "worker exited after cancellation"); err != nil {
					t.Fatal(err)
				}
				settled, _ := s.Get(context.Background(), before.ID)
				if settled.State != Stale {
					t.Fatal("transport cancellation concealed supersession")
				}
			}
		})
	}
}

func TestPureRerequestAndChecksOnlyDoNotInvalidateEvidence(t *testing.T) {
	for _, event := range []string{"same-event", "new-request"} {
		t.Run(event, func(t *testing.T) {
			s, item, before := supersessionFixture(t, Prepared)
			if event == "same-event" {
				event = item.EventKey
			}
			applySupersession(t, s, item, false, event)
			after, err := s.Get(context.Background(), before.ID)
			if err != nil || after.State != Prepared || after.Superseded || after.PayloadID != before.PayloadID {
				t.Fatal("nonmaterial discovery invalidated a valid preview")
			}
		})
	}
}

func TestSupersededActiveWorkRejectsInputAndExplicitCancellationStillWins(t *testing.T) {
	s, item, run := supersessionFixture(t, Reviewing)
	ctx := context.Background()
	if err := s.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	request := clarification.Request{ID: "pending", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which?"}}}
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	applySupersession(t, s, item, true, "changed-requirements")
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"obsolete"}}}}); err == nil {
		t.Fatal("accepted answer for superseded evidence")
	}
	request.ID = "late"
	if err := s.OpenInput(ctx, run.ID, request); err == nil {
		t.Fatal("opened a question after supersession")
	}
	if _, err := s.RequestCancellation(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, Stale, "late stale outcome"); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, run.ID)
	if err != nil || after.State != Cancelled || !after.CancellationRequested || !after.Superseded || after.ThreadID != "thread" || after.TurnID != "turn" {
		t.Fatal("explicit cancellation lost to an older classification")
	}
}

func TestLostOwnerPreservesSupersessionCause(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE runs SET record=json_set(record,'$.superseded',json('true')) WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverPrivate(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, run.ID)
	if err != nil || after.State != Stale || !after.Superseded || after.CancellationRequested || !after.OwnerClosed {
		t.Fatal("recovery forgot material supersession")
	}
}
