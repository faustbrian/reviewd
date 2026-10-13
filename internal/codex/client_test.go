package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// QA-X05: the real JSON-RPC transport must retain response correlation even
// when progress and server requests are interleaved with responses.
func TestClientCorrelatesResponsesAndServerRequests(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	client := NewClient(local, local)
	t.Cleanup(func() { _ = client.Close() })
	go func() {
		decoder, encoder := json.NewDecoder(remote), json.NewEncoder(remote)
		var request Message
		if decoder.Decode(&request) != nil {
			return
		}
		_ = encoder.Encode(Message{Method: "item/tool/call", ID: json.RawMessage(`"read-1"`), Params: json.RawMessage(`{"tool":"reviewd_read_evidence"}`)})
		_ = encoder.Encode(Message{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1"}`)})
		_ = encoder.Encode(Message{ID: request.ID, Result: json.RawMessage(`{"value":"complete"}`)})
		var response Message
		if decoder.Decode(&response) == nil {
			_ = encoder.Encode(Message{Method: "reply_received", Params: response.Result})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result struct {
		Value string `json:"value"`
	}
	if err := client.Call(ctx, "fixture", map[string]string{"scope": "read"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != "complete" {
		t.Fatalf("lost correlated response: %+v", result)
	}
	event := <-client.Events()
	if string(event.ID) != `"read-1"` || event.Method != "item/tool/call" {
		t.Fatalf("lost server request: %+v", event)
	}
	if err := client.Reply(event.ID, map[string]bool{"success": true}); err != nil {
		t.Fatal(err)
	}
	if event := <-client.Events(); event.Method != "turn/started" {
		t.Fatalf("lost progress: %+v", event)
	}
	if event := <-client.Events(); event.Method != "reply_received" || string(event.Params) != `{"success":true}` {
		t.Fatalf("invalid server reply: %+v", event)
	}
}

// QA-18/X05: EOF and protocol errors are failures, never a clean result.
func TestClientSurfacesPeerFailure(t *testing.T) {
	for _, input := range []string{"", "not-json\n"} {
		t.Run(input, func(t *testing.T) {
			client := NewClient(strings.NewReader(input), io.Discard)
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.Call(ctx, "fixture", nil, nil); err == nil {
				t.Fatal("peer failure became success")
			}
		})
	}
}

func TestClientRetainsRPCErrorAndCancellation(t *testing.T) {
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	client := NewClient(local, local)
	t.Cleanup(func() { _ = client.Close() })
	go func() {
		var request Message
		if json.NewDecoder(remote).Decode(&request) == nil {
			_ = json.NewEncoder(remote).Encode(Message{ID: request.ID, Error: &RPCError{Code: -32601, Message: "unsupported capability"}})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := client.Call(ctx, "unsupported", nil, nil)
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.Code != -32601 {
		t.Fatalf("lost protocol error: %v", err)
	}
	closed, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := client.Call(closed, "never-start", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestClientAcceptsCompleteResponseBeforePeerEOF(t *testing.T) {
	for range 30 {
		local, remote := net.Pipe()
		client := NewClient(local, local)
		go func() {
			defer remote.Close()
			var request Message
			if json.NewDecoder(remote).Decode(&request) == nil {
				_ = json.NewEncoder(remote).Encode(Message{ID: request.ID, Result: json.RawMessage(`{"value":"received"}`)})
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var result struct {
			Value string `json:"value"`
		}
		err := client.Call(ctx, "fixture", nil, &result)
		cancel()
		_ = client.Close()
		if err != nil || result.Value != "received" {
			t.Fatalf("complete response lost to EOF: %v %+v", err, result)
		}
	}
}

type observedWriter struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (w *observedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.Conn.Write(data)
}

func TestCancelledWritesUnblockWithoutPeerReading(t *testing.T) {
	for _, operation := range []string{"reply", "reject", "notify"} {
		t.Run(operation, func(t *testing.T) {
			local, remote := net.Pipe()
			t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
			writer := &observedWriter{Conn: local, entered: make(chan struct{})}
			client := NewClient(local, writer)
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				switch operation {
				case "reply":
					returned <- client.ReplyContext(ctx, json.RawMessage(`"request"`), map[string]bool{"success": false})
				case "reject":
					returned <- client.RejectContext(ctx, json.RawMessage(`"request"`), &RPCError{Code: -32601, Message: "denied"})
				case "notify":
					returned <- client.NotifyContext(ctx, "initialized", map[string]any{})
				}
			}()
			<-writer.entered
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost write cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled write remains blocked until explicit Close")
			}
		})
	}
}
