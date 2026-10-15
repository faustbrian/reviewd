package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
)

// Profile overrides never select this root: all profiles for one OS user must
// serialize modification of the same canonical repository/PR identity.
func feedbackModifierDirectory() (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if runtime.GOOS == "darwin" {
		root = ""
	}
	if root != "" && !filepath.IsAbs(root) {
		return "", errors.New("XDG_STATE_HOME must be an absolute directory")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if runtime.GOOS == "darwin" {
			root = filepath.Join(home, "Library", "Application Support")
		} else {
			root = filepath.Join(home, ".local", "state")
		}
	}
	return filepath.Join(root, "reviewd", "feedback-modifiers"), nil
}

func feedbackInstalledSkill(name, selected string) (string, error) {
	if selected != "" {
		return readableSkill(selected)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	for _, root := range []string{filepath.Join(codexHome, "skills"), filepath.Join(home, ".agents", "skills")} {
		path := filepath.Join(root, name, "SKILL.md")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		return readableSkill(path)
	}
	return "", errors.New("selected installed feedback/QA skill was not found")
}
func captureFeedbackSkill(policy config.Snapshot, parent string) (*review.CapturedSkill, error) {
	path, err := feedbackInstalledSkill(policy.String("feedback.skill"), policy.String("feedback.skill_path"))
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	qaPath := filepath.Join(filepath.Dir(filepath.Dir(resolved)), "qa-test-planner", "SKILL.md")
	if _, err = readableSkill(qaPath); err != nil {
		qaPath, err = feedbackInstalledSkill("qa-test-planner", "")
		if err != nil {
			return nil, err
		}
	}
	return review.CaptureFeedbackSkill(policy.String("feedback.skill"), path, qaPath, parent)
}

func (e *execution) feedbackExecutor(deps Dependencies, durable *store.Store, directory string) func(context.Context, *config.Document, *github.Client, store.InboxItem) (store.Run, error) {
	return func(ctx context.Context, document *config.Document, client *github.Client, item store.InboxItem) (store.Run, error) {
		if item.Category != "feedback_handling" || item.ExecutionKind != "feedback_v1" || client == nil {
			return store.Run{}, errors.New("feedback executor accepts only its separately authorized typed event")
		}
		policy, err := config.Resolve(document, item.Repository, nil, item.Overrides)
		if err != nil {
			return store.Run{}, err
		}
		hold := func(reason string) (store.Run, error) {
			if err := durable.SetFeedbackExecutionHold(ctx, item, reason); err != nil {
				return store.Run{}, err
			}
			return store.Run{}, store.ErrHeld
		}
		if runtime.GOOS == "darwin" && deps.FeedbackPrepare == nil && (policy.String("feedback.container_endpoint") == "" || policy.String("feedback.container_image") == "") {
			return hold("macOS feedback requires operator feedback.container_endpoint, an immutable feedback.container_image and the paired installed helper; ordinary reviewing continues.")
		}
		service, err := e.feedbackService(deps, durable, directory, policy, client)
		if err != nil {
			return store.Run{}, err
		}
		kind := "automatic_feedback_v1"
		if item.Manual {
			kind = "manual_feedback_v1"
		}
		ref := github.PullReference{Repository: item.Repository, Number: item.Number, URL: "https://" + item.Repository + "/pull/" + fmt.Sprint(item.Number)}
		run, err := service.Start(ctx, document, ref, item.Overrides, feedback.Authorization{Kind: kind, Item: &item})
		if err != nil && run.ID == "" && ctx.Err() == nil && github.RetryAfter(err) == 0 {
			return hold(err.Error())
		}
		return run, err
	}
}

// The typed watcher and explicit same-run continuation share one service and
// the same cross-profile modifier/primary slot boundaries.
func (e *execution) feedbackService(deps Dependencies, durable *store.Store, directory string, policy config.Snapshot, client *github.Client) (feedback.Service, error) {
	modifiers, err := feedbackModifierDirectory()
	if err != nil {
		return feedback.Service{}, err
	}
	agent := deps.FeedbackAgent
	if agent == nil {
		agent = feedback.RunNativeAgent
	}
	worker := func(ctx context.Context, options codex.RuntimeOptions, request codex.FeedbackRequest) (codex.ReviewOutput, error) {
		if request.Role != "" {
			return agent(ctx, options, request)
		}
		lease, err := watch.AcquireSlot(ctx, directory, int(policy.Int("limits.max_concurrent_reviews")))
		if err != nil {
			return codex.ReviewOutput{}, err
		}
		defer lease.Close()
		return agent(ctx, options, request)
	}
	service := feedback.Service{GitHub: client, Store: durable, ModifierDirectory: modifiers, CaptureSkill: captureFeedbackSkill, Source: deps.FeedbackSource, Prepare: deps.FeedbackPrepare, Agent: worker, CaptureGitPolicy: deps.FeedbackGitPolicy, OperatorInstructions: policy.String("review.instructions")}
	return service, nil
}
