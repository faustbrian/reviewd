package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

type RetentionReport struct {
	Cutoff    time.Time `json:"cutoff"`
	DryRun    bool      `json:"dry_run"`
	Eligible  []string  `json:"eligible"`
	Expired   []string  `json:"expired"`
	BatchFull bool      `json:"batch_full"`
}

// ExpireHistory performs one bounded local pass. It neither deletes run
// identities nor starts/retries reviews, reconciles publication, or removes
// filesystem resources whose ownership has not been established.
func ExpireHistory(ctx context.Context, durable *store.Store, days int, dryRun bool, now time.Time) (RetentionReport, error) {
	report := RetentionReport{DryRun: dryRun, Eligible: []string{}, Expired: []string{}}
	if days < 1 || days > 3650 || now.IsZero() {
		return report, errors.New("retention requires 1–3650 days and a current clock")
	}
	report.Cutoff = now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ids, err := durable.RetentionIDs(ctx, report.Cutoff)
	if err != nil {
		return report, err
	}
	report.BatchFull = len(ids) == store.RetentionBatchLimit
	for _, id := range ids {
		run, err := durable.Get(ctx, id)
		if err != nil {
			return report, err
		}
		if !run.UpdatedAt.Before(report.Cutoff) || run.Archive != nil {
			continue
		}
		archive := store.Archive{ExpiredAt: now.UTC().Format(time.RFC3339Nano)}
		// A failed capture need not contain a reusable complete policy/result.
		// Preserve only its scope-matched source identity, never promote it to a
		// completed review or make expiry depend on decoding unrelated fields.
		var captured struct {
			Pull         github.Pull `json:"pull"`
			MergeBaseSHA string      `json:"merge_base_sha"`
		}
		if json.Unmarshal(run.Context, &captured) == nil && captured.Pull.Repository == run.Repository && captured.Pull.ID == run.PullID && captured.Pull.RepositoryID == run.RepositoryID {
			archive.HeadSHA = captured.Pull.HeadSHA
			archive.TargetBaseSHA = captured.Pull.TargetBaseSHA
			archive.MergeBaseSHA = captured.MergeBaseSHA
		}
		var policy config.Snapshot
		if json.Unmarshal(run.Configuration, &policy) == nil {
			archive.Effort = policy.String("reviewer.reasoning_effort")
			archive.PolicyFingerprint = policy.Fingerprint
		}
		if len(run.Prepared) > 0 {
			var prepared PreparedResult
			if json.Unmarshal(run.Prepared, &prepared) != nil || ValidatePrepared(prepared) != nil || prepared.PayloadID != run.PayloadID {
				return report, fmt.Errorf("review %s has invalid private evidence; its history was not expired", run.ID)
			}
			archive.Reviewed = true
			archive.HeadSHA = prepared.Boundary.Pull.HeadSHA
			archive.TargetBaseSHA = prepared.Boundary.Pull.TargetBaseSHA
			archive.MergeBaseSHA = prepared.Boundary.MergeBaseSHA
		}
		var compact json.RawMessage
		if len(run.Outcome) > 0 {
			var outcome PublicationOutcome
			if json.Unmarshal(run.Outcome, &outcome) != nil {
				return report, fmt.Errorf("review %s has invalid publication evidence; preserve its history", run.ID)
			}
			outcome.Review.Body = ""
			for i := range outcome.Comments {
				outcome.Comments[i].Body = ""
			}
			for i := range outcome.Repairs {
				outcome.Repairs[i].Body = ""
			}
			outcome.CurrentChecks = nil
			compact, _ = json.Marshal(outcome)
		}
		specialists := []codex.SpecialistOutput{}
		if len(run.Specialists) > 0 && json.Unmarshal(run.Specialists, &specialists) != nil {
			return report, fmt.Errorf("review %s has invalid specialist history; preserve it", run.ID)
		}
		for i := range specialists {
			specialists[i].Task, specialists[i].Text = "", ""
			if specialists[i].Error != "" {
				specialists[i].Error = "Specialist did not complete; private diagnostics expired."
			}
		}
		leaves, _ := json.Marshal(specialists)
		report.Eligible = append(report.Eligible, id)
		if !dryRun {
			won, err := durable.ExpireArtifacts(ctx, run, archive, compact, leaves, report.Cutoff)
			if err != nil {
				return report, err
			}
			if won {
				report.Expired = append(report.Expired, id)
			}
		}
	}
	return report, nil
}
