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
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/faustbrian/reviewd/internal/github"
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
	Execute func(context.Context, *config.Document, *github.Client, store.InboxItem) (store.Run, error)

	Store         *store.Store
	Configuration *config.Manager
	Connect       func(context.Context, string) (*github.Client, error)
	Now           func() time.Time
	active        map[string]activeWork
	done          chan completion
	closing       bool
	client        *github.Client
	admitted      map[string]bool
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

// Tick observes first, reconciles eligibility, and admits only durable
// queued decisions. It does not block discovery on a model turn.
func (e *Engine) Tick(ctx context.Context) (resultErr error) {
	e.initialize()
	e.admitted = map[string]bool{}
	document := e.Configuration.Document()
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
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
		if err == nil && client != nil {
			client.ReuseDiscoveryCache(e.client)
		}
		return client, err
	}
	// Discovery controls admission, not accepted skills. The user's selected
	// lifetime contract (docs/SPECIFICATION.md, Event and run behavior) leaves
	// active runs to explicit cancellation, timeout or watcher shutdown.
	acquisition := (discovery.Service{Store: e.Store, Connect: connect}).Sync(ctx, document)
	// Discovery may have acquired a server constraint. Do not announce an
	// identity or admit work through another endpoint in this same tick.
	if github.RetryAfter(acquisition) > 0 {
		return acquisition
	}
	if client == nil {
		return acquisition
	}
	if held, err := e.cooling(ctx, host); held || err != nil {
		return errors.Join(acquisition, err)
	}
	e.client = client
	return errors.Join(acquisition, e.Admit(ctx))
}

func (e *Engine) Admit(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	// Operator settings freeze for this pickup pass, including GitHub
	// preparation (docs/SPECIFICATION.md). Reloads affect later pickups;
	// PR-busy discovered work returns queued instead of retaining a waiter.
	document := e.Configuration.Document()
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
		if controls.Holds(item) || !allowed || scope.ActorID != item.ActorID || (item.State != "queued" && item.State != "retry_wait") || item.RunID != "" || item.Category == "own_pr_feedback" || item.Baseline {
			continue
		}
		if e.Now().Before(item.RetryAt) {
			continue
		}
		if _, exists := e.active[item.ID]; exists {
			continue
		}
		workKey := dispatch.ItemWorkKey(item)
		if e.admitted[workKey] {
			continue
		}
		busy := false
		for _, work := range e.active {
			if work.item.Repository == item.Repository && work.item.PullID == item.PullID {
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
		if e.Execute == nil {
			return errors.New("agent launcher is unavailable")
		}
		workerCtx, cancel := context.WithCancel(ctx)
		e.admitted[workKey] = true
		e.active[item.ID] = activeWork{item: item, cancel: cancel}
		go func(item store.InboxItem, document *config.Document, client *github.Client) {
			run, err := e.Execute(workerCtx, document, client, item)
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

// Complete releases capacity and projects only the factual native process outcome.
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
		} else if resultErr != nil && result.run.ID == "" {
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
	if errors.Is(result.err, store.ErrHeld) || (result.run.ID == "" && errors.Is(result.err, dispatch.ErrExecutionLeaseBusy)) {
		return nil
	}
	return result.err
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
