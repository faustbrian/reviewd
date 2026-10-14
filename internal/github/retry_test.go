package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// DC-04: deadline extraction is shared by read and publication responses.
func TestRemoteRetryGuidanceSurvivesReadsAndPublication(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, scenario := range []string{"seconds", "http-date", "reset", "fallback", "secondary-message", "access"} {
			t.Run(method+"/"+scenario, func(t *testing.T) {
				deadline := time.Now().Add(time.Hour).Truncate(time.Second)
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					status := http.StatusTooManyRequests
					switch scenario {
					case "seconds":
						w.Header().Set("Retry-After", "3600")
					case "http-date":
						w.Header().Set("Retry-After", deadline.UTC().Format(http.TimeFormat))
					case "reset":
						status = http.StatusForbidden
						w.Header().Set("X-RateLimit-Remaining", "0")
						w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(deadline.Unix(), 10))
					case "access":
						status = http.StatusForbidden
					case "secondary-message":
						w.WriteHeader(http.StatusForbidden)
						w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
						return
					}
					w.WriteHeader(status)
				}))
				defer peer.Close()
				client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-retry-fixture"})
				if err != nil {
					t.Fatal(err)
				}
				if method == http.MethodGet {
					_, err = client.Identity(context.Background())
				} else {
					err = client.postPublication(context.Background(), "owned-submission", struct{}{}, &struct{}{})
				}
				var remote *HTTPError
				if !errors.As(err, &remote) {
					t.Fatalf("typed remote error lost: %v", err)
				}
				delay := RetryAfter(err)
				switch scenario {
				case "access":
					if delay != 0 {
						t.Fatal("permission failure became a rate limit")
					}
				case "fallback", "secondary-message":
					if delay < time.Minute {
						t.Fatalf("secondary-limit fallback too short: %v", delay)
					}
				default:
					if delay < 59*time.Minute || delay > time.Hour {
						t.Fatalf("server deadline lost: %v", delay)
					}
				}
			})
		}
	}
}

func TestGraphQLApplicationRateLimitsPreserveTypedGuidance(t *testing.T) {
	for _, scenario := range []string{"primary", "secondary-retry", "secondary-fallback", "secondary-message", "secondary-403", "permission", "permission-403", "validation"} {
		t.Run(scenario, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "primary":
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				case "secondary-retry":
					w.Header().Set("Retry-After", "3600")
				case "secondary-403":
					w.WriteHeader(http.StatusForbidden)
				case "permission-403":
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(`{"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`))
					return
				case "permission":
					w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible"}]}`))
					return
				case "validation":
					w.Write([]byte(`{"errors":[{"type":"GRAPHQL_VALIDATION_FAILED","message":"Field does not exist"}]}`))
					return
				case "secondary-message":
					w.Write([]byte(`{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit."}]}`))
					return
				}
				w.Write([]byte(`{"data":null,"errors":[{"type":"RATE_LIMITED","message":"You have exceeded a secondary rate limit."}]}`))
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-graphql-cooldown"})
			if err != nil {
				t.Fatal(err)
			}
			err = client.query(context.Background(), "query OwnedRead { viewer { login } }", nil, &struct{}{})
			var remote *HTTPError
			if scenario == "permission" || scenario == "validation" || scenario == "permission-403" {
				if err == nil || RetryAfter(err) != 0 || (scenario != "permission-403" && errors.As(err, &remote)) {
					t.Fatal("GraphQL permission failure became a throttle or success")
				}
				return
			}
			minimum := 59 * time.Minute
			if scenario == "secondary-fallback" || scenario == "secondary-message" || scenario == "secondary-403" {
				minimum = time.Minute
			}
			if !errors.As(err, &remote) || RetryAfter(err) < minimum {
				t.Fatalf("GraphQL application throttle lost guidance: %v", err)
			}
		})
	}
}
