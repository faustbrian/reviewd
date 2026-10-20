package cli

import (
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"strings"
	"time"
)

// PR identity is captured by discovery/preparation. ActorLogin is the operator,
// not the author; absent/expired historical context must remain unknown.
func capturedPull(raw json.RawMessage, repository string, pullID int64, number int) github.Pull {
	var pull github.Pull
	if json.Unmarshal(raw, &pull) != nil || pull.Repository != repository || pull.ID != pullID || pull.Number != number {
		return github.Pull{}
	}
	return pull
}

func projectRun(entry *terminal.Entry, run store.Run) {
	entry.Backend = run.Backend()
	entry.UpdatedAt = run.UpdatedAt
	entry.Activity = run.Progress
	entry.ActivityAt = run.ProgressAt
	if strings.Contains(entry.Activity, "command_execution") {
		entry.Activity = "Tool: " + entry.Activity
	}
	entry.Number = run.Number
	pull := capturedPull(run.Context, run.Repository, run.PullID, run.Number)
	entry.Author = pull.Author.Login
	if pull.Title != "" {
		entry.PRTitle = pull.Title
	}
	var frozen struct {
		Values map[string]any `json:"values"`
	}
	entry.Model, entry.Effort = "unknown", "unknown"
	prefix := "reviewer."
	if run.Purpose == "feedback" {
		prefix = "feedback."
	}
	if json.Unmarshal(run.Configuration, &frozen) == nil {
		if value, ok := frozen.Values[prefix+"model"].(string); ok {
			entry.Model = value
		}
		if value, ok := frozen.Values[prefix+"reasoning_effort"].(string); ok {
			entry.Effort = value
		}
	}
	if run.RequestedModel != "" {
		entry.Model = run.RequestedModel
	}
	if run.Purpose == "feedback" {
		entry.Lane = "feedback"
	} else if run.Purpose == "review" || run.Purpose == "" {
		entry.Lane = "review"
	}
	if run.State == store.AgentRunning && run.ThreadID == "" {
		entry.State = "preparing"
		entry.Activity = "Preparing execution; native session not yet reported"
	}
	if run.CancellationRequested && (run.State == store.AgentRunning || run.State == store.Queued) {
		entry.Activity = "Cancellation requested; waiting for process outcome"
	}
}

func waitingReasons(item store.InboxItem, policy config.Snapshot, controls store.Controls, scopes map[string]store.DiscoveryScope, cooldowns []store.GitHubCooldown, now time.Time, occupied, limit int, capacityKnown, prBusy bool) string {
	if item.State != "queued" && item.State != "retry_wait" && item.State != "pending_decision" && item.State != "legacy_pending" {
		return ""
	}
	reasons := []string{}
	if item.Baseline {
		reasons = append(reasons, "Initial backlog: explicit start required")
	} else if item.State == "pending_decision" || item.State == "legacy_pending" {
		reasons = append(reasons, "Awaiting explicit decision")
	}
	if controls.Paused(item.Repository) {
		reasons = append(reasons, "Project paused")
	}
	for _, hold := range controls.Snoozes {
		if hold.ItemID == item.ID {
			if hold.HeadSHA != "" {
				reasons = append(reasons, "Snoozed until new commits")
			} else {
				reasons = append(reasons, "Snoozed until "+hold.Until.Format(time.RFC3339))
			}
		}
	}
	if item.RetryAt.After(now) {
		reasons = append(reasons, "Retry at "+item.RetryAt.Format(time.RFC3339))
	}
	host, _, _ := strings.Cut(item.Repository, "/")
	for _, cooldown := range cooldowns {
		if cooldown.Host == host && cooldown.Until.After(now) {
			reasons = append(reasons, "GitHub cooldown until "+cooldown.Until.Format(time.RFC3339))
		}
	}
	if scope, ok := scopes[item.Repository]; ok && scope.State != "ok" {
		reasons = append(reasons, "Discovery "+scope.State)
	}
	if item.ExecutionHold != "" {
		reasons = append(reasons, item.ExecutionHold)
	}
	if prBusy {
		reasons = append(reasons, "Another execution owns this PR")
	}
	if capacityKnown && occupied >= limit {
		reasons = append(reasons, "All execution slots occupied")
	}
	if (item.Trigger == "head_updated" || item.Trigger == "context_changed") && item.UpdatedAt.Add(policy.Duration("watch.debounce")).After(now) {
		reasons = append(reasons, "Waiting for update debounce")
	}
	if len(reasons) == 0 {
		return "Queued for watcher pickup; exact wait reason not recorded"
	}
	return strings.Join(reasons, " · ")
}

func executionDetails(entry terminal.Entry) string {
	backend := agent.Label(entry.Backend)
	if entry.Backend == "" {
		backend = "unknown / historical"
	}
	return fmt.Sprintf("\nPR author: %s\nLane: %s\nExecution backend: %s\nFrozen/requested model: %s / %s\nLatest activity: %s\nActivity changed: %s\nRun record updated: %s\n", known(entry.Author), known(entry.Lane), backend, nativeValue(entry.Model), nativeValue(entry.Effort), known(entry.Activity), timeValue(entry.ActivityAt), timeValue(entry.UpdatedAt))
}
func known(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
func nativeValue(s string) string {
	if s == "" {
		return "native default"
	}
	return s
}
func timeValue(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format(time.RFC3339)
}
