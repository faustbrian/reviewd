package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

type feedbackServiceRemote struct {
	target        github.FeedbackTarget
	context       github.ReviewContext
	actor         github.Identity
	ready         bool
	requests      github.FeedbackRequests
	push          func(context.Context, *source.Snapshot, string, string, string, []byte) error
	replyMode     string
	resolveMode   string
	replyCalls    int
	metadataCalls int
	requestCalls  int
}

func (f *feedbackServiceRemote) Identity(context.Context) (github.Identity, error) {
	return f.actor, nil
}
func (f *feedbackServiceRemote) FeedbackTarget(context.Context, string, int) (github.FeedbackTarget, error) {
	return f.target, nil
}
func (f *feedbackServiceRemote) Context(context.Context, github.Pull) (github.ReviewContext, error) {
	return f.context, nil
}
func (f *feedbackServiceRemote) Checks(_ context.Context, pull github.Pull) (github.CheckReport, error) {
	return github.CheckReport{HeadSHA: pull.HeadSHA, Ready: f.ready, Checks: []github.MandatoryCheck{}}, nil
}
func (f *feedbackServiceRemote) PushFeedbackCommit(ctx context.Context, target github.FeedbackTarget, snapshot *source.Snapshot, workspace, commit, tree string, raw []byte) (github.FeedbackTarget, error) {
	if f.push == nil {
		return github.FeedbackTarget{}, errors.New("owned peer has no push capability")
	}
	if err := f.push(ctx, snapshot, workspace, commit, tree, raw); err != nil {
		return github.FeedbackTarget{}, err
	}
	f.target.Pull.HeadSHA = commit
	f.target.Pull.CommitCount++
	return f.target, nil
}
func (f *feedbackServiceRemote) UpdateFeedbackMetadata(_ context.Context, _ github.Pull, title, body string) error {
	f.metadataCalls++
	f.target.Pull.Title = title
	f.target.Pull.Body = body
	return nil
}
func (f *feedbackServiceRemote) ReplyToFeedbackDiscussion(_ context.Context, _ github.Pull, body string) (github.FeedbackReply, error) {
	f.replyCalls++
	if f.replyMode == "unknown" {
		return github.FeedbackReply{}, errors.New("owned lost response without canonical confirmation")
	}
	reply := github.FeedbackReply{ID: 900 + int64(len(f.context.Conversation)), Body: body, User: f.actor, URL: f.target.Pull.URL + "#issuecomment-owned"}
	raw, _ := json.Marshal(reply)
	f.context.Conversation = append(f.context.Conversation, raw)
	if f.replyMode == "lost" {
		return github.FeedbackReply{}, errors.New("owned response lost after canonical acceptance")
	}
	return reply, nil
}
func (f *feedbackServiceRemote) ReplyToFeedbackThread(_ context.Context, _ github.Pull, threadID, body string) (github.FeedbackReply, error) {
	for i := range f.context.Threads {
		thread := &f.context.Threads[i]
		if thread.ID != threadID || thread.Comments.Nodes == nil {
			continue
		}
		id := 900 + int64(len(*thread.Comments.Nodes))
		reply := github.FeedbackReply{ID: id, NodeID: fmt.Sprintf("owned-reply-%d", id), Body: body, User: f.actor, URL: f.target.Pull.URL + "#discussion-owned"}
		raw, _ := json.Marshal(map[string]any{"id": reply.NodeID, "databaseId": id, "body": body, "author": map[string]string{"login": f.actor.Login}})
		*thread.Comments.Nodes = append(*thread.Comments.Nodes, raw)
		if f.replyMode == "thread-new-human" {
			*thread.Comments.Nodes = append(*thread.Comments.Nodes, json.RawMessage(`{"id":"owned-new-human","databaseId":721,"body":"The new review concern must remain open","author":{"login":"reviewer"}}`))
		}
		return reply, nil
	}
	return github.FeedbackReply{}, errors.New("owned original thread unavailable")
}
func (f *feedbackServiceRemote) ResolveFeedbackThread(_ context.Context, _ github.Pull, id string) error {
	if f.resolveMode == "unknown" {
		return errors.New("owned lost resolution without canonical acceptance")
	}
	for i := range f.context.Threads {
		if f.context.Threads[i].ID == id {
			resolved := true
			f.context.Threads[i].IsResolved = &resolved
			if f.resolveMode == "lost" {
				return errors.New("owned lost response after canonical resolution")
			}
			return nil
		}
	}
	return errors.New("owned original thread unavailable")
}
func (f *feedbackServiceRemote) FeedbackReviewRequests(context.Context, github.Pull) (github.FeedbackRequests, error) {
	return f.requests, nil
}
func (f *feedbackServiceRemote) RequestFeedbackReviewer(_ context.Context, _ github.Pull, login, team string) error {
	f.requestCalls++
	if login != "" {
		f.requests.Users = append(f.requests.Users, github.Identity{ID: 8, Login: login})
	}
	if team != "" {
		f.requests.Teams = append(f.requests.Teams, github.RequestedTeam{ID: 33, Slug: team})
	}
	return nil
}

func feedbackServiceFixture(t *testing.T) (*feedbackServiceRemote, *store.Store, config.Snapshot) {
	t.Helper()
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { durable.Close() })
	policy, err := config.Resolve(nil, "github.com/owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := &feedbackServiceRemote{actor: github.Identity{ID: 43, Login: "author"}, target: github.FeedbackTarget{HeadRepository: "github.com/owner/repo", HeadRepositoryID: 99, Pull: github.Pull{PullReference: github.PullReference{Repository: "github.com/owner/repo", Number: 17, URL: "https://github.com/owner/repo/pull/17"}, ID: 700, RepositoryID: 99, State: "open", HeadRef: "feature/existing", BaseRef: "main", Author: github.Identity{ID: 43, Login: "author"}, TargetBaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}}, context: github.ReviewContext{Reviews: []json.RawMessage{}, Comments: []json.RawMessage{}, Conversation: []json.RawMessage{}, Threads: []github.ReviewThread{}, Gaps: []string{}}}
	return remote, durable, policy
}

func TestFeedbackServiceRequiresNewScopedConsentAndIndependentAuthorPolicy(t *testing.T) {
	remote, durable, _ := feedbackServiceFixture(t)
	ctx := context.Background()
	service := Service{GitHub: remote, Store: durable, ModifierDirectory: filepath.Join(t.TempDir(), "shared")}
	for _, kind := range []string{"", "private_review", "own_pr_feedback", "automatic_feedback_v1"} {
		if _, err := service.Start(ctx, nil, remote.target.Pull.PullReference, nil, Authorization{Kind: kind}); err == nil {
			t.Fatal("legacy/default consent started a feedback lane", kind)
		}
	}
	runs, err := durable.List(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatal("denied consent created work", err)
	}
	remote.target.Pull.Author = github.Identity{ID: 9, Login: "other"}
	if _, err := service.Start(ctx, nil, remote.target.Pull.PullReference, nil, Authorization{Kind: "manual_feedback_v1"}); err == nil {
		t.Fatal("feedback ignored PR author filter")
	}
}

func TestFeedbackServiceEmptyInventoryRecordsNoopWithoutAnAgentOrSource(t *testing.T) {
	remote, durable, _ := feedbackServiceFixture(t)
	ctx := context.Background()
	service := Service{GitHub: remote, Store: durable, ModifierDirectory: filepath.Join(t.TempDir(), "shared")}
	run, err := service.Start(ctx, nil, remote.target.Pull.PullReference, nil, Authorization{Kind: "manual_feedback_v1"})
	if err != nil || run.Purpose != "feedback" || run.State != store.FeedbackDelivered || !run.OwnerClosed {
		t.Fatalf("empty feedback outcome: %+v %v", run, err)
	}
	var journal Journal
	if err := json.Unmarshal(run.Outcome, &journal); err != nil || !journal.Noop || len(journal.Execution.Roles) != 0 || len(journal.Execution.Commands) != 0 {
		t.Fatal("no-op manufactured work", err)
	}
}

func TestFeedbackServiceRetainsActualSkillAndFreshModelButCannotInventDelivery(t *testing.T) {
	exerciseFeedbackService(t, false)
}

func TestFeedbackServiceDeliversAcceptedFixEndToEnd(t *testing.T) {
	exerciseFeedbackService(t, false, true)
}

func TestFeedbackServiceExplicitReconciliationContinuesAfterCurrentChecks(t *testing.T) {
	for _, suffix := range []string{"ready", "new-head", "new-human", "local-work", "changed-projection"} {
		t.Run(suffix, func(t *testing.T) { exerciseFeedbackServiceScenario(t, false, true, "resume-checks-"+suffix) })
	}
}

func TestFeedbackServiceExplicitReconciliationNeverRepostsAnUnknownReply(t *testing.T) {
	for _, suffix := range []string{"unknown", "accepted"} {
		t.Run(suffix, func(t *testing.T) { exerciseFeedbackServiceScenario(t, false, true, "resume-reply-"+suffix) })
	}
}

func TestFeedbackServiceMixedPartialReconcilesWithoutRepostingSettledReplies(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "resume-mixed-partial")
}

func TestFeedbackServiceAllHandledResumeFinishesRetainedDeliveryWithoutReplay(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "resume-finalization")
}

func TestFeedbackServiceSetupFailuresResumeInTheOriginalOwner(t *testing.T) {
	for _, stage := range []string{"source", "prepare", "checkpoint"} {
		t.Run(stage, func(t *testing.T) { exerciseFeedbackServiceScenario(t, false, true, "resume-setup-"+stage) })
	}
}

func TestFeedbackServiceUnstagedProposalCanBeCorrectedBeforeAnyEffect(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "unstaged-proposal")
}

func TestFeedbackServiceExplicitResumeKeepsTheStagedFixAndOriginalOwner(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "resume-staged-fix")
}

func TestFeedbackServiceOriginalThreadResolutionUsesCanonicalState(t *testing.T) {
	for _, scenario := range []string{"thread-settled", "thread-resolution-lost", "thread-resolution-unknown", "thread-new-human"} {
		t.Run(scenario, func(t *testing.T) { exerciseFeedbackServiceScenario(t, false, true, scenario) })
	}
}

func TestFeedbackServiceAlreadyAddressedAnswersWithoutAnEmptyCommit(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "already-addressed")
}

func TestFeedbackServiceNewHumanRevisionDoesNotReplayHandledFeedback(t *testing.T) {
	exerciseFeedbackServiceScenario(t, false, true, "new-human-after-delivery")
}

func TestFeedbackServiceCanonicalEffectsCannotBeInventedOrReplayed(t *testing.T) {
	for _, scenario := range []string{"checks-pending", "reply-unknown", "reply-lost-response", "concurrent-human", "post-audit-fails"} {
		t.Run(scenario, func(t *testing.T) { exerciseFeedbackServiceScenario(t, false, true, scenario) })
	}
}

// Physical native Linux commands run here; the model/remote peers are
// deliberately owned fixtures, not live LLM/GitHub delivery. macOS command
// execution needs separately enabled owned-container/paired-package acceptance;
// the Linux capability flag must never request native fallback on macOS.
func TestCodexCapabilityFeedbackService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux service capability; macOS requires separate owned-container installed-workflow acceptance")
	}
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for the native feedback service journey")
	}
	exerciseFeedbackService(t, true)
}
func exerciseFeedbackService(t *testing.T, native bool, full ...bool) {
	complete := len(full) > 0 && full[0]
	exerciseFeedbackServiceScenario(t, native, complete, "")
}
func exerciseFeedbackServiceScenario(t *testing.T, native, complete bool, scenario string) {
	delivered := complete && (scenario == "" || scenario == "reply-lost-response" || scenario == "new-human-after-delivery" || scenario == "already-addressed" || scenario == "thread-settled" || scenario == "thread-resolution-lost" || scenario == "resume-staged-fix" || scenario == "unstaged-proposal" || scenario == "resume-checks-ready" || scenario == "resume-reply-accepted" || scenario == "resume-finalization" || strings.HasPrefix(scenario, "resume-setup-"))
	remote, durable, _ := feedbackServiceFixture(t)
	ctx := context.Background()
	var checkpointDB *sql.DB
	if scenario == "resume-setup-checkpoint" {
		var err error
		checkpointDB, err = sql.Open("sqlite", "file:"+filepath.Join(durable.Directory(), "reviewd.sqlite")+"?mode=rw")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { checkpointDB.Close() })
		// Preserve only the durable prefix at first retention by rejecting every
		// later journal write. This is inert SQLite fault injection, not an OS
		// crash, interruption or process-lifetime experiment.
		_, err = checkpointDB.ExecContext(ctx, `
CREATE TABLE owned_checkpoint_fault(active INTEGER NOT NULL);
INSERT INTO owned_checkpoint_fault VALUES(0);
CREATE TRIGGER owned_arm_checkpoint_fault AFTER UPDATE OF processes ON private_owners
WHEN EXISTS(SELECT 1 FROM json_each(NEW.processes) WHERE json_extract(CASE WHEN json_valid(value) THEN value ELSE '{}' END,'$.kind')='feedback_workspace')
AND NOT EXISTS(SELECT 1 FROM json_each(OLD.processes) WHERE json_extract(CASE WHEN json_valid(value) THEN value ELSE '{}' END,'$.kind')='feedback_workspace')
BEGIN UPDATE owned_checkpoint_fault SET active=1; END;
CREATE TRIGGER owned_reject_checkpoint BEFORE UPDATE OF outcome ON runs
WHEN (SELECT active FROM owned_checkpoint_fault)=1
BEGIN SELECT RAISE(ABORT,'owned post-retention persistence failure'); END;`)
		if err != nil {
			t.Fatal(err)
		}
	}
	skill := executionSkill(t)
	remote.context.Reviews = []json.RawMessage{json.RawMessage(`{"id":719,"user":{"id":8,"login":"reviewer"},"body":"Fix the bounded source behavior","state":"CHANGES_REQUESTED"}`)}
	if scenario == "resume-mixed-partial" {
		remote.context.Reviews = append(remote.context.Reviews, json.RawMessage(`{"id":720,"user":{"id":8,"login":"reviewer"},"body":"A separate concern must remain deferred","state":"COMMENTED"}`))
	}
	if strings.HasPrefix(scenario, "thread-") {
		remote.context.Reviews = []json.RawMessage{json.RawMessage(`{"id":719,"user":{"id":8,"login":"reviewer"},"body":"","state":"CHANGES_REQUESTED"}`)}
		var thread github.ReviewThread
		if e := json.Unmarshal([]byte(`{"id":"owned-thread","isResolved":false,"isOutdated":false,"path":"source.txt","comments":{"nodes":[{"id":"owned-comment","databaseId":719,"body":"Fix the bounded source behavior","author":{"login":"reviewer"}}]}}`), &thread); e != nil {
			t.Fatal(e)
		}
		remote.context.Threads = []github.ReviewThread{thread}
		if scenario == "thread-resolution-unknown" {
			remote.resolveMode = "unknown"
		}
		if scenario == "thread-resolution-lost" {
			remote.resolveMode = "lost"
		}
		if scenario == "thread-new-human" {
			remote.replyMode = "thread-new-human"
		}
	}
	original := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = original
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + original, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=f@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=f@example.invalid"}
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(data))
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q", "-b", "main", "--template=")
	if err := os.WriteFile(filepath.Join(original, "source.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-qb", "feature/existing")
	current := []byte("current\n")
	if scenario == "already-addressed" {
		current = []byte("fixed\n")
	}
	if err := os.WriteFile(filepath.Join(original, "source.txt"), current, 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "head")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/17/head", head)
	remote.target.Pull.HeadSHA = head
	remote.target.Pull.TargetBaseSHA = base
	remote.target.Pull.ChangedFiles = 1
	remote.target.Pull.CommitCount = 1
	bare := ""
	pushCalls := 0
	if complete {
		bare = filepath.Join(t.TempDir(), "remote.git")
		git("clone", "-q", "--bare", "--no-hardlinks", original, bare)
		remote.ready = true
		if scenario == "checks-pending" || strings.HasPrefix(scenario, "resume-checks-") {
			remote.ready = false
		}
		if scenario == "reply-unknown" || strings.HasPrefix(scenario, "resume-reply-") {
			remote.replyMode = "unknown"
		}
		if scenario == "reply-lost-response" {
			remote.replyMode = "lost"
		}
		remote.push = func(ctx context.Context, snapshot *source.Snapshot, workspace, commit, tree string, raw []byte) error {
			pushCalls++
			if scenario == "already-addressed" {
				t.Error("already-addressed feedback attempted an empty commit/push")
				return errors.New("no code changed")
			}
			pack, err := snapshot.FeedbackPushPack(ctx, workspace, commit, tree, raw)
			if err != nil {
				return err
			}
			command := exec.CommandContext(ctx, "git", "--git-dir="+bare, "index-pack", "--stdin", "--strict")
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + bare, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
			command.Stdin = strings.NewReader(string(pack))
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("owned remote object import: %w %s", err, output)
			}
			git("--git-dir="+bare, "update-ref", "refs/heads/feature/existing", commit, head)
			git("--git-dir="+bare, "update-ref", "refs/pull/17/head", commit)
			if git("--git-dir="+bare, "show", commit+":source.txt") != "fixed" {
				return errors.New("remote fixture content differs")
			}
			if scenario == "concurrent-human" {
				remote.context.Reviews = append(remote.context.Reviews, json.RawMessage(`{"id":720,"user":{"id":9,"login":"other-reviewer"},"body":"A newly submitted human requirement","state":"COMMENTED"}`))
			}
			return nil
		}
	}
	modifierDirectory := filepath.Join(t.TempDir(), "shared")

	if scenario == "new-human-after-delivery" {
		oldInventory, e := Collect(remote.context, remote.actor)
		if e != nil {
			t.Fatal(e)
		}
		previousOwner, e := durable.BeginPrivateOwner(ctx)
		if e != nil {
			t.Fatal(e)
		}
		prior, _, e := durable.Create(ctx, store.Run{Purpose: "feedback", OwnerID: previousOwner.ID, Repository: remote.target.Pull.Repository, RepositoryID: remote.target.Pull.RepositoryID, PullID: remote.target.Pull.ID, Number: 17, ActorID: remote.actor.ID, ActorLogin: remote.actor.Login, WorkKey: "previous-delivered", State: store.Queued, Configuration: json.RawMessage(`{}`)})
		if e != nil {
			t.Fatal(e)
		}
		if claimed, e := durable.ClaimFeedback(ctx, prior.ID); e != nil || !claimed {
			t.Fatal(e)
		}
		modifier, e := AcquireModifier(ctx, modifierDirectory, ModifierKey{Host: "github.com", RepositoryID: 99, PullID: 700})
		if e != nil {
			t.Fatal(e)
		}
		if e := modifier.Attach(ctx, durable, prior.ID); e != nil {
			t.Fatal(e)
		}
		if e := modifier.Close(); e != nil {
			t.Fatal(e)
		}
		old := oldInventory.Items[0]
		data, _ := json.Marshal(Journal{Version: 1, Target: remote.target, Handled: []HandledRevision{{Key: old.Key, Revision: old.Revision}}})
		if e := durable.FinishFeedback(ctx, prior.ID, store.FeedbackDelivered, data, ""); e != nil {
			t.Fatal(e)
		}
		if e := previousOwner.Close(); e != nil {
			t.Fatal(e)
		}
		remote.context.Reviews = append(remote.context.Reviews, json.RawMessage(`{"id":720,"user":{"id":8,"login":"reviewer"},"body":"Fix the bounded source behavior from this newer human review only","state":"CHANGES_REQUESTED"}`))
	}
	calls := 0
	roleCalls := 0
	closed := false
	finalizationFailed := false
	workspaceCommands := 0
	sourceCalls, prepareCalls := 0, 0
	workspaceDirectory := ""
	selectedSkillPath := ""
	service := Service{GitHub: remote, Store: durable, ModifierDirectory: modifierDirectory,
		CaptureGitPolicy: func(context.Context, string) (CommitPolicy, error) {
			return CommitPolicy{Name: "Fixture", Email: "fixture@example.invalid"}, nil
		},
		CaptureSkill: func(_ config.Snapshot, parent string) (*review.CapturedSkill, error) {
			captured, err := review.CaptureFeedbackSkill("handle-pr-feedback", skill.Path, filepath.Join(filepath.Dir(filepath.Dir(skill.Path)), "qa-test-planner", "SKILL.md"), parent)
			if err == nil {
				selectedSkillPath = captured.Path
			}
			return captured, err
		},
		Source: func(ctx context.Context, pull github.Pull, parent string) (*source.Snapshot, error) {
			sourceCalls++
			if (scenario == "resume-setup-source" || scenario == "resume-setup-checkpoint") && sourceCalls == 1 {
				return nil, errors.New("owned transient immutable source acquisition failure")
			}
			remotePath := original
			if pull.HeadSHA != head {
				remotePath = bare
			}
			return source.Acquire(ctx, source.Options{RemoteURL: "file://" + remotePath, AllowLocalFixture: true, TempRoot: parent}, source.Boundary{Number: 17, HeadSHA: pull.HeadSHA, TargetBaseSHA: pull.TargetBaseSHA, ChangedFiles: pull.ChangedFiles})
		},
		Prepare: func(ctx context.Context, owner *store.PrivateOwner, snapshot *source.Snapshot, branch string, options WorkspaceOptions) (*ExecutionResources, error) {
			prepareCalls++
			if scenario == "resume-setup-prepare" && prepareCalls == 1 {
				return nil, errors.New("owned transient workspace prerequisite failure")
			}
			if complete {
				retained := options.retainedDirectory
				files := &source.FeedbackWorkspace{Directory: retained}
				if retained == "" {
					var err error
					files, err = snapshot.PrepareFeedbackWorkspace(ctx, owner.Root, branch)
					if err != nil {
						return nil, err
					}
				}

				host := inspectionGitHost{files.Directory}
				checkout := [][]string{}
				if retained == "" {
					checkout = [][]string{{"git", "read-tree", snapshot.HeadSHA}, {"git", "checkout-index", "--all"}}
				}
				for _, args := range checkout {
					result, err := host.Run(ctx, args, time.Minute)
					if err != nil || result.ExitCode != 0 {
						return nil, fmt.Errorf("owned full-journey checkout: %v", err)
					}
				}
				workspaceDirectory = files.Directory
				return &ExecutionResources{Directory: files.Directory, Environment: CommandEnvironment{Backend: "owned-test-adapter", OS: "fixture", Workspace: files.Directory}, Inspect: func(ctx context.Context) (GitInspection, error) {
					expectedHead := snapshot.HeadSHA
					if options.inspectionHead != "" {
						expectedHead = options.inspectionHead
					}
					return inspectGitCommandsAtHead(ctx, host, "git", snapshot, branch, expectedHead)
				}, Command: func(ctx context.Context, args []string, timeout time.Duration, _ bool) (codex.CommandResult, error) {
					workspaceCommands++
					return host.Run(ctx, args, timeout)
				}, Quiesce: func() error {
					closed = true
					if scenario == "resume-finalization" && roleCalls == 6 && !finalizationFailed {
						finalizationFailed = true
						return errors.New("owned transient finalization failure after successful post-delivery audit")
					}
					return nil
				}}, nil
			}
			dir := filepath.Join(owner.Root, "reviewd-feedback-workspace-owned-fixture")
			workspaceDirectory = dir
			if err := os.Mkdir(dir, 0700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("current\n"), 0600); err != nil {
				return nil, err
			}
			return &ExecutionResources{Directory: dir, Inspect: func(context.Context) (GitInspection, error) {
				return GitInspection{Head: snapshot.HeadSHA, Branch: "refs/heads/" + branch}, nil
			}, Command: func(context.Context, []string, time.Duration, bool) (codex.CommandResult, error) {
				return codex.CommandResult{}, errors.New("fixture grants no commands")
			}, Quiesce: func() error { closed = true; return nil }}, nil
		},
		Agent: func(ctx context.Context, options codex.RuntimeOptions, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
			if request.Role != "" {
				roleCalls++
				if request.OnStatus != nil || request.Participant != "specialist" || len(request.Tools) != 3 {
					t.Fatal("independent role inherited modifying/primary capability")
				}
				if _, err := request.Handle(ctx, "reviewd_feedback_command", json.RawMessage(`{"argv":["/bin/false"]}`)); err == nil {
					t.Fatal("read-only independent role gained command access")
				}
				text := `{"disposition":"APPROVE","summary":"Owned peer native report","findings":[]}`
				if request.Role == "change_mapper" || request.Role == "qa_test_planner" {
					text = `{"summary":"Bounded input","findings":["Verify the requested source behavior"],"blockers":[]}`
				}
				if scenario == "post-audit-fails" && roleCalls == 6 {
					text = `{"disposition":"REQUEST_CHANGES","summary":"Canonical delivery still has an owned unresolved gap","findings":["The owned final delivery requirement is not proven."]}`
				}
				return codex.ReviewOutput{ThreadID: fmt.Sprintf("owned-role-%d", roleCalls), TurnID: fmt.Sprintf("owned-role-turn-%d", roleCalls), RequestedModel: request.Model, EffectiveModel: request.Model, Text: text}, nil
			}
			calls++
			primaryThread := "actual-primary"
			if scenario == "resume-staged-fix" && calls > 1 {
				primaryThread = "actual-primary-resumed"
			}
			if request.Model != "gpt-6.1-sol" || request.Effort != "high" || request.SkillName != "handle-pr-feedback" || (request.SkillPath != selectedSkillPath && !(scenario == "resume-staged-fix" && calls > 1) && !strings.HasPrefix(scenario, "resume-setup-")) || request.Role != "" {
				t.Error("fresh primary uses wrong skill/model/role")
			}
			if err := request.OnStatus(codex.ReviewOutput{ThreadID: primaryThread, TurnID: "actual-turn", RequestedModel: request.Model, EffectiveModel: request.Model, Progress: "reviewing"}); err != nil {
				t.Fatal(err)
			}
			text, err := request.Handle(ctx, "reviewd_feedback_read_evidence", json.RawMessage(`{"key":"feedback-inventory"}`))
			if err != nil || !strings.Contains(text, "bounded source behavior") {
				t.Error("captured substantive feedback did not reach the actual instruction broker", err)
			}
			if native || complete {
				call := func(name, args string) {
					t.Helper()
					text, err := request.Handle(ctx, "reviewd_feedback_"+name, json.RawMessage(args))
					if err != nil {
						t.Fatal(name, err)
					}
					if name == "command" {
						var result codex.CommandResult
						if json.Unmarshal([]byte(text), &result) != nil || result.ExitCode != 0 {
							t.Fatal("native command failed", text)
						}
					}
				}
				if !(scenario == "resume-staged-fix" && calls > 1) {
					if _, err := request.Handle(ctx, "reviewd_feedback_command", json.RawMessage(`{"argv":["/bin/sh","-c","printf 'forbidden\\n' > source.txt"],"timeout_seconds":10,"access":"write","paths":["source.txt"]}`)); err == nil {
						t.Fatal("native service ran an edit before independent mapping/QA")
					}
				}
				if native {
					denied, err := request.Handle(ctx, "reviewd_feedback_command", json.RawMessage(`{"argv":["/bin/sh","-c","printf forbidden > source.txt"],"timeout_seconds":20,"access":"read"}`))
					var deniedResult codex.CommandResult
					if err != nil || json.Unmarshal([]byte(denied), &deniedResult) != nil || deniedResult.ExitCode == 0 {
						t.Fatal("read command did not enforce native read-only workspace", denied, err)
					}
				}
				if calls == 1 || scenario != "resume-staged-fix" {
					for _, role := range []string{"change_mapper", "qa_test_planner"} {
						call("delegate", `{"role":"`+role+`"}`)
					}
				}
				if scenario != "already-addressed" && !(scenario == "resume-staged-fix" && calls > 1) {
					call("command", `{"argv":["/bin/sh","-c","printf 'fixed\\n' > source.txt"],"timeout_seconds":20,"access":"write","paths":["source.txt"]}`)
				}
				if complete && scenario != "already-addressed" && scenario != "unstaged-proposal" && !(scenario == "resume-staged-fix" && calls > 1) {
					call("command", `{"argv":["git","add","--","source.txt"],"timeout_seconds":20,"access":"write","paths":["source.txt"]}`)
				}
				if scenario == "resume-staged-fix" && calls == 1 {
					return codex.ReviewOutput{ThreadID: primaryThread, TurnID: "actual-turn", RequestedModel: request.Model, EffectiveModel: request.Model, Text: "The staged fix is retained; explicitly resume this same run."}, nil
				}
				call("command", `{"argv":["/bin/sh","-c","test \"$(cat source.txt)\" = fixed && test -z \"$GH_TOKEN$SSH_AUTH_SOCK$OPENAI_API_KEY$CODEX_HOME\""],"timeout_seconds":20,"access":"verify"}`)
				for _, role := range []string{"regression_reviewer", "correctness_reviewer", "delivery_auditor"} {
					call("delegate", `{"role":"`+role+`"}`)
				}
				if complete {
					inventory, err := Collect(remote.context, remote.actor)
					if err != nil {
						t.Fatal(err)
					}
					item := inventory.Items[0]
					if scenario == "new-human-after-delivery" {
						item = inventory.Items[1]
					}
					decisionStatus := "accepted"
					if scenario == "already-addressed" {
						decisionStatus = "already_addressed"
					}
					decision, _ := json.Marshal(map[string]string{"key": item.Key, "revision": item.Revision, "status": decisionStatus, "evidence": "The owned source now returns the requested fixed result; unchanged-tree verification and all native roles completed."})
					call("disposition", string(decision))
					proposal, _ := json.Marshal(map[string]any{"commit_message": "fix: preserve the requested source behavior\n\nApply the accepted feedback to the existing PR.\n", "title": "fix: preserve the requested source behavior", "body": "Preserve the source behavior requested in review.", "replies": []map[string]any{{"key": item.Key, "body": "The requested behavior is fixed and verified.", "resolve": strings.HasPrefix(scenario, "thread-")}}})
					if scenario == "resume-mixed-partial" {
						second := inventory.Items[1]
						decision, _ := json.Marshal(map[string]string{"key": second.Key, "revision": second.Revision, "status": "deferred", "evidence": "The separate concern remains outstanding; no speculative implementation is authorized by its current evidence."})
						call("disposition", string(decision))
						var expanded DeliveryProposal
						_ = json.Unmarshal(proposal, &expanded)
						expanded.Replies = append(expanded.Replies, DeliveryReply{Key: second.Key, Body: "This concern remains deferred pending its missing evidence."})
						proposal, _ = json.Marshal(expanded)
					}
					if scenario == "unstaged-proposal" {
						if _, e := request.Handle(ctx, "reviewd_feedback_deliver", proposal); e == nil {
							t.Fatal("unstaged source was published")
						}
						call("command", `{"argv":["git","add","--","source.txt"],"timeout_seconds":20,"access":"write","paths":["source.txt"]}`)
						call("command", `{"argv":["/bin/sh","-c","test \"$(cat source.txt)\" = fixed"],"timeout_seconds":20,"access":"verify"}`)
						for _, role := range []string{"regression_reviewer", "correctness_reviewer", "delivery_auditor"} {
							call("delegate", `{"role":"`+role+`"}`)
						}
					}
					if scenario == "checks-pending" || strings.HasPrefix(scenario, "resume-checks-") || scenario == "reply-unknown" || strings.HasPrefix(scenario, "resume-reply-") || scenario == "concurrent-human" || scenario == "thread-resolution-unknown" || scenario == "thread-new-human" {
						if _, err := request.Handle(ctx, "reviewd_feedback_deliver", proposal); err == nil {
							t.Fatal("unverified delivery became success")
						}
						if _, err := request.Handle(ctx, "reviewd_feedback_deliver", proposal); err == nil {
							t.Fatal("an attempted delivery was replayed")
						}
					} else {
						call("deliver", string(proposal))
					}
				}
			}
			return codex.ReviewOutput{ThreadID: primaryThread, TurnID: "actual-turn", RequestedModel: request.Model, EffectiveModel: request.Model, Text: "All done, pushed and resolved!"}, nil
		},
	}
	if native {
		service.Prepare = func(ctx context.Context, owner *store.PrivateOwner, snapshot *source.Snapshot, branch string, options WorkspaceOptions) (*ExecutionResources, error) {
			resources, err := prepareExecutionResources(ctx, owner, snapshot, branch, options)
			if err != nil {
				return nil, err
			}
			workspaceDirectory = resources.Directory
			originalClose := resources.Quiesce
			resources.Quiesce = func() error {
				err := originalClose()
				if err == nil {
					closed = true
				}
				return err
			}
			return resources, nil
		}
	}
	run, err := service.Start(ctx, nil, remote.target.Pull.PullReference, nil, Authorization{Kind: "manual_feedback_v1"})
	if err != nil && scenario != "concurrent-human" && scenario != "thread-new-human" && scenario != "resume-finalization" && !strings.HasPrefix(scenario, "resume-setup-") {
		t.Fatal(err)
	}
	if strings.HasPrefix(scenario, "resume-setup-") {
		if checkpointDB != nil {
			if _, e := checkpointDB.ExecContext(ctx, "DROP TRIGGER owned_reject_checkpoint; DROP TRIGGER owned_arm_checkpoint_fault;"); e != nil {
				t.Fatal(e)
			}
			if _, e := durable.RecoverPrivate(ctx, false); e != nil {
				t.Fatal(e)
			}
			var e error
			run, e = durable.Get(ctx, run.ID)
			if e != nil {
				t.Fatal(e)
			}
		}
		originalRun := run
		var before Journal
		if err == nil || json.Unmarshal(run.Outcome, &before) != nil || run.State != store.FeedbackPending || run.OwnerClosed || calls != 0 || roleCalls != 0 || remote.replyCalls != 0 || pushCalls != 0 {
			t.Fatal("setup failure ran implementation or lost the original pending owner", err)
		}
		if ResumeConfirmation(run) == "" || before.Retained == nil || before.Retained.Skill.Name == "" || before.Retained.Workspace.Name != "" {
			t.Fatal("pending setup cannot preview explicit continuation of its captured instructions")
		}
		if scenario == "resume-setup-prepare" {
			if before.Retained.Source.Name == "" {
				t.Fatal("acquired immutable source was not checkpointed before preparation")
			}
			service.Source = nil // The acquired immutable source must be reopened, not replaced.
		}
		service.CaptureSkill = nil // Continue the actual frozen instructions, not the installed tree.
		run, err = service.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1", Confirmation: ResumeConfirmation(run)})
		if err != nil {
			t.Fatal("transient setup could not continue the original run", err)
		}
		var after Journal
		if json.Unmarshal(run.Outcome, &after) != nil || run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID || run.SkillDigest != originalRun.SkillDigest || run.State != store.FeedbackDelivered || !run.OwnerClosed || after.Noop || after.Retained == nil || (scenario == "resume-setup-prepare" && after.Retained.Source != before.Retained.Source) {
			t.Fatal("setup continuation replaced the run, owner, frozen instructions or adopted source")
		}
		if ((scenario == "resume-setup-source" || scenario == "resume-setup-checkpoint") && (sourceCalls != 2 || prepareCalls != 1)) || (scenario == "resume-setup-prepare" && (sourceCalls != 1 || prepareCalls != 2)) {
			t.Fatal("setup continuation did not finish only its missing acquisition/preparation", sourceCalls, prepareCalls)
		}
	}
	if scenario == "resume-finalization" {
		originalRun := run
		var before Journal
		if err == nil || !finalizationFailed || json.Unmarshal(run.Outcome, &before) != nil || run.State != store.FeedbackPending || run.OwnerClosed || !before.Execution.Delivery.Complete || len(before.Handled) != 1 || remote.replyCalls != 1 || roleCalls != 6 {
			t.Fatal("fixture did not interrupt finalization after verified effects and handling", err)
		}
		commandsBefore := workspaceCommands
		mutationsBefore := []int{pushCalls, remote.metadataCalls, remote.replyCalls, remote.requestCalls}
		run, err = service.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1"})
		if err != nil {
			t.Fatal("all-handled retained delivery could not finish its original run", err)
		}
		var after Journal
		if json.Unmarshal(run.Outcome, &after) != nil || run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID || run.State != store.FeedbackDelivered || !run.OwnerClosed || after.Noop || calls != 1 || roleCalls != 7 || workspaceCommands != commandsBefore || fingerprint([]int{pushCalls, remote.metadataCalls, remote.replyCalls, remote.requestCalls}) != fingerprint(mutationsBefore) || len(after.Execution.Delivery.Effects) != len(before.Execution.Delivery.Effects) || after.Execution.Delivery.Commit.OID != before.Execution.Delivery.Commit.OID || fingerprint(after.Handled) != fingerprint(before.Handled) {
			t.Fatal("finalization replaced work, replayed commands/mutations, changed handled facts or omitted fresh audit/closure")
		}
		for i, effect := range after.Execution.Delivery.Effects {
			prior := before.Execution.Delivery.Effects[i]
			if effect.Kind != prior.Kind || effect.Key != prior.Key || effect.State != "verified" {
				t.Fatal("finalization replaced a recorded effect identity or lost confirmation")
			}
		}
		if _, err := os.Stat(workspaceDirectory); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("delivered retained workspace was not cleaned", err)
		}
	}
	if scenario == "resume-mixed-partial" {
		originalRun := run
		var before Journal
		if json.Unmarshal(run.Outcome, &before) != nil || run.State != store.Partial || run.OwnerClosed || !before.Execution.Delivery.Complete || len(before.Handled) != 1 || remote.replyCalls != 2 {
			t.Fatal("mixed response did not retain settled and unsettled outcomes")
		}
		for range 2 {
			run, err = service.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1"})
			if err != nil {
				t.Fatal("same-run partial cannot reconcile its original frozen replies", err)
			}
			var after Journal
			if json.Unmarshal(run.Outcome, &after) != nil || run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID || run.State != store.Partial || run.OwnerClosed || calls != 1 || remote.replyCalls != 2 || len(after.Handled) != 1 || len(after.Execution.Delivery.Effects) != len(before.Execution.Delivery.Effects) || fingerprint(after.Execution.Dispositions) != fingerprint(before.Execution.Dispositions) {
				t.Fatal("partial reconciliation replaced work, replayed effects or settled an outstanding concern")
			}
		}
		return
	}
	if strings.HasPrefix(scenario, "resume-reply-") {
		originalRun := run
		var previous Journal
		if json.Unmarshal(run.Outcome, &previous) != nil || previous.Execution.Delivery.Complete || remote.replyCalls != 1 {
			t.Fatal("missing initial uncertain reply")
		}
		remote.replyMode = ""
		if scenario == "resume-reply-accepted" {
			item := previous.Inventory.Items[0]
			body := feedbackReplyBody(run.ID, item, previous.Execution.Dispositions[0], previous.Execution.Delivery.Proposal.Replies[0], remote.target.Pull.HeadSHA)
			raw, _ := json.Marshal(github.FeedbackReply{ID: 950, Body: body, User: remote.actor})
			remote.context.Conversation = append(remote.context.Conversation, raw)
		}
		run, err = service.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1"})
		if remote.replyCalls != 1 || calls != 1 || run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID {
			t.Fatal("uncertain reply was replayed or run replaced", err)
		}
		if scenario == "resume-reply-unknown" {
			if err == nil || run.OwnerClosed || run.State != store.FeedbackPending || len(remote.requests.Users) != 0 {
				t.Fatal("unconfirmed reply became delivered", err)
			}
			return
		}
		if err != nil {
			t.Fatal("canonical accepted reply did not reconcile", err)
		}
	}
	if strings.HasPrefix(scenario, "resume-checks-") {
		originalRun := run
		var before Journal
		if json.Unmarshal(run.Outcome, &before) != nil {
			t.Fatal("missing pending delivery")
		}
		if run.State != store.FeedbackPending || run.OwnerClosed || before.Execution.Delivery.Commit.OID == "" || before.Execution.Delivery.Complete || len(remote.context.Conversation) != 0 {
			t.Fatal("initial waiting-checks delivery crossed its boundary")
		}
		type explicitResumer interface {
			Resume(context.Context, *config.Document, string, map[string]any, Authorization) (store.Run, error)
		}
		resumer, ok := any(service).(explicitResumer)
		if !ok {
			t.Fatal("post-push CI-pending work has no canonical reconciliation")
		}
		remote.ready = true
		switch scenario {
		case "resume-checks-new-head":
			remote.target.Pull.HeadSHA = strings.Repeat("c", 40)
		case "resume-checks-new-human":
			remote.context.Reviews = append(remote.context.Reviews, json.RawMessage(`{"id":720,"user":{"id":9,"login":"other-reviewer"},"body":"New concern","state":"COMMENTED"}`))
		case "resume-checks-local-work":
			if err := os.WriteFile(filepath.Join(workspaceDirectory, "source.txt"), []byte("new local work\n"), 0600); err != nil {
				t.Fatal(err)
			}
		case "resume-checks-changed-projection":
			ownerRoot := filepath.Dir(workspaceDirectory)
			if before.Execution.Delivery.CandidateFile == nil {
				t.Fatal("reviewed projection not retained")
			}
			if err := os.WriteFile(filepath.Join(ownerRoot, before.Execution.Delivery.CandidateFile.Name), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		run, err = resumer.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1"})
		if scenario != "resume-checks-ready" {
			if err == nil || run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID || calls != 1 || run.OwnerClosed || len(remote.context.Conversation) != 0 || len(remote.requests.Users) != 0 {
				t.Fatal("changed continuation lost work, restarted implementation or performed effects", err)
			}
			var held Journal
			if json.Unmarshal(run.Outcome, &held) != nil || len(held.Execution.Delivery.Effects) != len(before.Execution.Delivery.Effects) {
				t.Fatal("changed continuation altered attempted effects")
			}
			if _, err := os.Stat(workspaceDirectory); err != nil {
				t.Fatal("changed continuation deleted pending workspace", err)
			}
			return
		}
		if err != nil {
			t.Fatal("verified push cannot continue after checks become ready", err)
		}
		if run.ID != originalRun.ID || run.OwnerID != originalRun.OwnerID || calls != 1 {
			t.Fatal("effect reconciliation replaced the run/owner or restarted implementation")
		}
		var after Journal
		if json.Unmarshal(run.Outcome, &after) != nil {
			t.Fatal("missing reconciled delivery")
		}
		count := map[string]int{}
		for _, effect := range after.Execution.Delivery.Effects {
			count[effect.Kind]++
		}
		if count["commit"] != 1 || count["push"] != 1 || count["pre_push"] != 1 || count["reply"] != 1 || count["review_request"] != 1 {
			t.Fatal("reconciliation replayed or omitted effects", count)
		}
	}
	if scenario == "resume-staged-fix" {
		if run.State != store.FeedbackPending || run.OwnerClosed {
			t.Fatal("first attempt did not retain pending work")
		}
		retainedRun := run
		priorPath := workspaceDirectory
		type explicitResumer interface {
			Resume(context.Context, *config.Document, string, map[string]any, Authorization) (store.Run, error)
		}
		resumer, ok := any(service).(explicitResumer)
		if !ok {
			t.Fatal("pending staged feedback has no same-owner service resume capability")
		}
		service.CaptureSkill = nil
		service.Source = nil
		resumer = any(service).(explicitResumer)
		run, err = resumer.Resume(ctx, nil, run.ID, nil, Authorization{Kind: "manual_feedback_v1"})
		if err != nil {
			t.Fatal(err)
		}
		if run.ID != retainedRun.ID || run.OwnerID != retainedRun.OwnerID || workspaceDirectory != priorPath || len(run.PriorExecutions) != 1 {
			t.Fatal("resume replaced the existing run/workspace or erased actual native history")
		}
	}
	expectedState := store.FeedbackPending
	if delivered {
		expectedState = store.FeedbackDelivered
	}
	expectedThread := "actual-primary"
	expectedCalls := 1
	if scenario == "resume-checks-ready" || scenario == "resume-reply-accepted" || scenario == "resume-finalization" {
		expectedThread = ""
	}
	if scenario == "resume-staged-fix" {
		expectedThread = "actual-primary-resumed"
		expectedCalls = 2
	}
	if run.State != expectedState || run.ThreadID != expectedThread || run.OwnerClosed != delivered || calls != expectedCalls || !closed {
		t.Fatalf("primary claim became delivery or lost execution/lifetime facts: %+v", run)
	}
	for _, recover := range []bool{false, true} {
		if delivered {
			break
		}
		if recover {
			if _, err := durable.RecoverPrivate(ctx, false); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(filepath.Join(workspaceDirectory, "source.txt"))
		if err != nil || len(data) == 0 {
			t.Fatal("quiescent pending feedback lost its workspace", recover, err)
		}
	}
	var journal Journal
	if json.Unmarshal(run.Outcome, &journal) != nil || (!delivered && len(journal.Remaining) == 0) || journal.Noop {
		t.Fatal("unconnected delivery boundary was hidden")
	}
	second, secondErr := service.Start(ctx, nil, remote.target.Pull.PullReference, nil, Authorization{Kind: "manual_feedback_v1"})
	if delivered {
		var repeated Journal
		if secondErr != nil || json.Unmarshal(second.Outcome, &repeated) != nil || !repeated.Noop || calls != expectedCalls {
			t.Fatal("verified original human revisions were replayed", secondErr)
		}
	} else if secondErr == nil {
		t.Fatal("new invocation silently replayed prior pending work")
	}
	expectedHandled := 1
	if scenario == "new-human-after-delivery" {
		expectedHandled = 2
	}
	if scenario == "already-addressed" {
		if remote.target.Pull.HeadSHA != head || journal.Execution.Delivery.Commit.OID != "" {
			t.Fatal("non-code feedback created or pushed an empty commit")
		}
		for _, effect := range journal.Execution.Delivery.Effects {
			if effect.Kind == "commit" || effect.Kind == "push" || effect.Kind == "pre_push" {
				t.Fatal("non-code answer entered Git delivery", effect.Kind)
			}
		}
	}
	replies := len(remote.context.Conversation)
	if strings.HasPrefix(scenario, "thread-") {
		replies = 0
		for _, raw := range *remote.context.Threads[0].Comments.Nodes {
			var row struct{ Author struct{ Login string } }
			_ = json.Unmarshal(raw, &row)
			if row.Author.Login == remote.actor.Login {
				replies++
			}
		}
		if *remote.context.Threads[0].IsResolved != delivered {
			t.Fatal("thread resolution did not reflect fully settled canonical feedback", scenario)
		}
	}
	expectedRoles := 6
	if scenario == "unstaged-proposal" {
		expectedRoles = 9
	}
	if scenario == "resume-finalization" {
		expectedRoles = 7
	}
	if delivered && (len(journal.Handled) != expectedHandled || replies != 1 || (scenario != "already-addressed" && !feedbackRequestPresent(remote.requests, "reviewer", "")) || roleCalls != expectedRoles) {
		t.Fatal("full delivery omitted original reply, reviewer, dedupe or post-effect audit")
	}
	if native && (roleCalls != 5 || len(journal.Execution.Roles) != 5 || len(journal.Execution.Commands) != 3) {
		t.Fatal("native service lost role/command observations")
	}
	if calls != expectedCalls {
		t.Fatal("pending prior feedback spawned another modifier")
	}
	if git("rev-parse", "HEAD") != head {
		t.Fatal("ordinary checkout changed")
	}
}
