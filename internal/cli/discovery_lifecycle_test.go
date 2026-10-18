package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/notify"
)

type lifecycleItem struct {
	ID, Category, State, Trigger, Action, Explanation string
	EventKey                                          string `json:"event_key"`
	ExecutionKind                                     string `json:"execution_kind"`
	HeadSHA                                           string `json:"head_sha"`
	ContextKey                                        string `json:"context_key"`
	Number                                            int
	Baseline                                          bool
}
type lifecycleInbox struct {
	Items     []lifecycleItem
	Runs      []json.RawMessage
	Discovery []struct{ State string }
}

// The owned peer serves complete REST/GraphQL reads to the real CLI and store.
func lifecycleCLI(t *testing.T, action string, peer http.Handler) func(string, ...string) lifecycleInbox {
	invoke, _ := lifecycleCLIConfig(t, action, peer)
	return invoke
}

func lifecycleCLIConfig(t *testing.T, action string, peer http.Handler) (func(string, ...string) lifecycleInbox, string) {
	invoke, path, _ := lifecycleCLIConfigDeps(t, action, peer)
	return invoke, path
}

func lifecycleCLIConfigDeps(t *testing.T, action string, peer http.Handler, allowReviewWrites ...bool) (func(string, ...string) lifecycleInbox, string, Dependencies) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	configuration := "schema_version = 1\n[github]\nrepositories = [\"owner/repo\"]\n[actions]\nreview_requested = \"auto\"\nhead_updated = \"auto\"\nnew_pull_request = " + strconv.Quote(action) + "\n[state]\ndirectory = " + strconv.Quote(filepath.Join(root, "state")) + "\n"
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reviewWrite := len(allowReviewWrites) == 1 && allowReviewWrites[0] && r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/pulls/1/reviews"
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == "/graphql") && !reviewWrite {
			t.Errorf("discovery attempted a write: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		peer.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := github.New(github.Options{Host: "github.com", BaseURL: server.URL, Token: "owned-lifecycle-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{Desktop: func(context.Context, notify.Message) error { return nil }, GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, LaunchAgent: func(context.Context, agent.Request) (agent.Output, error) {
		t.Fatal("discovery started a reviewer")
		return agent.Output{}, nil
	}}
	return func(command string, extra ...string) lifecycleInbox {
		t.Helper()
		var out, diagnostics bytes.Buffer
		args := append([]string{"--config", path, command}, extra...)
		args = append(args, "--json")
		if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
			t.Fatalf("%s: %d %s", command, code, diagnostics.String())
		}
		var result lifecycleInbox
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Runs) != 0 {
			t.Fatal("discovery started review work")
		}
		return result
	}, path, deps
}

func lifecyclePull(number int, state string, userRequest, teamRequest bool) map[string]any {
	users, teams := []any{}, []any{}
	if userRequest {
		users = append(users, map[string]any{"id": 43, "login": "reviewer"})
	}
	if teamRequest {
		teams = append(teams, map[string]any{"id": 12, "node_id": "TEAM12", "slug": "reviewers"})
	}
	return map[string]any{"id": 700 + number, "number": number, "state": state, "draft": false, "title": fmt.Sprintf("Pull %d", number), "body": "Required behavior.", "updated_at": "2026-10-13T20:00:00Z", "user": map[string]any{"id": 8, "login": "author"}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main", "repo": map[string]any{"id": 99, "full_name": "owner/repo"}}, "head": map[string]any{"sha": strings.Repeat("a", 40), "ref": "feature"}, "changed_files": 1, "commits": 1, "requested_reviewers": users, "requested_teams": teams}
}

func lifecycleGraphQL(t *testing.T, r *http.Request, events func(int) []any) any {
	t.Helper()
	var input struct {
		Query     string
		Variables struct{ Number int }
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		t.Fatal(err)
	}
	page := map[string]any{"hasNextPage": false, "endCursor": nil}
	if strings.Contains(input.Query, "ReviewdTeamMembership") {
		return map[string]any{"data": map[string]any{"node": map[string]any{"id": "TEAM12", "databaseId": 12, "members": map[string]any{"nodes": []any{map[string]any{"databaseId": 43, "login": "reviewer"}}, "pageInfo": page}}}}
	}
	connection := map[string]any{"nodes": []any{}, "pageInfo": page}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"databaseId": 700 + input.Variables.Number, "number": input.Variables.Number, "timelineItems": map[string]any{"nodes": events(input.Variables.Number), "pageInfo": page}, "reviewThreads": connection, "closingIssuesReferences": connection}}}}
}

func TestOptionalDiscoveryRetiresClosedWork(t *testing.T) {
	for _, action := range []string{"notify", "auto"} {
		t.Run(action, func(t *testing.T) {
			var listed, closed atomic.Bool
			var reads atomic.Int32
			invoke := lifecycleCLI(t, action, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any = []any{}
				switch r.URL.Path {
				case "/user":
					value = map[string]any{"id": 43, "login": "reviewer"}
				case "/repos/owner/repo":
					value = map[string]any{"id": 99, "full_name": "owner/repo"}
				case "/repos/owner/repo/pulls":
					if listed.Load() {
						value = []any{lifecyclePull(1, "open", false, false)}
					}
				case "/repos/owner/repo/pulls/1":
					reads.Add(1)
					state := "open"
					if closed.Load() {
						state = "closed"
					}
					value = lifecyclePull(1, state, false, false)
				case "/graphql":
					value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			invoke("sync")
			listed.Store(true)
			opened := invoke("sync")
			want := "attention"
			if action == "auto" {
				want = "queued"
			}
			if len(opened.Items) != 1 || opened.Items[0].State != want || opened.Items[0].Baseline {
				t.Fatalf("new optional work not imported: %+v", opened)
			}
			listed.Store(false)
			closed.Store(true)
			invoke("sync")
			ended := invoke("inbox")
			if len(ended.Items) != 0 || len(ended.Discovery) != 1 || ended.Discovery[0].State != "ok" || reads.Load() != 1 {
				t.Fatalf("closed optional work survived: %+v missing reads=%d", ended, reads.Load())
			}
			invoke("sync")
			if reads.Load() != 1 {
				t.Fatalf("verified closed work was refetched: %d", reads.Load())
			}
		})
	}
}

func TestUnrelatedReviewerActivityCannotReviveUnfollowedWork(t *testing.T) {
	var otherPR, otherUser, freshRequest, newRequirement atomic.Bool
	event := func(id, kind string, reviewer int) any {
		return map[string]any{"id": id, "createdAt": "2026-10-13T20:00:00Z", "requestedReviewer": map[string]any{"__typename": kind, "databaseId": reviewer}}
	}
	invoke := lifecycleCLI(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			rows := []any{lifecyclePull(1, "open", true, false)}
			if otherPR.Load() {
				rows = append(rows, lifecyclePull(2, "open", false, true))
			}
			value = rows
		case "/repos/owner/repo/issues/1/comments":
			if newRequirement.Load() {
				value = []any{map[string]any{"id": 95, "user": map[string]any{"id": 8, "login": "author"}, "body": "New caller requirement: preserve idempotency."}}
			}
		case "/repos/owner/repo/issues/1/timeline":
			rows := []any{map[string]any{"id": 91, "event": "review_requested", "requested_reviewer": map[string]any{"id": 43, "login": "reviewer"}}, map[string]any{"id": 92, "event": "review_request_removed", "requested_team": map[string]any{"id": 12, "slug": "reviewers"}}}
			if otherUser.Load() {
				rows = append(rows, map[string]any{"id": 93, "event": "review_requested", "requested_reviewer": map[string]any{"id": 49, "login": "other"}})
			}
			if freshRequest.Load() {
				rows = append(rows, map[string]any{"id": 94, "event": "review_requested", "requested_reviewer": map[string]any{"id": 43, "login": "reviewer"}})
			}
			value = rows
		case "/graphql":
			value = lifecycleGraphQL(t, r, func(number int) []any {
				if number != 1 {
					return []any{event("B_TEAM", "Team", 12)}
				}
				rows := []any{event("OLD_USER", "User", 43), event("OLD_TEAM", "Team", 12)}
				if otherUser.Load() {
					rows = append(rows, event("OTHER_USER", "User", 49))
				}
				if freshRequest.Load() {
					rows = append(rows, event("FRESH_USER", "User", 43))
				}
				return rows
			})
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	initial := invoke("sync")
	if len(initial.Items) != 1 || initial.Items[0].State != "pending_decision" {
		t.Fatalf("initial work not held: %+v", initial)
	}
	id := initial.Items[0].ID
	otherUser.Store(true)
	unchanged := invoke("sync")
	if len(unchanged.Items) != 1 || unchanged.Items[0].ID != id || unchanged.Items[0].State != "pending_decision" || !unchanged.Items[0].Baseline {
		t.Fatalf("another reviewer bypassed held backlog: %+v", unchanged)
	}
	invoke("unfollow", id)
	if stopped := invoke("sync"); len(stopped.Items) != 0 {
		t.Fatalf("unchanged unfollowed work revived: %+v", stopped)
	}
	otherPR.Store(true)
	changed := invoke("sync")
	for _, item := range changed.Items {
		if item.Number == 1 {
			t.Fatalf("unrelated team request revived stopped PR: %+v", item)
		}
	}
	freshRequest.Store(true)
	requested := invoke("sync")
	found := false
	for _, item := range requested.Items {
		if item.Number == 1 {
			found = true
			if item.ID != id || item.State != "queued" || item.Trigger != "review_requested" {
				t.Fatalf("genuine same-head request not restored: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("genuine new request was suppressed")
	}
	newRequirement.Store(true)
	requirements := invoke("sync")
	found = false
	for _, item := range requirements.Items {
		if item.Number == 1 {
			found = true
			if item.Trigger != "context_changed" || item.State != "queued" {
				t.Fatalf("genuine controlling context was ignored: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("requirement change disappeared")
	}
}
