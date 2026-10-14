package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ReviewComment struct {
	Path string `json:"path"`
	Side string `json:"side"`
	Line int    `json:"line"`
	Body string `json:"body"`
}
type ReviewSubmission struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments"`
}
type RemoteReview struct {
	ID          int64      `json:"id"`
	User        Identity   `json:"user"`
	CommitID    string     `json:"commit_id"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	SubmittedAt *time.Time `json:"submitted_at"`
	URL         string     `json:"html_url"`
}
type RemoteComment struct {
	ID               int64    `json:"id"`
	ReviewID         int64    `json:"pull_request_review_id"`
	User             Identity `json:"user"`
	CommitID         string   `json:"commit_id"`
	OriginalCommitID string   `json:"original_commit_id"`
	Path             string   `json:"path"`
	Side             string   `json:"side"`
	Line             *int     `json:"line"`
	OriginalLine     *int     `json:"original_line"`
	Body             string   `json:"body"`
	URL              string   `json:"html_url"`
	ReplyTo          int64    `json:"in_reply_to_id"`
}
type PublicationHistory struct {
	Reviews  []RemoteReview
	Comments []RemoteComment
}

func (c *Client) pullPath(pull Pull) (string, error) {
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil || pull.Number < 1 || pull.ID <= 0 || pull.RepositoryID <= 0 {
		return "", errors.New("publication requires a validated pull-request scope")
	}
	return prefix + "/pulls/" + strconv.Itoa(pull.Number), nil
}

func (c *Client) PublicationHistory(ctx context.Context, pull Pull) (PublicationHistory, error) {
	prefix, err := c.pullPath(pull)
	if err != nil {
		return PublicationHistory{}, err
	}
	reviews, err := c.rawPages(ctx, prefix+"/reviews")
	if err != nil {
		return PublicationHistory{}, err
	}
	comments, err := c.rawPages(ctx, prefix+"/comments")
	if err != nil {
		return PublicationHistory{}, err
	}
	history := PublicationHistory{Reviews: []RemoteReview{}, Comments: []RemoteComment{}}
	for _, data := range reviews {
		var row RemoteReview
		if json.Unmarshal(data, &row) != nil || row.ID <= 0 || row.User.ID <= 0 || row.User.Login == "" || !objectPattern.MatchString(row.CommitID) {
			return history, errors.New("GitHub review history is incomplete; publication is unavailable")
		}
		history.Reviews = append(history.Reviews, row)
	}
	for _, data := range comments {
		var row RemoteComment
		if json.Unmarshal(data, &row) != nil || row.ID <= 0 || row.User.ID <= 0 || row.User.Login == "" || !objectPattern.MatchString(row.OriginalCommitID) || row.Path == "" {
			return history, errors.New("GitHub inline history is incomplete; publication is unavailable")
		}
		history.Comments = append(history.Comments, row)
	}
	return history, nil
}

func (c *Client) Review(ctx context.Context, pull Pull, id int64) (RemoteReview, error) {
	prefix, err := c.pullPath(pull)
	if err != nil || id <= 0 {
		return RemoteReview{}, errors.New("review readback requires its exact remote identity")
	}
	var row RemoteReview
	err = c.get(ctx, prefix+"/reviews/"+strconv.FormatInt(id, 10), &row)
	return row, err
}

func (c *Client) ReviewComments(ctx context.Context, pull Pull, id int64) ([]RemoteComment, error) {
	prefix, err := c.pullPath(pull)
	if err != nil || id <= 0 {
		return nil, errors.New("comment readback requires its exact review identity")
	}
	rows, err := c.rawPages(ctx, prefix+"/reviews/"+strconv.FormatInt(id, 10)+"/comments")
	if err != nil {
		return nil, err
	}
	comments := []RemoteComment{}
	for _, data := range rows {
		var row RemoteComment
		if json.Unmarshal(data, &row) != nil || row.ID <= 0 || row.ReviewID != id {
			return nil, errors.New("GitHub returned incomplete or mismatched review comments")
		}
		comments = append(comments, row)
	}
	return comments, nil
}

// CreateReview is only called by the application publication owner. It never
// retries or follows redirects: a failed response may conceal a completed write.
func (c *Client) CreateReview(ctx context.Context, pull Pull, submission ReviewSubmission) (RemoteReview, error) {
	prefix, err := c.pullPath(pull)
	if err != nil {
		return RemoteReview{}, err
	}
	if !objectPattern.MatchString(submission.CommitID) || submission.CommitID != pull.HeadSHA || strings.TrimSpace(submission.Body) == "" || (submission.Event != "APPROVE" && submission.Event != "REQUEST_CHANGES" && submission.Event != "COMMENT") {
		return RemoteReview{}, errors.New("publication requires an explicit reviewed commit, event and nonempty body")
	}
	for _, comment := range submission.Comments {
		if comment.Path == "" || comment.Line < 1 || (comment.Side != "LEFT" && comment.Side != "RIGHT") || strings.TrimSpace(comment.Body) == "" {
			return RemoteReview{}, errors.New("publication contains an unusable inline finding")
		}
	}
	var row RemoteReview
	err = c.postPublication(ctx, prefix+"/reviews", submission, &row)
	if err == nil && row.ID <= 0 {
		err = errors.New("GitHub publication response was incomplete; reconcile before another write")
	}
	return row, err
}

func (c *Client) Comment(ctx context.Context, pull Pull, id int64) (RemoteComment, error) {
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil || id <= 0 {
		return RemoteComment{}, errors.New("inline readback requires its exact identity")
	}
	var row RemoteComment
	err = c.get(ctx, prefix+"/pulls/comments/"+strconv.FormatInt(id, 10), &row)
	return row, err
}

func (c *Client) CreateComment(ctx context.Context, pull Pull, comment ReviewComment) (RemoteComment, error) {
	prefix, err := c.pullPath(pull)
	if err != nil {
		return RemoteComment{}, err
	}
	if !objectPattern.MatchString(pull.HeadSHA) || comment.Path == "" || comment.Line < 1 || (comment.Side != "LEFT" && comment.Side != "RIGHT") || strings.TrimSpace(comment.Body) == "" {
		return RemoteComment{}, errors.New("repair requires an explicit immutable inline anchor")
	}
	submission := struct {
		CommitID string `json:"commit_id"`
		ReviewComment
	}{pull.HeadSHA, comment}
	var row RemoteComment
	err = c.postPublication(ctx, prefix+"/comments", submission, &row)
	if err == nil && row.ID <= 0 {
		err = errors.New("GitHub inline response was incomplete; reconcile before another write")
	}
	return row, err
}

func (c *Client) postPublication(ctx context.Context, path string, submission any, target any) error {
	payload, err := json.Marshal(submission)
	if err != nil {
		return errors.New("publication payload could not be encoded")
	}
	endpoint := strings.TrimRight(c.endpoint.String(), "/") + "/" + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid publication request")
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
		return errors.New("GitHub publication response was unavailable; reconcile before another write")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return remoteHTTPError(response, time.Now())
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 || json.Unmarshal(data, target) != nil {
		return errors.New("GitHub publication response was incomplete; reconcile before another write")
	}
	return nil
}
