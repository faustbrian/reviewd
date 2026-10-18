package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/spf13/cobra"
)

type diagnosticReport struct {
	SchemaVersion  int              `json:"schema_version"`
	Ready          bool             `json:"ready"`
	Created        bool             `json:"created,omitempty"`
	ConfigPath     string           `json:"config_path"`
	StateDirectory string           `json:"state_directory,omitempty"`
	Identity       *github.Identity `json:"github_identity,omitempty"`
	Diagnostics    []Diagnostic     `json:"diagnostics"`
	Policies       []policyReport   `json:"policies"`
	Redacted       bool             `json:"redacted,omitempty"`
}
type policyReport struct {
	Backend         string `json:"backend"`
	FeedbackBackend string `json:"feedback_backend"`
	Scope           string `json:"scope"`
	ReviewRequested string `json:"review_requested"`
	NewPullRequest  string `json:"new_pull_request"`
	HeadUpdated     string `json:"head_updated"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func policy(snapshot config.Snapshot) policyReport {
	scope := snapshot.Repository
	if scope == "" {
		scope = "global"
	}
	return policyReport{Scope: scope, ReviewRequested: snapshot.String("actions.review_requested"), NewPullRequest: snapshot.String("actions.new_pull_request"), HeadUpdated: snapshot.String("actions.head_updated"), Model: snapshot.String("reviewer.model"), ReasoningEffort: snapshot.String("reviewer.reasoning_effort"), Backend: snapshot.String("reviewer.backend"), FeedbackBackend: snapshot.String("feedback.backend")}
}
func (e *execution) doctorCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "doctor", Short: "Check account, review settings and prerequisites without reviewing PRs", Args: cobra.NoArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		document, path, err := e.load(command, false)
		if err != nil {
			report := diagnosticReport{SchemaVersion: 1, ConfigPath: path, Diagnostics: []Diagnostic{{Check: "configuration", Status: "error", Message: err.Error(), Correction: "Correct the configuration or run reviewd init for a missing file."}}, Policies: []policyReport{}}
			e.resultCode = 1
			return e.presentDiagnostics(command.OutOrStdout(), report)
		}
		report := e.diagnose(command.Context(), document, path, e.overrides(command), deps)
		if !report.Ready {
			e.resultCode = 1
		}
		return e.presentDiagnostics(command.OutOrStdout(), report)
	}
	return command
}
func (e *execution) initCommand(deps Dependencies) *cobra.Command {
	command := &cobra.Command{Use: "init", Short: "Create a safe personal configuration without modifying repositories", Args: cobra.NoArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		path, err := e.configPath()
		if err != nil {
			return failure(2, err.Error())
		}
		// O_EXCL is the final authority, including concurrent initializers. An
		// existing symlink or file is never followed or overwritten.
		if _, err = os.Lstat(path); err == nil {
			return failure(2, "configuration already exists; use reviewd doctor or config explain")
		} else if !errors.Is(err, os.ErrNotExist) {
			return failure(1, "cannot inspect configuration path; check permissions")
		}
		document, err := config.Parse([]byte(initialConfiguration), path)
		if err != nil {
			return failure(2, err.Error())
		}
		selected, err := config.ResolveDefaults(document, e.overrides(command))
		if err != nil {
			return failure(2, err.Error())
		}
		configuration := initialConfiguration
		additions := ""
		for _, setting := range []struct{ flag, key, value string }{{"backend", "backend", selected.String("reviewer.backend")}, {"executable", "executable", selected.String("reviewer.executable")}, {"model", "model", selected.String("reviewer.model")}, {"reasoning-effort", "reasoning_effort", selected.String("reviewer.reasoning_effort")}} {
			if command.Flags().Changed(setting.flag) {
				additions += setting.key + " = " + strconv.Quote(setting.value) + "\n"
			}
		}
		configuration = strings.Replace(configuration, "[reviewer]\n", "[reviewer]\n"+additions, 1)
		document, err = config.Parse([]byte(configuration), path)
		if err != nil {
			return failure(2, err.Error())
		}
		// Diagnostics precede creation. Initial policy never enables automatic
		// execution/publication and does not choose customer repositories.
		report := e.diagnose(command.Context(), document, path, nil, deps)
		if command.Context().Err() != nil {
			return command.Context().Err()
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return failure(1, "cannot create configuration directory; check permissions")
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return failure(1, "configuration could not be created exclusively; it may already exist")
		}
		_, writeErr := io.WriteString(file, configuration)
		err = errors.Join(writeErr, file.Sync(), file.Close())
		if err != nil {
			// Only this initializer's newly created file is owned here.
			removeErr := os.Remove(path)
			if removeErr != nil {
				return failure(1, "configuration write failed and the partial file could not be removed; inspect the selected config path")
			}
			return failure(1, "configuration write failed; no configuration was retained")
		}
		report.Created = true
		return e.presentDiagnostics(command.OutOrStdout(), report)
	}
	return command
}
func (e *execution) diagnose(ctx context.Context, document *config.Document, path string, overrides map[string]any, deps Dependencies) diagnosticReport {
	report := diagnosticReport{SchemaVersion: 1, ConfigPath: path, Diagnostics: []Diagnostic{}, Policies: []policyReport{}}
	defaults, err := config.ResolveDefaults(document, overrides)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "configuration", Status: "error", Message: err.Error(), Correction: "Correct the selected configuration and command overrides."})
		return report
	}
	report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "configuration", Status: "ok", Message: "Configuration is valid."})
	scopes := []config.Snapshot{defaults}
	report.Policies = append(report.Policies, policy(defaults))
	state, err := stateDirectory(defaults, path)
	if err == nil {
		err = probeDirectory(state)
	}
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "local state", Status: "error", Message: "Private local state is not writable.", Correction: "Choose a writable state.directory owned by your account."})
	} else {
		report.StateDirectory = state
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "local state", Status: "ok", Message: "Private state storage can be created; no durable state was initialized."})
	}
	client, err := deps.GitHub(ctx, defaults.String("github.host"))
	if err == nil {
		var actor github.Identity
		actor, err = client.Identity(ctx)
		if err == nil {
			report.Identity = &actor
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "GitHub account", Status: "ok", Message: "Authenticated posting identity confirmed."})
		}
	}
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "GitHub account", Status: "error", Message: err.Error(), Correction: "Install gh, sign in for the configured github.host and confirm repository access."})
	}
	for _, repository := range defaults.Strings("github.repositories") {
		var preferences *config.RepositoryPreferences
		if err == nil {
			source, readErr := client.DefaultPreferences(ctx, repository)
			if readErr != nil {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "repository preferences", Scope: repository, Status: "error", Message: readErr.Error(), Correction: "Confirm the repository identity, permissions and complete default-branch evidence."})
				continue
			}
			preferences = source.Preferences
		} else {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "repository preferences", Scope: repository, Status: "error", Message: "Trusted repository preferences were not checked.", Correction: "Restore GitHub account access and rerun doctor."})
			continue
		}
		resolved, resolveErr := config.Resolve(document, repository, preferences, overrides)
		if resolveErr != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "repository configuration", Scope: repository, Status: "error", Message: resolveErr.Error(), Correction: "Correct trusted repository preferences or exact project settings."})
			continue
		}
		scopes = append(scopes, resolved)
		report.Policies = append(report.Policies, policy(resolved))
	}
	if len(defaults.Strings("github.repositories")) == 0 {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "repository scope", Status: "warning", Message: "No repositories are selected for monitoring.", Correction: "Set github.repositories to an explicit allowlist before starting watch."})
	}
	report.Diagnostics = append(report.Diagnostics, deps.Codex(ctx, scopes)...)
	report.Ready = true
	for _, item := range report.Diagnostics {
		if item.Status == "error" {
			report.Ready = false
		}
	}
	return report
}
func (e *execution) presentDiagnostics(output io.Writer, report diagnosticReport) error {
	// All paths, including early invalid-config failures, share one display
	// boundary. Redact a serialized presentation copy rather than mutate
	// private configuration, identity or diagnostic inputs.
	report, masked, err := safeReport(report)
	if err != nil {
		return err
	}
	report.Redacted = masked
	if e.json {
		return writeJSON(output, report)
	}
	if report.Created {
		fmt.Fprintf(output, "Created %s (private configuration).\n", report.ConfigPath)
	} else {
		fmt.Fprintf(output, "Configuration: %s\n", report.ConfigPath)
	}
	if report.Redacted {
		fmt.Fprintln(output, "Credential-shaped strings are redacted.")
	}
	if report.Identity != nil {
		fmt.Fprintf(output, "GitHub identity: %s (id %d)\n", report.Identity.Login, report.Identity.ID)
	}
	for _, item := range report.Policies {
		fmt.Fprintf(output, "%s: requested=%s new=%s updated=%s; backend=%s feedback=%s; %s / %s\n", item.Scope, item.ReviewRequested, item.NewPullRequest, item.HeadUpdated, item.Backend, item.FeedbackBackend, item.Model, item.ReasoningEffort)
	}
	for _, item := range report.Diagnostics {
		fmt.Fprintf(output, "%s: %s", item.Status, item.Check)
		if item.Scope != "" {
			fmt.Fprintf(output, " (%s)", item.Scope)
		}
		fmt.Fprintf(output, " — %s\n", item.Message)
		if item.Correction != "" {
			fmt.Fprintf(output, "  %s\n", item.Correction)
		}
	}
	return nil
}

// Only disposable probe storage is created. Existing directories/configuration
// are not chmodded, and no scheduler, review or operating-system service starts.
func probeDirectory(path string) error {
	parent := path
	for {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return errors.New("state path is not a directory")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return err
		}
		parent = next
	}
	owned, err := os.MkdirTemp(parent, ".reviewd-probe-")
	if err != nil {
		return err
	}
	return os.Remove(owned)
}

// Retains the existing dependency-hook name for callers; probes native agents
// independently and only checks availability/version, never model or MCP access.
func probeCodex(ctx context.Context, scopes []config.Snapshot) []Diagnostic {
	var result []Diagnostic
	seen := map[string]bool{}
	if len(scopes) > 1 {
		scopes = scopes[1:]
	}
	for _, policy := range scopes {
		for _, prefix := range []string{"reviewer.", "feedback."} {
			if prefix == "feedback." && !policy.FeedbackMonitoringConfigured() && policy.Provenance()[prefix+"backend"].Layer == "built-in" {
				continue
			}
			backend := policy.String(prefix + "backend")
			if backend == "" {
				backend = "codex"
			}
			selected := policy.String(prefix + "executable")
			key := backend + "\x00" + selected
			if seen[key] {
				continue
			}
			seen[key] = true
			label := agent.Label(backend)
			executable, err := exec.LookPath(selected)
			if err != nil {
				result = append(result, Diagnostic{Check: label + " CLI", Status: "error", Message: "Configured native executable is unavailable.", Correction: "Install " + label + " or correct " + prefix + "executable."})
				continue
			}
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			version, err := exec.CommandContext(probe, executable, "--version").Output()
			cancel()
			if err != nil {
				result = append(result, Diagnostic{Check: label + " CLI", Status: "error", Message: "Native CLI version could not be read.", Correction: "Check the configured executable."})
				continue
			}
			result = append(result, Diagnostic{Check: label + " CLI", Status: "ok", Message: strings.TrimSpace(string(version))}, Diagnostic{Check: label + " settings", Status: "warning", Message: "Agents inherit normal native authentication, skills, MCP servers and tools. Doctor does not start an agent or validate provider/model access.", Correction: "Configure and authenticate " + label + " normally; use a manual review or feedback action to validate execution."})
		}
	}
	return result
}

const initialConfiguration = `# Personal reviewd configuration. Select repositories and action policies below.
# Skills run with your selected native agent configuration and tool permissions.
schema_version = 1

[github]
host = "github.com"
repositories = []

[reviewer]
skill = "review"

[actions]
new_pull_request = "ignore"
review_requested = "ask"
head_updated = "ask"
review_submitted = "notify"
review_comment_added = "notify"
`
