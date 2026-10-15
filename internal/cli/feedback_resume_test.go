package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
)

func TestFeedbackResumePreviewIsReadOnlyAndConfirmationNamesTheExistingRun(t *testing.T) {
	path, durable, item := feedbackDecisionFixture(t)
	ctx := context.Background()
	data, _ := os.ReadFile(path)
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.Resolve(document, item.Repository, nil, map[string]any{"feedback.model": "original-one-run-model"})
	if err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(policy)
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	run, _, err := durable.Create(ctx, store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "author", WorkKey: "owned-resume-preview", State: store.Queued, Configuration: frozen})
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.FinishFeedback(ctx, run.ID, store.FeedbackPending, json.RawMessage(`{"schema_version":1,"retained":{},"remaining":["CI is pending"],"execution":{"delivery":{"started":false}}}`), ""); err != nil {
		t.Fatal(err)
	}
	connections := 0
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		connections++
		return nil, errors.New("owned authentication boundary")
	}}
	invoke := func(extra ...string) (int, string, string) {
		t.Helper()
		var out, diagnostics strings.Builder
		code := Run(ctx, append([]string{"--config", path, "--json", "feedback-resume", run.ID}, extra...), &out, &diagnostics, deps)
		return code, out.String(), diagnostics.String()
	}
	code, output, diagnostics := invoke()
	if code != 0 {
		t.Fatalf("same-run continuation preview is unavailable: %d %s", code, diagnostics)
	}
	var preview struct {
		Confirmation, Model, State string
		Run                        struct{ ID string }
	}
	if json.Unmarshal([]byte(output), &preview) != nil || preview.Confirmation == "" || preview.Run.ID != run.ID || preview.Model != "original-one-run-model" || preview.State != "preview_only" || connections != 0 {
		t.Fatal("preview changed authority, model or run", output, connections)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, durable, document)
	if err != nil {
		t.Fatal(err)
	}
	var retained terminal.Entry
	for _, entry := range snapshot.Entries {
		if entry.RunID == run.ID {
			retained = entry
		}
	}
	if retained.Purpose != "feedback" || retained.Section != "feedback" || retained.FeedbackResumeConfirmation != preview.Confirmation || retained.Model != preview.Model {
		t.Fatal("terminal lost same-run continuation or original model", retained)
	}
	for _, extra := range [][]string{{"--confirm", "stale"}, {"--confirm", preview.Confirmation, "--model", "replacement-model"}} {
		if code, _, _ := invoke(extra...); code == 0 || connections != 0 {
			t.Fatal("stale confirmation or new model contacted GitHub")
		}
	}
	code, _, diagnostics = invoke("--confirm", preview.Confirmation)
	if code == 0 || connections != 1 || !strings.Contains(diagnostics, "owned authentication boundary") {
		t.Fatal("confirmed continuation did not reach its configured authenticated boundary", code, connections, diagnostics)
	}
	current, _ := durable.Get(ctx, run.ID)
	if current.State != store.FeedbackPending || current.OwnerID != owner.ID {
		t.Fatal("preview/authentication failure replaced existing work")
	}
	_, err = terminalAction(ctx, []string{"--config", path}, terminal.Action{Kind: "feedback-resume", Entry: retained}, deps)
	if err == nil || connections != 2 || !strings.Contains(err.Error(), "owned authentication boundary") {
		t.Fatal("terminal continuation did not share the ordinary command", err)
	}
}
