package cli

import (
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) controlStore(command *cobra.Command) (*store.Store, config.Snapshot, error) {
	if len(e.overrides(command)) != 0 {
		return nil, config.Snapshot{}, failure(2, "personal controls do not select a reviewer model")
	}
	document, path, err := e.load(command, false)
	if err != nil {
		return nil, config.Snapshot{}, failure(2, err.Error())
	}
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		return nil, defaults, failure(2, err.Error())
	}
	directory, err := stateDirectory(defaults, path)
	if err != nil {
		return nil, defaults, failure(1, err.Error())
	}
	durable, err := store.Open(command.Context(), directory)
	if err != nil {
		return nil, defaults, failure(1, err.Error())
	}
	return durable, defaults, nil
}

func (e *execution) projectPauseCommand(paused bool) *cobra.Command {
	name, description := "resume", "Resume a project under its existing policy and decisions"
	if paused {
		name, description = "pause", "Pause scheduled work, automatic publication and notifications for a project"
	}
	command := &cobra.Command{Use: name + " <owner/repo>", Short: description, Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, defaults, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		repository, err := config.Repository(defaults.String("github.host"), args[0])
		if err != nil {
			return failure(2, err.Error())
		}
		allowed := false
		for _, configured := range defaults.Strings("github.repositories") {
			if configured == repository {
				allowed = true
			}
		}
		if !allowed {
			return failure(2, "project must be in this profile's github.repositories allowlist")
		}
		if err := durable.PauseProject(command.Context(), repository, paused); err != nil {
			return failure(1, err.Error())
		}
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}

func (e *execution) snoozeCommand() *cobra.Command {
	var duration time.Duration
	var untilHead bool
	command := &cobra.Command{Use: "snooze <item-id>", Short: "Hold one inbox category without losing its decisions or prepared review", Args: cobra.ExactArgs(1)}
	command.Flags().DurationVar(&duration, "for", 0, "Snooze for a positive duration, such as 2h")
	command.Flags().BoolVar(&untilHead, "until-head", false, "Snooze until the PR head changes; same-head re-requests do not wake it")
	command.RunE = func(command *cobra.Command, args []string) error {
		if command.Flags().Changed("for") == untilHead || (!untilHead && duration <= 0) {
			return failure(2, "choose exactly one of --for <positive-duration> or --until-head")
		}
		durable, _, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		now := time.Now().UTC()
		until := time.Time{}
		if !untilHead {
			until = now.Add(duration)
		}
		if err := durable.SnoozeItem(command.Context(), args[0], until, untilHead, now); err != nil {
			return failure(1, err.Error())
		}
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}

func (e *execution) unsnoozeCommand() *cobra.Command {
	command := &cobra.Command{Use: "unsnooze <item-id>", Short: "Release a named snooze without authorizing new work", Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, _, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		if err := durable.UnsnoozeItem(command.Context(), args[0]); err != nil {
			return failure(1, err.Error())
		}
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}
