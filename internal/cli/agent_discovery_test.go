package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	watcher "github.com/faustbrian/reviewd/internal/watch"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnAgentReviewDoesNotCreateAnotherAutomaticReview(t *testing.T) {
	phase := 0
	invoke, _, _ := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pull := lifecyclePull(1, "open", true, false)
		if phase > 0 {
			pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
		}
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			value = []any{pull}
		case "/repos/owner/repo/pulls/1/reviews":
			if phase > 1 {
				value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 43, "login": "reviewer"}, "state": "COMMENTED", "body": "Agent's review output.", "submitted_at": "2026-10-18T00:00:00Z"}}
			}
		case "/graphql":
			value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	invoke("sync")
	phase = 1
	before := invoke("sync")
	if len(before.Items) != 1 || before.Items[0].State != "queued" {
		t.Fatalf("new head did not queue: %+v", before)
	}
	phase = 2
	after := invoke("sync")
	if len(after.Items) != 1 || after.Items[0].EventKey != before.Items[0].EventKey {
		t.Fatalf("agent's own publication retriggered automatic review: before=%+v after=%+v", before, after)
	}
}

func TestFollowedRequestRemovalDoesNotLaunchAnUnchangedReview(t *testing.T) {
	for _, following := range []bool{true, false} {
		t.Run(fmt.Sprintf("active_following=%t", following), func(t *testing.T) {
			testRequestRemoval(t, following)
		})
	}
}

func testRequestRemoval(t *testing.T, keepFollowing bool) {
	t.Setenv("HOME", t.TempDir())
	phase := 0
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pull := lifecyclePull(1, "open", phase == 1 || phase == 4, false)
		if phase >= 3 {
			pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
		}
		var value any = []any{}
		base, tree := strings.Repeat("b", 40), strings.Repeat("d", 40)
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			value = []any{pull}
		case "/repos/owner/repo/pulls/1":
			value = pull
		case "/repos/owner/repo/commits/" + base:
			value = map[string]any{"sha": base, "commit": map[string]any{"tree": map[string]any{"sha": tree}}}
		case "/repos/owner/repo/git/trees/" + tree:
			value = map[string]any{"sha": tree, "truncated": false, "tree": []any{}}
		case "/repos/owner/repo/pulls/1/reviews":
			if phase >= 5 {
				value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 43, "login": "reviewer"}, "state": "COMMENTED", "body": "The running skill posted its review.", "submitted_at": "2026-10-18T00:00:00Z"}}
			}
		case "/graphql":
			value = lifecycleGraphQL(t, r, func(int) []any {
				if phase == 0 {
					return []any{}
				}
				rows := []any{map[string]any{"id": "request-one", "createdAt": "2026-10-18T00:00:00Z", "requestedReviewer": map[string]any{"__typename": "User", "databaseId": 43}}}
				if phase >= 4 {
					rows = append(rows, map[string]any{"id": "request-two", "createdAt": "2026-10-18T00:01:00Z", "requestedReviewer": map[string]any{"__typename": "User", "databaseId": 43}})
				}
				return rows
			})
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("\n[watch]\ndebounce='0s'\n[notifications]\ndesktop=false\nterminal=false\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("test configuration: %v %v", writeErr, closeErr)
	}
	invoke("sync")
	phase = 1
	requested := invoke("sync")
	if len(requested.Items) != 1 || requested.Items[0].State != "queued" || requested.Items[0].Baseline {
		t.Fatalf("new request did not queue: %+v", requested)
	}
	invoke("follow", requested.Items[0].ID)
	invoke("sync") // Persist following while the unstarted request is still present.
	phase = 2
	var calls atomic.Int32
	deps.LaunchAgent = func(context.Context, agent.Request) (agent.Output, error) {
		calls.Add(1)
		return agent.Output{ThreadID: "followed-session", Completed: true}, nil
	}
	watch := func() {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &out, &diagnostics, deps); code != 0 {
			t.Fatalf("watch: %d %s", code, diagnostics.String())
		}
	}
	for i := 0; i < 2; i++ {
		if got := invoke("sync"); len(got.Items) != 0 {
			t.Fatalf("removed request retained actionable follow work: %+v", got)
		}
		watch()
		if calls.Load() != 0 {
			t.Fatal("unchanged followed PR launched after its request was removed")
		}
	}
	phase = 3
	watch()
	if calls.Load() != 1 {
		t.Fatalf("genuine followed head change did not launch exactly once: %d", calls.Load())
	}

	// Re-enter through the CLI's real executor, keeping the native skill alive
	// while discovery observes its own publication clearing the review request.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(raw, path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.ResolveDefaults(manager.Document(), nil)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := stateDirectory(policy, path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	durable, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	if !keepFollowing {
		if err := durable.SetFollow(ctx, requested.Items[0].ID, false); err != nil {
			t.Fatal(err)
		}
	}
	started, finish := make(chan context.Context, 1), make(chan struct{})
	deps.LaunchAgent = func(worker context.Context, request agent.Request) (agent.Output, error) {
		calls.Add(1)
		output := agent.Output{ThreadID: "still-running-session"}
		if err := request.OnProgress(output); err != nil {
			return output, err
		}
		started <- worker
		select {
		case <-finish:
			output.Completed = true
			return output, nil
		case <-worker.Done():
			return output, worker.Err()
		}
	}
	execution := &execution{}
	engine := watcher.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Execute: execution.itemExecutor(deps, durable, directory)}
	defer engine.Close()
	phase = 4
	if err := engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var worker context.Context
	select {
	case worker = <-started:
	case <-ctx.Done():
		t.Fatal("new review request did not start")
	}
	items, _, err := durable.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].RunID == "" {
		t.Fatalf("active review not linked: %+v %v", items, err)
	}
	runID := items[0].RunID
	phase = 5
	for i := 0; i < 2; i++ {
		if err := engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if worker.Err() != nil {
			t.Fatal("clearing its own request cancelled the running review skill")
		}
		run, readErr := durable.Get(ctx, runID)
		if readErr != nil || run.State != store.AgentRunning || run.ThreadID != "still-running-session" || calls.Load() != 2 {
			t.Fatalf("active skill changed state/session or duplicated: run=%+v calls=%d error=%v", run, calls.Load(), readErr)
		}
	}
	close(finish)
	if err := engine.Complete(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := durable.Get(ctx, runID)
	if err != nil || run.State != store.AgentCompleted || run.ThreadID != "still-running-session" || calls.Load() != 2 {
		t.Fatalf("completed skill was interrupted or replayed: %+v calls=%d %v", run, calls.Load(), err)
	}
}

func TestWatchRoutesBothSkillsAndNeverReplaysConsumedEvents(t *testing.T) {
	for _, lane := range []string{"review", "feedback"} {
		for _, action := range []string{"ignore", "notify", "ask", "auto"} {
			t.Run(lane+"/"+action, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				phase := 0
				invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					pull := lifecyclePull(1, "open", lane == "review", false)
					if lane == "feedback" {
						pull["user"] = map[string]any{"id": 43, "login": "reviewer"}
					} else if phase > 0 {
						pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
					}
					var value any = []any{}
					base, tree := strings.Repeat("b", 40), strings.Repeat("d", 40)
					switch r.URL.Path {
					case "/user":
						value = map[string]any{"id": 43, "login": "reviewer"}
					case "/repos/owner/repo":
						value = map[string]any{"id": 99, "full_name": "owner/repo"}
					case "/repos/owner/repo/pulls":
						value = []any{pull}
					case "/repos/owner/repo/pulls/1":
						value = pull
					case "/repos/owner/repo/commits/" + base:
						value = map[string]any{"sha": base, "commit": map[string]any{"tree": map[string]any{"sha": tree}}}
					case "/repos/owner/repo/git/trees/" + tree:
						value = map[string]any{"sha": tree, "truncated": false, "tree": []any{}}
					case "/repos/owner/repo/pulls/1/reviews":
						if lane == "feedback" {
							value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 9, "login": "other-reviewer"}, "state": "COMMENTED", "body": fmt.Sprintf("Human concern %d", phase), "submitted_at": "2026-10-18T00:00:00Z"}}
						}
					case "/graphql":
						value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
					}
					if err := json.NewEncoder(w).Encode(value); err != nil {
						t.Error(err)
					}
				}))
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				configuration := string(raw)
				if lane == "review" {
					configuration = strings.Replace(configuration, `head_updated = "auto"`, `head_updated = "`+action+`"`, 1)
				} else {
					configuration += "\n[feedback]\nauthors=['@me']\naction='" + action + "'\n"
				}
				configuration += "\n[watch]\ndebounce='0s'\n[notifications]\ndesktop=false\nterminal=false\n"
				if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				deps.LaunchAgent = func(_ context.Context, request agent.Request) (agent.Output, error) {
					calls.Add(1)
					want := "review"
					if lane == "feedback" {
						want = "handle-pr-feedback"
					}
					if request.Skill != want || request.URL != "https://github.com/owner/repo/pull/1" {
						t.Errorf("wrong discovered skill: %+v", request)
					}
					return agent.Output{ThreadID: "native-session", Text: "The skill owns delivery.", Completed: true}, nil
				}
				invoke("sync")
				phase = 1
				observed := invoke("sync")
				var selected *lifecycleItem
				for i := range observed.Items {
					item := &observed.Items[i]
					if (lane == "review" && item.Category == "reviews_owed") || (lane == "feedback" && item.Category == "feedback_handling") {
						selected = item
					}
				}
				runWatch := func() lifecycleInbox {
					t.Helper()
					var out, diagnostics bytes.Buffer
					if code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &out, &diagnostics, deps); code != 0 {
						t.Fatalf("watch failed: %d %s", code, diagnostics.String())
					}
					var result lifecycleInbox
					if err := json.Unmarshal(out.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					return result
				}
				runWatch()
				want := int32(0)
				if action == "auto" {
					want = 1
				}
				if calls.Load() != want {
					t.Fatalf("%s launched before its required choice: %d", action, calls.Load())
				}
				if action == "ask" {
					if selected == nil || selected.State != "pending_decision" {
						t.Fatalf("ask was not a durable choice: %+v", observed)
					}
					var out, diagnostics bytes.Buffer
					if code := Run(context.Background(), []string{"--config", path, "start", selected.ID, "--event", selected.EventKey, "--json"}, &out, &diagnostics, deps); code != 0 {
						t.Fatalf("start failed: %d %s", code, diagnostics.String())
					}
					want = 1
				}
				runWatch()
				last := runWatch()
				if calls.Load() != want || len(last.Runs) != int(want) {
					t.Fatalf("unchanged event replayed or disappeared: calls=%d runs=%d wanted=%d", calls.Load(), len(last.Runs), want)
				}
				if want == 1 {
					var run store.Run
					if json.Unmarshal(last.Runs[0], &run) != nil || run.Executor != store.NativeExecutor || run.State != store.AgentCompleted {
						t.Fatalf("watch reported workflow delivery instead of native completion: %s", last.Runs[0])
					}
				}
			})
		}
	}
}
