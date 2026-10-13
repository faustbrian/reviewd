package github

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/faustbrian/reviewd/internal/source"
)

// Source is a privileged acquisition operation owned by the application. Its
// credential never enters the snapshot or the private reviewer's environment.
func (c *Client) Source(ctx context.Context, pull Pull, tempRoot string) (*source.Snapshot, error) {
	canonical, _, err := c.repositoryScope(pull.Repository)
	if err != nil || canonical != pull.Repository || pull.ID <= 0 || pull.RepositoryID <= 0 {
		return nil, errors.New("source acquisition requires captured pull-request identity on the authenticated host")
	}
	credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + c.token))
	snapshot, err := source.Acquire(ctx, source.Options{RemoteURL: "https://" + canonical + ".git", Authorization: "Authorization: Basic " + credential, TempRoot: tempRoot}, source.Boundary{TargetBaseSHA: pull.TargetBaseSHA, HeadSHA: pull.HeadSHA, Number: pull.Number, ChangedFiles: pull.ChangedFiles})
	if err != nil {
		return nil, err
	}
	current, err := c.Pull(ctx, canonical, pull.Number)
	if err != nil || !SameReviewContext(pull, current) {
		cleanup := snapshot.Close()
		if err == nil {
			err = errors.New("pull-request identity, revision or controlling context changed during source acquisition; review the current boundary")
		}
		return nil, errors.Join(err, cleanup)
	}
	return snapshot, nil
}

// SameReviewContext deliberately excludes observation timestamps: a check-only
// update must not invalidate still-valid source evidence.
func SameReviewContext(a, b Pull) bool {
	return a.PullReference == b.PullReference && a.RepositoryID == b.RepositoryID && a.ID == b.ID && a.State == b.State && a.Draft == b.Draft && a.Author == b.Author && a.Title == b.Title && a.Body == b.Body && a.TargetBaseSHA == b.TargetBaseSHA && a.HeadSHA == b.HeadSHA && a.BaseRef == b.BaseRef && a.HeadRef == b.HeadRef && a.ChangedFiles == b.ChangedFiles && a.CommitCount == b.CommitCount
}
