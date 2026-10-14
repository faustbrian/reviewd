package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

// This opt-in check uses the operator-selected installed review skill, not the
// protocol fixture skill. Source and GitHub are owned; publication stays off.
func TestLiveCodexWatcherUsesInstalledReviewSkill(t *testing.T) {
	if os.Getenv("REVIEWD_LIVE_CODEX") != "1" || os.Getenv("REVIEWD_LIVE_SKILL_PATH") == "" {
		t.Skip("set REVIEWD_LIVE_CODEX=1 and REVIEWD_LIVE_SKILL_PATH to the authorized installed review SKILL.md")
	}
	base := "package fixture\n\n// Discount returns the amount after a percentage discount.\nfunc Discount(total, percent int) int {\n\treturn total - total*percent/100\n}\n\nfunc Total() int { return Discount(100, 25) }\n"
	head := strings.Replace(base, "total*percent/100", "total*percent", 1)
	invoke, path, deps, durable := privateApplicationFixture(t, "REAL-INSTALLED-SKILL-WATCHER-ACCEPTANCE", [2]string{base, head})
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	skill, err := filepath.Abs(os.Getenv("REVIEWD_LIVE_SKILL_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	donor := os.Getenv("CODEX_HOME")
	if donor == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		donor = filepath.Join(home, ".codex")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	replacements := map[string]string{"auth_home=": strconv.Quote(donor), "executable=": strconv.Quote(executable), "skill_path=": strconv.Quote(skill), "model=": `"gpt-6.1-sol"`, "reasoning_effort=": `"high"`}
	for i, line := range lines {
		for prefix, value := range replacements {
			if strings.HasPrefix(line, prefix) {
				lines[i] = prefix + value
			}
		}
	}
	data = []byte(strings.Replace(strings.Join(lines, "\n"), "[actions]", "[actions]\npublish_review=\"never\"", 1) + "\n[limits]\nreview_timeout=\"3m\"\n[notifications]\ndesktop=false\nterminal=false\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	deps.Review = nil
	initial := invoke("sync")
	if len(initial.Items) != 1 {
		t.Fatal("installed-skill watcher has no held request")
	}
	invoke("start", initial.Items[0].ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var output, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("installed-skill watcher failed: exit=%d %s", code, diagnostics.String())
	}
	runs, err := durable.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatal("installed-skill watcher did not retain exactly one run")
	}
	run := runs[0]
	if run.State != store.Prepared || !run.Private || !run.OwnerClosed || run.OwnerID == "" || run.ThreadID == "" || run.TurnID == "" || run.RequestedModel != "gpt-6.1-sol" || run.EffectiveModel == "" || run.SkillDigest == "" || len(run.Outcome) != 0 {
		t.Fatal("installed-skill watcher lost private execution/cleanup identity")
	}
	var prepared review.PreparedResult
	if json.Unmarshal(run.Prepared, &prepared) != nil || review.ValidatePrepared(prepared) != nil || prepared.Boundary.Skill.Digest != run.SkillDigest {
		t.Fatal("installed review skill did not produce a valid retained preview")
	}
	found := false
	for _, finding := range prepared.Report.Findings {
		t.Logf("Reported finding: kind=%s path=%q side=%s line=%d", finding.Kind, finding.Path, finding.Side, finding.Line)
		if finding.Kind == "blocker" && finding.Check == "" && finding.Path == "file.go" && finding.Side == "RIGHT" && finding.Line == 5 && (strings.Contains(finding.Body, "100") || strings.Contains(strings.ToLower(finding.Body), "percent")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("installed skill did not retain the expected canonical percentage-contract blocker: disposition=%s findings=%d gaps=%d", prepared.Disposition, len(prepared.Report.Findings), len(prepared.Gaps))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state", "private-work", run.OwnerID)); !os.IsNotExist(err) {
		t.Fatal("installed-skill watcher retained owned private storage")
	}
	for _, command := range [][]string{{"show", run.ID}, {"inbox", "--json"}} {
		var shown, errors bytes.Buffer
		if code := Run(ctx, append([]string{"--config", path}, command...), &shown, &errors, deps); code != 0 || !strings.Contains(shown.String(), run.ID) || !strings.Contains(shown.String(), run.ThreadID) {
			t.Fatal("installed-skill private result unavailable to ordinary clients")
		}
	}
	t.Logf("Installed skill digest %s: actual model %s found seeded source defect at file.go RIGHT:5; private prepared result and closed owner retained; no GitHub publication", run.SkillDigest, run.EffectiveModel)
}
