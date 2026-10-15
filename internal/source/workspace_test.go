package source

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackWorkspaceBranchBoundaryMatchesGit(t *testing.T) {
	for _, branch := range []string{"feature/existing-pr", "foo./bar", "@", "feature/@", "feature/日本語", "HEAD", "", "feature/../../outside", "feature//nested", "feature/foo.lock/bar", "feature/.hidden", "-option", "feature/trailing.", "feature/@{previous}", "feature/with space", "feature/with\nline", "feature/with\\separator", "feature/[pattern"} {
		command := exec.Command("git", "check-ref-format", "--branch", branch)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
		valid := command.Run() == nil
		if feedbackBranch(branch) != valid {
			t.Fatalf("branch %q differs from native Git reference validation", branch)
		}
	}
}

// QA09/QA05: acquisition remains sealed/bare. The separately requested export
// must carry real Git objects, not the lossy UTF-8 evidence representation.
func TestFeedbackWorkspacePreservesObjectsWithoutCheckoutOrSharedStorage(t *testing.T) {
	ctx := context.Background()
	original := t.TempDir()
	gitFixture(t, original, "init", "-q", "-b", "main", "--template=")
	if err := os.WriteFile(filepath.Join(original, "base.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, original, "add", "base.txt")
	gitFixture(t, original, "commit", "-qm", "base")
	base := gitFixture(t, original, "rev-parse", "HEAD")
	gitFixture(t, original, "checkout", "-qb", "feature/existing-pr")
	binary := []byte{0, 255, 13, 10, 128, 0}
	if err := os.WriteFile(filepath.Join(original, "binary.bin"), binary, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "executable"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside-user-data", filepath.Join(original, "link")); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, original, "add", "binary.bin", "executable", "link")
	gitFixture(t, original, "commit", "-qm", "existing head")
	head := gitFixture(t, original, "rev-parse", "HEAD")
	gitFixture(t, original, "update-ref", "refs/pull/19/head", head)
	if err := os.WriteFile(filepath.Join(original, "dirty-sentinel"), []byte("user-owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owned := t.TempDir()
	snapshot, err := Acquire(ctx, Options{RemoteURL: "file://" + original, AllowLocalFixture: true, TempRoot: owned}, Boundary{TargetBaseSHA: base, HeadSHA: head, Number: 19, ChangedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	workspace, err := snapshot.PrepareFeedbackWorkspace(ctx, owned, "feature/existing-pr")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	if data, err := os.ReadDir(workspace.Directory); err != nil || len(data) != 1 || data[0].Name() != ".git" {
		t.Fatal("export checked out untrusted files", err)
	}
	if gitFixture(t, workspace.Directory, "branch", "--show-current") != "feature/existing-pr" || gitFixture(t, workspace.Directory, "rev-parse", "HEAD") != head {
		t.Fatal("existing branch/head identity changed")
	}
	if got := gitFixture(t, workspace.Directory, "merge-base", head, base); got != base {
		t.Fatal("history was not exported")
	}
	// Only fixture-owned source is materialized here. Production must perform
	// these fixed operations through the native isolated command host.
	gitFixture(t, workspace.Directory, "read-tree", "HEAD")
	gitFixture(t, workspace.Directory, "checkout-index", "--all")
	if data, err := os.ReadFile(filepath.Join(workspace.Directory, "binary.bin")); err != nil || !bytes.Equal(data, binary) {
		t.Fatal("binary source was lost", err)
	}
	if info, err := os.Stat(filepath.Join(workspace.Directory, "executable")); err != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("executable mode was lost", err)
	}
	if target, err := os.Readlink(filepath.Join(workspace.Directory, "link")); err != nil || target != "../outside-user-data" {
		t.Fatal("symlink was dereferenced or changed", err)
	}
	config, err := os.ReadFile(filepath.Join(workspace.Directory, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{original, snapshot.root, "extraheader", "hooksPath", "credential", "remote \""} {
		if strings.Contains(string(config), forbidden) {
			t.Fatal("export inherited acquisition authority or external storage")
		}
	}
	objects := filepath.Join(snapshot.root, "objects")
	err = filepath.WalkDir(objects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(objects, path)
		if err != nil {
			return err
		}
		a, err := os.Stat(path)
		if err != nil {
			return err
		}
		b, err := os.Stat(filepath.Join(workspace.Directory, ".git", "objects", rel))
		if err != nil {
			return err
		}
		if os.SameFile(a, b) {
			t.Fatal("mutable workspace shares an immutable object inode")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if gitFixture(t, workspace.Directory, "rev-parse", "HEAD") != head {
		t.Fatal("workspace relies on acquisition storage")
	}
	if data, err := os.ReadFile(filepath.Join(original, "dirty-sentinel")); err != nil || string(data) != "user-owned\n" {
		t.Fatal("ordinary checkout changed")
	}
	if gitFixture(t, original, "branch", "--show-current") != "feature/existing-pr" {
		t.Fatal("ordinary checkout switched branches")
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(owned)
	if err != nil || len(entries) != 0 {
		t.Fatal("owned export storage leaked", err)
	}
}

func TestFeedbackWorkspaceRejectsInvalidOrExternalObjectStorage(t *testing.T) {
	for _, kind := range []string{"bad branch", "head escape", "symlink object", "alternates", "oversized object", "closed snapshot"} {
		t.Run(kind, func(t *testing.T) {
			owned := t.TempDir()
			bare := filepath.Join(t.TempDir(), "bare")
			if err := os.MkdirAll(filepath.Join(bare, "objects", "info"), 0700); err != nil {
				t.Fatal(err)
			}
			snapshot := &Snapshot{root: bare, HeadSHA: strings.Repeat("a", 40), Manifests: map[string][]Entry{"head": {}}}
			branch := "feature/existing-pr"
			switch kind {
			case "bad branch":
				branch = "feature/../../outside"
			case "head escape":
				snapshot.HeadSHA = "../outside"
			case "symlink object":
				if err := os.Symlink(t.TempDir(), filepath.Join(bare, "objects", "external")); err != nil {
					t.Fatal(err)
				}
			case "alternates":
				if err := os.WriteFile(filepath.Join(bare, "objects", "info", "alternates"), []byte("/outside\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized object":
				f, err := os.Create(filepath.Join(bare, "objects", "huge"))
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(1<<30 + 1)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "closed snapshot":
				if err := snapshot.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if workspace, err := snapshot.PrepareFeedbackWorkspace(context.Background(), owned, branch); err == nil {
				workspace.Close()
				t.Fatal("unsafe or incomplete object export succeeded")
			}
			entries, err := os.ReadDir(owned)
			if err != nil || len(entries) != 0 {
				t.Fatal("failed export left private resources", err)
			}
		})
	}
}
