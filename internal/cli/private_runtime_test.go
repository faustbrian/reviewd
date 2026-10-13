package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/review"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli 0.162.1")
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		privateWorkerPeer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func privateWorkerPeer() {
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	thread, model, turn := "cli-private-thread", "fixture-model", "cli-private-turn"
	for {
		var message codex.Message
		if decoder.Decode(&message) != nil {
			return
		}
		reply := func(value any) {
			data, _ := json.Marshal(value)
			encoder.Encode(codex.Message{ID: message.ID, Result: data})
		}
		switch message.Method {
		case "initialize":
			reply(map[string]string{"userAgent": "owned-private-cli-peer"})
		case "initialized":
		case "account/read":
			reply(map[string]any{"requiresOpenaiAuth": true, "account": map[string]string{"type": "apiKey"}})
		case "model/list":
			reply(map[string]any{"data": []any{map[string]any{"model": model, "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "xhigh"}}}}, "nextCursor": nil})
		case "thread/start":
			var request struct {
				Model        string `json:"model"`
				DynamicTools []struct {
					Name string `json:"name"`
				} `json:"dynamicTools"`
			}
			json.Unmarshal(message.Params, &request)
			if request.Model != model || os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("SSH_AUTH_SOCK") != "" {
				return
			}
			reply(map[string]any{"thread": map[string]string{"id": thread}, "model": model})
		case "turn/start":
			var request struct {
				Model  string                        `json:"model"`
				Effort string                        `json:"effort"`
				Input  []struct{ Type, Path string } `json:"input"`
			}
			json.Unmarshal(message.Params, &request)
			if request.Model != model || request.Effort != "xhigh" {
				return
			}
			found := false
			for _, input := range request.Input {
				if input.Type == "skill" {
					text, err := os.ReadFile(input.Path)
					if err == nil && strings.Contains(string(text), "Only in actual frozen installed skill") {
						found = true
					}
				}
			}
			if !found {
				return
			}
			reply(map[string]any{"turn": map[string]string{"id": turn, "status": "inProgress"}})
			params, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "tool": "reviewd_read_source", "arguments": map[string]string{"revision": "target", "path": "AGENTS.md"}})
			encoder.Encode(codex.Message{ID: json.RawMessage(`"trusted-source"`), Method: "item/tool/call", Params: params})
		case "":
			if string(message.ID) != `"trusted-source"` || !strings.Contains(string(message.Result), "immutable trusted target instructions") {
				return
			}
			params, _ := json.Marshal(map[string]any{"threadId": thread, "turnId": turn, "item": map[string]string{"type": "agentMessage", "phase": "final_answer", "text": `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`}})
			encoder.Encode(codex.Message{Method: "item/completed", Params: params})
			params, _ = json.Marshal(map[string]any{"threadId": thread, "turn": map[string]string{"id": turn, "status": "completed"}})
			encoder.Encode(codex.Message{Method: "turn/completed", Params: params})
		case "turn/interrupt":
			reply(map[string]any{})
		}
	}
}

func TestPrivateCLIWorkerUsesCapturedSkillAndActualOverrideOnStdio(t *testing.T) {
	t.Setenv("GH_TOKEN", "publisher-only-canary")
	t.Setenv("GITHUB_TOKEN", "publisher-only-canary")
	t.Setenv("SSH_AUTH_SOCK", "publisher-only-agent")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	auth := t.TempDir()
	os.WriteFile(filepath.Join(auth, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0600)
	skillFile := filepath.Join(t.TempDir(), "SKILL.md")
	os.WriteFile(skillFile, []byte("---\nname: requesting-code-review\n---\nOnly in actual frozen installed skill.\n"), 0600)
	policy, err := config.Resolve(nil, "owner/repo", nil, map[string]any{"reviewer.executable": executable, "reviewer.auth_home": auth, "reviewer.model": "fixture-model", "reviewer.reasoning_effort": "xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	skill, err := review.CaptureSkill("requesting-code-review", skillFile, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer skill.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := runPrivateWorker(ctx, review.WorkerRequest{Policy: policy, Skill: skill, Evidence: codex.NewEvidenceBroker(map[string]map[string]string{"target": {"AGENTS.md": "immutable trusted target instructions"}}, nil), Prompt: "Review the granted immutable evidence."}, nil)
	if err != nil || output.ThreadID != "cli-private-thread" || output.RequestedModel != "fixture-model" || output.EffectiveModel != "fixture-model" {
		t.Fatalf("private worker wiring failed: %+v %v", output, err)
	}
}
