package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func pickupConfig(version string, project bool) []byte {
	effort, timeout := "medium", "1m"
	if version == "new" {
		effort, timeout = "high", "2m"
	}
	text := fmt.Sprintf(`schema_version=1
[github]
repositories=['owner/repo']
[reviewer]
skill='review-%s'
model='model-%s'
reasoning_effort='%s'
[review]
instructions='instructions-%s'
[feedback]
authors=['author']
action='auto'
skill='feedback-%s'
model='model-%s'
reasoning_effort='%s'
instructions='instructions-%s'
[limits]
review_timeout='%s'
feedback_timeout='%s'
`, version, version, effort, version, version, version, effort, version, timeout, timeout)
	if project {
		text += fmt.Sprintf(`
[projects."owner/repo".feedback]
skill='project-feedback-%s'
model='project-model-%s'
reasoning_effort='%s'
instructions='project-instructions-%s'
[projects."owner/repo".limits]
feedback_timeout='%s'
`, version, version, effort, version, timeout)
	}
	return []byte(text)
}

type preparingRemote struct {
	leaseRemote
	entered chan context.Context
	release <-chan struct{}
	calls   atomic.Int32
}

func (r *preparingRemote) Identity(ctx context.Context) (github.Identity, error) {
	if r.calls.Add(1) == 1 {
		r.entered <- ctx
		select {
		case <-r.release:
		case <-ctx.Done():
			return github.Identity{}, ctx.Err()
		}
	}
	return r.leaseRemote.Identity(ctx)
}

func pickupEvent(t *testing.T, ctx context.Context, durable *store.Store, lane, event string) {
	t.Helper()
	before, err := durable.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ActorID, after.RepositoryID, after.State, after.Initialized = 43, 99, "ok", true
	item := store.InboxItem{Repository: after.Repository, ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", State: "queued", Action: "auto", Trigger: "review_requested", EventKey: event}
	if lane == "feedback" {
		item.Category, item.ExecutionKind, item.Trigger = "feedback_handling", "feedback_v1", "review_submitted"
	}
	record, _ := json.Marshal(map[string]any{"pull": github.Pull{Author: github.Identity{ID: 42, Login: "author"}}})
	if err := durable.ApplyDiscovery(ctx, before, after, []store.Observation{{PullID: 700, Number: 17, Record: record}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
}

func assertPickupSelection(t *testing.T, run store.Run, request agent.Request, lane, version string, project, manual bool) {
	t.Helper()
	prefix, instructionsKey := "reviewer.", "review.instructions"
	if lane == "feedback" {
		prefix, instructionsKey = "feedback.", "feedback.instructions"
	}
	skill, model, instructions := lane+"-"+version, "model-"+version, "instructions-"+version
	if project {
		skill, model, instructions = "project-"+skill, "project-"+model, "project-"+instructions
	}
	effort, timeout := "medium", time.Minute
	if version == "new" {
		effort, timeout = "high", 2*time.Minute
	}
	if manual {
		model, effort = "manual-model", "low"
	}
	if request.Skill != skill || request.Model != model || request.Effort != effort || request.Instructions != instructions {
		t.Fatalf("picked-up selection changed: %+v", request)
	}
	var policy config.Snapshot
	if err := json.Unmarshal(run.Configuration, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.String(prefix+"skill") != skill || policy.String(prefix+"model") != model || policy.String(prefix+"reasoning_effort") != effort || policy.String(instructionsKey) != instructions || policy.Duration("limits."+lane+"_timeout") != timeout {
		t.Fatalf("persisted selection differs from picked-up request: %s", run.Configuration)
	}
	if run.State != store.AgentCompleted || run.Executor != store.NativeExecutor || run.Purpose != lane || run.ThreadID == "" || run.RequestedModel != model {
		t.Fatalf("lost factual native completion: %+v", run)
	}
}

func TestPickedUpSettingsSurviveReloadDuringPreparation(t *testing.T) {
	for _, tc := range []struct {
		lane           string
		project, early bool
	}{{lane: "review"}, {lane: "feedback", project: true}, {lane: "review", early: true}} {
		t.Run(fmt.Sprintf("%s/project=%t/early_parent=%t", tc.lane, tc.project, tc.early), func(t *testing.T) {
			parentBudget := 3 * time.Minute
			if tc.early {
				parentBudget = 30 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), parentBudget)
			defer cancel()
			durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			manager, err := config.NewManager(pickupConfig("old", tc.project), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			pickupEvent(t, ctx, durable, tc.lane, "first-event")
			entered, release := make(chan context.Context, 1), make(chan struct{})
			remote := &preparingRemote{entered: entered, release: release}
			type invocation struct {
				request  agent.Request
				deadline time.Time
			}
			requests := make(chan invocation, 3)
			var calls atomic.Int32
			service := dispatch.Service{Store: durable, GitHub: remote, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(worker context.Context, request agent.Request) (agent.Output, error) {
				deadline, _ := worker.Deadline()
				requests <- invocation{request, deadline}
				return agent.Output{ThreadID: fmt.Sprintf("pickup-session-%d", calls.Add(1)), Completed: true}, nil
			}}
			client, _ := github.New(github.Options{Host: "github.com", Token: "fixture-token"})
			engine := Engine{Store: durable, Configuration: manager, client: client, Execute: func(worker context.Context, document *config.Document, _ *github.Client, item store.InboxItem) (store.Run, error) {
				return service.Run(worker, document, dispatch.Job{Target: github.PullReference{Repository: item.Repository, Number: item.Number}, Purpose: tc.lane, Item: &item})
			}}
			engine.initialize()
			engine.admitted = map[string]bool{}
			defer engine.Close()
			pickupAt := time.Now()
			if err := engine.Admit(ctx); err != nil {
				t.Fatal(err)
			}
			var preparing context.Context
			select {
			case preparing = <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("job never entered GitHub preparation")
			}
			preparedAt := time.Now()
			deadline, ok := preparing.Deadline()
			if !ok {
				t.Fatal("preparation lost selected timeout")
			}
			if tc.early {
				parentDeadline, _ := ctx.Deadline()
				if !deadline.Equal(parentDeadline) {
					t.Fatal("selected timeout extended earlier caller deadline")
				}
			} else if deadline.Before(pickupAt.Add(time.Minute)) || deadline.After(preparedAt.Add(time.Minute)) {
				t.Fatalf("preparation did not start the selected one-minute budget: %v", deadline)
			}
			if err := manager.Reload(pickupConfig("new", tc.project)); err != nil {
				t.Fatal(err)
			}
			if err := engine.Admit(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err := durable.List(ctx)
			if err != nil || len(runs) != 0 || calls.Load() != 0 || preparing.Err() != nil || engine.Active() != 1 {
				t.Fatalf("reload cancelled/duplicated preparing work or prematurely created a run: runs=%d calls=%d active=%d err=%v", len(runs), calls.Load(), engine.Active(), err)
			}
			close(release)
			if err := engine.Complete(ctx, true); err != nil {
				t.Fatal(err)
			}
			first := <-requests
			if !first.deadline.Equal(deadline) {
				t.Fatal("reload or launch restarted preparation's accepted timeout")
			}
			runs, err = durable.List(ctx)
			if err != nil || len(runs) != 1 {
				t.Fatalf("wrong first execution count: %d %v", len(runs), err)
			}
			assertPickupSelection(t, runs[0], first.request, tc.lane, "old", tc.project, false)
			pickupEvent(t, ctx, durable, tc.lane, "second-event")
			nextPickupAt := time.Now()
			if err := engine.Admit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := engine.Complete(ctx, true); err != nil {
				t.Fatal(err)
			}
			second := <-requests
			if tc.early {
				parentDeadline, _ := ctx.Deadline()
				if !second.deadline.Equal(parentDeadline) {
					t.Fatal("later pickup extended the caller deadline")
				}
			} else if second.deadline.Before(nextPickupAt.Add(2*time.Minute)) || second.deadline.After(time.Now().Add(2*time.Minute)) {
				t.Fatal("later pickup did not receive its new two-minute budget")
			}
			runs, err = durable.List(ctx)
			if err != nil || len(runs) != 2 || calls.Load() != 2 || engine.Active() != 0 {
				t.Fatalf("later event not executed exactly once: runs=%d calls=%d %v", len(runs), calls.Load(), err)
			}
			foundSecond := false
			for _, run := range runs {
				if run.ThreadID == "pickup-session-2" {
					foundSecond = true
					assertPickupSelection(t, run, second.request, tc.lane, "new", tc.project, false)
				}
			}
			if !foundSecond {
				t.Fatal("second event lost its distinct native session")
			}
		})
	}
}

func TestManualURLRetainsSelectedSettingsWhilePRIsBusy(t *testing.T) {
	for _, lane := range []string{"review", "feedback"} {
		t.Run(lane, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			manager, err := config.NewManager(pickupConfig("old", false), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			locks := filepath.Join(t.TempDir(), "locks")
			lease, err := dispatch.AcquireExecutionLease(ctx, locks, dispatch.ExecutionLeaseKey{Host: "github.com", RepositoryID: 99, PullID: 700})
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			entered, release := make(chan context.Context, 1), make(chan struct{})
			close(release)
			remote := &preparingRemote{entered: entered, release: release}
			requests := make(chan agent.Request, 1)
			service := dispatch.Service{Store: durable, GitHub: remote, LockDirectory: locks, Launch: func(_ context.Context, request agent.Request) (agent.Output, error) {
				requests <- request
				return agent.Output{ThreadID: "manual-session", Completed: true}, nil
			}}
			prefix := "reviewer."
			if lane == "feedback" {
				prefix = "feedback."
			}
			document := manager.Document()
			var run store.Run
			var runErr error
			done := make(chan struct{})
			go func() {
				run, runErr = service.Run(ctx, document, dispatch.Job{Target: github.PullReference{Repository: "github.com/owner/repo", Number: 17}, Purpose: lane, Overrides: map[string]any{prefix + "model": "manual-model", prefix + "reasoning_effort": "low"}})
				close(done)
			}()
			defer func() {
				cancel()
				_ = lease.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("manual test worker did not stop")
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("manual invocation never prepared")
			}
			if err := manager.Reload(pickupConfig("new", false)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requests:
				t.Fatal("manual invocation overlapped the held PR lease")
			case <-time.After(50 * time.Millisecond):
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("manual invocation did not finish after PR became available")
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			assertPickupSelection(t, run, <-requests, lane, "old", false, true)
			items, _, err := durable.Inbox(ctx)
			if err != nil || len(items) != 0 {
				t.Fatalf("manual URL fabricated discovered work: %+v %v", items, err)
			}
		})
	}
}
