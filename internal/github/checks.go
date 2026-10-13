package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
)

type MandatoryCheck struct {
	Name          string `json:"name"`
	IntegrationID int64  `json:"integration_id,omitempty"`
	State         string `json:"state"`
	Evidence      string `json:"evidence,omitempty"`
	Detail        string `json:"detail,omitempty"`
}
type CheckReport struct {
	HeadSHA     string           `json:"head_sha"`
	Ready       bool             `json:"ready"`
	RulesDigest string           `json:"rules_digest"`
	Checks      []MandatoryCheck `json:"checks"`
}
type requiredCheck struct {
	Context       string `json:"context"`
	IntegrationID *int64 `json:"integration_id"`
}
type branchRule struct {
	Type       string `json:"type"`
	Parameters struct {
		Checks    *[]requiredCheck `json:"required_status_checks"`
		Workflows *[]struct {
			Path         string `json:"path"`
			RepositoryID int64  `json:"repository_id"`
			Ref          string `json:"ref"`
			SHA          string `json:"sha"`
		} `json:"workflows"`
	} `json:"parameters"`
}
type checkRun struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}
type commitStatus struct {
	ID      int64  `json:"id"`
	Context string `json:"context"`
	State   string `json:"state"`
}

func unavailableChecks(head, detail string) CheckReport {
	return CheckReport{HeadSHA: head, Checks: []MandatoryCheck{{Name: "required verification", State: "unavailable", Detail: detail}}}
}

// Checks resolves active trusted rules, classic protection and exact-head
// evidence. An inaccessible definition cannot be inferred from aggregate CI.
func (c *Client) Checks(ctx context.Context, pull Pull) (CheckReport, error) {
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil {
		return CheckReport{}, err
	}
	if !objectPattern.MatchString(pull.HeadSHA) || !objectPattern.MatchString(pull.TargetBaseSHA) || pull.BaseRef == "" {
		return CheckReport{}, errors.New("mandatory verification requires a captured immutable PR boundary")
	}
	rawRules, err := c.rawPages(ctx, prefix+"/rules/branches/"+url.PathEscape(pull.BaseRef))
	if err != nil {
		if ctx.Err() != nil {
			return CheckReport{}, ctx.Err()
		}
		return unavailableChecks(pull.HeadSHA, "Complete active branch rules could not be resolved; check repository permissions, pagination and GitHub rules API support."), nil
	}
	rules := []branchRule{}
	for _, raw := range rawRules {
		var rule branchRule
		if json.Unmarshal(raw, &rule) != nil || rule.Type == "" {
			return unavailableChecks(pull.HeadSHA, "Active branch-rule definitions were incomplete."), nil
		}
		rules = append(rules, rule)
	}

	required := []MandatoryCheck{}
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			if rule.Parameters.Checks == nil {
				return unavailableChecks(pull.HeadSHA, "Required status-check definitions were incomplete."), nil
			}
			for _, check := range *rule.Parameters.Checks {
				app := int64(0)
				if check.IntegrationID != nil && *check.IntegrationID > 0 {
					app = *check.IntegrationID
				}
				if check.Context == "" || (check.IntegrationID != nil && *check.IntegrationID < -1) {
					return unavailableChecks(pull.HeadSHA, "Required check identity was invalid."), nil
				}
				required = append(required, MandatoryCheck{Name: check.Context, IntegrationID: app})
			}
		}
		if rule.Type == "workflows" {
			if rule.Parameters.Workflows == nil {
				return unavailableChecks(pull.HeadSHA, "Required workflow identities were incomplete."), nil
			}
			for _, workflow := range *rule.Parameters.Workflows {
				// A display name alone does not prove the required repository,
				// path and immutable workflow revision. Preserve that gap.
				required = append(required, MandatoryCheck{Name: "workflow: " + workflow.Path, State: "unavailable", Detail: "Required reusable-workflow source identity has not been verified against this head."})
			}
		}
	}
	var branch struct {
		Protected *bool `json:"protected"`
	}
	if err := c.get(ctx, prefix+"/branches/"+url.PathEscape(pull.BaseRef), &branch); err != nil {
		if ctx.Err() != nil {
			return CheckReport{}, ctx.Err()
		}
		return unavailableChecks(pull.HeadSHA, "Target branch protection could not be resolved."), nil
	}
	if branch.Protected == nil {
		return unavailableChecks(pull.HeadSHA, "GitHub omitted target branch protection evidence."), nil
	}
	var classic json.RawMessage
	if *branch.Protected {
		var protection struct {
			Contexts *[]string `json:"contexts"`
			Checks   *[]struct {
				Context string `json:"context"`
				AppID   *int64 `json:"app_id"`
			} `json:"checks"`
		}
		if err := c.get(ctx, prefix+"/branches/"+url.PathEscape(pull.BaseRef)+"/protection/required_status_checks", &classic); err != nil {
			if ctx.Err() != nil {
				return CheckReport{}, ctx.Err()
			}
			return unavailableChecks(pull.HeadSHA, "Classic required-check protection is inaccessible; a 404 is not proof that a protected branch has no additional mandatory checks."), nil
		}
		if json.Unmarshal(classic, &protection) != nil || protection.Contexts == nil || protection.Checks == nil {
			return unavailableChecks(pull.HeadSHA, "Classic required-check protection was incomplete."), nil
		}
		covered := map[string]bool{}
		for _, check := range *protection.Checks {
			if check.Context == "" || (check.AppID != nil && *check.AppID < -1) {
				return unavailableChecks(pull.HeadSHA, "Classic required-check identity was invalid."), nil
			}
			app := int64(0)
			if check.AppID != nil && *check.AppID > 0 {
				app = *check.AppID
			}
			required = append(required, MandatoryCheck{Name: check.Context, IntegrationID: app})
			covered[check.Context] = true
		}
		for _, name := range *protection.Contexts {
			if name == "" {
				return unavailableChecks(pull.HeadSHA, "Classic required-check context was empty."), nil
			}
			if !covered[name] {
				required = append(required, MandatoryCheck{Name: name})
			}
		}
	}
	definition, _ := json.Marshal(struct {
		Rules     []json.RawMessage
		Protected bool
		Classic   json.RawMessage
	}{rawRules, *branch.Protected, classic})
	digest := sha256.Sum256(definition)
	report := CheckReport{HeadSHA: pull.HeadSHA, Ready: true, RulesDigest: hex.EncodeToString(digest[:]), Checks: []MandatoryCheck{}}
	runs, statuses, err := c.checkEvidence(ctx, prefix, pull.HeadSHA)
	if err != nil {
		if ctx.Err() != nil {
			return CheckReport{}, ctx.Err()
		}
		report.Ready = false
		report.Checks = append([]MandatoryCheck{}, required...)
		if len(report.Checks) == 0 {
			report.Checks = []MandatoryCheck{{Name: "required verification"}}
		}
		for i := range report.Checks {
			report.Checks[i].State = "unavailable"
			report.Checks[i].Detail = "Exact-head check evidence was incomplete or inaccessible."
		}
		return report, nil
	}
	seen := map[string]bool{}
	for _, check := range required {
		key := check.Name + "\x00" + strconv.FormatInt(check.IntegrationID, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		components := []MandatoryCheck{check}
		if check.State == "" {
			check.State = "missing"
			check.Evidence = "check_run"
			var latestRun checkRun
			for _, run := range runs {
				if run.HeadSHA == pull.HeadSHA && run.Name == check.Name && (check.IntegrationID == 0 || check.IntegrationID == run.App.ID) && run.ID > latestRun.ID {
					latestRun = run
				}
			}
			var latestStatus commitStatus
			// This list is fetched from the immutable head endpoint and GitHub
			// returns it in reverse chronology. Status objects do not have a SHA.
			for _, status := range statuses {
				if status.Context == check.Name {
					latestStatus = status
					break
				}
			}
			if latestRun.ID > 0 {
				check.State = checkRunState(latestRun)
				check.Detail = "GitHub check-run evidence."
				components = []MandatoryCheck{check}
				if latestStatus.ID > 0 {
					// GitHub requires both same-name sources to settle; a passed run
					// cannot hide a failed or pending legacy status (or vice versa).
					components = append(components, MandatoryCheck{Name: check.Name, Evidence: "commit_status", State: commitStatusState(latestStatus), Detail: "Legacy commit-status evidence."})
				}
			} else if check.IntegrationID == 0 && latestStatus.ID > 0 {
				check.Evidence = "commit_status"
				check.State = commitStatusState(latestStatus)
				check.Detail = "Legacy commit-status evidence."
				components = []MandatoryCheck{check}
			} else {
				components = []MandatoryCheck{check}
			}
		}
		for _, component := range components {
			if component.State != "passed" {
				report.Ready = false
			}
			report.Checks = append(report.Checks, component)
		}
	}
	sort.Slice(report.Checks, func(i, j int) bool {
		a, b := report.Checks[i], report.Checks[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.IntegrationID != b.IntegrationID {
			return a.IntegrationID < b.IntegrationID
		}
		return a.Evidence < b.Evidence
	})
	return report, nil
}

func commitStatusState(status commitStatus) string {
	switch status.State {
	case "success":
		return "passed"
	case "pending":
		return "pending"
	case "failure", "error":
		return "failed"
	default:
		return "unavailable"
	}
}

func checkRunState(run checkRun) string {
	if run.Status != "completed" {
		return "pending"
	}
	if run.Conclusion == nil {
		return "unavailable"
	}
	switch *run.Conclusion {
	case "success":
		return "passed"
	case "neutral", "skipped":
		return "skipped"
	case "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
		return "failed"
	default:
		return "unavailable"
	}
}

func (c *Client) checkEvidence(ctx context.Context, prefix, sha string) ([]checkRun, []commitStatus, error) {
	runs := []checkRun{}
	statuses := []commitStatus{}
	seen := map[int64]bool{}
	complete := false
	for page := 1; page <= 200; page++ {
		var response struct {
			Total *int        `json:"total_count"`
			Runs  *[]checkRun `json:"check_runs"`
		}
		if err := c.get(ctx, prefix+"/commits/"+sha+"/check-runs?filter=latest&per_page=100&page="+strconv.Itoa(page), &response); err != nil {
			return nil, nil, err
		}
		if response.Total == nil || response.Runs == nil || *response.Total < 0 {
			return nil, nil, errors.New("check-run evidence was incomplete")
		}
		for _, run := range *response.Runs {
			if run.ID <= 0 || run.Name == "" || run.Status == "" || seen[run.ID] {
				return nil, nil, errors.New("check-run pagination stalled or omitted identity")
			}
			seen[run.ID] = true
			runs = append(runs, run)
		}
		if len(*response.Runs) < 100 {
			if len(runs) != *response.Total {
				return nil, nil, errors.New("check-run count did not reconcile")
			}
			complete = true
			break
		}
	}
	if !complete {
		return nil, nil, errors.New("check-run pagination exceeded its explicit bound")
	}
	seen = map[int64]bool{}
	complete = false
	for page := 1; page <= 200; page++ {
		var response []commitStatus
		if err := c.get(ctx, prefix+"/commits/"+sha+"/statuses?per_page=100&page="+strconv.Itoa(page), &response); err != nil {
			return nil, nil, err
		}
		if response == nil {
			return nil, nil, errors.New("commit-status evidence was incomplete")
		}
		for _, status := range response {
			if status.ID <= 0 || status.Context == "" || seen[status.ID] {
				return nil, nil, errors.New("commit-status pagination stalled or omitted identity")
			}
			seen[status.ID] = true
			statuses = append(statuses, status)
		}
		if len(response) < 100 {
			complete = true
			break
		}
	}
	if !complete {
		return nil, nil, errors.New("commit-status pagination exceeded its explicit bound")
	}
	return runs, statuses, nil
}
