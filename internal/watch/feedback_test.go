package watch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

func feedbackAdmissionItem(t *testing.T, f *admissionFixture) store.InboxItem {
	t.Helper()
	ctx := context.Background()
	before, err := f.durable.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.ActorID, after.RepositoryID, after.State = 43, 99, "ok"
	item := store.InboxItem{Repository: before.Repository, ActorID: 43, PullID: 701, Number: 1, Category: "feedback_handling", ExecutionKind: "feedback_v1", Trigger: "review_submitted", Action: "auto", State: "queued", EventKey: "human-feedback", HeadSHA: strings.Repeat("b", 40), TargetBaseSHA: strings.Repeat("a", 40)}
	record, _ := json.Marshal(map[string]any{"pull": map[string]any{"author": f.author}})
	if err := f.durable.ApplyDiscovery(ctx, before, after, []store.Observation{{PullID: 701, Number: 1, Record: record}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, _, err := f.durable.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("missing fixture: %+v %v", items, err)
	}
	return items[0]
}

func TestFeedbackAdmissionCannotFallBackToPrivateReviewer(t *testing.T) {
	f := newAdmissionFixture(t)
	feedbackAdmissionItem(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	privateStarts := make(chan int, 1)
	stop := errors.New("owned private source checkpoint")
	engine := &Engine{Store: f.durable, Configuration: f.manager, Now: func() time.Time { return f.now }, Reviewer: func(client *github.Client) review.Service {
		return review.Service{Store: f.durable, GitHub: client, Worker: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
			t.Error("fixture unexpectedly started model")
			return codex.ReviewOutput{}, stop
		}, SkillPath: func(config.Snapshot) (string, error) { return "", errors.New("no model") }, Source: func(_ context.Context, pull github.Pull) (*source.Snapshot, error) {
			privateStarts <- pull.Number
			return nil, stop
		}}
	}}
	engine.initialize()
	engine.client, engine.admitted = f.client, map[string]bool{}
	defer engine.Close()
	for range 6 {
		if err := engine.Admit(ctx); err != nil || engine.Active() != 0 {
			t.Fatalf("unavailable feedback executor disrupted monitoring or started a reviewer: active=%d err=%v", engine.Active(), err)
		}
	}
	items, _, err := f.durable.Inbox(ctx)
	runs, runErr := f.durable.List(ctx)
	if err != nil || runErr != nil || len(items) != 1 || items[0].State != "queued" || items[0].ExecutionHold == "" || items[0].Attempts != 0 || len(runs) != 0 {
		t.Fatalf("unavailable backend consumed fixing work: %+v %+v %v %v", items, runs, err, runErr)
	}
	f.add(t, 2, "review_requested", f.now)
	if err := engine.Admit(ctx); err != nil || engine.Active() != 1 {
		t.Fatalf("unrelated private work was blocked: active=%d err=%v", engine.Active(), err)
	}
	if err := engine.Complete(ctx, true); !errors.Is(err, stop) {
		t.Fatalf("private source did not remain available: %v", err)
	}
	select {
	case number := <-privateStarts:
		if number != 2 {
			t.Fatalf("feedback entered private source acquisition: %d", number)
		}
	default:
		t.Fatal("unrelated private source not acquired")
	}
}

func TestFeedbackRevocationUsesItsOwnAuthorsBeforeCooldown(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "revoked"}[allowed], func(t *testing.T) {
			f := newAdmissionFixture(t, github.Identity{ID: 43, Login: "reviewer"})
			item := feedbackAdmissionItem(t, f)
			filter := "['@me']"
			if !allowed {
				filter = "[]"
			}
			if err := f.manager.Reload([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[feedback]\nauthors=" + filter + "\naction='auto'\n")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			engine := &Engine{Store: f.durable, Configuration: f.manager, Now: func() time.Time { return f.now }, active: map[string]activeWork{item.ID: {item: item, cancel: cancel}}}
			if err := f.durable.DeferGitHub(context.Background(), "github.com", f.now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := engine.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if (ctx.Err() == nil) != allowed {
				t.Fatalf("review exclusion controlled separate feedback authority: allowed=%t cancelled=%t", allowed, ctx.Err() != nil)
			}
		})
	}
}

func TestFeedbackAdmissionInvokesOnlySeparateExecutor(t *testing.T) {
	f := newAdmissionFixture(t)
	item := feedbackAdmissionItem(t, f)
	if err := f.manager.Reload([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[feedback]\nauthors=['author']\naction='auto'\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selected := make(chan store.InboxItem, 1)
	engine := &Engine{Store: f.durable, Configuration: f.manager, Reviewer: func(*github.Client) review.Service {
		t.Error("modifying work entered private reviewer")
		return review.Service{}
	}, Feedback: func(_ context.Context, _ *config.Document, _ *github.Client, actual store.InboxItem) (store.Run, error) {
		selected <- actual
		return store.Run{}, nil
	}}
	engine.initialize()
	engine.client, engine.admitted = f.client, map[string]bool{}
	defer engine.Close()
	if err := engine.Admit(ctx); err != nil || engine.Active() != 1 {
		t.Fatalf("feedback not separately admitted: active=%d err=%v", engine.Active(), err)
	}
	if err := engine.Complete(ctx, true); err != nil {
		t.Fatal(err)
	}
	select {
	case actual := <-selected:
		if actual.ID != item.ID || actual.ExecutionKind != "feedback_v1" {
			t.Fatalf("wrong typed item: %+v", actual)
		}
	default:
		t.Fatal("separate executor not invoked")
	}
}
