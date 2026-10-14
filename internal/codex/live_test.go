package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test uses the actual installed Codex and an owned fixture skill.
// It proves protocol and evidence access, not full capability denial, staff review quality or any
// GitHub publication. Ordinary tests remain offline and credential-free.
func TestLiveCodexReadOnlyEvidenceAndCancellation(t *testing.T) {
	if os.Getenv("REVIEWD_LIVE_CODEX") != "1" {
		t.Skip("set REVIEWD_LIVE_CODEX=1 for an authorized live Codex integration")
	}
	parent := t.TempDir()
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: reviewd-integration-fixture\ndescription: Read-only protocol fixture\n---\nUse reviewd_read_evidence to read the fixture document. Never guess its value. Return only JSON containing value and shell_available. Do not write, install, publish, browse, or run anything.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runtime, err := StartRuntime(ctx, RuntimeOptions{TempRoot: parent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	fixtureRead := false
	var liveStatuses []ReviewOutput
	result, err := runtime.Review(ctx, ReviewRequest{Model: "gpt-6.1-sol", Effort: "high", SkillName: "reviewd-integration-fixture", SkillPath: skill, Prompt: "Read the fixture evidence document and report its value. Report whether a shell tool is available.", Evidence: NewEvidenceBroker(nil, map[string]string{"fixture": "value=reviewd_live_read_only_483"}), OnStatus: func(status ReviewOutput) error {
		liveStatuses = append(liveStatuses, status)
		if status.Text != "" {
			t.Error("actual Codex status exposed model answer text")
		}
		return nil
	}, OnProgress: func(message Message) {
		if message.Method != "item/tool/call" {
			return
		}
		var call struct {
			Tool      string
			Arguments struct{ Key string }
		}
		if json.Unmarshal(message.Params, &call) == nil && call.Tool == "reviewd_read_evidence" && call.Arguments.Key == "fixture" {
			fixtureRead = true
		}
	}, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]string{"type": "string"}, "shell_available": map[string]string{"type": "boolean"}}, "required": []string{"value", "shell_available"}, "additionalProperties": false}})
	if err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Value          string `json:"value"`
		ShellAvailable bool   `json:"shell_available"`
	}
	if err := json.Unmarshal([]byte(result.Text), &actual); err != nil {
		t.Fatalf("actual Codex did not return a valid structured result: %v", err)
	}
	if actual.Value != "reviewd_live_read_only_483" || actual.ShellAvailable || !fixtureRead {
		t.Fatalf("live read-only fixture was not exercised: %+v", actual)
	}
	if result.EffectiveModel != "gpt-6.1-sol" || result.ThreadID == "" || result.TurnID == "" {
		t.Fatalf("live identity/model missing: %+v", result)
	}
	if len(liveStatuses) < 3 || liveStatuses[0].ThreadID != result.ThreadID || liveStatuses[0].TurnID != "" || liveStatuses[len(liveStatuses)-1].Progress != "finished" {
		t.Fatal("actual Codex did not expose thread identity before turn creation and final status")
	}
	activeTurnReported := false
	for _, status := range liveStatuses[:len(liveStatuses)-1] {
		if status.ThreadID == result.ThreadID && status.TurnID == result.TurnID && status.Progress != "finished" {
			activeTurnReported = true
		}
	}
	if !activeTurnReported {
		t.Fatal("actual Codex turn was only reported retrospectively")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("live process/runtime cleanup failed: %v %v", entries, err)
	}
	// Cancellation crosses the actual subprocess boundary, not just a fake.
	short, cancelShort := context.WithCancel(context.Background())
	second, err := StartRuntime(short, RuntimeOptions{TempRoot: parent})
	if err != nil {
		cancelShort()
		t.Fatal(err)
	}
	cancelShort()
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled live runtime retained: %v %v", entries, err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("empty live result")
	}
}
