package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// These fixed adapters belong only to the separate feedback delivery owner.
// They do not retry or confer policy authority. The owner journals before an
// attempt and verifies canonical state before acknowledging/retrying it.
type FeedbackReply struct {
	ID     int64    `json:"id"`
	NodeID string   `json:"node_id,omitempty"`
	Body   string   `json:"body"`
	URL    string   `json:"html_url"`
	User   Identity `json:"user"`
}

func feedbackText(text string) bool {
	return len(text) > 0 && len(text) <= 65536 && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func (c *Client) feedbackWrite(ctx context.Context, method string, endpoint url.URL, payload any, result any, headers ...*http.Header) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("feedback payload could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid scoped feedback request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "reviewd")
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return errors.New("feedback response was unavailable; reconcile canonical remote state before retrying")
	}
	defer response.Body.Close()
	if len(headers) > 0 {
		*headers[0] = response.Header.Clone()
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return remoteHTTPError(response, time.Now())
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil || len(data) > 8<<20 || json.Unmarshal(data, result) != nil {
		return errors.New("feedback response was incomplete; reconcile canonical remote state before retrying")
	}
	return nil
}
func (c *Client) feedbackREST(ctx context.Context, method, path string, payload, result any) error {
	endpoint := *c.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + path
	return c.feedbackWrite(ctx, method, endpoint, payload, result)
}
func (c *Client) feedbackGraphQL(ctx context.Context, query string, variables any, result any) error {
	endpoint := *c.endpoint
	if strings.HasSuffix(endpoint.Path, "/api/v3") {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/api/v3") + "/api/graphql"
	} else {
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/graphql"
	}
	var response struct {
		Data   json.RawMessage
		Errors []json.RawMessage
	}
	var headers http.Header
	if err := c.feedbackWrite(ctx, http.MethodPost, endpoint, map[string]any{"query": query, "variables": variables}, &response, &headers); err != nil {
		return err
	}
	if len(response.Errors) > 0 && (rateLimitErrors(response.Errors) || headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") == "0") {
		limited := remoteHTTPError(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers}, time.Now())
		limited.Status = http.StatusOK
		return limited
	}
	if len(response.Errors) != 0 || len(response.Data) == 0 || string(response.Data) == "null" || json.Unmarshal(response.Data, result) != nil {
		return errors.New("feedback mutation response was incomplete or denied; reconcile before retrying")
	}
	return nil
}

func (c *Client) UpdateFeedbackMetadata(ctx context.Context, pull Pull, title, body string) error {
	path, err := c.pullPath(pull)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" || len(title) > 256 || strings.ContainsAny(title, "\r\n\x00") || !feedbackText(body) {
		return errors.New("feedback metadata requires a complete bounded title and body")
	}
	var result map[string]json.RawMessage
	return c.feedbackREST(ctx, http.MethodPatch, path, map[string]string{"title": title, "body": body}, &result)
}
func (c *Client) ReplyToFeedbackDiscussion(ctx context.Context, pull Pull, body string) (FeedbackReply, error) {
	if _, err := c.pullPath(pull); err != nil {
		return FeedbackReply{}, err
	}
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil || !feedbackText(body) {
		return FeedbackReply{}, errors.New("feedback reply requires scoped nonempty text")
	}
	var result FeedbackReply
	err = c.feedbackREST(ctx, http.MethodPost, prefix+"/issues/"+strconv.Itoa(pull.Number)+"/comments", map[string]string{"body": body}, &result)
	if err == nil && (result.ID <= 0 || result.URL == "") {
		err = errors.New("feedback reply response omitted identity; reconcile before retrying")
	}
	return result, err
}

// Thread IDs are global node identities. Prove membership independently of
// the caller's syntactically valid PR before either thread mutation.
func (c *Client) feedbackThreadScope(ctx context.Context, pull Pull, thread string) error {
	const query = `query ReviewdFeedbackThreadScope($thread:ID!){node(id:$thread){... on PullRequestReviewThread{id pullRequest{databaseId number repository{databaseId nameWithOwner}}}}}`
	var data struct {
		Node *struct {
			ID   string
			Pull struct {
				ID         int64 `json:"databaseId"`
				Number     int
				Repository struct {
					ID   int64  `json:"databaseId"`
					Name string `json:"nameWithOwner"`
				}
			} `json:"pullRequest"`
		}
	}
	if err := c.query(ctx, query, map[string]any{"thread": thread}, &data); err != nil {
		return err
	}
	if data.Node == nil || data.Node.ID != thread || data.Node.Pull.ID != pull.ID || data.Node.Pull.Number != pull.Number || data.Node.Pull.Repository.ID != pull.RepositoryID || !strings.EqualFold(data.Node.Pull.Repository.Name, strings.TrimPrefix(pull.Repository, c.host+"/")) {
		return errors.New("feedback thread does not belong to the exact captured PR")
	}
	return nil
}

func (c *Client) ReplyToFeedbackThread(ctx context.Context, pull Pull, thread, body string) (FeedbackReply, error) {
	if _, err := c.pullPath(pull); err != nil {
		return FeedbackReply{}, err
	}
	if thread == "" || len(thread) > 512 || !feedbackText(body) {
		return FeedbackReply{}, errors.New("feedback reply requires its original thread and bounded text")
	}
	if err := c.feedbackThreadScope(ctx, pull, thread); err != nil {
		return FeedbackReply{}, err
	}
	const mutation = `mutation ReviewdFeedbackReply($thread:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$thread,body:$body}){comment{id databaseId body url}}}`
	var response struct {
		Reply struct {
			Comment struct {
				ID         string
				DatabaseID int64 `json:"databaseId"`
				Body, URL  string
			}
		} `json:"addPullRequestReviewThreadReply"`
	}
	err := c.feedbackGraphQL(ctx, mutation, map[string]string{"thread": thread, "body": body}, &response)
	row := response.Reply.Comment
	if err == nil && (row.DatabaseID <= 0 || row.ID == "" || row.URL == "") {
		err = errors.New("thread reply response omitted identity; reconcile before retrying")
	}
	return FeedbackReply{ID: row.DatabaseID, NodeID: row.ID, Body: row.Body, URL: row.URL}, err
}
func (c *Client) ResolveFeedbackThread(ctx context.Context, pull Pull, thread string) error {
	if _, err := c.pullPath(pull); err != nil {
		return err
	}
	if thread == "" || len(thread) > 512 {
		return errors.New("feedback resolution requires its captured thread identity")
	}
	if err := c.feedbackThreadScope(ctx, pull, thread); err != nil {
		return err
	}
	const mutation = `mutation ReviewdResolveFeedback($thread:ID!){resolveReviewThread(input:{threadId:$thread}){thread{id isResolved}}}`
	var response struct {
		Resolution struct {
			Thread struct {
				ID       string
				Resolved *bool `json:"isResolved"`
			}
		} `json:"resolveReviewThread"`
	}
	if err := c.feedbackGraphQL(ctx, mutation, map[string]string{"thread": thread}, &response); err != nil {
		return err
	}
	if response.Resolution.Thread.ID != thread || response.Resolution.Thread.Resolved == nil || !*response.Resolution.Thread.Resolved {
		return errors.New("thread resolution response did not confirm its exact identity/state")
	}
	return nil
}
func (c *Client) RequestFeedbackReviewer(ctx context.Context, pull Pull, login, team string) error {
	path, err := c.pullPath(pull)
	if err != nil {
		return err
	}
	if (login == "") == (team == "") || len(login) > 100 || len(team) > 100 || strings.ContainsAny(login+team, "/\\\x00\r\n") {
		return errors.New("feedback re-review requires exactly one existing user or team")
	}
	users, teams := []string{}, []string{}
	if login != "" {
		users = append(users, login)
	} else {
		teams = append(teams, team)
	}
	var response map[string]json.RawMessage
	return c.feedbackREST(ctx, http.MethodPost, path+"/requested_reviewers", map[string]any{"reviewers": users, "team_reviewers": teams}, &response)
}

type FeedbackRequests struct {
	Users []Identity      `json:"users"`
	Teams []RequestedTeam `json:"teams"`
}

func (c *Client) FeedbackReviewRequests(ctx context.Context, pull Pull) (FeedbackRequests, error) {
	path, err := c.pullPath(pull)
	if err != nil {
		return FeedbackRequests{}, err
	}
	result := FeedbackRequests{Users: []Identity{}, Teams: []RequestedTeam{}}
	users, teams := map[int64]bool{}, map[int64]bool{}
	for page := 1; page <= 200; page++ {
		var current FeedbackRequests
		headers, err := c.getPage(ctx, path+"/requested_reviewers?per_page=100&page="+strconv.Itoa(page), &current)
		if err != nil {
			return FeedbackRequests{}, err
		}
		more, err := linkedNextPage(headers.Get("Link"))
		if err != nil || current.Users == nil || current.Teams == nil {
			return FeedbackRequests{}, errors.New("requested-reviewer inventory is incomplete")
		}
		for _, user := range current.Users {
			if user.ID <= 0 || user.Login == "" || users[user.ID] {
				return FeedbackRequests{}, errors.New("requested user identity was missing or repeated")
			}
			users[user.ID] = true
			result.Users = append(result.Users, user)
		}
		for _, team := range current.Teams {
			if team.ID <= 0 || team.Slug == "" || teams[team.ID] {
				return FeedbackRequests{}, errors.New("requested team identity was missing or repeated")
			}
			teams[team.ID] = true
			result.Teams = append(result.Teams, team)
		}
		if !more && len(current.Users) < 100 && len(current.Teams) < 100 {
			return result, nil
		}
	}
	return FeedbackRequests{}, errors.New("requested-reviewer inventory exceeded its pagination bound")
}
