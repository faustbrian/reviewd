package notify

import (
	"context"
	"strings"
	"testing"
)

func TestDesktopArgumentsNeverInterpretRemoteTextAsCodeOrOptions(t *testing.T) {
	message := Message{Title: `owner/repo #1 "quoted"`, Body: `$(touch /tmp/not-permitted) <b>feedback</b>`}
	for _, platform := range []string{"darwin", "linux"} {
		path, args, err := desktopCommand(platform, message)
		if err != nil || path == "" || len(args) < 2 {
			t.Fatalf("%s: no notification invocation: %v", platform, err)
		}
		if platform == "darwin" {
			if path != "/usr/bin/osascript" || args[len(args)-2] != message.Title || args[len(args)-1] != message.Body || strings.Contains(args[1], message.Body) {
				t.Fatal("notification text was interpolated into AppleScript")
			}
		} else if path != "notify-send" || args[len(args)-3] != "--" || strings.Contains(args[len(args)-1], "<b>") {
			t.Fatal("Linux notification accepts options or unsanitized markup")
		}
	}
	if _, _, err := desktopCommand("windows", message); err == nil {
		t.Fatal("unsupported operating system was accepted")
	}
	var text strings.Builder
	if err := Terminal(&text)(context.Background(), Message{Title: "title\x1b[31m", Body: "feedback\nnext"}); err != nil || strings.ContainsAny(text.String(), "\x1b\r") || strings.Count(text.String(), "\n") != 1 {
		t.Fatal("terminal notification can inject terminal controls or lines")
	}
}
