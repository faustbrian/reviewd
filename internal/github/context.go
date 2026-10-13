package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

type ReviewContext struct {
	Reviews      []json.RawMessage `json:"reviews"`
	Comments     []json.RawMessage `json:"comments"`
	Conversation []json.RawMessage `json:"conversation"`
	Timeline     []json.RawMessage `json:"timeline"`
	Threads      []ReviewThread    `json:"threads"`
	Requirements []json.RawMessage `json:"requirements"`
	Gaps         []string          `json:"gaps"`
	Digest       string            `json:"digest"`
}
type ReviewThread struct {
	ID         string     `json:"id"`
	IsResolved *bool      `json:"isResolved"`
	IsOutdated *bool      `json:"isOutdated"`
	Path       string     `json:"path"`
	Line       *int       `json:"line"`
	Side       string     `json:"diffSide"`
	Comments   connection `json:"comments"`
}
type pageInfo struct {
	HasNextPage *bool   `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}
type connection struct {
	Nodes    *[]json.RawMessage `json:"nodes"`
	PageInfo pageInfo           `json:"pageInfo"`
}

var pageRelation = regexp.MustCompile(`(?:^|;)\s*rel\s*=\s*"([^"]+)"`)

// The Link header is a continuation signal only. Requests always advance the
// application-owned path/page, never a remote-supplied URL or credential scope.
func linkedNextPage(header string) (bool, error) {
	more := false
	for strings.TrimSpace(header) != "" {
		header = strings.TrimSpace(header)
		if !strings.HasPrefix(header, "<") {
			return false, errors.New("remote pagination link was malformed")
		}
		end := strings.Index(header, ">")
		if end < 2 {
			return false, errors.New("remote pagination link was incomplete")
		}
		tail := header[end+1:]
		next := strings.Index(tail, "<")
		parameters := tail
		if next >= 0 {
			parameters = tail[:next]
			header = tail[next:]
		} else {
			header = ""
		}
		relation := pageRelation.FindStringSubmatch(parameters)
		if len(relation) != 2 {
			return false, errors.New("remote pagination omitted its relation")
		}
		for _, name := range strings.Fields(relation[1]) {
			if name == "next" {
				more = true
			}
		}
	}
	return more, nil
}

func (c *Client) rawPages(ctx context.Context, path string) ([]json.RawMessage, error) {
	result := []json.RawMessage{}
	seen := map[string]bool{}
	for page := 1; page <= 200; page++ {
		var items []json.RawMessage
		headers, err := c.getPage(ctx, path+"?per_page=100&page="+strconv.Itoa(page), &items)
		if err != nil {
			return nil, err
		}
		more, err := linkedNextPage(headers.Get("Link"))
		if err != nil {
			return nil, err
		}
		if items == nil {
			return nil, errors.New("remote context page was incomplete")
		}
		for _, item := range items {
			var object map[string]json.RawMessage
			if json.Unmarshal(item, &object) != nil || object == nil {
				return nil, errors.New("remote context item was not an object")
			}
			digest := sha256.Sum256(item)
			key := hex.EncodeToString(digest[:])
			if seen[key] {
				return nil, errors.New("remote context pagination repeated an item")
			}
			seen[key] = true
			result = append(result, item)
		}
		if len(items) < 100 && !more {
			return result, nil
		}
	}
	return nil, errors.New("remote context pagination exceeded its explicit bound")
}

// Context acquires complete read-only conversation, request history, resolved
// thread state and closing requirements. Access failures remain explicit gaps,
// never empty-success evidence or permission to approve.
func (c *Client) Context(ctx context.Context, pull Pull) (ReviewContext, error) {
	canonical, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil {
		return ReviewContext{}, err
	}
	if pull.ID <= 0 || pull.Number < 1 {
		return ReviewContext{}, errors.New("review context requires captured pull-request identity")
	}
	result := ReviewContext{Reviews: []json.RawMessage{}, Comments: []json.RawMessage{}, Conversation: []json.RawMessage{}, Timeline: []json.RawMessage{}, Threads: []ReviewThread{}, Requirements: []json.RawMessage{}, Gaps: []string{}}
	number := strconv.Itoa(pull.Number)
	for _, input := range []struct {
		name, path string
		target     *[]json.RawMessage
	}{{"reviews", prefix + "/pulls/" + number + "/reviews", &result.Reviews}, {"inline comments", prefix + "/pulls/" + number + "/comments", &result.Comments}, {"conversation", prefix + "/issues/" + number + "/comments", &result.Conversation}, {"request timeline", prefix + "/issues/" + number + "/timeline", &result.Timeline}} {
		items, readErr := c.rawPages(ctx, input.path)
		if readErr != nil {
			if ctx.Err() != nil {
				return ReviewContext{}, ctx.Err()
			}
			result.Gaps = append(result.Gaps, "Complete "+input.name+" could not be acquired: "+readErr.Error())
			continue
		}
		*input.target = items
	}
	parts := strings.Split(canonical, "/")
	variables := map[string]any{"owner": parts[1], "name": parts[2], "number": pull.Number, "after": nil}
	threads, readErr := c.readThreads(ctx, pull, variables)
	if readErr != nil {
		if ctx.Err() != nil {
			return ReviewContext{}, ctx.Err()
		}
		result.Gaps = append(result.Gaps, "Complete review-thread state could not be acquired: "+readErr.Error())
	} else {
		result.Threads = threads
	}
	variables["after"] = nil
	requirements, readErr := c.readRequirements(ctx, pull, variables)
	if readErr != nil {
		if ctx.Err() != nil {
			return ReviewContext{}, ctx.Err()
		}
		result.Gaps = append(result.Gaps, "Linked closing requirements could not be acquired: "+readErr.Error())
	} else {
		result.Requirements = requirements
	}
	data, _ := json.Marshal(result)
	digest := sha256.Sum256(data)
	result.Digest = hex.EncodeToString(digest[:])
	return result, nil
}

// Queries are compiled application-owned reads, not supplied by the reviewer.
const threadQuery = `query ReviewdThreads($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){databaseId number reviewThreads(first:100,after:$after){nodes{id isResolved isOutdated path line diffSide comments(first:100){nodes{id databaseId body url createdAt updatedAt author{login} pullRequestReview{databaseId}} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`
const commentQuery = `query ReviewdThreadComments($id:ID!,$after:String){node(id:$id){... on PullRequestReviewThread{id comments(first:100,after:$after){nodes{id databaseId body url createdAt updatedAt author{login} pullRequestReview{databaseId}} pageInfo{hasNextPage endCursor}}}}}`
const requirementQuery = `query ReviewdRequirements($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){databaseId number closingIssuesReferences(first:100,after:$after){nodes{id databaseId number title body url updatedAt repository{nameWithOwner}} pageInfo{hasNextPage endCursor}}}}}`

func (c *Client) query(ctx context.Context, query string, variables map[string]any, target any) error {
	data, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	endpoint := *c.endpoint
	if strings.HasSuffix(endpoint.Path, "/api/v3") {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/api/v3") + "/api/graphql"
	} else {
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/graphql"
	}
	var response struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := c.readRequest(ctx, "POST", endpoint.String(), data, &response); err != nil {
		return err
	}
	if len(response.Errors) > 0 || len(response.Data) == 0 || string(response.Data) == "null" || json.Unmarshal(response.Data, target) != nil {
		return errors.New("GitHub GraphQL read was incomplete or denied")
	}
	return nil
}
func advance(info pageInfo, seen map[string]bool) (string, bool, error) {
	if info.HasNextPage == nil {
		return "", false, errors.New("GraphQL context omitted completeness evidence")
	}
	if !*info.HasNextPage {
		return "", false, nil
	}
	if info.EndCursor == nil || *info.EndCursor == "" || seen[*info.EndCursor] {
		return "", false, errors.New("GraphQL context pagination stalled")
	}
	seen[*info.EndCursor] = true
	return *info.EndCursor, true, nil
}
func (c *Client) readThreads(ctx context.Context, pull Pull, variables map[string]any) ([]ReviewThread, error) {
	result := []ReviewThread{}
	cursors := map[string]bool{}
	ids := map[string]bool{}
	for page := 0; page < 200; page++ {
		var data struct {
			Repository struct {
				Pull *struct {
					ID      int64 `json:"databaseId"`
					Number  int   `json:"number"`
					Threads struct {
						Nodes    *[]ReviewThread `json:"nodes"`
						PageInfo pageInfo        `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.query(ctx, threadQuery, variables, &data); err != nil {
			return nil, err
		}
		item := data.Repository.Pull
		if item == nil || item.ID != pull.ID || item.Number != pull.Number || item.Threads.Nodes == nil {
			return nil, errors.New("review-thread PR identity or completeness was mismatched")
		}
		for _, thread := range *item.Threads.Nodes {
			if thread.ID == "" || ids[thread.ID] || thread.IsResolved == nil || thread.IsOutdated == nil || thread.Comments.Nodes == nil {
				return nil, errors.New("review-thread state was incomplete or repeated")
			}
			ids[thread.ID] = true
			comments, err := c.completeThreadComments(ctx, thread)
			if err != nil {
				return nil, err
			}
			thread.Comments.Nodes = &comments
			done := false
			thread.Comments.PageInfo = pageInfo{HasNextPage: &done}
			result = append(result, thread)
		}
		cursor, more, err := advance(item.Threads.PageInfo, cursors)
		if err != nil {
			return nil, err
		}
		if !more {
			return result, nil
		}
		variables["after"] = cursor
	}
	return nil, errors.New("review-thread pagination exceeded its explicit bound")
}
func (c *Client) completeThreadComments(ctx context.Context, thread ReviewThread) ([]json.RawMessage, error) {
	result := append([]json.RawMessage{}, (*thread.Comments.Nodes)...)
	info := thread.Comments.PageInfo
	cursors := map[string]bool{}
	ids := map[string]bool{}
	validate := func(items []json.RawMessage) error {
		for _, raw := range items {
			var comment struct {
				ID         string  `json:"id"`
				DatabaseID int64   `json:"databaseId"`
				Body       *string `json:"body"`
			}
			if json.Unmarshal(raw, &comment) != nil || comment.ID == "" || comment.DatabaseID <= 0 || comment.Body == nil || ids[comment.ID] {
				return errors.New("thread comments were incomplete or repeated")
			}
			ids[comment.ID] = true
		}
		return nil
	}
	if err := validate(result); err != nil {
		return nil, err
	}
	for page := 0; page < 200; page++ {
		cursor, more, err := advance(info, cursors)
		if err != nil {
			return nil, err
		}
		if !more {
			return result, nil
		}
		var data struct {
			Node *struct {
				ID       string     `json:"id"`
				Comments connection `json:"comments"`
			} `json:"node"`
		}
		if err := c.query(ctx, commentQuery, map[string]any{"id": thread.ID, "after": cursor}, &data); err != nil {
			return nil, err
		}
		if data.Node == nil || data.Node.ID != thread.ID || data.Node.Comments.Nodes == nil {
			return nil, errors.New("thread comment identity or completeness changed")
		}
		if err := validate(*data.Node.Comments.Nodes); err != nil {
			return nil, err
		}
		result = append(result, (*data.Node.Comments.Nodes)...)
		info = data.Node.Comments.PageInfo
	}
	return nil, errors.New("thread comment pagination exceeded its explicit bound")
}
func (c *Client) readRequirements(ctx context.Context, pull Pull, variables map[string]any) ([]json.RawMessage, error) {
	result := []json.RawMessage{}
	cursors := map[string]bool{}
	ids := map[string]bool{}
	for page := 0; page < 200; page++ {
		var data struct {
			Repository struct {
				Pull *struct {
					ID           int64      `json:"databaseId"`
					Number       int        `json:"number"`
					Requirements connection `json:"closingIssuesReferences"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.query(ctx, requirementQuery, variables, &data); err != nil {
			return nil, err
		}
		item := data.Repository.Pull
		if item == nil || item.ID != pull.ID || item.Number != pull.Number || item.Requirements.Nodes == nil {
			return nil, errors.New("linked-requirement PR identity or completeness was mismatched")
		}
		for _, raw := range *item.Requirements.Nodes {
			var issue struct {
				ID         string  `json:"id"`
				DatabaseID int64   `json:"databaseId"`
				Title      *string `json:"title"`
				Body       *string `json:"body"`
			}
			if json.Unmarshal(raw, &issue) != nil || issue.ID == "" || issue.DatabaseID <= 0 || issue.Title == nil || issue.Body == nil || ids[issue.ID] {
				return nil, errors.New("linked requirements were incomplete or repeated")
			}
			ids[issue.ID] = true
			result = append(result, raw)
		}
		cursor, more, err := advance(item.Requirements.PageInfo, cursors)
		if err != nil {
			return nil, err
		}
		if !more {
			return result, nil
		}
		variables["after"] = cursor
	}
	return nil, errors.New("linked-requirement pagination exceeded its explicit bound")
}
