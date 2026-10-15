package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
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
	Scope           string `json:"scope"`
	ReviewRequested string `json:"review_requested"`
	NewPullRequest  string `json:"new_pull_request"`
	HeadUpdated     string `json:"head_updated"`
	PublishReview   string `json:"publish_review"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func policy(snapshot config.Snapshot) policyReport {
	scope := snapshot.Repository
	if scope == "" {
		scope = "global"
	}
	return policyReport{scope, snapshot.String("actions.review_requested"), snapshot.String("actions.new_pull_request"), snapshot.String("actions.head_updated"), snapshot.String("actions.publish_review"), snapshot.String("reviewer.model"), snapshot.String("reviewer.reasoning_effort")}
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
		configuration := strings.Replace(initialConfiguration, `model = "gpt-6.1-sol"`, "model = "+strconv.Quote(selected.String("reviewer.model")), 1)
		configuration = strings.Replace(configuration, `reasoning_effort = "high"`, "reasoning_effort = "+strconv.Quote(selected.String("reviewer.reasoning_effort")), 1)
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
	if runtime.GOOS == "darwin" {
		for _, scope := range scopes {
			if !scope.FeedbackMonitoringConfigured() || len(scope.Strings("feedback.authors")) == 0 || scope.FeedbackAction("review_submitted") == "ignore" {
				continue
			}
			correction := "Use a supported local runtime, the paired package helper and a cached Linux image with Git, project tools and offline dependencies. Linux checks do not establish native macOS-only checks."
			if scope.String("feedback.container_endpoint") == "" || scope.String("feedback.container_image") == "" {
				correction = "Set feedback.container_endpoint and feedback.container_image in operator global or exact-project configuration. " + correction
			}
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Check: "feedback command backend", Scope: policy(scope).Scope, Status: "warning", Message: "Modifying feedback execution is not connected in this build; its optional container dependency does not disable ordinary reviews. Container execution was not probed by doctor.", Correction: correction})
		}
	}
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
		fmt.Fprintf(output, "%s: requested=%s new=%s updated=%s publish=%s; %s / %s\n", item.Scope, item.ReviewRequested, item.NewPullRequest, item.HeadUpdated, item.PublishReview, item.Model, item.ReasoningEffort)
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

func probeCodex(ctx context.Context, scopes []config.Snapshot) []Diagnostic {
	if len(scopes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	root := scopes[0]
	result := []Diagnostic{}
	for _, scope := range scopes {
		name := policy(scope).Scope
		if _, err := skillPath(scope); err != nil {
			result = append(result, Diagnostic{Check: "review skill", Scope: name, Status: "error", Message: err.Error(), Correction: "Install the selected skill or set reviewer.skill_path to its readable SKILL.md."})
		} else {
			result = append(result, Diagnostic{Check: "review skill", Scope: name, Status: "ok", Message: "Selected installed instructions are readable."})
		}
	}
	executable, err := exec.LookPath(root.String("reviewer.executable"))
	if err != nil {
		return append(result, Diagnostic{Check: "Codex CLI", Status: "error", Message: "Configured Codex executable is unavailable.", Correction: "Install Codex CLI 0.162.1 or correct reviewer.executable."})
	}
	version, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.162.1" {
		return append(result, Diagnostic{Check: "Codex compatibility", Status: "error", Message: "Codex version is not the tested 0.162.1 protocol/capability baseline.", Correction: "Use Codex CLI 0.162.1; other versions require capability verification before support."})
	}
	worker, err := codex.StartRuntime(ctx, codex.RuntimeOptions{Executable: executable, AuthHome: root.String("reviewer.auth_home")})
	if err != nil {
		return append(result, Diagnostic{Check: "Codex account", Status: "error", Message: "Isolated Codex initialization failed.", Correction: "Sign in using Codex file authentication (auth.json) or supported provider authentication, then rerun doctor."})
	}
	accountErr := worker.CheckAccount(ctx)
	var models []codex.Model
	var readErr error
	if accountErr == nil {
		models, readErr = worker.Models(ctx)
	}
	closeErr := worker.Close()
	if accountErr != nil {
		result = append(result, Diagnostic{Check: "Codex account", Status: "error", Message: "Codex account metadata is missing, unsupported or unavailable.", Correction: "Check existing Codex file authentication or supported provider authentication and rerun doctor; actual provider acceptance is established by a review turn."})
	} else if readErr != nil {
		result = append(result, Diagnostic{Check: "Codex models", Status: "error", Message: "Account model discovery failed.", Correction: "Confirm Codex authentication/network access and rerun doctor."})
	} else {
		for _, scope := range scopes {
			item := Diagnostic{Check: "Codex model", Scope: policy(scope).Scope, Status: "ok", Message: "Configured model and reasoning effort are advertised; no fallback was selected."}
			if err := codex.ValidateModelSelection(models, scope.String("reviewer.model"), scope.String("reviewer.reasoning_effort")); err != nil {
				item.Status = "error"
				item.Message = err.Error()
				item.Correction = "Choose a model/effort pair available to your Codex account."
			}
			result = append(result, item)
		}
	}
	if closeErr != nil {
		result = append(result, Diagnostic{Check: "Codex cleanup", Status: "error", Message: "Owned worker cleanup failed.", Correction: "Inspect remaining reviewd worker processes and temporary storage before starting reviews."})
	}
	return result
}
func skillPath(scope config.Snapshot) (string, error) {
	selected := scope.String("reviewer.skill_path")
	if selected != "" {
		return readableSkill(selected)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("user home unavailable for installed skill discovery")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	for _, root := range []string{filepath.Join(codexHome, "skills"), filepath.Join(home, ".agents", "skills")} {
		path := filepath.Join(root, scope.String("reviewer.skill"), "SKILL.md")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		return readableSkill(path)
	}
	return "", errors.New("selected installed review skill was not found")
}
func readableSkill(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("invalid skill path")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("selected skill cannot be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 1024*1024 {
		return "", errors.New("selected skill must be a nonempty regular instruction file under 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 || strings.TrimSpace(string(data)) == "" {
		return "", errors.New("selected skill instructions are empty, oversized or unreadable")
	}
	return path, nil
}

const initialConfiguration = `# Personal reviewd configuration. No repositories or automatic publication
# are enabled by initialization. Run reviewd doctor after selecting your scope.
schema_version = 1

[github]
host = "github.com"
repositories = []

[reviewer]
model = "gpt-6.1-sol"
reasoning_effort = "high"
skill = "requesting-code-review"

[actions]
new_pull_request = "ignore"
review_requested = "ask"
head_updated = "ask"
review_submitted = "notify"
review_comment_added = "notify"
publish_review = "ask"
`
