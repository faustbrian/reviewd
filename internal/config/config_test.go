package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// QA-14/25: resolution is runtime behavior, not a defaults-registration test.
// The oracle is the declared precedence, not the resolver under test.
func TestConfigurationRejectsSkillTraversalAndCredentialShapedSelection(t *testing.T) {
	for _, assignment := range []string{"skill = '../outside'", "model = 'ghp_abcdefghijklmnopqrstuvwxyz1234567890'"} {
		if _, err := Parse([]byte("schema_version=1\n[reviewer]\n"+assignment+"\n"), "fixture"); err == nil {
			t.Fatalf("unsafe reviewer selection accepted: %s", assignment)
		}
	}
}

func TestRestoredSnapshotCannotChangeSchemaOrForgeRepositoryAuthority(t *testing.T) {
	snapshot, err := Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"schema", "repository authority"} {
		values := snapshot.Values()
		origins := snapshot.Provenance()
		if scenario == "schema" {
			values["schema_version"] = int64(2)
		} else {
			values["actions.publish_review"] = "auto"
			origins["actions.publish_review"] = Origin{Layer: "repository", Source: ".reviewd", Revision: strings.Repeat("a", 40)}
		}
		fingerprint, err := snapshotHash(snapshot.Repository, values)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(snapshotWire{1, snapshot.Repository, fingerprint, values, origins})
		if err != nil {
			t.Fatal(err)
		}
		var restored Snapshot
		if err := json.Unmarshal(data, &restored); err == nil {
			t.Errorf("restored snapshot accepted %s", scenario)
		}
		if len(restored.Values()) != 0 {
			t.Errorf("failed snapshot restore retained partial %s", scenario)
		}
	}
}

func TestFiveLayersResolveFieldsAndProvenance(t *testing.T) {
	global := []byte(`schema_version = 1
[github]
host = "github.com"
repositories = ["shipitfi/api", "shipitfi/bill"]
[reviewer]
reasoning_effort = "medium"
[notifications]
desktop = true
[projects."shipitfi/api".actions]
review_requested = "notify"
[projects."shipitfi/api".reviewer]
reasoning_effort = "high"
[projects."shipitfi/bill".actions]
review_requested = "auto"
publish_review = "auto"
`)
	document, err := Parse(global, "owned-config.toml")
	if err != nil {
		t.Fatal(err)
	}
	preferences := &RepositoryPreferences{BaseSHA: strings.Repeat("a", 40), Data: []byte(`schema_version = 1
[reviewer]
reasoning_effort = "low"
[review]
instructions = "Check monetary idempotency."
`)}
	resolved, err := Resolve(document, "Shipitfi/API", preferences, map[string]any{"reviewer.model": "gpt-6.1-sol", "notifications.desktop": false, "watch.debounce": "0s", "github.repositories": []string{"shipitfi/api"}})
	if err != nil {
		t.Fatal(err)
	}
	values := resolved.Values()
	expected := map[string]any{"reviewer.reasoning_effort": "high", "review.instructions": "Check monetary idempotency.", "actions.review_requested": "notify", "actions.publish_review": "ask", "notifications.desktop": false, "watch.debounce": "0s"}
	for key, want := range expected {
		if values[key] != want {
			t.Errorf("%s: got %v want %v", key, values[key], want)
		}
	}
	origins := resolved.Provenance()
	for key, want := range map[string]string{"reviewer.reasoning_effort": "project", "review.instructions": "repository", "notifications.desktop": "cli", "actions.publish_review": "built-in"} {
		if origins[key].Layer != want {
			t.Errorf("%s source: %+v want %s", key, origins[key], want)
		}
	}
	repositories, ok := values["github.repositories"].([]string)
	if !ok || len(repositories) != 1 || repositories[0] != "github.com/shipitfi/api" {
		t.Fatalf("list was not replaced/canonicalized: %#v", values["github.repositories"])
	}
	repositories[0] = "mutated"
	if resolved.Values()["github.repositories"].([]string)[0] == "mutated" {
		t.Fatal("returned configuration aliases frozen snapshot")
	}
	bill, err := Resolve(document, "shipitfi/bill", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bill.Values()["actions.review_requested"] != "auto" || bill.Values()["actions.publish_review"] != "auto" {
		t.Fatal("exact Bill policy did not resolve")
	}
}

// QA-15: invalid reload cannot install a partial policy or alter old snapshots.
func TestReloadRetainsValidPolicyAndFrozenRuns(t *testing.T) {
	manager, err := NewManager([]byte("schema_version = 1\n[actions]\nreview_requested = 'notify'\n"), "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Resolve("shipitfi/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reload([]byte("schema_version = 1\n[actions]\nreview_requested = 'auto'\npublish_review = 'invalid'\n")); err == nil {
		t.Fatal("invalid reload accepted")
	}
	second, err := manager.Resolve("shipitfi/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fingerprint != first.Fingerprint || second.String("actions.review_requested") != "notify" {
		t.Fatal("invalid reload replaced valid policy")
	}
	if err := manager.Reload([]byte("schema_version = 1\n[actions]\nreview_requested = 'auto'\n")); err != nil {
		t.Fatal(err)
	}
	latest, err := manager.Resolve("shipitfi/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if latest.String("actions.review_requested") != "auto" || first.String("actions.review_requested") != "notify" || latest.Fingerprint == first.Fingerprint {
		t.Fatal("reload changed an active snapshot or did not reach new jobs")
	}
}

// QA-13/X07: repository data must not become operator authority; malformed
// policy must return an error without a usable partial snapshot.
func TestInvalidInputCannotReturnPartialAutomation(t *testing.T) {
	document, err := Parse([]byte("schema_version = 1\n"), "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(document, "shipitfi/api", &RepositoryPreferences{BaseSHA: strings.Repeat("b", 40), Data: []byte("schema_version = 1\n[actions]\npublish_review = 'auto'\n")}, nil)
	if err == nil {
		t.Fatal("repository granted publication authority")
	}
	bad, err := Resolve(document, "shipitfi/api", nil, map[string]any{"actions.publish_review": "invalid"})
	if err == nil {
		t.Fatal("invalid publication action accepted")
	}
	if len(bad.Values()) != 0 {
		t.Fatal("invalid override exposed usable partial configuration")
	}
}

func TestSnapshotRoundTripRetainsFrozenConfig(t *testing.T) {
	document, err := Parse([]byte("schema_version = 1\n[actions]\nreview_requested = 'notify'\n"), "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	original, err := Resolve(document, "shipitfi/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.String("actions.review_requested") != "notify" || restored.Fingerprint != original.Fingerprint || !reflect.DeepEqual(restored.Provenance(), original.Provenance()) {
		t.Fatal("persisted snapshot lost frozen settings or provenance")
	}
}

func TestPresentEmptyRepositoryPreferencesAreInvalid(t *testing.T) {
	_, err := Resolve(nil, "shipitfi/api", &RepositoryPreferences{BaseSHA: strings.Repeat("a", 40), Data: []byte{}}, nil)
	if err == nil {
		t.Fatal("present empty .reviewd silently ignored instead of reporting invalid schema")
	}
}

func TestInvalidConfigurationGivesFieldErrorsWithoutValues(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"host", "schema_version = 1\n[github]\nhost = 'https://github.com'\n", "github.host"},
		{"duration", "schema_version = 1\n[watch]\npoll_interval = 'not-time'\n", "watch.poll_interval"},
		{"project unknown", "schema_version = 1\n[projects.\"shipitfi/api\".notifications]\ndeskotp = true\n", "notifications.deskotp"},
		{"empty unknown table", "schema_version = 1\n[credentials]\n", "credentials"},
		{"false type", "schema_version = 1\n[notifications]\ndesktop = 'false'\n", "notifications.desktop"},
		{"invalid zero", "schema_version = 1\n[limits]\nmax_concurrent_reviews = 0\n", "limits.max_concurrent_reviews"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, err := Parse([]byte(item.body), "config.toml")
			if err == nil || !strings.Contains(err.Error(), item.field) {
				t.Fatalf("expected corrective field %s, got %v", item.field, err)
			}
		})
	}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		canary := prefix + strings.Repeat("a", 36)
		for _, body := range []string{"schema_version = 1\n[credentials]\ntoken = '" + canary + "'\n", "schema_version = 1\n'" + canary + "' = true\n", "schema_version = 1\n[reviewer]\nmodel = '" + canary + "\n"} {
			_, err := Parse([]byte(body), "config.toml")
			if err == nil {
				t.Fatal("invalid secret fixture accepted")
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatal("configuration diagnostic exposed a credential-shaped value")
			}
		}
	}
}

func TestGlobalDefaultsDoNotSelectAnArbitraryProject(t *testing.T) {
	document, err := Parse([]byte("schema_version = 1\n[projects.\"reviewd/validation\".actions]\npublish_review = 'auto'\n"), "config.toml")
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := ResolveDefaults(document, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Repository != "" || defaults.String("actions.publish_review") != "ask" {
		t.Fatal("profile diagnostics inherited an arbitrary project's automation")
	}
}
