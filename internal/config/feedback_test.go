package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFeedbackPolicySeparatesAuthorAuthorityAndAgentSelection(t *testing.T) {
	doc, err := Parse([]byte(`schema_version=1
[reviewer]
model="review-model"
reasoning_effort="low"
[feedback]
authors=["@me", "OTHER"]
action="auto"
model="fix-model"
reasoning_effort="high"
[projects."owner/notify".feedback]
authors=["special"]
action="notify"
model="notify-model"
`), "operator")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := Resolve(doc, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.FeedbackAuthorAllowed(43, "renamed", 43) || !policy.FeedbackAuthorAllowed(8, "other", 43) || policy.FeedbackAuthorAllowed(9, "someone", 43) {
		t.Fatal("feedback author matching lost stable self identity or exact logins")
	}
	if policy.FeedbackAction("review_submitted") != "auto" || policy.String("feedback.model") != "fix-model" || policy.String("reviewer.model") != "review-model" || !policy.ReviewAuthorExcluded(43, "renamed", 43) {
		t.Fatal("feedback settings changed reviewer behavior")
	}
	local, err := Resolve(doc, "owner/notify", nil, nil)
	if err != nil || local.FeedbackAuthorAllowed(43, "renamed", 43) || !local.FeedbackAuthorAllowed(9, "SPECIAL", 43) || local.FeedbackAction("review_submitted") != "notify" || local.String("feedback.model") != "notify-model" {
		t.Fatalf("project policy leaked or concatenated authors: %v", err)
	}
	for _, table := range []string{`[feedback]
action="auto"`, `[feedback]
authors=["@me"]`, `[feedback]
reasoning_effort="high"`} {
		if _, err := Resolve(doc, "owner/repo", &RepositoryPreferences{BaseSHA: strings.Repeat("a", 40), Data: []byte("schema_version=1\n" + table)}, nil); err == nil {
			t.Fatal("repository preferences acquired feedback authority")
		}
	}
}

func TestFeedbackDefaultAndLegacyActionsNeverImplicitlyGrantFixes(t *testing.T) {
	for _, action := range []string{"ignore", "notify", "ask", "auto"} {
		doc, err := Parse([]byte("schema_version=1\n[actions]\nreview_submitted='"+action+"'\n"), "operator")
		if err != nil {
			t.Fatal(err)
		}
		policy, err := Resolve(doc, "owner/repo", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := action
		if action == "auto" {
			want = "ask"
		}
		if policy.FeedbackAction("review_submitted") != want || policy.String("feedback.model") != "gpt-6.1-sol" || policy.String("feedback.reasoning_effort") != "high" || policy.String("feedback.skill") != "handle-pr-feedback" {
			t.Fatal("legacy action granted fixing or defaults drifted")
		}
	}
	for _, invalid := range []string{"authors=['@other']", "authors=['x','X']", "action='yes'", "model='github_pat_secret'", "skill='../outside'", "reasoning_effort='magic'"} {
		if _, err := Parse([]byte("schema_version=1\n[feedback]\n"+invalid+"\n"), "operator"); err == nil {
			t.Fatalf("invalid feedback selection accepted: %s", invalid)
		}
	}
}

func TestFeedbackSnapshotUpgradePreservesAllHistoricalPayloadIdentities(t *testing.T) {
	policy, _ := Resolve(nil, "owner/repo", nil, nil)
	for _, old := range []bool{false, true} {
		values, origins := policy.Values(), policy.Provenance()
		for key := range values {
			if strings.HasPrefix(key, "feedback.") || key == "limits.feedback_timeout" {
				delete(values, key)
				delete(origins, key)
			}
		}
		if old {
			delete(values, "reviewer.exclude_authors")
			delete(origins, "reviewer.exclude_authors")
		}
		hash, _ := snapshotHash(policy.Repository, values)
		before, _ := json.Marshal(snapshotWire{1, policy.Repository, hash, values, origins})
		var restored Snapshot
		if err := json.Unmarshal(before, &restored); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(restored)
		if string(before) != string(after) {
			t.Fatal("adding feedback changed a historical prepared payload")
		}
		if restored.FeedbackAction("review_submitted") == "auto" {
			t.Fatal("historical snapshot acquired fixing authority")
		}
	}
}

func TestFeedbackAutomaticAuthorityCannotComeFromCLIProvenance(t *testing.T) {
	for _, overrides := range []map[string]any{{"feedback.action": "auto"}, {"feedback.authors": []string{"someone"}}} {
		if _, err := Resolve(nil, "owner/repo", nil, overrides); err == nil {
			t.Fatal("CLI override broadened automatic feedback authority")
		}
	}
	doc, err := Parse([]byte("schema_version=1\n[feedback]\naction='auto'\n[projects.\"owner/repo\".feedback]\naction='ask'\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	global, _ := Resolve(doc, "owner/other", nil, nil)
	exact, _ := Resolve(doc, "owner/repo", nil, map[string]any{"feedback.model": "custom-model", "feedback.reasoning_effort": "low"})
	if global.FeedbackAction("review_submitted") != "auto" || exact.FeedbackAction("review_submitted") != "ask" || exact.String("feedback.model") != "custom-model" {
		t.Fatal("restricting authority removed operator policy or single-run agent selection")
	}
}
