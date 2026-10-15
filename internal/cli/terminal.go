package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"github.com/spf13/cobra"
)

func (e *execution) attachInbox(command *cobra.Command, deps Dependencies, watching bool) error {
	durable, _, err := e.controlStore(command)
	if err != nil {
		return err
	}
	defer durable.Close()
	_, path, err := e.load(command, false)
	if err != nil {
		return failure(2, err.Error())
	}
	base := []string{"--config", path, "--profile", e.profile}
	backend := terminal.Backend{
		Load: func(ctx context.Context) (terminal.Snapshot, error) {
			document, _, err := e.load(command, false)
			if err != nil {
				return terminal.Snapshot{}, err
			}
			return e.terminalSnapshot(ctx, durable, document)
		},
		Act: func(ctx context.Context, action terminal.Action) (string, error) {
			return terminalAction(ctx, base, action, deps)
		},
	}
	if watching {
		backend.Watch = func(ctx context.Context, diagnostics io.Writer) int {
			args := append(append([]string{}, base...), "watch", "--headless", "--json")
			// Exactly the existing application watcher. The attachment never ticks
			// another scheduler or owns publication/review policy.
			return Run(ctx, args, io.Discard, diagnostics, deps)
		}
	}
	if err := terminal.Run(command.Context(), command.InOrStdin(), command.OutOrStdout(), backend); err != nil {
		if errors.Is(err, terminal.ErrInterrupted) {
			return failure(130, "terminal attachment interrupted; unconfirmed choices were not granted")
		}
		return failure(1, err.Error())
	}
	return nil
}

func (e *execution) terminalSnapshot(ctx context.Context, durable *store.Store, document *config.Document) (terminal.Snapshot, error) {
	runs, err := durable.List(ctx)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	items, scopes, err := durable.Inbox(ctx)
	if err != nil {
		return terminal.Snapshot{}, err
	}
	controls, err := durable.Controls(ctx, time.Now())
	if err != nil {
		return terminal.Snapshot{}, err
	}
	paused := map[string]bool{}
	for _, repository := range controls.PausedProjects {
		paused[repository] = true
	}
	byRun := map[string]store.Run{}
	used := map[string]bool{}
	for _, run := range runs {
		byRun[run.ID] = run
	}
	result := terminal.Snapshot{Status: "Cached local inbox; start choices queue private work for a watcher."}
	for _, scope := range scopes {
		if scope.State != "ok" {
			result.Status = fmt.Sprintf("Discovery %s for %s: %s", scope.State, scope.Repository, scope.Failure)
		}
	}
	cooldowns, err := durable.GitHubCooldowns(ctx, time.Now())
	if err != nil {
		return terminal.Snapshot{}, err
	}
	for _, cooldown := range cooldowns {
		result.Status += fmt.Sprintf(" Automatic GitHub work for %s waits until %s (server retry guidance).", cooldown.Host, cooldown.Until.Format(time.RFC3339))
	}
	add := func(item *store.InboxItem, run *store.Run) error {
		entry := terminal.Entry{Section: "owed"}
		var text strings.Builder
		if item != nil {
			entry.ExecutionKind = item.ExecutionKind
			entry.FeedbackConfirmation = feedbackConfirmation(*item)
			entry.Key, entry.ItemID, entry.EventKey = "item:"+item.ID, item.ID, item.EventKey
			entry.Project, entry.State, entry.RunID = item.Repository, item.State, item.RunID
			entry.Title = fmt.Sprintf("%s #%d %s", strings.TrimPrefix(item.Repository, "github.com/"), item.Number, item.Title)
			feedback := item.Category == "feedback_handling" && item.ExecutionKind == "feedback_v1"
			if item.Category == "own_pr_feedback" || feedback {
				entry.Section = "feedback"
			}
			fmt.Fprintf(&text, "PR: %s #%d\nTitle: %q\nWhy: %s\nTrigger: %s\nAction policy: %s\nInbox state: %s\nHead: %s\nControlling base: %s\n", item.Repository, item.Number, item.Title, item.Explanation, item.Trigger, item.Action, item.State, item.HeadSHA, item.TargetBaseSHA)
			policy, err := config.Resolve(document, item.Repository, nil, nil)
			if err != nil {
				return err
			}
			prefix := "reviewer."
			if feedback {
				prefix = "feedback."
			}
			entry.Model, entry.Effort = policy.String(prefix+"model"), policy.String(prefix+"reasoning_effort")
			if feedback {
				fmt.Fprintf(&text, "Operator/project defaults: %s / %s\nThis is the separate feedback-handling workflow.\n%s\nEvent: %s\nHuman feedback: %s\nUse h for explicit confirmation. Queued work remains held while its executor is unavailable; private-review start never grants these permissions.\n", entry.Model, entry.Effort, feedbackScope, item.EventKey, item.ContextKey)
			} else {
				fmt.Fprintf(&text, "Operator/project defaults: %s / %s\nPublication policy: %s\nTrusted repository preferences are resolved at review start.\n", entry.Model, entry.Effort, policy.String("actions.publish_review"))
			}
			if len(item.Overrides) > 0 {
				chosen, err := config.Resolve(document, item.Repository, nil, item.Overrides)
				if err != nil {
					return err
				}
				fmt.Fprintf(&text, "Existing queued decision model/effort: %s / %s\n", chosen.String(prefix+"model"), chosen.String(prefix+"reasoning_effort"))
			}
			for _, hold := range controls.Snoozes {
				if hold.ItemID == item.ID {
					reason := "until new commits"
					if !hold.Until.IsZero() {
						reason = "until " + hold.Until.Format(time.RFC3339)
					}
					fmt.Fprintln(&text, "Snoozed:", reason)
				}
			}
			if item.Failure != "" {
				fmt.Fprintln(&text, "Attention:", item.Failure)
			}
			if item.ExecutionHold != "" {
				fmt.Fprintln(&text, "Execution held:", item.ExecutionHold)
			}
		}
		if run != nil {
			used[run.ID] = true
			if item == nil {
				entry.Key, entry.Project = "run:"+run.ID, run.Repository
				entry.Title = fmt.Sprintf("%s #%d · run %s", strings.TrimPrefix(run.Repository, "github.com/"), run.Number, run.ID[:min(8, len(run.ID))])
			}
			entry.RunID, entry.PayloadID = run.ID, run.PayloadID
			if item == nil || (item.State != "skipped" && !(item.State == "failed" && item.Attempts >= 3)) {
				entry.State = string(run.State)
			}
			var policy config.Snapshot
			if jsonErr := policy.UnmarshalJSON(run.Configuration); jsonErr == nil {
				prefix := "reviewer."
				if run.Purpose == "feedback" {
					prefix = "feedback."
				}
				fmt.Fprintf(&text, "Frozen run model/effort: %s / %s\n", policy.String(prefix+"model"), policy.String(prefix+"reasoning_effort"))
			}
			fmt.Fprintln(&text)
			if err := e.presentRun(&text, *run); err != nil {
				return err
			}
		}
		switch entry.State {
		case "queued", "reviewing", "waiting_checks", "retry_wait":
			entry.Section = "running"
		case "prepared", "publishing", "published", "publication_unknown", "partial":
			entry.Section = "publication"
		}
		entry.Paused = paused[entry.Project]
		if entry.Paused {
			fmt.Fprintln(&text, "Project paused: scheduled work, automatic publication and notifications are held.")
		}
		entry.Title, entry.Details = terminalText(entry.Title), terminalText(text.String())
		entry.Model, entry.Effort = terminalText(entry.Model), terminalText(entry.Effort)
		result.Entries = append(result.Entries, entry)
		return nil
	}
	for index := range items {
		item := &items[index]
		var linked *store.Run
		if run, exists := byRun[item.RunID]; exists {
			linked = &run
		}
		if err := add(item, linked); err != nil {
			return result, err
		}
	}
	for index := range runs {
		if !used[runs[index].ID] {
			if err := add(nil, &runs[index]); err != nil {
				return result, err
			}
		}
	}
	for _, run := range runs {
		for _, input := range run.Inputs {
			if input.State != "pending" || run.State != store.Reviewing || run.CancellationRequested || run.Superseded {
				continue
			}
			var details strings.Builder
			fmt.Fprintf(&details, "Input %s\nRun %s\nParticipant: %s\nThread: %q\nTurn: %q\n", input.ID, run.ID, input.Participant, input.ThreadID, input.TurnID)
			for _, q := range input.Questions {
				fmt.Fprintf(&details, "%q: %s\n", q.ID, q.Question)
				for _, o := range q.Options {
					fmt.Fprintf(&details, "  %s — %s\n", o.Label, o.Description)
				}
			}
			result.Entries = append(result.Entries, terminal.Entry{Key: "input:" + input.ID, Section: "running", State: "waiting_input", Title: terminalText("Clarification · " + run.Repository), Details: terminalText(details.String()), RunID: run.ID, InputID: input.ID, Questions: input.Questions, Project: run.Repository})
		}
	}
	rank := map[string]int{"running": 0, "publication": 1, "owed": 2, "feedback": 3}
	sort.SliceStable(result.Entries, func(i, j int) bool { return rank[result.Entries[i].Section] < rank[result.Entries[j].Section] })
	result.Status = terminalText(result.Status)
	return result, nil
}

// Dispatch through the ordinary commands so headless/JSON and keyboard users
// share validation, transactional decisions and the sole publication owner.
func terminalAction(ctx context.Context, base []string, action terminal.Action, deps Dependencies) (string, error) {
	entry := action.Entry
	var args []string
	message := "Your selected action was applied."
	switch action.Kind {
	case "answer":
		if entry.InputID == "" {
			return "", errors.New("select an exact pending clarification")
		}
		raw, err := json.Marshal(action.Answers)
		if err != nil {
			return "", errors.New("invalid clarification answers")
		}
		deps.Input = strings.NewReader(string(raw))
		args = []string{"answer", entry.InputID, "--from-file", "-"}
		message = "Answer selected locally; inspect its actual delivery state. This grants no execution or publication permission."
	case "start", "skip":
		if entry.ItemID == "" || entry.EventKey == "" {
			return "", errors.New("select a current inbox event")
		}
		args = []string{action.Kind, entry.ItemID, "--event", entry.EventKey}
		if action.Kind == "start" {
			if action.Model != "" {
				args = append(args, "--model", action.Model)
			}
			if action.Effort != "" {
				args = append(args, "--reasoning-effort", action.Effort)
			}
			if action.Now {
				args = append(args, "--now")
			}
			message = "Private review queued; publication remains a separate decision."
		}
	case "feedback":
		if entry.ItemID == "" || entry.ExecutionKind != "feedback_v1" || entry.FeedbackConfirmation == "" {
			return "", errors.New("select an exact displayed feedback target for its separate full-scope confirmation")
		}
		args = []string{"feedback", entry.ItemID, "--confirm", entry.FeedbackConfirmation}
		if action.Model != "" {
			args = append(args, "--model", action.Model)
		}
		if action.Effort != "" {
			args = append(args, "--reasoning-effort", action.Effort)
		}
		message = "Feedback permission queued for the displayed target; unavailable execution remains held. No delivery is implied."
	case "publish", "repair":
		if entry.RunID == "" || entry.PayloadID == "" {
			return "", errors.New("select an exact prepared preview")
		}
		args = []string{"publish", entry.RunID, "--payload", entry.PayloadID}
		if action.Kind == "repair" {
			args = append(args, "--repair")
		}
		message = "Publication operation returned; inspect the actual durable GitHub outcome below."
	case "reconcile":
		if entry.RunID == "" || entry.State == "prepared" {
			return "", errors.New("select an existing submitted/unknown publication; use v for private check refresh")
		}
		args = []string{"publish", entry.RunID}
		message = "Reconciliation returned; inspect its actual remote outcome below."
	case "refresh", "cancel":
		if entry.RunID == "" {
			return "", errors.New("select an existing run")
		}
		args = []string{action.Kind, entry.RunID}
		if action.Kind == "cancel" {
			message = "Cancellation requested; only the run's actual terminal state confirms that it stopped."
		}
	case "pause", "resume":
		args = []string{action.Kind, entry.Project}
	case "snooze":
		args = []string{"snooze", entry.ItemID, "--for", action.Duration}
	case "snooze-head":
		args = []string{"snooze", entry.ItemID, "--until-head"}
	case "unsnooze", "follow", "unfollow":
		args = []string{action.Kind, entry.ItemID}
	case "open", "conversation":
		id := entry.ItemID
		if entry.RunID != "" {
			id = entry.RunID
		}
		if id == "" {
			return "", errors.New("select a local item or run")
		}
		args = []string{"open", id}
		if action.Kind == "conversation" {
			args = append(args, "--conversation")
		}
		message = "Browser launch requested; this does not confirm browser visibility."
	default:
		return "", errors.New("unknown terminal action")
	}
	var output, diagnostics strings.Builder
	args = append(append(append([]string{}, base...), args...), "--json")
	if code := Run(ctx, args, &output, &diagnostics, deps); code != 0 {
		return "", errors.New(strings.TrimSpace(diagnostics.String()))
	}
	return message, nil
}
