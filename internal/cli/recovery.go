package cli

import (
	"fmt"

	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) recoveryCommand() *cobra.Command {
	var dry bool
	command := &cobra.Command{Use: "recover", Short: "Inspect private owners and recover only proven interrupted work", Args: cobra.NoArgs}
	command.Flags().BoolVar(&dry, "dry-run", false, "Classify owners without changing history or private storage")
	command.RunE = func(command *cobra.Command, _ []string) error {
		durable, _, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		report, err := durable.RecoverPrivate(command.Context(), dry)
		if presentErr := e.presentRecovery(command, report); presentErr != nil {
			return presentErr
		}
		if err != nil {
			return failure(1, err.Error())
		}
		if report.NeedsAttention() {
			e.resultCode = 1
		}
		return nil
	}
	return command
}

func (e *execution) presentRecovery(command *cobra.Command, report store.RecoveryReport) error {
	if e.json {
		return writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "recovery": report})
	}
	for _, owner := range report.Owners {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "Private owner %s: %s", owner.ID, owner.State); err != nil {
			return err
		}
		if report.DryRun {
			fmt.Fprint(command.OutOrStdout(), " (dry run)")
		}
		fmt.Fprintln(command.OutOrStdout())
		if owner.Storage != "" {
			fmt.Fprintln(command.OutOrStdout(), "  Storage retained:", terminalText(owner.Storage))
		}
	}
	for _, id := range report.UnknownRuns {
		fmt.Fprintf(command.OutOrStdout(), "Review %s: owner unknown; history preserved, no session resumed.\n", id)
	}
	if len(report.Owners) == 0 && len(report.UnknownRuns) == 0 {
		fmt.Fprintln(command.OutOrStdout(), "No interrupted private owners found.")
	}
	if report.BatchFull {
		fmt.Fprintln(command.OutOrStdout(), "This bounded pass is full; repeat recover to inspect further work.")
	}
	return nil
}

func (e *execution) recoverBeforeWork(command *cobra.Command, durable *store.Store) {
	report, err := durable.RecoverPrivate(command.Context(), false)
	recovered := 0
	for _, owner := range report.Owners {
		if owner.State == "recovered" {
			recovered++
		}
	}
	if recovered > 0 {
		fmt.Fprintf(command.ErrOrStderr(), "Recovered %d interrupted private owners; history remains in reviewd show.\n", recovered)
	}
	if err != nil || report.NeedsAttention() {
		fmt.Fprintln(command.ErrOrStderr(), "Some private owners need attention; inspect reviewd recover. No session was resumed.")
	}
}
