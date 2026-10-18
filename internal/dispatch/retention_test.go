package dispatch

import (
	"context"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeRetentionKeepsWorkIdentityAndUncertainHistory(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	policy, err := config.Resolve(nil, "owner/repo", nil, map[string]any{"review.instructions": "private-instruction-canary"})
	if err != nil {
		t.Fatal(err)
	}
	configuration, _ := json.Marshal(policy)
	var completed, uncertain store.Run
	for _, state := range []store.State{store.AgentCompleted, store.AgentInterrupted} {
		owner, err := s.BeginPrivateOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := s.Create(ctx, store.Run{Executor: store.NativeExecutor, Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: string(state), State: store.Queued, Configuration: configuration})
		if err != nil {
			t.Fatal(err)
		}
		if won, err := s.ClaimAgent(ctx, run.ID); err != nil || !won {
			t.Fatal("claim", err)
		}
		if err := s.RecordAgent(ctx, run.ID, "session-"+string(state), "chosen-model", nil); err != nil {
			t.Fatal(err)
		}
		outcome, _ := json.Marshal(agent.Output{ThreadID: "session-" + string(state), Completed: state == store.AgentCompleted, Text: "private-output-canary"})
		if err := s.FinishAgent(ctx, run.ID, state, outcome, ""); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if state == store.AgentCompleted {
			completed = run
		} else {
			uncertain = run
		}
	}
	report, err := ExpireHistory(ctx, s, 1, false, time.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Expired) != 1 || report.Expired[0] != completed.ID {
		t.Fatalf("wrong history expired: %+v", report)
	}
	done, err := s.Get(ctx, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(done)
	if done.State != store.AgentCompleted || done.ThreadID != "session-completed" || done.WorkKey != completed.WorkKey || done.Archive == nil || strings.Contains(string(raw), "private-output-canary") || strings.Contains(string(raw), "private-instruction-canary") {
		t.Fatalf("expiry lost identity or retained private text: %s", raw)
	}
	pending, err := s.Get(ctx, uncertain.ID)
	if err != nil || pending.Archive != nil || !strings.Contains(string(pending.Outcome), "private-output-canary") {
		t.Fatal("uncertain execution history was discarded", err)
	}
}
