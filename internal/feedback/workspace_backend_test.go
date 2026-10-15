package feedback

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

type ownershipWorkspaceHost struct {
	root       string
	completion lifetime.Process
}

func (h *ownershipWorkspaceHost) Run(ctx context.Context, _ []string, _ time.Duration) (codex.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return codex.CommandResult{}, err
	}
	h.root = lifetime.Root(ctx, "unowned-runtime-storage")
	var err error
	h.completion, err = lifetime.PendingCompletion(ctx)
	return codex.CommandResult{}, err
}
func (*ownershipWorkspaceHost) Close() error { return nil }

// Inert adapter plus real durable ownership: no OS child or cancellation probe.
// Missing/foreign caller scope must never change workspace cleanup authority.
func TestWorkspaceCommandsKeepOriginalOwner(t *testing.T) {
	for _, caller := range []string{"background", "foreign-owner"} {
		t.Run(caller, func(t *testing.T) {
			ctx := context.Background()
			durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			owner, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer foreign.Close()
			var pending []lifetime.Process
			defer func() {
				for _, slot := range pending {
					_ = lifetime.Finish(slot)
				}
				_ = owner.Close()
				_, _ = durable.ReconcilePrivateOwner(ctx, owner.ID)
			}()
			callerCtx := ctx
			if caller == "foreign-owner" {
				callerCtx = foreign.Context(ctx)
			}
			database, err := sql.Open("sqlite", "file:"+filepath.Join(durable.Directory(), "reviewd.sqlite")+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			count := func(id string) int {
				t.Helper()
				var n int
				if err := database.QueryRow("SELECT count(*) FROM private_owners, json_each(processes) WHERE private_owners.id=?", id).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			probe := &ownershipWorkspaceHost{}
			host, _, _, err := openWorkspaceCommands(callerCtx, owner, func(startCtx context.Context) (CommandHost, CommandEnvironment, string, error) {
				root := lifetime.Root(startCtx, t.TempDir())
				scratch, err := os.MkdirTemp(root, "inert-runtime-")
				if err != nil {
					return nil, CommandEnvironment{}, "", err
				}
				defer os.RemoveAll(scratch)
				slot, err := lifetime.PendingCompletion(startCtx)
				pending = append(pending, slot)
				if err != nil {
					return nil, CommandEnvironment{}, "", err
				}
				if filepath.Dir(scratch) != owner.Root || count(owner.ID) != 1 || count(foreign.ID) != 0 {
					t.Error("startup storage/launch escaped original workspace owner")
				}
				if err := lifetime.Finish(slot); err != nil {
					return nil, CommandEnvironment{}, "", err
				}
				pending = nil
				return probe, CommandEnvironment{}, "git", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := host.Run(callerCtx, []string{"inert-command"}, time.Second); err != nil {
				t.Fatal(err)
			}
			pending = append(pending, probe.completion)
			if probe.root != owner.Root || count(owner.ID) != 1 || count(foreign.ID) != 0 {
				t.Fatal("command completion escaped original workspace owner")
			}
			if err := owner.Close(); err == nil {
				t.Fatal("unresolved command permitted original owner deletion")
			}
			if _, err := os.Stat(owner.Root); err != nil {
				t.Fatal("unresolved original owner storage was erased", err)
			}
			cancelled, cancel := context.WithCancel(callerCtx)
			cancel()
			_, _, _, err = openWorkspaceCommands(cancelled, owner, func(startCtx context.Context) (CommandHost, CommandEnvironment, string, error) {
				if lifetime.Root(startCtx, "unowned") != owner.Root {
					t.Error("cancelled startup lost original owner")
				}
				return nil, CommandEnvironment{}, "", startCtx.Err()
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatal("ownership binding lost startup cancellation", err)
			}
			if _, err := host.Run(cancelled, []string{"inert-command"}, time.Second); !errors.Is(err, context.Canceled) {
				t.Fatal("ownership binding lost caller cancellation", err)
			}
		})
	}
}

type uncertainWorkspaceHost struct{ failure error }

func (h *uncertainWorkspaceHost) Run(context.Context, []string, time.Duration) (codex.CommandResult, error) {
	return codex.CommandResult{}, errors.New("fixture must never be dispatched")
}
func (h *uncertainWorkspaceHost) Close() error { return h.failure }

func TestWorkspaceUncertaintyPreventsBackendReplacement(t *testing.T) {
	unknown := errors.New("owned fixture shutdown is unproven")
	host := &uncertainWorkspaceHost{failure: unknown}
	workspace := &Workspace{Directory: t.TempDir(), Commands: host}
	if err := workspace.Close(); !errors.Is(err, unknown) {
		t.Fatal("close lost prior uncertainty", err)
	}
	for _, writable := range []bool{false, true} {
		replacement, err := workspace.StartCommands(context.Background(), writable)
		if replacement != nil || !errors.Is(err, unknown) || workspace.Commands != host {
			t.Fatal("failed prior shutdown reconstructed or replaced its backend", err)
		}
	}
}

// MAC-01: missing container prerequisites must hold modifying checkout before
// even probing the old native command server. No real model/runtime is started.
func TestMacFeedbackCheckoutNeverFallsBackToNativeCommands(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS backend selection")
	}
	ctx := context.Background()
	original := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = original
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + original, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("owned Git fixture: %v %s", err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q", "-b", "main", "--template=")
	git("commit", "--allow-empty", "-qm", "owned empty fixture")
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/21/head", head)
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := source.Acquire(owner.Context(ctx), source.Options{RemoteURL: "file://" + original, AllowLocalFixture: true}, source.Boundary{TargetBaseSHA: head, HeadSHA: head, Number: 21})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "native-command-invoked")
	executable := filepath.Join(t.TempDir(), "inert-codex")
	script := "#!/bin/sh\nprintf invoked > '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\nexit 2\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	for _, options := range []WorkspaceOptions{
		{Runtime: codex.RuntimeOptions{Executable: executable}},
		{Runtime: codex.RuntimeOptions{Executable: executable}, ContainerEndpoint: "unix://" + filepath.ToSlash(filepath.Join(t.TempDir(), "missing.sock")), ContainerImage: "sha256:" + strings.Repeat("a", 64)},
	} {
		workspace, err := PrepareWorkspace(ctx, owner, snapshot, "feature/existing-pr", options)
		if workspace != nil {
			_ = workspace.Close()
		}
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "container") {
			t.Fatal("missing/unavailable container prerequisites did not produce an actionable hold", err)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatal("macOS modifying checkout reached native command execution", statErr)
		}
	}
}
