package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestCancellationRespectsRunAndPublicationPartitions(t *testing.T) {
	for _, state := range []State{Queued, WaitingChecks, Reviewing, Prepared, Publishing, Published, PublicationUnknown, Partial, Failed, Stale, Cancelled} {
		t.Run(string(state), func(t *testing.T) {
			s, item := schedulingFixture(t)
			ctx := context.Background()
			run, _, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "cancel-partition", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
			if err != nil {
				t.Fatal(err)
			}
			if state == WaitingChecks {
				err = s.WaitForChecks(ctx, run.ID)
			} else if state != Queued {
				if _, err = s.Claim(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				if state == Cancelled || state == Failed || state == Stale {
					err = s.Fail(ctx, run.ID, state, "existing diagnostic")
				} else if state != Reviewing {
					err = s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{"body":"retained preview"}`), "thread", "turn")
					if err != nil {
						t.Fatal(err)
					}
					if state != Prepared {
						if err = s.DecidePublication(ctx, run.ID, "payload"); err != nil {
							t.Fatal(err)
						}
						if _, claimed, err := s.BeginPublication(ctx, run.ID, "payload"); err != nil || !claimed {
							t.Fatalf("publication fixture: %v", err)
						}
						if state != Publishing {
							current, readErr := s.Get(ctx, run.ID)
							if readErr != nil {
								t.Fatal(readErr)
							}
							err = s.MarkOutcome(ctx, current, state, json.RawMessage(`{"actual":"retained"}`))
						}
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			requested, err := s.RequestCancellation(ctx, run.ID)
			allowed := state == Queued || state == WaitingChecks || state == Reviewing || state == Cancelled
			if (err == nil) != allowed {
				t.Fatalf("wrong cancellation partition: state=%s err=%v", state, err)
			}
			after, err := s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := state
			if state == Queued || state == WaitingChecks {
				want = Cancelled
			}
			if after.State != want || after.InboxItemID != before.InboxItemID || string(after.Prepared) != string(before.Prepared) || string(after.Outcome) != string(before.Outcome) {
				t.Fatal("request changed factual identity, preview or publication outcome")
			}
			if allowed && (!after.CancellationRequested || requested.State != want) {
				t.Fatal("accepted request was not durable or lied about active work")
			}
			if !allowed && after.CancellationRequested {
				t.Fatal("rejected request changed the run")
			}
			if state == Queued || state == WaitingChecks {
				items, _, err := s.Inbox(ctx)
				if err != nil || len(items) != 1 || items[0].State != "cancelled" {
					t.Fatal("no-worker cancellation was not projected into inbox")
				}
			}
		})
	}
}

func TestCancellationRequestPreventsLatePreparation(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	run, _, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "cancel-before-prepare", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
		t.Fatal("claim failed")
	}
	if _, err := s.RequestCancellation(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(ctx, run.ID, "late-payload", json.RawMessage(`{}`), "thread", "turn"); err == nil {
		t.Fatal("accepted cancellation still permitted a clean prepared review")
	}
	if err := s.RecordWorker(ctx, run.ID, "thread", "turn", "requested", "effective", nil, nil); err != nil {
		t.Fatal("cancellation lost available worker identity")
	}
}

func TestAcceptedCancellationWinsOverPreviouslyChosenWorkerFailure(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	run, _, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "cancel-before-failure", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
		t.Fatal("claim failed")
	}
	// The owner has selected failure, but has not persisted its terminal state.
	if requested, err := s.CancellationRequested(ctx, run.ID); err != nil || requested {
		t.Fatal("unexpected initial cancellation")
	}
	var sequence int
	var name, path string
	if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if acknowledged, err := other.RequestCancellation(ctx, run.ID); err != nil || acknowledged.State != Reviewing || !acknowledged.CancellationRequested {
		t.Fatalf("active cancellation was not acknowledged: %+v %v", acknowledged, err)
	}
	const diagnostic = "worker exited before producing a valid result"
	if err := s.Fail(ctx, run.ID, Failed, diagnostic); err != nil {
		t.Fatal(err)
	}
	finished, err := other.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != Cancelled || !finished.CancellationRequested {
		t.Fatalf("accepted cancellation lost during finalization: state=%s requested=%v", finished.State, finished.CancellationRequested)
	}
	if finished.Failure != diagnostic || finished.InboxItemID != item.ID || len(finished.Prepared) != 0 {
		t.Fatal("cancellation lost the worker diagnostic or originating item, or prepared a review")
	}
}

func TestCancellationAndPreparationHaveOneTransactionalWinner(t *testing.T) {
	for range 5 {
		s, item := schedulingFixture(t)
		ctx := context.Background()
		run, _, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "race", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
			t.Fatal("claim failed")
		}
		var sequence int
		var name, path string
		if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		other, err := Open(ctx, filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		cancelled, prepared := make(chan bool, 1), make(chan bool, 1)
		go func() { <-start; _, err := other.RequestCancellation(ctx, run.ID); cancelled <- err == nil }()
		go func() {
			<-start
			prepared <- s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn") == nil
		}()
		close(start)
		accepted, completed := <-cancelled, <-prepared
		other.Close()
		if accepted == completed {
			t.Fatalf("cancellation and preparation must have one winner: accepted=%v completed=%v", accepted, completed)
		}
		current, err := s.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed && (current.State != Prepared || current.CancellationRequested) {
			t.Fatal("rejected cancellation changed prepared result")
		}
		if accepted && (current.State != Reviewing || !current.CancellationRequested || len(current.Prepared) != 0) {
			t.Fatal("accepted cancellation prepared a result or falsely confirmed worker exit")
		}
	}
}
