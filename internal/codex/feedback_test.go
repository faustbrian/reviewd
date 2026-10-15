package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFeedbackSessionUsesActualSkillAndOnlyGrantedScopedTools(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: handle-pr-feedback\ndescription: owned installed skill\n---\nExact workflow fixture.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	observed := make(chan map[string]any, 1)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		defer right.Close()
		dec, enc := json.NewDecoder(right), json.NewEncoder(right)
		reply := func(req Message, data any) {
			raw, _ := json.Marshal(data)
			_ = enc.Encode(Message{ID: req.ID, Result: raw})
		}
		event := func(id, method string, data any) {
			raw, _ := json.Marshal(data)
			wire := json.RawMessage(nil)
			if id != "" {
				wire = json.RawMessage(`"` + id + `"`)
			}
			_ = enc.Encode(Message{ID: wire, Method: method, Params: raw})
		}
		for {
			var req Message
			if dec.Decode(&req) != nil {
				return
			}
			switch req.Method {
			case "model/list":
				reply(req, map[string]any{"data": []any{map[string]any{"model": "gpt-6.1-sol", "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}}}}, "nextCursor": nil})
			case "thread/start":
				reply(req, map[string]any{"thread": map[string]string{"id": "feedback-thread"}, "model": "gpt-6.1-sol"})
			case "turn/start":
				var params map[string]any
				_ = json.Unmarshal(req.Params, &params)
				observed <- params
				reply(req, map[string]any{"turn": map[string]string{"id": "feedback-turn"}})
				event("wrong", "item/tool/call", map[string]any{"threadId": "other-thread", "turnId": "feedback-turn", "tool": "reviewd_feedback_inspect", "arguments": map[string]string{}})
			default:
				if string(req.ID) == `"wrong"` {
					if req.Error == nil {
						t.Error("foreign thread was not rejected")
					}
					event("allowed", "item/tool/call", map[string]any{"threadId": "feedback-thread", "turnId": "feedback-turn", "tool": "reviewd_feedback_inspect", "arguments": map[string]string{}})
				}
				if string(req.ID) == `"allowed"` {
					event("", "item/completed", map[string]any{"threadId": "feedback-thread", "turnId": "feedback-turn", "item": map[string]string{"type": "agentMessage", "phase": "final_answer", "text": "complete fixture"}})
					event("", "turn/completed", map[string]any{"threadId": "feedback-thread", "turn": map[string]string{"id": "feedback-turn", "status": "completed"}})
				}
			}
		}
	}()
	runtime := &Runtime{Client: client, isolation: &Isolation{Workspace: root}}
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runtime.HandleFeedback(ctx, FeedbackRequest{Model: "gpt-6.1-sol", Effort: "high", SkillName: "handle-pr-feedback", SkillPath: skill, Prompt: "Owned feedback", Tools: []map[string]any{{"type": "function", "name": "reviewd_feedback_inspect", "inputSchema": map[string]any{"type": "object"}}}, Handle: func(_ context.Context, name string, args json.RawMessage) (string, error) {
		calls++
		return "owned scope", nil
	}})
	if err != nil || result.Text != "complete fixture" || result.ThreadID != "feedback-thread" || calls != 1 {
		t.Fatalf("feedback tool session failed: %+v calls=%d err=%v", result, calls, err)
	}
	params := <-observed
	input := params["input"].([]any)
	selected := input[1].(map[string]any)
	if params["model"] != "gpt-6.1-sol" || params["effort"] != "high" || selected["type"] != "skill" || selected["name"] != "handle-pr-feedback" || selected["path"] != skill {
		t.Fatalf("actual selected skill/model not sent: %+v", params)
	}
	_ = client.Close()
	<-peerDone
}

func TestIndependentFeedbackRoleCannotGrantCommandOrPrimaryStatus(t *testing.T) {
	for _, variant := range []string{"command", "primary-status", "unknown-role"} {
		t.Run(variant, func(t *testing.T) {
			left, right := net.Pipe()
			client := NewClient(left, left)
			defer client.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer right.Close()
				dec, enc := json.NewDecoder(right), json.NewEncoder(right)
				for {
					var req Message
					if dec.Decode(&req) != nil {
						return
					}
					if req.Method != "model/list" {
						t.Error("invalid independent role reached native session creation", req.Method)
						return
					}
					raw, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"model": "gpt-6.1-sol", "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}}}}, "nextCursor": nil})
					_ = enc.Encode(Message{ID: req.ID, Result: raw})
				}
			}()
			root := t.TempDir()
			skill := filepath.Join(root, "SKILL.md")
			if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\n---\nOwned instruction\n"), 0600); err != nil {
				t.Fatal(err)
			}
			tool := "reviewd_feedback_read_evidence"
			role := "correctness_reviewer"
			var status func(ReviewOutput) error
			if variant == "command" {
				tool = "reviewd_feedback_command"
			}
			if variant == "primary-status" {
				status = func(ReviewOutput) error { return nil }
			}
			if variant == "unknown-role" {
				role = "unbounded-supervisor"
			}
			runtime := &Runtime{Client: client, isolation: &Isolation{Workspace: root}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runtime.HandleFeedback(ctx, FeedbackRequest{Role: role, Participant: "specialist", Model: "gpt-6.1-sol", Effort: "high", SkillName: "requesting-code-review", SkillPath: skill, OnStatus: status, Tools: []map[string]any{{"type": "function", "name": tool}}, Handle: func(context.Context, string, json.RawMessage) (string, error) {
				t.Error("invalid role reached application tool")
				return "", nil
			}})
			if err == nil {
				t.Fatal("independent role gained a modifying/primary capability")
			}
			_ = client.Close()
			<-done
		})
	}
}
