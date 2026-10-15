//go:build darwin || linux

package codex

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

func configureProcess(command *exec.Cmd) {
	lifetime.Configure(command)
}

// superviseProcess owns the one group created by Setpgid. Cleanup begins at
// parent exit, not a later Close, so a completed runtime cannot retain children.
func superviseProcess(command *exec.Cmd, parent <-chan error, stop <-chan struct{}) (result error) {
	// No competing Cmd.Wait may release the leader PID while group signals
	// remain possible. Reap only after this supervisor's final signal.
	exited := parent
	defer func() {
		if result != nil {
			// Inspection failure must not leave a known-owned live server
			// running merely because its private storage will be retained.
			result = errors.Join(result, signalProcessGroup(command.Process.Pid, syscall.SIGKILL))
		}
		// Exit status is not a protocol outcome: normal interruption kills the
		// server. Only an inability to reap must prevent isolation cleanup.
		_, err := lifetime.Reap(command, exited)
		result = errors.Join(result, err)
	}()
	select {
	case err := <-parent:
		observed := make(chan error, 1)
		observed <- err
		exited = observed
	case <-stop:
	}
	alive, err := processGroupAlive(command.Process.Pid)
	if err != nil {
		return err
	}
	if alive {
		if err := signalProcessGroup(command.Process.Pid, syscall.SIGTERM); err != nil {
			return err
		}
		if err := waitProcessGroup(command.Process.Pid, 3*time.Second); err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if err := signalProcessGroup(command.Process.Pid, syscall.SIGKILL); err != nil {
				return err
			}
			if err := waitProcessGroup(command.Process.Pid, 3*time.Second); err != nil {
				return err
			}
		}
	}
	return nil
}

func signalProcessGroup(group int, signal syscall.Signal) error {
	err := lifetime.SignalGroup(group, signal)
	if errors.Is(err, syscall.EPERM) {
		// Every signal edge, including escalation and deferred KILL, can race
		// the last member's exit on Darwin. Only bounded proof that this
		// still-reserved owned group cannot execute supersedes the denial.
		if deadErr := waitProcessGroup(group, 3*time.Second); deadErr != nil {
			return errors.Join(err, deadErr)
		}
		return nil
	}
	return err
}

func waitProcessGroup(group int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := processGroupAlive(group)
		if err != nil || !alive {
			return err
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processGroupAlive(group int) (bool, error) {
	// Both supported operating systems provide ps. Only process metadata is
	// inspected: never command arguments or environment values. Zombies cannot
	// execute or retain credentials and may await an unrelated init's reaper.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return lifetime.GroupAlive(ctx, group)
}
