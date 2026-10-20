package terminal

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
)

func TestSearchTextCannotDispatchActions(t *testing.T) {
	m := skillModel()
	press(m, "/")
	for _, key := range []string{"s", "x", "c", "a", "f", "o", "q"} {
		press(m, key)
	}
	if m.pending != nil || m.busy {
		t.Fatal("typing in search offered or performed an action")
	}
	if !strings.Contains(m.View().Content, "sxcafoq") {
		t.Fatal("search input is not visible")
	}
}

func TestTableShowsSeparateIdentityColumns(t *testing.T) {
	m := skillModel()
	view := m.View().Content
	for _, label := range []string{"search", "Status", "Lane", "Repository", "Author", "Title"} {
		if !strings.Contains(view, label) {
			t.Fatalf("table missing %s", label)
		}
	}
}

func TestSearchFilterSortAndSelectionCompose(t *testing.T) {
	m := skillModel()
	m.ensureSearch()
	m.snapshot.Entries = []Entry{
		{Key: "a", Project: "github.com/acme/api", Number: 12, Author: "Alice", PRTitle: "Ledger fix", Lane: "review", State: "queued", Section: "running", Activity: "Checking totals"},
		{Key: "b", Project: "github.com/acme/bill", Number: 21, Author: "Bob", PRTitle: "Shipping patch", Lane: "feedback", State: "running", Section: "running", Activity: "Reading feedback"},
	}
	for _, q := range []string{"ACME/API", "12", "alice", "LEDGER", "REVIEW", "QUEUED", "checking"} {
		m.search.SetValue(q)
		got := m.entries()
		if len(got) != 1 || got[0].Key != "a" {
			t.Fatalf("search %q: %+v", q, got)
		}
	}
	m.search.SetValue("acme")
	m.filter = 2
	if got := m.entries(); len(got) != 1 || got[0].Key != "b" {
		t.Fatal("search did not compose with lane filter")
	}
	m.search.SetValue("")
	m.filter = 0
	m.snapshot.Entries[0].ActivityAt = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	m.snapshot.Entries[1].ActivityAt = m.snapshot.Entries[0].ActivityAt.Add(time.Hour)
	m.cursor = 1
	press(m, "t")
	if m.entries()[0].Key != "b" || m.cursor != 0 {
		t.Fatal("recent ordering did not reorder while following selection")
	}
	if e, _ := m.selected(); e.Key != "b" {
		t.Fatal("sort moved selected identity")
	}
	m.filter = 3
	if len(m.entries()) != 2 {
		t.Fatal("running view omitted queued work")
	}
	m.filter = 1
	if got := m.entries(); len(got) != 1 || got[0].Key != "a" {
		t.Fatal("running section erased review lane")
	}
}

func TestPastedSearchShortcutsStayInSearch(t *testing.T) {
	m := skillModel()
	press(m, "/")
	m.Update(tea.PasteMsg{Content: "s x c a z f o q 123"})
	if m.pending != nil || m.busy || !strings.Contains(m.search.Value(), "s x c") {
		t.Fatal("pasted search escaped focus")
	}
}

func TestAllNewColumnsAreSafeAndFitViewports(t *testing.T) {
	m := skillModel()
	canary := "ghp_" + strings.Repeat("A", 35)
	m.snapshot.Entries = []Entry{{Key: "mixed", Section: "running", Lane: "feedback", State: "running", Project: "github.com/acme/配送", Number: 42, Author: "作者\x1b[2J " + canary, PRTitle: "café e\u0301 👩‍💻 fix", Activity: "Tool: command_execution completed " + canary, Details: strings.Repeat("detail\n", 100)}}
	for _, light := range []bool{false, true} {
		m.light = light
		for _, size := range [][2]int{{40, 12}, {80, 24}, {140, 35}, {1, 1}, {24, 10}} {
			m.width, m.height = size[0], size[1]
			plain := ansi.Strip(m.View().Content)
			if strings.Contains(plain, canary) || strings.ContainsAny(plain, "\x1b\t") {
				t.Fatalf("untrusted column escaped rendering: %q", plain)
			}
			if len(strings.Split(plain, "\n")) > size[1] {
				t.Fatal("height overflow")
			}
			for _, line := range strings.Split(plain, "\n") {
				if ansi.StringWidth(line) > size[0] {
					t.Fatalf("width overflow %d: %q", size[0], line)
				}
			}
		}
	}
}

func TestShortTableKeepsADataRow(t *testing.T) {
	m := skillModel()
	m.width, m.height = 40, 12
	m.snapshot.Entries = []Entry{{Key: "one", Project: "github.com/acme/api", Number: 7, Author: "alice", Lane: "review", State: "running"}}
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if !strings.Contains(lines[3], "alice") || !strings.Contains(lines[3], "#7") {
		t.Fatalf("short table lost its selected data row: %q", lines[3])
	}
}

func TestShortConfirmationKeepsCapturedIdentityAndChoiceVisible(t *testing.T) {
	m := skillModel()
	m.width, m.height = 24, 10
	m.snapshot.Entries[0].Project = "github.com/acme/api"
	m.snapshot.Entries[0].Number = 7
	m.snapshot.Entries[0].Author = "alice"
	m.snapshot.Entries[0].Title = "github.com/acme/api #7 " + strings.Repeat("long title ", 20)
	press(m, "s")
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "#7 acme/api") || !strings.Contains(view, "y start") {
		t.Fatalf("confirmation hides identity or choice: %s", view)
	}
}

func TestDiagnosticsExposeEveryRepositoryWithoutWatcherAnnouncements(t *testing.T) {
	m := skillModel()
	m.snapshot.DiscoveryDetails = "acme/first failed: denied\nacme/second failed: unavailable"
	press(m, "l")
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "acme/first failed: denied") || !strings.Contains(view, "acme/second failed: unavailable") {
		t.Fatal("cached discovery diagnostics unavailable without owned watcher")
	}
}

func TestPagingKeepsTheSelectedTableRowVisible(t *testing.T) {
	for _, key := range []rune{tea.KeyPgDown, tea.KeyEnd} {
		m := skillModel()
		m.snapshot.Entries = nil
		for i := 0; i < 40; i++ {
			m.snapshot.Entries = append(m.snapshot.Entries, Entry{Key: fmt.Sprint(i), Project: "github.com/acme/api", Number: i + 1, Author: fmt.Sprintf("user%02d", i), Lane: "review", State: "queued", Section: "running"})
		}
		m.Update(tea.KeyPressMsg{Code: key})
		e, _ := m.selected()
		if !strings.Contains(ansi.Strip(m.tableView(14)), e.Author) {
			t.Fatal("selected action target is outside rendered table after paging")
		}
	}
}

func TestReferenceLayoutKeepsSeparatedControlsAndAnchoredFooter(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {196, 68}} {
		m := skillModel()
		m.width, m.height = size[0], size[1]
		lines := strings.Split(ansi.Strip(m.View().Content), "\n")
		if !strings.Contains(lines[0], "reviewd") || lines[1] != "" || !strings.Contains(lines[2], "search") || !strings.Contains(lines[2], "Filter:") || lines[3] != "" || lines[4] != "" {
			t.Fatalf("%v: missing cdx header/control spacing: %q", size, lines[:6])
		}
		if !strings.Contains(lines[5], "Repository") || !strings.Contains(lines[6], "› ") || !strings.Contains(lines[size[1]-1], "quit") {
			t.Fatalf("%v: missing table selection or anchored footer", size)
		}
		if strings.Contains(lines[5], "Activity") {
			t.Fatal("wide viewport silently changed the core column preset")
		}
	}
}

func TestReferenceScrollbarAndCoreIdentitySurvivePagingAndResize(t *testing.T) {
	m := skillModel()
	m.snapshot.Entries = nil
	for i := range 100 {
		m.snapshot.Entries = append(m.snapshot.Entries, Entry{Key: fmt.Sprint(i), Project: "github.com/acme/api-documentation", Number: i + 1, Author: fmt.Sprintf("author%02d", i), Lane: "feedback", State: "pending_decision"})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	for _, size := range [][2]int{{196, 68}, {80, 24}, {196, 68}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		plain := ansi.Strip(m.View().Content)
		if !strings.Contains(plain, "author99") || !strings.Contains(plain, "100/100") || !strings.Contains(plain, "▲") || !strings.Contains(plain, "▼") {
			t.Fatalf("%v: selection/position/scrollbar disappeared", size)
		}
		body := ansi.Strip(m.tableView(14))
		if !strings.Contains(body, "author99") {
			t.Fatalf("%v: selected identity missing from table body", size)
		}
		for _, row := range strings.Split(body, "\n") {
			if strings.Contains(row, "author99") && !strings.Contains(row, "#100") {
				t.Fatalf("%v: long repository name displaced the PR number: %s", size, row)
			}
		}
	}
}

func TestStartupSearchMatchesReferenceWithoutActionShortcuts(t *testing.T) {
	m := skillModel()
	m.Init()
	for _, key := range []string{"s", "x", "c", "a", "f", "o", "q", "1", "2", "3", "é"} {
		press(m, key)
	}
	if m.focus != 1 || m.search.Value() != "sxcafoq123é" || m.pending != nil || m.busy || m.filter != 0 {
		t.Fatal("initial search interpreted text as application shortcuts")
	}
	m.search.SetValue("")
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.focus != 1 {
		t.Fatal("browsing silently enabled action shortcuts")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	press(m, "s")
	if m.pending == nil || m.pending.Kind != "start" {
		t.Fatal("explicit action focus lost the existing start action")
	}
}

func TestNormalWidthReservesActivityAgeCapacityAndDiscovery(t *testing.T) {
	m := skillModel()
	m.width = 80
	m.snapshot.CapacityKnown, m.snapshot.Capacity, m.snapshot.Occupied = true, 4, 3
	m.snapshot.Discovery = "Discovery: 1 ready · 0 pending · 0 failed"
	m.snapshot.Entries[0].Activity = "Inspecting the changed contract and callers"
	m.snapshot.Entries[0].ActivityAt = time.Now().Add(-2 * time.Minute)
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	status := strings.Join(lines[len(lines)-4:len(lines)-2], "\n")
	for _, text := range []string{"Inspecting", "2m", "Slots: 3/4", "1 free", "0 failed"} {
		if !strings.Contains(status, text) {
			t.Fatalf("ordinary viewport displaced %q from progress status: %s", text, status)
		}
	}
}

func TestLongSearchKeepsControlsAnchoredThroughEditingAndResize(t *testing.T) {
	for _, paste := range []bool{false, true} {
		m := skillModel()
		m.Init()
		query := strings.Repeat("abcdefghijklmnop", 4)
		if paste {
			m.Update(tea.PasteMsg{Content: query})
		} else {
			for _, r := range query {
				press(m, string(r))
			}
		}
		for _, position := range []int{len(query), 25} {
			m.search.SetCursor(position)
			for _, size := range [][2]int{{80, 24}, {120, 40}, {196, 68}, {80, 24}} {
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				line := ansi.Strip(m.header()[2])
				if ansi.StringWidth(line) > size[0] || !strings.HasSuffix(line, "Recent") || !strings.Contains(line, "Filter:") {
					t.Fatalf("paste=%v cursor=%d size=%v: query displaced controls: %s", paste, position, size, line)
				}
				if m.search.Value() != query || m.search.Position() != position {
					t.Fatal("layout changed the query or logical edit cursor")
				}
			}
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		if m.search.Value() != query[:23]+query[24:] || m.search.Position() != 23 {
			t.Fatal("cursor editing changed after rendering a narrow query viewport")
		}
	}
}

func TestCompactFooterRetainsAnEffectiveExitControl(t *testing.T) {
	m := skillModel()
	m.width, m.height = 40, 12
	m.Init()
	footer := func() string {
		lines := strings.Split(ansi.Strip(m.View().Content), "\n")
		return strings.Join(lines[len(lines)-2:], "\n")
	}
	if !strings.Contains(footer(), "ctrl+c quit") {
		t.Fatal("compact search footer hides its exit control")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(footer(), "q quit") || press(m, "q") == nil {
		t.Fatal("compact table footer hides its effective quit shortcut")
	}
}

func TestHealthyDiscoveryDoesNotHideSnapshotDiagnostics(t *testing.T) {
	const notice = "GitHub work for github.com waits until 2026-10-21T12:30:00Z."
	for _, history := range []bool{false, true} {
		m := skillModel()
		m.width = 80
		m.snapshot.Status = "Cached inbox. Completed means native execution finished, not verified delivery. " + notice
		m.snapshot.Discovery = "Discovery: 1 ready · 0 pending · 0 failed"
		if !history {
			m.snapshot.Entries = nil
		}
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, notice) {
			t.Fatalf("history=%v: healthy discovery hid the cooldown host/deadline: %s", history, view)
		}
		m.logView = true
		if !strings.Contains(strings.Join(m.wrappedBody(), "\n"), notice) {
			t.Fatal("full snapshot diagnostic is not reachable in the diagnostic view")
		}
	}
}

func TestDismissedChoiceDoesNotDisplaceSelectedHoldReason(t *testing.T) {
	for _, state := range []string{"queued", "pending_decision"} {
		m := skillModel()
		m.width = 80
		e := &m.snapshot.Entries[0]
		e.State, e.Section, e.WaitReason = state, "running", "Initial backlog: explicit start required"
		if state == "pending_decision" {
			e.Section = "owed"
		}
		press(m, "s")
		press(m, "n")
		view := ansi.Strip(m.View().Content)
		if !strings.Contains(view, "Initial backlog") || !strings.Contains(view, "Choice dismissed") {
			t.Fatalf("%s: announcement displaced the selected hold explanation: %s", state, view)
		}
	}
}
