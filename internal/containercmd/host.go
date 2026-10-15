// Package containercmd supervises application-owned checkout commands without
// granting the model access to the local container control plane.
package containercmd

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/faustbrian/reviewd/internal/commandcapture"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/containerapi"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/store"
)

type Options struct {
	Endpoint, Image, Workspace string
	Helper, HelperSHA256       string
	Writable                   bool
}
type Environment struct{ OS, Architecture, Image, Workspace string }
type activeCommand struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type Host struct {
	owner      *store.PrivateOwner
	options    Options
	client     *containerapi.Client
	identity   containerapi.Identity
	helper     string
	mu         sync.Mutex
	state      sync.Mutex
	active     *activeCommand
	closing    bool
	uncertain  error
	closeOnce  sync.Once
	closeError error
}

func New(ctx context.Context, owner *store.PrivateOwner, options Options) (*Host, error) {
	if owner == nil {
		return nil, errors.New("container commands require a durable private owner")
	}
	root, err := filepath.EvalSymlinks(owner.Root)
	if err != nil {
		return nil, err
	}
	workspace, err := filepath.EvalSymlinks(options.Workspace)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, workspace)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("container checkout must be inside its private owner")
	}
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("container commands require a private workspace")
	}
	git, err := os.Lstat(filepath.Join(workspace, ".git"))
	if err != nil || !git.IsDir() || git.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("container checkout requires isolated local Git metadata")
	}
	client, err := containerapi.New(options.Endpoint)
	if err != nil {
		return nil, err
	}
	identity, err := client.CheckImage(ctx, options.Image)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	helper, err := freezeHelper(root, options.Helper, options.HelperSHA256, identity.Architecture)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	options.Workspace = workspace
	return &Host{owner: owner, options: options, client: client, identity: identity, helper: helper}, nil
}
func (h *Host) Environment() Environment {
	return Environment{OS: "linux", Architecture: h.identity.Architecture, Image: h.options.Image, Workspace: "/workspace"}
}

func validCommand(argv []string, timeout time.Duration) bool {
	if len(argv) == 0 || len(argv) > 256 || timeout <= 0 || timeout > 20*time.Minute {
		return false
	}
	total := 0
	for _, argument := range argv {
		total += len(argument)
		if strings.ContainsRune(argument, 0) {
			return false
		}
	}
	return total <= 128<<10
}
func (h *Host) Run(ctx context.Context, argv []string, timeout time.Duration) (result codex.CommandResult, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !validCommand(argv, timeout) {
		return result, errors.New("container command needs bounded argv and timeout up to twenty minutes")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	operation, cancel := context.WithTimeout(h.owner.Context(ctx), timeout)
	active := &activeCommand{cancel: cancel, done: make(chan struct{})}
	h.state.Lock()
	if h.closing || h.uncertain != nil {
		prior := h.uncertain
		h.state.Unlock()
		cancel()
		return result, errors.Join(errors.New("container command host is closed or prior completion is uncertain"), prior)
	}
	h.active = active
	h.state.Unlock()
	var completion lifetime.Process
	var owned *store.PrivateContainer
	var reference containerapi.Resource
	scopeClosed := true
	startAttempted, outputKnown := false, false
	remove := func(cleanup context.Context) error {
		if scopeClosed || owned == nil {
			return nil
		}
		if reference.ContainerID == "" {
			return containerapi.ErrUncertain
		}
		if err := h.client.Remove(cleanup, reference); err != nil {
			return err
		}
		if err := owned.Finished(cleanup); err != nil {
			return err
		}
		scopeClosed = true
		return nil
	}
	defer func() {
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		cleanupErr := remove(cleanup)
		complete := scopeClosed && cleanupErr == nil && (outputKnown || !startAttempted)
		if complete {
			if finishErr := lifetime.Finish(completion); finishErr != nil {
				complete = false
				cleanupErr = errors.Join(cleanupErr, finishErr)
			}
		}
		h.state.Lock()
		if !complete {
			h.uncertain = errors.Join(lifetime.ErrCleanupUncertain, err, cleanupErr)
			err = h.uncertain
			result = codex.CommandResult{}
		} else {
			err = errors.Join(err, cleanupErr)
		}
		h.active = nil
		close(active.done)
		h.state.Unlock()
	}()
	capture, err := os.MkdirTemp(h.owner.Root, "command-output-")
	if err != nil {
		return result, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return result, err
	}
	// The key is a private input of this owner, outside every container mount.
	// Retain it with unresolved work; no second journal or replay authority exists.
	if err := os.WriteFile(capture+".key", key, 0600); err != nil {
		return result, err
	}
	completion, err = lifetime.PendingCompletion(operation)
	if err != nil {
		return result, err
	}
	owned, err = h.owner.StartingContainer(operation, h.options.Endpoint, h.identity.DaemonID)
	if err != nil {
		return result, err
	}
	scopeClosed = false
	reference, err = owned.Resource(operation)
	if err != nil {
		return result, err
	}
	id, createErr := h.client.Create(operation, reference, containerapi.CommandSpec{Image: h.options.Image, Workspace: h.options.Workspace, Helper: h.helper, Capture: capture, Key: key, Argv: append([]string{}, argv...), Writable: h.options.Writable})
	if id != "" {
		reference.ContainerID = id
		binding, stop := context.WithTimeout(context.Background(), 15*time.Second)
		err = owned.Bind(binding, id)
		stop()
		if err != nil {
			return result, errors.Join(createErr, err)
		}
	}
	if createErr != nil {
		return result, createErr
	}
	if reference.ContainerID == "" {
		return result, containerapi.ErrUncertain
	}
	startAttempted = true
	if err = h.client.Start(operation, reference); err != nil {
		return result, err
	}
	exit, err := h.client.Wait(operation, reference)
	if err != nil {
		return result, err
	}
	cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
	err = remove(cleanup)
	stop()
	if err != nil {
		return result, err
	}
	if err := operation.Err(); err != nil {
		return result, err
	}
	captured, err := commandcapture.Read(capture, key, commandcapture.Request{Nonce: reference.ResourceID, Argv: argv}, exit)
	if err != nil {
		return result, err
	}
	if err := operation.Err(); err != nil {
		return result, err
	}
	outputKnown = true
	return codex.CommandResult{ExitCode: captured.ExitCode, Stdout: captured.Stdout, Stderr: captured.Stderr}, nil
}
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.state.Lock()
		h.closing = true
		active := h.active
		h.state.Unlock()
		if active != nil {
			active.cancel()
			timer := time.NewTimer(40 * time.Second)
			defer timer.Stop()
			select {
			case <-active.done:
			case <-timer.C:
				h.closeError = lifetime.ErrCleanupUncertain
			}
		}
		h.state.Lock()
		h.closeError = errors.Join(h.closeError, h.uncertain)
		h.state.Unlock()
		h.closeError = errors.Join(h.closeError, h.client.Close())
	})
	return h.closeError
}
