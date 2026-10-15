package feedback

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// QA05/QA09: exercise acquisition -> private full Git repository -> actual
// credential-free native checkout/edit -> proven owner cleanup, not a mock
// command result. This fixture does not invoke a model or publish anything.
func TestCodexCapabilityFeedbackWorkspace(t *testing.T) {
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for native feedback checkout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Setenv("OPENAI_API_KEY", "owned-provider-canary")
	t.Setenv("GH_TOKEN", "owned-github-canary")
	root := t.TempDir()
	original := filepath.Join(root, "ordinary-checkout")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = original
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + original, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned repository fixture: %v %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q", "-b", "main", "--template=")
	if err := os.WriteFile(filepath.Join(original, "source.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "fixture base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-qb", "feature/existing-pr")
	binary := []byte{0, 255, 13, 10, 128, 0}
	for name, data := range map[string][]byte{"binary.bin": binary, "executable": []byte("#!/bin/sh\nexit 99\n")} {
		if err := os.WriteFile(filepath.Join(original, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../user-secret", filepath.Join(original, "outside-link")); err != nil {
		t.Fatal(err)
	}
	git("add", "binary.bin", "executable", "outside-link")
	git("commit", "-qm", "fixture head")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/19/head", head)
	if err := os.WriteFile(filepath.Join(original, "dirty-sentinel"), []byte("user-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "user-secret")
	if err := os.WriteFile(secret, []byte("private-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := store.Open(ctx, filepath.Join(root, "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The one owner gates source, command host and workspace cleanup together.
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	ownedCtx := owner.Context(ctx)
	snapshot, err := source.Acquire(ownedCtx, source.Options{RemoteURL: "file://" + original, AllowLocalFixture: true}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, Number: 19, ChangedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := PrepareWorkspace(ownedCtx, owner, snapshot, "feature/existing-pr", codex.RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if data, err := os.ReadFile(filepath.Join(workspace.Directory, "binary.bin")); err != nil || !bytes.Equal(data, binary) {
		t.Fatal("native checkout lost binary source", err)
	}
	if info, err := os.Stat(filepath.Join(workspace.Directory, "executable")); err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("native checkout lost executable mode", err)
	}
	if target, err := os.Readlink(filepath.Join(workspace.Directory, "outside-link")); err != nil || target != "../user-secret" {
		t.Fatal("native checkout changed symlink data", err)
	}
	result, err := workspace.Commands.Run(ownedCtx, []string{"/bin/sh", "-c", `if cat "$1" >/dev/null 2>&1 || test -n "$OPENAI_API_KEY$GH_TOKEN$SSH_AUTH_SOCK$CODEX_HOME"; then echo LEAK; exit 9; fi; printf 'after\n' > source.txt; "$2" add -- source.txt; "$2" diff --cached -- source.txt`, "owned-probe", secret, workspace.Git}, 20*time.Second)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "+after") || strings.Contains(result.Stdout, "LEAK") {
		t.Fatalf("native edit/index or credential boundary: %+v %v", result, err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("proven owner did not remove private workspace", err)
	}
	if data, err := os.ReadFile(filepath.Join(original, "source.txt")); err != nil || string(data) != "before\n" {
		t.Fatal("ordinary source changed")
	}
	if data, err := os.ReadFile(filepath.Join(original, "dirty-sentinel")); err != nil || string(data) != "user-owned" {
		t.Fatal("ordinary dirty storage changed")
	}
	if git("branch", "--show-current") != "feature/existing-pr" || git("rev-parse", "HEAD") != head {
		t.Fatal("ordinary checkout branch/head changed")
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "private-canary" {
		t.Fatal("outside user storage changed")
	}
}
