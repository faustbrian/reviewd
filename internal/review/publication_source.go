package review

import (
	"context"
	"errors"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/source"
)

// Publication preflight has its own resource scope, not the completed review's
// owner. Recovering this scope must never change consent or replay publication.
func (p Publisher) acquireSource(ctx context.Context, pull github.Pull) (*source.Snapshot, func() error, error) {
	owner, err := p.Store.BeginPrivateOwner(ctx)
	if err != nil {
		return nil, nil, err
	}
	acquire := p.Source
	if acquire == nil {
		acquire = func(ctx context.Context, pull github.Pull) (*source.Snapshot, error) {
			return p.GitHub.Source(ctx, pull, "")
		}
	}
	snapshot, err := acquire(owner.Context(ctx), pull)
	closeSource := func() error {
		// This also handles acquisition returning no Snapshot after uncertain
		// child shutdown: the registered root remains available to recover.
		if err := owner.Close(); err != nil {
			return err
		}
		if snapshot != nil {
			return snapshot.Close()
		}
		return nil
	}
	if err != nil {
		return nil, nil, errors.Join(err, closeSource())
	}
	if snapshot == nil {
		return nil, nil, errors.Join(errors.New("source acquisition returned no snapshot"), closeSource())
	}
	return snapshot, closeSource, nil
}
