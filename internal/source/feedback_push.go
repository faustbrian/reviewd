package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// FeedbackPushPack imports only object data after the caller has positively
// quiesced the workspace. Git runs in fresh application-owned bare storage,
// never against the workspace's config, refs, hooks, alternates or executables.
// expectedCommit is the exact commit constructed by the trusted scoped signer;
// the model's object IDs or claimed signing outcome are not an authority.
func (s *Snapshot) FeedbackPushPack(ctx context.Context, workspace, commit, tree string, expectedCommit []byte) (_ []byte, err error) {
	if s.root == "" || !objectID.MatchString(s.HeadSHA) || !objectID.MatchString(commit) || !objectID.MatchString(tree) || len(commit) != len(s.HeadSHA) || len(tree) != len(commit) || commit == s.HeadSHA || len(expectedCommit) == 0 || len(expectedCommit) > 2<<20 {
		return nil, errors.New("feedback transport requires the exact approved additive commit")
	}
	headers, _, ok := bytes.Cut(expectedCommit, []byte("\n\n"))
	if !ok || !bytes.HasPrefix(headers, []byte("tree "+tree+"\nparent "+s.HeadSHA+"\n")) {
		return nil, errors.New("feedback commit does not match its reviewed tree and original parent")
	}
	parents := 0
	for _, line := range bytes.Split(headers, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("parent ")) {
			parents++
		}
	}
	if parents != 1 {
		return nil, errors.New("feedback transport cannot publish a merge or rewritten parent")
	}
	storage, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, errors.New("quiescent feedback workspace is unavailable")
	}
	defer storage.Close()
	// OpenRoot confines traversal even when a workspace component is replaced.
	objects, err := storage.OpenRoot(".git/objects")
	if err != nil {
		return nil, errors.New("feedback objects escape or are unavailable")
	}
	defer objects.Close()
	root, err := os.MkdirTemp(lifetime.Root(ctx, s.root), "reviewd-feedback-push-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if !errors.Is(err, lifetime.ErrCleanupUncertain) {
			err = errors.Join(err, os.RemoveAll(root))
		}
	}()
	r := runner{executable: "git", root: root, env: GitEnvironment(os.Environ(), root, "")}
	format := "sha1"
	if len(commit) == 64 {
		format = "sha256"
	}
	if _, err = r.run(ctx, nil, 1<<20, "init", "--bare", "--template=", "--object-format="+format, root); err != nil {
		return nil, err
	}
	remaining := int64(1 << 30)
	err = fs.WalkDir(objects.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot read complete feedback objects")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || name == "info/alternates" || name == "info/http-alternates" {
			return errors.New("feedback transport rejects external object storage")
		}
		destination := filepath.Join(root, "objects", filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > remaining {
			return errors.New("feedback objects exceed the bounded private import")
		}
		input, err := objects.Open(name)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			input.Close()
			return err
		}
		count, copyErr := io.Copy(output, io.LimitReader(contextObjectReader{ctx, input}, info.Size()+1))
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil || closeErr != nil || count != info.Size() {
			return errors.Join(copyErr, closeErr, errors.New("feedback objects changed during import"))
		}
		remaining -= count
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err = r.run(ctx, nil, 1<<20, "--no-replace-objects", "fsck", "--strict", "--no-reflogs", "--no-dangling", commit); err != nil {
		return nil, err
	}
	actual, err := r.run(ctx, nil, 2<<20, "--no-replace-objects", "cat-file", "commit", commit)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(actual, expectedCommit) {
		return nil, errors.New("feedback object differs from the exact approved signed commit")
	}
	// Git itself creates the pack. No untrusted process receives authentication;
	// the authenticated application transport sends these validated bytes only.
	pack, err := r.run(ctx, []byte(commit+"\n^"+s.HeadSHA+"\n"), 256<<20, "--no-replace-objects", "pack-objects", "--stdout", "--revs")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(pack[:min(len(pack), 4)]), "PACK") {
		return nil, errors.New("feedback pack is incomplete")
	}
	return pack, nil
}
