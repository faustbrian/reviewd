package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/store"
)

func TestPrivateClarificationWaitsForExplicitSecondClientAnswer(t *testing.T) {
	for _, mode := range []string{"manual", "watch"} {
		t.Run(mode, func(t *testing.T) {
			invoke, path, deps, durable := privateApplicationFixture(t, "INPUT-CLARIFICATION-ACCEPTANCE")
			deps.Review = nil
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			args := []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}
			if mode == "watch" {
				initial := invoke("sync")
				invoke("start", initial.Items[0].ID)
				args = []string{"--config", path, "watch", "--headless", "--once", "--json"}
			}
			var output, diagnostic bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &output, &diagnostic, deps) }()
			finished := false
			defer func() {
				cancel()
				if finished {
					return
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
				}
			}()
			var run store.Run
			var inputID string
			for inputID == "" {
				runs, err := durable.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(runs) == 1 {
					run = runs[0]
					raw, _ := json.Marshal(run)
					var view struct {
						Inputs []struct {
							ID, State string
							Questions []json.RawMessage
						} `json:"inputs"`
					}
					_ = json.Unmarshal(raw, &view)
					if len(view.Inputs) == 1 && view.Inputs[0].State == "pending" && len(view.Inputs[0].Questions) == 2 && strings.Contains(string(run.Usage), "777") {
						inputID = view.Inputs[0].ID
						break
					}
				}
				select {
				case code := <-done:
					finished = true
					t.Fatalf("clarification was not durably pending before reviewer returned: exit=%d state=%s diagnostics=%s", code, run.State, diagnostic.String())
				case <-ctx.Done():
					t.Fatalf("clarification was not visible within the whole journey budget: state=%s progress=%s: %v", run.State, run.Progress, ctx.Err())
				case <-time.After(20 * time.Millisecond):
				}
			}
			if run.Progress != "waiting_input" {
				t.Errorf("dynamic clarification activity is %q, not waiting_input", run.Progress)
			}
			if run.State != store.Reviewing || len(run.Prepared) != 0 {
				t.Fatal("unanswered input prepared a result")
			}
			for _, format := range []string{"plain", "json"} {
				var shown, errors bytes.Buffer
				cmd := []string{"--config", path, "inputs", run.ID}
				if format == "json" {
					cmd = append(cmd, "--json")
				}
				if code := Run(ctx, cmd, &shown, &errors, deps); code != 0 || !strings.Contains(shown.String(), inputID) || !strings.Contains(shown.String(), "Which variant") {
					t.Fatalf("%s question view missing: %s", format, errors.String())
				}
			}
			select {
			case <-done:
				finished = true
				t.Fatal("review did not wait for explicit answer")
			case <-time.After(100 * time.Millisecond):
			}
			// Ambiguous schema aliases must not choose either answer or send a reply.
			deps.Input = strings.NewReader(`{"answers":{"scope":{"answers":["Billing"]},"private":{"answers":["first"]}},"Answers":{"scope":{"answers":["Billing"]},"private":{"answers":["second"]}}}`)
			var rejected, rejectErrors bytes.Buffer
			if code := Run(ctx, []string{"--config", path, "answer", inputID, "--from-file", "-", "--json"}, &rejected, &rejectErrors, deps); code == 0 {
				t.Fatal("ambiguous aliased JSON selected an answer")
			}
			stillPending, err := durable.Get(ctx, run.ID)
			if err != nil || stillPending.Inputs[0].State != "pending" || stillPending.Inputs[0].SelectedAt != "" {
				t.Fatal("rejected alias JSON mutated input consent")
			}
			answers := filepath.Join(t.TempDir(), "answers.json")
			if err := os.WriteFile(answers, []byte(`{"answers":{"scope":{"answers":["Billing"]},"private":{"answers":["private-answer-canary-λ"]}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			var answered, errors bytes.Buffer
			if code := Run(ctx, []string{"--config", path, "answer", inputID, "--from-file", answers, "--json"}, &answered, &errors, deps); code != 0 {
				t.Fatalf("explicit answer failed: %s", errors.String())
			}
			select {
			case code := <-done:
				finished = true
				if code != 0 {
					t.Fatalf("review failed after answer: %s", diagnostic.String())
				}
			case <-ctx.Done():
				t.Fatal("review failed to consume explicit answer")
			}
			current, err := durable.Get(ctx, run.ID)
			if err != nil || current.State != store.Prepared {
				t.Fatalf("answer did not produce private prepared result: %s %v", current.State, err)
			}
			if len(current.Inputs) != 1 || current.Inputs[0].ID != inputID || current.Inputs[0].State != "closed" || current.Inputs[0].SelectedAt == "" || current.Inputs[0].WrittenAt == "" || current.Inputs[0].ClosedAt == "" {
				t.Fatal("prepared result omitted the exact input's selected, written or closed facts")
			}
			if strings.Contains(output.String()+diagnostic.String()+answered.String()+errors.String(), "private-answer-canary") {
				t.Fatal("private answer leaked into ambient output")
			}
		})
	}
}

func TestPrivateClarificationTerminalEventsCannotPrepareWithoutAnswer(t *testing.T) {
	for _, mode := range []string{"cancel", "clear", "eof"} {
		t.Run(mode, func(t *testing.T) {
			_, path, deps, durable := privateApplicationFixture(t, "INPUT-"+strings.ToUpper(mode)+"-CLARIFICATION-ACCEPTANCE")
			deps.Review = nil
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var output, diagnostics bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- Run(ctx, []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}, &output, &diagnostics, deps)
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
			var run store.Run
			for run.ID == "" {
				runs, err := durable.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(runs) == 1 && len(runs[0].Inputs) == 1 {
					run = runs[0]
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("input never became durable")
				case <-time.After(20 * time.Millisecond):
				}
			}
			if mode == "cancel" {
				var response, errors bytes.Buffer
				if code := Run(ctx, []string{"--config", path, "cancel", run.ID, "--json"}, &response, &errors, deps); code != 0 {
					t.Fatalf("cancel: %s", errors.String())
				}
			}
			select {
			case code := <-done:
				finished = true
				if code == 0 {
					t.Fatal("unanswered/cleared/EOF input produced a successful review")
				}
			case <-ctx.Done():
				t.Fatal("terminal input event did not stop private worker")
			}
			current, err := durable.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != store.Failed && current.State != store.Cancelled {
				t.Fatalf("unfinished input lifecycle retained %s", current.State)
			}
			if len(current.Prepared) != 0 || len(current.Inputs) != 1 || current.Inputs[0].State != "closed" || current.Inputs[0].SelectedAt != "" {
				t.Fatal("unfinished question became answered, disappeared or prepared")
			}
			var selected, errors bytes.Buffer
			deps.Input = strings.NewReader(`{"answers":{"scope":{"answers":["Billing"]},"private":{"answers":["late"]}}}`)
			if code := Run(ctx, []string{"--config", path, "answer", current.Inputs[0].ID, "--from-file", "-"}, &selected, &errors, deps); code == 0 {
				t.Fatal("closed question accepted late answer")
			}
		})
	}
}

func TestPrivateSpecialistClarificationKeepsPrimaryIdentity(t *testing.T) {
	_, path, deps, durable := privateApplicationFixture(t, "INPUT-LEAF-CLARIFICATION-ACCEPTANCE")
	deps.Review = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output, diagnostics bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"--config", path, "review", "https://github.com/owner/repo/pull/1", "--private", "--json"}, &output, &diagnostics, deps)
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
	var run store.Run
	for run.ID == "" {
		runs, err := durable.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && len(runs[0].Inputs) == 1 {
			run = runs[0]
			break
		}
		select {
		case code := <-done:
			finished = true
			t.Fatalf("leaf ended before question: %d %s", code, diagnostics.String())
		case <-ctx.Done():
			t.Fatal("leaf never asked")
		case <-time.After(20 * time.Millisecond):
		}
	}
	input := run.Inputs[0]
	if input.Participant != "specialist" || input.ThreadID != "cli-leaf-thread" || run.ThreadID != "cli-private-thread" || input.WireID != `"clarify-1"` {
		t.Fatal("leaf question lost its scope or replaced primary identity")
	}
	deps.Input = strings.NewReader(`{"answers":{"scope":{"answers":["Billing"]},"private":{"answers":["private-answer-canary-λ"]}}}`)
	var selected, errors bytes.Buffer
	if code := Run(ctx, []string{"--config", path, "answer", input.ID, "--from-file", "-", "--json"}, &selected, &errors, deps); code != 0 {
		t.Fatalf("leaf answer: %s", errors.String())
	}
	select {
	case code := <-done:
		finished = true
		if code != 0 {
			t.Fatalf("primary failed after sealed leaf: %s", diagnostics.String())
		}
	case <-ctx.Done():
		t.Fatal("leaf answer did not finish")
	}
	current, err := durable.Get(ctx, run.ID)
	if err != nil || current.State != store.Prepared || current.ThreadID != "cli-private-thread" || !strings.Contains(string(current.Specialists), "cli-leaf-thread") {
		t.Fatal("leaf result replaced or escaped primary identity")
	}
}
