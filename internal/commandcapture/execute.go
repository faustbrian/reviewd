package commandcapture

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Execute runs only inside the owned Linux container. The key stays in this
// protected process; the child receives neither it nor capture descriptors.
// No signal handler can turn interruption into an authenticated completion.
func Execute(directory string, key []byte, request Request) (int, error) {
	if err := protect(); err != nil {
		return 125, ErrIncomplete
	}
	session, err := New(directory, key, request)
	if err != nil {
		return 125, ErrIncomplete
	}
	ready, childReady, err := os.Pipe()
	if err != nil {
		return 125, ErrIncomplete
	}
	defer ready.Close()
	defer childReady.Close()
	command := exec.Command(GuestHelper, append([]string{"--restricted-child", "--"}, request.Argv...)...)
	command.ExtraFiles = []*os.File{childReady}
	command.Env = make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, KeyEnvironment+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Stdout = session.Stdout
	command.Stderr = session.Stderr
	exit, complete := 0, true
	err = command.Start()
	_ = childReady.Close()
	if err != nil {
		return 125, ErrIncomplete
	}
	// The trusted child closes descriptor 3 on successful exec. A setup error
	// writes one byte instead. It never passes this descriptor to repository code.
	setup, setupErr := io.ReadAll(io.LimitReader(ready, 1))
	complete = setupErr == nil && len(setup) == 0
	err = command.Wait()
	if err != nil {
		var failure *exec.ExitError
		if errors.As(err, &failure) && failure.ExitCode() >= 0 && failure.ExitCode() <= 255 {
			exit = failure.ExitCode()
		} else {
			exit = 125
			complete = false
		}
	}
	if err := session.Finish(exit, complete); err != nil {
		return 125, ErrIncomplete
	}
	return exit, nil
}
