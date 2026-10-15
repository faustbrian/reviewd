package codex

import (
	"context"
	"fmt"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexCapabilityBoundaryFeedbackCommands(t *testing.T) {
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for native command isolation")
	}
	t.Setenv("OPENAI_API_KEY", "owned-provider-canary")
	t.Setenv("GH_TOKEN", "owned-github-canary")
	root := t.TempDir()
	secret := filepath.Join(root, "outside-credential")
	if err := os.WriteFile(secret, []byte("owned-credential-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	git := "/usr/bin/git"
	if runtime.GOOS == "darwin" {
		// /usr/bin/git is an xcrun shim which writes per-user discovery caches.
		// Probe actual Git behavior, not shim/cache or arbitrary PATH discovery.
		git = ""
		for _, candidate := range []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				git = candidate
				break
			}
		}
		if git == "" {
			t.Fatal("native probe requires an installed Apple Git executable, not its /usr/bin/git discovery shim")
		}
	}
	init := exec.Command(git, "-c", "init.defaultBranch=feedback", "init", workspace)
	init.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("owned Git initialization: %v %s", err, output)
	}
	original := filepath.Join(workspace, "source")
	if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "owned-network-canary")
	}))
	defer server.Close()
	// Prove that the fixture and actual curl executable work outside isolation;
	// a missing binary or unreachable listener cannot pass the denial oracle.
	curl := "/usr/bin/curl"
	arguments := []string{"--disable", "--noproxy", "*", "--connect-timeout", "2", "--max-time", "3", "--fail", "--silent", "--show-error", server.URL}
	control := exec.CommandContext(ctx, curl, arguments...)
	if output, err := control.Output(); err != nil || string(output) != "owned-network-canary" || requests.Load() != 1 {
		t.Fatalf("owned network fixture control failed: %v", err)
	}
	for _, writable := range []bool{false, true} {
		host, err := StartCommandRuntime(ctx, CommandOptions{RuntimeOptions: RuntimeOptions{TempRoot: root}, Workspace: workspace, Writable: writable, ReadRoots: []string{git}})
		if err != nil {
			t.Fatal(err)
		}
		type operation struct {
			name     string
			argv     []string
			expected string
		}
		writeWant, indexWant := "WRITE_DENIED", "INDEX_DENIED"
		if writable {
			writeWant, indexWant = "WRITE_ALLOWED", "INDEX_ALLOWED"
		}
		operations := []operation{
			{"read", []string{"/bin/sh", "-c", `if cat "$1" 2>/dev/null; then echo CREDENTIAL_LEAK; else echo READ_DENIED; fi`, "probe", secret}, "READ_DENIED"},
			{"write", []string{"/bin/sh", "-c", `if printf changed > source 2>/dev/null; then echo WRITE_ALLOWED; else echo WRITE_DENIED; fi`}, writeWant},
			{"git executable", []string{git, "--version"}, "git version "},
			{"index", []string{"/bin/sh", "-c", `if "$1" add source 2>/dev/null; then echo INDEX_ALLOWED; else echo INDEX_DENIED; fi`, "probe", git}, indexWant},
			{"environment", []string{"/bin/sh", "-c", `if test -n "$OPENAI_API_KEY$GH_TOKEN$CODEX_HOME$SSH_AUTH_SOCK"; then echo ENV_LEAK; else echo ENV_CLEAN; fi`}, "ENV_CLEAN"},
		}
		results := make([]CommandResult, len(operations))
		failures := make([]error, len(operations))
		for i, operation := range operations {
			started := time.Now()
			results[i], failures[i] = host.Run(ctx, operation.argv, 20*time.Second)
			t.Logf("native operation %q writable=%t duration=%s exit=%d", operation.name, writable, time.Since(started), results[i].ExitCode)
			if failures[i] != nil || results[i].ExitCode != 0 {
				break
			}
		}
		version, versionErr := host.Run(ctx, []string{curl, "--disable", "--version"}, 5*time.Second)
		network, networkErr := host.Run(ctx, append([]string{curl}, arguments...), 5*time.Second)
		if closeErr := host.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		for i, operation := range operations {
			result := results[i]
			if failures[i] != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, operation.expected) || strings.Contains(result.Stdout, "LEAK") {
				t.Fatalf("native %s boundary failed writable=%t: %+v %v", operation.name, writable, result, failures[i])
			}
		}
		if versionErr != nil || version.ExitCode != 0 || !strings.Contains(version.Stdout, "curl ") {
			t.Fatalf("native network-probe executable unavailable: %+v %v", version, versionErr)
		}
		if networkErr != nil || (network.ExitCode != 7 && network.ExitCode != 28) || strings.Contains(network.Stdout, "owned-network-canary") || requests.Load() != 1 {
			t.Fatalf("native network denial failed: %+v %v requests=%d", network, networkErr, requests.Load())
		}
		// Inspect effects independently of shell result markers. A failed Git
		// command for an unrelated reason cannot prove the index contract.
		wantSource := "original"
		if writable {
			wantSource = "changed"
		}
		if data, err := os.ReadFile(original); err != nil || string(data) != wantSource {
			t.Fatalf("unexpected source effect writable=%t: %v", writable, err)
		}
		inspect := exec.Command(git, "-C", workspace, "ls-files", "--stage")
		inspect.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		index, err := inspect.Output()
		if err != nil || strings.Contains(string(index), "\tsource\n") != writable {
			t.Fatalf("unexpected index effect writable=%t: %v", writable, err)
		}

	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "owned-credential-canary" {
		t.Fatal("outside credential changed")
	}
}

func TestFeedbackCommandSetupDoesNotFollowRepositoryScratchLink(t *testing.T) {
	root := t.TempDir()
	workspace, outside := filepath.Join(root, "workspace"), filepath.Join(root, "outside")
	for _, path := range []string{workspace, outside} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".reviewd-work")); err != nil {
		t.Fatal(err)
	}
	_, err := StartCommandRuntime(context.Background(), CommandOptions{RuntimeOptions: RuntimeOptions{Executable: filepath.Join(root, "missing-codex")}, Workspace: workspace})
	if err == nil {
		t.Fatal("missing executable should fail")
	}
	if _, err := os.Lstat(filepath.Join(outside, "tmp")); !os.IsNotExist(err) {
		t.Fatal("repository scratch symlink caused a host write outside the workspace")
	}
}

func TestFeedbackCommandSetupRejectsExternalGitMetadata(t *testing.T) {
	for _, gitfile := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "gitfile"}[gitfile], func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workspace, ".git")
			var err error
			if gitfile {
				err = os.WriteFile(path, []byte("gitdir: ../external\n"), 0600)
			} else {
				err = os.Symlink(root, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = StartCommandRuntime(context.Background(), CommandOptions{Workspace: workspace, Writable: true})
			if err == nil || !strings.Contains(err.Error(), "isolated directory") {
				t.Fatalf("external Git metadata accepted: %v", err)
			}
		})
	}
}

func TestFeedbackCommandInitializationPreservesRuntimeOwnedRetention(t *testing.T) {
	root := t.TempDir()
	workspace, storage, tools := filepath.Join(root, "workspace"), filepath.Join(root, "runtimes"), filepath.Join(root, "tools")
	for _, path := range []string{workspace, storage, tools} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	pidPath := filepath.Join(root, "owned.pid")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	executable := filepath.Join(tools, "reject-init")
	script := "#!/bin/sh\necho $$ > " + quote(pidPath) + "\nprintf 'invalid-json\\n'\nexec /bin/sleep 60\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte("#!/bin/sh\nprintf 'invalid-metadata\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+originalPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := StartCommandRuntime(ctx, CommandOptions{RuntimeOptions: RuntimeOptions{Executable: executable, TempRoot: storage}, Workspace: workspace})
	// The error path must terminate/reap its actual owned child, but a failed
	// metadata proof still retains the root until independent reconciliation.
	t.Setenv("PATH", originalPath)
	pidData, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if alive, inspectErr := lifetime.GroupAlive(ctx, pid); inspectErr != nil || alive {
		t.Fatalf("owned fixture process shutdown unproven: alive=%t err=%v", alive, inspectErr)
	}
	if err == nil || !strings.Contains(err.Error(), "private runtime retained") {
		t.Fatalf("lost initialization cleanup uncertainty: %v", err)
	}
	entries, readErr := os.ReadDir(storage)
	if readErr != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "reviewd-commands-") {
		t.Fatalf("outer constructor deleted runtime-owned retained isolation: entries=%v err=%v", entries, readErr)
	}
}
