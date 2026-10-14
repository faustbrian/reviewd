package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/watch"
)

// DC-01/07: discovery-only constraints survive restart/--once without sleeping
// or requiring GitHub access for the operator's ordinary local controls.
func TestDiscoveryCooldownKeepsLocalCommandsAvailableAcrossWatcherRestart(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	var member atomic.Bool
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("EXISTING", "User", 43)} }, &member)
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/repos/owner/repo/pulls/1/reviews" {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/owner/second") {
			t.Error("discovery continued to another repository after server hold")
			w.WriteHeader(500)
			return
		}
		peer.ServeHTTP(w, r)
	}))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `["owner/repo"]`, `["owner/repo","owner/second"]`, 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(data, path)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(filepath.Dir(path), "state")
	durable, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { durable.Close() }()
	now := time.Now()
	engine := &watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Now: func() time.Time { return now }}
	if err := engine.Tick(ctx); github.RetryAfter(err) < time.Hour {
		t.Fatalf("discovery lost guidance: %v", err)
	}
	count := requests.Load()
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	durable, err = store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	engine = &watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Now: func() time.Time { return now.Add(time.Hour - time.Nanosecond) }}
	defer engine.Close()
	if err := engine.Tick(ctx); err != nil || requests.Load() != count {
		t.Fatalf("discovery hold was not absolute/durable: requests=%d wanted=%d err=%v", requests.Load(), count, err)
	}
	run, _, err := durable.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "local-cancel-during-cooldown", State: store.Queued, Private: true, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"watch", "--headless", "--once", "--json"}, {"inbox", "--json"}, {"inbox"}, {"show", run.ID}, {"cancel", run.ID, "--json"}, {"pause", "owner/repo"}} {
		var output, diagnostics bytes.Buffer
		commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		code := Run(commandCtx, append([]string{"--config", path}, args...), &output, &diagnostics, deps)
		cancel()
		if code != 0 || requests.Load() != count {
			t.Fatalf("local command %v blocked/bypassed cooldown: %d %s", args, code, diagnostics.String())
		}
		if args[0] == "inbox" && !strings.Contains(output.String(), "github.com") {
			t.Fatal("cooldown reason/host absent from inbox")
		}
	}
	current, err := durable.Get(ctx, run.ID)
	if err != nil || current.State != store.Cancelled {
		t.Fatal("cooldown lost scoped cancellation")
	}
}

func TestTerminalSnapshotExplainsActiveGitHubCooldown(t *testing.T) {
	path, durable, _ := terminalFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := durable.DeferGitHub(context.Background(), "github.com", until); err != nil {
		t.Fatal(err)
	}
	value, err := (&execution{}).terminalSnapshot(context.Background(), durable, document)
	if err != nil || !strings.Contains(value.Status, "github.com") || !strings.Contains(value.Status, until.Format(time.RFC3339)) {
		t.Fatalf("terminal does not explain cooldown: status=%q error=%v", value.Status, err)
	}
}
