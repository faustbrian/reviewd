package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFeedbackTargetRequiresWritableOpenUnqueuedExactForkHead(t *testing.T) {
	for _, scenario := range []string{"valid", "closed", "merged", "merge-queued", "no-write", "head-moved", "wrong-fork", "protected", "missing-queue-evidence"} {
		t.Run(scenario, func(t *testing.T) {
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo":
					fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
				case "/repos/fork/repo":
					push := scenario != "no-write"
					name := "fork/repo"
					if scenario == "wrong-fork" {
						name = "other/repo"
					}
					fmt.Fprintf(w, `{"id":101,"full_name":%q,"archived":false,"disabled":false,"default_branch":"main","permissions":{"push":%t}}`, name, push)
				case "/repos/owner/repo/pulls/17":
					state := "open"
					if scenario == "closed" {
						state = "closed"
					}
					fmt.Fprintf(w, `{"id":700,"number":17,"state":%q,"merged":false,"draft":false,"title":"Existing PR","body":"contract","updated_at":"2026-10-15T00:00:00Z","user":{"id":43,"login":"author"},"base":{"ref":"main","sha":%q,"repo":{"id":99,"full_name":"owner/repo"}},"head":{"ref":"feature/fix","sha":%q,"repo":{"id":101,"full_name":"fork/repo"}},"changed_files":1,"commits":1}`, state, base, head)
				case "/repos/fork/repo/branches/feature%2Ffix", "/repos/fork/repo/branches/feature/fix":
					fmt.Fprintf(w, `{"name":"feature/fix","protected":%t}`, scenario == "protected")
				case "/repos/fork/repo/rules/branches/feature%2Ffix", "/repos/fork/repo/rules/branches/feature/fix":
					fmt.Fprint(w, `[]`)
				case "/graphql":
					queue := `null`
					if scenario == "merge-queued" {
						queue = `{"id":"queue-entry"}`
					}
					returned := head
					if scenario == "head-moved" {
						returned = strings.Repeat("c", 40)
					}
					values := map[string]any{"databaseId": 700, "number": 17, "state": "OPEN", "merged": scenario == "merged", "headRefOid": returned, "baseRefOid": base, "headRepository": map[string]any{"databaseId": 101, "nameWithOwner": "fork/repo"}}
					if scenario != "missing-queue-evidence" {
						values["mergeQueueEntry"] = json.RawMessage(queue)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": values}}})
				default:
					t.Errorf("unexpected feedback target endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
			target, err := client.FeedbackTarget(context.Background(), "owner/repo", 17)
			if scenario != "valid" {
				if err == nil {
					t.Fatalf("unsafe feedback target accepted: %+v", target)
				}
				return
			}
			if err != nil || target.HeadRepository != "github.com/fork/repo" || target.HeadRepositoryID != 101 || target.Pull.HeadRef != "feature/fix" || target.Pull.Repository != "github.com/owner/repo" {
				t.Fatalf("fork push target confused with base/current checkout: %+v %v", target, err)
			}
		})
	}
}

func TestFeedbackMutationsUseOnlyOriginalPRAndDiscussionEndpoints(t *testing.T) {
	seen := map[string]bool{}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owned-fixture" {
			t.Error("missing owned authentication")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/17":
			if r.Method != "PATCH" || len(payload) != 2 || string(payload["title"]) != `"fix: preserve contract"` || string(payload["body"]) != `"Explain the resulting behavior."` {
				t.Error("metadata expanded beyond title/body")
			}
			seen["metadata"] = true
			fmt.Fprint(w, `{"number":17}`)
		case "/repos/owner/repo/issues/17/comments":
			if r.Method != "POST" || len(payload) != 1 {
				t.Error("discussion reply scope expanded")
			}
			seen["discussion"] = true
			fmt.Fprint(w, `{"id":21,"body":"answer","html_url":"https://github.com/owner/repo/pull/17#issuecomment-21","user":{"id":43,"login":"reviewer"}}`)
		case "/repos/owner/repo/pulls/17/requested_reviewers":
			if r.Method != "POST" || len(payload) != 2 {
				t.Error("review request scope expanded")
			}
			seen["requests"] = true
			fmt.Fprint(w, `{"number":17}`)
		case "/graphql":
			var query string
			_ = json.Unmarshal(payload["query"], &query)
			var variables map[string]any
			_ = json.Unmarshal(payload["variables"], &variables)
			if variables["thread"] != "THREAD1" {
				t.Error("original thread identity lost")
			}
			if strings.Contains(query, "ReviewdFeedbackThreadScope") {
				fmt.Fprint(w, `{"data":{"node":{"id":"THREAD1","pullRequest":{"databaseId":700,"number":17,"repository":{"databaseId":99,"nameWithOwner":"owner/repo"}}}}}`)
				return
			}
			if strings.Contains(query, "addPullRequestReviewThreadReply") {
				seen["thread-reply"] = true
				fmt.Fprint(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"COMMENT12","databaseId":12,"body":"answer","url":"https://github.com/owner/repo/pull/17#discussion_r12"}}}}`)
			} else if strings.Contains(query, "resolveReviewThread") {
				seen["resolution"] = true
				fmt.Fprint(w, `{"data":{"resolveReviewThread":{"thread":{"id":"THREAD1","isResolved":true}}}}`)
			} else {
				t.Error("arbitrary mutation capability")
				http.Error(w, "denied", 403)
			}
		default:
			t.Errorf("unexpected mutation endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
	pull := Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, RepositoryID: 99, ID: 700, HeadSHA: strings.Repeat("b", 40)}
	ctx := context.Background()
	if err := client.UpdateFeedbackMetadata(ctx, pull, "fix: preserve contract", "Explain the resulting behavior."); err != nil {
		t.Fatal(err)
	}
	if reply, err := client.ReplyToFeedbackDiscussion(ctx, pull, "answer"); err != nil || reply.ID != 21 {
		t.Fatalf("discussion reply: %+v %v", reply, err)
	}
	if reply, err := client.ReplyToFeedbackThread(ctx, pull, "THREAD1", "answer"); err != nil || reply.ID != 12 {
		t.Fatalf("original thread reply: %+v %v", reply, err)
	}
	if err := client.ResolveFeedbackThread(ctx, pull, "THREAD1"); err != nil {
		t.Fatal(err)
	}
	if err := client.RequestFeedbackReviewer(ctx, pull, "human", ""); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 5 {
		t.Fatalf("incomplete effect families: %v", seen)
	}
}

func TestFeedbackGraphQLThrottleRetainsServerDeadline(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(request.Query, "ReviewdFeedbackThreadScope") {
			fmt.Fprint(w, `{"data":{"node":{"id":"THREAD1","pullRequest":{"databaseId":700,"number":17,"repository":{"databaseId":99,"nameWithOwner":"owner/repo"}}}}}`)
			return
		}
		w.Header().Set("Retry-After", "75")
		fmt.Fprint(w, `{"errors":[{"type":"RATE_LIMITED","message":"wait"}],"data":null}`)
	}))
	defer peer.Close()
	client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
	pull := Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, RepositoryID: 99, ID: 700}
	err := client.ResolveFeedbackThread(context.Background(), pull, "THREAD1")
	if RetryAfter(err) < 75*time.Second {
		t.Fatalf("GraphQL write lost acquired server constraint: %v", err)
	}
}

func TestFeedbackMutationNeverFollowsRedirect(t *testing.T) {
	var calls atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }))
	defer outside.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, outside.URL, http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
	pull := Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, RepositoryID: 99, ID: 700}
	if _, err := client.ReplyToFeedbackDiscussion(context.Background(), pull, "scoped reply"); err == nil || calls.Load() != 0 {
		t.Fatal("feedback mutation followed a remote-selected redirect")
	}
}

func TestFeedbackThreadMutationRejectsForeignPRBeforeWriting(t *testing.T) {
	for _, kind := range []string{"reply", "resolve"} {
		t.Run(kind, func(t *testing.T) {
			var mutations atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Query string }
				_ = json.NewDecoder(r.Body).Decode(&request)
				if strings.Contains(request.Query, "mutation") {
					mutations.Add(1)
					fmt.Fprint(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"OTHER","databaseId":12,"body":"answer","url":"https://github.com/owner/repo/pull/18#discussion_r12"}},"resolveReviewThread":{"thread":{"id":"FOREIGN","isResolved":true}}}}`)
					return
				}
				fmt.Fprint(w, `{"data":{"node":{"id":"FOREIGN","pullRequest":{"databaseId":701,"number":18,"repository":{"databaseId":99,"nameWithOwner":"owner/repo"}}}}}`)
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
			pull := Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, RepositoryID: 99, ID: 700}
			var err error
			if kind == "reply" {
				_, err = client.ReplyToFeedbackThread(context.Background(), pull, "FOREIGN", "answer")
			} else {
				err = client.ResolveFeedbackThread(context.Background(), pull, "FOREIGN")
			}
			if err == nil || mutations.Load() != 0 {
				t.Fatalf("PR17 mutated a PR18 thread: writes=%d err=%v", mutations.Load(), err)
			}
		})
	}
}
