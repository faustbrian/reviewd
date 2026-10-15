package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type DiscoveryScope struct {
	FeedbackSchema int       `json:"feedback_schema,omitempty"`
	Repository     string    `json:"repository"`
	RepositoryID   int64     `json:"repository_id"`
	ActorID        int64     `json:"actor_id"`
	Revision       int64     `json:"revision"`
	Initialized    bool      `json:"initialized"`
	State          string    `json:"state"`
	Failure        string    `json:"failure,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type Observation struct {
	// MaterialChange is classified from the complete old/current acquisition,
	// including title/body changes that may accompany a review re-request.
	MaterialChange bool            `json:"-"`
	PullID         int64           `json:"pull_id"`
	Number         int             `json:"number"`
	HeadSHA        string          `json:"head_sha,omitempty"`
	Record         json.RawMessage `json:"record"`
}
type InboxItem struct {
	ExecutionKind string         `json:"execution_kind,omitempty"`
	ExecutionHold string         `json:"execution_hold,omitempty"`
	ID            string         `json:"id"`
	RunID         string         `json:"run_id,omitempty"`
	DecisionID    string         `json:"decision_id,omitempty"`
	Manual        bool           `json:"manual,omitempty"`
	ReviewNow     bool           `json:"review_now,omitempty"`
	Overrides     map[string]any `json:"overrides,omitempty"`
	Attempts      int            `json:"attempts,omitempty"`
	RetryAt       time.Time      `json:"retry_at,omitempty"`
	Failure       string         `json:"failure,omitempty"`
	Repository    string         `json:"repository"`
	PullID        int64          `json:"pull_id"`
	Number        int            `json:"number"`
	ActorID       int64          `json:"actor_id"`
	Category      string         `json:"category"`
	Trigger       string         `json:"trigger"`
	State         string         `json:"state"`
	Baseline      bool           `json:"baseline"`
	EventKey      string         `json:"event_key"`
	HeadSHA       string         `json:"head_sha"`
	TargetBaseSHA string         `json:"target_base_sha"`
	ContextKey    string         `json:"context_key"`
	Title         string         `json:"title"`
	URL           string         `json:"url"`
	Action        string         `json:"action"`
	Explanation   string         `json:"explanation"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

var ErrDiscoveryChanged = errors.New("another synchronization changed this repository; retry its current observation")

// ConfigureDiscovery reconciles the complete allowlist and captures acquisition
// revisions in one transaction, before authentication or any other external I/O.
// Historical runs, previews, decisions and remote outcomes are not removed.
func (s *Store) ConfigureDiscovery(ctx context.Context, repositories []string) (map[string]DiscoveryScope, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	query := `UPDATE discovery_scopes SET revision=revision+1,
 record=json_set(record,'$.revision',revision+1,'$.state','inactive','$.failure','','$.updated_at',?)
 WHERE json_extract(record,'$.state')!='inactive'`
	args := []any{now.Format(time.RFC3339Nano)}
	if len(repositories) > 0 {
		query += " AND repository NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(repositories)), ",") + ")"
		for _, repository := range repositories {
			args = append(args, repository)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return nil, err
	}
	scopes := make(map[string]DiscoveryScope, len(repositories))
	for _, repository := range repositories {
		initial, _ := json.Marshal(DiscoveryScope{Repository: repository, State: "pending", UpdatedAt: now})
		if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,0,0,?)
 ON CONFLICT(repository) DO UPDATE SET revision=revision+1,
 record=json_set(record,'$.revision',revision+1,'$.state','pending','$.failure','','$.updated_at',?)
 WHERE json_extract(record,'$.state')='inactive'`, repository, string(initial), now.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT record FROM discovery_scopes WHERE repository=?", repository).Scan(&raw); err != nil {
			return nil, err
		}
		var scope DiscoveryScope
		if json.Unmarshal([]byte(raw), &scope) != nil {
			return nil, errors.New("invalid durable discovery scope")
		}
		scopes[repository] = scope
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return scopes, nil
}

// DiscoveryScope captures an acquisition revision. Registering a first scope
// before returning it lets concurrent allowlist revocation invalidate that poll
// even before its first success or failure has been persisted.
func (s *Store) DiscoveryScope(ctx context.Context, repository string) (DiscoveryScope, error) {
	initial := DiscoveryScope{Repository: repository, State: "pending", UpdatedAt: time.Now().UTC()}
	record, _ := json.Marshal(initial)
	var raw string
	err := s.db.QueryRowContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,0,0,?)
 ON CONFLICT(repository) DO NOTHING RETURNING record`, repository, string(record)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, "SELECT record FROM discovery_scopes WHERE repository=?", repository).Scan(&raw)
	}
	var scope DiscoveryScope
	if err != nil {
		return scope, err
	}
	if json.Unmarshal([]byte(raw), &scope) != nil {
		return scope, errors.New("invalid durable discovery scope")
	}
	return scope, nil
}
func (s *Store) Observations(ctx context.Context, repository string, actor int64) (map[int64]Observation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pull_id,number,head_sha,record FROM observations WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]Observation{}
	for rows.Next() {
		var id int64
		var number int
		var raw, head string
		if err := rows.Scan(&id, &number, &head, &raw); err != nil {
			return nil, err
		}
		result[id] = Observation{PullID: id, Number: number, HeadSHA: head, Record: json.RawMessage(raw)}
	}
	return result, rows.Err()
}

// ApplyDiscovery commits a complete repository snapshot and baseline atomically.
// The observation revision prevents an older concurrent poll from replacing it.
func (s *Store) ApplyDiscovery(ctx context.Context, observed DiscoveryScope, next DiscoveryScope, rows []Observation, items []InboxItem) error {
	if next.Repository != observed.Repository || next.ActorID <= 0 || next.RepositoryID <= 0 || next.State != "ok" {
		return errors.New("invalid discovery boundary")
	}
	next.Revision = observed.Revision + 1
	next.Initialized = true
	next.UpdatedAt = time.Now().UTC()
	record, _ := json.Marshal(next)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,?,?,?)
 ON CONFLICT(repository) DO UPDATE SET revision=excluded.revision,actor_id=excluded.actor_id,record=excluded.record WHERE discovery_scopes.revision=?`, next.Repository, next.Revision, next.ActorID, string(record), observed.Revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrDiscoveryChanged
	}
	for _, row := range rows {
		if row.MaterialChange {
			if err := supersedeItemRuns(ctx, tx, next.Repository, next.ActorID, row.PullID); err != nil {
				return err
			}
		}
		if row.HeadSHA != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM item_snoozes WHERE head_sha!='' AND head_sha!=? AND item_id IN
   (SELECT id FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=?)`, row.HeadSHA, next.Repository, next.ActorID, row.PullID); err != nil {
				return err
			}
		}
		if row.PullID <= 0 || row.Number <= 0 || !json.Valid(row.Record) {
			return errors.New("invalid discovery observation")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observations(repository,actor_id,pull_id,number,head_sha,record) VALUES(?,?,?,?,?,?)
  ON CONFLICT(repository,actor_id,pull_id) DO UPDATE SET number=excluded.number,head_sha=excluded.head_sha,record=excluded.record`, next.Repository, next.ActorID, row.PullID, row.Number, row.HeadSHA, string(row.Record)); err != nil {
			return err
		}
	}
	// A successful acquisition is a complete category snapshot for every observed
	// pull. Omitted categories must not retain an earlier actionable decision.
	supplied := map[int64]map[string]bool{}
	for _, item := range items {
		if supplied[item.PullID] == nil {
			supplied[item.PullID] = map[string]bool{}
		}
		supplied[item.PullID][item.Category] = true
	}
	for _, row := range rows {
		stored, err := tx.QueryContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=?", next.Repository, next.ActorID, row.PullID)
		if err != nil {
			return err
		}
		retired := []InboxItem{}
		for stored.Next() {
			var raw string
			if err := stored.Scan(&raw); err != nil {
				stored.Close()
				return err
			}
			var item InboxItem
			if json.Unmarshal([]byte(raw), &item) != nil {
				stored.Close()
				return errors.New("invalid stored inbox item")
			}
			if supplied[row.PullID][item.Category] || item.State == "ineligible" || item.State == "closed" {
				continue
			}
			item.State, item.Trigger, item.Action = "ineligible", "ineligible", ""
			item.EventKey = "retired:" + item.EventKey
			item.Explanation = "This category is no longer present in the complete discovery snapshot."
			item.UpdatedAt = next.UpdatedAt
			retired = append(retired, item)
		}
		if err := stored.Err(); err != nil {
			stored.Close()
			return err
		}
		if err := stored.Close(); err != nil {
			return err
		}
		for _, item := range retired {
			raw, _ := json.Marshal(item)
			if _, err := tx.ExecContext(ctx, "UPDATE inbox_items SET record=? WHERE id=?", string(raw), item.ID); err != nil {
				return err
			}
		}
	}
	for _, item := range items {
		if item.Repository != next.Repository || item.ActorID != next.ActorID || item.PullID <= 0 || item.Number <= 0 || item.Category == "" || item.EventKey == "" {
			return errors.New("invalid inbox boundary")
		}
		var previous string
		err := tx.QueryRowContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=? AND pull_id=? AND category=?", item.Repository, item.ActorID, item.PullID, item.Category).Scan(&previous)
		if err == nil {
			var old InboxItem
			if json.Unmarshal([]byte(previous), &old) != nil {
				return errors.New("invalid stored inbox item")
			}
			item.ID = old.ID
			if old.EventKey == item.EventKey {
				item.ExecutionHold = old.ExecutionHold
				changedFeedbackTarget := item.ExecutionKind == "feedback_v1" && (old.HeadSHA != item.HeadSHA || old.TargetBaseSHA != item.TargetBaseSHA || old.ContextKey != item.ContextKey || old.ExecutionKind != item.ExecutionKind)
				item.RunID = old.RunID
				item.DecisionID, item.Manual, item.ReviewNow = old.DecisionID, old.Manual, old.ReviewNow
				item.Overrides = old.Overrides
				if changedFeedbackTarget && old.Manual && old.RunID == "" {
					// Manual modifying consent described a particular target. A
					// stable human-feedback event does not approve another head.
					item.DecisionID, item.Manual, item.ReviewNow, item.Overrides = "", false, false, nil
					item.Baseline = true
					if item.Action == "auto" {
						item.State = "pending_decision"
					}
				} else if old.RunID != "" || old.Manual || (old.Action == "auto" && item.Action == "auto") {
					item.Baseline = old.Baseline
				}
				item.Attempts, item.RetryAt, item.Failure = old.Attempts, old.RetryAt, old.Failure
				// Discovery owns current policy and eligibility; it does not replace
				// a frozen run or revoke an unchanged explicit private decision.
				eligible := item.State != "ignored" && item.State != "ineligible" && item.State != "closed" && item.State != "following"
				if eligible && old.RunID != "" {
					var state State
					var purpose string
					if err := tx.QueryRowContext(ctx, "SELECT state,COALESCE(json_extract(record,'$.purpose'),'') FROM runs WHERE id=? AND repository=? AND actor_id=? AND pull_id=?", old.RunID, item.Repository, item.ActorID, item.PullID).Scan(&state, &purpose); err != nil {
						return errors.New("linked review run is unavailable; preserve its durable state")
					}
					if (purpose == "feedback") != (item.ExecutionKind == "feedback_v1" && item.Category == "feedback_handling") {
						return errors.New("linked run and inbox execution kinds disagree; preserve their distinct authority")
					}
					item.State = string(state)
					if state == Queued {
						item.State = "reviewing"
						if purpose == "feedback" {
							item.State = "handling_feedback"
						}
					}
					if state == WaitingChecks && old.Attempts > 0 {
						item.State = "retry_wait"
						if old.Attempts >= 3 {
							item.State = "failed"
						}
					}
					if state == Prepared && old.Attempts >= 3 {
						item.State = "failed"
					}
				} else if eligible && (item.Manual || (item.Action == "auto" && !item.Baseline)) {
					item.State = "queued"
				}
				if eligible && old.RunID == "" && (item.Manual || (item.Action == "auto" && !item.Baseline)) && old.Attempts > 0 {
					item.State = "retry_wait"
					if old.Attempts >= 3 {
						item.State = "failed"
					}
				}
			}
			if old.EventKey == item.EventKey && old.State == "skipped" {
				item.State = old.State
			}
			if item.RunID != "" || (item.State != "queued" && item.State != "retry_wait") {
				item.ExecutionHold = ""
			}
			// Event identity intentionally excludes target-head and metadata
			// movement for feedback. Still persist refreshed display/validation
			// facts without resetting arrival age on a genuinely unchanged poll.
			item.UpdatedAt = old.UpdatedAt
			oldRecord, _ := json.Marshal(old)
			newRecord, _ := json.Marshal(item)
			if bytes.Equal(oldRecord, newRecord) {
				continue
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			item.ID, err = newID()
			if err != nil {
				return err
			}
		} else {
			return err
		}
		item.UpdatedAt = next.UpdatedAt
		raw, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_items(id,repository,actor_id,pull_id,category,record) VALUES(?,?,?,?,?,?)
  ON CONFLICT(repository,actor_id,pull_id,category) DO UPDATE SET record=excluded.record`, item.ID, item.Repository, item.ActorID, item.PullID, item.Category, string(raw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) DiscoveryFailure(ctx context.Context, observed DiscoveryScope, message string) error {
	next := observed
	next.State = "failed"
	next.Failure = message
	next.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(next)
	result, err := s.db.ExecContext(ctx, `INSERT INTO discovery_scopes(repository,revision,actor_id,record) VALUES(?,?,?,?)
 ON CONFLICT(repository) DO UPDATE SET record=excluded.record WHERE discovery_scopes.revision=?`, next.Repository, next.Revision, next.ActorID, string(raw), observed.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDiscoveryChanged
	}
	return nil
}
func (s *Store) Inbox(ctx context.Context) ([]InboxItem, []DiscoveryScope, error) {
	scopes := []DiscoveryScope{}
	items := []InboxItem{}
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM discovery_scopes ORDER BY repository")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var scope DiscoveryScope
		if json.Unmarshal([]byte(raw), &scope) != nil {
			err = errors.New("invalid discovery status")
			break
		}
		scopes = append(scopes, scope)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT i.record FROM inbox_items i JOIN discovery_scopes s ON i.repository=s.repository AND i.actor_id=s.actor_id WHERE json_extract(s.record,'$.state')!='inactive' AND json_extract(i.record,'$.state') NOT IN ('ignored','closed','skipped','following','ineligible') ORDER BY i.repository,i.pull_id,i.category")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, nil, err
		}
		var item InboxItem
		if json.Unmarshal([]byte(raw), &item) != nil {
			return nil, nil, errors.New("invalid inbox record")
		}
		items = append(items, item)
	}
	return items, scopes, rows.Err()
}

type FollowState struct {
	PullID          int64
	Number          int
	Following       bool
	IgnoredRequests []string
}

// DiscoveryItems includes hidden records so current operator policy can be
// reconciled against the original trigger without inventing a new PR event.
func (s *Store) DiscoveryItems(ctx context.Context, repository string, actor int64) ([]InboxItem, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM inbox_items WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []InboxItem{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item InboxItem
		if json.Unmarshal([]byte(raw), &item) != nil {
			return nil, errors.New("invalid stored inbox item")
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// FinishItem projects the linked run's actual state, without replacing a newer
// event or overriding an operator exclusion that arrived during execution.
func (s *Store) FinishItem(ctx context.Context, item InboxItem, run Run) error {
	result, err := s.db.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state',?,'$.run_id',?,'$.updated_at',?)
 WHERE id=? AND json_extract(record,'$.event_key')=? AND json_extract(record,'$.run_id')=?
 AND json_extract(record,'$.state') NOT IN ('ignored','skipped','closed','ineligible')`, string(run.State), run.ID, time.Now().UTC().Format(time.RFC3339Nano), item.ID, item.EventKey, run.ID)
	if err != nil {
		return err
	}
	_, err = result.RowsAffected()
	return err
}

// ClaimWaitingItem revalidates local event eligibility and resumes the existing
// frozen run in one transaction. A competing process cannot start its model.
func (s *Store) ClaimWaitingItem(ctx context.Context, run Run, item InboxItem) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if hold, err := held(ctx, tx, item, time.Now()); err != nil {
		return false, err
	} else if hold {
		return false, ErrHeld
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET state='reviewing',record=json_set(record,'$.owner_id',?,'$.owner_closed',json('false')),updated_at=? WHERE id=? AND state='waiting_checks'
 AND EXISTS(SELECT 1 FROM inbox_items i JOIN discovery_scopes s ON i.repository=s.repository AND i.actor_id=s.actor_id
 WHERE i.id=? AND i.actor_id=? AND json_extract(s.record,'$.state')='ok'
 AND json_extract(i.record,'$.event_key')=? AND COALESCE(json_extract(i.record,'$.decision_id'),'')=?
	 AND json_extract(i.record,'$.run_id')=runs.id AND json_extract(i.record,'$.state') IN ('waiting_checks','retry_wait'))`, run.OwnerID, timestamp(), run.ID, item.ID, item.ActorID, item.EventKey, item.DecisionID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state','reviewing') WHERE id=?`, item.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DecideItem records an explicit choice against the current immutable event.
// A decision does not authorize a later revision or grant publication rights.
func (s *Store) DecideItem(ctx context.Context, id string, start, now bool, overrides map[string]any, expectedEvent ...string) (InboxItem, error) {
	return s.decideItem(ctx, id, start, now, overrides, nil, expectedEvent...)
}

type FeedbackDecision struct {
	EventKey, HeadSHA, TargetBaseSHA, ContextKey string
}

// DecideFeedbackItem is invoked only after the distinct operator confirmation
// for fixes, normal commit/push, metadata, original replies/settled resolutions
// and renewed existing reviewer requests. A private-review start is not consent.
func (s *Store) DecideFeedbackItem(ctx context.Context, id string, overrides map[string]any, expected FeedbackDecision) (InboxItem, error) {
	return s.decideItem(ctx, id, true, false, overrides, &expected, expected.EventKey)
}
func (s *Store) decideItem(ctx context.Context, id string, start, now bool, overrides map[string]any, feedback *FeedbackDecision, expectedEvent ...string) (InboxItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboxItem{}, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT i.record FROM inbox_items i JOIN discovery_scopes d ON d.repository=i.repository AND d.actor_id=i.actor_id
 WHERE i.id=? AND json_extract(d.record,'$.state')='ok'`, id).Scan(&raw)
	var item InboxItem
	if err != nil || json.Unmarshal([]byte(raw), &item) != nil {
		return item, errors.New("current review item is unavailable; synchronize before deciding")
	}
	if start && ((feedback != nil && (item.Category != "feedback_handling" || item.ExecutionKind != "feedback_v1")) || (feedback == nil && (item.Category == "feedback_handling" || item.ExecutionKind != ""))) {
		return item, errors.New("private review and feedback handling require distinct versioned decisions")
	}
	if feedback != nil && (feedback.EventKey == "" || feedback.HeadSHA == "" || feedback.TargetBaseSHA == "" || feedback.ContextKey == "" || feedback.HeadSHA != item.HeadSHA || feedback.TargetBaseSHA != item.TargetBaseSHA || feedback.ContextKey != item.ContextKey) {
		return item, errors.New("displayed feedback target changed or is incomplete; inspect its current head, base and feedback before confirming")
	}
	if len(expectedEvent) > 1 || (len(expectedEvent) == 1 && item.EventKey != expectedEvent[0]) {
		return item, errors.New("displayed review event changed; inspect the current item before deciding")
	}
	if item.State == "closed" || item.State == "ineligible" || item.State == "reviewing" || item.State == "publishing" || item.State == "handling_feedback" {
		return item, errors.New("item is ineligible or already running; inspect its current run")
	}
	if item.RunID != "" {
		var state State
		if err := tx.QueryRowContext(ctx, "SELECT state FROM runs WHERE id=? AND repository=? AND actor_id=? AND pull_id=?", item.RunID, item.Repository, item.ActorID, item.PullID).Scan(&state); err != nil {
			return item, errors.New("linked review run is unavailable; inspect its durable state before deciding")
		}
		if state == Publishing || state == HandlingFeedback {
			return item, errors.New("publication is already in flight; inspect the run instead of revoking its claimed attempt")
		}
	}
	item.UpdatedAt = time.Now().UTC()
	if start {
		decision, err := newID()
		if err != nil {
			return item, err
		}
		item.State = "queued"
		item.DecisionID = decision
		item.Manual = true
		item.ReviewNow = now
		item.Overrides = overrides
		item.RunID = ""
		item.Baseline = false
		item.Attempts, item.Failure, item.RetryAt = 0, "", time.Time{}
	} else {
		item.State = "skipped"
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM item_snoozes WHERE item_id=?", id); err != nil {
		return item, err
	}
	data, err := json.Marshal(item)
	if err != nil {
		return item, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE inbox_items SET record=? WHERE id=?", string(data), id); err != nil {
		return item, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE discovery_scopes SET revision=revision+1,record=json_set(record,'$.revision',revision+1) WHERE repository=?`, item.Repository); err != nil {
		return item, err
	}
	if err = tx.Commit(); err != nil {
		return item, err
	}
	return item, nil
}

func (s *Store) FollowStates(ctx context.Context, repository string, actor int64) (map[int64]FollowState, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pull_id,number,following,ignored_requests FROM follows WHERE repository=? AND actor_id=?", repository, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[int64]FollowState{}
	for rows.Next() {
		var row FollowState
		var following int
		var raw string
		if err := rows.Scan(&row.PullID, &row.Number, &following, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &row.IgnoredRequests) != nil {
			return nil, errors.New("invalid follow request history")
		}
		row.Following = following == 1
		result[row.PullID] = row
	}
	return result, rows.Err()
}
func (s *Store) SetFollow(ctx context.Context, id string, following bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	flag := 0
	if following {
		flag = 1
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO follows(repository,actor_id,pull_id,number,following,ignored_requests)
 SELECT i.repository,i.actor_id,i.pull_id,CAST(json_extract(i.record,'$.number') AS INTEGER),?,
 COALESCE(json_extract(o.record,'$.requests'),'[]')
 FROM inbox_items i JOIN observations o ON i.repository=o.repository AND i.actor_id=o.actor_id AND i.pull_id=o.pull_id
 JOIN discovery_scopes s ON s.repository=i.repository AND s.actor_id=i.actor_id AND json_extract(s.record,'$.state')!='inactive' WHERE i.id=?
 ON CONFLICT(repository,actor_id,pull_id) DO UPDATE SET following=excluded.following,ignored_requests=excluded.ignored_requests`, flag, id)
	if err = changed(result, err, "item is unavailable or belongs to another synchronization identity"); err != nil {
		return err
	}
	if !following {
		if _, err = tx.ExecContext(ctx, `UPDATE inbox_items SET record=json_set(record,'$.state','skipped')
 WHERE category='reviews_owed' AND (repository,actor_id,pull_id)=
 (SELECT repository,actor_id,pull_id FROM inbox_items WHERE id=?)`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE discovery_scopes SET revision=revision+1,record=json_set(record,'$.revision',revision+1) WHERE repository=(SELECT repository FROM inbox_items WHERE id=?)", id); err != nil {
		return err
	}
	return tx.Commit()
}
