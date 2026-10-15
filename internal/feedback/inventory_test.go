package feedback

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
)

func inventoryFixture(t *testing.T) github.ReviewContext {
	t.Helper()
	var result github.ReviewContext
	if err := json.Unmarshal([]byte(`{
 "reviews":[{"id":1,"user":{"id":8,"login":"human"},"state":"CHANGES_REQUESTED","body":"Preserve the caller contract.","submitted_at":"2026-10-15T02:00:00Z","html_url":"https://github.com/owner/repo/pull/1#pullrequestreview-1"}],
 "comments":[{"id":11,"node_id":"COMMENT11","user":{"id":8,"login":"human"},"body":"Handle an empty input.","created_at":"2026-10-15T02:00:00Z","updated_at":"2026-10-15T02:00:00Z","html_url":"https://github.com/owner/repo/pull/1#discussion_r11","pull_request_review_id":1}],
 "conversation":[{"id":20,"user":{"id":9,"login":"another"},"body":"The metadata must explain the new behavior.","created_at":"2026-10-15T02:01:00Z","updated_at":"2026-10-15T02:01:00Z","html_url":"https://github.com/owner/repo/pull/1#issuecomment-20"}],
 "threads":[{"id":"THREAD1","isResolved":false,"isOutdated":false,"path":"file.go","line":3,"diffSide":"RIGHT","comments":{"nodes":[{"id":"COMMENT11","databaseId":11,"author":{"login":"human"},"body":"Handle an empty input.","createdAt":"2026-10-15T02:00:00Z","updatedAt":"2026-10-15T02:00:00Z","url":"https://github.com/owner/repo/pull/1#discussion_r11","pullRequestReview":{"databaseId":1}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],
 "gaps":[]
}`), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInventoryCoalescesAllHumanFeedbackWithoutRESTGraphQLDuplicates(t *testing.T) {
	inventory, err := Collect(inventoryFixture(t), github.Identity{ID: 43, Login: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, item := range inventory.Items {
		keys = append(keys, item.Key)
	}
	if !reflect.DeepEqual(keys, []string{"discussion:20", "inline:11", "review:1"}) {
		t.Fatalf("lost or duplicated substantive feedback: %v", keys)
	}
	inline := inventory.Items[1]
	if inline.ThreadID != "THREAD1" || inline.NodeID != "COMMENT11" || inline.Author.ID != 8 || inline.ReviewID != 1 || inline.Body != "Handle an empty input." {
		t.Fatalf("incomplete merged inline feedback: %+v", inline)
	}
	if len(inventory.Threads) != 1 || len(inventory.Threads[0].ItemKeys) != 1 || inventory.Threads[0].ItemKeys[0] != "inline:11" || inventory.Threads[0].Resolved {
		t.Fatalf("thread delivery scope lost: %+v", inventory.Threads)
	}
}

func TestOwnRepliesResolutionAndLineMovementDoNotCreateNewHumanFeedback(t *testing.T) {
	actor := github.Identity{ID: 43, Login: "reviewer"}
	before := inventoryFixture(t)
	initial, err := Collect(before, actor)
	if err != nil {
		t.Fatal(err)
	}
	after := inventoryFixture(t)
	after.Comments = append(after.Comments, json.RawMessage(`{"id":12,"user":{"id":43,"login":"reviewer"},"body":"**Addressed in abc:** Fixed.","updated_at":"2026-10-15T02:02:00Z"}`))
	after.Conversation = append(after.Conversation, json.RawMessage(`{"id":21,"user":{"id":43,"login":"reviewer"},"body":"Delivery summary."}`))
	*after.Threads[0].Comments.Nodes = append(*after.Threads[0].Comments.Nodes, json.RawMessage(`{"id":"COMMENT12","databaseId":12,"author":{"login":"REVIEWER"},"body":"**Addressed in abc:** Fixed."}`))
	resolved, outdated := true, true
	after.Threads[0].IsResolved = &resolved
	after.Threads[0].IsOutdated = &outdated
	after.Threads[0].Line = nil
	updated, err := Collect(after, actor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Digest != initial.Digest || len(updated.Items) != 3 || !updated.Threads[0].Resolved {
		t.Fatal("own writes triggered feedback or erased complete resolution state")
	}
	*after.Threads[0].Comments.Nodes = append(*after.Threads[0].Comments.Nodes, json.RawMessage(`{"id":"COMMENT13","databaseId":13,"author":{"login":"human"},"body":"The empty input still fails.","updatedAt":"2026-10-15T02:03:00Z"}`))
	newHuman, err := Collect(after, actor)
	if err != nil {
		t.Fatal(err)
	}
	if newHuman.Digest == initial.Digest || len(newHuman.Items) != 4 {
		t.Fatal("new human reply in handled thread was suppressed")
	}
}

func TestEditedAndReorderedFeedbackKeepsStableCoalescingIdentity(t *testing.T) {
	actor := github.Identity{ID: 43, Login: "reviewer"}
	base := inventoryFixture(t)
	first, err := Collect(base, actor)
	if err != nil {
		t.Fatal(err)
	}
	base.Reviews = append(base.Reviews, json.RawMessage(`{"id":2,"user":{"id":8,"login":"human"},"state":"APPROVED","body":""}`), json.RawMessage(`{"id":3,"user":{"id":8,"login":"human"},"state":"PENDING","body":"Not yet submitted"}`))
	second, err := Collect(base, actor)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatal("empty approval or pending review manufactured substantive work")
	}
	base.Conversation[0] = json.RawMessage(`{"id":20,"user":{"id":9,"login":"another"},"body":"Updated metadata requirement.","updated_at":"2026-10-15T02:04:00Z"}`)
	edited, err := Collect(base, actor)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Digest == first.Digest {
		t.Fatal("edited same-ID feedback was lost")
	}
	base.Reviews[0], base.Reviews[2] = base.Reviews[2], base.Reviews[0]
	reordered, err := Collect(base, actor)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Digest != edited.Digest {
		t.Fatal("acquisition order created a second feedback batch")
	}
}

func TestAutomaticApprovalDismissalRetainsHumanRevisionAndActualState(t *testing.T) {
	remote := inventoryFixture(t)
	remote.Reviews[0] = json.RawMessage(`{"id":1,"user":{"id":8,"login":"human"},"state":"APPROVED","body":"Keep this nonblocking suggestion.","submitted_at":"2026-10-15T02:00:00Z"}`)
	actor := github.Identity{ID: 43, Login: "reviewer"}
	before, err := Collect(remote, actor)
	if err != nil {
		t.Fatal(err)
	}
	remote.Reviews[0] = json.RawMessage(`{"id":1,"user":{"id":8,"login":"human"},"state":"DISMISSED","body":"Keep this nonblocking suggestion.","submitted_at":"2026-10-15T02:00:00Z"}`)
	after, err := Collect(remote, actor)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest != after.Digest {
		t.Fatal("automatic approval dismissal manufactured human feedback")
	}
	if len(after.Items) != 3 || after.Items[2].ReviewState != "DISMISSED" {
		t.Fatal("actual review state was lost instead of excluded from human revision")
	}
}

func TestIncompleteOrInconsistentFeedbackCannotBecomeAnActionableInventory(t *testing.T) {
	for _, scenario := range []string{"gap", "thread_page", "identity", "edited_during_read"} {
		t.Run(scenario, func(t *testing.T) {
			remote := inventoryFixture(t)
			switch scenario {
			case "gap":
				remote.Gaps = []string{"later page denied"}
			case "thread_page":
				remote.Threads[0].Comments.Nodes = nil
			case "identity":
				remote.Conversation = append(remote.Conversation, json.RawMessage(`{"body":"Unattributed identity"}`))
			case "edited_during_read":
				(*remote.Threads[0].Comments.Nodes)[0] = json.RawMessage(`{"id":"COMMENT11","databaseId":11,"author":{"login":"human"},"body":"Edited while collecting."}`)
			}
			if result, err := Collect(remote, github.Identity{ID: 43, Login: "reviewer"}); err == nil || len(result.Items) != 0 || result.Digest != "" {
				t.Fatalf("incomplete inventory became actionable: %+v %v", result, err)
			}
		})
	}
}

func TestLaterRESTEnrichmentDoesNotRetriggerGraphQLFeedback(t *testing.T) {
	actor := github.Identity{ID: 43, Login: "reviewer"}
	first := inventoryFixture(t)
	first.Comments = nil
	graph, err := Collect(first, actor)
	if err != nil {
		t.Fatal(err)
	}
	enriched, err := Collect(inventoryFixture(t), actor)
	if err != nil {
		t.Fatal(err)
	}
	if graph.Digest != enriched.Digest {
		t.Fatal("REST author enrichment created a second feedback event")
	}
	if enriched.Items[1].Author.ID != 8 || enriched.Items[1].ThreadID != "THREAD1" {
		t.Fatal("enrichment discarded delivery identity")
	}
}
