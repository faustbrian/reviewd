package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/store"
)

// Recorded feedback effects are not review-publication results. Showing them
// neither fetches new remote facts nor grants execution/publication authority.
func presentFeedbackOutcome(output io.Writer, run store.Run) error {
	if len(run.Outcome) == 0 {
		fmt.Fprintln(output, "Recorded delivery operations: not attempted.")
		return nil
	}
	var journal feedback.Journal
	if json.Unmarshal(run.Outcome, &journal) != nil || journal.Version != 1 {
		return failure(1, "stored feedback outcome is invalid; preserve the profile for recovery")
	}
	if journal.Target.Pull.HeadSHA != "" {
		fmt.Fprintf(output, "Original source head: %s\n", terminalText(journal.Target.Pull.HeadSHA))
	}
	if journal.Target.Pull.TargetBaseSHA != "" {
		fmt.Fprintf(output, "Original target base: %s\n", terminalText(journal.Target.Pull.TargetBaseSHA))
	}
	delivery := journal.Execution.Delivery
	if delivery.Candidate != "" {
		fmt.Fprintf(output, "Reviewed source candidate: %s\n", terminalText(delivery.Candidate))
	}
	if delivery.Commit.OID != "" {
		fmt.Fprintf(output, "Recorded commit: %s\n", terminalText(delivery.Commit.OID))
	}
	if delivery.Target.Pull.HeadSHA != "" {
		fmt.Fprintf(output, "Recorded PR head: %s\n", terminalText(delivery.Target.Pull.HeadSHA))
	}
	status := "not attempted"
	if delivery.Started {
		status = "pending"
	}
	if delivery.Complete {
		status = "complete"
	}
	fmt.Fprintf(output, "Recorded delivery operations: %s\n", status)
	if delivery.Checks.HeadSHA != "" {
		checks := "not ready"
		if delivery.Checks.Ready {
			checks = "ready"
		}
		fmt.Fprintf(output, "Recorded mandatory checks: %s (head %s)\n", checks, terminalText(delivery.Checks.HeadSHA))
	}
	for _, effect := range delivery.Effects {
		fmt.Fprintf(output, "Effect %s: %s", terminalText(effect.Kind), terminalText(effect.State))
		if effect.Key != "" {
			fmt.Fprintf(output, " (%s)", terminalText(effect.Key))
		}
		fmt.Fprintln(output)
		if effect.Error != "" {
			fmt.Fprintln(output, terminalText(effect.Error))
		}
	}
	if journal.Noop {
		fmt.Fprintln(output, "No unhandled human feedback; no agent or delivery work started.")
	}
	for _, remaining := range journal.Remaining {
		fmt.Fprintln(output, "Remaining:", terminalText(remaining))
	}
	if run.State == store.FeedbackDelivered {
		fmt.Fprintln(output, "Feedback handling completed.")
	} else {
		fmt.Fprintln(output, "Feedback handling is not complete; recorded operations do not settle outstanding concerns or unknown effects.")
	}
	return nil
}
