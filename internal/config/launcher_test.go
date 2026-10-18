package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLauncherConfigurationSeparatesProjectRoutingAndPrompts(t *testing.T) {
	document, err := Parse([]byte(`schema_version=1
[agent]
working_directory="/operator/projects"
[feedback]
authors=["@me"]
action="ask"
instructions="Use my feedback instructions."
[projects."acme/api".feedback]
action="notify"
[projects."acme/bill".feedback]
action="auto"
instructions="Use the Bill project's installed skills."
[projects."acme/bill".agent]
working_directory="/operator/projects/bill"
`), "operator")
	if err != nil {
		t.Fatal(err)
	}
	api, err := Resolve(document, "acme/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bill, err := Resolve(document, "acme/bill", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if api.FeedbackAction("review_submitted") != "notify" || bill.FeedbackAction("review_submitted") != "auto" || api.String("feedback.instructions") != "Use my feedback instructions." || bill.String("feedback.instructions") != "Use the Bill project's installed skills." || api.String("agent.working_directory") != "/operator/projects" || bill.String("agent.working_directory") != "/operator/projects/bill" {
		t.Fatal("exact-project routing or launch inputs leaked into another project")
	}
	for key := range bill.Values() {
		if obsoleteSetting(key) {
			t.Fatalf("obsolete workflow setting still exposed by runtime configuration: %s", key)
		}
	}
}

func TestPublicV1ConfigurationAndNativeSnapshot(t *testing.T) {
	document, err := Parse([]byte("schema_version=1\n[actions]\nreview_requested='auto'\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Resolve(document, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Int("schema_version") != 1 || defaults.Int("schema_version") != 1 || snapshot.String("actions.review_requested") != "auto" {
		t.Fatal("public v1 policy was not retained")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(restored)
	if err != nil || !bytes.Equal(data, encoded) {
		t.Fatal("current v1 snapshot changed during roundtrip")
	}
}

func TestPublicV1RejectsUnsupportedVersionsAndRemovedSettings(t *testing.T) {
	for _, version := range []string{"", "schema_version='1'\n", "schema_version=0\n", "schema_version=2\n", "schema_version=3\n"} {
		if document, err := Parse([]byte(version), "operator"); err == nil || document != nil || !strings.Contains(err.Error(), "schema_version=1") {
			t.Fatalf("unsupported live version accepted or wrong diagnostic: %q / %v", version, err)
		}
	}
	for _, setting := range []string{"[actions]\npublish_review='ask'", "[feedback]\ncontainer_endpoint='unix:///old/runtime.sock'", "[feedback]\ncontainer_image='old/image'", "[feedback]\nmetadata_validators='/old/validators'", "[feedback]\nmetadata_profile='/old/profile'", "[feedback]\nlabel_catalog='/old/catalog'", "[feedback]\nskill_path='/old/skill'", "[reviewer]\nauth_home='/old/copied-auth'", "[reviewer]\nskill_path='/old/skill'"} {
		if document, err := Parse([]byte("schema_version=1\n"+setting+"\n"), "operator"); err == nil || document != nil || !strings.Contains(err.Error(), "removed") {
			t.Fatalf("obsolete setting silently accepted: %s / %v", setting, err)
		}
	}
}

func TestLauncherReadsPreviousNativeConfigurationWithoutRewritingIt(t *testing.T) {
	data, err := os.ReadFile("testdata/schema2-launcher-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := json.Marshal(snapshot)
	if err != nil || !bytes.Equal(data, restored) || snapshot.Int("schema_version") != 2 {
		t.Fatal("previous native configuration identity or bytes changed")
	}
}

func TestLauncherReadsHistoricalConfigurationWithoutRewritingIt(t *testing.T) {
	data, err := os.ReadFile("testdata/schema1-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	var historical Snapshot
	if err = json.Unmarshal(data, &historical); err != nil {
		t.Fatal(err)
	}
	restored, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compact.Bytes(), restored) {
		t.Fatal("historical configuration identity or bytes changed")
	}
	if historical.Int("schema_version") != 1 {
		t.Fatal("legacy configuration was upgraded into launch authority")
	}
}

// Historical source is a recorded schema-1 snapshot, not today's defaults.
func historicalSnapshot(t *testing.T) Snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/schema1-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var result Snapshot
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNativeSnapshotRejectsIncompleteOrMixedGenerations(t *testing.T) {
	snapshot, err := Resolve(nil, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing working directory", "missing instructions", "missing both", "mixed workflow"} {
		values, origins := snapshot.Values(), snapshot.Provenance()
		if scenario == "missing working directory" || scenario == "missing both" {
			delete(values, "agent.working_directory")
			delete(origins, "agent.working_directory")
		}
		if scenario == "missing instructions" || scenario == "missing both" {
			delete(values, "feedback.instructions")
			delete(origins, "feedback.instructions")
		}
		if scenario == "mixed workflow" {
			values["actions.publish_review"] = "ask"
			origins["actions.publish_review"] = Origin{Layer: "built-in"}
		}
		hash, err := snapshotHash(snapshot.Repository, values)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(snapshotWire{1, snapshot.Repository, hash, values, origins})
		if err != nil {
			t.Fatal(err)
		}
		restored := snapshot
		if err = json.Unmarshal(data, &restored); err == nil || len(restored.Values()) != 0 {
			t.Fatalf("invalid %s snapshot retained usable policy: %v", scenario, err)
		}
	}
}
