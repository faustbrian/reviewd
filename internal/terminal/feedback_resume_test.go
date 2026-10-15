package terminal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRetainedFeedbackContinuationRequiresItsOwnCapturedConfirmation(t *testing.T) {
	var entry Entry
	if err := json.Unmarshal([]byte(`{"Key":"retained","Section":"feedback","Purpose":"feedback","RunID":"original-run","FeedbackResumeConfirmation":"original-outcome","State":"feedback_pending","Title":"owner/repo #17","Details":"Continue the same retained work. No uncertain effect replay. Fix/commit/push/GitHub; no merge/force/bypass."}`), &entry); err != nil {
		t.Fatal(err)
	}
	var received Action
	m := &model{ctx: context.Background(), width: 100, height: 24, snapshot: Snapshot{Entries: []Entry{entry}}, backend: Backend{Act: func(_ context.Context, a Action) (string, error) { received = a; return "", nil }}}
	if command := press(m, "g"); command != nil || m.pending == nil || m.pending.Kind != "feedback-resume" {
		t.Fatal("feedback continuation dispatched ordinary publication or skipped confirmation")
	}
	if !strings.Contains(m.View().Content, "Continue retained feedback") {
		t.Fatal("same-run continuation scope not shown")
	}
	press(m, "m")
	if m.editing != "" {
		t.Fatal("continuation offered a different model")
	}
	press(m, "n")
	if received.Kind != "" {
		t.Fatal("dismissal granted continuation")
	}
	press(m, "g")
	command := press(m, "y")
	if command == nil {
		t.Fatal("explicit same-run continuation missing")
	}
	command()
	if received.Kind != "feedback-resume" || received.Entry.RunID != "original-run" {
		t.Fatal("continuation replaced run/scope")
	}
}
