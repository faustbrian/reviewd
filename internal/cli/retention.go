package cli

import (
	"fmt"
	"time"

	"github.com/faustbrian/reviewd/internal/review"
	"github.com/spf13/cobra"
)

func (e *execution) cleanupCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{Use: "cleanup", Short: "Expire old terminal private review text, keeping summaries and unresolved work", Args: cobra.NoArgs}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Show eligible history without expiring any private text")
	command.RunE = func(command *cobra.Command, _ []string) error {
		durable, policy, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		report, cleanupErr := review.ExpireHistory(command.Context(), durable, int(policy.Int("state.retention_days")), dryRun, time.Now())
		if e.json {
			if err := writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "retention": report}); err != nil {
				return err
			}
		} else if dryRun {
			fmt.Fprintf(command.OutOrStdout(), "%d terminal reviews have private text eligible to expire; no history changed.\n", len(report.Eligible))
		} else {
			fmt.Fprintf(command.OutOrStdout(), "Expired private text for %d terminal reviews; summaries and unresolved work remain.\n", len(report.Expired))
		}
		if !e.json && report.BatchFull {
			fmt.Fprintln(command.OutOrStdout(), "One bounded batch was inspected; repeat cleanup to check for more old history.")
		}
		if cleanupErr != nil {
			return failure(1, cleanupErr.Error())
		}
		return nil
	}
	return command
}
