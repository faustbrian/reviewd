package feedback

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Workspace never owns cleanup of its files independently of the durable
// PrivateOwner. Close stops only the command host; the common owner must prove
// all children stopped before removing source, workspace and runtime storage.
type Workspace struct {
	Directory string
	Git       string
	Commands  *codex.CommandRuntime
}

func (w *Workspace) Close() error { return w.Commands.Close() }

// PrepareWorkspace connects immutable acquisition to a complete existing-head
// checkout. No PR-controlled file is materialized or executed outside native
// isolation, and no credentials or ambient Git policy enter the command host.
// Commit hooks/signing and publication have their own later scoped boundaries.
func PrepareWorkspace(ctx context.Context, owner *store.PrivateOwner, snapshot *source.Snapshot, branch string, options codex.RuntimeOptions) (_ *Workspace, err error) {
	if owner == nil || snapshot == nil {
		return nil, errors.New("feedback checkout requires a durable private owner and immutable source")
	}
	ctx = owner.Context(ctx)
	git, err := workspaceGit()
	if err != nil {
		return nil, err
	}
	files, err := snapshot.PrepareFeedbackWorkspace(ctx, owner.Root, branch)
	if err != nil {
		return nil, err
	}
	commands, err := codex.StartCommandRuntime(ctx, codex.CommandOptions{RuntimeOptions: options, Workspace: files.Directory, Writable: true, ReadRoots: []string{git}})
	if err != nil {
		// The shared owner, not this constructor, decides when an uncertain
		// failed launch has stopped and its complete private inputs can go.
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, commands.Close())
		}
	}()
	for _, arguments := range [][]string{{"read-tree", snapshot.HeadSHA}, {"checkout-index", "--all"}, {"rev-parse", "--verify", "HEAD"}} {
		result, runErr := commands.Run(ctx, append([]string{git}, arguments...), time.Minute)
		if runErr != nil {
			return nil, runErr
		}
		if result.ExitCode != 0 {
			detail := strings.TrimSpace(result.Stderr)
			if len(detail) > 4096 {
				detail = detail[:4096]
			}
			return nil, fmt.Errorf("isolated feedback checkout failed (exit %d): %s", result.ExitCode, detail)
		}
		if arguments[0] == "rev-parse" && strings.TrimSpace(result.Stdout) != snapshot.HeadSHA {
			return nil, errors.New("isolated feedback checkout omitted the exact immutable PR head")
		}
	}
	return &Workspace{Directory: files.Directory, Git: git, Commands: commands}, nil
}

func workspaceGit() (string, error) {
	candidates := []string{}
	if runtime.GOOS == "darwin" {
		// /usr/bin/git is an xcrun discovery shim requiring shared cache
		// writes. Use installed Git, without granting those external writes.
		candidates = []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"}
	} else if selected, err := exec.LookPath("git"); err == nil {
		candidates = []string{selected}
	}
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return resolved, nil
		}
	}
	return "", errors.New("isolated feedback commands require installed Git; on macOS install Apple Command Line Tools or Xcode")
}
