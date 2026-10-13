package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faustbrian/reviewd/internal/store"
)

func TestInboxJSONPreservesOpaqueIntegerIdentity(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	actor := int64(9007199254740993)
	previous := store.DiscoveryScope{Repository: "github.com/owner/repo"}
	next := previous
	next.ActorID = actor
	next.RepositoryID = 99
	next.State = "ok"
	item := store.InboxItem{Repository: previous.Repository, ActorID: actor, PullID: 700, Number: 17, Category: "reviews_owed", State: "pending_decision", EventKey: "one-request"}
	if err := durable.ApplyDiscovery(ctx, previous, next, nil, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := (&execution{json: true}).presentInbox(ctx, &output, durable); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []struct {
			ActorID int64 `json:"actor_id"`
		}
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || len(result.Items) != 1 || result.Items[0].ActorID != actor {
		t.Fatalf("integer identity changed in JSON: %s", output.String())
	}
}

func TestInboxDoesNotReportUnfinishedAcquisitionAsSuccessfulEmpty(t *testing.T) {
	ctx := context.Background()
	durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "profile"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	if _, err := durable.ConfigureDiscovery(ctx, []string{"github.com/owner/repo"}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := (&execution{}).presentInbox(ctx, &output, durable); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "last successful synchronization") || !strings.Contains(output.String(), "not completed") {
		t.Fatalf("unfinished acquisition looked successful: %s", output.String())
	}
}
