package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This interface lets the red regression exercise missing capability rather
// than rely on a compilation failure as evidence.
type feedbackPackPreparer interface {
	FeedbackPushPack(context.Context, string, string, string, []byte) ([]byte, error)
}

func TestFeedbackPushPackImportsOnlyExactAdditiveCommit(t *testing.T) {
	root := t.TempDir()
	gitFixture(t, root, "init", "-q", "-b", "feature/existing", "--template=")
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("before\n"), 0600)
	gitFixture(t, root, "add", "source.txt")
	gitFixture(t, root, "commit", "-qm", "base")
	parent := gitFixture(t, root, "rev-parse", "HEAD")
	gitFixture(t, root, "update-ref", "refs/pull/17/head", parent)
	snapshot, err := Acquire(context.Background(), Options{RemoteURL: "file://" + root, AllowLocalFixture: true, TempRoot: t.TempDir()}, Boundary{TargetBaseSHA: parent, HeadSHA: parent, Number: 17})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	preparer, ok := any(snapshot).(feedbackPackPreparer)
	if !ok {
		t.Fatal("validated additive feedback pack capability is missing")
	}
	os.WriteFile(filepath.Join(root, "source.txt"), []byte("after\n"), 0600)
	gitFixture(t, root, "add", "source.txt")
	gitFixture(t, root, "commit", "-qm", "fix: preserve feedback behavior")
	commit := gitFixture(t, root, "rev-parse", "HEAD")
	tree := gitFixture(t, root, "rev-parse", "HEAD^{tree}")
	contents := []byte(gitFixture(t, root, "cat-file", "commit", commit) + "\n")
	pack, err := preparer.FeedbackPushPack(context.Background(), root, commit, tree, contents)
	if err != nil || !bytes.HasPrefix(pack, []byte("PACK")) {
		t.Fatalf("exact additive pack: %v", err)
	}
	for _, wrong := range []struct {
		commit, tree string
		contents     []byte
	}{
		{parent, tree, contents}, {commit, strings.Repeat("a", 40), contents}, {commit, tree, append(bytes.Clone(contents), 'x')},
	} {
		if _, err := preparer.FeedbackPushPack(context.Background(), root, wrong.commit, wrong.tree, wrong.contents); err == nil {
			t.Fatal("unvalidated object proposal received a push pack")
		}
	}
	// Host Git must never consume workspace config, hooks or external object
	// storage. The hostile config is irrelevant; external objects are rejected.
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("not git configuration\n"), 0600)
	if _, err := preparer.FeedbackPushPack(context.Background(), root, commit, tree, contents); err != nil {
		t.Fatal("workspace configuration was consumed", err)
	}
	os.MkdirAll(filepath.Join(root, ".git", "objects", "info"), 0700)
	os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/outside\n"), 0600)
	if _, err := preparer.FeedbackPushPack(context.Background(), root, commit, tree, contents); err == nil {
		t.Fatal("external object storage accepted")
	}
}
