//go:build darwin || linux

package watch

import (
	"errors"
	"path/filepath"

	"github.com/faustbrian/reviewd/internal/lease"
)

// ProfileLock is a kernel-owned lease, automatically released on process exit.
// Never unlink its inode: other contenders must lock the same stable file.
type ProfileLock = lease.Lease

var errLeaseBusy = lease.ErrBusy

func LockProfile(directory string) (*ProfileLock, error) {
	lease, err := lockFile(directory, "watch.lock")
	if errors.Is(err, errLeaseBusy) {
		return nil, errors.New("another watcher owns this profile; use inbox and decision commands or choose another profile")
	}
	return lease, err
}

func lockFile(directory, name string) (*ProfileLock, error) {
	return lease.Acquire(filepath.Join(directory, name), true)
}
