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
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/containerapi"
	"github.com/faustbrian/reviewd/internal/containercmd"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Workspace never owns cleanup of its files independently of the durable
// PrivateOwner. Close stops only the command host; the common owner must prove
// all children stopped before removing source, workspace and runtime storage.
type Workspace struct {
	Directory   string
	Git         string
	Commands    CommandHost
	Environment CommandEnvironment
	owner       *store.PrivateOwner
	options     WorkspaceOptions
	mu          sync.Mutex
	uncertain   error
}

type CommandHost interface {
	Run(context.Context, []string, time.Duration) (codex.CommandResult, error)
	Close() error
}

type CommandEnvironment struct {
	Backend, OS, Architecture, Workspace string
	Image                                string `json:",omitempty"`
	Instructions                         string `json:",omitempty"`
}

// Container selectors come only from frozen operator global/project policy.
// Neither the model, repository preferences nor the CLI can grant them.
type WorkspaceOptions struct {
	Runtime                           codex.RuntimeOptions
	ContainerEndpoint, ContainerImage string
	// Application-only acceptance seam: actual relocated package, never PATH,
	// model input or a configurable replacement helper.
	nativeExecutable string
	// Exact operator hook files only; never HOME, credentials or repository-
	// selected host paths. Containers instead need the frozen policy in-image.
	trustedReadRoots  []string
	instructionRoot   string
	retainedDirectory string
	inspectionHead    string
}

func (w *Workspace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeCommands()
}

func (w *Workspace) closeCommands() error {
	if w.Commands != nil {
		w.uncertain = errors.Join(w.uncertain, w.Commands.Close())
		if w.uncertain == nil {
			w.Commands = nil
		}
	}
	return w.uncertain
}

// StartCommands opens the same frozen backend only after known prior shutdown.
// A read/verify phase can therefore obtain a genuinely read-only command grant.
func (w *Workspace) StartCommands(ctx context.Context, writable bool) (CommandHost, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.closeCommands(); err != nil {
		return nil, err
	}
	host, environment, git, err := startWorkspaceCommands(ctx, w.owner, w.Directory, writable, w.options)
	if err != nil {
		w.uncertain = err
		return nil, err
	}
	w.Commands, w.Environment, w.Git = host, environment, git
	return host, nil
}

func startWorkspaceCommands(ctx context.Context, owner *store.PrivateOwner, directory string, writable bool, options WorkspaceOptions) (CommandHost, CommandEnvironment, string, error) {
	return openWorkspaceCommands(ctx, owner, func(ctx context.Context) (CommandHost, CommandEnvironment, string, error) {
		return startWorkspaceBackend(ctx, owner, directory, writable, options)
	})
}

// Keep backend acquisition and subsequent commands behind the same ownership
// boundary. The opener is the platform adapter, not a caller-selected backend.
func openWorkspaceCommands(ctx context.Context, owner *store.PrivateOwner, open func(context.Context) (CommandHost, CommandEnvironment, string, error)) (CommandHost, CommandEnvironment, string, error) {
	if owner == nil {
		return nil, CommandEnvironment{}, "", errors.New("feedback commands require their original private owner")
	}
	host, environment, git, err := open(owner.Context(ctx))
	if err != nil {
		return nil, environment, git, err
	}
	return &ownedWorkspaceCommands{CommandHost: host, owner: owner}, environment, git, nil
}

type ownedWorkspaceCommands struct {
	CommandHost
	owner *store.PrivateOwner
}

func (h *ownedWorkspaceCommands) Run(ctx context.Context, command []string, timeout time.Duration) (codex.CommandResult, error) {
	return h.CommandHost.Run(h.owner.Context(ctx), command, timeout)
}

func startWorkspaceBackend(ctx context.Context, owner *store.PrivateOwner, directory string, writable bool, options WorkspaceOptions) (CommandHost, CommandEnvironment, string, error) {
	environment := CommandEnvironment{Backend: "native", OS: runtime.GOOS, Architecture: runtime.GOARCH, Workspace: directory}
	environment.Instructions = options.instructionRoot
	if runtime.GOOS == "linux" {
		git, err := workspaceGit()
		if err != nil {
			return nil, environment, "", err
		}
		reads := append([]string{git}, options.trustedReadRoots...)
		if options.instructionRoot != "" {
			reads = append(reads, options.instructionRoot)
		}
		host, err := codex.StartCommandRuntime(ctx, codex.CommandOptions{RuntimeOptions: options.Runtime, Workspace: directory, Writable: writable, ReadRoots: reads, InstructionsRoot: options.instructionRoot})
		return host, environment, git, err
	}
	if runtime.GOOS != "darwin" {
		return nil, environment, "", errors.New("feedback commands support only macOS and Linux")
	}
	if options.ContainerEndpoint == "" || options.ContainerImage == "" {
		return nil, environment, "", errors.New("macOS feedback checkout requires operator feedback.container_endpoint and feedback.container_image, a running supported local container runtime and the paired package helper; native fallback is disabled")
	}
	client, err := containerapi.New(options.ContainerEndpoint)
	if err != nil {
		return nil, environment, "", err
	}
	defer client.Close()
	identity, err := client.CheckImage(ctx, options.ContainerImage)
	if err != nil {
		return nil, environment, "", errors.Join(errors.New("macOS feedback container prerequisites are unavailable; no native fallback"), err)
	}
	native := options.nativeExecutable
	if native == "" {
		native, err = os.Executable()
		if err != nil {
			return nil, environment, "", err
		}
	}
	helper, err := containercmd.FromPackage(native, identity.Architecture)
	if err != nil {
		return nil, environment, "", err
	}
	host, err := containercmd.New(ctx, owner, containercmd.Options{Endpoint: options.ContainerEndpoint, Image: options.ContainerImage, Workspace: directory, Writable: writable, Helper: helper.Path, HelperSHA256: helper.SHA256, InstructionsRoot: options.instructionRoot})
	if err != nil {
		return nil, environment, "", err
	}
	guest := host.Environment()
	environment = CommandEnvironment{Backend: "container", OS: guest.OS, Architecture: guest.Architecture, Workspace: guest.Workspace, Image: guest.Image}
	environment.Instructions = guest.Instructions
	return host, environment, "git", nil
}

// PrepareWorkspace connects immutable acquisition to a complete existing-head
// checkout. No PR-controlled file is materialized or executed outside the
// platform's isolated backend; no credentials or ambient Git policy enter it.
// Commit hooks/signing and publication have their own later scoped boundaries.
func PrepareWorkspace(ctx context.Context, owner *store.PrivateOwner, snapshot *source.Snapshot, branch string, options WorkspaceOptions) (_ *Workspace, err error) {
	if owner == nil || snapshot == nil {
		return nil, errors.New("feedback checkout requires a durable private owner and immutable source")
	}
	ctx = owner.Context(ctx)
	if snapshot.HeadTreeSHA == "" {
		return nil, errors.New("feedback checkout requires the exact immutable head tree identity")
	}
	files, err := snapshot.PrepareFeedbackWorkspace(ctx, owner.Root, branch)
	if err != nil {
		return nil, err
	}
	w := &Workspace{Directory: files.Directory, owner: owner, options: options}
	commands, err := w.StartCommands(ctx, true)
	if err != nil {
		// The shared owner, not this constructor, decides when an uncertain
		// failed launch has stopped and its complete private inputs can go.
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, w.Close())
		}
	}()
	for _, arguments := range [][]string{{"read-tree", snapshot.HeadSHA}, {"checkout-index", "--all"}, {"rev-parse", "--verify", "HEAD"}, {"symbolic-ref", "HEAD"}, {"write-tree"}} {
		result, runErr := commands.Run(ctx, append([]string{w.Git}, arguments...), time.Minute)
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
		want := map[string]string{"rev-parse": snapshot.HeadSHA, "symbolic-ref": "refs/heads/" + branch, "write-tree": snapshot.HeadTreeSHA}[arguments[0]]
		if want != "" && strings.TrimSpace(result.Stdout) != want {
			return nil, errors.New("isolated feedback checkout changed the immutable head, branch or complete index tree")
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := snapshot.ValidateFeedbackWorkspace(ctx, files.Directory); err != nil {
		return nil, err
	}
	return w, nil
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
