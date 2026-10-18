package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestContinuousWatcherKeepsAcceptedSkillThroughRepeatedReadFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var updated, outage atomic.Bool
	var failures, launches atomic.Int32
	invoke, path, deps := lifecycleCLIConfigDeps(t, "ignore", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outage.Load() && r.URL.Path == "/repos/owner/repo/pulls" {
			failures.Add(1)
			http.Error(w, "temporary fixture outage", http.StatusInternalServerError)
			return
		}
		pull := lifecyclePull(1, "open", true, false)
		if updated.Load() {
			pull["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
		}
		var value any = []any{}
		base, tree := strings.Repeat("b", 40), strings.Repeat("d", 40)
		switch r.URL.Path {
		case "/user":
			value = map[string]any{"id": 43, "login": "reviewer"}
		case "/repos/owner/repo":
			value = map[string]any{"id": 99, "full_name": "owner/repo"}
		case "/repos/owner/repo/pulls":
			value = []any{pull}
		case "/repos/owner/repo/pulls/1":
			value = pull
		case "/repos/owner/repo/commits/" + base:
			value = map[string]any{"sha": base, "commit": map[string]any{"tree": map[string]any{"sha": tree}}}
		case "/repos/owner/repo/git/trees/" + tree:
			value = map[string]any{"sha": tree, "truncated": false, "tree": []any{}}
		case "/graphql":
			value = lifecycleGraphQL(t, r, func(int) []any { return []any{} })
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	configuration := string(raw) + "\n[watch]\npoll_interval='1s'\ndebounce='0s'\n[notifications]\ndesktop=false\nterminal=false\n"
	if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("sync")
	updated.Store(true)
	invoke("sync")
	document, err := config.Parse([]byte(configuration), path)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := config.ResolveDefaults(document, nil)
	directory, _ := stateDirectory(policy, path)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	durable, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	entered, finish := make(chan context.Context, 1), make(chan struct{})
	deps.LaunchAgent = func(worker context.Context, request agent.Request) (agent.Output, error) {
		launches.Add(1)
		output := agent.Output{ThreadID: "continuous-accepted-session", Text: "Skill output survives the outage."}
		if err := request.OnProgress(output); err != nil {
			return output, err
		}
		entered <- worker
		select {
		case <-finish:
			output.Completed = true
			return output, nil
		case <-worker.Done():
			return output, worker.Err()
		}
	}
	reader, writer := io.Pipe()
	fifthFrame := make(chan struct{}, 1)
	decoded := make(chan struct{})
	go func() {
		defer close(decoded)
		decoder := json.NewDecoder(reader)
		for {
			var frame json.RawMessage
			if decoder.Decode(&frame) != nil {
				return
			}
			if failures.Load() >= 5 {
				select {
				case fifthFrame <- struct{}{}:
				default:
				}
			}
		}
	}()
	var diagnostics bytes.Buffer
	stopped := make(chan struct{})
	code := 0
	go func() {
		defer close(stopped)
		code = Run(ctx, []string{"--config", path, "watch", "--headless", "--json"}, writer, &diagnostics, deps)
	}()
	defer func() {
		cancel()
		reader.Close()
		writer.Close()
		<-stopped
		<-decoded
	}()
	var worker context.Context
	select {
	case worker = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("continuous watcher never started accepted skill")
	}
	outage.Store(true)
	select {
	case <-fifthFrame:
	case <-stopped:
		t.Fatal("watcher stopped before its fifth failed poll")
	case <-ctx.Done():
		t.Fatal("watcher did not exercise five consecutive failed polls")
	}
	select {
	case <-worker.Done():
		t.Fatal("repeated GitHub read failures cancelled accepted skill")
	case <-stopped:
		t.Fatal("repeated GitHub read failures shut down monitoring")
	case <-time.After(500 * time.Millisecond):
	}
	close(finish)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		runs, err := durable.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].State == store.AgentCompleted && runs[0].ThreadID == "continuous-accepted-session" && launches.Load() == 1 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("accepted run did not complete through outage: %+v", runs)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-stopped
	if code != 130 {
		t.Fatalf("explicit watcher stop returned %d: %s", code, diagnostics.String())
	}
}
