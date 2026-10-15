package watch

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/faustbrian/reviewd/internal/store"
)

func TestCancelledForegroundStillProjectsConsumedCompletion(t *testing.T) {
	for _, state := range []store.State{store.Cancelled, store.Prepared} {
		t.Run(string(state), func(t *testing.T) {
			f := newAdmissionFixture(t)
			f.add(t, 1, "review_requested", f.now)
			ctx := context.Background()
			items, _, err := f.durable.Inbox(ctx)
			if err != nil || len(items) != 1 {
				t.Fatalf("fixture inbox: %v %+v", err, items)
			}
			item := items[0]
			run, created, err := f.durable.CreateForItem(ctx, store.Run{Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: item.ActorID, ActorLogin: "reviewer", WorkKey: "completion", State: store.Queued, Configuration: json.RawMessage(`{}`)}, item)
			if err != nil || !created {
				t.Fatalf("create fixture: %v", err)
			}
			if claimed, err := f.durable.Claim(ctx, run.ID); err != nil || !claimed {
				t.Fatalf("claim fixture: %v", err)
			}
			if state == store.Cancelled {
				if _, err := f.durable.RequestCancellation(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.durable.Fail(ctx, run.ID, store.Cancelled, "owner stopped"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.durable.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err != nil {
					t.Fatal(err)
				}
				if err := f.manager.Reload([]byte("schema_version=1\n[github]\nrepositories=['owner/repo']\n[actions]\npublish_review='auto'\n")); err != nil {
					t.Fatal(err)
				}
			}
			run, err = f.durable.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			engine := &Engine{Store: f.durable, Configuration: f.manager}
			engine.initialize()
			stopped := false
			engine.active[item.ID] = activeWork{item: item, cancel: func() { stopped = true }}
			var workerErr error
			if state == store.Cancelled {
				workerErr = context.Canceled
			}
			engine.done <- completion{item: item, run: run, err: workerErr}
			foreground, cancel := context.WithCancel(ctx)
			cancel()
			if err := engine.Complete(foreground, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost worker cancellation: %v", err)
			}
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}
			items, _, err = f.durable.Inbox(ctx)
			if err != nil || len(items) != 1 || items[0].State != string(state) || items[0].RunID != run.ID {
				t.Fatalf("consumed completion was not projected after cancellation: %+v %v", items, err)
			}
			if !stopped || engine.Active() != 0 {
				t.Fatal("completion retained active execution")
			}
		})
	}
	// No Connect, Reviewer or Announce capability exists: cancellation projection
	// cannot depend on a new model, a GitHub read or publication.
}
