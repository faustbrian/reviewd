//go:build darwin || linux

package watch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestReviewSlotsBoundAllProcessesAndReleaseOnExit(t *testing.T) {
	if directory := os.Getenv("REVIEWD_FIXTURE_SLOT"); directory != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		lease, err := AcquireSlot(ctx, directory, 1)
		if errors.Is(err, context.DeadlineExceeded) {
			os.Exit(23)
		}
		if err != nil {
			os.Exit(24)
		}
		_ = lease
		os.Exit(0)
	}
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := AcquireSlot(ctx, directory, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireSlot(ctx, directory, 2)
	if err != nil {
		t.Fatal(err)
	}
	child := func() error {
		command := exec.Command(os.Args[0], "-test.run=^TestReviewSlotsBoundAllProcessesAndReleaseOnExit$")
		command.Env = append(os.Environ(), "REVIEWD_FIXTURE_SLOT="+directory)
		return command.Run()
	}
	if err := child(); err == nil {
		t.Fatal("separate process exceeded global cap")
	}
	first.Close()
	// A newly lowered limit still counts a worker admitted under an older cap.
	if err := child(); err == nil {
		t.Fatal("lowered global cap ignored an existing higher-numbered slot")
	}
	second.Close()
	if err := child(); err != nil {
		t.Fatalf("released capacity unavailable: %v", err)
	}
	if err := child(); err != nil {
		t.Fatalf("process death stranded a slot: %v", err)
	}
}
