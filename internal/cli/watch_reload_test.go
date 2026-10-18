package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/store"
)

// QA15/X06/X07: one continuously running ordinary headless command must retain
// the last valid policy, not fall back to defaults or require a process restart.
func TestContinuousWatcherReloadKeepsLastValidPolicyAndRecovers(t *testing.T) {
	var phase atomic.Int32
	var member atomic.Bool
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any {
		events := []any{}
		for i := int32(0); i <= phase.Load(); i++ {
			events = append(events, authorityEvent(fmt.Sprintf("RELOAD_REQUEST_%d", i), "User", 43))
		}
		return events
	}, &member)
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(peer.ServeHTTP))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configuration := strings.Replace(string(original), `review_requested = "auto"`, `review_requested = "notify"`, 1) + "\n[watch]\npoll_interval=\"1s\"\ndebounce=\"0s\"\n[notifications]\ndesktop=false\nterminal=false\n"
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	var sources atomic.Int32
	deps.LaunchAgent = func(context.Context, agent.Request) (agent.Output, error) {
		sources.Add(1)
		return agent.Output{}, fmt.Errorf("notification/ask policy must not start a private review")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	type frame struct {
		SchemaVersion int               `json:"schema_version"`
		Items         []store.InboxItem `json:"items"`
	}
	updates := make(chan frame, 16)
	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		decoder := json.NewDecoder(reader)
		for {
			var value frame
			if decoder.Decode(&value) != nil {
				return
			}
			select {
			case updates <- value:
			case <-ctx.Done():
				return
			}
		}
	}()
	var diagnostics bytes.Buffer
	finished := make(chan struct{})
	code := 0
	go func() {
		defer close(finished)
		code = Run(ctx, []string{"--config", path, "watch", "--headless", "--json"}, writer, &diagnostics, deps)
	}()
	defer func() { cancel(); reader.Close(); writer.Close(); <-finished; <-decodeDone }()
	lastEvent := ""
	await := func(action, state string) {
		t.Helper()
		for {
			select {
			case value := <-updates:
				if value.SchemaVersion != 1 {
					t.Fatal("watch output lost its stable JSON envelope")
				}
				if len(value.Items) == 1 && value.Items[0].EventKey != lastEvent && value.Items[0].Action == action && value.Items[0].State == state {
					lastEvent = value.Items[0].EventKey
					return
				}
			case <-finished:
				t.Fatalf("watch stopped before reload acceptance: code=%d diagnostics=%s", code, diagnostics.String())
			case <-ctx.Done():
				t.Fatal("continuous watcher did not expose the expected fresh-event policy")
			}
		}
	}
	await("notify", "attention")
	for i, step := range []struct{ data, action, state string }{
		// A valid-looking auto prefix must not partially activate before parsing fails.
		{strings.Replace(configuration, `review_requested = "notify"`, `review_requested = "auto"`, 1) + "\n[unterminated", "notify", "attention"},
		{strings.Replace(configuration, `review_requested = "notify"`, `review_requested = "ask"`, 1), "ask", "pending_decision"},
		// The retained policy is now ask, not the original notify or built-in defaults.
		{configuration + "\n[unterminated", "ask", "pending_decision"},
		{configuration, "notify", "attention"},
	} {
		if err := os.WriteFile(path, []byte(step.data), 0600); err != nil {
			t.Fatal(err)
		}
		phase.Store(int32(i + 1))
		await(step.action, step.state)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop after acceptance")
	}
	if code != 130 || sources.Load() != 0 || !strings.Contains(diagnostics.String(), "Configuration reload rejected; keeping the last valid settings") {
		t.Fatalf("reload lost durable policy or its diagnostic: code=%d source=%d diagnostics=%s", code, sources.Load(), diagnostics.String())
	}
}
