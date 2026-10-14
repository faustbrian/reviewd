package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
)

func TestWatcherRechecksClosureBeforeStartingQueuedDecision(t *testing.T) {
	var closed, member atomic.Bool
	pull := func() map[string]any {
		state := "open"
		if closed.Load() {
			state = "closed"
		}
		return lifecyclePull(1, state, true, false)
	}
	peer := authorityPeer(t, pull, func() []any { return []any{authorityEvent("REQUEST", "User", 43)} }, &member)
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/pulls/1" {
			json.NewEncoder(w).Encode(pull())
			return
		}
		if r.URL.Path == "/repos/owner/repo/pulls" && closed.Load() {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		peer.ServeHTTP(w, r)
	}))
	initial := invoke("sync")
	if len(initial.Items) != 1 {
		t.Fatalf("no held request: %+v", initial)
	}
	var output, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"--config", path, "start", initial.Items[0].ID, "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("queue: %d %s", code, diagnostics.String())
	}
	closed.Store(true)
	deps.Source = func(context.Context, github.Pull) (*source.Snapshot, error) {
		t.Error("closed queued work acquired source")
		return nil, context.Canceled
	}
	output.Reset()
	diagnostics.Reset()
	if code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("closure: %d %s", code, diagnostics.String())
	}
	var result lifecycleInbox
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 || len(result.Runs) != 0 {
		t.Fatalf("closed decision remained actionable: %+v", result)
	}
}
