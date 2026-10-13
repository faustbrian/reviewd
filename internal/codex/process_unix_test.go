//go:build darwin || linux

package codex

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

func TestCloseStopsDescendantAfterParentExit(t *testing.T) {
	for _, ignoreTERM := range []bool{false, true} {
		t.Run(strconv.FormatBool(ignoreTERM), func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			script := `sleep 60 </dev/null >/dev/null 2>&1 & echo $! > "$1"; exit 0`
			if ignoreTERM {
				script = `sh -c 'trap "" TERM; exec sleep 60' </dev/null >/dev/null 2>&1 & echo $! > "$1"; exit 0`
			}
			command := exec.CommandContext(context.Background(), "sh", "-c", script, "reviewd-test", pidFile)
			configureProcess(command)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			// Give the child time to install its inherited TERM policy.
			time.Sleep(50 * time.Millisecond)
			r := &Runtime{command: command, wait: make(chan error, 1), stop: make(chan struct{})}
			parent := make(chan error, 1)
			parent <- nil
			go func() { r.wait <- superviseProcess(command, parent, r.stop) }()
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for {
				output, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
				if ctx.Err() != nil {
					t.Fatalf("Close returned with live owned child %d", pid)
				}
				if err != nil || strings.HasPrefix(strings.TrimSpace(string(output)), "Z") {
					return
				}
				if ctx.Err() != nil {
					t.Fatalf("Close returned with live owned child %d", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
