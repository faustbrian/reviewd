package cli

import (
	"context"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorChecksSelectedBackendsWithoutRequiringUnusedCLI(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "native-fixture")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nif [ \"$1\" != --version ]; then exit 91; fi\nprintf '2.1.292 fixture\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, mixed := range []bool{false, true} {
		text := "schema_version=1\n[reviewer]\nbackend='claude'\nexecutable=" + strconv.Quote(executable) + "\n"
		if mixed {
			text += "[feedback]\nbackend='codex'\nexecutable='/definitely/missing/codex'\naction='auto'\n"
		}
		document, err := config.Parse([]byte(text), "fixture")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := config.ResolveDefaults(document, nil)
		if err != nil {
			t.Fatal(err)
		}
		issues := probeCodex(context.Background(), []config.Snapshot{snapshot})
		claudeOK, codexMissing := false, false
		for _, issue := range issues {
			claudeOK = claudeOK || (strings.Contains(issue.Check, "Claude Code") && issue.Status == "ok")
			codexMissing = codexMissing || (strings.Contains(issue.Check, "Codex") && issue.Status == "error")
			if !mixed && strings.Contains(issue.Check, "Codex") {
				t.Fatalf("unused Codex prerequisite: %+v", issues)
			}
		}
		if !claudeOK || codexMissing != mixed {
			t.Fatalf("independent backend diagnostics missing: mixed=%v %+v", mixed, issues)
		}
	}
}

func TestTerminalDistinguishesCurrentSelectionFromAcceptedBackend(t *testing.T) {
	_, durable, item := terminalFixture(t)
	ctx := context.Background()
	if _, err := durable.DecideItem(ctx, item.ID, true, false, nil); err != nil {
		t.Fatal(err)
	}
	items, _, err := durable.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("current item missing: %v", err)
	}
	item = items[0]
	document, err := config.Parse([]byte("schema_version=1\n[reviewer]\nbackend='claude'\nmodel='sonnet'\n"), "before-reload")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := config.Resolve(document, item.Repository, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	configuration, _ := json.Marshal(frozen)
	run, created, err := durable.CreateForItem(ctx, store.Run{Executor: store.ClaudeExecutor, Purpose: "review", Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "operator", WorkKey: "accepted-claude", State: store.Queued, Configuration: configuration}, item)
	if err != nil || !created {
		t.Fatalf("linked run missing: %v", err)
	}
	if claimed, err := durable.ClaimAgent(ctx, run.ID); err != nil || !claimed {
		t.Fatalf("claim failed: %v", err)
	}
	if err := durable.RecordAgent(ctx, run.ID, "accepted-claude-session", "sonnet", nil); err != nil {
		t.Fatal(err)
	}
	scope, err := durable.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Parse([]byte("schema_version=1\n"), "after-reload")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, durable, reloaded)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("linked terminal entry missing: %+v %v", snapshot, err)
	}
	details := snapshot.Entries[0].Details
	if !strings.Contains(details, "Prospective operator settings — backend: Codex") || !strings.Contains(details, "Backend: Claude Code") || !strings.Contains(details, "Claude Code session: accepted-claude-session") {
		t.Fatalf("current policy confused with accepted execution: %s", details)
	}
}
