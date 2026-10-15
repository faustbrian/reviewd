package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	Workspace        string
	ReadRoots        []string
	Writable         bool
	InstructionsRoot string
}
type CommandResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}
type CommandRuntime struct {
	runtime         *Runtime
	workspace       string
	mu              sync.Mutex
	stateMu         sync.Mutex
	sequence        uint64
	active          *commandExecution
	closing         bool
	uncertain       error
	linuxCompletion bool
	closeOnce       sync.Once
	closeError      error
}

type commandExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
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
	executable, err = commandExecutable(ctx, executable)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	version := exec.CommandContext(probe, executable, "--version")
	version.Dir = workspace
	version.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + workspace}
	output, versionErr := lifetime.Output(probe, version)
	cancel()
	if versionErr != nil || strings.TrimSpace(string(output)) != "codex-cli 0.162.1" {
		return nil, errors.Join(errors.New("feedback command execution requires tested Codex CLI 0.162.1; no checkout command was started"), versionErr)
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
	// The Linux sandbox's inner helper is this exact native file. Do not expose
	// its parent installation, temporary directory, HOME or provider state.
	fmt.Fprintf(&configuration, "%s=\"read\"\n", strconv.Quote(executable))
	for _, path := range options.ReadRoots {
		absolute, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			return nil, errors.New("feedback instruction/toolchain read root is unavailable")
		}
		absolute, resolveErr = filepath.Abs(absolute)
		if resolveErr != nil || absolute == string(filepath.Separator) {
			return nil, errors.New("feedback read grant cannot expose the host root")
		}
		if absolute == workspace || absolute == executable {
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
	if options.InstructionsRoot != "" {
		env["REVIEWD_INSTRUCTIONS_PATH"] = options.InstructionsRoot
		env["PYTHONDONTWRITEBYTECODE"] = "1"
	}
	for key, value := range env {
		fmt.Fprintf(&configuration, "%s=%s\n", key, strconv.Quote(value))
	}
	if err = os.WriteFile(filepath.Join(isolation.Control, "config.toml"), []byte(configuration.String()), 0600); err != nil {
		return nil, err
	}
	// Transfer cleanup authority before launch. startServer retains its root
	// on any unproven teardown, not only errors with a particular sentinel.
	runtimeOwnsIsolation = true
	processContext := ctx
	if runtimePlatformLinux() {
		// Keep the transport alive long enough to obtain canonical command exit
		// after caller cancellation. Close remains the sole server supervisor.
		processContext = context.WithoutCancel(ctx)
	}
	runtime, err := startServerContext(ctx, processContext, executable, isolation, []string{"PATH=" + path, "HOME=" + workspace, "CODEX_HOME=" + isolation.Control}, "-c", "features.use_legacy_landlock=false", "-c", "default_permissions=\"reviewd-feedback\"")
	if err != nil {
		return nil, err
	}
	host := &CommandRuntime{runtime: runtime, workspace: workspace, linuxCompletion: runtimePlatformLinux()}
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
	if err := ctx.Err(); err != nil {
		return result, err
	}
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	active := &commandExecution{cancel: cancel, done: make(chan struct{})}
	r.stateMu.Lock()
	if r.closing || r.uncertain != nil {
		prior := r.uncertain
		r.stateMu.Unlock()
		return result, errors.Join(errors.New("command host is closed or its prior completion is uncertain"), prior)
	}
	r.active = active
	r.sequence++
	processID := "reviewd-command-" + strconv.FormatUint(r.sequence, 10)
	r.stateMu.Unlock()
	defer func() { r.stateMu.Lock(); r.active = nil; close(active.done); r.stateMu.Unlock() }()
	var completion lifetime.Process
	if r.linuxCompletion {
		var err error
		completion, err = lifetime.PendingCompletion(ctx)
		if err != nil {
			return result, err
		}
	}
	result, complete, err := r.executeCommand(operation, processID, command, timeout)
	if complete {
		if finishErr := lifetime.Finish(completion); finishErr != nil {
			complete = false
			err = errors.Join(err, finishErr)
		}
	}
	if !complete {
		r.stateMu.Lock()
		r.uncertain = errors.Join(lifetime.ErrCleanupUncertain, err)
		r.stateMu.Unlock()
		return result, errors.Join(lifetime.ErrCleanupUncertain, err)
	}
	return result, err
}

func runtimePlatformLinux() bool { return runtime.GOOS == "linux" }

// A terminate ACK accepts a control request. Only the original terminal result
// acknowledges process exit. Keep its correlation alive during cancellation.
func (r *CommandRuntime) executeCommand(ctx context.Context, processID string, command []string, timeout time.Duration) (CommandResult, bool, error) {
	type response struct {
		raw json.RawMessage
		err error
	}
	wire, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	sent := make(chan struct{})
	received := make(chan response, 1)
	go func() {
		var raw json.RawMessage
		err := r.runtime.Client.call(wire, "command/exec", map[string]any{"processId": processID, "command": command, "cwd": r.workspace, "timeoutMs": timeout.Milliseconds(), "outputBytesCap": 2 << 20}, &raw, func() { close(sent) })
		received <- response{raw, err}
	}()
	decode := func(reply response) (CommandResult, bool, error) {
		var result CommandResult
		if reply.err != nil {
			return result, false, reply.err
		}
		var value struct {
			ExitCode *int    `json:"exitCode"`
			Stdout   *string `json:"stdout"`
			Stderr   *string `json:"stderr"`
		}
		// Native capture caps raw bytes before decoding; valid UTF-8 text may
		// expand beyond that cap. The transport bounds the response frame.
		if json.Unmarshal(reply.raw, &value) != nil || value.ExitCode == nil || value.Stdout == nil || value.Stderr == nil {
			return result, false, errors.New("command omitted canonical exit/output facts")
		}
		result = CommandResult{ExitCode: *value.ExitCode, Stdout: *value.Stdout, Stderr: *value.Stderr}
		return result, true, ctx.Err()
	}
	deadline := time.NewTimer(timeout + 5*time.Second)
	defer deadline.Stop()
	var cause error
	select {
	case reply := <-received:
		return decode(reply)
	case <-ctx.Done():
		cause = ctx.Err()
	case <-deadline.C:
		cause = context.DeadlineExceeded
	}
	control, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case reply := <-received:
		result, complete, err := decode(reply)
		return result, complete, errors.Join(cause, err)
	case <-sent:
	case <-control.Done():
		return CommandResult{}, false, errors.Join(cause, control.Err())
	}
	// A server-side race can report no live process after it already completed;
	// the correlated terminal result, not this ACK/error, decides completion.
	terminateErr := r.runtime.Client.Call(control, "command/exec/terminate", map[string]string{"processId": processID}, nil)
	wait, finish := context.WithTimeout(context.Background(), 3*time.Second)
	defer finish()
	select {
	case reply := <-received:
		result, complete, err := decode(reply)
		if complete {
			return result, true, errors.Join(cause, err)
		}
		return result, false, errors.Join(cause, terminateErr, err)
	case <-wait.Done():
		return CommandResult{}, false, errors.Join(cause, terminateErr, errors.New("terminal command acknowledgement is unavailable"))
	}
}

func (r *CommandRuntime) Close() error {
	r.closeOnce.Do(func() {
		r.stateMu.Lock()
		r.closing = true
		active := r.active
		if active != nil {
			active.cancel()
		}
		r.stateMu.Unlock()
		if active != nil {
			select {
			case <-active.done:
			case <-time.After(10 * time.Second):
				r.stateMu.Lock()
				r.uncertain = errors.Join(r.uncertain, lifetime.ErrCleanupUncertain)
				r.stateMu.Unlock()
			}
		}
		r.stateMu.Lock()
		uncertain := r.uncertain
		r.stateMu.Unlock()
		r.closeError = errors.Join(uncertain, r.runtime.close(uncertain != nil))
	})
	return r.closeError
}
