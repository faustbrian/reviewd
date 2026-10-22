package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func pendingOverview() *model {
	m := skillModel()
	m.snapshot.Entries = []Entry{
		{Key: "a", ItemID: "a", EventKey: "event-a", Project: "github.com/acme/api", Number: 1, Author: "alice", Lane: "review", State: "pending_decision"},
		{Key: "b", ItemID: "b", EventKey: "event-b", Project: "github.com/acme/api", Number: 2, Author: "bob", Lane: "feedback", ExecutionKind: "feedback_v1", State: "pending_decision"},
		{Key: "c", ItemID: "c", EventKey: "event-c", Project: "github.com/acme/other", Number: 3, State: "pending_decision"},
		{Key: "queued", ItemID: "queued", EventKey: "queued-event", Project: "github.com/acme/api", State: "queued"},
	}
	m.Init()
	return m
}

func TestOverviewOneKeyStartsOrSkipsTheCapturedPendingItem(t *testing.T) {
	for _, key := range []string{"s", "x"} {
		m := pendingOverview()
		var got []Action
		m.backend.Act = func(_ context.Context, a Action) (string, error) { got = append(got, a); return "applied", nil }
		command := press(m, key)
		if command == nil || m.pending != nil || !m.busy {
			t.Fatalf("%s did not perform one-key overview action", key)
		}
		if press(m, key) != nil {
			t.Fatal("repeat key dispatched while busy")
		}
		m.snapshot.Entries[0].EventKey = "newer"
		m.cursor = 1
		command()
		want := map[string]string{"s": "start", "x": "skip"}[key]
		if len(got) != 1 || got[0].Kind != want || !got[0].PendingOnly || got[0].Entry.ItemID != "a" || got[0].Entry.EventKey != "event-a" || got[0].Model != "" || got[0].Effort != "" {
			t.Fatalf("one-key action lost captured target or defaults: %+v", got)
		}
	}
}

func TestOverviewShortcutsRemainTextInExplicitSearch(t *testing.T) {
	m := pendingOverview()
	press(m, "/")
	for _, key := range []string{"s", "x", "S", "X", "y"} {
		if press(m, key) != nil || m.busy || m.pending != nil {
			t.Fatal("search dispatched work")
		}
	}
	if m.search.Value() != "sxSXy" {
		t.Fatalf("lost query: %q", m.search.Value())
	}
}

func TestOverviewBatchCapturesFilteredPendingEntriesAndReportsPartialFailure(t *testing.T) {
	for _, key := range []string{"S", "X"} {
		m := pendingOverview()
		m.search.SetValue("acme/api")
		var got []Action
		m.backend.Act = func(_ context.Context, a Action) (string, error) {
			got = append(got, a)
			if a.Entry.Key == "b" {
				return "", errors.New("event changed")
			}
			return "applied", nil
		}
		if press(m, key) != nil || m.pending == nil || m.busy {
			t.Fatal("batch did not wait for one confirmation")
		}
		m.snapshot.Entries[0].EventKey = "newer"
		m.snapshot.Entries = append(m.snapshot.Entries, Entry{Key: "new", ItemID: "new", EventKey: "new", Project: "github.com/acme/api", State: "pending_decision"})
		m.search.SetValue("")
		command := press(m, "y")
		if command == nil {
			t.Fatal("batch confirmation did not dispatch")
		}
		result := command()
		if len(got) != 2 || got[0].Entry.EventKey != "event-a" || got[1].Entry.Key != "b" {
			t.Fatalf("batch scope changed: %+v", got)
		}
		want := map[string]string{"S": "start", "X": "skip"}[key]
		for _, action := range got {
			if action.Kind != want || !action.PendingOnly {
				t.Fatal("wrong batch action")
			}
		}
		m.backend.Load = func(context.Context) (Snapshot, error) { return m.snapshot, nil }
		m.Update(result)
		if m.busy || !strings.Contains(m.status, "1 failed") || !strings.Contains(m.status, "1 applied") {
			t.Fatalf("partial outcome hidden: %q", m.status)
		}
	}
}

func TestOverviewBatchCancelAndTinySingleDoNotDispatch(t *testing.T) {
	m := pendingOverview()
	press(m, "X")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.pending != nil || m.busy {
		t.Fatal("cancel dispatched batch")
	}
	m.width, m.height = 12, 5
	if press(m, "x") != nil || m.busy {
		t.Fatal("tiny view dispatched without visible identity")
	}
}

func TestOverviewGroupUsesSearchAndLaneIncludingOffscreenPendingWork(t *testing.T) {
	m := pendingOverview()
	m.height = 10
	m.filter = 1
	m.search.SetValue("acme/api")
	for i := 0; i < 30; i++ {
		m.snapshot.Entries = append(m.snapshot.Entries, Entry{Key: strings.Repeat("z", i+1), ItemID: strings.Repeat("z", i+1), EventKey: "pending", Project: "github.com/acme/api", State: "pending_decision", Lane: "review"})
	}
	press(m, "S")
	if m.pending == nil || len(m.pending.Entries) != 31 {
		t.Fatal("group lost offscreen work or ignored search/lane/state")
	}
	if m.pending.Entries[0].Key != "a" || m.pending.Scope != "Filter: Reviews · search: acme/api" {
		t.Fatal("group does not identify captured scope")
	}
	for _, e := range m.pending.Entries {
		if e.Key == "b" || e.Key == "c" || e.Key == "queued" {
			t.Fatal("group included unrelated work")
		}
	}
	press(m, "n")
	m.snapshot.Entries = nil
	press(m, "X")
	if m.pending != nil || m.busy || !strings.Contains(m.status, "No matching pending") {
		t.Fatal("empty group offered an action or no explanation")
	}
}

func TestOverviewCannotStartNotificationOrModifyAlreadyRunningWork(t *testing.T) {
	for _, state := range []string{"queued", "running", "completed", "skipped", "legacy_pending"} {
		for _, key := range []string{"s", "x"} {
			m := pendingOverview()
			m.snapshot.Entries[0].State = state
			if press(m, key) != nil || m.busy || m.pending != nil {
				t.Fatalf("%s acted on %s", key, state)
			}
		}
	}
	m := pendingOverview()
	m.snapshot.Entries = m.snapshot.Entries[1:2]
	m.snapshot.Entries[0].ExecutionKind = ""
	if press(m, "s") != nil || m.busy {
		t.Fatal("notification-only feedback started")
	}
	press(m, "h")
	if m.pending == nil {
		t.Fatal("advanced start was lost")
	}
}
