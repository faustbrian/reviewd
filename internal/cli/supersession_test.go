package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
)

// Head and requirement invalidation must be distinguishable from a user's
// cancellation. Exercise the real watcher/service/store path, including a
// worker that returns a late successful answer after its context is cancelled.
func TestWatcherSupersededEvidenceBecomesStaleWithoutGrantingPublication(t *testing.T) {
	for _, stage := range []string{"reviewing", "reviewing-late-success", "waiting_checks", "waiting-resume", "prepared", "manual-final"} {
		for _, change := range []string{"head", "context", "context-rerequest"} {
			t.Run(stage+"/"+change, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var requested, member atomic.Bool
				var revision atomic.Int32
				pull := func() map[string]any {
					value := lifecyclePull(1, "open", requested.Load(), false)
					if revision.Load() != 0 {
						if change == "head" {
							value["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
						} else {
							value["body"] = "Changed required caller behavior."
						}
					}
					return value
				}
				events := func() []any {
					if requested.Load() {
						if change == "context-rerequest" && revision.Load() != 0 {
							return []any{authorityEvent("REQUEST", "User", 43), authorityEvent("REQUEST_AGAIN", "User", 43)}
						}
						return []any{authorityEvent("REQUEST", "User", 43)}
					}
					return []any{}
				}
				peer := authorityPeer(t, pull, events, &member)
				_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/repos/owner/repo/pulls/1":
						json.NewEncoder(w).Encode(pull())
					case r.URL.Path == "/repos/owner/repo/branches/main":
						fmt.Fprint(w, `{"protected":false}`)
					case r.URL.Path == "/repos/owner/repo/rules/branches/main":
						if strings.HasPrefix(stage, "waiting") {
							fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":77}]}}]`)
						} else {
							fmt.Fprint(w, `[]`)
						}
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						if strings.HasPrefix(stage, "waiting") {
							fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":5,"name":"quality","head_sha":%q,"status":"in_progress","conclusion":null,"app":{"id":77}}]}`, pull()["head"].(map[string]any)["sha"])
						} else {
							fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
						}
					default:
						peer.ServeHTTP(w, r)
					}
				}))
				skill := filepath.Join(t.TempDir(), "SKILL.md")
				if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nReview only the owned fixture privately.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, []byte("\n[reviewer]\nskill_path="+strconv.Quote(skill)+"\n[limits]\nmax_concurrent_reviews=1\n")...)
				manager, err := config.NewManager(data, path)
				if err != nil {
					t.Fatal(err)
				}
				durable, err := store.Open(ctx, filepath.Join(filepath.Dir(path), "state"))
				if err != nil {
					t.Fatal(err)
				}
				defer durable.Close()
				var sources, workers atomic.Int32
				entered := make(chan struct{})
				deps.Source = func(_ context.Context, pull github.Pull) (*source.Snapshot, error) {
					sources.Add(1)
					return &source.Snapshot{TargetBaseSHA: pull.TargetBaseSHA, HeadSHA: pull.HeadSHA, MergeBaseSHA: pull.TargetBaseSHA, CommitCount: 1, Changes: []source.Change{{Path: "file.go", Status: "M"}}, Sources: map[string]map[string]string{"base": {"file.go": "old\n"}, "head": {"file.go": "new\n"}, "target": {}}, Manifests: map[string][]source.Entry{"target": {}}, Diff: "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\n"}, nil
				}
				deps.Review = func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
					workers.Add(1)
					output := codex.ReviewOutput{Text: `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`, ThreadID: "superseded-thread", TurnID: "superseded-turn", RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol", Progress: "reviewing"}
					if err := request.OnStatus(output); err != nil {
						return output, err
					}
					close(entered)
					if stage == "manual-final" {
						revision.Store(1)
					}
					if strings.HasPrefix(stage, "reviewing") {
						<-ctx.Done()
						if stage != "reviewing-late-success" {
							return output, ctx.Err()
						}
					}
					return output, nil
				}
				engine := watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Reviewer: func(client *github.Client) review.Service {
					return review.Service{Store: durable, GitHub: client, Source: deps.Source, SkillPath: skillPath, Worker: deps.Review}
				}}
				defer engine.Close()
				if stage == "manual-final" {
					client, err := deps.GitHub(ctx, "github.com")
					if err != nil {
						t.Fatal(err)
					}
					_, reviewErr := engine.Reviewer(client).Start(ctx, manager.Document(), github.PullReference{Repository: "github.com/owner/repo", Number: 1}, nil, true, true)
					runs, err := durable.List(ctx)
					if reviewErr == nil || err != nil || len(runs) != 1 || runs[0].State != store.Stale || len(runs[0].Prepared) != 0 || runs[0].ThreadID != "superseded-thread" || runs[0].TurnID != "superseded-turn" {
						t.Fatalf("manual final boundary change did not retain a stale non-publishable run: %+v review=%v read=%v", runs, reviewErr, err)
					}
					return
				}
				if err := engine.Tick(ctx); err != nil || engine.Active() != 0 {
					t.Fatalf("initial discovery: %v", err)
				}
				requested.Store(true)
				if err := engine.Tick(ctx); err != nil || engine.Active() != 1 {
					t.Fatalf("new request was not admitted: %v", err)
				}
				if strings.HasPrefix(stage, "reviewing") {
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("owned worker never became active")
					}
				} else if err := engine.Complete(ctx, true); err != nil {
					t.Fatal(err)
				}
				runs, err := durable.List(ctx)
				if err != nil || len(runs) != 1 {
					t.Fatalf("missing original run: %+v %v", runs, err)
				}
				before := runs[0]
				if stage == "prepared" {
					if err := durable.DecidePublication(ctx, before.ID, before.PayloadID); err != nil {
						t.Fatal(err)
					}
				}
				revision.Store(1)
				if stage == "waiting-resume" {
					items, _, err := durable.Inbox(ctx)
					if err != nil || len(items) != 1 {
						t.Fatal("missing waiting item")
					}
					client, err := deps.GitHub(ctx, "github.com")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := engine.Reviewer(client).StartItem(ctx, manager.Document(), items[0], nil, false, true); err == nil {
						t.Fatal("obsolete waiting review resumed")
					}
				} else if err := engine.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(stage, "reviewing") {
					_ = engine.Complete(ctx, true) // Supersession is a non-clean outcome.
				}
				after, err := durable.Get(ctx, before.ID)
				if err != nil || after.State != store.Stale {
					t.Fatalf("obsolete %s evidence must be stale, not %s: %v", stage, after.State, err)
				}
				if after.PayloadID != before.PayloadID || after.ThreadID != before.ThreadID || after.TurnID != before.TurnID || string(after.Context) != string(before.Context) {
					t.Fatal("supersession replaced retained source/session identity")
				}
				if strings.HasPrefix(stage, "reviewing") && len(after.Prepared) != 0 {
					t.Fatal("a late answer prepared superseded work")
				}
				if stage == "prepared" {
					if _, claimed, err := durable.BeginPublication(ctx, before.ID, before.PayloadID); err != nil || claimed {
						t.Fatalf("old material-boundary decision remained usable: claimed=%t err=%v", claimed, err)
					}
				}
				if sources.Load() != 1 || workers.Load() > 1 {
					t.Fatal("replacement bypassed debounce or overlapped superseded work")
				}
			})
		}
	}
}
