// Package notify presents durable attention without owning review decisions.
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
)

type Message struct{ Title, Body string }
type Sink func(context.Context, Message) error

type Service struct {
	Store             *store.Store
	Desktop, Terminal Sink
	Now               func() time.Time
	Warn              func(error)
}

func fingerprint(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

// quietAt compares the actual instant's local wall clock. Both occurrences of
// a repeated DST hour are quiet; skipped local minutes need no invented timer.
// Intervals are start-inclusive/end-exclusive, including across midnight.
func quietAt(now time.Time, location *time.Location, start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	// Go's clock parser has always accepted H:MM as well as HH:MM. Keep
	// persisted configuration identities intact, but compare canonical clocks.
	startClock, startErr := time.Parse("15:04", start)
	endClock, endErr := time.Parse("15:04", end)
	if startErr != nil || endErr != nil {
		return true // Invalid internal input cannot silently bypass quiet hours.
	}
	start, end = startClock.Format("15:04"), endClock.Format("15:04")
	clock := now.In(location).Format("15:04")
	if start < end {
		return clock >= start && clock < end
	}
	return clock >= start || clock < end
}

func quiet(now time.Time, policy config.Snapshot) bool {
	location, _ := time.LoadLocation(policy.String("notifications.timezone"))
	return quietAt(now, location, policy.String("notifications.quiet_start"), policy.String("notifications.quiet_end"))
}

// Pump runs inside the existing foreground owner's loop. It performs no
// GitHub or model work. force ends a one-shot grouping window, never quiet
// hours, and never a pending start/publication decision.
func (s *Service) Pump(ctx context.Context, document *config.Document, force bool) error {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		return err
	}
	policies := map[string]config.Snapshot{}
	for _, repository := range defaults.Strings("github.repositories") {
		policy, err := config.Resolve(document, repository, nil, nil)
		if err != nil {
			return err
		}
		policies[repository] = policy
	}
	items, scopes, err := s.Store.Inbox(ctx)
	if err != nil {
		return err
	}
	runs, err := s.Store.NotificationRuns(ctx)
	if err != nil {
		return err
	}
	actors := map[string]int64{}
	observations := []store.Notification{}
	add := func(note store.Notification, actionable bool, observed time.Time) {
		policy, allowed := policies[note.Repository]
		if !allowed {
			return
		}
		note.Pending = actionable && (policy.Bool("notifications.desktop") || policy.Bool("notifications.terminal"))
		note.DueAt = now.Add(policy.Duration("notifications.group_interval"))
		note.Digest = quiet(now, policy) || (!observed.IsZero() && now.Sub(observed) > 5*time.Minute)
		observations = append(observations, note)
	}
	for _, scope := range scopes {
		if scope.State == "inactive" {
			continue
		}
		actors[scope.Repository] = scope.ActorID
		add(store.Notification{Source: "scope:" + scope.Repository, Repository: scope.Repository, ActorID: scope.ActorID,
			Fingerprint: fingerprint(scope.State, fmt.Sprint(scope.ActorID)),
			Body:        "Discovery needs attention; inspect reviewd inbox and reviewd doctor."}, scope.State == "failed", scope.UpdatedAt)
	}
	for _, item := range items {
		actionable := item.State == "pending_decision" || item.State == "attention" || item.State == "failed"
		body := "Review requested; use reviewd start " + item.ID + " or reviewd skip " + item.ID + "."
		if item.State == "attention" {
			body = "Review attention; inspect reviewd inbox."
		}
		if item.Category == "own_pr_feedback" {
			body = "Incoming feedback; inspect the PR conversation and reviewd inbox. No fixes are started."
		}
		if item.State == "failed" {
			body = "Review work needs attention; inspect reviewd inbox."
		}
		add(store.Notification{Source: "item:" + item.ID, Repository: item.Repository, ActorID: item.ActorID,
			PullID: item.PullID, Number: item.Number, Fingerprint: fingerprint(item.EventKey, item.State),
			Body: body, Initial: item.Baseline}, actionable, item.UpdatedAt)
	}
	for _, run := range runs {
		if actor, exists := actors[run.Repository]; exists && actor > 0 && actor != run.ActorID {
			continue
		}
		body := ""
		switch run.State {
		case store.Reviewing:
			if len(run.Inputs) > 0 {
				body = "Reviewer asks for clarification; inspect reviewd inputs " + run.ID + ". No answer is selected automatically."
			}
		case store.Prepared:
			body = "Review prepared; preview the exact result with reviewd show " + run.ID + "."
		case store.Published:
			body = "Review publication verified; inspect reviewd show " + run.ID + "."
		case store.Failed:
			body = "Review failed; inspect reviewd show " + run.ID + "."
		case store.Stale:
			body = "Review is stale and does not certify the current PR; inspect reviewd show " + run.ID + "."
		case store.Partial, store.PublicationUnknown:
			body = "Publication needs reconciliation, not a blind retry; inspect reviewd show " + run.ID + "."
		}
		identity := fingerprint(string(run.State), run.PayloadID)
		if inputs := inputFingerprint(run); inputs != "" {
			identity = fingerprint(string(run.State), run.PayloadID, inputs)
		}
		add(store.Notification{Source: "run:" + run.ID, Repository: run.Repository, ActorID: run.ActorID,
			PullID: run.PullID, Number: run.Number, Fingerprint: identity, Body: body}, body != "", run.CreatedAt)
	}
	pending, err := s.Store.ObserveNotifications(ctx, observations)
	if err != nil {
		return err
	}
	type group struct {
		notes             []store.Notification
		desktop, terminal bool
		digest            bool
	}
	groups := map[string]*group{}
	controls, err := s.Store.Controls(ctx, now)
	if err != nil {
		return err
	}
	heldRuns := map[string]bool{}
	for _, run := range runs {
		if run.InboxItemID != "" && controls.Snoozed(run.InboxItemID) {
			heldRuns["run:"+run.ID] = true
		}
	}
	for _, item := range items {
		if item.RunID != "" && controls.Snoozed(item.ID) {
			heldRuns["run:"+item.RunID] = true
		}
	}
	for _, note := range pending {
		policy := policies[note.Repository]
		if quiet(now, policy) || controls.Paused(note.Repository) || controls.Snoozed(strings.TrimPrefix(note.Source, "item:")) || heldRuns[note.Source] {
			continue
		}
		desktop, terminal := policy.Bool("notifications.desktop"), policy.Bool("notifications.terminal")
		key := fmt.Sprintf("%s:%d:%d", note.Repository, note.ActorID, note.PullID)
		digest := note.Digest || note.Initial
		if digest {
			// Aggregate backlog across projects with the same delivery settings.
			// Quiet projects are excluded above; one project's mute cannot leak.
			key = fmt.Sprintf("digest:%t:%t", desktop, terminal)
		}
		if groups[key] == nil {
			groups[key] = &group{desktop: desktop, terminal: terminal, digest: digest}
		}
		groups[key].notes = append(groups[key].notes, note)
	}
	if len(groups) > 5 {
		// Reconnection can reveal many PRs at once. Bound that burst by
		// delivery-policy groups rather than replaying one popup per PR.
		folded := map[string]*group{}
		for _, batch := range groups {
			key := fmt.Sprintf("digest:%t:%t", batch.desktop, batch.terminal)
			if folded[key] == nil {
				folded[key] = &group{desktop: batch.desktop, terminal: batch.terminal, digest: true}
			}
			for _, note := range batch.notes {
				note.Digest = true
				folded[key].notes = append(folded[key].notes, note)
			}
		}
		groups = folded
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		batch := groups[key]
		due := false
		for _, note := range batch.notes {
			due = due || !now.Before(note.DueAt)
		}
		if !force && !due {
			continue
		}
		taken, err := s.Store.TakeNotifications(ctx, batch.notes)
		if err != nil {
			return err
		}
		if len(taken) == 0 {
			continue
		}
		message := present(taken, batch.digest)
		for _, delivery := range []struct {
			enabled bool
			sink    Sink
		}{{batch.terminal, s.Terminal}, {batch.desktop, s.Desktop}} {
			if delivery.enabled && delivery.sink != nil {
				if err := delivery.sink(ctx, message); err != nil && s.Warn != nil {
					s.Warn(err)
				}
			}
		}
	}
	return nil
}

func identity(note store.Notification) string {
	repository := strings.TrimPrefix(note.Repository, "github.com/")
	if note.Number > 0 {
		return fmt.Sprintf("%s #%d", repository, note.Number)
	}
	return repository
}

func present(notes []store.Notification, digest bool) Message {
	sort.Slice(notes, func(i, j int) bool { return notes[i].Source < notes[j].Source })
	message := Message{Title: identity(notes[0])}
	entries := []string{}
	seen, pulls, projects := map[string]bool{}, map[string]bool{}, map[string]bool{}
	initial, quietDigest := false, false
	for _, note := range notes {
		initial = initial || note.Initial
		quietDigest = quietDigest || note.Digest
		label := identity(note)
		if note.PullID > 0 {
			pulls[fmt.Sprintf("%s:%d:%d", note.Repository, note.ActorID, note.PullID)] = true
		} else {
			projects[note.Repository] = true
		}
		text := note.Body
		if digest {
			text = label + ": " + text
		}
		if !seen[text] && len(entries) < 5 {
			entries = append(entries, text)
			seen[text] = true
		}
	}
	if digest {
		message.Title = "Review digest"
		if initial && !quietDigest {
			message.Title = "Review inbox"
		}
		message.Body = fmt.Sprintf("%d pull requests need attention. ", len(pulls))
		if len(projects) > 0 {
			message.Body = fmt.Sprintf("%d pull requests and %d projects need attention. ", len(pulls), len(projects))
		}
	}
	message.Body += strings.Join(entries, " ") + " All work remains available in reviewd inbox."
	message.Title, message.Body = safeText(message.Title, 160), safeText(message.Body, 1200)
	return message
}

func safeText(value string, limit int) string {
	runes := []rune(strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return ' '
		}
		return char
	}, value))
	if len(runes) > limit {
		runes = append(runes[:limit-1], '…')
	}
	return string(runes)
}

func inputFingerprint(run store.Run) string {
	ids := []string{}
	for _, input := range run.Inputs {
		ids = append(ids, input.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
