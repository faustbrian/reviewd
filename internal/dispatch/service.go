// Package dispatch owns action launch and run observation, never skill workflows.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/store"
)

type Remote interface {
	Identity(context.Context) (github.Identity, error)
	Pull(context.Context, string, int) (github.Pull, error)
	PullPreferences(context.Context, github.Pull) (*config.RepositoryPreferences, error)
}
type Service struct {
	Store         *store.Store
	GitHub        Remote
	LockDirectory string
	Launch        func(context.Context, agent.Request) (agent.Output, error)
}
type Job struct {
	Target    github.PullReference
	Purpose   string
	Item      *store.InboxItem
	Overrides map[string]any
	Private   bool
}

func ItemWorkKey(item store.InboxItem) string {
	data, _ := json.Marshal([]string{"skill-event", item.ID, item.EventKey, item.DecisionID})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s Service) Run(ctx context.Context, document *config.Document, job Job) (run store.Run, err error) {
	if s.Store == nil || s.GitHub == nil {
		return run, errors.New("agent dispatch dependencies are unavailable")
	}
	if job.Purpose == "" {
		job.Purpose = "review"
	}
	if job.Purpose != "review" && job.Purpose != "feedback" {
		return run, errors.New("unknown agent action")
	}
	if job.Item != nil {
		feedback := job.Item.Category == "feedback_handling" && job.Item.ExecutionKind == "feedback_v1"
		if feedback != (job.Purpose == "feedback") || job.Item.Category == "own_pr_feedback" {
			return run, errors.New("inbox action and selected skill lane disagree")
		}
		if job.Item.RunID != "" {
			return s.Store.Get(ctx, job.Item.RunID)
		}
	}
	prefix, timeout := "reviewer.", "limits.review_timeout"
	if job.Purpose == "feedback" {
		prefix, timeout = "feedback.", "limits.feedback_timeout"
	}
	policy, err := config.Resolve(document, job.Target.Repository, nil, job.Overrides)
	if err != nil {
		return run, err
	}
	ctx, cancel := context.WithTimeout(ctx, policy.Duration(timeout))
	defer cancel()
	actor, err := s.GitHub.Identity(ctx)
	if err != nil {
		return run, err
	}
	pull, err := s.GitHub.Pull(ctx, job.Target.Repository, job.Target.Number)
	if err != nil {
		return run, err
	}
	preferences, err := s.GitHub.PullPreferences(ctx, pull)
	if err != nil {
		return run, err
	}
	policy, err = config.Resolve(document, pull.Repository, preferences, job.Overrides)
	if err != nil {
		return run, err
	}
	if job.Item != nil {
		if job.Item.ActorID != actor.ID || job.Item.Repository != pull.Repository || job.Item.PullID != pull.ID {
			return run, errors.New("discovered PR or account identity changed")
		}
		if pull.State != "open" || (pull.Draft && !policy.Bool("watch.include_drafts")) {
			return run, errors.New("discovered PR is no longer eligible")
		}
		if job.Purpose == "feedback" {
			if !policy.FeedbackMonitoringConfigured() || !policy.FeedbackAuthorAllowed(pull.Author.ID, pull.Author.Login, actor.ID) {
				return run, errors.New("PR author is not selected for feedback handling")
			}
			if !job.Item.Manual && policy.FeedbackAction(job.Item.Trigger) != "auto" {
				return run, errors.New("automatic feedback action is no longer selected")
			}
		} else if policy.ReviewAuthorExcluded(pull.Author.ID, pull.Author.Login, actor.ID) {
			return run, errors.New("PR author is excluded from discovered reviews")
		}
	}
	directory := s.LockDirectory
	if directory == "" {
		directory = s.Store.Directory()
	}
	host, _, _ := strings.Cut(pull.Repository, "/")
	var admission *ExecutionLease
	for {
		admission, err = AcquireExecutionLease(ctx, directory, ExecutionLeaseKey{Host: host, RepositoryID: pull.RepositoryID, PullID: pull.ID})
		if !errors.Is(err, ErrExecutionLeaseBusy) {
			break
		}
		// Discovered contenders remain future work. Re-admission resolves the
		// latest operator settings instead of accepting a stale lease waiter.
		if job.Item != nil {
			return run, err
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err != nil {
		return run, err
	}
	defer admission.Close()
	owner, err := s.Store.BeginPrivateOwner(ctx)
	if err != nil {
		return run, err
	}
	createdRun, finalized := false, false
	defer func() {
		cleanupErr := owner.Close()
		err = errors.Join(err, cleanupErr)
		if !createdRun || finalized {
			return
		}
		save, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		current, readErr := s.Store.Get(save, run.ID)
		if readErr != nil {
			err = errors.Join(err, readErr)
			return
		}
		if current.State != store.Queued && current.State != store.AgentRunning {
			run = current
			return
		}
		if err == nil {
			err = errors.New("agent did not reach execution completion")
		}
		empty, _ := json.Marshal(agent.Output{})
		err = errors.Join(err, s.Store.FinishAgent(save, run.ID, terminalState(ctx, err, cleanupErr), empty, err.Error()))
		if current, readErr = s.Store.Get(save, run.ID); readErr == nil {
			run = current
		} else {
			err = errors.Join(err, readErr)
		}
	}()
	workKey := ""
	if job.Item != nil {
		workKey = ItemWorkKey(*job.Item)
	} else {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return run, err
		}
		workKey = "manual:" + hex.EncodeToString(nonce[:])
	}
	configuration, err := json.Marshal(policy)
	if err != nil {
		return run, err
	}
	target, err := json.Marshal(pull)
	if err != nil {
		return run, err
	}
	candidate := store.Run{Executor: store.NativeExecutor, Purpose: job.Purpose, OwnerID: owner.ID, Repository: pull.Repository, RepositoryID: pull.RepositoryID, PullID: pull.ID, Number: pull.Number, ActorID: actor.ID, ActorLogin: actor.Login, WorkKey: workKey, State: store.Queued, Private: job.Private, Configuration: configuration, Context: target}
	var created bool
	if job.Item == nil {
		run, created, err = s.Store.Create(ctx, candidate)
	} else {
		run, created, err = s.Store.CreateForItem(ctx, candidate, *job.Item)
	}
	if err != nil || !created {
		return run, err
	}
	createdRun = true
	if err = admission.Attach(ctx, s.Store, run.ID); err != nil {
		return run, err
	}
	claimed, err := s.Store.ClaimAgent(ctx, run.ID)
	if err != nil {
		return run, err
	}
	if !claimed {
		finalized = true
		return s.Store.Get(ctx, run.ID)
	}
	stop, monitorErr := s.observeCancellation(ctx, run.ID, cancel)
	var output agent.Output
	if monitorErr == nil {
		instructions := policy.String("review.instructions")
		if job.Purpose == "feedback" && policy.String("feedback.instructions") != "" {
			instructions = policy.String("feedback.instructions")
		}
		if job.Private {
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += "Private, read-only review: do not post comments or change repository or remote state."
		}
		launch := s.Launch
		if launch == nil {
			launch = agent.Run
		}
		output, err = launch(owner.Context(ctx), agent.Request{Executable: policy.String("reviewer.executable"), Directory: policy.String("agent.working_directory"), Skill: policy.String(prefix + "skill"), URL: pull.URL, Model: policy.String(prefix + "model"), Effort: policy.String(prefix + "reasoning_effort"), Instructions: instructions, OnProgress: func(progress agent.Output) error {
			if progress.ThreadID == "" {
				return nil
			}
			return s.Store.RecordAgent(ctx, run.ID, progress.ThreadID, policy.String(prefix+"model"), progress.Usage, progress.Activity)
		}})
		err = errors.Join(err, stop())
	} else {
		err = monitorErr
	}
	save, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	if output.ThreadID != "" {
		err = errors.Join(err, s.Store.RecordAgent(save, run.ID, output.ThreadID, policy.String(prefix+"model"), output.Usage, output.Activity))
	}
	cleanupErr := owner.Close()
	err = errors.Join(err, cleanupErr)
	state := terminalState(ctx, err, cleanupErr)
	if err == nil && !output.Completed {
		state = store.AgentInterrupted
		err = errors.New("agent returned without a completed native turn")
	}
	outcome, encodeErr := json.Marshal(output)
	err = errors.Join(err, encodeErr)
	if encodeErr == nil {
		diagnostic := ""
		if err != nil {
			diagnostic = err.Error()
		}
		finishErr := s.Store.FinishAgent(save, run.ID, state, outcome, diagnostic)
		err = errors.Join(err, finishErr)
		finalized = finishErr == nil
	}
	current, readErr := s.Store.Get(save, run.ID)
	if readErr != nil {
		return run, errors.Join(err, readErr)
	}
	if job.Item != nil {
		err = errors.Join(err, s.Store.FinishItem(save, *job.Item, current))
	}
	return current, err
}

func PRURL(item store.InboxItem) string {
	return fmt.Sprintf("https://%s/pull/%d", item.Repository, item.Number)
}

func terminalState(ctx context.Context, err, cleanupErr error) store.State {
	if err == nil {
		return store.AgentCompleted
	}
	if cleanupErr != nil || errors.Is(err, lifetime.ErrCleanupUncertain) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return store.AgentInterrupted
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return store.Cancelled
	}
	return store.Failed
}
