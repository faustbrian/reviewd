package watch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
)

// DC-03: a worker's observed throttle must hold the next poll even before
// the foreground loop has projected that worker's queued completion.
func TestCompletedWorkerPublishesCooldownBeforeForegroundProjection(t *testing.T) {
	f := newAdmissionFixture(t)
	f.add(t, 1, "review_requested", f.now)
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer peer.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-completion-cooldown"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var clock atomic.Int64
	clock.Store(f.now.UnixNano())
	engine := &Engine{Store: f.durable, Configuration: f.manager, Now: func() time.Time { return time.Unix(0, clock.Load()) }, Connect: func(context.Context, string) (*github.Client, error) { return client, nil }, Reviewer: func(c *github.Client) review.Service {
		return review.Service{Store: f.durable, GitHub: c, SkillPath: func(config.Snapshot) (string, error) { return "", errors.New("no model should start") }, Worker: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
			t.Error("rate-limited acquisition started a model")
			return codex.ReviewOutput{}, errors.New("unexpected model")
		}}
	}}
	engine.initialize()
	engine.client = client
	engine.admitted = map[string]bool{}
	defer engine.Close()
	if err := engine.Admit(ctx); err != nil || engine.Active() != 1 {
		t.Fatalf("eligible request did not dispatch: %v", err)
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(engine.done) == 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("worker did not return its throttled acquisition")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("fixture did not acquire exactly one limiting response")
	}
	if err := engine.Tick(ctx); err != nil || requests.Load() != 1 {
		t.Fatalf("queued completion left a discovery bypass: requests=%d error=%v", requests.Load(), err)
	}
	deadline := f.now.Add(time.Hour)
	clock.Store(f.now.Add(time.Minute).UnixNano())
	if err := engine.Complete(ctx, true); github.RetryAfter(err) < time.Hour {
		t.Fatalf("worker failure guidance lost: %v", err)
	}
	if got, err := f.durable.GitHubRetryAt(ctx, "github.com"); err != nil || !got.Equal(deadline) {
		t.Fatalf("foreground projection moved the already observed deadline: %v %v", got, err)
	}
}
