package terminal

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"unicode/utf8"
)

func press(m *model, key string) tea.Cmd {
	_, command := m.Update(tea.KeyPressMsg{Code: []rune(key)[0], Text: key})
	return command
}

func skillModel() *model {
	return &model{ctx: context.Background(), width: 100, height: 24, snapshot: Snapshot{Entries: []Entry{{Key: "selected", RunID: "run-original", ItemID: "item-original", EventKey: "event-original", Section: "history", State: "completed", Title: "owner/repo #17", Details: "GitHub event: COMMENT\nExact body: **No findings.**\nInline file.go:12: **Action requested:** Keep this original comment."}}}}
}

func TestStartFromHelpShowsCapturedEvent(t *testing.T) {
	m := skillModel()
	press(m, "?")
	press(m, "h")
	view := m.View().Content
	if !strings.Contains(view, "Exact body: **No findings.**") || !strings.Contains(view, "Inline file.go:12:") || !strings.Contains(view, "GitHub event: COMMENT") {
		t.Fatal("publication confirmation hid its exact body, inline comments or GitHub event behind help")
	}
}

func TestStartKeepsCapturedEventAcrossRefreshAndSelection(t *testing.T) {
	m := skillModel()
	var received Action
	m.backend = Backend{Load: func(context.Context) (Snapshot, error) {
		return Snapshot{Entries: []Entry{{Key: "selected", RunID: "run-new", ItemID: "item-new", EventKey: "event-new", Details: "NEW unrelated preview"}}}, nil
	}, Act: func(_ context.Context, action Action) (string, error) { received = action; return "", nil }}
	press(m, "h")
	m.Update(tick{})
	if view := m.View().Content; !strings.Contains(view, "Exact body: **No findings.**") || strings.Contains(view, "NEW unrelated preview") {
		t.Fatal("refresh silently replaced the preview being authorized")
	}
	command := press(m, "y")
	if command == nil {
		t.Fatal("explicit confirmation did not dispatch")
	}
	command()
	if received.Kind != "start" || received.Entry.RunID != "run-original" || received.Entry.EventKey != "event-original" {
		t.Fatal("confirmation authorized a different run or payload")
	}
}

func TestUnicodeViewsRespectViewportWithoutControlExecution(t *testing.T) {
	m := skillModel()
	m.snapshot.Entries[0].Title = "配送 café e\u0301 👩‍💻\x1b[2J spoofed\nrow"
	m.snapshot.Entries[0].Details = strings.Repeat("配送 café e\u0301 👩‍💻\ttext\n", 20)
	for _, width := range []int{1, 12, 28, 80, 120} {
		for _, height := range []int{1, 5, 10, 40} {
			for _, details := range []bool{false, true} {
				m.width, m.height, m.details = width, height, details
				view := m.View()
				view.Content = ansi.Strip(view.Content)
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
	m := skillModel()
	m.Update(logMessage("GitHub reviewer (43), owner/api: reviews=auto"))
	m.Update(logMessage("GitHub reviewer (43), owner/bill: reviews=ask"))
	press(m, "l")
	view := m.View().Content
	if !strings.Contains(view, "owner/api: reviews=auto") || !strings.Contains(view, "owner/bill: reviews=ask") {
		t.Fatal("monitoring announcements lost identity or an earlier project's automation policy")
	}
}

func TestLongUntrustedTitleCannotHideActualPublicationState(t *testing.T) {
	m := skillModel()
	m.snapshot.Entries[0].Title = strings.Repeat("long PR title ", 30)
	m.snapshot.Entries[0].State = "publication_unknown"
	if !strings.Contains(m.View().Content, "publication unknown") {
		t.Fatal("long PR title hid the actual unknown publication state")
	}
}

func TestDetailEndCanScrollBackOneLine(t *testing.T) {
	m := skillModel()
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

func TestTinyViewportCannotConfirmSkillStart(t *testing.T) {
	m := skillModel()
	press(m, "h")
	m.Update(tea.WindowSizeMsg{Width: 12, Height: 5})
	if command := press(m, "y"); command != nil || m.pending == nil {
		t.Fatal("tiny viewport authorized a publication without room to display its confirmation")
	}
}
