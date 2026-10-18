package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type remoteFixture struct{}

func (remoteFixture) Identity(context.Context) (github.Identity, error) {
	return github.Identity{ID: 43, Login: "operator"}, nil
}
func (remoteFixture) Pull(_ context.Context, repo string, n int) (github.Pull, error) {
	return github.Pull{PullReference: github.PullReference{Repository: repo, Number: n, URL: "https://" + repo + "/pull/17"}, RepositoryID: 99, ID: 700, State: "open", HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40), Author: github.Identity{ID: 42, Login: "author"}}, nil
}
func (remoteFixture) PullPreferences(context.Context, github.Pull) (*config.RepositoryPreferences, error) {
	return nil, nil
}
func TestDispatchBothSkillsAndRecordFactualOutcome(t *testing.T) {
	for _, purpose := range []string{"review", "feedback"} {
		t.Run(purpose, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			doc, err := config.Parse([]byte(`schema_version=1
[reviewer]
skill="review"
model="review-model"
reasoning_effort="medium"
[feedback]
authors=["@me"]
action="auto"
model="feedback-model"
reasoning_effort="high"
`), "operator")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			service := Service{Store: s, GitHub: remoteFixture{}, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(ctx context.Context, r agent.Request) (agent.Output, error) {
				calls++
				wantSkill, wantModel, wantEffort := "review", "review-model", "medium"
				if purpose == "feedback" {
					wantSkill, wantModel, wantEffort = "handle-pr-feedback", "feedback-model", "high"
				}
				if r.Skill != wantSkill || r.URL != "https://github.com/owner/repo/pull/17" || r.Model != wantModel || r.Effort != wantEffort || r.Instructions != "" {
					t.Fatalf("configured skill launch changed: %+v", r)
				}
				if r.OnProgress == nil {
					t.Fatal("run/session observation missing")
				}
				if err := r.OnProgress(agent.Output{ThreadID: "fresh-session"}); err != nil {
					return agent.Output{}, err
				}
				return agent.Output{ThreadID: "fresh-session", Text: "Skill returned normally.", Completed: true}, nil
			}}
			run, err := service.Run(ctx, doc, Job{Target: github.PullReference{Repository: "github.com/owner/repo", Number: 17, URL: "https://github.com/owner/repo/pull/17"}, Purpose: purpose})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || run.State != store.AgentCompleted || run.Executor != store.NativeExecutor || run.ThreadID != "fresh-session" || run.Purpose != purpose || len(run.Prepared) != 0 || run.PayloadID != "" {
				t.Fatalf("wrong dispatch/outcome: calls=%d run=%+v", calls, run)
			}
			var outcome agent.Output
			if json.Unmarshal(run.Outcome, &outcome) != nil || outcome.Text != "Skill returned normally." {
				t.Fatalf("native outcome lost: %s", run.Outcome)
			}
			var policy config.Snapshot
			if json.Unmarshal(run.Configuration, &policy) != nil || policy.String(map[string]string{"review": "reviewer.model", "feedback": "feedback.model"}[purpose]) != map[string]string{"review": "review-model", "feedback": "feedback-model"}[purpose] {
				t.Fatal("stored configuration differs from launched request")
			}
		})
	}
}
func TestDispatchFailureIsNotDeliveryAndDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sentinel := errors.New("provider failed after some work")
	svc := Service{Store: s, GitHub: remoteFixture{}, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(context.Context, agent.Request) (agent.Output, error) {
		return agent.Output{ThreadID: "interrupted-session", Text: "Partial native response"}, sentinel
	}}
	run, err := svc.Run(ctx, nil, Job{Target: github.PullReference{Repository: "github.com/owner/repo", Number: 17, URL: "https://github.com/owner/repo/pull/17"}, Purpose: "feedback"})
	if !errors.Is(err, sentinel) || run.ID == "" || run.State != store.Failed || run.ThreadID != "interrupted-session" || run.Executor != store.NativeExecutor {
		t.Fatalf("lost factual failed outcome: %+v %v", run, err)
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("failed execution was retried: %+v %v", runs, err)
	}
}

type selectedAuthorRemote struct {
	remoteFixture
	author github.Identity
}

func (r selectedAuthorRemote) Pull(ctx context.Context, repo string, n int) (github.Pull, error) {
	p, err := r.remoteFixture.Pull(ctx, repo, n)
	p.Author = r.author
	return p, err
}

func TestDiscoveredSkillsRespectFreshPRAuthorPolicy(t *testing.T) {
	for _, purpose := range []string{"review", "feedback"} {
		t.Run(purpose, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			doc, err := config.Parse([]byte("schema_version=1\n[feedback]\nauthors=['@me']\naction='auto'\n"), "operator")
			if err != nil {
				t.Fatal(err)
			}
			item := store.InboxItem{Repository: "github.com/owner/repo", ActorID: 43, PullID: 700, Number: 17, Category: "reviews_owed", State: "queued", Action: "auto", Trigger: "review_requested", EventKey: "event"}
			author := github.Identity{ID: 43, Login: "renamed-operator"}
			if purpose == "feedback" {
				item.Category = "feedback_handling"
				item.ExecutionKind = "feedback_v1"
				item.Trigger = "review_submitted"
				author = github.Identity{ID: 42, Login: "other-author"}
			}
			before, err := s.DiscoveryScope(ctx, item.Repository)
			if err != nil {
				t.Fatal(err)
			}
			after := before
			after.ActorID = 43
			after.RepositoryID = 99
			after.State = "ok"
			after.Initialized = true
			if err := s.ApplyDiscovery(ctx, before, after, []store.Observation{{PullID: 700, Number: 17, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
				t.Fatal(err)
			}
			items, _, err := s.Inbox(ctx)
			if err != nil || len(items) != 1 {
				t.Fatal("missing item", err)
			}
			item = items[0]
			starts := 0
			svc := Service{Store: s, GitHub: selectedAuthorRemote{author: author}, LockDirectory: filepath.Join(t.TempDir(), "locks"), Launch: func(context.Context, agent.Request) (agent.Output, error) {
				starts++
				return agent.Output{ThreadID: "unexpected", Completed: true}, nil
			}}
			_, err = svc.Run(ctx, doc, Job{Target: github.PullReference{Repository: item.Repository, Number: 17}, Purpose: purpose, Item: &item})
			runs, readErr := s.List(ctx)
			if err == nil || !strings.Contains(err.Error(), "author") || starts != 0 || readErr != nil || len(runs) != 0 {
				t.Fatalf("fresh author policy bypassed: starts=%d runs=%d err=%v read=%v", starts, len(runs), err, readErr)
			}
		})
	}
}

func TestExecutionSerializesBothSkillLanesAcrossProfiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locks := filepath.Join(t.TempDir(), "locks")
	firstStarted, releaseFirst, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	first, err := store.Open(ctx, filepath.Join(t.TempDir(), "first"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := store.Open(ctx, filepath.Join(t.TempDir(), "second"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	firstService := Service{Store: first, GitHub: remoteFixture{}, LockDirectory: locks, Launch: func(ctx context.Context, _ agent.Request) (agent.Output, error) {
		close(firstStarted)
		select {
		case <-releaseFirst:
		case <-ctx.Done():
			return agent.Output{}, ctx.Err()
		}
		return agent.Output{ThreadID: "review-session", Completed: true}, nil
	}}
	secondService := Service{Store: second, GitHub: remoteFixture{}, LockDirectory: locks, Launch: func(context.Context, agent.Request) (agent.Output, error) {
		close(secondStarted)
		return agent.Output{ThreadID: "feedback-session", Completed: true}, nil
	}}
	target := github.PullReference{Repository: "github.com/owner/repo", Number: 17}
	go func() { _, err := firstService.Run(ctx, nil, Job{Target: target, Purpose: "review"}); firstDone <- err }()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("first lane never started")
	}
	go func() {
		_, err := secondService.Run(ctx, nil, Job{Target: target, Purpose: "feedback", Overrides: map[string]any{"feedback.backend": "claude"}})
		secondDone <- err
	}()
	select {
	case <-secondStarted:
		close(releaseFirst)
		t.Fatal("two profiles ran review/feedback on the same PR concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondStarted:
	case <-ctx.Done():
		t.Fatal("released PR was not admitted")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}
