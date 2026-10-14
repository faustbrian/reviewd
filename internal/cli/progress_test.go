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

// Observe the application while its real stdio adapter is handling a source
// request, before the peer sends its final answer. A completed output cannot
// satisfy this assertion by retrospectively recording worker metadata.
func TestActivePrivateProgressIsDurableBeforeWorkerReturns(t *testing.T) {
	for _, mode := range []string{"manual", "watch"} {
		t.Run(mode, func(t *testing.T) {
			invoke, path, deps, durable := privateApplicationFixture(t, "STREAM-PROGRESS-ACCEPTANCE")
			observed := 0
			deps.Review = func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
				return runPrivateWorker(ctx, request, func(event codex.Message) {
					if event.Method != "item/tool/call" {
						return
					}
					observed++
					runs, err := durable.List(ctx)
					if err != nil || len(runs) != 1 {
						t.Error("active review unavailable to an independent SQLite client")
						return
					}
					run := runs[0]
					if run.State != store.Reviewing || run.ThreadID != "cli-private-thread" || run.TurnID != "cli-private-turn" || run.RequestedModel != "fixture-model" || run.EffectiveModel != "fixture-effective" || !strings.Contains(string(run.Usage), `"totalTokens":17`) || strings.Contains(string(run.Usage), "999") {
						t.Errorf("active identity/model/usage were not durable before worker completion: state=%s thread=%q turn=%q model=%q effective=%q usage=%s", run.State, run.ThreadID, run.TurnID, run.RequestedModel, run.EffectiveModel, run.Usage)
					}
					var output, diagnostics bytes.Buffer
					if code := Run(ctx, []string{"--config", path, "show", run.ID}, &output, &diagnostics, deps); code != 0 || !strings.Contains(output.String(), "cli-private-thread") || !strings.Contains(output.String(), "cli-private-turn") || !strings.Contains(output.String(), "Activity:") {
						t.Error("plain show did not expose active identity and activity")
					}
					output.Reset()
					if code := Run(ctx, []string{"--config", path, "show", run.ID, "--json"}, &output, &diagnostics, deps); code != 0 || !strings.Contains(output.String(), `"progress"`) || !strings.Contains(output.String(), "cli-private-thread") {
						t.Error("JSON show did not expose active identity and progress")
					}
				})
			}
			args := []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}
			if mode == "watch" {
				initial := invoke("sync")
				invoke("start", initial.Items[0].ID)
				args = []string{"--config", path, "watch", "--headless", "--once", "--json"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var output, diagnostics bytes.Buffer
			if code := Run(ctx, args, &output, &diagnostics, deps); code != 0 {
				t.Fatalf("private CLI journey failed: %d %s", code, diagnostics.String())
			}
			if observed != 1 {
				t.Fatalf("active protocol observation missing: %d", observed)
			}
			runs, err := durable.List(ctx)
			if err != nil || len(runs) != 1 || runs[0].State != store.Prepared || runs[0].EffectiveModel != "fixture-effective" {
				t.Fatal("streaming progress corrupted the final private result")
			}
		})
	}
}

func privateApplicationFixture(t *testing.T, title string, versions ...[2]string) (func(string, ...string) lifecycleInbox, string, Dependencies, *store.Store) {
	t.Helper()
	fixture := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = fixture
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned immutable fixture: %v %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q", "-b", "main", "--template=")
	baseText, headText := "old\n", "new\n"
	if len(versions) > 1 {
		t.Fatal("private fixture accepts one source boundary")
	}
	if len(versions) == 1 {
		baseText, headText = versions[0][0], versions[0][1]
	}
	for name, text := range map[string]string{"file.go": baseText, "AGENTS.md": "immutable trusted target instructions"} {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "file.go", "AGENTS.md")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte(headText), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "file.go")
	git("commit", "-qm", "head")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/1/head", head)
	pull := func() map[string]any {
		p := lifecyclePull(1, "open", true, false)
		p["title"] = title
		if len(versions) == 1 {
			p["body"] = "Preserve the percentage contract: Discount(100,25) and its Total caller must return 75. Review the complete immutable change and all granted callers; do not execute fixture code or publish."
		}
		p["head"].(map[string]any)["sha"] = head
		p["base"].(map[string]any)["sha"] = base
		return p
	}
	var member atomic.Bool
	peer := authorityPeer(t, pull, func() []any { return []any{authorityEvent("REQUEST", "User", 43)} }, &member)
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	auth := t.TempDir()
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nOnly in actual frozen installed skill.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg = append(cfg, []byte("\n[reviewer]\nexecutable="+strconv.Quote(executable)+"\nauth_home="+strconv.Quote(auth)+"\nskill_path="+strconv.Quote(skill)+"\nmodel=\"fixture-model\"\nreasoning_effort=\"xhigh\"\n")...)
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	deps.Source = func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
		return source.Acquire(ctx, source.Options{RemoteURL: "file://" + fixture, AllowLocalFixture: true, TempRoot: t.TempDir()}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, ChangedFiles: pull.ChangedFiles, Number: pull.Number})
	}
	durable, err := store.Open(context.Background(), filepath.Join(filepath.Dir(path), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	return invoke, path, deps, durable
}
