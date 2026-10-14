package notify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"os/exec"
	"runtime"
	"time"
)

func Terminal(writer io.Writer) Sink {
	return func(_ context.Context, message Message) error {
		_, err := fmt.Fprintf(writer, "[reviewd] %s: %s\n", safeText(message.Title, 160), safeText(message.Body, 1200))
		return err
	}
}

func desktopCommand(platform string, message Message) (string, []string, error) {
	title, body := safeText(message.Title, 160), safeText(message.Body, 1200)
	switch platform {
	case "darwin":
		// Static program, literal argv: PR/repository text is never executable
		// AppleScript. No action, shell, or consent callback is installed.
		return "/usr/bin/osascript", []string{"-e", "on run argv\n display notification (item 2 of argv) with title (item 1 of argv)\nend run", title, body}, nil
	case "linux":
		// notify-send is an optional desktop-session prerequisite, not a build
		// dependency. Escape body markup and terminate option parsing explicitly.
		return "notify-send", []string{"--app-name=reviewd", "--expire-time=10000", "--", title, html.EscapeString(body)}, nil
	default:
		return "", nil, errors.New("desktop notifications support macOS and Linux only")
	}
}

func Desktop() Sink {
	return func(ctx context.Context, message Message) error {
		path, arguments, err := desktopCommand(runtime.GOOS, message)
		if err != nil {
			return err
		}
		bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		command := exec.CommandContext(bounded, path, arguments...)
		command.WaitDelay = time.Second
		if err := command.Run(); err != nil {
			return errors.New("desktop notifications unavailable; check the desktop session and notification permissions; work remains in reviewd inbox")
		}
		return nil
	}
}
