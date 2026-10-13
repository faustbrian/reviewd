package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestManualPrivateJourneyPersistsActualFrozenInputsAndNeverPublishes(t *testing.T) {
	fixture := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = fixture
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + fixture, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned source fixture failed: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main", "--template=")
	os.WriteFile(filepath.Join(fixture, "file.go"), []byte("old\n"), 0600)
	os.WriteFile(filepath.Join(fixture, ".reviewd"), []byte("schema_version = 1\n[reviewer]\nreasoning_effort = \"high\"\n"), 0600)
	git("add", "file.go", ".reviewd")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(fixture, "file.go"), []byte("new\n"), 0600)
	git("add", "file.go")
	git("commit", "-qm", "head")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/17/head", head)
	posts := 0
	var checkFailed atomic.Bool
	var legacyPending atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path != "/graphql" {
			posts++
			t.Error("private preparation attempted publication")
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":43,"login":"reviewer"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":99,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/pulls/17":
			fmt.Fprintf(w, `{"id":700,"number":17,"state":"open","draft":false,"title":"Exact source","body":"Required caller parity","updated_at":"2026-10-13T14:00:00Z","user":{"id":42,"login":"author"},"base":{"sha":%q,"ref":"main","repo":{"id":99,"full_name":"owner/repo"}},"head":{"sha":%q,"ref":"feature"},"changed_files":1,"commits":1}`, base, head)
		case "/repos/owner/repo/branches/main":
			fmt.Fprint(w, `{"protected":false}`)
		case "/repos/owner/repo/rules/branches/main":
			if legacyPending.Load() {
				fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":0}]}}]`)
				return
			}
			fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":77}]}}]`)
		case "/repos/owner/repo/commits/" + head + "/check-runs":
			if legacyPending.Load() {
				fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				return
			}
			conclusion := "success"
			if checkFailed.Load() {
				conclusion = "failure"
			}
			fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":5,"name":"quality","head_sha":%q,"status":"completed","conclusion":%q,"app":{"id":77}}]}`, head, conclusion)
		case "/repos/owner/repo/commits/" + head + "/statuses":
			if legacyPending.Load() {
				fmt.Fprint(w, `[{"id":8,"context":"quality","state":"pending"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/graphql":
			var input struct {
				Query string `json:"query"`
			}
			json.NewDecoder(r.Body).Decode(&input)
			field := "reviewThreads"
			if strings.Contains(input.Query, "closingIssuesReferences") {
				field = "closingIssuesReferences"
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"databaseId":700,"number":17,%q:{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`, field)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer peer.Close()
	client, _ := github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nUnique actual installed instruction.\n"), 0600)
	document, _ := config.Parse([]byte("schema_version = 1\n[actions]\npublish_review = \"auto\"\n"), "owned global config")
	workerCalls := 0
	service := Service{GitHub: client, Store: s, SkillPath: func(config.Snapshot) (string, error) { return skill, nil }, Source: func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
		return source.Acquire(ctx, source.Options{RemoteURL: "file://" + fixture, AllowLocalFixture: true, TempRoot: t.TempDir()}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, ChangedFiles: pull.ChangedFiles, Number: pull.Number})
	}, Worker: func(ctx context.Context, request WorkerRequest) (codex.ReviewOutput, error) {
		workerCalls++
		if request.Policy.String("reviewer.model") != "gpt-6.1-sol" || request.Policy.String("reviewer.reasoning_effort") != "high" || request.Skill.Identity.Digest == "" || !strings.Contains(request.Prompt, "Required caller parity") {
			t.Error("actual frozen run policy/skill/requirements missing")
		}
		text, err := request.Evidence.Handle("reviewd_read_source", json.RawMessage(`{"revision":"target","path":".reviewd"}`))
		if err != nil || !strings.Contains(text, "schema_version") {
			t.Error("trusted target evidence did not reach worker")
		}
		return codex.ReviewOutput{Text: `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`, ThreadID: "thread-fixture", TurnID: "turn-fixture", RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol"}, nil
	}}
	ref, _ := github.ParsePullURL("github.com", "https://github.com/owner/repo/pull/17")
	run, err := service.Start(context.Background(), document, ref, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.Prepared || !run.Private || workerCalls != 1 || posts != 0 {
		t.Fatalf("private journey confused prepared and published: %+v", run)
	}
	var result PreparedResult
	if json.Unmarshal(run.Prepared, &result) != nil || result.Body != "**No findings.**" || result.Boundary.Configuration.Provenance()["reviewer.reasoning_effort"].Revision != base {
		t.Fatal("durable exact preview or target-base policy identity lost")
	}
	if err := ValidatePrepared(result); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.BeginPublication(context.Background(), run.ID, run.PayloadID); err != nil || claimed {
		t.Error("project auto policy bypassed --private")
	}
	originalWorker := service.Worker
	service.Worker = func(ctx context.Context, request WorkerRequest) (codex.ReviewOutput, error) {
		output, err := originalWorker(ctx, request)
		checkFailed.Store(true)
		return output, err
	}
	changed, err := service.Start(context.Background(), document, ref, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	var current PreparedResult
	if json.Unmarshal(changed.Prepared, &current) != nil || current.Event != "REQUEST_CHANGES" || !strings.HasPrefix(current.Body, "**Not ready.**") || workerCalls != 2 {
		t.Error("check-only change became an old clean preview or triggered another full paid source pass")
	}
	legacyPending.Store(true)
	service.Worker = originalWorker
	waiting, err := service.Start(context.Background(), document, ref, nil, true, false)
	if err != nil || waiting.State != store.WaitingChecks || workerCalls != 2 {
		t.Fatalf("pending legacy status did not wait without a paid turn: %v %+v calls=%d", err, waiting, workerCalls)
	}
	immediate, err := service.Start(context.Background(), document, ref, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(immediate.Prepared, &current) != nil || current.Event != "REQUEST_CHANGES" || !strings.HasPrefix(current.Body, "**Not ready.**") || workerCalls != 3 {
		t.Fatal("review-now lost pending legacy readiness gap")
	}
	legacyPending.Store(false)
	checkFailed.Store(true)
	service.Worker = func(ctx context.Context, request WorkerRequest) (codex.ReviewOutput, error) {
		output, err := originalWorker(ctx, request)
		output.Text = `{"conclusion":"REQUEST_CHANGES","summary":"","findings":[],"gaps":[{"kind":"readiness","detail":"Required quality failed at capture.","check":"quality"}]}`
		checkFailed.Store(false)
		return output, err
	}
	settled, err := service.Start(context.Background(), document, ref, nil, true, true)
	if err != nil {
		t.Fatalf("check-only refresh could not reconcile identified model observation: %v", err)
	}
	if json.Unmarshal(settled.Prepared, &current) != nil || !current.Boundary.Checks.Ready || current.Event != "APPROVE" || current.Body != "**No findings.**" || workerCalls != 4 {
		t.Fatalf("settled CI retained obsolete readiness or reran source review: event=%s body=%q calls=%d", current.Event, current.Body, workerCalls)
	}
	if err := s.DecidePublication(context.Background(), settled.ID, settled.PayloadID); err != nil {
		t.Fatal(err)
	}
	checkFailed.Store(true)
	refreshed, err := service.RefreshChecks(context.Background(), settled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(refreshed.Prepared, &current) != nil || current.Event != "REQUEST_CHANGES" || workerCalls != 4 || refreshed.PayloadID == settled.PayloadID {
		t.Fatal("durable checks refresh lost source evidence or paid for another review")
	}
	if _, claimed, err := s.BeginPublication(context.Background(), refreshed.ID, refreshed.PayloadID); err != nil || claimed {
		t.Fatal("old publication decision authorized changed preview")
	}
	checkFailed.Store(false)
	restored, err := service.RefreshChecks(context.Background(), settled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(restored.Prepared, &current) != nil || current.Event != "APPROVE" || workerCalls != 4 {
		t.Fatal("settled durable checks could not reuse valid source review")
	}
	if _, claimed, err := s.BeginPublication(context.Background(), restored.ID, restored.PayloadID); err != nil || claimed {
		t.Fatal("returning to an earlier payload resurrected revoked consent")
	}
	checkFailed.Store(true)
	service.Worker = func(ctx context.Context, request WorkerRequest) (codex.ReviewOutput, error) {
		output, err := originalWorker(ctx, request)
		output.Text = `{"conclusion":"INCONCLUSIVE","summary":"External caller compatibility remains unverified.","findings":[],"gaps":[{"kind":"readiness","detail":"Quality failed at capture.","check":"quality"}]}`
		return output, err
	}
	uncertain, err := service.Start(context.Background(), document, ref, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	checkFailed.Store(false)
	refreshedUncertain, err := service.RefreshChecks(context.Background(), uncertain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(refreshedUncertain.Prepared, &current) != nil || current.Disposition != "INCONCLUSIVE" || current.Event != "COMMENT" || !strings.HasPrefix(current.Body, "**Inconclusive.**") || workerCalls != 5 || refreshedUncertain.ThreadID != uncertain.ThreadID || refreshedUncertain.TurnID != uncertain.TurnID {
		t.Fatalf("durable refresh erased source uncertainty or execution identity: disposition=%s event=%s body=%q calls=%d", current.Disposition, current.Event, current.Body, workerCalls)
	}

}
