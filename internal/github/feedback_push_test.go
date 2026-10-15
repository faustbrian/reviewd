package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/source"
)

type feedbackCommitPusher interface {
	PushFeedbackCommit(context.Context, FeedbackTarget, *source.Snapshot, string, string, string, []byte) (FeedbackTarget, error)
}

func TestFeedbackPushExactRefCASAndCanonicalReadback(t *testing.T) {
	for _, scenario := range []string{"delivered", "concurrent-rewind", "lost-status-readback", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "work")
			os.Mkdir(work, 0700)
			environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=f@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=f@example.invalid"}
			git := func(directory string, args ...string) []byte {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = directory
				cmd.Env = environment
				data, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("owned Git fixture: %v: %s", err, data)
				}
				return data
			}
			git(work, "init", "-q", "-b", "main", "--template=")
			os.WriteFile(filepath.Join(work, "source.txt"), []byte("base\n"), 0600)
			git(work, "add", "source.txt")
			git(work, "commit", "-qm", "base")
			base := strings.TrimSpace(string(git(work, "rev-parse", "HEAD")))
			git(work, "checkout", "-qb", "feature/existing")
			os.WriteFile(filepath.Join(work, "source.txt"), []byte("before\n"), 0600)
			git(work, "add", "source.txt")
			git(work, "commit", "-qm", "original PR")
			old := strings.TrimSpace(string(git(work, "rev-parse", "HEAD")))
			git(work, "update-ref", "refs/pull/17/head", old)
			bare := filepath.Join(root, "remote.git")
			git(work, "clone", "-q", "--bare", "--no-hardlinks", work, bare)
			snapshot, err := source.Acquire(context.Background(), source.Options{RemoteURL: "file://" + work, AllowLocalFixture: true, TempRoot: root}, source.Boundary{TargetBaseSHA: base, HeadSHA: old, Number: 17, ChangedFiles: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			os.WriteFile(filepath.Join(work, "source.txt"), []byte("fixed\n"), 0600)
			git(work, "add", "source.txt")
			git(work, "commit", "-qm", "fix: preserve feedback behavior")
			commit := strings.TrimSpace(string(git(work, "rev-parse", "HEAD")))
			tree := strings.TrimSpace(string(git(work, "rev-parse", "HEAD^{tree}")))
			raw := git(work, "cat-file", "commit", commit)
			var writes atomic.Int32
			var foreign atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1) }))
			defer outside.Close()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				head := strings.TrimSpace(string(git(work, "--git-dir="+bare, "rev-parse", "refs/heads/feature/existing")))
				switch r.URL.Path {
				case "/fork/repo.git/info/refs", "/fork/repo.git/git-receive-pack":
					user, password, ok := r.BasicAuth()
					if !ok || user != "x-access-token" || password != "owned-fixture" {
						t.Error("missing scoped transport auth")
						w.WriteHeader(401)
						return
					}
					if scenario == "redirect" {
						http.Redirect(w, r, outside.URL, http.StatusTemporaryRedirect)
						return
					}
					args := []string{"-c", "receive.denyNonFastForwards=true", "receive-pack", "--stateless-rpc"}
					if r.Method == "GET" {
						if r.URL.RawQuery != "service=git-receive-pack" {
							t.Error("wrong discovery scope")
						}
						args = append(args, "--advertise-refs")
						w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
						fmt.Fprint(w, "001f# service=git-receive-pack\n0000")
					} else if r.Method == "POST" {
						writes.Add(1)
						w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
					} else {
						t.Error("unexpected Git method")
						return
					}
					args = append(args, bare)
					cmd := exec.Command("git", args...)
					cmd.Dir = root
					cmd.Env = environment
					cmd.Stdin = r.Body
					var output bytes.Buffer
					cmd.Stdout = &output
					cmd.Stderr = io.Discard
					// A rejected compare-and-swap is a normal protocol response.
					_ = cmd.Run()
					if r.Method == "POST" && scenario == "lost-status-readback" {
						fmt.Fprint(w, "0008oops")
						return
					}
					w.Write(output.Bytes())
					if r.Method == "GET" && scenario == "concurrent-rewind" {
						git(work, "--git-dir="+bare, "update-ref", "refs/heads/feature/existing", base, old)
					}
				case "/repos/owner/repo", "/repos/fork/repo":
					if strings.Contains(r.URL.Path, "fork") {
						fmt.Fprint(w, `{"id":101,"full_name":"fork/repo","archived":false,"disabled":false,"default_branch":"main","permissions":{"push":true}}`)
					} else {
						fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
					}
				case "/repos/owner/repo/pulls/17":
					count := 1
					if head == commit {
						count = 2
					}
					fmt.Fprintf(w, `{"id":700,"number":17,"state":"open","merged":false,"draft":false,"title":"Existing PR","body":"contract","updated_at":"2026-10-15T00:00:00Z","user":{"id":43,"login":"author"},"base":{"ref":"main","sha":%q,"repo":{"id":99,"full_name":"owner/repo"}},"head":{"ref":"feature/existing","sha":%q,"repo":{"id":101,"full_name":"fork/repo"}},"changed_files":1,"commits":%d}`, base, head, count)
				case "/repos/fork/repo/branches/feature/existing":
					fmt.Fprint(w, `{"name":"feature/existing","protected":false}`)
				case "/repos/fork/repo/rules/branches/feature/existing":
					fmt.Fprint(w, `[]`)
				case "/graphql":
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"databaseId": 700, "number": 17, "state": "OPEN", "merged": false, "mergeQueueEntry": nil, "headRefOid": head, "baseRefOid": base, "headRepository": map[string]any{"databaseId": 101, "nameWithOwner": "fork/repo"}}}}})
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture"})
			target, err := client.FeedbackTarget(context.Background(), "owner/repo", 17)
			if err != nil {
				t.Fatal(err)
			}
			pusher, ok := any(client).(feedbackCommitPusher)
			if !ok {
				t.Fatal("exact-ref additive feedback transport is missing")
			}
			delivered, err := pusher.PushFeedbackCommit(context.Background(), target, snapshot, work, commit, tree, raw)
			actual := strings.TrimSpace(string(git(work, "--git-dir="+bare, "rev-parse", "refs/heads/feature/existing")))
			if scenario == "concurrent-rewind" {
				if err == nil || actual != base || writes.Load() != 1 {
					t.Fatalf("concurrent rewind was overwritten: head=%s writes=%d err=%v", actual, writes.Load(), err)
				}
				return
			}
			if scenario == "redirect" {
				if err == nil || actual != old || writes.Load() != 0 || foreign.Load() != 0 {
					t.Fatal("redirect expanded publication scope", err)
				}
				return
			}
			if err != nil || delivered.Pull.HeadSHA != commit || actual != commit || writes.Load() != 1 {
				t.Fatalf("full transport/readback: head=%s writes=%d err=%v", actual, writes.Load(), err)
			}
			if strings.TrimSpace(string(git(work, "--git-dir="+bare, "show", "refs/heads/feature/existing:source.txt"))) != "fixed" {
				t.Fatal("remote object contents differ")
			}
		})
	}
}
