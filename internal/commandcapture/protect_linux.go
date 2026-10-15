package commandcapture

import (
	"golang.org/x/sys/unix"
	"os"
)

func protect() error {
	// The app disables Docker's optional init: the static trusted helper is PID1.
	// Fail closed before any repository-controlled process exists. Dumpability
	// protection blocks same-UID /proc environment, descriptor and memory access;
	// ALL capabilities stay dropped, including CAP_SYS_PTRACE.
	if os.Getpid() != 1 {
		return ErrIncomplete
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return ErrIncomplete
	}
	value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || value != 0 {
		return ErrIncomplete
	}
	return nil
}
