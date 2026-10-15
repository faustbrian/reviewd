package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestReviewAuthorExclusionsResolveIdentityAndReplacePerProject(t *testing.T) {
	doc, err := Parse([]byte("schema_version=1\n[reviewer]\nexclude_authors=['@me','Dependabot[bot]']\n[projects.\"owner/repo\".reviewer]\nexclude_authors=['Other']\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		repo, login   string
		author, actor int64
		excluded      bool
	}{
		{"owner/sibling", "renamed", 43, 43, true},
		{"owner/sibling", "DEPENDABOT[bot]", 8, 43, true},
		{"owner/sibling", "reviewer", 8, 43, false},
		{"owner/repo", "renamed", 43, 43, false},
		{"owner/repo", "OTHER", 9, 43, true},
	} {
		policy, err := Resolve(doc, test.repo, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := policy.ReviewAuthorExcluded(test.author, test.login, test.actor); got != test.excluded {
			t.Errorf("%+v: excluded=%v", test, got)
		}
	}
	policy, err := Resolve(doc, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.Strings("reviewer.exclude_authors"), []string{"other"}) || policy.Provenance()["reviewer.exclude_authors"].Layer != "project" {
		t.Fatal("project replacement or provenance lost")
	}
	if _, err := Resolve(doc, "owner/repo", &RepositoryPreferences{BaseSHA: strings.Repeat("a", 40), Data: []byte("schema_version=1\n[reviewer]\nexclude_authors=[]\n")}, nil); err == nil {
		t.Fatal("untrusted repository changed author authority")
	}
}

func TestReviewAuthorExclusionsDefaultOptOutAndRejectAmbiguousNames(t *testing.T) {
	policy, err := Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.ReviewAuthorExcluded(43, "renamed", 43) || policy.ReviewAuthorExcluded(8, "renamed", 43) {
		t.Fatal("@me must use stable authenticated identity")
	}
	doc, err := Parse([]byte("schema_version=1\n[reviewer]\nexclude_authors=[]\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	policy, err = Resolve(doc, "owner/repo", nil, nil)
	if err != nil || policy.ReviewAuthorExcluded(43, "reviewer", 43) {
		t.Fatal("explicit empty replacement must opt out", err)
	}
	for _, author := range []string{"@someone", "owner/*", "", " alice ", "alice/other", "alice','ALICE"} {
		if _, err := Parse([]byte("schema_version=1\n[reviewer]\nexclude_authors=['"+author+"']\n"), "operator"); err == nil {
			t.Errorf("ambiguous filter accepted: %q", author)
		}
	}
}

func TestReviewAuthorExclusionsNormalizeBotSuffixBeforeValidation(t *testing.T) {
	var fingerprints []string
	for _, login := range []string{"dependabot[bot]", "DEPENDABOT[BOT]"} {
		doc, err := Parse([]byte("schema_version=1\n[reviewer]\nexclude_authors=['"+login+"']\n"), "operator")
		if err != nil {
			t.Fatal(err)
		}
		policy, err := Resolve(doc, "owner/repo", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !policy.ReviewAuthorExcluded(8, "Dependabot[bot]", 43) {
			t.Fatal("bot exclusion lost after normalization")
		}
		fingerprints = append(fingerprints, policy.Fingerprint)
	}
	if fingerprints[0] != fingerprints[1] {
		t.Fatal("equivalent author filters changed effective policy identity")
	}
}

func TestLegacySnapshotRetainsBytesAndSafeAuthorDefault(t *testing.T) {
	policy, err := Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, origins := policy.Values(), policy.Provenance()
	for key := range values {
		if feedbackSetting(key) {
			delete(values, key)
			delete(origins, key)
		}
	}
	delete(values, "reviewer.exclude_authors")
	delete(origins, "reviewer.exclude_authors")
	fingerprint, err := snapshotHash(policy.Repository, values)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(snapshotWire{1, policy.Repository, fingerprint, values, origins})
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(legacy, &restored); err != nil {
		t.Fatal(err)
	}
	roundtrip, err := json.Marshal(restored)
	if err != nil || string(legacy) != string(roundtrip) {
		t.Fatal("historical payload identity changed", err)
	}
	if !restored.ReviewAuthorExcluded(43, "reviewer", 43) {
		t.Fatal("historical snapshot bypassed safe default")
	}
	delete(values, "reviewer.model")
	delete(origins, "reviewer.model")
	malformed, _ := json.Marshal(snapshotWire{1, policy.Repository, fingerprint, values, origins})
	if json.Unmarshal(malformed, &restored) == nil {
		t.Fatal("incomplete historical snapshot accepted")
	}
}
