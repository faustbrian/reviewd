package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in path uses the real pinned host and a synthetic loopback provider,
// then answers through the ordinary CLI. It is not a real-account model test.
func TestCodexCapabilityClarificationThroughActualCLI(t *testing.T) {
	if os.Getenv("REVIEWD_CODEX_CAPABILITIES") != "1" {
		t.Skip("set REVIEWD_CODEX_CAPABILITIES=1 for actual installed-host clarification")
	}
	_, path, deps, durable := privateApplicationFixture(t, "ACTUAL-HOST-CLARIFICATION-ACCEPTANCE")
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	requested, received, denied := false, false, false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
			Input []map[string]any  `json:"input"`
		}
		var payload map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			http.Error(w, "invalid fixture JSON", 400)
			return
		}
		encoded, _ := json.Marshal(payload)
		if json.Unmarshal(encoded, &body) != nil {
			http.Error(w, "invalid fixture JSON", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		requests++
		sequence := requests
		if sequence == 1 {
			_, present := payload["tools"]
			t.Logf("Synthetic provider tools field present=%t; availability is checked by correlated host invocation and separately by the real-model fixture", present)
		}

		for _, item := range body.Input {
			if item["type"] == "function_call_output" && item["call_id"] == "clarification_call" {
				raw, _ := json.Marshal(item["output"])
				received = received || strings.Contains(string(raw), "host-answer-λ")
			}
			if item["type"] == "function_call_output" && item["call_id"] == "escape_call" {
				raw, _ := json.Marshal(item["output"])
				denied = denied || strings.Contains(string(raw), "unsupported call: exec_command")
			}
		}
		arguments, _ := json.Marshal(map[string]any{"questions": []any{map[string]string{"id": "scope", "header": "Scope", "question": "Which fixture answer?"}}})
		item := map[string]any{"type": "function_call", "id": "fc_clarify", "call_id": "clarification_call", "name": "reviewd_request_input", "arguments": string(arguments)}
		if sequence == 2 {
			item = map[string]any{"type": "function_call", "id": "fc_escape", "call_id": "escape_call", "name": "exec_command", "arguments": `{"cmd":"write-untrusted-file"}`}
		}
		if sequence > 2 {
			item = map[string]any{"type": "message", "id": "msg_result", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": `{"conclusion":"APPROVE","summary":"","findings":[],"gaps":[]}`, "annotations": []any{}}}}
		}
		id := fmt.Sprintf("owned_response_%d", sequence)
		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress"}},
			{"type": "response.output_item.added", "output_index": 0, "item": item},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}},
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
		}
	}))
	defer peer.Close()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	wrapper := filepath.Join(t.TempDir(), "codex-fixture")
	script := "#!/bin/sh\nexec " + quote(executable) + ` "$@"`
	for _, value := range []string{`model_provider="fixture"`, `model_providers.fixture.name="fixture"`, `model_providers.fixture.base_url="` + peer.URL + `"`, `model_providers.fixture.wire_api="responses"`, `model_providers.fixture.env_key="OPENAI_API_KEY"`} {
		script += " -c " + quote(value)
	}
	if os.WriteFile(wrapper, []byte(script+"\n"), 0700) != nil {
		t.Fatal("cannot create owned host wrapper")
	}
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(cfg), "\n") {
		if strings.HasPrefix(line, "auth_home=") {
			donor, err := strconv.Unquote(strings.TrimPrefix(line, "auth_home="))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(donor, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"owned-fake-provider-key"}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	original, _ := os.Executable()
	cfg = bytes.ReplaceAll(cfg, []byte(fmt.Sprintf("executable=%q", original)), []byte(fmt.Sprintf("executable=%q", wrapper)))
	cfg = bytes.ReplaceAll(cfg, []byte(`model="fixture-model"`), []byte(`model="gpt-6.1-sol"`))
	cfg = bytes.ReplaceAll(cfg, []byte(`reasoning_effort="xhigh"`), []byte(`reasoning_effort="high"`))
	if err = os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "owned-fake-provider-key")
	// Synthetic custom providers do not establish a personal login. This
	// adapter bypasses only the account-metadata prerequisite, not Review,
	// application persistence/answering, model selection or capability grants.
	// The separate real-account test exercises unmodified default CLI wiring.
	deps.Review = func(ctx context.Context, request review.WorkerRequest) (output codex.ReviewOutput, err error) {
		runtime, err := codex.StartRuntime(ctx, codex.RuntimeOptions{Executable: wrapper, AuthHome: request.Policy.String("reviewer.auth_home"), TempRoot: t.TempDir()})
		if err != nil {
			return output, err
		}
		defer func() {
			if closeErr := runtime.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
		var metadata struct {
			Requires bool `json:"requiresOpenaiAuth"`
			Account  *struct {
				Type string `json:"type"`
			} `json:"account"`
		}
		if err = runtime.Client.Call(ctx, "account/read", map[string]bool{"refreshToken": false}, &metadata); err != nil {
			return output, err
		}
		accountType := "absent"
		if metadata.Account != nil {
			accountType = metadata.Account.Type
		}
		t.Logf("Synthetic provider metadata: requires_auth=%t account_type=%q; not personal authentication evidence", metadata.Requires, accountType)
		return runtime.Review(ctx, codex.ReviewRequest{Model: request.Policy.String("reviewer.model"), Effort: request.Policy.String("reviewer.reasoning_effort"), SkillPath: request.Skill.Path, SkillName: request.Skill.Identity.Name, Prompt: request.Prompt, Evidence: request.Evidence, OnStatus: request.OnStatus, OnInput: request.OnInput, OnInputEvent: func(ctx context.Context, input clarification.Request, event, reason string) error {
			if event == "opened" {
				requested = input.Transport == "dynamic" && input.ThreadID != "" && input.TurnID != "" && len(input.Questions) == 1 && input.Questions[0].ID == "scope"
			}
			return request.OnInputEvent(ctx, input, event, reason)
		}, OutputSchema: review.OutputSchema()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var output, diagnostic bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}, &output, &diagnostic, deps)
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
	}()
	inputID := ""
	for inputID == "" {
		inputs, err := durable.Inputs(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) == 1 && inputs[0].State == "pending" {
			inputID = inputs[0].ID
			break
		}
		select {
		case code := <-done:
			finished = true
			t.Fatalf("actual host ended before input: exit=%d %s", code, diagnostic.String())
		case <-ctx.Done():
			t.Fatal("actual host never requested clarification")
		case <-time.After(20 * time.Millisecond):
		}
	}
	deps.Input = strings.NewReader(`{"answers":{"scope":{"answers":["host-answer-λ"]}}}`)
	var selected, errors bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "answer", inputID, "--from-file", "-", "--json"}, &selected, &errors, deps); code != 0 {
		t.Fatalf("answer CLI: %s", errors.String())
	}
	select {
	case code := <-done:
		finished = true
		if code != 0 {
			t.Fatalf("actual host result: %s", diagnostic.String())
		}
	case <-ctx.Done():
		t.Fatal("actual host did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if !requested || !received || !denied || requests != 3 {
		t.Fatalf("actual host boundaries: host_requested=%t answered=%t denied=%t requests=%d", requested, received, denied, requests)
	}
}

// Separate from the synthetic-provider contract: an actual authorized account
// and model must ask, consume an unpredictable answer, and finish privately.
func TestLiveCodexClarificationThroughActualCLI(t *testing.T) {
	if os.Getenv("REVIEWD_LIVE_CODEX") != "1" {
		t.Skip("set REVIEWD_LIVE_CODEX=1 for authorized model clarification acceptance")
	}
	for _, mode := range []string{"manual", "watch"} {
		t.Run(mode, func(t *testing.T) { liveCodexClarificationThroughActualCLI(t, mode) })
	}
}

func liveCodexClarificationThroughActualCLI(t *testing.T, mode string) {
	invoke, path, deps, durable := privateApplicationFixture(t, "REAL-MODEL-CLARIFICATION-ACCEPTANCE")
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	donor := os.Getenv("CODEX_HOME")
	if donor == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		donor = filepath.Join(home, ".codex")
	}
	original, _ := os.Executable()
	lines := strings.Split(string(cfg), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "auth_home=") {
			lines[i] = "auth_home=" + strconv.Quote(donor)
		}
		if strings.HasPrefix(line, "skill_path=") {
			skill, err := strconv.Unquote(strings.TrimPrefix(line, "skill_path="))
			if err != nil {
				t.Fatal(err)
			}
			text := "---\nname: requesting-code-review\ndescription: Owned textual clarification acceptance fixture\n---\nOnly in actual frozen installed skill. Before returning your private result, call reviewd_request_input with exactly one question, id scope, asking the operator for the unpredictable fixture token. You cannot know it in advance. Wait for the answer. Then return the required private JSON report with conclusion APPROVE, no findings, no gaps, and summary exactly equal to the token you received. This is an owned protocol acceptance fixture, not a real code-review claim. Do not publish, execute, browse, edit, install or delegate.\n"
			if err = os.WriteFile(skill, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg = []byte(strings.Join(lines, "\n"))
	cfg = bytes.ReplaceAll(cfg, []byte(fmt.Sprintf("executable=%q", original)), []byte(fmt.Sprintf("executable=%q", executable)))
	cfg = bytes.ReplaceAll(cfg, []byte(`model="fixture-model"`), []byte(`model="gpt-6.1-sol"`))
	cfg = bytes.ReplaceAll(cfg, []byte(`reasoning_effort="xhigh"`), []byte(`reasoning_effort="high"`))
	if err = os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	deps.Review = nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var output, diagnostic bytes.Buffer
	args := []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}
	if mode == "watch" {
		initial := invoke("sync")
		if len(initial.Items) != 1 {
			t.Fatal("actual-model watcher has no held request")
		}
		invoke("start", initial.Items[0].ID)
		args = []string{"--config", path, "watch", "--headless", "--once", "--json"}
	}
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, args, &output, &diagnostic, deps)
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
	}()
	inputID, runID := "", ""
	for inputID == "" {
		inputs, err := durable.Inputs(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs) == 1 && inputs[0].State == "pending" {
			inputID, runID = inputs[0].ID, inputs[0].RunID
			break
		}
		select {
		case code := <-done:
			finished = true
			t.Fatalf("actual model ended before question: exit=%d %s", code, diagnostic.String())
		case <-ctx.Done():
			t.Fatal("actual model did not ask")
		case <-time.After(40 * time.Millisecond):
		}
	}
	active, err := durable.Get(ctx, runID)
	if err != nil || active.State != store.Reviewing || active.ThreadID == "" || active.TurnID == "" || active.RequestedModel != "gpt-6.1-sol" || active.EffectiveModel == "" {
		t.Fatal("actual pending clarification lost its active primary identity")
	}
	for _, command := range [][]string{{"show", runID}, {"inbox", "--json"}} {
		var shown, errors bytes.Buffer
		args := append([]string{"--config", path}, command...)
		if code := Run(ctx, args, &shown, &errors, deps); code != 0 || !strings.Contains(shown.String(), runID) || !strings.Contains(shown.String(), active.ThreadID) || !strings.Contains(shown.String(), inputID) {
			t.Fatal("another actual CLI client could not inspect the live run/input identity")
		}
	}
	token, err := clarification.NewID()
	if err != nil {
		t.Fatal(err)
	}
	answers, _ := json.Marshal(clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{token}}}})
	deps.Input = bytes.NewReader(answers)
	var selected, errors bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "answer", inputID, "--from-file", "-", "--json"}, &selected, &errors, deps); code != 0 {
		t.Fatalf("actual model answer: %s", errors.String())
	}
	select {
	case code := <-done:
		finished = true
		if code != 0 {
			t.Fatalf("actual model private result: %s", diagnostic.String())
		}
	case <-ctx.Done():
		t.Fatal("actual model did not consume answer")
	}
	run, err := durable.Get(ctx, runID)
	if err != nil || run.State != store.Prepared || !bytes.Contains(run.Prepared, []byte(token)) {
		t.Fatal("actual model did not consume the independently generated answer into its private fixture result")
	}
	if !run.OwnerClosed || run.OwnerID == "" {
		t.Fatal("actual private model did not acknowledge resource shutdown")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state", "private-work", run.OwnerID)); !os.IsNotExist(err) {
		t.Fatal("actual private model retained source, frozen skill or authentication/runtime storage")
	}
	if mode == "watch" {
		var repeated, errors bytes.Buffer
		if code := Run(ctx, []string{"--config", path, "watch", "--headless", "--once", "--json"}, &repeated, &errors, deps); code != 0 {
			t.Fatalf("actual-model watcher restart: %d %s", code, errors.String())
		}
		runs, err := durable.List(ctx)
		if err != nil || len(runs) != 1 || runs[0].State != store.Prepared || runs[0].ID != run.ID || runs[0].ThreadID != run.ThreadID || runs[0].TurnID != run.TurnID || runs[0].PayloadID != run.PayloadID {
			t.Fatal("watcher restart repeated a paid turn or replaced its private result")
		}
	}
	t.Logf("Actual model %s: pending primary identity visible through plain/JSON clients, explicit answer consumed, private preview retained, owned storage removed; mode=%s", run.EffectiveModel, mode)
}
