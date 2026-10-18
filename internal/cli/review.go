package cli

import (
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"github.com/spf13/cobra"
	"io"
	"strings"
)

func (e *execution) localStore(command *cobra.Command) (*store.Store, error) {
	document, path, err := e.load(command, false)
	if err != nil {
		return nil, err
	}
	policy, err := config.ResolveDefaults(document, e.overrides(command))
	if err != nil {
		return nil, err
	}
	directory, err := stateDirectory(policy, path)
	if err != nil {
		return nil, err
	}
	return store.Open(command.Context(), directory)
}
func (e *execution) reviewCommand(deps Dependencies) *cobra.Command {
	return e.skillCommand("review", deps)
}

func (e *execution) showCommand() *cobra.Command {
	command := &cobra.Command{Use: "show <run-id>", Short: "Show the agent output and actual durable state", Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		run, err := durable.Get(command.Context(), args[0])
		if err != nil {
			return failure(1, err.Error())
		}
		return e.presentRun(command.OutOrStdout(), run)
	}
	return command
}
func (e *execution) inboxCommand(deps Dependencies) *cobra.Command {
	var interactive bool
	command := &cobra.Command{Use: "inbox", Short: "List review and feedback work in the selected profile", Args: cobra.NoArgs}
	command.Flags().BoolVar(&interactive, "interactive", false, "Attach the optional keyboard inbox; redirected/JSON output stays plain")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if interactive && !e.json && terminal.IsTerminal(command.InOrStdin(), command.OutOrStdout()) {
			return e.attachInbox(command, deps, false)
		}
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}

func terminalText(text string) string {
	var result strings.Builder
	for _, char := range redact(text) {
		if (char < 32 && char != '\n' && char != '\t') || char == 127 {
			fmt.Fprintf(&result, "\\u%04x", char)
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}
func (e *execution) presentRun(output io.Writer, run store.Run) error {
	if e.json {
		data, err := json.Marshal(run)
		if err != nil {
			return failure(1, "could not present durable agent state")
		}
		safe := redact(string(data))
		return writeJSON(output, map[string]any{"schema_version": 1, "run": json.RawMessage(safe), "redacted": safe != string(data)})
	}
	fmt.Fprintf(output, "%s agent %s\nRepository: %s #%d\nState: %s\n", terminalText(run.Purpose), run.ID, terminalText(run.Repository), run.Number, run.State)
	if !store.IsNativeExecutor(run.Executor) {
		fmt.Fprintln(output, "Historical workflow run; retained read-only and never automatically replayed.")
		encoded, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(output, terminalText(string(encoded)))
		return nil
	}
	if run.Archive != nil {
		fmt.Fprintf(output, "Agent text expired at %s; run identity remains.\n", terminalText(run.Archive.ExpiredAt))
	}
	fmt.Fprintf(output, "Backend: %s\n", agent.Label(run.Backend()))
	if run.ThreadID != "" {
		fmt.Fprintf(output, "%s session: %s\n", agent.Label(run.Backend()), terminalText(run.ThreadID))
	}
	if run.RequestedModel != "" {
		fmt.Fprintf(output, "Requested model: %s\n", terminalText(run.RequestedModel))
	}
	if run.Progress != "" {
		fmt.Fprintf(output, "Activity: %s\n", terminalText(run.Progress))
	}
	if run.CancellationRequested && run.State == store.AgentRunning {
		fmt.Fprintln(output, "Cancellation requested; waiting for the agent to stop.")
	}
	var outcome agent.Output
	if len(run.Outcome) > 0 && json.Unmarshal(run.Outcome, &outcome) != nil {
		return failure(1, "stored agent outcome is invalid")
	}
	if outcome.Model != "" {
		fmt.Fprintf(output, "Native model: %s\n", terminalText(outcome.Model))
	}
	if outcome.Text != "" {
		fmt.Fprintln(output, terminalText(outcome.Text))
	}
	if outcome.Diagnostic != "" {
		fmt.Fprintln(output, terminalText(outcome.Diagnostic))
	}
	if run.Failure != "" {
		fmt.Fprintln(output, terminalText(run.Failure))
	}
	if run.State == store.AgentCompleted {
		fmt.Fprintf(output, "%s execution completed. Delivery follows the selected skill; reviewd does not verify those effects.\n", agent.Label(run.Backend()))
	}
	return nil
}
