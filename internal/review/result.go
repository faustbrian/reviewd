// Package review prepares private results. Publication belongs to a separate
// application operation; neither model output nor a clean opinion grants it.
package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
)

type ResultGap struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Check  string `json:"check,omitempty"`
}
type Finding struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Side  string `json:"side"`
	Line  int    `json:"line"`
	Body  string `json:"body"`
	Check string `json:"check,omitempty"`
}
type Report struct {
	Conclusion string      `json:"conclusion"`
	Summary    string      `json:"summary"`
	Findings   []Finding   `json:"findings"`
	Gaps       []ResultGap `json:"gaps"`
}
type SkillIdentity struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}
type ExistingFinding struct {
	Key       string `json:"key"`
	CommentID int64  `json:"comment_id"`
	URL       string `json:"url"`
}
type Boundary struct {
	ExistingFindings []ExistingFinding  `json:"existing_findings,omitempty"`
	Pull             github.Pull        `json:"pull"`
	Actor            github.Identity    `json:"actor"`
	MergeBaseSHA     string             `json:"merge_base_sha"`
	ContextDigest    string             `json:"context_digest"`
	Configuration    config.Snapshot    `json:"configuration"`
	Skill            SkillIdentity      `json:"skill"`
	Checks           github.CheckReport `json:"checks"`
	Anchors          source.AnchorSet   `json:"anchors"`
	SourceGaps       []source.Gap       `json:"source_gaps"`
	ContextGaps      []string           `json:"context_gaps"`
}
type Comment struct {
	Path string `json:"path"`
	Side string `json:"side"`
	Line int    `json:"line"`
	Body string `json:"body"`
	Key  string `json:"key"`
}
type PreparedResult struct {
	SchemaVersion int         `json:"schema_version"`
	Boundary      Boundary    `json:"boundary"`
	Report        Report      `json:"report"`
	Disposition   string      `json:"disposition"`
	Event         string      `json:"event"`
	Body          string      `json:"body"`
	Comments      []Comment   `json:"comments"`
	Gaps          []ResultGap `json:"gaps"`
	PayloadID     string      `json:"payload_id"`
}

var credentialShape = regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{20,}|gh[opusr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,}`)

func publicationText(value string) bool {
	if credentialShape.MatchString(value) {
		return false
	}
	for _, char := range value {
		if (char < 32 && char != '\n' && char != '\t') || char == 127 {
			return false
		}
	}
	return true
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func CommentKey(comment Comment) string {
	return digest(struct {
		Path string `json:"path"`
		Side string `json:"side"`
		Line int    `json:"line"`
		Body string `json:"body"`
	}{comment.Path, comment.Side, comment.Line, comment.Body})
}
func DecodeReport(text string) (Report, error) {
	var result Report
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return Report{}, errors.New("Codex did not return the selected structured review contract; no result was prepared")
	}
	if result.Findings == nil || result.Gaps == nil {
		return Report{}, errors.New("Codex omitted complete findings or gap classification")
	}
	return result, nil
}

func Prepare(boundary Boundary, report Report) (PreparedResult, error) {
	return prepareVersion(boundary, report, 2)
}

func prepareVersion(boundary Boundary, report Report, version int) (PreparedResult, error) {
	if version == 1 && len(boundary.ExistingFindings) > 0 {
		return PreparedResult{}, errors.New("legacy prepared review cannot contain publication references")
	}
	if report.Conclusion != "APPROVE" && report.Conclusion != "REQUEST_CHANGES" && report.Conclusion != "INCONCLUSIVE" {
		return PreparedResult{}, errors.New("reviewer returned no supported internal disposition")
	}
	if !publicationText(report.Summary) {
		return PreparedResult{}, errors.New("review summary contains credential-shaped or unsafe publication text; keep sensitive evidence private")
	}
	result := PreparedResult{SchemaVersion: version, Boundary: boundary, Report: report, Comments: []Comment{}, Gaps: []ResultGap{}}
	blocker, readiness, inconclusive, actions := false, false, false, false
	checkReason := false
	knownCheck := func(name string) bool {
		for _, check := range boundary.Checks.Checks {
			if check.Name == name {
				return true
			}
		}
		return false
	}
	for _, gap := range report.Gaps {
		if gap.Check != "" {
			if gap.Kind != "readiness" || !knownCheck(gap.Check) || strings.TrimSpace(gap.Detail) == "" || !publicationText(gap.Detail) {
				return PreparedResult{}, errors.New("check observation does not identify an acquired mandatory check")
			}
			checkReason = true
			continue
		}
		result.Gaps = append(result.Gaps, gap)
	}
	references := map[string]ExistingFinding{}
	for _, existing := range boundary.ExistingFindings {
		if existing.CommentID <= 0 || len(existing.Key) != 64 || !reviewURL(existing.URL, boundary.Pull.URL, "discussion_r", existing.CommentID) {
			return PreparedResult{}, errors.New("existing finding reference has an invalid canonical identity")
		}
		if _, duplicate := references[existing.Key]; duplicate {
			return PreparedResult{}, errors.New("duplicate existing finding reference")
		}
		references[existing.Key] = existing
	}
	referenced := []ExistingFinding{}
	seen := map[string]bool{}
	labels := map[string]string{"blocker": "**Blocker:**", "readiness_gap": "**Readiness gap:**", "action": "**Action requested:**", "question": "**Question:**", "suggestion": "**Suggestion:**"}
	for _, finding := range report.Findings {
		if finding.Check != "" {
			if finding.Kind != "readiness_gap" || !knownCheck(finding.Check) || strings.TrimSpace(finding.Body) == "" || !publicationText(finding.Body) {
				return PreparedResult{}, errors.New("check-derived finding does not identify acquired mandatory verification")
			}
			checkReason = true
			continue
		}
		label, ok := labels[finding.Kind]
		if !ok || strings.TrimSpace(finding.Body) == "" || !publicationText(finding.Body) || !publicationText(finding.Path) || !boundary.Anchors.Contains(finding.Path, finding.Side, finding.Line) {
			return PreparedResult{}, errors.New("review finding has unsupported severity, unsafe text or a noncanonical immutable diff anchor")
		}
		comment := Comment{Path: finding.Path, Side: finding.Side, Line: finding.Line, Body: label + " " + strings.TrimSpace(finding.Body)}
		comment.Key = CommentKey(comment)
		if seen[comment.Key] {
			return PreparedResult{}, errors.New("reviewer returned duplicate canonical findings")
		}
		seen[comment.Key] = true
		if existing, found := references[comment.Key]; found {
			referenced = append(referenced, existing)
		} else {
			result.Comments = append(result.Comments, comment)
		}
		switch finding.Kind {
		case "blocker":
			blocker = true
		case "readiness_gap":
			readiness = true
		case "question":
			inconclusive = true
		default:
			actions = true
		}
	}
	if len(referenced) != len(references) {
		return PreparedResult{}, errors.New("existing finding reference does not match a source finding")
	}
	for _, gap := range boundary.SourceGaps {
		result.Gaps = append(result.Gaps, ResultGap{Kind: "source", Detail: gap.Revision + " " + gap.Path + ": " + gap.Reason})
	}
	for _, gap := range boundary.ContextGaps {
		result.Gaps = append(result.Gaps, ResultGap{Kind: "context", Detail: gap})
	}
	if !boundary.Checks.Ready {
		readiness = true
		for _, check := range boundary.Checks.Checks {
			if check.State != "passed" {
				name := check.Name
				if check.IntegrationID > 0 {
					name += " [app_id=" + strconv.FormatInt(check.IntegrationID, 10) + "]"
				}
				conclusion := check.Conclusion
				if conclusion == "" {
					conclusion = check.State
				}
				detail := "**Readiness gap:** Required check `" + name + "`: " + conclusion + " (" + check.State + ") at head " + boundary.Pull.HeadSHA + ". " + check.Detail
				if version == 1 {
					detail = "Required check " + check.Name + ": " + check.State + ". " + check.Detail
				}
				result.Gaps = append(result.Gaps, ResultGap{Kind: "readiness", Detail: detail})
			}
		}
		if len(boundary.Checks.Checks) == 0 {
			result.Gaps = append(result.Gaps, ResultGap{Kind: "readiness", Detail: "Mandatory verification was not conclusively resolved."})
		}
	}
	for _, gap := range result.Gaps {
		if strings.TrimSpace(gap.Detail) == "" || !publicationText(gap.Detail) {
			return PreparedResult{}, errors.New("review gap contains unusable or sensitive publication text")
		}
		switch gap.Kind {
		case "readiness":
			readiness = true
		case "context", "source":
			inconclusive = true
		default:
			return PreparedResult{}, errors.New("review gap has no supported classification")
		}
	}
	if report.Conclusion == "REQUEST_CHANGES" && !blocker && !readiness && !checkReason {
		return PreparedResult{}, errors.New("change-request disposition has no supported finding or mandatory readiness gap")
	}
	if report.Conclusion == "INCONCLUSIVE" {
		inconclusive = true
		if len(result.Gaps) == 0 && strings.TrimSpace(report.Summary) == "" && len(result.Comments) == 0 {
			return PreparedResult{}, errors.New("inconclusive review omitted its material uncertainty")
		}
	}
	prefix := "**No findings.**"
	result.Disposition = "APPROVE"
	result.Event = "APPROVE"
	switch {
	case blocker:
		prefix = "**Changes needed.**"
		result.Disposition = "REQUEST_CHANGES"
		result.Event = "REQUEST_CHANGES"
	case readiness:
		prefix = "**Not ready.**"
		result.Disposition = "REQUEST_CHANGES"
		result.Event = "REQUEST_CHANGES"
	case inconclusive:
		prefix = "**Inconclusive.**"
		result.Disposition = "INCONCLUSIVE"
		result.Event = "COMMENT"
	case actions:
		prefix = "**No blockers.**"
	}
	if boundary.Actor.ID == boundary.Pull.Author.ID {
		result.Event = "COMMENT"
	}
	var body bytes.Buffer
	body.WriteString(prefix)
	if strings.TrimSpace(report.Summary) != "" {
		body.WriteString("\n\n" + strings.TrimSpace(report.Summary))
	}
	for _, gap := range result.Gaps {
		body.WriteString("\n\n" + gap.Detail)
	}
	for _, existing := range referenced {
		body.WriteString("\n\nExisting finding: [continue the original conversation](" + existing.URL + ").")
	}
	result.Body = body.String()
	result.PayloadID = digest(result)
	return result, nil
}

func ValidatePrepared(result PreparedResult) error {
	if result.SchemaVersion != 1 && result.SchemaVersion != 2 {
		return errors.New("prepared review uses an unsupported schema")
	}
	current, err := prepareVersion(result.Boundary, result.Report, result.SchemaVersion)
	if err != nil {
		return err
	}
	if digest(current) != digest(result) {
		return errors.New("prepared review does not match its frozen source result and exact preview")
	}
	return nil
}

func OutputSchema() map[string]any {
	text := map[string]any{"type": "string"}
	object := func(properties map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	finding := object(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"blocker", "readiness_gap", "action", "question", "suggestion"}}, "path": text, "side": map[string]any{"type": "string", "enum": []string{"LEFT", "RIGHT"}}, "line": map[string]any{"type": "integer", "minimum": 1}, "body": text, "check": map[string]any{"type": "string", "description": "Empty for source findings. Only readiness_gap may identify an acquired mandatory check here; reviewd regenerates check disposition from fresh evidence."}}, []string{"kind", "path", "side", "line", "body", "check"})
	gap := object(map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"readiness", "context", "source"}}, "detail": text, "check": map[string]any{"type": "string", "description": "Empty for source/context/readiness gaps unrelated to CI. A mandatory-check observation must use kind readiness and the exact acquired check name; reviewd owns its final current classification."}}, []string{"kind", "detail", "check"})
	return object(map[string]any{"conclusion": map[string]any{"type": "string", "enum": []string{"APPROVE", "REQUEST_CHANGES", "INCONCLUSIVE"}}, "summary": map[string]any{"type": "string", "description": "Material source/context summary only. Do not narrate review activity or put check-status statements here; identify check observations separately so reviewd can refresh them without another source review."}, "findings": map[string]any{"type": "array", "items": finding}, "gaps": map[string]any{"type": "array", "items": gap}}, []string{"conclusion", "summary", "findings", "gaps"})
}
