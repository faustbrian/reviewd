package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// DC-04/05: maximum deadlines are atomic across connections, host-scoped,
// absolute (including nanoseconds), and independent across profile databases.
func TestGitHubCooldownMaximumPersistsWithoutBlockingOtherHostsOrProfiles(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "profile")
	s, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	other, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	now := time.Date(2026, 10, 14, 1, 0, 0, 123456789, time.UTC)
	long := now.Add(time.Hour)
	results := make(chan error, 8)
	for i := range 8 {
		go func(i int) {
			client := s
			if i%2 == 1 {
				client = other
			}
			results <- client.DeferGitHub(ctx, "GitHub.COM", now.Add(time.Duration(i+1)*time.Hour/8))
		}(i)
	}
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeferGitHub(ctx, "github.com", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GitHubRetryAt(ctx, "GITHUB.COM"); err != nil || !got.Equal(long) {
		t.Fatalf("deadline shortened or lost across reopen: %v %v", got, err)
	}
	for _, point := range []struct {
		at    time.Time
		count int
	}{{long.Add(-time.Nanosecond), 1}, {long, 0}, {long.Add(time.Nanosecond), 0}} {
		rows, err := s.GitHubCooldowns(ctx, point.at)
		if err != nil || len(rows) != point.count {
			t.Fatalf("absolute boundary %v: %+v %v", point.at, rows, err)
		}
	}
	if got, err := s.GitHubRetryAt(ctx, "enterprise.example"); err != nil || !got.IsZero() {
		t.Fatal("cooldown leaked across hosts")
	}
	independent, err := Open(ctx, filepath.Join(t.TempDir(), "independent"))
	if err != nil {
		t.Fatal(err)
	}
	defer independent.Close()
	if got, err := independent.GitHubRetryAt(ctx, "github.com"); err != nil || !got.IsZero() {
		t.Fatal("cooldown leaked across profiles")
	}
	// A successful unrelated update cannot clear a server constraint.
	if err := s.RecoverScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GitHubRetryAt(ctx, "github.com"); err != nil || !got.Equal(long) {
		t.Fatal("local recovery erased cooldown")
	}
}

func TestCooldownMigrationPreservesExistingWork(t *testing.T) {
	s, item := schedulingFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "DROP TABLE github_cooldowns; PRAGMA user_version=6"); err != nil {
		t.Fatal(err)
	}
	directory := s.directory
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	items, _, err := upgraded.Inbox(ctx)
	if err != nil || len(items) != 1 || items[0].ID != item.ID || items[0].EventKey != item.EventKey {
		t.Fatal("cooldown upgrade changed existing work")
	}
	if err := upgraded.DeferGitHub(ctx, "github.com", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}
