package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/faustbrian/reviewd/internal/clarification"
	"strings"
	"time"
)

// Notification is presentation state, not a review or publication decision.
// Source has one latest observation; newer events replace a pending message
// without moving the beginning of its bounded grouping window.
type Notification struct {
	Source      string    `json:"source"`
	Fingerprint string    `json:"fingerprint"`
	Repository  string    `json:"repository"`
	ActorID     int64     `json:"actor_id"`
	PullID      int64     `json:"pull_id"`
	Number      int       `json:"number"`
	Body        string    `json:"body"`
	Pending     bool      `json:"pending"`
	Initial     bool      `json:"initial"`
	Digest      bool      `json:"digest"`
	DueAt       time.Time `json:"due_at"`
}

// NotificationRuns reads only presentation metadata, not complete source,
// prepared payloads or frozen instruction bundles on every notification tick.
func (s *Store) NotificationRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,repository,pull_id,actor_id,
 json_extract(record,'$.number'),state,COALESCE(payload_id,''),json_extract(record,'$.private'),
 json_extract(record,'$.created_at'),COALESCE(json_extract(record,'$.inbox_item_id'),''),
 (SELECT COALESCE(group_concat(id),'') FROM input_requests WHERE run_id=runs.id AND state='pending') FROM runs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var run Run
		var created, inputIDs string
		if err := rows.Scan(&run.ID, &run.Repository, &run.PullID, &run.ActorID, &run.Number, &run.State, &run.PayloadID, &run.Private, &created, &run.InboxItemID, &inputIDs); err != nil {
			return nil, err
		}
		run.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, errors.New("notification source has an invalid run creation time")
		}
		if inputIDs != "" {
			for _, id := range strings.Split(inputIDs, ",") {
				run.Inputs = append(run.Inputs, Input{Request: clarification.Request{ID: id}, State: "pending"})
			}
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// ObserveNotifications atomically deduplicates a complete current local view.
// Missing, muted and no-longer-actionable sources lose pending presentation,
// never their inbox work, review state or consent requirements.
func (s *Store) ObserveNotifications(ctx context.Context, observations []Notification) ([]Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT source,record FROM notification_sources")
	if err != nil {
		return nil, err
	}
	previous := map[string]Notification{}
	for rows.Next() {
		var source, raw string
		if err = rows.Scan(&source, &raw); err != nil {
			break
		}
		var note Notification
		if err = json.Unmarshal([]byte(raw), &note); err != nil {
			break
		}
		previous[source] = note
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	write := func(note Notification) error {
		raw, err := json.Marshal(note)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO notification_sources(source,fingerprint,pending,record) VALUES(?,?,?,?)
 ON CONFLICT(source) DO UPDATE SET fingerprint=excluded.fingerprint,pending=excluded.pending,record=excluded.record`, note.Source, note.Fingerprint, note.Pending, string(raw))
		return err
	}
	pending := []Notification{}
	seen := map[string]bool{}
	for _, next := range observations {
		if next.Source == "" || next.Fingerprint == "" || next.Repository == "" || seen[next.Source] {
			return nil, errors.New("notification requires a unique current source and fingerprint")
		}
		seen[next.Source] = true
		old, exists := previous[next.Source]
		if exists && old.Fingerprint == next.Fingerprint {
			// Enabling a channel does not replay work observed while muted.
			if !old.Pending {
				continue
			}
			next.DueAt = old.DueAt
			next.Digest = next.Digest || old.Digest
			next.Initial = next.Initial || old.Initial
		} else if exists && old.Pending && next.Pending {
			next.DueAt = old.DueAt
			next.Digest = next.Digest || old.Digest
			next.Initial = next.Initial || old.Initial
		}
		if !exists || next != old {
			if err := write(next); err != nil {
				return nil, err
			}
		}
		if next.Pending {
			pending = append(pending, next)
		}
	}
	for source, old := range previous {
		if !seen[source] && old.Pending {
			old.Pending = false
			if err := write(old); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return pending, nil
}

// TakeNotifications records a best-effort delivery attempt before calling OS
// adapters. A crash can lose a popup, but cannot replay a storm or grant consent.
// Compare-and-swap prevents two clients from delivering the same observation.
func (s *Store) TakeNotifications(ctx context.Context, notes []Notification) ([]Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	taken := []Notification{}
	for _, note := range notes {
		// Hold predicates belong in the first write, not a preceding SELECT.
		// This keeps the existing two-client claim serializable without a
		// deferred read transaction failing when it upgrades to a writer.
		note.Pending = false
		raw, err := json.Marshal(note)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE notification_sources SET pending=0,record=?
 WHERE source=? AND fingerprint=? AND pending=1
 AND NOT EXISTS(SELECT 1 FROM project_pauses WHERE repository=?)
 AND NOT EXISTS(SELECT 1 FROM item_snoozes z JOIN inbox_items i ON i.id=z.item_id
 WHERE (z.until_at='' OR z.until_at>?) AND (('item:'||i.id)=? OR ('run:'||json_extract(i.record,'$.run_id'))=?
 OR EXISTS(SELECT 1 FROM runs r WHERE r.id=? AND json_extract(r.record,'$.inbox_item_id')=i.id
 AND r.repository=i.repository AND r.actor_id=i.actor_id AND r.pull_id=i.pull_id)))`,
			string(raw), note.Source, note.Fingerprint, note.Repository, time.Now().UTC().Format(deadlineFormat), note.Source, note.Source, strings.TrimPrefix(note.Source, "run:"))
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count == 1 {
			taken = append(taken, note)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return taken, nil
}
