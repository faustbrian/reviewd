// Package discovery turns complete repository-scoped reads into durable inbox work.
// It has no reviewer, publisher, fixer or notification capability.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/faustbrian/reviewd/internal/config"
	feedbacklane "github.com/faustbrian/reviewd/internal/feedback"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
)

type Service struct {
	GitHub  *github.Client
	Connect func(context.Context, string) (*github.Client, error)
	Store   *store.Store
}
type captured struct {
	FeedbackSchema   int                   `json:"feedback_schema,omitempty"`
	FeedbackEligible bool                  `json:"feedback_eligible,omitempty"`
	FeedbackDigest   string                `json:"feedback_digest,omitempty"`
	FeedbackReviews  string                `json:"feedback_reviews,omitempty"`
	Following        bool                  `json:"following"`
	Requested        bool                  `json:"requested"`
	Pull             github.DiscoveredPull `json:"pull"`
	Owed             bool                  `json:"owed"`
	Own              bool                  `json:"own"`
	Eligible         bool                  `json:"eligible"`
	Requests         []string              `json:"requests"`
	RelevantRequests []string              `json:"relevant_requests"`
	Context          github.ReviewContext  `json:"context"`
	Reviews          string                `json:"reviews"`
	Comments         string                `json:"comments"`
}

func key(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Sync imports one complete snapshot per configured repository. Initial auto
// work is held for an explicit backlog decision; discovery itself never runs it.
func (s Service) Sync(ctx context.Context, document *config.Document) error {
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		return err
	}
	repositories := defaults.Strings("github.repositories")
	scopes, err := s.Store.ConfigureDiscovery(ctx, repositories)
	if err != nil {
		return err
	}
	if len(repositories) == 0 {
		return errors.New("no repositories configured; set github.repositories before synchronizing")
	}
	if s.Connect != nil {
		s.GitHub, err = s.Connect(ctx, defaults.String("github.host"))
		if err != nil {
			original := err
			for _, repository := range repositories {
				err = errors.Join(err, s.Store.DiscoveryFailure(ctx, scopes[repository], original.Error()))
			}
			return err
		}
	}
	if s.GitHub == nil {
		return errors.New("GitHub discovery client is unavailable")
	}
	s.GitHub = s.GitHub.DiscoveryReader()
	actor, err := s.GitHub.Identity(ctx)
	if err != nil {
		original := err
		for _, repository := range repositories {
			err = errors.Join(err, s.Store.DiscoveryFailure(ctx, scopes[repository], original.Error()))
		}
		return err
	}
	var failures error
	for _, repository := range repositories {
		err := s.syncRepository(ctx, document, scopes[repository], actor)
		if err != nil {
			saveErr := s.Store.DiscoveryFailure(ctx, scopes[repository], err.Error())
			failures = errors.Join(failures, fmt.Errorf("%s: %w", repository, err), saveErr)
			if github.RetryAfter(err) > 0 {
				break // All repositories share this authenticated host's constraint.
			}
		}
	}
	return failures
}
func (s Service) syncRepository(ctx context.Context, document *config.Document, scope store.DiscoveryScope, actor github.Identity) error {
	policy, err := config.Resolve(document, scope.Repository, nil, nil)
	if err != nil {
		return err
	}
	storedItems, err := s.Store.DiscoveryItems(ctx, scope.Repository, actor.ID)
	if err != nil {
		return err
	}
	type itemIdentity struct {
		pullID   int64
		category string
	}
	previousItems := map[itemIdentity]store.InboxItem{}
	for _, item := range storedItems {
		previousItems[itemIdentity{item.PullID, item.Category}] = item
	}
	repositoryID, pulls, err := s.GitHub.OpenPulls(ctx, scope.Repository)
	if err != nil {
		return err
	}
	if scope.RepositoryID > 0 && scope.RepositoryID != repositoryID {
		return errors.New("repository was replaced; existing work cannot be reassigned")
	}
	previous, err := s.Store.Observations(ctx, scope.Repository, actor.ID)
	if err != nil {
		return err
	}

	tracking, err := s.Store.FollowStates(ctx, scope.Repository, actor.ID)
	if err != nil {
		return err
	}
	runs, err := s.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Repository != scope.Repository || run.RepositoryID != repositoryID || run.ActorID != actor.ID {
			continue
		}
		if run.Archive == nil || !run.Archive.Reviewed {
			var prepared review.PreparedResult
			if json.Unmarshal(run.Prepared, &prepared) != nil || review.ValidatePrepared(prepared) != nil {
				continue
			}
		}
		if _, explicit := tracking[run.PullID]; !explicit {
			tracking[run.PullID] = store.FollowState{PullID: run.PullID, Number: run.Number, Following: true}
		}
	}
	listed := map[int64]bool{}
	for _, pull := range pulls {
		listed[pull.ID] = true
	}
	missing := map[int64]int{}
	// Every actionable category needs verified closure reconciliation, not only
	// requests or followed PRs. A missing open-list entry is not itself closure.
	existingItems, _, err := s.Store.Inbox(ctx)
	if err != nil {
		return err
	}
	for _, item := range existingItems {
		if item.Repository == scope.Repository && item.ActorID == actor.ID && !listed[item.PullID] {
			missing[item.PullID] = item.Number
		}
	}
	for id, ref := range tracking {
		if ref.Following && !listed[id] {
			missing[id] = ref.Number
		}
	}
	for id, row := range previous {
		var old captured
		if json.Unmarshal(row.Record, &old) != nil {
			return errors.New("stored observation is invalid")
		}
		if old.Pull.State == "open" && (old.Owed || old.Own || old.Following || old.FeedbackEligible) && !listed[id] {
			missing[id] = row.Number
		}
		if old.Pull.State == "closed" {
			delete(missing, id)
		}
	}
	for id, number := range missing {
		pull, err := s.GitHub.Pull(ctx, scope.Repository, number)
		if err != nil {
			return err
		}
		if pull.ID != id || pull.RepositoryID != repositoryID {
			return errors.New("tracked pull-request identity changed")
		}
		if pull.State == "open" {
			return errors.New("discovery snapshot is incomplete: a pull absent from the open list is still open; retry synchronization")
		}
		pulls = append(pulls, github.DiscoveredPull{Pull: pull, RequestedUsers: []github.Identity{}, RequestedTeams: []github.RequestedTeam{}})
	}
	baseline := !scope.Initialized || scope.ActorID != actor.ID
	teams := map[int64]bool{}
	checked := map[string]bool{}
	if policy.Bool("github.watch_review_requests") {
		for _, pull := range pulls {
			if pull.Draft && !policy.Bool("watch.include_drafts") {
				continue
			}
			for _, team := range pull.RequestedTeams {
				if checked[team.NodeID] {
					continue
				}
				member, err := s.GitHub.TeamMember(ctx, team, actor)
				if err != nil {
					return err
				}
				checked[team.NodeID] = true
				teams[team.ID] = member
			}
		}
	}
	rows := []store.Observation{}
	items := []store.InboxItem{}
	for _, pull := range pulls {
		current := captured{Pull: pull, Eligible: pull.State == "open" && (!pull.Draft || policy.Bool("watch.include_drafts")), Requests: []string{}}
		if policy.Bool("github.watch_review_requests") {
			for _, user := range pull.RequestedUsers {
				if user.ID == actor.ID {
					current.Owed = true
				}
			}
			for _, team := range pull.RequestedTeams {
				if teams[team.ID] {
					current.Owed = true
				}
			}
		}
		current.Requested = current.Owed
		followState, tracked := tracking[pull.ID]
		current.Following = followState.Following
		current.Owed = current.Owed || current.Following
		separateFeedback := policy.FeedbackMonitoringConfigured()
		current.Own = !separateFeedback && policy.Bool("github.watch_own_prs") && pull.Author.ID == actor.ID
		if separateFeedback {
			current.FeedbackSchema = 1
			current.FeedbackEligible = current.Eligible && policy.FeedbackAuthorAllowed(pull.Author.ID, pull.Author.Login, actor.ID)
		}
		excludedReview := policy.ReviewAuthorExcluded(pull.Author.ID, pull.Author.Login, actor.ID)
		if excludedReview {
			current.Owed = false
		}
		relevant := current.Eligible && (current.Owed || current.Own || current.FeedbackEligible || policy.String("actions.new_pull_request") != "ignore")
		var old captured
		if previous[pull.ID].Record != nil && json.Unmarshal(previous[pull.ID].Record, &old) != nil {
			return errors.New("stored discovery observation is invalid")
		}
		current.Requests = append([]string{}, old.Requests...)
		current.RelevantRequests = append([]string{}, old.RelevantRequests...)
		if pull.State == "open" {
			events, err := s.GitHub.RequestEvents(ctx, pull.Pull)
			if err != nil {
				return err
			}
			// Keep all observed IDs, but advance review-event identity only when a
			// newly observed request is applicable now. Membership changes cannot
			// manufacture a new event from already observed historical evidence.
			known := map[string]bool{}
			for _, id := range old.Requests {
				known[id] = true
			}
			applicableTeams := map[int64]bool{}
			for _, team := range pull.RequestedTeams {
				applicableTeams[team.ID] = teams[team.ID]
			}
			for _, event := range events {
				if known[event.ID] {
					continue
				}
				current.Requests = append(current.Requests, event.ID)
				known[event.ID] = true
				if policy.Bool("github.watch_review_requests") && ((event.ReviewerKind == "User" && event.ReviewerID == actor.ID) || (event.ReviewerKind == "Team" && applicableTeams[event.ReviewerID])) {
					current.RelevantRequests = append(current.RelevantRequests, event.ID)
				}
			}
			sort.Strings(current.Requests)
			sort.Strings(current.RelevantRequests)

		}
		var inventory feedbacklane.Inventory
		if relevant {
			current.Context, err = s.GitHub.Context(ctx, pull.Pull)
			if err != nil {
				return err
			}
			if len(current.Context.Gaps) > 0 {
				return errors.New("discovery context is incomplete: " + current.Context.Gaps[0])
			}
			if current.FeedbackEligible {
				inventory, err = feedbacklane.Collect(current.Context, actor)
				if err != nil {
					return err
				}
				current.FeedbackDigest = inventory.Digest
				revisions := []string{}
				for _, item := range inventory.Items {
					if item.Kind == "review" {
						revisions = append(revisions, item.Revision)
					}
				}
				current.FeedbackReviews = key(revisions)
			}

			// Exclude only application-recorded writes, not all activity by this user.
			current.Context, err = review.EventContext(current.Context, pull.Pull, actor, runs)
			if err != nil {
				return err
			}
			if tracked && !followState.Following && !newRequest(current.RelevantRequests, followState.IgnoredRequests) {
				current.Owed = false
			}
			current.Reviews, current.Comments, err = feedback(current.Context, actor)
			if err != nil {
				return err
			}
		}
		headChanged := old.Pull.ID != 0 && old.Pull.HeadSHA != pull.HeadSHA
		contextChanged := old.Pull.ID != 0 && (old.Pull.TargetBaseSHA != pull.TargetBaseSHA || old.Pull.Title != pull.Title || old.Pull.Body != pull.Body || (old.Context.Digest != "" && current.Context.Digest != "" && old.Context.DiscoveryDigest() != current.Context.DiscoveryDigest()))
		raw, _ := json.Marshal(current)
		rows = append(rows, store.Observation{PullID: pull.ID, Number: pull.Number, HeadSHA: pull.HeadSHA, Record: raw, MaterialChange: headChanged || contextChanged})
		// Existing private-review consent keeps its original category and scope;
		// it can never be transferred into the new modifying lane on upgrade.
		retainedLegacy := false
		if legacy, exists := previousItems[itemIdentity{pull.ID, "own_pr_feedback"}]; separateFeedback && exists && current.Eligible && (legacy.Manual || legacy.RunID != "" || legacy.State == "skipped") {
			items = append(items, legacy)
			retainedLegacy = true
		}

		if !relevant {
			if old.Owed || old.Following || old.Own {
				state := "ineligible"
				if pull.State == "closed" {
					state = "closed"
				}
				for _, category := range []string{"reviews_owed", "own_pr_feedback"} {
					if category == "own_pr_feedback" && retainedLegacy {
						continue
					}
					items = append(items, store.InboxItem{Repository: scope.Repository, PullID: pull.ID, Number: pull.Number, ActorID: actor.ID, Category: category, State: state, Trigger: state, EventKey: key([]any{state, pull.HeadSHA, pull.Draft}), HeadSHA: pull.HeadSHA, TargetBaseSHA: pull.TargetBaseSHA, Title: pull.Title, URL: pull.URL})
				}
			}
			continue
		}
		newExplicit := newRequest(current.RelevantRequests, old.RelevantRequests)
		// Becoming eligible is not a new event. Existing first-action work stays
		// held unless a real request, code or acquired controlling context changed.
		eventBaseline := baseline || (old.Pull.ID != 0 && !newExplicit && !headChanged && !contextChanged)
		workEvent := key([]any{pull.HeadSHA, pull.TargetBaseSHA, pull.Title, pull.Body, current.RelevantRequests, current.Context.DiscoveryDigest()})
		updateAction := func(trigger, action string) (string, string) {
			if newExplicit {
				return "review_requested", policy.String("actions.review_requested")
			}
			if headChanged {
				return "head_updated", policy.String("actions.head_updated")
			}
			if contextChanged {
				return "context_changed", policy.String("actions.head_updated")
			}
			return trigger, action
		}
		makeItem := func(category, trigger, action, event string) store.InboxItem {
			if previous, exists := previousItems[itemIdentity{pull.ID, category}]; exists && previous.EventKey == event && (previous.Trigger != "review_requested" || policy.Bool("github.watch_review_requests")) {
				policyTrigger := previous.Trigger
				if policyTrigger == "context_changed" {
					policyTrigger = "head_updated"
				}
				switch policyTrigger {
				case "new_pull_request", "review_requested", "head_updated", "review_submitted", "review_comment_added":
					trigger, action = previous.Trigger, policy.String("actions."+policyTrigger)
				}
			}
			itemBaseline := eventBaseline
			if previous, exists := previousItems[itemIdentity{pull.ID, category}]; exists && previous.EventKey == event && (previous.RunID != "" || previous.Manual || (previous.Action == "auto" && action == "auto")) {
				itemBaseline = previous.Baseline
			}
			state := "pending_decision"
			if action == "ignore" {
				state = "ignored"
			} else if action == "notify" {
				state = "attention"
			} else if action == "auto" && !itemBaseline {
				state = "queued"
			}
			explanation := "Review requested from you or one of your teams."
			switch trigger {
			case "new_pull_request":
				explanation = "Optional discovery is enabled for this repository; no review request is implied."
			case "head_updated":
				explanation = "Code changed on this monitored pull request."
			case "context_changed":
				explanation = "Review requirements or controlling context changed."
			}
			if category == "own_pr_feedback" {
				explanation = "Human feedback on your pull request; no fixing or thread-resolution authority is granted."
			}
			if current.Following && !current.Requested && trigger == "review_requested" {
				explanation = "Following this PR for code and controlling-context changes."
			}
			if itemBaseline && action == "auto" {
				explanation += " Existing backlog is held until an explicit start decision."
			}
			return store.InboxItem{Repository: scope.Repository, PullID: pull.ID, Number: pull.Number, ActorID: actor.ID, Category: category, Trigger: trigger, Action: action, State: state, Baseline: itemBaseline, EventKey: event, HeadSHA: pull.HeadSHA, TargetBaseSHA: pull.TargetBaseSHA, ContextKey: current.Context.DiscoveryDigest(), Title: pull.Title, URL: pull.URL, Explanation: explanation}
		}

		if current.Owed {
			trigger, action := updateAction("review_requested", policy.String("actions.review_requested"))
			item := makeItem("reviews_owed", trigger, action, workEvent)
			if current.Following && !current.Requested && item.Trigger == "review_requested" && (old.Pull.ID == 0 || (old.Pull.HeadSHA == pull.HeadSHA && old.Pull.TargetBaseSHA == pull.TargetBaseSHA && old.Pull.Title == pull.Title && old.Pull.Body == pull.Body && (!old.Following || (old.Context.DiscoveryDigest() == current.Context.DiscoveryDigest() && !newExplicit)))) {
				item.State = "following"
			}
			items = append(items, item)
		} else if !excludedReview && !current.Own && (!tracked || followState.Following) && policy.String("actions.new_pull_request") != "ignore" {
			trigger, action := updateAction("new_pull_request", policy.String("actions.new_pull_request"))
			items = append(items, makeItem("reviews_owed", trigger, action, workEvent))
		}
		if current.Own && (current.Reviews != "" || current.Comments != "") {
			trigger, action := "review_submitted", policy.String("actions.review_submitted")
			if current.Comments != "" && (current.Reviews == "" || (old.Pull.ID != 0 && old.Reviews == current.Reviews)) {
				trigger, action = "review_comment_added", policy.String("actions.review_comment_added")
			}
			items = append(items, makeItem("own_pr_feedback", trigger, action, key([]any{current.Reviews, current.Comments})))
		}
		if current.FeedbackEligible && len(inventory.Items) > 0 {
			trigger := "review_submitted"
			if old.FeedbackReviews == current.FeedbackReviews || current.FeedbackReviews == key([]string{}) {
				trigger = "review_comment_added"
			}
			event := key([]string{"feedback_v1", inventory.Digest})
			if prior, exists := previousItems[itemIdentity{pull.ID, "feedback_handling"}]; exists && prior.EventKey == event {
				trigger = prior.Trigger
			}
			action := policy.FeedbackAction(trigger)
			held := baseline || scope.FeedbackSchema != 1 || (old.Pull.ID != 0 && (old.FeedbackSchema != 1 || !old.FeedbackEligible)) || old.FeedbackDigest == current.FeedbackDigest
			if prior, exists := previousItems[itemIdentity{pull.ID, "feedback_handling"}]; exists && prior.EventKey == event {
				changedTarget := prior.HeadSHA != pull.HeadSHA || prior.TargetBaseSHA != pull.TargetBaseSHA || prior.ContextKey != inventory.Digest
				if prior.RunID != "" || (prior.Manual && !changedTarget) || (!prior.Manual && prior.Action == "auto" && action == "auto") {
					held = prior.Baseline
				}
			}
			state := "pending_decision"
			switch action {
			case "ignore":
				state = "ignored"
			case "notify":
				state = "attention"
			case "auto":
				if !held {
					state = "queued"
				}
			}
			explanation := "Human feedback selected for the separate feedback-handling workflow; private-review start is not fixing consent."
			if held && action == "auto" {
				explanation += " Existing backlog is held until an explicit feedback-handling decision."
			}
			items = append(items, store.InboxItem{Repository: scope.Repository, PullID: pull.ID, Number: pull.Number, ActorID: actor.ID, Category: "feedback_handling", ExecutionKind: "feedback_v1", Trigger: trigger, Action: action, State: state, Baseline: held, EventKey: event, HeadSHA: pull.HeadSHA, TargetBaseSHA: pull.TargetBaseSHA, ContextKey: inventory.Digest, Title: pull.Title, URL: pull.URL, Explanation: explanation})
		}
	}
	next := scope
	next.ActorID = actor.ID
	next.RepositoryID = repositoryID
	next.State = "ok"
	next.Failure = ""
	next.FeedbackSchema = 0
	if policy.FeedbackMonitoringConfigured() {
		next.FeedbackSchema = 1
	}
	return s.Store.ApplyDiscovery(ctx, scope, next, rows, items)
}
func feedback(remote github.ReviewContext, actor github.Identity) (string, string, error) {
	reviews := []json.RawMessage{}
	comments := []json.RawMessage{}
	for _, input := range []struct {
		rows   []json.RawMessage
		target *[]json.RawMessage
	}{{remote.Reviews, &reviews}, {remote.Comments, &comments}} {
		for _, raw := range input.rows {
			var row struct {
				ID    int64            `json:"id"`
				User  *github.Identity `json:"user"`
				State string           `json:"state"`
			}
			if json.Unmarshal(raw, &row) != nil || row.ID <= 0 {
				return "", "", errors.New("feedback identity is incomplete")
			}
			if row.State == "PENDING" || (row.User != nil && row.User.ID == actor.ID) {
				continue
			}
			*input.target = append(*input.target, raw)
		}
	}
	for _, thread := range remote.Threads {
		external := false
		if thread.Comments.Nodes == nil {
			return "", "", errors.New("thread feedback is incomplete")
		}
		for _, raw := range *thread.Comments.Nodes {
			var row struct {
				Author *struct {
					Login string `json:"login"`
				} `json:"author"`
			}
			if json.Unmarshal(raw, &row) != nil {
				return "", "", errors.New("thread feedback is invalid")
			}
			if row.Author == nil || row.Author.Login != actor.Login {
				external = true
			}
		}
		if external {
			raw, _ := json.Marshal(thread)
			comments = append(comments, raw)
		}
	}
	reviewKey, commentKey := "", ""
	if len(reviews) > 0 {
		reviewKey = key(reviews)
	}
	if len(comments) > 0 {
		commentKey = key(comments)
	}
	return reviewKey, commentKey, nil
}

func newRequest(current, previous []string) bool {
	seen := map[string]bool{}
	for _, id := range previous {
		seen[id] = true
	}
	for _, id := range current {
		if !seen[id] {
			return true
		}
	}
	return false
}
