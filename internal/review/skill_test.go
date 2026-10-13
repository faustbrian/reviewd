package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillCaptureUsesActualInstructionsReferencesAndFrozenDigest(t *testing.T) {
	installed := t.TempDir()
	owned := t.TempDir()
	os.Mkdir(filepath.Join(installed, "references"), 0700)
	main := filepath.Join(installed, "SKILL.md")
	os.WriteFile(main, []byte("---\nname: requesting-code-review\n---\nUnique installed instruction. Read references/method.md.\n"), 0600)
	os.WriteFile(filepath.Join(installed, "references", "method.md"), []byte("Unique causal review reference.\n"), 0600)
	if err := os.Symlink("/outside-private-file", filepath.Join(installed, "references", "outside.md")); err != nil {
		t.Fatal(err)
	}
	if capture, err := CaptureSkill("requesting-code-review", main, owned); err == nil {
		capture.Close()
		t.Error("skill reference symlink could expand evidence outside selected installed package")
	}
	os.Remove(filepath.Join(installed, "references", "outside.md"))
	capture, err := CaptureSkill("requesting-code-review", main, owned)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	if capture.Identity.Name != "requesting-code-review" || capture.Identity.Digest == "" || !strings.Contains(capture.Documents["skill/references/method.md"], "Unique causal") {
		t.Error("actual selected instructions/reference identity not retained")
	}
	os.WriteFile(main, []byte("changed installed skill"), 0600)
	data, err := os.ReadFile(capture.Path)
	if err != nil || !strings.Contains(string(data), "Unique installed") {
		t.Error("in-flight skill read changed after capture")
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(owned)
	if len(entries) != 0 {
		t.Error("frozen private skill storage leaked")
	}
}

func TestSkillCaptureRetainsExplicitNormativeSiblingDependency(t *testing.T) {
	installed := t.TempDir()
	primary := filepath.Join(installed, "review-skill")
	dependency := filepath.Join(installed, "database-investigation")
	os.Mkdir(primary, 0700)
	os.Mkdir(dependency, 0700)
	os.WriteFile(filepath.Join(primary, "SKILL.md"), []byte("Use [database investigation](../database-investigation/SKILL.md) when required by the changed behavior."), 0600)
	os.WriteFile(filepath.Join(dependency, "SKILL.md"), []byte("Actual required database evidence contract."), 0600)
	capture, err := CaptureSkill("review-skill", filepath.Join(primary, "SKILL.md"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	if !strings.Contains(capture.Documents["skill-dependency/database-investigation/SKILL.md"], "Actual required") {
		t.Error("selected skill's explicit normative dependency was silently omitted")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(capture.Path), "..", "database-investigation", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "Actual required") {
		t.Error("relative installed skill dependency was not privately frozen")
	}
}
