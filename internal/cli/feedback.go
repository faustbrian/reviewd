package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/spf13/cobra"
)

const feedbackScope = "Authorize verified feedback fixes, normal commit hooks/signing and additive ordinary push to this existing PR branch; necessary metadata, original replies, resolve only fully settled concerns, and renewed prior/current reviewer requests. No merge, deployment, force-push, rebase, hook bypass or unrelated work."

// This is a projection of the existing transactional FeedbackDecision, not a
// bearer credential or a separate authorization store. Reading a preview never
// records consent; the explicit command and the store CAS are both required.
func feedbackConfirmation(item store.InboxItem) string {
	if item.Category != "feedback_handling" || item.ExecutionKind != "feedback_v1" || item.ID == "" || item.EventKey == "" || item.HeadSHA == "" || item.TargetBaseSHA == "" || item.ContextKey == "" {
		return ""
	}
	raw, _ := json.Marshal([]any{"feedback_v1", item.ID, item.Repository, item.PullID, item.ActorID, item.EventKey, item.HeadSHA, item.TargetBaseSHA, item.ContextKey})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (e *execution) feedbackOverrides(command *cobra.Command) map[string]any {
	result := map[string]any{}
	if command.Flags().Changed("model") {
		result["feedback.model"] = e.model
	}
	if command.Flags().Changed("reasoning-effort") {
		result["feedback.reasoning_effort"] = e.effort
	}
	return result
}

func (e *execution) feedbackCommand() *cobra.Command {
	var confirmation string
	command := &cobra.Command{Use: "feedback <item-id>", Short: "Preview separate feedback-fixing permission; --confirm queues the exact displayed target", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&confirmation, "confirm", "", "Explicitly authorize fix/commit/push/metadata/reply/settled-resolution/re-review scope using the displayed confirmation identity")
	command.RunE = func(command *cobra.Command, args []string) error {
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
		items, _, err := durable.Inbox(command.Context())
		if err != nil {
			return failure(1, err.Error())
		}
		var selected *store.InboxItem
		for _, item := range items {
			if item.ID == args[0] {
				copy := item
				selected = &copy
				break
			}
		}
		if selected == nil || feedbackConfirmation(*selected) == "" {
			return failure(1, "current typed feedback item with a complete target was not found; synchronize before deciding")
		}
		allowed := false
		for _, repository := range defaults.Strings("github.repositories") {
			if repository == selected.Repository {
				allowed = true
			}
		}
		if !allowed {
			return failure(1, "feedback project is no longer in this profile's repository allowlist")
		}
		overrides := e.feedbackOverrides(command)
		policy, err := config.Resolve(document, selected.Repository, nil, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		if !policy.FeedbackMonitoringConfigured() || len(policy.Strings("feedback.authors")) == 0 || policy.FeedbackAction(selected.Trigger) == "ignore" {
			return failure(1, "feedback monitoring is not enabled for this project; synchronize its current operator policy")
		}
		if selected.RunID != "" {
			return failure(1, "feedback already has a linked run; inspect and reconcile that run rather than authorizing a replacement")
		}
		expected := feedbackConfirmation(*selected)
		if command.Flags().Changed("confirm") {
			if confirmation == "" || confirmation != expected {
				return failure(1, "displayed feedback target changed or confirmation is incomplete; preview its current head, base and inventory again")
			}
			if _, err := durable.DecideFeedbackItem(command.Context(), selected.ID, overrides, store.FeedbackDecision{EventKey: selected.EventKey, HeadSHA: selected.HeadSHA, TargetBaseSHA: selected.TargetBaseSHA, ContextKey: selected.ContextKey}); err != nil {
				return failure(1, err.Error())
			}
			return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
		}
		report := struct {
			SchemaVersion int             `json:"schema_version"`
			Item          store.InboxItem `json:"item"`
			Scope         string          `json:"scope"`
			Model         string          `json:"model"`
			Effort        string          `json:"reasoning_effort"`
			Confirmation  string          `json:"confirmation"`
			State         string          `json:"state"`
		}{1, *selected, feedbackScope, policy.String("feedback.model"), policy.String("feedback.reasoning_effort"), expected, "preview_only"}
		safe, _, err := safeReport(report)
		if err != nil {
			return err
		}
		if e.json {
			return writeJSON(command.OutOrStdout(), safe)
		}
		fmt.Fprintf(command.OutOrStdout(), "Feedback permission preview: %s #%d\nEvent: %s\nHead: %s\nBase: %s\nHuman feedback: %s\nModel: %s / %s\n\n%s\n\nNo permission recorded. Confirmation queues work for a watcher; an unavailable executor keeps it held.\n", safe.Item.Repository, safe.Item.Number, safe.Item.EventKey, safe.Item.HeadSHA, safe.Item.TargetBaseSHA, safe.Item.ContextKey, safe.Model, safe.Effort, safe.Scope)
		parts := []string{"reviewd", "--config", path, "--profile", e.profile, "feedback", selected.ID, "--confirm", expected}
		if command.Flags().Changed("model") {
			parts = append(parts, "--model", e.model)
		}
		if command.Flags().Changed("reasoning-effort") {
			parts = append(parts, "--reasoning-effort", e.effort)
		}
		for i, value := range parts {
			parts[i] = "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
		copyable := strings.Join(parts, " ")
		if redact(copyable) != copyable {
			fmt.Fprintf(command.OutOrStdout(), "Using the same config/profile and chosen model arguments, add --confirm %s. The full command is omitted because it contains a sensitive-shaped value.\n", expected)
		} else {
			fmt.Fprintln(command.OutOrStdout(), "To authorize this exact target and scope:\n"+copyable)
		}
		return nil
	}
	return command
}
