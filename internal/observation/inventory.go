// Package observation identifies new human feedback for event coalescing.
// It does not interpret feedback or execute a response workflow.
package observation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/github"
)

type Item struct {
	Key         string          `json:"key"`
	Kind        string          `json:"kind"`
	ID          int64           `json:"id"`
	NodeID      string          `json:"node_id,omitempty"`
	ThreadID    string          `json:"thread_id,omitempty"`
	ReviewID    int64           `json:"review_id,omitempty"`
	ReviewState string          `json:"review_state,omitempty"`
	Author      github.Identity `json:"author"`
	Body        string          `json:"body"`
	URL         string          `json:"url,omitempty"`
	CreatedAt   time.Time       `json:"created_at,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at,omitempty"`
	Revision    string          `json:"revision"`
}

type Thread struct {
	ID       string   `json:"id"`
	Resolved bool     `json:"resolved"`
	Outdated bool     `json:"outdated"`
	ItemKeys []string `json:"item_keys"`
}

type Inventory struct {
	Items   []Item   `json:"items"`
	Threads []Thread `json:"threads"`
	Digest  string   `json:"digest"`
}

func fingerprint(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Collect coalesces substantive submitted review bodies, inline feedback and
// PR discussion. REST and GraphQL describe the same inline comment, not two
// requests. Thread states remain available for delivery decisions but do not
// manufacture new human feedback when our own fix moves lines or resolves it.
// Own contributions are removed individually, never by dropping a mixed thread.
func Collect(remote github.ReviewContext, actor github.Identity) (result Inventory, err error) {
	if actor.ID <= 0 || actor.Login == "" || len(remote.Gaps) != 0 {
		return Inventory{}, errors.New("feedback requires an authenticated actor and complete paginated context")
	}
	defer func() {
		if err != nil {
			result = Inventory{}
		}
	}()
	items := map[string]Item{}
	add := func(item Item) error {
		if item.ID <= 0 {
			return errors.New("feedback item identity is incomplete")
		}
		if item.Author.ID == actor.ID || (item.Author.Login != "" && strings.EqualFold(item.Author.Login, actor.Login)) || item.ReviewState == "PENDING" || strings.TrimSpace(item.Body) == "" {
			return nil
		}
		item.Author.Login = strings.ToLower(item.Author.Login)
		item.Key = fmt.Sprintf("%s:%d", item.Kind, item.ID)
		if previous, exists := items[item.Key]; exists {
			if previous.Body != item.Body || (previous.Author.ID != 0 && item.Author.ID != 0 && previous.Author.ID != item.Author.ID) || (previous.Author.Login != "" && item.Author.Login != "" && previous.Author.Login != item.Author.Login) || (previous.ReviewID != 0 && item.ReviewID != 0 && previous.ReviewID != item.ReviewID) || (previous.NodeID != "" && item.NodeID != "" && previous.NodeID != item.NodeID) || (previous.ThreadID != "" && item.ThreadID != "" && previous.ThreadID != item.ThreadID) || (!previous.CreatedAt.IsZero() && !item.CreatedAt.IsZero() && !previous.CreatedAt.Equal(item.CreatedAt)) || (!previous.UpdatedAt.IsZero() && !item.UpdatedAt.IsZero() && !previous.UpdatedAt.Equal(item.UpdatedAt)) {
				return errors.New("feedback changed between REST and GraphQL reads; synchronize a complete current inventory")
			}
			if item.Author.ID != 0 {
				previous.Author.ID = item.Author.ID
			}
			if item.Author.Login != "" {
				previous.Author.Login = item.Author.Login
			}
			if item.NodeID != "" {
				previous.NodeID = item.NodeID
			}
			if item.ThreadID != "" {
				previous.ThreadID = item.ThreadID
			}
			if item.ReviewID != 0 {
				previous.ReviewID = item.ReviewID
			}
			if item.URL != "" {
				previous.URL = item.URL
			}
			if !item.CreatedAt.IsZero() {
				previous.CreatedAt = item.CreatedAt
			}
			if !item.UpdatedAt.IsZero() {
				previous.UpdatedAt = item.UpdatedAt
			}
			item = previous
		}
		items[item.Key] = item
		return nil
	}
	for _, source := range []struct {
		kind string
		rows []json.RawMessage
	}{{"review", remote.Reviews}, {"inline", remote.Comments}, {"discussion", remote.Conversation}} {
		for _, raw := range source.rows {
			var row struct {
				ID        int64            `json:"id"`
				NodeID    string           `json:"node_id"`
				User      *github.Identity `json:"user"`
				Body      string           `json:"body"`
				State     string           `json:"state"`
				URL       string           `json:"html_url"`
				ReviewID  int64            `json:"pull_request_review_id"`
				Created   time.Time        `json:"created_at"`
				Updated   time.Time        `json:"updated_at"`
				Submitted time.Time        `json:"submitted_at"`
			}
			if json.Unmarshal(raw, &row) != nil {
				return result, errors.New("feedback item could not be decoded")
			}
			author := github.Identity{}
			if row.User != nil {
				author = *row.User
			}
			if row.Created.IsZero() {
				row.Created = row.Submitted
			}
			if err = add(Item{Kind: source.kind, ID: row.ID, NodeID: row.NodeID, ReviewID: row.ReviewID, Author: author, Body: row.Body, ReviewState: row.State, URL: row.URL, CreatedAt: row.Created, UpdatedAt: row.Updated}); err != nil {
				return result, err
			}
		}
	}
	seenThreads := map[string]bool{}
	result.Threads = []Thread{}
	for _, thread := range remote.Threads {
		if thread.ID == "" || seenThreads[thread.ID] || thread.IsResolved == nil || thread.IsOutdated == nil || thread.Comments.Nodes == nil {
			return result, errors.New("feedback thread identity or paginated state is incomplete")
		}
		seenThreads[thread.ID] = true
		captured := Thread{ID: thread.ID, Resolved: *thread.IsResolved, Outdated: *thread.IsOutdated, ItemKeys: []string{}}
		for _, raw := range *thread.Comments.Nodes {
			var row struct {
				ID         string `json:"id"`
				DatabaseID int64  `json:"databaseId"`
				Author     *struct {
					Login string `json:"login"`
				} `json:"author"`
				Body    string    `json:"body"`
				URL     string    `json:"url"`
				Created time.Time `json:"createdAt"`
				Updated time.Time `json:"updatedAt"`
				Review  *struct {
					ID int64 `json:"databaseId"`
				} `json:"pullRequestReview"`
			}
			if json.Unmarshal(raw, &row) != nil || row.ID == "" {
				return result, errors.New("feedback thread comment identity is incomplete")
			}
			author, reviewID := github.Identity{}, int64(0)
			if row.Author != nil {
				author.Login = row.Author.Login
			}
			if row.Review != nil {
				reviewID = row.Review.ID
			}
			item := Item{Kind: "inline", ID: row.DatabaseID, NodeID: row.ID, ThreadID: thread.ID, ReviewID: reviewID, Author: author, Body: row.Body, URL: row.URL, CreatedAt: row.Created, UpdatedAt: row.Updated}
			if err = add(item); err != nil {
				return result, err
			}
			key := fmt.Sprintf("inline:%d", row.DatabaseID)
			if existing, ok := items[key]; ok && existing.ThreadID == thread.ID {
				captured.ItemKeys = append(captured.ItemKeys, key)
			}
		}
		sort.Strings(captured.ItemKeys)
		result.Threads = append(result.Threads, captured)
	}
	result.Items = []Item{}
	for _, item := range items {
		// Delivery-only fields are intentionally outside the human-content
		// revision: API URL/view differences and our own thread resolution or
		// line movement cannot create another automatic fixing request. Approval
		// dismissal can also be an automatic effect of a push/base change; keep
		// its current state as evidence, not as new human-content identity.
		item.CreatedAt, item.UpdatedAt = item.CreatedAt.UTC(), item.UpdatedAt.UTC()
		item.Revision = fingerprint(struct {
			Key, Body        string
			Created, Updated time.Time
		}{item.Key, item.Body, item.CreatedAt, item.UpdatedAt})
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Key < result.Items[j].Key })
	sort.Slice(result.Threads, func(i, j int) bool { return result.Threads[i].ID < result.Threads[j].ID })
	revisions := []string{}
	for _, item := range result.Items {
		revisions = append(revisions, item.Revision)
	}
	result.Digest = fingerprint(revisions)
	return result, nil
}
