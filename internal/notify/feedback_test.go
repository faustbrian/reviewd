package notify

import (
	"context"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAgentOutcomeNotificationsArePrivateFactualAndDeduplicated(t *testing.T) {
	for _, purpose := range []string{"review", "feedback"} {
		for _, state := range []store.State{store.AgentCompleted, store.AgentInterrupted, store.Failed, store.Cancelled} {
			t.Run(purpose+"/"+string(state), func(t *testing.T) {
				ctx := context.Background()
				s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				doc, err := config.Parse([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[notifications]\ndesktop=false\ngroup_interval='0s'\n"), "fixture")
				if err != nil {
					t.Fatal(err)
				}
				run, _, err := s.Create(ctx, store.Run{Executor: store.NativeExecutor, Purpose: purpose, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "native-outcome", State: store.Queued, Configuration: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				if won, err := s.ClaimAgent(ctx, run.ID); err != nil || !won {
					t.Fatal("claim", err)
				}
				if err := s.FinishAgent(ctx, run.ID, state, json.RawMessage(`{"text":"private-output-canary"}`), ""); err != nil {
					t.Fatal(err)
				}
				var messages []Message
				service := Service{Store: s, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
				if err := service.Pump(ctx, doc, true); err != nil {
					t.Fatal(err)
				}
				if len(messages) != 1 || !strings.Contains(strings.ToLower(messages[0].Body), purpose) || !strings.Contains(messages[0].Body, string(state)) || strings.Contains(messages[0].Body, "private-output-canary") || strings.Contains(messages[0].Body, "delivered") || strings.Contains(messages[0].Body, "publication verified") {
					t.Fatalf("native execution became workflow delivery or leaked output: %+v", messages)
				}
				if err := service.Pump(ctx, doc, true); err != nil || len(messages) != 1 {
					t.Fatal("terminal outcome replayed", err, len(messages))
				}
			})
		}
	}
}
