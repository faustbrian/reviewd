package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Isolation struct {
	Root, Control, Workspace string
	ownedRoot                string
	closeOnce                sync.Once
	closeError               error
}

// WorkerEnvironment deliberately excludes GitHub tokens, SSH agents, proxy
// credentials and inherited writable MCP/plugin configuration.
func WorkerEnvironment(source []string, control string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "OPENAI_API_KEY": true}
	result := make([]string, 0, len(allowed)+1)
	for _, item := range source {
		key, _, ok := strings.Cut(item, "=")
		if ok && allowed[strings.ToUpper(key)] {
			result = append(result, item)
		}
	}
	return append(result, "CODEX_HOME="+control)
}

func WorkerArgs() []string {
	args := []string{"app-server", "--strict-config", "-c", "web_search=\"disabled\""}
	for _, feature := range []string{
		"shell_tool", "unified_exec", "apps", "remote_plugin", "browser_use",
		"computer_use", "code_mode", "skill_mcp_dependency_install", "view_image",
		"image_generation", "multi_agent", "plugins", "hooks", "memories",
		"skill_search", "tool_suggest", "workspace_dependencies", "auth_elicitation",
		"goals",
	} {
		args = append(args, "-c", "features."+feature+"=false")
	}
	args = append(args, "-c", "features.skip_host_skill_discovery=true")
	// The tool host must stay enabled for bounded application-owned tools. It
	// does not grant shell/network access; mutation requests are never approved.
	return args
}

func NewIsolation(parent, authHome string) (result *Isolation, err error) {
	root, err := os.MkdirTemp(parent, "reviewd-codex-")
	if err != nil {
		return nil, err
	}
	result = &Isolation{Root: root, ownedRoot: root, Control: filepath.Join(root, "control"), Workspace: filepath.Join(root, "workspace")}
	owned := result
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	for _, name := range []string{result.Control, result.Workspace} {
		if err = os.Mkdir(name, 0700); err != nil {
			return nil, err
		}
	}
	if authHome == "" {
		authHome = os.Getenv("CODEX_HOME")
		if authHome == "" {
			var home string
			home, err = os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			authHome = filepath.Join(home, ".codex")
		}
	}
	data, readErr := os.ReadFile(filepath.Join(authHome, "auth.json"))
	if readErr == nil {
		if len(data) > 4*1024*1024 {
			return nil, errors.New("Codex authentication file exceeds supported size")
		}
		if err = os.WriteFile(filepath.Join(result.Control, "auth.json"), data, 0600); err != nil {
			return nil, err
		}
	} else if os.Getenv("OPENAI_API_KEY") == "" {
		return nil, fmt.Errorf("Codex file authentication unavailable; sign in with Codex or configure provider authentication: %w", readErr)
	}
	return result, nil
}

func (i *Isolation) Close() error {
	i.closeOnce.Do(func() { i.closeError = os.RemoveAll(i.ownedRoot) })
	return i.closeError
}
