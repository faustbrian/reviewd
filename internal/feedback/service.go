package feedback

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Authorization is constructed by the feedback-specific operator entry point.
// Legacy private-review decisions deliberately do not map to these versions.
type Authorization struct {
	Kind         string
	Item         *store.InboxItem
	Confirmation string
}

type Remote interface {
	Identity(context.Context) (github.Identity, error)
	FeedbackTarget(context.Context, string, int) (github.FeedbackTarget, error)
	Context(context.Context, github.Pull) (github.ReviewContext, error)
	Checks(context.Context, github.Pull) (github.CheckReport, error)
}

type Service struct {
	GitHub               Remote
	Store                *store.Store
	ModifierDirectory    string
	CaptureSkill         func(config.Snapshot, string) (*review.CapturedSkill, error)
	Source               func(context.Context, github.Pull, string) (*source.Snapshot, error)
	Prepare              func(context.Context, *store.PrivateOwner, *source.Snapshot, string, WorkspaceOptions) (*ExecutionResources, error)
	Agent                func(context.Context, codex.RuntimeOptions, codex.FeedbackRequest) (codex.ReviewOutput, error)
	CaptureGitPolicy     func(context.Context, string) (CommitPolicy, error)
	OperatorInstructions string
}

type HandledRevision struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
}

// Journal is the existing feedback run's facts, not a second replay owner.
// Unverified model prose never populates delivered effects or handled revisions.
type Journal struct {
	Version            int                   `json:"schema_version"`
	Target             github.FeedbackTarget `json:"target"`
	Actor              github.Identity       `json:"actor"`
	Inventory          Inventory             `json:"inventory"`
	Execution          ExecutionJournal      `json:"execution"`
	Handled            []HandledRevision     `json:"handled"`
	PrimaryText        string                `json:"primary_text,omitempty"`
	Remaining          []string              `json:"remaining"`
	Noop               bool                  `json:"noop"`
	Retained           *retainedExecution    `json:"retained,omitempty"`
	PriorNotes         []string              `json:"prior_notes,omitempty"`
	RequirementsDigest string                `json:"requirements_digest"`
}

func (s Service) Start(ctx context.Context, document *config.Document, ref github.PullReference, overrides map[string]any, authorization Authorization) (run store.Run, err error) {
	return s.execute(ctx, document, ref, overrides, authorization, nil)
}

// Resume is an explicit operator action on the SAME existing run. It never
// starts a replacement owner/checkout or retries an uncertain remote effect.
func (s Service) Resume(ctx context.Context, document *config.Document, id string, overrides map[string]any, authorization Authorization) (store.Run, error) {
	if s.Store == nil {
		return store.Run{}, errors.New("feedback resume requires its original profile")
	}
	observed, err := s.Store.Get(ctx, id)
	if err != nil {
		return observed, err
	}
	if authorization.Confirmation != "" && authorization.Confirmation != ResumeConfirmation(observed) {
		return observed, errors.New("displayed retained feedback run changed; preview its current outcome before continuing")
	}
	if observed.Purpose != "feedback" || observed.OwnerClosed || (observed.State != store.FeedbackPending && observed.State != store.Partial) {
		return observed, errors.New("explicit resume requires the original retained pending/partial feedback run")
	}
	var previous Journal
	if json.Unmarshal(observed.Outcome, &previous) != nil || previous.Version != 1 || previous.Retained == nil {
		return observed, errors.New("original retained feedback execution is unavailable; no replacement checkout is permitted")
	}
	if previous.Execution.Delivery.Started && previous.Execution.Delivery.CandidateFile == nil {
		return observed, errors.New("attempted delivery lacks its original reviewed projection; preserve it for manual reconciliation")
	}
	return s.execute(ctx, document, previous.Target.Pull.PullReference, overrides, authorization, &observed)
}

func (s Service) execute(ctx context.Context, document *config.Document, ref github.PullReference, overrides map[string]any, authorization Authorization, observed *store.Run) (run store.Run, err error) {
	if observed != nil {
		run = *observed
	}
	if s.GitHub == nil || s.Store == nil || s.ModifierDirectory == "" {
		return run, errors.New("feedback service requires authenticated GitHub, durable profile and shared modifier directory")
	}
	if authorization.Kind != "manual_feedback_v1" && authorization.Kind != "automatic_feedback_v1" {
		return run, errors.New("feedback needs a new explicit fix/push/reply/resolve/re-review decision; private-review consent never grants it")
	}
	policy, err := config.Resolve(document, ref.Repository, nil, overrides)
	if err != nil {
		return run, err
	}
	if authorization.Kind == "automatic_feedback_v1" && (authorization.Item == nil || authorization.Item.Category != "feedback_handling" || authorization.Item.ExecutionKind != "feedback_v1" || policy.FeedbackAction(authorization.Item.Trigger) != "auto") {
		return run, errors.New("automatic feedback requires its own current feedback-handling event and explicit operator auto policy")
	}
	ctx, cancel := context.WithTimeout(ctx, policy.Duration("limits.feedback_timeout"))
	defer cancel()
	actor, err := s.GitHub.Identity(ctx)
	if err != nil {
		return run, err
	}
	target, err := s.GitHub.FeedbackTarget(ctx, ref.Repository, ref.Number)
	if err != nil {
		return run, err
	}
	if target.Pull.Repository != ref.Repository || target.Pull.Number != ref.Number || actor.ID <= 0 || actor.Login == "" || !policy.FeedbackAuthorAllowed(target.Pull.Author.ID, target.Pull.Author.Login, actor.ID) {
		return run, errors.New("current authenticated identity or PR author does not match the operator's feedback filter")
	}
	remoteContext, err := s.GitHub.Context(ctx, target.Pull)
	if err != nil {
		return run, err
	}
	inventory, err := Collect(remoteContext, actor)
	if err != nil {
		return run, err
	}
	if authorization.Item != nil {
		item := authorization.Item
		if item.Category != "feedback_handling" || item.ExecutionKind != "feedback_v1" || item.ActorID != actor.ID || item.Repository != ref.Repository || item.PullID != target.Pull.ID || item.HeadSHA != target.Pull.HeadSHA || item.TargetBaseSHA != target.Pull.TargetBaseSHA || item.ContextKey != inventory.Digest {
			return run, errors.New("feedback event identity or human inventory changed; synchronize and decide its current revision")
		}
	}
	modifier, err := AcquireModifier(ctx, s.ModifierDirectory, ModifierKey{Host: strings.Split(ref.Repository, "/")[0], RepositoryID: target.Pull.RepositoryID, PullID: target.Pull.ID})
	if err != nil {
		return run, err
	}
	defer func() { err = errors.Join(err, modifier.Close()) }()
	journal := Journal{Version: 1, Target: target, Actor: actor, Inventory: inventory, RequirementsDigest: fingerprint(remoteContext.Requirements), Execution: ExecutionJournal{Roles: []NativeRoleReport{}, Commands: []CommandObservation{}}, Handled: []HandledRevision{}, Remaining: []string{}}
	if observed != nil {
		if modifier.Prior == nil || modifier.Prior.Profile != s.Store.Directory() || modifier.Prior.Run.ID != observed.ID || !bytes.Equal(modifier.Prior.Run.Outcome, observed.Outcome) {
			return *observed, errors.New("the shared modifier does not identify this exact retained run")
		}
		if json.Unmarshal(observed.Outcome, &journal) != nil || journal.Version != 1 || journal.Retained == nil || journal.Actor != actor || journal.Inventory.Digest != inventory.Digest || journal.RequirementsDigest != fingerprint(remoteContext.Requirements) || !resumeTargetMatches(journal, target) {
			return *observed, errors.New("retained feedback identity, source or human inventory changed; reconcile before resuming")
		}
		journal.PriorNotes = append(journal.PriorNotes, journal.PrimaryText)
		journal.PriorNotes = append(journal.PriorNotes, journal.Remaining...)
		journal.PrimaryText = ""
		journal.Remaining = []string{}
		if journal.Execution.Delivery.Started {
			journal.Execution.Delivery.Target = target
		}
	} else if modifier.Prior != nil {
		prior := modifier.Prior.Run
		var previous Journal
		if prior.State != store.FeedbackDelivered || json.Unmarshal(prior.Outcome, &previous) != nil || previous.Version != 1 || previous.Target.Pull.ID != target.Pull.ID || previous.Target.Pull.RepositoryID != target.Pull.RepositoryID {
			return run, errors.New("prior feedback work remains pending, partial or unrecognized; explicitly reconcile that run before starting another modifier")
		}
		journal.Handled = append(journal.Handled, previous.Handled...)
	}
	outstanding := []Item{}
	for _, item := range inventory.Items {
		handled := false
		for _, old := range journal.Handled {
			if old.Key == item.Key && old.Revision == item.Revision {
				handled = true
				break
			}
		}
		if !handled {
			outstanding = append(outstanding, item)
		}
	}
	var owner *store.PrivateOwner
	if observed != nil {
		owner, err = s.Store.ResumeFeedbackOwner(ctx, *observed)
	} else {
		owner, err = s.Store.BeginPrivateOwner(ctx)
	}
	if err != nil {
		return run, err
	}
	var resources *ExecutionResources
	var skill *review.CapturedSkill
	var snapshot *source.Snapshot
	ownedRun := false
	retainedWorkspace := observed != nil
	finalState := store.FeedbackPending
	// Close every worker before asking the common owner for deletion proof. Only
	// that proof permits a terminal delivery claim, including a genuine no-op.
	defer func() {
		if resources != nil {
			err = errors.Join(err, resources.Quiesce())
		}
		var closeErr error
		if retainedWorkspace && finalState != store.FeedbackDelivered {
			closeErr = owner.SuspendFeedback()
		} else {
			closeErr = owner.Close()
		}
		err = errors.Join(err, closeErr)
		saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if ownedRun && run.ID != "" {
			err = errors.Join(err, s.Store.CloseInputs(saveCtx, run.ID))
			if err != nil {
				journal.Remaining = append(journal.Remaining, err.Error())
				finalState = store.FeedbackPending
			}
			data, _ := json.Marshal(journal)
			if closeErr == nil {
				err = errors.Join(err, s.Store.FinishFeedback(saveCtx, run.ID, finalState, data, strings.Join(journal.Remaining, "; ")))
			} else {
				err = errors.Join(err, s.Store.SaveFeedback(saveCtx, run.ID, data))
			}
			if current, getErr := s.Store.Get(saveCtx, run.ID); getErr == nil {
				run = current
			} else {
				err = errors.Join(err, getErr)
			}
		}
		// Injected resources can live outside the common owner only in controlled
		// fixtures. Even those must not be removed before positive child shutdown.
		if closeErr == nil && (!retainedWorkspace || finalState == store.FeedbackDelivered) {
			if skill != nil {
				err = errors.Join(err, skill.Close())
			}
			if snapshot != nil {
				err = errors.Join(err, snapshot.Close())
			}
		} else if closeErr == nil {
			// Positive owner shutdown also permits deleting a new capture that
			// was never adopted by the durable retained execution. Do not leave
			// superseded setup resources after a failed pre-claim/resume check.
			if skill != nil && (journal.Retained == nil || journal.Retained.Skill.Name != filepath.Base(filepath.Dir(filepath.Dir(skill.Path)))) {
				err = errors.Join(err, skill.Close())
			}
			if snapshot != nil && (journal.Retained == nil || journal.Retained.Source.Name != filepath.Base(snapshot.RetainedDirectory())) {
				err = errors.Join(err, snapshot.Close())
			}
		}
	}()
	ownedCtx := owner.Context(ctx)
	oldSkillRoot := ""
	continuingDelivery := observed != nil && journal.Execution.Delivery.Started
	if len(outstanding) > 0 || continuingDelivery {
		if s.CaptureSkill == nil && observed == nil {
			return run, errors.New("feedback requires the actual installed handle-pr-feedback and linked workflow/QA instructions; configure their paths")
		}
		if observed != nil {
			oldSkillRoot, err = journal.Retained.Skill.reopen(owner.Root, "reviewd-skill-")
			if err == nil {
				if filepath.Base(journal.Retained.SkillDirectory) != journal.Retained.SkillDirectory {
					return *observed, errors.New("retained instruction package path is invalid")
				}
				skill, err = review.CaptureFeedbackSkill(journal.Retained.SkillName, filepath.Join(oldSkillRoot, journal.Retained.SkillDirectory, "SKILL.md"), filepath.Join(oldSkillRoot, "qa-test-planner", "SKILL.md"), owner.Root)
				if err == nil && skill.Identity.Digest != observed.SkillDigest {
					err = errors.New("original captured feedback instructions changed")
				}
			}
		} else {
			skill, err = s.CaptureSkill(policy, owner.Root)
		}
		if err != nil {
			return run, err
		}
		if observed == nil {
			// Checkpoint frozen instructions before the run can retain ownership.
			// Source and workspace locators are adopted only after their setup
			// succeeds, so explicit continuation can finish the missing phase.
			instructionRoot, rootErr := skill.FeedbackCommandRoot(owner.Root)
			if rootErr != nil {
				return run, rootErr
			}
			retained := &retainedExecution{SkillName: skill.Identity.Name, SkillDirectory: filepath.Base(filepath.Dir(skill.Path))}
			retained.Skill, err = retainPath(owner.Root, instructionRoot, "reviewd-skill-")
			if err != nil {
				return run, err
			}
			journal.Retained = retained
		}
	}
	configuration, _ := json.Marshal(policy)
	if observed != nil && !bytes.Equal(configuration, observed.Configuration) {
		return run, errors.New("resume requires the run's frozen operator configuration; restore that configuration before continuing")
	}
	boundary, _ := json.Marshal(struct {
		Target    github.FeedbackTarget `json:"target"`
		Actor     github.Identity       `json:"actor"`
		Inventory Inventory             `json:"inventory"`
	}{target, actor, inventory})
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return run, err
	}
	workKey := "feedback-v1:" + hex.EncodeToString(nonce[:])
	if authorization.Item != nil {
		workKey = "feedback-v1:" + authorization.Item.EventKey + ":" + authorization.Item.DecisionID
	}
	queued := store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: ref.Repository, RepositoryID: target.Pull.RepositoryID, PullID: target.Pull.ID, Number: ref.Number, ActorID: actor.ID, ActorLogin: actor.Login, WorkKey: workKey, State: store.Queued, Configuration: configuration, Context: boundary}
	if skill != nil {
		queued.SkillDigest = skill.Identity.Digest
	}
	var created bool
	if observed != nil {
		run = *observed
		created, err = s.Store.ResumeFeedback(ownedCtx, *observed, owner.ID)
	} else if authorization.Item == nil {
		run, created, err = s.Store.Create(ownedCtx, queued)
	} else {
		run, created, err = s.Store.CreateForItem(ownedCtx, queued, *authorization.Item)
	}
	if err != nil {
		return run, err
	}
	if !created {
		return run, errors.New("feedback event was already consumed; inspect its existing run instead of replaying")
	}
	ownedRun = true
	if err = modifier.Attach(ownedCtx, s.Store, run.ID); err != nil {
		return run, err
	}
	claimed := created
	if observed == nil {
		claimed, err = s.Store.ClaimFeedback(ownedCtx, run.ID)
	}
	if err != nil {
		return run, err
	}
	if !claimed {
		return run, errors.New("feedback work could not be exclusively claimed")
	}
	save := func() error { data, _ := json.Marshal(journal); return s.Store.SaveFeedback(ownedCtx, run.ID, data) }
	// The first durable retained-owner state must already have its frozen
	// setup checkpoint. Recovery cannot reconstruct in-memory instructions.
	if err = save(); err != nil {
		return run, err
	}
	if len(outstanding) > 0 && observed == nil {
		if err = owner.KeepFeedbackWorkspace(ownedCtx, run.ID); err != nil {
			return run, err
		}
		retainedWorkspace = true
	}
	stopCancellation, err := observeFeedbackCancellation(ownedCtx, s.Store, run.ID, cancel)
	if err != nil {
		return run, err
	}
	defer func() { err = errors.Join(err, stopCancellation()) }()
	expectedTarget := target
	revalidate := func() error {
		currentActor, err := s.GitHub.Identity(ownedCtx)
		if err != nil {
			return err
		}
		current, err := s.GitHub.FeedbackTarget(ownedCtx, ref.Repository, ref.Number)
		if err != nil {
			return err
		}
		if currentActor != actor || current.HeadRepository != expectedTarget.HeadRepository || current.HeadRepositoryID != expectedTarget.HeadRepositoryID || !github.SameReviewContext(current.Pull, expectedTarget.Pull) || !policy.FeedbackAuthorAllowed(current.Pull.Author.ID, current.Pull.Author.Login, currentActor.ID) {
			return errors.New("feedback identity, existing branch, author policy or source changed; preserve pending work")
		}
		context, err := s.GitHub.Context(ownedCtx, current.Pull)
		if err != nil {
			return err
		}
		latest, err := Collect(context, currentActor)
		if err != nil {
			return err
		}
		if latest.Digest != inventory.Digest || fingerprint(context.Requirements) != fingerprint(remoteContext.Requirements) {
			return errors.New("new or edited human feedback arrived; reconcile its current inventory before further effects")
		}
		return nil
	}
	if len(outstanding) == 0 && !continuingDelivery {
		if observed != nil {
			return run, errors.New("retained feedback has no outstanding work or recorded delivery to reconcile; preserve its existing owner")
		}
		if err = revalidate(); err != nil {
			return run, err
		}
		journal.Noop = true
		finalState = store.FeedbackDelivered
		return run, nil
	}
	acquire := s.Source
	if acquire == nil {
		if client, ok := s.GitHub.(*github.Client); ok {
			acquire = client.Source
		}
	}
	hasRetainedSource := observed != nil && journal.Retained.Source.Name != ""
	if acquire == nil && !hasRetainedSource {
		return run, errors.New("feedback immutable source acquisition is unavailable")
	}
	sourceTarget := target
	if observed != nil {
		sourceTarget = journal.Target
	}
	if hasRetainedSource {
		var directory string
		directory, err = journal.Retained.Source.reopen(owner.Root, "reviewd-source-")
		if err == nil {
			snapshot, err = source.ReopenFeedbackSnapshot(ownedCtx, owner.Root, directory, source.Boundary{Number: sourceTarget.Pull.Number, HeadSHA: sourceTarget.Pull.HeadSHA, TargetBaseSHA: sourceTarget.Pull.TargetBaseSHA, ChangedFiles: sourceTarget.Pull.ChangedFiles})
		}
	} else {
		snapshot, err = acquire(ownedCtx, target.Pull, owner.Root)
	}
	if err != nil {
		return run, err
	}
	if snapshot == nil || snapshot.HeadSHA != sourceTarget.Pull.HeadSHA || snapshot.TargetBaseSHA != sourceTarget.Pull.TargetBaseSHA || len(snapshot.Changes) != sourceTarget.Pull.ChangedFiles || snapshot.CommitCount != sourceTarget.Pull.CommitCount {
		return run, errors.New("feedback source omitted the complete immutable PR comparison")
	}
	if err = revalidate(); err != nil {
		return run, err
	}
	options := codex.RuntimeOptions{Executable: policy.String("reviewer.executable"), AuthHome: policy.String("reviewer.auth_home"), TempRoot: owner.Root}
	captureGitPolicy := s.CaptureGitPolicy
	if captureGitPolicy == nil {
		captureGitPolicy = CaptureCommitPolicy
	}
	gitPolicy, gitPolicyErr := captureGitPolicy(ownedCtx, owner.Root)
	instructionRoot, err := skill.FeedbackCommandRoot(owner.Root)
	if err != nil {
		return run, err
	}
	retained := &retainedExecution{SkillName: skill.Identity.Name, SkillDirectory: filepath.Base(filepath.Dir(skill.Path))}
	if retained.Source, err = retainPath(owner.Root, snapshot.RetainedDirectory(), "reviewd-source-"); err != nil {
		return run, err
	}
	if retained.Skill, err = retainPath(owner.Root, instructionRoot, "reviewd-skill-"); err != nil {
		return run, err
	}
	if observed != nil {
		retained.Workspace = journal.Retained.Workspace
	}
	journal.Retained = retained
	if err = save(); err != nil {
		return run, err
	}
	if oldSkillRoot != "" && oldSkillRoot != instructionRoot {
		if err = os.RemoveAll(oldSkillRoot); err != nil {
			return run, err
		}
	}
	prepare := s.Prepare
	if prepare == nil {
		prepare = prepareExecutionResources
	}
	trustedReadRoots := []string{}
	if gitPolicyErr == nil && strings.HasPrefix(gitPolicy.HooksPath, "/") {
		for _, name := range normalFeedbackHooks {
			if _, ok := gitPolicy.RequiredHooks[name]; ok {
				trustedReadRoots = append(trustedReadRoots, gitPolicy.HooksPath+"/"+name)
			}
		}
	}
	workspaceOptions := WorkspaceOptions{Runtime: options, ContainerEndpoint: policy.String("feedback.container_endpoint"), ContainerImage: policy.String("feedback.container_image"), trustedReadRoots: trustedReadRoots, instructionRoot: instructionRoot}
	if observed != nil && journal.Retained.Workspace.Name != "" {
		workspaceOptions.retainedDirectory, err = journal.Retained.Workspace.reopen(owner.Root, "reviewd-feedback-workspace-")
		if err != nil {
			return run, err
		}
		if continuingDelivery && journal.Execution.Delivery.Commit.OID != "" {
			workspaceOptions.inspectionHead = journal.Execution.Delivery.Commit.OID
		}
	}
	resources, err = prepare(ownedCtx, owner, snapshot, target.Pull.HeadRef, workspaceOptions)
	if err != nil {
		return run, err
	}
	if retained.Workspace, err = retainPath(owner.Root, resources.Directory, "reviewd-feedback-workspace-"); err != nil {
		return run, err
	}
	journal.Retained = retained
	if err = save(); err != nil {
		return run, err
	}
	var frozen *executionTree
	if continuingDelivery {
		var projection executionTree
		projection, err = journal.Execution.Delivery.CandidateFile.load(owner.Root, journal.Execution.Delivery.Candidate)
		if err != nil {
			return run, err
		}
		frozen = &projection
		current := snapshot
		if target.Pull.HeadSHA != snapshot.HeadSHA {
			if acquire == nil {
				return run, errors.New("canonical delivered-head source acquisition is unavailable")
			}
			current, err = acquire(ownedCtx, target.Pull, owner.Root)
			if err != nil {
				return run, err
			}
		}
		validateErr := validateReconciledWorkspace(ownedCtx, resources, current, target.Pull, projection)
		if current != snapshot {
			validateErr = errors.Join(validateErr, current.Close())
		}
		if validateErr != nil {
			return run, validateErr
		}
	}
	documents := map[string]string{"operator-instructions": s.OperatorInstructions, "captured-helper-execution": "The exact frozen feedback instruction bundle is read-only at execution-environment.Instructions (also REVIEWD_INSTRUCTIONS_PATH in commands). Invoke captured Python helpers using the isolated interpreter and that guest-visible root, preserving sibling package layout. This grants no installed host skill tree, host HOME, authentication, signing key, runtime socket, network or new write root. Missing interpreters/tools remain explicit pending requirements. Ordinary private reviewers never receive this helper capability."}
	encode := func(key string, value any) { data, _ := json.Marshal(value); documents[key] = string(data) }
	encode("feedback-target", target)
	encode("feedback-inventory", inventory)
	encode("outstanding-feedback", outstanding)
	encode("pull-context", remoteContext)
	encode("frozen-operator-policy", policy)
	encode("execution-environment", resources.Environment)
	if continuingDelivery {
		encode("reconciled-current-workspace", map[string]any{"head": target.Pull.HeadSHA, "branch": target.Pull.HeadRef, "matches_canonical_delivered_source_and_index": true, "matches_original_reviewed_source": true, "boundary": "Fresh application byte/mode/path and canonical index comparison after positive quiescence; historical verification/reviews remain bound to the separately retained original candidate."})
	}
	agent := s.Agent
	if agent == nil {
		agent = RunNativeAgent
	}
	onInput := func(ctx context.Context, input clarification.Request) (clarification.Answers, error) {
		return s.Store.WaitInput(ctx, input.ID)
	}
	onInputEvent := func(ctx context.Context, input clarification.Request, event, reason string) error {
		if event == "opened" {
			return s.Store.OpenInput(ctx, run.ID, input)
		}
		return s.Store.InputEvent(ctx, input.ID, event, reason)
	}
	// Keep the complete human inventory for concurrency and whole-thread checks,
	// but grant decisions/replies only for revisions not already handled. The
	// digest still identifies the complete captured remote context, not this
	// application-owned projection of the work remaining.
	activeInventory := inventory
	activeInventory.Items = outstanding
	var previousExecution *ExecutionJournal
	if observed != nil {
		prior := cloneJournal(journal.Execution)
		previousExecution = &prior
	}
	delivery := func() (deliveryOptions, error) {
		if gitPolicyErr != nil {
			return deliveryOptions{}, gitPolicyErr
		}
		publisher, ok := s.GitHub.(deliveryRemote)
		if !ok {
			return deliveryOptions{}, errors.New("scoped Git/GitHub delivery adapters are unavailable")
		}
		return deliveryOptions{Remote: publisher, Resources: resources, Snapshot: snapshot, Policy: gitPolicy, RunID: run.ID, Target: journal.Target, Actor: actor, Inventory: inventory, Handled: journal.Handled, Context: remoteContext, Revalidate: revalidate, AdoptTarget: func(current github.FeedbackTarget) { expectedTarget = current }}, nil
	}
	execution, err := NewExecution(ExecutionOptions{OwnerRoot: owner.Root, Frozen: frozen, Previous: previousExecution, Policy: policy, Skill: skill, Snapshot: snapshot, Workspace: resources.Directory, Environment: resources.Environment, Inspect: resources.Inspect, Inventory: activeInventory, BeforeWrite: func(context.Context) error { return revalidate() }, Documents: documents, OnInput: onInput, OnInputEvent: onInputEvent, Command: resources.Command, Quiesce: resources.Quiesce, Agent: func(ctx context.Context, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
		return agent(ctx, options, request)
	}, Save: func(observations ExecutionJournal) error { journal.Execution = observations; return save() }, Deliver: func(ctx context.Context, proposal DeliveryProposal, candidate string, decisions []ItemDisposition, record func(DeliveryJournal) error) (DeliveryJournal, error) {
		publisher, publisherErr := delivery()
		if publisherErr != nil {
			return DeliveryJournal{}, publisherErr
		}
		return publisher.deliver(ctx, proposal, candidate, decisions, record)
	}})
	if err != nil {
		return run, err
	}
	if continuingDelivery {
		publisher, publisherErr := delivery()
		if publisherErr != nil {
			return run, publisherErr
		}
		_, err = publisher.reconcile(ownedCtx, journal.Execution.Delivery, journal.Execution.Dispositions, execution.recordReconciliation)
		journal.Execution = execution.Journal()
		if err != nil {
			return run, err
		}
	} else {
		keys := []string{}
		for key := range documents {
			keys = append(keys, key)
		}
		for key := range skill.Documents {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		prompt := fmt.Sprintf("Apply the actual installed $%s workflow to existing PR %s at immutable head %s, target base %s and comparison base %s. Full operator authorization is scoped to feedback fixes, normal hooks/signing, additive ordinary existing-branch push, necessary metadata, original replies/settled resolutions and renewed prior/current reviewer requests. It never grants merging, deployment, force, bypasses, other branches or customer-wide work. Your native tools remain read-only; use the separate application broker. Read complete captured instructions/dependencies, operator-instructions, target/head AGENTS, full original-diff/original-commits and paginated inventory. Preserve a per-item accepted/already-addressed/rejected/deferred/needs-clarification disposition with source evidence. Before modifying behavior obtain fresh mapper and one bounded QA pass through delegate; run required regression and complete final-diff review privately, then delivery audit of native facts. Read execution-environment before choosing commands: macOS repository commands execute in the identified Linux guest and cannot certify macOS-only checks. New paths are independently discovered. Explicitly stage only your changed paths through the command broker before final verification and reviews; delivery does not stage for you. Verification must run on unchanged complete source and index. Follow parent/role capability denials; model prose never proves an effect. Isolated repository commands have no host credentials or network. A missing dependency, mandatory check or delivery capability must remain explicit pending, not a substitute workflow or fabricated success. Do clear independent safe work while clarification waits. Available evidence keys: %s.", skill.Identity.Name, ref.URL, target.Pull.HeadSHA, target.Pull.TargetBaseSHA, snapshot.MergeBaseSHA, strings.Join(keys, ", "))
		if observed != nil && len(journal.Execution.Roles) > 0 {
			prompt += " This is an explicit continuation of the SAME retained run/owner/workspace with a fresh primary session. Read native-role-reports and command-observations. Reuse the existing single bounded mapper/QA inputs; they are historical implementation inputs, not fresh verification. Do not ask for another QA plan. Inspect the actual retained staged work, execute current verification and fresh regression/correctness/delivery review; never replay commands or claim old checks/reviews certify this attempt."
		}
		output, workerErr := agent(ownedCtx, options, codex.FeedbackRequest{Model: policy.String("feedback.model"), Effort: policy.String("feedback.reasoning_effort"), SkillName: skill.Identity.Name, SkillPath: skill.Path, Prompt: prompt, Tools: execution.Tools(), Handle: execution.Handle,
			OnStatus: func(status codex.ReviewOutput) error {
				if observed != nil && status.ThreadID == observed.ThreadID {
					return errors.New("explicit feedback resume must use a fresh primary session")
				}
				if status.RequestedModel != policy.String("feedback.model") {
					return errors.New("feedback primary changed frozen model")
				}
				if err := execution.SetPrimaryIdentity(status.ThreadID); err != nil {
					return err
				}
				return s.Store.RecordProgress(ownedCtx, run.ID, status.ThreadID, status.TurnID, status.RequestedModel, status.EffectiveModel, status.Progress, status.Usage)
			},
			OnInput: onInput, OnInputEvent: onInputEvent,
		})
		journal.Execution = execution.Journal()
		if len(output.Text) > 256<<10 {
			workerErr = errors.Join(workerErr, errors.New("feedback final text exceeded supported journal bound"))
		} else {
			journal.PrimaryText = output.Text
		}
		saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		workerErr = errors.Join(workerErr, s.Store.RecordWorker(saveCtx, run.ID, output.ThreadID, output.TurnID, output.RequestedModel, output.EffectiveModel, output.Usage, nil))
		stop()
		if output.ThreadID == "" || output.TurnID == "" || output.RequestedModel != policy.String("feedback.model") || output.EffectiveModel != output.RequestedModel {
			workerErr = errors.Join(workerErr, errors.New("feedback primary omitted fresh actual model/thread/turn identity"))
		}
		if workerErr != nil {
			return run, workerErr
		}
	}
	if err = revalidate(); err != nil {
		return run, err
	}
	if !journal.Execution.Delivery.Complete {
		journal.Remaining = append(journal.Remaining, "Feedback delivery has not completed its scoped hooks, commit/push, mandatory checks and canonical original responses/re-review boundaries.")
		return run, nil
	}
	// A pre-publication audit cannot certify subsequent effects. Obtain a fresh
	// native auditor with the original reports and canonical effect journal.
	if _, err = execution.Handle(ownedCtx, "reviewd_feedback_delegate", json.RawMessage(`{"role":"delivery_auditor"}`)); err != nil {
		return run, err
	}
	journal.Execution = execution.Journal()
	last := journal.Execution.Roles[len(journal.Execution.Roles)-1]
	if last.Role != "delivery_auditor" || !last.AfterDelivery || last.Disposition != "APPROVE" || last.Error != "" {
		journal.Remaining = append(journal.Remaining, "Post-delivery independent audit did not conclusively approve canonical delivery facts.")
		return run, nil
	}
	for _, decision := range journal.Execution.Dispositions {
		if decision.Status == "accepted" || decision.Status == "already_addressed" || decision.Status == "rejected" {
			alreadyHandled := false
			for _, prior := range journal.Handled {
				if prior.Key == decision.Key && prior.Revision == decision.Revision {
					alreadyHandled = true
					break
				}
			}
			if !alreadyHandled {
				journal.Handled = append(journal.Handled, HandledRevision{Key: decision.Key, Revision: decision.Revision})
			}
		} else {
			journal.Remaining = append(journal.Remaining, "Unsettled feedback: "+decision.Key)
		}
	}
	if len(journal.Remaining) > 0 {
		finalState = store.Partial
		return run, nil
	}
	if err = resources.Quiesce(); err != nil {
		return run, err
	}
	if err = save(); err != nil {
		return run, err
	}
	if err = owner.ReleaseFeedbackWorkspace(ownedCtx, run.ID); err != nil {
		return run, err
	}
	retainedWorkspace = false
	finalState = store.FeedbackDelivered
	return run, nil
}

func observeFeedbackCancellation(ctx context.Context, durable *store.Store, id string, cancel context.CancelFunc) (func() error, error) {
	requested, err := durable.StopRequested(ctx, id)
	if err != nil {
		return nil, err
	}
	if requested {
		cancel()
		return nil, context.Canceled
	}
	monitor, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitor.Done():
				done <- nil
				return
			case <-ticker.C:
				readCtx, end := context.WithTimeout(monitor, time.Second)
				requested, err := durable.StopRequested(readCtx, id)
				end()
				if monitor.Err() != nil {
					done <- nil
					return
				}
				if err != nil {
					cancel()
					done <- errors.New("feedback stopped because durable cancellation state is unavailable")
					return
				}
				if requested {
					cancel()
					done <- nil
					return
				}
			}
		}
	}()
	return func() error { stop(); return <-done }, nil
}
