package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// QA-13/16/X06: explain uses immutable default-branch evidence; diagnostics
// establish host identity separately and never invoke a reviewer.
func TestIdentityAndTrustedPreferencesUsePinnedObjects(t *testing.T) {
	baseSHA, treeSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	preferences := "schema_version = 1\n[reviewer]\nreasoning_effort = 'low'\n"
	var paths []string
	var pathsMu sync.Mutex
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("read/auth boundary was not preserved")
			http.Error(w, "denied", 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"full_name":"owner/repo","default_branch":"main"}`)
		case "/repos/owner/repo/commits/main":
			fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, baseSHA, treeSHA)
		case "/repos/owner/repo/git/trees/" + treeSHA:
			fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":".reviewd","mode":"100644","type":"blob","sha":%q}]}`, treeSHA, blobSHA)
		case "/repos/owner/repo/git/blobs/" + blobSHA:
			fmt.Fprintf(w, `{"sha":%q,"encoding":"base64","size":%d,"content":%q}`, blobSHA, len(preferences), base64.StdEncoding.EncodeToString([]byte(preferences)))
		default:
			t.Error("unplanned mutable/head endpoint requested")
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Identity(context.Background())
	if err != nil || identity.Login != "fixture-user" || identity.ID != 42 {
		t.Fatalf("identity not established: %+v %v", identity, err)
	}
	source, err := client.DefaultPreferences(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if source.BaseSHA != baseSHA || source.Branch != "main" || source.Preferences == nil || string(source.Preferences.Data) != preferences || source.Preferences.BaseSHA != baseSHA {
		t.Fatalf("preferences did not retain pinned source: %+v", source)
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(paths) != 5 {
		t.Fatalf("unexpected evidence acquisition paths: %v", paths)
	}
	if _, err := client.DefaultPreferences(context.Background(), "other.example/owner/repo"); err == nil {
		t.Fatal("host-mismatched evidence source accepted")
	}
	if len(paths) != 5 {
		t.Fatal("credentials were sent to a mismatched repository host")
	}
}

func TestMissingPreferencesAreDistinctFromUnavailableOrUntrustedEvidence(t *testing.T) {
	baseSHA, treeSHA, blobSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, scenario := range []string{"absent", "empty", "denied", "truncated", "missing tree", "symlink", "mismatched blob", "invalid JSON"} {
		t.Run(scenario, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo":
					fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","default_branch":"main"}`)
				case "/repos/owner/repo/commits/main":
					fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, baseSHA, treeSHA)
				case "/repos/owner/repo/git/trees/" + treeSHA:
					switch scenario {
					case "denied":
						http.Error(w, "private response must not be echoed", 403)
					case "invalid JSON":
						fmt.Fprint(w, `{"secret":"ghp_private_response"`)
					case "missing tree":
						fmt.Fprintf(w, `{"sha":%q,"truncated":false}`, treeSHA)
					case "absent":
						fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, treeSHA)
					case "truncated":
						fmt.Fprintf(w, `{"sha":%q,"truncated":true,"tree":[]}`, treeSHA)
					default:
						mode := "100644"
						if scenario == "symlink" {
							mode = "120000"
						}
						fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":".reviewd","mode":%q,"type":"blob","sha":%q}]}`, treeSHA, mode, blobSHA)
					}
				case "/repos/owner/repo/git/blobs/" + blobSHA:
					sha := blobSHA
					if scenario == "mismatched blob" {
						sha = baseSHA
					}
					fmt.Fprintf(w, `{"sha":%q,"encoding":"base64","size":0,"content":""}`, sha)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			source, err := client.DefaultPreferences(context.Background(), "owner/repo")
			switch scenario {
			case "absent":
				if err != nil || source.Preferences != nil || source.BaseSHA != baseSHA {
					t.Fatalf("normal absence lost immutable boundary: %+v %v", source, err)
				}
			case "empty":
				if err != nil || source.Preferences == nil || len(source.Preferences.Data) != 0 {
					t.Fatal("present empty preferences became absent")
				}
			default:
				if err == nil || source.Preferences != nil {
					t.Fatalf("untrusted evidence became usable/absent: %+v %v", source, err)
				}
				if strings.Contains(err.Error(), "private_response") {
					t.Fatal("GitHub evidence leaked into diagnostics")
				}
			}
		})
	}
}

func TestGitHubCredentialsDoNotFollowEndpointRedirects(t *testing.T) {
	var received atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		fmt.Fprint(w, `{"id":99,"login":"outside"}`)
	}))
	defer outside.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, outside.URL+"/user", http.StatusFound)
	}))
	defer peer.Close()
	client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Identity(context.Background()); err == nil {
		t.Fatal("redirect produced an authenticated identity")
	}
	if received.Load() != 0 {
		t.Fatal("credential-bearing request reached another API endpoint")
	}
}

func TestRateLimitGuidanceCannotOverflowIntoImmediateRetry(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9223372036854775807")
		w.WriteHeader(429)
	}))
	defer peer.Close()
	client, err := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Identity(context.Background())
	var limited *HTTPError
	if !errors.As(err, &limited) || limited.RetryAfter < time.Hour {
		t.Fatalf("server backoff overflowed: %+v", limited)
	}
}
