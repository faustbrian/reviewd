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

type feedbackCommitter interface {
	CommitFeedback(context.Context, *source.Snapshot, CommitPolicy, string) (CommittedFeedback, error)
}

func TestFeedbackCommitRunsNormalHooksAndScopedSigning(t *testing.T) {
	for _, scenario := range []string{"toolchain-without-bin-test", "signed", "hook-rejected", "hook-source-change", "hook-unavailable", "pre-push-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			host := inspectionGitHost{root}
			run := func(args ...string) string {
				t.Helper()
				result, err := host.Run(context.Background(), args, time.Minute)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("owned commit fixture: %v %s", err, result.Stderr)
				}
				return strings.TrimSpace(result.Stdout)
			}
			run("git", "init", "-q", "-b", "feature/existing", "--template=")
			os.WriteFile(filepath.Join(root, "source.txt"), []byte("before\n"), 0600)
			run("git", "add", "source.txt")
			run("git", "commit", "-qm", "base")
			parent := run("git", "rev-parse", "HEAD")
			os.WriteFile(filepath.Join(root, "source.txt"), []byte("fixed\n"), 0600)
			run("git", "add", "source.txt")
			hooks := filepath.Join(root, ".git", "hooks")
			os.MkdirAll(hooks, 0700)
			for _, name := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "pre-push"} {
				body := "#!/bin/sh\nprintf '%s\\n' '" + name + "' >> .git/hook-observations\n"
				if name == "commit-msg" && scenario == "hook-rejected" {
					body += "exit 19\n"
				}
				if name == "pre-commit" && scenario == "hook-source-change" {
					body += "printf 'hook changed source\\n' > source.txt\ngit add -- source.txt\n"
				}
				if name == "pre-push" {
					body += "printf '%s\\n' \"$1\" \"$2\" > .git/push-arguments\ncat > .git/push-input\n"
					if scenario == "pre-push-rejected" {
						body += "exit 23\n"
					}
				}
				os.WriteFile(filepath.Join(hooks, name), []byte(body), 0700)
			}
			key := filepath.Join(t.TempDir(), "signing-key")
			cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(output))
			}
			resources := &ExecutionResources{Directory: root, Environment: CommandEnvironment{Backend: "owned-test-adapter", Workspace: root}, Inspect: func(context.Context) (GitInspection, error) {
				return inspectGitCommands(context.Background(), host, "git", &source.Snapshot{HeadSHA: parent, MergeBaseSHA: parent}, "feature/existing")
			}, Command: func(ctx context.Context, args []string, timeout time.Duration, _ bool) (codex.CommandResult, error) {
				if scenario == "toolchain-without-bin-test" && args[0] == "/bin/test" {
					return codex.CommandResult{ExitCode: 127, Stderr: "the isolated toolchain provides test on PATH, not /bin/test"}, nil
				}
				return host.Run(ctx, args, timeout)
			}, Quiesce: func() error { return nil }}
			committer, ok := any(resources).(feedbackCommitter)
			if !ok {
				t.Fatal("ordinary hooked and scoped signed commit capability is missing")
			}
			program, _ := exec.LookPath("ssh-keygen")
			policy := CommitPolicy{Name: "Fixture", Email: "f@example.invalid", HooksPath: ".git/hooks", Sign: true, SigningFormat: "ssh", SigningKey: key, SigningProgram: program, Environment: []string{"PATH=" + os.Getenv("PATH")}}
			if scenario == "hook-unavailable" {
				policy.HooksPath = filepath.Join(root, "missing-operator-hooks")
			}
			result, err := committer.CommitFeedback(context.Background(), &source.Snapshot{HeadSHA: parent, MergeBaseSHA: parent}, policy, "fix: preserve feedback behavior\n\nKeep the requested source result.\n")
			actual := run("git", "rev-parse", "HEAD")
			if scenario != "signed" && scenario != "pre-push-rejected" && scenario != "toolchain-without-bin-test" {
				if err == nil || actual != parent {
					t.Fatalf("rejected or changed hook published a commit: %s %v", actual, err)
				}
				if scenario == "hook-rejected" || scenario == "hook-source-change" {
					observed, readErr := os.ReadFile(filepath.Join(root, ".git", "hook-observations"))
					if readErr != nil || !strings.HasPrefix(string(observed), "pre-commit\n") {
						t.Fatal("negative fixture did not exercise its actual hook", readErr)
					}
				}
				return
			}
			if err != nil || actual != result.OID || result.Parent != parent || !strings.Contains(string(result.Contents), "\ngpgsig -----BEGIN SSH SIGNATURE-----\n") {
				t.Fatalf("normal signed commit: %v", err)
			}
			observed, err := os.ReadFile(filepath.Join(root, ".git", "hook-observations"))
			if err != nil || string(observed) != "pre-commit\nprepare-commit-msg\ncommit-msg\npost-commit\n" {
				t.Fatal("normal hook order was skipped", string(observed), err)
			}
			allowed := filepath.Join(t.TempDir(), "allowed-signers")
			public, err := os.ReadFile(key + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(allowed, append([]byte("f@example.invalid "), public...), 0600)
			run("git", "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", result.OID)
			pushErr := resources.CheckFeedbackPush(context.Background(), policy, "refs/heads/feature/existing", "https://github.com/owner/repo.git", result)
			input, readErr := os.ReadFile(filepath.Join(root, ".git", "push-input"))
			if readErr != nil || string(input) != "refs/heads/feature/existing "+result.OID+" refs/heads/feature/existing "+parent+"\n" {
				t.Fatal("normal pre-push stdin was not the exact old/new ref", readErr)
			}
			arguments, readErr := os.ReadFile(filepath.Join(root, ".git", "push-arguments"))
			if readErr != nil || string(arguments) != "origin\nhttps://github.com/owner/repo.git\n" {
				t.Fatal("normal pre-push arguments differ", readErr)
			}
			if (pushErr != nil) != (scenario == "pre-push-rejected") {
				t.Fatal("normal pre-push result was ignored", pushErr)
			}
		})
	}
}
