package lifetime

import (
	"errors"

	"golang.org/x/sys/unix"
)

// WaitExited observes our exclusive, unreaped child without releasing its PID.
// Darwin's process filter is edge-triggered. ESRCH at registration is normal
// when this still-unreaped child has already exited; nobody else may reap it.
func WaitExited(pid int) error {
	queue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(queue)
	unix.CloseOnExec(queue)
	changes := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	for {
		_, err = unix.Kevent(queue, changes, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(queue, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 1 {
			if events[0].Flags&unix.EV_ERROR != 0 {
				return unix.Errno(events[0].Data)
			}
			if events[0].Fflags&unix.NOTE_EXIT != 0 {
				return nil
			}
		}
	}
}
