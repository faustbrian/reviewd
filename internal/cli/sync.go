package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/discovery"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) syncCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "sync", Short: "Synchronize the configured review inbox without starting a reviewer", Args: cobra.NoArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		if len(e.overrides(command)) != 0 {
			return failure(2, "sync does not start reviews; select model overrides when starting a review")
		}
		document, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		policy, err := config.ResolveDefaults(document, nil)
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
		err = (discovery.Service{Connect: deps.GitHub, Store: durable}).Sync(command.Context(), document)
		if presentErr := e.presentInbox(command.Context(), command.OutOrStdout(), durable); presentErr != nil {
			return presentErr
		}
		if err != nil {
			return failure(1, err.Error())
		}
		return nil
	}
	return command
}

func (e *execution) presentInbox(ctx context.Context, output io.Writer, durable *store.Store) error {
	runs, err := durable.List(ctx)
	if err != nil {
		return failure(1, err.Error())
	}
	items, scopes, err := durable.Inbox(ctx)
	if err != nil {
		return failure(1, err.Error())
	}
	if e.json {
		value := map[string]any{"schema_version": 1, "runs": runs, "items": items, "discovery": scopes}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		safe := redact(string(data))
		var result map[string]json.RawMessage
		if json.Unmarshal([]byte(safe), &result) != nil {
			return failure(1, "cannot present a safely redacted inbox")
		}
		result["redacted"] = json.RawMessage("false")
		if safe != string(data) {
			result["redacted"] = json.RawMessage("true")
		}
		return writeJSON(output, result)
	}
	failed := false
	pending := false
	for _, scope := range scopes {
		if scope.State == "failed" {
			failed = true
			fmt.Fprintf(output, "Discovery failed for %q: %s\n", redact(scope.Repository), terminalText(scope.Failure))
		}
		if scope.State == "pending" {
			pending = true
			fmt.Fprintf(output, "Discovery has not completed for %q.\n", redact(scope.Repository))
		}
	}
	for _, item := range items {
		fmt.Fprintf(output, "%s  %s  %q #%d  %s\n  %s\n", item.ID, item.Category, redact(item.Repository), item.Number, item.State, terminalText(item.Explanation))
	}
	for _, run := range runs {
		fmt.Fprintf(output, "%s  %q #%d  %s\n", run.ID, redact(run.Repository), run.Number, run.State)
	}
	if len(items) == 0 && len(runs) == 0 {
		if failed {
			fmt.Fprintln(output, "No cached work; failed discovery is not an empty inbox.")
		} else if pending {
			fmt.Fprintln(output, "No cached work; synchronization has not completed.")
		} else if len(scopes) == 0 {
			fmt.Fprintln(output, "No local review runs. Monitoring is not enabled by this command.")
		} else {
			fmt.Fprintln(output, "No actionable work in the last successful synchronization.")
		}
	}
	return nil
}

func (e *execution) followCommand(following bool) *cobra.Command {
	name, description := "unfollow", "Stop follow-up review work; new explicit requests may still appear"
	if following {
		name, description = "follow", "Follow an inbox PR after its original request disappears"
	}
	command := &cobra.Command{Use: name + " <item-id>", Short: description, Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		if len(e.overrides(command)) != 0 {
			return failure(2, "follow settings do not start a review")
		}
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		if err := durable.SetFollow(command.Context(), args[0], following); err != nil {
			return failure(1, err.Error())
		}
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}
