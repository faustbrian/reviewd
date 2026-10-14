package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// WithoutPublications excludes only application-attributed writes themselves.
// Other replies, requirements and thread state remain controlling context.
func (c ReviewContext) WithoutPublications(reviewIDs []int64, commentIDs []int64) ReviewContext {
	ownedReviews := map[int64]bool{}
	for _, id := range reviewIDs {
		ownedReviews[id] = true
	}
	owned := map[int64]bool{}
	for _, id := range commentIDs {
		owned[id] = true
	}
	filter := func(rows []json.RawMessage, remove func(json.RawMessage) bool) []json.RawMessage {
		result := []json.RawMessage{}
		for _, row := range rows {
			if !remove(row) {
				result = append(result, row)
			}
		}
		return result
	}
	id := func(row json.RawMessage) int64 {
		var value struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(row, &value)
		return value.ID
	}
	c.Reviews = filter(c.Reviews, func(row json.RawMessage) bool { return ownedReviews[id(row)] })
	c.Comments = filter(c.Comments, func(row json.RawMessage) bool { return owned[id(row)] })
	c.Timeline = filter(c.Timeline, func(row json.RawMessage) bool {
		var value struct {
			ID    int64  `json:"id"`
			Event string `json:"event"`
		}
		_ = json.Unmarshal(row, &value)
		return value.Event == "reviewed" && ownedReviews[value.ID]
	})
	threads := []ReviewThread{}
	for _, thread := range c.Threads {
		if thread.Comments.Nodes == nil {
			threads = append(threads, thread)
			continue
		}
		original := *thread.Comments.Nodes
		rows := filter(original, func(row json.RawMessage) bool {
			var value struct {
				ID int64 `json:"databaseId"`
			}
			_ = json.Unmarshal(row, &value)
			return owned[value.ID]
		})
		if len(original) > 0 && len(rows) == 0 && thread.IsResolved != nil && !*thread.IsResolved && thread.IsOutdated != nil && !*thread.IsOutdated {
			continue
		}
		thread.Comments.Nodes = &rows
		threads = append(threads, thread)
	}
	c.Threads = threads
	c.Digest = ""
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	c.Digest = hex.EncodeToString(sum[:])
	return c
}
