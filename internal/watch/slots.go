//go:build darwin || linux

package watch

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func AcquireSlot(ctx context.Context, directory string, limit int) (*ProfileLock, error) {
	if limit < 1 || limit > 16 {
		return nil, errors.New("global review concurrency must be between 1 and 16")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lease, err := trySlot(directory, limit)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, errLeaseBusy) {
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func trySlot(directory string, limit int) (*ProfileLock, error) {
	admission, err := lockFile(directory, "review-admission.lock")
	if err != nil {
		return nil, err
	}
	defer admission.Close()
	var available *ProfileLock
	occupied := 0
	for index := 0; index < 16; index++ {
		slot, err := lockFile(directory, fmt.Sprintf("review-slot-%02d.lock", index))
		if errors.Is(err, errLeaseBusy) {
			occupied++
			continue
		}
		if err != nil {
			if available != nil {
				available.Close()
			}
			return nil, err
		}
		if available == nil {
			available = slot
		} else {
			slot.Close()
		}
	}
	if occupied >= limit || available == nil {
		if available != nil {
			available.Close()
		}
		return nil, errLeaseBusy
	}
	return available, nil
}
