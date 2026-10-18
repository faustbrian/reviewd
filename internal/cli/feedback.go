package cli

import "github.com/spf13/cobra"

func (e *execution) feedbackOverrides(command *cobra.Command) map[string]any {
	result := map[string]any{}
	if command.Flags().Changed("backend") {
		result["feedback.backend"] = e.backend
	}
	if command.Flags().Changed("executable") {
		result["feedback.executable"] = e.executable
	}
	if command.Flags().Changed("model") {
		result["feedback.model"] = e.model
	}
	if command.Flags().Changed("reasoning-effort") {
		result["feedback.reasoning_effort"] = e.effort
	}
	return result
}
