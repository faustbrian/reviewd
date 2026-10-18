//go:build darwin || linux

package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProfileLeaseIsProcessScopedAndRecoversAfterOwnerExit(t *testing.T) {
	if directory := os.Getenv("REVIEWD_FIXTURE_LOCK"); directory != "" {
		lease, err := LockProfile(directory)
		if err != nil {
			os.Exit(23)
		}
		_ = lease // Deliberately bypass defers to exercise process-death release.
		os.Exit(0)
	}
	directory := t.TempDir()
	lease, err := LockProfile(directory)
	if err != nil {
		t.Fatal(err)
	}
	child := func(directory string) error {
		command := exec.Command(os.Args[0], "-test.run=^TestProfileLeaseIsProcessScopedAndRecoversAfterOwnerExit$")
		command.Env = append(os.Environ(), "REVIEWD_FIXTURE_LOCK="+directory)
		return command.Run()
	}
	if err := child(directory); err == nil {
		t.Fatal("second process acquired the same profile")
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := child(other); err != nil {
		t.Fatalf("another profile was incorrectly blocked: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child(directory); err != nil {
		t.Fatalf("released profile stranded: %v", err)
	}
	// The helper exits while owning its descriptor. The kernel, not a stale
	// PID-file cleanup decision, makes the same profile available again.
	if err := child(directory); err != nil {
		t.Fatalf("exited process stranded profile: %v", err)
	}
}
