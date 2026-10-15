package commandcapture

import (
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ExecRestricted is entered only by the key-free trusted child. Restrict the
// calling OS thread, then replace it directly: no other Go runtime thread may
// launch the repository process. Landlock is inherited across exec and forks.
func ExecRestricted(argv []string) error {
	if os.Getppid() != 1 || os.Getenv(KeyEnvironment) != "" || len(argv) == 0 {
		return ErrIncomplete
	}
	failure := os.NewFile(3, "capture-setup")
	if failure == nil {
		return ErrIncomplete
	}
	defer failure.Close()
	defer func() { _, _ = failure.Write([]byte{1}) }()
	runtime.LockOSThread()
	// Deliberately never unlock a restricted thread. All error paths exit the
	// trusted child; successful exec removes the Go runtime and its other threads.
	unix.CloseOnExec(3)
	if err := restrictWrites(); err != nil {
		return ErrIncomplete
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		return ErrIncomplete
	}
	return unix.Exec(executable, argv, os.Environ())
}

func restrictWrites() error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	// ABI 3 is required: WRITE_FILE alone does not prevent truncate(2).
	if errno != 0 || abi < 3 {
		return ErrIncomplete
	}
	const writes = uint64(unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	attr := struct{ Handled uint64 }{writes}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return ErrIncomplete
	}
	defer unix.Close(int(fd))
	// Mount permissions still decide whether /workspace is read-only. No rule
	// grants access to the capture bind, even through a link or alternate path.
	for _, rule := range []struct {
		path   string
		access uint64
	}{
		{"/workspace", writes}, {"/tmp", writes}, {"/dev/shm", writes},
		{"/dev/null", unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		{"/dev/zero", unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		{"/dev/random", unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		{"/dev/urandom", unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
	} {
		path, err := unix.Open(rule.path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return ErrIncomplete
		}
		// Linux UAPI landlock_path_beneath_attr is packed, exactly 12 bytes.
		var beneath [12]byte
		for i := range 8 {
			beneath[i] = byte(rule.access >> (8 * i))
		}
		for i := range 4 {
			beneath[8+i] = byte(uint32(path) >> (8 * i))
		}
		_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, 1, uintptr(unsafe.Pointer(&beneath)), 0, 0, 0)
		_ = unix.Close(path)
		if errno != 0 {
			return ErrIncomplete
		}
	}
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return ErrIncomplete
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return ErrIncomplete
	}
	return nil
}
