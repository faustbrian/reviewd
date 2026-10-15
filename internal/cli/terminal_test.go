package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
)

func TestTerminalActionsShareDurableControlsAndPrivateStart(t *testing.T) {
	path, durable, item := terminalFixture(t)
	ctx := context.Background()
	entry := terminal.Entry{Key: item.ID, ItemID: item.ID, EventKey: item.EventKey, Project: item.Repository}
	var launched string
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		t.Fatal("local terminal control connected to GitHub")
		return nil, nil
	}, OpenURL: func(_ context.Context, target string) error { launched = target; return nil }}
	apply := func(action terminal.Action) {
		t.Helper()
		action.Entry = entry
		if _, err := terminalAction(ctx, []string{"--config", path}, action, deps); err != nil {
			t.Fatal(err)
		}
	}
	apply(terminal.Action{Kind: "pause"})
	apply(terminal.Action{Kind: "snooze", Duration: "1h"})
	controls, err := durable.Controls(ctx, time.Now())
	if err != nil || len(controls.PausedProjects) != 1 || len(controls.Snoozes) != 1 {
		t.Fatal("terminal holds were not persisted")
	}
	apply(terminal.Action{Kind: "start", Model: "fixture-review-model", Effort: "low"})
	items, _, err := durable.Inbox(ctx)
	controls, controlErr := durable.Controls(ctx, time.Now())
	runs, runErr := durable.List(ctx)
	if err != nil || controlErr != nil || runErr != nil || len(items) != 1 || items[0].State != "queued" || !items[0].Manual || items[0].Overrides["reviewer.model"] != "fixture-review-model" || items[0].Overrides["reviewer.reasoning_effort"] != "low" || len(controls.Snoozes) != 0 || len(controls.PausedProjects) != 1 || len(runs) != 0 {
		t.Fatal("terminal start lost scope/overrides, resumed a paused project or started a worker")
	}
	apply(terminal.Action{Kind: "follow"})
	follows, err := durable.FollowStates(ctx, item.Repository, item.ActorID)
	if err != nil || !follows[item.PullID].Following {
		t.Fatal("terminal follow did not reach the application store")
	}
	apply(terminal.Action{Kind: "open"})
	if launched != "https://github.com/owner/repo/pull/17" {
		t.Fatal("terminal opener trusted unscoped remote URL")
	}
	apply(terminal.Action{Kind: "resume"})
	apply(terminal.Action{Kind: "skip"})
	items, _, err = durable.Inbox(ctx)
	controls, controlErr = durable.Controls(ctx, time.Now())
	history, historyErr := durable.DiscoveryItems(ctx, item.Repository, item.ActorID)
	if err != nil || controlErr != nil || historyErr != nil || len(items) != 0 || len(history) != 1 || history[0].State != "skipped" || len(controls.PausedProjects) != 0 {
		t.Fatal("terminal skip/resume did not reach shared durable actions")
	}
}

func TestTerminalFeedbackShowsIndependentModelWithoutGrantingPrivateConsent(t *testing.T) {
	path, durable, item := terminalFixture(t)
	ctx := context.Background()
	scope, err := durable.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	item.ID, item.Category, item.ExecutionKind = "", "feedback_handling", "feedback_v1"
	if err := durable.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n[feedback]\naction='ask'\nmodel='separate-fixing-model'\nreasoning_effort='low'\n")...)
	doc, err := config.Parse(raw, path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, durable, doc)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("feedback missing: %+v %v", snapshot, err)
	}
	entry := snapshot.Entries[0]
	if entry.Section != "feedback" || entry.Model != "separate-fixing-model" || entry.Effort != "low" || !strings.Contains(entry.Details, "separate feedback-handling") {
		t.Fatalf("private reviewer presented as fixing worker: %+v", entry)
	}
	if _, err := terminalAction(ctx, []string{"--config", path}, terminal.Action{Kind: "start", Entry: entry}, Dependencies{}); err == nil {
		t.Fatal("private terminal start granted fixing authority")
	}
}

func TestTerminalSnapshotsDoNotEchoSensitiveDefaults(t *testing.T) {
	path, durable, _ := terminalFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := "sk-proj-" + strings.Repeat("x", 32)
	// Namespaced identifiers are valid model syntax; presentation must still
	// redact an embedded credential-shaped value without changing the snapshot.
	configured := "fixture/" + sensitive
	data = append(data, []byte("\n[reviewer]\nmodel=\""+configured+"\"\n")...)
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(context.Background(), durable, document)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sensitive) {
		t.Fatal("terminal presentation leaked a sensitive-shaped configured model")
	}
	resolved, err := config.Resolve(document, "github.com/owner/repo", nil, nil)
	if err != nil || resolved.String("reviewer.model") != configured {
		t.Fatal("presentation redaction replaced the actual model configuration")
	}
}

func TestRedirectedInteractiveInboxRemainsPlainOrJSON(t *testing.T) {
	path, _, _ := terminalFixture(t)
	for _, jsonMode := range []bool{false, true} {
		var output, diagnostics strings.Builder
		args := []string{"--config", path, "inbox", "--interactive"}
		if jsonMode {
			args = append(args, "--json")
		}
		if code := Run(context.Background(), args, &output, &diagnostics, Dependencies{}); code != 0 {
			t.Fatalf("redirected inbox failed: %s", diagnostics.String())
		}
		if strings.Contains(output.String(), "\x1b") {
			t.Fatal("terminal control sequences entered redirected output")
		}
		if jsonMode && !json.Valid([]byte(output.String())) {
			t.Fatal("interactive flag corrupted JSON")
		}
	}
}

func TestTerminalOpenerReportsLauncherFailureWithoutChangingDecision(t *testing.T) {
	path, durable, item := terminalFixture(t)
	_, err := terminalAction(context.Background(), []string{"--config", path}, terminal.Action{Kind: "open", Entry: terminal.Entry{ItemID: item.ID}}, Dependencies{OpenURL: func(context.Context, string) error { return errors.New("desktop unavailable") }})
	if err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatal("missing launcher had no usable correction")
	}
	items, _, err := durable.Inbox(context.Background())
	if err != nil || len(items) != 1 || items[0].State != "pending_decision" {
		t.Fatal("failed browser launch granted a decision")
	}
}

func TestWatcherAnnouncementKeepsModelPresentationCredentialSafe(t *testing.T) {
	var member atomic.Bool
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("REQUEST", "User", 43)} }, &member)
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", peer)
	deps.Source = func(context.Context, github.Pull) (*source.Snapshot, error) {
		t.Fatal("held initial backlog acquired source")
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := "sk-proj-" + strings.Repeat("x", 32)
	configuration := strings.Replace(string(data), "[actions]", "[actions]\npublish_review=\"auto\"", 1) + "\n[reviewer]\nmodel=\"fixture/" + sensitive + "\"\n"
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics strings.Builder
	if code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatal("owned announcement journey failed")
	}
	if strings.Contains(output.String(), sensitive) || strings.Contains(diagnostics.String(), sensitive) {
		t.Fatal("watcher announcement exposed a sensitive-shaped model value")
	}
	if !strings.Contains(diagnostics.String(), "GitHub reviewer") || !strings.Contains(diagnostics.String(), "publish=auto") {
		t.Fatal("credential filtering hid posting identity or automation policy")
	}
}

func TestTerminalNewStartDefaultsDoNotInheritEarlierRunOverrides(t *testing.T) {
	for _, linkedRun := range []bool{false, true} {
		path, durable, item := terminalFixture(t)
		ctx := context.Background()
		overrides := map[string]any{"reviewer.model": "earlier-model", "reviewer.reasoning_effort": "low"}
		current, err := durable.DecideItem(ctx, item.ID, true, false, overrides)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if linkedRun {
			document, err := config.Parse(data, path)
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := config.Resolve(document, item.Repository, nil, overrides)
			if err != nil {
				t.Fatal(err)
			}
			configuration, _ := json.Marshal(frozen)
			if _, _, err := durable.CreateForItem(ctx, store.Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "frozen", State: store.Queued, Configuration: configuration}, current); err != nil {
				t.Fatal(err)
			}
		}
		data = append(data, []byte("\n[reviewer]\nmodel=\"new-default-model\"\nreasoning_effort=\"high\"\n")...)
		document, err := config.Parse(data, path)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := (&execution{}).terminalSnapshot(ctx, durable, document)
		if err != nil || len(snapshot.Entries) != 1 {
			t.Fatal("could not load exact current terminal entry")
		}
		entry := snapshot.Entries[0]
		if entry.Model != "new-default-model" || entry.Effort != "high" {
			t.Fatal("new-start confirmation inherited an earlier decision/run model rather than current project defaults")
		}
		if !strings.Contains(entry.Details, "earlier-model") {
			t.Fatal("presentation lost the earlier decision/run's actual frozen settings")
		}
	}
}
