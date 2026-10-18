package config

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestBackendSelectionResolvesBothLanesAndNativeDefaults(t *testing.T) {
	doc, err := Parse([]byte(`schema_version=1
[reviewer]
executable="my-codex"
[feedback]
backend="claude"
[projects."acme/api".reviewer]
backend="claude"
executable="my-claude"
model="sonnet"
reasoning_effort="high"
[projects."acme/api".feedback]
backend="codex"
`), "operator")
	if err != nil {
		t.Fatal(err)
	}
	base, err := Resolve(doc, "acme/other", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"reviewer.backend": "codex", "reviewer.executable": "my-codex", "reviewer.model": "gpt-6.1-sol", "reviewer.reasoning_effort": "high", "feedback.backend": "claude", "feedback.executable": "claude", "feedback.model": "", "feedback.reasoning_effort": ""} {
		if got := base.String(key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	project, err := Resolve(doc, "acme/api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"reviewer.backend": "claude", "reviewer.executable": "my-claude", "reviewer.model": "sonnet", "feedback.backend": "codex", "feedback.executable": "codex", "feedback.model": "gpt-6.1-sol"} {
		if got := project.String(key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	data, _ := json.Marshal(project)
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Fingerprint != project.Fingerprint {
		t.Fatal("backend snapshot identity changed")
	}
}

func TestBackendChoicesRejectIncompatibleExplicitModelsAndRepositoryAuthority(t *testing.T) {
	for _, settings := range []string{`backend="other"`, "backend=\"claude\"\nmodel=\"gpt-6.1-sol\"", "backend=\"claude\"\nreasoning_effort=\"ultra\""} {
		if _, err := Parse([]byte("schema_version=1\n[reviewer]\n"+settings), "operator"); err == nil {
			t.Errorf("accepted incompatible choice %s", settings)
		}
	}
	for _, settings := range []string{"backend=\"claude\"", "executable=\"claude\""} {
		if _, err := Resolve(nil, "acme/api", &RepositoryPreferences{BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Data: []byte("schema_version=1\n[reviewer]\n" + settings)}, nil); err == nil {
			t.Error("repository acquired backend/executable authority")
		}
	}
}

func TestClaudeNamespacedSkillAndHistoricalPublicV1(t *testing.T) {
	if _, err := Parse([]byte("schema_version=1\n[reviewer]\nbackend='claude'\nskill='team:review'\n"), "operator"); err != nil {
		t.Fatalf("native plugin skill cannot be selected: %v", err)
	}
	// Fixed pre-backend public-v1 shape; fingerprint is an independent golden,
	// not computed by the production snapshotHash helper in this test.
	before, err := os.ReadFile("testdata/schema1-native-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(before, after) || restored.String("reviewer.executable") != "codex" || restored.String("reviewer.backend") != "" {
		t.Fatal("historical public-v1 identity was rewritten")
	}
}
