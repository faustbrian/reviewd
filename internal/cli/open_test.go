package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestOpenPrintUsesDurableScopeWithoutConnectingOrLaunching(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, path := filepath.Join(directory, "state"), filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[state]\ndirectory="+strconv.Quote(state)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 17, ActorID: 43, ActorLogin: "reviewer", WorkKey: "open", State: store.Queued, Private: true, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		t.Fatal("local navigation connected to GitHub")
		return nil, nil
	}, OpenURL: func(context.Context, string) error {
		t.Fatal("print-only navigation launched a browser")
		return nil
	}}
	var output, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "open", run.ID, "--print", "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("could not obtain scoped PR URL: %d %s", code, diagnostics.String())
	}
	var result struct {
		URL      string `json:"url"`
		Launched bool   `json:"launched"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://github.com/owner/repo/pull/17" || result.Launched {
		t.Fatalf("navigation changed scope or falsely claimed launch: %+v", result)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != store.Queued {
		t.Fatal("navigation changed the run")
	}
	if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
		t.Fatal("fixture claim failed")
	}
	if err := s.Prepare(ctx, run.ID, "preview", json.RawMessage(`{}`), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecidePublication(ctx, run.ID, "preview"); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.BeginPublication(ctx, run.ID, "preview"); err != nil || !claimed {
		t.Fatal("fixture publication claim failed")
	}
	for _, target := range []string{"https://github.com/owner/repo/pull/17#pullrequestreview-81", "https://other.invalid/owner/repo/pull/17#pullrequestreview-81", "https://github.com/owner/repo/pull/18#pullrequestreview-81", "https://github.com/owner/repo/pull/17?token=private#pullrequestreview-81", "https://github.com/owner/repo/pull/17#unrelated"} {
		current, err := s.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		outcome, _ := json.Marshal(map[string]any{"review": map[string]any{"id": 81, "html_url": target}})
		if err := s.MarkOutcome(ctx, current, store.Published, outcome); err != nil {
			t.Fatal(err)
		}
		output.Reset()
		diagnostics.Reset()
		code := Run(ctx, []string{"--config", path, "open", run.ID, "--conversation", "--print", "--json"}, &output, &diagnostics, deps)
		if target == "https://github.com/owner/repo/pull/17#pullrequestreview-81" {
			if code != 0 || json.Unmarshal(output.Bytes(), &result) != nil || result.URL != target || result.Launched {
				t.Fatalf("recorded conversation lost scope: %d %s %s", code, output.String(), diagnostics.String())
			}
		} else if code == 0 || output.Len() != 0 {
			t.Fatal("navigation accepted an unscoped or credential-bearing conversation")
		}
	}
}
