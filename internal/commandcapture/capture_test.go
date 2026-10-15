package commandcapture

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func captureFixture(t *testing.T, out, diagnostic []byte, exit int) (string, []byte, Request) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x73}, 32)
	request := Request{Nonce: strings.Repeat("a", 32), Argv: []string{"fixture-command", "literal space"}}
	session, err := New(directory, key, request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(session.Stdout, struct{ io.Reader }{bytes.NewReader(out)})
	_, _ = io.Copy(session.Stderr, struct{ io.Reader }{bytes.NewReader(diagnostic)})
	if err := session.Finish(exit, true); err != nil {
		t.Fatal(err)
	}
	return directory, key, request
}

func TestCapturedOutputIsExactSeparateAndExplicitlyTerminal(t *testing.T) {
	for _, exit := range []int{0, 7} {
		for _, out := range [][]byte{nil, []byte("literal\x00 output\n你好\n")} {
			diagnostic := []byte("separate error\né\n")
			directory, key, request := captureFixture(t, out, diagnostic, exit)
			result, err := Read(directory, key, request, exit)
			if err != nil || result.Stdout != string(out) || result.Stderr != string(diagnostic) || result.ExitCode != exit {
				t.Fatalf("exact terminal capture lost: %+v %v", result, err)
			}
			if _, err := Read(directory, key, request, (exit+1)%256); err == nil {
				t.Fatal("conflicting canonical exit accepted")
			}
		}
	}
}

func TestOutputLimitsNeverCertifyTruncatedPrefixes(t *testing.T) {
	for _, count := range []int{Limit - 1, Limit, Limit + 1} {
		for _, stream := range []string{"stdout", "stderr", "both"} {
			t.Run(stream+"/"+strconv.Itoa(count), func(t *testing.T) {
				payload := bytes.Repeat([]byte("x"), count)
				var out, diagnostic []byte
				if stream != "stderr" {
					out = payload
				}
				if stream != "stdout" {
					diagnostic = payload
				}
				directory, key, request := captureFixture(t, out, diagnostic, 0)
				for _, name := range []string{"stdout", "stderr"} {
					info, err := os.Stat(filepath.Join(directory, name))
					if err != nil || info.Size() > int64(Limit) {
						t.Fatal("capture grew past the physical stream cap", err)
					}
				}
				result, err := Read(directory, key, request, 0)
				if count > Limit {
					if err == nil || result.Stdout != "" || result.Stderr != "" {
						t.Fatal("overflow certified a prefix", err)
					}
					return
				}
				if err != nil || result.Stdout != string(out) || result.Stderr != string(diagnostic) {
					t.Fatal("valid limit rejected or changed", err)
				}
			})
		}
	}
}

func TestHostRejectsUntrustedOrIncompleteCaptureWithoutDisclosingOutsideData(t *testing.T) {
	for _, mode := range []string{"changed-output", "stale-request", "wrong-key", "missing-output", "missing-result", "malformed-result", "oversized", "symlink", "fifo", "invalid-utf8"} {
		t.Run(mode, func(t *testing.T) {
			directory, key, request := captureFixture(t, []byte("literal output"), nil, 0)
			outside := filepath.Join(t.TempDir(), "outside")
			const secret = "outside fixture must never escape"
			if err := os.WriteFile(outside, []byte(secret), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(directory, "stdout")
			switch mode {
			case "changed-output":
				_ = os.WriteFile(output, []byte("forged output"), 0600)
			case "stale-request":
				request.Nonce = strings.Repeat("b", 32)
			case "wrong-key":
				key = bytes.Repeat([]byte{0x74}, 32)
			case "missing-output":
				_ = os.Remove(output)
			case "missing-result":
				_ = os.Remove(filepath.Join(directory, "result.json"))
			case "malformed-result":
				_ = os.WriteFile(filepath.Join(directory, "result.json"), []byte(`{"exit_code":0}`), 0600)
			case "oversized":
				_ = os.Truncate(output, Limit+1)
			case "symlink":
				_ = os.Remove(output)
				if err := os.Symlink(outside, output); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				_ = os.Remove(output)
				if err := syscall.Mkfifo(output, 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-utf8":
				directory, key, request = captureFixture(t, []byte{0xff, 0xfe}, nil, 0)
			}
			result, err := Read(directory, key, request, 0)
			if err == nil || result.Stdout != "" || result.Stderr != "" || strings.Contains(err.Error(), secret) {
				t.Fatalf("untrusted capture escaped: %+v %v", result, err)
			}
		})
	}
}

func TestIncompleteProducerAndHostInvocationNeverBecomeSuccess(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x73}, 32)
	request := Request{Nonce: strings.Repeat("a", 32), Argv: []string{"fixture-command"}}
	session, err := New(directory, key, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Finish(7, false); err != nil {
		t.Fatal(err)
	}
	if result, err := Read(directory, key, request, 7); err == nil || result.Stdout != "" {
		t.Fatal("incomplete nonzero result accepted", err)
	}
	if os.Getpid() != 1 {
		request.Argv = []string{"/bin/sh", "-c", "printf must-not-run >\"$1\"", "owned", filepath.Join(directory, "not-started")}
		if exit, err := Execute(directory, key, request); exit != 125 || err == nil {
			t.Fatal("container helper executed on the host", exit, err)
		}
		if _, err := os.Stat(filepath.Join(directory, "not-started")); !os.IsNotExist(err) {
			t.Fatal("host child was launched")
		}
	}
}
