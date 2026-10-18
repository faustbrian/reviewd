package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	watcher "github.com/faustbrian/reviewd/internal/watch"
)

// A new connector client on every tick is intentional: this is the production
// FromCLI lifetime, unlike fixtures which accidentally retain one client.
type pollingFixture struct {
	t                                                  *testing.T
	engine                                             *watcher.Engine
	durable                                            *store.Store
	requested, ignored                                 bool
	issueTitle, issueBody, issueURL, reviewBody        string
	requests                                           []string
	fullBodies, threads, connections, historyFromStart int
}

func newPollingFixture(t *testing.T) *pollingFixture {
	t.Helper()
	f := &pollingFixture{t: t, requested: true, issueTitle: "Requirement", issueBody: "First requirement", issueURL: "https://github.com/owner/repo/issues/9", reviewBody: "Human feedback", requests: []string{"request-one"}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	manager, err := config.NewManager([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[actions]\nreview_requested='notify'\nhead_updated='notify'\n[feedback]\nauthors=['author']\naction='notify'\n[notifications]\ndesktop=false\nterminal=false\n"), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	f.durable, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.durable.Close() })
	f.engine = &watcher.Engine{Store: f.durable, Configuration: manager, Connect: func(context.Context, string) (*github.Client, error) {
		f.connections++
		return github.New(github.Options{Host: "github.com", BaseURL: server.URL, Token: "owned-polling-fixture"})
	}}
	t.Cleanup(func() { f.engine.Close() })
	return f
}

func (f *pollingFixture) serve(w http.ResponseWriter, r *http.Request) {
	var value any = []any{}
	switch r.URL.Path {
	case "/user":
		value = map[string]any{"id": 43, "login": "reviewer"}
	case "/repos/owner/repo":
		value = map[string]any{"id": 99, "full_name": "owner/repo"}
	case "/repos/owner/repo/pulls":
		pulls := []any{lifecyclePull(1, "open", f.requested, false)}
		if f.ignored {
			pull := lifecyclePull(2, "open", true, false)
			pull["user"] = map[string]any{"id": 43, "login": "reviewer"}
			pulls = append(pulls, pull)
		}
		value = pulls
	case "/repos/owner/repo/pulls/1/reviews":
		value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 88, "login": "human"}, "state": "COMMENTED", "body": f.reviewBody, "submitted_at": "2026-10-18T00:00:00Z"}}
	case "/repos/owner/repo/pulls/1/comments", "/repos/owner/repo/issues/1/comments", "/repos/owner/repo/issues/1/timeline":
	case "/graphql":
		var input struct {
			Query     string
			Variables struct {
				Number int
				After  *string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			f.t.Error(err)
			w.WriteHeader(500)
			return
		}
		if input.Variables.Number != 1 {
			f.t.Errorf("irrelevant PR history/context was acquired: %d", input.Variables.Number)
			w.WriteHeader(503)
			return
		}
		page := map[string]any{"hasNextPage": false, "endCursor": nil}
		events := []any{}
		if strings.Contains(input.Query, "ReviewdRequestEvents") {
			start := 0
			if input.Variables.After == nil {
				f.historyFromStart++
			} else {
				for i := range f.requests {
					if *input.Variables.After == fmt.Sprintf("cursor-%d", i+1) {
						start = i + 1
					}
				}
			}
			for i := start; i < len(f.requests); i++ {
				events = append(events, map[string]any{"id": f.requests[i], "createdAt": "2026-10-18T00:00:00Z", "requestedReviewer": map[string]any{"__typename": "User", "databaseId": 43}})
			}
			if len(events) > 0 {
				page["endCursor"] = fmt.Sprintf("cursor-%d", len(f.requests))
			}
		}
		if strings.Contains(input.Query, "ReviewdThreads") {
			f.threads++
		}
		requirement := map[string]any{"id": "ISSUE9", "databaseId": 900, "number": 9, "title": f.issueTitle, "body": f.issueBody, "url": f.issueURL, "updatedAt": "2026-10-18T00:00:00Z", "repository": map[string]any{"nameWithOwner": "owner/repo"}}
		value = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"databaseId": 701, "number": 1, "timelineItems": map[string]any{"nodes": events, "pageInfo": page}, "reviewThreads": map[string]any{"nodes": []any{}, "pageInfo": page}, "closingIssuesReferences": map[string]any{"nodes": []any{requirement}, "pageInfo": page}}}}}
	default:
		f.t.Errorf("unexpected acquisition: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Error(err)
		return
	}
	if r.Method == http.MethodGet {
		etag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reviews") || strings.HasSuffix(r.URL.Path, "/comments") || strings.HasSuffix(r.URL.Path, "/timeline") {
			f.fullBodies++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (f *pollingFixture) tick() map[string]store.InboxItem {
	f.t.Helper()
	if err := f.engine.Tick(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	items, _, err := f.durable.Inbox(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	result := map[string]store.InboxItem{}
	for _, item := range items {
		result[item.Category] = item
	}
	return result
}

func TestLinkedRequirementChangesOnlyRenewReviewWork(t *testing.T) {
	for _, followed := range []bool{false, true} {
		t.Run(fmt.Sprint(followed), func(t *testing.T) {
			f := newPollingFixture(t)
			previous := f.tick()
			if followed {
				if err := f.durable.SetFollow(context.Background(), previous["reviews_owed"].ID, true); err != nil {
					t.Fatal(err)
				}
				f.requested = false
				previous = f.tick()
			}
			feedbackKey := previous["feedback_handling"].EventKey
			seen := map[string]bool{previous["reviews_owed"].EventKey: true}
			for _, change := range []func(){func() { f.issueTitle = "Changed title" }, func() { f.issueBody = "Changed body" }, func() { f.issueBody = "First requirement" }} {
				change()
				next := f.tick()
				if seen[next["reviews_owed"].EventKey] {
					t.Fatal("returning requirements reused an earlier work identity")
				}
				seen[next["reviews_owed"].EventKey] = true
				if next["reviews_owed"].EventKey == previous["reviews_owed"].EventKey || next["reviews_owed"].Trigger != "context_changed" {
					t.Fatalf("linked-issue edit did not renew review: before=%+v after=%+v", previous, next)
				}
				if next["feedback_handling"].EventKey != feedbackKey {
					t.Fatal("linked issue edit became incoming feedback")
				}
				stable := f.tick()
				if stable["reviews_owed"].EventKey != next["reviews_owed"].EventKey {
					t.Fatal("unchanged requirements retriggered")
				}
				previous = next
			}
			f.issueURL += "?view=metadata"
			next := f.tick()
			if next["reviews_owed"].EventKey != previous["reviews_owed"].EventKey {
				t.Fatal("URL-only metadata renewed review")
			}
		})
	}
}

func TestProductionPollingRetainsValidatedBodiesAndRequestCursor(t *testing.T) {
	f := newPollingFixture(t)
	first := f.tick()
	bodies := f.fullBodies
	second := f.tick()
	if f.connections != 2 || f.fullBodies != bodies {
		t.Fatalf("fresh connector discarded validated context: connections=%d bodies=%d->%d", f.connections, bodies, f.fullBodies)
	}
	if f.threads != 0 || f.historyFromStart != 1 {
		t.Fatalf("unchanged full histories reacquired: threads=%d request scans=%d", f.threads, f.historyFromStart)
	}
	if second["reviews_owed"].EventKey != first["reviews_owed"].EventKey {
		t.Fatal("unchanged poll renewed review")
	}
	f.requests = append(f.requests, "request-two")
	requested := f.tick()
	if requested["reviews_owed"].EventKey == second["reviews_owed"].EventKey || requested["reviews_owed"].Trigger != "review_requested" {
		t.Fatal("same-head rerequest missed")
	}
	f.reviewBody = "Edited human feedback"
	feedback := f.tick()
	if feedback["feedback_handling"].EventKey == requested["feedback_handling"].EventKey {
		t.Fatal("feedback edit hidden by unchanged PR metadata")
	}
}

func TestExcludedPullCannotBreakRepositoryAcquisition(t *testing.T) {
	f := newPollingFixture(t)
	f.ignored = true
	items := f.tick()
	if len(items) != 2 {
		t.Fatalf("excluded author's PR leaked into review/feedback: %+v", items)
	}
}

func TestNewlyRelevantRequestIsFreshButPolicyToggleIsBaseline(t *testing.T) {
	f := newPollingFixture(t)
	f.requested = false
	f.tick()
	f.requested = true
	f.requests = append(f.requests, "new-request")
	newWork := f.tick()["reviews_owed"]
	if newWork.EventKey == "" || newWork.Baseline {
		t.Fatal("real request added to a previously irrelevant PR became backlog")
	}
	data := []byte("schema_version=1\n[github]\nrepositories=['owner/repo']\nwatch_review_requests=false\n[actions]\nreview_requested='notify'\nhead_updated='notify'\n[feedback]\nauthors=['author']\naction='notify'\n")
	if err := f.engine.Configuration.Reload(data); err != nil {
		t.Fatal(err)
	}
	f.tick()
	data = []byte(strings.Replace(string(data), "watch_review_requests=false", "watch_review_requests=true", 1))
	if err := f.engine.Configuration.Reload(data); err != nil {
		t.Fatal(err)
	}
	resumed := f.tick()["reviews_owed"]
	if !resumed.Baseline || resumed.EventKey != newWork.EventKey {
		t.Fatalf("policy change manufactured a fresh request: %+v", resumed)
	}
}

func TestOlderObservationsKeepConsumedReviewIdentityOnAdoption(t *testing.T) {
	f := newPollingFixture(t)
	before := f.tick()
	ctx := context.Background()
	items, scopes, err := f.durable.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := f.durable.Observations(ctx, "github.com/owner/repo", 43)
	if err != nil {
		t.Fatal(err)
	}
	rows := []store.Observation{}
	for _, row := range observations {
		var recorded map[string]json.RawMessage
		if err := json.Unmarshal(row.Record, &recorded); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"requirements_revision", "request_cursor", "requests_tracked"} {
			delete(recorded, field)
		}
		row.Record, _ = json.Marshal(recorded)
		rows = append(rows, row)
	}
	if len(scopes) != 1 {
		t.Fatal("fixture scope missing")
	}
	if err := f.durable.ApplyDiscovery(ctx, scopes[0], scopes[0], rows, items); err != nil {
		t.Fatal(err)
	}
	after := f.tick()
	for _, lane := range []string{"reviews_owed", "feedback_handling"} {
		if after[lane].EventKey != before[lane].EventKey || after[lane].ContextKey != before[lane].ContextKey {
			t.Fatalf("adoption manufactured %s work", lane)
		}
	}
}
