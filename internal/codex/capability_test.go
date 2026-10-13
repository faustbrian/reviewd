package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// QA-26: force a provider tool call rather than ask a cooperative model to
// honor instructions. Only an owned local endpoint and synthetic key are used.
func TestCodexCapabilityBoundaryRejectsOutsideImage(t *testing.T) {
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for the installed Codex capability contract")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("install the supported Codex CLI to verify capability enforcement")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	donor := filepath.Join(root, "donor")
	if err := os.Mkdir(donor, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.png")
	file, err := os.Create(outside)
	if err != nil {
		t.Fatal(err)
	}
	picture := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			picture.Set(x, y, color.RGBA{G: 255, A: 255})
		}
	}
	if err := png.Encode(file, picture); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	leaked, denied := false, false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer owned-fake-provider-key" {
			t.Error("fixture received unexpected provider authentication")
			http.Error(w, "fixture authentication mismatch", 403)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		data, _ := json.Marshal(body)
		mu.Lock()
		requests++
		sequence := requests
		leaked = leaked || strings.Contains(string(data), "data:image/")
		denied = denied || strings.Contains(string(data), "unsupported call: view_image")
		mu.Unlock()
		args, _ := json.Marshal(map[string]string{"path": outside})
		item := map[string]any{"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "name": "view_image", "arguments": string(args)}
		if sequence > 1 {
			item = map[string]any{"type": "message", "id": "msg_probe", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "fixture completed", "annotations": []any{}}}}
		}
		responseID := fmt.Sprintf("response_fixture_%d", sequence)
		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": responseID, "status": "in_progress"}},
			{"type": "response.output_item.added", "output_index": 0, "item": item},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"id": responseID, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}},
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			encoded, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
	}))
	defer peer.Close()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	overrides := []string{`model_provider="fixture"`, `model_providers.fixture.name="fixture"`, `model_providers.fixture.base_url="` + peer.URL + `"`, `model_providers.fixture.wire_api="responses"`, `model_providers.fixture.env_key="OPENAI_API_KEY"`}
	script := "#!/bin/sh\nexec " + quote(executable) + ` "$@"`
	for _, value := range overrides {
		script += " -c " + quote(value)
	}
	wrapper := filepath.Join(root, "codex-fixture")
	if err := os.WriteFile(wrapper, []byte(script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: owned-fixture\ndescription: Deterministic owned fixture\n---\nRead only broker evidence.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "owned-fake-provider-key")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := StartRuntime(ctx, RuntimeOptions{Executable: wrapper, AuthHome: donor, TempRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	imageRead := false
	_, err = runtime.Review(ctx, ReviewRequest{Model: "gpt-6.1-sol", Effort: "high", SkillPath: skill, SkillName: "owned-fixture", Prompt: "Read only broker evidence.", Evidence: NewEvidenceBroker(nil, nil), OnProgress: func(message Message) {
		if message.Method != "item/completed" {
			return
		}
		var body struct {
			Item struct{ Type string } `json:"item"`
		}
		if json.Unmarshal(message.Params, &body) == nil && body.Item.Type == "imageView" {
			imageRead = true
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if imageRead || leaked || !denied || requests < 2 {
		t.Fatalf("outside broker image read: event=%t provider_image=%t explicit_denial=%t requests=%d", imageRead, leaked, denied, requests)
	}
}
