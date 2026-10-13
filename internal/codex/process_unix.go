//go:build darwin || linux

package codex

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The supervisor owns group termination, including descendants after the
	// original process exits. CommandContext only wakes that supervisor.
	command.Cancel = func() error { return nil }
}

// superviseProcess owns the one group created by Setpgid. Cleanup begins at
// parent exit, not a later Close, so a completed runtime cannot retain children.
func superviseProcess(command *exec.Cmd, parent <-chan error, stop <-chan struct{}) error {
	parentExited := false
	select {
	case <-parent:
		parentExited = true
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
	if !parentExited {
		select {
		case <-parent:
		case <-time.After(time.Second):
			return errors.New("Codex parent was not reaped after group termination")
		}
	}
	return nil
}

func signalProcessGroup(group int, signal syscall.Signal) error {
	err := syscall.Kill(-group, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		// Darwin may return EPERM after the last eligible member exits. The
		// earlier metadata scan cannot certify the state at this signal.
		alive, inspectErr := processGroupAlive(group)
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if !alive {
			return nil
		}
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
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pgid=,stat=").Output()
	if err != nil {
		return false, fmt.Errorf("inspect owned Codex process group: %w", err)
	}
	expected := strconv.Itoa(group)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == expected && !strings.HasPrefix(fields[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}
