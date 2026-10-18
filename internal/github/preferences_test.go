package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPullPreferencesUseTargetBaseNotHeadOrDefaultBranch(t *testing.T) {
	base, tree, blob := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	data := "schema_version=1\n[review]\ninstructions='Project review guidance'\n"
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != "GET" {
					t.Error("configuration acquisition must be read-only")
				}
				switch r.URL.Path {
				case "/repos/owner/repo/commits/" + base:
					sha := base
					if mismatch {
						sha = strings.Repeat("d", 40)
					}
					fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, sha, tree)
				case "/repos/owner/repo/git/trees/" + tree:
					fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":".reviewd","mode":"100644","type":"blob","sha":%q}]}`, tree, blob)
				case "/repos/owner/repo/git/blobs/" + blob:
					fmt.Fprintf(w, `{"sha":%q,"encoding":"base64","size":%d,"content":%q}`, blob, len(data), base64.StdEncoding.EncodeToString([]byte(data)))
				default:
					t.Errorf("unexpected mutable/head request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := New(Options{Host: "github.com", BaseURL: server.URL, Token: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			preferences, err := client.PullPreferences(context.Background(), Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 17}, TargetBaseSHA: base, HeadSHA: strings.Repeat("e", 40)})
			if mismatch {
				if err == nil || preferences != nil || requests != 1 {
					t.Fatalf("mismatched commit accepted: %+v %v requests=%d", preferences, err, requests)
				}
			} else if err != nil || preferences == nil || preferences.BaseSHA != base || string(preferences.Data) != data || requests != 3 {
				t.Fatalf("target-base preferences lost: %+v %v requests=%d", preferences, err, requests)
			}
		})
	}
}
