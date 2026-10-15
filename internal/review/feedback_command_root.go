package review

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FeedbackCommandRoot grants only the captured text bundle to the separately
// authorized feedback command backend. Ordinary review captures cannot grant
// this capability. It neither selects a host install directory nor an owner-
// wide root; relative linked dependencies remain inside the frozen bundle.
func (s *CapturedSkill) FeedbackCommandRoot(ownerRoot string) (string, error) {
	if s == nil || !s.feedbackCommands || s.root == "" || s.Identity.Digest != digest(s.Documents) {
		return "", errors.New("only an intact feedback workflow capture may grant instruction reads")
	}
	ownerInfo, err := os.Lstat(ownerRoot)
	if err != nil || !ownerInfo.IsDir() {
		return "", errors.New("feedback instruction owner must be a directory, not a symlink")
	}
	owner, err := filepath.EvalSymlinks(ownerRoot)
	if err != nil {
		return "", err
	}
	canonicalInfo, err := os.Lstat(owner)
	if err != nil || !os.SameFile(ownerInfo, canonicalInfo) {
		return "", errors.New("feedback instruction owner identity changed during admission")
	}
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", err
	}
	if root != s.root || filepath.Dir(root) != owner || !strings.HasPrefix(filepath.Base(root), "reviewd-skill-") {
		return "", errors.New("feedback instruction bundle must be the exact private owner's captured child")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("feedback instruction capture is not private directory storage")
	}
	expected := map[string]string{}
	for key, text := range s.Documents {
		var name string
		if strings.HasPrefix(key, "skill/") {
			name = filepath.Base(filepath.Dir(s.Path)) + "/" + strings.TrimPrefix(key, "skill/")
		} else if strings.HasPrefix(key, "skill-dependency/") {
			name = strings.TrimPrefix(key, "skill-dependency/")
		} else {
			return "", errors.New("instruction capture contains an unrecognized authority")
		}
		if !fs.ValidPath(name) {
			return "", errors.New("instruction capture contains an invalid path")
		}
		expected[name] = text
	}
	storage, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer storage.Close()
	seen := 0
	err = fs.WalkDir(storage.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("instruction capture must not contain links")
		}
		if entry.IsDir() {
			return nil
		}
		text, ok := expected[name]
		if !ok {
			return errors.New("uncaptured file would expand instruction read access")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("instruction capture contains unsupported storage")
		}
		data, err := storage.ReadFile(name)
		if err != nil || string(data) != text {
			return errors.New("frozen instruction bytes changed")
		}
		seen++
		return nil
	})
	if err != nil {
		return "", err
	}
	if seen != len(expected) {
		return "", errors.New("frozen instruction bundle is incomplete")
	}
	return root, nil
}
