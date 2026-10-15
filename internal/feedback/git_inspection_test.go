package feedback

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/source"
)

// Runs only finite foreground Git queries in a task-owned fixture. Isolation
// itself is covered separately by workspace/backend acceptance.
type inspectionGitHost struct{ directory string }

func (h inspectionGitHost) Close() error { return nil }
func (h inspectionGitHost) Run(ctx context.Context, args []string, _ time.Duration) (codex.CommandResult, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = h.directory
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.directory, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
	data, err := cmd.CombinedOutput()
	result := codex.CommandResult{Stdout: string(data)}
	if exit, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

func TestExecutionGitInspectionDistinguishesIndexFromCurrentWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	host := inspectionGitHost{root}
	git := func(args ...string) string {
		t.Helper()
		result, err := host.Run(ctx, append([]string{"git"}, args...), time.Minute)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("owned Git fixture: %v %s", err, result.Stdout)
		}
		return strings.TrimSpace(result.Stdout)
	}
	git("init", "-q", "-b", "feature/existing", "--template=")
	file := filepath.Join(root, "source.txt")
	if err := os.WriteFile(file, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")
	blob := git("rev-parse", "HEAD:source.txt")
	if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "head")
	head := git("rev-parse", "HEAD")
	git("update-index", "--cacheinfo", "100755,"+blob+",source.txt")
	inspection, err := inspectGitCommands(ctx, host, "git", &source.Snapshot{HeadSHA: head, MergeBaseSHA: base}, "feature/existing")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Head != head || inspection.Branch != "refs/heads/feature/existing" || !strings.Contains(inspection.Index, "100755 "+blob+" 0\tsource.txt\x00") || !strings.Contains(inspection.StagedDiff, "+before") || !strings.Contains(inspection.StagedDiff, "-after") || !strings.Contains(inspection.ComparisonDiff, "+after") || !strings.Contains(inspection.Status, "source.txt\x00") {
		t.Fatal("actual index/current-source distinction was omitted or conflated")
	}
	git("symbolic-ref", "HEAD", "refs/heads/unexpected")
	if _, err := inspectGitCommands(ctx, host, "git", &source.Snapshot{HeadSHA: head, MergeBaseSHA: base}, "feature/existing"); err == nil {
		t.Fatal("changed branch retained original target certification")
	}
}
