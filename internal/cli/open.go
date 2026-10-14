package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/spf13/cobra"
)

func (e *execution) openCommand(deps Dependencies) *cobra.Command {
	var printOnly, conversation bool
	command := &cobra.Command{Use: "open <item-or-run-id>", Short: "Open the selected PR or its recorded review conversation", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&printOnly, "print", false, "Print the URL without launching a browser")
	command.Flags().BoolVar(&conversation, "conversation", false, "Open the run's recorded GitHub review instead of the PR")
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, defaults, err := e.controlStore(command)
		if err != nil {
			return err
		}
		defer durable.Close()
		runs, err := durable.List(command.Context())
		if err != nil {
			return failure(1, err.Error())
		}
		repository, number, recorded := "", 0, ""
		for _, run := range runs {
			if run.ID != args[0] {
				continue
			}
			repository, number = run.Repository, run.Number
			if conversation {
				var outcome review.PublicationOutcome
				if json.Unmarshal(run.Outcome, &outcome) == nil {
					recorded = outcome.Review.URL
				}
			}
			break
		}
		if repository == "" {
			items, _, err := durable.Inbox(command.Context())
			if err != nil {
				return failure(1, err.Error())
			}
			for _, item := range items {
				if item.ID == args[0] {
					repository, number = item.Repository, item.Number
					break
				}
			}
		}
		if repository == "" {
			return failure(1, "local item or run was not found; use inbox to select its exact ID")
		}
		host := defaults.String("github.host")
		if parts := strings.Split(repository, "/"); len(parts) == 3 {
			host = parts[0] // Historical runs retain their original host; no auth is used.
		}
		canonical, err := config.Repository(host, repository)
		if err != nil || number < 1 {
			return failure(1, "stored PR identity is invalid; no browser was launched")
		}
		target := "https://" + canonical + "/pull/" + strconv.Itoa(number)
		if conversation {
			ref, err := github.ParsePullURL(host, recorded)
			parsed, parseErr := url.Parse(recorded)
			if err != nil || parseErr != nil || ref.URL != target || !reviewFragment(parsed.Fragment) {
				return failure(1, "no valid recorded review conversation for this PR; omit --conversation to open the PR")
			}
			target += "#" + parsed.Fragment
		}
		if !printOnly {
			launch := deps.OpenURL
			if launch == nil {
				launch = openBrowser
			}
			if err := launch(command.Context(), target); err != nil {
				return failure(1, "browser launch unavailable; use open --print to obtain the URL without a desktop session")
			}
		}
		if e.json {
			return writeJSON(command.OutOrStdout(), map[string]any{"schema_version": 1, "url": target, "launched": !printOnly})
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), target)
		return err
	}
	return command
}

func reviewFragment(fragment string) bool {
	for _, prefix := range []string{"pullrequestreview-", "discussion_r"} {
		if suffix, ok := strings.CutPrefix(fragment, prefix); ok {
			number, err := strconv.ParseInt(suffix, 10, 64)
			return err == nil && number > 0 && suffix == strconv.FormatInt(number, 10)
		}
	}
	return false
}

func openBrowser(ctx context.Context, target string) error {
	var command *exec.Cmd
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(bounded, "/usr/bin/open", "--", target)
	case "linux":
		command = exec.CommandContext(bounded, "xdg-open", target)
	default:
		return errors.New("browser opening supports macOS and Linux only")
	}
	command.WaitDelay = time.Second
	return command.Run()
}
