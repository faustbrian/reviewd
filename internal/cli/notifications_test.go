package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/notify"
)

func TestHeadlessNotificationsSummarizeInitialWorkOnceWithoutGrantingDecisions(t *testing.T) {
	var member atomic.Bool
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("EXISTING_REQUEST", "User", 43)} }, &member)
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(peer.ServeHTTP))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n[notifications]\ndesktop = false\ngroup_interval = \"0s\"\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		var output, diagnostics bytes.Buffer
		code := Run(context.Background(), []string{"--config", path, "watch", "--headless", "--once", "--json"}, &output, &diagnostics, deps)
		if code != 0 {
			t.Fatalf("watch failed: %d %s", code, diagnostics.String())
		}
		var inbox lifecycleInbox
		if json.Unmarshal(output.Bytes(), &inbox) != nil {
			t.Fatal("notification corrupted JSON stdout")
		}
		if len(inbox.Items) != 1 || inbox.Items[0].State != "pending_decision" || len(inbox.Runs) != 0 {
			t.Fatalf("notification granted review authority: %+v", inbox)
		}
		count := strings.Count(diagnostics.String(), "[reviewd] Review inbox")
		want := 1
		if attempt == 1 {
			want = 0
		}
		if count != want {
			t.Fatalf("initial work must produce one durable summary, not a restart storm: attempt=%d want=%d diagnostics=%q", attempt, want, diagnostics.String())
		}
	}
	if current := invoke("inbox"); len(current.Items) != 1 || current.Items[0].State != "pending_decision" {
		t.Fatal("notification expiry consumed the pending decision")
	}
}

func TestContinuousHeadlessNotificationsDoNotNeedAnotherPollOrStopOnDesktopFailure(t *testing.T) {
	var member atomic.Bool
	var reads atomic.Int32
	peer := authorityPeer(t, func() map[string]any { return lifecyclePull(1, "open", true, false) }, func() []any { return []any{authorityEvent("EXISTING_REQUEST", "User", 43)} }, &member)
	_, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1); peer.ServeHTTP(w, r) }))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n[watch]\npoll_interval=\"1h\"\n[notifications]\ngroup_interval=\"10ms\"\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan notify.Message, 1)
	deps.Desktop = func(_ context.Context, message notify.Message) error {
		select {
		case delivered <- message:
		default:
		}
		return errors.New("fixture desktop unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output, diagnostics bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"--config", path, "watch", "--headless", "--json"}, &output, &diagnostics, deps)
	}()
	select {
	case <-delivered:
	case code := <-done:
		t.Fatalf("watch ended before notification: %d", code)
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("local grouping deadline waited for the next GitHub poll")
	}
	after := reads.Load()
	select {
	case code := <-done:
		t.Fatalf("desktop failure ended watcher: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	if reads.Load() != after {
		t.Fatal("notification delivery caused extra GitHub requests")
	}
	cancel()
	code := <-done
	if code != 0 && code != 130 {
		t.Fatalf("watch cancellation failed: %d %s", code, diagnostics.String())
	}
	var inbox lifecycleInbox
	if json.Unmarshal(output.Bytes(), &inbox) != nil || len(inbox.Items) != 1 || inbox.Items[0].State != "pending_decision" || len(inbox.Runs) != 0 {
		t.Fatal("notification changed JSON output or decision authority")
	}
	if strings.Count(diagnostics.String(), "Notification delivery unavailable") != 1 {
		t.Fatal("desktop failure did not produce one bounded warning")
	}
}
