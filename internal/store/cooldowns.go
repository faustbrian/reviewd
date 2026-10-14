package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
)

const cooldownSchema = `CREATE TABLE IF NOT EXISTS github_cooldowns (
 host TEXT PRIMARY KEY, until_at TEXT NOT NULL
);`

// Fixed precision preserves timestamp ordering in the atomic maximum update,
// including server durations beyond the Unix-nanosecond range.
const cooldownTime = "2006-01-02T15:04:05.000000000Z"

type GitHubCooldown struct {
	Host  string    `json:"host"`
	Until time.Time `json:"until"`
}

func cooldownHost(host string) (string, error) {
	canonical, err := config.Repository(host, "reviewd/cooldown")
	if err != nil {
		return "", err
	}
	name, _, _ := strings.Cut(canonical, "/")
	return name, nil
}

// DeferGitHub extends but never shortens a profile/host server constraint.
// It does not change work state, retry attempts, or publication authority.
func (s *Store) DeferGitHub(ctx context.Context, host string, until time.Time) error {
	host, err := cooldownHost(host)
	if err != nil {
		return err
	}
	if until.IsZero() {
		return errors.New("GitHub cooldown requires a deadline")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO github_cooldowns(host,until_at) VALUES(?,?)
 ON CONFLICT(host) DO UPDATE SET until_at=max(github_cooldowns.until_at,excluded.until_at)`, host, until.UTC().Format(cooldownTime))
	return err
}

func (s *Store) GitHubRetryAt(ctx context.Context, host string) (time.Time, error) {
	host, err := cooldownHost(host)
	if err != nil {
		return time.Time{}, err
	}
	var raw string
	err = s.db.QueryRowContext(ctx, "SELECT until_at FROM github_cooldowns WHERE host=?", host).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(cooldownTime, raw)
}

func (s *Store) GitHubCooldowns(ctx context.Context, now time.Time) ([]GitHubCooldown, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT host,until_at FROM github_cooldowns WHERE until_at>? ORDER BY host", now.UTC().Format(cooldownTime))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []GitHubCooldown{}
	for rows.Next() {
		var row GitHubCooldown
		var raw string
		if err := rows.Scan(&row.Host, &raw); err != nil {
			return nil, err
		}
		row.Until, err = time.Parse(cooldownTime, raw)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
