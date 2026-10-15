package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// FeedbackWorkspace is a separate full-object repository, not a shared worktree
// or a projection of text review evidence. Its worktree is initially empty.
// Checkout and execution belong exclusively to the native isolated command
// host. The caller must prove child shutdown through its PrivateOwner before
// Close, including when construction of a later executor fails.
type FeedbackWorkspace struct {
	Directory string
	closeOnce sync.Once
	closeErr  error
}

func (w *FeedbackWorkspace) Close() error {
	w.closeOnce.Do(func() {
		if err := os.RemoveAll(w.Directory); err != nil {
			w.closeErr = errors.New("cannot remove owned feedback workspace")
		}
	})
	return w.closeErr
}

func feedbackBranch(branch string) bool {
	if branch == "" || len(branch) > 1024 || branch == "HEAD" || strings.HasPrefix(branch, "-") || strings.HasSuffix(branch, ".") || !utf8.ValidString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return false
	}
	for _, char := range branch {
		if char <= 32 || char == 127 || strings.ContainsRune(`~^:?*[\`, char) {
			return false
		}
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// PrepareFeedbackWorkspace copies complete immutable objects into fresh private
// storage. It does not copy config, hooks, refs, credentials, alternates or hard
// links, and never checks out or executes PR-controlled files on the host.
func (s *Snapshot) PrepareFeedbackWorkspace(ctx context.Context, parent, branch string) (_ *FeedbackWorkspace, err error) {
	if !feedbackBranch(branch) || !objectID.MatchString(s.HeadSHA) || s.root == "" {
		return nil, errors.New("feedback workspace requires an exact immutable head and existing branch")
	}
	if _, _, err := s.feedbackManifest(); err != nil {
		return nil, err
	}
	objects, err := os.OpenRoot(filepath.Join(s.root, "objects"))
	if err != nil {
		return nil, errors.New("immutable Git objects are unavailable")
	}
	defer objects.Close()
	directory, err := os.MkdirTemp(lifetime.Root(ctx, parent), "reviewd-feedback-workspace-")
	if err != nil {
		return nil, errors.New("cannot create private feedback workspace")
	}
	workspace := &FeedbackWorkspace{Directory: directory}
	defer func() {
		if err != nil {
			err = errors.Join(err, workspace.Close())
		}
	}()
	git := filepath.Join(directory, ".git")
	if err = os.MkdirAll(filepath.Join(git, "objects"), 0700); err != nil {
		return nil, err
	}
	remaining := int64(1 << 30)
	err = fs.WalkDir(objects.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot completely export immutable Git objects")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || name == "info/alternates" || name == "info/http-alternates" {
			return errors.New("feedback Git objects must not use symlinks or external object storage")
		}
		destination := filepath.Join(git, "objects", filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > remaining {
			return errors.New("complete feedback Git objects exceed the 1-GiB export limit or contain unsupported storage")
		}
		input, err := objects.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		count, copyErr := io.Copy(output, io.LimitReader(contextObjectReader{ctx, input}, remaining+1))
		closeErr := output.Close()
		if copyErr != nil || closeErr != nil || count != info.Size() || count > remaining {
			return errors.Join(copyErr, closeErr, errors.New("immutable Git object export was incomplete or changed"))
		}
		remaining -= count
		return nil
	})
	if err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	if err = os.MkdirAll(filepath.Dir(filepath.Join(git, ref)), 0700); err != nil {
		return nil, err
	}
	version, extension := "0", ""
	if len(s.HeadSHA) == 64 {
		version, extension = "1", "\n[extensions]\n objectFormat = sha256\n"
	}
	config := "[core]\n repositoryFormatVersion = " + version + "\n fileMode = true\n bare = false\n logAllRefUpdates = true\n" + extension
	for path, data := range map[string]string{"HEAD": "ref: " + ref + "\n", "config": config, ref: s.HeadSHA + "\n"} {
		if err = os.WriteFile(filepath.Join(git, path), []byte(data), 0600); err != nil {
			return nil, err
		}
	}
	return workspace, nil
}

type contextObjectReader struct {
	context.Context
	io.Reader
}

func (r contextObjectReader) Read(data []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}
