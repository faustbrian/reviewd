package review

import (
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
)

func TestPreparedResultCannotApproveGapsSelfOrInventedAnchors(t *testing.T) {
	boundary := Boundary{Pull: github.Pull{PullReference: github.PullReference{Repository: "github.com/owner/repo", Number: 17}, Author: github.Identity{ID: 42, Login: "author"}, TargetBaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}, Actor: github.Identity{ID: 43, Login: "reviewer"}, MergeBaseSHA: strings.Repeat("c", 40), Anchors: source.AnchorSet{{Path: "file.go", Side: "RIGHT", Start: 1, End: 3}}, Checks: github.CheckReport{Ready: true, Checks: []github.MandatoryCheck{}}}
	clean := Report{Conclusion: "APPROVE", Summary: "", Findings: []Finding{}, Gaps: []ResultGap{}}
	prepared, err := Prepare(boundary, clean)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Event != "APPROVE" || prepared.Body != "**No findings.**" || prepared.PayloadID == "" {
		t.Fatal("clean exact preview is not canonical")
	}
	boundary.Actor.ID = 42
	self, err := Prepare(boundary, clean)
	if err != nil || self.Event != "COMMENT" || self.Disposition != "APPROVE" {
		t.Fatal("self-review confused internal opinion and GitHub event")
	}
	boundary.Actor.ID = 43
	boundary.Checks.Ready = false
	boundary.Checks.Checks = []github.MandatoryCheck{{Name: "quality", State: "missing"}}
	gap, err := Prepare(boundary, clean)
	if err != nil || gap.Event != "REQUEST_CHANGES" || !strings.HasPrefix(gap.Body, "**Not ready.**") {
		t.Fatal("missing mandatory verification became clean approval")
	}
	boundary.Checks.Ready = true
	boundary.Checks.Checks = []github.MandatoryCheck{}
	for _, bad := range []Report{{Conclusion: "APPROVE", Findings: []Finding{{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 99, Body: "fabricated anchor"}}}, {Conclusion: "APPROVE", Findings: []Finding{{Kind: "suggestion", Path: "file.go", Side: "RIGHT", Line: 2, Body: "token ghp_" + strings.Repeat("a", 40)}}}, {Conclusion: "MAYBE"}} {
		if _, err := Prepare(boundary, bad); err == nil {
			t.Error("unusable/sensitive result returned publishable preview")
		}
	}
	blocking := Report{Conclusion: "REQUEST_CHANGES", Findings: []Finding{{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 2, Body: "This removes the required caller contract."}}, Gaps: []ResultGap{}}
	result, err := Prepare(boundary, blocking)
	if err != nil || result.Event != "REQUEST_CHANGES" || result.Comments[0].Body != "**Blocker:** This removes the required caller contract." || !strings.HasPrefix(result.Body, "**Changes needed.**") {
		t.Fatal("supported source finding lost publication contract")
	}
}

func TestRefreshedChecksSupersedeOnlyIdentifiedCheckObservations(t *testing.T) {
	boundary := Boundary{Pull: github.Pull{Author: github.Identity{ID: 42}}, Actor: github.Identity{ID: 43}, Anchors: source.AnchorSet{{Path: "file.go", Side: "RIGHT", Start: 1, End: 3}}, Checks: github.CheckReport{Ready: true, Checks: []github.MandatoryCheck{{Name: "quality", State: "passed"}}}}
	report := Report{Conclusion: "REQUEST_CHANGES", Findings: []Finding{}, Gaps: []ResultGap{{Kind: "readiness", Detail: "Quality failed at capture.", Check: "quality"}}}
	result, err := Prepare(boundary, report)
	if err != nil || result.Event != "APPROVE" || result.Body != "**No findings.**" || len(result.Gaps) != 0 {
		t.Fatalf("obsolete check observation remained authoritative: %+v %v", result, err)
	}
	report.Findings = []Finding{{Kind: "readiness_gap", Path: "file.go", Side: "RIGHT", Line: 2, Body: "Captured quality failure.", Check: "quality"}}
	result, err = Prepare(boundary, report)
	if err != nil || len(result.Comments) != 0 || result.Event != "APPROVE" {
		t.Fatal("obsolete CI became a source inline finding")
	}
	report.Gaps = append(report.Gaps, ResultGap{Kind: "readiness", Detail: "Required compatibility rehearsal remains unavailable."})
	result, err = Prepare(boundary, report)
	if err != nil || result.Event != "REQUEST_CHANGES" || len(result.Gaps) != 1 || !strings.Contains(result.Body, "compatibility rehearsal") {
		t.Fatal("check refresh discarded unrelated mandatory readiness")
	}
	report.Gaps = report.Gaps[:1]
	report.Findings = []Finding{{Kind: "blocker", Path: "file.go", Side: "RIGHT", Line: 2, Body: "This breaks the existing caller."}}
	result, err = Prepare(boundary, report)
	if err != nil || result.Event != "REQUEST_CHANGES" || len(result.Comments) != 1 || !strings.HasPrefix(result.Body, "**Changes needed.**") {
		t.Fatal("check refresh discarded source defect")
	}
	report.Findings = nil
	report.Gaps[0].Check = "unknown"
	if _, err := Prepare(boundary, report); err == nil {
		t.Fatal("invented check identity became authority")
	}
}

func TestSourceUncertaintySurvivesSettledCheckObservation(t *testing.T) {
	boundary := Boundary{Pull: github.Pull{Author: github.Identity{ID: 42}}, Actor: github.Identity{ID: 43}, Checks: github.CheckReport{Ready: true, Checks: []github.MandatoryCheck{{Name: "quality", State: "passed"}}}}
	report := Report{Conclusion: "INCONCLUSIVE", Summary: "Caller compatibility cannot be established because the external consumer contract is unavailable.", Findings: []Finding{}, Gaps: []ResultGap{{Kind: "readiness", Detail: "Quality was pending at capture.", Check: "quality"}}}
	result, err := Prepare(boundary, report)
	if err != nil || result.Disposition != "INCONCLUSIVE" || result.Event != "COMMENT" || !strings.HasPrefix(result.Body, "**Inconclusive.**") || !strings.Contains(result.Body, report.Summary) {
		t.Fatalf("settled CI erased source uncertainty: disposition=%s event=%s body=%q error=%v", result.Disposition, result.Event, result.Body, err)
	}
}
