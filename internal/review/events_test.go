package review

import (
	"encoding/json"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/store"
)

// DC-02/03: attributing an acknowledged own write for discovery does not
// certify publication, hide a human reply, or guess an unacknowledged write.
func TestUnknownPublicationCandidateDoesNotTriggerAnotherSourceReview(t *testing.T) {
	for _, variant := range []struct {
		name     string
		existing bool
		id       int64
		removed  bool
	}{{"acknowledged", false, 71, true}, {"existing", true, 71, false}, {"unacknowledged", false, 0, false}} {
		t.Run(variant.name, func(t *testing.T) {
			var remote github.ReviewContext
			if err := json.Unmarshal([]byte(`{"reviews":[{"id":71,"user":{"id":43,"login":"reviewer"}}],"comments":[{"id":72,"pull_request_review_id":71,"user":{"id":43}},{"id":73,"pull_request_review_id":71,"user":{"id":55}},{"id":74,"pull_request_review_id":71,"in_reply_to_id":72,"user":{"id":43}}],"threads":[{"id":"thread","isResolved":false,"isOutdated":false,"comments":{"nodes":[{"databaseId":72},{"databaseId":73},{"databaseId":74}]}}]}`), &remote); err != nil {
				t.Fatal(err)
			}
			outcome, _ := json.Marshal(PublicationOutcome{Existing: variant.existing, Review: github.RemoteReview{ID: variant.id}})
			run := store.Run{Repository: "github.com/owner/repo", PullID: 701, ActorID: 43, State: store.PublicationUnknown, Outcome: outcome}
			filtered, err := EventContext(remote, github.Pull{PullReference: github.PullReference{Repository: run.Repository}, ID: run.PullID}, github.Identity{ID: 43, Login: "reviewer"}, []store.Run{run})
			if err != nil {
				t.Fatal(err)
			}
			if variant.removed {
				if len(filtered.Reviews) != 0 || len(filtered.Comments) != 2 || len(filtered.Threads) != 1 || len(*filtered.Threads[0].Comments.Nodes) != 2 {
					t.Fatal("acknowledged root/inline remained new context, or external replies disappeared")
				}
				for index, want := range []int64{73, 74} {
					var comment struct {
						ID int64 `json:"id"`
					}
					var node struct {
						ID int64 `json:"databaseId"`
					}
					if json.Unmarshal(filtered.Comments[index], &comment) != nil || json.Unmarshal((*filtered.Threads[0].Comments.Nodes)[index], &node) != nil || comment.ID != want || node.ID != want {
						t.Fatal("attribution hid an external or later reply instead of the acknowledged own finding")
					}
				}
			} else if len(filtered.Reviews) != 1 || len(filtered.Comments) != 3 {
				t.Fatal("unattributed or pre-existing activity was hidden")
			}
		})
	}
}
