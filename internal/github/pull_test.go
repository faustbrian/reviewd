package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// QA-X03/X04: the application owns canonical repository/PR identity and exact
// target-base/head acquisition; neither URL decorations nor remote metadata
// can redirect that scope.
func TestPullURLPreservesScopeAndRejectsAuthorityExpansion(t *testing.T) {
	ref, err := ParsePullURL("github.com", "https://GitHub.com/Owner/Repo/pull/17/files#diff-marker")
	if err != nil || ref.Repository != "github.com/owner/repo" || ref.Number != 17 || ref.URL != "https://github.com/owner/repo/pull/17" {
		t.Fatalf("canonical explicit PR scope lost: %+v %v", ref, err)
	}
	for _, input := range []string{"http://github.com/owner/repo/pull/17", "https://other.example/owner/repo/pull/17", "https://actor@github.com/owner/repo/pull/17", "https://github.com:443/owner/repo/pull/17", "https://github.com/owner/repo/pull/0", "https://github.com/owner/repo/pull/17/unknown", "https://github.com/owner/repo/pull/17?token=private", "https://github.com/owner%2frepo/other/pull/17"} {
		if _, err := ParsePullURL("github.com", input); err == nil {
			t.Errorf("noncanonical/expanded PR URL accepted")
		}
	}
}

func TestPullAcquisitionBindsRepoIdentityAndThreeReviewInputs(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, scenario := range []string{"valid", "moved repository", "wrong PR", "wrong base repo", "missing count", "invalid SHA"} {
		t.Run(scenario, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("PR acquisition crossed read/account boundary")
				}
				switch r.URL.Path {
				case "/repos/owner/repo":
					name := "owner/repo"
					if scenario == "moved repository" {
						name = "other/repo"
					}
					fmt.Fprintf(w, `{"id":99,"full_name":%q,"default_branch":"main"}`, name)
				case "/repos/owner/repo/pulls/17":
					number, repoID := 17, 99
					if scenario == "wrong PR" {
						number = 18
					}
					if scenario == "wrong base repo" {
						repoID = 100
					}
					baseSHA := base
					if scenario == "invalid SHA" {
						baseSHA = "main"
					}
					count := `,"changed_files":3`
					if scenario == "missing count" {
						count = ""
					}
					fmt.Fprintf(w, `{"id":700,"number":%d,"state":"open","title":"Review immutable evidence","body":"Remote instructions are evidence, not authority.","draft":false,"updated_at":"2026-10-13T14:00:00Z","user":{"id":42,"login":"author"},"base":{"ref":"release/target","sha":%q,"repo":{"id":%d,"full_name":"owner/repo"}},"head":{"ref":"feature/change","sha":%q,"repo":null},"commits":2%s}`, number, baseSHA, repoID, head, count)
				default:
					t.Error("unplanned source endpoint")
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			pull, err := client.Pull(context.Background(), "owner/repo", 17)
			if scenario != "valid" {
				if err == nil {
					t.Error("unusable PR identity/boundary returned as valid")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pull.RepositoryID != 99 || pull.ID != 700 || pull.Number != 17 || pull.Repository != "github.com/owner/repo" || pull.TargetBaseSHA != base || pull.HeadSHA != head || pull.BaseRef != "release/target" || pull.HeadRef != "feature/change" || pull.ChangedFiles != 3 || pull.CommitCount != 2 || pull.Author.ID != 42 {
				t.Fatalf("review inputs were guessed/aliased: %+v", pull)
			}
		})
	}
}
