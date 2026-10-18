package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// Profile overrides never select this root: all profiles for one OS user must
// serialize modification of the same canonical repository/PR identity.
func feedbackModifierDirectory() (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if runtime.GOOS == "darwin" {
		root = ""
	}
	if root != "" && !filepath.IsAbs(root) {
		return "", errors.New("XDG_STATE_HOME must be an absolute directory")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if runtime.GOOS == "darwin" {
			root = filepath.Join(home, "Library", "Application Support")
		} else {
			root = filepath.Join(home, ".local", "state")
		}
	}
	return filepath.Join(root, "reviewd", "feedback-modifiers"), nil
}
