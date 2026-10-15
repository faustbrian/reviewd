//go:build !linux

package commandcapture

func protect() error { return ErrIncomplete }

func ExecRestricted([]string) error { return ErrIncomplete }
