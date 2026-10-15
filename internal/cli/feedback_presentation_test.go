package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestFeedbackPresentationKeepsRecordedEffectsAndOutstandingWorkDistinct(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-reply", true: "settled-operations-unsettled-feedback"}[complete], func(t *testing.T) {
			head, parent := strings.Repeat("b", 40), strings.Repeat("a", 40)
			replyState := "unknown"
			if complete {
				replyState = "verified"
			}
			journal := feedback.Journal{Version: 1, Target: github.FeedbackTarget{Pull: github.Pull{HeadSHA: parent}}, Remaining: []string{"A separate concern remains deferred"}, Execution: feedback.ExecutionJournal{Delivery: feedback.DeliveryJournal{Started: true, Complete: complete, Target: github.FeedbackTarget{Pull: github.Pull{HeadSHA: head}}, Commit: feedback.CommittedFeedback{OID: head}, Checks: github.CheckReport{HeadSHA: head, Ready: complete}, Effects: []feedback.DeliveryEffect{{Kind: "push", State: "verified"}, {Kind: "reply", State: replyState}}}}}
			outcome, _ := json.Marshal(journal)
			run := store.Run{ID: "original-feedback-run", Purpose: "feedback", State: store.Partial, Repository: "github.com/owner/repo", Number: 17, Configuration: json.RawMessage(`{"values":{"reviewer.reasoning_effort":"low","feedback.reasoning_effort":"high"}}`), Outcome: outcome}
			var out strings.Builder
			if err := (&execution{}).presentRun(&out, run); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			for _, expected := range []string{"Feedback original-feedback-run", "Reasoning effort: \"high\"", "Original source head: " + parent, "Recorded PR head: " + head, "Recorded commit: " + head, "push: verified", "reply: " + replyState, "A separate concern remains deferred", "Feedback handling is not complete"} {
				if !strings.Contains(text, expected) {
					t.Fatal("feedback view lost its actual purpose/effect distinction", expected, text)
				}
			}
			for _, wrong := range []string{"Stored GitHub readback:", "Reviewed head:", "Reasoning effort: \"low\"", "Feedback handling completed"} {
				if strings.Contains(text, wrong) {
					t.Fatal("feedback was presented as review publication or overall completion", text)
				}
			}
		})
	}
}
