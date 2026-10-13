package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitFixture(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("owned Git fixture: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

// Full object acquisition must preserve target-base versus merge-base, renamed,
// deleted, binary and symlink evidence, without checking out or executing code.
func TestSnapshotCompleteObjectsAndCleanup(t *testing.T) {
	root := t.TempDir()
	gitFixture(t, root, "init", "-q", "-b", "main", "--template=")
	for name, value := range map[string]string{"old.txt": "base\n", "deleted.txt": "remove\n", ".reviewd": "schema_version = 1\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitFixture(t, root, "add", "old.txt", "deleted.txt", ".reviewd")
	gitFixture(t, root, "commit", "-qm", "base")
	merge := gitFixture(t, root, "rev-parse", "HEAD")
	gitFixture(t, root, "checkout", "-qb", "feature")
	gitFixture(t, root, "mv", "old.txt", "renamed.txt")
	gitFixture(t, root, "rm", "deleted.txt")
	os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 2}, 0600)
	os.WriteFile(filepath.Join(root, ".reviewd"), []byte("schema_version = 1\n[actions]\npublish_review = \"auto\"\n"), 0600)
	if err := os.Symlink("/outside-private-file", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "add", "binary.bin", ".reviewd", "link")
	gitFixture(t, root, "commit", "-qm", "untrusted changes")
	head := gitFixture(t, root, "rev-parse", "HEAD")
	gitFixture(t, root, "update-ref", "refs/pull/17/head", head)
	gitFixture(t, root, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(root, "target-only.txt"), []byte("trusted target\n"), 0600)
	gitFixture(t, root, "add", "target-only.txt")
	gitFixture(t, root, "commit", "-qm", "target advanced")
	target := gitFixture(t, root, "rev-parse", "HEAD")
	owned := t.TempDir()
	snapshot, err := Acquire(context.Background(), Options{RemoteURL: "file://" + root, AllowLocalFixture: true, TempRoot: owned}, Boundary{TargetBaseSHA: target, HeadSHA: head, Number: 17, ChangedFiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if snapshot.MergeBaseSHA != merge || snapshot.TargetBaseSHA != target || snapshot.HeadSHA != head || len(snapshot.Changes) != 5 {
		t.Fatalf("source boundaries/completeness lost: %+v", snapshot)
	}
	if snapshot.Sources["base"]["old.txt"] != "base\n" || snapshot.Sources["head"]["renamed.txt"] != "base\n" || snapshot.Sources["target"]["target-only.txt"] != "trusted target\n" || strings.Contains(snapshot.Sources["target"][".reviewd"], "auto") {
		t.Error("source or trusted-target evidence aliased")
	}
	if snapshot.Sources["head"]["link"] != "/outside-private-file" {
		t.Error("symlink must be inspected as data without following its target")
	}
	if len(snapshot.Gaps) != 1 || snapshot.Gaps[0].Path != "binary.bin" {
		t.Fatalf("changed binary evidence limitation disappeared: %+v", snapshot.Gaps)
	}
	if !strings.Contains(snapshot.Diff, "deleted.txt") || !strings.Contains(snapshot.Diff, "rename from old.txt") {
		t.Error("complete raw comparison lost deletion/rename")
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(owned)
	if err != nil || len(entries) != 0 {
		t.Error("source snapshot not cleaned")
	}
	if gitFixture(t, root, "branch", "--show-current") != "main" || gitFixture(t, root, "rev-parse", "HEAD") != target {
		t.Error("ordinary checkout changed during acquisition")
	}
	for _, invalid := range []Boundary{{TargetBaseSHA: target, HeadSHA: merge, Number: 17, ChangedFiles: 5}, {TargetBaseSHA: target, HeadSHA: head, Number: 17, ChangedFiles: 4}} {
		if partial, err := Acquire(context.Background(), Options{RemoteURL: "file://" + root, AllowLocalFixture: true, TempRoot: owned}, invalid); err == nil {
			partial.Close()
			t.Error("mismatched remote boundary/count returned complete evidence")
		}
		entries, _ := os.ReadDir(owned)
		if len(entries) != 0 {
			t.Error("failed source acquisition leaked owned storage")
		}
	}
}

func TestSourceCredentialAndExecutionBoundary(t *testing.T) {
	for _, remote := range []string{"https://actor:secret@github.com/owner/repo.git", "ssh://git@github.com/owner/repo.git", "file:///outside", "https://github.com/owner/repo.git?token=secret"} {
		if snapshot, err := Acquire(context.Background(), Options{RemoteURL: remote, TempRoot: t.TempDir()}, Boundary{TargetBaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Number: 1}); err == nil {
			snapshot.Close()
			t.Error("remote acquisition accepted an expanded credential/execution boundary")
		}
	}
	env := GitEnvironment([]string{"PATH=/trusted/bin", "HOME=/operator", "GH_TOKEN=private", "GIT_SSH_COMMAND=execute-me", "HTTPS_PROXY=private", "GIT_CONFIG_COUNT=9", "LANG=C"}, "/owned", "Basic fixture")
	joined := strings.Join(env, "\n")
	for _, denied := range []string{"/operator", "GH_TOKEN", "GIT_SSH_COMMAND", "HTTPS_PROXY", "execute-me", "GIT_CONFIG_COUNT=9"} {
		if strings.Contains(joined, denied) {
			t.Error("source process inherited ambient authority")
		}
	}
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(joined, "GIT_CONFIG_NOSYSTEM=1") {
		t.Error("source process can discover ambient authentication/configuration")
	}
}
