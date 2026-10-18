package watch

import (
	"context"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestSkillSchedulerDispatchesTypedFeedbackWithoutAWorkflowBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	manager, err := config.NewManager([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[feedback]\nauthors=['author']\naction='auto'\n"), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	before, err := durable.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ActorID = 43
	after.RepositoryID = 99
	after.State = "ok"
	after.Initialized = true
	record, _ := json.Marshal(map[string]any{"pull": github.Pull{Author: github.Identity{ID: 42, Login: "author"}}})
	item := store.InboxItem{Repository: after.Repository, PullID: 700, Number: 17, ActorID: 43, Category: "feedback_handling", ExecutionKind: "feedback_v1", State: "queued", Action: "auto", Trigger: "review_submitted", EventKey: "human-event"}
	if err := durable.ApplyDiscovery(ctx, before, after, []store.Observation{{PullID: 700, Number: 17, Record: record}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	called := make(chan store.InboxItem, 1)
	client, _ := github.New(github.Options{Host: "github.com", Token: "fixture-token"})
	engine := Engine{Store: durable, Configuration: manager, client: client, Execute: func(_ context.Context, _ *config.Document, _ *github.Client, selected store.InboxItem) (store.Run, error) {
		called <- selected
		return store.Run{State: store.AgentCompleted}, nil
	}}
	engine.initialize()
	engine.admitted = map[string]bool{}
	defer engine.Close()
	if err := engine.Admit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if got.ExecutionKind != "feedback_v1" {
			t.Fatal("wrong lane dispatched")
		}
	case <-ctx.Done():
		t.Fatal("ordinary feedback dispatch never started")
	}
	if err := engine.Complete(ctx, true); err != nil {
		t.Fatal(err)
	}
	if engine.Active() != 0 {
		t.Fatal("completed skill retained capacity")
	}
}
