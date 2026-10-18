package cli

import (
	"context"
	"fmt"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
	"github.com/spf13/cobra"
)

func (e *execution) skillCommand(purpose string, deps Dependencies) *cobra.Command {
	private := false
	command := &cobra.Command{Use: purpose + " <pr-url>", Short: "Run the configured " + purpose + " skill with your normal Codex settings", Args: cobra.ExactArgs(1)}
	if purpose == "review" {
		command.Flags().BoolVar(&private, "private", false, "Ask the review skill for a private, read-only review")
	}
	command.RunE = func(command *cobra.Command, args []string) error {
		document, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		overrides := e.overrides(command)
		if purpose == "feedback" {
			overrides = e.feedbackOverrides(command)
		}
		policy, err := config.ResolveDefaults(document, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		ref, err := github.ParsePullURL(policy.String("github.host"), args[0])
		if err != nil {
			return failure(2, err.Error())
		}
		directory, err := stateDirectory(policy, path)
		if err != nil {
			return failure(1, err.Error())
		}
		durable, err := store.Open(command.Context(), directory)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		client, err := deps.GitHub(command.Context(), policy.String("github.host"))
		if err != nil {
			return failure(1, err.Error())
		}
		service, err := e.agentService(deps, durable, directory, client, policy)
		if err != nil {
			return failure(1, err.Error())
		}
		run, err := service.Run(command.Context(), document, dispatch.Job{Target: ref, Purpose: purpose, Overrides: overrides, Private: private})
		if run.ID != "" {
			if presentErr := e.presentRun(command.OutOrStdout(), run); presentErr != nil {
				return presentErr
			}
		}
		if err != nil {
			return failure(1, fmt.Sprintf("%s agent: %s", purpose, err.Error()))
		}
		return nil
	}
	return command
}

func (e *execution) agentService(deps Dependencies, durable *store.Store, directory string, client *github.Client, policy config.Snapshot) (dispatch.Service, error) {
	locks, err := feedbackModifierDirectory()
	if err != nil {
		return dispatch.Service{}, err
	}
	launch := deps.LaunchAgent
	if launch == nil {
		launch = agent.Run
	}
	return dispatch.Service{Store: durable, GitHub: client, LockDirectory: locks, Launch: func(ctx context.Context, request agent.Request) (agent.Output, error) {
		slot, err := watch.AcquireSlot(ctx, directory, int(policy.Int("limits.max_concurrent_reviews")))
		if err != nil {
			return agent.Output{}, err
		}
		defer slot.Close()
		return launch(ctx, request)
	}}, nil
}

func (e *execution) itemExecutor(deps Dependencies, durable *store.Store, directory string) func(context.Context, *config.Document, *github.Client, store.InboxItem) (store.Run, error) {
	return func(ctx context.Context, document *config.Document, client *github.Client, item store.InboxItem) (store.Run, error) {
		policy, err := config.Resolve(document, item.Repository, nil, item.Overrides)
		if err != nil {
			return store.Run{}, err
		}
		service, err := e.agentService(deps, durable, directory, client, policy)
		if err != nil {
			return store.Run{}, err
		}
		purpose := "review"
		if item.ExecutionKind == "feedback_v1" {
			purpose = "feedback"
		}
		return service.Run(ctx, document, dispatch.Job{Target: github.PullReference{Repository: item.Repository, Number: item.Number, URL: dispatch.PRURL(item)}, Purpose: purpose, Item: &item, Overrides: item.Overrides})
	}
}
