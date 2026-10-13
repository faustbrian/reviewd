package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test executable is a real stdio peer. No shell or installed provider is
// needed, and assertions cross the subprocess/JSONL boundary (QA-18/25/26/X05).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		fakePeer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakePeer() {
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	var activeTurn map[string]any
	threadID := "thread-fixture"
	threadCount := 0
	for {
		var request Message
		if decoder.Decode(&request) != nil {
			return
		}
		response := func(value any) {
			data, _ := json.Marshal(value)
			_ = encoder.Encode(Message{ID: request.ID, Result: data})
		}
		switch request.Method {
		case "initialize":
			response(map[string]any{"userAgent": "reviewd-test-peer/1"})
		case "initialized":
		case "model/list":
			response(map[string]any{"data": []any{map[string]any{"model": "gpt-6.1-sol", "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}}}}, "nextCursor": nil})
		case "thread/start":
			threadCount++
			if threadCount > 1 {
				threadID = fmt.Sprintf("thread-fixture-%d", threadCount)
			}
			response(map[string]any{"thread": map[string]string{"id": threadID}, "model": "gpt-6.1-sol"})
		case "turn/start":
			_ = json.Unmarshal(request.Params, &activeTurn)
			response(map[string]any{"turn": map[string]string{"id": "turn-fixture", "status": "inProgress"}})
			params, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": "turn-fixture", "callId": "source-1", "tool": "reviewd_read_source", "arguments": map[string]string{"revision": "head", "path": "main.go"}})
			_ = encoder.Encode(Message{ID: json.RawMessage(`"source-1"`), Method: "item/tool/call", Params: params})
		case "turn/interrupt":
			response(map[string]any{})
		default:
			if request.Method == "" && string(request.ID) == `"source-1"` {
				if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("SSH_AUTH_SOCK") != "" {
					return
				}
				usage, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": "turn-fixture", "tokenUsage": map[string]any{"total": map[string]int{"totalTokens": 41}}})
				_ = encoder.Encode(Message{Method: "thread/tokenUsage/updated", Params: usage})
				stale, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": "previous-turn", "tokenUsage": map[string]any{"total": map[string]int{"totalTokens": 999}}})
				_ = encoder.Encode(Message{Method: "thread/tokenUsage/updated", Params: stale})
				_ = encoder.Encode(Message{Method: "thread/tokenUsage/updated", Params: json.RawMessage(`{"threadId":"previous-thread","turnId":"previous-turn","tokenUsage":{"total":999}}`)})
				data, _ := json.Marshal(map[string]any{"type": "agentMessage", "phase": "final_answer", "text": "prepared fixture", "observed": activeTurn})
				params, _ := json.Marshal(map[string]any{"threadId": threadID, "turnId": "turn-fixture", "item": json.RawMessage(data)})
				_ = encoder.Encode(Message{Method: "item/completed", Params: params})
				params, _ = json.Marshal(map[string]any{"threadId": threadID, "turn": map[string]string{"id": "turn-fixture", "status": "completed"}})
				_ = encoder.Encode(Message{Method: "turn/completed", Params: params})
			}
		}
	}
}

func testRuntimeOptions(t *testing.T) RuntimeOptions {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeOptions{Executable: executable, AuthHome: home, TempRoot: t.TempDir()}
}

func TestAccountDiagnosticsRejectMissingAuthenticationWithoutStartingTurn(t *testing.T) {
	for _, response := range []string{`{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`, `{"account":null,"requiresOpenaiAuth":true}`, `{}`} {
		left, right := net.Pipe()
		client := NewClient(left, left)
		done := make(chan Message, 1)
		go func() {
			defer right.Close()
			var request Message
			_ = json.NewDecoder(right).Decode(&request)
			done <- request
			_ = json.NewEncoder(right).Encode(Message{ID: request.ID, Result: json.RawMessage(response)})
		}()
		worker := &Runtime{Client: client}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := worker.CheckAccount(ctx)
		cancel()
		_ = client.Close()
		request := <-done
		if request.Method != "account/read" || !strings.Contains(string(request.Params), `"refreshToken":false`) {
			t.Fatalf("diagnostics did not use account discovery: %+v", request)
		}
		if (response == `{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}`) != (err == nil) {
			t.Fatalf("account evidence incorrectly classified: %s %v", response, err)
		}
	}
}

func TestSessionRunsPrivateSkillWithExplicitModelAndEffort(t *testing.T) {
	t.Setenv("GH_TOKEN", "publisher-canary")
	t.Setenv("GITHUB_TOKEN", "publisher-canary")
	t.Setenv("SSH_AUTH_SOCK", "publisher-agent")
	opts := testRuntimeOptions(t)
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("---\nname: requesting-code-review\ndescription: fixture review\n---\nPerform a private review.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runtime, err := StartRuntime(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var observed map[string]any
	request := ReviewRequest{Model: "gpt-6.1-sol", Effort: "high", SkillPath: skill, SkillName: "requesting-code-review", Prompt: "Review pinned fixture", Evidence: NewEvidenceBroker(map[string]map[string]string{"head": {"main.go": "immutable fixture"}}, nil), OnProgress: func(message Message) {
		if message.Method != "item/completed" {
			return
		}
		var event struct {
			Item struct {
				Observed map[string]any `json:"observed"`
			} `json:"item"`
		}
		if json.Unmarshal(message.Params, &event) == nil && event.Item.Observed != nil {
			observed = event.Item.Observed
		}
	}}
	result, err := runtime.Review(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "prepared fixture" || result.ThreadID != "thread-fixture" || result.TurnID != "turn-fixture" {
		t.Fatalf("lost completed result: %+v", result)
	}
	if !strings.Contains(string(result.Usage), `"totalTokens":41`) || strings.Contains(string(result.Usage), "999") {
		t.Fatalf("foreign thread contaminated current usage: %s", result.Usage)
	}
	if observed["model"] != "gpt-6.1-sol" || observed["effort"] != "high" {
		t.Fatalf("model override didn't reach peer: %+v", observed)
	}
	encoded, _ := json.Marshal(observed)
	if !strings.Contains(string(encoded), `"networkAccess":false`) || !strings.Contains(string(encoded), `"type":"readOnly"`) || !strings.Contains(string(encoded), `"type":"skill"`) || !strings.Contains(string(encoded), "do not post") {
		t.Fatalf("private skill boundary not on wire: %s", encoded)
	}
	second, err := runtime.Review(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if second.ThreadID == result.ThreadID || !strings.Contains(string(second.Usage), `"totalTokens":41`) || strings.Contains(string(second.Usage), "999") {
		t.Fatalf("reused runtime mixed review identities or usage: %+v", second)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(opts.TempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runtime cleanup failed: %v %v", entries, err)
	}
}

func TestSessionRejectsUnsupportedEffortBeforeReview(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runtime, err := StartRuntime(ctx, testRuntimeOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	_, err = runtime.Review(ctx, ReviewRequest{Model: "gpt-6.1-sol", Effort: "unsupported"})
	if err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("unsupported effort accepted: %v", err)
	}
}

func TestRuntimeStartupFailureRemovesOwnedDirectory(t *testing.T) {
	opts := testRuntimeOptions(t)
	opts.Executable = filepath.Join(t.TempDir(), "missing-codex")
	if runtime, err := StartRuntime(context.Background(), opts); err == nil {
		runtime.Close()
		t.Fatal("missing executable became success")
	}
	entries, err := os.ReadDir(opts.TempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("startup failure leaked resources: %v %v", entries, err)
	}
}

func TestServerCannotUseAnotherThreadEvidence(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	client := NewClient(local, local)
	t.Cleanup(func() { _ = client.Close() })
	runtime := &Runtime{Client: client}
	response := make(chan Message, 1)
	go func() { var message Message; _ = json.NewDecoder(remote).Decode(&message); response <- message }()
	event := Message{ID: json.RawMessage(`"cross-thread"`), Method: "item/tool/call", Params: json.RawMessage(`{"threadId":"other-thread","turnId":"other-turn","tool":"reviewd_read_evidence","arguments":{"key":"private"}}`)}
	err := runtime.handleRequest(context.Background(), event, ReviewRequest{Evidence: NewEvidenceBroker(nil, map[string]string{"private": "frozen-current-context"})})
	if err != nil {
		t.Fatal(err)
	}
	message := <-response
	if message.Error == nil || strings.Contains(string(message.Result), "frozen-current-context") {
		t.Fatalf("foreign thread accessed current evidence: %+v", message)
	}
}

func TestReviewRequestReplyCancellation(t *testing.T) {
	for _, method := range []string{"item/tool/call", "item/commandExecution/requestApproval", "item/permissions/requestApproval", "unknown/capability"} {
		t.Run(method, func(t *testing.T) {
			local, remote := net.Pipe()
			t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
			writer := &observedWriter{Conn: local, entered: make(chan struct{})}
			client := NewClient(local, writer)
			t.Cleanup(func() { _ = client.Close() })
			runtime := &Runtime{Client: client, activeThread: "current", activeTurn: "turn"}
			event := Message{ID: json.RawMessage(`"blocked"`), Method: method, Params: json.RawMessage(`{"threadId":"current","turnId":"turn","tool":"reviewd_read_evidence","arguments":{"key":"fixture"}}`)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- runtime.handleRequest(ctx, event, ReviewRequest{Evidence: NewEvidenceBroker(nil, map[string]string{"fixture": "private"})})
			}()
			select {
			case <-writer.entered:
			case <-time.After(time.Second):
				t.Fatal("reply was not attempted")
			}
			cancel()
			select {
			case err := <-done:
				if err != context.Canceled {
					t.Fatalf("cancelled request: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request handler remained blocked")
			}
		})
	}
}
