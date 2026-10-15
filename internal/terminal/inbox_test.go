package terminal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestFeedbackConfirmationIsExplicitCapturedAndDistinctFromPrivateStart(t *testing.T) {
	var entry Entry
	if err := json.Unmarshal([]byte(`{"Key":"feedback","Section":"feedback","Title":"owner/repo #17","ItemID":"feedback-item","EventKey":"event","ExecutionKind":"feedback_v1","FeedbackConfirmation":"captured-target","Details":"Fix, normal commit and push; metadata, original replies, resolve settled concerns and re-request reviewers. No merge, force or bypass.","Model":"gpt-6.1-sol","Effort":"high"}`), &entry); err != nil {
		t.Fatal(err)
	}
	var received Action
	m := &model{ctx: context.Background(), width: 100, height: 24, snapshot: Snapshot{Entries: []Entry{entry}}}
	m.backend = Backend{Load: func(context.Context) (Snapshot, error) {
		next := entry
		next.Details = "NEW TARGET"
		return Snapshot{Entries: []Entry{next}}, nil
	}, Act: func(_ context.Context, a Action) (string, error) { received = a; return "", nil }}
	press(m, "s")
	if m.pending != nil {
		t.Fatal("private start offered modifying consent")
	}
	press(m, "h")
	if m.pending == nil || m.pending.Kind != "feedback" || !strings.Contains(m.View().Content, "Handle PR feedback") || !strings.Contains(m.View().Content, "normal commit and push") {
		t.Fatal("feedback full-scope confirmation was not presented")
	}
	m.Update(tick{})
	if strings.Contains(m.View().Content, "NEW TARGET") {
		t.Fatal("refresh replaced captured modifying target")
	}
	press(m, "n")
	if received.Kind != "" {
		t.Fatal("dismissal dispatched feedback")
	}
	m.snapshot.Entries = []Entry{entry}
	press(m, "h")
	command := press(m, "y")
	if command == nil {
		t.Fatal("explicit feedback confirmation did not dispatch")
	}
	command()
	if received.Kind != "feedback" || received.Entry.EventKey != "event" {
		t.Fatal("feedback confirmation changed scope")
	}
}

func TestFeedbackConfirmationRequiresScrollableDisplayedTarget(t *testing.T) {
	for _, width := range []int{24, 92} {
		entry := Entry{Key: "feedback", Section: "feedback", ItemID: "item", EventKey: "event", ExecutionKind: "feedback_v1", FeedbackConfirmation: "captured-target", Details: "Event: fixture-event\nHead: fixture-head\nBase: fixture-base\nInventory: fixture-items\nScope: fix/commit/push/GitHub; no merge/force/bypass."}
		var received Action
		m := &model{ctx: context.Background(), width: width, height: 10, snapshot: Snapshot{Entries: []Entry{entry}}, backend: Backend{Act: func(_ context.Context, a Action) (string, error) { received = a; return "", nil }}}
		press(m, "h")
		if command := press(m, "y"); command != nil || m.pending == nil {
			t.Fatalf("%dx10 confirmed feedback with all target details hidden", width)
		}
		m.Update(tea.WindowSizeMsg{Width: width, Height: 11})
		seen := map[string]bool{}
		for range 30 {
			view := m.View().Content
			for _, label := range []string{"Event: fixture-event", "Head: fixture-head", "Base: fixture-base", "Inventory: fixture-items", "Scope:"} {
				if strings.Contains(view, label) {
					seen[label] = true
				}
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		if len(seen) != 5 {
			t.Fatalf("%dx11 cannot inspect captured target/scope by scrolling: %+v", width, seen)
		}
		command := press(m, "y")
		if command == nil {
			t.Fatalf("%dx11 rejected inspectable feedback confirmation", width)
		}
		command()
		if received.Kind != "feedback" || received.Entry.FeedbackConfirmation != "captured-target" {
			t.Fatal("resize replaced captured feedback permission")
		}
	}
}

func press(m *model, key string) tea.Cmd {
	_, command := m.Update(tea.KeyPressMsg{Code: []rune(key)[0], Text: key})
	return command
}

func preparedModel() *model {
	return &model{ctx: context.Background(), width: 100, height: 24, snapshot: Snapshot{Entries: []Entry{{Key: "selected", RunID: "run-original", PayloadID: "payload-original", Section: "publication", State: "prepared", Title: "owner/repo #17", Details: "GitHub event: COMMENT\nExact body: **No findings.**\nInline file.go:12: **Action requested:** Keep this original comment."}}}}
}

func TestPublicationFromHelpShowsExactCapturedPreview(t *testing.T) {
	m := preparedModel()
	press(m, "?")
	press(m, "p")
	view := m.View().Content
	if !strings.Contains(view, "Exact body: **No findings.**") || !strings.Contains(view, "Inline file.go:12:") || !strings.Contains(view, "GitHub event: COMMENT") {
		t.Fatal("publication confirmation hid its exact body, inline comments or GitHub event behind help")
	}
}

func TestConfirmationKeepsCapturedPayloadAcrossRefreshAndSelection(t *testing.T) {
	m := preparedModel()
	var received Action
	m.backend = Backend{Load: func(context.Context) (Snapshot, error) {
		return Snapshot{Entries: []Entry{{Key: "selected", RunID: "run-new", PayloadID: "payload-new", Details: "NEW unrelated preview"}}}, nil
	}, Act: func(_ context.Context, action Action) (string, error) { received = action; return "", nil }}
	press(m, "p")
	m.Update(tick{})
	if view := m.View().Content; !strings.Contains(view, "Exact body: **No findings.**") || strings.Contains(view, "NEW unrelated preview") {
		t.Fatal("refresh silently replaced the preview being authorized")
	}
	command := press(m, "y")
	if command == nil {
		t.Fatal("explicit confirmation did not dispatch")
	}
	command()
	if received.Kind != "publish" || received.Entry.RunID != "run-original" || received.Entry.PayloadID != "payload-original" {
		t.Fatal("confirmation authorized a different run or payload")
	}
}

func TestUnicodeViewsRespectViewportWithoutControlExecution(t *testing.T) {
	m := preparedModel()
	m.snapshot.Entries[0].Title = "配送 café e\u0301 👩‍💻\x1b[2J spoofed\nrow"
	m.snapshot.Entries[0].Details = strings.Repeat("配送 café e\u0301 👩‍💻\ttext\n", 20)
	for _, width := range []int{1, 12, 28, 80, 120} {
		for _, height := range []int{1, 5, 10, 40} {
			for _, details := range []bool{false, true} {
				m.width, m.height, m.details = width, height, details
				view := m.View()
				if !utf8.ValidString(view.Content) || strings.ContainsAny(view.Content, "\x1b\t") || len(strings.Split(view.Content, "\n")) > height {
					t.Fatalf("invalid or oversized terminal view at %dx%d", width, height)
				}
				for _, line := range strings.Split(view.Content, "\n") {
					if ansi.StringWidth(line) > width {
						t.Fatalf("row exceeds %d columns: %q", width, line)
					}
				}
			}
		}
	}
}

func TestMonitoringIdentityAndPoliciesRemainReadable(t *testing.T) {
	m := preparedModel()
	m.Update(logMessage("GitHub reviewer (43), owner/api: publish=auto"))
	m.Update(logMessage("GitHub reviewer (43), owner/bill: publish=ask"))
	press(m, "l")
	view := m.View().Content
	if !strings.Contains(view, "owner/api: publish=auto") || !strings.Contains(view, "owner/bill: publish=ask") {
		t.Fatal("monitoring announcements lost identity or an earlier project's automation policy")
	}
}

func TestLongUntrustedTitleCannotHideActualPublicationState(t *testing.T) {
	m := preparedModel()
	m.snapshot.Entries[0].Title = strings.Repeat("long PR title ", 30)
	m.snapshot.Entries[0].State = "publication_unknown"
	if !strings.Contains(m.View().Content, "publication unknown") {
		t.Fatal("long PR title hid the actual unknown publication state")
	}
}

func TestDetailEndCanScrollBackOneLine(t *testing.T) {
	m := preparedModel()
	m.details = true
	var lines []string
	for range 100 {
		lines = append(lines, "Earlier detail")
	}
	lines = append(lines, "FINAL DETAIL")
	m.snapshot.Entries[0].Details = strings.Join(lines, "\n")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if !strings.Contains(m.View().Content, "FINAL DETAIL") {
		t.Fatal("end did not reach final detail")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if strings.Contains(m.View().Content, "FINAL DETAIL") {
		t.Fatal("up after end could not scroll back through details")
	}
}

func TestTinyViewportCannotConfirmPublication(t *testing.T) {
	m := preparedModel()
	press(m, "p")
	m.Update(tea.WindowSizeMsg{Width: 12, Height: 5})
	if command := press(m, "y"); command != nil || m.pending == nil {
		t.Fatal("tiny viewport authorized a publication without room to display its confirmation")
	}
}
