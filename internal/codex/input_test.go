package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
)

func testInputEvent(transport string) Message {
	params := map[string]any{"threadId": "thread", "turnId": "turn", "itemId": "item", "isBlocking": true, "questions": []any{map[string]string{"id": "scope", "header": "Scope", "question": "Which?"}}}
	method := "item/tool/requestUserInput"
	if transport == "dynamic" {
		method = "item/tool/call"
		params = map[string]any{"threadId": "thread", "turnId": "turn", "callId": "item", "tool": "reviewd_request_input", "arguments": map[string]any{"questions": params["questions"]}}
	}
	raw, _ := json.Marshal(params)
	return Message{ID: json.RawMessage(`42`), Method: method, Params: raw}
}
func TestInputWaitConsumesServerClearAndRejectsStaleReply(t *testing.T) {
	for _, transport := range []string{"native", "dynamic"} {
		t.Run(transport, func(t *testing.T) {
			left, right := net.Pipe()
			client := NewClient(left, left)
			defer client.Close()
			defer right.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			events := []string{}
			waits := newInputWaits(ReviewRequest{OnInput: func(ctx context.Context, r clarification.Request) (clarification.Answers, error) {
				<-ctx.Done()
				return clarification.Answers{}, ctx.Err()
			}, OnInputEvent: func(ctx context.Context, r clarification.Request, event, reason string) error {
				events = append(events, event+":"+reason)
				return nil
			}}, client)
			if err := waits.start(ctx, testInputEvent(transport)); err != nil {
				t.Fatal(err)
			}
			if err := waits.resolved(ctx, Message{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"other-thread","requestId":42}`)}); err != nil || waits.jobs["42"].closed {
				t.Fatal("stale clear closed current question")
			}
			if err := waits.resolved(ctx, Message{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":42}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-waits.done:
				if err := waits.result(ctx, result); err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("clear did not interrupt input wait")
			}
			if !waits.gap || strings.Join(events, ",") != "opened:,closed:server_cleared" {
				t.Fatal("clear manufactured answer or lost lifecycle")
			}
			_ = right.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			var b [1]byte
			if n, _ := right.Read(b[:]); n != 0 {
				t.Fatal("stale reply was written after clear")
			}
			if err := waits.finish("session_ended"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestInputReplyCancellationPreservesUnknownDelivery(t *testing.T) {
	for _, transport := range []string{"native", "dynamic"} {
		t.Run(transport, func(t *testing.T) {
			left, right := net.Pipe()
			client := NewClient(left, left)
			defer client.Close()
			defer right.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			written := false
			waits := newInputWaits(ReviewRequest{OnInput: func(context.Context, clarification.Request) (clarification.Answers, error) {
				return clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"private-sentinel"}}}}, nil
			}, OnInputEvent: func(ctx context.Context, r clarification.Request, event, reason string) error {
				written = written || event == "written"
				return nil
			}}, client)
			if err := waits.start(ctx, testInputEvent(transport)); err != nil {
				t.Fatal(err)
			}
			result := <-waits.done
			if err := waits.result(ctx, result); err != nil {
				t.Fatal(err)
			}
			// The peer deliberately never reads the write. A clear must still be
			// consumed and cancel the blocked transport, without claiming acceptance.
			if err := waits.resolved(ctx, Message{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":42}`)}); err != nil {
				t.Fatal(err)
			}
			if err := waits.finish("session_ended"); err != nil {
				t.Fatal(err)
			}
			if written || !waits.gap {
				t.Fatal("blocked/cleared reply was reported written or clean")
			}
		})
	}
}

func TestInputSuccessfulWriteFactSurvivesEarlierProcessedClear(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wroteFact := false
	waits := newInputWaits(ReviewRequest{OnInput: func(context.Context, clarification.Request) (clarification.Answers, error) {
		return clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"sent"}}}}, nil
	}, OnInputEvent: func(ctx context.Context, r clarification.Request, event, reason string) error {
		wroteFact = wroteFact || event == "written"
		return nil
	}}, client)
	if err := waits.start(ctx, testInputEvent("native")); err != nil {
		t.Fatal(err)
	}
	if err := waits.result(ctx, <-waits.done); err != nil {
		t.Fatal(err)
	}
	var reply Message
	if json.NewDecoder(right).Decode(&reply) != nil {
		t.Fatal("reply not written")
	}
	written := <-waits.done
	if written.err != nil || !written.writing {
		t.Fatal("fixture did not complete write")
	}
	// The host's clear can be selected before a queued successful write result.
	// Preserve the completed write fact even though acceptance remains uncertain.
	if err := waits.resolved(ctx, Message{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":42}`)}); err != nil {
		t.Fatal(err)
	}
	if err := waits.result(ctx, written); err != nil {
		t.Fatal(err)
	}
	if !wroteFact || !waits.jobs["42"].written {
		t.Fatal("completed transport write was lost because the clear was processed first")
	}
	if !waits.gap {
		t.Fatal("out-of-order clear/write was mislabeled atomic remote acceptance")
	}
	_ = waits.finish("session_ended")
}

func TestInputFinishJoinsMaximumClearedRequestsWithoutBlockedResults(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waits := newInputWaits(ReviewRequest{OnInput: func(ctx context.Context, r clarification.Request) (clarification.Answers, error) {
		<-ctx.Done()
		return clarification.Answers{}, ctx.Err()
	}, OnInputEvent: func(context.Context, clarification.Request, string, string) error { return nil }}, client)
	for i := range 64 {
		event := testInputEvent("native")
		event.ID = json.RawMessage(fmt.Sprint(i))
		if err := waits.start(ctx, event); err != nil {
			t.Fatal(err)
		}
		if err := waits.close(ctx, waits.jobs[string(event.ID)], "server_cleared"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- waits.finish("session_ended") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		// Release a broken implementation's owned goroutines before failing.
		for len(waits.done) > 0 {
			<-waits.done
		}
		t.Fatal("maximum cleared requests deadlocked on result delivery")
	}
}

func TestSequentialWrittenDynamicRepliesReleaseOutstandingCapacity(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waits := newInputWaits(ReviewRequest{OnInput: func(context.Context, clarification.Request) (clarification.Answers, error) {
		return clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"chosen"}}}}, nil
	}, OnInputEvent: func(context.Context, clarification.Request, string, string) error { return nil }}, client)
	decoder := json.NewDecoder(right)
	for i := range 9 {
		event := testInputEvent("dynamic")
		event.ID = json.RawMessage(fmt.Sprint(i))
		if err := waits.start(ctx, event); err != nil {
			_ = waits.finish("session_ended")
			t.Fatalf("sequential request %d rejected after previous replies were written: %v", i+1, err)
		}
		if err := waits.result(ctx, <-waits.done); err != nil {
			t.Fatal(err)
		}
		var reply Message
		if decoder.Decode(&reply) != nil || string(reply.ID) != string(event.ID) {
			t.Fatal("wrong sequential response identity")
		}
		if err := waits.result(ctx, <-waits.done); err != nil {
			t.Fatal(err)
		}
	}
	if err := waits.finish("turn_completed"); err != nil || waits.gap {
		t.Fatal("sequential written answers did not finish cleanly")
	}
}
func TestNinthUnansweredInputStillExceedsOutstandingCapacity(t *testing.T) {
	left, right := net.Pipe()
	client := NewClient(left, left)
	defer client.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waits := newInputWaits(ReviewRequest{OnInput: func(ctx context.Context, r clarification.Request) (clarification.Answers, error) {
		<-ctx.Done()
		return clarification.Answers{}, ctx.Err()
	}, OnInputEvent: func(context.Context, clarification.Request, string, string) error { return nil }}, client)
	defer waits.finish("session_ended")
	for i := range 9 {
		event := testInputEvent("dynamic")
		event.ID = json.RawMessage(fmt.Sprint(i))
		err := waits.start(ctx, event)
		if (err != nil) != (i == 8) {
			t.Fatalf("unanswered request %d capacity result: %v", i+1, err)
		}
	}
}
