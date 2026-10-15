package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
)

func TestExecutionCaptureIncludesUndeclaredWorktreeChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := captureExecutionTree(root, []string{"known.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "undeclared.txt"), []byte("must reach independent review\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := captureExecutionTree(root, []string{"known.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if after.ID == before.ID || after.Sources["new/undeclared.txt"] != "must reach independent review\n" {
		t.Fatal("undeclared new source disappeared from current-tree evidence")
	}
}

func TestExecutionCaptureBindsIndexOnlyChanges(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("owned foreground Git fixture: %v %s", err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q", "-b", "main", "--template=")
	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("unchanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "known.txt")
	before, err := captureExecutionTree(root, []string{"known.txt"})
	if err != nil {
		t.Fatal(err)
	}
	git("update-index", "--chmod=+x", "known.txt")
	after, err := captureExecutionTree(root, []string{"known.txt"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "known.txt"))
	if err != nil || string(data) != "unchanged\n" {
		t.Fatal("index-only fixture changed worktree content", err)
	}
	info, err := os.Stat(filepath.Join(root, "known.txt"))
	if err != nil || info.Mode().Perm()&0111 != 0 {
		t.Fatal("index-only fixture changed worktree mode", err)
	}
	if before.ID == after.ID {
		t.Fatal("index-only mutation retained current-tree approval identity")
	}
}

func executionSkill(t *testing.T) *review.CapturedSkill {
	return executionSkillWithReview(t, "requesting-code-review")
}

func TestExecutionPlanningIsInputNotApprovalOfAnUnfixedBug(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("known defect\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := config.Resolve(nil, "github.com/owner/repo", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			commands := 0
			broker, err := NewExecution(ExecutionOptions{Policy: policy, Skill: executionSkill(t), Workspace: root,
				Snapshot: &source.Snapshot{Sources: map[string]map[string]string{"head": {"source.txt": "known defect\n"}}, Manifests: map[string][]source.Entry{"head": {{Path: "source.txt", Mode: "100644", Type: "blob"}}}},
				Inspect: func(context.Context) (GitInspection, error) {
					return GitInspection{Head: "original", Branch: "refs/heads/existing"}, nil
				},
				Command: func(context.Context, []string, time.Duration, bool) (codex.CommandResult, error) {
					commands++
					return codex.CommandResult{}, nil
				}, Quiesce: func() error { return nil }, Save: func(ExecutionJournal) error { return nil },
				Agent: func(_ context.Context, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
					blockers := []map[string]string{}
					if blocked && request.Role == "qa_test_planner" {
						blockers = append(blockers, map[string]string{"kind": "invalid_design", "reason": "The requested deletion removes the only supported user data without a replacement."})
					}
					body, _ := json.Marshal(map[string]any{"summary": "One bounded preimplementation pass completed", "findings": []string{"The original defect remains present; verify its regression before the fix and its correction afterward. This is implementation work, not a reason to reject planning."}, "blockers": blockers})
					return codex.ReviewOutput{ThreadID: "planning-" + request.Role, TurnID: "turn-" + request.Role, RequestedModel: request.Model, EffectiveModel: request.Model, Text: string(body)}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"change_mapper", "qa_test_planner"} {
				if _, err := broker.Handle(ctx, "reviewd_feedback_delegate", json.RawMessage(`{"role":"`+role+`"}`)); err != nil {
					t.Fatal("bounded planning was treated as review approval", role, err)
				}
			}
			_, err = broker.Handle(ctx, "reviewd_feedback_command", json.RawMessage(`{"argv":["owned-edit"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`))
			if blocked {
				if err == nil || commands != 0 {
					t.Fatal("a concrete invalid design did not block writing")
				}
			} else if err != nil || commands != 1 {
				t.Fatal("expected unfixed bug prevented its own implementation", err)
			}
			if broker.DeliveryReady() {
				t.Fatal("planning reports certified unimplemented, unverified delivery")
			}
		})
	}
}

func executionSkillWithReview(t *testing.T, reviewName string) *review.CapturedSkill {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"handle-pr-feedback", "qa-test-planner", "independent-engineering-workflow", reviewName} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: owned workflow\n---\nRead-only bounded role.\n"
		if name == "handle-pr-feedback" {
			body += "[workflow](../independent-engineering-workflow/SKILL.md)\n[review](../" + reviewName + "/SKILL.md)\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	skill, err := review.CaptureFeedbackSkill("handle-pr-feedback", filepath.Join(root, "handle-pr-feedback", "SKILL.md"), filepath.Join(root, "qa-test-planner", "SKILL.md"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := skill.Close(); err != nil {
			t.Error(err)
		}
	})
	return skill
}

func TestExecutionBrokerRequiresNativeRolesAndInvalidatesChangedTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := config.Resolve(nil, "github.com/owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	roleCalls := []string{}
	saved := 0
	broker, err := NewExecution(ExecutionOptions{Environment: CommandEnvironment{Backend: "container", OS: "linux", Architecture: "arm64", Workspace: "/workspace", Image: "sha256:" + strings.Repeat("a", 64)}, Inspect: func(context.Context) (GitInspection, error) {
		return GitInspection{Head: "fixture", Branch: "refs/heads/fixture"}, nil
	}, Policy: policy, Skill: executionSkillWithReview(t, "review"), Workspace: dir,
		Snapshot:  &source.Snapshot{Sources: map[string]map[string]string{"head": {"source.txt": "before\n"}}, Manifests: map[string][]source.Entry{"head": {{Path: "source.txt", Mode: "100644", Type: "blob"}}}},
		Documents: map[string]string{"requirements": "Fix this exact PR only"},
		Command: func(_ context.Context, args []string, _ time.Duration, writable bool) (codex.CommandResult, error) {
			commands++
			if writable && args[0] == "edit" {
				if err := os.WriteFile(path, []byte("after\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return codex.CommandResult{Stdout: "owned output"}, nil
		},
		Quiesce: func() error { return nil },
		Agent: func(_ context.Context, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
			roleCalls = append(roleCalls, request.Role)
			view, err := request.Handle(ctx, "reviewd_feedback_read_evidence", json.RawMessage(`{"key":"execution-environment"}`))
			if err != nil || !strings.Contains(view, "linux") || !strings.Contains(view, "/workspace") {
				t.Error("independent role did not receive actual guest environment", err)
			}
			if request.Participant != "specialist" || request.OnStatus != nil || len(request.Tools) != 3 {
				t.Error("leaf inherited primary capabilities or status identity")
			}
			for _, tool := range request.Tools {
				if strings.Contains(tool["name"].(string), "command") || strings.Contains(tool["name"].(string), "delegate") {
					t.Error("independent leaf can modify or recursively delegate")
				}
			}
			if strings.Contains(request.Prompt, "parent implementation claim") {
				t.Error("primary task contaminated blind independent role")
			}
			if request.Role == "qa_test_planner" && request.SkillName != "qa-test-planner" {
				t.Error("QA did not invoke actual captured QA skill")
			}
			body := `{"disposition":"APPROVE","summary":"Owned native report","findings":["The inspected consumer and executed assertion cover the requested contract; no material finding remains."]}`
			if request.Role == "change_mapper" || request.Role == "qa_test_planner" {
				body = `{"summary":"Bounded implementation input","findings":["Implement the requested behavior and run its focused regression"],"blockers":[]}`
			}
			return codex.ReviewOutput{ThreadID: "thread-" + request.Role, TurnID: "turn-" + request.Role, RequestedModel: "gpt-6.1-sol", EffectiveModel: "gpt-6.1-sol", Text: body}, nil
		}, Save: func(j ExecutionJournal) error { saved++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(name, args string) error {
		_, err := broker.Handle(ctx, "reviewd_feedback_"+name, json.RawMessage(args))
		return err
	}
	if call("command", `{"argv":["edit"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`) == nil || commands != 0 {
		t.Fatal("write ran before independent mapping/QA")
	}
	if call("delegate", `{"role":"correctness_reviewer"}`) == nil {
		t.Fatal("final review ran before verification/regression")
	}
	for _, role := range []string{"change_mapper", "qa_test_planner"} {
		if err := call("delegate", `{"role":"`+role+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := call("command", `{"argv":["edit"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`); err != nil {
		t.Fatal(err)
	}
	if call("delegate", `{"role":"regression_reviewer"}`) == nil {
		t.Fatal("regression certified unverified edits")
	}
	if err := call("command", `{"argv":["verify"],"timeout_seconds":10,"access":"verify"}`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"regression_reviewer", "correctness_reviewer", "delivery_auditor"} {
		if err := call("delegate", `{"role":"`+role+`"}`); err != nil {
			t.Fatal(role, err)
		}
	}
	if len(broker.Journal().Roles) != 5 || saved < 5 || len(roleCalls) != 5 {
		t.Fatal("native reports were not retained")
	}
	if err := call("command", `{"argv":["edit"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`); err != nil {
		t.Fatal(err)
	}
	if broker.DeliveryReady() {
		t.Fatal("a later writable command retained prior final-diff approval")
	}
	if call("delegate", `{"role":"qa_test_planner"}`) == nil {
		t.Fatal("one bounded QA pass became an amendment series")
	}
	if call("delegate", `{"role":"correctness_reviewer","thread_id":"forged"}`) == nil {
		t.Fatal("parent supplied independent execution identity")
	}
	if call("publish", `{}`) == nil {
		t.Fatal("an unconnected publisher claimed delivery")
	}
}

func TestExecutionCaptureDoesNotFollowSymlinksAndBindsBinaryAndModes(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("owned-secret-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0, 255, 1}, 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := captureExecutionTree(root, []string{"link", "binary"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(func() string { data, _ := json.Marshal(tree.Sources); return string(data) }(), "owned-secret-canary") {
		t.Fatal("capture followed a repository symlink")
	}
	if len(tree.Gaps) != 1 || tree.ID == "" {
		t.Fatal("binary evidence gap or tree identity missing")
	}
	if err := os.Chmod(filepath.Join(root, "binary"), 0700); err != nil {
		t.Fatal(err)
	}
	changed, err := captureExecutionTree(root, []string{"link", "binary"})
	if err != nil || changed.ID == tree.ID {
		t.Fatal("executable mode not bound to tree")
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := captureExecutionTree(root, []string{"escape/secret"}); err == nil {
		t.Fatal("capture accepted a parent symlink")
	}
}

func TestExecutionFailedChecksAndNativeFindingsCannotAuthorizeDelivery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := config.Resolve(nil, "github.com/owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reportCalls := 0
	finding := true
	mutateTest := false
	failTest := true
	commandFailure := false
	deliveries := 0
	failSave := false
	broker, err := NewExecution(ExecutionOptions{Inspect: func(context.Context) (GitInspection, error) {
		return GitInspection{Head: "fixture", Branch: "refs/heads/fixture"}, nil
	}, Policy: policy, Skill: executionSkill(t), Workspace: dir, Snapshot: &source.Snapshot{Sources: map[string]map[string]string{"head": {"source.txt": "before"}}, Manifests: map[string][]source.Entry{"head": {{Path: "source.txt", Mode: "100644", Type: "blob"}}}}, Quiesce: func() error { return nil }, Save: func(ExecutionJournal) error {
		if failSave {
			return errors.New("owned journal unavailable")
		}
		return nil
	}, Command: func(_ context.Context, args []string, _ time.Duration, writable bool) (codex.CommandResult, error) {
		if commandFailure {
			return codex.CommandResult{}, errors.New("owned verifier transport failed")
		}
		if mutateTest {
			if err := os.WriteFile(path, []byte("modified by test"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if failTest {
			return codex.CommandResult{ExitCode: 9}, nil
		}
		return codex.CommandResult{}, nil
	}, Deliver: func(context.Context, DeliveryProposal, string, []ItemDisposition, func(DeliveryJournal) error) (DeliveryJournal, error) {
		deliveries++
		return DeliveryJournal{}, nil
	}, Agent: func(_ context.Context, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
		reportCalls++
		body := `{"disposition":"APPROVE","summary":"owned report","findings":[]}`
		if request.Role == "change_mapper" || request.Role == "qa_test_planner" {
			body = `{"summary":"Bounded input","findings":[],"blockers":[]}`
		}
		if request.Role == "regression_reviewer" && finding {
			body = `{"disposition":"REQUEST_CHANGES","summary":"Concrete incorrect behavior","findings":["Owned defect"]}`
		}
		return codex.ReviewOutput{ThreadID: fmt.Sprintf("thread-%d", reportCalls), TurnID: fmt.Sprintf("turn-%d", reportCalls), RequestedModel: request.Model, EffectiveModel: request.Model, Text: body}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(name, args string) error {
		_, err := broker.Handle(ctx, "reviewd_feedback_"+name, json.RawMessage(args))
		return err
	}
	for _, role := range []string{"change_mapper", "qa_test_planner"} {
		if err := call("delegate", `{"role":"`+role+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	verify := `{"argv":["owned-check"],"timeout_seconds":10,"access":"verify"}`
	if err := call("command", verify); err != nil {
		t.Fatal(err)
	}
	if call("delegate", `{"role":"regression_reviewer"}`) == nil {
		t.Fatal("a failed command became passing verification")
	}
	failTest = false
	mutateTest = true
	if call("command", verify) == nil {
		t.Fatal("a source-mutating check certified its new tree")
	}
	mutateTest = false
	if err := call("command", verify); err != nil {
		t.Fatal(err)
	}
	if err := call("delegate", `{"role":"regression_reviewer"}`); err != nil {
		t.Fatal(err)
	}
	if call("delegate", `{"role":"correctness_reviewer"}`) == nil || broker.DeliveryReady() {
		t.Fatal("a concrete independent finding was ignored")
	}
	finding = false
	if err := call("delegate", `{"role":"regression_reviewer"}`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"correctness_reviewer", "delivery_auditor"} {
		if err := call("delegate", `{"role":"`+role+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if !broker.DeliveryReady() {
		t.Fatal("actual matching role/check facts were lost")
	}
	for _, kind := range []string{"nonzero", "command-error"} {
		failTest = kind == "nonzero"
		commandFailure = kind == "command-error"
		_ = call("command", verify)
		if broker.DeliveryReady() {
			t.Fatal("later failed verification retained an earlier success", kind)
		}
		if call("deliver", `{"commit_message":"fix: preserve behavior","title":"fix: preserve behavior","body":"Preserve the accepted contract.","replies":[]}`) == nil || deliveries != 0 {
			t.Fatal("failed current verification reached delivery", kind)
		}
		failTest = false
		commandFailure = false
		if err := call("command", verify); err != nil || !broker.DeliveryReady() {
			t.Fatal("same check's successful retry was not recognized", err)
		}
	}
	other := `{"argv":["another-required-check"],"timeout_seconds":10,"access":"verify"}`
	failTest = true
	_ = call("command", other)
	failTest = false
	if err := call("command", verify); err != nil {
		t.Fatal(err)
	}
	if broker.DeliveryReady() {
		t.Fatal("a different passing check erased a still-failed required check")
	}
	if err := call("command", other); err != nil || !broker.DeliveryReady() {
		t.Fatal("all distinct checks did not recover after their own successful retry", err)
	}
	failSave = true
	if call("command", `{"argv":["edit"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`) == nil {
		t.Fatal("unrecordable command was launched")
	}
	if broker.DeliveryReady() {
		t.Fatal("lost journal retained delivery capability")
	}
}

func TestExecutionDispositionIsScopedToCapturedHumanRevision(t *testing.T) {
	policy, err := config.Resolve(nil, "github.com/owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewExecution(ExecutionOptions{Inspect: func(context.Context) (GitInspection, error) {
		return GitInspection{Head: "fixture", Branch: "refs/heads/fixture"}, nil
	}, Policy: policy, Skill: executionSkill(t), Workspace: t.TempDir(), Snapshot: &source.Snapshot{}, Inventory: Inventory{Items: []Item{{Key: "inline:719", Revision: "actual-human-revision", ThreadID: "original-thread"}}}, Command: func(context.Context, []string, time.Duration, bool) (codex.CommandResult, error) {
		t.Fatal("disposition ran a command")
		return codex.CommandResult{}, nil
	}, Quiesce: func() error { return nil }, Agent: func(context.Context, codex.FeedbackRequest) (codex.ReviewOutput, error) {
		t.Fatal("disposition spawned an agent")
		return codex.ReviewOutput{}, nil
	}, Save: func(ExecutionJournal) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"key":"foreign:9","revision":"actual-human-revision","status":"accepted","evidence":"verified source"}`, `{"key":"inline:719","revision":"edited-human-revision","status":"accepted","evidence":"verified source"}`, `{"key":"inline:719","revision":"actual-human-revision","status":"resolved","evidence":"verified source"}`, `{"key":"inline:719","revision":"actual-human-revision","status":"accepted","evidence":""}`} {
		if _, err := broker.Handle(context.Background(), "reviewd_feedback_disposition", json.RawMessage(args)); err == nil {
			t.Fatal("unscoped/stale/unproved disposition accepted")
		}
	}
	for _, status := range []string{"accepted", "already_addressed", "rejected", "deferred", "needs_clarification"} {
		args, _ := json.Marshal(map[string]string{"key": "inline:719", "revision": "actual-human-revision", "status": status, "evidence": "Concrete source-backed reason"})
		if _, err := broker.Handle(context.Background(), "reviewd_feedback_disposition", args); err != nil {
			t.Fatal(err)
		}
		journal := broker.Journal()
		if len(journal.Dispositions) != 1 || journal.Dispositions[0].Status != status || journal.Dispositions[0].ThreadID != "original-thread" {
			t.Fatal("per-item disposition lost its original identity/reason")
		}
	}
	if broker.DeliveryReady() {
		t.Fatal("recording a disposition manufactured verification/delivery")
	}
}
