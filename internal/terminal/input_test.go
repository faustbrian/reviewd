package terminal

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/faustbrian/reviewd/internal/clarification"
)

func TestClarificationEditorCapturesExactTargetAndRequiresConfirmation(t *testing.T) {
	entry := Entry{Key: "input:old", InputID: "old", RunID: "run", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which?"}, {ID: "private", Header: "Private", Question: "Token?", IsSecret: true}}}
	var action Action
	m := &model{ctx: context.Background(), snapshot: Snapshot{Entries: []Entry{entry}}, width: 80, height: 24, backend: Backend{Act: func(ctx context.Context, a Action) (string, error) { action = a; return "selected", nil }}}
	key := func(s string) {
		_, cmd := m.Update(tea.KeyPressMsg{Code: []rune(s)[0], Text: s})
		if cmd != nil {
			t.Fatal("action dispatched before confirmation")
		}
	}
	key("i")
	m.Update(tea.PasteMsg{Content: "Billing"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(tea.PasteMsg{Content: "secret-canary"})
	if strings.Contains(m.View().Content, "secret-canary") {
		t.Fatal("secret input echoed")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.snapshot.Entries[0].InputID = "new" // refresh cannot retarget captured answers
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("explicit confirmation did not dispatch")
	}
	cmd()
	if action.Entry.InputID != "old" || action.Answers.Answers["private"].Answers[0] != "secret-canary" {
		t.Fatal("answer target or text changed")
	}
}
