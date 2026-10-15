package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
)

type DeliveryProposal struct {
	CommitMessage string          `json:"commit_message"`
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	Replies       []DeliveryReply `json:"replies"`
}
type DeliveryReply struct {
	Key     string `json:"key"`
	Body    string `json:"body"`
	Resolve bool   `json:"resolve"`
}
type DeliveryEffect struct {
	Kind    string          `json:"kind"`
	Key     string          `json:"key"`
	State   string          `json:"state"`
	Receipt json.RawMessage `json:"receipt,omitempty"`
	Error   string          `json:"error,omitempty"`
}
type DeliveryJournal struct {
	Started          bool                  `json:"started"`
	Candidate        string                `json:"candidate"`
	Proposal         DeliveryProposal      `json:"proposal"`
	Commit           CommittedFeedback     `json:"commit"`
	Target           github.FeedbackTarget `json:"target"`
	Checks           github.CheckReport    `json:"checks"`
	Effects          []DeliveryEffect      `json:"effects"`
	Complete         bool                  `json:"complete"`
	CandidateFile    *retainedCandidate    `json:"candidate_file,omitempty"`
	CodeChanged      bool                  `json:"code_changed"`
	RereviewRequired bool                  `json:"rereview_required"`
}

type deliveryRemote interface {
	Remote
	PushFeedbackCommit(context.Context, github.FeedbackTarget, *source.Snapshot, string, string, string, []byte) (github.FeedbackTarget, error)
	UpdateFeedbackMetadata(context.Context, github.Pull, string, string) error
	ReplyToFeedbackDiscussion(context.Context, github.Pull, string) (github.FeedbackReply, error)
	ReplyToFeedbackThread(context.Context, github.Pull, string, string) (github.FeedbackReply, error)
	ResolveFeedbackThread(context.Context, github.Pull, string) error
	FeedbackReviewRequests(context.Context, github.Pull) (github.FeedbackRequests, error)
	RequestFeedbackReviewer(context.Context, github.Pull, string, string) error
}

type deliveryOptions struct {
	previous    *DeliveryJournal
	Remote      deliveryRemote
	Resources   *ExecutionResources
	Snapshot    *source.Snapshot
	Policy      CommitPolicy
	RunID       string
	Target      github.FeedbackTarget
	Actor       github.Identity
	Inventory   Inventory
	Handled     []HandledRevision
	Context     github.ReviewContext
	Revalidate  func() error
	AdoptTarget func(github.FeedbackTarget)
}

func feedbackReviewerKey(login, team string) string {
	if team != "" {
		return "team:" + strings.ToLower(team)
	}
	return "user:" + strings.ToLower(login)
}

func (o deliveryOptions) reconcile(ctx context.Context, previous DeliveryJournal, decisions []ItemDisposition, record func(DeliveryJournal) error) (DeliveryJournal, error) {
	o.previous = &previous
	return o.deliver(ctx, previous.Proposal, previous.Candidate, decisions, record)
}

// Reads may recognize an attempted effect; they never repeat it. A verified
// hook is a historical fact, not permission to execute it again after restart.
func (o deliveryOptions) reconcileEffect(ctx context.Context, journal DeliveryJournal, prior DeliveryEffect, decisions map[string]ItemDisposition) (any, error) {
	verified := prior.State == "verified"
	switch prior.Kind {
	case "commit", "pre_push":
		if verified && objectID.MatchString(journal.Commit.OID) {
			return json.RawMessage(prior.Receipt), nil
		}
	case "push":
		current, err := o.Remote.FeedbackTarget(ctx, journal.Target.Pull.Repository, journal.Target.Pull.Number)
		if err == nil && objectID.MatchString(journal.Commit.OID) && current.Pull.HeadSHA == journal.Commit.OID && current.HeadRepository == journal.Target.HeadRepository && current.HeadRepositoryID == journal.Target.HeadRepositoryID && github.SameReviewContext(current.Pull, journal.Target.Pull) {
			return current, nil
		}
	case "metadata":
		current, err := o.Remote.FeedbackTarget(ctx, journal.Target.Pull.Repository, journal.Target.Pull.Number)
		if err == nil && current.Pull.Title == journal.Proposal.Title && current.Pull.Body == journal.Proposal.Body && github.SameReviewContext(current.Pull, journal.Target.Pull) {
			return current, nil
		}
	case "reply":
		var item *Item
		for index := range o.Inventory.Items {
			if o.Inventory.Items[index].Key == prior.Key {
				item = &o.Inventory.Items[index]
				break
			}
		}
		var reply *DeliveryReply
		for index := range journal.Proposal.Replies {
			if journal.Proposal.Replies[index].Key == prior.Key {
				reply = &journal.Proposal.Replies[index]
				break
			}
		}
		if item == nil || reply == nil {
			return nil, errors.New("attempted reply no longer identifies its original human revision")
		}
		body := feedbackReplyBody(o.RunID, *item, decisions[prior.Key], *reply, journal.Target.Pull.HeadSHA)
		var receipt github.FeedbackReply
		if len(prior.Receipt) > 0 {
			_ = json.Unmarshal(prior.Receipt, &receipt)
		}
		current, err := o.Remote.Context(ctx, journal.Target.Pull)
		if err == nil && canonicalFeedbackReply(current, o.Actor, item.ThreadID, body, receipt.ID, receipt.NodeID) {
			return receipt, nil
		}
	case "resolve":
		current, err := o.Remote.Context(ctx, journal.Target.Pull)
		if err == nil && len(current.Gaps) == 0 {
			for _, thread := range current.Threads {
				if thread.ID == prior.Key && thread.IsResolved != nil && *thread.IsResolved {
					return thread, nil
				}
			}
		}
	case "review_request":
		// An already verified request may have been answered or cancelled since
		// its canonical receipt. Do not remove/re-add it to notify again.
		if verified {
			return json.RawMessage(prior.Receipt), nil
		}
		kind, value, ok := strings.Cut(prior.Key, ":")
		if ok && (kind == "user" || kind == "team") {
			current, err := o.Remote.FeedbackReviewRequests(ctx, journal.Target.Pull)
			login, team := value, ""
			if kind == "team" {
				login, team = "", value
			}
			if err == nil && feedbackRequestPresent(current, login, team) {
				return current, nil
			}
		}
	}
	return nil, errors.New("attempted " + prior.Kind + " is not canonically identified; no replay was performed")
}

func feedbackReplyBody(runID string, item Item, decision ItemDisposition, reply DeliveryReply, head string) string {
	body := "**Still open:** " + reply.Body
	if decision.Status == "accepted" || decision.Status == "already_addressed" {
		body = "**Addressed in " + head + ":** " + reply.Body
	} else if decision.Status == "needs_clarification" {
		body = "**Clarification:** " + reply.Body
	}
	return body + "\n\n<!-- reviewd-feedback:" + runID + ":" + item.Key + ":" + item.Revision + " -->"
}

// deliver owns fixed effects only. Facts live in the existing execution/run
// journal; every attempted mutation is persisted before I/O. An error never
// automatically repeats an uncertain operation or converts model prose to fact.
func (o deliveryOptions) deliver(ctx context.Context, proposal DeliveryProposal, candidate string, decisions []ItemDisposition, record func(DeliveryJournal) error) (journal DeliveryJournal, err error) {
	journal = DeliveryJournal{Started: true, Candidate: candidate, Proposal: proposal, Target: o.Target, Effects: []DeliveryEffect{}}
	resuming := o.previous != nil
	if resuming {
		data, _ := json.Marshal(o.previous)
		if json.Unmarshal(data, &journal) != nil || !journal.Started || journal.Candidate != candidate {
			return journal, errors.New("retained delivery journal is invalid")
		}
	}
	if o.Remote == nil || o.Resources == nil || o.Resources.Inspect == nil || o.Resources.Quiesce == nil || o.Snapshot == nil || o.Revalidate == nil || o.AdoptTarget == nil {
		return journal, errors.New("scoped feedback delivery prerequisites are unavailable")
	}
	if strings.TrimSpace(proposal.Title) == "" || len(proposal.Title) > 256 || len(proposal.Body) > 65536 || len(proposal.Replies) > 1000 || strings.ContainsAny(proposal.Title+proposal.Body, "\x00") {
		return journal, errors.New("feedback delivery proposal is incomplete or exceeds its bound")
	}
	settled := map[string]bool{}
	byKey := map[string]Item{}
	previouslyProposed := map[string]bool{}
	if resuming {
		for _, reply := range proposal.Replies {
			previouslyProposed[reply.Key] = true
		}
	}
	for _, item := range o.Inventory.Items {
		for _, previous := range o.Handled {
			if previous.Key == item.Key && previous.Revision == item.Revision {
				settled[item.Key] = true
				break
			}
		}
		if !settled[item.Key] || previouslyProposed[item.Key] {
			byKey[item.Key] = item
		}
	}
	dispositions := map[string]ItemDisposition{}
	for _, decision := range decisions {
		dispositions[decision.Key] = decision
		settled[decision.Key] = decision.Status == "accepted" || decision.Status == "already_addressed" || decision.Status == "rejected"
	}
	seen := map[string]bool{}
	for _, reply := range proposal.Replies {
		item, ok := byKey[reply.Key]
		decision, decided := dispositions[reply.Key]
		if !ok || !decided || decision.Revision != item.Revision || seen[reply.Key] || strings.TrimSpace(reply.Body) == "" || len(reply.Body) > 48<<10 {
			return journal, errors.New("each proposed reply must name one exact, decided human revision")
		}
		seen[reply.Key] = true
		if reply.Resolve && (item.ThreadID == "" || !settled[item.Key]) {
			return journal, errors.New("unsettled feedback cannot resolve a thread")
		}
		if reply.Resolve {
			for _, thread := range o.Inventory.Threads {
				if thread.ID == item.ThreadID {
					for _, key := range thread.ItemKeys {
						if !settled[key] {
							return journal, errors.New("a thread still contains unsettled human feedback")
						}
					}
				}
			}
		}
	}
	for key := range byKey {
		if !seen[key] {
			return journal, errors.New("delivery omitted an original human feedback reply/disposition")
		}
	}
	if err = o.Revalidate(); err != nil {
		return journal, err
	}
	codeChanged := journal.CodeChanged
	if !resuming {
		inspection, inspectErr := o.Resources.Inspect(ctx)
		if inspectErr = errors.Join(inspectErr, o.Resources.Quiesce()); inspectErr != nil {
			return journal, inspectErr
		}
		codeChanged = inspection.StagedDiff != ""
		if !codeChanged && inspection.Status != "" {
			return journal, errors.New("unstaged feedback work must be reconciled before a non-code answer")
		}
		journal.CodeChanged = codeChanged
		journal.RereviewRequired = codeChanged || proposal.Title != journal.Target.Pull.Title || proposal.Body != journal.Target.Pull.Body
	}
	if err = record(journal); err != nil {
		return journal, err
	}
	effect := func(kind, key string, operation func() (any, error)) error {
		if err := o.Revalidate(); err != nil {
			return err
		}
		if resuming {
			for index := range journal.Effects {
				prior := &journal.Effects[index]
				if prior.Kind == kind && prior.Key == key {
					receipt, readErr := o.reconcileEffect(ctx, journal, *prior, dispositions)
					if readErr != nil {
						return readErr
					}
					prior.State = "verified"
					prior.Error = ""
					if receipt != nil {
						prior.Receipt, _ = json.Marshal(receipt)
					}
					return record(journal)
				}
			}
		}
		journal.Effects = append(journal.Effects, DeliveryEffect{Kind: kind, Key: key, State: "attempting"})
		index := len(journal.Effects) - 1
		if err := record(journal); err != nil {
			return err
		}
		receipt, opErr := operation()
		state := &journal.Effects[index]
		if opErr != nil {
			state.State = "unknown"
			state.Error = opErr.Error()
		} else {
			state.State = "verified"
		}
		if receipt != nil {
			state.Receipt, _ = json.Marshal(receipt)
		}
		return errors.Join(opErr, record(journal))
	}
	if codeChanged {
		if err = effect("commit", o.Target.Pull.HeadSHA, func() (any, error) {
			commit, commitErr := o.Resources.CommitFeedback(ctx, o.Snapshot, o.Policy, proposal.CommitMessage)
			journal.Commit = commit
			return commit, commitErr
		}); err != nil {
			return journal, err
		}
		if err = effect("pre_push", journal.Commit.OID, func() (any, error) {
			return map[string]any{"hook": "pre-push", "commit": journal.Commit.OID}, o.Resources.CheckFeedbackPush(ctx, o.Policy, "refs/heads/"+o.Target.Pull.HeadRef, "https://"+o.Target.HeadRepository+".git", journal.Commit)
		}); err != nil {
			return journal, err
		}
		if err = effect("push", journal.Commit.OID, func() (any, error) {
			delivered, pushErr := o.Remote.PushFeedbackCommit(ctx, o.Target, o.Snapshot, o.Resources.Directory, journal.Commit.OID, journal.Commit.Tree, journal.Commit.Contents)
			if pushErr != nil {
				return nil, pushErr
			}
			journal.Target = delivered
			o.AdoptTarget(delivered)
			return delivered, nil
		}); err != nil {
			return journal, err
		}
	}
	metadataChanged := proposal.Title != journal.Target.Pull.Title || proposal.Body != journal.Target.Pull.Body
	for _, prior := range journal.Effects {
		if prior.Kind == "metadata" {
			metadataChanged = true
		}
	}
	if metadataChanged {
		if err = effect("metadata", journal.Commit.OID, func() (any, error) {
			writeErr := o.Remote.UpdateFeedbackMetadata(ctx, journal.Target.Pull, proposal.Title, proposal.Body)
			current, readErr := o.Remote.FeedbackTarget(ctx, journal.Target.Pull.Repository, journal.Target.Pull.Number)
			if readErr != nil {
				return nil, errors.Join(writeErr, readErr)
			}
			expected := journal.Target
			expected.Pull.Title, expected.Pull.Body = proposal.Title, proposal.Body
			if current.HeadRepository != expected.HeadRepository || current.HeadRepositoryID != expected.HeadRepositoryID || !github.SameReviewContext(current.Pull, expected.Pull) {
				return nil, errors.Join(writeErr, errors.New("PR metadata write is not canonically confirmed"))
			}
			journal.Target = current
			o.AdoptTarget(current)
			return current, nil
		}); err != nil {
			return journal, err
		}
	}
	checks, err := o.Remote.Checks(ctx, journal.Target.Pull)
	journal.Checks = checks
	if saveErr := record(journal); err != nil || saveErr != nil {
		return journal, errors.Join(err, saveErr)
	}
	if checks.HeadSHA != journal.Target.Pull.HeadSHA || !checks.Ready {
		return journal, errors.New("current pushed-head mandatory checks are not ready; retain pending delivery without settling threads")
	}
	for _, reply := range proposal.Replies {
		item := byKey[reply.Key]
		decision := dispositions[reply.Key]
		body := feedbackReplyBody(o.RunID, item, decision, reply, journal.Target.Pull.HeadSHA)
		if err = effect("reply", item.Key, func() (any, error) {
			var receipt github.FeedbackReply
			var writeErr error
			if item.ThreadID != "" {
				receipt, writeErr = o.Remote.ReplyToFeedbackThread(ctx, journal.Target.Pull, item.ThreadID, body)
			} else {
				receipt, writeErr = o.Remote.ReplyToFeedbackDiscussion(ctx, journal.Target.Pull, body)
			}
			context, readErr := o.Remote.Context(ctx, journal.Target.Pull)
			if readErr != nil {
				return receipt, errors.Join(writeErr, readErr)
			}
			if !canonicalFeedbackReply(context, o.Actor, item.ThreadID, body, receipt.ID, receipt.NodeID) {
				return receipt, errors.Join(writeErr, errors.New("original feedback reply is not canonically confirmed; do not replay it"))
			}
			return receipt, nil
		}); err != nil {
			return journal, err
		}
		if reply.Resolve {
			if err = effect("resolve", item.ThreadID, func() (any, error) {
				writeErr := o.Remote.ResolveFeedbackThread(ctx, journal.Target.Pull, item.ThreadID)
				context, readErr := o.Remote.Context(ctx, journal.Target.Pull)
				if readErr != nil {
					return nil, errors.Join(writeErr, readErr)
				}
				for _, thread := range context.Threads {
					if thread.ID == item.ThreadID && thread.IsResolved != nil && *thread.IsResolved {
						return thread, nil
					}
				}
				return nil, errors.Join(writeErr, errors.New("original thread resolution is not canonically confirmed"))
			}); err != nil {
				return journal, err
			}
		}
	}
	if journal.RereviewRequired {
		requests, err := o.Remote.FeedbackReviewRequests(ctx, journal.Target.Pull)
		if err != nil {
			return journal, err
		}
		wanted := feedbackReviewerUnion(o.Context, requests, journal.Target.Pull.Author)
		for _, reviewer := range wanted {
			login, team := reviewer.User, reviewer.Team
			attempted := false
			for _, prior := range journal.Effects {
				if prior.Kind == "review_request" && prior.Key == feedbackReviewerKey(login, team) {
					attempted = true
				}
			}
			if feedbackRequestPresent(requests, login, team) && !attempted {
				continue
			}
			if err = effect("review_request", feedbackReviewerKey(login, team), func() (any, error) {
				writeErr := o.Remote.RequestFeedbackReviewer(ctx, journal.Target.Pull, login, team)
				current, readErr := o.Remote.FeedbackReviewRequests(ctx, journal.Target.Pull)
				if readErr != nil {
					return nil, errors.Join(writeErr, readErr)
				}
				if !feedbackRequestPresent(current, login, team) {
					return current, errors.Join(writeErr, errors.New("prior reviewer re-request is not canonically confirmed"))
				}
				requests = current
				return current, nil
			}); err != nil {
				return journal, err
			}
		}
	}
	if err = o.Revalidate(); err != nil {
		return journal, err
	}
	journal.Complete = true
	return journal, record(journal)
}

func canonicalFeedbackReply(context github.ReviewContext, actor github.Identity, thread, body string, id int64, node string) bool {
	if len(context.Gaps) > 0 {
		return false
	}
	if thread == "" {
		for _, raw := range context.Conversation {
			var row github.FeedbackReply
			if json.Unmarshal(raw, &row) == nil && row.ID > 0 && (id == 0 || row.ID == id) && row.Body == body && row.User.ID == actor.ID && strings.EqualFold(row.User.Login, actor.Login) {
				return true
			}
		}
		return false
	}
	for _, candidate := range context.Threads {
		if candidate.ID != thread || candidate.Comments.Nodes == nil {
			continue
		}
		for _, raw := range *candidate.Comments.Nodes {
			var row struct {
				ID         string
				DatabaseID int64 `json:"databaseId"`
				Body       string
				Author     struct{ Login string }
			}
			if json.Unmarshal(raw, &row) == nil && row.ID != "" && (node == "" || row.ID == node) && (id == 0 || row.DatabaseID == id) && row.Body == body && strings.EqualFold(row.Author.Login, actor.Login) {
				return true
			}
		}
	}
	return false
}

type feedbackReviewer struct{ User, Team string }

func feedbackRequestPresent(requests github.FeedbackRequests, login, team string) bool {
	for _, user := range requests.Users {
		if login != "" && strings.EqualFold(user.Login, login) {
			return true
		}
	}
	for _, candidate := range requests.Teams {
		if team != "" && strings.EqualFold(candidate.Slug, team) {
			return true
		}
	}
	return false
}
func feedbackReviewerUnion(context github.ReviewContext, current github.FeedbackRequests, author github.Identity) []feedbackReviewer {
	users, teams := map[string]bool{}, map[string]bool{}
	addUser := func(user github.Identity) {
		if user.Login != "" && user.ID != author.ID && !strings.EqualFold(user.Login, author.Login) {
			users[strings.ToLower(user.Login)] = true
		}
	}
	for _, user := range current.Users {
		addUser(user)
	}
	for _, team := range current.Teams {
		if team.Slug != "" {
			teams[strings.ToLower(team.Slug)] = true
		}
	}
	for _, raw := range context.Reviews {
		var row struct {
			User  github.Identity
			State string
		}
		if json.Unmarshal(raw, &row) == nil && row.State != "PENDING" {
			addUser(row.User)
		}
	}
	for _, raw := range context.Timeline {
		var row struct {
			Event string
			User  github.Identity      `json:"requested_reviewer"`
			Team  github.RequestedTeam `json:"requested_team"`
		}
		if json.Unmarshal(raw, &row) == nil && row.Event == "review_requested" {
			addUser(row.User)
			if row.Team.Slug != "" {
				teams[strings.ToLower(row.Team.Slug)] = true
			}
		}
	}
	result := []feedbackReviewer{}
	for user := range users {
		result = append(result, feedbackReviewer{User: user})
	}
	for team := range teams {
		result = append(result, feedbackReviewer{Team: team})
	}
	sort.Slice(result, func(i, j int) bool { return fmt.Sprint(result[i]) < fmt.Sprint(result[j]) })
	return result
}
