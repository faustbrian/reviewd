package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// QA-26: credentials and writable connectors are excluded by construction,
// not by the generated review prompt.
func TestWorkerEnvironmentExcludesPublisherCapabilities(t *testing.T) {
	env := WorkerEnvironment([]string{"PATH=/bin", "HOME=/operator", "GH_TOKEN=publisher-canary", "GITHUB_TOKEN=publisher-canary", "SSH_AUTH_SOCK=/agent", "CODEX_HOME=/unsafe", "OPENAI_API_KEY=provider-canary"}, "/isolated/control")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "publisher-canary") || strings.Contains(joined, "SSH_AUTH_SOCK") || strings.Contains(joined, "/unsafe") {
		t.Fatalf("publisher access inherited: %s", joined)
	}
	if !strings.Contains(joined, "CODEX_HOME=/isolated/control") || !strings.Contains(joined, "OPENAI_API_KEY=provider-canary") {
		t.Fatalf("provider auth or isolated control lost: %s", joined)
	}
	args := strings.Join(WorkerArgs(), " ")
	for _, flag := range []string{"features.shell_tool=false", "features.unified_exec=false", "features.apps=false", "features.remote_plugin=false", "web_search=\"disabled\""} {
		if !strings.Contains(args, flag) {
			t.Fatalf("worker mutation surface remains enabled: %s", flag)
		}
	}
}

func TestIsolatedRuntimeCopiesOnlyProviderAuthAndCleansUp(t *testing.T) {
	home := t.TempDir()
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"provider-canary"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp_servers.github]\ncommand='write-capable'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	isolation, err := NewIsolation(parent, home)
	if err != nil {
		t.Fatal(err)
	}
	root := isolation.Root
	if _, err := os.Stat(filepath.Join(isolation.Control, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("unrestricted global config copied")
	}
	if data, err := os.ReadFile(filepath.Join(isolation.Control, "auth.json")); err != nil || !strings.Contains(string(data), "provider-canary") {
		t.Fatalf("provider authentication absent: %v", err)
	}
	if err := isolation.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("runtime retained after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal("user auth was removed")
	}
}

// QA-27: authentication/setup failure must also remove only task-owned state.
func TestIsolationFailureCleansUpWithoutTouchingDonor(t *testing.T) {
	parent, donor := t.TempDir(), t.TempDir()
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := NewIsolation(parent, donor); err == nil {
		t.Fatal("missing authentication became success")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed isolation leaked resources: %v %v", entries, err)
	}
	if _, err := os.Stat(donor); err != nil {
		t.Fatal("donor directory was removed")
	}
}
