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
	for _, label := range []string{"Search", "Status", "Lane", "Repository", "Author", "Title"} {
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
	if !strings.Contains(lines[5], "alice") || !strings.Contains(lines[5], "#7") {
		t.Fatalf("short table lost its selected data row: %q", lines[5])
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
