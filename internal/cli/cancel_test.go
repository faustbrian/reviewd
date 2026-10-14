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

func TestCancelQueuedRunIsScopedAndDurableWithoutGitHub(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state := filepath.Join(directory, "state")
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[state]\ndirectory="+strconv.Quote(state)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seed := func(key string) store.Run {
		t.Helper()
		run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: key, State: store.Queued, Private: true, Configuration: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	selected, other := seed("selected"), seed("independent")
	var output, diagnostics bytes.Buffer
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		t.Fatal("local cancellation connected to GitHub")
		return nil, nil
	}}
	code := Run(ctx, []string{"--config", path, "cancel", selected.ID, "--json"}, &output, &diagnostics, deps)
	if code != 0 {
		t.Fatalf("queued cancellation failed: %d %s", code, diagnostics.String())
	}
	var report struct {
		Run store.Run `json:"run"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Run.ID != selected.ID || report.Run.State != store.Cancelled {
		t.Fatalf("cancellation did not expose the selected terminal state: %s", output.String())
	}
	current, err := s.Get(ctx, selected.ID)
	if err != nil || current.State != store.Cancelled {
		t.Fatal("another client did not observe cancellation")
	}
	if claimed, err := s.Claim(ctx, selected.ID); err != nil || claimed {
		t.Fatal("cancelled queued run could still start")
	}
	current, err = s.Get(ctx, other.ID)
	if err != nil || current.State != store.Queued {
		t.Fatal("cancellation affected another run on the same PR")
	}
}
