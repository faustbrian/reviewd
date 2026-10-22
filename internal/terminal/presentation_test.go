package terminal

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func manyRows() *model {
	m := skillModel()
	m.width, m.height = 177, 40
	m.snapshot.Entries = nil
	for i := range 80 {
		m.snapshot.Entries = append(m.snapshot.Entries, Entry{Key: fmt.Sprint(i), Project: "github.com/acme/api", Number: i + 1, Author: fmt.Sprintf("author%02d", i), PRTitle: "A long change whose second line should remain readable instead of being silently truncated at the title column boundary", Lane: "review", State: "queued", Section: "running", Activity: "Inspecting the contract", Details: strings.Repeat("detail content\n", 100)})
	}
	return m
}

func TestDensityControlWrapsContentWithoutChangingSelectionOrSearch(t *testing.T) {
	m := manyRows()
	m.Init()
	press(m, "/")
	m.cursor = 5
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "View: Comfortable") {
		t.Fatal("independent row-density control absent")
	}
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[6], "#1") || !strings.Contains(lines[7], "column boundary") {
		t.Fatalf("comfortable row did not render its second title line: %q", lines[6:8])
	}
	if m.cursor != 5 || m.focus != 1 || m.pending != nil || m.search.Value() != "" {
		t.Fatal("presentation control changed action focus or identity")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if !strings.Contains(ansi.Strip(m.tableView(m.bodyHeight())), "#80") {
		t.Fatal("comfortable paging hid selected row")
	}
	m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	view = ansi.Strip(m.View().Content)
	if !strings.Contains(view, "View: Comfortable") || !strings.Contains(view, "Cols: Full") {
		t.Fatal("column preset and row density are not independent")
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if !strings.Contains(ansi.Strip(m.View().Content), "View: Compact") {
		t.Fatal("density did not toggle back")
	}
}

func TestWheelNavigationStaysInsideTheVisibleBodyAndDoesNotEnableActions(t *testing.T) {
	m := manyRows()
	m.Init()
	press(m, "/")
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("wheel reporting disabled")
	}
	wheel := func(y int, button tea.MouseButton) { m.Update(tea.MouseWheelMsg{X: 5, Y: y, Button: button}) }
	wheel(2, tea.MouseWheelDown)
	if m.cursor != 0 {
		t.Fatal("header wheel moved table")
	}
	wheel(7, tea.MouseWheelDown)
	if m.cursor != 3 || m.focus != 1 || m.pending != nil {
		t.Fatal("wheel did not browse safely in search focus")
	}
	wheel(7, tea.MouseWheelUp)
	if m.cursor != 0 {
		t.Fatal("upward wheel did not reverse navigation")
	}
	m.height = 24
	for _, surface := range []string{"details", "help", "diagnostics", "confirmation"} {
		m.details, m.help, m.logView = false, false, false
		m.pending = nil
		m.offset = 0
		switch surface {
		case "details":
			m.details = true
		case "help":
			m.help = true
		case "diagnostics":
			m.logView = true
			m.logs = []string{strings.Repeat("diagnostic\n", 100)}
		case "confirmation":
			m.pending = &Action{Kind: "start", Entry: m.snapshot.Entries[0]}
		}
		wheel(7, tea.MouseWheelDown)
		if m.offset <= 0 || m.cursor != 0 || m.busy {
			t.Fatalf("%s wheel changed target or did not scroll content", surface)
		}
		if surface == "confirmation" && (m.pending == nil || m.pending.Entry.Key != "0") {
			t.Fatal("wheel changed captured action")
		}
	}
}

func TestWheelCaptureCanBeReleasedForNativeTextSelection(t *testing.T) {
	m := manyRows()
	m.Init()
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("cannot release mouse to terminal")
	}
	m.Update(tea.MouseWheelMsg{X: 5, Y: 7, Button: tea.MouseWheelDown})
	if m.cursor != 0 {
		t.Fatal("disabled mouse still moved table")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.View().MouseMode != tea.MouseModeCellMotion || m.search.Value() != "" {
		t.Fatal("mouse toggle changed search text")
	}
}

func TestTableIdentityHasDistinctColorsOutsideNeutralSelection(t *testing.T) {
	for _, light := range []bool{false, true} {
		m := manyRows()
		m.light = light
		view := m.tableView(12)
		foreground := func(value string) string {
			i := strings.Index(view, value)
			if i < 0 {
				t.Fatalf("missing identity %s", value)
			}
			prefix := view[:i]
			start := strings.LastIndex(prefix, "\x1b[")
			if start < 0 {
				t.Fatalf("unselected %s has no identity style", value)
			}
			return prefix[start:]
		}
		repo, author := foreground("acme/api #2"), foreground("author01")
		if repo == author || strings.Contains(repo, "[m") || strings.Contains(author, "[m") {
			t.Fatal("repository and author colors are not distinct")
		}
		selected := strings.Split(view, "\n")[1]
		if !strings.Contains(selected, "\x1b[7m") || strings.Contains(selected, repo) || strings.Contains(selected, author) {
			t.Fatal("identity colors contaminated neutral selected row")
		}
	}
}

func TestComfortableRowsRemainWholeAndBoundedAcrossBreakpoints(t *testing.T) {
	for _, size := range [][2]int{{24, 10}, {40, 12}, {59, 15}, {60, 15}, {80, 24}, {119, 24}, {120, 40}, {121, 40}, {196, 68}} {
		m := manyRows()
		m.width, m.height = size[0], size[1]
		m.Init()
		m.snapshot.Entries[len(m.snapshot.Entries)-1].PRTitle = "配送 café é 👩‍💻 " + strings.Repeat("long title ", 30)
		m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
		view := ansi.Strip(m.View().Content)
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatalf("height overflow at %v", size)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("width overflow at %v", size)
			}
		}
		table := m.tableView(m.bodyHeight())
		if strings.Count(table, "› ") != 1 {
			t.Fatalf("selection marker absent/duplicated at %v", size)
		}
		if size[0] >= 40 && !strings.Contains(ansi.Strip(table), "#80") {
			t.Fatalf("last selected PR not visible at %v", size)
		}
		if m.bodyHeight() >= 3 && strings.Count(table, "\x1b[7m") != 2 {
			t.Fatalf("comfortable logical selection was cut at %v", size)
		}
	}
}

func TestPresentationKeysCannotRetargetOrConfirmPendingWork(t *testing.T) {
	m := manyRows()
	m.pending = &Action{Kind: "start", Entry: m.snapshot.Entries[0]}
	for _, key := range []tea.KeyPressMsg{{Code: 'o', Mod: tea.ModCtrl}, {Code: 'v', Mod: tea.ModCtrl}, {Code: tea.KeyF2}} {
		_, cmd := m.Update(key)
		if cmd != nil || m.pending == nil || m.pending.Entry.Key != "0" || m.busy {
			t.Fatal("presentation key changed captured consent")
		}
	}
}

func TestMouseCaptureCanBeReleasedInsideCapturedConfirmationAndEditor(t *testing.T) {
	m := manyRows()
	m.pending = &Action{Kind: "start", Entry: m.snapshot.Entries[0]}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.View().MouseMode != tea.MouseModeNone || m.pending == nil || m.pending.Entry.Key != "0" || m.busy {
		t.Fatal("confirmation cannot release capture without changing consent")
	}
	m.editing, m.text = "model", "one-run-model"
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.View().MouseMode != tea.MouseModeCellMotion || m.pending == nil || m.pending.Entry.Key != "0" || m.editing != "model" || m.text != "one-run-model" || m.busy || cmd != nil {
		t.Fatal("editor cannot restore capture without editing or dispatching")
	}
}
