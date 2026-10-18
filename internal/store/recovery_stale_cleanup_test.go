package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Capture the exact stale-observation boundary without depending on scheduler
// timing: another proven cleaner finishes deletion after our identity read.
func TestPrivateRootCleanupReconcilesAnotherCompletedCleaner(t *testing.T) {
	for _, purpose := range []string{"review", "feedback"} {
		for _, stage := range []string{"acknowledged", "closing barrier", "missing unproven", "replacement"} {
			t.Run(purpose+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				s, owner, run := privateOwnerRun(t)
				journal := json.RawMessage(`{"effects":[{"kind":"reply","remote_id":719,"verified":true}]}`)
				if purpose == "feedback" {
					var err error
					run, _, err = s.Create(ctx, Run{Purpose: "feedback", OwnerID: owner.ID, Repository: run.Repository, RepositoryID: run.RepositoryID, PullID: run.PullID, Number: run.Number, ActorID: run.ActorID, ActorLogin: run.ActorLogin, WorkKey: "closing-feedback", State: Queued, Configuration: json.RawMessage(`{}`)})
					if err != nil {
						t.Fatal(err)
					}
					if won, err := s.ClaimFeedback(ctx, run.ID); err != nil || !won {
						t.Fatal("feedback claim failed", err)
					}
					if err := s.SaveFeedback(ctx, run.ID, journal); err != nil {
						t.Fatal(err)
					}
				}
				observed, err := os.Stat(owner.Root)
				if err != nil {
					t.Fatal(err)
				}
				// Holding the old directory avoids inode reuse in the replacement case.
				held, err := os.Open(owner.Root)
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
				user := t.TempDir()
				canary := filepath.Join(user, "untouched")
				if err := os.WriteFile(canary, []byte("user"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(user, filepath.Join(owner.Root, "user-link")); err != nil {
					t.Fatal(err)
				}
				if stage != "missing unproven" {
					if err := s.failLostOwner(ctx, owner.ID); err != nil {
						t.Fatal(err)
					}
				}
				switch stage {
				case "acknowledged", "replacement":
					if err := owner.Close(); err != nil {
						t.Fatal(err)
					}
					if stage == "replacement" {
						if err := os.Mkdir(owner.Root, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(owner.Root, "replacement-data"), []byte("foreign"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				case "closing barrier", "missing unproven":
					if stage == "closing barrier" {
						if _, err := s.db.ExecContext(ctx, "UPDATE private_owners SET closing=1 WHERE id=?", owner.ID); err != nil {
							t.Fatal(err)
						}
					}
					if err := owner.lease.Close(); err != nil {
						t.Fatal(err)
					}
					if err := os.RemoveAll(owner.Root); err != nil {
						t.Fatal(err)
					}
				}
				err = s.removePrivateRoot(ctx, owner.ID, owner.Root, "{}", observed)
				settled := stage == "acknowledged" || stage == "closing barrier"
				if settled && err != nil {
					t.Fatalf("completed concurrent deletion manufactured attention: %v", err)
				}
				if !settled && err == nil {
					t.Fatal("unproven or replaced root treated as completed cleanup")
				}
				actual, err := s.Get(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected := Failed
				if purpose == "feedback" {
					expected = FeedbackPending
				}
				if stage == "missing unproven" {
					expected = Reviewing
					if purpose == "feedback" {
						expected = HandlingFeedback
					}
				}
				if actual.State != expected || actual.WorkKey != run.WorkKey || actual.OwnerClosed != (stage != "missing unproven") {
					t.Fatalf("cleanup lost durable work/closure identity: %+v", actual)
				}
				if purpose == "feedback" && string(actual.Outcome) != string(journal) {
					t.Fatal("cleanup discarded delivered feedback effects")
				}
				if data, err := os.ReadFile(canary); err != nil || string(data) != "user" {
					t.Fatal("cleanup followed source link into user storage")
				}
				if stage == "replacement" {
					if data, err := os.ReadFile(filepath.Join(owner.Root, "replacement-data")); err != nil || string(data) != "foreign" {
						t.Fatal("cleanup removed replacement user storage")
					}
				}
			})
		}
	}
}

func TestPrivateRootCleanupHonorsShutdownBarrierWithoutAcceptingLateLaunch(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new launch", true: "existing shutdown barrier"}[closing], func(t *testing.T) {
			s, owner, run := privateOwnerRun(t)
			ctx := context.Background()
			observed := "{}"
			current := `{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":0}`
			if closing {
				// Another cleaner has already proved shutdown and cleared the
				// old process snapshot. A stale observer cannot undo that fact.
				observed, current = `{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":123}`, "{}"
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE private_owners SET closing=?,processes=? WHERE id=?", closing, current, owner.ID); err != nil {
				t.Fatal(err)
			}
			err := s.removePrivateRoot(ctx, owner.ID, owner.Root, observed, owner.info)
			if closing && err != nil {
				t.Fatalf("proven shutdown lost to stale process observation: %v", err)
			}
			if !closing && err == nil {
				t.Fatal("late process registration was mistaken for shutdown")
			}
			actual, err := s.Get(ctx, run.ID)
			if err != nil || actual.OwnerClosed != closing {
				t.Fatal("incorrect durable shutdown fact", err)
			}
			_, statErr := os.Stat(owner.Root)
			if closing && !os.IsNotExist(statErr) || !closing && statErr != nil {
				t.Fatal("cleanup crossed shutdown boundary", statErr)
			}
			if err := owner.lease.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
