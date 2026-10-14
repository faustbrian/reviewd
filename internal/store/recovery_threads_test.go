package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// Linux's process-only ps reports Zl for an exited leader with live sibling
// threads. Inject that valid producer status while the actual owned child is
// live: the observable contract is preservation of its real private storage.
func TestRecoveryPreservesPrivateStorageForZombieLeaderWithLiveThreads(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if _, err := lifetime.Start(owner.Context(ctx), child); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	canary := filepath.Join(owner.Root, "live-thread-private-input")
	if err := os.WriteFile(canary, []byte("owned private input"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s Zl\\n' '%d'\n", child.Process.Pid)
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || !report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
		t.Errorf("live sibling metadata was classified as stopped: %+v %v", report, err)
	}
	if err := syscall.Kill(child.Process.Pid, 0); err != nil {
		t.Fatalf("recovery interfered with the live child: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("live-thread metadata erased actual private storage: %v", err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed || current.OwnerClosed {
		t.Fatalf("lost-owner work or private shutdown acknowledgement is incorrect: %+v %v", current, err)
	}
}
