package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
)

func TestHeadlessWatchImportsDecisionsWithoutStartingInitialBacklog(t *testing.T) {
	for _, action := range []string{"notify", "auto"} {
		t.Run(action, func(t *testing.T) {
			var member atomic.Bool
			peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("EXISTING_REQUEST", "User", 43)} }, &member)
			invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(peer.ServeHTTP))
			changeLifecycleConfig(t, path, `review_requested = "auto"`, `review_requested = `+strconv.Quote(action))
			changeLifecycleConfig(t, path, "[actions]", "[actions]\npublish_review = \"auto\"")
			deps.Source = func(context.Context, github.Pull) (*source.Snapshot, error) {
				t.Fatal("initial backlog or notify-only policy began source acquisition")
				return nil, nil
			}
			var output, diagnostics bytes.Buffer
			code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps)
			if code != 0 {
				t.Fatalf("headless synchronization did not import durable work: code=%d %s", code, diagnostics.String())
			}
			if !strings.Contains(diagnostics.String(), "reviewer") || !strings.Contains(diagnostics.String(), "publish=auto") {
				t.Fatalf("automatic publication policy was enabled without its posting identity: %s", diagnostics.String())
			}
			var current lifecycleInbox
			if err := json.Unmarshal(output.Bytes(), &current); err != nil {
				t.Fatal(err)
			}
			want := "attention"
			if action == "auto" {
				want = "pending_decision"
			}
			if len(current.Items) != 1 || current.Items[0].State != want || !current.Items[0].Baseline || len(current.Runs) != 0 {
				t.Fatalf("headless policy did not preserve initial work: %+v", current)
			}
			if persisted := invoke("inbox"); len(persisted.Items) != 1 || persisted.Items[0].ID != current.Items[0].ID || persisted.Items[0].State != want || len(persisted.Runs) != 0 {
				t.Fatalf("headless decisions did not survive reopening: %+v", persisted)
			}
		})
	}
}

func TestHeadlessWatchPreparesNewRequestOnceAndKeepsPublicationPrivate(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			testHeadlessWatchPreparation(t, pending, false, 1)
		})
	}
	t.Run("automatic-publication", func(t *testing.T) { testHeadlessWatchPreparation(t, true, true, 1) })
	t.Run("drain-multiple", func(t *testing.T) { testHeadlessWatchPreparation(t, false, false, 2) })
	for _, scenario := range []string{"debounced-reentry", "debounced-burst", "acquisition-retry", "same-head-rerequest", "trusted-project-preferences"} {
		t.Run(scenario, func(t *testing.T) { testHeadlessWatchPreparation(t, false, false, 1, scenario) })
	}
	t.Run("prepared-refresh-retry", func(t *testing.T) { testHeadlessWatchPreparation(t, false, false, 1, "prepared-refresh-retry") })
	t.Run("publication-preflight-retry", func(t *testing.T) { testHeadlessWatchPreparation(t, false, true, 1, "publication-preflight-retry") })
	for _, scenario := range []string{"prepared-refresh-shared-retry", "prepared-refresh-graphql-retry", "prepared-refresh-graphql200-retry", "publication-shared-retry"} {
		t.Run(scenario, func(t *testing.T) {
			testHeadlessWatchPreparation(t, false, strings.HasPrefix(scenario, "publication"), 1, scenario)
		})
	}
	for _, scenario := range []string{"publication-submit-retry", "publication-readback-retry"} {
		t.Run(scenario, func(t *testing.T) { testHeadlessWatchPreparation(t, false, true, 1, scenario) })
	}
	for _, decision := range []string{"skip", "start"} {
		t.Run(decision+"-during-publication-preflight", func(t *testing.T) { testHeadlessWatchPreparation(t, false, true, 1, decision) })
	}
}

func testHeadlessWatchPreparation(t *testing.T, initiallyPending, automaticPublication bool, pullCount int, decisionDuringPublication ...string) {
	scenario := ""
	if len(decisionDuringPublication) > 0 {
		scenario = decisionDuringPublication[0]
	}
	fixture := t.TempDir()
	fixtureGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = fixture
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned source fixture failed: %v %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	fixtureGit("init", "-q", "-b", "main", "--template=")
	if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if scenario == "trusted-project-preferences" {
		if err := os.WriteFile(filepath.Join(fixture, ".reviewd"), []byte("schema_version=1\n[reviewer]\nreasoning_effort=\"medium\"\n[review]\ninstructions=\"Trusted base instructions.\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		fixtureGit("add", ".reviewd")
	}
	fixtureGit("add", "file.go")
	fixtureGit("commit", "-qm", "base")
	base := fixtureGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if scenario == "trusted-project-preferences" {
		if err := os.WriteFile(filepath.Join(fixture, ".reviewd"), []byte("schema_version=1\n[actions]\npublish_review=\"auto\"\n[reviewer]\nmodel=\"untrusted-proposed-model\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		fixtureGit("add", ".reviewd")
	}
	fixtureGit("add", "file.go")
	fixtureGit("commit", "-qm", "head")
	head := fixtureGit("rev-parse", "HEAD")
	middleHead := head
	if scenario == "debounced-burst" {
		if err := os.WriteFile(filepath.Join(fixture, "file.go"), []byte("newest\n"), 0600); err != nil {
			t.Fatal(err)
		}
		fixtureGit("add", "file.go")
		fixtureGit("commit", "-qm", "final burst head")
		head = fixtureGit("rev-parse", "HEAD")
	}
	for number := 1; number <= pullCount; number++ {
		fixtureGit("update-ref", fmt.Sprintf("refs/pull/%d/head", number), head)
	}
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nReview the actual immutable source privately.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requested, member atomic.Bool
	var rerequested atomic.Bool
	var checksPending atomic.Bool
	var remoteReview atomic.Pointer[github.RemoteReview]
	var submissions atomic.Int32
	var boundaryChanged atomic.Bool
	var finalBoundary atomic.Bool
	var throttled atomic.Bool
	var throttledReads atomic.Int32
	var requests atomic.Int32
	var afterPublicationLimit atomic.Int32
	var publicationLimited atomic.Bool
	sharedRetry := strings.Contains(scenario, "shared") || strings.Contains(scenario, "graphql")
	checksPending.Store(initiallyPending)
	pull := func(number int) map[string]any {
		value := lifecyclePull(number, "open", requested.Load(), false)
		value["head"].(map[string]any)["sha"] = head
		if scenario == "debounced-reentry" && !boundaryChanged.Load() {
			value["head"].(map[string]any)["sha"] = base
		}
		if scenario == "debounced-burst" {
			value["commits"] = 2
			if !boundaryChanged.Load() {
				value["head"].(map[string]any)["sha"] = base
				value["commits"], value["changed_files"] = 0, 0
			} else if !finalBoundary.Load() {
				value["head"].(map[string]any)["sha"] = middleHead
				value["commits"] = 1
			}
		}
		value["base"].(map[string]any)["sha"] = base
		if scenario == "trusted-project-preferences" {
			value["changed_files"] = 2
		}
		return value
	}
	events := func() []any {
		if requested.Load() {
			if rerequested.Load() {
				return []any{authorityEvent("NEW_REQUEST", "User", 43), authorityEvent("REREQUEST", "User", 43)}
			}
			return []any{authorityEvent("NEW_REQUEST", "User", 43)}
		}
		return []any{}
	}
	peer := authorityPeer(t, func() map[string]any { return pull(1) }, events, &member)
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if publicationLimited.Load() {
			afterPublicationLimit.Add(1)
		}
		if sharedRetry && throttled.Load() && ((!strings.Contains(scenario, "graphql") && r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1/reviews") || (strings.Contains(scenario, "graphql") && r.URL.Path == "/graphql")) {
			throttledReads.Add(1)
			if strings.Contains(scenario, "graphql200") {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
				fmt.Fprint(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
				return
			}
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls" {
			rows := []any{}
			for number := 1; number <= pullCount; number++ {
				rows = append(rows, pull(number))
			}
			json.NewEncoder(w).Encode(rows)
			return
		}
		if r.URL.Path == "/graphql" {
			json.NewEncoder(w).Encode(lifecycleGraphQL(t, r, func(int) []any { return events() }))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/owner/repo/pulls/") {
			number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/pulls/"))
			if err == nil {
				json.NewEncoder(w).Encode(pull(number))
				return
			}
		}
		if r.URL.Path == "/repos/owner/repo/pulls/1/reviews" {
			if r.Method == http.MethodPost {
				if scenario == "publication-submit-retry" {
					publicationLimited.Store(true)
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if !automaticPublication {
					t.Error("private scheduling attempted a review write")
					w.WriteHeader(500)
					return
				}
				var submission github.ReviewSubmission
				if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				if submission.CommitID != head || submission.Event != "APPROVE" || submission.Body != "**No findings.**" || len(submission.Comments) != 0 {
					t.Error("automatic publication did not submit the exact prepared payload")
				}
				now := time.Now().UTC()
				row := github.RemoteReview{ID: 111, User: github.Identity{ID: 43, Login: "reviewer"}, CommitID: head, State: "APPROVED", Body: submission.Body, SubmittedAt: &now, URL: "https://github.com/owner/repo/pull/1#pullrequestreview-111"}
				remoteReview.Store(&row)
				submissions.Add(1)
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(row)
				return
			}
			rows := []github.RemoteReview{}
			if row := remoteReview.Load(); row != nil {
				rows = append(rows, *row)
			}
			json.NewEncoder(w).Encode(rows)
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls/1/reviews/111" {
			if scenario == "publication-readback-retry" {
				publicationLimited.Store(true)
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			json.NewEncoder(w).Encode(remoteReview.Load())
			return
		}
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == "/graphql") {
			t.Errorf("private scheduling attempted remote publication: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main":
			fmt.Fprint(w, `{"protected":false}`)
		case "/repos/owner/repo/rules/branches/main":
			fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":77}]}}]`)
		case "/repos/owner/repo/commits/" + head + "/check-runs":
			if !sharedRetry && throttled.Load() {
				throttledReads.Add(1)
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			status, conclusion := "completed", `"success"`
			if checksPending.Load() {
				status, conclusion = "in_progress", "null"
			}
			fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":5,"name":"quality","head_sha":%q,"status":%q,"conclusion":%s,"app":{"id":77}}]}`, head, status, conclusion)
		default:
			peer.ServeHTTP(w, r)
		}
	}), automaticPublication)
	configuration, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configuration = append(configuration, []byte("[reviewer]\nskill_path = "+strconv.Quote(skill)+"\n")...)
	if scenario == "trusted-project-preferences" {
		configuration = append(configuration, []byte("[projects.\"owner/repo\".reviewer]\nreasoning_effort=\"low\"\n")...)
	}
	if pullCount > 1 {
		configuration = append(configuration, []byte("[limits]\nmax_concurrent_reviews = 1\n")...)
	}
	if automaticPublication {
		configuration = []byte(strings.Replace(string(configuration), "[actions]", "[actions]\npublish_review = \"auto\"", 1))
	}
	if err := os.WriteFile(path, configuration, 0600); err != nil {
		t.Fatal(err)
	}
	var workers atomic.Int32
	var activeWorkers, peakWorkers atomic.Int32
	var sourceAttempts atomic.Int32
	deps.Source = func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
		attempt := sourceAttempts.Add(1)
		if scenario == "acquisition-retry" && attempt == 1 {
			return nil, errors.New("transient source acquisition failure")
		}
		return source.Acquire(ctx, source.Options{RemoteURL: "file://" + fixture, AllowLocalFixture: true, TempRoot: t.TempDir()}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, ChangedFiles: pull.ChangedFiles, Number: pull.Number})
	}
	deps.Review = func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
		workers.Add(1)
		active := activeWorkers.Add(1)
		defer activeWorkers.Add(-1)
		for peak := peakWorkers.Load(); active > peak; peak = peakWorkers.Load() {
			if peakWorkers.CompareAndSwap(peak, active) {
				break
			}
		}
		expectedEffort := "high"
		if scenario == "trusted-project-preferences" {
			expectedEffort = "low"
			origins := request.Policy.Provenance()
			if request.Policy.String("review.instructions") != "Trusted base instructions." || request.Policy.String("actions.publish_review") != "ask" || origins["review.instructions"].Revision != base || origins["reviewer.reasoning_effort"].Layer != "project" {
				t.Error("automatic work did not preserve trusted base preferences and exact-project precedence/provenance")
			}
		}
		if request.Policy.String("reviewer.model") != "gpt-6.1-sol" || request.Policy.String("reviewer.reasoning_effort") != expectedEffort {
			t.Error("scheduler did not pass the actual model/effort snapshot")
		}
		text, err := request.Evidence.Handle("reviewd_read_source", json.RawMessage(`{"revision":"head","path":"file.go"}`))
		if err != nil || !strings.Contains(text, "new") {
			t.Error("scheduled worker did not receive actual immutable source")
		}
		if scenario == "debounced-burst" && !strings.Contains(text, "newest") {
			t.Error("coalesced worker received an intermediate revision")
		}
		return codex.ReviewOutput{Text: `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`, ThreadID: "watch-thread", TurnID: "watch-turn", RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol"}, nil
	}
	decision := ""
	if scenario == "skip" || scenario == "start" {
		decision = scenario
	}
	if scenario == "publication-submit-retry" || scenario == "publication-readback-retry" {
		ctx := context.Background()
		directory := filepath.Join(filepath.Dir(path), "state")
		durable, err := store.Open(ctx, directory)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { durable.Close() }()
		manager, err := config.NewManager(configuration, path)
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Now()
		makeEngine := func() *watch.Engine {
			return &watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Now: func() time.Time { return clock }, Announce: func(context.Context, *github.Client, *config.Document) error { return nil }, Reviewer: func(client *github.Client) review.Service {
				return review.Service{GitHub: client, Store: durable, SkillPath: skillPath, Source: deps.Source, Worker: deps.Review}
			}}
		}
		engine := makeEngine()
		defer func() { engine.Close() }()
		if err := engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		requested.Store(true)
		if err := engine.Tick(ctx); err != nil || engine.Active() != 1 {
			t.Fatalf("fresh request: %v", err)
		}
		err = engine.Complete(ctx, true)
		if github.RetryAfter(err) < time.Hour || afterPublicationLimit.Load() != 0 {
			t.Fatalf("publication lost throttle or performed immediate readback: retry=%v new_reads=%d error=%v", github.RetryAfter(err), afterPublicationLimit.Load(), err)
		}
		runs, err := durable.List(ctx)
		if err != nil || len(runs) != 1 || runs[0].State != store.PublicationUnknown || workers.Load() != 1 {
			t.Fatal("throttled submission/readback lost ambiguous outcome")
		}
		runID := runs[0].ID
		before := requests.Load()
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		if err := durable.Close(); err != nil {
			t.Fatal(err)
		}
		durable, err = store.Open(ctx, directory)
		if err != nil {
			t.Fatal(err)
		}
		engine = makeEngine()
		clock = clock.Add(59 * time.Minute)
		if err := engine.Tick(ctx); err != nil || requests.Load() != before || engine.Active() != 0 {
			t.Fatal("unknown publication cooldown was lost on restart")
		}
		clock = clock.Add(time.Minute)
		publicationLimited.Store(false)
		if err := engine.Tick(ctx); err != nil || engine.Active() != 0 || workers.Load() != 1 {
			t.Fatalf("unknown publication was automatically retried: %v", err)
		}
		current, err := durable.Get(ctx, runID)
		wantSubmissions := int32(0)
		if scenario == "publication-readback-retry" {
			wantSubmissions = 1
		}
		if err != nil || current.State != store.PublicationUnknown || submissions.Load() != wantSubmissions {
			t.Fatal("unknown publication was reposted or relabeled")
		}
		return
	}
	if strings.HasPrefix(scenario, "prepared-refresh-") || scenario == "publication-preflight-retry" || scenario == "publication-shared-retry" {
		ctx := context.Background()
		directory := filepath.Join(filepath.Dir(path), "state")
		durable, err := store.Open(ctx, directory)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { durable.Close() }()
		manager, err := config.NewManager(configuration, path)
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Now()
		originalConnect := deps.GitHub
		var injected atomic.Bool
		if automaticPublication {
			deps.GitHub = func(ctx context.Context, host string) (*github.Client, error) {
				if workers.Load() > 0 && injected.CompareAndSwap(false, true) {
					throttled.Store(true)
				}
				return originalConnect(ctx, host)
			}
		}
		makeEngine := func() *watch.Engine {
			return &watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Now: func() time.Time { return clock }, Announce: func(context.Context, *github.Client, *config.Document) error { return nil }, Reviewer: func(client *github.Client) review.Service {
				// Inject only after discovery succeeds, at the prepared operation's
				// actual application boundary, not by excluding discovery endpoints.
				if !automaticPublication && workers.Load() > 0 && injected.CompareAndSwap(false, true) {
					throttled.Store(true)
				}
				return review.Service{GitHub: client, Store: durable, SkillPath: skillPath, Source: deps.Source, Worker: deps.Review}
			}}
		}
		engine := makeEngine()
		defer func() { engine.Close() }()
		if err = engine.Tick(ctx); err != nil || engine.Active() != 0 {
			t.Fatalf("initial backlog: %v", err)
		}
		requested.Store(true)
		if err = engine.Tick(ctx); err != nil || engine.Active() != 1 {
			t.Fatalf("fresh request: %v", err)
		}
		err = engine.Complete(ctx, true)
		if !automaticPublication {
			if err != nil {
				t.Fatal(err)
			}
			if err = engine.Tick(ctx); err != nil || engine.Active() != 1 {
				t.Fatalf("prepared refresh: %v", err)
			}
			err = engine.Complete(ctx, true)
		}
		items, _, inboxErr := durable.Inbox(ctx)
		minimumRetry := clock.Add(time.Hour)
		if strings.Contains(scenario, "graphql200") {
			minimumRetry = clock.Add(59 * time.Minute)
		}
		if err == nil || inboxErr != nil || len(items) != 1 || items[0].Attempts != 1 || items[0].RetryAt.Before(minimumRetry) || workers.Load() != 1 || throttledReads.Load() != 1 || submissions.Load() != 0 {
			t.Fatalf("prepared retry deadline was lost: items=%+v reads=%d workers=%d error=%v inbox=%v", items, throttledReads.Load(), workers.Load(), err, inboxErr)
		}
		runID := items[0].RunID
		beforeDeadline := requests.Load()
		if err = engine.Admit(ctx); err != nil || engine.Active() != 0 || requests.Load() != beforeDeadline {
			t.Fatalf("refill bypassed cooldown: active=%d requests=%d want=%d error=%v", engine.Active(), requests.Load(), beforeDeadline, err)
		}
		var localOutput, localErrors bytes.Buffer
		if code := Run(ctx, []string{"--config", path, "inbox", "--json"}, &localOutput, &localErrors, deps); code != 0 || !bytes.Contains(localOutput.Bytes(), []byte(runID)) || requests.Load() != beforeDeadline {
			t.Fatal("local inbox became unavailable or acquired GitHub during cooldown")
		}
		if err = engine.Close(); err != nil {
			t.Fatal(err)
		}
		if err = durable.Close(); err != nil {
			t.Fatal(err)
		}
		durable, err = store.Open(ctx, directory)
		if err != nil {
			t.Fatal(err)
		}
		if err = durable.RecoverScheduled(ctx); err != nil {
			t.Fatal(err)
		}
		engine = makeEngine()
		clock = clock.Add(59 * time.Minute)
		if err = engine.Tick(ctx); err != nil || engine.Active() != 0 || requests.Load() != beforeDeadline || throttledReads.Load() != 1 || workers.Load() != 1 || submissions.Load() != 0 {
			t.Fatalf("reopened watcher retried before deadline: active=%d reads=%d requests=%d want=%d error=%v", engine.Active(), throttledReads.Load(), requests.Load(), beforeDeadline, err)
		}
		clock = clock.Add(time.Minute)
		throttled.Store(false)
		if err = engine.Tick(ctx); err != nil || engine.Active() != 1 {
			t.Fatalf("due prepared retry not admitted: %v", err)
		}
		if err = engine.Complete(ctx, true); err != nil {
			t.Fatal(err)
		}
		runs, err := durable.List(ctx)
		items, _, inboxErr = durable.Inbox(ctx)
		want := store.Prepared
		if automaticPublication {
			want = store.Published
		}
		if err != nil || inboxErr != nil || len(runs) != 1 || runs[0].ID != runID || runs[0].State != want || workers.Load() != 1 || len(items) != 1 || items[0].Attempts != 0 || !items[0].RetryAt.IsZero() || items[0].Failure != "" {
			t.Fatalf("prepared retry replaced evidence or retained failure: runs=%+v items=%+v workers=%d errors=%v %v", runs, items, workers.Load(), err, inboxErr)
		}
		if automaticPublication && submissions.Load() != 1 {
			t.Fatal("due publication did not submit exactly once")
		}
		return
	}
	if scenario == "debounced-reentry" || scenario == "debounced-burst" || scenario == "acquisition-retry" {
		ctx := context.Background()
		durable, err := store.Open(ctx, filepath.Join(filepath.Dir(path), "state"))
		if err != nil {
			t.Fatal(err)
		}
		defer durable.Close()
		manager, err := config.NewManager(configuration, path)
		if err != nil {
			t.Fatal(err)
		}
		clock := time.Now()
		if scenario == "acquisition-retry" {
			clock = clock.Add(-time.Minute)
		}
		engine := watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Now: func() time.Time { return clock }, Reviewer: func(client *github.Client) review.Service {
			return review.Service{GitHub: client, Store: durable, SkillPath: skillPath, Source: deps.Source, Worker: deps.Review}
		}}
		defer engine.Close()
		if scenario == "debounced-reentry" || scenario == "debounced-burst" {
			requested.Store(true)
		}
		if err = engine.Tick(ctx); err != nil || engine.Active() != 0 {
			t.Fatalf("initial work was not held: %v", err)
		}
		requested.Store(true)
		boundaryChanged.Store(true)
		if err = engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if scenario == "debounced-reentry" || scenario == "debounced-burst" {
			if engine.Active() != 0 {
				t.Fatal("code update bypassed debounce")
			}
		} else {
			if engine.Active() != 1 {
				t.Fatal("fresh request was not admitted")
			}
			if err = engine.Complete(ctx, true); err == nil {
				t.Fatal("acquisition failure was concealed")
			}
		}
		items, _, err := durable.Inbox(ctx)
		if err != nil || len(items) != 1 || items[0].Baseline || workers.Load() != 0 {
			t.Fatalf("fresh deferred work lost: %+v %v", items, err)
		}
		if scenario == "debounced-burst" {
			finalBoundary.Store(true)
			if err := engine.Tick(ctx); err != nil || engine.Active() != 0 || workers.Load() != 0 || sourceAttempts.Load() != 0 {
				t.Fatalf("intermediate burst update started source/model work: active=%d sources=%d workers=%d err=%v", engine.Active(), sourceAttempts.Load(), workers.Load(), err)
			}
			items, _, err = durable.Inbox(ctx)
			if err != nil || len(items) != 1 || items[0].HeadSHA != head || items[0].RunID != "" {
				t.Fatalf("burst did not coalesce to its final undispatched boundary: %+v %v", items, err)
			}
		}
		clock = clock.Add(time.Minute)
		if err = engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if engine.Active() != 1 {
			t.Fatalf("unchanged poll stranded deferred automatic work: %+v", items[0])
		}
		if err = engine.Complete(ctx, true); err != nil {
			t.Fatal(err)
		}
		runs, err := durable.List(ctx)
		items, _, inboxErr := durable.Inbox(ctx)
		if err != nil || inboxErr != nil || len(runs) != 1 || runs[0].State != store.Prepared || workers.Load() != 1 || len(items) != 1 || items[0].RunID != runs[0].ID {
			t.Fatalf("deferred work did not prepare exactly one linked review: runs=%+v workers=%d errors=%v %v", runs, workers.Load(), err, inboxErr)
		}
		if err = engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if engine.Active() > 0 {
			if err = engine.Complete(ctx, true); err != nil {
				t.Fatal(err)
			}
		}
		if workers.Load() != 1 {
			t.Fatal("unchanged re-entry repeated paid work")
		}
		if scenario == "debounced-burst" {
			var boundary review.Boundary
			if sourceAttempts.Load() != 1 || json.Unmarshal(runs[0].Context, &boundary) != nil || boundary.Pull.HeadSHA != head || boundary.Pull.CommitCount != 2 {
				t.Fatal("burst acquired more than one snapshot or prepared an intermediate boundary")
			}
		}
		return
	}
	var revoked atomic.Bool
	if decision != "" {
		originalConnect := deps.GitHub
		deps.GitHub = func(ctx context.Context, host string) (*github.Client, error) {
			if workers.Load() > 0 && revoked.CompareAndSwap(false, true) {
				durable, err := store.Open(ctx, filepath.Join(filepath.Dir(path), "state"))
				if err != nil {
					t.Fatal(err)
				}
				defer durable.Close()
				items, _, err := durable.Inbox(ctx)
				if err != nil || len(items) != 1 || items[0].State != "prepared" {
					t.Fatalf("not before publication claim: %v %+v", err, items)
				}
				if _, err = durable.DecideItem(ctx, items[0].ID, decision == "start", false, nil); err != nil {
					t.Fatal(err)
				}
			}
			return originalConnect(ctx, host)
		}
	}
	lastCode := 0
	watchOnce := func() lifecycleInbox {
		t.Helper()
		var output, diagnostics bytes.Buffer
		lastCode = Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps)
		if lastCode != 0 && !(decision != "" && revoked.Load()) {
			t.Fatalf("watch did not process eligible work: %d %s", lastCode, diagnostics.String())
		}
		var result lifecycleInbox
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if baseline := watchOnce(); len(baseline.Runs) != 0 || workers.Load() != 0 {
		t.Fatalf("baseline unexpectedly started work: %+v", baseline)
	}
	requested.Store(true)
	current := watchOnce()
	if decision != "" {
		if !revoked.Load() || lastCode == 0 || submissions.Load() != 0 {
			t.Fatalf("accepted %s did not revoke old automatic publication: code=%d submissions=%d", decision, lastCode, submissions.Load())
		}
		want := 1
		if decision == "start" {
			want = 2
		}
		if len(current.Runs) != want || workers.Load() != int32(want) {
			t.Fatalf("replacement work lost: runs=%d workers=%d", len(current.Runs), workers.Load())
		}
		for _, raw := range current.Runs {
			var run store.Run
			if json.Unmarshal(raw, &run) != nil || run.State != store.Prepared || len(run.Outcome) != 0 {
				t.Fatalf("revoked publication advanced: %+v", run)
			}
		}
		if repeated := watchOnce(); len(repeated.Runs) != want || workers.Load() != int32(want) || submissions.Load() != 0 {
			t.Fatal("restart resurrected revoked automatic publication")
		}
		return
	}
	if initiallyPending {
		if len(current.Runs) != 1 || workers.Load() != 0 {
			t.Fatalf("pending checks started paid work: %+v workers=%d", current, workers.Load())
		}
		var waiting store.Run
		if err := json.Unmarshal(current.Runs[0], &waiting); err != nil || waiting.State != store.WaitingChecks {
			t.Fatalf("pending work was not durable: %+v %v", waiting, err)
		}
		if repeated := watchOnce(); len(repeated.Runs) != 1 || workers.Load() != 0 {
			t.Fatalf("pending checks duplicated paid work: %+v", repeated)
		}
		configuration = append(configuration, []byte("reasoning_effort = \"low\"\n")...)
		if err := os.WriteFile(path, configuration, 0600); err != nil {
			t.Fatal(err)
		}
		changeLifecycleConfig(t, path, `review_requested = "auto"`, `review_requested = "ask"`)
		checksPending.Store(false)
		current = watchOnce()
		var resumed store.Run
		if len(current.Runs) != 1 || json.Unmarshal(current.Runs[0], &resumed) != nil || resumed.ID != waiting.ID {
			t.Fatalf("settled checks replaced the frozen run: %+v", current)
		}
	}
	if len(current.Runs) != pullCount || workers.Load() != int32(pullCount) {
		t.Fatalf("new request did not produce exactly one review: %+v workers=%d", current, workers.Load())
	}
	if pullCount > 1 {
		if peakWorkers.Load() != 1 {
			t.Fatalf("concurrency cap exceeded: %d", peakWorkers.Load())
		}
		var details struct {
			Items []store.InboxItem
			Runs  []store.Run
		}
		var output, diagnostics bytes.Buffer
		if code := Run(context.Background(), []string{"--config", path, "inbox", "--json"}, &output, &diagnostics, deps); code != 0 {
			t.Fatalf("inbox: %d %s", code, diagnostics.String())
		}
		if err := json.Unmarshal(output.Bytes(), &details); err != nil {
			t.Fatal(err)
		}
		for _, item := range details.Items {
			matched := false
			for _, run := range details.Runs {
				if item.RunID == run.ID && item.PullID == run.PullID && item.State == "prepared" {
					matched = true
				}
			}
			if !matched {
				t.Fatalf("item lost its corresponding prepared run: %+v", item)
			}
		}
		if repeated := watchOnce(); len(repeated.Runs) != pullCount || workers.Load() != int32(pullCount) {
			t.Fatalf("multi-item restart repeated work: %d workers=%d", len(repeated.Runs), workers.Load())
		}
		return
	}
	var run store.Run
	if err := json.Unmarshal(current.Runs[0], &run); err != nil {
		t.Fatal(err)
	}
	if automaticPublication {
		var outcome review.PublicationOutcome
		if run.State != store.Published || json.Unmarshal(run.Outcome, &outcome) != nil || !outcome.Verified || outcome.Review.ID != 111 || submissions.Load() != 1 {
			t.Fatalf("automatic review did not verify its remote outcome: state=%s submissions=%d", run.State, submissions.Load())
		}
	} else if run.State != store.Prepared || run.ThreadID != "watch-thread" || len(run.Prepared) == 0 || len(run.Outcome) != 0 {
		t.Fatalf("private preparation confused with publication: %+v", run)
	}
	if repeated := watchOnce(); len(repeated.Runs) != 1 || workers.Load() != 1 {
		t.Fatalf("restarted watcher duplicated work: %+v workers=%d", repeated, workers.Load())
	}
	if scenario == "same-head-rerequest" {
		rerequested.Store(true)
		second := watchOnce()
		if len(second.Runs) != 2 || workers.Load() != 2 {
			t.Fatalf("same-head explicit re-request was not dispatched once: %+v workers=%d", second, workers.Load())
		}
		previousFound, freshFound := false, false
		for _, raw := range second.Runs {
			var candidate store.Run
			var boundary review.Boundary
			if json.Unmarshal(raw, &candidate) != nil || json.Unmarshal(candidate.Context, &boundary) != nil || candidate.State != store.Prepared || boundary.Pull.HeadSHA != head || boundary.Pull.TargetBaseSHA != base {
				t.Fatal("same-head request changed its source boundary or failed to prepare")
			}
			if candidate.ID == run.ID {
				previousFound = candidate.PayloadID == run.PayloadID && candidate.WorkKey == run.WorkKey
			} else {
				freshFound = candidate.WorkKey != run.WorkKey
			}
		}
		if !previousFound || !freshFound {
			t.Fatal("re-request replaced prior evidence instead of creating distinct event work")
		}
		if repeated := watchOnce(); len(repeated.Runs) != 2 || workers.Load() != 2 || submissions.Load() != 0 {
			t.Fatal("repeated re-request observation duplicated work or granted publication")
		}
		return
	}
	if automaticPublication {
		if submissions.Load() != 1 {
			t.Fatal("restarted watcher reposted its own review")
		}
		return
	}
	checksPending.Store(true)
	changed := watchOnce()
	var refreshed store.Run
	if len(changed.Runs) != 1 || json.Unmarshal(changed.Runs[0], &refreshed) != nil || refreshed.ID != run.ID || refreshed.PayloadID == run.PayloadID || workers.Load() != 1 || !strings.Contains(string(refreshed.Prepared), "**Not ready.**") {
		t.Fatalf("check-only change did not reuse and refresh the source review: state=%s workers=%d", refreshed.State, workers.Load())
	}
	checksPending.Store(false)
	restored := watchOnce()
	if len(restored.Runs) != 1 || json.Unmarshal(restored.Runs[0], &refreshed) != nil || refreshed.PayloadID != run.PayloadID || workers.Load() != 1 {
		t.Fatal("settled CI reran the model or failed to refresh the same source result")
	}
}

func TestStartAndSkipKeepExactPendingDecisionsWithoutAWatcher(t *testing.T) {
	var member atomic.Bool
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("INITIAL", "User", 43)} }, &member))
	initial := invoke("sync")
	if len(initial.Items) != 1 || !initial.Items[0].Baseline {
		t.Fatalf("initial work was not held: %+v", initial)
	}
	id := initial.Items[0].ID
	var output, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"--config", path, "start", id, "--now", "--reasoning-effort", "low", "--json"}, &output, &diagnostics, deps)
	if code != 0 {
		t.Fatalf("explicit start choice was not retained: %d %s", code, diagnostics.String())
	}
	var result struct {
		Items []store.InboxItem
		Runs  []store.Run
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].State != "queued" || !result.Items[0].Manual || result.Items[0].Baseline || !result.Items[0].ReviewNow || result.Items[0].DecisionID == "" || result.Items[0].Overrides["reviewer.reasoning_effort"] != "low" || len(result.Runs) != 0 {
		t.Fatalf("choice scope was lost or started without a watcher: %+v", result)
	}
	if persisted := invoke("inbox"); len(persisted.Items) != 1 || persisted.Items[0].State != "queued" {
		t.Fatalf("choice expired without a terminal: %+v", persisted)
	}
	output.Reset()
	diagnostics.Reset()
	if code := Run(context.Background(), []string{"--config", path, "skip", id, "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("skip did not persist: %d %s", code, diagnostics.String())
	}
	if persisted := invoke("sync"); len(persisted.Items) != 0 {
		t.Fatalf("repeat discovery revived skipped work: %+v", persisted)
	}
}
