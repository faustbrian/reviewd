package discovery

import (
	"encoding/json"

	"github.com/faustbrian/reviewd/internal/store"
)

// Legacy Prepare (726c8a8, internal/review/result.go) stored a SHA-256 of this
// ordered envelope with an empty payload_id. Keep only its implicit-follow
// fact, as Archive.Reviewed does; never reuse its preview or publication consent.
// Raw nested values preserve the historical serialization without retaining the
// removed review/check/anchor/publication policy engine.
func legacyReviewed(run store.Run) bool {
	if run.Executor != "" || run.Purpose == "feedback" || run.PayloadID == "" || run.ThreadID == "" || run.TurnID == "" {
		return false
	}
	var preview struct {
		SchemaVersion int             `json:"schema_version"`
		Boundary      json.RawMessage `json:"boundary"`
		Report        json.RawMessage `json:"report"`
		Disposition   string          `json:"disposition"`
		Event         string          `json:"event"`
		Body          string          `json:"body"`
		Comments      json.RawMessage `json:"comments"`
		Gaps          json.RawMessage `json:"gaps"`
		PayloadID     string          `json:"payload_id"`
	}
	if json.Unmarshal(run.Prepared, &preview) != nil || (preview.SchemaVersion != 1 && preview.SchemaVersion != 2) || preview.PayloadID == "" {
		return false
	}
	payloadID := preview.PayloadID
	preview.PayloadID = ""
	return key(preview) == payloadID
}
