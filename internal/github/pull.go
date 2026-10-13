package github

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
)

// PullReference is application-owned scope, not a server-provided clone URL.
type PullReference struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	URL        string `json:"url"`
}

// ParsePullURL accepts only a PR on the configured host. Known GitHub views
// and fragment anchors do not widen scope or enter authenticated requests.
func ParsePullURL(host, input string) (PullReference, error) {
	invalid := errors.New("use an HTTPS pull-request URL on the configured GitHub host without credentials, query parameters or a port")
	if _, err := config.Repository(host, "reviewd/validation"); err != nil {
		return PullReference{}, invalid
	}
	parsed, err := url.Parse(input)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.RawPath != "" || !strings.EqualFold(parsed.Host, host) {
		return PullReference{}, invalid
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) == 5 && (parts[4] == "files" || parts[4] == "commits") {
		parts = parts[:4]
	}
	if len(parts) != 4 || parts[2] != "pull" || parts[3] == "" {
		return PullReference{}, invalid
	}
	for _, char := range parts[3] {
		if char < '0' || char > '9' {
			return PullReference{}, invalid
		}
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return PullReference{}, invalid
	}
	repository, err := config.Repository(strings.ToLower(host), parts[0]+"/"+parts[1])
	if err != nil {
		return PullReference{}, invalid
	}
	canonicalURL := "https://" + repository + "/pull/" + strconv.Itoa(number)
	return PullReference{Repository: repository, Number: number, URL: canonicalURL}, nil
}

// Pull records target-base and head separately. A complete source acquisition
// must additionally determine the merge base before comparing changed objects.
type Pull struct {
	PullReference
	RepositoryID  int64     `json:"repository_id"`
	ID            int64     `json:"id"`
	State         string    `json:"state"`
	Draft         bool      `json:"draft"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	UpdatedAt     time.Time `json:"updated_at"`
	Author        Identity  `json:"author"`
	BaseRef       string    `json:"base_ref"`
	HeadRef       string    `json:"head_ref"`
	TargetBaseSHA string    `json:"target_base_sha"`
	HeadSHA       string    `json:"head_sha"`
	ChangedFiles  int       `json:"changed_files"`
	CommitCount   int       `json:"commit_count"`
}

func (c *Client) repositoryScope(repository string) (canonical, prefix string, err error) {
	canonical, err = config.Repository(c.host, repository)
	if err != nil || !strings.HasPrefix(canonical, c.host+"/") {
		return "", "", errors.New("repository host does not match the authenticated GitHub host")
	}
	parts := strings.Split(canonical, "/")
	return canonical, "repos/" + url.PathEscape(parts[1]) + "/" + url.PathEscape(parts[2]), nil
}

func (c *Client) Pull(ctx context.Context, repository string, number int) (Pull, error) {
	canonical, prefix, err := c.repositoryScope(repository)
	if err != nil {
		return Pull{}, err
	}
	if number < 1 {
		return Pull{}, errors.New("pull-request number must be positive")
	}
	var metadata struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := c.get(ctx, prefix, &metadata); err != nil {
		return Pull{}, err
	}
	relative := strings.TrimPrefix(canonical, c.host+"/")
	if metadata.ID <= 0 || strings.ToLower(metadata.FullName) != relative {
		return Pull{}, errors.New("repository identity changed; update the configured repository scope")
	}
	type branch struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	}
	var evidence struct {
		ID           int64     `json:"id"`
		Number       int       `json:"number"`
		State        string    `json:"state"`
		Draft        *bool     `json:"draft"`
		Title        *string   `json:"title"`
		Body         *string   `json:"body"`
		UpdatedAt    time.Time `json:"updated_at"`
		Author       Identity  `json:"user"`
		Base         branch    `json:"base"`
		Head         branch    `json:"head"`
		ChangedFiles *int      `json:"changed_files"`
		CommitCount  *int      `json:"commits"`
	}
	if err := c.get(ctx, prefix+"/pulls/"+strconv.Itoa(number), &evidence); err != nil {
		return Pull{}, err
	}
	if evidence.ID <= 0 || evidence.Number != number || evidence.Base.Repo == nil || evidence.Base.Repo.ID != metadata.ID || strings.ToLower(evidence.Base.Repo.FullName) != relative {
		return Pull{}, errors.New("GitHub returned mismatched pull-request or target repository identity")
	}
	if !objectPattern.MatchString(evidence.Base.SHA) || !objectPattern.MatchString(evidence.Head.SHA) || evidence.Base.Ref == "" || evidence.Head.Ref == "" {
		return Pull{}, errors.New("GitHub returned no immutable pull-request boundary")
	}
	if evidence.Draft == nil || evidence.Title == nil || evidence.UpdatedAt.IsZero() || evidence.Author.ID <= 0 || strings.TrimSpace(evidence.Author.Login) == "" || (evidence.State != "open" && evidence.State != "closed") || evidence.ChangedFiles == nil || *evidence.ChangedFiles < 0 || evidence.CommitCount == nil || *evidence.CommitCount < 0 {
		return Pull{}, errors.New("GitHub returned incomplete pull-request context or comparison counts")
	}
	body := ""
	if evidence.Body != nil {
		body = *evidence.Body
	}
	return Pull{
		PullReference: PullReference{Repository: canonical, Number: number, URL: "https://" + canonical + "/pull/" + strconv.Itoa(number)},
		RepositoryID:  metadata.ID, ID: evidence.ID, State: evidence.State, Draft: *evidence.Draft,
		Title: *evidence.Title, Body: body, UpdatedAt: evidence.UpdatedAt, Author: evidence.Author,
		BaseRef: evidence.Base.Ref, HeadRef: evidence.Head.Ref, TargetBaseSHA: evidence.Base.SHA, HeadSHA: evidence.Head.SHA,
		ChangedFiles: *evidence.ChangedFiles, CommitCount: *evidence.CommitCount,
	}, nil
}
