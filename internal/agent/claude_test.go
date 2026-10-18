package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeNativeSkillLaunchAndSuccess(t *testing.T) {
	var request Request
	// Decode at the request boundary so the initial red run demonstrates missing
	// backend behavior rather than failing to compile on a not-yet-added field.
	if err := json.Unmarshal([]byte(`{"Backend":"claude","Executable":"claude-fixture","Skill":"handle-pr-feedback","URL":"https://github.com/acme/api/pull/7","Model":"sonnet","Effort":"high"}`), &request); err != nil {
		t.Fatal(err)
	}
	got, err := run(context.Background(), request, func(_ context.Context, c *exec.Cmd) error {
		want := []string{"claude-fixture", "--print", "--output-format", "stream-json", "--verbose", "--model", "sonnet", "--effort", "high", "/handle-pr-feedback https://github.com/acme/api/pull/7"}
		if !reflect.DeepEqual(c.Args, want) {
			t.Fatalf("native Claude invocation: got %q want %q", c.Args, want)
		}
		if c.Env != nil {
			t.Fatal("native environment was replaced")
		}
		_, err := io.WriteString(c.Stdout, "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"claude-fresh\",\"model\":\"sonnet\"}\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"claude-fresh\",\"result\":\"Skill finished\",\"usage\":{\"input_tokens\":2}}\n")
		return err
	})
	if string(got.Usage) != `{"input_tokens":2}` {
		t.Fatalf("native usage lost: %s", got.Usage)
	}
	if err != nil || !got.Completed || got.ThreadID != "claude-fresh" || got.Text != "Skill finished" {
		t.Fatalf("native outcome: %+v / %v", got, err)
	}
}

func TestClaudeResultPartitions(t *testing.T) {
	const init = `{"type":"system","subtype":"init","session_id":"main"}` + "\n"
	const success = `{"type":"result","subtype":"success","is_error":false,"session_id":"main","result":"done","usage":{"input_tokens":4}}`
	cases := []struct {
		name, frames string
		exit         bool
		want         bool
	}{
		{"success without newline", init + success, false, true},
		{"result without init", success, false, false},
		{"missing result", init, false, false},
		{"native error with zero exit", init + `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"main","errors":["authentication failed"]}`, false, false},
		{"success with failure flag", init + `{"type":"result","subtype":"success","is_error":true,"session_id":"main","result":"failed"}`, false, false},
		{"missing error flag", init + `{"type":"result","subtype":"success","session_id":"main","result":"done"}`, false, false},
		{"changed session", init + `{"type":"result","subtype":"success","is_error":false,"session_id":"other","result":"done"}`, false, false},
		{"nested completion only", init + `{"type":"result","subtype":"success","is_error":false,"session_id":"child","parent_tool_use_id":"agent-call","result":"done"}`, false, false},
		{"malformed trailing frame", init + success + "\n{", false, false},
		{"successful result nonzero exit", init + success, true, false},
		{"retry then success", init + `{"type":"system","subtype":"api_retry","session_id":"main","attempt":1}` + "\n" + success, false, true},
		{"nested messages then success", init + `{"type":"assistant","session_id":"child","parent_tool_use_id":"agent-call","message":{"content":[{"type":"text","text":"nested"}]}}` + "\n" + success, false, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := run(context.Background(), Request{Backend: "claude", Skill: "review", URL: "https://github.com/acme/api/pull/7"}, func(_ context.Context, c *exec.Cmd) error {
				for _, chunk := range []string{tt.frames[:len(tt.frames)/2], tt.frames[len(tt.frames)/2:]} {
					if _, err := io.WriteString(c.Stdout, chunk); err != nil {
						return err
					}
				}
				if tt.exit {
					return fmt.Errorf("exit status 1")
				}
				return nil
			})
			if (err == nil) != tt.want || got.Completed != tt.want {
				t.Fatalf("native outcome %+v / %v, want success=%t", got, err, tt.want)
			}
		})
	}
}

func TestClaudeProgressIsRedactedAndCannotCertifyCompletion(t *testing.T) {
	canary := "sk-ant-api03-" + strings.Repeat("X", 35)
	var seen []Output
	got, err := run(context.Background(), Request{Backend: "claude", Skill: "review", URL: "https://github.com/acme/api/pull/7", OnProgress: func(o Output) error { seen = append(seen, o); return nil }}, func(_ context.Context, c *exec.Cmd) error {
		frames := []string{`{"type":"system","subtype":"init","session_id":"main"}`, `{"type":"assistant","session_id":"main","message":{"content":[{"type":"tool_use","input":{"private":"secret-tool-argument"}}]}}`, `{"type":"assistant","session_id":"main","message":{"content":[{"type":"text","text":"Inspecting ` + canary + `\u001b[31m"}]}}`}
		for _, frame := range frames {
			if _, err := io.WriteString(c.Stdout, frame+"\n"); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil || got.Completed {
		t.Fatal("partial progress became success")
	}
	raw, _ := json.Marshal(seen)
	if !strings.Contains(string(raw), "tool_use started") || !strings.Contains(string(raw), "redacted") || strings.Contains(string(raw), canary) || strings.Contains(string(raw), "secret-tool-argument") {
		t.Fatalf("unsafe/missing progress: %s", raw)
	}
}

func TestNativeJSONLFrameLimit(t *testing.T) {
	for _, backend := range []string{"codex", "claude"} {
		for _, size := range []int{16 << 20, (16 << 20) + 1} {
			t.Run(fmt.Sprintf("%s/%d", backend, size), func(t *testing.T) {
				head, tail := `{"type":"future","padding":"`, `"}`
				frame := head + strings.Repeat("x", size-len(head)-len(tail)) + tail
				stream := &events{backend: backend, cancel: func() {}}
				_, err := stream.Write([]byte(frame[:len(frame)/2]))
				if err != nil {
					t.Fatal(err)
				}
				_, err = stream.Write([]byte(frame[len(frame)/2:] + "\n"))
				if (err != nil) != (size > 16<<20) {
					t.Fatalf("frame size %d: %v", size, err)
				}
			})
		}
	}
}

func TestClaudeNativeFailureRetainsDiagnosticsAndUsage(t *testing.T) {
	got, err := run(context.Background(), Request{Backend: "claude", Skill: "review", URL: "https://github.com/acme/api/pull/7"}, func(_ context.Context, c *exec.Cmd) error {
		_, err := io.WriteString(c.Stdout, "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"main\"}\n{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"session_id\":\"main\",\"errors\":[\"authentication failed\"],\"usage\":{\"input_tokens\":7}}\n")
		return err
	})
	if err == nil || got.Completed || got.Diagnostic != "authentication failed" || string(got.Usage) != `{"input_tokens":7}` {
		t.Fatalf("native failure payload lost: %+v / %v", got, err)
	}
}
