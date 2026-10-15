package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// FeedbackTarget is a separately acquired modifying boundary. Adding it does
// not change frozen private-review Pull JSON or reinterpret old payload hashes.
type FeedbackTarget struct {
	Pull             Pull   `json:"pull"`
	HeadRepository   string `json:"head_repository"`
	HeadRepositoryID int64  `json:"head_repository_id"`
}

const feedbackTargetQuery = `query ReviewdFeedbackTarget($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){databaseId number state merged mergeQueueEntry{id} headRefOid baseRefOid headRepository{databaseId nameWithOwner}}}}`

func (c *Client) FeedbackTarget(ctx context.Context, repository string, number int) (FeedbackTarget, error) {
	pull, err := c.Pull(ctx, repository, number)
	if err != nil {
		return FeedbackTarget{}, err
	}
	if pull.State != "open" {
		return FeedbackTarget{}, errors.New("feedback cannot modify a closed or merged PR")
	}
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil {
		return FeedbackTarget{}, err
	}
	var detail struct {
		ID     int64 `json:"id"`
		Number int   `json:"number"`
		Merged *bool `json:"merged"`
		Head   struct {
			SHA, Ref string
			Repo     *struct {
				ID       int64
				FullName string `json:"full_name"`
			}
		} `json:"head"`
	}
	if err = c.get(ctx, prefix+"/pulls/"+strconv.Itoa(number), &detail); err != nil {
		return FeedbackTarget{}, err
	}
	if detail.ID != pull.ID || detail.Number != number || detail.Merged == nil || *detail.Merged || detail.Head.SHA != pull.HeadSHA || detail.Head.Ref != pull.HeadRef || detail.Head.Repo == nil || detail.Head.Repo.ID <= 0 {
		return FeedbackTarget{}, errors.New("feedback requires the current open PR and exact existing head repository/branch")
	}
	head, headPrefix, err := c.repositoryScope(detail.Head.Repo.FullName)
	if err != nil {
		return FeedbackTarget{}, err
	}
	var access struct {
		ID            int64
		Name          string `json:"full_name"`
		Archived      *bool
		Disabled      *bool
		DefaultBranch string `json:"default_branch"`
		Permissions   *struct{ Push *bool }
	}
	if err = c.get(ctx, headPrefix, &access); err != nil {
		return FeedbackTarget{}, err
	}
	if access.ID != detail.Head.Repo.ID || !strings.EqualFold(access.Name, strings.TrimPrefix(head, c.host+"/")) || access.Archived == nil || access.Disabled == nil || *access.Archived || *access.Disabled || access.Permissions == nil || access.Permissions.Push == nil || !*access.Permissions.Push || access.DefaultBranch == "" {
		return FeedbackTarget{}, errors.New("feedback requires verified write access to the exact active head repository")
	}
	if pull.HeadRef == access.DefaultBranch || pull.HeadRef == "main" || pull.HeadRef == "develop" {
		return FeedbackTarget{}, errors.New("feedback will not automatically push a default/main/develop branch; an explicit protected operation is required")
	}
	var branch struct {
		Name      string
		Protected *bool
	}
	if err = c.get(ctx, headPrefix+"/branches/"+url.PathEscape(pull.HeadRef), &branch); err != nil {
		return FeedbackTarget{}, err
	}
	if branch.Name != pull.HeadRef || branch.Protected == nil || *branch.Protected {
		return FeedbackTarget{}, errors.New("feedback head branch is protected or its protection state is unavailable")
	}
	var rules []json.RawMessage
	if err = c.get(ctx, headPrefix+"/rules/branches/"+url.PathEscape(pull.HeadRef), &rules); err != nil {
		return FeedbackTarget{}, err
	}
	if rules == nil || len(rules) != 0 {
		return FeedbackTarget{}, errors.New("feedback head branch has rules requiring an explicit operator decision")
	}
	parts := strings.Split(pull.Repository, "/")
	var data struct {
		Repository struct {
			Pull *struct {
				ID         int64 `json:"databaseId"`
				Number     int
				State      string
				Merged     *bool
				Queue      json.RawMessage `json:"mergeQueueEntry"`
				Head       string          `json:"headRefOid"`
				Base       string          `json:"baseRefOid"`
				Repository *struct {
					ID   int64  `json:"databaseId"`
					Name string `json:"nameWithOwner"`
				} `json:"headRepository"`
			} `json:"pullRequest"`
		}
	}
	if err = c.query(ctx, feedbackTargetQuery, map[string]any{"owner": parts[1], "name": parts[2], "number": number}, &data); err != nil {
		return FeedbackTarget{}, err
	}
	current := data.Repository.Pull
	if current == nil || current.ID != pull.ID || current.Number != number || current.State != "OPEN" || current.Merged == nil || *current.Merged || string(current.Queue) != "null" || current.Head != pull.HeadSHA || current.Base != pull.TargetBaseSHA || current.Repository == nil || current.Repository.ID != access.ID || !strings.EqualFold(current.Repository.Name, access.Name) {
		return FeedbackTarget{}, errors.New("feedback target changed, merged, entered the merge queue or omitted eligibility evidence")
	}
	return FeedbackTarget{Pull: pull, HeadRepository: head, HeadRepositoryID: access.ID}, nil
}
