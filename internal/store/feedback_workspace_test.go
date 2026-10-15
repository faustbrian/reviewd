package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingFeedbackWorkspaceSurvivesQuiescenceAndRecovery(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "retained-feedback", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil || !created {
		t.Fatal("create owned feedback run", err)
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal("claim owned feedback run", err)
	}
	if err := owner.KeepFeedbackWorkspace(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", owner.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatal(err)
	}
	for id, value := range entries {
		if _, err := decodeContainer(value, owner.ID, id); err == nil {
			t.Fatal("older container decoder would accept and reclaim retained work")
		}
	}
	path := filepath.Join(owner.Root, "unfinished-source")
	if err := os.WriteFile(path, []byte("keep the operator's pending fix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.SuspendFeedback(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Starting(ctx); err == nil {
		t.Fatal("suspended owner still grants native process launches")
	}
	for i := 0; i < 2; i++ {
		report, err := s.RecoverPrivate(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, result := range report.Owners {
			if result.ID == owner.ID {
				found = result.State == "feedback_pending" && result.Storage == owner.Root
			}
		}
		if !found || !report.NeedsAttention() {
			t.Fatal("recovery lost the explicit retained-work state", report)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep the operator's pending fix\n" {
			t.Fatal("recovery erased quiescent pending work", err)
		}
	}
	actual, err := s.Get(ctx, run.ID)
	if err != nil || actual.State != FeedbackPending || actual.OwnerClosed {
		t.Fatal("pending retention fabricated owner closure", err)
	}
}

func TestRetainedFeedbackExplicitResumeKeepsOriginalWorkspaceAndHistory(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "retained-resume", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := owner.KeepFeedbackWorkspace(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProgress(ctx, run.ID, "original-thread", "original-turn", "gpt-6.1-sol", "gpt-6.1-sol", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(owner.Root, "pending.txt")
	if err := os.WriteFile(path, []byte("unfinished fix"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishFeedback(ctx, run.ID, FeedbackPending, json.RawMessage(`{"effects":[],"remaining":"explicit resume"}`), "resume"); err != nil {
		t.Fatal(err)
	}
	active, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeFeedbackOwner(ctx, active); err == nil {
		t.Fatal("explicit resume stole an active owner lease")
	}
	if err := owner.SuspendFeedback(); err != nil {
		t.Fatal(err)
	}
	observed, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ResumeFeedbackOwner(ctx, observed)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.SuspendFeedback()
	claimed, err := s.ResumeFeedback(ctx, observed, resumed.ID)
	if err != nil || !claimed {
		t.Fatal("explicit retained-owner resume was refused", err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != HandlingFeedback || current.OwnerID != owner.ID || current.OwnerClosed || current.ThreadID != "" || len(current.PriorExecutions) != 1 || string(current.Outcome) != string(observed.Outcome) {
		t.Fatal("resume replaced work or erased original native history", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "unfinished fix" {
		t.Fatal("explicit resume lost original pending source", err)
	}
	if claimed, _ := s.ResumeFeedback(ctx, observed, resumed.ID); claimed {
		t.Fatal("stale resume claimed a duplicate agent")
	}
}

func TestRetainedFeedbackWorkspaceCannotBeDeletedByOrdinaryClose(t *testing.T) {
	s, item := feedbackSchedulingFixture(t)
	ctx := context.Background()
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.CreateForItem(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "retained-close", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal(err)
	}
	if err := owner.KeepFeedbackWorkspace(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); !errors.Is(err, ErrFeedbackWorkspaceRetained) {
		t.Fatal("ordinary cleanup erased or concealed retained feedback", err)
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("retained owner root was deleted", err)
	}
}
