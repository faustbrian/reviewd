package lifetime

import (
	"errors"

	"golang.org/x/sys/unix"
)

// WaitExited observes our exclusive, unreaped child. WNOWAIT reserves its PID
// while the caller finishes signalling the process group it created.
func WaitExited(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
