//go:build darwin || linux

package lifetime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// These are valid Linux producer representations. Exercise the shared full-
// scan decoder on either supported OS without claiming native Linux execution.
func TestFullScanPreservesLinuxKernelAndThreadSemantics(t *testing.T) {
	for _, fixture := range []struct {
		name, output  string
		live, invalid bool
	}{
		{"kernel-and-live", "0 S\n123 S\n", true, false},
		{"kernel-and-stopped", "0 S\n123 Z\n", false, false},
		{"zombie-leader-live-threads", "123 Zl\n", true, false},
		{"zombie-foreground-live-threads", "123 Zl+\n", true, false},
		{"ordinary-zombie", "123 Z+\n", false, false},
		{"kernel-only-is-incomplete", "0 S\n", false, true},
		{"negative-group", "-1 S\n123 Z\n", false, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nprintf '%s' '"+fixture.output+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			groups, err := LiveGroups(context.Background())
			if fixture.invalid {
				if err == nil {
					t.Fatal("invalid or incomplete process metadata was accepted")
				}
				return
			}
			if err != nil || groups[123] != fixture.live || groups[0] {
				t.Fatalf("valid Linux metadata lost group liveness: live=%t want=%t kernel=%t error=%v", groups[123], fixture.live, groups[0], err)
			}
		})
	}
}
