package feedback

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/source"
)

// Actual read-only Git results, not a model's description of staged changes.
// NUL-separated index/status names retain their exact meaning.
type GitInspection struct {
	Head           string `json:"head"`
	Branch         string `json:"branch"`
	Index          string `json:"index"`
	Status         string `json:"status"`
	StagedDiff     string `json:"staged_diff"`
	ComparisonDiff string `json:"comparison_diff"`
}

func inspectWorkspaceGit(ctx context.Context, workspace *Workspace, snapshot *source.Snapshot, branch string) (_ GitInspection, err error) {
	var result GitInspection
	host, err := workspace.StartCommands(ctx, false)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, workspace.Close()) }()
	expectedHead := snapshot.HeadSHA
	if workspace.options.inspectionHead != "" {
		expectedHead = workspace.options.inspectionHead
	}
	return inspectGitCommandsAtHead(ctx, host, workspace.Git, snapshot, branch, expectedHead)
}

func inspectGitCommands(ctx context.Context, host CommandHost, git string, snapshot *source.Snapshot, branch string) (GitInspection, error) {
	return inspectGitCommandsAtHead(ctx, host, git, snapshot, branch, snapshot.HeadSHA)
}

func inspectGitCommandsAtHead(ctx context.Context, host CommandHost, git string, snapshot *source.Snapshot, branch, expectedHead string) (GitInspection, error) {
	var result GitInspection
	queries := []struct {
		arguments []string
		output    *string
	}{
		{[]string{"rev-parse", "--verify", "HEAD"}, &result.Head},
		{[]string{"symbolic-ref", "HEAD"}, &result.Branch},
		{[]string{"ls-files", "--stage", "--full-name", "-z"}, &result.Index},
		{[]string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, &result.Status},
		{[]string{"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", "HEAD", "--"}, &result.StagedDiff},
		{[]string{"diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", snapshot.MergeBaseSHA, "--"}, &result.ComparisonDiff},
	}
	for _, query := range queries {
		observation, runErr := host.Run(ctx, append([]string{git, "--no-pager", "--no-replace-objects", "--git-dir=.git", "--work-tree=."}, query.arguments...), time.Minute)
		if runErr != nil {
			return result, runErr
		}
		if observation.ExitCode != 0 {
			return result, errors.New("complete isolated Git index/comparison inspection failed; preserve pending work")
		}
		*query.output = observation.Stdout
	}
	result.Head = strings.TrimSpace(result.Head)
	result.Branch = strings.TrimSpace(result.Branch)
	if result.Head != expectedHead || result.Branch != "refs/heads/"+branch {
		return result, errors.New("feedback commands changed the existing immutable parent or branch before scoped delivery")
	}
	return result, nil
}
