package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in, credential-free integration: the real installed Codex loads an
// ordinary operator config and installed skills, then calls a harmless MCP.
// This does not test termination, signal groups, customer PRs or live providers.
func TestNativeCodexInheritsSkillsAndMCP(t *testing.T) {
	if os.Getenv("REVIEWD_NATIVE_AGENT") != "1" {
		t.Skip("set REVIEWD_NATIVE_AGENT=1 for the installed Codex/local MCP fixture")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "mcp.py")
	code := `import json,os,sys
for line in sys.stdin:
    request=json.loads(line)
    method=request.get("method")
    if "id" not in request: continue
    if method=="initialize":
        result={"protocolVersion":request["params"]["protocolVersion"],"capabilities":{"tools":{}},"serverInfo":{"name":"reviewd-fixture","version":"1"}}
    elif method=="tools/list":
        result={"tools":[{"name":"ping","description":"Return the private reviewd fixture sentinel.","inputSchema":{"type":"object","properties":{},"additionalProperties":False},"annotations":{"readOnlyHint":True,"destructiveHint":False}}]}
    elif method=="tools/call":
        result={"content":[{"type":"text","text":os.environ["REVIEWD_NATIVE_MCP_SENTINEL"]}]}
    else:
        result={}
    print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":result}),flush=True)
`
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"review", "handle-pr-feedback"} {
		directory := filepath.Join(home, "skills", skill)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		text := "---\nname: " + skill + "\ndescription: Owned local reviewd integration fixture.\n---\nREVIEWD_SKILL_MARKER_" + skill + "\nCall the fixture ping tool and return its text. Do not access the PR.\n"
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", home)
	t.Setenv("REVIEWD_NATIVE_PROVIDER_KEY", "owned-local-provider-key")
	t.Setenv("REVIEWD_NATIVE_MCP_SENTINEL", "owned-mcp-response")
	var mu sync.Mutex
	seenSkills := map[string]bool{}
	toolCalled := false
	mcpSessions := map[string]bool{}
	nativeSessions := map[string]bool{}
	modelEffortSeen := false
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		data, _ := json.Marshal(body)
		raw := string(data)
		mu.Lock()
		defer mu.Unlock()
		requests++
		for _, skill := range []string{"review", "handle-pr-feedback"} {
			if strings.Contains(raw, "REVIEWD_SKILL_MARKER_"+skill) {
				seenSkills[skill] = true
			}
		}
		if body["model"] == "gpt-6.1-sol" && strings.Contains(raw, `"effort":"high"`) {
			modelEffortSeen = true
		}
		toolCalled = toolCalled || strings.Contains(raw, "owned-mcp-response")
		if key, _ := body["prompt_cache_key"].(string); key != "" && strings.Contains(raw, "owned-mcp-response") {
			mcpSessions[key] = true
		}
		var toolName, toolNamespace, execNamespace string
		var shapes []string
		var visit func(any, string)
		visit = func(value any, namespace string) {
			switch node := value.(type) {
			case []any:
				for _, child := range node {
					visit(child, namespace)
				}
			case map[string]any:
				kind, _ := node["type"].(string)
				name, _ := node["name"].(string)
				if name != "" {
					shapes = append(shapes, namespace+"/"+kind+":"+name)
				}
				if kind == "namespace" {
					visit(node["tools"], name)
					return
				}
				if kind == "custom" && name == "exec" {
					execNamespace = namespace
				}
				if (strings.Contains(name, "reviewd_fixture") && strings.Contains(name, "ping")) || (strings.Contains(namespace, "reviewd_fixture") && name == "ping") {
					toolName, toolNamespace = name, namespace
				}
				visit(node["tools"], namespace)
			}
		}
		visit(body["tools"], "")
		// Codex 0.163's Responses Lite carries tools in AdditionalTools input
		// items, not the root tools field (openai/codex rust-v0.163.0,
		// codex-rs/core/src/client.rs build_responses_request).
		if inputs, ok := body["input"].([]any); ok {
			for _, input := range inputs {
				if row, ok := input.(map[string]any); ok {
					visit(row["tools"], "")
				}
			}
		}
		if requests == 1 {
			t.Logf("Native tool names/types: %v", shapes)
		}
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", requests), "call_id": fmt.Sprintf("call_%d", requests), "name": toolName, "arguments": "{}"}
		if toolNamespace != "" {
			item["namespace"] = toolNamespace
		}
		if toolName == "" && execNamespace != "" {
			item = map[string]any{"type": "custom_tool_call", "id": fmt.Sprintf("exec_%d", requests), "call_id": fmt.Sprintf("call_%d", requests), "namespace": execNamespace, "name": "exec", "input": "text(await tools.mcp__reviewd_fixture__ping({}));"}
		}
		if (toolName == "" && execNamespace == "") || requests > 8 || strings.Contains(raw, "owned-mcp-response") {
			item = map[string]any{"type": "message", "id": fmt.Sprintf("msg_%d", requests), "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "Local fixture completed.", "annotations": []any{}}}}
		}
		id := fmt.Sprintf("response_%d", requests)
		events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress"}}, {"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}}}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			encoded, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
	}))
	defer server.Close()
	configuration := fmt.Sprintf(`model_provider="fixture"
approval_policy="never"
sandbox_mode="read-only"
[model_providers.fixture]
name="fixture"
base_url=%s
wire_api="responses"
env_key="REVIEWD_NATIVE_PROVIDER_KEY"
[mcp_servers.reviewd_fixture]
command=%s
args=[%s]
env_vars=["REVIEWD_NATIVE_MCP_SENTINEL"]
`, strconv.Quote(server.URL), strconv.Quote(python), strconv.Quote(script))
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"review", "handle-pr-feedback"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var diagnostic strings.Builder
		seenActivity, seenTool := false, false
		output, err := Run(ctx, Request{Executable: executable, Directory: workspace, Skill: skill, URL: "https://github.com/fixture/fixture/pull/1", Model: "gpt-6.1-sol", Effort: "high", Diagnostics: &diagnostic, OnProgress: func(progress Output) error {
			if !progress.Completed && progress.Activity != "" {
				seenActivity = true
				seenTool = seenTool || strings.Contains(progress.Activity, "mcp_tool_call") || strings.Contains(progress.Activity, "command_execution")
			}
			return nil
		}})
		cancel()
		if !seenActivity || !seenTool {
			t.Fatalf("installed Codex did not expose running tool activity: activity=%t tool=%t", seenActivity, seenTool)
		}
		if nativeSessions[output.ThreadID] {
			t.Fatal("skill launch resumed a previous session instead of starting fresh")
		}
		nativeSessions[output.ThreadID] = true
		if err != nil || !output.Completed || output.ThreadID == "" || output.Text != "Local fixture completed." {
			t.Fatalf("native skill execution failed: %+v error=%v diagnostic=%s", output, err, diagnostic.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !seenSkills["review"] || !seenSkills["handle-pr-feedback"] || !toolCalled || !modelEffortSeen || len(mcpSessions) != 2 {
		t.Fatalf("normal runtime inheritance missing: skills=%v mcp=%t model_effort=%t", seenSkills, toolCalled, modelEffortSeen)
	}
}
