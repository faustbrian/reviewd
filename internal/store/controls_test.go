package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestSnoozeHeadReleaseIsPermanentEvenForRetiredCategories(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.SnoozeItem(ctx, item.ID, time.Time{}, true, now); err != nil {
		t.Fatal(err)
	}
	apply := func(head, event string, include bool) {
		t.Helper()
		observed, err := s.DiscoveryScope(ctx, item.Repository)
		if err != nil {
			t.Fatal(err)
		}
		next := item
		next.HeadSHA, next.EventKey = head, event
		items := []InboxItem{}
		if include {
			items = append(items, next)
		}
		if err := s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: head, Record: json.RawMessage(`{}`)}}, items); err != nil {
			t.Fatal(err)
		}
	}
	apply(item.HeadSHA, "same-head-rerequest", true)
	c, err := s.Controls(ctx, now)
	if err != nil || !c.Snoozed(item.ID) {
		t.Fatal("same-head re-request released the snooze")
	}
	apply("new-head", "new-event", false)
	c, err = s.Controls(ctx, now)
	if err != nil || c.Snoozed(item.ID) {
		t.Fatal("new commit failed to release a retired category's snooze")
	}
	apply(item.HeadSHA, "reverted-head", true)
	c, err = s.Controls(ctx, now)
	if err != nil || c.Snoozed(item.ID) {
		t.Fatal("reverting the head resurrected an old snooze")
	}
	rows, err := s.Observations(ctx, item.Repository, item.ActorID)
	if err != nil || rows[item.PullID].HeadSHA != item.HeadSHA {
		t.Fatal("observation head failed to round trip")
	}
}

func TestDurationSnoozeExpiresAtExactDeadlineWithoutChangingDecision(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	until := now.Add(100 * time.Millisecond)
	if err := s.SnoozeItem(ctx, item.ID, until, false, now); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now, until.Add(-time.Nanosecond), until, until.Add(time.Nanosecond)} {
		c, err := s.Controls(ctx, at)
		if err != nil || c.Snoozed(item.ID) != at.Before(until) {
			t.Fatalf("deadline boundary incorrect: %s %+v %v", at, c, err)
		}
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].EventKey != item.EventKey || items[0].State != item.State || items[0].DecisionID != item.DecisionID {
		t.Fatal("snooze expiry mutated decision authority")
	}
}

func TestHoldsGuardAutomaticPublicationButNotExplicitPublication(t *testing.T) {
	for _, mode := range []string{"pause", "snooze"} {
		t.Run(mode, func(t *testing.T) {
			s, item, run := preparedInboxFixture(t)
			ctx := context.Background()
			var err error
			if mode == "pause" {
				err = s.PauseProject(ctx, item.Repository, true)
			} else {
				err = s.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, claimed, _ := s.BeginItemPublication(ctx, run.ID, run.PayloadID, item, json.RawMessage(`{}`)); claimed {
				t.Fatal("held item claimed automatic publication")
			}
			var decisions, attempts int
			if err := s.db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM publication_decisions),(SELECT count(*) FROM publication_attempts)").Scan(&decisions, &attempts); err != nil || decisions != 0 || attempts != 0 {
				t.Fatal("held claim recorded consent or an attempt")
			}
			if err := s.DecidePublication(ctx, run.ID, run.PayloadID); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := s.BeginPublication(ctx, run.ID, run.PayloadID); err != nil || !claimed {
				t.Fatalf("explicit publication was blocked by a scheduling hold: %v", err)
			}
			if err := s.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now()); err == nil {
				t.Fatal("late snooze acknowledged after publication claim")
			}
		})
	}
}

func TestHoldsGuardNewAndWaitingClaimsWithoutLosingExplicitStart(t *testing.T) {
	for _, mode := range []string{"pause", "snooze"} {
		t.Run(mode, func(t *testing.T) {
			s, item := schedulingFixture(t)
			ctx := context.Background()
			run := Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "event", State: Queued, Configuration: json.RawMessage(`{}`)}
			if mode == "pause" {
				if err := s.PauseProject(ctx, item.Repository, true); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, created, _ := s.CreateForItem(ctx, run, item); created {
				t.Fatal("hold admitted new review")
			}
			decided, err := s.DecideItem(ctx, item.ID, true, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			c, err := s.Controls(ctx, time.Now())
			if err != nil || c.Snoozed(item.ID) || c.Paused(item.Repository) != (mode == "pause") {
				t.Fatal("explicit start cleared project pause or retained snooze")
			}
			if mode == "pause" {
				if err := s.PauseProject(ctx, item.Repository, false); err != nil {
					t.Fatal(err)
				}
			}
			run, created, err := s.CreateForItem(ctx, run, decided)
			if err != nil || !created {
				t.Fatalf("resume lost explicit private decision: %v", err)
			}
			if err := s.WaitForChecks(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			run, err = s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishItem(ctx, decided, run); err != nil {
				t.Fatal(err)
			}
			items, _, err := s.Inbox(ctx)
			if err != nil || len(items) != 1 {
				t.Fatal("waiting item lost")
			}
			if err := s.PauseProject(ctx, item.Repository, true); err != nil {
				t.Fatal(err)
			}
			if claimed, _ := s.ClaimWaitingItem(ctx, run, items[0]); claimed {
				t.Fatal("pause resumed waiting worker")
			}
		})
	}
}

func TestConcurrentSchemaUpgradeRetainsExistingWork(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "kept", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, "DROP TABLE input_requests; DROP TABLE item_snoozes; DROP TABLE project_pauses; ALTER TABLE observations DROP COLUMN head_sha; PRAGMA user_version=3;"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	for range 8 {
		go func() {
			client, err := Open(ctx, directory)
			if err == nil {
				defer client.Close()
				_, err = client.Get(ctx, run.ID)
			}
			results <- err
		}()
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("schema upgrade regressed: %d %v", version, err)
	}
}

func TestFailedControlMigrationRollsBackEntireUpgrade(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `DROP TABLE input_requests; DROP TABLE item_snoozes; DROP TABLE project_pauses; ALTER TABLE observations DROP COLUMN head_sha;
 CREATE TABLE project_pauses (unexpected TEXT); PRAGMA user_version=3;`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if failed, err := Open(ctx, directory); err == nil {
		failed.Close()
		t.Fatal("invalid predecessor schema unexpectedly upgraded")
	}
	// Inspect the owned database without invoking the migration again.
	db, err := sql.Open("sqlite", filepath.Join(directory, "reviewd.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, columns int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatal("failed migration advanced its version")
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('observations') WHERE name='head_sha'").Scan(&columns); err != nil || columns != 0 {
		t.Fatal("failed migration retained a partial schema change")
	}
}

func TestNotificationClaimsRespectHoldsAcceptedAfterSelection(t *testing.T) {
	for _, mode := range []string{"pause", "snooze"} {
		for _, kind := range []string{"item", "run"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				s, item, run := preparedInboxFixture(t)
				ctx := context.Background()
				var sequence int
				var name, path string
				if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
					t.Fatal(err)
				}
				other, err := Open(ctx, filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				source := "item:" + item.ID
				if kind == "run" {
					source = "run:" + run.ID
				}
				note := Notification{Source: source, Fingerprint: "selected-before-hold", Repository: item.Repository, ActorID: item.ActorID, PullID: item.PullID, Number: item.Number, Pending: true, DueAt: time.Now()}
				selected, err := s.ObserveNotifications(ctx, []Notification{note})
				if err != nil || len(selected) != 1 {
					t.Fatalf("selection: %v", err)
				}
				if kind == "run" {
					observed, err := s.DiscoveryScope(ctx, item.Repository)
					if err != nil {
						t.Fatal(err)
					}
					next := item
					next.RunID = ""
					next.EventKey += "-new-event"
					next.State = "queued"
					if err := s.ApplyDiscovery(ctx, observed, observed, []Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: json.RawMessage(`{}`)}}, []InboxItem{next}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "pause" {
					err = other.PauseProject(ctx, item.Repository, true)
				} else {
					err = other.SnoozeItem(ctx, item.ID, time.Now().Add(time.Hour), false, time.Now())
				}
				if err != nil {
					t.Fatal(err)
				}
				if taken, err := s.TakeNotifications(ctx, selected); err != nil || len(taken) != 0 {
					t.Fatalf("stale selection escaped acknowledged hold: %v %+v", err, taken)
				}
				var pending bool
				if err := s.db.QueryRowContext(ctx, "SELECT pending FROM notification_sources WHERE source=?", source).Scan(&pending); err != nil || !pending {
					t.Fatal("hold consumed the pending delivery")
				}
				if mode == "pause" {
					err = other.PauseProject(ctx, item.Repository, false)
				} else {
					err = other.UnsnoozeItem(ctx, item.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if taken, err := s.TakeNotifications(ctx, selected); err != nil || len(taken) != 1 {
					t.Fatalf("release lost pending delivery: %v %+v", err, taken)
				}
				if taken, err := other.TakeNotifications(ctx, selected); err != nil || len(taken) != 0 {
					t.Fatalf("release duplicated delivery: %v %+v", err, taken)
				}
			})
		}
	}
}
