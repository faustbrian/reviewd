package review

import (
	"os"
	"path/filepath"
	"testing"
)

type feedbackInstructionRoot interface{ FeedbackCommandRoot(string) (string, error) }

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
