package terminal

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"github.com/faustbrian/reviewd/internal/activity"
	"strings"
	"time"
)

// Use terminal palette colors like cdx; in particular, selection reverses the
// terminal's own foreground/background instead of reversing the cyan accent.
func (m *model) colors() (lipgloss.Style, lipgloss.Style) {
	accent := "6"
	if m.light {
		accent = "4"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(accent)).Bold(true), lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
}

func (m *model) header() []string {
	m.ensureSearch()
	accent, muted := m.colors()
	if m.pending != nil {
		return append([]string{accent.Render("reviewd") + "  confirm captured action"}, m.confirmationHeader()...)
	}
	labels := []string{"All", "Reviews", "Feedback", "Running", "History"}
	var filters []string
	for i, label := range labels {
		count := 0
		for _, e := range m.snapshot.Entries {
			if matchesFilter(e, i) {
				count++
			}
		}
		text := fmt.Sprintf("%s %d", label, count)
		if i == m.filter {
			text = "[" + text + "]"
		}
		filters = append(filters, text)
	}
	sortLabel := "[Queue] Recent"
	if m.sortRecent {
		sortLabel = "Queue [Recent]"
	}
	filterLabel := "Filter: " + strings.Join(filters, " ")
	sortLabel = "Sort: " + sortLabel
	if ansi.StringWidth(filterLabel+sortLabel) > m.width-24 {
		filterLabel = fmt.Sprintf("Filter: [%s %d]", labels[m.filter], len(m.entries()))
	}
	if ansi.StringWidth(filterLabel+sortLabel) > m.width-20 {
		sortLabel = "Sort: [Queue]"
		if m.sortRecent {
			sortLabel = "Sort: [Recent]"
		}
	}
	if m.width < 60 {
		sortLabel = ""
	}
	controlStyle := func(focus int) lipgloss.Style {
		if m.focus == focus {
			return lipgloss.NewStyle().Bold(true)
		}
		return muted
	}
	controls := controlStyle(2).Render(filterLabel)
	if sortLabel != "" {
		controls += "   " + controlStyle(3).Render(sortLabel)
	}
	position := m.search.Position()
	m.search.SetWidth(max(1, m.width-ansi.StringWidth(controls)-3))
	// Bubbles v2.2.1 SetWidth does not recalculate visible text offsets.
	// Rebuild the viewport from its end, then restore the logical edit cursor;
	// restoring an interior cursor alone can retain an oversized old viewport.
	m.search.CursorEnd()
	m.search.SetCursor(position)
	search := m.search
	styles := search.Styles()
	styles.Focused.Text = lipgloss.NewStyle().Bold(true)
	styles.Focused.Placeholder = lipgloss.NewStyle().Bold(true)
	styles.Blurred.Text = muted
	styles.Blurred.Placeholder = muted
	search.SetStyles(styles)
	left := search.View()
	gap := max(1, m.width-ansi.StringWidth(left)-ansi.StringWidth(controls))
	line := left + strings.Repeat(" ", gap) + controls
	// cdx reserves two rows each for title and controls, followed by a spacer.
	// Short terminals compact the same regions so a data row remains visible.
	if m.height < 15 {
		return []string{accent.Render("reviewd"), line}
	}
	return []string{accent.Render("reviewd  personal review inbox"), "", line, "", ""}
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
	width := max(1, m.width-1) // Reserve the last cell for the scrollbar.
	type column struct {
		name  string
		width int
		value func(Entry) string
	}
	status := func(e Entry) string {
		if e.State == "pending_decision" {
			if width < 100 {
				return "pending"
			}
			return "needs choice"
		}
		return strings.ReplaceAll(e.State, "_", " ")
	}
	repoPR := func(width int) func(Entry) string {
		return func(e Entry) string {
			number := numberLabel(e.Number)
			return clipped(shortRepo(e.Project), max(1, width-ansi.StringWidth(number)-1)) + " " + number
		}
	}
	author := func(e Entry) string { return fallback(e.Author, "unknown") }
	title := func(e Entry) string { return fallback(e.PRTitle, e.Title) }
	lane := func(e Entry) string { return fallback(e.Lane, "unknown") }
	activity := func(e Entry) string { return fallback(e.Activity, e.WaitReason) }
	now := time.Now()
	var cols []column
	switch {
	case width < 59:
		cols = []column{{"Status", 7, status}, {"Lane", 4, lane}, {"Repository / PR", max(1, width-24), repoPR(max(1, width-24))}, {"Author", 8, author}}
	case m.full && width >= 119:
		cols = []column{{"Status", 12, status}, {"Lane", 8, lane}, {"Repository / PR", 25, repoPR(25)}, {"Author", 16, author}, {"Title", max(1, width-96), title}, {"Activity / waiting", 22, activity}, {"Age", 5, func(e Entry) string { return age(e.ActivityAt, now) }}}
	case width >= 119:
		cols = []column{{"Status", 12, status}, {"Lane", 8, lane}, {"Repository / PR", 28, repoPR(28)}, {"Author", 18, author}, {"Title", width - 72, title}}
	default:
		cols = []column{{"Status", 10, status}, {"Lane", 8, lane}, {"Repository / PR", 18, repoPR(18)}, {"Author", 10, author}, {"Title", max(1, width-52), title}}
	}
	gap := 1 // cdx TABLE_COLUMN_SPACING in src/lib.rs.
	_, muted := m.colors()
	repoColor, authorColor := "6", "5"
	if m.light {
		repoColor, authorColor = "4", "5"
	}
	identityStyle := func(name string) lipgloss.Style {
		switch name {
		case "Repository / PR":
			return lipgloss.NewStyle().Foreground(lipgloss.Color(repoColor))
		case "Author":
			return lipgloss.NewStyle().Foreground(lipgloss.Color(authorColor))
		case "Title":
			return lipgloss.NewStyle().Bold(true)
		default:
			return lipgloss.NewStyle()
		}
	}
	pad := func(text string, size int) string {
		return text + strings.Repeat(" ", max(0, size-ansi.StringWidth(text)))
	}
	header := "  "
	for i, c := range cols {
		header += pad(clipped(c.name, c.width), c.width)
		if i < len(cols)-1 {
			header += strings.Repeat(" ", gap)
		}
	}
	lines := []string{muted.Render(pad(ansi.Truncate(header, width, "…"), width))}
	entries := m.entries()
	rowHeight, visible := m.rowHeight(), m.visibleRows(height)
	first := max(0, m.cursor-visible+1)
	for index := first; index < min(len(entries), first+visible); index++ {
		e := entries[index]
		values := make([][]string, len(cols))
		for i, c := range cols {
			text := cellText(c.value(e))
			values[i] = []string{clipped(text, c.width)}
			if rowHeight == 2 {
				if c.name == "Title" || c.name == "Activity / waiting" {
					wrapped := strings.Split(ansi.Hardwrap(text, c.width, false), "\n")
					values[i] = []string{wrapped[0], ""}
					if len(wrapped) > 1 {
						values[i][1] = clipped(strings.Join(wrapped[1:], ""), c.width)
					}
				} else {
					values[i] = append(values[i], "")
				}
			}
		}
		for line := range rowHeight {
			marker := "  "
			if index == m.cursor && line == 0 {
				marker = "› "
			}
			row := marker
			for i, c := range cols {
				cell := pad(values[i][line], c.width)
				// Keep the selected logical row neutral across both lines;
				// per-cell foregrounds would become uneven reverse backgrounds.
				if index != m.cursor {
					cell = identityStyle(c.name).Render(cell)
				}
				row += cell
				if i < len(cols)-1 {
					row += strings.Repeat(" ", gap)
				}
			}
			row = pad(ansi.Truncate(row, width, "…"), width)
			if index == m.cursor {
				row = lipgloss.NewStyle().Reverse(true).Render(row)
			}
			lines = append(lines, row)
		}
	}
	if len(entries) == 0 && height > 1 {
		lines = append(lines, muted.Render("No work matches the search and filter."))
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	lines = lines[:min(len(lines), height)]
	for i, line := range lines {
		bar := " "
		if len(entries) > 0 && height > 2 {
			switch i {
			case 0:
				bar = "▲"
			case len(lines) - 1:
				bar = "▼"
			default:
				bar = "│"
				track := max(1, height-2)
				thumb := max(1, track*min(len(entries), visible)/len(entries))
				position := (track - thumb) * m.cursor / max(1, len(entries)-1)
				if i-1 >= position && i-1 < position+thumb {
					bar = "█"
				}
			}
		}
		lines[i] = pad(ansi.Truncate(line, width, "…"), width) + muted.Render(bar)
	}
	return strings.Join(lines, "\n")
}

// cdx's Comfortable mode gives each logical entry two terminal lines. Compact
// fallback keeps a whole selectable entry visible in the smallest usable body.
func (m *model) rowHeight() int {
	if m.comfortable && m.bodyHeight() >= 3 {
		return 2
	}
	return 1
}

func (m *model) visibleRows(height int) int {
	return max(1, (height-1)/m.rowHeight())
}

func (m *model) footer() []string {
	if m.pending != nil {
		return []string{"y confirm · n/esc dismiss · arrows scroll · m/e model/effort", "Choices apply only to the captured event; refresh never grants consent."}
	}
	if m.focus == 2 || m.focus == 3 {
		if m.width < 70 {
			return []string{"←/→ change · Tab focus · ctrl+c quit", "enter actions · ctrl+v columns"}
		}
		return []string{"Tab/shift+Tab focus · ←/→ or space change option · enter table actions", "ctrl+v columns   ctrl+o view   F2 mouse   ctrl+c quit"}
	}
	if m.focus == 1 {
		if m.width < 70 {
			return []string{"enter actions · Tab focus · ctrl+c quit", "type search · ctrl+u clear · ↑/↓ browse"}
		}
		if m.width < 120 {
			return []string{"enter actions   tab focus   ←/→ change option   ctrl+c quit", "type search   ctrl+v columns   ctrl+o view   ↑/↓ browse   ctrl+c quit"}
		}
		return []string{"enter actions   ctrl+c quit   tab focus search/filter/sort   ←/→ or space change option", "type search   ctrl+u clear   ctrl+v columns   ctrl+o view   ↑/↓ wheel browse   F2 mouse   ctrl+c quit"}
	}
	if m.details || m.help || m.logView {
		if m.width < 70 {
			return []string{"↑/↓ PgUp/PgDn scroll · esc back", "? help · q quit · ctrl+c interrupt"}
		}
		return []string{"↑/↓ PgUp/PgDn Home/End scroll · esc back · ? help · q quit", "s start · x skip · c cancel · a pause · z snooze · f follow · o open"}
	}
	if m.width < 70 {
		return []string{"/ search · enter details · q quit", "↑/↓ select · s start · ? help"}
	}
	if m.width < 120 {
		return []string{"enter details   / search   tab focus   t sort   ? help   q quit", "ctrl+v columns   ctrl+o view   ↑/↓ wheel browse   s start   q quit"}
	}
	return []string{"enter details   / search   tab focus search/filter/sort   0–4 filter   t sort   ? help   q quit", "ctrl+v columns   ctrl+o view   ↑/↓ wheel browse   F2 mouse   s start   x skip   c cancel   ctrl+c quit"}
}

func (m *model) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	lines := m.header()
	_, muted := m.colors()
	if height < 10 {
		lines = []string{"reviewd · terminal too short", "Resize to at least 24×10; Ctrl-C exits."}
		lines = lines[:min(height, len(lines))]
	} else {
		available := m.bodyHeight()
		if wrapped := m.wrappedBody(); wrapped != nil {
			start := min(m.offset, max(0, len(wrapped)-available))
			lines = append(lines, wrapped[start:min(len(wrapped), start+available)]...)
		} else {
			tableLines := strings.Split(m.tableView(available), "\n")
			lines = append(lines, tableLines[:min(len(tableLines), available)]...)
		}
		for len(lines) < height-4 {
			lines = append(lines, "")
		}
		if len(lines) > height-4 {
			lines = lines[:height-4]
		}
		entries := m.entries()
		preset := "Core"
		if m.full {
			preset = "Full"
		}
		capacity := "Slots: unknown"
		if m.snapshot.CapacityKnown {
			capacity = fmt.Sprintf("Slots: %d/%d · %d free", m.snapshot.Occupied, m.snapshot.Capacity, max(0, m.snapshot.Capacity-m.snapshot.Occupied))
		}
		density := "Compact"
		if m.comfortable {
			density = "Comfortable"
		}
		mouse := "On"
		if m.mouseOff {
			mouse = "Off"
		}
		position := fmt.Sprintf("%d/%d  Cols: %s  View: %s  %s  Mouse: %s", min(m.cursor+1, len(entries)), len(entries), preset, density, capacity, mouse)
		if width < 120 {
			position = fmt.Sprintf("%d/%d  Cols: %s  View: %s  %s", min(m.cursor+1, len(entries)), len(entries), preset, density, capacity)
		}
		if width < 70 {
			position = fmt.Sprintf("%d/%d  %s/%s  %s", min(m.cursor+1, len(entries)), len(entries), preset, density, capacity)
		}
		selection, current, changed := "No selected work", "No activity recorded", time.Time{}
		selectedProgress := false
		if e, ok := m.selected(); ok {
			selection = fmt.Sprintf("%s · %s · %s · %s", strings.ReplaceAll(e.State, "_", " "), fallback(e.Lane, "unknown"), fallback(e.Author, "unknown"), shortRepo(e.Project)+" "+numberLabel(e.Number))
			current, changed = fallback(e.Activity, fallback(e.WaitReason, current)), e.ActivityAt
			// Current hold explanations need the same space as agent activity;
			// dismissing a choice must not hide why this work is still waiting.
			selectedProgress = e.Section != "history" && (e.Activity != "" || e.WaitReason != "")
		}
		selection = clipped(selection, max(1, width-ansi.StringWidth(position)-2))
		lines = append(lines, muted.Render(selection+strings.Repeat(" ", max(1, width-ansi.StringWidth(selection)-ansi.StringWidth(position)))+position))
		announcement := m.snapshot.Discovery
		diagnostic := strings.TrimSpace(strings.TrimPrefix(m.snapshot.Status, CachedInboxStatus))
		if diagnostic != "" {
			announcement = diagnostic
		}
		if m.status != "" {
			announcement = m.status
		}
		// Reserve activity plus its age before laying out discovery/announcements:
		// the core table deliberately omits those full-mode columns.
		if !selectedProgress && (diagnostic != "" || m.status != "") {
			lines = append(lines, muted.Render(clipped(announcement, width)))
		} else {
			announcement = clipped(activity.Redact(announcement), max(1, width-27))
			leftWidth := max(1, width-ansi.StringWidth(announcement)-2)
			ageLabel := " · age: " + age(changed, time.Now())
			progress := clipped(current, max(1, leftWidth-ansi.StringWidth(ageLabel))) + ageLabel
			lines = append(lines, muted.Render(progress+strings.Repeat(" ", max(1, width-ansi.StringWidth(progress)-ansi.StringWidth(announcement)))+announcement))
		}
		for _, line := range m.footer() {
			lines = append(lines, muted.Render(line))
		}
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(strings.ReplaceAll(line, "\t", "    "), width, "…")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	if m.mouseOff {
		view.MouseMode = tea.MouseModeNone
	}
	return view
}
func numberLabel(n int) string {
	if n <= 0 {
		return "#?"
	}
	return fmt.Sprintf("#%d", n)
}

const help = `Navigation: arrows/j/k, Home/End, PgUp/PgDn; enter details, esc back.
Search is focused on startup. Type freely; ctrl+u clears, enter enables table actions.
/ or Tab returns to search; arrows browse without enabling action shortcuts.
Tab/Shift+Tab cycles table, search, filter and sort; arrows/space change focused controls.
0 all; 1 reviews; 2 feedback; 3 running/queued; 4 history. t sorts by recent activity.
Ctrl+V toggles full columns; Ctrl+O toggles Compact/Comfortable rows.
F2 toggles mouse capture (on initially); turn it off for ordinary terminal text selection.
The wheel browses table rows or scrolls the current details/confirmation, never both.
Narrow terminals use compact identity columns.
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
