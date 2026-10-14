package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/spf13/cobra"
)

func (e *execution) inputsCommand() *cobra.Command {
	command := &cobra.Command{Use: "inputs [run-id]", Short: "Inspect private clarification questions and exact answer targets", Args: cobra.MaximumNArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		runID := ""
		if len(args) == 1 {
			runID = args[0]
		}
		inputs, err := durable.Inputs(command.Context(), runID)
		if err != nil {
			return failure(1, err.Error())
		}
		if e.json {
			return writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "inputs": inputs})
		}
		if len(inputs) == 0 {
			fmt.Fprintln(command.OutOrStdout(), "No clarification requests.")
		}
		for _, input := range inputs {
			fmt.Fprintf(command.OutOrStdout(), "Input %s · %s\nRun: %s\nParticipant: %s\nThread: %q\nTurn: %q\n", input.ID, input.State, input.RunID, input.Participant, input.ThreadID, input.TurnID)
			if input.TextExpiredAt != "" {
				fmt.Fprintln(command.OutOrStdout(), "Private clarification text expired; delivery facts remain. No answer can be selected.")
				continue
			}
			for _, q := range input.Questions {
				fmt.Fprintf(command.OutOrStdout(), "%q: %s\n", q.ID, terminalText(q.Question))
				for _, o := range q.Options {
					fmt.Fprintf(command.OutOrStdout(), "  %s — %s\n", terminalText(o.Label), terminalText(o.Description))
				}
			}
			fmt.Fprintln(command.OutOrStdout(), "Answer with reviewd answer", input.ID, "--from-file <answers.json|->. An answer never grants permissions or publication consent.")
		}
		return nil
	}
	return command
}
func (e *execution) answerCommand() *cobra.Command {
	var path string
	command := &cobra.Command{Use: "answer <input-id>", Short: "Explicitly select textual answers for one exact pending clarification", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&path, "from-file", "", "Complete answers JSON from a private file, or - for stdin")
	command.RunE = func(command *cobra.Command, args []string) error {
		if path == "" {
			return failure(2, "use --from-file <private answers.json|->; answers are not accepted as command-line values")
		}
		var reader io.Reader = command.InOrStdin()
		if path != "-" {
			file, err := os.Open(path)
			if err != nil {
				return failure(2, "cannot open private answer file")
			}
			defer file.Close()
			reader = file
		}
		raw, err := io.ReadAll(io.LimitReader(reader, clarification.MaxPayload+1))
		if err != nil {
			return failure(2, "cannot read private answers")
		}
		var answers clarification.Answers
		if err = clarification.Decode(raw, &answers); err != nil {
			return failure(2, err.Error())
		}
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		input, err := durable.SelectInput(command.Context(), args[0], answers)
		if err != nil {
			return failure(1, err.Error())
		}
		if e.json {
			return writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "input": input, "note": "Selected locally; transport delivery is not remote acceptance."})
		}
		fmt.Fprintln(command.OutOrStdout(), "Answer selected for", input.ID, "— local selection is not remote acceptance.")
		return nil
	}
	return command
}
