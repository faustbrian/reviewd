//go:build darwin || linux

package codex

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A failed inspector cannot authorize removal of a still-live runtime's root.
func TestProcessInspectionNeverTreatsMalformedMetadataAsDead(t *testing.T) {
	for _, output := range []string{"malformed-process-metadata", ""} {
		t.Run(output, func(t *testing.T) {
			bin := t.TempDir()
			text := "#!/bin/sh\nprintf '%s' '" + output + "'\n"
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(text), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			alive, err := processGroupAlive(syscall.Getpgrp())
			if err == nil || alive {
				t.Fatalf("malformed inspection was accepted as death: alive=%t error=%v", alive, err)
			}
		})
	}
}

func TestKernelProvenGroupAbsenceDoesNotDependOnProcessTableInspection(t *testing.T) {
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-child.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned fixture group is not provably absent: %v", err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	alive, err := processGroupAlive(child.Process.Pid)
	if err != nil || alive {
		t.Fatalf("kernel-proven absence unnecessarily depends on unavailable inspection: alive=%t error=%v", alive, err)
	}
}
