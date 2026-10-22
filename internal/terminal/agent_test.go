package terminal

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"strings"
	"testing"
)

func TestSkillInboxUsesOneStartChoiceForEitherLane(t *testing.T) {
	for _, lane := range []string{"", "feedback_v1"} {
		m := &model{ctx: context.Background(), width: 100, height: 30, snapshot: Snapshot{Entries: []Entry{{Key: "work", ItemID: "item", EventKey: "event", ExecutionKind: lane, Title: "A pull request", Model: "gpt-6.1-sol", Effort: "high"}}}}
		m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
		if m.pending == nil || m.pending.Kind != "start" {
			t.Fatalf("lane %q cannot use ordinary start", lane)
		}
		view := m.View().Content
		for _, obsolete := range []string{"Publish exact", "Fix/commit/push/GitHub", "No merge/force/bypass", "Start private review"} {
			if strings.Contains(view, obsolete) {
				t.Fatalf("workflow control leaked into launcher UI: %s", obsolete)
			}
		}
	}
}
