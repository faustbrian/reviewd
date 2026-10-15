package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
)

func feedbackDecisionFixture(t *testing.T) (string, *store.Store, store.InboxItem) {
	t.Helper()
	path, durable, item := terminalFixture(t)
	ctx := context.Background()
	scope, err := durable.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	item.ID, item.Category, item.ExecutionKind = "", "feedback_handling", "feedback_v1"
	item.Trigger, item.Action, item.Explanation = "review_submitted", "ask", "A human submitted feedback."
	if err := durable.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n[feedback]\nauthors=['@me']\naction='ask'\nmodel='fix-project-model'\nreasoning_effort='high'\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	items, _, err := durable.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("feedback fixture: %+v %v", items, err)
	}
	return path, durable, items[0]
}

func TestFeedbackDecisionPreviewAndExactExplicitConsent(t *testing.T) {
	path, durable, item := feedbackDecisionFixture(t)
	ctx := context.Background()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		t.Fatal("local feedback choice connected to GitHub")
		return nil, nil
	}}
	invoke := func(args ...string) (int, string, string) {
		t.Helper()
		var out, diagnostics strings.Builder
		code := Run(ctx, append([]string{"--config", path, "feedback", item.ID, "--json"}, args...), &out, &diagnostics, deps)
		return code, out.String(), diagnostics.String()
	}
	code, output, diagnostics := invoke("--model", "one-run-fixer", "--reasoning-effort", "low")
	if code != 0 {
		t.Fatalf("feedback preview unavailable: %d %s", code, diagnostics)
	}
	var preview struct {
		Confirmation string          `json:"confirmation"`
		Scope        string          `json:"scope"`
		Model        string          `json:"model"`
		Effort       string          `json:"reasoning_effort"`
		Item         store.InboxItem `json:"item"`
	}
	if err := json.Unmarshal([]byte(output), &preview); err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"fix", "commit", "push", "metadata", "replies", "resolve", "reviewer", "merge", "force", "bypass"} {
		if !strings.Contains(preview.Scope, phrase) {
			t.Fatalf("scope omitted %q: %s", phrase, preview.Scope)
		}
	}
	if preview.Confirmation == "" || preview.Model != "one-run-fixer" || preview.Effort != "low" || preview.Item.HeadSHA != item.HeadSHA || preview.Item.ContextKey != item.ContextKey {
		t.Fatalf("preview lost target or feedback model: %+v", preview)
	}
	items, _, err := durable.Inbox(ctx)
	if err != nil || items[0].Manual || items[0].DecisionID != "" {
		t.Fatal("preview granted modifying authority")
	}
	if code, _, _ := invoke("--confirm", "wrong-displayed-target"); code == 0 {
		t.Fatal("invalid confirmation granted authority")
	}
	if code, _, diagnostics := invoke("--confirm", preview.Confirmation, "--model", "one-run-fixer", "--reasoning-effort", "low"); code != 0 {
		t.Fatalf("explicit feedback choice failed: %s", diagnostics)
	}
	items, _, err = durable.Inbox(ctx)
	runs, runErr := durable.List(ctx)
	if err != nil || runErr != nil || len(runs) != 0 || len(items) != 1 || !items[0].Manual || items[0].State != "queued" || items[0].DecisionID == "" || items[0].Overrides["feedback.model"] != "one-run-fixer" || items[0].Overrides["feedback.reasoning_effort"] != "low" || items[0].Overrides["reviewer.model"] != nil {
		t.Fatalf("feedback choice escalated reviewer or started execution: %+v %v %v", items, err, runErr)
	}
}

func TestFeedbackDecisionCannotConfirmChangedDisplayedTarget(t *testing.T) {
	for _, changed := range []string{"head", "base", "inventory", "event"} {
		t.Run(changed, func(t *testing.T) {
			path, durable, item := feedbackDecisionFixture(t)
			ctx := context.Background()
			var out, diagnostics strings.Builder
			args := []string{"--config", path, "feedback", item.ID, "--json"}
			if code := Run(ctx, args, &out, &diagnostics, Dependencies{}); code != 0 {
				t.Fatalf("preview unavailable: %s", diagnostics.String())
			}
			var preview struct {
				Confirmation string `json:"confirmation"`
			}
			if err := json.Unmarshal([]byte(out.String()), &preview); err != nil {
				t.Fatal(err)
			}
			scope, err := durable.DiscoveryScope(ctx, item.Repository)
			if err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "head":
				item.HeadSHA = strings.Repeat("c", 40)
			case "base":
				item.TargetBaseSHA = strings.Repeat("d", 40)
			case "inventory":
				item.ContextKey = "new-human-feedback"
			case "event":
				item.EventKey = "new-feedback-event"
			}
			if err := durable.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			diagnostics.Reset()
			if code := Run(ctx, append(args, "--confirm", preview.Confirmation), &out, &diagnostics, Dependencies{}); code == 0 {
				t.Fatal("old displayed target authorized changed work")
			}
			items, _, err := durable.Inbox(ctx)
			if err != nil || len(items) != 1 || items[0].Manual || items[0].DecisionID != "" {
				t.Fatal("rejected stale confirmation changed authority")
			}
		})
	}
}

func TestTerminalFeedbackConfirmationUsesSeparateDurableChoice(t *testing.T) {
	path, durable, _ := feedbackDecisionFixture(t)
	ctx := context.Background()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, durable, doc)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("feedback absent: %v", err)
	}
	entry := snapshot.Entries[0]
	if _, err := terminalAction(ctx, []string{"--config", path}, terminal.Action{Kind: "feedback", Entry: entry, Model: "terminal-fixer", Effort: "medium"}, Dependencies{}); err != nil {
		t.Fatalf("separate terminal feedback decision unavailable: %v", err)
	}
	items, _, err := durable.Inbox(ctx)
	if err != nil || len(items) != 1 || !items[0].Manual || items[0].Overrides["feedback.model"] != "terminal-fixer" || items[0].Overrides["feedback.reasoning_effort"] != "medium" {
		t.Fatalf("terminal did not use shared feedback decision: %+v %v", items, err)
	}
}

func TestFeedbackDecisionRejectsLegacyAndRemovedOperatorScope(t *testing.T) {
	for _, change := range []string{"private-item", "disabled-authors", "removed-project", "invalid-effort"} {
		t.Run(change, func(t *testing.T) {
			path, durable, item := feedbackDecisionFixture(t)
			if change == "private-item" {
				path, durable, item = terminalFixture(t)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "disabled-authors":
				data = []byte(strings.Replace(string(data), "authors=['@me']", "authors=[]", 1))
			case "removed-project":
				data = []byte(strings.Replace(string(data), `repositories=["owner/repo"]`, `repositories=[]`, 1))
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--config", path, "feedback", item.ID, "--json"}
			if change == "invalid-effort" {
				args = append(args, "--reasoning-effort", "not-an-effort")
			}
			var out, diagnostics strings.Builder
			if code := Run(context.Background(), args, &out, &diagnostics, Dependencies{}); code == 0 {
				t.Fatal("ineligible feedback preview accepted removed/wrong scope")
			}
			items, _, err := durable.Inbox(context.Background())
			if err != nil || len(items) != 1 || items[0].Manual || items[0].DecisionID != "" {
				t.Fatal("rejected feedback command recorded modifying authority")
			}
		})
	}
}
