package cli

import (
	"errors"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) publishCommand(deps Dependencies) *cobra.Command {
	expected := ""
	repair := false
	command := &cobra.Command{Use: "publish <run-id>", Short: "Publish an explicitly selected preview and verify GitHub readback", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&expected, "payload", "", "Exact preview identity authorized for publication; omission only refreshes its preview")
	command.Flags().BoolVar(&repair, "repair", false, "Explicitly repair one missing inline finding after canonical reconciliation")
	command.RunE = func(command *cobra.Command, args []string) error {
		if len(e.overrides(command)) != 0 {
			return failure(2, "publication preserves the source review; use review to change reviewer settings")
		}
		document, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
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
		client, err := deps.GitHub(command.Context(), defaults.String("github.host"))
		if err != nil {
			return failure(1, err.Error())
		}
		publisher := review.Publisher{GitHub: client, Store: durable, Source: deps.Source}
		var run store.Run
		var publishErr error
		if repair {
			run, publishErr = publisher.Repair(command.Context(), document, args[0], expected)
		} else {
			run, publishErr = publisher.Publish(command.Context(), document, args[0], expected, false)
		}
		if run.ID != "" {
			if err := e.presentRun(command.OutOrStdout(), run); err != nil {
				return err
			}
		}
		if publishErr != nil {
			return failure(1, publishErr.Error())
		}
		if expected == "" && run.State == store.Prepared {
			return failure(2, "no publication decision was granted; inspect this preview and rerun publish with --payload "+run.PayloadID)
		}
		if run.ID == "" {
			return errors.New("publication returned no durable state")
		}
		return nil
	}
	return command
}
