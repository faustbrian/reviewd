package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalAnchorsBindChangedObjectAndSide(t *testing.T) {
	diff := "diff --git a/old name.txt b/new name.txt\nsimilarity index 70%\nrename from old name.txt\nrename to new name.txt\n--- a/old name.txt\n+++ b/new name.txt\n@@ -1,3 +1,3 @@\n same\n-old\n+new\n last\n"
	anchors, err := ParseAnchors(diff)
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range []struct {
		path, side string
		line       int
	}{{"new name.txt", "RIGHT", 2}, {"new name.txt", "LEFT", 2}, {"new name.txt", "RIGHT", 3}} {
		if !anchors.Contains(valid.path, valid.side, valid.line) {
			t.Errorf("canonical review anchor rejected: %+v", valid)
		}
	}
	for _, invalid := range []struct {
		path, side string
		line       int
	}{{"old name.txt", "RIGHT", 2}, {"new name.txt", "RIGHT", 4}, {"new name.txt", "LEFT", 0}, {"new name.txt", "LEFT", 9}, {"new name.txt", "BOGUS", 2}} {
		if anchors.Contains(invalid.path, invalid.side, invalid.line) {
			t.Errorf("fabricated/noncanonical anchor accepted: %+v", invalid)
		}
	}
	if _, err := ParseAnchors("--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n-one\n+two\n"); err == nil {
		t.Error("incomplete diff hunk returned usable canonical anchors")
	}
}

func TestNativeGitHeaderDelimitersPreserveCanonicalFilename(t *testing.T) {
	for _, tc := range []struct{ name, oldPath, newPath string }{
		{"modified-space", "file name.go", "file name.go"},
		{"renamed-space", "old name.go", "new name.go"},
		{"trailing-space", "file name.go ", "file name.go "},
		{"quoted-tab", "tab\tname.go", "tab\tname.go"},
		{"quoted-unicode", "unicode-å.go", "unicode-å.go"},
		{"quoted-space", "file \"name.go", "file \"name.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			git := func(args ...string) string {
				command := exec.Command("git", args...)
				command.Dir = directory
				command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + directory, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("owned native Git fixture failed: %v %s", err, output)
				}
				return string(output)
			}
			git("init", "-q", "-b", "main", "--template=")
			if err := os.WriteFile(filepath.Join(directory, tc.oldPath), []byte("same1\nsame2\nold\nsame3\nsame4\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "--", tc.oldPath)
			git("commit", "-qm", "base")
			base := strings.TrimSpace(git("rev-parse", "HEAD"))
			if tc.oldPath != tc.newPath {
				if err := os.Rename(filepath.Join(directory, tc.oldPath), filepath.Join(directory, tc.newPath)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(directory, tc.newPath), []byte("same1\nsame2\nnew\nsame3\nsame4\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "--", tc.oldPath, tc.newPath)
			git("commit", "-qm", "head")
			diff := git("diff", "--find-renames", base, "HEAD", "--")
			anchors, err := ParseAnchors(diff)
			if err != nil {
				t.Fatal(err)
			}
			for _, side := range []string{"LEFT", "RIGHT"} {
				if !anchors.Contains(tc.newPath, side, 3) {
					t.Fatalf("native header delimiter rejected real filename %q side %s: %+v", tc.newPath, side, anchors)
				}
			}
			if anchors.Contains(tc.newPath+"\t", "RIGHT", 3) {
				t.Fatal("syntactic delimiter became part of filename")
			}
		})
	}
}
