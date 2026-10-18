package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
)

func TestAcceptedSkillsFinishDespiteDiscoveryAndPolicyChanges(t *testing.T) {
	for _, lane := range []string{"review", "feedback"} {
		for _, change := range []string{"content_removed", "closed", "draft", "read_failure", "account_changed", "scope_removed", "author_policy", "action_policy", "new_settings", "new_event", "explicit_cancel", "watcher_shutdown"} {
			t.Run(lane+"/"+change, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				var phase atomic.Int32
				invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p := phase.Load()
					changed := p >= 2
					if changed && change == "read_failure" && r.URL.Path == "/repos/owner/repo/pulls" {
						http.Error(w, "temporary fixture outage", http.StatusInternalServerError)
						return
					}
					pull := lifecyclePull(1, "open", lane == "review" && !(changed && change == "content_removed"), false)
					if lane == "feedback" {
						pull["user"] = map[string]any{"id": 43, "login": "reviewer"}
					} else if p > 0 {
						head := "c"
						if p >= 3 || (changed && change == "new_event") {
							head = "e"
						}
						pull["head"].(map[string]any)["sha"] = strings.Repeat(head, 40)
					}
					if changed && change == "closed" {
						pull["state"] = "closed"
					}
					if changed && change == "draft" {
						pull["draft"] = true
					}
					var value any = []any{}
					base, tree := strings.Repeat("b", 40), strings.Repeat("d", 40)
					switch r.URL.Path {
					case "/user":
						id, login := 43, "reviewer"
						if changed && change == "account_changed" {
							id, login = 44, "another-operator"
						}
						value = map[string]any{"id": id, "login": login}
					case "/repos/owner/repo":
						value = map[string]any{"id": 99, "full_name": "owner/repo"}
					case "/repos/owner/repo/pulls":
						if pull["state"] != "closed" {
							value = []any{pull}
						}
					case "/repos/owner/repo/pulls/1":
						value = pull
					case "/repos/owner/repo/commits/" + base:
						value = map[string]any{"sha": base, "commit": map[string]any{"tree": map[string]any{"sha": tree}}}
					case "/repos/owner/repo/git/trees/" + tree:
						value = map[string]any{"sha": tree, "truncated": false, "tree": []any{}}
					case "/repos/owner/repo/pulls/1/reviews":
						if lane == "feedback" && !(changed && change == "content_removed") {
							revision := p
							if p == 2 && change != "new_event" {
								revision = 1 // No new arrival unless the test asks for future work.
							}
							value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 9, "login": "human-reviewer"}, "state": "COMMENTED", "body": fmt.Sprintf("Human concern %d", revision), "submitted_at": "2026-10-18T00:00:00Z"}}
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
				configuration := string(raw) + "\n[watch]\ndebounce='0s'\n[limits]\nreview_timeout='2m'\nfeedback_timeout='2m'\n"
				if lane == "feedback" {
					configuration += "\n[feedback]\nauthors=['@me']\naction='auto'\n"
				}
				if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
					t.Fatal(err)
				}
				invoke("sync")
				phase.Store(1)
				invoke("sync")
				manager, err := config.NewManager([]byte(configuration), path)
				if err != nil {
					t.Fatal(err)
				}
				policy, _ := config.ResolveDefaults(manager.Document(), nil)
				directory, err := stateDirectory(policy, path)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				durable, err := store.Open(ctx, directory)
				if err != nil {
					t.Fatal(err)
				}
				defer durable.Close()
				type accepted struct {
					context context.Context
					request agent.Request
				}
				entered, finish := make(chan accepted, 1), make(chan struct{})
				var calls atomic.Int32
				deps.LaunchAgent = func(worker context.Context, request agent.Request) (agent.Output, error) {
					n := calls.Add(1)
					output := agent.Output{ThreadID: fmt.Sprintf("accepted-session-%d", n), Text: "Skill-owned output."}
					if err := request.OnProgress(output); err != nil {
						return output, err
					}
					if n == 1 {
						entered <- accepted{worker, request}
						select {
						case <-finish:
						case <-worker.Done():
							return output, worker.Err()
						}
					} else {
						want := "gpt-6.1-sol"
						if change == "new_settings" {
							want = "future-model"
						}
						if request.Model != want {
							t.Errorf("future work did not use current model: %s", request.Model)
						}
					}
					output.Completed = true
					return output, nil
				}
				e := &execution{}
				engine := watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Execute: e.itemExecutor(deps, durable, directory)}
				defer engine.Close()
				if err := engine.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				var active accepted
				select {
				case active = <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("eligible skill never entered launcher")
				}
				runs, err := durable.List(ctx)
				if err != nil || len(runs) != 1 || runs[0].ThreadID != "accepted-session-1" {
					t.Fatalf("acceptance/session was not persisted: %+v %v", runs, err)
				}
				runID := runs[0].ID
				deadline, _ := active.context.Deadline()
				next := configuration
				switch change {
				case "scope_removed":
					next = strings.Replace(next, `repositories = ["owner/repo"]`, `repositories = []`, 1)
				case "author_policy":
					if lane == "review" {
						next += "\n[reviewer]\nexclude_authors=['author']\n"
					} else {
						next = strings.Replace(next, "authors=['@me']", "authors=[]", 1)
					}
				case "action_policy", "explicit_cancel", "watcher_shutdown":
					if lane == "review" {
						next = strings.ReplaceAll(next, `= "auto"`, `= "ignore"`)
					} else {
						next = strings.Replace(next, "action='auto'", "action='ignore'", 1)
					}
				case "new_settings":
					next = strings.ReplaceAll(next, "timeout='2m'", "timeout='1m'")
					if lane == "review" {
						next += "\n[reviewer]\nmodel='future-model'\n"
					} else {
						next += "model='future-model'\n"
					}
				}
				if err := manager.Reload([]byte(next)); err != nil {
					t.Fatal(err)
				}
				phase.Store(2)
				for i := 0; i < 2; i++ {
					err := engine.Tick(ctx)
					if (change == "read_failure" || change == "scope_removed") != (err != nil) {
						t.Fatalf("wrong discovery health result: %v", err)
					}
					if active.context.Err() != nil {
						t.Fatalf("%s cancelled accepted %s skill: %v", change, lane, active.context.Err())
					}
					run, err := durable.Get(ctx, runID)
					afterDeadline, _ := active.context.Deadline()
					if err != nil || run.State != store.AgentRunning || run.ThreadID != "accepted-session-1" || calls.Load() != 1 || !afterDeadline.Equal(deadline) || active.request.Model != "gpt-6.1-sol" {
						t.Fatalf("accepted execution changed or duplicated: %+v calls=%d %v", run, calls.Load(), err)
					}
					snapshot, err := e.terminalSnapshot(ctx, durable, manager.Document())
					visible := false
					for _, entry := range snapshot.Entries {
						visible = visible || (entry.RunID == runID && entry.Section == "running")
					}
					if err != nil || !visible {
						t.Fatalf("accepted execution lost factual running view: %v", err)
					}
				}
				if change == "explicit_cancel" || change == "watcher_shutdown" {
					if change == "explicit_cancel" {
						var output, diagnostics bytes.Buffer
						if code := Run(ctx, []string{"--config", path, "cancel", runID, "--json"}, &output, &diagnostics, deps); code != 0 {
							t.Fatalf("explicit stop failed: %d %s", code, diagnostics.String())
						}
						select {
						case <-active.context.Done():
						case <-time.After(5 * time.Second):
							t.Fatal("explicit stop did not reach accepted skill")
						}
						if err := engine.Complete(ctx, true); !errors.Is(err, context.Canceled) {
							t.Fatalf("wrong explicit stop result: %v", err)
						}
					} else if err := engine.Close(); !errors.Is(err, context.Canceled) {
						t.Fatalf("wrong watcher shutdown result: %v", err)
					}
					run, err := durable.Get(ctx, runID)
					if err != nil || run.State != store.Cancelled || run.ThreadID != "accepted-session-1" || active.context.Err() == nil || engine.Active() != 0 || calls.Load() != 1 {
						t.Fatalf("stop lost identity/state or retained capacity: %+v %v", run, err)
					}
					slotCtx, end := context.WithTimeout(ctx, time.Second)
					slot, err := watch.AcquireSlot(slotCtx, directory, 1)
					end()
					if err != nil {
						t.Fatalf("stop retained execution slot: %v", err)
					}
					slot.Close()
					return
				}
				close(finish)
				if err := engine.Complete(ctx, true); err != nil {
					t.Fatal(err)
				}
				run, err := durable.Get(ctx, runID)
				if err != nil || run.State != store.AgentCompleted || run.ThreadID != "accepted-session-1" {
					t.Fatalf("accepted execution failed to finish: %+v %v", run, err)
				}
				// A new observation may launch only under the current policy. Failed
				// acquisition and every revocation row must still block new work.
				if change != "content_removed" {
					phase.Store(3)
				}
				_ = engine.Tick(ctx)
				if change == "new_settings" || change == "new_event" {
					if engine.Active() != 1 {
						t.Fatal("eligible future work was not admitted")
					}
					if err := engine.Complete(ctx, true); err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 2 {
						t.Fatalf("eligible future work did not launch once: %d", calls.Load())
					}
				} else if calls.Load() != 1 || engine.Active() != 0 {
					t.Fatalf("changed policy/event launched unexpected work: %d", calls.Load())
				}
			})
		}
	}
}
