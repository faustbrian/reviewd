package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type reviewDelayedWriter struct {
	net.Conn
	release chan struct{}
	once    sync.Once
}

func (w *reviewDelayedWriter) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	var message Message
	_ = json.Unmarshal(p, &message)
	// Data is already delivered. Delay only the goroutine's write-return
	// bookkeeping, a legal order for independent reader/writer goroutines.
	if string(message.ID) == `"delegate"` {
		<-w.release
	}
	return n, err
}
func (w *reviewDelayedWriter) Close() error {
	w.once.Do(func() { close(w.release) })
	return w.Conn.Close()
}
func TestCompletedSpecialistReplyBeforeWriterReturns(t *testing.T) {
	left, right := net.Pipe()
	writer := &reviewDelayedWriter{Conn: left, release: make(chan struct{})}
	client := NewClient(left, writer)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		decoder, encoder := json.NewDecoder(right), json.NewEncoder(right)
		for {
			var message Message
			if decoder.Decode(&message) != nil {
				return
			}
			send := func(value string) { _ = encoder.Encode(Message{ID: message.ID, Result: json.RawMessage(value)}) }
			switch message.Method {
			case "model/list":
				send(`{"data":[{"model":"model","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}`)
			case "thread/start":
				send(`{"thread":{"id":"thread"},"model":"model"}`)
			case "turn/start":
				send(`{"turn":{"id":"turn"}}`)
				_ = encoder.Encode(Message{ID: json.RawMessage(`"delegate"`), Method: "item/tool/call", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","tool":"reviewd_delegate_specialist","arguments":{"task":"audit source","paths":["main.go"],"effort":"high"}}`)})
			case "turn/interrupt":
				send(`{}`)
			case "":
				if string(message.ID) != `"delegate"` {
					continue
				}
				var response struct {
					Success bool `json:"success"`
				}
				_ = json.Unmarshal(message.Result, &response)
				if !response.Success {
					return
				}
				_ = encoder.Encode(Message{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","phase":"final_answer","text":"private result"}}`)})
				_ = encoder.Encode(Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)})
				time.AfterFunc(30*time.Millisecond, func() { writer.once.Do(func() { close(writer.release) }) })
			}
		}
	}()
	runtime := &Runtime{Client: client, isolation: &Isolation{Workspace: t.TempDir()}}
	output, err := runtime.Review(ctx, ReviewRequest{
		Model: "model", Effort: "high", SkillPath: skill, SkillName: "fixture",
		Evidence: NewEvidenceBroker(nil, nil),
		OnSpecialist: func(context.Context, SpecialistRequest) (SpecialistOutput, error) {
			return SpecialistOutput{Text: "completed leaf", ThreadID: "leaf", TurnID: "leaf-turn"}, nil
		},
	})
	if err != nil {
		t.Fatalf("completed leaf and received successful response incorrectly rejected: %v; sealed reports=%d", err, len(output.Specialists))
	}
	if output.Text != "private result" || len(output.Specialists) != 1 || output.Specialists[0].ThreadID != "leaf" {
		t.Fatal("completed primary result or sealed specialist identity was lost")
	}
}

func TestPrimaryCompletionCannotCertifyAnUnfinishedSpecialist(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	skill := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	go func() {
		decoder, encoder := json.NewDecoder(right), json.NewEncoder(right)
		for {
			var message Message
			if decoder.Decode(&message) != nil {
				return
			}
			send := func(value string) { _ = encoder.Encode(Message{ID: message.ID, Result: json.RawMessage(value)}) }
			switch message.Method {
			case "model/list":
				send(`{"data":[{"model":"model","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}`)
			case "thread/start":
				send(`{"thread":{"id":"thread"},"model":"model"}`)
			case "turn/start":
				send(`{"turn":{"id":"turn"}}`)
				_ = encoder.Encode(Message{ID: json.RawMessage(`"delegate"`), Method: "item/tool/call", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","tool":"reviewd_delegate_specialist","arguments":{"task":"audit source","paths":["main.go"],"effort":"high"}}`)})
				select {
				case <-started:
				case <-ctx.Done():
					return
				}
				_ = encoder.Encode(Message{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","phase":"final_answer","text":"premature result"}}`)})
				_ = encoder.Encode(Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)})
			case "turn/interrupt":
				send(`{}`)
			}
		}
	}()
	runtime := &Runtime{Client: client, isolation: &Isolation{Workspace: t.TempDir()}}
	_, err := runtime.Review(ctx, ReviewRequest{Model: "model", Effort: "high", SkillPath: skill, SkillName: "fixture", Evidence: NewEvidenceBroker(nil, nil),
		OnSpecialist: func(ctx context.Context, _ SpecialistRequest) (SpecialistOutput, error) {
			close(started)
			<-ctx.Done()
			return SpecialistOutput{}, ctx.Err()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "review completed before its specialist returned") {
		t.Fatalf("unfinished specialist did not prevent a clean result: %v", err)
	}
}
