package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/store"
)

func terminalFixture(t *testing.T) (string, *store.Store, store.InboxItem) {
	t.Helper()
	ctx := context.Background()
	directory := t.TempDir()
	path, state := filepath.Join(directory, "config.toml"), filepath.Join(directory, "state")
	data := "schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[state]\ndirectory=" + strconv.Quote(state) + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	scopes, err := s.ConfigureDiscovery(ctx, []string{"github.com/owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	previous := scopes["github.com/owner/repo"]
	next := previous
	next.ActorID, next.RepositoryID, next.State = 43, 99, "ok"
	item := store.InboxItem{Repository: next.Repository, PullID: 701, Number: 17, ActorID: 43, Category: "reviews_owed", Trigger: "review_requested", Action: "ask", State: "pending_decision", EventKey: "displayed-event", HeadSHA: strings.Repeat("a", 40), TargetBaseSHA: strings.Repeat("b", 40), ContextKey: "captured-context", Title: "配送 café review", URL: "https://other.invalid/untrusted", Explanation: "A human requested your review."}
	if err := s.ApplyDiscovery(ctx, previous, next, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatal("could not seed terminal item")
	}
	return path, s, items[0]
}

func TestTerminalEventDecisionCannotAuthorizeNewerWork(t *testing.T) {
	path, s, item := terminalFixture(t)
	ctx := context.Background()
	invoke := func(kind, event string) int {
		t.Helper()
		var output, diagnostics strings.Builder
		code := Run(ctx, []string{"--config", path, kind, item.ID, "--event", event, "--json"}, &output, &diagnostics, Dependencies{})
		if code != 0 && event == item.EventKey {
			t.Fatalf("displayed event could not be selected: %d %s", code, diagnostics.String())
		}
		return code
	}
	if invoke("start", item.EventKey) != 0 {
		t.Fatal("current event start failed")
	}
	scope, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	next := item
	next.EventKey, next.HeadSHA, next.State = "newer-event", strings.Repeat("c", 40), "pending_decision"
	if err := s.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: next.HeadSHA, Record: json.RawMessage(`{}`)}}, []store.InboxItem{next}); err != nil {
		t.Fatal(err)
	}
	if err := s.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Both choices were opened from an older terminal frame.
	for _, kind := range []string{"start", "skip"} {
		var output, diagnostics strings.Builder
		if code := Run(ctx, []string{"--config", path, kind, item.ID, "--event", item.EventKey}, &output, &diagnostics, Dependencies{}); code == 0 {
			t.Fatalf("stale %s authorized the newly observed event", kind)
		}
	}
	items, _, err := s.Inbox(ctx)
	controls, controlErr := s.Controls(ctx, time.Now())
	if err != nil || controlErr != nil || len(items) != 1 || items[0].EventKey != next.EventKey || items[0].State != "pending_decision" || items[0].DecisionID != "" || len(controls.Snoozes) != 1 {
		t.Fatal("stale terminal choice mutated newer work or cleared its hold")
	}
}
