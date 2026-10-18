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
)

func TestSyncImportsLaterPageRequestsAndOwnFeedbackWithoutRunningBacklog(t *testing.T) {
	t.Run("user", func(t *testing.T) { syncBaselineFixture(t, false, false, false, false, false, false, "ignore") })
	t.Run("team", func(t *testing.T) { syncBaselineFixture(t, true, false, false, false, false, false, "ignore") })
	t.Run("conditional", func(t *testing.T) { syncBaselineFixture(t, false, true, false, false, false, false, "ignore") })
	t.Run("following", func(t *testing.T) { syncBaselineFixture(t, false, false, true, false, false, false, "ignore") })
	t.Run("overlapping_categories", func(t *testing.T) { syncBaselineFixture(t, true, false, false, false, true, false, "ignore") })
	t.Run("unfollow_feedback_category", func(t *testing.T) { syncBaselineFixture(t, true, false, false, false, true, true, "ignore") })
	t.Run("following_optional_notify", func(t *testing.T) { syncBaselineFixture(t, false, false, true, false, false, false, "notify") })
	t.Run("following_optional_auto", func(t *testing.T) { syncBaselineFixture(t, false, false, true, false, false, false, "auto") })
}

func syncBaselineFixture(t *testing.T, viaTeam, conditional, following, ownedPublication, overlapping, unfollowOverlap bool, optionalAction string) {
	ctx := context.Background()
	root := t.TempDir()
	configuration := filepath.Join(root, "config.toml")
	text := "schema_version = 1\n[github]\nrepositories = [\"owner/repo\"]\n[actions]\nreview_requested = \"auto\"\nnew_pull_request = " + strconv.Quote(optionalAction) + "\n[state]\ndirectory = " + strconv.Quote(filepath.Join(root, "state")) + "\n"
	if overlapping {
		text += "\n[reviewer]\nexclude_authors=[]\n"
	}
	if err := os.WriteFile(configuration, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	var deny, removed, restored, closed, ownPublished atomic.Bool
	var lostRequest atomic.Bool
	var revision atomic.Int32
	var secondPage atomic.Int32
	var firstRequests, secondRequests atomic.Int32
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	pull := func(number int, author int64, requested, draft bool) map[string]any {
		if number == 101 && removed.Load() && !restored.Load() {
			requested = false
		}
		effectiveHead := head
		if number == 101 && revision.Load() > 0 {
			effectiveHead = fmt.Sprintf("%040x", revision.Load())
		}
		reviewers := []any{}
		teams := []any{}
		if requested {
			if viaTeam {
				teams = append(teams, map[string]any{"id": 12, "node_id": "TEAM12", "slug": "reviewers"})
			} else {
				reviewers = append(reviewers, map[string]any{"id": 43, "login": "reviewer"})
			}
		}
		return map[string]any{"id": 700 + number, "number": number, "state": "open", "draft": draft, "title": fmt.Sprintf("Pull %d", number), "body": "Retain required behavior.", "updated_at": "2026-10-13T20:00:00Z", "created_at": "2026-10-13T19:00:00Z", "user": map[string]any{"id": author, "login": "author"}, "base": map[string]any{"sha": base, "ref": "main", "repo": map[string]any{"id": 99, "full_name": "owner/repo"}}, "head": map[string]any{"sha": effectiveHead, "ref": "feature"}, "changed_files": 1, "commits": 1, "requested_reviewers": reviewers, "requested_teams": teams}
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deny.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.URL.Path != "/graphql" {
			t.Errorf("discovery attempted mutation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			rows := []any{}
			if conditional {
				counter, tag := &firstRequests, "\"page-one\""
				if r.URL.Query().Get("page") == "2" {
					counter, tag = &secondRequests, "\"page-two\""
				}
				call := counter.Add(1)
				w.Header().Set("ETag", tag)
				if call > 1 {
					if r.Header.Get("If-None-Match") != tag {
						w.WriteHeader(http.StatusPreconditionFailed)
						return
					}
					if r.URL.Query().Get("page") == "2" {
						secondPage.Add(1)
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			if r.URL.Query().Get("page") == "2" {
				secondPage.Add(1)
				if !closed.Load() {
					rows = append(rows, pull(101, 8, true, false))
				}
				rows = append(rows, pull(102, 43, overlapping && !lostRequest.Load(), false), pull(103, 8, true, true))
			} else {
				for i := 1; i <= 100; i++ {
					rows = append(rows, pull(i, 8, false, false))
				}
				w.Header().Set("Link", "<http://fixture.invalid/repos/owner/repo/pulls?per_page=100&page=2>; rel=\"next\"")
			}
			value = rows
		case "/repos/owner/repo/pulls/101/reviews":
			if ownPublished.Load() {
				value = []any{map[string]any{"id": 810, "user": map[string]any{"id": 43, "login": "reviewer"}, "commit_id": head, "state": "APPROVED", "body": "**No findings.**", "submitted_at": "2026-10-13T20:00:00Z", "html_url": "https://github.com/owner/repo/pull/101#pullrequestreview-810"}}
			}
		case "/repos/owner/repo/pulls/101":
			value = pull(101, 8, false, false)
			if closed.Load() {
				value.(map[string]any)["state"] = "closed"
			}
		case "/repos/owner/repo/pulls/102/reviews":
			value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 8, "login": "human"}, "state": "COMMENTED", "body": "Please clarify this requirement.", "commit_id": head, "submitted_at": "2026-10-13T20:00:00Z", "html_url": "https://github.com/owner/repo/pull/102#pullrequestreview-800"}}
		case "/graphql":
			var input struct {
				Query     string
				Variables struct{ Number int }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if strings.Contains(input.Query, "ReviewdTeamMembership") {
				value = map[string]any{"data": map[string]any{"node": map[string]any{"id": "TEAM12", "databaseId": 12, "members": map[string]any{"nodes": []any{map[string]any{"databaseId": 43, "login": "reviewer"}}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}}}
				break
			}
			events := []any{}
			if input.Variables.Number == 101 && restored.Load() && strings.Contains(input.Query, "ReviewdRequestEvents") {
				events = append(events, map[string]any{"id": "REQUEST_SECOND", "createdAt": "2026-10-13T21:00:00Z", "requestedReviewer": map[string]any{"__typename": "User", "databaseId": 43, "login": "reviewer"}})
			}
			connection := map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
			value = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"databaseId": 700 + input.Variables.Number, "number": input.Variables.Number, "timelineItems": map[string]any{"nodes": events, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}, "reviewThreads": connection, "closingIssuesReferences": connection}}}}
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	defer peer.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-discovery-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, LaunchAgent: func(context.Context, agent.Request) (agent.Output, error) {
		t.Fatal("initial synchronization started a reviewer")
		return agent.Output{}, nil
	}}
	type inbox struct {
		Items []struct {
			ID       string `json:"id"`
			Head     string `json:"head_sha"`
			Trigger  string `json:"trigger"`
			Number   int    `json:"number"`
			Category string `json:"category"`
			State    string `json:"state"`
			Baseline bool   `json:"baseline"`
		}
		Runs      []json.RawMessage `json:"runs"`
		Discovery []struct {
			State string `json:"state"`
		} `json:"discovery"`
	}
	invoke := func(command string, extra ...string) (int, inbox, string) {
		var out, diagnostics bytes.Buffer
		args := append([]string{"--config", configuration, command}, extra...)
		args = append(args, "--json")
		code := Run(ctx, args, &out, &diagnostics, deps)
		var result inbox
		if out.Len() > 0 && json.Unmarshal(out.Bytes(), &result) != nil {
			t.Fatalf("invalid %s JSON: %s", command, out.String())
		}
		return code, result, diagnostics.String()
	}
	for poll := 0; poll < 2; poll++ {
		code, _, diagnostic := invoke("sync")
		if code != 0 {
			t.Fatalf("sync: code=%d %s", code, diagnostic)
		}
		code, result, diagnostic := invoke("inbox")
		if code != 0 || len(result.Items) != 2+boolCount(overlapping)+100*boolCount(optionalAction != "ignore") || len(result.Runs) != 0 {
			t.Fatalf("baseline lost, duplicated or ran work: %+v code=%d %s", result, code, diagnostic)
		}
		for _, item := range result.Items {
			switch item.Number {
			case 101:
				if item.Category != "reviews_owed" || item.State != "pending_decision" || !item.Baseline {
					t.Errorf("automatic backlog was not held: %+v", item)
				}
			case 102:
				if overlapping && item.Category == "reviews_owed" {
					if item.State != "pending_decision" || !item.Baseline {
						t.Errorf("own-PR requested review backlog was not held: %+v", item)
					}
					continue
				}
				if item.Category != "own_pr_feedback" || item.State != "attention" {
					t.Errorf("feedback was classified as review work: %+v", item)
				}
			default:
				if optionalAction == "ignore" || item.Number > 100 || item.Category != "reviews_owed" || item.Trigger != "new_pull_request" {
					t.Errorf("unrelated or excluded draft entered inbox: %+v", item)
				} else if optionalAction == "auto" && (item.State != "pending_decision" || !item.Baseline) {
					t.Errorf("optional automatic backlog was not held: %+v", item)
				}
			}
		}
	}
	if secondPage.Load() != 2 {
		t.Fatal("repository scope was not exhausted on each synchronization")
	}

	if unfollowOverlap {
		_, initial, _ := invoke("inbox")
		id := ""
		for _, item := range initial.Items {
			if item.Number == 102 && item.Category == "own_pr_feedback" {
				id = item.ID
			}
		}
		if id == "" {
			t.Fatal("overlap feedback item missing")
		}
		if code, result, message := invoke("unfollow", id); code != 0 {
			t.Fatalf("feedback unfollow failed: %d %s", code, message)
		} else {
			for _, item := range result.Items {
				if item.Number == 102 && item.Category == "reviews_owed" {
					t.Fatalf("unfollow command retained sibling owed work: %+v", item)
				}
			}
		}
		_, reopened, _ := invoke("inbox")
		feedback := false
		for _, item := range reopened.Items {
			if item.Number == 102 {
				if item.Category == "reviews_owed" {
					t.Fatalf("reopened inbox retained unfollowed sibling work: %+v", item)
				}
				if item.Category == "own_pr_feedback" && item.State == "attention" {
					feedback = true
				}
			}
		}
		if !feedback {
			t.Fatal("unfollowing erased independent feedback")
		}
		return
	}
	if overlapping {
		lostRequest.Store(true)
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("request loss sync: %d %s", code, message)
		}
		_, result, _ := invoke("inbox")
		foundFeedback := false
		for _, item := range result.Items {
			if item.Number == 102 {
				if item.Category != "own_pr_feedback" || item.State != "attention" {
					t.Fatalf("obsolete owed category survived request removal: %+v", item)
				}
				foundFeedback = true
			}
		}
		if !foundFeedback {
			t.Fatal("request removal erased independent feedback")
		}
		return
	}
	if following {
		_, initial, _ := invoke("inbox")
		id := ""
		for _, item := range initial.Items {
			if item.Number == 101 {
				id = item.ID
			}
		}
		if id == "" {
			t.Fatal("review item missing")
		}
		if code, _, message := invoke("follow", id); code != 0 {
			t.Fatalf("follow failed: %d %s", code, message)
		}
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("unchanged follow sync: %d %s", code, message)
		}
		_, unchanged, _ := invoke("inbox")
		for _, item := range unchanged.Items {
			if item.Number == 101 && (item.ID != id || item.State != "pending_decision" || !item.Baseline) {
				t.Fatalf("following unchanged backlog bypassed its decision: %+v", item)
			}
		}
		removed.Store(true)
		revision.Store(1)
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("follow sync: %d %s", code, message)
		}
		_, changed, _ := invoke("inbox")
		found := false
		for _, item := range changed.Items {
			if item.Number == 101 {
				found = true
				if item.Head != fmt.Sprintf("%040x", 1) || item.Trigger != "head_updated" {
					t.Fatalf("request removal lost followed updates: %+v", item)
				}
			}
		}
		if !found {
			t.Fatal("followed review disappeared after request removal")
		}
		if code, _, message := invoke("unfollow", id); code != 0 {
			t.Fatalf("unfollow: %d %s", code, message)
		}
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("unchanged unfollow sync: %d %s", code, message)
		}
		_, unchangedStopped, _ := invoke("inbox")
		for _, item := range unchangedStopped.Items {
			if item.Number == 101 {
				t.Fatalf("optional discovery revived unchanged unfollowed work: %+v", item)
			}
		}
		revision.Store(2)
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("unfollow sync: %d %s", code, message)
		}
		_, stopped, _ := invoke("inbox")
		for _, item := range stopped.Items {
			if item.Number == 101 {
				t.Fatal("unfollowed update returned to inbox")
			}
		}
		restored.Store(true)
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("request sync: %d %s", code, message)
		}
		_, requested, _ := invoke("inbox")
		found = false
		for _, item := range requested.Items {
			if item.Number == 101 {
				found = true
				if item.ID != id || item.Trigger != "review_requested" {
					t.Fatalf("new request did not restore distinct work: %+v", item)
				}
			}
		}
		if !found {
			t.Fatal("new explicit request was suppressed by unfollow")
		}
		closed.Store(true)
		if code, _, message := invoke("sync"); code != 0 {
			t.Fatalf("closure sync: %d %s", code, message)
		}
		_, ended, _ := invoke("inbox")
		for _, item := range ended.Items {
			if item.Number == 101 {
				t.Fatal("closed PR remained actionable")
			}
		}
		return
	}
	deny.Store(true)
	code, _, diagnostic := invoke("sync")
	if code != 1 || !strings.Contains(strings.ToLower(diagnostic), "authentication") {
		t.Errorf("auth failure looked successful: code=%d %s", code, diagnostic)
	}
	code, result, diagnostic := invoke("inbox")
	if code != 0 || len(result.Items) != 2 || len(result.Discovery) != 1 || result.Discovery[0].State != "failed" {
		t.Fatalf("failed discovery became empty success or erased prior work: %+v code=%d %s", result, code, diagnostic)
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
