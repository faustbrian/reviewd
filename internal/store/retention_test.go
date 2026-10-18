package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
)

func TestArtifactExpiryRollsBackWhenClarificationTextCannotExpire(t *testing.T) {
	ctx := context.Background()
	s, _, run, input := inputFixture(t)
	if _, err := s.SelectInput(ctx, input.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"private retained answer"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseInputs(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, run.ID, Failed, "fixture"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_input_expiry BEFORE UPDATE OF answers ON input_requests WHEN NEW.answers IS NULL BEGIN SELECT RAISE(ABORT,'owned fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	cutoff := before.UpdatedAt.Add(24 * time.Hour)
	won, err := s.ExpireArtifacts(ctx, before, Archive{ExpiredAt: cutoff.Add(time.Hour).Format(time.RFC3339Nano)}, nil, json.RawMessage(`[]`), cutoff)
	if err == nil || won {
		t.Fatal("partial expiry claimed success after persistence failure")
	}
	after, err := s.Get(ctx, run.ID)
	old, _ := json.Marshal(before)
	current, _ := json.Marshal(after)
	if err != nil || string(old) != string(current) {
		t.Fatal("failed expiry lost run or question history")
	}
	var answers string
	if err := s.db.QueryRowContext(ctx, "SELECT answers FROM input_requests WHERE id=?", input.ID).Scan(&answers); err != nil || answers == "" {
		t.Fatal("failed expiry erased private answers outside its transaction")
	}
}
