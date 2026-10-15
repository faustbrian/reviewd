package notify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestFeedbackRunNotificationsKeepTheirOwnPurposeAndPrivateInputs(t *testing.T) {
	for _, state := range []store.State{store.HandlingFeedback, store.FeedbackPending, store.FeedbackDelivered, store.Partial, store.Failed, store.Cancelled} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			owner, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			run, _, err := durable.Create(ctx, store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "author", WorkKey: "feedback-notification", State: store.Queued, Configuration: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if won, err := durable.ClaimFeedback(ctx, run.ID); err != nil || !won {
				t.Fatal("feedback claim", err)
			}
			if state == store.HandlingFeedback {
				if err := durable.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
					t.Fatal(err)
				}
				request := clarification.Request{ID: "feedback-input", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "private-feedback-question-canary"}}}
				if err := durable.OpenInput(ctx, run.ID, request); err != nil {
					t.Fatal(err)
				}
			} else if err := durable.FinishFeedback(ctx, run.ID, state, json.RawMessage(`{"private_source":"private-feedback-result-canary"}`), ""); err != nil {
				t.Fatal(err)
			}
			var messages []Message
			service := Service{Store: durable, Terminal: func(_ context.Context, message Message) error { messages = append(messages, message); return nil }}
			if err := service.Pump(ctx, document(t, "desktop=false\n"), true); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || !strings.Contains(strings.ToLower(messages[0].Body), "feedback") || strings.Contains(messages[0].Body, "Publication") || strings.Contains(messages[0].Body, "private-feedback-") {
				t.Fatalf("feedback purpose/state/privacy lost: %+v", messages)
			}
			if state == store.HandlingFeedback && !strings.Contains(messages[0].Body, "reviewd inputs "+run.ID) {
				t.Fatal("pending feedback question has no explicit input action")
			}
			if err := service.Pump(ctx, document(t, "desktop=false\n"), true); err != nil || len(messages) != 1 {
				t.Fatal("unchanged feedback notification replayed", err)
			}
		})
	}
}
