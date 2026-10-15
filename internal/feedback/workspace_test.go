package feedback

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "darwin" {
		if os.Getenv("REVIEWD_CONTAINER_CHECKOUT_NATIVE") != "1" {
			t.Skip("explicit owned-container checkout acceptance is not enabled")
		}
	} else if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for native Linux feedback checkout")
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
	if err := os.WriteFile(filepath.Join(original, "source.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(original, "unchanged.txt"), []byte("unchanged\n"), 0600)
	git("add", "source.txt", "unchanged.txt")
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
	os.Mkdir(filepath.Join(original, "nested"), 0700)
	os.WriteFile(filepath.Join(original, "nested", "日本語 space.txt"), []byte("complete nested source\n"), 0600)
	os.WriteFile(filepath.Join(original, "source.txt"), []byte("before\n"), 0600)
	git("add", "source.txt", "binary.bin", "executable", "outside-link", "nested/日本語 space.txt")
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
	profileParent, err := os.MkdirTemp(os.Getenv("REVIEWD_CONTAINER_FIXTURE_PARENT"), "native-checkout-")
	if err != nil {
		t.Fatal(err)
	}
	var durable *store.Store
	var owner *store.PrivateOwner
	var workspace *Workspace
	defer func() {
		var cleanup error
		if workspace != nil {
			cleanup = errors.Join(cleanup, workspace.Close())
		}
		if owner != nil {
			cleanup = errors.Join(cleanup, owner.Close())
		}
		if durable != nil {
			cleanup = errors.Join(cleanup, durable.Close())
		}
		if cleanup != nil {
			t.Errorf("unproven owned checkout retained at %s: %v", profileParent, cleanup)
			return
		}
		if err := os.RemoveAll(profileParent); err != nil {
			t.Error(err)
		}
	}()
	durable, err = store.Open(ctx, filepath.Join(profileParent, "profile"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err = durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ownedCtx := owner.Context(ctx)
	snapshot, err := source.Acquire(ownedCtx, source.Options{RemoteURL: "file://" + original, AllowLocalFixture: true}, source.Boundary{TargetBaseSHA: base, HeadSHA: head, Number: 19, ChangedFiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	options := WorkspaceOptions{Runtime: codex.RuntimeOptions{}}
	if runtime.GOOS == "darwin" {
		options.ContainerEndpoint = os.Getenv("REVIEWD_CONTAINER_ENDPOINT")
		options.ContainerImage = os.Getenv("REVIEWD_CONTAINER_IMAGE")
		options.nativeExecutable = os.Getenv("REVIEWD_ACCEPTANCE_NATIVE")
		if options.ContainerEndpoint == "" || options.ContainerImage == "" || options.nativeExecutable == "" {
			t.Fatal("owned Mac checkout requires explicit local endpoint, cached immutable Git/toolchain image and real paired native package")
		}
	}
	workspace, err = PrepareWorkspace(ownedCtx, owner, snapshot, "feature/existing-pr", options)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace.Directory, "binary.bin")); err != nil || !bytes.Equal(data, binary) {
		t.Fatal("native checkout lost binary source", err)
	}
	if info, err := os.Stat(filepath.Join(workspace.Directory, "executable")); err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("native checkout lost executable mode", err)
	}
	if target, err := os.Readlink(filepath.Join(workspace.Directory, "outside-link")); err != nil || target != "../user-secret" {
		t.Fatal("native checkout changed symlink data", err)
	}
	if runtime.GOOS == "darwin" && (workspace.Environment.Backend != "container" || workspace.Environment.OS != "linux" || workspace.Environment.Workspace != "/workspace" || workspace.Git != "git") {
		t.Fatal("macOS checkout silently changed backend or guest paths")
	}
	for name, want := range map[string]string{"unchanged.txt": "unchanged\n", "nested/日本語 space.txt": "complete nested source\n"} {
		if data, err := os.ReadFile(filepath.Join(workspace.Directory, filepath.FromSlash(name))); err != nil || string(data) != want {
			t.Fatal("complete checkout source missing", name, err)
		}
	}
	read, err := workspace.StartCommands(ownedCtx, false)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := read.Run(ownedCtx, []string{"/bin/sh", "-c", `test "$(cat source.txt)" = before || exit 2; if (printf changed >source.txt) 2>/dev/null; then exit 3; fi; if (printf changed >.git/index) 2>/dev/null; then exit 4; fi; "$1" merge-base "$2" "$3"`, "owned-read-fixture", workspace.Git, head, base}, 20*time.Second)
	if err != nil || protected.ExitCode != 0 || strings.TrimSpace(protected.Stdout) != base {
		t.Fatalf("read-only complete Git/history grant: %+v %v", protected, err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ValidateFeedbackWorkspace(ownedCtx, workspace.Directory); err != nil {
		t.Fatal("read-only command changed immutable source", err)
	}
	commands, err := workspace.StartCommands(ownedCtx, true)
	if err != nil {
		t.Fatal(err)
	}
	result, err := commands.Run(ownedCtx, []string{"/bin/sh", "-c", `if cat "$1" >/dev/null 2>&1 || test -n "$OPENAI_API_KEY$GH_TOKEN$SSH_AUTH_SOCK$CODEX_HOME"; then echo LEAK; exit 9; fi; printf 'after\n' > source.txt; "$2" add -- source.txt; "$2" diff --cached -- source.txt`, "owned-probe", secret, workspace.Git}, 20*time.Second)
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
