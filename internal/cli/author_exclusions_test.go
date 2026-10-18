package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestExcludedAuthorsNeverEnterReviewInboxButKeepFeedback(t *testing.T) {
	for _, route := range []string{"user", "team", "optional", "following", "head", "context", "rerequest", "own_monitoring_disabled", "named_author", "policy_reload"} {
		t.Run(route, func(t *testing.T) {
			phase := 0
			invoke, path, _ := lifecycleCLIConfigDeps(t, "auto", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pull := lifecyclePull(1, "open", route == "user" || route == "following" || route == "head" || route == "context" || route == "rerequest" || route == "own_monitoring_disabled" || route == "policy_reload", route == "team")
				author := map[string]any{"id": 43, "login": "renamed-actor"}
				if route == "named_author" {
					author = map[string]any{"id": 8, "login": "DePeNdAbOt[bot]"}
				}
				pull["user"] = author
				if phase > 0 && route == "head" {
					pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
				}
				if phase > 0 && route == "context" {
					pull["body"] = "Updated controlling context"
				}
				var value any = []any{}
				switch r.URL.Path {
				case "/user":
					value = map[string]any{"id": 43, "login": "reviewer"}
				case "/repos/owner/repo":
					value = map[string]any{"id": 99, "full_name": "owner/repo"}
				case "/repos/owner/repo/pulls":
					value = []any{pull}
				case "/repos/owner/repo/pulls/1":
					value = pull
				case "/repos/owner/repo/pulls/1/reviews":
					value = []any{map[string]any{"id": 800, "user": map[string]any{"id": 9, "login": "human"}, "state": "COMMENTED", "body": "Please clarify this requirement.", "commit_id": strings.Repeat("a", 40), "submitted_at": "2026-10-13T20:00:00Z", "html_url": "https://github.com/owner/repo/pull/1#pullrequestreview-800"}}
				case "/graphql":
					value = lifecycleGraphQL(t, r, func(int) []any {
						if route == "rerequest" && phase > 0 {
							return []any{authorityEvent("NEW_REQUEST", "User", 43)}
						}
						return []any{}
					})
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					t.Error(err)
				}
			}))
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			configuration := string(original)
			if route == "own_monitoring_disabled" {
				configuration = strings.Replace(configuration, "[github]", "[github]\nwatch_own_prs=false", 1)
			}
			if route == "named_author" {
				configuration += "\n[reviewer]\nexclude_authors=['@me','dependabot[bot]']\n"
			}
			if route == "following" || route == "policy_reload" {
				configuration += "\n[reviewer]\nexclude_authors=[]\n"
			}
			if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
				t.Fatal(err)
			}
			first := invoke("sync")
			if route == "following" || route == "policy_reload" {
				for _, item := range first.Items {
					if item.Category == "reviews_owed" && route == "following" {
						invoke("follow", item.ID)
					}
				}
				configuration = strings.Replace(configuration, "exclude_authors=[]", "exclude_authors=['@me']", 1)
				if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
					t.Fatal(err)
				}
			}
			phase++
			result := invoke("sync")
			feedback := false
			for _, item := range result.Items {
				if item.Category == "reviews_owed" {
					t.Errorf("excluded author admitted through %s: %+v", route, item)
				}
				if item.Category == "own_pr_feedback" {
					feedback = true
				}
			}
			if route != "own_monitoring_disabled" && route != "named_author" && !feedback {
				t.Fatal("review exclusion erased independent feedback")
			}
		})
	}
}
