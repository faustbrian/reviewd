package notify

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/clarification"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/store"
)

func document(t *testing.T, extra string) *config.Document {
	t.Helper()
	d, err := config.Parse([]byte("schema_version=1\n[github]\nrepositories=[\"owner/repo\"]\n[notifications]\ngroup_interval=\"5s\"\n"+extra), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func seed(t *testing.T, s *store.Store, items ...store.InboxItem) {
	t.Helper()
	ctx := context.Background()
	old, err := s.DiscoveryScope(ctx, "github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.State = "ok"
	next.ActorID = 43
	next.RepositoryID = 99
	next.Initialized = true
	next.UpdatedAt = time.Now().UTC()
	rows := []store.Observation{}
	seen := map[int64]bool{}
	for i := range items {
		items[i].Repository = next.Repository
		items[i].ActorID = 43
		if !seen[items[i].PullID] {
			rows = append(rows, store.Observation{PullID: items[i].PullID, Number: items[i].Number, Record: json.RawMessage(`{}`)})
			seen[items[i].PullID] = true
		}
	}
	if err := s.ApplyDiscovery(ctx, old, next, rows, items); err != nil {
		t.Fatal(err)
	}
}
func work(category, event string, number int) store.InboxItem {
	return store.InboxItem{PullID: int64(700 + number), Number: number, Category: category, Trigger: "review_requested", State: "pending_decision", Action: "ask", EventKey: event}
}

func TestFeedbackNotificationQueuesTheConfiguredSkill(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	var messages []Message
	service := Service{Store: s, Now: func() time.Time { return now }, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	item := work("feedback_handling", "human-feedback", 1)
	item.ExecutionKind = "feedback_v1"
	seed(t, s, item)
	doc := document(t, "desktop=false\n")
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "reviewd start ") || !strings.Contains(messages[0].Body, "feedback skill") {
		t.Fatalf("fixing event suggested private consent: %+v", messages)
	}
	item.State = "queued"
	seed(t, s, item)
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	if err := s.SetFeedbackExecutionHold(ctx, items[0], "Owned unavailable backend"); err != nil {
		t.Fatal(err)
	}
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || !strings.Contains(messages[1].Body, "held") {
		t.Fatalf("unavailable feedback hold was hidden: %+v", messages)
	}
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatal("unchanged unavailable hold notified again")
	}
}
func TestGroupedNotificationsSurviveRestartAndDoNotChangeInboxAuthority(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	now := time.Now().UTC()
	var messages []Message
	sink := func(_ context.Context, m Message) error { messages = append(messages, m); return nil }
	service := Service{Store: s, Now: func() time.Time { return now }, Terminal: sink}
	doc := document(t, "desktop=false\n")
	a, b := work("reviews_owed", "request-1", 1), work("own_pr_feedback", "comments-1", 1)
	b.Trigger = "review_comment_added"
	b.State = "attention"
	b.Action = "notify"
	seed(t, s, a, b)
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatal("group window delivered immediately")
	}
	now = now.Add(4 * time.Second)
	b.EventKey = "comments-2"
	seed(t, s, a, b)
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	service.Store = s
	now = now.Add(time.Second)
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Title, "owner/repo #1") || !strings.Contains(messages[0].Body, "feedback") || !strings.Contains(messages[0].Body, "start") {
		t.Fatalf("burst was not one actionable PR notification: %+v", messages)
	}
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatal("repeat observation notified twice")
	}
	items, _, err := s.Inbox(ctx)
	if err != nil || len(items) != 2 {
		t.Fatal("notification erased inbox")
	}
	for _, i := range items {
		if i.State != "pending_decision" && i.State != "attention" {
			t.Fatalf("notification granted authority: %+v", i)
		}
	}
	a.EventKey = "same-head-explicit-rerequest"
	seed(t, s, a, b)
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatal("explicit same-head re-request was suppressed")
	}
}
func TestQuietHoursReleaseOneDigestAndDisabledProjectDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC)
	var messages []Message
	service := Service{Store: s, Now: func() time.Time { return now }, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	doc := document(t, "desktop=false\nquiet_start=\"22:00\"\nquiet_end=\"07:00\"\ntimezone=\"UTC\"\n")
	a, b := work("reviews_owed", "first", 1), work("reviews_owed", "second", 2)
	seed(t, s, a, b)
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatal("force granted permission to bypass quiet hours")
	}
	now = now.Add(8*time.Hour + 59*time.Minute)
	a.EventKey = "burst-latest"
	seed(t, s, a, b)
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := service.Pump(ctx, doc, false); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Title != "Review digest" || !strings.Contains(messages[0].Body, "2 pull requests") {
		t.Fatalf("quiet backlog was replayed as a storm: %+v", messages)
	}
	disabled := document(t, "desktop=false\n[projects.\"owner/repo\".notifications]\nterminal=false\n")
	a.EventKey = "disabled"
	seed(t, s, a, b)
	if err := service.Pump(ctx, disabled, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatal("project notification setting ignored")
	}
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatal("reenabling notifications replayed already observed work")
	}
}
func TestInitialBacklogAndDesktopFailureAreNonAuthoritativeBestEffort(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	var messages []Message
	warnings := 0
	desktopAttempts := 0
	service := Service{Store: s, Now: func() time.Time { return now }, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }, Desktop: func(context.Context, Message) error { desktopAttempts++; return errors.New("no desktop session") }, Warn: func(error) { warnings++ }}
	items := []store.InboxItem{}
	for n := 1; n <= 20; n++ {
		item := work("reviews_owed", "baseline", n)
		item.Baseline = true
		items = append(items, item)
	}
	seed(t, s, items...)
	doc := document(t, "")
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatalf("desktop failure ended watcher: %v", err)
	}
	if desktopAttempts != 1 || warnings != 1 || len(messages) != 1 || messages[0].Title != "Review inbox" || !strings.Contains(messages[0].Body, "20 pull requests") {
		t.Fatalf("initial backlog storm or failure: desktop=%d warnings=%d %+v", desktopAttempts, warnings, messages)
	}
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if desktopAttempts != 1 || len(messages) != 1 {
		t.Fatal("desktop failure was retried indefinitely")
	}
	runs, err := s.List(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatal("notification started review work")
	}
}
func TestQuietMembershipUsesActualLocalTimeAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		utc  string
		want bool
	}{
		{"2026-11-01T05:30:00Z", true}, {"2026-11-01T06:30:00Z", true}, {"2026-11-01T07:00:00Z", false},
		{"2026-03-08T06:59:00Z", true}, {"2026-03-08T07:00:00Z", false},
	} {
		instant, _ := time.Parse(time.RFC3339, row.utc)
		if got := quietAt(instant, location, "01:00", "02:00"); got != row.want {
			t.Errorf("%s: quiet=%t want=%t", row.utc, got, row.want)
		}
	}
}

func TestReconnectBurstUsesDigestInsteadOfPerPullStorm(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items := []store.InboxItem{}
	for number := 1; number <= 12; number++ {
		items = append(items, work("reviews_owed", "missed-while-disconnected", number))
	}
	seed(t, s, items...)
	var messages []Message
	service := Service{Store: s, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	if err := service.Pump(ctx, document(t, "desktop=false\n"), true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Title != "Review digest" || !strings.Contains(messages[0].Body, "12 pull requests") {
		t.Fatalf("reconnect replayed a per-PR notification storm: %+v", messages)
	}
}

func TestRunLifecycleNotificationsKeepPreparedPublishedAndFailureDistinct(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var messages []Message
	service := Service{Store: s, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	doc := document(t, "desktop=false\n")
	for _, state := range []store.State{store.Prepared, store.Published, store.Failed, store.Stale, store.Partial, store.PublicationUnknown} {
		run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: string(state), State: store.Queued, Configuration: json.RawMessage(`{}`), Private: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err != nil {
			t.Fatal(err)
		}
		switch state {
		case store.Failed, store.Stale:
			if err := s.Fail(ctx, run.ID, state, "fixture"); err != nil {
				t.Fatal(err)
			}
		case store.Published, store.Partial, store.PublicationUnknown:
			if err := s.DecidePublication(ctx, run.ID, "payload"); err != nil {
				t.Fatal(err)
			}
			if _, won, err := s.BeginPublication(ctx, run.ID, "payload"); err != nil || !won {
				t.Fatal("fixture publication claim failed")
			}
			current, err := s.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.MarkOutcome(ctx, current, state, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
		}
		if err := service.Pump(ctx, doc, true); err != nil {
			t.Fatal(err)
		}
		if len(messages) == 0 || !strings.Contains(messages[len(messages)-1].Body, run.ID) {
			t.Fatalf("%s is invisible: %+v", state, messages)
		}
		body := messages[len(messages)-1].Body
		if !strings.Contains(body, "Historical workflow state "+string(state)) || strings.Contains(body, "publication verified") {
			t.Fatalf("historical state became current delivery: %s", body)
		}
		if state == store.Prepared {
			if strings.Contains(body, "publication verified") {
				t.Fatal("prepared is presented as published")
			}
			if _, won, _ := s.BeginPublication(ctx, run.ID, "payload"); won {
				t.Fatal("notification granted publication consent")
			}
		}
	}
}

func TestPersistedBaseRunNotificationDoesNotReplayAfterInputSupport(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "upgrade", State: store.Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim: %t %v", won, err)
	}
	if err := s.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	// Base-version durable receipts used two components. Seed through the real
	// persistence API, consume once, and reopen before the new service observes it.
	pending, err := s.ObserveNotifications(ctx, []store.Notification{{Source: "run:" + run.ID, Repository: run.Repository, ActorID: run.ActorID, PullID: run.PullID, Number: run.Number, Fingerprint: fingerprint(string(store.Prepared), "payload"), Body: "Review prepared", Pending: true}})
	if err != nil || len(pending) != 1 {
		t.Fatalf("observe base receipt: %+v %v", pending, err)
	}
	if taken, err := s.TakeNotifications(ctx, pending); err != nil || len(taken) != 1 {
		t.Fatalf("consume base receipt: %+v %v", taken, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	var messages []Message
	service := Service{Store: s, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	if err := service.Pump(ctx, document(t, "desktop=false\n"), true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("unchanged prepared run replayed %d notifications", len(messages))
	}
	if err := s.Fail(ctx, run.ID, store.Failed, "fixture failure"); err != nil {
		t.Fatal(err)
	}
	if err := service.Pump(ctx, document(t, "desktop=false\n"), true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "state failed") {
		t.Fatalf("real state transition was suppressed: %+v", messages)
	}
}

func TestPersistedOrdinaryReceiptsSurviveFeedbackSupport(t *testing.T) {
	for _, purpose := range []string{"", "review"} {
		for _, input := range []bool{false, true} {
			t.Run(purpose+"/"+map[bool]string{false: "prepared", true: "input"}[input], func(t *testing.T) {
				ctx := context.Background()
				durable, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
				if err != nil {
					t.Fatal(err)
				}
				defer durable.Close()
				run, _, err := durable.Create(ctx, store.Run{Purpose: purpose, Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "ordinary-receipt", State: store.Queued, Configuration: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				if won, err := durable.Claim(ctx, run.ID); err != nil || !won {
					t.Fatal("claim", err)
				}
				legacy := fingerprint(string(store.Prepared), "payload")
				request := clarification.Request{ID: "input-first", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "owned private question"}}}
				if input {
					if err := durable.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
						t.Fatal(err)
					}
					if err := durable.OpenInput(ctx, run.ID, request); err != nil {
						t.Fatal(err)
					}
					legacy = fingerprint(string(store.Reviewing), "", "input-first")
				} else if err := durable.Prepare(ctx, run.ID, "payload", json.RawMessage(`{}`), "thread", "turn"); err != nil {
					t.Fatal(err)
				}
				pending, err := durable.ObserveNotifications(ctx, []store.Notification{{Source: "run:" + run.ID, Repository: run.Repository, ActorID: run.ActorID, PullID: run.PullID, Number: run.Number, Fingerprint: legacy, Body: "previously delivered ordinary notification", Pending: true}})
				if err != nil || len(pending) != 1 {
					t.Fatal("seed previous receipt", err)
				}
				if taken, err := durable.TakeNotifications(ctx, pending); err != nil || len(taken) != 1 {
					t.Fatal("acknowledge previous receipt", err)
				}
				var messages []Message
				service := Service{Store: durable, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
				doc := document(t, "desktop=false\n")
				if err := service.Pump(ctx, doc, true); err != nil || len(messages) != 0 {
					t.Fatal("unchanged ordinary receipt replayed after feedback support", err, len(messages))
				}
				if input {
					request.ID = "input-second"
					if err := durable.OpenInput(ctx, run.ID, request); err != nil {
						t.Fatal(err)
					}
				} else if err := durable.Fail(ctx, run.ID, store.Failed, "owned transition"); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := service.Pump(ctx, doc, true); err != nil || len(messages) != 1 {
						t.Fatal("genuine ordinary transition was suppressed or replayed", err, len(messages))
					}
				}
			})
		}
	}
}

func TestClarificationNotificationsTrackPendingIdentityWithoutPrivateText(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, _, err := s.Create(ctx, store.Run{Repository: "github.com/owner/repo", RepositoryID: 99, PullID: 701, Number: 1, ActorID: 43, ActorLogin: "reviewer", WorkKey: "input", State: store.Queued, Configuration: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if won, err := s.Claim(ctx, run.ID); err != nil || !won {
		t.Fatalf("claim: %t %v", won, err)
	}
	if err := s.RecordProgress(ctx, run.ID, "thread", "turn", "model", "model", "reviewing", nil); err != nil {
		t.Fatal(err)
	}
	var messages []Message
	service := Service{Store: s, Terminal: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }}
	doc := document(t, "desktop=false\n")
	pump := func(want int) {
		t.Helper()
		if err := service.Pump(ctx, doc, true); err != nil {
			t.Fatal(err)
		}
		if len(messages) != want {
			t.Fatalf("notifications=%d want=%d: %+v", len(messages), want, messages)
		}
		for _, message := range messages {
			if strings.Contains(message.Body+message.Title, "private-question-canary") || strings.Contains(message.Body+message.Title, "private-answer-canary") {
				t.Fatal("private clarification text leaked to a notification")
			}
		}
	}
	pump(0)
	request := clarification.Request{ID: "input-first", ThreadID: "thread", TurnID: "turn", WireID: `"wire"`, ItemID: "item", Participant: "primary", Transport: "dynamic", Questions: []clarification.Question{{ID: "scope", Header: "Scope", Question: "private-question-canary"}}}
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	pump(1)
	if !strings.Contains(messages[0].Body, "reviewd show "+run.ID) {
		t.Fatal("clarification notification lacks an explicit inspection action")
	}
	pump(1)
	if _, err := s.SelectInput(ctx, request.ID, clarification.Answers{Answers: map[string]clarification.Answer{"scope": {Answers: []string{"private-answer-canary"}}}}); err != nil {
		t.Fatal(err)
	}
	pump(1)
	request.ID = "input-second"
	if err := s.OpenInput(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	pump(2)
	pump(2)
}

func TestAcceptedUnpaddedQuietHoursSuppressAtTheActualLocalClock(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	var messages []Message
	service := Service{Store: s, Now: func() time.Time { return now }, Terminal: func(_ context.Context, message Message) error { messages = append(messages, message); return nil }}
	doc := document(t, "desktop=false\nquiet_start=\"7:00\"\nquiet_end=\"9:00\"\ntimezone=\"UTC\"\n")
	frozen, err := config.Resolve(doc, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var restored config.Snapshot
	if err := json.Unmarshal(wire, &restored); err != nil || restored.Fingerprint != frozen.Fingerprint || restored.String("notifications.quiet_start") != "7:00" {
		t.Fatal("previously accepted frozen policy cannot be restored unchanged")
	}
	seed(t, s, work("reviews_owed", "accepted-unpadded-time", 1))
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("accepted 7:00-9:00 quiet config delivered %d notices at 08:00 UTC", len(messages))
	}
	now = now.Add(time.Hour)
	if err := service.Pump(ctx, doc, true); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Title != "Review digest" {
		t.Fatal("end-exclusive quiet digest did not release at 09:00")
	}
}
