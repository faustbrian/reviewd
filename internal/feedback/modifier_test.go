package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/store"
)

func modifierRun(t *testing.T, profile string, key ModifierKey) (*store.Store, *store.PrivateOwner, store.Run) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.BeginPrivateOwner(ctx)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	run, won, err := s.Create(ctx, store.Run{Purpose: "feedback", OwnerID: owner.ID, Repository: key.Host + "/owner/repo", RepositoryID: key.RepositoryID, PullID: key.PullID, Number: 17, ActorID: 31, ActorLogin: "fixture", WorkKey: filepath.Base(profile), State: store.Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil || !won {
		t.Fatal("fixture run not created", err)
	}
	return s, owner, run
}

// MODIFIER-REG-01: no durable pointer/private-owner fallback exists yet. Two
// live processes must be serialized by the stable-inode admission lease itself.
func TestModifierColdStartAcrossLiveProcessesBeforeAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
	if os.Getenv("REVIEWD_MODIFIER_HOLD") == "1" {
		lock, err := AcquireModifier(ctx, os.Getenv("REVIEWD_MODIFIER_ROOT"), key)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := json.NewEncoder(os.Stdout).Encode(map[string]bool{"ready": true}); err != nil {
			t.Fatal(err)
		}
		var release [1]byte
		if _, err := os.Stdin.Read(release[:]); err != nil {
			t.Fatal(err)
		}
		return
	}
	directory := filepath.Join(t.TempDir(), "shared")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestModifierColdStartAcrossLiveProcessesBeforeAttach$")
	command.Env = append(os.Environ(), "REVIEWD_MODIFIER_HOLD=1", "REVIEWD_MODIFIER_ROOT="+directory)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_, _ = stdin.Write([]byte{1})
		_ = stdin.Close()
		if !waited {
			if err := command.Wait(); err != nil {
				t.Error("owned admission helper stopped unsuccessfully", err)
			}
		}
	}()
	var ready struct{ Ready bool }
	if err := json.NewDecoder(stdout).Decode(&ready); err != nil || !ready.Ready {
		t.Fatal("owned admission helper never acquired its lease", err)
	}
	if _, err := os.Stat(filepath.Join(directory, modifierName(key)+".json")); !os.IsNotExist(err) {
		t.Fatal("cold-start fixture unexpectedly has a prior pointer")
	}
	if contender, err := AcquireModifier(ctx, directory, key); !errors.Is(err, ErrModifierBusy) {
		if contender != nil {
			contender.Close()
		}
		t.Fatal("cold-start admission allowed two live processes before Attach", err)
	}
	otherKey := key
	otherKey.PullID++
	other, err := AcquireModifier(ctx, directory, otherKey)
	if err != nil {
		t.Fatal("unrelated PR blocked by cold-start lease", err)
	}
	other.Close()
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		waited = true
		t.Fatal(err)
	}
	waited = true
	next, err := AcquireModifier(ctx, directory, key)
	if err != nil {
		t.Fatal("released cold-start lease could not be acquired", err)
	}
	defer next.Close()
	if next.Prior != nil {
		t.Fatal("cold-start lease manufactured a prior workflow")
	}
}

// QA08: profile admission and kernel-lock release are not child-lifetime proof.
func TestModifierSerializesProfilesAndRetainsPriorRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	locks := filepath.Join(root, "shared-modifiers")
	key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
	first, err := AcquireModifier(ctx, locks, key)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	s, owner, run := modifierRun(t, filepath.Join(root, "profile-one"), key)
	defer s.Close()
	defer owner.Close()
	if err := first.Attach(ctx, s, run.ID); err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireModifier(ctx, locks, key); !errors.Is(err, ErrModifierBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatal("same PR admitted twice", err)
	}
	otherKey := key
	otherKey.PullID++
	other, err := AcquireModifier(ctx, locks, otherKey)
	if err != nil {
		t.Fatal("independent PR blocked", err)
	}
	other.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireModifier(ctx, locks, key); !errors.Is(err, ErrModifierBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatal("released admission lease bypassed live private owner", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireModifier(ctx, locks, key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Prior == nil || second.Prior.Run.ID != run.ID || second.Prior.Run.State != store.FeedbackPending || !second.Prior.Run.OwnerClosed {
		t.Fatal("prior feedback/recovery identity was lost")
	}
	two, ownerTwo, runTwo := modifierRun(t, filepath.Join(root, "profile-two"), key)
	defer two.Close()
	defer ownerTwo.Close()
	if err := second.Attach(ctx, two, runTwo.ID); err != nil {
		t.Fatal(err)
	}
	if err := ownerTwo.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireModifier(ctx, locks, ModifierKey{Host: "GITHUB.COM", RepositoryID: 19, PullID: 71})
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if third.Prior == nil || third.Prior.Run.ID != runTwo.ID || third.Prior.Profile != two.Directory() {
		t.Fatal("latest cross-profile journal pointer was lost")
	}
}

func TestModifierDoesNotForgetMissingOrInvalidPriorProfile(t *testing.T) {
	for _, kind := range []string{"database missing", "run missing", "wrong PR", "symlink pointer"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			locks := filepath.Join(root, "shared")
			key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
			first, err := AcquireModifier(ctx, locks, key)
			if err != nil {
				t.Fatal(err)
			}
			s, owner, run := modifierRun(t, filepath.Join(root, "profile"), key)
			if err := first.Attach(ctx, s, run.ID); err != nil {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			s.Close()
			first.Close()
			pointer := filepath.Join(locks, modifierName(key)+".json")
			switch kind {
			case "database missing":
				if err := os.Remove(filepath.Join(s.Directory(), "reviewd.sqlite")); err != nil {
					t.Fatal(err)
				}
			case "run missing", "wrong PR":
				data, err := os.ReadFile(pointer)
				if err != nil {
					t.Fatal(err)
				}
				var record modifierRecord
				if json.Unmarshal(data, &record) != nil {
					t.Fatal("fixture pointer invalid")
				}
				if kind == "run missing" {
					record.RunID = strings.Repeat("b", 32)
				} else {
					record.Key.PullID++
				}
				data, _ = json.Marshal(record)
				if err := os.WriteFile(pointer, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink pointer":
				if err := os.Remove(pointer); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "foreign"), pointer); err != nil {
					t.Fatal(err)
				}
			}
			if second, err := AcquireModifier(ctx, locks, key); err == nil {
				second.Close()
				t.Fatal("unavailable or mismatched journal became new authority")
			}
			if kind == "database missing" {
				if _, err := os.Stat(filepath.Join(s.Directory(), "reviewd.sqlite")); !os.IsNotExist(err) {
					t.Fatal("missing profile database was recreated")
				}
			}
		})
	}
}

func TestModifierAbruptOwnerExitRetainsLiveChildAndDeliveredFacts(t *testing.T) {
	if os.Getenv("REVIEWD_MODIFIER_CHILD") == "1" {
		root := os.Getenv("REVIEWD_MODIFIER_ROOT")
		key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
		ctx := context.Background()
		lock, err := AcquireModifier(ctx, filepath.Join(root, "shared"), key)
		if err != nil {
			panic(err)
		}
		s, owner, run := modifierRun(t, filepath.Join(root, "profile-one"), key)
		if err := lock.Attach(ctx, s, run.ID); err != nil {
			panic(err)
		}
		if won, err := s.ClaimFeedback(ctx, run.ID); err != nil || !won {
			panic("claim failed")
		}
		if err := s.SaveFeedback(ctx, run.ID, json.RawMessage(`{"effects":[{"kind":"reply","remote_id":719,"verified":true}]}`)); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(owner.Root, "retained-input"), []byte("owned-input"), 0600); err != nil {
			panic(err)
		}
		child := exec.Command("sleep", "30")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if _, err := lifetime.Start(owner.Context(ctx), child); err != nil {
			panic(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"pid": child.Process.Pid, "run": run.ID, "root": owner.Root}); err != nil {
			panic(err)
		}
		os.Exit(0) // Deliberately no defers: durable pointer and child survive.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "reviewd-modifier-abrupt-")
	if err != nil {
		t.Fatal(err)
	}
	// An unproved child must retain its whole input scope even on test failure.
	// Do not register this live-resource root with t.TempDir's unconditional removal.
	stopped := true
	defer func() {
		if stopped {
			if err := os.RemoveAll(root); err != nil {
				t.Error(err)
			}
		} else {
			t.Errorf("unproved owned child: retained %s", root)
		}
	}()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestModifierAbruptOwnerExitRetainsLiveChildAndDeliveredFacts$")
	command.Env = append(os.Environ(), "REVIEWD_MODIFIER_CHILD=1", "REVIEWD_MODIFIER_ROOT="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stopped = false // Until the helper reports its child, shutdown is unknown.
	var observed struct {
		PID       int `json:"pid"`
		Run, Root string
	}
	if err := json.NewDecoder(stdout).Decode(&observed); err != nil {
		command.Wait()
		t.Fatal(err)
	}
	if observed.PID <= 1 {
		command.Wait()
		t.Fatal("owned helper omitted child identity")
	}
	stopped = false
	defer func() {
		if !stopped {
			_ = syscall.Kill(-observed.PID, syscall.SIGKILL)
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			for cleanup.Err() == nil {
				alive, err := lifetime.GroupAlive(cleanup, observed.PID)
				if err == nil && !alive {
					stopped = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
	if second, err := AcquireModifier(ctx, filepath.Join(root, "shared"), key); !errors.Is(err, ErrModifierBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatal("dead parent with live child admitted another modifier", err)
	}
	if data, err := os.ReadFile(filepath.Join(observed.Root, "retained-input")); err != nil || string(data) != "owned-input" {
		t.Fatal("live child's private inputs were removed", err)
	}
	if err := syscall.Kill(-observed.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for {
		alive, err := lifetime.GroupAlive(ctx, observed.PID)
		if err == nil && !alive {
			stopped = true
			break
		}
		// Darwin may lose the group between signal-zero and its selected
		// ps scan. An inspection error is not death: retry until a fresh
		// successful query positively establishes the group has stopped.
		select {
		case <-ctx.Done():
			t.Fatal("owned child shutdown was not proved", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	second, err := AcquireModifier(ctx, filepath.Join(root, "shared"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Prior == nil || second.Prior.Run.ID != observed.Run || second.Prior.Run.State != store.FeedbackPending || !second.Prior.Run.OwnerClosed || string(second.Prior.Run.Outcome) != `{"effects":[{"kind":"reply","remote_id":719,"verified":true}]}` {
		t.Fatal("recovery lost delivered facts or explicit pending state")
	}
	if _, err := os.Stat(observed.Root); !os.IsNotExist(err) {
		t.Fatal("proven dead private scope was not cleaned", err)
	}
}

func TestModifierLegacyAncestorAliasKeepsTheAuthoritativePendingRun(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(root, "physical")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	key := ModifierKey{Host: "github.com", RepositoryID: 19, PullID: 71}
	locks := filepath.Join(root, "shared")
	first, err := AcquireModifier(ctx, locks, key)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	s, owner, run := modifierRun(t, filepath.Join(physical, "profile"), key)
	defer s.Close()
	defer owner.Close()
	if err := first.Attach(ctx, s, run.ID); err != nil {
		t.Fatal(err)
	}
	if won, err := s.ClaimFeedback(ctx, run.ID); err != nil || !won {
		t.Fatal("claim", err)
	}
	if err := owner.KeepFeedbackWorkspace(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(owner.Root, "pending.txt")
	if err := os.WriteFile(pending, []byte("pending fix"), 0600); err != nil {
		t.Fatal(err)
	}
	outcome := json.RawMessage(`{"effects":[{"kind":"reply","state":"verified","id":719}]}`)
	if err := s.FinishFeedback(ctx, run.ID, store.Partial, outcome, "resume"); err != nil {
		t.Fatal(err)
	}
	if err := owner.SuspendFeedback(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// An existing schema-1 client persisted the profile's ancestor-aliased spelling.
	pointer := filepath.Join(locks, modifierName(key)+".json")
	record := modifierRecord{Schema: 1, Key: key, Profile: filepath.Join(alias, "profile"), RunID: run.ID}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, data, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := AcquireModifier(ctx, locks, key)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Prior == nil || next.Prior.Profile != s.Directory() || next.Prior.Run.ID != run.ID || next.Prior.Run.OwnerID != owner.ID || next.Prior.Run.State != store.Partial || string(next.Prior.Run.Outcome) != string(outcome) {
		t.Fatal("legacy ancestor alias lost the same authoritative pending profile/run")
	}
	persisted, err := os.ReadFile(pointer)
	if err != nil || string(persisted) != string(data) {
		t.Fatal("admission rewrote the legacy pointer", err)
	}
	if contents, err := os.ReadFile(pending); err != nil || string(contents) != "pending fix" {
		t.Fatal("admission removed unpublished work", err)
	}
}
