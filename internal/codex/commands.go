package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// CommandOptions is application-owned capability data. It is never decoded
// from a model, repository preference or remote feedback. No credentials are
// borrowed by this command-only server and no model turn is available here.
type CommandOptions struct {
	RuntimeOptions
	Workspace string
	ReadRoots []string
	Writable  bool
}
type CommandResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
type CommandRuntime struct {
	runtime   *Runtime
	workspace string
	mu        sync.Mutex
}

func StartCommandRuntime(ctx context.Context, options CommandOptions) (_ *CommandRuntime, err error) {
	workspace, err := filepath.EvalSymlinks(options.Workspace)
	if err != nil {
		return nil, errors.New("feedback workspace is unavailable")
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("feedback commands require a private isolated workspace")
	}
	gitDirectory := filepath.Join(workspace, ".git")
	if options.Writable {
		if gitInfo, statErr := os.Lstat(gitDirectory); statErr == nil {
			if !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("feedback Git metadata must be an isolated directory inside the workspace")
			}
		} else {
			return nil, errors.New("feedback Git metadata must be an isolated directory inside the workspace")
		}
	}
	executable := options.Executable
	if executable == "" {
		executable = "codex"
	}
	executable, err = exec.LookPath(executable)
	if err != nil {
		return nil, errors.New("Codex CLI is required for enforced feedback command isolation")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(lifetime.Root(ctx, options.TempRoot), "reviewd-commands-")
	if err != nil {
		return nil, err
	}
	isolation := &Isolation{Root: root, ownedRoot: root, Control: filepath.Join(root, "control"), Workspace: workspace}
	runtimeOwnsIsolation := false
	defer func() {
		if err != nil && !runtimeOwnsIsolation {
			err = errors.Join(err, isolation.Close())
		}
	}()
	if err = os.Mkdir(isolation.Control, 0700); err != nil {
		return nil, err
	}
	scratch := filepath.Join(root, "scratch")
	if err = os.MkdirAll(filepath.Join(scratch, "tmp"), 0700); err != nil {
		return nil, err
	}
	// Scratch and tool caches are app-owned siblings, never repository paths
	// whose symlinks or tracked contents could redirect setup writes.
	// The legacy readOnly/workspaceWrite RPC policy cannot express restricted
	// reads. Omit it: this named native profile denies host credentials by root.
	var configuration strings.Builder
	configuration.WriteString("approval_policy=\"never\"\ndefault_permissions=\"reviewd-feedback\"\n[permissions.reviewd-feedback.filesystem]\n\":root\"=\"deny\"\n\":minimal\"=\"read\"\n")
	for _, path := range options.ReadRoots {
		absolute, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return nil, errors.New("feedback instruction/toolchain read root is unavailable")
		}
		absolute, resolveErr = filepath.Abs(absolute)
		if resolveErr != nil || absolute == string(filepath.Separator) {
			return nil, errors.New("feedback read grant cannot expose the host root")
		}
		if absolute == workspace {
			continue
		}
		fmt.Fprintf(&configuration, "%s=\"read\"\n", strconv.Quote(absolute))
	}
	fmt.Fprintf(&configuration, "%s=\"write\"\n", strconv.Quote(scratch))
	permission := "read"
	if options.Writable {
		permission = "write"
	}
	fmt.Fprintf(&configuration, "%s=%s\n", strconv.Quote(workspace), strconv.Quote(permission))
	if options.Writable {
		// Native workspace-write deliberately protects Git metadata unless a more
		// specific grant exists. Only the isolated repository, never a worktree
		// gitfile or external common directory, receives the modifying capability.
		fmt.Fprintf(&configuration, "%s=\"write\"\n", strconv.Quote(gitDirectory))
	}
	fmt.Fprintf(&configuration, "[permissions.reviewd-feedback.network]\nenabled=false\n[shell_environment_policy]\ninherit=\"none\"\n[shell_environment_policy.set]\n")
	path := os.Getenv("PATH")
	env := map[string]string{"PATH": path, "HOME": workspace, "TMPDIR": filepath.Join(scratch, "tmp"), "GOCACHE": filepath.Join(scratch, "go-build"), "GOMODCACHE": filepath.Join(scratch, "go-modules"), "GOTMPDIR": filepath.Join(scratch, "tmp"), "GOPATH": filepath.Join(scratch, "go"), "GOWORK": "off", "GIT_OPTIONAL_LOCKS": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0"}
	for key, value := range env {
		fmt.Fprintf(&configuration, "%s=%s\n", key, strconv.Quote(value))
	}
	if err = os.WriteFile(filepath.Join(isolation.Control, "config.toml"), []byte(configuration.String()), 0600); err != nil {
		return nil, err
	}
	// Transfer cleanup authority before launch. startServer retains its root
	// on any unproven teardown, not only errors with a particular sentinel.
	runtimeOwnsIsolation = true
	runtime, err := startServer(ctx, executable, isolation, []string{"PATH=" + path, "HOME=" + workspace, "CODEX_HOME=" + isolation.Control})
	if err != nil {
		return nil, err
	}
	host := &CommandRuntime{runtime: runtime, workspace: workspace}
	// Standalone execution is buffered. Drain bounded native progress without
	// exposing a second tool/approval channel or stranding the JSONL reader.
	go func() {
		for event := range runtime.Client.Events() {
			if len(event.ID) > 0 {
				_ = runtime.Client.RejectContext(ctx, event.ID, &RPCError{Code: -32601, Message: "command host grants no model or approval capability"})
			}
		}
	}()
	return host, nil
}

func (r *CommandRuntime) Run(ctx context.Context, command []string, timeout time.Duration) (CommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result CommandResult
	if len(command) == 0 || len(command) > 256 || timeout <= 0 || timeout > 20*time.Minute {
		return result, errors.New("feedback command requires a bounded argv and timeout up to twenty minutes")
	}
	total := 0
	for _, arg := range command {
		total += len(arg)
		if strings.ContainsRune(arg, 0) {
			return result, errors.New("feedback command contains a NUL argument")
		}
	}
	if total > 128<<10 {
		return result, errors.New("feedback command arguments exceed 128 KiB")
	}
	err := r.runtime.Client.Call(ctx, "command/exec", map[string]any{"command": command, "cwd": r.workspace, "timeoutMs": timeout.Milliseconds(), "outputBytesCap": 2 << 20}, &result)
	return result, err
}
func (r *CommandRuntime) Close() error { return r.runtime.Close() }
