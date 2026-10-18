package github

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/faustbrian/reviewd/internal/config"
	"net/url"
)

// PullPreferences reads only the PR's target-base configuration, never the
// proposed head. It does not acquire source or govern the launched skill.
func (c *Client) PullPreferences(ctx context.Context, pull Pull) (*config.RepositoryPreferences, error) {
	_, prefix, err := c.repositoryScope(pull.Repository)
	if err != nil {
		return nil, err
	}
	if !objectPattern.MatchString(pull.TargetBaseSHA) {
		return nil, errors.New("pull has no target-base revision")
	}
	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := c.get(ctx, prefix+"/commits/"+url.PathEscape(pull.TargetBaseSHA), &commit); err != nil {
		return nil, err
	}
	if commit.SHA != pull.TargetBaseSHA || !objectPattern.MatchString(commit.Commit.Tree.SHA) {
		return nil, errors.New("repository preferences commit does not match the target base")
	}
	return c.preferencesAtTree(ctx, prefix, commit.SHA, commit.Commit.Tree.SHA)
}

func (c *Client) preferencesAtTree(ctx context.Context, prefix, baseSHA, treeSHA string) (*config.RepositoryPreferences, error) {
	var tree struct {
		SHA       string                                    `json:"sha"`
		Truncated *bool                                     `json:"truncated"`
		Tree      *[]struct{ Path, Mode, Type, SHA string } `json:"tree"`
	}
	if err := c.get(ctx, prefix+"/git/trees/"+treeSHA, &tree); err != nil {
		return nil, err
	}
	if tree.Truncated == nil || *tree.Truncated || tree.Tree == nil || tree.SHA != treeSHA {
		return nil, errors.New("repository preference tree is incomplete or mismatched")
	}
	for _, entry := range *tree.Tree {
		if entry.Path != ".reviewd" {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || !objectPattern.MatchString(entry.SHA) {
			return nil, errors.New("trusted .reviewd must be a regular file, not a symlink or executable tool")
		}
		var blob struct {
			SHA      string `json:"sha"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
			Size     int64  `json:"size"`
		}
		if err := c.get(ctx, prefix+"/git/blobs/"+entry.SHA, &blob); err != nil {
			return nil, err
		}
		if blob.SHA != entry.SHA || blob.Encoding != "base64" || blob.Size < 0 || blob.Size > 1024*1024 {
			return nil, errors.New("trusted .reviewd blob is mismatched, oversized or has an unsupported encoding")
		}
		data, err := base64.StdEncoding.DecodeString(blob.Content)
		if err != nil || int64(len(data)) != blob.Size {
			return nil, errors.New("trusted .reviewd blob is incomplete")
		}
		return &config.RepositoryPreferences{BaseSHA: baseSHA, Data: data}, nil
	}
	return nil, nil
}
