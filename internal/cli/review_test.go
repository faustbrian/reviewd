package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestInboxAndShowExposeDurableStateWithoutStartingOrPosting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	configuration := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configuration, []byte("schema_version = 1\n[state]\ndirectory = "+strings.ReplaceAll(strconvQuote(directory), "\\", "\\\\")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 1, PullID: 2, Number: 17, ActorID: 3, ActorLogin: "reviewer", State: store.Queued, WorkKey: "owned-run", Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	s.Claim(ctx, run.ID)
	payload := json.RawMessage(`{"schema_version":1,"event":"COMMENT","body":"**Inconclusive.**\n\nMissing requirement context.","comments":[],"payload_id":"exact-payload","gaps":[{"kind":"context","detail":"Missing requirement context."}]}`)
	if err := s.Prepare(ctx, run.ID, "exact-payload", payload, "thread-one", "turn-one"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, args := range [][]string{{"--config", configuration, "inbox", "--json"}, {"--config", configuration, "show", run.ID, "--json"}, {"--config", configuration, "show", run.ID}} {
		var output, diagnostics bytes.Buffer
		code := Run(ctx, args, &output, &diagnostics, Dependencies{})
		if code != 0 || !strings.Contains(output.String(), "prepared") || !strings.Contains(output.String(), run.ID) {
			t.Fatalf("durable command failed: %d %s %s", code, output.String(), diagnostics.String())
		}
		if args[2] == "show" && !strings.Contains(output.String(), "Inconclusive") {
			t.Error("show lost the exact proposed body")
		}
	}
	var output, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"--config", configuration, "show", "unknown-run"}, &output, &diagnostics, Dependencies{}); code == 0 || !strings.Contains(diagnostics.String(), "not found") {
		t.Error("missing run was presented as empty success")
	}
}

func strconvQuote(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestRefreshCommandUpdatesExactStoredPreviewWithoutCodex(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	var failed atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.URL.Path != "/graphql" {
			t.Error("check refresh attempted remote mutation")
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":43,"login":"reviewer"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/pulls/17":
			fmt.Fprintf(w, `{"id":700,"number":17,"state":"open","draft":false,"title":"Source review","body":"Caller parity","updated_at":"2026-10-13T14:00:00Z","user":{"id":42,"login":"author"},"base":{"sha":%q,"ref":"main","repo":{"id":99,"full_name":"owner/repo"}},"head":{"sha":%q,"ref":"feature"},"changed_files":1,"commits":1}`, sha, sha)
		case "/repos/owner/repo/rules/branches/main":
			fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":0}]}}]`)
		case "/repos/owner/repo/branches/main":
			fmt.Fprint(w, `{"protected":false}`)
		case "/repos/owner/repo/commits/" + sha + "/check-runs":
			fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		case "/repos/owner/repo/commits/" + sha + "/statuses":
			state := "success"
			if failed.Load() {
				state = "failure"
			}
			fmt.Fprintf(w, `[{"id":9,"context":"quality","state":%q}]`, state)
		case "/graphql":
			var input struct{ Query string }
			json.NewDecoder(r.Body).Decode(&input)
			field := "reviewThreads"
			if strings.Contains(input.Query, "closingIssuesReferences") {
				field = "closingIssuesReferences"
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,%q:{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`, field)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer peer.Close()
	client, _ := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	pull, err := client.Pull(ctx, "owner/repo", 17)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.Context(ctx, pull)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := client.Checks(ctx, pull)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := config.Resolve(nil, "owner/repo", nil, nil)
	prepared, err := review.Prepare(review.Boundary{Pull: pull, Actor: github.Identity{ID: 43, Login: "reviewer"}, Configuration: policy, ContextDigest: remote.Digest, Checks: checks}, review.Report{Conclusion: "APPROVE", Findings: []review.Finding{}, Gaps: []review.ResultGap{}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	configuration := filepath.Join(root, "config.toml")
	os.WriteFile(configuration, []byte("schema_version = 1\n[state]\ndirectory = "+strconvQuote(directory)+"\n"), 0600)
	durable, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	run, _, err := durable.Create(ctx, store.Run{Repository: pull.Repository, RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "reviewer", WorkKey: "stored-source-opinion", State: store.Queued, Configuration: frozen, Private: true})
	if err != nil {
		t.Fatal(err)
	}
	durable.Claim(ctx, run.ID)
	payload, _ := json.Marshal(prepared)
	if err := durable.Prepare(ctx, run.ID, prepared.PayloadID, payload, "existing-thread", "existing-turn"); err != nil {
		t.Fatal(err)
	}
	durable.Close()
	failed.Store(true)
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, Review: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
		t.Fatal("refresh started Codex")
		return codex.ReviewOutput{}, nil
	}}
	for _, jsonMode := range []bool{false, true} {
		args := []string{"--config", configuration, "refresh", run.ID}
		if jsonMode {
			args = append(args, "--json")
		}
		var out, diagnostics bytes.Buffer
		if code := Run(ctx, args, &out, &diagnostics, deps); code != 0 || !strings.Contains(out.String(), "Not ready.") || !strings.Contains(out.String(), "existing-thread") && jsonMode {
			t.Fatalf("stored check refresh command failed: %d %s %s", code, out.String(), diagnostics.String())
		}
	}
}
