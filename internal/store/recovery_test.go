package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/faustbrian/reviewd/internal/clarification"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// QA-X05/X08: a scheduler link is not an owner-liveness proof. A manual
// start client can still own linked work when another watcher is started.
func TestScheduledRecoveryPreservesLinkedWorkWithoutOwnerLossEvidence(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	run, won, err := s.CreateForItem(ctx, Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "linked-owner-unknown", State: Queued, Configuration: json.RawMessage(`{}`)}, item)
	if err != nil || !won {
		t.Fatalf("create: %t %v", won, err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim: %t %v", won, err)
	}
	if err := s.RecoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Reviewing {
		t.Fatalf("a link was mistaken for proof of owner death: %+v %v", current, err)
	}
}

func TestRecoveryRetainsResourcesWhenProcessMetadataIsMalformed(t *testing.T) {
	s, owner, _ := privateOwnerRun(t)
	ctx := context.Background()
	child := exec.CommandContext(ctx, "sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if _, err := lifetime.Start(owner.Context(ctx), child); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-child.Process.Pid, syscall.SIGKILL); child.Wait() }()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\necho malformed-process-metadata\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || !report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
		t.Fatalf("malformed inspection was mistaken for an empty process set: %+v %v", report, err)
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("unknown process liveness erased private storage")
	}
}

func TestRecoveryRollsBackRunAndInputTogetherAndNeverReplaysSendingAnswer(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	if err := s.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	request := clarification.Request{ID: "owner-input", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "Which?"}}}
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"private-answer"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WaitInput(ctx, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_owner_input BEFORE UPDATE OF closed_at ON input_requests BEGIN SELECT RAISE(ABORT,'owned rollback fixture'); END`); err != nil {
		t.Fatal(err)
	}
	failedReport, recoveryErr := s.RecoverPrivate(ctx, false)
	if recoveryErr == nil {
		t.Fatal("failed persistence became successful recovery")
	}
	if len(failedReport.Owners) != 1 || failedReport.Owners[0].State != "recovery_failed" {
		t.Fatalf("a rolled-back operation was reported as recovered: %+v", failedReport)
	}
	before, err := s.Get(ctx, run.ID)
	if err != nil || before.State != Reviewing || len(before.Inputs) != 1 || before.Inputs[0].State != "sending" || before.Inputs[0].ClosedAt != "" {
		t.Fatalf("partial recovery escaped rollback: %+v %v", before, err)
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("failed recovery removed its private evidence")
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER reject_owner_input"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverPrivate(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(ctx, run.ID)
	if err != nil || after.State != Failed || after.Inputs[0].State != "delivery_unknown" || after.Inputs[0].ClosedAt == "" || after.Inputs[0].Reason != "owner_lost" {
		t.Fatalf("lost delivery facts were fabricated or discarded: %+v %v", after, err)
	}
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"replay"}}}}); err == nil {
		t.Fatal("recovery allowed an old answer to be selected again")
	}
}

func TestRecoveryDoesNotMistakeAReplacedLeaseForOwnerDeath(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	defer owner.Close()
	ctx := context.Background()
	path := filepath.Join(owner.Root, "owner.lock")
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || !report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "unknown" {
		t.Fatalf("a new lease inode falsely proved that the live owner died: %+v %v", report, err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Reviewing {
		t.Fatal("replacement lease interrupted live work")
	}
}

func TestRecoveryConcurrentClientsPreserveHistoryAndUserStorage(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	userRoot := t.TempDir()
	canary := filepath.Join(userRoot, "untouched")
	if err := os.WriteFile(canary, []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userRoot, filepath.Join(owner.Root, "source-link")); err != nil {
		t.Fatal(err)
	}
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for _, client := range []*Store{s, second} {
		wg.Add(1)
		go func(client *Store) {
			defer wg.Done()
			<-start
			_, err := client.RecoverPrivate(ctx, false)
			errors <- err
		}(client)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed || !current.OwnerClosed || current.WorkKey != run.WorkKey {
		t.Fatalf("concurrent recovery lost durable identity: %+v %v", current, err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("owned root survived completed recovery")
	}
	if data, err := os.ReadFile(canary); err != nil || string(data) != "user" {
		t.Fatal("cleanup followed a source symlink into user storage")
	}
}

func TestRecoveryDoesNotDeleteAReplacedPrivateRoot(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s, owner, run := privateOwnerRun(t)
			ctx := context.Background()
			if err := owner.lease.Close(); err != nil {
				t.Fatal(err)
			}
			original := owner.Root + ".original"
			if err := os.Rename(owner.Root, original); err != nil {
				t.Fatal(err)
			}
			user := t.TempDir()
			if kind == "symlink" {
				if err := os.Symlink(user, owner.Root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(owner.Root, 0700); err != nil {
					t.Fatal(err)
				}
				user = owner.Root
				if err := os.WriteFile(filepath.Join(user, "owner.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			canary := filepath.Join(user, "user-data")
			if err := os.WriteFile(canary, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			report, err := s.RecoverPrivate(ctx, false)
			if err != nil || !report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "unknown" {
				t.Fatalf("a replaced root was treated as owned storage: %+v %v", report, err)
			}
			current, err := s.Get(ctx, run.ID)
			if err != nil || current.State != Reviewing {
				t.Fatal("replacement storage became proof of owner death")
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "untouched" {
				t.Fatal("recovery erased a replacement user directory")
			}
		})
	}
}

func TestRecoveryFinishesPartiallyDeletedClosingRoot(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	// Simulate the durable shutdown barrier followed by an interrupted
	// recursive deletion which already removed the lease inode.
	if _, err := s.db.ExecContext(ctx, "UPDATE private_owners SET closing=1 WHERE id=?", owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(owner.Root, "owner.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner.Root, "remaining-private-text"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || report.NeedsAttention() || report.Owners[0].State != "recovered" {
		t.Fatalf("proven shutdown stranded partial deletion: %+v %v", report, err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed || !current.OwnerClosed {
		t.Fatalf("partial deletion lost terminal history: %+v %v", current, err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("partially deleted closed root was not reclaimed")
	}
}

func TestPrivateProcessRegistrationRequiresAnOwnedProcessGroup(t *testing.T) {
	s, owner, _ := privateOwnerRun(t)
	defer owner.Close()
	child := exec.CommandContext(context.Background(), "sleep", "30")
	_, err := lifetime.Start(owner.Context(context.Background()), child)
	if child.Process != nil {
		child.Process.Kill()
		child.Wait()
	}
	if err == nil || child.Process != nil {
		t.Fatal("a process without a dedicated group was started and recorded under a misleading group identity")
	}
	var raw string
	if err := s.db.QueryRow("SELECT processes FROM private_owners WHERE id=?", owner.ID).Scan(&raw); err != nil || raw != "{}" {
		t.Fatal("rejected process left an ambiguous launch record")
	}
}

func privateOwnerRun(t *testing.T) (*Store, *PrivateOwner, Run) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, won, err := s.Create(ctx, Run{OwnerID: owner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "owner-event", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || !won {
		t.Fatalf("create %t %v", won, err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim %t %v", won, err)
	}
	return s, owner, run
}

func TestRecoveryBoundedPassEventuallyReachesOwnersBehindLiveWork(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < RecoveryBatchLimit; i++ {
		owner, err := s.BeginPrivateOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
	}
	orphan, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := orphan.lease.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := s.RecoverPrivate(ctx, false)
	if err != nil || !first.BatchFull || len(first.Owners) != RecoveryBatchLimit {
		t.Fatalf("first bounded pass: %+v %v", first, err)
	}
	second, err := s.RecoverPrivate(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range second.Owners {
		if owner.ID == orphan.ID && owner.State == "recovered" {
			return
		}
	}
	t.Fatalf("old live owners indefinitely hide a later dead owner's resources: %+v", second)
}

// QA-X08/27: file deletion and SQLite acknowledgement are not atomic. A
// restart between those effects must finish safe cleanup, not strand the run.
func TestRecoveryFinishesDeletionWhoseDatabaseAcknowledgementFailed(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER interrupt_owner_ack BEFORE DELETE ON private_owners BEGIN SELECT RAISE(ABORT,'owned fixture interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err == nil {
		t.Fatal("fixture did not interrupt the final database acknowledgement")
	}
	if _, err := os.Lstat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("fixture did not reach storage-deleted/acknowledgement-lost boundary")
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER interrupt_owner_ack"); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "recovered" {
		t.Fatalf("safe completed cleanup stranded after restart: %+v %v", report, err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed {
		t.Fatalf("interrupted run remained active after proven execution shutdown: %+v %v", current, err)
	}
}

func TestRecoveryFinishesOwnerThatClosedJustBeforeRunFailure(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	// Service cleanup precedes its final failure defer. A crash after resource
	// closure but before that state write must not leave an immortal live run.
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverPrivate(ctx, false)
	if err != nil || report.NeedsAttention() {
		t.Fatalf("acknowledged shutdown became an unknown owner: %+v %v", report, err)
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed {
		t.Fatalf("finished owner left an executable run: %+v %v", current, err)
	}
}

func TestUnknownLegacyRunsDoNotStarveAcknowledgedShutdown(t *testing.T) {
	s, owner, run := privateOwnerRun(t)
	ctx := context.Background()
	for i := 0; i < RecoveryBatchLimit; i++ {
		legacy, won, err := s.Create(ctx, Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: fmt.Sprintf("legacy-%d", i), State: Queued, Configuration: json.RawMessage(`{}`)})
		if err != nil || !won {
			t.Fatalf("legacy fixture: %t %v", won, err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE runs SET updated_at='2000-01-01T00:00:00Z' WHERE id=?", legacy.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 3; pass++ {
		if _, err := s.RecoverPrivate(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.Get(ctx, run.ID)
	if err != nil || current.State != Failed {
		t.Fatalf("unknown legacy rows permanently hid proven shutdown: %+v %v", current, err)
	}
	var unknown int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE work_key LIKE 'legacy-%' AND state='queued' AND updated_at='2000-01-01T00:00:00Z'").Scan(&unknown); err != nil || unknown != RecoveryBatchLimit {
		t.Fatalf("recovery changed unknown legacy work: %d %v", unknown, err)
	}
}

func TestOwnerSpecificFailureDoesNotStrandOtherRecoverableOwners(t *testing.T) {
	s, failedOwner, failedRun := privateOwnerRun(t)
	ctx := context.Background()
	healthyOwner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	healthyRun, won, err := s.Create(ctx, Run{OwnerID: healthyOwner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "healthy-later-owner", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || !won {
		t.Fatalf("healthy fixture: %t %v", won, err)
	}
	if won, err := s.Claim(ctx, healthyRun.ID); err != nil || !won {
		t.Fatalf("healthy claim: %t %v", won, err)
	}
	if err := failedOwner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := healthyOwner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("CREATE TRIGGER fail_only_one_owner BEFORE UPDATE OF state ON runs WHEN OLD.id='%s' BEGIN SELECT RAISE(ABORT,'owned failure fixture'); END", failedRun.ID)
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = s.db.ExecContext(ctx, "DROP TRIGGER fail_only_one_owner")
		_, _ = s.RecoverPrivate(ctx, false)
	}()
	for pass := 0; pass < 2; pass++ {
		report, err := s.RecoverPrivate(ctx, false)
		if err == nil || !report.NeedsAttention() {
			t.Fatal("owner-specific failure was concealed")
		}
	}
	current, err := s.Get(ctx, healthyRun.ID)
	if err != nil || current.State != Failed || !current.OwnerClosed {
		t.Errorf("one failed owner stranded independently recoverable work: %+v %v", current, err)
	}
	if _, err := os.Stat(healthyOwner.Root); !os.IsNotExist(err) {
		t.Error("one failed owner prevented another dead owner's reclamation")
	}
	if _, err := os.Stat(failedOwner.Root); err != nil {
		t.Fatal("failed owner was deleted despite its rejected transition")
	}
}

func TestClosedRunFailureDoesNotStrandOtherAcknowledgedRuns(t *testing.T) {
	s, failedOwner, failedRun := privateOwnerRun(t)
	ctx := context.Background()
	if err := failedOwner.Close(); err != nil {
		t.Fatal(err)
	}
	healthyOwner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	healthyRun, won, err := s.Create(ctx, Run{OwnerID: healthyOwner.ID, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "healthy-acknowledged-owner", State: Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || !won {
		t.Fatalf("healthy fixture: %t %v", won, err)
	}
	if err := healthyOwner.Close(); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf("CREATE TRIGGER fail_only_closed_run BEFORE UPDATE OF state ON runs WHEN OLD.id='%s' BEGIN SELECT RAISE(ABORT,'owned closed-run failure'); END", failedRun.ID)
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = s.db.ExecContext(ctx, "DROP TRIGGER fail_only_closed_run") }()
	report, err := s.RecoverPrivate(ctx, false)
	if err == nil || !report.NeedsAttention() {
		t.Fatal("failed closed-run transition was hidden")
	}
	for _, owner := range report.Owners {
		if owner.ID == failedOwner.ID && owner.State != "recovery_failed" {
			t.Error("rejected closed-run transition was reported as recovered")
		}
	}
	current, err := s.Get(ctx, healthyRun.ID)
	if err != nil || current.State != Failed {
		t.Fatalf("one failed closed run stranded another acknowledged run: %+v %v", current, err)
	}
}

func TestFullFailedClosedBatchDoesNotHideLaterAcknowledgedRun(t *testing.T) {
	s, owner, first := privateOwnerRun(t)
	ctx := context.Background()
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	failed := []Run{first}
	var healthy Run
	for i := 1; i <= RecoveryBatchLimit; i++ {
		nextOwner, err := s.BeginPrivateOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run, won, err := s.Create(ctx, Run{OwnerID: nextOwner.ID, Repository: first.Repository, RepositoryID: first.RepositoryID, PullID: first.PullID, Number: first.Number, ActorID: first.ActorID, ActorLogin: first.ActorLogin, WorkKey: fmt.Sprintf("closed-batch-%02d", i), State: Queued, Configuration: json.RawMessage(`{}`)})
		if err != nil || !won {
			t.Fatalf("closed fixture: %t %v", won, err)
		}
		if err := nextOwner.Close(); err != nil {
			t.Fatal(err)
		}
		if i == RecoveryBatchLimit {
			healthy = run
		} else {
			failed = append(failed, run)
		}
	}
	// Only terminal transitions for the first full batch fail. Maintenance may
	// still persist progress, and the later independent run remains writable.
	for i := range failed {
		current, err := s.Get(ctx, failed[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		failed[i] = current
	}
	query := fmt.Sprintf("CREATE TRIGGER fail_closed_batch BEFORE UPDATE OF state ON runs WHEN OLD.id!='%s' BEGIN SELECT RAISE(ABORT,'closed batch fixture'); END", healthy.ID)
	if _, err := s.db.ExecContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = s.db.ExecContext(ctx, "DROP TRIGGER fail_closed_batch") }()
	for pass := 0; pass < 3; pass++ {
		// A fresh client must retain progress: an in-memory iterator is not
		// sufficient for repeated one-shot recovery or watcher restarts.
		client, err := Open(ctx, s.directory)
		if err != nil {
			t.Fatal(err)
		}
		report, err := client.RecoverPrivate(ctx, false)
		client.Close()
		if err == nil || !report.NeedsAttention() {
			t.Fatal("persistent recovery failures were concealed")
		}
		for _, result := range report.Owners {
			if result.ID != healthy.OwnerID && result.State != "recovery_failed" {
				t.Fatalf("failed recovery was reported as successful: %+v", result)
			}
		}
	}
	current, err := s.Get(ctx, healthy.ID)
	if err != nil || current.State != Failed {
		t.Fatalf("full failed batch hid later acknowledged work: state=%s error=%v", current.State, err)
	}
	for _, original := range failed {
		current, err := s.Get(ctx, original.ID)
		if err != nil || current.State != original.State || !current.UpdatedAt.Equal(original.UpdatedAt) || current.WorkKey != original.WorkKey || !current.OwnerClosed || len(current.Outcome) != 0 {
			t.Fatalf("failed recovery changed durable history: %+v %v", current, err)
		}
	}
}

// QA-24: a second client can retain a candidate snapshot while the first
// finishes. That observation must not fabricate new operator attention.
func TestRecoveryStaleCandidateDoesNotFabricateUnknownOwner(t *testing.T) {
	s, owner, _ := privateOwnerRun(t)
	ctx := context.Background()
	if err := owner.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverPrivate(ctx, false); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	result, err := second.recoverPrivateOwner(ctx, owner.ID, "{}", false)
	if err != nil || result.State != "already_recovered" {
		t.Fatalf("completed cleanup became a false unknown-owner warning: %+v %v", result, err)
	}
}

// QA-X05/X08/27: a dead application owner does not make a saved group safe to
// signal. Retain its resources while it is live, then reclaim after it exits.
func TestRecoveryRetainsLiveChildAndAmbiguousLaunchWithoutSignalling(t *testing.T) {
	for _, kind := range []string{"live-child", "launch-unknown"} {
		t.Run(kind, func(t *testing.T) {
			s, owner, run := privateOwnerRun(t)
			ctx := context.Background()
			canary := filepath.Join(owner.Root, "private-fixture")
			if err := os.WriteFile(canary, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			var child *exec.Cmd
			if kind == "live-child" {
				child = exec.CommandContext(ctx, "sleep", "30")
				child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if _, err := lifetime.Start(owner.Context(ctx), child); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if child != nil {
						syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
						child.Wait()
					}
				}()
			} else {
				if _, err := owner.Starting(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := owner.lease.Close(); err != nil {
				t.Fatal(err)
			}
			report, err := s.RecoverPrivate(ctx, false)
			if err != nil || !report.NeedsAttention() || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
				t.Fatalf("unsettled private child was hidden: %+v %v", report, err)
			}
			current, err := s.Get(ctx, run.ID)
			if err != nil || current.State != Failed {
				t.Fatalf("dead owner's run stayed executable: %+v %v", current, err)
			}
			if _, err := os.Stat(canary); err != nil {
				t.Fatal("possibly live storage was erased")
			}
			if child != nil {
				if err := syscall.Kill(child.Process.Pid, 0); err != nil {
					t.Fatal("recovery signalled a saved process identity")
				}
				if err := syscall.Kill(-child.Process.Pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				child.Wait()
				child = nil
				report, err = s.RecoverPrivate(ctx, false)
				if err != nil || report.NeedsAttention() || report.Owners[0].State != "recovered" {
					t.Fatalf("dead private resource could not be reclaimed: %+v %v", report, err)
				}
				if _, err := os.Stat(canary); !os.IsNotExist(err) {
					t.Fatal("stopped owned child retained private storage")
				}
			}
		})
	}
}
