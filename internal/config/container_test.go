package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFeedbackContainerSelectionIsOperatorOnlyAndProjectScoped(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	doc, err := Parse([]byte("schema_version=1\n[feedback]\ncontainer_endpoint='unix:///tmp/global.sock'\ncontainer_image='"+image+"'\n[projects.\"owner/repo\".feedback]\ncontainer_endpoint='unix:///tmp/project.sock'\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	exact, err := Resolve(doc, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Resolve(doc, "owner/other", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exact.String("feedback.container_endpoint") != "unix:///tmp/project.sock" || other.String("feedback.container_endpoint") != "unix:///tmp/global.sock" || exact.String("feedback.container_image") != image {
		t.Fatal("container authority leaked between projects")
	}
	for _, key := range []string{"container_endpoint", "container_image"} {
		value := "unix:///tmp/expanded.sock"
		if key == "container_image" {
			value = image
		}
		if _, err := Resolve(doc, "owner/repo", &RepositoryPreferences{BaseSHA: strings.Repeat("a", 40), Data: []byte("schema_version=1\n[feedback]\n" + key + "='" + value + "'\n")}, nil); err == nil {
			t.Fatal("repository acquired container capability")
		}
		if _, err := Resolve(doc, "owner/repo", nil, map[string]any{"feedback." + key: value}); err == nil {
			t.Fatal("CLI broadened container authority")
		}
	}
}

func TestHistoricalFeedbackSnapshotDoesNotAcquireContainerAuthority(t *testing.T) {
	current, _ := Resolve(nil, "owner/repo", nil, nil)
	values, origins := current.Values(), current.Provenance()
	for _, key := range []string{"feedback.container_endpoint", "feedback.container_image"} {
		delete(values, key)
		delete(origins, key)
	}
	hash, _ := snapshotHash(current.Repository, values)
	before, _ := json.Marshal(snapshotWire{1, current.Repository, hash, values, origins})
	var restored Snapshot
	if err := json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if string(before) != string(after) || restored.String("feedback.container_endpoint") != "" || restored.String("feedback.container_image") != "" {
		t.Fatal("historical prepared payload gained authority or changed identity")
	}
}
