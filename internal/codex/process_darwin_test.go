//go:build darwin

package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// Hold the parent unreaped while a real metadata scan races its exit. Darwin
// returns EPERM for the resulting zombie-only group, not necessarily ESRCH.
func TestCloseRechecksDarwinGroupAfterLastLiveMemberExits(t *testing.T) {
	for _, state := range []string{"actual", "transient", "live", "unavailable"} {
		t.Run(state, func(t *testing.T) { closeDarwinGroupState(t, state) })
	}
}

func closeDarwinGroupState(t *testing.T, postState string) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	isolation := &Isolation{Root: private, ownedRoot: private}
	t.Cleanup(func() { _ = isolation.Close() })
	command := exec.CommandContext(context.Background(), "/bin/sleep", "60")
	configureProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	marker, gate := filepath.Join(root, "first-scan"), filepath.Join(root, "post-signal-scan")
	shim := `#!/bin/sh
set -eu
if [ ! -e "$REVIEWD_TEST_SCAN_MARKER" ]; then
  /bin/ps "$@" > "$REVIEWD_TEST_CAPTURE"
  : > "$REVIEWD_TEST_SCAN_MARKER"
  kill -TERM "$REVIEWD_TEST_TARGET_PID"
  ready=false
  for attempt in $(/usr/bin/seq 1 100); do
    state=$(/bin/ps -p "$REVIEWD_TEST_TARGET_PID" -o stat=)
    case "$state" in *Z*) ready=true; break;; esac
    /bin/sleep 0.01
  done
  [ "$ready" = true ]
else
  case "$REVIEWD_TEST_POST_STATE" in
    transient)
      if [ ! -e "$REVIEWD_TEST_SCAN_MARKER.transient" ]; then
        : > "$REVIEWD_TEST_SCAN_MARKER.transient"
        printf '%s S\n' "$REVIEWD_TEST_TARGET_PID" > "$REVIEWD_TEST_CAPTURE"
      else
        /bin/ps "$@" > "$REVIEWD_TEST_CAPTURE"
      fi;;
    live) printf '%s S\n' "$REVIEWD_TEST_TARGET_PID" > "$REVIEWD_TEST_CAPTURE";;
    unavailable) : > "$REVIEWD_TEST_POST_SCAN"; exit 1;;
    *) /bin/ps "$@" > "$REVIEWD_TEST_CAPTURE";;
  esac
  : > "$REVIEWD_TEST_POST_SCAN"
fi
/bin/cat "$REVIEWD_TEST_CAPTURE"
`
	if err := os.WriteFile(filepath.Join(root, "ps"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWD_TEST_POST_STATE", postState)
	t.Setenv("REVIEWD_TEST_TARGET_PID", strconv.Itoa(command.Process.Pid))
	t.Setenv("REVIEWD_TEST_SCAN_MARKER", marker)
	t.Setenv("REVIEWD_TEST_POST_SCAN", gate)
	t.Setenv("REVIEWD_TEST_CAPTURE", filepath.Join(root, "metadata"))
	parent, observed, finish := make(chan error, 1), make(chan struct{}), make(chan struct{})
	reapReady, releaseReap := make(chan struct{}), make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			select {
			case <-finish:
				goto reap
			case <-time.After(time.Millisecond):
			}
		}
	reap:
		close(reapReady)
		select {
		case <-releaseReap:
		case <-finish:
		}
		parent <- lifetime.WaitExited(command.Process.Pid)
		close(observed)
	}()
	defer func() { close(finish); <-observed }()
	r := &Runtime{command: command, isolation: isolation, wait: make(chan error, 1), stop: make(chan struct{})}
	go func() { r.wait <- superviseProcess(command, parent, r.stop) }()
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	if postState == "actual" || postState == "transient" {
		select {
		case <-reapReady:
		case err := <-closed:
			t.Fatalf("cleanup finished without parent reap: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("owned process did not reach verified-dead scan")
		}
		select {
		case err := <-closed:
			t.Fatalf("cleanup bypassed held parent reap: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := os.Stat(private); err != nil {
			t.Fatal("private state removed before parent completion")
		}
		close(releaseReap)
	} else {
		close(releaseReap)
	}
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-time.After(8 * time.Second):
		t.Fatal("cleanup did not return")
	}
	if postState != "actual" && postState != "transient" {
		if !errors.Is(closeErr, syscall.EPERM) {
			t.Fatalf("uncertain/denied liveness lost signal failure: %v", closeErr)
		}
		if _, err := os.Stat(private); err != nil {
			t.Fatal("uncertain cleanup deleted private state")
		}
		return
	}
	if closeErr != nil {
		t.Fatalf("exit between metadata and signal prevented safe cleanup: %v", closeErr)
	}
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatal("verified-dead runtime retained private state")
	}
}

// Every signal edge in the runtime supervisor shares this boundary, including
// escalation and deferred KILL. A held zombie guarantees a real Darwin EPERM;
// only metadata is controlled to exercise transient versus uncertain liveness.
func TestDarwinSignalsRequirePostDenialDeathProof(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, state := range []string{"transient", "live", "unavailable"} {
			t.Run(strconv.Itoa(int(signal))+"/"+state, func(t *testing.T) {
				command := exec.CommandContext(context.Background(), "/bin/sleep", "0.05")
				configureProcess(command)
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				defer command.Wait()
				if err := lifetime.WaitExited(command.Process.Pid); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(-command.Process.Pid, 0); !errors.Is(err, syscall.EPERM) {
					t.Fatalf("held owned zombie did not produce Darwin denial: %v", err)
				}
				root := t.TempDir()
				script := `#!/bin/sh
set -eu
case "$REVIEWD_TEST_DENIAL_MODE" in
 unavailable) exit 1;;
 live) printf '%s S\n' "$REVIEWD_TEST_DENIAL_PID"; exit 0;;
esac
if [ ! -e "$REVIEWD_TEST_DENIAL_MARKER" ]; then
 : > "$REVIEWD_TEST_DENIAL_MARKER"
 printf '%s S\n' "$REVIEWD_TEST_DENIAL_PID"
else
 exec /bin/ps "$@"
fi
`
				if err := os.WriteFile(filepath.Join(root, "ps"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", root)
				t.Setenv("REVIEWD_TEST_DENIAL_MODE", state)
				t.Setenv("REVIEWD_TEST_DENIAL_PID", strconv.Itoa(command.Process.Pid))
				t.Setenv("REVIEWD_TEST_DENIAL_MARKER", filepath.Join(root, "observed"))
				err := signalProcessGroup(command.Process.Pid, signal)
				if state == "transient" {
					if err != nil {
						t.Fatalf("proven subsequent shutdown retained signal denial: %v", err)
					}
				} else if !errors.Is(err, syscall.EPERM) {
					t.Fatalf("live or unknown liveness lost signal denial: %v", err)
				}
			})
		}
	}
}
