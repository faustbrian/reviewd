package codex

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCommandExecutableRetainsOnlySelectedStandaloneFile(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "selected")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	actual, err := commandExecutable(context.Background(), link)
	if err != nil || actual != binary {
		t.Fatalf("selected canonical executable lost: %q %v", actual, err)
	}
}

func TestCommandExecutableResolvesOfficialNpmNativeHelper(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node prerequisite for npm packaging unavailable")
	}
	probe, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	identity := exec.CommandContext(probe, node, "-p", `process.platform+"/"+process.arch`)
	identity.Env = []string{"PATH=" + os.Getenv("PATH")}
	output, err := identity.Output()
	if err != nil {
		t.Fatal(err)
	}
	var triple, platform string
	switch strings.TrimSpace(string(output)) {
	case "linux/x64":
		triple, platform = "x86_64-unknown-linux-musl", "linux-x64"
	case "linux/arm64":
		triple, platform = "aarch64-unknown-linux-musl", "linux-arm64"
	case "darwin/x64":
		triple, platform = "x86_64-apple-darwin", "darwin-x64"
	case "darwin/arm64":
		triple, platform = "aarch64-apple-darwin", "darwin-arm64"
	default:
		t.Fatal("unsupported Node fixture platform")
	}
	for _, kind := range []string{"optional sibling", "bundled fallback", "missing native"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			pkg := filepath.Join(root, "node_modules", "@openai", "codex")
			if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex","version":"0.162.1","bin":{"codex":"bin/codex.js"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			launcher := filepath.Join(pkg, "bin", "codex.js")
			if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\nthrow new Error('launcher must not execute during resolution');\n"), 0700); err != nil {
				t.Fatal(err)
			}
			vendor := filepath.Join(pkg, "vendor")
			if kind == "optional sibling" {
				sibling := filepath.Join(root, "node_modules", "@openai", "codex-"+platform)
				if err := os.MkdirAll(sibling, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sibling, "package.json"), []byte(`{"name":"owned-native-fixture","version":"0.162.1"}`), 0600); err != nil {
					t.Fatal(err)
				}
				vendor = filepath.Join(sibling, "vendor")
			}
			native := filepath.Join(vendor, triple, "bin", "codex")
			if kind != "missing native" {
				if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(native, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			selected := filepath.Join(root, "codex")
			if err := os.Symlink(launcher, selected); err != nil {
				t.Fatal(err)
			}
			actual, err := commandExecutable(context.Background(), selected)
			if kind == "missing native" {
				if err == nil {
					t.Fatal("missing npm native helper accepted")
				}
				return
			}
			if err != nil || actual != native {
				t.Fatalf("wrong native helper: %q %v", actual, err)
			}
		})
	}
}

func TestCodexCapabilityBoundarySelectedExecutable(t *testing.T) {
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for native executable isolation")
	}
	native, err := commandExecutable(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(native)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	root, err := os.MkdirTemp("", "reviewd-native-executable-")
	if err != nil {
		t.Fatal(err)
	}
	stopped := true
	defer func() {
		if stopped {
			if err := os.RemoveAll(root); err != nil {
				t.Error(err)
			}
		} else {
			t.Logf("unproven native runtime shutdown; owned fixture retained: %s", root)
		}
	}()
	selected, secret, workspace := filepath.Join(root, "codex"), filepath.Join(root, "adjacent-credential"), filepath.Join(root, "workspace")
	output, err := os.OpenFile(selected, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal("cannot prepare owned native executable")
	}
	if err := os.WriteFile(secret, []byte("owned-adjacent-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, readRoots := range [][]string{nil, {selected}} {
		stopped = false
		host, err := StartCommandRuntime(ctx, CommandOptions{RuntimeOptions: RuntimeOptions{Executable: selected, TempRoot: root}, Workspace: workspace, ReadRoots: readRoots})
		if err != nil {
			t.Fatal(err)
		}
		version, versionErr := host.Run(ctx, []string{selected, "--version"}, 20*time.Second)
		denied, deniedErr := host.Run(ctx, []string{"/bin/cat", secret}, 5*time.Second)
		if err := host.Close(); err != nil {
			t.Fatal(err)
		}
		stopped = true
		if versionErr != nil || version.ExitCode != 0 || !strings.Contains(version.Stdout, "codex-cli 0.162.1") {
			t.Fatalf("selected native executable hidden: %+v %v", version, versionErr)
		}
		if deniedErr != nil || denied.ExitCode == 0 || denied.ExitCode == 124 || strings.Contains(denied.Stdout, "owned-adjacent-credential") {
			t.Fatalf("adjacent credential boundary failed: %+v %v", denied, deniedErr)
		}
	}
}

func TestCommandExecutableUsesSelectedNodeArchitecture(t *testing.T) {
	realNode, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	realNode, err = filepath.Abs(realNode)
	if err != nil {
		t.Fatal(err)
	}
	arch, triple, platform := "x64", "x86_64-unknown-linux-musl", "linux-x64"
	if runtime.GOARCH == "amd64" {
		arch, triple, platform = "arm64", "aarch64-unknown-linux-musl", "linux-arm64"
	}
	if runtime.GOOS == "darwin" {
		triple, platform = "x86_64-apple-darwin", "darwin-x64"
		if arch == "arm64" {
			triple, platform = "aarch64-apple-darwin", "darwin-arm64"
		}
	}
	for _, bundled := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "bundled"}[bundled], func(t *testing.T) {
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0700); err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			// Use real Node with a controlled platform identity; do not execute a
			// foreign-architecture binary or replace resolution with canned output.
			prefix := `Object.defineProperty(process,"arch",{value:"` + arch + `"});Object.defineProperty(process,"platform",{value:"` + runtime.GOOS + `"});`
			wrapper := "#!/bin/sh\nset -eu\n[ \"$1\" = -e ] || exit 91\nprogram=$2\nshift 2\nexec " + quote(realNode) + " -e " + quote(prefix) + "\"$program\" \"$@\"\n"
			if err := os.WriteFile(filepath.Join(tools, "node"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			pkg := filepath.Join(root, "node_modules", "@openai", "codex")
			if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@openai/codex","bin":{"codex":"bin/codex.js"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			launcher := filepath.Join(pkg, "bin", "codex.js")
			if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\nthrow new Error('launcher must not execute');\n"), 0700); err != nil {
				t.Fatal(err)
			}
			vendor := filepath.Join(pkg, "vendor")
			if !bundled {
				sibling := filepath.Join(root, "node_modules", "@openai", "codex-"+platform)
				if err := os.MkdirAll(sibling, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sibling, "package.json"), []byte(`{"name":"owned-native-fixture"}`), 0600); err != nil {
					t.Fatal(err)
				}
				vendor = filepath.Join(sibling, "vendor")
			}
			expected := filepath.Join(vendor, triple, "bin", "codex")
			if err := os.MkdirAll(filepath.Dir(expected), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(expected, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			actual, err := commandExecutable(context.Background(), launcher)
			if err != nil || actual != expected {
				t.Fatalf("selected Node %s disagrees with Go %s: %q %v", arch, runtime.GOARCH, actual, err)
			}
		})
	}
}
