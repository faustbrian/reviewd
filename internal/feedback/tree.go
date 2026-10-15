package feedback

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

type executionEntry struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Digest string `json:"digest,omitempty"`
	Target string `json:"symlink_target,omitempty"`
}
type executionTree struct {
	ID          string
	Entries     []executionEntry
	GitMetadata []executionEntry
	Git         GitInspection
	Sources     map[string]string
	Gaps        []string
}

func executionPath(name string) bool {
	if name == "" || name == "." || name != path.Clean(name) || path.IsAbs(name) || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00:\r\n") || !utf8.ValidString(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// Caller must first stop all command children. Discover the complete current
// worktree, including undeclared additions. Original paths retain deletions.
// Git metadata is identity evidence, never source text or a followed link.
func captureExecutionTree(directory string, paths []string) (executionTree, error) {
	result := executionTree{Entries: []executionEntry{}, GitMetadata: []executionEntry{}, Sources: map[string]string{}, Gaps: []string{}}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return result, err
	}
	defer root.Close()
	present := map[string]bool{}
	for _, name := range paths {
		if !executionPath(name) || present[name] {
			return result, errors.New("feedback tree requires unique canonical source paths")
		}
		present[name] = true
	}
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == ".git" && !entry.IsDir() {
			return errors.New("feedback Git metadata must remain an isolated directory")
		}
		// Object identities/content are read through Git inside the command
		// sandbox. Index, refs, hooks and configuration must still invalidate
		// approvals when changed, without repeatedly hashing all history.
		if name == ".git/objects" {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("feedback Git objects must remain private directory storage")
			}
			return fs.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		metadata := strings.HasPrefix(name, ".git/")
		candidate := name
		if metadata {
			candidate = strings.TrimPrefix(name, ".git/")
		}
		if !executionPath(candidate) {
			return errors.New("feedback tree contains an unsupported current path")
		}
		present[name] = true
		if len(present) > 100000 {
			return errors.New("feedback current tree exceeds the bounded path inventory")
		}
		return nil
	}); err != nil {
		return result, err
	}
	paths = make([]string, 0, len(present))
	for name := range present {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	remaining := int64(256 << 20)
	previous := ""
	for _, name := range paths {
		metadata := strings.HasPrefix(name, ".git/")
		if (!metadata && !executionPath(name)) || name == previous {
			return result, errors.New("feedback tree requires unique canonical source paths")
		}
		previous = name
		parts := strings.Split(name, "/")
		missing := false
		for i := 1; i < len(parts); i++ {
			info, err := root.Lstat(strings.Join(parts[:i], "/"))
			if errors.Is(err, os.ErrNotExist) {
				missing = true
				break
			}
			if err != nil || !info.IsDir() {
				return result, errors.New("feedback tree contains a parent symlink or unsupported directory")
			}
		}
		info, statErr := root.Lstat(name)
		if missing || errors.Is(statErr, os.ErrNotExist) {
			result.Entries = append(result.Entries, executionEntry{Path: name, Mode: "deleted"})
			continue
		}
		if statErr != nil {
			return result, statErr
		}
		entry := executionEntry{Path: name, Mode: "100644"}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.Mode = "120000"
			entry.Target, err = root.Readlink(name)
			if err != nil {
				return result, err
			}
			if metadata {
				result.GitMetadata = append(result.GitMetadata, entry)
			} else {
				result.Entries = append(result.Entries, entry)
			}
			continue
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > 8<<20 || info.Size() > remaining {
			return result, errors.New("feedback source exceeds bounded regular-file capture; preserve the workspace for explicit handling")
		}
		if info.Mode().Perm()&0111 != 0 {
			entry.Mode = "100755"
		}
		file, err := root.Open(name)
		if err != nil {
			return result, err
		}
		opened, statErr := file.Stat()
		data, readErr := io.ReadAll(io.LimitReader(file, info.Size()+1))
		closeErr := file.Close()
		if statErr != nil || !os.SameFile(info, opened) || readErr != nil || closeErr != nil || int64(len(data)) != info.Size() {
			return result, errors.New("feedback source changed during stopped-child capture")
		}
		digest := sha256.Sum256(data)
		entry.Digest = hex.EncodeToString(digest[:])
		remaining -= int64(len(data))
		if metadata {
			result.GitMetadata = append(result.GitMetadata, entry)
			continue
		}
		result.Entries = append(result.Entries, entry)
		if utf8.Valid(data) && !strings.ContainsRune(string(data), 0) {
			result.Sources[name] = string(data)
		} else {
			result.Gaps = append(result.Gaps, "Binary source is bound by content digest but unavailable as text: "+name)
		}
	}
	encoded, _ := json.Marshal(struct{ Entries, GitMetadata []executionEntry }{result.Entries, result.GitMetadata})
	sum := sha256.Sum256(encoded)
	result.ID = hex.EncodeToString(sum[:])
	return result, nil
}
