// Package terminal is an optional presentation over ordinary application actions.
// It owns no scheduler, publication client, decision store, or mouse reporting.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

type Entry struct {
	Purpose, ExecutionKind           string
	Key, Section, Title, Details     string
	ItemID, EventKey, RunID, Project string
	Model, Effort, State             string
	Paused                           bool
}

type Snapshot struct {
	Entries []Entry
	Status  string
}

type Action struct {
	Kind, Model, Effort, Duration string
	Entry                         Entry
}

type Backend struct {
	Load  func(context.Context) (Snapshot, error)
	Act   func(context.Context, Action) (string, error)
	Watch func(context.Context, io.Writer) int
}

var ErrInterrupted = tea.ErrInterrupted

func IsTerminal(input io.Reader, output io.Writer) bool {
	reader, ok := input.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(reader.Fd()) {
		return false
	}
	writer, ok := output.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(writer.Fd())
}

// Run joins application work before returning, even if the terminal detaches.
// A separate headless watcher may continue when this attachment owns no watcher.
func Run(parent context.Context, input io.Reader, output io.Writer, backend Backend) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	initial, err := backend.Load(ctx)
	if err != nil {
		return err
	}
	var tasks sync.WaitGroup
	var guard sync.Mutex
	closed := false
	act := backend.Act
	backend.Act = func(ctx context.Context, action Action) (string, error) {
		guard.Lock()
		if closed {
			guard.Unlock()
			return "", context.Canceled
		}
		tasks.Add(1)
		guard.Unlock()
		defer tasks.Done()
		return act(ctx, action)
	}
	model := &model{ctx: ctx, backend: backend, snapshot: initial, width: 80, height: 24}
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output))
	var watching chan int
	if backend.Watch != nil {
		watching = make(chan int, 1)
		go func() {
			code := backend.Watch(ctx, messageWriter{send: func(text string) { program.Send(logMessage(text)) }})
			watching <- code
			program.Send(watchStopped(code))
		}()
	}
	_, err = program.Run()
	guard.Lock()
	closed = true
	cancel()
	guard.Unlock()
	tasks.Wait()
	if watching != nil {
		code := <-watching
		if err == nil && parent.Err() == nil && code != 0 && code != 130 {
			err = errors.New("monitoring stopped with an error; cached work remains available")
		}
	}
	return err
}

type messageWriter struct{ send func(string) }

func (w messageWriter) Write(data []byte) (int, error) {
	// The command already redacts credentials. Bound the terminal status, not
	// stored evidence or an exact preview, and never execute terminal controls.
	text := []rune(clean(string(data)))
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	w.send(strings.TrimSpace(string(text)))
	return len(data), nil
}

type tick struct{}
type logMessage string
type watchStopped int
type actionResult struct {
	message string
	err     error
}

type model struct {
	ctx            context.Context
	backend        Backend
	snapshot       Snapshot
	cursor, filter int
	width, height  int
	details, help  bool
	logView        bool
	logs           []string
	offset         int
	pending        *Action
	editing, text  string
	busy           bool
	status         string
}

func (m *model) Init() tea.Cmd { return nextTick() }
func nextTick() tea.Cmd        { return tea.Tick(time.Second, func(time.Time) tea.Msg { return tick{} }) }

func (m *model) entries() []Entry {
	if m.filter == 0 {
		return m.snapshot.Entries
	}
	sections := []string{"owed", "feedback", "running", "history"}
	var entries []Entry
	for _, entry := range m.snapshot.Entries {
		if entry.Section == sections[m.filter-1] {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (m *model) selected() (Entry, bool) {
	entries := m.entries()
	if m.cursor < 0 || m.cursor >= len(entries) {
		return Entry{}, false
	}
	return entries[m.cursor], true
}

func (m *model) refresh() {
	previous, _ := m.selected()
	next, err := m.backend.Load(m.ctx)
	if err != nil {
		m.status = "Refresh failed; retained the last view: " + err.Error()
		return
	}
	m.snapshot = next
	entries := m.entries()
	m.cursor = min(m.cursor, max(0, len(entries)-1))
	for index, entry := range entries {
		if entry.Key == previous.Key {
			m.cursor = index
			break
		}
	}
}

func (m *model) execute(action Action) tea.Cmd {
	m.busy, m.pending, m.editing, m.text = true, nil, "", ""
	m.status = "Applying your selected action…"
	return func() tea.Msg {
		message, err := m.backend.Act(m.ctx, action)
		return actionResult{message, err}
	}
}

func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, value.Width), max(1, value.Height)
	case tick:
		m.refresh()
		return m, nextTick()
	case logMessage:
		m.status = string(value)
		m.logs = append(m.logs, string(value))
		if len(m.logs) > 100 {
			m.logs = m.logs[len(m.logs)-100:]
		}
	case watchStopped:
		m.status = fmt.Sprintf("Monitoring stopped (exit %d); cached inbox remains available. Press q to detach.", value)
	case actionResult:
		m.busy = false
		m.status = value.message
		if value.err != nil {
			m.status = "Action not completed: " + value.err.Error()
		}
		m.refresh()
	case tea.PasteMsg:
		if m.editing != "" {
			m.appendText(value.String())
		}
	case tea.KeyPressMsg:
		key := value.String()
		if key == "ctrl+c" {
			return m, tea.Interrupt
		}
		if m.editing != "" {
			switch key {
			case "esc":
				m.editing, m.text = "", ""
			case "enter":
				switch m.editing {
				case "model":
					m.pending.Model = strings.TrimSpace(m.text)
				case "effort":
					m.pending.Effort = strings.TrimSpace(m.text)
				case "duration":
					m.pending.Duration = strings.TrimSpace(m.text)
				}
				m.editing, m.text = "", ""
			case "backspace":
				runes := []rune(m.text)
				m.text = string(runes[:max(0, len(runes)-1)])
			default:
				if value.Text != "" {
					m.appendText(value.Text)
				}
			}
			return m, nil
		}
		if m.pending != nil {
			switch key {
			case "esc", "n":
				m.pending = nil
				m.status = "Choice dismissed; no decision was granted."
			case "y":
				if m.width < 24 || m.height < 10 {
					m.status = "Resize to at least 24×10 to confirm, or use ordinary commands."
					return m, nil
				}
				return m, m.execute(*m.pending)
			case "m", "e":
				if m.pending.Kind == "start" {
					m.editing, m.text = map[string]string{"m": "model", "e": "effort"}[key], ""
				}
			case "d":
				if m.pending.Kind == "snooze" {
					m.editing, m.text = "duration", m.pending.Duration
				}
			case "down", "j", "pgdown", "up", "k", "pgup", "home", "end":
				m.scroll(key)
			}
			return m, nil
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "esc":
			m.details, m.help, m.offset = false, false, 0
			m.logView = false
		case "enter":
			m.details, m.help, m.offset = !m.details, false, 0
			m.logView = false
		case "?":
			m.help, m.offset = !m.help, 0
			m.logView = false
		case "l":
			m.logView, m.help, m.offset = !m.logView, false, 0
		case "0", "1", "2", "3", "4":
			m.filter = int(key[0] - '0')
			m.cursor, m.offset = 0, 0
		case "up", "k", "down", "j", "pgdown", "pgup", "home", "end":
			delta := 1
			if key == "up" || key == "k" || key == "pgup" {
				delta = -1
			}
			if key == "pgdown" || key == "pgup" {
				delta *= max(1, m.height-8)
			}
			if m.details || m.help || m.logView {
				m.scroll(key)
			} else {
				m.cursor = min(max(0, len(m.entries())-1), max(0, m.cursor+delta))
				if key == "home" {
					m.cursor = 0
				}
				if key == "end" {
					m.cursor = max(0, len(m.entries())-1)
				}
			}
			return m, nil
		case "r":
			m.refresh()
		default:
			entry, ok := m.selected()
			if !ok || m.busy {
				return m, nil
			}
			action := Action{Entry: entry}
			switch key {
			case "s", "h":
				action.Kind = "start"
			case "x":
				action.Kind = "skip"
			case "c":
				action.Kind = "cancel"
			case "z":
				action.Kind, action.Duration = "snooze", "2h"
			case "Z":
				action.Kind = "snooze-head"
			case "u":
				action.Kind = "unsnooze"
			case "a":
				action.Kind = "pause"
				if entry.Paused {
					action.Kind = "resume"
				}
			case "f":
				action.Kind = "follow"
			case "F":
				action.Kind = "unfollow"
			case "o":
				return m, m.execute(Action{Kind: "open", Entry: entry})
			case "O":
				return m, m.execute(Action{Kind: "conversation", Entry: entry})
			default:
				return m, nil
			}
			if needsItem(action.Kind) && entry.ItemID == "" {
				m.status = "Select a current inbox event for this action."
				return m, nil
			}
			if action.Kind == "cancel" && entry.RunID == "" {
				m.status = "No active run; use skip for an unstarted inbox event."
				return m, nil
			}
			m.pending, m.offset = &action, 0
			m.help, m.logView = false, false
		}
	}
	return m, nil
}

func needsItem(kind string) bool {
	switch kind {
	case "start", "skip", "snooze", "snooze-head", "unsnooze", "follow", "unfollow":
		return true
	}
	return false
}

func (m *model) appendText(value string) { m.text = boundedInput(m.text + value) }

func boundedInput(text string) string {
	var runes []rune
	for _, char := range text {
		if !unicode.IsControl(char) {
			runes = append(runes, char)
		}
		if len(runes) == 256 {
			break
		}
	}
	return string(runes)
}

func clean(text string) string {
	var safe strings.Builder
	for _, char := range text {
		if unicode.IsControl(char) && char != '\n' && char != '\t' {
			fmt.Fprintf(&safe, "\\u%04x", char)
		} else {
			safe.WriteRune(char)
		}
	}
	return safe.String()
}

func clipped(text string, width int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(clean(text), "\n", " "), "\t", "    ")
	return ansi.Truncate(text, max(1, width), "…")
}

func (m *model) header() []string {
	line := func(text string) string { return clipped(text, max(1, m.width)) }
	lines := []string{line("reviewd — personal review inbox"), line("0 All | 1 Reviews owed | 2 Feedback | 3 Running | 4 History")}
	if m.pending != nil {
		entry := m.pending.Entry
		heading := map[string]string{"start": "Start configured skill", "skip": "Skip event", "cancel": "Cancel agent", "pause": "Pause project", "resume": "Resume project", "snooze": "Snooze event", "snooze-head": "Snooze until new commits", "unsnooze": "Release snooze", "follow": "Follow PR", "unfollow": "Stop following PR"}[m.pending.Kind]
		lines = append(lines, line(heading+": "+entry.Title))
		if m.pending.Kind == "start" {
			model, effort := m.pending.Model, m.pending.Effort
			if model == "" {
				model = entry.Model
			}
			if effort == "" {
				effort = entry.Effort
			}
			lines = append(lines, line(fmt.Sprintf("Model %s / %s", model, effort)), line("m model · e effort · y start · n/esc cancel"))
		} else if m.pending.Kind == "snooze" {
			lines = append(lines, line("Duration "+m.pending.Duration+" · d edit · y confirm · n/esc cancel"))
		} else {
			lines = append(lines, line("y confirm · n/esc cancel"))
		}
		if m.editing != "" {
			lines = append(lines, line(m.editing+" (enter saves; blank keeps default): "+m.text))
		}
	}
	return lines
}

func (m *model) wrappedBody() []string {
	var body string
	switch {
	case m.pending != nil:
		body = m.pending.Entry.Details
	case m.help:
		body = help
	case m.logView:
		body = strings.Join(m.logs, "\n\n")
		if body == "" {
			body = "No monitoring announcements in this attachment. A separate watcher retains its own diagnostics."
		}
	case m.details:
		if entry, ok := m.selected(); ok {
			body = entry.Details
		} else {
			body = "No selected work."
		}
	}
	if body == "" {
		return nil
	}
	body = strings.ReplaceAll(clean(body), "\t", "    ")
	return strings.Split(ansi.Hardwrap(body, max(1, m.width), false), "\n")
}

func (m *model) bodyHeight() int { return max(0, m.height-len(m.header())-3) }

func (m *model) scroll(key string) {
	limit := max(0, len(m.wrappedBody())-m.bodyHeight())
	delta := 1
	if key == "up" || key == "k" || key == "pgup" {
		delta = -1
	}
	if key == "pgup" || key == "pgdown" {
		delta *= max(1, m.bodyHeight())
	}
	m.offset = min(limit, max(0, min(m.offset, limit)+delta))
	if key == "home" {
		m.offset = 0
	}
	if key == "end" {
		m.offset = limit
	}
}

func (m *model) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	line := func(text string) string { return clipped(text, width) }
	lines := m.header()
	footer := "↑/↓ select · enter details · s start skill · x skip · ? help · q detach"
	if width < 65 {
		footer = "enter details · ? help · q detach"
	}
	remaining := m.bodyHeight()
	if wrapped := m.wrappedBody(); wrapped != nil {
		start := min(m.offset, max(0, len(wrapped)-remaining))
		for _, row := range wrapped[start:min(len(wrapped), start+remaining)] {
			lines = append(lines, line(row))
		}
	} else {
		entries := m.entries()
		start := max(0, m.cursor-remaining+1)
		for index := start; index < len(entries) && index < start+remaining; index++ {
			prefix := "  "
			if index == m.cursor {
				prefix = "> "
			}
			state := strings.ReplaceAll(entries[index].State, "_", " ")
			lines = append(lines, line(prefix+state+" ["+entries[index].Section+"] "+entries[index].Title))
		}
		if len(entries) == 0 && remaining > 0 {
			lines = append(lines, line("No cached items in this view. Sync or run watch to discover work."))
		}
	}
	for len(lines) < height-3 {
		lines = append(lines, "")
	}
	status := m.status
	if status == "" {
		status = m.snapshot.Status
	}
	lines = append(lines, line(status), line(footer), line("Skills own their workflows and tools; completed means Codex finished."))
	if len(lines) > height {
		lines = lines[:height]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeNone
	return view
}

const help = `Navigation: arrows/j/k, Home/End, PgUp/PgDn; enter details, esc back.
0 all; 1 reviews owed; 2 feedback; 3 running; 4 history.
s start the selected event's configured review or feedback skill; h is an alias.
m/e in the start confirmation select a one-run model and effort.
x skip this event; f follow; F stop following; c request agent cancellation.
a pause/resume project; z snooze duration (d edits); Z until new commits; u unsnooze.
o open PR; O open a historical recorded GitHub review conversation.
r refresh; l monitoring log; ? help; q detach; Ctrl-C interrupt.
y confirms a pending choice. Escape/n dismiss without changing the decision.
A running skill uses your normal Codex settings, tools and MCP servers.
Reviewd does not manage the skill's review or feedback workflow.
No mouse reporting is enabled: ordinary drag remains terminal-owned.`
