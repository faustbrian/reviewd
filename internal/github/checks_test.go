package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Aggregate green does not settle a trusted mandatory check; the exact head,
// check context and required integration are the oracle.
func TestMandatoryChecksClassifyExactTrustedEvidence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, scenario := range []string{"passed", "failed", "pending", "skipped", "missing", "wrong head", "wrong app", "unavailable rules"} {
		t.Run(scenario, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo/rules/branches/main":
					if scenario == "unavailable rules" {
						w.WriteHeader(403)
						return
					}
					fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":77}]}}]`)
				case "/repos/owner/repo/branches/main":
					fmt.Fprint(w, `{"protected":false,"commit":{"sha":"`+sha+`"}}`)
				case "/repos/owner/repo/commits/" + sha + "/check-runs":
					if scenario == "missing" {
						fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
						return
					}
					status, conclusion, head, app := "completed", "success", sha, 77
					if scenario == "failed" {
						conclusion = "failure"
					}
					if scenario == "skipped" {
						conclusion = "skipped"
					}
					if scenario == "pending" {
						status = "in_progress"
						conclusion = ""
					}
					if scenario == "wrong head" {
						head = strings.Repeat("b", 40)
					}
					if scenario == "wrong app" {
						app = 88
					}
					fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":5,"name":"quality","head_sha":%q,"status":%q,"conclusion":%q,"app":{"id":%d}}]}`, head, status, conclusion, app)
				case "/repos/owner/repo/commits/" + sha + "/statuses":
					fmt.Fprint(w, `[{"id":9,"context":"aggregate","state":"success"}]`)
				default:
					t.Errorf("unexpected check acquisition: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			report, err := client.Checks(context.Background(), Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 1}, BaseRef: "main", HeadSHA: sha, TargetBaseSHA: sha})
			if err != nil {
				t.Fatal(err)
			}
			expected := scenario
			if scenario == "wrong head" || scenario == "wrong app" {
				expected = "missing"
			}
			if scenario == "unavailable rules" {
				expected = "unavailable"
			}
			if len(report.Checks) != 1 || report.Checks[0].State != expected || (report.Ready != (scenario == "passed")) {
				t.Fatalf("mandatory check state guessed from aggregate/other identity: %+v", report)
			}
		})
	}
}

// List-statuses objects do not contain a SHA: identity belongs to the pinned
// request. If a check and status share a required name, neither can hide the
// other. Separate components preserve a pending sibling alongside a failure.
func TestMandatoryLegacyAndDualSourceChecks(t *testing.T) {
	sha := strings.Repeat("a", 40)
	scenarios := []struct {
		name, status, run, wantStatus, wantRun string
		integration                            int
		ready                                  bool
	}{
		{name: "legacy success", status: "success", wantStatus: "passed", ready: true},
		{name: "legacy pending", status: "pending", wantStatus: "pending"},
		{name: "legacy failure", status: "failure", wantStatus: "failed"},
		{name: "legacy error", status: "error", wantStatus: "failed"},
		{name: "unknown legacy", status: "unexpected", wantStatus: "unavailable"},
		{name: "both passed", status: "success", run: "success", wantStatus: "passed", wantRun: "passed", ready: true},
		{name: "failed status cannot hide", status: "failure", run: "success", wantStatus: "failed", wantRun: "passed"},
		{name: "pending status cannot hide", status: "pending", run: "failure", wantStatus: "pending", wantRun: "failed"},
		{name: "failed run cannot hide", status: "success", run: "failure", wantStatus: "passed", wantRun: "failed"},
		{name: "pinned integration is not proven by legacy", status: "success", integration: 77, wantRun: "missing"},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo/rules/branches/main":
					fmt.Fprintf(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"quality","integration_id":%d}]}}]`, scenario.integration)
				case "/repos/owner/repo/branches/main":
					fmt.Fprint(w, `{"protected":false}`)
				case "/repos/owner/repo/commits/" + sha + "/check-runs":
					if scenario.run == "" {
						fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
					} else {
						fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":5,"name":"quality","head_sha":%q,"status":"completed","conclusion":%q,"app":{"id":77}}]}`, sha, scenario.run)
					}
				case "/repos/owner/repo/commits/" + sha + "/statuses":
					// The endpoint promises reverse chronology, not sorting by numeric ID.
					json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "context": "quality", "state": scenario.status}, {"id": 99, "context": "quality", "state": "success"}})
				default:
					t.Errorf("unscoped check acquisition: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			report, err := client.Checks(context.Background(), Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 1}, BaseRef: "main", HeadSHA: sha, TargetBaseSHA: sha})
			if err != nil {
				t.Fatal(err)
			}
			states := map[string]string{}
			for _, check := range report.Checks {
				states[check.Evidence] = check.State
			}
			if report.Ready != scenario.ready || states["commit_status"] != scenario.wantStatus || states["check_run"] != scenario.wantRun {
				t.Fatalf("required verification lost a source: %+v", report)
			}
		})
	}
}

func TestMandatoryRulesExhaustPaginationAndFailClosed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, scenario := range []string{"later required", "short linked", "later denied", "stalled"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo/rules/branches/main":
					requests++
					page := r.URL.Query().Get("page")
					if page == "2" && scenario == "later denied" {
						w.WriteHeader(403)
						return
					}
					if page == "2" && scenario != "stalled" {
						fmt.Fprint(w, `[{"type":"required_status_checks","ruleset_id":901,"parameters":{"required_status_checks":[{"context":"security","integration_id":0}]}}]`)
						return
					}
					count := 30
					if r.URL.Query().Get("per_page") == "100" && scenario != "short linked" {
						count = 100
					}
					rules := make([]map[string]any, count)
					for i := range rules {
						rules[i] = map[string]any{"type": "creation", "ruleset_id": i + 1, "ruleset_source_type": "Organization", "ruleset_source": "owner"}
					}
					rules[0] = map[string]any{"type": "required_status_checks", "ruleset_id": 900, "parameters": map[string]any{"required_status_checks": []map[string]any{{"context": "quality", "integration_id": 0}}}}
					w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?per_page=100&page=2>; rel="next"`)
					json.NewEncoder(w).Encode(rules)
				case "/repos/owner/repo/branches/main":
					fmt.Fprint(w, `{"protected":false}`)
				case "/repos/owner/repo/commits/" + sha + "/check-runs":
					fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":1,"name":"quality","head_sha":%q,"status":"completed","conclusion":"success","app":{"id":77}}]}`, sha)
				case "/repos/owner/repo/commits/" + sha + "/statuses":
					fmt.Fprint(w, `[]`)
				default:
					t.Errorf("unexpected acquisition %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer peer.Close()
			client, _ := New(Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
			report, err := client.Checks(context.Background(), Pull{PullReference: PullReference{Repository: "github.com/owner/repo", Number: 1}, BaseRef: "main", HeadSHA: sha, TargetBaseSHA: sha})
			if err != nil {
				t.Fatal(err)
			}
			if report.Ready || requests < 2 {
				t.Fatalf("partial rule definition became ready: requests=%d report=%+v", requests, report)
			}
			expected := "missing"
			if scenario != "later required" && scenario != "short linked" {
				expected = "unavailable"
			}
			found := false
			for _, check := range report.Checks {
				if check.State == expected {
					found = true
				}
			}
			if !found {
				t.Fatalf("later rule outcome lost: %+v", report)
			}
		})
	}
}
