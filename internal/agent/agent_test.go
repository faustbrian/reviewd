package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestOrdinarySkillLaunch(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("REVIEWD_TEST_MCP_SENTINEL", "normal-config")
	for _, skill := range []string{"review", "handle-pr-feedback"} {
		t.Run(skill, func(t *testing.T) {
			calls := 0
			directory := t.TempDir()
			got, err := run(context.Background(), Request{Executable: "codex-fixture", Directory: directory, Skill: skill, URL: "https://github.com/acme/api/pull/7", Model: "gpt-6.1-sol", Effort: "high"}, func(_ context.Context, command *exec.Cmd) error {
				calls++
				if command.Dir != directory {
					t.Fatal("configured working directory was lost")
				}
				want := []string{"codex-fixture", "exec", "--json", "--model", "gpt-6.1-sol", "-c", `model_reasoning_effort="high"`, "--skip-git-repo-check", "$" + skill + " https://github.com/acme/api/pull/7"}
				if !reflect.DeepEqual(command.Args, want) {
					t.Fatalf("ordinary invocation: got %q want %q", command.Args, want)
				}
				if command.Env != nil {
					t.Fatal("launcher replaced the inherited environment")
				}
				if !strings.Contains(strings.Join(command.Environ(), "\n"), "REVIEWD_TEST_MCP_SENTINEL=normal-config") {
					t.Fatal("normal tool environment missing")
				}
				_, err := io.WriteString(command.Stdout, "{\"type\":\"thread.started\",\"thread_id\":\"fresh-thread\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"Skill finished\"}}\n{\"type\":\"turn.completed\"}\n")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || got.ThreadID != "fresh-thread" || got.Text != "Skill finished" || !got.Completed {
				t.Fatalf("launch outcome: %+v, calls=%d", got, calls)
			}
		})
	}
}

func TestLaunchConfigurationAndFailures(t *testing.T) {
	tests := []struct {
		name, events string
		exit         error
		wantError    bool
	}{
		{"complete", `{"type":"thread.started","thread_id":"one"}` + "\n" + `{"type":"turn.completed"}` + "\n", nil, false},
		{"exit failure", `{"type":"thread.started","thread_id":"one"}` + "\n", errors.New("exit status 1"), true},
		{"uncompleted native error", `{"type":"thread.started","thread_id":"one"}` + "\n" + `{"type":"error","message":"connection lost"}` + "\n", nil, true},
		{"lost completion", `{"type":"thread.started","thread_id":"one"}` + "\n", nil, true},
		{"failed turn", `{"type":"turn.failed","error":{"message":"provider unavailable"}}` + "\n", nil, true},
		{"malformed", "not-json\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := run(context.Background(), Request{Skill: "custom-review", URL: "https://github.com/acme/api/pull/8", Model: "chosen-model", Effort: "medium", Instructions: "Follow project instructions."}, func(_ context.Context, c *exec.Cmd) error {
				if c.Args[4] != "chosen-model" || c.Args[6] != `model_reasoning_effort="medium"` || c.Args[8] != "$custom-review https://github.com/acme/api/pull/8\n\nFollow project instructions." {
					t.Fatalf("selection lost: %q", c.Args)
				}
				_, err := fmt.Fprint(c.Stdout, tt.events)
				if err != nil {
					return err
				}
				return tt.exit
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("got %+v / %v; want error=%v", got, err, tt.wantError)
			}
		})
	}
}

func TestLaunchProgressFailureCannotReportSuccess(t *testing.T) {
	sentinel := errors.New("state unavailable")
	_, err := run(context.Background(), Request{Skill: "review", URL: "https://github.com/acme/api/pull/7", OnProgress: func(Output) error { return sentinel }}, func(_ context.Context, c *exec.Cmd) error {
		_, err := io.WriteString(c.Stdout, "{\"type\":\"thread.started\",\"thread_id\":\"one\"}\n{\"type\":\"turn.completed\"}\n")
		return err
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost persistence failure: %v", err)
	}
}

func TestLaunchLeavesNativeRetriesToCodex(t *testing.T) {
	const notice = "Reconnecting... 1/5"
	got, err := run(context.Background(), Request{Skill: "handle-pr-feedback", URL: "https://github.com/acme/api/pull/7"}, func(ctx context.Context, c *exec.Cmd) error {
		_, err := io.WriteString(c.Stdout, `{"type":"thread.started","thread_id":"retrying-session"}`+"\n"+`{"type":"error","message":"Reconnecting... 1/5"}`+"\n")
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errors.New("reviewd interrupted native retry")
		}
		_, err = io.WriteString(c.Stdout, `{"type":"turn.completed"}`+"\n")
		return err
	})
	if err != nil || !got.Completed {
		t.Fatalf("native recovered turn was lost: %+v / %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), notice) {
		t.Fatalf("native retry diagnostic was lost: %s", raw)
	}
}

func TestLaunchRetainsStartupDiagnosticsWithoutAConfiguredSink(t *testing.T) {
	got, err := run(context.Background(), Request{Skill: "review", URL: "https://github.com/fixture/fixture/pull/1"}, func(_ context.Context, c *exec.Cmd) error {
		if c.Stderr != nil {
			fmt.Fprint(c.Stderr, "Configured Codex profile could not be loaded.")
		}
		return errors.New("exit status 2")
	})
	if err == nil || !strings.Contains(got.Diagnostic, "profile could not be loaded") {
		t.Fatalf("startup failure became an opaque exit status: %+v %v", got, err)
	}
}

func TestNativeActivityIsVisibleBeforeCompletion(t *testing.T) {
	canary := "ghp_" + strings.Repeat("A", 36)
	var statuses []string
	got, err := run(context.Background(), Request{Skill: "review", URL: "https://github.com/acme/api/pull/7", OnProgress: func(output Output) error {
		data, _ := json.Marshal(output)
		statuses = append(statuses, string(data))
		return nil
	}}, func(_ context.Context, c *exec.Cmd) error {
		frames := []string{
			`{"type":"thread.started","thread_id":"live-session"}`,
			`{"type":"item.started","item":{"type":"command_execution","command":"echo private-payload","status":"in_progress"}}`,
			`{"type":"item.updated","item":{"type":"mcp_tool_call","server":"fixture","tool":"inspect","arguments":{"token":"private-argument"},"status":"in_progress"}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"Reading feedback ` + canary + `\u001b[31m"}}`,
			`{"type":"future.unknown","payload":"not activity"}`,
		}
		for _, frame := range frames {
			if _, err := io.WriteString(c.Stdout, frame+"\n"); err != nil {
				return err
			}
		}
		joined := strings.Join(statuses, "\n")
		for _, want := range []string{"command_execution", "mcp_tool_call", "Reading feedback"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("running activity missing %q: %s", want, joined)
			}
		}
		for _, private := range []string{canary, "private-payload", "private-argument", `\u001b`} {
			if strings.Contains(joined, private) {
				t.Fatalf("unsafe activity retained %q", private)
			}
		}
		if strings.Contains(joined, `"completed":true`) {
			t.Fatal("activity certified completion")
		}
		_, err := io.WriteString(c.Stdout, `{"type":"turn.completed"}`+"\n")
		return err
	})
	if err != nil || !got.Completed {
		t.Fatalf("native completion lost: %+v %v", got, err)
	}
}
