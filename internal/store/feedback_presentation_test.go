package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackNotificationProjectionPreservesPrivateCompletionAndLegacyGaps(t *testing.T) {
	for _, scenario := range []string{"complete", "legacy", "unknown-effect", "failed-audit", "open-owner", "deferred", "missing-decision"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			owner, err := s.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			run, _, err := s.Create(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "author", WorkKey: "projection", State: Queued, Configuration: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if won, err := s.ClaimFeedback(ctx, run.ID); err != nil || !won {
				t.Fatal(err)
			}
			if scenario != "open-owner" {
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
			}
			awaiting, effect, audit := true, "verified", "APPROVE"
			if scenario == "legacy" {
				awaiting = false
			}
			if scenario == "unknown-effect" {
				effect = "unknown"
			}
			if scenario == "failed-audit" {
				audit = "REQUEST_CHANGES"
			}
			decisions := []any{map[string]string{"status": "needs_clarification"}}
			if scenario == "deferred" {
				decisions = []any{map[string]string{"status": "deferred"}}
			}
			if scenario == "missing-decision" {
				decisions = nil
			}
			outcome, _ := json.Marshal(map[string]any{
				"schema_version": 1, "awaiting_feedback": awaiting,
				"remaining": []string{"private-concern-canary"}, "primary_text": strings.Repeat("private-source-canary", 4096),
				"execution": map[string]any{
					"dispositions": decisions,
					"delivery":     map[string]any{"complete": true, "effects": []any{map[string]any{"state": effect, "receipt": "private-reply-canary"}}},
					"roles":        []any{map[string]any{"role": "delivery_auditor", "after_delivery": true, "disposition": audit, "text": "private-role-canary"}},
				},
			})
			if err := s.FinishFeedback(ctx, run.ID, Partial, outcome, ""); err != nil {
				t.Fatal(err)
			}
			full, err := s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			views, err := s.NotificationRuns(ctx)
			if err != nil || len(views) != 1 {
				t.Fatal("notification projection unavailable", err)
			}
			view := views[0]
			want := scenario == "complete"
			if AwaitingHumanFeedback(full) != want || AwaitingHumanFeedback(view) != want || view.FeedbackAwaitingHuman != want {
				t.Fatal("presentation invented or lost completed-response facts", scenario)
			}
			encoded, _ := json.Marshal(view)
			if len(view.Outcome) != 0 || strings.Contains(string(encoded), "private-") || len(encoded) > 4096 {
				t.Fatal("notification tick acquired private outcome/source instead of bounded presentation")
			}
		})
	}
}
