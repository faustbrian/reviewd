// Package agent launches ordinary native CLI sessions. Skills own their workflows.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/faustbrian/reviewd/internal/activity"
	"github.com/faustbrian/reviewd/internal/lifetime"
)

type Request struct {
	Backend                                                        string
	Executable, Directory, Skill, URL, Model, Effort, Instructions string
	Diagnostics                                                    io.Writer
	OnProgress                                                     func(Output) error
}
type Output struct {
	Model      string          `json:"model,omitempty"`
	ThreadID   string          `json:"thread_id,omitempty"`
	Text       string          `json:"text,omitempty"`
	Activity   string          `json:"activity,omitempty"`
	Completed  bool            `json:"completed"`
	Diagnostic string          `json:"diagnostic,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
}

func Label(backend string) string {
	if backend == "claude" {
		return "Claude Code"
	}
	return "Codex"
}

type executor func(context.Context, *exec.Cmd) error

func Run(ctx context.Context, request Request) (Output, error) {
	return run(ctx, request, lifetime.Run)
}

func run(ctx context.Context, request Request, execute executor) (Output, error) {
	if request.Skill == "" || request.URL == "" {
		return Output{}, errors.New("agent launch requires a skill and PR URL")
	}
	if request.Backend == "" {
		request.Backend = "codex"
	}
	if request.Backend != "codex" && request.Backend != "claude" {
		return Output{}, errors.New("unknown agent backend")
	}
	if request.Executable == "" {
		request.Executable = request.Backend
	}
	if request.Model == "" && request.Backend == "codex" {
		request.Model = "gpt-6.1-sol"
	}
	if request.Effort == "" && request.Backend == "codex" {
		request.Effort = "high"
	}
	prompt := "$" + request.Skill + " " + request.URL
	if request.Backend == "claude" {
		prompt = "/" + request.Skill + " " + request.URL
	}
	if request.Instructions != "" {
		prompt += "\n\n" + request.Instructions
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"exec", "--json", "--model", request.Model, "-c", "model_reasoning_effort=" + strconv.Quote(request.Effort), "--skip-git-repo-check", prompt}
	if request.Backend == "claude" {
		// Claude Code CLI reference: print + verbose stream-json loads ordinary
		// skills/MCP/settings. No --bare, permission override or resume flags.
		// https://code.claude.com/docs/en/headless
		args = []string{"--print", "--output-format", "stream-json", "--verbose"}
		if request.Model != "" {
			args = append(args, "--model", request.Model)
		}
		if request.Effort != "" {
			args = append(args, "--effort", request.Effort)
		}
		args = append(args, prompt)
	}
	command := exec.CommandContext(ctx, request.Executable, args...)
	// URL-driven skills may start from a monitoring directory, not a checkout.
	// Codex exec documents --skip-git-repo-check for this case; it does not
	// bypass configured permissions, sandboxing or hook trust.
	command.Dir = request.Directory
	// A nil Env deliberately inherits the normal Codex home, MCP/tool settings,
	// credentials and environment. Do not recreate Codex configuration here.
	diagnostic := &diagnostics{}
	command.Stderr = diagnostic
	if request.Diagnostics != nil {
		command.Stderr = io.MultiWriter(diagnostic, request.Diagnostics)
	}
	stream := &events{backend: request.Backend, progress: request.OnProgress, cancel: cancel}
	command.Stdout = stream
	err := execute(ctx, command)
	stream.finish()
	if text := strings.TrimSpace(string(diagnostic.tail)); text != "" {
		if diagnostic.truncated {
			text = "[Earlier agent diagnostics omitted after the 16 MiB capture limit.]\n" + text
		}
		if stream.output.Diagnostic != "" {
			stream.output.Diagnostic += "\n"
		}
		stream.output.Diagnostic += text
	}
	if err != nil || stream.err != nil {
		stream.output.Completed = false
		return stream.output, errors.Join(err, stream.err)
	}
	if !stream.output.Completed || stream.output.ThreadID == "" {
		stream.output.Completed = false
		return stream.output, fmt.Errorf("%s exited without a completed turn and session identity; inspect the run before retrying", request.Backend)
	}
	return stream.output, nil
}

// Stderr is unframed. Retain at most the JSONL frame budget so observing a
// noisy child does not grow the daemon's memory without bound. This limits only
// stored diagnostics; it neither blocks tools nor interrupts the native run.
type diagnostics struct {
	tail      []byte
	truncated bool
}

func (d *diagnostics) Write(p []byte) (int, error) {
	size := len(p)
	if len(p) >= maxFrame {
		d.tail = append(d.tail[:0], p[len(p)-maxFrame:]...)
		d.truncated = true
		return size, nil
	}
	if overflow := len(d.tail) + len(p) - maxFrame; overflow > 0 {
		copy(d.tail, d.tail[overflow:])
		d.tail = d.tail[:len(d.tail)-overflow]
		d.truncated = true
	}
	d.tail = append(d.tail, p...)
	return size, nil
}

type events struct {
	backend  string
	pending  []byte
	output   Output
	progress func(Output) error
	cancel   context.CancelFunc
	err      error
}

// Match the existing JSONL transport's 16 MiB frame bound. Oversize frames fail
// the run rather than silently truncating an agent response or reporting success.
const maxFrame = 16 << 20

func (s *events) Write(data []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	size := len(data)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		n := len(data)
		if i >= 0 {
			n = i
		}
		if len(s.pending)+n > maxFrame {
			s.fail(errors.New("agent JSONL frame exceeds 16 MiB"))
			return 0, s.err
		}
		s.pending = append(s.pending, data[:n]...)
		if i < 0 {
			break
		}
		s.consume(s.pending)
		s.pending = nil
		if s.err != nil {
			return 0, s.err
		}
		data = data[i+1:]
	}
	return size, nil
}
func (s *events) finish() {
	if len(s.pending) > 0 && s.err == nil {
		s.consume(s.pending)
		s.pending = nil
	}
}
func (s *events) fail(err error) { s.err = err; s.output.Completed = false; s.cancel() }
func (s *events) consume(data []byte) {
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}
	if s.backend == "claude" {
		s.consumeClaude(data)
		return
	}
	var event struct {
		Message  string `json:"message"`
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type == "" {
		s.fail(errors.New("invalid Codex JSONL event"))
		return
	}
	switch event.Type {
	case "thread.started":
		if event.ThreadID == "" || (s.output.ThreadID != "" && event.ThreadID != s.output.ThreadID) {
			s.fail(errors.New("Codex session identity is missing or changed"))
			return
		}
		s.output.ThreadID = event.ThreadID
	case "item.started", "item.updated", "item.completed":
		if event.Item.Type == "agent_message" {
			s.output.Activity = activity.Clean(event.Item.Text)
			if event.Type == "item.completed" {
				s.output.Text = event.Item.Text
			}
		} else {
			switch event.Item.Type {
			case "command_execution", "mcp_tool_call", "web_search", "file_change", "reasoning", "todo_list", "collab_tool_call", "error":
				// Native kinds: openai/codex rust-v0.163.0, exec/src/exec_events.rs
				// ThreadItemDetails (serde snake_case).
				// Tool arguments, command lines and results may contain credentials.
				// Report only the native item kind/phase, not their raw payloads.
				s.output.Activity = activity.Clean(event.Item.Type + " " + strings.TrimPrefix(event.Type, "item."))
			}
		}
	case "turn.completed":
		s.output.Completed = true
		s.output.Usage = append(json.RawMessage(nil), event.Usage...)
	case "error":
		// Codex rust-v0.163.0 maps retryable StreamError to this envelope:
		// app-server/src/bespoke_event_handling.rs ->
		// exec/src/event_processor_with_jsonl_output.rs in openai/codex.
		// Preserve diagnostics, but let Codex own retries and the final outcome.
		s.output.Diagnostic = event.Message
	case "turn.failed":
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Error, &failure)
		if failure.Message != "" {
			s.output.Diagnostic = failure.Message
		}
		s.fail(fmt.Errorf("Codex reported %s; inspect the session for details", event.Type))
		return
	default:
		// Unknown events neither change activity nor certify execution/delivery.
	}
	if s.progress != nil {
		status := s.output
		status.Text = ""
		status.Usage = append(json.RawMessage(nil), s.output.Usage...)
		if err := s.progress(status); err != nil {
			s.fail(err)
		}
	}
}
