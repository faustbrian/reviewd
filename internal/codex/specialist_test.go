package codex

import (
	"context"
	"encoding/json"
	"net"
	"testing"
)

func TestSelectedSkillDelegationIsSingleSealedLeafCapability(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	runtime := &Runtime{Client: client, activeThread: "primary", activeTurn: "turn"}
	called := 0
	request := ReviewRequest{OnSpecialist: func(ctx context.Context, task SpecialistRequest) (SpecialistOutput, error) {
		called++
		return SpecialistOutput{Task: task.Task, Effort: task.Effort, Text: "independent source-backed report", ThreadID: "leaf-thread", TurnID: "leaf-turn"}, nil
	}}
	for index, args := range []string{`{"task":"Review complete retry boundary","paths":["retry.go"],"effort":"high"}`, `{"task":"Another specialist","paths":["other.go"],"effort":"high"}`} {
		params, _ := json.Marshal(map[string]any{"threadId": "primary", "turnId": "turn", "tool": "reviewd_delegate_specialist", "arguments": json.RawMessage(args)})
		done := make(chan error, 1)
		go func() {
			done <- runtime.handleRequest(context.Background(), Message{ID: json.RawMessage(`"delegate"`), Method: "item/tool/call", Params: params}, request)
		}()
		var response Message
		if json.NewDecoder(right).Decode(&response) != nil {
			t.Fatal("missing delegation response")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		var result struct {
			Success bool `json:"success"`
		}
		json.Unmarshal(response.Result, &result)
		if result.Success != (index == 0) {
			t.Error("first independent leaf was rejected or a second leaf acquired authority")
		}
	}
	if called != 1 || len(runtime.specialistOutputs) != 1 {
		t.Fatal("selected skill can spawn more than one application-owned leaf")
	}
}
