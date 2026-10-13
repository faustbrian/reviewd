package review

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Publisher is the only application operation that may submit review content.
// Its clients are never passed to a model, and its consent is payload-specific.
type Publisher struct {
	GitHub *github.Client
	Store  *store.Store
	Source func(context.Context, github.Pull) (*source.Snapshot, error)
}
type RepairIntent struct {
	CommentKey  string  `json:"comment_key"`
	PreviousIDs []int64 `json:"previous_ids"`
	CandidateID int64   `json:"candidate_id,omitempty"`
}
type PublicationOutcome struct {
	CurrentChecks    *github.CheckReport    `json:"current_checks,omitempty"`
	Repairs          []github.RemoteComment `json:"repairs,omitempty"`
	InitialReviewIDs []int64                `json:"initial_review_ids,omitempty"`
	PendingRepair    *RepairIntent          `json:"pending_repair,omitempty"`
	Review           github.RemoteReview    `json:"review"`
	Comments         []github.RemoteComment `json:"comments"`
	ReviewedHead     string                 `json:"reviewed_head"`
	CurrentHead      string                 `json:"current_head"`
	Verified         bool                   `json:"verified"`
	Existing         bool                   `json:"existing"`
	Failure          string                 `json:"failure,omitempty"`
}

func (p Publisher) Publish(ctx context.Context, document *config.Document, id, expected string, automatic bool) (result store.Run, resultErr error) {
	if p.GitHub == nil || p.Store == nil {
		return store.Run{}, errors.New("publication dependencies are unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run, err := p.Store.Get(ctx, id)
	if err != nil {
		return run, err
	}
	var prepared PreparedResult
	if json.Unmarshal(run.Prepared, &prepared) != nil || ValidatePrepared(prepared) != nil || prepared.PayloadID != run.PayloadID {
		return run, errors.New("run has no valid exact prepared preview")
	}
	if run.Repository != prepared.Boundary.Pull.Repository || run.RepositoryID != prepared.Boundary.Pull.RepositoryID || run.PullID != prepared.Boundary.Pull.ID || run.ActorID != prepared.Boundary.Actor.ID {
		return run, errors.New("prepared publication scope is inconsistent")
	}
	if run.State == store.Publishing || run.State == store.PublicationUnknown || run.State == store.Partial || run.State == store.Published {
		return p.reconcile(ctx, run, prepared, 0, false)
	}
	if run.State != store.Prepared {
		return run, errors.New("only a current prepared review can publish")
	}
	policy, err := config.Resolve(document, run.Repository, nil, nil)
	if err != nil {
		return run, err
	}
	if policy.String("github.host") != prepared.Boundary.Configuration.String("github.host") {
		return run, errors.New("configured GitHub host changed; start a review in the intended profile")
	}
	mode := policy.String("actions.publish_review")
	if mode == "never" || (automatic && (mode != "auto" || run.Private)) {
		return run, errors.New("current operator policy keeps this review private")
	}
	if expected != "" && expected != run.PayloadID {
		return run, errors.New("publication decision does not match the current preview; inspect reviewd show")
	}
	service := Service{GitHub: p.GitHub, Store: p.Store}
	run, err = service.RefreshChecks(ctx, id)
	if err != nil {
		return run, err
	}
	if json.Unmarshal(run.Prepared, &prepared) != nil {
		return run, errors.New("refreshed preview is unavailable")
	}
	if expected == "" {
		return run, nil
	}
	if run.PayloadID != expected {
		return run, errors.New("fresh evidence changed the preview; inspect it and decide on its new payload")
	}
	pull := prepared.Boundary.Pull
	if pull.Draft && !policy.Bool("watch.include_drafts") {
		return run, errors.New("draft pull request is not eligible under current operator policy")
	}
	acquire := p.Source
	if acquire == nil {
		acquire = func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
			return p.GitHub.Source(ctx, pull, "")
		}
	}
	snapshot, err := acquire(ctx, pull)
	if err != nil {
		return run, err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	anchors, err := source.ParseAnchors(snapshot.Diff)
	if err != nil {
		return run, err
	}
	if snapshot.HeadSHA != pull.HeadSHA || snapshot.TargetBaseSHA != pull.TargetBaseSHA || snapshot.MergeBaseSHA != prepared.Boundary.MergeBaseSHA || snapshot.CommitCount != pull.CommitCount {
		return run, errors.New("immutable source identity changed; publication is unavailable")
	}
	for _, comment := range prepared.Comments {
		if !anchors.Contains(comment.Path, comment.Side, comment.Line) {
			return run, errors.New("publication finding has no verified immutable anchor")
		}
	}
	history, err := p.GitHub.PublicationHistory(ctx, pull)
	if err != nil {
		return run, err
	}
	remoteID := int64(0)
	for _, row := range history.Reviews {
		if matchingReview(row, prepared) {
			if remoteID != 0 {
				return run, errors.New("multiple equivalent remote reviews exist; publication requires reconciliation")
			}
			remoteID = row.ID
		}
	}
	if remoteID == 0 {
		// Rebuild references from freshly acquired remote identities. A changed
		// wire preview invalidates consent; it is never silently submitted.
		boundary := prepared.Boundary
		boundary.ExistingFindings = nil
		full, err := Prepare(boundary, prepared.Report)
		if err != nil {
			return run, err
		}
		for _, comment := range full.Comments {
			for _, remote := range history.Comments {
				if matchingComment(remote, comment, pull.HeadSHA) && reviewURL(remote.URL, pull.URL, "discussion_r", remote.ID) {
					boundary.ExistingFindings = append(boundary.ExistingFindings, ExistingFinding{Key: comment.Key, CommentID: remote.ID, URL: remote.URL})
					break
				}
			}
		}
		normalized, err := Prepare(boundary, prepared.Report)
		if err != nil {
			return run, err
		}
		if normalized.PayloadID != prepared.PayloadID {
			data, _ := json.Marshal(normalized)
			if err := p.Store.RefreshPrepared(ctx, id, run.PayloadID, normalized.PayloadID, data); err != nil {
				return run, err
			}
			current, getErr := p.Store.Get(ctx, id)
			return current, errors.Join(errors.New("existing conversations changed the preview; inspect its references and decide on the new payload"), getErr)
		}
	}

	// Repeat the current-boundary/check/context preflight after source collection.
	run, err = service.RefreshChecks(ctx, id)
	if err != nil {
		return run, err
	}
	if run.PayloadID != expected {
		return run, errors.New("evidence changed during preflight; inspect the new preview before publishing")
	}
	if err = p.Store.DecidePublication(ctx, id, expected); err != nil {
		return run, err
	}
	initial := PublicationOutcome{ReviewedHead: pull.HeadSHA, Existing: remoteID != 0, Review: github.RemoteReview{ID: remoteID}, Comments: []github.RemoteComment{}}
	for _, row := range history.Reviews {
		initial.InitialReviewIDs = append(initial.InitialReviewIDs, row.ID)
	}
	initialData, _ := json.Marshal(initial)
	_, claimed, err := p.Store.BeginPublication(ctx, id, expected, initialData)
	if err != nil {
		return run, err
	}
	if !claimed {
		return run, errors.New("publication is already claimed or its decision changed")
	}
	run, err = p.Store.Get(ctx, id)
	if err != nil {
		return run, err
	}
	if remoteID != 0 {
		return p.reconcile(ctx, run, prepared, remoteID, true)
	}
	comments := make([]github.ReviewComment, 0, len(prepared.Comments))
	for _, comment := range prepared.Comments {
		comments = append(comments, github.ReviewComment{Path: comment.Path, Side: comment.Side, Line: comment.Line, Body: comment.Body})
	}
	remote, submitErr := p.GitHub.CreateReview(ctx, pull, github.ReviewSubmission{CommitID: pull.HeadSHA, Event: prepared.Event, Body: prepared.Body, Comments: comments})
	// Even an HTTP error must reconcile. Cancellation does not erase a write.
	reconcileCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	result, reconcileErr := p.reconcile(reconcileCtx, run, prepared, remote.ID, false)
	if reconcileErr != nil && submitErr != nil {
		return result, errors.Join(submitErr, reconcileErr)
	}
	return result, reconcileErr
}

func eventState(event string) string {
	switch event {
	case "APPROVE":
		return "APPROVED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	default:
		return "COMMENTED"
	}
}
func matchingReview(row github.RemoteReview, result PreparedResult) bool {
	return row.ID > 0 && row.User.ID == result.Boundary.Actor.ID && strings.EqualFold(row.User.Login, result.Boundary.Actor.Login) && row.CommitID == result.Boundary.Pull.HeadSHA && row.State == eventState(result.Event) && row.Body == result.Body && row.SubmittedAt != nil && !row.SubmittedAt.IsZero()
}
func matchingComment(row github.RemoteComment, comment Comment, head string) bool {
	line := row.OriginalLine
	if line == nil {
		line = row.Line
	}
	return row.ReplyTo == 0 && row.OriginalCommitID == head && line != nil && *line == comment.Line && row.Path == comment.Path && row.Side == comment.Side && row.Body == comment.Body
}

func (p Publisher) reconcile(ctx context.Context, run store.Run, prepared PreparedResult, remoteID int64, existing bool, repairHint ...int64) (store.Run, error) {
	hint := int64(0)
	if len(repairHint) > 0 {
		hint = repairHint[0]
	}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := p.reconcileOnce(ctx, run, prepared, remoteID, existing, hint)
		if !errors.Is(err, store.ErrOutcomeChanged) {
			return current, err
		}
		if current.ID == "" || current.State == store.Stale {
			return current, err
		}
		run = current
		remoteID = 0
		existing = false
		hint = 0
	}
	return run, store.ErrOutcomeChanged
}

func (p Publisher) reconcileOnce(ctx context.Context, run store.Run, prepared PreparedResult, remoteID int64, existing bool, repairHint int64) (store.Run, error) {
	var prior PublicationOutcome
	if json.Unmarshal(run.Outcome, &prior) == nil && prior.Review.ID > 0 && remoteID == 0 {
		remoteID = prior.Review.ID
		existing = prior.Existing
	}
	outcome := PublicationOutcome{ReviewedHead: prepared.Boundary.Pull.HeadSHA, Existing: existing, Comments: []github.RemoteComment{}}
	if remoteID > 0 {
		outcome.Review.ID = remoteID
	}
	outcome.InitialReviewIDs = prior.InitialReviewIDs
	outcome.Repairs = prior.Repairs
	outcome.PendingRepair = prior.PendingRepair
	if outcome.PendingRepair != nil && repairHint > 0 {
		outcome.PendingRepair.CandidateID = repairHint
	}
	fail := func(message string) (store.Run, error) {
		if run.State == store.Published {
			return run, errors.New(message + "; the earlier verified publication remains recorded")
		}
		// Keep already observed remote identities visible when fresh readback fails;
		// retained evidence is not a claim that the current inventory is verified.
		if outcome.Review.URL == "" && prior.Review.ID == outcome.Review.ID {
			outcome.Review = prior.Review
		}
		known := map[int64]bool{}
		for _, row := range outcome.Comments {
			known[row.ID] = true
		}
		for _, row := range prior.Comments {
			if !known[row.ID] {
				outcome.Comments = append(outcome.Comments, row)
				known[row.ID] = true
			}
		}
		if outcome.CurrentHead == "" {
			outcome.CurrentHead = prior.CurrentHead
		}
		outcome.Failure = message
		data, _ := json.Marshal(outcome)
		saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		err := p.Store.MarkOutcome(saveCtx, run, store.PublicationUnknown, data)
		current, getErr := p.Store.Get(saveCtx, run.ID)
		return current, errors.Join(errors.New(message), err, getErr)
	}
	actor, err := p.GitHub.Identity(ctx)
	if err != nil || actor.ID != run.ActorID {
		return fail("publication identity could not be re-established; no retry was submitted")
	}
	if remoteID == 0 {
		history, err := p.GitHub.PublicationHistory(ctx, prepared.Boundary.Pull)
		if err != nil {
			return fail("publication history is unavailable; outcome remains unknown")
		}
		excluded := map[int64]bool{}
		for _, id := range outcome.InitialReviewIDs {
			excluded[id] = true
		}
		for _, row := range history.Reviews {
			if !excluded[row.ID] && matchingReview(row, prepared) {
				if remoteID != 0 {
					return fail("publication outcome is ambiguous; equivalent remote reviews require inspection")
				}
				remoteID = row.ID
			}
		}
		if remoteID == 0 {
			return fail("no verified remote review was found; outcome remains unknown and was not reposted")
		}
	}
	remote, err := p.GitHub.Review(ctx, prepared.Boundary.Pull, remoteID)
	if err != nil || !matchingReview(remote, prepared) || remote.ID != remoteID || !reviewURL(remote.URL, prepared.Boundary.Pull.URL, "pullrequestreview-", remoteID) {
		return fail("remote review identity, event, body or timestamp did not match the submission")
	}
	outcome.Review = remote
	comments, err := p.GitHub.ReviewComments(ctx, prepared.Boundary.Pull, remoteID)
	if err != nil {
		return fail("remote inline readback was unavailable; publication was not verified")
	}
	if outcome.PendingRepair != nil {
		intent := outcome.PendingRepair
		var wanted Comment
		for _, row := range prepared.Comments {
			if row.Key == intent.CommentKey {
				wanted = row
				break
			}
		}
		if wanted.Key == "" {
			return fail("repair intent no longer matches the exact prepared payload")
		}
		candidate := intent.CandidateID
		if candidate == 0 {
			history, err := p.GitHub.PublicationHistory(ctx, prepared.Boundary.Pull)
			if err != nil {
				return fail("repair history is unavailable; no repair was retried")
			}
			previous := map[int64]bool{}
			for _, id := range intent.PreviousIDs {
				previous[id] = true
			}
			for _, row := range history.Comments {
				if !previous[row.ID] && row.User.ID == run.ActorID && matchingComment(row, wanted, outcome.ReviewedHead) {
					if candidate != 0 {
						return fail("repair outcome is ambiguous; no inline was reposted")
					}
					candidate = row.ID
				}
			}
		}
		if candidate == 0 {
			return fail("repair outcome remains unknown; no inline was reposted")
		}
		repaired, err := p.GitHub.Comment(ctx, prepared.Boundary.Pull, candidate)
		if err != nil || repaired.ID != candidate || repaired.User.ID != run.ActorID || !matchingComment(repaired, wanted, outcome.ReviewedHead) || !reviewURL(repaired.URL, prepared.Boundary.Pull.URL, "discussion_r", candidate) {
			return fail("repair inline identity or canonical readback did not match")
		}
		outcome.Repairs = append(outcome.Repairs, repaired)
		outcome.PendingRepair = nil
	}

	for _, repair := range outcome.Repairs {
		canonical, err := p.GitHub.Comment(ctx, prepared.Boundary.Pull, repair.ID)
		if err != nil || canonical.ID != repair.ID || canonical.User.ID != run.ActorID || !reviewURL(canonical.URL, prepared.Boundary.Pull.URL, "discussion_r", canonical.ID) {
			return fail("previously repaired inline could not be canonically verified")
		}
		var wanted Comment
		for _, row := range prepared.Comments {
			if matchingComment(canonical, row, outcome.ReviewedHead) {
				wanted = row
				break
			}
		}
		if wanted.Key == "" {
			return fail("repaired inline no longer matches the source finding")
		}
		repairReview, err := p.GitHub.Review(ctx, prepared.Boundary.Pull, canonical.ReviewID)
		if err != nil || repairReview.ID != canonical.ReviewID || repairReview.User.ID != run.ActorID || repairReview.CommitID != outcome.ReviewedHead || !reviewURL(repairReview.URL, prepared.Boundary.Pull.URL, "pullrequestreview-", repairReview.ID) {
			return fail("repaired inline review identity could not be verified")
		}
		comments = append(comments, canonical)
	}
	missing := false
	used := map[int64]bool{}
	for _, wanted := range prepared.Comments {
		found := false
		for _, row := range comments {
			if row.User.ID == run.ActorID && (row.ReviewID == remoteID || len(outcome.Repairs) > 0) && matchingComment(row, wanted, outcome.ReviewedHead) && reviewURL(row.URL, prepared.Boundary.Pull.URL, "discussion_r", row.ID) && !used[row.ID] {
				outcome.Comments = append(outcome.Comments, row)
				used[row.ID] = true
				found = true
				break
			}
		}
		if !found {
			missing = true
		}
	}
	if len(used) != len(comments) {
		missing = true
	}
	current, err := p.GitHub.Pull(ctx, run.Repository, run.Number)
	if err != nil || current.RepositoryID != run.RepositoryID || current.ID != run.PullID {
		return fail("post-publication pull identity could not be verified")
	}
	outcome.CurrentHead = current.HeadSHA
	contextChanged := false
	if !missing {
		remoteContext, contextErr := p.GitHub.Context(ctx, current)
		if contextErr != nil {
			return fail("post-publication controlling context was unavailable")
		}
		contextChanged = publicationContext(remoteContext, outcome).Digest != prepared.Boundary.ContextDigest
		checks, checkErr := p.GitHub.Checks(ctx, current)
		if checkErr != nil {
			return fail("post-publication mandatory evidence was unavailable")
		}
		outcome.CurrentChecks = &checks
		boundary := prepared.Boundary
		boundary.Checks = checks
		updated, prepareErr := Prepare(boundary, prepared.Report)
		if prepareErr != nil || updated.PayloadID != prepared.PayloadID {
			contextChanged = true
		}
	}
	state := store.Published
	if missing && run.State == store.Published {
		return fail("current remote inline inventory differs from the earlier verified publication")
	}
	if missing {
		state = store.Partial
		outcome.Failure = "remote inline inventory differs; repair requires an explicit decision and must not repost verified findings"
	} else {
		outcome.Verified = true
		if contextChanged || !github.SameReviewContext(prepared.Boundary.Pull, current) {
			state = store.Stale
			outcome.Failure = "The current boundary or controlling evidence differs; reassess before treating this review as current."
		}
	}
	data, _ := json.Marshal(outcome)
	if err := p.Store.MarkOutcome(ctx, run, state, data); err != nil {
		current, getErr := p.Store.Get(ctx, run.ID)
		return current, errors.Join(err, getErr)
	}
	result, err := p.Store.Get(ctx, run.ID)
	if missing {
		return result, errors.Join(errors.New(outcome.Failure), err)
	}
	return result, err
}
func reviewURL(actual, pull, prefix string, id int64) bool {
	value, err := url.Parse(actual)
	expected, expectedErr := url.Parse(pull)
	return err == nil && expectedErr == nil && value.Scheme == "https" && value.User == nil && value.RawQuery == "" && !value.ForceQuery && strings.EqualFold(value.Host, expected.Host) && strings.EqualFold(value.Path, expected.Path) && value.Fragment == prefix+fmtInt(id)
}
func fmtInt(value int64) string { return strconv.FormatInt(value, 10) }
