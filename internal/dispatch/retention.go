package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
	"time"
)

type RetentionReport struct {
	Expired  []string `json:"expired"`
	Eligible []string `json:"eligible"`
}

// ExpireHistory retains durable identity/deduplication and every uncertain or
// legacy workflow. Only completed native text is subject to this retention.
func ExpireHistory(ctx context.Context, s *store.Store, days int, dry bool, now time.Time) (RetentionReport, error) {
	report := RetentionReport{Expired: []string{}, Eligible: []string{}}
	if days < 1 || days > 3650 || now.IsZero() {
		return report, errors.New("retention requires 1–3650 days and a current clock")
	}
	cutoff := now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
	ids, err := s.NativeRetentionIDs(ctx, cutoff)
	if err != nil {
		return report, err
	}
	for _, id := range ids {
		run, err := s.Get(ctx, id)
		if err != nil {
			return report, err
		}
		var output agent.Output
		if err := json.Unmarshal(run.Outcome, &output); err != nil {
			return report, err
		}
		archive := store.Archive{ExpiredAt: now.UTC().Format(time.RFC3339Nano)}
		var policy config.Snapshot
		if json.Unmarshal(run.Configuration, &policy) == nil {
			prefix := "reviewer."
			if run.Purpose == "feedback" {
				prefix = "feedback."
			}
			archive.Effort = policy.String(prefix + "reasoning_effort")
			archive.PolicyFingerprint = policy.Fingerprint
		}
		report.Eligible = append(report.Eligible, id)
		if dry {
			continue
		}
		output.Text, output.Diagnostic = "", ""
		compact, err := json.Marshal(output)
		if err != nil {
			return report, err
		}
		expired, err := s.ExpireArtifacts(ctx, run, archive, compact, json.RawMessage(`[]`), cutoff)
		if err != nil {
			return report, err
		}
		if expired {
			report.Expired = append(report.Expired, id)
		}
	}
	return report, nil
}
