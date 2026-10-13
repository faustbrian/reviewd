package github

import (
	"encoding/json"
	"testing"
)

func TestPublicationContextExcludesOnlyVerifiedWrites(t *testing.T) {
	base := (ReviewContext{}).WithoutPublications(nil, nil)
	current := base
	current.Reviews = []json.RawMessage{json.RawMessage(`{"id":71,"body":"own review"}`)}
	current.Comments = []json.RawMessage{json.RawMessage(`{"id":72,"body":"own finding"}`)}
	current.Timeline = []json.RawMessage{json.RawMessage(`{"id":71,"event":"reviewed"}`)}
	nodes := []json.RawMessage{json.RawMessage(`{"databaseId":72,"body":"own finding"}`)}
	unresolved, outdated := false, false
	current.Threads = []ReviewThread{{ID: "THREAD_A", IsResolved: &unresolved, IsOutdated: &outdated, Comments: connection{Nodes: &nodes}}}
	if current.WithoutPublications([]int64{71}, []int64{72}).Digest != base.Digest {
		t.Fatal("verified own publication invalidated its source context")
	}
	nodes = append(nodes, json.RawMessage(`{"databaseId":73,"body":"new external requirement"}`))
	if current.WithoutPublications([]int64{71}, []int64{72}).Digest == base.Digest {
		t.Fatal("external reply was hidden by publication exclusion")
	}
	current.Threads = nil
	current.Reviews = append(current.Reviews, json.RawMessage(`{"id":81,"body":"another review"}`))
	if current.WithoutPublications([]int64{71}, []int64{72}).Digest == base.Digest {
		t.Fatal("unrelated review was hidden by publication exclusion")
	}
}

func TestPublicationContextRetainsExternalResolutionAndOutdatedChanges(t *testing.T) {
	base := (ReviewContext{}).WithoutPublications(nil, nil)
	for _, resolved := range []bool{false, true} {
		for _, outdated := range []bool{false, true} {
			if !resolved && !outdated {
				continue
			}
			rows := []json.RawMessage{json.RawMessage(`{"databaseId":72,"body":"own finding"}`)}
			current := base
			current.Threads = []ReviewThread{{ID: "THREAD_A", IsResolved: &resolved, IsOutdated: &outdated, Comments: connection{Nodes: &rows}}}
			if current.WithoutPublications([]int64{71}, []int64{72}).Digest == base.Digest {
				t.Fatalf("external thread change disappeared: resolved=%v outdated=%v", resolved, outdated)
			}
		}
	}
}
