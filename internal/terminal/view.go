package terminal

import (
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/faustbrian/reviewd/internal/activity"
	"strings"
	"time"
)

func (m *model) colors() (lipgloss.Style, lipgloss.Style) {
	accent, muted := "#67E8F9", "#A1A1AA"
	if m.light {
		accent, muted = "#006778", "#52525B"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(accent)).Bold(true), lipgloss.NewStyle().Foreground(lipgloss.Color(muted))
}

func (m *model) header() []string {
	m.ensureSearch()
	accent, muted := m.colors()
	if m.pending != nil {
		return append([]string{accent.Render("reviewd") + "  confirm captured action"}, m.confirmationHeader()...)
	}
	search := m.search
	search.SetWidth(max(1, m.width-11))
	labels := []string{"All", "Reviews", "Feedback", "Running", "History"}
	var filters []string
	for i, label := range labels {
		count := 0
		for _, e := range m.snapshot.Entries {
			if matchesFilter(e, i) {
				count++
			}
		}
		text := fmt.Sprintf("%d %s %d", i, label, count)
		if i == m.filter {
			text = accent.Render("[" + text + "]")
		} else {
			text = muted.Render(text)
		}
		filters = append(filters, text)
	}
	sortLabel := "queue order"
	if m.sortRecent {
		sortLabel = "recent activity"
	}
	focus := []string{"table", "search", "filter", "sort"}[m.focus]
	lines := []string{accent.Render("reviewd") + "  personal review inbox", search.View(), strings.Join(filters, "  ")}
	lines = append(lines, muted.Render("Sort: "+sortLabel+"  ·  Focus: "+focus+"  ·  Tab moves focus"))
	if m.width < 70 {
		lines[2] = fmt.Sprintf("View: %s  ·  0 All 1 Reviews 2 Feedback 3 Running 4 History", labels[m.filter])
	}
	return append(lines, m.confirmationHeader()...)
}

func shortRepo(repo string) string { return strings.TrimPrefix(repo, "github.com/") }
func fallback(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}
func age(t, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	if t.After(now) {
		return "clock ahead"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func (m *model) tableView(height int) string {
	width := max(1, m.width)
	type column struct {
		name  string
		width int
		value func(Entry) string
	}
	status := func(e Entry) string { return strings.ReplaceAll(e.State, "_", " ") }
	pr := func(e Entry) string {
		if e.Number == 0 {
			return "?"
		}
		return fmt.Sprintf("#%d", e.Number)
	}
	repo := func(e Entry) string { return shortRepo(e.Project) }
	author := func(e Entry) string { return fallback(e.Author, "unknown") }
	title := func(e Entry) string { return fallback(e.PRTitle, e.Title) }
	lane := func(e Entry) string { return fallback(e.Lane, "unknown") }
	activity := func(e Entry) string { return fallback(e.Activity, e.WaitReason) }
	now := time.Now()
	var cols []column
	switch {
	case width < 60:
		cols = []column{{"Status", 7, status}, {"Lane", 4, lane}, {"Repository / PR", max(1, width-27), func(e Entry) string { return repo(e) + " " + pr(e) }}, {"Author", 8, author}}
	case width >= 125 || (m.full && width >= 105):
		cols = []column{{"Status", 11, status}, {"Lane", 8, lane}, {"Repository", 18, repo}, {"PR", 6, pr}, {"Author", 12, author}, {"Title", max(8, width-99), title}, {"Activity / waiting", 22, activity}, {"Age", 6, func(e Entry) string { return age(e.ActivityAt, now) }}}
	default:
		cols = []column{{"Status", 11, status}, {"Lane", 8, lane}, {"Repository", 16, repo}, {"PR", 6, pr}, {"Author", 11, author}, {"Title", max(1, width-64), title}}
	}
	columns := make([]table.Column, len(cols))
	for i, c := range cols {
		columns[i] = table.Column{Title: c.name, Width: c.width}
	}
	entries := m.entries()
	rows := make([]table.Row, len(entries))
	for i, e := range entries {
		for _, c := range cols {
			rows[i] = append(rows[i], clipped(c.value(e), c.width))
		}
	}
	accent, muted := m.colors()
	t := table.New(table.WithColumns(columns), table.WithRows(rows), table.WithHeight(max(2, height)), table.WithWidth(width), table.WithStyles(table.Styles{Header: muted.Padding(0, 1).Bold(true), Cell: lipgloss.NewStyle().Padding(0, 1), Selected: accent.Reverse(true)}))
	// A fresh table needs viewport-aware navigation, not SetCursor: Bubbles
	// v2.2.1 SetCursor updates content but leaves the initial scroll offset.
	t.MoveDown(m.cursor)
	if len(rows) == 0 {
		return strings.Split(t.View(), "\n")[0] + "\nNo work matches the search and filter."
	}
	return t.View()
}

func (m *model) footer() []string {
	if m.pending != nil {
		return []string{"y confirm · n/esc dismiss · arrows scroll · m/e model/effort", "Choices apply only to the captured event; refresh never grants consent."}
	}
	if m.focus == 1 {
		return []string{"type search · backspace edit · ctrl+u clear · enter results · Tab focus", "Search never starts, skips or cancels work. Ctrl-C exits."}
	}
	if m.details || m.help || m.logView {
		return []string{"↑/↓ PgUp/PgDn Home/End scroll · esc back · ? help · q quit", "s start · x skip · c cancel · a pause · z snooze · f follow · o open"}
	}
	if m.width < 70 {
		return []string{"/ search · Tab focus · enter details · ? help · q quit", "↑/↓ select · s start · x skip · c cancel"}
	}
	return []string{"↑/↓ select · / search · Tab focus · 0–4 filter · t sort · ctrl+v columns", "enter details · s start · x skip · c cancel · o open · l diag · ? help · q quit"}
}

func (m *model) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	lines := m.header()
	if height < 10 {
		lines = []string{"reviewd · terminal too short", "Resize to at least 24×10; q exits, Ctrl-C stops."}
		lines = lines[:min(height, len(lines))]
	} else {
		available := max(0, height-len(lines)-4)
		if wrapped := m.wrappedBody(); wrapped != nil {
			start := min(m.offset, max(0, len(wrapped)-available))
			lines = append(lines, wrapped[start:min(len(wrapped), start+available)]...)
		} else {
			tableHeight := max(1, available-2)
			tableLines := strings.Split(m.tableView(tableHeight), "\n")
			lines = append(lines, tableLines[:min(len(tableLines), tableHeight)]...)
			for len(lines) < len(m.header())+tableHeight {
				lines = append(lines, "")
			}
			if e, ok := m.selected(); ok {
				lines = append(lines, clipped(fmt.Sprintf("%s %s · author %s · %s · %s", shortRepo(e.Project), numberLabel(e.Number), fallback(e.Author, "unknown"), fallback(e.Lane, "unknown"), strings.ReplaceAll(e.State, "_", " ")), width))
				a := fallback(e.Activity, e.WaitReason)
				if a == "" {
					a = "No activity recorded"
				}
				lines = append(lines, clipped(a+" · activity age: "+age(e.ActivityAt, time.Now()), width))
			}
		}
		for len(lines) < height-4 {
			lines = append(lines, "")
		}
		if len(lines) > height-4 {
			lines = lines[:height-4]
		}
		capacity := "Slots: unknown"
		if m.snapshot.CapacityKnown {
			capacity = fmt.Sprintf("Slots: %d/%d occupied · %d available", m.snapshot.Occupied, m.snapshot.Capacity, max(0, m.snapshot.Capacity-m.snapshot.Occupied))
		}
		status := clipped(activity.Redact(fallback(m.status, m.snapshot.Status)), width)
		lines = append(lines, capacity+" · "+clipped(m.snapshot.Discovery, width), status)
		lines = append(lines, m.footer()...)
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(strings.ReplaceAll(line, "\t", "    "), width, "…")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	return view
}
func numberLabel(n int) string {
	if n <= 0 {
		return "#?"
	}
	return fmt.Sprintf("#%d", n)
}

const help = `Navigation: arrows/j/k, Home/End, PgUp/PgDn; enter details, esc back.
/ or Tab focuses search; type freely, ctrl+u clears, enter returns to table.
Tab/Shift+Tab cycles table, search, filter and sort; arrows/space change focused controls.
0 all; 1 reviews; 2 feedback; 3 running/queued; 4 history. t sorts by recent activity.
Ctrl+V toggles full columns where width permits. Narrow terminals use compact identity columns.
s start the selected event's configured review or feedback skill; h is an alias.
m/e in start confirmation select one-run model and effort.
x skip this event; f follow; F stop following; c request agent cancellation.
a pause/resume project; z snooze duration (d edits); Z until new commits; u unsnooze.
o open PR; O open historical recorded review conversation; r refresh; l cached discovery/monitoring log.
y confirms the captured choice; Escape/n dismisses without changing decisions.
q quits this attachment. If it owns watch, shutdown joins its agents; an independent headless watcher continues.
Slots report a point-in-time kernel-lease observation, not a promise of admission.
Activity age is preview-change time; unknown historical clocks remain unknown.
A tool event finishing is not overall execution or delivery completion.`
