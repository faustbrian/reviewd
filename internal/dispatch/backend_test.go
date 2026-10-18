package dispatch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestMixedBackendsUseLaneSelectionAndDurableNativeOutcomes(t *testing.T) {
	for _, selected := range []string{"reviewer", "feedback"} {
		t.Run(selected, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			doc, err := config.Parse([]byte("schema_version=1\n["+selected+"]\nbackend='claude'\n"), "operator")
			if err != nil {
				t.Fatal(err)
			}
			for _, purpose := range []string{"review", "feedback"} {
				want := "codex"
				if (purpose == "review" && selected == "reviewer") || (purpose == "feedback" && selected == "feedback") {
					want = "claude"
				}
				svc := Service{Store: s, GitHub: remoteFixture{}, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(_ context.Context, r agent.Request) (agent.Output, error) {
					if r.Backend != want || r.Executable != want {
						t.Fatalf("wrong lane backend: %+v; want %s", r, want)
					}
					if want == "claude" && (r.Model != "" || r.Effort != "") {
						t.Fatal("Codex defaults leaked into Claude")
					}
					output := agent.Output{ThreadID: want + "-fresh", Text: "Finished", Completed: true, Activity: "Inspecting feedback", Usage: json.RawMessage(`{"input_tokens":11,"output_tokens":3}`)}
					if err := r.OnProgress(output); err != nil {
						return output, err
					}
					return output, nil
				}}
				run, err := svc.Run(ctx, doc, Job{Target: github.PullReference{Repository: "github.com/owner/repo", Number: 17}, Purpose: purpose})
				if err != nil {
					t.Fatal(err)
				}
				if run.Executor != want+"_exec_v1" || run.State != store.AgentCompleted || run.ThreadID != want+"-fresh" {
					t.Fatalf("wrong native outcome: %+v", run)
				}
				if string(run.Usage) != `{"input_tokens":11,"output_tokens":3}` {
					t.Fatalf("native usage not durable: %s", run.Usage)
				}
				var snapshot config.Snapshot
				if err := json.Unmarshal(run.Configuration, &snapshot); err != nil {
					t.Fatal(err)
				}
				prefix := "reviewer."
				if purpose == "feedback" {
					prefix = "feedback."
				}
				if snapshot.String(prefix+"backend") != want {
					t.Fatal("persisted backend differs from executed selection")
				}
			}
		})
	}
}
