package review

import (
	"os"
	"path/filepath"
	"testing"
)

type feedbackInstructionRoot interface{ FeedbackCommandRoot(string) (string, error) }

func TestFeedbackCaptureCanonicalizesAnAliasedAncestorWithoutGrantingCaptureLinks(t *testing.T) {
	base, installed := t.TempDir(), t.TempDir()
	physical := filepath.Join(base, "physical")
	if err := os.MkdirAll(filepath.Join(physical, "owner"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"handle-pr-feedback", "qa-test-planner"} {
		if err := os.Mkdir(filepath.Join(installed, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(installed, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: owned alias fixture\n---\nRead only frozen instructions.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Default storage may itself use the operating system's temporary alias;
	// it is not an explicit owner grant and must remain available.
	defaultReview, err := CaptureSkill("handle-pr-feedback", filepath.Join(installed, "handle-pr-feedback", "SKILL.md"), "")
	if err != nil {
		t.Fatal("ordinary default temporary storage changed", err)
	}
	defer defaultReview.Close()
	defaultFeedback, err := CaptureFeedbackSkill("handle-pr-feedback", filepath.Join(installed, "handle-pr-feedback", "SKILL.md"), filepath.Join(installed, "qa-test-planner", "SKILL.md"), "")
	if err != nil {
		t.Fatal("feedback default temporary storage changed", err)
	}
	defer defaultFeedback.Close()
	owner := filepath.Join(alias, "owner")
	capture, err := CaptureFeedbackSkill("handle-pr-feedback", filepath.Join(installed, "handle-pr-feedback", "SKILL.md"), filepath.Join(installed, "qa-test-planner", "SKILL.md"), owner)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	root, err := capture.FeedbackCommandRoot(owner)
	if err != nil {
		t.Fatal("legitimate ancestor alias denied the owned frozen capture", err)
	}
	canonicalOwner, err := filepath.EvalSymlinks(owner)
	if err != nil || filepath.Dir(root) != canonicalOwner || filepath.Dir(filepath.Dir(capture.Path)) != root {
		t.Fatal("capture consumers disagree on canonical owned storage", err)
	}
	ownerLink := filepath.Join(base, "linked-owner")
	if err := os.Symlink(canonicalOwner, ownerLink); err != nil {
		t.Fatal(err)
	}
	if linked, err := CaptureFeedbackSkill("handle-pr-feedback", filepath.Join(installed, "handle-pr-feedback", "SKILL.md"), filepath.Join(installed, "qa-test-planner", "SKILL.md"), ownerLink); err == nil {
		linked.Close()
		t.Error("a final owner-directory link admitted a new frozen capture")
	}
	if _, err := capture.FeedbackCommandRoot(ownerLink); err == nil {
		t.Error("a final owner-directory link acquired instruction authority")
	}
	if err := os.Rename(root, root+"-held"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(root) // Exact test-owned link only; never follow it.
		if err := os.Rename(root+"-held", root); err != nil {
			t.Error(err)
		}
	}()
	if err := os.Symlink(root+"-held", root); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.FeedbackCommandRoot(owner); err == nil {
		t.Fatal("a final capture-directory link acquired instruction authority")
	}
}

func TestFeedbackCommandRootIsOnlyTheExactOwnedFrozenText(t *testing.T) {
	installed, owner := t.TempDir(), t.TempDir()
	for _, name := range []string{"handle-pr-feedback", "qa-test-planner"} {
		os.Mkdir(filepath.Join(installed, name), 0700)
		os.WriteFile(filepath.Join(installed, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: owned instructions\n---\nRead the frozen validator.\n"), 0600)
	}
	primary := filepath.Join(installed, "handle-pr-feedback", "SKILL.md")
	qa := filepath.Join(installed, "qa-test-planner", "SKILL.md")
	capture, err := CaptureFeedbackSkill("handle-pr-feedback", primary, qa, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	grant, ok := any(capture).(feedbackInstructionRoot)
	if !ok {
		t.Fatal("owned frozen feedback instruction read boundary is missing")
	}
	root, err := grant.FeedbackCommandRoot(owner)
	if err != nil || root != filepath.Dir(filepath.Dir(capture.Path)) {
		t.Fatal("grant did not select only its exact frozen bundle", err)
	}
	if _, err := grant.FeedbackCommandRoot(installed); err == nil {
		t.Fatal("an unrelated root acquired instruction authority")
	}
	if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("must not expand the capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := grant.FeedbackCommandRoot(owner); err == nil {
		t.Fatal("an uncaptured file acquired command read access")
	}
	if err := os.Remove(filepath.Join(root, "extra.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capture.Path, []byte("modified after capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := grant.FeedbackCommandRoot(owner); err == nil {
		t.Fatal("changed instructions retained their frozen grant")
	}
	ordinary, err := CaptureSkill("handle-pr-feedback", primary, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if _, err := any(ordinary).(feedbackInstructionRoot).FeedbackCommandRoot(owner); err == nil {
		t.Fatal("ordinary review capture acquired command capability")
	}
}
