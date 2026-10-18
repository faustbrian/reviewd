package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNotificationAttemptIsSingleWinnerAndCannotConsumeSupersedingEvent(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	a, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	first := Notification{Source: "item:one", Fingerprint: "request-one", Repository: "github.com/owner/repo", Pending: true, DueAt: time.Now().UTC()}
	if _, err := a.ObserveNotifications(ctx, []Notification{first}); err != nil {
		t.Fatal(err)
	}
	next := first
	next.Fingerprint = "request-two"
	if _, err := b.ObserveNotifications(ctx, []Notification{next}); err != nil {
		t.Fatal(err)
	}
	if taken, err := a.TakeNotifications(ctx, []Notification{first}); err != nil || len(taken) != 0 {
		t.Fatal("old notification consumed a new request")
	}
	results := make(chan int, 2)
	for _, client := range []*Store{a, b} {
		go func(s *Store) {
			taken, err := s.TakeNotifications(ctx, []Notification{next})
			if err != nil {
				results <- -100
				return
			}
			results <- len(taken)
		}(client)
	}
	if wins := <-results + <-results; wins != 1 {
		t.Fatalf("two clients did not preserve one delivery attempt: %d", wins)
	}
}

func TestNotificationMigrationRetainsPendingWorkAndRollsBackInvalidSnapshot(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	// Rehearse the actual version-two upgrade without touching user state.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE input_requests; DROP TABLE item_snoozes; DROP TABLE project_pauses; ALTER TABLE observations DROP COLUMN head_sha; DROP TABLE notification_sources; PRAGMA user_version=2;"); err != nil {
		t.Fatal(err)
	}
	old, err := s.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.State = "ok"
	next.ActorID = 43
	next.RepositoryID = 99
	item := InboxItem{Repository: old.Repository, ActorID: 43, PullID: 701, Number: 1, Category: "reviews_owed", Trigger: "review_requested", State: "pending_decision", Action: "ask", EventKey: "kept-request"}
	if err := s.ApplyDiscovery(ctx, old, next, nil, []InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 || (items[0].State != "legacy_pending" || items[0].LegacyState != "pending_decision") {
		t.Fatal("notification migration changed a pending review decision")
	}
	note := Notification{Source: "item:one", Fingerprint: "one", Repository: old.Repository, Pending: true, DueAt: time.Now().UTC()}
	if _, err := s.ObserveNotifications(ctx, []Notification{note, {Source: "invalid"}}); err == nil {
		t.Fatal("partial notification snapshot accepted")
	}
	if taken, err := s.TakeNotifications(ctx, []Notification{note}); err != nil || len(taken) != 0 {
		t.Fatal("failed snapshot left a partial pending notification")
	}
	if _, err := s.ObserveNotifications(ctx, []Notification{note}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if taken, err := s.TakeNotifications(ctx, []Notification{note}); err != nil || len(taken) != 1 {
		t.Fatal("pending notification did not survive process restart")
	}
	items, _, err = s.Inbox(ctx)
	if err != nil || len(items) != 1 || (items[0].State != "legacy_pending" || items[0].LegacyState != "pending_decision") {
		t.Fatal("delivery consumed review authority")
	}
}
