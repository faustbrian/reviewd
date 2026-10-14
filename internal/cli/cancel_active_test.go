package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestSecondClientCancellationStopsActivePrivateReview(t *testing.T) {
	for _, mode := range []string{"manual", "once", "continuous", "manual-late-success"} {
		t.Run(mode, func(t *testing.T) {
			fixture := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = fixture
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("owned fixture: %v %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q", "-b", "main", "--template=")
			if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte("old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "file.go")
			git("commit", "-qm", "base")
			base := git("rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte("new\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "file.go")
			git("commit", "-qm", "head")
			head := git("rev-parse", "HEAD")
			git("update-ref", "refs/pull/1/head", head)
			pull := func() map[string]any {
				p := lifecyclePull(1, "open", true, false)
				p["head"].(map[string]any)["sha"] = head
				p["base"].(map[string]any)["sha"] = base
				return p
			}
			var member atomic.Bool
			var reads atomic.Int32
			peer := authorityPeer(t, pull, func() []any { return []any{authorityEvent("REQUEST", "User", 43)} }, &member)
			invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				switch r.URL.Path {
				case "/repos/owner/repo/pulls/1":
					json.NewEncoder(w).Encode(pull())
				case "/repos/owner/repo/branches/main":
					fmt.Fprint(w, `{"protected":false}`)
				case "/repos/owner/repo/rules/branches/main":
					fmt.Fprint(w, `[]`)
				case "/repos/owner/repo/commits/" + head + "/check-runs":
					fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				default:
					peer.ServeHTTP(w, r)
				}
			}))
			skill := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nReview privately.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg = append(cfg, []byte("\n[watch]\npoll_interval=\"1h\"\n[reviewer]\nskill_path="+strconv.Quote(skill)+"\n")...)
			if err := os.WriteFile(path, cfg, 0600); err != nil {
				t.Fatal(err)
			}
			sources := t.TempDir()
			deps.Source = func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
				return source.Acquire(ctx, source.Options{RemoteURL: "file://" + fixture, AllowLocalFixture: true, TempRoot: sources}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, ChangedFiles: pull.ChangedFiles, Number: pull.Number})
			}
			entered, stopped := make(chan struct{}), make(chan struct{})
			deps.Review = func(ctx context.Context, _ review.WorkerRequest) (codex.ReviewOutput, error) {
				close(entered)
				<-ctx.Done()
				close(stopped)
				output := codex.ReviewOutput{Text: `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`, ThreadID: "cancel-thread", TurnID: "cancel-turn", RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol"}
				if mode == "manual-late-success" {
					return output, nil
				}
				return output, ctx.Err()
			}
			args := []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}
			if !strings.HasPrefix(mode, "manual") {
				initial := invoke("sync")
				invoke("start", initial.Items[0].ID)
				args = []string{"--config", path, "watch", "--headless", "--json"}
				if mode == "once" {
					args = append(args, "--once")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Acquisition has its own startup watchdog. It must not become a
			// competing cancellation trigger once durable cancel is tested.
			startup, stopStartup := context.WithTimeout(ctx, 30*time.Second)
			defer stopStartup()
			var output, diagnostics bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &output, &diagnostics, deps) }()
			finished := false
			defer func() {
				cancel()
				if !finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("owned CLI did not stop")
					}
				}
			}()
			select {
			case <-entered:
			case code := <-done:
				finished = true
				t.Fatalf("review never entered worker: %d %s", code, diagnostics.String())
			case <-startup.Done():
				t.Fatalf("review did not enter worker within the startup budget: %v", startup.Err())
			}
			stopStartup()
			durable, err := store.Open(context.Background(), filepath.Join(filepath.Dir(path), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			runs, err := durable.List(context.Background())
			if err != nil || len(runs) != 1 || runs[0].State != store.Reviewing {
				t.Fatalf("active run unavailable: %v %+v", err, runs)
			}
			beforeReads := reads.Load()
			var reply, errors bytes.Buffer
			if code := Run(context.Background(), []string{"--config", path, "cancel", runs[0].ID, "--json"}, &reply, &errors, deps); code != 0 {
				t.Fatalf("second client could not request cancellation: %d %s", code, errors.String())
			}
			var acknowledgement struct {
				Run struct {
					State     store.State `json:"state"`
					Requested bool        `json:"cancellation_requested"`
				} `json:"run"`
			}
			if json.Unmarshal(reply.Bytes(), &acknowledgement) != nil || !acknowledgement.Run.Requested || acknowledgement.Run.State != store.Reviewing {
				t.Fatalf("request was misrepresented as stopped work: %s", reply.String())
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("accepted cancellation did not reach the active private worker")
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				run, err := durable.Get(context.Background(), runs[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.State == store.Cancelled {
					if run.ThreadID != "cancel-thread" || run.TurnID != "cancel-turn" || run.InboxItemID != runs[0].InboxItemID || len(run.Prepared) != 0 {
						t.Fatalf("cancellation lost identity or prepared a result: %+v", run)
					}
					if !run.OwnerClosed || run.OwnerID == "" {
						t.Fatal("cancellation did not acknowledge actual private-resource closure")
					}
					if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state", "private-work", run.OwnerID)); !os.IsNotExist(err) {
						t.Fatal("cancelled review retained its actual source/skill/runtime scope")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker exit was not persisted: %+v", run)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if entries, err := os.ReadDir(sources); err != nil || len(entries) != 0 {
				t.Fatal("cancelled review retained its source snapshot")
			}
			if reads.Load() != beforeReads {
				t.Fatal("local cancellation waited for another GitHub poll or performed remote work")
			}
			cancel()
			select {
			case <-done:
				finished = true
			case <-time.After(5 * time.Second):
				t.Fatal("CLI did not exit after cancellation")
			}
			if !strings.HasPrefix(mode, "manual") {
				items, _, err := durable.Inbox(context.Background())
				if err != nil || len(items) != 1 || items[0].State != "cancelled" {
					t.Fatal("watcher did not expose factual cancelled state")
				}
			}
		})
	}
}
