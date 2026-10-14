package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestCleanupExpiresTerminalPrivateArtifactsButKeepsRunAndInputFacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := filepath.Join(root, "profile")
	path := filepath.Join(root, "config.toml")
	text := fmt.Sprintf("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[state]\ndirectory=%q\nretention_days=1\n[review]\ninstructions=\"old-instructions-canary\"\n", directory)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse([]byte(text), path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.Resolve(document, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "retention", State: store.Queued, Configuration: frozen, Context: json.RawMessage(`{"private":"old-context-canary"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim: %t %v", won, err)
	}
	if err := s.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	request := clarification.Request{ID: "retention-input", ThreadID: "thread", TurnID: "turn", WireID: `"request"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "old-question-canary"}}}
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"old-answer-canary"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitInput(ctx, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.InputEvent(ctx, request.ID, "written", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseInputs(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, store.Failed, "Private review interrupted."); err != nil {
		t.Fatal(err)
	}
	fixture, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	if _, err := fixture.ExecContext(ctx, "UPDATE runs SET updated_at=? WHERE id=?", old, run.ID); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) string {
		t.Helper()
		var out, diagnostics bytes.Buffer
		code := Run(ctx, append([]string{"--config", path}, args...), &out, &diagnostics, Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
			t.Fatal("local retention attempted GitHub access")
			return nil, nil
		}})
		if code != 0 {
			t.Fatalf("cleanup command failed: %d %s", code, diagnostics.String())
		}
		return out.String()
	}
	invoke("cleanup", "--dry-run", "--json")
	before, err := s.Get(ctx, run.ID)
	if err != nil || !strings.Contains(string(before.Configuration), "old-instructions-canary") {
		t.Fatal("dry run changed private artifacts")
	}
	invoke("cleanup", "--json")
	after, err := s.Get(ctx, run.ID)
	if err != nil || after.State != store.Failed || after.ThreadID != "thread" || after.TurnID != "turn" || len(after.Inputs) != 1 || after.Inputs[0].WrittenAt == "" || after.Inputs[0].ClosedAt == "" || after.UpdatedAt.Format(time.RFC3339Nano) != old {
		t.Fatalf("retention lost lifecycle facts or changed event time: %+v %v", after, err)
	}
	var record, input string
	var answers sql.NullString
	if err := fixture.QueryRowContext(ctx, "SELECT record FROM runs WHERE id=?", run.ID).Scan(&record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.QueryRowContext(ctx, "SELECT record,answers FROM input_requests WHERE id=?", request.ID).Scan(&input, &answers); err != nil {
		t.Fatal(err)
	}
	if answers.Valid || strings.Contains(record+input, "canary") {
		t.Fatal("expired private instructions/context/question/answer remain readable")
	}
	plain := invoke("show", run.ID)
	if !strings.Contains(plain, "expired") || !strings.Contains(plain, "thread") || !strings.Contains(plain, "failed") {
		t.Fatalf("plain show does not explain expiry with retained run facts: %s", plain)
	}
	invoke("cleanup", "--json")
	repeated, err := s.Get(ctx, run.ID)
	if err != nil || repeated.State != after.State || !repeated.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("repeated retention changed the historical result")
	}
}

func TestArchivedReviewStillFollowsUpdatesAndHonorsExplicitUnfollow(t *testing.T) {
	ctx := context.Background()
	var updated, member atomic.Bool
	pull := func() map[string]any {
		value := lifecyclePull(1, "open", false, false)
		if updated.Load() {
			value["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
		}
		return value
	}
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", authorityPeer(t, pull, func() []any { return []any{} }, &member))
	directory := filepath.Join(filepath.Dir(path), "state")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	policy, err := config.Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "implicit-follow", State: store.Queued, Configuration: frozen})
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim: %t %v", won, err)
	}
	prepared, err := review.Prepare(review.Boundary{Configuration: policy, Pull: github.Pull{PullReference: github.PullReference{Repository: run.Repository, Number: 1}, ID: 701, RepositoryID: 99, Author: github.Identity{ID: 8}, HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40)}, Actor: github.Identity{ID: 43}, Checks: github.CheckReport{Ready: true}}, review.Report{Conclusion: "APPROVE", Findings: []review.Finding{}, Gaps: []review.ResultGap{}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(prepared)
	var persisted review.PreparedResult
	if err := json.Unmarshal(payload, &persisted); err != nil {
		t.Fatal(err)
	}
	if err := review.ValidatePrepared(persisted); err != nil {
		t.Fatalf("invalid owned follow fixture: %v", err)
	}
	if err := s.Prepare(ctx, run.ID, prepared.PayloadID, payload, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, store.Stale, "fixture"); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) lifecycleInbox {
		t.Helper()
		var out, diagnostics bytes.Buffer
		if code := Run(ctx, append([]string{"--config", path, "--json"}, args...), &out, &diagnostics, deps); code != 0 {
			t.Fatalf("retained follow command: %d %s", code, diagnostics.String())
		}
		var result lifecycleInbox
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := call("sync")
	following, err := s.DiscoveryItems(ctx, run.Repository, run.ActorID)
	if err != nil || len(initial.Items) != 0 || len(following) != 1 || following[0].State != "following" {
		t.Fatal("unchanged followed PR was not retained silently")
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE runs SET updated_at=? WHERE id=?", time.Now().Add(-31*24*time.Hour).UTC().Format(time.RFC3339Nano), run.ID); err != nil {
		t.Fatal(err)
	}
	call("cleanup")
	if archived, err := s.Get(ctx, run.ID); err != nil || archived.Archive == nil || len(archived.Prepared) != 0 {
		t.Fatal("follow control did not actually expire its private preview")
	}
	after := call("sync")
	retained, err := s.DiscoveryItems(ctx, run.Repository, run.ActorID)
	if err != nil || len(after.Items) != 0 || len(retained) != 1 || retained[0].ID != following[0].ID || retained[0].State != "following" {
		t.Fatal("private-text expiry silently stopped following the PR")
	}
	updated.Store(true)
	changed := call("sync")
	if len(changed.Items) != 1 || changed.Items[0].ID != following[0].ID || changed.Items[0].Trigger != "head_updated" || changed.Items[0].State != "queued" || len(changed.Runs) != 1 {
		t.Fatalf("archived review did not discover new code without duplicating old work: %+v", changed)
	}
	call("unfollow", changed.Items[0].ID)
	updated.Store(false)
	ignored := call("sync")
	states, err := s.FollowStates(ctx, run.Repository, run.ActorID)
	if err != nil || len(ignored.Items) != 0 || states[run.PullID].Following {
		t.Fatal("archived follow metadata overrode an explicit unfollow")
	}
}

func TestHeadlessWatcherAppliesConfiguredRetentionWithoutStartingOldWork(t *testing.T) {
	ctx := context.Background()
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		}
		json.NewEncoder(w).Encode(value)
	}))
	directory := filepath.Join(filepath.Dir(path), "state")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "old", State: store.Queued, Configuration: json.RawMessage(`{}`), Context: json.RawMessage(`{"private":"old-private-canary"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, store.Failed, "fixture"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE runs SET updated_at=? WHERE id=?", time.Now().Add(-31*24*time.Hour).UTC().Format(time.RFC3339Nano), run.ID); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "watch", "--headless", "--once", "--json"}, &out, &diagnostics, deps); code != 0 {
		t.Fatalf("headless watch: %d %s", code, diagnostics.String())
	}
	after, err := s.Get(ctx, run.ID)
	if err != nil || after.Archive == nil || len(after.Context) != 0 || after.State != store.Failed {
		t.Fatal("watcher ignored configured retention or restarted terminal work")
	}
}
