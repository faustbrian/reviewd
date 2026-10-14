package notify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/store"
)

func TestPersonalHoldsDeferRatherThanConsumeNotifications(t *testing.T) {
	for _, mode := range []string{"pause", "snooze"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			seed(t, s, work("reviews_owed", "request-1", 1))
			items, _, err := s.Inbox(ctx)
			if err != nil || len(items) != 1 {
				t.Fatal("missing fixture item")
			}
			if mode == "pause" {
				err = s.PauseProject(ctx, items[0].Repository, true)
			} else {
				err = s.SnoozeItem(ctx, items[0].ID, time.Now().Add(time.Hour), false, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			service := Service{Store: s, Terminal: func(context.Context, Message) error { count++; return nil }}
			doc := document(t, "desktop=false\n")
			for range 2 {
				if err := service.Pump(ctx, doc, true); err != nil {
					t.Fatal(err)
				}
			}
			if count != 0 {
				t.Fatal("held work emitted a notification")
			}
			if mode == "pause" {
				err = s.PauseProject(ctx, items[0].Repository, false)
			} else {
				err = s.UnsnoozeItem(ctx, items[0].ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := service.Pump(ctx, doc, true); err != nil {
					t.Fatal(err)
				}
			}
			if count != 1 {
				t.Fatalf("resume lost or replayed pending attention: %d", count)
			}
			after, _, err := s.Inbox(ctx)
			if err != nil || len(after) != 1 || after[0].State != "pending_decision" {
				t.Fatal("hold or notification consumed the decision")
			}
		})
	}
}

func TestDurationSnoozeRetainsAutomaticRunCategoryAcrossEventsAndRestart(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	owed, feedback := work("reviews_owed", "request-1", 1), work("own_pr_feedback", "feedback-1", 1)
	owed.State, owed.Action, owed.HeadSHA = "queued", "auto", "same-head"
	feedback.State, feedback.Action = "attention", "notify"
	seed(t, s, owed, feedback)
	items, _, err := s.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Category == "reviews_owed" {
			owed = item
		}
	}
	prepare := func(bound bool) store.Run {
		t.Helper()
		run := store.Run{Repository: owed.Repository, RepositoryID: 99, PullID: owed.PullID, Number: owed.Number, ActorID: owed.ActorID, ActorLogin: "reviewer", WorkKey: "automatic", State: store.Queued, Configuration: json.RawMessage(`{}`)}
		if bound {
			run, _, err = s.CreateForItem(ctx, run, owed)
		} else {
			run.WorkKey = "independent-manual"
			run.Private = true
			run, _, err = s.Create(ctx, run)
		}
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := s.Claim(ctx, run.ID); err != nil || !claimed {
			t.Fatalf("claim: %v", err)
		}
		if err := s.Prepare(ctx, run.ID, "payload-"+run.WorkKey, json.RawMessage(`{"body":"prepared"}`), "thread", "turn"); err != nil {
			t.Fatal(err)
		}
		run, err = s.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if bound {
			if err := s.FinishItem(ctx, owed, run); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}
	prepare(true)
	if err := s.SnoozeItem(ctx, owed.ID, time.Now().Add(time.Hour), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	var messages []Message
	service := Service{Store: s, Terminal: func(_ context.Context, message Message) error { messages = append(messages, message); return nil }}
	doc := document(t, "desktop=false\n")
	pump := func() {
		t.Helper()
		if err := service.Pump(ctx, doc, true); err != nil {
			t.Fatal(err)
		}
	}
	pump()
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "feedback") || strings.Contains(messages[0].Body, "prepared") {
		t.Fatalf("snooze leaked or suppressed independent feedback: %+v", messages)
	}
	messages = nil
	next := owed
	next.EventKey = "same-head-rerequest"
	next.RunID = ""
	seed(t, s, next, feedback)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	service.Store = s
	controls, err := s.Controls(ctx, time.Now())
	if err != nil || !controls.Snoozed(owed.ID) {
		t.Fatal("new event lost duration snooze")
	}
	pump()
	if len(messages) != 0 {
		t.Fatalf("old automatic run escaped category snooze after event/restart: %+v", messages)
	}
	prepare(false)
	pump()
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "prepared") {
		t.Fatalf("category snooze suppressed independent manual review: %+v", messages)
	}
	messages = nil
	feedback.EventKey = "feedback-2"
	seed(t, s, next, feedback)
	pump()
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "feedback") || strings.Contains(messages[0].Body, "prepared") {
		t.Fatalf("snooze widened to other category or leaked old run: %+v", messages)
	}
	messages = nil
	if err := s.UnsnoozeItem(ctx, owed.ID); err != nil {
		t.Fatal(err)
	}
	pump()
	pump()
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "prepared") {
		t.Fatalf("release lost or replayed pending run notice: %+v", messages)
	}
}
