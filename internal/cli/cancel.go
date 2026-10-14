package cli

import "github.com/spf13/cobra"

func (e *execution) cancelCommand() *cobra.Command {
	command := &cobra.Command{Use: "cancel <run-id>", Short: "Request cancellation of one queued, waiting or active private review run", Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, _, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		run, err := durable.RequestCancellation(command.Context(), args[0])
		if err != nil {
			return failure(1, err.Error())
		}
		return e.presentRun(command.OutOrStdout(), run)
	}
	return command
}
