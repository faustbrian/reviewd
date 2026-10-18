package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestProgressPreservesPrimaryIdentityAndNeverAdvancesLifecycle(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	a, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	run, _, err := a.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 42, ActorLogin: "reviewer", WorkKey: "active-progress", State: Queued, Configuration: json.RawMessage(`{}`), Private: true})
	if err != nil {
		t.Fatal(err)
	}
	record := func(thread, turn, activity string) error {
		return a.RecordProgress(ctx, run.ID, thread, turn, "requested", "effective", activity, json.RawMessage(`{"tokenUsage":{"total":{"totalTokens":17}}}`))
	}
	if err := record("primary", "", "starting"); err == nil {
		t.Fatal("progress claimed an unclaimed queued run")
	}
	if claimed, err := a.Claim(ctx, run.ID); err != nil || !claimed {
		t.Fatal("could not claim fixture")
	}
	if err := record("primary", "", "starting"); err != nil {
		t.Fatal(err)
	}
	if err := record("primary", "turn", "reviewing"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"specialist", "other-turn"}, {"primary", "other-turn"}, {"primary", ""}} {
		if err := record(pair[0], pair[1], "reading_evidence"); err == nil {
			t.Error("foreign or regressing progress replaced captured primary identity")
		}
	}
	if err := record("primary", "turn", "untrusted model text"); err == nil {
		t.Error("arbitrary model text became application activity")
	}
	if err := a.RecordProgress(ctx, run.ID, "primary", "turn", "requested", "effective", "reviewing", json.RawMessage(`invalid`)); err == nil {
		t.Error("malformed usage was stored")
	}
	current, err := b.Get(ctx, run.ID)
	if err != nil || current.State != Reviewing || current.ThreadID != "primary" || current.TurnID != "turn" || current.Progress != "reviewing" || len(current.Prepared) != 0 || len(current.Outcome) != 0 {
		t.Fatal("second client lost exact active identity or progress changed lifecycle")
	}
	if err := b.Fail(ctx, run.ID, Failed, "worker ended"); err != nil {
		t.Fatal(err)
	}
	if err := record("primary", "turn", "finished"); err == nil {
		t.Fatal("late progress revived a failed worker")
	}
	current, err = a.Get(ctx, run.ID)
	if err != nil || current.State != Failed || current.Failure != "worker ended" || current.ThreadID != "primary" || current.TurnID != "turn" {
		t.Fatal("terminal failure lost diagnostic identity")
	}
}
