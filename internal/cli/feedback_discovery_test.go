package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// These fixtures exercise real acquisition, CLI configuration and SQLite. Sync
// may discover modifying work, but must never start a model or remote mutation.
func TestFeedbackDiscoveryHasIndependentAuthorsAndUpgradeBaseline(t *testing.T) {
	for _, selected := range []string{"self", "named"} {
		t.Run(selected, func(t *testing.T) {
			body, extra, head, own, closed := "Initial concern", "", strings.Repeat("a", 40), false, false
			authorID, authorLogin, filter := 43, "renamed-actor", "@me"
			if selected == "named" {
				authorID, authorLogin, filter = 8, "DePeNdAbOt[bot]", "dependabot[bot]"
			}
			invoke, path := feedbackDiscoveryCLI(t, &body, &extra, &head, &own, &closed, authorID, authorLogin)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.Replace(string(original), "[github]", "[github]\nwatch_own_prs=false", 1) + "\n[reviewer]\nexclude_authors=['@me','dependabot[bot]']\n"
			write := func(settings string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(base+settings), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("")
			if before := invoke("sync"); len(before.Items) != 0 {
				t.Fatalf("legacy monitoring unexpectedly selected author: %+v", before)
			}
			write("\n[feedback]\nauthors=['" + filter + "']\naction='auto'\n")
			first := feedbackDiscoveryItem(t, invoke("sync"))
			if first.State != "pending_decision" || !first.Baseline || first.ExecutionKind != "feedback_v1" {
				t.Fatalf("upgrade acquired automatic authority: %+v", first)
			}
			body = "Edited human concern at the same head"
			fresh := feedbackDiscoveryItem(t, invoke("sync"))
			if fresh.State != "queued" || fresh.Baseline || fresh.ID != first.ID || fresh.ContextKey == first.ContextKey {
				t.Fatalf("fresh same-head feedback did not queue distinct work: %+v", fresh)
			}
			if again := feedbackDiscoveryItem(t, invoke("sync")); again.ContextKey != fresh.ContextKey || again.State != "queued" {
				t.Fatalf("unchanged poll lost feedback: %+v", again)
			}
			write("\n[feedback]\nauthors=[]\naction='auto'\n")
			if disabled := invoke("sync"); len(disabled.Items) != 0 {
				t.Fatalf("empty author filter retained work: %+v", disabled)
			}
			write("\n[feedback]\nauthors=['" + filter + "']\naction='auto'\n")
			if enabled := feedbackDiscoveryItem(t, invoke("sync")); enabled.State != "pending_decision" || !enabled.Baseline {
				t.Fatalf("re-enabling filter manufactured new feedback: %+v", enabled)
			}
			closed = true
			if ended := invoke("sync"); len(ended.Items) != 0 {
				t.Fatalf("closed feedback survived: %+v", ended)
			}
		})
	}
}

func TestFeedbackDiscoveryIdentityIgnoresOwnEffectsAndRefreshesHead(t *testing.T) {
	body, extra, head, own, closed := "Original concern", "", strings.Repeat("a", 40), false, false
	invoke, path := feedbackDiscoveryCLI(t, &body, &extra, &head, &own, &closed, 43, "reviewer")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := string(original) + "\n[feedback]\naction='auto'\n"
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	first := feedbackDiscoveryItem(t, invoke("sync"))
	if !first.Baseline {
		t.Fatal("initial backlog not held")
	}
	body = "Please handle this revised concern"
	fresh := feedbackDiscoveryItem(t, invoke("sync"))
	if fresh.State != "queued" || fresh.ContextKey == first.ContextKey {
		t.Fatalf("edit did not become fresh work: %+v", fresh)
	}
	// The application may push, update metadata and reply itself. None is new
	// human feedback, but the durable item must display the actual new target.
	head, own = strings.Repeat("c", 40), true
	unchanged := feedbackDiscoveryItem(t, invoke("sync"))
	if unchanged.ContextKey != fresh.ContextKey || unchanged.HeadSHA != head || unchanged.State != "queued" || unchanged.Baseline {
		t.Fatalf("own effects caused replay or stale head: %+v", unchanged)
	}
	extra = "A new human PR discussion comment"
	discussion := feedbackDiscoveryItem(t, invoke("sync"))
	if discussion.ContextKey == fresh.ContextKey || discussion.Trigger != "review_comment_added" || discussion.State != "queued" {
		t.Fatalf("discussion was omitted: %+v", discussion)
	}
	// Policy-only changes cannot turn held historical content into a new event.
	if err := os.WriteFile(path, []byte(strings.Replace(base, "action='auto'", "action='notify'", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	feedbackDiscoveryItem(t, invoke("sync"))
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if enabled := feedbackDiscoveryItem(t, invoke("sync")); enabled.State != "pending_decision" || !enabled.Baseline || enabled.ContextKey != discussion.ContextKey {
		t.Fatalf("policy toggle manufactured new feedback: %+v", enabled)
	}
}

func feedbackDiscoveryItem(t *testing.T, inbox lifecycleInbox) lifecycleItem {
	t.Helper()
	if len(inbox.Items) != 1 || inbox.Items[0].Category != "feedback_handling" {
		t.Fatalf("expected one separate feedback item: %+v", inbox)
	}
	if !inbox.Items[0].Baseline && strings.Contains(inbox.Items[0].State, "queued") && strings.Contains(inbox.Items[0].Explanation, "backlog is held") {
		t.Fatalf("fresh queued feedback is falsely described as held backlog: %+v", inbox)
	}
	return inbox.Items[0]
}

func TestFeedbackDiscoveryQueuesFirstFeedbackOnNewPullAfterEmptyBaseline(t *testing.T) {
	body, extra, head, own, closed := "Fresh submitted concern", "", strings.Repeat("a", 40), false, false
	present := false
	invoke, path := feedbackDiscoveryCLI(t, &body, &extra, &head, &own, &closed, 43, "reviewer", &present)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n[feedback]\naction='auto'\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if initial := invoke("sync"); len(initial.Items) != 0 {
		t.Fatalf("empty repository not empty: %+v", initial)
	}
	present = true
	if fresh := feedbackDiscoveryItem(t, invoke("sync")); fresh.State != "queued" || fresh.Baseline {
		t.Fatalf("fresh new-PR feedback incorrectly held as upgrade backlog: %+v", fresh)
	}
}

func TestFeedbackAutomaticDismissalCannotReleaseHeldBacklog(t *testing.T) {
	body, extra, head, own, closed := "Approved with a nonblocking suggestion", "", strings.Repeat("a", 40), false, false
	invoke, path := feedbackDiscoveryCLI(t, &body, &extra, &head, &own, &closed, 43, "reviewer")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n[feedback]\naction='auto'\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	initial := feedbackDiscoveryItem(t, invoke("sync"))
	if initial.State != "pending_decision" || !initial.Baseline {
		t.Fatalf("fixture backlog not held: %+v", initial)
	}
	head, own = strings.Repeat("c", 40), true
	after := feedbackDiscoveryItem(t, invoke("sync"))
	if after.ContextKey != initial.ContextKey || after.State != "pending_decision" || !after.Baseline || after.HeadSHA != head {
		t.Fatalf("automatic dismissal released historical human feedback: before=%+v after=%+v", initial, after)
	}
}

func feedbackDiscoveryCLI(t *testing.T, body, extra, head *string, own, closed *bool, authorID int, authorLogin string, present ...*bool) (func(string, ...string) lifecycleInbox, string) {
	t.Helper()
	invoke, path, _ := feedbackDiscoveryDeps(t, body, extra, head, own, closed, authorID, authorLogin, present...)
	return invoke, path
}

func feedbackDiscoveryDeps(t *testing.T, body, extra, head *string, own, closed *bool, authorID int, authorLogin string, present ...*bool) (func(string, ...string) lifecycleInbox, string, Dependencies) {
	t.Helper()
	return lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "open"
		if *closed {
			state = "closed"
		}
		pull := lifecyclePull(1, state, false, false)
		pull["user"] = map[string]any{"id": authorID, "login": authorLogin}
		pull["head"].(map[string]any)["sha"] = *head
		if *own {
			pull["title"], pull["body"] = "Updated title", "Updated PR description"
		}
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			if !*closed && (len(present) == 0 || *present[0]) {
				value = []any{pull}
			}
		case "/repos/owner/repo/pulls/1":
			value = pull
		case "/repos/owner/repo/pulls/1/reviews":
			reviewState := "APPROVED"
			if *own {
				reviewState = "DISMISSED"
			}
			value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 9, "login": "human"}, "state": reviewState, "body": *body, "commit_id": *head, "submitted_at": "2026-10-13T20:00:00Z"}}
		case "/repos/owner/repo/issues/1/comments":
			rows := []any{}
			if *extra != "" {
				rows = append(rows, map[string]any{"id": 900, "user": map[string]any{"id": 9, "login": "human"}, "body": *extra})
			}
			if *own {
				rows = append(rows, map[string]any{"id": 901, "user": map[string]any{"id": 43, "login": "reviewer"}, "body": "Addressed in a commit"})
			}
			value = rows
		case "/graphql":
			value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
}
