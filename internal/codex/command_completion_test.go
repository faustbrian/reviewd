package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/store"
)

func TestFeedbackCommandRejectsUnsupportedVersionBeforeServerDispatch(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	selected, marker := filepath.Join(root, "selected-codex"), filepath.Join(root, "server-started")
	script := "#!/bin/sh\nif test \"$1\" = --version; then echo 'codex-cli unsupported'; exit 0; fi\nprintf started > '" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'\n"
	if err := os.WriteFile(selected, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if host, err := StartCommandRuntime(ctx, CommandOptions{RuntimeOptions: RuntimeOptions{Executable: selected, TempRoot: root}, Workspace: workspace}); err == nil {
		_ = host.Close()
		t.Fatal("unsupported command executable was accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsupported selected executable reached app-server dispatch before compatibility validation")
	}
}

func TestLinuxCommandCloseWaitsCanonicalCompletionBeforeOwnerRelease(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	host, peer := commandCompletionPeer(t)
	host.linuxCompletion = true
	returned := make(chan error, 1)
	go func() {
		_, err := host.Run(owner.Context(ctx), []string{"owned-inert-command"}, time.Minute)
		returned <- err
	}()
	decoder, encoder := json.NewDecoder(peer), json.NewEncoder(peer)
	var execution Message
	if err := decoder.Decode(&execution); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- host.Close() }()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var terminate Message
	if err := decoder.Decode(&terminate); err != nil {
		t.Fatalf("Close did not request canonical termination: %v", err)
	}
	if err := encoder.Encode(Message{ID: terminate.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
		t.Fatal("Close released ownership on terminate ACK before terminal completion")
	case <-time.After(30 * time.Millisecond):
	}
	if err := encoder.Encode(Message{ID: execution.ID, Result: json.RawMessage(`{"exitCode":130,"stdout":"","stderr":""}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("acknowledged terminal completion remained uncertain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not join terminal command")
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("complete command left durable uncertainty: %v", err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("canonical completion did not release owned storage")
	}
}

func TestLinuxTerminateAckWithoutTerminalCompletionRetainsOwnership(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	host, peer := commandCompletionPeer(t)
	host.linuxCompletion = true
	operation, cancel := context.WithCancel(owner.Context(ctx))
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := host.Run(operation, []string{"owned-inert-command"}, time.Minute); returned <- err }()
	decoder, encoder := json.NewDecoder(peer), json.NewEncoder(peer)
	var execution Message
	if err := decoder.Decode(&execution); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var terminate Message
	if err := decoder.Decode(&terminate); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(Message{ID: terminate.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-returned:
		if !errors.Is(err, lifetime.ErrCleanupUncertain) {
			t.Fatalf("terminate ACK hid absent terminal proof: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing terminal completion did not return bounded uncertainty")
	}
	if _, err := host.Run(ctx, []string{"must-not-replay"}, time.Minute); !errors.Is(err, lifetime.ErrCleanupUncertain) {
		t.Fatal("uncertain command host accepted another execution")
	}
	if err := host.Close(); !errors.Is(err, lifetime.ErrCleanupUncertain) {
		t.Fatal("terminate ACK authorized clean runtime shutdown")
	}
	if err := owner.Close(); err == nil {
		t.Fatal("terminate ACK authorized owner deletion")
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("unacknowledged command scope was erased")
	}
}

// A real correlated transport and inert peer prove the command protocol. No
// operating-system child, background command, model or credential is involved.
func commandCompletionPeer(t *testing.T) (*CommandRuntime, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	client := NewClient(local, local)
	t.Cleanup(func() { _ = client.Close(); _ = remote.Close() })
	return &CommandRuntime{runtime: &Runtime{Client: client}, workspace: t.TempDir()}, remote
}

func TestLinuxUnknownCommandCompletionSurvivesDurableRecovery(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	durable, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	run, created, err := durable.Create(ctx, store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 17, ActorID: 43, ActorLogin: "fixture", WorkKey: "inert-unknown-command", State: store.Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || !created {
		t.Fatalf("run: %t %v", created, err)
	}
	if claimed, err := durable.ClaimFeedback(ctx, run.ID); err != nil || !claimed {
		t.Fatal("claim", err)
	}
	journal := json.RawMessage(`{"effects":[{"kind":"reply","state":"unknown"}]}`)
	if err := durable.SaveFeedback(ctx, run.ID, journal); err != nil {
		t.Fatal(err)
	}
	host, peer := commandCompletionPeer(t)
	// The actual constructor selects this only for the pinned restricted Linux
	// backend. Inject the same capability for inert cross-platform coverage.
	host.linuxCompletion = true
	root := filepath.Join(owner.Root, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	host.runtime.isolation = &Isolation{Root: root, ownedRoot: root}
	go func() { var request Message; _ = json.NewDecoder(peer).Decode(&request); _ = peer.Close() }()
	if _, err := host.Run(owner.Context(ctx), []string{"owned-inert-command"}, time.Minute); err == nil {
		t.Fatal("lost command response became complete")
	}
	if err := host.Close(); err == nil {
		t.Fatal("unknown command completion became clean shutdown")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("command runtime storage erased despite missing completion")
	}
	if err := owner.Close(); err == nil {
		t.Fatal("absent app-server PGID erased unknown command ownership")
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.RecoverPrivate(ctx, false)
	if err != nil || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
		t.Fatalf("unknown command did not survive reopened recovery: %+v %v", report, err)
	}
	actual, err := reopened.Get(ctx, run.ID)
	if err != nil || actual.OwnerClosed || actual.State != store.FeedbackPending || string(actual.Outcome) != string(journal) {
		t.Fatalf("recovery lost uncertain effects or claimed closed ownership: %+v %v", actual, err)
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("recovery removed unknown command storage")
	}
}

func TestCommandCancellationRequiresOriginalTerminalResponse(t *testing.T) {
	host, peer := commandCompletionPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type answer struct {
		result CommandResult
		err    error
	}
	returned := make(chan answer, 1)
	go func() {
		result, err := host.Run(ctx, []string{"owned-inert-command"}, time.Minute)
		returned <- answer{result, err}
	}()
	decoder, encoder := json.NewDecoder(peer), json.NewEncoder(peer)
	var execution Message
	if err := decoder.Decode(&execution); err != nil {
		t.Fatal(err)
	}
	var request struct {
		ProcessID string `json:"processId"`
	}
	if err := json.Unmarshal(execution.Params, &request); err != nil {
		t.Fatal(err)
	}
	if execution.Method != "command/exec" || request.ProcessID == "" {
		t.Fatal("command has no client-owned identity for canonical termination")
	}
	cancel()
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	var terminate Message
	if err := decoder.Decode(&terminate); err != nil {
		t.Fatalf("cancel did not request native command termination: %v", err)
	}
	var selection struct {
		ProcessID string `json:"processId"`
	}
	if err := json.Unmarshal(terminate.Params, &selection); err != nil {
		t.Fatal(err)
	}
	if terminate.Method != "command/exec/terminate" || selection.ProcessID != request.ProcessID {
		t.Fatal("termination selected another command")
	}
	if err := encoder.Encode(Message{ID: terminate.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
		t.Fatal("terminate acknowledgement became proof of terminal command completion")
	case <-time.After(30 * time.Millisecond):
	}
	if err := encoder.Encode(Message{ID: execution.ID, Result: json.RawMessage(`{"exitCode":130,"stdout":"partial output","stderr":""}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case actual := <-returned:
		if !errors.Is(actual.err, context.Canceled) || actual.result.ExitCode != 130 || actual.result.Stdout != "partial output" {
			t.Fatalf("lost cancellation or canonical terminal facts: %+v", actual)
		}
	case <-time.After(time.Second):
		t.Fatal("canonical terminal response did not complete cancellation")
	}
}

func TestCommandCompletionRejectsMissingTerminalFacts(t *testing.T) {
	for _, result := range []string{`{}`, `{"exitCode":0}`, `{"exitCode":0,"stdout":null,"stderr":""}`} {
		t.Run(result, func(t *testing.T) {
			host, peer := commandCompletionPeer(t)
			go func() {
				var request Message
				if json.NewDecoder(peer).Decode(&request) == nil {
					_ = json.NewEncoder(peer).Encode(Message{ID: request.ID, Result: json.RawMessage(result)})
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := host.Run(ctx, []string{"owned-inert-command"}, time.Minute); err == nil {
				t.Fatal("missing canonical exit/output facts became successful completion")
			}
		})
	}
}

func TestCompletedNonzeroCommandRemainsAnExecutionResult(t *testing.T) {
	host, peer := commandCompletionPeer(t)
	go func() {
		var request Message
		if json.NewDecoder(peer).Decode(&request) == nil {
			_ = json.NewEncoder(peer).Encode(Message{ID: request.ID, Result: json.RawMessage(`{"exitCode":7,"stdout":"","stderr":"owned verification failed"}`)})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := host.Run(ctx, []string{"owned-inert-command"}, time.Minute)
	if err != nil || result.ExitCode != 7 || !strings.Contains(result.Stderr, "verification failed") {
		t.Fatalf("nonzero completion was lost: %+v %v", result, err)
	}
}

// The pinned native cap applies before text decoding. A CP1252 smart quote
// expands from one raw byte to three UTF-8 bytes without invalidating the
// canonical terminal response or its completion acknowledgement.
func TestLinuxDecodedOutputExpansionReleasesCompletedOwnership(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	host, peer := commandCompletionPeer(t)
	host.linuxCompletion = true
	output := strings.Repeat("A", (2<<20)-1) + "“"
	sent := make(chan error, 1)
	go func() {
		var request Message
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			sent <- err
			return
		}
		raw, err := json.Marshal(CommandResult{ExitCode: 0, Stdout: output, Stderr: output})
		if err != nil {
			sent <- err
			return
		}
		sent <- json.NewEncoder(peer).Encode(Message{ID: request.ID, Result: raw})
	}()
	operation, cancel := context.WithTimeout(owner.Context(ctx), 5*time.Second)
	defer cancel()
	result, err := host.Run(operation, []string{"owned-inert-command"}, time.Minute)
	if err != nil {
		t.Fatalf("valid decoded terminal output became uncertain: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != output || result.Stderr != output {
		t.Fatal("canonical terminal output was changed")
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("completed command host remained uncertain: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("valid decoded terminal response did not clear ownership: %v", err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("completed command retained owner storage")
	}
}
