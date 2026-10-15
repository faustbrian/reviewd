// Package watch owns foreground scheduling, not discovery or publication.
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/discovery"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

type activeWork struct {
	item   store.InboxItem
	cancel context.CancelFunc
}

type completion struct {
	item       store.InboxItem
	run        store.Run
	err        error
	observedAt time.Time
}

type Engine struct {
	Store         *store.Store
	Configuration *config.Manager
	Connect       func(context.Context, string) (*github.Client, error)
	Reviewer      func(*github.Client) review.Service
	// Feedback is deliberately separate from the read-only reviewer factory.
	// A missing executor holds typed work; it never falls back to reviewing.
	Feedback func(context.Context, *config.Document, *github.Client, store.InboxItem) (store.Run, error)
	Announce func(context.Context, *github.Client, *config.Document) error
	Now      func() time.Time
	active   map[string]activeWork
	done     chan completion
	closing  bool
	client   *github.Client
	admitted map[string]bool
}

func (e *Engine) initialize() {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.active == nil {
		e.active = map[string]activeWork{}
	}
	if e.done == nil {
		e.done = make(chan completion, 16)
	}
}

// Tick observes first, invalidates superseded work, and admits only durable
// queued decisions. It does not block discovery on a model turn.
func (e *Engine) Tick(ctx context.Context) (resultErr error) {
	e.initialize()
	e.admitted = map[string]bool{}
	document := e.Configuration.Document()
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		return err
	}
	if err := e.revokeExcludedReviews(ctx, document); err != nil {
		return err
	}
	host := defaults.String("github.host")
	if held, err := e.cooling(ctx, host); held || err != nil {
		return err
	}
	defer func() { resultErr = e.rememberCooldown(ctx, host, resultErr, e.Now()) }()
	var client *github.Client
	connect := func(ctx context.Context, host string) (*github.Client, error) {
		var err error
		client, err = e.Connect(ctx, host)
		return client, err
	}
	acquisition := (discovery.Service{Store: e.Store, Connect: connect}).Sync(ctx, document)
	// Discovery may have acquired a server constraint. Do not announce an
	// identity or admit work through another endpoint in this same tick.
	if github.RetryAfter(acquisition) > 0 {
		return acquisition
	}
	items, scopes, err := e.Store.Inbox(ctx)
	if err != nil {
		return errors.Join(acquisition, err)
	}
	current := map[string]store.InboxItem{}
	eligible := map[string]store.DiscoveryScope{}
	for _, scope := range scopes {
		if scope.State == "ok" {
			eligible[scope.Repository] = scope
		}
	}
	for _, item := range items {
		current[item.ID] = item
	}
	for id, work := range e.active {
		item, exists := current[id]
		scope, allowed := eligible[work.item.Repository]
		changedFeedbackTarget := work.item.ExecutionKind == "feedback_v1" && (item.HeadSHA != work.item.HeadSHA || item.TargetBaseSHA != work.item.TargetBaseSHA || item.ContextKey != work.item.ContextKey)
		if !exists || !allowed || scope.ActorID != work.item.ActorID || item.EventKey != work.item.EventKey || changedFeedbackTarget || item.State == "ineligible" || item.State == "closed" {
			work.cancel()
		}
	}
	if client == nil {
		return acquisition
	}
	if held, err := e.cooling(ctx, host); held || err != nil {
		return errors.Join(acquisition, err)
	}
	if e.Announce != nil {
		if err := e.Announce(ctx, client, document); err != nil {
			return errors.Join(acquisition, err)
		}
	}
	e.client = client
	return errors.Join(acquisition, e.Admit(ctx))
}

// Operator revocation is local authority and must not wait for GitHub: both a
// durable cooldown and a newly throttled acquisition can return before the
// ordinary event reconciliation below. Cached observations supply author facts
// even for jobs started before the latest configuration was loaded.
func (e *Engine) revokeExcludedReviews(ctx context.Context, document *config.Document) error {
	for _, work := range e.active {
		// Legacy feedback-triggered jobs are private reviewers. Versioned
		// modifying work instead follows its independent author/action policy.
		policy, err := config.Resolve(document, work.item.Repository, nil, nil)
		if err != nil {
			work.cancel()
			return err
		}
		feedback := work.item.Category == "feedback_handling" && work.item.ExecutionKind == "feedback_v1"
		if !feedback && len(policy.Strings("reviewer.exclude_authors")) == 0 {
			continue
		}
		observations, err := e.Store.Observations(ctx, work.item.Repository, work.item.ActorID)
		if err != nil {
			work.cancel()
			return err
		}
		var record struct {
			Pull github.Pull `json:"pull"`
		}
		if json.Unmarshal(observations[work.item.PullID].Record, &record) != nil || record.Pull.Author.ID <= 0 || record.Pull.Author.Login == "" {
			// Without the captured author there is no basis to retain discovered
			// execution authority under a policy that excludes authors.
			work.cancel()
			continue
		}
		if feedback {
			if !policy.FeedbackAuthorAllowed(record.Pull.Author.ID, record.Pull.Author.Login, work.item.ActorID) || (!work.item.Manual && policy.FeedbackAction(work.item.Trigger) != "auto") {
				work.cancel()
			}
		} else if policy.ReviewAuthorExcluded(record.Pull.Author.ID, record.Pull.Author.Login, work.item.ActorID) {
			work.cancel()
		}
	}
	return nil
}

// Admit refills capacity from the current durable snapshot without polling
// GitHub again or reprocessing an event already handled in this poll cycle.
func (e *Engine) Admit(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	document := e.Configuration.Document()
	if err := e.revokeExcludedReviews(ctx, document); err != nil {
		return err
	}
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		return err
	}
	if held, err := e.cooling(ctx, defaults.String("github.host")); held || err != nil {
		return err
	}
	items, scopes, err := e.Store.Inbox(ctx)
	if err != nil {
		return err
	}
	controls, err := e.Store.Controls(ctx, e.Now())
	if err != nil {
		return err
	}
	eligible := map[string]store.DiscoveryScope{}
	for _, scope := range scopes {
		if scope.State == "ok" {
			eligible[scope.Repository] = scope
		}
	}
	priority := func(item store.InboxItem) int {
		if item.Trigger == "review_requested" {
			return 0
		}
		if item.Trigger == "new_pull_request" {
			return 2
		}
		return 1
	}
	sort.SliceStable(items, func(i, j int) bool {
		// Prefer requests without indefinitely starving older eligible work.
		// Virtual arrival times use durable ages, not an in-memory quota that
		// would reset after each poll or watcher restart.
		a := items[i].UpdatedAt.Add(time.Duration(priority(items[i])) * 5 * time.Minute)
		b := items[j].UpdatedAt.Add(time.Duration(priority(items[j])) * 5 * time.Minute)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	limit := int(defaults.Values()["limits.max_concurrent_reviews"].(int64))
	var heldExecution error
	for _, item := range items {
		if len(e.active) >= limit {
			break
		}
		scope, allowed := eligible[item.Repository]
		if controls.Holds(item) || !allowed || scope.ActorID != item.ActorID || (item.State != "queued" && item.State != "waiting_checks" && item.State != "retry_wait" && item.State != "prepared") || item.Baseline {
			continue
		}
		if e.Now().Before(item.RetryAt) {
			continue
		}
		if _, exists := e.active[item.ID]; exists {
			continue
		}
		workKey := review.ItemWorkKey(item)
		if e.admitted[workKey] {
			continue
		}
		busy := false
		for _, work := range e.active {
			if work.item.Repository == item.Repository && work.item.PullID == item.PullID && work.item.ActorID == item.ActorID {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		policy, err := config.Resolve(document, item.Repository, nil, nil)
		if err != nil {
			return err
		}
		feedback := item.Category == "feedback_handling" && item.ExecutionKind == "feedback_v1"
		if item.ExecutionKind != "" || item.Category == "feedback_handling" {
			if !feedback {
				heldExecution = errors.Join(heldExecution, errors.New("unknown inbox execution kind; work is held without reviewer fallback"))
				continue
			}
			if e.Feedback == nil {
				if err := e.Store.SetFeedbackExecutionHold(ctx, item, "Feedback automation is unavailable; this item is held without starting a private review. Other review monitoring continues."); err != nil {
					return err
				}
				continue
			}
			if !item.Manual && policy.FeedbackAction(item.Trigger) != "auto" {
				continue
			}
			observations, err := e.Store.Observations(ctx, item.Repository, item.ActorID)
			if err != nil {
				return err
			}
			var captured struct {
				Pull github.Pull `json:"pull"`
			}
			if json.Unmarshal(observations[item.PullID].Record, &captured) != nil || captured.Pull.Author.ID <= 0 || captured.Pull.Author.Login == "" || !policy.FeedbackAuthorAllowed(captured.Pull.Author.ID, captured.Pull.Author.Login, item.ActorID) {
				continue
			}
			if err := e.Store.SetFeedbackExecutionHold(ctx, item, ""); err != nil {
				return err
			}
		}
		if (item.Trigger == "head_updated" || item.Trigger == "context_changed") && e.Now().Before(item.UpdatedAt.Add(policy.Duration("watch.debounce"))) {
			continue
		}
		if held, err := e.cooling(ctx, defaults.String("github.host")); held || err != nil {
			return err
		}
		workerCtx, cancel := context.WithCancel(ctx)
		e.admitted[workKey] = true
		e.active[item.ID] = activeWork{item: item, cancel: cancel}
		go func(item store.InboxItem, document *config.Document, client *github.Client) {
			var run store.Run
			var err error
			if feedback {
				run, err = e.Feedback(workerCtx, document, client, item)
			} else {
				service := e.Reviewer(client)
				if item.State == "prepared" {
					run, err = service.RefreshChecks(workerCtx, item.RunID)
				} else {
					run, err = service.StartItem(workerCtx, document, item, item.Overrides, item.Manual, item.ReviewNow)
				}
			}
			if run.ID != "" {
				if current, readErr := e.Store.Get(context.Background(), run.ID); readErr == nil {
					run = current
				} else {
					err = errors.Join(err, readErr)
				}
			}
			observed := e.Now()
			host, _, _ := strings.Cut(item.Repository, "/")
			err = e.rememberCooldown(workerCtx, host, err, observed)
			e.done <- completion{item: item, run: run, err: err, observedAt: observed}
		}(item, document, e.client)
	}
	return heldExecution
}

// Complete projects factual run state and uses the existing publication owner
// only when current operator policy authorizes automatic publication.
func (e *Engine) Complete(ctx context.Context, block bool) (resultErr error) {
	e.initialize()
	var result completion
	if block {
		select {
		case result = <-e.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		select {
		case result = <-e.done:
		default:
			return nil
		}
	}
	if work, exists := e.active[result.item.ID]; exists {
		work.cancel()
		delete(e.active, result.item.ID)
	}
	defer func() {
		host, _, _ := strings.Cut(result.item.Repository, "/")
		observed := e.Now()
		if github.RetryAfter(result.err) > 0 {
			observed = result.observedAt
		}
		resultErr = e.rememberCooldown(ctx, host, resultErr, observed)
		if e.closing {
			return
		}
		if resultErr == nil && result.run.ID != "" {
			resultErr = e.Store.ClearItemFailure(ctx, result.item, result.run.ID)
		} else if resultErr != nil && (result.run.ID == "" || result.run.State == store.WaitingChecks || result.run.State == store.Prepared) {
			item := result.item
			item.RunID = result.run.ID
			resultErr = errors.Join(resultErr, e.Store.ItemFailure(ctx, item, resultErr.Error(), e.Now(), github.RetryAfter(resultErr)))
		}
	}()
	if result.run.ID != "" {
		// A consumed completion is no longer available to Close. Persist its
		// factual local projection even if foreground shutdown has begun; this
		// bounded context grants no further model or remote work.
		projection, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		result.err = errors.Join(result.err, e.Store.FinishItem(projection, result.item, result.run))
		cancel()
	}
	if errors.Is(result.err, store.ErrHeld) {
		return nil
	}
	if result.err != nil || result.run.State != store.Prepared || e.closing {
		return result.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	document := e.Configuration.Document()
	policy, err := config.Resolve(document, result.run.Repository, nil, nil)
	if err != nil {
		return err
	}
	if policy.String("actions.publish_review") != "auto" || result.run.Private {
		return nil
	}
	items, scopes, err := e.Store.Inbox(ctx)
	if err != nil {
		return err
	}
	allowed := false
	for _, scope := range scopes {
		if scope.Repository == result.run.Repository && scope.ActorID == result.run.ActorID && scope.State == "ok" {
			allowed = true
		}
	}
	current := false
	for _, item := range items {
		if item.ID == result.item.ID && item.EventKey == result.item.EventKey {
			current = true
		}
	}
	if !allowed || !current {
		return errors.New("prepared event is no longer eligible for automatic publication")
	}
	controls, err := e.Store.Controls(ctx, e.Now())
	if err != nil {
		return err
	}
	if controls.Holds(result.item) {
		return nil
	}
	if held, err := e.cooling(ctx, policy.String("github.host")); held || err != nil {
		return err
	}
	client, err := e.Connect(ctx, policy.String("github.host"))
	if err != nil {
		return err
	}
	if e.Announce == nil {
		return errors.New("automatic publication requires a visible posting identity and policy announcement")
	}
	if err := e.Announce(ctx, client, document); err != nil {
		return err
	}
	service := e.Reviewer(client)
	run, err := (review.Publisher{GitHub: client, Store: e.Store, Source: service.Source}).PublishItem(ctx, document, result.run.ID, result.run.PayloadID, result.item)
	if run.ID != "" {
		result.run = run
		err = errors.Join(err, e.Store.FinishItem(ctx, result.item, run))
	}
	if errors.Is(err, store.ErrHeld) {
		return nil
	}
	return err
}

func (e *Engine) cooling(ctx context.Context, host string) (bool, error) {
	deadline, err := e.Store.GitHubRetryAt(ctx, host)
	return e.Now().Before(deadline), err
}

func (e *Engine) rememberCooldown(ctx context.Context, host string, err error, observed time.Time) error {
	if delay := github.RetryAfter(err); delay > 0 {
		// A canceled foreground context must not erase already received server
		// guidance before a subsequent watcher opens the same profile.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, e.Store.DeferGitHub(saveCtx, host, observed.Add(delay)))
	}
	return err
}

func (e *Engine) Active() int { return len(e.active) }

func (e *Engine) Close() error {
	e.initialize()
	e.closing = true
	for _, work := range e.active {
		work.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	for len(e.active) > 0 {
		err = errors.Join(err, e.Complete(ctx, true))
		if ctx.Err() != nil {
			break
		}
	}
	return err
}
