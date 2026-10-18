package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/faustbrian/reviewd/internal/clarification"
)

func inputFixture(t *testing.T) (*Store, string, Run, clarification.Request) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profile")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	run, _, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 1, PullID: 2, Number: 1, ActorID: 3, ActorLogin: "reviewer", State: Queued, WorkKey: "input", Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordProgress(ctx, run.ID, "primary-thread", "primary-turn", "fixture-model", "fixture-model", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	request := clarification.Request{ID: "opaque-first", ThreadID: "primary-thread", TurnID: "primary-turn", WireID: `"same-wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which?"}}}
	if err = s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	return s, path, run, request
}
func TestInputSelectionIsExactSingleWinnerAndNeverReplayed(t *testing.T) {
	s, path, run, request := inputFixture(t)
	ctx := context.Background()
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	wins := make(chan string, 2)
	for i, client := range []*Store{s, other} {
		wg.Add(1)
		go func(i int, client *Store) {
			defer wg.Done()
			value := []string{"private-one", "private-two"}[i]
			_, err := client.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{value}}}})
			if err == nil {
				wins <- value
			}
		}(i, client)
	}
	wg.Wait()
	close(wins)
	winner := <-wins
	if winner == "" || len(wins) != 0 {
		t.Fatal("competing submissions did not select exactly one answer")
	}
	answer, err := s.WaitInput(ctx, request.ID)
	if err != nil || answer.Answers["scope"].Answers[0] != winner {
		t.Fatal("winner changed before transport")
	}
	if _, err = s.WaitInput(ctx, request.ID); err == nil {
		t.Fatal("sending answer replayed")
	}
	if err = s.InputEvent(ctx, request.ID, "written", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.InputEvent(ctx, request.ID, "closed", "turn_completed"); err != nil {
		t.Fatal(err)
	}
	next := request
	next.ID = "opaque-next"
	next.TurnID = "next-turn"
	next.Questions[0].Question = "A different question?"
	if err = s.OpenInput(ctx, run.ID, next); err == nil {
		t.Fatal("primary mismatched turn accepted")
	}
	next.Participant = "specialist"
	next.ThreadID = "leaf-thread"
	if err = s.OpenInput(ctx, run.ID, next); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"late"}}}}); err == nil {
		t.Fatal("old input answered a newer request")
	}
	current, err := other.Get(ctx, run.ID)
	raw, _ := json.Marshal(current)
	if err != nil || len(current.Inputs) != 2 || strings.Contains(string(raw), "private-one") || strings.Contains(string(raw), "private-two") || current.ThreadID != "primary-thread" {
		t.Fatal("question correlation or answer privacy lost")
	}
}
func TestInputHistorySurvivesReopenWithoutImplicitAnswer(t *testing.T) {
	for _, phase := range []string{"pending", "selected", "sending", "sent"} {
		t.Run(phase, func(t *testing.T) {
			s, path, run, request := inputFixture(t)
			ctx := context.Background()
			if phase != "pending" {
				if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"private-sentinel"}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "sending" || phase == "sent" {
				if _, err := s.WaitInput(ctx, request.ID); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "sent" {
				if err := s.InputEvent(ctx, request.ID, "written", ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			inputs, err := reopened.Inputs(ctx, run.ID)
			if err != nil || len(inputs) != 1 || inputs[0].State != phase {
				t.Fatal("reopen changed unanswered/delivery history")
			}
			if err := reopened.CloseInputs(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			inputs, _ = reopened.Inputs(ctx, run.ID)
			want := "closed"
			if phase == "sending" || phase == "sent" {
				want = "delivery_unknown"
			}
			if inputs[0].State != want || inputs[0].ClosedAt == "" {
				t.Fatal("lost owner did not preserve honest delivery state")
			}
			if _, err := reopened.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"late"}}}}); err == nil {
				t.Fatal("closed owner accepted new answer")
			}
		})
	}
}

func TestCompletedInputWriteFactCanFollowServerClearWithoutReopening(t *testing.T) {
	s, _, run, request := inputFixture(t)
	ctx := context.Background()
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"sent"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitInput(ctx, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.InputEvent(ctx, request.ID, "closed", "server_cleared"); err != nil {
		t.Fatal(err)
	}
	if err := s.InputEvent(ctx, request.ID, "written", ""); err != nil {
		t.Fatal(err)
	}
	inputs, err := s.Inputs(ctx, run.ID)
	if err != nil || len(inputs) != 1 || inputs[0].WrittenAt == "" || inputs[0].ClosedAt == "" || inputs[0].State != "delivery_unknown" {
		t.Fatal("out-of-order write fact disappeared or reopened answered work")
	}
}
