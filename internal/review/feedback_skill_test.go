package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeedbackCaptureIncludesActualLinkedWorkflowAndQAWithoutWeakeningReview(t *testing.T) {
	installed, owned := t.TempDir(), t.TempDir()
	write := func(name, body string) string {
		directory := filepath.Join(installed, name)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "SKILL.md")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	primary := "---\nname: handle-pr-feedback\ndescription: full fixture workflow\n---\nApply all linked instructions.\n"
	for i := range 16 {
		name := fmt.Sprintf("workflow-%d", i)
		primary += "[step](../" + name + "/SKILL.md)\n"
		write(name, "Required workflow instructions.\n"+strings.Repeat("x", 600<<10))
	}
	selected := write("handle-pr-feedback", primary)
	qa := write("qa-test-planner", "---\nname: qa-test-planner\ndescription: actual QA fixture\n---\nReturn one bounded independent coverage plan.\n")
	if capture, err := CaptureSkill("handle-pr-feedback", selected, owned); err == nil {
		capture.Close()
		t.Fatal("private review capture limit was widened")
	}
	captured, err := CaptureFeedbackSkill("handle-pr-feedback", selected, qa, owned)
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	qaFrozen, err := captured.InstructionPath("qa-test-planner")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := os.ReadFile(qa)
	if err := os.WriteFile(qa, []byte("later unrelated instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(qaFrozen)
	if err != nil || string(actual) != string(expected) || captured.Documents["skill/SKILL.md"] != primary {
		t.Fatal("actual frozen workflow or QA instructions drifted")
	}
	if _, err := captured.InstructionPath("ungranted"); err == nil {
		t.Fatal("uncaptured host skill became available")
	}
}
