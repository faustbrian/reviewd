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
	"strconv"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestOrdinarySkillCommandsLaunchAndShowWithoutWorkflowOwnership(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[state]\ndirectory="+strconv.Quote(filepath.Join(root, "profile"))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sha, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("reviewd attempted to own a skill's remote writes")
			http.Error(w, "denied", 500)
			return
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":43,"login":"operator"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/pulls/17":
			fmt.Fprintf(w, `{"id":700,"number":17,"state":"open","draft":false,"title":"Fixture","body":"","updated_at":"2026-10-18T00:00:00Z","user":{"id":42,"login":"author"},"base":{"sha":%q,"ref":"main","repo":{"id":99,"full_name":"owner/repo"}},"head":{"sha":%q,"ref":"feature"},"changed_files":1,"commits":1}`, sha, sha)
		case "/repos/owner/repo/commits/" + sha:
			fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, sha, tree)
		case "/repos/owner/repo/git/trees/" + tree:
			fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, tree)
		default:
			t.Errorf("unexpected workflow endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: server.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"review", "feedback"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, LaunchAgent: func(ctx context.Context, r agent.Request) (agent.Output, error) {
				calls++
				want := "review"
				if name == "feedback" {
					want = "handle-pr-feedback"
				}
				if r.Skill != want || r.URL != "https://github.com/owner/repo/pull/17" || r.Model != "gpt-6.1-sol" || r.Effort != "high" || r.Instructions != "" {
					t.Fatalf("ordinary skill launch changed: %+v", r)
				}
				var progress agent.Output
				if err := json.Unmarshal([]byte(`{"thread_id":"session-`+name+`","activity":"Reading current review feedback"}`), &progress); err != nil {
					t.Fatal(err)
				}
				if err := r.OnProgress(progress); err != nil {
					t.Fatal(err)
				}
				reader, err := store.OpenExisting(ctx, filepath.Join(root, "profile"))
				if err != nil {
					t.Fatal(err)
				}
				runs, err := reader.List(ctx)
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				document, parseErr := config.Parse(data, path)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				snapshot, snapshotErr := (&execution{}).terminalSnapshot(ctx, reader, document)
				reader.Close()
				if snapshotErr != nil {
					t.Fatal(snapshotErr)
				}
				visible := false
				for _, entry := range snapshot.Entries {
					visible = visible || strings.Contains(entry.Details, "Reading current review feedback")
				}
				if !visible {
					t.Fatal("live TUI details lack activity")
				}
				if err != nil {
					t.Fatal(err)
				}
				var active store.Run
				for _, run := range runs {
					if run.State == store.AgentRunning {
						active = run
					}
				}
				if active.ID == "" || active.Progress != "Reading current review feedback" || active.ThreadID != progress.ThreadID {
					t.Fatalf("live activity not durable for second reader: %+v", active)
				}
				for _, asJSON := range []bool{false, true} {
					var live, diagnostics bytes.Buffer
					args := []string{"--config", path, "show", active.ID}
					if asJSON {
						args = append(args, "--json")
					}
					if code := Run(ctx, args, &live, &diagnostics, Dependencies{}); code != 0 || !strings.Contains(live.String(), "Reading current review feedback") {
						t.Fatalf("live show missing activity: %d %s %s", code, live.String(), diagnostics.String())
					}
				}
				return agent.Output{ThreadID: "session-" + name, Completed: true, Text: "Skill finished using its own workflow."}, nil
			}}
			var out, diag bytes.Buffer
			code := Run(context.Background(), []string{"--config", path, name, "https://github.com/owner/repo/pull/17", "--json"}, &out, &diag, deps)
			var response struct {
				Run store.Run `json:"run"`
			}
			decodeErr := json.Unmarshal(out.Bytes(), &response)
			if code != 0 || calls != 1 || decodeErr != nil || response.Run.State != store.AgentCompleted || !strings.Contains(out.String(), "Skill finished") {
				t.Fatalf("ordinary command failed: code=%d calls=%d out=%s diagnostics=%s", code, calls, out.String(), diag.String())
			}
		})
	}
}
