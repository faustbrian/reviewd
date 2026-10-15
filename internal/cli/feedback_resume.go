package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/notify"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

func (e *execution) feedbackResumeCommand(deps Dependencies) *cobra.Command {
	var confirmation string
	command := &cobra.Command{Use: "feedback-resume <run-id>", Short: "Preview continuation of the same retained feedback run; --confirm performs it", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&confirmation, "confirm", "", "Continue only the exact displayed existing run/outcome; uncertain effects are never replayed")
	command.RunE = func(command *cobra.Command, args []string) error {
		if command.Flags().Changed("model") || command.Flags().Changed("reasoning-effort") {
			return failure(2, "feedback continuation retains the original run's frozen model and effort; replacements are not supported")
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
		run, err := durable.Get(command.Context(), args[0])
		if err != nil {
			return failure(1, err.Error())
		}
		expected := feedback.ResumeConfirmation(run)
		if expected == "" {
			return failure(1, "select an existing pending/partial feedback run with its retained original owner and work")
		}
		allowed := false
		for _, repository := range defaults.Strings("github.repositories") {
			if repository == run.Repository {
				allowed = true
			}
		}
		if !allowed {
			return failure(1, "feedback project is no longer in this profile's repository allowlist")
		}
		var frozen config.Snapshot
		if err = frozen.UnmarshalJSON(run.Configuration); err != nil {
			return failure(1, "original feedback configuration is unavailable")
		}
		overrides := map[string]any{}
		for _, key := range []string{"feedback.model", "feedback.reasoning_effort"} {
			if frozen.Provenance()[key].Layer == "cli" {
				overrides[key] = frozen.String(key)
			}
		}
		policy, err := config.Resolve(document, run.Repository, nil, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		current, _ := json.Marshal(policy)
		if !bytes.Equal(current, run.Configuration) {
			return failure(1, "restore the run's frozen operator configuration before continuing; no new model or authority is substituted")
		}
		if !command.Flags().Changed("confirm") {
			report := struct {
				SchemaVersion int       `json:"schema_version"`
				Run           store.Run `json:"run"`
				Scope         string    `json:"scope"`
				Model         string    `json:"model"`
				Effort        string    `json:"reasoning_effort"`
				Confirmation  string    `json:"confirmation"`
				State         string    `json:"state"`
			}{1, run, feedbackScope, policy.String("feedback.model"), policy.String("feedback.reasoning_effort"), expected, "preview_only"}
			safe, _, err := safeReport(report)
			if err != nil {
				return err
			}
			if e.json {
				return writeJSON(command.OutOrStdout(), safe)
			}
			if err = e.presentRun(command.OutOrStdout(), run); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "\n%s\nContinue the same retained run with its original model/effort. Recorded uncertain effects are checked, never repeated. No permission recorded.\n", safe.Scope)
			parts := []string{"reviewd", "--config", path, "--profile", e.profile, "feedback-resume", run.ID, "--confirm", expected}
			for i, value := range parts {
				parts[i] = "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
			}
			copyable := strings.Join(parts, " ")
			if redact(copyable) == copyable {
				fmt.Fprintln(command.OutOrStdout(), copyable)
			} else {
				fmt.Fprintf(command.OutOrStdout(), "Using this config/profile, add --confirm %s.\n", expected)
			}
			return nil
		}
		if confirmation == "" || confirmation != expected {
			return failure(1, "displayed feedback run/outcome changed; preview it again before continuing")
		}
		client, err := deps.GitHub(command.Context(), policy.String("github.host"))
		if err != nil {
			return failure(1, err.Error())
		}
		service, err := e.feedbackService(deps, durable, directory, policy, client)
		if err != nil {
			return failure(1, err.Error())
		}
		resumed, resumeErr := service.Resume(command.Context(), document, run.ID, overrides, feedback.Authorization{Kind: "manual_feedback_v1", Confirmation: confirmation})
		if resumed.ID != "" {
			items, _, listErr := durable.Inbox(command.Context())
			resumeErr = errors.Join(resumeErr, listErr)
			for _, item := range items {
				if item.RunID == run.ID {
					resumeErr = errors.Join(resumeErr, durable.FinishItem(command.Context(), item, resumed))
				}
			}
			notifications := notify.Service{Store: durable, Desktop: deps.Desktop}
			resumeErr = errors.Join(resumeErr, notifications.Pump(command.Context(), document, false), e.presentRun(command.OutOrStdout(), resumed))
		}
		if resumeErr != nil {
			return failure(1, resumeErr.Error())
		}
		if resumed.State != store.FeedbackDelivered {
			return failure(1, "feedback continuation remains pending/partial; inspect its recorded outcome")
		}
		return nil
	}
	return command
}
