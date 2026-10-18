package lifetime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Inspecting one owned group must not depend on collecting unrelated groups.
// A full-host inspector can be unavailable while targeted metadata is usable.
func TestOwnedGroupInspectionDoesNotRequireFullHostScan(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"-axo\" ]; then exec /bin/sleep 5; fi\nexec /bin/ps \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	alive, err := GroupAlive(ctx, child.Process.Pid)
	if err != nil || !alive {
		t.Fatalf("targeted live owned group depended on full-host inspection: alive=%t error=%v", alive, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := WaitExited(child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	alive, err = GroupAlive(ctx, child.Process.Pid)
	if err != nil || alive {
		t.Fatalf("held zombie-only group is not stopped: alive=%t error=%v", alive, err)
	}
}

type exitedDarwinTracker struct{}

func (exitedDarwinTracker) Starting(context.Context) (Process, error) {
	return exitedDarwinTracker{}, nil
}
func (exitedDarwinTracker) Finished(context.Context) error { return nil }
func (exitedDarwinTracker) Started(ctx context.Context, pid int) error {
	// Hold the actual child unreaped and wait until Darwin's denied-signal
	// zombie case is certain, rather than depending on an exit timing race.
	for {
		output, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil {
			return err
		}
		if strings.HasPrefix(strings.TrimSpace(string(output)), "Z") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRunAcceptsProvenShutdownAfterTransientDarwinSignalDenial(t *testing.T) {
	bin := t.TempDir()
	// The first post-denial metadata observation is transiently live; the
	// actual subsequent inspection proves that the held group is stopped.
	script := "#!/bin/sh\nif [ ! -e \"$REVIEWD_TEST_FIRST_SCAN\" ]; then\n  : > \"$REVIEWD_TEST_FIRST_SCAN\"\n  printf '%s S\\n' \"$2\"\nelse\n  exec /bin/ps \"$@\"\nfi\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	marker := filepath.Join(bin, "first-scan")
	t.Setenv("REVIEWD_TEST_FIRST_SCAN", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = WithScope(ctx, Scope{Tracker: exitedDarwinTracker{}})
	if err := Run(ctx, exec.CommandContext(ctx, "/usr/bin/true")); err != nil {
		t.Fatalf("already-proven shutdown retained a transient signal failure: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("Darwin denied-signal inspection was not exercised")
	}
}
