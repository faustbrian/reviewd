package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/source"
)

// CommitPolicy is application-captured operator Git policy, never model input
// or repository preferences. Signing authority stays outside repo commands.
type CommitPolicy struct {
	Name, Email, HooksPath                    string
	Sign                                      bool
	SigningFormat, SigningKey, SigningProgram string
	Environment                               []string
	RequiredHooks                             map[string]string
}

type CommittedFeedback struct {
	OID, Tree, Parent string
	Contents          []byte
	Hooks             []string
}

func (r *ExecutionResources) checkHooks(ctx context.Context, policy CommitPolicy) error {
	read := func(argv ...string) (string, error) {
		result, err := r.Command(ctx, argv, time.Minute, false)
		err = errors.Join(err, r.Quiesce())
		if err != nil {
			return "", err
		}
		if result.ExitCode != 0 {
			return "", errors.New("configured normal Git hooks are unavailable in the isolated command environment; no hook bypass is permitted")
		}
		return result.Stdout, nil
	}
	if policy.HooksPath != "" {
		if _, err := read("test", "-d", policy.HooksPath); err != nil {
			return err
		}
	}
	for _, name := range normalFeedbackHooks {
		expected, ok := policy.RequiredHooks[name]
		if !ok {
			continue
		}
		path := filepath.Join(policy.HooksPath, name)
		if _, err := read("test", "-x", path); err != nil {
			return err
		}
		hash, err := read("sha256sum", "--", path)
		if err != nil {
			return err
		}
		fields := strings.Fields(hash)
		if len(fields) < 1 || fields[0] != expected {
			return errors.New("isolated Git hook differs from the captured operator policy")
		}
	}
	return nil
}

func (r *ExecutionResources) git(ctx context.Context, policy CommitPolicy, args ...string) (string, error) {
	hooks := policy.HooksPath
	if hooks == "" {
		hooks = ".git/hooks"
	}
	argv := []string{"git", "--no-pager", "--no-replace-objects", "--git-dir=.git", "--work-tree=.", "-c", "core.hooksPath=" + hooks}
	result, err := r.Command(ctx, append(argv, args...), 20*time.Minute, true)
	err = errors.Join(err, r.Quiesce())
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", errors.New("normal Git hook or scoped commit command failed; preserve pending work")
	}
	return result.Stdout, nil
}

func (r *ExecutionResources) writeGitControl(name string, data []byte) error {
	if err := r.Quiesce(); err != nil {
		return err
	}
	root, err := os.OpenRoot(r.Directory)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(".git"); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("scoped commit requires private Git metadata")
	}
	path := ".git/" + name
	if info, err := root.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("scoped commit control path is not a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Close())
}

func (r *ExecutionResources) readGitControl(name string) ([]byte, error) {
	root, err := os.OpenRoot(r.Directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat(".git/" + name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("commit hook produced an invalid message file")
	}
	return root.ReadFile(".git/" + name)
}

func sameCommitCandidate(a, b executionTree, before, after GitInspection) bool {
	// Git's message, reflog, index-cache representation and hook diagnostics may
	// change during ordinary hooks. The complete source bytes/modes and canonical
	// index/parent/branch must not. Configuration/hook authority remains pinned.
	aa, _ := json.Marshal(a.Entries)
	bb, _ := json.Marshal(b.Entries)
	if !bytes.Equal(aa, bb) || before.Head != after.Head || before.Branch != after.Branch || before.Index != after.Index {
		return false
	}
	aa, _ = json.Marshal(commitPolicyEntries(a))
	bb, _ = json.Marshal(commitPolicyEntries(b))
	return bytes.Equal(aa, bb)
}

func commitPolicyEntries(tree executionTree) []executionEntry {
	entries := []executionEntry{}
	for _, entry := range tree.GitMetadata {
		if entry.Path == ".git/config" || entry.Path == ".git/HEAD" || strings.HasPrefix(entry.Path, ".git/hooks/") || strings.HasPrefix(entry.Path, ".git/info/") {
			entries = append(entries, entry)
		}
	}
	return entries
}

// CommitFeedback runs normal pre-commit/prepare-commit-msg/commit-msg hooks in
// the existing isolated command backend, then signs only the exact approved
// payload outside it. Source/index changes by hooks require renewed checks and
// review rather than silently expanding the accepted candidate. Object writing
// and the exact-parent ref CAS run inside the backend; no repo code runs here.
func (r *ExecutionResources) CommitFeedback(ctx context.Context, snapshot *source.Snapshot, policy CommitPolicy, message string) (result CommittedFeedback, err error) {
	if r.Command == nil || r.Quiesce == nil || r.Inspect == nil || snapshot == nil || strings.TrimSpace(message) == "" || len(message) > 64<<10 || strings.ContainsRune(message, 0) {
		return result, errors.New("scoped commit requires an owned verified candidate and bounded message")
	}
	if policy.Name == "" || policy.Email == "" || strings.ContainsAny(policy.HooksPath, "\r\n\x00") {
		return result, errors.New("normal operator Git identity/hooks policy is unavailable")
	}
	if policy.Sign && (policy.SigningFormat != "ssh" || policy.SigningKey == "" || policy.SigningProgram == "") {
		return result, errors.New("required Git signing policy is unavailable; scoped signing currently requires SSH")
	}
	if err = r.checkHooks(ctx, policy); err != nil {
		return result, err
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	before, err := r.Inspect(ctx)
	if err != nil {
		return result, err
	}
	if err = r.Quiesce(); err != nil {
		return result, err
	}
	original, err := captureExecutionTree(r.Directory, nil)
	if err != nil {
		return result, err
	}
	if before.Head != snapshot.HeadSHA || !strings.HasPrefix(before.Branch, "refs/heads/") {
		return result, errors.New("scoped commit no longer has its original parent/branch")
	}
	if _, err = r.git(ctx, policy, "diff", "--quiet", "--no-ext-diff", "--no-textconv", "--exit-code", "--"); err != nil {
		return result, errors.New("unstaged source must be explicitly reconciled before committing")
	}
	untracked, err := r.git(ctx, policy, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return result, err
	}
	if untracked != "" {
		return result, errors.New("new source must be explicitly staged before committing")
	}
	if before.StagedDiff == "" {
		return result, errors.New("no staged feedback change is available for an additive commit")
	}
	if err = r.writeGitControl("COMMIT_EDITMSG", []byte(message)); err != nil {
		return result, err
	}
	for _, hook := range []struct {
		name string
		args []string
	}{{"pre-commit", nil}, {"prepare-commit-msg", []string{".git/COMMIT_EDITMSG", "message"}}, {"commit-msg", []string{".git/COMMIT_EDITMSG"}}} {
		args := []string{"hook", "run", "--ignore-missing", hook.name}
		if len(hook.args) > 0 {
			args = append(append(args, "--"), hook.args...)
		}
		if _, err = r.git(ctx, policy, args...); err != nil {
			return result, fmt.Errorf("%s: %w", hook.name, err)
		}
		result.Hooks = append(result.Hooks, hook.name)
	}
	after, err := r.Inspect(ctx)
	if err != nil {
		return result, err
	}
	if err = r.Quiesce(); err != nil {
		return result, err
	}
	current, err := captureExecutionTree(r.Directory, nil)
	if err != nil {
		return result, err
	}
	if !sameCommitCandidate(original, current, before, after) {
		return result, errors.New("normal hooks changed the reviewed source, index or Git policy; rerun affected verification and independent review")
	}
	finalMessage, err := r.readGitControl("COMMIT_EDITMSG")
	if err != nil {
		return result, err
	}
	tree, err := r.git(ctx, policy, "write-tree")
	if err != nil {
		return result, err
	}
	tree = strings.TrimSpace(tree)
	identity := fmt.Sprintf("%s <%s> %d +0000", policy.Name, policy.Email, time.Now().Unix())
	scope := CommitSigningScope{Tree: tree, Parent: snapshot.HeadSHA, Author: identity, Committer: identity, Message: string(finalMessage)}
	payload := []byte("tree " + tree + "\nparent " + scope.Parent + "\nauthor " + identity + "\ncommitter " + identity + "\n\n" + scope.Message)
	if err = ValidateCommitSigningPayload(scope, payload); err != nil {
		return result, err
	}
	contents := payload
	if policy.Sign {
		signed, signErr := SignSSHCommit(ctx, SSHSignOptions{Executable: policy.SigningProgram, KeyFile: policy.SigningKey, TempRoot: filepath.Dir(r.Directory), Environment: policy.Environment}, scope, payload)
		if signErr != nil {
			return result, signErr
		}
		header, body, _ := bytes.Cut(payload, []byte("\n\n"))
		signature := strings.TrimSuffix(string(signed.Signature), "\n")
		contents = append(append(bytes.Clone(header), []byte("\ngpgsig "+strings.ReplaceAll(signature, "\n", "\n ")+"\n\n")...), body...)
	}
	// Data is a positional argument to fixed application code, never evaluated
	// as shell source. No signing key/provider auth reaches this command.
	object, err := r.Command(ctx, []string{"/bin/sh", "-c", `printf %s "$1" | git --no-pager --no-replace-objects --git-dir=.git hash-object -t commit -w --stdin`, "reviewd-scoped-object", string(contents)}, time.Minute, true)
	err = errors.Join(err, r.Quiesce())
	if err != nil {
		return result, err
	}
	oid := strings.TrimSpace(object.Stdout)
	if object.ExitCode != 0 || !objectID.MatchString(oid) {
		return result, errors.New("scoped signed commit object could not be written")
	}
	if _, err = r.git(ctx, policy, "update-ref", before.Branch, oid, snapshot.HeadSHA); err != nil {
		return result, err
	}
	result.OID, result.Tree, result.Parent, result.Contents = oid, tree, snapshot.HeadSHA, contents
	if _, err = r.git(ctx, policy, "hook", "run", "--ignore-missing", "post-commit"); err != nil {
		return result, err
	}
	result.Hooks = append(result.Hooks, "post-commit")
	if err = r.checkHooks(ctx, policy); err != nil {
		return result, err
	}
	finalIndex, indexErr := r.git(ctx, policy, "ls-files", "--stage", "--full-name", "-z")
	if indexErr != nil {
		return result, indexErr
	}
	finalSource, sourceErr := captureExecutionTree(r.Directory, nil)
	if sourceErr != nil {
		return result, sourceErr
	}
	post := before
	post.Index = finalIndex
	if !sameCommitCandidate(original, finalSource, before, post) {
		return result, errors.New("post-commit hook changed the approved candidate; preserve the local commit and rerun affected verification/review before pushing")
	}
	return result, nil
}

// CheckFeedbackPush runs the ordinary pre-push hook with Git's exact positional
// arguments and old/new-ref stdin, but with no network/provider authority. The
// authenticated transport is a separate application operation after this pass.
func (r *ExecutionResources) CheckFeedbackPush(ctx context.Context, policy CommitPolicy, branch, remote string, commit CommittedFeedback) error {
	if !strings.HasPrefix(branch, "refs/heads/") || !objectID.MatchString(commit.OID) || !objectID.MatchString(commit.Parent) || strings.ContainsAny(branch+remote, "\x00\r\n") {
		return errors.New("pre-push requires the exact existing branch and scoped commit")
	}
	if err := r.checkHooks(ctx, policy); err != nil {
		return err
	}
	before, err := captureExecutionTree(r.Directory, nil)
	if err != nil {
		return err
	}
	index, err := r.git(ctx, policy, "ls-files", "--stage", "--full-name", "-z")
	if err != nil {
		return err
	}
	if err = r.writeGitControl("reviewd-pre-push-input", []byte(branch+" "+commit.OID+" "+branch+" "+commit.Parent+"\n")); err != nil {
		return err
	}
	hooks := policy.HooksPath
	if hooks == "" {
		hooks = ".git/hooks"
	}
	argv := []string{"git", "--no-pager", "--no-replace-objects", "--git-dir=.git", "--work-tree=.", "-c", "core.hooksPath=" + hooks, "hook", "run", "--ignore-missing", "--to-stdin=.git/reviewd-pre-push-input", "pre-push", "--", "origin", remote}
	observation, err := r.Command(ctx, argv, 20*time.Minute, true)
	err = errors.Join(err, r.Quiesce())
	if err != nil {
		return err
	}
	if observation.ExitCode != 0 {
		return errors.New("normal pre-push hook rejected delivery; preserve the local commit")
	}
	after, err := captureExecutionTree(r.Directory, nil)
	if err != nil {
		return err
	}
	finalIndex, err := r.git(ctx, policy, "ls-files", "--stage", "--full-name", "-z")
	if err != nil {
		return err
	}
	if !sameCommitCandidate(before, after, GitInspection{Index: index}, GitInspection{Index: finalIndex}) {
		return errors.New("normal pre-push changed the reviewed source or index; renew verification/review before delivery")
	}
	head, err := r.git(ctx, policy, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != commit.OID {
		return errors.New("pre-push changed the scoped local commit")
	}
	return r.checkHooks(ctx, policy)
}
