//go:build darwin || linux

package lifetime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExitObservationHoldsChildUntilExplicitReap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "/bin/sh", "-c", "/bin/sleep 0.2; kill -STOP $$; exit 7")
	Configure(child)
	if _, err := Start(ctx, child); err != nil {
		t.Fatal(err)
	}
	exited := Observe(child)
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = SignalGroup(child.Process.Pid, syscall.SIGKILL)
			_, _ = Reap(child, Observe(child))
		}
	})
	// CONT sent before the child's own STOP is ineffective. Observe the
	// stopped state first, including deliberately delayed child startup.
	for {
		state, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(child.Process.Pid), "-o", "stat=").Output()
		if err != nil {
			t.Fatalf("cannot establish the owned child's stopped state: %v", err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("owned child never entered its explicit stopped state")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-exited:
		t.Fatalf("stopped child was mistaken for an exited child: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := child.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("exit observation never completed")
	}
	state, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(child.Process.Pid), "-o", "ppid=,stat=").Output()
	fields := strings.Fields(string(state))
	if err != nil || len(fields) != 2 || fields[0] != strconv.Itoa(os.Getpid()) || !strings.HasPrefix(fields[1], "Z") {
		t.Fatalf("observation released the child's identity: %q, %v", state, err)
	}
	// Registration after exit must also complete, not miss an edge forever.
	again := Observe(child)
	commandErr, cleanupErr := Reap(child, again)
	reaped = true
	var exitErr *exec.ExitError
	if cleanupErr != nil || !errors.As(commandErr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("exclusive reap lost the actual exit status: %v, %v", commandErr, cleanupErr)
	}
}
