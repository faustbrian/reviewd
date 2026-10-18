package dispatch

import (
	"context"
	"errors"
	"time"
)

// Observe the same durable run for manual clients and both watcher modes.
// This is local control, independent of GitHub polling and its rate limits.
func (s Service) observeCancellation(ctx context.Context, id string, cancel context.CancelFunc) (func() error, error) {
	requested, err := s.Store.StopRequested(ctx, id)
	if err != nil {
		return nil, err
	}
	if requested {
		cancel()
		return nil, context.Canceled
	}
	monitorCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				readCtx, end := context.WithTimeout(monitorCtx, time.Second)
				requested, err := s.Store.StopRequested(readCtx, id)
				end()
				if monitorCtx.Err() != nil {
					done <- nil
					return
				}
				if err != nil {
					cancel()
					done <- errors.New("cannot observe agent stop state; stopped rather than ignore local control")
					return
				}
				if requested {
					cancel()
					done <- nil
					return
				}
			}
		}
	}()
	return func() error { stop(); return <-done }, nil
}
