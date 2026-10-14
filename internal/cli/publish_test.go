package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestExplicitPublicationVerifiesPinnedReviewWithoutStartingCodex(t *testing.T) {
	cases := []struct {
		name                                                                                                                                                                                                                                                                               string
		self, lost, hidden, partial, mixedURL, contextChanged, noDecision, never, existingInline, requirementChanged, existingReview, repairLost, repairHidden, checksChanged, postCheckChanged, clean, twoFindings, repairRace, resolvedThread, partialOutage, repairABA, uncertainSource bool
		state                                                                                                                                                                                                                                                                              store.State
		code                                                                                                                                                                                                                                                                               int
		posts                                                                                                                                                                                                                                                                              int
	}{
		{name: "normal", state: store.Published, posts: 1},
		{name: "source_shutdown_uncertain", uncertainSource: true, state: store.Prepared, code: 1},
		{name: "repair_source_shutdown_uncertain", uncertainSource: true, partial: true, state: store.Partial, code: 1, posts: 1},
		{name: "checks_refresh_rebinds_consent", checksChanged: true, state: store.Prepared, code: 1},
		{name: "checks_fail_during_approval", postCheckChanged: true, clean: true, state: store.Stale, posts: 1},
		{name: "adopt_existing_review", existingReview: true, state: store.Published},
		{name: "self", self: true, state: store.Published, posts: 1},
		{name: "lost_response", lost: true, state: store.Published, posts: 1},
		{name: "unknown_response", lost: true, hidden: true, state: store.PublicationUnknown, code: 1, posts: 1},
		{name: "partial_inline", partial: true, state: store.Partial, code: 1, posts: 1},
		{name: "repair_reconcile_overlap", partial: true, repairRace: true, state: store.Partial, code: 1, posts: 1},
		{name: "own_thread_resolved_during_post", resolvedThread: true, state: store.Stale, posts: 1},
		{name: "repair_after_partial_settlement", partial: true, twoFindings: true, repairABA: true, state: store.Partial, code: 1, posts: 1},
		{name: "partial_readback_outage", partial: true, twoFindings: true, partialOutage: true, state: store.Partial, code: 1, posts: 1},
		{name: "partial_one_present_one_missing", partial: true, twoFindings: true, state: store.Partial, code: 1, posts: 1},
		{name: "partial_lost_response", partial: true, repairLost: true, state: store.Partial, code: 1, posts: 1},
		{name: "partial_unknown_response", partial: true, repairLost: true, repairHidden: true, state: store.Partial, code: 1, posts: 1},
		{name: "mixed_case_url", mixedURL: true, state: store.Published, posts: 1},
		{name: "context_changes_during_post", contextChanged: true, state: store.Stale, posts: 1},
		{name: "no_decision", noDecision: true, state: store.Prepared, code: 2},
		{name: "policy_revoked", never: true, state: store.Prepared, code: 1},
		{name: "existing_inline_reference", existingInline: true, state: store.Prepared, code: 1},
		{name: "linked_requirement_changes_during_post", requirementChanged: true, state: store.Stale, posts: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			root := t.TempDir()
			fixture := filepath.Join(root, "repository")
			if err := os.Mkdir(fixture, 0700); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				command := exec.Command("git", args...)
				command.Dir = fixture
				command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
				out, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("owned Git fixture: %v %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q", "-b", "main", "--template=")
			os.WriteFile(filepath.Join(fixture, "file.go"), []byte("old\n"), 0600)
			git("add", "file.go")
			git("commit", "-qm", "base")
			base := git("rev-parse", "HEAD")
			os.WriteFile(filepath.Join(fixture, "file.go"), []byte(func() string {
				if tc.repairABA {
					return "new\nsecond new\nthird new\n"
				}
				if tc.twoFindings {
					return "new\nsecond new\n"
				}
				return "new\n"
			}()), 0600)
			git("add", "file.go")
			git("commit", "-qm", "head")
			head := git("rev-parse", "HEAD")
			git("update-ref", "refs/pull/17/head", head)
			actor := int64(43)
			authorLogin := "author"
			event, state := "REQUEST_CHANGES", "CHANGES_REQUESTED"
			if tc.clean {
				event, state = "APPROVE", "APPROVED"
			}
			if tc.self {
				actor = 42
				authorLogin = "reviewer"
				event, state = "COMMENT", "COMMENTED"
			}
			var mu sync.Mutex
			posts := 0
			repairPosts := 0
			var repairSubmission map[string]any
			var submission map[string]any
			publishedURL := "https://github.com/owner/repo/pull/17"
			if tc.mixedURL {
				publishedURL = "https://github.com/Owner/Repo/pull/17"
			}
			hidden := false
			repairHidden := false
			repairCompleted := false
			staleReadArmed := false
			historyArmed := false
			historyReads := 0
			historyCaptured, releaseHistory := make(chan struct{}), make(chan struct{})
			var historyOnce sync.Once
			releaseHistoryRead := func() { historyOnce.Do(func() { close(releaseHistory) }) }
			t.Cleanup(releaseHistoryRead)
			var completedRepairs []any
			readerCaptured, repairEntered, releaseRepair := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRepair) }) }
			t.Cleanup(release)
			existingRemote := false
			readbackUnavailable := false
			preflightStarted := false
			submittedAt := time.Now().UTC().Format(time.RFC3339Nano)
			remoteReview := func() map[string]any {
				return map[string]any{"id": 71, "user": map[string]any{"id": actor, "login": "reviewer"}, "commit_id": head, "state": state, "body": submission["body"], "submitted_at": submittedAt, "html_url": publishedURL + "#pullrequestreview-71"}
			}
			remoteComment := func(reviewID int64) map[string]any {
				return map[string]any{"id": 72, "pull_request_review_id": reviewID, "user": map[string]any{"id": actor, "login": "reviewer"}, "commit_id": head, "original_commit_id": head, "path": "file.go", "side": "RIGHT", "line": 1, "original_line": 1, "body": "**Blocker:** The change removes required caller behavior.", "html_url": publishedURL + "#discussion_r72"}
			}
			repairComment := func() map[string]any {
				row := remoteComment(73)
				if tc.twoFindings {
					row["id"] = 74
					row["line"] = 2
					row["original_line"] = 2
					row["body"] = "**Blocker:** The second change removes another required caller behavior."
					row["html_url"] = publishedURL + "#discussion_r74"
				}
				return row
			}
			thirdComment := func() map[string]any {
				row := remoteComment(76)
				row["id"] = 75
				row["line"] = 3
				row["original_line"] = 3
				row["body"] = "**Blocker:** The third change removes required caller behavior."
				row["html_url"] = publishedURL + "#discussion_r75"
				return row
			}
			remoteComments := func() []any {
				if tc.twoFindings {
					return []any{remoteComment(71)}
				}
				if tc.partial || tc.clean {
					return []any{}
				}
				return []any{remoteComment(71)}
			}

			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == "POST" && r.URL.Path == "/repos/owner/repo/pulls/17/reviews" {
					posts++
					if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
						t.Error(err)
					}
					if tc.lost {
						hidden = tc.hidden
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					json.NewEncoder(w).Encode(remoteReview())
					return
				}
				if r.Method == "POST" && r.URL.Path == "/repos/owner/repo/pulls/17/comments" {
					repairPosts++
					if err := json.NewDecoder(r.Body).Decode(&repairSubmission); err != nil {
						t.Error(err)
					}
					if tc.repairRace && repairPosts == 1 {
						mu.Unlock()
						close(repairEntered)
						select {
						case <-releaseRepair:
						case <-r.Context().Done():
						}
						mu.Lock()
					}
					if tc.repairABA {
						row := repairComment()
						if repairSubmission["line"] == float64(3) {
							row = thirdComment()
						}
						completedRepairs = append(completedRepairs, row)
						json.NewEncoder(w).Encode(row)
						return
					}
					repairCompleted = true
					if tc.repairLost {
						repairHidden = tc.repairHidden
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					json.NewEncoder(w).Encode(repairComment())
					return
				}
				if r.Method != "GET" && r.URL.Path != "/graphql" {
					t.Errorf("unexpected mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				if historyArmed && r.URL.Path == "/repos/owner/repo/pulls/17/comments" {
					historyReads++
				}
				if historyArmed && historyReads == 2 && r.URL.Path == "/repos/owner/repo/pulls/17/comments" {
					historyArmed = false
					rows := append([]any{remoteComment(71)}, completedRepairs...)
					mu.Unlock()
					close(historyCaptured)
					select {
					case <-releaseHistory:
					case <-r.Context().Done():
					}
					mu.Lock()
					json.NewEncoder(w).Encode(rows)
					return
				}
				if readbackUnavailable && (strings.Contains(r.URL.Path, "/reviews") || strings.HasSuffix(r.URL.Path, "/comments")) && (!tc.partialOutage || r.URL.Path == "/repos/owner/repo/pulls/17/reviews/71/comments") {
					w.WriteHeader(503)
					return
				}
				if r.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs" {
					if tc.checksChanged || tc.postCheckChanged {
						conclusion := "success"
						if (tc.checksChanged && preflightStarted) || (tc.postCheckChanged && posts > 0) {
							conclusion = "failure"
						}
						fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":201,"name":"quality","head_sha":%q,"status":"completed","conclusion":%q,"details_url":"https://github.com/owner/repo/actions/runs/201","app":{"id":55}}]}`, head, conclusion)
					} else {
						fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
					}
					return
				}
				if staleReadArmed && r.URL.Path == "/repos/owner/repo/pulls/17/reviews/71/comments" {
					staleReadArmed = false
					mu.Unlock()
					close(readerCaptured)
					select {
					case <-repairEntered:
					case <-releaseRepair:
					case <-r.Context().Done():
					}
					mu.Lock()
				}
				switch r.URL.Path {
				case "/user":
					fmt.Fprintf(w, `{"id":%d,"login":"reviewer"}`, actor)
				case "/repos/owner/repo":
					fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
				case "/repos/owner/repo/pulls/17":
					title := "Caller parity"
					if posts > 0 && tc.contextChanged {
						title = "Changed requirements"
					}
					fmt.Fprintf(w, `{"id":700,"number":17,"state":"open","draft":false,"title":%q,"body":"Retain caller behavior.","updated_at":"2026-10-13T14:00:00Z","user":{"id":42,"login":%q},"base":{"sha":%q,"ref":"main","repo":{"id":99,"full_name":"owner/repo"}},"head":{"sha":%q,"ref":"feature"},"changed_files":1,"commits":1}`, title, authorLogin, base, head)
				case "/repos/owner/repo/branches/main":
					fmt.Fprint(w, `{"protected":false}`)
				case "/repos/owner/repo/rules/branches/main":
					if tc.checksChanged || tc.postCheckChanged {
						fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":55}]}}]`)
					} else {
						fmt.Fprint(w, `[]`)
					}
				case "/repos/owner/repo/pulls/17/reviews":
					if (posts == 0 && !existingRemote) || hidden {
						fmt.Fprint(w, `[]`)
					} else {
						json.NewEncoder(w).Encode([]any{remoteReview()})
					}
				case "/repos/owner/repo/pulls/comments/75":
					json.NewEncoder(w).Encode(thirdComment())
				case "/repos/owner/repo/pulls/comments/72", "/repos/owner/repo/pulls/comments/74":
					if repairHidden {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(repairComment())
				case "/repos/owner/repo/pulls/17/reviews/76":
					json.NewEncoder(w).Encode(map[string]any{"id": 76, "user": map[string]any{"id": actor, "login": "reviewer"}, "commit_id": head, "state": "COMMENTED", "body": "", "submitted_at": submittedAt, "html_url": publishedURL + "#pullrequestreview-76"})
				case "/repos/owner/repo/pulls/17/reviews/73":
					json.NewEncoder(w).Encode(map[string]any{"id": 73, "user": map[string]any{"id": actor, "login": "reviewer"}, "commit_id": head, "state": "COMMENTED", "body": "", "submitted_at": submittedAt, "html_url": publishedURL + "#pullrequestreview-73"})
				case "/repos/owner/repo/pulls/17/reviews/71":
					if hidden {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(remoteReview())
				case "/repos/owner/repo/pulls/17/comments", "/repos/owner/repo/pulls/17/reviews/71/comments":
					if tc.repairABA && posts > 0 && r.URL.Path == "/repos/owner/repo/pulls/17/comments" {
						json.NewEncoder(w).Encode(append([]any{remoteComment(71)}, completedRepairs...))
						return
					}
					if tc.partial && repairCompleted && !repairHidden && r.URL.Path == "/repos/owner/repo/pulls/17/comments" {
						rows := []any{repairComment()}
						if tc.twoFindings {
							rows = append([]any{remoteComment(71)}, rows...)
						}
						json.NewEncoder(w).Encode(rows)
						return
					}
					if tc.existingInline {
						if strings.Contains(r.URL.Path, "/reviews/") {
							fmt.Fprint(w, `[]`)
						} else {
							json.NewEncoder(w).Encode(remoteComments())
						}
						return
					}
					if (posts == 0 && !existingRemote) || hidden {
						fmt.Fprint(w, `[]`)
					} else {
						json.NewEncoder(w).Encode(remoteComments())
					}
				case "/graphql":
					var input struct{ Query string }
					json.NewDecoder(r.Body).Decode(&input)
					field := "reviewThreads"
					if posts > 0 && tc.resolvedThread && strings.Contains(input.Query, "reviewThreads") {
						fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,"reviewThreads":{"nodes":[{"id":"THREAD_A","isResolved":true,"isOutdated":false,"path":"file.go","line":1,"diffSide":"RIGHT","comments":{"nodes":[{"id":"COMMENT_A","databaseId":72,"body":"owned finding","url":%q,"author":{"login":"reviewer"},"pullRequestReview":{"databaseId":71}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`, publishedURL+"#discussion_r72")
						return
					}
					if strings.Contains(input.Query, "closingIssuesReferences") {
						field = "closingIssuesReferences"
						if posts > 0 && tc.requirementChanged {
							fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,"closingIssuesReferences":{"nodes":[{"id":"ISSUE_A","databaseId":800,"title":"New requirement","body":"New controlling behavior."}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
							return
						}
					}
					fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,%q:{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`, field)
				default:
					fmt.Fprint(w, `[]`)
				}
			}))
			defer func() { release(); releaseHistoryRead(); peer.Close() }()
			client, err := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "owned-fixture-token"})
			if err != nil {
				t.Fatal(err)
			}
			pull, err := client.Pull(ctx, "owner/repo", 17)
			if err != nil {
				t.Fatal(err)
			}
			remote, err := client.Context(ctx, pull)
			if err != nil {
				t.Fatal(err)
			}
			checks, err := client.Checks(ctx, pull)
			if err != nil {
				t.Fatal(err)
			}
			acquire := func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
				return source.Acquire(ctx, source.Options{RemoteURL: "file://" + fixture, AllowLocalFixture: true, TempRoot: root}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, ChangedFiles: 1, Number: 17})
			}
			snapshot, err := acquire(ctx, pull)
			if err != nil {
				t.Fatal(err)
			}
			anchors, err := source.ParseAnchors(snapshot.Diff)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Close()
			policy, err := config.Resolve(nil, "owner/repo", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			report := review.Report{Conclusion: "REQUEST_CHANGES", Findings: []review.Finding{{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 1, Body: "The change removes required caller behavior."}}, Gaps: []review.ResultGap{}}
			if tc.twoFindings {
				report.Findings = append(report.Findings, review.Finding{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 2, Body: "The second change removes another required caller behavior."})
			}
			if tc.repairABA {
				report.Findings = append(report.Findings, review.Finding{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 3, Body: "The third change removes required caller behavior."})
			}
			if tc.clean {
				report.Conclusion = "APPROVE"
				report.Findings = []review.Finding{}
			}
			prepared, err := review.Prepare(review.Boundary{Pull: pull, Actor: github.Identity{ID: actor, Login: "reviewer"}, MergeBaseSHA: base, ContextDigest: remote.Digest, Configuration: policy, Skill: review.SkillIdentity{Name: "requesting-code-review", Digest: "owned-skill"}, Checks: checks, Anchors: anchors}, report)
			if err != nil {
				t.Fatal(err)
			}
			if tc.existingReview {
				mu.Lock()
				existingRemote = true
				submission = map[string]any{"body": prepared.Body}
				mu.Unlock()
				currentContext, err := client.Context(ctx, pull)
				if err != nil {
					t.Fatal(err)
				}
				boundary := prepared.Boundary
				boundary.ContextDigest = currentContext.Digest
				prepared, err = review.Prepare(boundary, prepared.Report)
				if err != nil {
					t.Fatal(err)
				}
			}

			directory := filepath.Join(root, "state")
			configuration := filepath.Join(root, "config.toml")
			configurationText := "schema_version = 1\n[state]\ndirectory = " + strconvQuote(directory) + "\n"
			if tc.never {
				configurationText += "[actions]\npublish_review = \"never\"\n"
			}
			if err := os.WriteFile(configuration, []byte(configurationText), 0600); err != nil {
				t.Fatal(err)
			}
			durable, err := store.Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			frozen, _ := json.Marshal(policy)
			run, _, err := durable.Create(ctx, store.Run{Repository: pull.Repository, RepositoryID: 99, PullID: 700, Number: 17, ActorID: actor, ActorLogin: "reviewer", WorkKey: "publication-fixture", State: store.Queued, Configuration: frozen, Private: true})
			if err != nil {
				t.Fatal(err)
			}
			durable.Claim(ctx, run.ID)
			payload, _ := json.Marshal(prepared)
			if err := durable.Prepare(ctx, run.ID, prepared.PayloadID, payload, "existing-thread", "existing-turn"); err != nil {
				t.Fatal(err)
			}
			deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return client, nil }, Source: acquire, Review: func(context.Context, review.WorkerRequest) (codex.ReviewOutput, error) {
				t.Fatal("publication started Codex")
				return codex.ReviewOutput{}, nil
			}}
			mu.Lock()
			preflightStarted = true
			mu.Unlock()
			repairRequested := false
			invoke := func() (int, string) {
				var out, diagnostics bytes.Buffer
				args := []string{"--config", configuration, "publish", run.ID, "--json"}
				if repairRequested {
					args = append(args, "--repair")
				}
				if !tc.noDecision {
					args = append(args, "--payload", prepared.PayloadID)
				}
				return Run(ctx, args, &out, &diagnostics, deps), diagnostics.String()
			}
			var uncertain *uncertainPublicationSource
			beforeSource := observedPublication(t, durable, run.ID)
			if tc.uncertainSource && !tc.partial {
				uncertain = newUncertainPublicationSource(t)
				deps.Source = uncertain.acquire
			}
			code, message := invoke()
			if code != tc.code {
				t.Fatalf("publication exit: got %d want %d: %s", code, tc.code, message)
			}
			stored, err := durable.Get(ctx, run.ID)
			if err != nil || stored.State != tc.state {
				t.Fatalf("state: got %s want %s outcome=%s error=%v diagnostic=%s", stored.State, tc.state, stored.Outcome, err, message)
			}
			mu.Lock()
			gotPosts := posts
			if posts > 0 {
				wire, _ := submission["comments"].([]any)
				if len(wire) != len(prepared.Comments) {
					t.Errorf("outbound inline count: got %d want %d", len(wire), len(prepared.Comments))
				} else {
					for i, wanted := range prepared.Comments {
						row, ok := wire[i].(map[string]any)
						if !ok || row["path"] != wanted.Path || row["side"] != wanted.Side || row["line"] != float64(wanted.Line) || row["body"] != wanted.Body {
							t.Errorf("outbound inline differs from canonical finding: %v", row)
						}
					}
				}
			}
			if posts > 0 && (submission["commit_id"] != head || submission["event"] != event || submission["body"] != prepared.Body) {
				t.Errorf("submission lost exact head/event/body: %v", submission)
			}
			mu.Unlock()
			if gotPosts != tc.posts {
				t.Fatalf("writes: got %d want %d", gotPosts, tc.posts)
			}
			if uncertain != nil {
				uncertain.assertRecovered(t, durable, directory, beforeSource)
				return
			}
			if tc.existingInline {
				var referenced review.PreparedResult
				if json.Unmarshal(stored.Prepared, &referenced) != nil || len(referenced.Comments) != 0 || !strings.Contains(referenced.Body, "#discussion_r72") || referenced.PayloadID == prepared.PayloadID {
					t.Fatal("duplicate inline was not turned into a new consent-bound reference preview")
				}
				prepared = referenced
				code, message = invoke()
				if code != 0 {
					t.Fatalf("reference publication: %d %s", code, message)
				}
				mu.Lock()
				writes := posts
				inline, _ := submission["comments"].([]any)
				mu.Unlock()
				if writes != 1 || len(inline) != 0 {
					t.Fatal("reference publication duplicated an existing inline finding")
				}
			} else if tc.state != store.Stale {
				code, message = invoke()
				if code != tc.code {
					t.Fatalf("repeat exit: got %d want %d: %s", code, tc.code, message)
				}
				mu.Lock()
				repeats := posts
				mu.Unlock()
				if repeats != tc.posts {
					t.Fatal("repeat publication created another review")
				}
			}
			if tc.checksChanged {
				if json.Unmarshal(stored.Prepared, &prepared) != nil {
					t.Fatal("changed check preview unavailable")
				}
				code, message = invoke()
				after, err := durable.Get(ctx, run.ID)
				if code != 0 || err != nil || after.State != store.Published {
					t.Fatalf("new check decision failed: %d %s", code, message)
				}
				mu.Lock()
				writes := posts
				mu.Unlock()
				if writes != 1 || !strings.Contains(prepared.Body, "`quality [app_id=55]`") || !strings.Contains(prepared.Body, "failure") {
					t.Fatal("fresh check consent did not preserve exact mandatory gap")
				}
			}
			if tc.repairABA {
				publisher := review.Publisher{GitHub: client, Store: durable, Source: acquire}
				mu.Lock()
				historyArmed = true
				mu.Unlock()
				staleDone := make(chan error, 1)
				go func() { _, err := publisher.Repair(ctx, nil, run.ID, prepared.PayloadID); staleDone <- err }()
				select {
				case <-historyCaptured:
				case <-ctx.Done():
					t.Fatal("stale repair history not captured")
				}
				first, _ := publisher.Repair(ctx, nil, run.ID, prepared.PayloadID)
				if first.State != store.Partial {
					t.Fatalf("first repair should leave one missing: %s", first.Outcome)
				}
				releaseHistoryRead()
				select {
				case err := <-staleDone:
					if err == nil {
						t.Error("stale preflight was allowed to write")
					}
				case <-ctx.Done():
					t.Fatal("stale repair did not finish")
				}
				after := observedPublication(t, durable, run.ID)
				var known review.PublicationOutcome
				json.Unmarshal(after.Outcome, &known)
				mu.Lock()
				writes := repairPosts
				mu.Unlock()
				if writes != 1 || len(known.Repairs) != 1 || known.Repairs[0].ID != 74 {
					t.Fatalf("stale repair reposted or erased completed repair: writes=%d outcome=%s", writes, after.Outcome)
				}
				final, err := publisher.Repair(ctx, nil, run.ID, prepared.PayloadID)
				mu.Lock()
				writes = repairPosts
				line := repairSubmission["line"]
				mu.Unlock()
				if err != nil || final.State != store.Published || writes != 2 || line != float64(3) {
					t.Fatalf("fresh repair did not target only remaining finding: writes=%d outcome=%s err=%v", writes, final.Outcome, err)
				}
				return
			}
			if tc.partialOutage {
				mu.Lock()
				readbackUnavailable = true
				mu.Unlock()
				code, _ = invoke()
				after := observedPublication(t, durable, run.ID)
				var known review.PublicationOutcome
				json.Unmarshal(after.Outcome, &known)
				if code != 1 || after.State != store.PublicationUnknown || known.Review.ID != 71 || known.Review.URL == "" || len(known.Comments) != 1 || known.Comments[0].ID != 72 || known.Comments[0].URL == "" {
					t.Errorf("partial readback outage erased known remote identities: %s", after.Outcome)
				}
				for _, format := range []bool{false, true} {
					var output, diagnostic bytes.Buffer
					args := []string{"--config", configuration, "show", run.ID}
					if format {
						args = append(args, "--json")
					}
					if code := Run(ctx, args, &output, &diagnostic, deps); code != 0 || !strings.Contains(output.String(), "#discussion_r72") || !strings.Contains(output.String(), "#pullrequestreview-71") {
						t.Errorf("reopened presentation lost remote IDs: code=%d %s", code, output.String())
					}
				}
				mu.Lock()
				readbackUnavailable = false
				mu.Unlock()
			}
			if tc.partial {
				if tc.uncertainSource {
					uncertain = newUncertainPublicationSource(t)
					deps.Source = uncertain.acquire
					repairRequested = true
					code, message = invoke()
					if code != 1 {
						t.Fatalf("uncertain repair unexpectedly succeeded: %d %s", code, message)
					}
					uncertain.assertRecovered(t, durable, directory, stored)
					mu.Lock()
					roots, repairs := posts, repairPosts
					mu.Unlock()
					if roots != 1 || repairs != 0 {
						t.Fatalf("uncertain repair or recovery submitted content: reviews=%d repairs=%d", roots, repairs)
					}
					return
				}
				if tc.repairRace {
					mu.Lock()
					staleReadArmed = true
					mu.Unlock()
					readerDone := make(chan int, 1)
					go func() { code, _ := invoke(); readerDone <- code }()
					select {
					case <-readerCaptured:
					case code := <-readerDone:
						t.Fatalf("stale reader returned before capture: %d", code)
					case <-ctx.Done():
						t.Fatal("stale reader was not captured")
					}
					repairRequested = true
					writerDone := make(chan struct {
						code    int
						message string
					}, 1)
					go func() {
						code, message := invoke()
						writerDone <- struct {
							code    int
							message string
						}{code, message}
					}()
					select {
					case <-repairEntered:
					case result := <-writerDone:
						t.Fatalf("repair returned before its captured write: %d %s", result.code, result.message)
					case <-ctx.Done():
						t.Fatal("repair write was not captured")
					}
					select {
					case <-readerDone:
					case <-ctx.Done():
						t.Fatal("overlapping reconcile did not finish")
					}
					code, message = invoke()
					if code != 1 {
						t.Errorf("another writer escaped the active repair: %d %s", code, message)
					}
					claimed, err := durable.Get(ctx, run.ID)
					if err != nil || !strings.Contains(string(claimed.Outcome), "pending_repair") {
						t.Error("older reconcile erased the active repair intent")
					}
					release()
					select {
					case result := <-writerDone:
						code, message = result.code, result.message
					case <-ctx.Done():
						t.Fatal("original repair did not settle")
					}
				} else {
					repairRequested = true
					code, message = invoke()
				}
				repairCode := 0
				finalState := store.Published
				if tc.repairHidden {
					repairCode = 1
					finalState = store.PublicationUnknown
				}
				if code != repairCode {
					t.Fatalf("explicit partial repair failed: %d %s", code, message)
				}
				after, err := durable.Get(ctx, run.ID)
				mu.Lock()
				roots, repairs := posts, repairPosts
				pinned := repairSubmission["commit_id"]
				expectedLine := float64(1)
				expectedBody := "**Blocker:** The change removes required caller behavior."
				if tc.twoFindings {
					expectedLine = 2
					expectedBody = "**Blocker:** The second change removes another required caller behavior."
				}
				if repairSubmission["path"] != "file.go" || repairSubmission["side"] != "RIGHT" || repairSubmission["line"] != expectedLine || repairSubmission["body"] != expectedBody {
					t.Errorf("repair did not submit only the canonical missing finding: %v", repairSubmission)
				}
				mu.Unlock()
				if err != nil || after.State != finalState || roots != 1 || repairs != 1 || (pinned != head) {
					t.Fatalf("repair duplicated or lost verified publication: state=%s roots=%d repairs=%d", after.State, roots, repairs)
				}
				code, message = invoke()
				if code != repairCode {
					t.Fatalf("repeat repair failed: %d %s", code, message)
				}
				mu.Lock()
				repeats := repairPosts
				mu.Unlock()
				if repeats != 1 {
					t.Fatal("repeat repair duplicated an inline finding")
				}
				if tc.repairHidden {
					mu.Lock()
					repairHidden = false
					mu.Unlock()
					code, message = invoke()
					after, err := durable.Get(ctx, run.ID)
					if code != 0 || err != nil || after.State != store.Published {
						t.Fatalf("later repair reconciliation: %d %s state=%s", code, message, after.State)
					}
					mu.Lock()
					repeats = repairPosts
					mu.Unlock()
					if repeats != 1 {
						t.Fatal("later recovery reposted the unknown repair")
					}
				}
			}
			if tc.state == store.Published {
				original := append([]byte{}, stored.Outcome...)
				mu.Lock()
				readbackUnavailable = true
				mu.Unlock()
				code, _ = invoke()
				after, err := durable.Get(ctx, run.ID)
				if code != 1 || err != nil || after.State != store.Published || !bytes.Equal(after.Outcome, original) {
					t.Fatal("readback outage discarded previously verified publication")
				}
			}

			if git("status", "--short") != "" {
				t.Fatal("publisher changed ordinary owned checkout")
			}
		})
	}
}

func observedPublication(t testing.TB, durable *store.Store, id string) store.Run {
	t.Helper()
	run, err := durable.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
