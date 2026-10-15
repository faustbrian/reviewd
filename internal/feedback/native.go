package feedback

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Each invocation owns a fresh runtime/session. No primary state, credentials,
// mutable tools or status callbacks are inherited by a delegated leaf.
func RunNativeAgent(ctx context.Context, options codex.RuntimeOptions, request codex.FeedbackRequest) (output codex.ReviewOutput, err error) {
	executable := options.Executable
	if executable == "" {
		executable = "codex"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	version, probeErr := lifetime.Output(probeCtx, exec.CommandContext(probeCtx, executable, "--version"))
	cancel()
	if probeErr != nil || strings.TrimSpace(string(version)) != "codex-cli 0.162.1" {
		return output, errors.New("feedback requires tested Codex CLI 0.162.1; run reviewd doctor for compatibility guidance")
	}
	worker, err := codex.StartRuntime(ctx, options)
	if err != nil {
		return output, err
	}
	defer func() { err = errors.Join(err, worker.Close()) }()
	if err = worker.CheckAccount(ctx); err != nil {
		return output, err
	}
	return worker.HandleFeedback(ctx, request)
}

type ExecutionResources struct {
	Directory   string
	Environment CommandEnvironment
	Inspect     func(context.Context) (GitInspection, error)
	Command     func(context.Context, []string, time.Duration, bool) (codex.CommandResult, error)
	Quiesce     func() error
}

func prepareExecutionResources(ctx context.Context, owner *store.PrivateOwner, snapshot *source.Snapshot, branch string, options WorkspaceOptions) (*ExecutionResources, error) {
	var workspace *Workspace
	var err error
	if options.retainedDirectory != "" {
		if _, err = retainPath(owner.Root, options.retainedDirectory, "reviewd-feedback-workspace-"); err != nil {
			return nil, err
		}
		workspace = &Workspace{Directory: options.retainedDirectory, owner: owner, options: options}
		_, err = inspectWorkspaceGit(ctx, workspace, snapshot, branch)
	} else {
		workspace, err = PrepareWorkspace(ctx, owner, snapshot, branch, options)
	}
	if err != nil {
		return nil, err
	}
	// Checkout is quiescent and validated. The workspace owns the single command
	// backend and preserves failed-start/close uncertainty across every phase.
	command := func(ctx context.Context, args []string, timeout time.Duration, writable bool) (codex.CommandResult, error) {
		host, err := workspace.StartCommands(ctx, writable)
		if err != nil {
			return codex.CommandResult{}, err
		}
		return host.Run(ctx, args, timeout)
	}
	return &ExecutionResources{Directory: workspace.Directory, Environment: workspace.Environment, Inspect: func(ctx context.Context) (GitInspection, error) {
		return inspectWorkspaceGit(ctx, workspace, snapshot, branch)
	}, Command: command, Quiesce: workspace.Close}, nil
}
