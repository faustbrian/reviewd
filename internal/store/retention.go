package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Archive retains identity, not reusable source evidence or publication consent.
// Reviewed preserves the implicit follow fact established by a valid preview.
type Archive struct {
	ExpiredAt         string `json:"expired_at"`
	Reviewed          bool   `json:"reviewed"`
	HeadSHA           string `json:"head_sha,omitempty"`
	TargetBaseSHA     string `json:"target_base_sha,omitempty"`
	MergeBaseSHA      string `json:"merge_base_sha,omitempty"`
	Effort            string `json:"reasoning_effort,omitempty"`
	PolicyFingerprint string `json:"policy_fingerprint,omitempty"`
}

const RetentionBatchLimit = 25

// Keep unresolved decisions, model work, input delivery and remote publication
// evidence. An old terminal state alone is not proof that publication settled.
// Feedback journals have a different schema and retain uncertain remote effects.
// Ordinary review expiry must not reinterpret or discard their reconciliation facts.
const retentionEligible = `state IN ('failed','cancelled','stale','published')
 AND COALESCE(json_extract(record,'$.purpose'),'review')!='feedback'
 AND json_extract(record,'$.archive') IS NULL
 AND NOT EXISTS(SELECT 1 FROM input_requests q WHERE q.run_id=runs.id AND q.closed_at='')
 AND (state!='published' OR json_extract(outcome,'$.verified')=1)
 AND (NOT EXISTS(SELECT 1 FROM publication_attempts a WHERE a.run_id=runs.id)
 OR (json_extract(outcome,'$.verified')=1 AND COALESCE(json_type(outcome,'$.pending_repair'),'null')='null'))`

// Only proven completed native executions are eligible. Interrupted/failed
// output may be the operator's only record of work that needs inspection.
const nativeRetentionEligible = `state='completed'
 AND json_extract(record,'$.executor') IN ('codex_exec_v1','claude_exec_v1')
 AND COALESCE(json_extract(record,'$.owner_closed'),0)=1
 AND json_extract(record,'$.archive') IS NULL`

func (s *Store) NativeRetentionIDs(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE `+nativeRetentionEligible+` AND updated_at<? ORDER BY updated_at,id LIMIT ?`, cutoff.UTC().Format(time.RFC3339Nano), RetentionBatchLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RetentionIDs returns a bounded candidate set without materializing private
// payloads. The application checks precise UTC age again after parsing it.
func (s *Store) RetentionIDs(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE `+retentionEligible+` AND updated_at<=? ORDER BY updated_at,id LIMIT ?`, cutoff.UTC().Format(time.RFC3339Nano), RetentionBatchLimit)
	if err != nil {
		return nil, errors.New("cannot inspect retained private history")
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ExpireArtifacts atomically removes expired private text while preserving the
// run, input delivery facts, work identity and compact publication receipts.
// Its state/outcome CAS prevents stale maintenance reads from erasing live work.
func (s *Store) ExpireArtifacts(ctx context.Context, observed Run, archive Archive, outcome, specialists json.RawMessage, cutoff time.Time) (bool, error) {
	when, err := time.Parse(time.RFC3339Nano, archive.ExpiredAt)
	if err != nil || !observed.UpdatedAt.Before(cutoff) || when.Before(cutoff) || !json.Valid(specialists) || (len(outcome) > 0 && !json.Valid(outcome)) {
		return false, errors.New("invalid private-history expiry boundary")
	}
	metadata, _ := json.Marshal(archive)
	var compactOutcome any
	if len(outcome) > 0 {
		compactOutcome = string(outcome)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET record=json_set(json_remove(record,'$.context','$.progress','$.progress_at'),
 '$.configuration',json('{}'),'$.specialists',json(?),'$.archive',json(?)),prepared=NULL,outcome=?
 WHERE id=? AND state=? AND updated_at=? AND COALESCE(payload_id,'')=?
 AND COALESCE(prepared,'')=? AND COALESCE(outcome,'')=? AND ((`+retentionEligible+`) OR (`+nativeRetentionEligible+`))`,
		string(specialists), string(metadata), compactOutcome, observed.ID, observed.State, observed.UpdatedAt.UTC().Format(time.RFC3339Nano), observed.PayloadID, string(observed.Prepared), string(observed.Outcome))
	if err != nil {
		return false, errors.New("cannot expire private review artifacts")
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE input_requests SET answers=NULL,
 record=json_set(json_remove(record,'$.questions'),'$.text_expired_at',?) WHERE run_id=?`, archive.ExpiredAt, observed.ID); err != nil {
		return false, errors.New("cannot expire private clarification text")
	}
	// Decisions cannot be reused without their preview; attempts/remote IDs stay
	// as durable evidence and work keys continue to suppress duplicate jobs.
	if _, err := tx.ExecContext(ctx, "DELETE FROM publication_decisions WHERE run_id=?", observed.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
