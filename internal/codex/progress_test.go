package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusReportsScopedExecutionWithoutLeakingReviewText(t *testing.T) {
	for _, failAt := range []string{"", "starting", "reviewing", "reading_evidence"} {
		t.Run("failure="+failAt, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			runtime, err := StartRuntime(ctx, testRuntimeOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			skill := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nPrivate fixture.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var statuses []ReviewOutput
			persistenceFailure := errors.New("owned persistence failure")
			result, err := runtime.Review(ctx, ReviewRequest{Model: "gpt-6.1-sol", Effort: "high", SkillPath: skill, SkillName: "requesting-code-review", Evidence: NewEvidenceBroker(map[string]map[string]string{"head": {"main.go": "immutable fixture"}}, nil), OnStatus: func(status ReviewOutput) error {
				statuses = append(statuses, status)
				if status.Text != "" || len(status.Specialists) != 0 || strings.Contains(string(status.Usage), "999") {
					t.Error("progress leaked review text or accepted foreign-turn usage")
				}
				if status.Progress == failAt {
					return persistenceFailure
				}
				return nil
			}})
			if len(statuses) == 0 || statuses[0].ThreadID != "thread-fixture" || statuses[0].TurnID != "" || statuses[0].RequestedModel != "gpt-6.1-sol" {
				t.Fatal("thread identity was not reported before starting a turn")
			}
			if failAt != "" {
				if !errors.Is(err, persistenceFailure) || result.Text != "" {
					t.Fatalf("failed progress persistence became a completed review: %v %+v", err, result)
				}
				return
			}
			last := statuses[len(statuses)-1]
			if err != nil || result.Text != "prepared fixture" || last.Progress != "finished" || last.TurnID != "turn-fixture" || !strings.Contains(string(last.Usage), `"totalTokens":41`) {
				t.Fatalf("final status lost correlated execution: %v %+v", err, last)
			}
		})
	}
}
