package cli

import (
	"fmt"
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/spf13/cobra"
	"time"
)

func (e *execution) cleanupCommand() *cobra.Command {
	dry := false
	command := &cobra.Command{Use: "cleanup", Short: "Expire old completed agent text, retaining identities and uncertain work", Args: cobra.NoArgs}
	command.Flags().BoolVar(&dry, "dry-run", false, "Show eligible completed native runs without changing history")
	command.RunE = func(command *cobra.Command, _ []string) error {
		durable, policy, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		report, err := dispatch.ExpireHistory(command.Context(), durable, int(policy.Int("state.retention_days")), dry, time.Now())
		if err != nil {
			return failure(1, err.Error())
		}
		if e.json {
			return writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "retention": report, "dry_run": dry})
		}
		if dry {
			fmt.Fprintf(command.OutOrStdout(), "%d completed native runs have output eligible to expire; no history changed.\n", len(report.Eligible))
		} else {
			fmt.Fprintf(command.OutOrStdout(), "Expired output for %d completed native runs; identities and uncertain history remain.\n", len(report.Expired))
		}
		return nil
	}
	return command
}
