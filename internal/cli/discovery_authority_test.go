package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/github"
)

func authorityPeer(t *testing.T, pull func() map[string]any, events func() []any, member *atomic.Bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value any = []any{}
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/other":
			value = map[string]any{"id": 199, "full_name": "owner/other"}
		case "/repos/owner/repo/pulls":
			value = []any{pull()}
		case "/graphql":
			var input struct {
				Query     string
				Variables struct{ Number int }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			page := map[string]any{"hasNextPage": false, "endCursor": nil}
			if strings.Contains(input.Query, "ReviewdTeamMembership") {
				nodes := []any{}
				if member.Load() {
					nodes = append(nodes, map[string]any{"databaseId": 43, "login": "reviewer"})
				}
				value = map[string]any{"data": map[string]any{"node": map[string]any{"id": "TEAM12", "databaseId": 12, "members": map[string]any{"nodes": nodes, "pageInfo": page}}}}
			} else {
				connection := map[string]any{"nodes": []any{}, "pageInfo": page}
				value = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"databaseId": 701, "number": 1, "timelineItems": map[string]any{"nodes": events(), "pageInfo": page}, "reviewThreads": connection, "closingIssuesReferences": connection}}}}
			}
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	})
}

func authorityEvent(id, kind string, reviewer int) any {
	return map[string]any{"id": id, "createdAt": "2026-10-13T20:00:00Z", "requestedReviewer": map[string]any{"__typename": kind, "databaseId": reviewer}}
}

func TestRequestRemovalCannotReleaseHeldOptionalBacklog(t *testing.T) {
	var removed, member atomic.Bool
	invoke := lifecycleCLI(t, "auto", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", !removed.Load(), false) }, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
	initial := invoke("sync")
	if len(initial.Items) != 1 || initial.Items[0].State != "pending_decision" || !initial.Items[0].Baseline {
		t.Fatalf("initial backlog not held: %+v", initial)
	}
	id := initial.Items[0].ID
	removed.Store(true)
	changed := invoke("sync")
	for _, item := range changed.Items {
		if item.ID == id && (item.State == "queued" || !item.Baseline) {
			t.Fatalf("request removal released held backlog: %+v", item)
		}
	}
	for _, item := range invoke("inbox").Items {
		if item.ID == id && item.State == "queued" {
			t.Fatalf("reopened backlog was queued: %+v", item)
		}
	}
}

func TestMembershipChangeCannotManufactureFreshHistoricalWork(t *testing.T) {
	var member, fresh atomic.Bool
	invoke := lifecycleCLI(t, "ignore", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", false, true) }, func() []any {
		rows := []any{authorityEvent("OLD_TEAM", "Team", 12)}
		if fresh.Load() {
			rows = append(rows, authorityEvent("FRESH_TEAM", "Team", 12))
		}
		return rows
	}, &member))
	if initial := invoke("sync"); len(initial.Items) != 0 {
		t.Fatalf("nonmember acquired owed work: %+v", initial)
	}
	member.Store(true)
	historical := invoke("sync")
	if len(historical.Items) != 1 || historical.Items[0].State != "pending_decision" || !historical.Items[0].Baseline {
		t.Fatalf("membership manufactured new authority: %+v", historical)
	}
	id := historical.Items[0].ID
	fresh.Store(true)
	requested := invoke("sync")
	if len(requested.Items) != 1 || requested.Items[0].ID != id || requested.Items[0].State != "queued" || requested.Items[0].Trigger != "review_requested" {
		t.Fatalf("genuine new team request not admitted: %+v", requested)
	}
}

func TestRemovedAllowlistWorkIsNotActionable(t *testing.T) {
	var member atomic.Bool
	invoke, path := lifecycleCLIConfig(t, "ignore", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
	if initial := invoke("sync"); len(initial.Items) != 1 {
		t.Fatalf("initial work missing: %+v", initial)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "owner/repo", "owner/other", 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if changed := invoke("sync"); len(changed.Items) != 0 {
		t.Fatalf("removed repository remains actionable: %+v", changed)
	}
	if reopened := invoke("inbox"); len(reopened.Items) != 0 {
		t.Fatalf("reopened excluded work remains actionable: %+v", reopened)
	}
}

func TestSlowClientAcquisitionCannotRestoreExcludedRepository(t *testing.T) {
	for _, established := range []bool{false, true} {
		t.Run(strconv.FormatBool(established), func(t *testing.T) { testSlowClientAcquisition(t, established) })
	}
}

func testSlowClientAcquisition(t *testing.T, established bool) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	configuration := "schema_version = 1\n[github]\nrepositories = [\"owner/repo\"]\n[actions]\nreview_requested = \"auto\"\n[state]\ndirectory = " + strconv.Quote(filepath.Join(root, "state")) + "\n"
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	var member atomic.Bool
	server := httptest.NewServer(authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
	defer server.Close()
	client, err := github.New(github.Options{Host: "github.com", BaseURL: server.URL, Token: "owned-authority-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var block atomic.Bool
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		if block.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return client, nil
	}}
	type outcome struct {
		code        int
		inbox       lifecycleInbox
		diagnostics string
	}
	invoke := func(command string) outcome {
		var output, diagnostics bytes.Buffer
		code := Run(context.Background(), []string{"--config", path, command, "--json"}, &output, &diagnostics, deps)
		var inbox lifecycleInbox
		if err := json.Unmarshal(output.Bytes(), &inbox); err != nil {
			return outcome{code: code, diagnostics: diagnostics.String() + " invalid inbox: " + err.Error()}
		}
		return outcome{code, inbox, diagnostics.String()}
	}
	if established {
		if initial := invoke("sync"); initial.code != 0 || len(initial.inbox.Items) != 1 {
			t.Fatalf("could not establish existing scope: %+v", initial)
		}
	}
	block.Store(true)
	done := make(chan outcome, 1)
	go func() { done <- invoke("sync") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first sync did not reach client acquisition")
	}
	configuration = strings.Replace(configuration, `["owner/repo"]`, `[]`, 1)
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	if current := invoke("sync"); current.code == 0 || len(current.inbox.Items) != 0 {
		t.Fatalf("empty allowlist did not revoke work: %+v", current)
	}
	unblock.Do(func() { close(release) })
	select {
	case late := <-done:
		if late.code == 0 || len(late.inbox.Items) != 0 {
			t.Fatalf("older client acquisition restored excluded work: %+v", late)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("older synchronization did not terminate")
	}
	if current := invoke("inbox"); current.code != 0 || len(current.inbox.Items) != 0 || len(current.inbox.Discovery) != 1 || current.inbox.Discovery[0].State != "inactive" {
		t.Fatalf("excluded work returned after reopening: %+v", current)
	}
	configuration = strings.Replace(configuration, `repositories = []`, `repositories = ["owner/repo"]`, 1)
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	if current := invoke("sync"); current.code != 0 || len(current.inbox.Items) != 1 || current.inbox.Discovery[0].State != "ok" {
		t.Fatalf("new invocation could not acquire re-added scope: %+v", current)
	}
}

func changeLifecycleConfig(t *testing.T, path, before, after string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), before) {
		t.Fatalf("configuration lacks expected setting %q", before)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), before, after, 1)), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedWorkReconcilesOperatorActionWithoutStartingBacklog(t *testing.T) {
	for _, initial := range []string{"ask", "auto"} {
		for _, replacement := range []string{"ignore", "notify"} {
			t.Run(initial+"-to-"+replacement, func(t *testing.T) {
				var member atomic.Bool
				invoke, path := lifecycleCLIConfig(t, "ignore", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
				changeLifecycleConfig(t, path, `review_requested = "auto"`, `review_requested = `+strconv.Quote(initial))
				before := invoke("sync")
				if len(before.Items) != 1 || before.Items[0].State != "pending_decision" {
					t.Fatalf("initial request not held: %+v", before)
				}
				id := before.Items[0].ID
				changeLifecycleConfig(t, path, `review_requested = `+strconv.Quote(initial), `review_requested = `+strconv.Quote(replacement))
				for _, command := range []string{"sync", "inbox"} {
					current := invoke(command)
					if replacement == "ignore" && len(current.Items) != 0 {
						t.Fatalf("ignored request remains actionable: %+v", current)
					}
					if replacement == "notify" && (len(current.Items) != 1 || current.Items[0].ID != id || current.Items[0].State != "attention" || current.Items[0].Action != "notify") {
						t.Fatalf("notification policy not applied: %+v", current)
					}
				}
				changeLifecycleConfig(t, path, `review_requested = `+strconv.Quote(replacement), `review_requested = "auto"`)
				if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].ID != id || current.Items[0].State != "pending_decision" || !current.Items[0].Baseline {
					t.Fatalf("policy edit manufactured automatic backlog authority: %+v", current)
				}
			})
		}
	}
}

func TestUnchangedHeadEventKeepsItsControllingAction(t *testing.T) {
	var member, changed atomic.Bool
	invoke, path := lifecycleCLIConfig(t, "ignore", authorityPeer(t, func() map[string]any {
		pull := lifecyclePull(1, "open", true, false)
		if changed.Load() {
			pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
		}
		return pull
	}, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
	changeLifecycleConfig(t, path, `review_requested = "auto"`, `review_requested = "notify"`)
	invoke("sync")
	changed.Store(true)
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].State != "queued" || current.Items[0].Trigger != "head_updated" {
		t.Fatalf("head event not admitted: %+v", current)
	}
	changeLifecycleConfig(t, path, `head_updated = "auto"`, `head_updated = "ignore"`)
	if current := invoke("sync"); len(current.Items) != 0 {
		t.Fatalf("revoked head action fell back to request action: %+v", current)
	}
	changeLifecycleConfig(t, path, `head_updated = "ignore"`, `head_updated = "notify"`)
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].Trigger != "head_updated" || current.Items[0].State != "attention" {
		t.Fatalf("head event identity lost across policy changes: %+v", current)
	}
}

func TestDisabledRequestWatcherCannotUpgradeOptionalNotifications(t *testing.T) {
	var member, fresh, requestedAgain atomic.Bool
	invoke, path := lifecycleCLIConfig(t, "notify", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", fresh.Load(), false) }, func() []any {
		if fresh.Load() {
			events := []any{authorityEvent("WHILE_DISABLED", "User", 43)}
			if requestedAgain.Load() {
				events = append(events, authorityEvent("AFTER_ENABLED", "User", 43))
			}
			return events
		}
		return []any{}
	}, &member))
	changeLifecycleConfig(t, path, "[github]\n", "[github]\nwatch_review_requests = false\n")
	initial := invoke("sync")
	if len(initial.Items) != 1 || initial.Items[0].State != "attention" || initial.Items[0].Trigger != "new_pull_request" {
		t.Fatalf("optional discovery not notification-only: %+v", initial)
	}
	fresh.Store(true)
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].State != "attention" || current.Items[0].Trigger != "new_pull_request" {
		t.Fatalf("disabled request watcher acquired automatic authority: %+v", current)
	}
	changeLifecycleConfig(t, path, "watch_review_requests = false", "watch_review_requests = true")
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].State == "queued" {
		t.Fatalf("enabling watcher revived historical request as fresh: %+v", current)
	}
	requestedAgain.Store(true)
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].State != "queued" || current.Items[0].Trigger != "review_requested" {
		t.Fatalf("new enabled request did not create work: %+v", current)
	}
	changeLifecycleConfig(t, path, `review_requested = "auto"`, `review_requested = "ignore"`)
	if current := invoke("sync"); len(current.Items) != 0 {
		t.Fatalf("queued request survived automatic-policy revocation: %+v", current)
	}
}

func TestFollowedOutstandingEventSurvivesPolicyChange(t *testing.T) {
	for _, replacement := range []string{"notify", "ask"} {
		t.Run(replacement, func(t *testing.T) {
			var member, removed, changed atomic.Bool
			invoke, path := lifecycleCLIConfig(t, "ignore", authorityPeer(t, func() map[string]any {
				pull := lifecyclePull(1, "open", !removed.Load(), false)
				if changed.Load() {
					pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
				}
				return pull
			}, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member))
			initial := invoke("sync")
			id := initial.Items[0].ID
			invoke("follow", id)
			removed.Store(true)
			changed.Store(true)
			if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].Trigger != "head_updated" || current.Items[0].State != "queued" {
				t.Fatalf("followed head event not admitted: %+v", current)
			}
			changeLifecycleConfig(t, path, `head_updated = "auto"`, `head_updated = `+strconv.Quote(replacement))
			want := "attention"
			if replacement == "ask" {
				want = "pending_decision"
			}
			for _, command := range []string{"sync", "inbox", "sync"} {
				current := invoke(command)
				if len(current.Items) != 1 || current.Items[0].ID != id || current.Items[0].Trigger != "head_updated" || current.Items[0].State != want {
					t.Fatalf("outstanding followed event disappeared: %+v", current)
				}
			}
		})
	}
}

func TestUnexpectedlyOpenMissingPullRetainsCachedWorkAndReportsFailure(t *testing.T) {
	var missing, member atomic.Bool
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("OLD_USER", "User", 43)} }, &member)
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/pulls/1" {
			json.NewEncoder(w).Encode(lifecyclePull(1, "open", true, false))
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls" && missing.Load() {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		peer.ServeHTTP(w, r)
	}))
	initial := invoke("sync")
	if len(initial.Items) != 1 {
		t.Fatalf("initial request missing: %+v", initial)
	}
	id := initial.Items[0].ID
	missing.Store(true)
	var output, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"--config", path, "sync", "--json"}, &output, &diagnostics, deps)
	var current lifecycleInbox
	if err := json.Unmarshal(output.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if code == 0 || len(current.Items) != 1 || current.Items[0].ID != id || len(current.Discovery) != 1 || current.Discovery[0].State != "failed" {
		t.Fatalf("incomplete snapshot silently retired owed work: code=%d inbox=%+v diagnostics=%s", code, current, diagnostics.String())
	}
	if current := invoke("inbox"); len(current.Items) != 1 || current.Items[0].ID != id || current.Discovery[0].State != "failed" {
		t.Fatalf("failed snapshot erased durable work: %+v", current)
	}
	missing.Store(false)
	if current := invoke("sync"); len(current.Items) != 1 || current.Items[0].ID != id || current.Discovery[0].State != "ok" {
		t.Fatalf("complete acquisition did not recover: %+v", current)
	}
}
