package review

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// Repair is a separate, explicit decision. One invocation writes at most one
// missing inline; an unknown earlier attempt is reconciled, never retried.
func (p Publisher) Repair(ctx context.Context, document *config.Document, id, expected string) (result store.Run, resultErr error) {
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	run, err := p.Store.Get(ctx, id)
	if err != nil {
		return run, err
	}
	if run.Archive != nil {
		return run, errors.New("private review evidence expired; inspect its recorded conversation or start a fresh review, not a publication retry")
	}
	if expected == "" || expected != run.PayloadID {
		return run, errors.New("repair requires the exact stored preview identity via --payload")
	}
	var prepared PreparedResult
	if json.Unmarshal(run.Prepared, &prepared) != nil || ValidatePrepared(prepared) != nil {
		return run, errors.New("repair has no valid frozen preview")
	}
	if run.State != store.Partial && run.State != store.PublicationUnknown && run.State != store.Publishing && run.State != store.Published {
		return run, errors.New("repair requires an existing publication outcome")
	}
	run, err = p.reconcile(ctx, run, prepared, 0, false)
	if run.State == store.Published || run.State != store.Partial {
		return run, err
	}
	var outcome PublicationOutcome
	if json.Unmarshal(run.Outcome, &outcome) != nil || outcome.Review.ID <= 0 || outcome.PendingRepair != nil {
		return run, errors.New("repair cannot start until the previous publication is reconciled")
	}
	policy, err := config.Resolve(document, run.Repository, nil, nil)
	if err != nil {
		return run, err
	}
	if policy.String("github.host") != prepared.Boundary.Configuration.String("github.host") || policy.String("actions.publish_review") == "never" {
		return run, errors.New("current operator policy does not authorize a new repair write")
	}
	var missing Comment
	for _, wanted := range prepared.Comments {
		found := false
		for _, row := range outcome.Comments {
			if matchingComment(row, wanted, prepared.Boundary.Pull.HeadSHA) {
				found = true
				break
			}
		}
		if !found {
			missing = wanted
			break
		}
	}
	if missing.Key == "" {
		return run, errors.New("remote inventory differs but no source finding is missing; inspect it rather than adding duplicates")
	}
	snapshot, closeSource, err := p.acquireSource(ctx, prepared.Boundary.Pull)
	if err != nil {
		return run, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSource()) }()
	anchors, err := source.ParseAnchors(snapshot.Diff)
	if err != nil {
		return run, err
	}
	if snapshot.HeadSHA != prepared.Boundary.Pull.HeadSHA || snapshot.TargetBaseSHA != prepared.Boundary.Pull.TargetBaseSHA || snapshot.MergeBaseSHA != prepared.Boundary.MergeBaseSHA || snapshot.CommitCount != prepared.Boundary.Pull.CommitCount || !anchors.Contains(missing.Path, missing.Side, missing.Line) {
		return run, errors.New("repair source identity or canonical anchor changed")
	}
	actor, err := p.GitHub.Identity(ctx)
	if err != nil || actor.ID != run.ActorID {
		return run, errors.New("repair posting identity changed")
	}
	pull, err := p.GitHub.Pull(ctx, run.Repository, run.Number)
	if err != nil {
		return run, err
	}
	if !github.SameReviewContext(pull, prepared.Boundary.Pull) || (pull.Draft && !policy.Bool("watch.include_drafts")) {
		return run, errors.New("repair boundary is no longer eligible; reassess the current pull request")
	}
	remoteContext, err := p.GitHub.Context(ctx, pull)
	if err != nil {
		return run, err
	}
	if publicationContext(remoteContext, outcome).Digest != prepared.Boundary.ContextDigest {
		return run, errors.New("controlling context changed; repair cannot reuse the old publication decision")
	}
	checks, err := p.GitHub.Checks(ctx, pull)
	if err != nil {
		return run, err
	}
	boundary := prepared.Boundary
	boundary.Checks = checks
	updated, err := Prepare(boundary, prepared.Report)
	if err != nil {
		return run, err
	}
	if checks.RulesDigest == "" || updated.PayloadID != prepared.PayloadID {
		return run, errors.New("mandatory evidence changed; reassess its new preview before repairing")
	}
	history, err := p.GitHub.PublicationHistory(ctx, pull)
	if err != nil {
		return run, err
	}
	intent := &RepairIntent{CommentKey: missing.Key, PreviousIDs: []int64{}}
	for _, row := range history.Comments {
		intent.PreviousIDs = append(intent.PreviousIDs, row.ID)
		if matchingComment(row, missing, pull.HeadSHA) {
			if row.User.ID != run.ActorID || !reviewURL(row.URL, pull.URL, "discussion_r", row.ID) {
				return run, errors.New("the missing finding already has a conversation; inspect it rather than reposting")
			}
			if intent.CandidateID != 0 {
				return run, errors.New("equivalent inline outcomes are ambiguous; no repair was submitted")
			}
			intent.CandidateID = row.ID
		}
	}
	outcome.PendingRepair = intent
	outcome.Failure = ""
	data, _ := json.Marshal(outcome)
	claimed, err := p.Store.BeginRepair(ctx, run, expected, data)
	if err != nil || !claimed {
		return run, errors.Join(err, errors.New("repair could not claim the exact partial publication"))
	}
	var submitErr error
	if intent.CandidateID == 0 {
		remote, err := p.GitHub.CreateComment(ctx, pull, github.ReviewComment{Path: missing.Path, Side: missing.Side, Line: missing.Line, Body: missing.Body})
		intent.CandidateID = remote.ID
		submitErr = err
	}
	run.State = store.Publishing
	run.Outcome = data
	reconcileCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, reconcileErr := p.reconcile(reconcileCtx, run, prepared, outcome.Review.ID, outcome.Existing, intent.CandidateID)
	if reconcileErr != nil {
		return result, errors.Join(submitErr, reconcileErr)
	}
	return result, nil
}

func publicationContext(remote github.ReviewContext, outcome PublicationOutcome) github.ReviewContext {
	reviews := []int64{}
	comments := []int64{}
	if !outcome.Existing {
		reviews = append(reviews, outcome.Review.ID)
		for _, row := range outcome.Comments {
			if row.ReviewID == outcome.Review.ID {
				comments = append(comments, row.ID)
			}
		}
	}
	for _, row := range outcome.Repairs {
		reviews = append(reviews, row.ReviewID)
		comments = append(comments, row.ID)
	}
	return remote.WithoutPublications(reviews, comments)
}
