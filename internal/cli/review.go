package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"github.com/spf13/cobra"
)

func (e *execution) localStore(command *cobra.Command) (*store.Store, error) {
	document, path, err := e.load(command, false)
	if err != nil {
		return nil, err
	}
	policy, err := config.ResolveDefaults(document, e.overrides(command))
	if err != nil {
		return nil, err
	}
	directory, err := stateDirectory(policy, path)
	if err != nil {
		return nil, err
	}
	return store.Open(command.Context(), directory)
}
func (e *execution) reviewCommand(deps Dependencies) *cobra.Command {
	private, now := false, false
	command := &cobra.Command{Use: "review <pr-url>", Short: "Prepare a private immutable PR review", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&private, "private", false, "Prepare only; do not grant automatic publication")
	command.Flags().BoolVar(&now, "now", false, "Review while mandatory checks are pending, retaining the readiness gap")
	command.RunE = func(command *cobra.Command, args []string) error {
		if !private {
			return failure(2, "this development version prepares private reviews only; use review <pr-url> --private")
		}
		document, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		overrides := e.overrides(command)
		policy, err := config.ResolveDefaults(document, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		ref, err := github.ParsePullURL(policy.String("github.host"), args[0])
		if err != nil {
			return failure(2, err.Error())
		}
		directory, err := stateDirectory(policy, path)
		if err != nil {
			return failure(1, err.Error())
		}
		durable, err := store.Open(command.Context(), directory)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		e.recoverBeforeWork(command, durable)
		client, err := deps.GitHub(command.Context(), policy.String("github.host"))
		if err != nil {
			return failure(1, err.Error())
		}
		worker := deps.Review
		if worker == nil {
			worker = func(ctx context.Context, request review.WorkerRequest) (codex.ReviewOutput, error) {
				return runPrivateWorker(ctx, request, func(message codex.Message) {
					if e.json {
						return
					}
					if message.Method == "item/tool/call" {
						fmt.Fprintln(command.ErrOrStderr(), "Reviewing immutable evidence…")
					}
				})
			}
		}
		service := review.Service{GitHub: client, Store: durable, SkillPath: skillPath, Source: deps.Source, Worker: withReviewSlot(directory, worker)}
		run, err := service.Start(command.Context(), document, ref, overrides, private, now)
		if err != nil {
			if run.ID != "" {
				return failure(1, "Review "+run.ID+": "+err.Error()+"; inspect it with reviewd show")
			}
			return failure(1, err.Error())
		}
		return e.presentRun(command.OutOrStdout(), run)
	}
	return command
}

func runPrivateWorker(ctx context.Context, request review.WorkerRequest, progress func(codex.Message)) (output codex.ReviewOutput, err error) {
	return executePrivateWorker(ctx, request, progress, request.Policy.String("reviewer.reasoning_effort"), true)
}

func executePrivateWorker(ctx context.Context, request review.WorkerRequest, progress func(codex.Message), effort string, allowSpecialist bool) (output codex.ReviewOutput, err error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	version, probeErr := lifetime.Output(probeCtx, exec.CommandContext(probeCtx, request.Policy.String("reviewer.executable"), "--version"))
	cancel()
	if probeErr != nil || strings.TrimSpace(string(version)) != "codex-cli 0.162.1" {
		return output, errors.New("private review requires the tested Codex CLI 0.162.1; run reviewd doctor for compatibility guidance")
	}
	worker, err := codex.StartRuntime(ctx, codex.RuntimeOptions{Executable: request.Policy.String("reviewer.executable"), AuthHome: request.Policy.String("reviewer.auth_home")})
	if err != nil {
		return output, err
	}
	defer func() { err = errors.Join(err, worker.Close()) }()
	if err = worker.CheckAccount(ctx); err != nil {
		return output, err
	}
	turn := codex.ReviewRequest{Model: request.Policy.String("reviewer.model"), Effort: effort, SkillPath: request.Skill.Path, SkillName: request.Skill.Identity.Name, Prompt: request.Prompt, Evidence: request.Evidence, OnProgress: progress, OnStatus: request.OnStatus, OnInput: request.OnInput, OnInputEvent: request.OnInputEvent, Participant: request.Participant}
	if allowSpecialist {
		turn.OutputSchema = review.OutputSchema()
		turn.OnSpecialist = func(ctx context.Context, task codex.SpecialistRequest) (codex.SpecialistOutput, error) {
			leaf := request
			// A specialist has a distinct session. It must not replace its
			// parent's durable thread/turn identity with its own.
			leaf.OnStatus = nil
			leaf.Participant = "specialist"
			leaf.Prompt = "As the sole independent specialist, audit the complete causal slice for this bounded task, treating any claims in the task as unverified. Return raw source-backed findings, gaps and counterevidence to the primary reviewer; do not publish or spawn further agents. Task: " + task.Task + ". Scope paths: " + strings.Join(task.Paths, ", ") + ". Frozen PR context and evidence access:\n" + request.Prompt
			result, err := executePrivateWorker(ctx, leaf, progress, task.Effort, false)
			return codex.SpecialistOutput{Task: task.Task, Effort: task.Effort, Text: result.Text, ThreadID: result.ThreadID, TurnID: result.TurnID, RequestedModel: result.RequestedModel, EffectiveModel: result.EffectiveModel, Usage: result.Usage}, err
		}
	}
	return worker.Review(ctx, turn)
}

func (e *execution) refreshCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "refresh <run-id>", Short: "Refresh a prepared preview's checks without another source review", Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		if len(e.overrides(command)) != 0 {
			return failure(2, "refresh uses the frozen source review; start a fresh review to change reviewer settings")
		}
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		run, err := durable.Get(command.Context(), args[0])
		if err != nil {
			return failure(1, err.Error())
		}
		var prepared review.PreparedResult
		if json.Unmarshal(run.Prepared, &prepared) != nil {
			return failure(1, "run has no valid prepared preview to refresh")
		}
		host := prepared.Boundary.Configuration.String("github.host")
		client, err := deps.GitHub(command.Context(), host)
		if err != nil {
			return failure(1, err.Error())
		}
		service := review.Service{GitHub: client, Store: durable}
		current, err := service.RefreshChecks(command.Context(), run.ID)
		if err != nil {
			return failure(1, err.Error())
		}
		return e.presentRun(command.OutOrStdout(), current)
	}
	return command
}

func (e *execution) showCommand() *cobra.Command {
	command := &cobra.Command{Use: "show <run-id>", Short: "Show the exact prepared preview and actual durable state", Args: cobra.ExactArgs(1)}
	command.RunE = func(command *cobra.Command, args []string) error {
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		run, err := durable.Get(command.Context(), args[0])
		if err != nil {
			return failure(1, err.Error())
		}
		return e.presentRun(command.OutOrStdout(), run)
	}
	return command
}
func (e *execution) inboxCommand(deps Dependencies) *cobra.Command {
	var interactive bool
	command := &cobra.Command{Use: "inbox", Short: "List private reviews in the selected durable profile", Args: cobra.NoArgs}
	command.Flags().BoolVar(&interactive, "interactive", false, "Attach the optional keyboard inbox; redirected/JSON output stays plain")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if interactive && !e.json && terminal.IsTerminal(command.InOrStdin(), command.OutOrStdout()) {
			return e.attachInbox(command, deps, false)
		}
		durable, err := e.localStore(command)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		return e.presentInbox(command.Context(), command.OutOrStdout(), durable)
	}
	return command
}

func terminalText(text string) string {
	var result strings.Builder
	for _, char := range redact(text) {
		if (char < 32 && char != '\n' && char != '\t') || char == 127 {
			fmt.Fprintf(&result, "\\u%04x", char)
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}
func (e *execution) presentRun(output io.Writer, run store.Run) error {
	if e.json {
		data, err := json.Marshal(run)
		if err != nil {
			return failure(1, "could not present durable review state")
		}
		safe := redact(string(data))
		return writeJSON(output, map[string]any{"schema_version": 1, "run": json.RawMessage(safe), "redacted": safe != string(data)})
	}
	fmt.Fprintf(output, "Review %s\nRepository: %q #%d\nState: %s\n", run.ID, redact(run.Repository), run.Number, run.State)
	if run.CancellationRequested && run.State != store.Cancelled {
		fmt.Fprintln(output, "Cancellation requested; the private worker has not yet confirmed that it stopped.")
	}
	if run.Superseded && run.State == store.Reviewing && !run.CancellationRequested {
		fmt.Fprintln(output, "Review evidence superseded; the private worker is stopping. This run does not certify the current pull request.")
	}
	fmt.Fprintf(output, "GitHub identity: %q\n", redact(run.ActorLogin))
	if run.Archive != nil {
		fmt.Fprintf(output, "Private review text expired: %s; retained summary only, not reusable review evidence.\n", terminalText(run.Archive.ExpiredAt))
		if run.Archive.HeadSHA != "" {
			label := "Archived source head"
			if run.Archive.Reviewed {
				label = "Archived reviewed head"
			}
			fmt.Fprintf(output, "%s: %s\nTarget base: %s\nComparison base: %s\n", label, terminalText(run.Archive.HeadSHA), terminalText(run.Archive.TargetBaseSHA), terminalText(run.Archive.MergeBaseSHA))
		}
		if run.Archive.Effort != "" {
			fmt.Fprintf(output, "Reasoning effort: %q\n", redact(run.Archive.Effort))
		}
	}
	if run.ThreadID != "" {
		fmt.Fprintf(output, "Thread: %q\n", redact(run.ThreadID))
	}
	if run.TurnID != "" {
		fmt.Fprintf(output, "Turn: %q\n", redact(run.TurnID))
	}
	if run.State == store.Reviewing && run.Progress != "" {
		fmt.Fprintf(output, "Activity: %s\n", terminalText(run.Progress))
	}
	var usage struct {
		TokenUsage struct {
			Total struct {
				TotalTokens *int64 `json:"totalTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(run.Usage, &usage) == nil && usage.TokenUsage.Total.TotalTokens != nil {
		fmt.Fprintf(output, "Reported token usage: %d total\n", *usage.TokenUsage.Total.TotalTokens)
	}
	for _, input := range run.Inputs {
		fmt.Fprintf(output, "Input: %s (%s, %s) — inspect reviewd inputs %s\n", input.ID, input.State, input.Participant, run.ID)
	}
	if run.State == store.Prepared {
		fmt.Fprintln(output, "Prepared privately; not published.")
	}
	if run.State == store.WaitingChecks {
		fmt.Fprintln(output, "Waiting for mandatory checks; no model turn was started. Use review --private --now for an immediate review with the readiness gap retained.")
	}
	if run.RequestedModel != "" {
		fmt.Fprintf(output, "Model: %q", redact(run.RequestedModel))
		if run.EffectiveModel != run.RequestedModel {
			fmt.Fprintf(output, " (effective: %q)", redact(run.EffectiveModel))
		}
		fmt.Fprintln(output)
	}
	var configuration struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if json.Unmarshal(run.Configuration, &configuration) == nil {
		var effort string
		if json.Unmarshal(configuration.Values["reviewer.reasoning_effort"], &effort) == nil && effort != "" {
			fmt.Fprintf(output, "Reasoning effort: %q\n", redact(effort))
		}
	}
	if run.Failure != "" {
		fmt.Fprintln(output, terminalText(run.Failure))
	}
	if len(run.Outcome) > 0 {
		var outcome review.PublicationOutcome
		if json.Unmarshal(run.Outcome, &outcome) != nil {
			return failure(1, "stored publication outcome is invalid; preserve the profile for recovery")
		}
		verified := "not verified"
		if outcome.Verified {
			verified = "verified"
		}
		fmt.Fprintf(output, "Stored GitHub readback: %s\n", verified)
		if outcome.Review.URL != "" {
			fmt.Fprintf(output, "Recorded review: %s (%s)\n", terminalText(outcome.Review.URL), terminalText(outcome.Review.State))
		}
		for _, comment := range outcome.Comments {
			fmt.Fprintf(output, "Recorded inline: %s\n", terminalText(comment.URL))
		}
		fmt.Fprintf(output, "Reviewed head: %s\nCurrent head: %s\n", terminalText(outcome.ReviewedHead), terminalText(outcome.CurrentHead))
		if outcome.Failure != "" {
			fmt.Fprintln(output, terminalText(outcome.Failure))
		}
	}
	if len(run.Prepared) > 0 {
		var preview struct {
			Event    string           `json:"event"`
			Body     string           `json:"body"`
			Comments []review.Comment `json:"comments"`
			Boundary struct {
				MergeBaseSHA string      `json:"merge_base_sha"`
				Pull         github.Pull `json:"pull"`
			} `json:"boundary"`
		}
		if json.Unmarshal(run.Prepared, &preview) != nil {
			return failure(1, "stored prepared preview is invalid; preserve the profile for recovery")
		}
		if preview.Boundary.Pull.HeadSHA != "" {
			fmt.Fprintf(output, "Head: %s\nTarget base: %s\nComparison base: %s\n", terminalText(preview.Boundary.Pull.HeadSHA), terminalText(preview.Boundary.Pull.TargetBaseSHA), terminalText(preview.Boundary.MergeBaseSHA))
		}
		fmt.Fprintf(output, "Proposed GitHub event: %s\n\n%s\n", terminalText(preview.Event), terminalText(preview.Body))
		for _, comment := range preview.Comments {
			fmt.Fprintf(output, "\n%q %s:%d\n%s\n", redact(comment.Path), terminalText(comment.Side), comment.Line, terminalText(comment.Body))
		}
	}
	return nil
}
