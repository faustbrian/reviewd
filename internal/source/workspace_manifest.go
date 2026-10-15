package source

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf8"
)

// Match the existing full-object export bound. Checkout must not expand a
// compressed repository into an unbounded host filesystem allocation.
const maxFeedbackTree = int64(1 << 30)

func feedbackPath(name string) bool {
	if name == "" || name == "." || path.Clean(name) != name || path.IsAbs(name) || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00:") || !utf8.ValidString(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func (s *Snapshot) feedbackManifest() (map[string]Entry, map[string]bool, error) {
	entries, present := s.Manifests["head"]
	if !present || !objectID.MatchString(s.HeadSHA) {
		return nil, nil, errors.New("feedback requires the complete immutable head manifest")
	}
	files, directories := map[string]Entry{}, map[string]bool{".": true}
	remaining := maxFeedbackTree
	for _, entry := range entries {
		if !feedbackPath(entry.Path) || entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755" && entry.Mode != "120000") || !objectID.MatchString(entry.OID) || len(entry.OID) != len(s.HeadSHA) || entry.Size < 0 || entry.Size > remaining {
			return nil, nil, errors.New("feedback checkout has an unsupported path, mode, submodule or size; complete trees up to 1 GiB are supported")
		}
		if _, duplicate := files[entry.Path]; duplicate {
			return nil, nil, errors.New("feedback head manifest contains a duplicate path")
		}
		files[entry.Path] = entry
		remaining -= entry.Size
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			directories[parent] = true
		}
	}
	for name := range files {
		if directories[name] {
			return nil, nil, errors.New("feedback head manifest aliases a file and directory")
		}
	}
	return files, directories, nil
}

// ValidateFeedbackWorkspace verifies the full quiescent worktree against Git
// blob identities, not the lossy text-review projection. Its caller MUST first
// close command hosts successfully. Links are compared as data, never followed.
// Exact directory-entry names expose case/Unicode sharing collisions instead of
// certifying a checkout that silently collapsed two immutable paths.
func (s *Snapshot) ValidateFeedbackWorkspace(ctx context.Context, directory string) error {
	expected, directories, err := s.feedbackManifest()
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	metadata, err := root.Lstat(".git")
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return errors.New("feedback checkout has no isolated Git metadata directory")
	}
	seen := map[string]bool{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == ".git" {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("feedback Git metadata is not a private directory")
			}
			return fs.SkipDir
		}
		if entry.IsDir() {
			if !directories[name] {
				return errors.New("feedback checkout contains an unexpected directory or incompatible filename semantics")
			}
			return nil
		}
		wanted, present := expected[name]
		if !present || seen[name] {
			return errors.New("feedback checkout contains an unexpected path or incompatible filename semantics")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		var digest hash.Hash = sha1.New() // Git SHA-1 object identity, not authentication.
		if len(s.HeadSHA) == 64 {
			digest = sha256.New()
		}
		fmt.Fprintf(digest, "blob %d\x00", wanted.Size)
		if wanted.Mode == "120000" {
			if info.Mode()&os.ModeSymlink == 0 {
				return errors.New("feedback checkout changed a symlink into another file type")
			}
			target, err := root.Readlink(name)
			if err != nil || int64(len(target)) != wanted.Size {
				return errors.New("feedback checkout changed symlink data")
			}
			_, _ = io.WriteString(digest, target)
		} else {
			if !info.Mode().IsRegular() || info.Size() != wanted.Size || (info.Mode().Perm()&0111 != 0) != (wanted.Mode == "100755") {
				return errors.New("feedback checkout changed file size, mode or type")
			}
			file, err := root.Open(name)
			if err != nil {
				return err
			}
			opened, statErr := file.Stat()
			count, readErr := io.Copy(digest, io.LimitReader(contextObjectReader{ctx, file}, wanted.Size+1))
			closeErr := file.Close()
			if statErr != nil || !os.SameFile(info, opened) || readErr != nil || closeErr != nil || count != wanted.Size {
				return errors.Join(errors.New("feedback checkout changed during quiescent validation"), readErr, closeErr)
			}
		}
		if hex.EncodeToString(digest.Sum(nil)) != wanted.OID {
			return errors.New("feedback checkout differs from immutable blob bytes; transformed or collapsed source cannot be reviewed as exact")
		}
		seen[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return errors.New("feedback checkout omitted immutable paths; the filesystem may not preserve their names")
	}
	return nil
}
