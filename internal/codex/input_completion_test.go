package codex

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
)

type receiptBarrierWriter struct {
	io.Writer
	complete chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (w *receiptBarrierWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	var message Message
	_ = json.Unmarshal(data, &message)
	if string(message.ID) == "42" {
		w.once.Do(func() { close(w.complete) })
		<-w.release
	}
	return n, err
}

// A complete answer frame is already consumed by the peer; only its local
// writer return is held. Normal turn completion must settle, not cancel, it.
func TestReviewCompletedTurnSettlesLateClarificationReceipt(t *testing.T) {
	for _, transport := range []string{"native", "dynamic"} {
		t.Run(transport, func(t *testing.T) {
			local, peer := net.Pipe()
			defer local.Close()
			defer peer.Close()
			writer := &receiptBarrierWriter{Writer: local, complete: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(writer.release) }) }
			defer unblock()
			client := NewClient(local, writer)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			skill := filepath.Join(t.TempDir(), "SKILL.md")
			if err := os.WriteFile(skill, []byte("owned inert fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			received := make(chan error, 1)
			go func() {
				decoder, encoder := json.NewDecoder(peer), json.NewEncoder(peer)
				for {
					var message Message
					if err := decoder.Decode(&message); err != nil {
						return
					}
					send := func(body string) { _ = encoder.Encode(Message{ID: message.ID, Result: json.RawMessage(body)}) }
					switch message.Method {
					case "model/list":
						send(`{"data":[{"model":"model","supportedReasoningEfforts":[{"reasoningEffort":"high"}]}]}`)
					case "thread/start":
						send(`{"thread":{"id":"thread"},"model":"model"}`)
					case "turn/start":
						send(`{"turn":{"id":"turn"}}`)
						_ = encoder.Encode(testInputEvent(transport))
					case "turn/interrupt":
						send(`{}`)
					case "":
						if string(message.ID) != "42" {
							continue
						}
						var answer clarification.Answers
						raw := message.Result
						if transport == "dynamic" {
							var envelope struct {
								Success bool
								Content []struct{ Text string } `json:"contentItems"`
							}
							if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.Success || len(envelope.Content) != 1 {
								received <- io.ErrUnexpectedEOF
								return
							}
							raw = []byte(envelope.Content[0].Text)
						}
						err := clarification.Decode(raw, &answer)
						if err != nil || len(answer.Answers) != 1 || strings.Join(answer.Answers["scope"].Answers, ",") != "selected-private-answer" {
							received <- io.ErrUnexpectedEOF
							return
						}
						received <- nil
						_ = encoder.Encode(Message{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","phase":"final_answer","text":"private result"}}`)})
						_ = encoder.Encode(Message{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`)})
					}
				}
			}()
			inputContext := make(chan context.Context, 1)
			completionHandled := make(chan struct{})
			lifecycle := []string{}
			runtime := &Runtime{Client: client, isolation: &Isolation{Workspace: t.TempDir()}}
			type result struct {
				output ReviewOutput
				err    error
			}
			finished := make(chan result, 1)
			go func() {
				output, err := runtime.Review(ctx, ReviewRequest{Model: "model", Effort: "high", SkillPath: skill, SkillName: "fixture", Evidence: NewEvidenceBroker(nil, nil),
					OnInput: func(ctx context.Context, _ clarification.Request) (clarification.Answers, error) {
						inputContext <- ctx
						return clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"selected-private-answer"}}}}, nil
					},
					OnInputEvent: func(_ context.Context, _ clarification.Request, event, _ string) error {
						lifecycle = append(lifecycle, event)
						return nil
					},
					OnProgress: func(event Message) {
						if event.Method == "turn/completed" {
							close(completionHandled)
						}
					},
				})
				finished <- result{output, err}
			}()
			var input context.Context
			select {
			case input = <-inputContext:
			case <-ctx.Done():
				t.Fatal("clarification not opened")
			}
			select {
			case err := <-received:
				if err != nil {
					t.Fatal("incorrect answer reached peer")
				}
			case <-ctx.Done():
				t.Fatal("answer frame not consumed")
			}
			<-writer.complete
			select {
			case <-completionHandled:
			case <-ctx.Done():
				t.Fatal("matching turn completion not handled")
			}
			canceledEarly := false
			select {
			case <-input.Done():
				canceledEarly = true
			case <-time.After(100 * time.Millisecond):
			}
			unblock()
			select {
			case result := <-finished:
				if canceledEarly || result.err != nil || result.output.Text != "private result" {
					t.Fatalf("normal completed turn rejected delivered answer: early_cancel=%v error=%v", canceledEarly, result.err)
				}
			case <-ctx.Done():
				t.Fatal("completed review did not finish")
			}
			if strings.Join(lifecycle, ",") != "opened,written,closed" {
				t.Fatal("successful exact request lifecycle lost", lifecycle)
			}
		})
	}
}

func TestInputNormalCompletionStillRejectsBlockedReply(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	client := NewClient(local, local)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wrote := false
	waits := newInputWaits(ReviewRequest{OnInput: func(context.Context, clarification.Request) (clarification.Answers, error) {
		return clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"selected"}}}}, nil
	}, OnInputEvent: func(_ context.Context, _ clarification.Request, event, _ string) error {
		wrote = wrote || event == "written"
		return nil
	}}, client)
	if err := waits.start(ctx, testInputEvent("dynamic")); err != nil {
		t.Fatal(err)
	}
	if err := waits.result(ctx, <-waits.done); err != nil {
		t.Fatal(err)
	}
	// The peer never reads. Completion is not a receipt and cannot make this
	// send successful. The write's own budget must bound finalization.
	finished := make(chan error, 1)
	go func() { finished <- waits.finish("turn_completed") }()
	select {
	case err := <-finished:
		if err != nil || wrote || !waits.gap {
			t.Fatal("blocked reply was certified written/clean", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-finished
		t.Fatal("blocked reply outlived its transport shutdown budget")
	}
}
