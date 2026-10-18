package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"os"
	"path/filepath"
	"testing"
)

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
