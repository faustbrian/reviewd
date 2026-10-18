package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/faustbrian/reviewd/internal/activity"
)

// Native message fields follow @anthropic-ai/claude-agent-sdk 0.3.185 sdk.d.ts
// SDKSystemMessage/SDKAssistantMessage/SDKResultMessage, also observed with
// Claude Code 2.1.292. Nested agent messages must not certify the main turn.
func (s *events) consumeClaude(data []byte) {
	var event struct {
		Type    string          `json:"type"`
		Subtype string          `json:"subtype"`
		Session string          `json:"session_id"`
		Model   string          `json:"model"`
		Parent  json.RawMessage `json:"parent_tool_use_id"`
		IsError *bool           `json:"is_error"`
		Result  *string         `json:"result"`
		Errors  []string        `json:"errors"`
		Usage   json.RawMessage `json:"usage"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type == "" {
		s.fail(errors.New("invalid Claude JSONL event"))
		return
	}
	if parent := bytes.TrimSpace(event.Parent); len(parent) > 0 && string(parent) != "null" && string(parent) != `""` {
		return
	}
	if event.Session != "" && s.output.ThreadID != "" && event.Session != s.output.ThreadID {
		s.fail(errors.New("Claude session identity changed"))
		return
	}
	switch event.Type {
	case "system":
		if event.Subtype == "init" {
			if event.Session == "" {
				s.fail(errors.New("Claude session identity is missing"))
				return
			}
			s.output.ThreadID = event.Session
			s.output.Model = event.Model
		}
	case "assistant":
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text":
				s.output.Activity = activity.Clean(block.Text)
			case "tool_use":
				s.output.Activity = "tool_use started"
			case "thinking":
				s.output.Activity = "reasoning"
			}
		}
	case "tool_progress":
		s.output.Activity = "tool_use running"
	case "result":
		if event.Session == "" || s.output.ThreadID == "" {
			s.fail(errors.New("Claude result has no initialized session identity"))
			return
		}
		s.output.Usage = append(json.RawMessage(nil), event.Usage...)
		if event.Result != nil {
			s.output.Text = *event.Result
		}
		if event.Subtype != "success" || event.IsError == nil || *event.IsError || event.Result == nil {
			s.output.Diagnostic = strings.Join(event.Errors, "\n")
			if s.output.Diagnostic == "" && event.Result != nil {
				s.output.Diagnostic = *event.Result
			}
			s.fail(errors.New("Claude did not return a successful native result; inspect the run diagnostics"))
			return
		}
		s.output.Completed = true
	default:
		// Unknown envelopes never grant completion or expose their payload.
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
