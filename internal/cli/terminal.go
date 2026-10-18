package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"github.com/spf13/cobra"
	"io"
	"sort"
	"strings"
	"time"
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
	for _, project := range controls.PausedProjects {
		paused[project] = true
	}
	byRun := map[string]store.Run{}
	used := map[string]bool{}
	for _, run := range runs {
		byRun[run.ID] = run
	}
	result := terminal.Snapshot{Status: "Cached inbox. Start queues the selected skill for a watcher."}
	for _, scope := range scopes {
		if scope.State != "ok" {
			result.Status = fmt.Sprintf("Discovery %s for %s: %s", scope.State, scope.Repository, scope.Failure)
		}
	}
	cooldowns, err := durable.GitHubCooldowns(ctx, time.Now())
	if err != nil {
		return result, err
	}
	for _, cooldown := range cooldowns {
		result.Status += fmt.Sprintf(" GitHub work for %s waits until %s.", cooldown.Host, cooldown.Until.Format(time.RFC3339))
	}
	add := func(item *store.InboxItem, run *store.Run) error {
		entry := terminal.Entry{Section: "owed"}
		var text strings.Builder
		if item != nil {
			entry.Key = "item:" + item.ID
			entry.ItemID = item.ID
			entry.EventKey = item.EventKey
			entry.ExecutionKind = item.ExecutionKind
			entry.Project = item.Repository
			entry.State = item.State
			entry.Title = fmt.Sprintf("%s #%d %s", item.Repository, item.Number, item.Title)
			if item.Category == "feedback_handling" || item.Category == "own_pr_feedback" {
				entry.Section = "feedback"
			}
			policy, err := config.Resolve(document, item.Repository, nil, item.Overrides)
			if err != nil {
				return err
			}
			prefix := "reviewer."
			if item.ExecutionKind == "feedback_v1" {
				prefix = "feedback."
			}
			entry.Model, entry.Effort = policy.String(prefix+"model"), policy.String(prefix+"reasoning_effort")
			fmt.Fprintf(&text, "PR: %s\nWhy: %s\nTrigger: %s\nAction: %s\nCurrent selection — backend: %s\nCurrent skill: %s%s\nCurrent model: %s / %s\n", item.URL, item.Explanation, item.Trigger, item.Action, agent.Label(policy.String(prefix+"backend")), map[string]string{"codex": "$", "claude": "/"}[policy.String(prefix+"backend")], policy.String(prefix+"skill"), entry.Model, entry.Effort)
			if item.Category == "own_pr_feedback" {
				fmt.Fprintln(&text, "Notification-only feedback. Configure the feedback lane to launch a handling skill.")
			}
			if item.State == "legacy_pending" {
				fmt.Fprintln(&text, "An earlier workflow decision was retained. Choose Start explicitly to use the current skill configuration.")
			}
			if item.Failure != "" {
				fmt.Fprintln(&text, item.Failure)
			}
			for _, hold := range controls.Snoozes {
				if hold.ItemID == item.ID {
					fmt.Fprintf(&text, "Snoozed until %s; head %s\n", hold.Until.Format(time.RFC3339), hold.HeadSHA)
				}
			}
		}
		if run != nil {
			used[run.ID] = true
			entry.RunID = run.ID
			entry.Purpose = run.Purpose
			if item == nil {
				entry.Key = "run:" + run.ID
				entry.Project = run.Repository
				entry.Title = fmt.Sprintf("%s #%d", run.Repository, run.Number)
				if run.Purpose == "feedback" {
					entry.Section = "feedback"
				}
			}
			entry.State = string(run.State)
			if err := e.presentRun(&text, *run); err != nil {
				return err
			}
		}
		if entry.State == "running" || entry.State == "queued" {
			entry.Section = "running"
		} else if run != nil && item == nil {
			entry.Section = "history"
		}
		entry.Paused = paused[entry.Project]
		if entry.Paused {
			fmt.Fprintln(&text, "Project paused; queued work and notifications are held.")
		}
		entry.Title, entry.Details = terminalText(entry.Title), terminalText(text.String())
		entry.Model, entry.Effort = terminalText(entry.Model), terminalText(entry.Effort)
		result.Entries = append(result.Entries, entry)
		return nil
	}
	for i := range items {
		item := &items[i]
		var linked *store.Run
		if run, ok := byRun[item.RunID]; ok {
			linked = &run
		}
		if err := add(item, linked); err != nil {
			return result, err
		}
	}
	for i := range runs {
		if !used[runs[i].ID] {
			if err := add(nil, &runs[i]); err != nil {
				return result, err
			}
		}
	}
	rank := map[string]int{"running": 0, "owed": 1, "feedback": 2, "history": 3}
	sort.SliceStable(result.Entries, func(i, j int) bool { return rank[result.Entries[i].Section] < rank[result.Entries[j].Section] })
	result.Status = terminalText(result.Status)
	return result, nil
}

func terminalAction(ctx context.Context, base []string, action terminal.Action, deps Dependencies) (string, error) {
	entry := action.Entry
	var args []string
	message := "Selected action applied."
	switch action.Kind {
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
			message = "Skill queued for the watcher."
		}
	case "cancel":
		if entry.RunID == "" {
			return "", errors.New("select an existing run")
		}
		args = []string{"cancel", entry.RunID}
		message = "Cancellation requested; inspect the actual terminal state."
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
		args = []string{"open", id}
		if action.Kind == "conversation" {
			args = append(args, "--conversation")
		}
		message = "Browser launch requested."
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
