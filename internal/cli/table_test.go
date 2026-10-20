package cli

import (
	"context"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTableProjectionUsesPRAuthorAndFrozenSettings(t *testing.T) {
	_, s, item := terminalFixture(t)
	ctx := context.Background()
	frozen, err := config.Resolve(nil, item.Repository, nil, map[string]any{"reviewer.model": "accepted-model", "reviewer.reasoning_effort": "high"})
	if err != nil {
		t.Fatal(err)
	}
	configuration, _ := json.Marshal(frozen)
	pull, _ := json.Marshal(github.Pull{PullReference: github.PullReference{Repository: item.Repository, Number: item.Number}, ID: item.PullID, Author: github.Identity{ID: 7, Login: "pr-author"}, Title: "Accepted title"})
	r, _, err := s.Create(ctx, store.Run{Executor: store.NativeExecutor, Purpose: "review", Repository: item.Repository, RepositoryID: 99, PullID: item.PullID, Number: item.Number, ActorID: 43, ActorLogin: "operator-not-author", WorkKey: "frozen", State: store.Queued, Configuration: configuration, Context: pull})
	if err != nil {
		t.Fatal(err)
	}
	s.ClaimAgent(ctx, r.ID)
	s.RecordAgent(ctx, r.ID, "session", "accepted-model", nil, "Reading code")
	current, err := config.Parse([]byte("schema_version=1\n[reviewer]\nmodel='new-model'\nreasoning_effort='low'\n"), "reload")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, s, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snapshot.Entries {
		if e.RunID == r.ID {
			if !strings.Contains(e.Details, "Accepted title") || !strings.Contains(e.Details, "State: running") {
				t.Fatal("accepted title/state missing from details")
			}
			if e.Author != "pr-author" || e.Model != "accepted-model" || e.Effort != "high" || e.Lane != "review" || e.Activity != "Reading code" {
				t.Fatalf("incorrect execution projection: %+v", e)
			}
			return
		}
	}
	t.Fatal("run missing")
}

func TestWaitingReasonsUseIndependentExistingFacts(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	policy, _ := config.Resolve(nil, "acme/api", nil, nil)
	item := store.InboxItem{ID: "item", Repository: "github.com/acme/api", State: "queued", Trigger: "review_requested", UpdatedAt: now.Add(-time.Minute)}
	checks := []struct {
		name   string
		change func(*store.InboxItem, *store.Controls)
		want   string
	}{
		{"baseline", func(i *store.InboxItem, c *store.Controls) { i.Baseline = true }, "Initial backlog"},
		{"pause", func(i *store.InboxItem, c *store.Controls) { c.PausedProjects = []string{i.Repository} }, "Project paused"},
		{"snooze", func(i *store.InboxItem, c *store.Controls) {
			c.Snoozes = []store.Snooze{{ItemID: i.ID, Until: now.Add(time.Hour)}}
		}, "Snoozed until"},
		{"retry", func(i *store.InboxItem, c *store.Controls) { i.RetryAt = now.Add(time.Hour) }, "Retry at"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			i := item
			c := store.Controls{}
			check.change(&i, &c)
			got := waitingReasons(i, policy, c, nil, nil, now, 0, 2, true, false)
			if !strings.Contains(got, check.want) || strings.Contains(got, "slots occupied") {
				t.Fatal(got)
			}
		})
	}
	if got := waitingReasons(item, policy, store.Controls{}, nil, nil, now, 2, 2, true, false); !strings.Contains(got, "slots occupied") {
		t.Fatal(got)
	}
	if got := waitingReasons(item, policy, store.Controls{}, nil, nil, now, 0, 2, true, true); !strings.Contains(got, "owns this PR") {
		t.Fatal(got)
	}
	if got := waitingReasons(item, policy, store.Controls{}, nil, nil, now, 0, 2, false, false); !strings.Contains(got, "exact wait reason not recorded") {
		t.Fatal(got)
	}
	item.RetryAt = now
	if got := waitingReasons(item, policy, store.Controls{}, nil, nil, now, 0, 2, true, false); strings.Contains(got, "Retry at") {
		t.Fatal("expired retry still held", got)
	}
}

func TestTableProjectionReadsObservationWithoutMisattributingLegacyAuthor(t *testing.T) {
	_, s, item := terminalFixture(t)
	ctx := context.Background()
	scope, err := s.DiscoveryScope(ctx, item.Repository)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pull": github.Pull{PullReference: github.PullReference{Repository: item.Repository, Number: item.Number}, ID: item.PullID, Author: github.Identity{Login: "discovered-author"}}})
	if err := s.ApplyDiscovery(ctx, scope, scope, []store.Observation{{PullID: item.PullID, Number: item.Number, HeadSHA: item.HeadSHA, Record: raw}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, s, nil)
	if err != nil || len(snapshot.Entries) != 1 || snapshot.Entries[0].Author != "discovered-author" {
		t.Fatalf("observation author lost: %+v %v", snapshot, err)
	}
	legacy := terminal.Entry{}
	projectRun(&legacy, store.Run{ActorLogin: "operator-not-author", Purpose: "feedback", State: store.AgentCompleted})
	if legacy.Author != "" || legacy.Model != "unknown" || legacy.Effort != "unknown" || !legacy.ActivityAt.IsZero() || legacy.Lane != "feedback" {
		t.Fatalf("legacy identity or settings fabricated: %+v", legacy)
	}
}

func TestTableDetailsRetainFullTitleAndState(t *testing.T) {
	_, s, item := terminalFixture(t)
	snapshot, err := (&execution{}).terminalSnapshot(context.Background(), s, nil)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("snapshot: %v", err)
	}
	details := snapshot.Entries[0].Details
	if !strings.Contains(details, item.Title) || !strings.Contains(details, "pending decision") {
		t.Fatalf("selected details omit title/state: %s", details)
	}
}

func TestTableRetainsEveryRepositoryDiscoveryFailure(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, repo := range []string{"github.com/acme/first", "github.com/acme/second"} {
		scope, err := s.DiscoveryScope(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DiscoveryFailure(ctx, scope, "Unavailable: "+repo); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapshot)
	for _, repo := range []string{"acme/first", "acme/second"} {
		if !strings.Contains(string(raw), repo) {
			t.Fatalf("repository failure lost: %s", repo)
		}
	}
}

func TestTableDisplayRedactionDoesNotChangeRepositoryActions(t *testing.T) {
	path, s, item := terminalFixture(t)
	ctx := context.Background()
	repo := "github.com/acme/ghp_" + strings.Repeat("a", 35)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `["owner/repo"]`, `["owner/repo","`+repo+`"]`, 1))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse(raw, path)
	if err != nil {
		t.Fatal("valid repository config rejected")
	}
	previous, err := s.DiscoveryScope(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	next := previous
	next.ActorID = 43
	next.RepositoryID = 100
	next.State = "ok"
	item.Repository = repo
	item.ID = ""
	item.EventKey = "redacted-name-event"
	if err := s.ApplyDiscovery(ctx, previous, next, []store.Observation{{PullID: item.PullID, Number: item.Number, Record: json.RawMessage(`{}`)}}, []store.InboxItem{item}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&execution{}).terminalSnapshot(ctx, s, document)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range snapshot.Entries {
		if entry.EventKey != item.EventKey {
			continue
		}
		if entry.Project != repo {
			t.Fatal("presentation changed canonical action repository")
		}
		if _, err := terminalAction(ctx, []string{"--config", path}, terminal.Action{Kind: "pause", Entry: entry}, Dependencies{}); err != nil {
			t.Fatal("pause failed for valid canonical repository")
		}
		controls, err := s.Controls(ctx, time.Now())
		if err != nil || !controls.Paused(repo) {
			t.Fatal("pause did not affect canonical repository")
		}
		if strings.Contains(entry.Details, repo) {
			t.Fatal("display details leaked credential-shaped name")
		}
		return
	}
	t.Fatal("new repository missing")
}
