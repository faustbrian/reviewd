package review

import (
	"encoding/json"
	"errors"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

// ErrSuperseded distinguishes invalid evidence from an execution failure.
var ErrSuperseded = errors.New("review evidence was superseded")

// ItemWorkKey identifies one logical review of an observed event, not merely a
// head SHA. It is stable over watcher restarts and duplicate observations.
func ItemWorkKey(item store.InboxItem) string {
	return digest([]string{"inbox-event", item.ID, item.EventKey, item.DecisionID})
}

// EventContext keeps discovery and dispatch on the same owner-recorded
// publication boundary. Full private-review evidence stays untouched.
func EventContext(remote github.ReviewContext, pull github.Pull, actor github.Identity, runs []store.Run) (github.ReviewContext, error) {
	for _, run := range runs {
		if run.Repository != pull.Repository || run.PullID != pull.ID || run.ActorID != actor.ID || len(run.Outcome) == 0 {
			continue
		}
		var outcome PublicationOutcome
		if json.Unmarshal(run.Outcome, &outcome) != nil {
			return remote, errors.New("stored publication evidence is invalid")
		}
		reviews, comments := []int64{}, []int64{}
		// An acknowledged write is attributable even when canonical readback
		// has not completed. Discovery exclusion does not verify its outcome.
		if !outcome.Existing && outcome.Review.ID > 0 {
			reviews = append(reviews, outcome.Review.ID)
			for _, row := range outcome.Comments {
				if row.ReviewID == outcome.Review.ID {
					comments = append(comments, row.ID)
				}
			}
			// The complete REST inventory can identify root findings before the
			// publisher has read back their IDs. Never suppress replies, including
			// this user's later reply, merely because they share the review ID.
			for _, raw := range remote.Comments {
				var row struct {
					ID       int64           `json:"id"`
					ReviewID int64           `json:"pull_request_review_id"`
					ReplyTo  int64           `json:"in_reply_to_id"`
					User     github.Identity `json:"user"`
				}
				if json.Unmarshal(raw, &row) == nil && row.ID > 0 && row.ReviewID == outcome.Review.ID && row.ReplyTo == 0 && row.User.ID == run.ActorID {
					comments = append(comments, row.ID)
				}
			}
		}
		for _, row := range outcome.Repairs {
			reviews = append(reviews, row.ReviewID)
			comments = append(comments, row.ID)
		}
		remote = remote.WithoutPublications(reviews, comments)
	}
	return remote, nil
}
