package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"io"
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

// Opt-in native fixture: ordinary installed skills, project MCP and permission
// settings with a synthetic local provider, never credentials or customer PRs.
func TestNativeClaudeInheritsSkillsAndMCP(t *testing.T) {
	if os.Getenv("REVIEWD_NATIVE_CLAUDE") != "1" {
		t.Skip("set REVIEWD_NATIVE_CLAUDE=1 for installed Claude/local provider fixture")
	}
	executable, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "claude")
	for _, directory := range []string{workspace, home, filepath.Join(workspace, ".claude")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "mcp.py")
	receipt := filepath.Join(root, "mcp-receipts")
	code := `import json,sys
for line in sys.stdin:
 r=json.loads(line)
 if 'id' not in r: continue
 method=r.get('method')
 if method=='initialize': result={'protocolVersion':r['params']['protocolVersion'],'capabilities':{'tools':{}},'serverInfo':{'name':'reviewd-fixture','version':'1'}}
 elif method=='tools/list': result={'tools':[{'name':'ping','description':'Return harmless owned fixture sentinel','inputSchema':{'type':'object','properties':{}},'annotations':{'readOnlyHint':True,'destructiveHint':False}}]}
 elif method=='tools/call':
  with open(sys.argv[1],'a') as f: f.write('ping\n')
  result={'content':[{'type':'text','text':'REVIEWD_CLAUDE_MCP_SENTINEL'}]}
 else: result={}
 print(json.dumps({'jsonrpc':'2.0','id':r['id'],'result':result}),flush=True)
`
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"command": python, "args": []string{script, receipt}}}})
	if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), mcp, 0600); err != nil {
		t.Fatal(err)
	}
	settings := `{"enableAllProjectMcpServers":true,"permissions":{"allow":["mcp__fixture__ping","ToolSearch"]}}`
	if err := os.WriteFile(filepath.Join(workspace, ".claude", "settings.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"review", "handle-pr-feedback"} {
		directory := filepath.Join(workspace, ".claude", "skills", skill)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		text := "---\nname: " + skill + "\ndescription: Owned local reviewd integration fixture.\n---\nREVIEWD_CLAUDE_SKILL_MARKER_" + skill + "\nCall fixture ping and return the sentinel; do not access the PR.\n"
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The disposable operator already trusts this owned fixture directory.
	// This is native fixture configuration, never a production launcher override.
	trust, _ := json.Marshal(map[string]any{"projects": map[string]any{workspace: map[string]any{"hasTrustDialogAccepted": true, "enabledMcpjsonServers": []string{"fixture"}}}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), trust, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("ANTHROPIC_API_KEY", "owned-local-fixture-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	var mu sync.Mutex
	seenSkills := map[string]bool{}
	seenModelEffort, seenMCP := false, false
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		raw, _ := json.Marshal(body)
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests > 12 {
			http.Error(w, "fixture request bound exceeded", 500)
			return
		}
		for _, skill := range []string{"review", "handle-pr-feedback"} {
			seenSkills[skill] = seenSkills[skill] || strings.Contains(string(raw), "REVIEWD_CLAUDE_SKILL_MARKER_"+skill)
		}
		output, _ := body["output_config"].(map[string]any)
		seenModelEffort = seenModelEffort || (strings.HasPrefix(fmt.Sprint(body["model"]), "claude-sonnet-") && output["effort"] == "high")
		hasReceipt := strings.Contains(string(raw), "tool_result") && strings.Contains(string(raw), "REVIEWD_CLAUDE_MCP_SENTINEL")
		seenMCP = seenMCP || hasReceipt
		stop := "end_turn"
		block := map[string]any{"type": "text", "text": "Local Claude fixture completed."}
		if !hasReceipt {
			name := ""
			if tools, ok := body["tools"].([]any); ok {
				for _, item := range tools {
					tool, _ := item.(map[string]any)
					if candidate, _ := tool["name"].(string); strings.HasPrefix(candidate, "mcp__fixture__") {
						name = candidate
						break
					}
				}
			}
			input := map[string]any{}
			if name == "" {
				// Native Claude defers MCP schemas behind ToolSearch. The synthetic
				// provider must discover the connected tool as a real model would.
				name = "ToolSearch"
				input = map[string]any{"query": "select:mcp__fixture__ping", "max_results": 1}
			}
			stop = "tool_use"
			block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("fixture-call-%d", requests), "name": name, "input": input}
		}
		message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"], "content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(kind string, value map[string]any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
		}
		message["content"] = []any{}
		message["stop_reason"] = nil
		emit("message_start", map[string]any{"type": "message_start", "message": message})
		startBlock := map[string]any{}
		for key, value := range block {
			startBlock[key] = value
		}
		delta := map[string]any{"type": "text_delta", "text": block["text"]}
		if block["type"] == "tool_use" {
			startBlock["input"] = map[string]any{}
			encoded, _ := json.Marshal(block["input"])
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}
		} else {
			startBlock["text"] = ""
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": startBlock})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}})
		emit("message_stop", map[string]any{"type": "message_stop"})
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	sessions := map[string]bool{}
	for _, skill := range []string{"review", "handle-pr-feedback"} {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		activeTool := false
		var nativeStream bytes.Buffer
		output, err := run(ctx, Request{Backend: "claude", Executable: executable, Directory: workspace, Skill: skill, URL: "https://github.com/acme/api/pull/7", Model: "sonnet", Effort: "high", OnProgress: func(o Output) error {
			activeTool = activeTool || (!o.Completed && strings.Contains(o.Activity, "tool_use"))
			return nil
		}}, func(ctx context.Context, c *exec.Cmd) error {
			c.Stdout = io.MultiWriter(c.Stdout, &nativeStream)
			return lifetime.Run(ctx, c)
		})
		cancel()
		if err != nil || !output.Completed || output.ThreadID == "" || output.Text != "Local Claude fixture completed." || output.Model == "" || !activeTool {
			for _, line := range strings.Split(nativeStream.String(), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) == nil && event["subtype"] == "init" {
					t.Logf("Native MCP startup metadata: %v", event["mcp_servers"])
				}
			}
			t.Fatalf("native Claude fixture failed: %+v / %v; activeTool=%v", output, err, activeTool)
		}
		if sessions[output.ThreadID] {
			t.Fatal("native session was reused")
		}
		sessions[output.ThreadID] = true
	}
	mu.Lock()
	defer mu.Unlock()
	actual, err := os.ReadFile(receipt)
	if !seenSkills["review"] || !seenSkills["handle-pr-feedback"] || !seenModelEffort || !seenMCP || err != nil || strings.Count(string(actual), "ping") != 2 {
		t.Fatalf("native inheritance missing: skills=%v model_effort=%v mcp=%v receipt=%q err=%v", seenSkills, seenModelEffort, seenMCP, actual, err)
	}
}
