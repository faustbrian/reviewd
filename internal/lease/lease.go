//go:build darwin || linux

// Package lease provides stable-inode, kernel-owned local leases. A lease is
// not a PID file, and a contender must never unlink another owner's inode.
package lease

import (
	"errors"
	"os"
	"syscall"
)

var ErrBusy = errors.New("local lease is already held")

type Lease struct{ file *os.File }

func Acquire(path string, create bool) (*Lease, error) {
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := syscall.Open(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, errors.New("local lease must be a private regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, errors.New("cannot acquire local kernel lease")
	}
	return &Lease{file: file}, nil
}

func (l *Lease) Close() error { return l.file.Close() }

// Info identifies the locked inode, not whatever currently occupies its path.
func (l *Lease) Info() (os.FileInfo, error) { return l.file.Stat() }
