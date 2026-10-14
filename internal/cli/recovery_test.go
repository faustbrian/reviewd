package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// QA-X05/X08: a pre-ownership record cannot prove that its worker died.
// Recovery is local maintenance, not permission to resume, publish or erase it.
func TestRecoveryPreservesUnknownLegacyOwnerAndPendingPublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[state]\ndirectory=%q\n", directory)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	create := func(key string) store.Run {
		t.Helper()
		run, won, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: key, State: store.Queued, Configuration: json.RawMessage(`{}`), Context: json.RawMessage(`{"private":"preserve-ownerless-context"}`)})
		if err != nil || !won {
			t.Fatalf("create: %t %v", won, err)
		}
		if won, err := s.Claim(ctx, run.ID); err != nil || !won {
			t.Fatalf("claim: %t %v", won, err)
		}
		return run
	}
	legacy := create("legacy")
	pending := create("publication")
	if err := s.Prepare(ctx, pending.ID, "payload", json.RawMessage(`{"private":"preserve-preview"}`), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		var out, diagnostics bytes.Buffer
		args := []string{"--config", path, "recover", "--json"}
		if dry {
			args = append(args, "--dry-run")
		}
		code := Run(ctx, args, &out, &diagnostics, Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
			t.Fatal("recovery attempted remote access")
			return nil, nil
		}})
		if code != 1 {
			t.Fatalf("unknown owner must remain actionable, not an empty success: code=%d output=%s diagnostic=%s", code, out.String(), diagnostics.String())
		}
		var report struct {
			SchemaVersion int `json:"schema_version"`
			Recovery      struct {
				DryRun  bool     `json:"dry_run"`
				Unknown []string `json:"unknown_runs"`
			} `json:"recovery"`
		}
		if json.Unmarshal(out.Bytes(), &report) != nil || report.SchemaVersion != 1 || report.Recovery.DryRun != dry || len(report.Recovery.Unknown) != 1 || report.Recovery.Unknown[0] != legacy.ID {
			t.Fatalf("missing honest owner classification: %s %s", out.String(), diagnostics.String())
		}
		active, err := s.Get(ctx, legacy.ID)
		if err != nil || active.State != store.Reviewing || string(active.Context) != `{"private":"preserve-ownerless-context"}` {
			t.Fatalf("unknown owner changed or erased: %+v %v", active, err)
		}
		preview, err := s.Get(ctx, pending.ID)
		if err != nil || preview.State != store.Prepared || preview.PayloadID != "payload" {
			t.Fatalf("recovery consumed a publication decision: %+v %v", preview, err)
		}
	}
}

// QA-X05/X08/27: cross-process owner loss, not elapsed time or a stale PID,
// distinguishes a live private review from one whose defers never ran.
func TestRecoveryClassifiesCrashedManualOwner(t *testing.T) {
	if path := os.Getenv("REVIEWD_RECOVERY_CHILD"); path != "" {
		client, err := github.New(github.Options{Host: "github.com", BaseURL: os.Getenv("REVIEWD_RECOVERY_PEER"), Token: "owned-recovery-fixture"})
		if err != nil {
			t.Fatal(err)
		}
		deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, Source: func(_ context.Context, pull github.Pull) (*source.Snapshot, error) {
			return &source.Snapshot{TargetBaseSHA: pull.TargetBaseSHA, HeadSHA: pull.HeadSHA, MergeBaseSHA: pull.TargetBaseSHA, CommitCount: 1, Changes: []source.Change{{Path: "file.go", Status: "M"}}, Sources: map[string]map[string]string{"base": {"file.go": "old\n"}, "head": {"file.go": "new\n"}, "target": {}}, Manifests: map[string][]source.Entry{"target": {}}, Diff: "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\n"}, nil
		}, Review: func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
			if err := request.OnStatus(codex.ReviewOutput{ThreadID: "orphan-thread", TurnID: "orphan-turn", RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol", Progress: "reviewing"}); err != nil {
				t.Fatal(err)
			}
			question := clarification.Request{ID: "orphan-input", ThreadID: "orphan-thread", TurnID: "orphan-turn", WireID: `"request"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which variant?"}}}
			if err := request.OnInputEvent(ctx, question, "opened", ""); err != nil {
				t.Fatal(err)
			}
			ready, _ := json.Marshal(map[string]string{"skill": request.Skill.Path})
			if err := os.WriteFile(os.Getenv("REVIEWD_RECOVERY_READY"), ready, 0600); err != nil {
				t.Fatal(err)
			}
			var signal [1]byte
			_, _ = os.Stdin.Read(signal[:])
			os.Exit(0) // Deliberately skip every cleanup defer and durable finish.
			return codex.ReviewOutput{}, nil
		}}
		var out, diagnostics bytes.Buffer
		code := Run(context.Background(), []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}, &out, &diagnostics, deps)
		t.Fatalf("helper did not enter private worker: %d %s %s", code, out.String(), diagnostics.String())
	}
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	directory := filepath.Join(root, "state")
	ready := filepath.Join(root, "ready.json")
	skillDirectory := filepath.Join(root, "requesting-code-review")
	if err := os.Mkdir(skillDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(skillDirectory, "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nReview this owned fixture privately.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[state]\ndirectory=%q\n[reviewer]\nskill_path=%q\n", directory, skill)
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	pull := func() map[string]any { return lifecyclePull(1, "open", true, false) }
	var member atomic.Bool
	peer := authorityPeer(t, pull, func() []any { return []any{authorityEvent("REQUEST", "User", 43)} }, &member)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/1":
			json.NewEncoder(w).Encode(pull())
		case "/repos/owner/repo/branches/main":
			fmt.Fprint(w, `{"protected":false}`)
		case "/repos/owner/repo/rules/branches/main":
			fmt.Fprint(w, `[]`)
		case "/repos/owner/repo/commits/" + strings.Repeat("a", 40) + "/check-runs":
			fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		default:
			peer.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestRecoveryClassifiesCrashedManualOwner$")
	child.Env = append(os.Environ(), "REVIEWD_RECOVERY_CHILD="+path, "REVIEWD_RECOVERY_PEER="+server.URL, "REVIEWD_RECOVERY_READY="+ready)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	child.Stdout, child.Stderr = &log, &log
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	}()
	var captured struct{ Skill string }
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(ready); err == nil && json.Unmarshal(data, &captured) == nil && captured.Skill != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned review did not enter its worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The old implementation freezes into a standalone temp root. Resolve and
	// remove only that exact task-created root even when the red assertion fails.
	oldRoot := filepath.Dir(filepath.Dir(captured.Skill))
	if strings.HasPrefix(filepath.Base(oldRoot), "reviewd-skill-") {
		t.Cleanup(func() { os.RemoveAll(oldRoot) })
	}
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != store.Reviewing {
		t.Fatalf("missing live review: %+v %v", runs, err)
	}
	invoke := func(extra ...string) {
		t.Helper()
		var out, diagnostics bytes.Buffer
		args := append([]string{"--config", path, "recover", "--json"}, extra...)
		if code := Run(ctx, args, &out, &diagnostics, Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
			t.Fatal("recovery contacted GitHub")
			return nil, nil
		}}); code != 0 {
			t.Fatalf("owned recovery: %d %s %s", code, out.String(), diagnostics.String())
		}
	}
	invoke()
	live, err := s.Get(ctx, runs[0].ID)
	if err != nil || live.State != store.Reviewing || live.Inputs[0].ClosedAt != "" {
		t.Fatal("recovery interrupted a live independent manual owner")
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	if err != nil {
		t.Fatalf("owned crash helper: %v %s", err, log.String())
	}
	invoke("--dry-run")
	before, err := s.Get(ctx, runs[0].ID)
	if err != nil || before.State != store.Reviewing {
		t.Fatal("dry-run recovered an orphan")
	}
	if _, err := os.Stat(captured.Skill); err != nil {
		t.Fatal("dry-run removed owned private storage")
	}
	invoke()
	after, err := s.Get(ctx, runs[0].ID)
	if err != nil || after.State != store.Failed || after.ThreadID != "orphan-thread" || after.TurnID != "orphan-turn" || len(after.Inputs) != 1 || after.Inputs[0].ClosedAt == "" || after.Inputs[0].Reason != "owner_lost" || len(after.Prepared) != 0 {
		t.Fatalf("crash recovery lost facts or fabricated success: %+v %v", after, err)
	}
	if _, err := os.Stat(captured.Skill); !os.IsNotExist(err) {
		t.Fatal("recovery did not remove the positively owned dead private root")
	}
	invoke()
	repeated, err := s.Get(ctx, runs[0].ID)
	if err != nil || !repeated.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("repeat recovery changed historical facts")
	}
}
