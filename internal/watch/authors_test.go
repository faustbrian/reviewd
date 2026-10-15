package watch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
)

func TestLoadedAuthorExclusionCancelsActiveWorkBeforeCooldownReturns(t *testing.T) {
	for _, category := range []string{"reviews_owed", "own_pr_feedback"} {
		for _, held := range []bool{true, false} {
			for _, excluded := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/held=%t/excluded=%t", category, held, excluded), func(t *testing.T) {
					author := github.Identity{ID: 42, Login: "author"}
					if category == "own_pr_feedback" {
						author.ID = 43
					}
					f := newAdmissionFixture(t, author)
					settings := "schema_version=1\n[github]\nrepositories=['owner/repo']\n[actions]\nreview_submitted='auto'\nreview_comment_added='auto'\n[reviewer]\nexclude_authors="
					if err := f.manager.Reload([]byte(settings + "[]\n")); err != nil {
						t.Fatal(err)
					}
					f.add(t, 1, "review_submitted", f.now, category)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					started := make(chan struct{})
					stopped := make(chan struct{})
					workerDone := make(chan (<-chan struct{}), 1)
					var requests atomic.Int32
					peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						w.Header().Set("Retry-After", "3600")
						w.WriteHeader(http.StatusTooManyRequests)
					}))
					defer peer.Close()
					limited, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-author-revocation"})
					if err != nil {
						t.Fatal(err)
					}
					engine := &Engine{Store: f.durable, Configuration: f.manager, Now: func() time.Time { return f.now }, Connect: func(context.Context, string) (*github.Client, error) { return limited, nil }, Reviewer: func(c *github.Client) review.Service {
						return review.Service{Store: f.durable, GitHub: c, SkillPath: func(config.Snapshot) (string, error) { return "", errors.New("no model should start") }, Worker: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
							t.Error("unexpected model")
							return codex.ReviewOutput{}, nil
						}, Source: func(worker context.Context, _ github.Pull) (*source.Snapshot, error) {
							workerDone <- worker.Done()
							close(started)
							<-worker.Done()
							close(stopped)
							return nil, worker.Err()
						}}
					}}
					engine.initialize()
					engine.client = f.client
					engine.admitted = map[string]bool{}
					defer engine.Close()
					if err := engine.Admit(ctx); err != nil {
						t.Fatal(err)
					}
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("worker did not reach barrier")
					}
					if held {
						if err := f.durable.DeferGitHub(ctx, "github.com", f.now.Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
					filter := "other"
					if excluded {
						filter = "author"
						if category == "own_pr_feedback" {
							filter = "@me"
						}
					}
					if err := f.manager.Reload([]byte(settings + "['" + filter + "']\n")); err != nil {
						t.Fatal(err)
					}
					err = engine.Tick(ctx)
					if held && (err != nil || requests.Load() != 0) {
						t.Fatalf("durable cooldown contacted remote: %d %v", requests.Load(), err)
					}
					if !held && github.RetryAfter(err) < time.Hour {
						t.Fatalf("fresh throttle lost: %v", err)
					}
					if excluded {
						select {
						case <-(<-workerDone):
						default:
							t.Fatal("exclusion did not cancel execution context before Tick returned")
						}
						select {
						case <-stopped:
						case <-time.After(time.Second):
							t.Fatal("loaded exclusion did not cancel running work before cooldown return")
						}
					} else {
						select {
						case <-stopped:
							t.Fatal("allowed control was cancelled")
						default:
						}
					}
					items, _, err := f.durable.Inbox(ctx)
					if err != nil || len(items) != 1 || items[0].Category != category {
						t.Fatalf("cancellation erased monitored category: %+v %v", items, err)
					}
				})
			}
		}
	}
}
