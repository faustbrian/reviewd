//go:build darwin || linux

// Package lifetime carries application-owned temporary storage and process
// registration through source and private-model execution. It grants no tool
// or publication authority and does not resume an interrupted process.
package lifetime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Process interface {
	Started(context.Context, int) error
	Finished(context.Context) error
}
type Tracker interface {
	Starting(context.Context) (Process, error)
}
type Scope struct {
	Root    string
	Tracker Tracker
}
type scopeKey struct{}

var ErrCleanupUncertain = errors.New("private command shutdown is unproven; private storage retained")

func WithScope(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}
func Root(ctx context.Context, fallback string) string {
	if scope, ok := ctx.Value(scopeKey{}).(Scope); ok {
		return scope.Root
	}
	return fallback
}

// PendingCompletion reuses the owner's unresolved-launch slot for execution
// whose lifetime cannot be inferred from its server's process group. Do not
// assign a group with Started: only a canonical terminal acknowledgement may
// Finish this slot. A lost acknowledgement deliberately survives recovery.
func PendingCompletion(ctx context.Context) (Process, error) {
	if scope, ok := ctx.Value(scopeKey{}).(Scope); ok && scope.Tracker != nil {
		return scope.Tracker.Starting(ctx)
	}
	return nil, nil
}

// Start records an unresolved launch before fork. A crash between fork and
// registration therefore retains the scope instead of treating it as empty.
func Start(ctx context.Context, command *exec.Cmd) (Process, error) {
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid || command.SysProcAttr.Pgid != 0 {
		return nil, errors.New("private process launch requires its own dedicated process group")
	}
	var process Process
	if scope, ok := ctx.Value(scopeKey{}).(Scope); ok && scope.Tracker != nil {
		var err error
		process, err = scope.Tracker.Starting(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := command.Start(); err != nil {
		return nil, errors.Join(err, Finish(process))
	}
	if process != nil {
		persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := process.Started(persistCtx, command.Process.Pid)
		cancel()
		if err != nil {
			// The direct child has not been reaped, so its group number cannot
			// have been reused. Do not return an untracked running child.
			killErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				return nil, errors.Join(ErrCleanupUncertain, err, killErr)
			}
			if deadErr := WaitDead(command.Process.Pid); deadErr != nil {
				return nil, errors.Join(ErrCleanupUncertain, err, deadErr)
			}
			return nil, errors.Join(err, Finish(process))
		}
	}
	return process, nil
}

// Call only after process-group cleanup has proved that no member can run.
func Finish(process Process) error {
	if process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return process.Finished(ctx)
}

// LiveGroups reads only metadata, never arguments or environment. Reused group
// numbers cause conservative retention, not signalling of an unrelated process.
func LiveGroups(ctx context.Context) (map[int]bool, error) {
	return inspectGroups(ctx, 0)
}

func inspectGroups(ctx context.Context, selectedGroup int) (map[int]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	args := []string{"-axo", "pgid=,stat="}
	if selectedGroup > 0 {
		args = []string{"-g", strconv.Itoa(selectedGroup), "-o", "pgid=,stat="}
	}
	output, err := exec.CommandContext(ctx, "ps", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("cannot establish private process-group liveness: %w", errors.Join(err, ctx.Err()))
	}
	groups := map[int]bool{}
	observed := false
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.New("incomplete process-group metadata; private storage retained")
		}
		group, err := strconv.Atoi(fields[0])
		if err != nil || group < 0 {
			return nil, errors.New("invalid process-group metadata; private storage retained")
		}
		if group == 0 && selectedGroup == 0 {
			// Linux full scans include unrelated kernel threads. Zero is
			// never an owned group, nor a shutdown proof for a pending launch.
			continue
		}
		if selectedGroup > 0 && group != selectedGroup {
			return nil, errors.New("unexpected process-group metadata; private storage retained")
		}
		observed = true
		// Linux's process-only Zl is an exited leader with sibling threads.
		// It is not proof that the group can no longer use private storage.
		if !strings.HasPrefix(fields[1], "Z") || strings.ContainsRune(fields[1], 'l') {
			groups[group] = true
		}
	}
	if !observed {
		return nil, errors.New("empty process-group inspection cannot establish liveness; private storage retained")
	}
	return groups, nil
}

// An absent kernel process group needs no full-host metadata scan. Positive or
// permission-ambiguous results still require inspection so zombies alone do
// not retain a private scope forever. Signal zero is a query, not termination.
func GroupAlive(ctx context.Context, group int) (bool, error) {
	if group <= 0 {
		return false, errors.New("private process group must be positive")
	}
	err := syscall.Kill(-group, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, fmt.Errorf("cannot query private process group: %w", err)
	}
	selectedGroup := 0
	if runtime.GOOS == "darwin" {
		// Darwin supports selection by process group. Do not collect every
		// unrelated process for each short Git command's held zombie leader.
		// Linux ps uses different -g semantics, so retains the full scan.
		selectedGroup = group
	}
	groups, err := inspectGroups(ctx, selectedGroup)
	if err != nil {
		return false, err
	}
	return groups[group], nil
}

func WaitDead(group int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		alive, err := GroupAlive(ctx, group)
		if err != nil {
			return err
		}
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("private process group has not stopped: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Output is for short application prerequisite probes, not model tools. The
// caller chooses the executable; neither PR content nor a worker selects it.
func Output(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	var output bytes.Buffer
	command.Stdout = &output
	err := Run(ctx, command)
	return output.Bytes(), err
}

// Configure disables CommandContext's competing cancellation callback. Only
// the supervisor signals groups, and it never releases the leader's identity
// until all such signals have finished.
func Configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return nil }
	command.WaitDelay = 5 * time.Second
}

// Observe must precede the sole Reap call. It never calls Cmd.Wait.
func Observe(command *exec.Cmd) <-chan error {
	exited := make(chan error, 1)
	go func() { exited <- WaitExited(command.Process.Pid) }()
	return exited
}

// Reap is called only after the caller's final group signal. On failure it
// still arranges eventual reaping, but the caller must retain private storage.
func Reap(command *exec.Cmd, exited <-chan error) (commandErr, cleanupErr error) {
	type result struct{ commandErr, cleanupErr error }
	done := make(chan result, 1)
	go func() {
		observationErr := <-exited
		done <- result{command.Wait(), observationErr}
	}()
	select {
	case result := <-done:
		return result.commandErr, result.cleanupErr
	case <-time.After(6 * time.Second):
		return nil, errors.New("private child reaping timed out; private storage retained")
	}
}

// Run supervises short, application-selected prerequisite and immutable-source
// commands. Even a naturally exited leader stays unreaped during group cleanup.
func Run(ctx context.Context, command *exec.Cmd) error {
	Configure(command)
	process, err := Start(ctx, command)
	if err != nil {
		return err
	}
	exited := Observe(command)
	observed := make(chan error, 1)
	select {
	case err := <-exited:
		observed <- err
		exited = observed
	case <-ctx.Done():
	}
	// The sole child waiter has not run. Even when the child already exited,
	// its PID cannot be reused for an unrelated process group at this signal.
	signalErr := SignalGroup(command.Process.Pid, syscall.SIGKILL)
	deadErr := WaitDead(command.Process.Pid)
	waitErr, reapErr := Reap(command, exited)
	// Darwin can deny a signal during the last member's exit. The later
	// successful group-shutdown proof supersedes that transient denial;
	// command status, cancellation and any inspection/reaping failure remain.
	if deadErr == nil && errors.Is(signalErr, syscall.EPERM) {
		signalErr = nil
	}
	if signalErr != nil || deadErr != nil || reapErr != nil {
		return errors.Join(ErrCleanupUncertain, ctx.Err(), signalErr, deadErr, waitErr, reapErr)
	}
	return errors.Join(ctx.Err(), waitErr, Finish(process))
}

// SignalGroup requires a dedicated group whose direct child is still held
// unreaped by this supervisor. Never call it with a recovered/stored group ID.
func SignalGroup(group int, signal syscall.Signal) error {
	if group <= 0 {
		return errors.New("private process group must be positive")
	}
	err := syscall.Kill(-group, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		alive, inspectErr := GroupAlive(ctx, group)
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if !alive {
			return nil
		}
	}
	return err
}
