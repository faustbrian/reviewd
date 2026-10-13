package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewContextCompletesRemotePagesAndFreezesMaterialEvidence(t *testing.T) {
	pages := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			pages++
			if r.URL.Query().Get("page") == "1" {
				items := []map[string]any{}
				for i := 1; i <= 100; i++ {
					items = append(items, map[string]any{"id": i, "body": "first page", "state": "COMMENTED", "commit_id": strings.Repeat("a", 40), "user": map[string]any{"id": 42, "login": "reviewer"}})
				}
				json.NewEncoder(w).Encode(items)
			} else {
				fmt.Fprint(w, `[{"id":101,"body":"later-page requirement","state":"CHANGES_REQUESTED","commit_id":"`+strings.Repeat("a", 40)+`","user":{"id":43,"login":"human"}}]`)
			}
			return
		}
		if r.URL.Path == "/graphql" {
			var request struct {
				Query string `json:"query"`
			}
			json.NewDecoder(r.Body).Decode(&request)
			if strings.Contains(request.Query, "reviewThreads") {
				fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,"reviewThreads":{"nodes":[{"id":"thread1","isResolved":false,"isOutdated":false,"path":"file.go","line":3,"diffSide":"RIGHT","comments":{"nodes":[{"id":"comment-node","databaseId":55,"body":"unresolved context","url":"https://github.com/owner/repo/pull/17#discussion_r55","author":{"login":"human"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
			} else {
				fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,"closingIssuesReferences":{"nodes":[{"id":"issue-node","databaseId":800,"number":20,"title":"Required outcome","body":"caller parity","url":"https://github.com/owner/repo/issues/20","updatedAt":"2026-10-13T14:00:00Z","repository":{"nameWithOwner":"owner/repo"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
			}
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer peer.Close()
	client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	input := Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, ID: 700, RepositoryID: 99, HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40)}
	evidence, err := client.Context(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 || len(evidence.Reviews) != 101 || len(evidence.Threads) != 1 || len(evidence.Requirements) != 1 || len(evidence.Gaps) != 0 {
		t.Fatalf("complete context unavailable/lost: %+v", evidence)
	}
	if !strings.Contains(string(evidence.Reviews[100]), "later-page requirement") || evidence.Digest == "" {
		t.Error("later-page material evidence or frozen context identity lost")
	}
}
