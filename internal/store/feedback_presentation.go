package store

import "encoding/json"

// AwaitingHumanFeedback uses the canonical outcome for full runs. The lightweight
// notification view has only the projection computed from that same predicate
// by FinishFeedback; it never loads private source/replies on every tick.
// This presentation predicate grants no execution or recovery capability.
func AwaitingHumanFeedback(run Run) bool {
	if run.Purpose != "feedback" || run.State != Partial || !run.OwnerClosed {
		return false
	}
	if len(run.Outcome) == 0 {
		return run.FeedbackAwaitingHuman
	}
	return completedFeedbackResponse(run.Outcome)
}

func completedFeedbackResponse(outcome json.RawMessage) bool {
	if len(outcome) > 8<<20 {
		return false
	}
	// Deliberately decode only completion facts, not source, receipts or role
	// text. One terminal write computes the cache; no second state authority.
	var view struct {
		Version   int               `json:"schema_version"`
		Awaiting  bool              `json:"awaiting_feedback"`
		Remaining []json.RawMessage `json:"remaining"`
		Execution struct {
			Dispositions []struct {
				Status string `json:"status"`
			} `json:"dispositions"`
			Delivery struct {
				Complete bool `json:"complete"`
				Effects  []struct {
					State string `json:"state"`
				} `json:"effects"`
			} `json:"delivery"`
			Roles []struct {
				Role          string `json:"role"`
				AfterDelivery bool   `json:"after_delivery"`
				Disposition   string `json:"disposition"`
				Error         string `json:"error"`
			} `json:"roles"`
		} `json:"execution"`
	}
	if json.Unmarshal(outcome, &view) != nil || view.Version != 1 || !view.Awaiting || len(view.Remaining) == 0 || !view.Execution.Delivery.Complete || len(view.Execution.Roles) == 0 {
		return false
	}
	for _, effect := range view.Execution.Delivery.Effects {
		if effect.State != "verified" {
			return false
		}
	}
	clarification := false
	for _, decision := range view.Execution.Dispositions {
		switch decision.Status {
		case "needs_clarification":
			clarification = true
		case "accepted", "already_addressed", "rejected":
		default:
			// A deferral needs verified tracker work, not merely human input.
			// Missing/unknown decision generations cannot establish this wait.
			return false
		}
	}
	if !clarification {
		return false
	}
	last := view.Execution.Roles[len(view.Execution.Roles)-1]
	return last.Role == "delivery_auditor" && last.AfterDelivery && last.Disposition == "APPROVE" && last.Error == ""
}
