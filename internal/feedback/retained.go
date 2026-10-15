package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// A projection of existing durable state, not a bearer credential or another
// authorization store. Service/store checks remain authoritative on execution.
func ResumeConfirmation(run store.Run) string {
	if run.ID == "" || run.OwnerID == "" || run.Purpose != "feedback" || run.OwnerClosed || (run.State != store.FeedbackPending && run.State != store.Partial) {
		return ""
	}
	var journal Journal
	if json.Unmarshal(run.Outcome, &journal) != nil || journal.Version != 1 || journal.Retained == nil {
		return ""
	}
	data, _ := json.Marshal(run)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Only mutations already recorded by this run may explain a changed target.
// Other humans' edits, commits or branch changes never become resume authority.
func resumeTargetMatches(journal Journal, current github.FeedbackTarget) bool {
	expected := journal.Target
	if journal.Execution.Delivery.Started {
		expected = journal.Execution.Delivery.Target
	}
	if current.HeadRepository != expected.HeadRepository || current.HeadRepositoryID != expected.HeadRepositoryID {
		return false
	}
	comparison := current.Pull
	for _, effect := range journal.Execution.Delivery.Effects {
		if effect.Kind == "push" && objectID.MatchString(journal.Execution.Delivery.Commit.OID) && comparison.HeadSHA == journal.Execution.Delivery.Commit.OID {
			comparison.HeadSHA, comparison.CommitCount, comparison.ChangedFiles = expected.Pull.HeadSHA, expected.Pull.CommitCount, expected.Pull.ChangedFiles
		}
		if effect.Kind == "metadata" && comparison.Title == journal.Execution.Delivery.Proposal.Title && comparison.Body == journal.Execution.Delivery.Proposal.Body {
			comparison.Title, comparison.Body = expected.Pull.Title, expected.Pull.Body
		}
	}
	return github.SameReviewContext(comparison, expected.Pull)
}

func validateReconciledWorkspace(ctx context.Context, resources *ExecutionResources, snapshot *source.Snapshot, pull github.Pull, frozen executionTree) error {
	if snapshot == nil || snapshot.HeadSHA != pull.HeadSHA || snapshot.TargetBaseSHA != pull.TargetBaseSHA || snapshot.CommitCount != pull.CommitCount || len(snapshot.Changes) != pull.ChangedFiles {
		return errors.New("reconciliation lacks the complete canonical delivered-head source")
	}
	inspection, err := resources.Inspect(ctx)
	if err = errors.Join(err, resources.Quiesce()); err != nil {
		return err
	}
	if inspection.Head != pull.HeadSHA || inspection.Branch != "refs/heads/"+pull.HeadRef || inspection.Status != "" || inspection.StagedDiff != "" {
		return errors.New("retained workspace is not clean on the canonical delivered head; preserve local work")
	}
	if err = snapshot.ValidateFeedbackWorkspace(ctx, resources.Directory); err != nil {
		return err
	}
	manifest, ok := snapshot.Manifests["head"]
	if !ok {
		return errors.New("canonical delivered head omitted its complete index manifest")
	}
	manifest = append([]source.Entry(nil), manifest...)
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	var index strings.Builder
	for _, entry := range manifest {
		index.WriteString(entry.Mode + " " + entry.OID + " 0\t" + entry.Path + "\x00")
	}
	if inspection.Index != index.String() {
		return errors.New("retained index differs from the canonical delivered source")
	}
	paths := make([]string, 0, len(frozen.Entries))
	for _, entry := range frozen.Entries {
		paths = append(paths, entry.Path)
	}
	current, err := captureExecutionTree(resources.Directory, paths)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(frozen.Entries)
	after, _ := json.Marshal(current.Entries)
	if !bytes.Equal(before, after) {
		return errors.New("retained source differs from the original reviewed candidate")
	}
	before, _ = json.Marshal(commitPolicyEntries(frozen))
	after, _ = json.Marshal(commitPolicyEntries(current))
	if !bytes.Equal(before, after) {
		return errors.New("retained Git hook/configuration authority changed after review")
	}
	return nil
}

type retainedCandidate struct {
	Name   string `json:"name"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	SHA256 string `json:"sha256"`
}

const maxRetainedCandidate = 512 << 20

// Preserve the already-reviewed immutable projection across a process restart.
// Only its locator/digest joins the existing journal; this is neither a second
// execution owner nor an authority to replay any operation.
func saveRetainedCandidate(ownerRoot string, tree executionTree) (_ *retainedCandidate, err error) {
	data, err := json.Marshal(tree)
	if err != nil || len(data) > maxRetainedCandidate {
		return nil, errors.New("reviewed candidate exceeds the retained evidence bound")
	}
	file, err := os.CreateTemp(ownerRoot, "feedback-candidate-")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return nil, err
	}
	if err = file.Sync(); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("reviewed candidate storage identity is unavailable")
	}
	digest := sha256.Sum256(data)
	return &retainedCandidate{Name: filepath.Base(name), Device: uint64(identity.Dev), Inode: uint64(identity.Ino), SHA256: hex.EncodeToString(digest[:])}, nil
}

func (p *retainedCandidate) load(ownerRoot, expected string) (executionTree, error) {
	var tree executionTree
	if p == nil || filepath.Base(p.Name) != p.Name || !strings.HasPrefix(p.Name, "feedback-candidate-") {
		return tree, errors.New("original reviewed candidate is unavailable")
	}
	root, err := os.OpenRoot(ownerRoot)
	if err != nil {
		return tree, err
	}
	defer root.Close()
	file, err := root.OpenFile(p.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return tree, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > maxRetainedCandidate {
		return tree, errors.New("reviewed candidate storage is unsupported")
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(identity.Dev) != p.Device || uint64(identity.Ino) != p.Inode {
		return tree, errors.New("reviewed candidate storage was replaced")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRetainedCandidate+1))
	if err != nil || len(data) > maxRetainedCandidate {
		return tree, errors.New("reviewed candidate could not be completely read")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != p.SHA256 || json.Unmarshal(data, &tree) != nil || tree.ID != expected {
		return executionTree{}, errors.New("original reviewed candidate identity changed")
	}
	return tree, nil
}

// These are locators inside the existing private owner, not another execution
// lease, recovery journal or source of permission. Reopening also requires the
// store's original-root/lease/resource proof and observed-run CAS.
type retainedPath struct {
	Name   string `json:"name"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type retainedExecution struct {
	Source         retainedPath `json:"source"`
	Workspace      retainedPath `json:"workspace"`
	Skill          retainedPath `json:"skill"`
	SkillName      string       `json:"skill_name"`
	SkillDirectory string       `json:"skill_directory"`
}

func retainPath(ownerRoot, path, prefix string) (retainedPath, error) {
	owner, err := filepath.EvalSymlinks(ownerRoot)
	if err != nil {
		return retainedPath{}, err
	}
	canonical, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	if err != nil || statErr != nil || canonical != path || filepath.Dir(path) != owner || !strings.HasPrefix(filepath.Base(path), prefix) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return retainedPath{}, errors.New("retained feedback path is not an exact private child of its existing owner")
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return retainedPath{}, errors.New("retained feedback path identity is unavailable")
	}
	return retainedPath{Name: filepath.Base(path), Device: uint64(identity.Dev), Inode: uint64(identity.Ino)}, nil
}

func (p retainedPath) reopen(ownerRoot, prefix string) (string, error) {
	if p.Name == "" || filepath.Base(p.Name) != p.Name {
		return "", errors.New("retained feedback locator is unavailable")
	}
	path := filepath.Join(ownerRoot, p.Name)
	current, err := retainPath(ownerRoot, path, prefix)
	if err != nil || current != p {
		return "", errors.New("original retained feedback storage was lost or replaced; no replacement checkout is permitted")
	}
	return path, nil
}
