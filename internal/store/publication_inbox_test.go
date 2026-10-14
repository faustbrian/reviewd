package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func preparedInboxFixture(t *testing.T) (*Store, InboxItem, Run) {
	t.Helper()
	s, item := schedulingFixture(t)
	ctx := context.Background()
	run, _, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "event", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err = s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{"body":"preview"}`), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	run, err = s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishItem(ctx, item, run); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("prepared inbox: %v %+v", err, items)
	}
	return s, items[0], run
}

func TestConcurrentSkipAndPublicationCannotBothSucceed(t *testing.T) {
	s, item, run := preparedInboxFixture(t)
	ctx := context.Background()
	var sequence int
	var name, path string
	if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start := make(chan struct{})
	publication := make(chan bool, 1)
	skipped := make(chan bool, 1)
	go func() {
		<-start
		_, claimed, _ := s.BeginItemPublication(ctx, run.ID, run.PayloadID, item, json.RawMessage(`{}`))
		publication <- claimed
	}()
	go func() { <-start; _, err := other.DecideItem(ctx, item.ID, false, false, nil); skipped <- err == nil }()
	close(start)
	claimed, accepted := <-publication, <-skipped
	if claimed == accepted {
		t.Fatalf("publication/skip did not have one linearized winner: claimed=%v skip=%v", claimed, accepted)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed && current.State != Publishing {
		t.Fatalf("claimed attempt lost: %+v", current)
	}
	if accepted && current.State != Prepared {
		t.Fatalf("accepted skip still published: %+v", current)
	}
}

func TestAutomaticPublicationClaimRejectsRevokedItemAuthority(t *testing.T) {
	for _, change := range []string{"skip", "start", "exclude", "supersede"} {
		t.Run(change, func(t *testing.T) {
			s, item, run := preparedInboxFixture(t)
			ctx := context.Background()
			if err := s.DecidePublication(ctx, run.ID, run.PayloadID); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "skip", "start":
				if _, err := s.DecideItem(ctx, item.ID, change == "start", false, nil); err != nil {
					t.Fatal(err)
				}
			case "exclude":
				if _, err := s.ConfigureDiscovery(ctx, nil); err != nil {
					t.Fatal(err)
				}
			case "supersede":
				observed, err := s.DiscoveryScope(ctx, item.Repository)
				if err != nil {
					t.Fatal(err)
				}
				next := InboxItem{Repository: item.Repository, ActorID: item.ActorID, PullID: item.PullID, Number: item.Number, Category: item.Category, Trigger: "head_updated", Action: "auto", State: "queued", EventKey: "new-event", HeadSHA: "new-head"}
				if err = s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{next}); err != nil {
					t.Fatal(err)
				}
			}
			if _, claimed, _ := s.BeginItemPublication(ctx, run.ID, run.PayloadID, item, json.RawMessage(`{}`)); claimed {
				t.Fatalf("%s still allowed automatic publication", change)
			}
			current, err := s.Get(ctx, run.ID)
			if err != nil || current.State != Prepared {
				t.Fatalf("revoked run claimed: %v %+v", err, current)
			}
			var attempts int
			if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM publication_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("revoked publication recorded an attempt: %d %v", attempts, err)
			}
		})
	}
}

func TestPublicationClaimProjectsInFlightStateAndRejectsLateDecisions(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual-claim", true: "automatic-claim"}[bound], func(t *testing.T) {
			s, item, run := preparedInboxFixture(t)
			ctx := context.Background()
			if err := s.DecidePublication(ctx, run.ID, run.PayloadID); err != nil {
				t.Fatal(err)
			}
			var claimed bool
			var err error
			if bound {
				_, claimed, err = s.BeginItemPublication(ctx, run.ID, run.PayloadID, item, json.RawMessage(`{}`))
			} else {
				_, claimed, err = s.BeginPublication(ctx, run.ID, run.PayloadID)
			}
			if err != nil || !claimed {
				t.Fatalf("publication not claimed: %v %v", claimed, err)
			}
			if bound {
				items, _, err := s.Inbox(ctx)
				if err != nil || len(items) != 1 || items[0].State != "publishing" {
					t.Fatalf("in-flight publication hidden: %v %+v", err, items)
				}
			}
			for _, start := range []bool{false, true} {
				if _, err := s.DecideItem(ctx, item.ID, start, false, nil); err == nil {
					t.Fatal("decision acknowledged after publication claim")
				}
			}
		})
	}
}

func TestPublicationOutcomeRecoversLinkedInboxWithoutFinish(t *testing.T) {
	for _, mode := range []string{"restart", "discovery", "manual-reconciliation"} {
		for _, outcome := range []State{Published, PublicationUnknown, Partial, Stale} {
			t.Run(mode+"/"+string(outcome), func(t *testing.T) {
				s, item, run := preparedInboxFixture(t)
				ctx := context.Background()
				if err := s.DecidePublication(ctx, run.ID, run.PayloadID); err != nil {
					t.Fatal(err)
				}
				var claimed bool
				var err error
				if mode == "manual-reconciliation" {
					_, claimed, err = s.BeginPublication(ctx, run.ID, run.PayloadID)
				} else {
					_, claimed, err = s.BeginItemPublication(ctx, run.ID, run.PayloadID, item, json.RawMessage(`{}`))
				}
				if err != nil || !claimed {
					t.Fatalf("claim: %v %v", claimed, err)
				}
				run, err = s.Get(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.MarkOutcome(ctx, run, outcome, json.RawMessage(`{"review_id":111}`)); err != nil {
					t.Fatal(err)
				}
				// The process may stop before Engine.Complete projects the outcome.
				if mode == "restart" {
					var sequence int
					var name, path string
					if err = s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
						t.Fatal(err)
					}
					if err = s.Close(); err != nil {
						t.Fatal(err)
					}
					s, err = Open(ctx, filepath.Dir(path))
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					if err = s.RecoverScheduled(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					observed, err := s.DiscoveryScope(ctx, item.Repository)
					if err != nil {
						t.Fatal(err)
					}
					fresh := item
					fresh.State, fresh.RunID, fresh.DecisionID = "queued", "", ""
					if err = s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []InboxItem{fresh}); err != nil {
						t.Fatal(err)
					}
				}
				items, _, err := s.Inbox(ctx)
				if err != nil || len(items) != 1 || items[0].State != string(outcome) || items[0].RunID != run.ID {
					t.Fatalf("recorded outcome stranded in inbox: want=%s items=%+v err=%v", outcome, items, err)
				}
				if _, err = s.DecideItem(ctx, item.ID, false, false, nil); err != nil {
					t.Fatalf("finished attempt still rejects a decision: %v", err)
				}
				if err = s.RecoverScheduled(ctx); err != nil {
					t.Fatal(err)
				}
				items, _, err = s.Inbox(ctx)
				if err != nil || len(items) != 0 {
					t.Fatalf("recovery revived skipped work: %+v %v", items, err)
				}
			})
		}
	}
}
