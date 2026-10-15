package review

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/notify"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

func retentionFixture(t *testing.T, state store.State, verified bool) (*store.Store, store.Run, string) {
	t.Helper()
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	policy, err := config.Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	captured, _ := json.Marshal(Boundary{Pull: github.Pull{PullReference: github.PullReference{Repository: "github.com/owner/repo", Number: 1}, ID: 701, RepositoryID: 99, HeadSHA: strings.Repeat("b", 40), TargetBaseSHA: strings.Repeat("a", 40)}, MergeBaseSHA: strings.Repeat("c", 40)})
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "retention", State: store.Queued, Configuration: frozen, Context: captured})
	if err != nil {
		t.Fatal(err)
	}
	if state == store.Queued {
		return s, run, directory
	}
	if state == store.WaitingChecks {
		if err := s.WaitForChecks(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		if won, err := s.Claim(ctx, run.ID); err != nil || !won {
			t.Fatalf("claim: %t %v", won, err)
		}
		if err := s.RecordWorker(ctx, run.ID, "thread", "turn", "gpt-6.1-sol", "gpt-6.1-sol", json.RawMessage(`{"tokens":5}`), json.RawMessage(`[{"task":"private-task-canary","text":"private-leaf-canary","thread_id":"leaf","turn_id":"leaf-turn","effort":"high"}]`)); err != nil {
			t.Fatal(err)
		}
		if state == store.Failed || state == store.Cancelled {
			if err := s.Fail(ctx, run.ID, state, "fixture"); err != nil {
				t.Fatal(err)
			}
		} else if state != store.Reviewing {
			boundary := Boundary{Pull: github.Pull{PullReference: github.PullReference{Repository: run.Repository, Number: 1, URL: "https://github.com/owner/repo/pull/1"}, ID: 701, RepositoryID: 99, Author: github.Identity{ID: 42, Login: "author"}, HeadSHA: strings.Repeat("b", 40), TargetBaseSHA: strings.Repeat("a", 40)}, Actor: github.Identity{ID: 43, Login: "reviewer"}, Configuration: policy, MergeBaseSHA: strings.Repeat("c", 40), ContextDigest: "context", Skill: SkillIdentity{Name: "requesting-code-review", Digest: "skill"}, Anchors: source.AnchorSet{{Path: "file.go", Side: "RIGHT", Start: 1, End: 1}}, Checks: github.CheckReport{Ready: true, Checks: []github.MandatoryCheck{}}}
			prepared, err := Prepare(boundary, Report{Conclusion: "APPROVE", Summary: "private-source-canary", Findings: []Finding{}, Gaps: []ResultGap{}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(prepared)
			if err := s.Prepare(ctx, run.ID, prepared.PayloadID, raw, "thread", "turn"); err != nil {
				t.Fatal(err)
			}
			if state != store.Prepared {
				if err := s.DecidePublication(ctx, run.ID, prepared.PayloadID); err != nil {
					t.Fatal(err)
				}
				if _, won, err := s.BeginPublication(ctx, run.ID, prepared.PayloadID); err != nil || !won {
					t.Fatalf("publication claim: %t %v", won, err)
				}
				if state != store.Publishing {
					current, err := s.Get(ctx, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					outcome := PublicationOutcome{Verified: verified, ReviewedHead: boundary.Pull.HeadSHA, CurrentHead: boundary.Pull.HeadSHA, Review: github.RemoteReview{ID: 71, URL: "https://github.com/owner/repo/pull/1#pullrequestreview-71", User: boundary.Actor, CommitID: boundary.Pull.HeadSHA, State: "APPROVED", Body: "private-remote-review-canary"}, Comments: []github.RemoteComment{{ID: 72, ReviewID: 71, URL: "https://github.com/owner/repo/pull/1#discussion_r72", Body: "private-remote-comment-canary"}}, Repairs: []github.RemoteComment{{ID: 73, ReviewID: 74, URL: "https://github.com/owner/repo/pull/1#discussion_r73", Body: "private-repair-canary"}}}
					data, _ := json.Marshal(outcome)
					if err := s.MarkOutcome(ctx, current, state, data); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, current, directory
}

func TestRetentionPreservesAllUnresolvedWorkAndPublicationEvidence(t *testing.T) {
	for _, state := range []store.State{store.Queued, store.WaitingChecks, store.Reviewing, store.Prepared, store.Publishing, store.Partial, store.PublicationUnknown, store.Stale} {
		t.Run(string(state), func(t *testing.T) {
			s, before, _ := retentionFixture(t, state, false)
			if state == store.Prepared {
				if err := s.DecidePublication(context.Background(), before.ID, before.PayloadID); err != nil {
					t.Fatal(err)
				}
			}
			report, err := ExpireHistory(context.Background(), s, 1, false, before.UpdatedAt.Add(48*time.Hour))
			if err != nil || len(report.Expired) != 0 {
				t.Fatalf("unresolved %s expired: %+v %v", state, report, err)
			}
			after, err := s.Get(context.Background(), before.ID)
			old, _ := json.Marshal(before)
			current, _ := json.Marshal(after)
			if err != nil || string(old) != string(current) {
				t.Fatal("retention changed unresolved evidence or identity")
			}
			if state == store.Prepared {
				if _, won, err := s.BeginPublication(context.Background(), before.ID, before.PayloadID); err != nil || !won {
					t.Fatal("retention lost a pending exact-preview decision")
				}
			}
		})
	}
}

func TestRetentionKeepsPublicationOwnershipAndCannotEnableReposting(t *testing.T) {
	ctx := context.Background()
	s, before, _ := retentionFixture(t, store.Published, true)
	document, err := config.Parse([]byte("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[notifications]\ndesktop=false\n"), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	notifications := notify.Service{Store: s, Terminal: func(context.Context, notify.Message) error { messages++; return nil }}
	if err := notifications.Pump(ctx, document, true); err != nil || messages != 1 {
		t.Fatal("fixture publication notification was not delivered")
	}
	now := before.UpdatedAt.Add(48 * time.Hour)
	report, err := ExpireHistory(ctx, s, 1, false, now)
	if err != nil || len(report.Expired) != 1 {
		t.Fatalf("verified history did not expire: %+v %v", report, err)
	}
	after, err := s.Get(ctx, before.ID)
	if err != nil || after.Archive == nil || !after.Archive.Reviewed || after.State != store.Published || len(after.Prepared) != 0 || after.ThreadID != before.ThreadID || after.PayloadID != before.PayloadID || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("expiry changed publication or review identity: %+v %v", after, err)
	}
	data, _ := json.Marshal(after)
	if strings.Contains(string(data), "canary") || !strings.Contains(string(after.Specialists), "leaf-turn") {
		t.Fatal("private text survived or sealed leaf identity was discarded")
	}
	var outcome PublicationOutcome
	if json.Unmarshal(after.Outcome, &outcome) != nil || outcome.Review.ID != 71 || outcome.Review.URL == "" || len(outcome.Comments) != 1 || len(outcome.Repairs) != 1 || !outcome.Verified {
		t.Fatal("verified root/comment/repair receipts were discarded")
	}
	remote := github.ReviewContext{Reviews: []json.RawMessage{json.RawMessage(`{"id":71}`), json.RawMessage(`{"id":74}`), json.RawMessage(`{"id":999,"body":"human"}`)}, Comments: []json.RawMessage{json.RawMessage(`{"id":72}`), json.RawMessage(`{"id":73}`), json.RawMessage(`{"id":1000,"body":"human"}`)}}
	filtered, err := EventContext(remote, github.Pull{PullReference: github.PullReference{Repository: before.Repository}, ID: before.PullID}, github.Identity{ID: before.ActorID}, []store.Run{after})
	if err != nil || len(filtered.Reviews) != 1 || len(filtered.Comments) != 1 || !strings.Contains(string(filtered.Reviews[0]), "999") || !strings.Contains(string(filtered.Comments[0]), "1000") {
		t.Fatal("expiry turned owned publications into new feedback or hid human feedback")
	}
	// Archived previews fail locally before any attempt to reconcile or submit.
	if _, err := (Publisher{Store: s, GitHub: new(github.Client)}).Publish(ctx, nil, after.ID, after.PayloadID, false); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal("archived publication did not fail with an actionable local boundary")
	}
	repeated, err := ExpireHistory(ctx, s, 1, false, now.Add(time.Hour))
	if err != nil || len(repeated.Expired) != 0 {
		t.Fatal("repeated cleanup changed the same history")
	}
	if err := notifications.Pump(ctx, document, true); err != nil || messages != 1 {
		t.Fatal("private-text expiry replayed a historical publication notification")
	}
}

func TestRetentionAgeBoundaryAndConcurrentClientsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	s, before, directory := retentionFixture(t, store.Cancelled, false)
	boundary := before.UpdatedAt.Add(24 * time.Hour)
	for _, now := range []time.Time{boundary.Add(-time.Nanosecond), boundary} {
		report, err := ExpireHistory(ctx, s, 1, false, now)
		if err != nil || len(report.Expired) != 0 {
			t.Fatal("retention expired at or before the age boundary")
		}
	}
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	other, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, client := range []*store.Store{s, other} {
		wg.Add(1)
		go func(client *store.Store) {
			defer wg.Done()
			report, err := ExpireHistory(ctx, client, 1, false, boundary.Add(time.Second))
			if err != nil {
				t.Error(err)
			}
			counts <- len(report.Expired)
		}(client)
	}
	wg.Wait()
	if count := <-counts + <-counts; count != 1 {
		t.Fatalf("concurrent retention claimed %d expiries", count)
	}
	request := before
	request.State = store.Queued
	_, created, err := s.Create(ctx, request)
	if err != nil || created {
		t.Fatal("expiry removed logical work deduplication")
	}
}

func TestRetentionCannotErasePublicationEvidenceFromAStaleRead(t *testing.T) {
	ctx := context.Background()
	s, before, _ := retentionFixture(t, store.Published, true)
	var changed PublicationOutcome
	if err := json.Unmarshal(before.Outcome, &changed); err != nil {
		t.Fatal(err)
	}
	changed.CurrentHead = strings.Repeat("d", 40)
	newEvidence, _ := json.Marshal(changed)
	if err := s.MarkOutcome(ctx, before, store.Published, newEvidence); err != nil {
		t.Fatal(err)
	}
	cutoff := before.UpdatedAt.Add(24 * time.Hour)
	won, err := s.ExpireArtifacts(ctx, before, store.Archive{ExpiredAt: cutoff.Add(24 * time.Hour).Format(time.RFC3339Nano)}, json.RawMessage(`{}`), json.RawMessage(`[]`), cutoff)
	if err != nil || won {
		t.Fatal("stale expiry erased a newer publication observation")
	}
	after, err := s.Get(ctx, before.ID)
	if err != nil || after.Archive != nil || string(after.Outcome) != string(newEvidence) || string(after.Prepared) != string(before.Prepared) {
		t.Fatal("stale maintenance changed durable review evidence")
	}
}

func TestRetentionPreservesOpenInputEvenAfterItsRunBecomesTerminal(t *testing.T) {
	ctx := context.Background()
	s, run, _ := retentionFixture(t, store.Reviewing, false)
	request := clarification.Request{ID: "open-input", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "pending private question"}}}
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, store.Failed, "interrupted before input closure"); err != nil {
		t.Fatal(err)
	}
	now := run.UpdatedAt.Add(48 * time.Hour)
	report, err := ExpireHistory(ctx, s, 1, false, now)
	if err != nil || len(report.Expired) != 0 {
		t.Fatal("terminal state hid unresolved input delivery")
	}
	before, err := s.Get(ctx, run.ID)
	if err != nil || len(before.Inputs) != 1 || len(before.Inputs[0].Questions) != 1 {
		t.Fatal("unresolved question was lost")
	}
	if err := s.CloseInputs(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	report, err = ExpireHistory(ctx, s, 1, false, now)
	if err != nil || len(report.Expired) != 1 {
		t.Fatal("resolved terminal history remained ineligible forever")
	}
}

func TestRetentionKeepsFailedCapturedBoundaryWithoutInventingACompletedReview(t *testing.T) {
	s, run, _ := retentionFixture(t, store.Failed, false)
	if _, err := ExpireHistory(context.Background(), s, 1, false, run.UpdatedAt.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(context.Background(), run.ID)
	if err != nil || after.Archive == nil || after.Archive.Reviewed || after.Archive.HeadSHA != strings.Repeat("b", 40) || after.Archive.TargetBaseSHA != strings.Repeat("a", 40) || len(after.Context) != 0 {
		var full Boundary
		decodeErr := json.Unmarshal(run.Context, &full)
		t.Fatalf("expiry lost the captured failure boundary or labelled it completed: archive=%+v context_decode=%v read=%v", after.Archive, decodeErr, err)
	}
}

func TestRetentionProcessesBoundedBatchesWithoutDeletingWorkIdentity(t *testing.T) {
	ctx := context.Background()
	s, original, _ := retentionFixture(t, store.Cancelled, false)
	for i := 1; i < 30; i++ {
		request := original
		request.WorkKey = fmt.Sprintf("history-%d", i)
		request.State = store.Queued
		run, created, err := s.Create(ctx, request)
		if err != nil || !created {
			t.Fatalf("seed independent history: %t %v", created, err)
		}
		if err := s.Fail(ctx, run.ID, store.Cancelled, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	now := original.UpdatedAt.Add(48 * time.Hour)
	first, err := ExpireHistory(ctx, s, 1, false, now)
	if err != nil || len(first.Expired) != 25 || !first.BatchFull {
		t.Fatalf("first expiry was not bounded: %+v %v", first, err)
	}
	second, err := ExpireHistory(ctx, s, 1, false, now)
	if err != nil || len(second.Expired) != 5 || second.BatchFull {
		t.Fatalf("remaining expiry was lost: %+v %v", second, err)
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 30 {
		t.Fatal("bounded expiry deleted logical work identities")
	}
	for _, run := range runs {
		if run.Archive == nil || run.State != store.Cancelled {
			t.Fatal("bounded passes lost a terminal summary")
		}
	}
}

func TestRetentionDoesNotDecodeFeedbackFactsAsReviewPublication(t *testing.T) {
	for _, state := range []store.State{store.Failed, store.Cancelled} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			owner, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			run, created, err := durable.Create(ctx, store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "author", WorkKey: "feedback-retention", State: store.Queued, Configuration: json.RawMessage(`{"feedback_authority":"frozen"}`), Context: json.RawMessage(`{"feedback_target":"original"}`)})
			if err != nil || !created {
				t.Fatal(err)
			}
			if claimed, err := durable.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			journal := json.RawMessage(`{"schema_version":1,"execution":{"roles":[{"role":"correctness_reviewer","thread_id":"native-thread","text":"Private native report"}],"delivery":{"started":true,"complete":false,"effects":[{"kind":"reply","state":"unknown"}]}},"handled":[{"key":"review:8","revision":"immutable"}]}`)
			if err := durable.FinishFeedback(ctx, run.ID, state, journal, "owned failure after ambiguous reply"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				report, err := ExpireHistory(ctx, durable, 1, false, time.Now().Add(48*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				actual, err := durable.Get(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Expired) != 0 || actual.Archive != nil || string(actual.Outcome) != string(journal) || string(actual.Configuration) != string(run.Configuration) || string(actual.Context) != string(run.Context) {
					t.Fatal("review retention discarded feedback execution/replay evidence")
				}

			}
		})
	}
}
