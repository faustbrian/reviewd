package store

import (
	"context"
	"encoding/json"
	"errors"
	"unicode"
)

func executionIdentifier(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

// RecordProgress cannot change lifecycle, publication consent, or an already
// captured primary thread/turn. Initial thread identity precedes turn creation.
func (s *Store) RecordProgress(ctx context.Context, id, thread, turn, requested, effective, activity string, usage json.RawMessage) error {
	if !executionIdentifier(thread) || (turn != "" && !executionIdentifier(turn)) || !executionIdentifier(requested) || !executionIdentifier(effective) {
		return errors.New("active reviewer returned an invalid execution identity")
	}
	switch activity {
	case "starting", "reviewing", "reading_evidence", "thinking", "responding", "delegating", "waiting_input", "finished":
	default:
		return errors.New("active reviewer returned an unknown activity")
	}
	if len(usage) == 0 {
		usage = json.RawMessage(`null`)
	}
	if len(usage) > 64*1024 || !json.Valid(usage) {
		return errors.New("active reviewer usage was invalid or too large")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET thread_id=?,turn_id=?,
 record=json_set(record,'$.requested_model',?,'$.effective_model',?,'$.progress',?,'$.usage',json(?)),updated_at=?
 WHERE id=? AND state IN ('reviewing','handling_feedback')
 AND (COALESCE(thread_id,'')='' OR thread_id=?)
 AND (COALESCE(turn_id,'')='' OR (?!='' AND turn_id=?))`, thread, turn, requested, effective, activity, string(usage), timestamp(), id, thread, turn, turn)
	return changed(result, err, "could not persist active reviewer progress; inspect this run before starting again")
}
