package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/review"
)

func TestServiceRetainsAllFrozenInputsWhileOwnedChildIsUnresolved(t *testing.T) {
	_, configPath, deps, durable := privateApplicationFixture(t, "UNRESOLVED-PRIVATE-WORKER")
	var child *exec.Cmd
	var scope, skill, sourceRoot string
	defer func() {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		_, _ = durable.RecoverPrivate(context.Background(), false)
	}()
	deps.Review = func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
		scope, skill = lifetime.Root(ctx, ""), request.Skill.Path
		entries, err := os.ReadDir(scope)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "reviewd-source-") {
				sourceRoot = filepath.Join(scope, entry.Name())
			}
		}
		child = exec.CommandContext(context.Background(), "/bin/sleep", "60")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if _, err := lifetime.Start(ctx, child); err != nil {
			t.Fatal(err)
		}
		return codex.ReviewOutput{}, errors.New("owned worker returned without proving child shutdown")
	}
	var output, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"--config", configPath, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}, &output, &diagnostics, deps)
	if code != 1 || child == nil || sourceRoot == "" {
		t.Fatalf("fixture did not reach unresolved worker teardown: %d %s", code, diagnostics.String())
	}
	for _, path := range []string{scope, skill, sourceRoot, filepath.Join(sourceRoot, "objects")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("service teardown deleted child-associated input %s: %v", filepath.Base(path), err)
		}
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("service takeover signalled the unresolved child")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	child = nil
	if _, err := durable.RecoverPrivate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scope); !os.IsNotExist(err) {
		t.Fatal("proven child exit did not make retained inputs reclaimable")
	}
}
