package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentRunStoresExecutionNotDelivery(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, created, err := s.Create(ctx, Run{Executor: NativeExecutor, Purpose: "feedback", Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "human-feedback", State: Queued, Configuration: json.RawMessage(`{"skill":"handle-pr-feedback"}`)})
	if err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	claimed, err := s.ClaimAgent(ctx, run.ID)
	if err != nil || !claimed {
		t.Fatalf("normal feedback launch cannot be claimed: %v %v", claimed, err)
	}
	if err = s.RecordAgent(ctx, run.ID, "native-session", "gpt-6.1-sol", nil); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordAgent(ctx, run.ID, "different-session", "gpt-6.1-sol", nil); err == nil {
		t.Fatal("unrelated session replaced run progress")
	}
	outcome := json.RawMessage(`{"thread_id":"native-session","completed":true,"text":"Skill returned; reviewd did not verify any GitHub effects."}`)
	if err = s.FinishAgent(ctx, run.ID, AgentCompleted, outcome, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != AgentCompleted || got.ThreadID != "native-session" || got.Executor != NativeExecutor || string(got.Outcome) != string(outcome) || len(got.Prepared) != 0 || got.PayloadID != "" {
		t.Fatalf("execution confused with workflow delivery: %+v", got)
	}
	if err = s.RecordAgent(ctx, run.ID, "native-session", "gpt-6.1-sol", nil); err == nil {
		t.Fatal("late progress changed terminal execution")
	}
	again, created, err := s.Create(ctx, Run{Executor: NativeExecutor, Purpose: "feedback", Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "human-feedback", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || created || again.ID != run.ID {
		t.Fatalf("consumed event duplicated: %+v %v %v", again, created, err)
	}
	claimed, err = s.ClaimAgent(ctx, run.ID)
	if err != nil || claimed {
		t.Fatalf("completed event replayed: %v %v", claimed, err)
	}
}

func TestClaudeRunClaimsAndRecordsNativeDefaultModel(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, Run{Executor: "claude_exec_v1", Purpose: "review", Repository: "github.com/acme/api", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "claude-event", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimAgent(ctx, run.ID); err != nil || !claimed {
		t.Fatalf("Claude run not claimable: %v / %v", claimed, err)
	}
	if err := s.RecordAgent(ctx, run.ID, "claude-session", "", nil, "Reading feedback"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAgent(ctx, run.ID, AgentCompleted, json.RawMessage(`{"thread_id":"claude-session","completed":true}`), ""); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimAgent(ctx, run.ID); err != nil || claimed {
		t.Fatalf("completed Claude run replayed: %v %v", claimed, err)
	}
	got, err := s.Get(ctx, run.ID)
	if err != nil || got.State != AgentCompleted || got.ThreadID != "claude-session" || got.RequestedModel != "" {
		t.Fatalf("wrong native identity %+v / %v", got, err)
	}
}
func TestAgentClaimCannotConvertLegacyReviewConsent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "old-private-review", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimAgent(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("legacy private consent silently became a normal skill launch")
	}
	got, err := s.Get(ctx, run.ID)
	if err != nil || got.State != Queued {
		t.Fatalf("legacy history changed: %+v %v", got, err)
	}
}

func TestAgentUpgradePreservesLegacyHistoryWithoutReplay(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "reviewd.sqlite")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/schema9.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(fixture)); err != nil {
		t.Fatal(err)
	}
	snapshot := func(db *sql.DB) []string {
		t.Helper()
		rows := []string{}
		for _, query := range []string{
			`SELECT json_group_array(json_array(id,record,state,prepared,payload_id,thread_id,turn_id,outcome,failure)) FROM (SELECT * FROM runs ORDER BY id)`,
			`SELECT json_group_array(json_array(id,created_at,root_identity,lease_identity,checked_at,closing,processes)) FROM (SELECT * FROM private_owners ORDER BY id)`,
			`SELECT json_group_array(json_array(source,fingerprint,pending,record)) FROM (SELECT * FROM notification_sources ORDER BY source)`,
		} {
			var value string
			if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, value)
		}
		return rows
	}
	before := snapshot(db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dir, "private-work", strings.Repeat("a", 32), "legacy-workspace")
	if err = os.MkdirAll(pending, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(pending, "uncommitted.go")
	if err = os.WriteFile(sentinel, []byte("retained user work"), 0600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		s, err := Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		after := snapshot(s.db)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("upgrade changed historical execution/effects/resources or notifications: before=%q after=%q", before, after)
		}
		var version int
		if err = s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 10 {
			t.Fatalf("normal agent state format unavailable: version=%d err=%v", version, err)
		}
		items, _, err := s.Inbox(ctx)
		if err != nil || len(items) != 1 || items[0].ID != strings.Repeat("c", 32) || items[0].State != "legacy_pending" || items[0].DecisionID != "legacy-private-consent" {
			t.Fatalf("old private consent could be silently replayed or was erased: %+v %v", items, err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "retained user work" {
		t.Fatalf("upgrade touched retained user work: %q %v", data, err)
	}
}

func TestAgentRecoveryRecordsInterruptedExecutionWithoutReplay(t *testing.T) {
	for _, executor := range []string{NativeExecutor, ClaudeExecutor} {
		t.Run(executor, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			owner, err := s.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := s.Create(ctx, Run{Executor: executor, Purpose: "feedback", OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 700, Number: 17, ActorID: 43, ActorLogin: "operator", WorkKey: "interrupted-native-event", State: Queued, Configuration: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if claimed, err := s.ClaimAgent(ctx, run.ID); err != nil || !claimed {
				t.Fatal("claim", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecoverPrivate(ctx, false); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != AgentInterrupted {
				t.Fatalf("lost normal agent became a feedback workflow or remained active: %s", got.State)
			}
			if claimed, err := s.ClaimAgent(ctx, run.ID); err != nil || claimed {
				t.Fatal("interrupted agent replayed", err)
			}

		})
	}
}
