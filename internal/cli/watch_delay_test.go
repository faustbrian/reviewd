package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/discovery"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestWatchPollingHonorsRateLimitAndBacksOffFailures(t *testing.T) {
	base := time.Minute
	limited := errors.Join(errors.New("repository acquisition incomplete"), &github.HTTPError{Status: 429, RetryAfter: time.Hour})
	if got := watchDelay(base, 2, limited); got < time.Hour {
		t.Fatalf("ignored server retry guidance: %v", got)
	}
	if got := watchDelay(base, 3, errors.New("offline")); got <= base || got > 15*time.Minute {
		t.Fatalf("unbounded or absent reconnect backoff: %v", got)
	}
	if got := watchDelay(base, 0, nil); got != base {
		t.Fatalf("healthy poll retained failure backoff: %v", got)
	}
}

func TestWatchPollingHonorsEveryRepositoryRetryDeadline(t *testing.T) {
	short := &github.HTTPError{Status: 429, RetryAfter: 30 * time.Second}
	long := &github.HTTPError{Status: 429, RetryAfter: time.Hour}
	for _, test := range []struct {
		name     string
		err      error
		failures int
		want     time.Duration
	}{
		{"short-first", errors.Join(short, long), 1, time.Hour},
		{"long-first", errors.Join(long, short), 1, time.Hour},
		{"zero-first", errors.Join(&github.HTTPError{Status: 500}, long), 1, time.Hour},
		{"nested", fmt.Errorf("sync: %w", errors.Join(short, fmt.Errorf("repository: %w", errors.Join(errors.New("offline"), long)))), 1, time.Hour},
		{"backoff-floor", errors.Join(short, errors.New("offline")), 3, 4 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := watchDelay(time.Minute, test.failures, test.err); got != test.want {
				t.Fatalf("combined repository deadlines: got %v, want %v", got, test.want)
			}
		})
	}
}

func TestWatchDiscoveryRateLimitPreservesServerRetryDeadline(t *testing.T) {
	// Discovery reads request events and closing requirements, not resolved
	// thread history. Both remaining GraphQL trigger reads must retain backoff.
	for _, endpoint := range []string{"reviews", "timelineItems", "closingIssuesReferences"} {
		t.Run(endpoint, func(t *testing.T) {
			limited := false
			_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any = []any{}
				switch r.URL.Path {
				case "/user":
					value = map[string]any{"id": 43, "login": "reviewer"}
				case "/repos/owner/repo":
					value = map[string]any{"id": 99, "full_name": "owner/repo"}
				case "/repos/owner/repo/pulls":
					value = []any{lifecyclePull(1, "open", true, false)}
				case "/repos/owner/repo/pulls/1/reviews":
					limited = endpoint == "reviews"
				case "/graphql":
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
					var request struct{ Query string }
					if err = json.Unmarshal(data, &request); err != nil {
						t.Error(err)
						return
					}
					limited = bytes.Contains([]byte(request.Query), []byte(endpoint))
					if !limited {
						value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
					}
				}
				if limited {
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				json.NewEncoder(w).Encode(value)
			}))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := config.Parse(data, path)
			if err != nil {
				t.Fatal(err)
			}
			defaults, err := config.ResolveDefaults(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			durable, err := store.Open(context.Background(), defaults.String("state.directory"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			err = (discovery.Service{Connect: deps.GitHub, Store: durable}).Sync(context.Background(), doc)
			var remote *github.HTTPError
			if !errors.As(err, &remote) || watchDelay(time.Minute, 1, err) < time.Hour {
				t.Fatalf("context retry guidance lost: delay=%v error=%v", watchDelay(time.Minute, 1, err), err)
			}
		})
	}
}
