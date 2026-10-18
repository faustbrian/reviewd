package cli

import (
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) decisionCommand(start bool) *cobra.Command {
	name, description := "skip", "Skip this event without affecting a future request or revision"
	if start {
		name, description = "start", "Queue the configured skill for this exact event; a watcher processes the choice"
	}
	var expectedEvent string
	command := &cobra.Command{Use: name + " <item-id>", Short: description, Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&expectedEvent, "event", "", "Require the exact displayed event identity; changed work needs a new decision")
	command.RunE = func(command *cobra.Command, args []string) error {
		document, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		overrides := e.overrides(command)
		if !start && len(overrides) != 0 {
			return failure(2, "skip does not select a reviewer model")
		}
		defaults, err := config.ResolveDefaults(document, nil)
		if err != nil {
			return failure(2, err.Error())
		}
		directory, err := stateDirectory(defaults, path)
		if err != nil {
			return failure(1, err.Error())
		}
		durable, err := store.Open(command.Context(), directory)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		items, scopes, err := durable.Inbox(command.Context())
		if err != nil {
			return failure(1, err.Error())
		}
		var selected *store.InboxItem
		for _, item := range items {
			if item.ID == args[0] {
				copy := item
				selected = &copy
				break
			}
		}
		if selected == nil {
			for _, scope := range scopes {
				if scope.State != "ok" {
					continue
				}
				stored, err := durable.DiscoveryItems(command.Context(), scope.Repository, scope.ActorID)
				if err != nil {
					return failure(1, err.Error())
				}
				for _, item := range stored {
					if item.ID == args[0] {
						copy := item
						selected = &copy
						break
					}
				}
			}
		}
		if selected == nil {
			return failure(1, "current review item was not found; synchronize before deciding")
		}
		if selected.ExecutionKind == "feedback_v1" {
			overrides = e.feedbackOverrides(command)
		}
		if _, err := config.Resolve(document, selected.Repository, nil, overrides); err != nil {
			return failure(2, err.Error())
		}
		var expected []string
		if command.Flags().Changed("event") {
			expected = []string{expectedEvent}
		}
		if _, err := durable.DecideItem(command.Context(), args[0], start, false, overrides, expected...); err != nil {
			return failure(1, err.Error())
		}
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}
