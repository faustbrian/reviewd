package watch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

type leaseRemote struct{}

func (leaseRemote) Identity(context.Context) (github.Identity, error) {
	return github.Identity{ID: 43, Login: "operator"}, nil
}
func (leaseRemote) Pull(_ context.Context, repo string, n int) (github.Pull, error) {
	return github.Pull{PullReference: github.PullReference{Repository: repo, Number: n, URL: "https://github.com/owner/repo/pull/17"}, RepositoryID: 99, ID: 700, State: "open", Author: github.Identity{ID: 42, Login: "author"}}, nil
}
func (leaseRemote) PullPreferences(context.Context, github.Pull) (*config.RepositoryPreferences, error) {
	return nil, nil
}

func TestBusyDiscoveredSkillsUseFreshSettingsOnNextAdmission(t *testing.T) {
	for _, lane := range []string{"review", "feedback"} {
		t.Run(lane, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			root := t.TempDir()
			first, err := store.Open(ctx, filepath.Join(root, "first"))
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := store.Open(ctx, filepath.Join(root, "second"))
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			configuration := func(version string) []byte { return pickupConfig(version, false) }
			manager, err := config.NewManager(configuration("old"), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			scope, err := second.DiscoveryScope(ctx, "github.com/owner/repo")
			if err != nil {
				t.Fatal(err)
			}
			after := scope
			after.ActorID = 43
			after.RepositoryID = 99
			after.State = "ok"
			after.Initialized = true
			item := store.InboxItem{Repository: after.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", State: "queued", Action: "auto", Trigger: "review_requested", EventKey: "event"}
			if lane == "feedback" {
				item.Category = "feedback_handling"
				item.ExecutionKind = "feedback_v1"
				item.Trigger = "review_submitted"
			}
			record, _ := json.Marshal(map[string]any{"pull": github.Pull{Author: github.Identity{ID: 42, Login: "author"}}})
			if err := second.ApplyDiscovery(ctx, scope, after, []store.Observation{{PullID: 700, Number: 17, Record: record}}, []store.InboxItem{item}); err != nil {
				t.Fatal(err)
			}
			locks := filepath.Join(root, "locks")
			started, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			firstService := dispatch.Service{Store: first, GitHub: leaseRemote{}, LockDirectory: locks, Launch: func(worker context.Context, _ agent.Request) (agent.Output, error) {
				close(started)
				select {
				case <-release:
					return agent.Output{ThreadID: "first-session", Completed: true}, nil
				case <-worker.Done():
					return agent.Output{}, worker.Err()
				}
			}}
			go func() {
				_, err := firstService.Run(ctx, manager.Document(), dispatch.Job{Target: github.PullReference{Repository: after.Repository, Number: 17}})
				firstDone <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first profile never started")
			}
			requests := make(chan agent.Request, 2)
			secondService := dispatch.Service{Store: second, GitHub: leaseRemote{}, LockDirectory: locks, Launch: func(worker context.Context, r agent.Request) (agent.Output, error) {
				deadline, ok := worker.Deadline()
				if !ok || time.Until(deadline) < 110*time.Second || time.Until(deadline) > 121*time.Second {
					t.Errorf("old timeout survived reload: %v", deadline)
				}
				requests <- r
				return agent.Output{ThreadID: "second-session", Completed: true}, nil
			}}
			client, _ := github.New(github.Options{Host: "github.com", Token: "fixture-token"})
			engine := Engine{Store: second, Configuration: manager, client: client, Execute: func(worker context.Context, doc *config.Document, _ *github.Client, selected store.InboxItem) (store.Run, error) {
				return secondService.Run(worker, doc, dispatch.Job{Target: github.PullReference{Repository: selected.Repository, Number: selected.Number}, Purpose: lane, Item: &selected})
			}}
			engine.initialize()
			engine.admitted = map[string]bool{}
			defer engine.Close()
			if err := engine.Admit(ctx); err != nil {
				t.Fatal(err)
			}
			// A busy PR is a future start, not an accepted skill or a failed attempt.
			bounded, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			err = engine.Complete(bounded, true)
			stop()
			if err != nil {
				t.Fatalf("busy discovered work did not defer promptly: %v", err)
			}
			rows, _, err := second.Inbox(ctx)
			if err != nil || len(rows) != 1 || rows[0].RunID != "" || rows[0].Attempts != 0 || rows[0].Failure != "" || !rows[0].RetryAt.IsZero() || rows[0].State != "queued" {
				t.Fatalf("contention consumed/failed queued work: %+v %v", rows, err)
			}
			runs, err := second.List(ctx)
			if err != nil || len(runs) != 0 {
				t.Fatalf("contention created execution: %+v %v", runs, err)
			}
			if err := manager.Reload(configuration("new")); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			// Tick resets this admission guard before its next discovery/admission pass.
			engine.admitted = map[string]bool{}
			if err := engine.Admit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := engine.Complete(ctx, true); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-requests:
				skill := "review-new"
				prefix := "reviewer."
				if lane == "feedback" {
					skill = "feedback-new"
					prefix = "feedback."
				}
				if request.Backend != "claude" || request.Executable != "claude" || request.Skill != skill || request.Model != "model-new" || request.Effort != "high" || request.Instructions != "instructions-new" {
					t.Fatalf("stale launch after reload: %+v", request)
				}
				runs, err = second.List(ctx)
				if err != nil || len(runs) != 1 {
					t.Fatalf("wrong run count: %+v %v", runs, err)
				}
				var snapshot config.Snapshot
				if json.Unmarshal(runs[0].Configuration, &snapshot) != nil || snapshot.String(prefix+"backend") != "claude" || snapshot.String(prefix+"skill") != skill || snapshot.String(prefix+"model") != "model-new" || snapshot.String(prefix+"reasoning_effort") != "high" || snapshot.String(map[string]string{"review": "review.instructions", "feedback": "feedback.instructions"}[lane]) != "instructions-new" || snapshot.Duration("limits."+map[string]string{"review": "review", "feedback": "feedback"}[lane]+"_timeout") != 2*time.Minute {
					t.Fatalf("persisted stale settings: %s", runs[0].Configuration)
				}
			default:
				t.Fatal("fresh skill never launched")
			}
			if engine.Active() != 0 {
				t.Fatal("contention retained capacity")
			}
		})
	}
}
