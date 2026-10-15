package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

type feedbackExecutorFactory interface {
	feedbackExecutor(Dependencies, *store.Store, string) func(context.Context, *config.Document, *github.Client, store.InboxItem) (store.Run, error)
}

func TestFeedbackExecutorUsesTheTypedDecisionAndRealService(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint("manual=", manual), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			ctx := context.Background()
			path, durable, item := feedbackDecisionFixture(t)
			var data []byte
			data, _ = os.ReadFile(path)
			data = []byte(strings.ReplaceAll(string(data), "action='ask'", "action='auto'"))
			data = append(data, []byte("container_endpoint='unix:///owned/fixture.sock'\ncontainer_image='sha256:"+strings.Repeat("a", 64)+"'\n")...)
			os.WriteFile(path, data, 0600)
			document, err := config.Parse(data, path)
			if err != nil {
				t.Fatal(err)
			}
			head, base := item.HeadSHA, item.TargetBaseSHA
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/user":
					fmt.Fprint(w, `{"id":43,"login":"author"}`)
				case "/repos/owner/repo":
					fmt.Fprint(w, `{"id":99,"full_name":"owner/repo","archived":false,"disabled":false,"default_branch":"main","permissions":{"push":true}}`)
				case "/repos/owner/repo/pulls/17":
					fmt.Fprintf(w, `{"id":701,"number":17,"state":"open","merged":false,"draft":false,"title":"Existing PR","body":"contract","updated_at":"2026-10-15T00:00:00Z","user":{"id":43,"login":"author"},"base":{"ref":"main","sha":%q,"repo":{"id":99,"full_name":"owner/repo"}},"head":{"ref":"feature/existing","sha":%q,"repo":{"id":99,"full_name":"owner/repo"}},"changed_files":1,"commits":1}`, base, head)
				case "/repos/owner/repo/branches/feature/existing":
					fmt.Fprint(w, `{"name":"feature/existing","protected":false}`)
				case "/repos/owner/repo/rules/branches/feature/existing":
					fmt.Fprint(w, `[]`)
				case "/repos/owner/repo/pulls/17/reviews", "/repos/owner/repo/pulls/17/comments", "/repos/owner/repo/issues/17/comments", "/repos/owner/repo/issues/17/timeline":
					fmt.Fprint(w, `[]`)
				case "/graphql":
					var request struct{ Query string }
					json.NewDecoder(r.Body).Decode(&request)
					row := map[string]any{"databaseId": 701, "number": 17}
					switch {
					case strings.Contains(request.Query, "ReviewdFeedbackTarget"):
						row["state"] = "OPEN"
						row["merged"] = false
						row["mergeQueueEntry"] = nil
						row["headRefOid"] = head
						row["baseRefOid"] = base
						row["headRepository"] = map[string]any{"databaseId": 99, "nameWithOwner": "owner/repo"}
					case strings.Contains(request.Query, "ReviewdThreads"):
						row["reviewThreads"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
					case strings.Contains(request.Query, "ReviewdRequirements"):
						row["closingIssuesReferences"] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
					default:
						t.Error("unexpected GraphQL query")
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": row}}})
				default:
					t.Errorf("unexpected feedback endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := feedback.Collect(github.ReviewContext{Reviews: []json.RawMessage{}, Comments: []json.RawMessage{}, Conversation: []json.RawMessage{}, Threads: []github.ReviewThread{}, Gaps: []string{}}, github.Identity{ID: 43, Login: "author"})
			if err != nil {
				t.Fatal(err)
			}
			scope, err := durable.DiscoveryScope(ctx, item.Repository)
			if err != nil {
				t.Fatal(err)
			}
			item.ContextKey = inventory.Digest
			item.Action = "auto"
			item.State = "queued"
			item.ID = ""
			if err := durable.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: head, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
				t.Fatal(err)
			}
			items, _, err := durable.Inbox(ctx)
			if err != nil || len(items) != 1 {
				t.Fatal(err)
			}
			item = items[0]
			if manual {
				_, err = durable.DecideFeedbackItem(ctx, item.ID, map[string]any{"feedback.model": "selected-manual-model"}, store.FeedbackDecision{EventKey: item.EventKey, HeadSHA: head, TargetBaseSHA: base, ContextKey: inventory.Digest})
				if err != nil {
					t.Fatal(err)
				}
				items, _, err = durable.Inbox(ctx)
				if err != nil {
					t.Fatal(err)
				}
				item = items[0]
			}
			factory, ok := any(&execution{}).(feedbackExecutorFactory)
			if !ok {
				t.Fatal("production typed feedback executor is missing")
			}
			runner := factory.feedbackExecutor(Dependencies{Review: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
				t.Fatal("typed feedback entered private reviewer")
				return codex.ReviewOutput{}, nil
			}}, durable, durable.Directory())
			run, err := runner(ctx, document, client, item)
			if err != nil || run.Purpose != "feedback" || run.State != store.FeedbackDelivered || !run.OwnerClosed {
				t.Fatalf("typed service dispatch: %+v %v", run, err)
			}
		})
	}
}
