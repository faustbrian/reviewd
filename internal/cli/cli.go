// Package cli presents application operations; it never owns review policy.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/faustbrian/reviewd/internal/activity"
	"github.com/faustbrian/reviewd/internal/agent"
	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/notify"
	"github.com/spf13/cobra"
)

type Diagnostic struct {
	Check      string `json:"check"`
	Scope      string `json:"scope,omitempty"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Correction string `json:"correction,omitempty"`
}
type Dependencies struct {
	LaunchAgent func(context.Context, agent.Request) (agent.Output, error)
	Version     string
	Input       io.Reader
	Desktop     notify.Sink
	GitHub      func(context.Context, string) (*github.Client, error)
	Codex       func(context.Context, []config.Snapshot) []Diagnostic
	OpenURL     func(context.Context, string) error
}
type commandError struct {
	code    int
	message string
}

func (e *commandError) Error() string        { return e.message }
func failure(code int, message string) error { return &commandError{code, message} }

type execution struct {
	path, profile, model, effort string
	json                         bool
	resultCode                   int
}
type preferenceReport struct {
	State   string `json:"state"`
	Branch  string `json:"branch,omitempty"`
	BaseSHA string `json:"base_sha,omitempty"`
	Note    string `json:"note,omitempty"`
}
type explainReport struct {
	SchemaVersion         int                    `json:"schema_version"`
	GitHubIdentity        *github.Identity       `json:"github_identity,omitempty"`
	RepositoryPreferences preferenceReport       `json:"repository_preferences"`
	Configuration         presentedConfiguration `json:"configuration"`
	Redacted              bool                   `json:"redacted,omitempty"`
}

// Explanation is a presentation, never a replacement frozen job snapshot.
// The original identity is retained; explicit redaction prevents silently
// restoring masked values as if they were the private run configuration.
type presentedConfiguration struct {
	SchemaVersion int                      `json:"schema_version"`
	Repository    string                   `json:"repository"`
	Fingerprint   string                   `json:"fingerprint"`
	Values        map[string]any           `json:"values"`
	Provenance    map[string]config.Origin `json:"provenance"`
	Redacted      bool                     `json:"redacted,omitempty"`
}

func presentConfiguration(snapshot config.Snapshot) presentedConfiguration {
	result := presentedConfiguration{SchemaVersion: 1, Repository: redact(snapshot.Repository), Fingerprint: snapshot.Fingerprint, Values: snapshot.Values(), Provenance: snapshot.Provenance()}
	clean := func(value string) string {
		safe := redact(value)
		if safe != value {
			result.Redacted = true
		}
		return safe
	}
	result.Repository = clean(snapshot.Repository)
	for key, value := range result.Values {
		switch typed := value.(type) {
		case string:
			result.Values[key] = clean(typed)
		case []string:
			for index, item := range typed {
				typed[index] = clean(item)
			}
			result.Values[key] = typed
		}
	}
	for key, origin := range result.Provenance {
		origin.Source = clean(origin.Source)
		result.Provenance[key] = origin
	}
	return result
}

func Run(ctx context.Context, args []string, output, diagnostics io.Writer, deps Dependencies) int {
	if deps.Version == "" {
		deps.Version = "0.1.0-dev"
	}
	if deps.GitHub == nil {
		deps.GitHub = github.FromCLI
	}
	if deps.Codex == nil {
		deps.Codex = probeCodex
	}
	options := &execution{profile: "default"}
	root := &cobra.Command{Use: "reviewd", Short: "A personal GitHub review inbox", Version: deps.Version, SilenceErrors: true, SilenceUsage: true}
	if deps.Input != nil {
		root.SetIn(deps.Input)
	}
	root.SetOut(output)
	root.SetErr(diagnostics)
	root.SetArgs(args)
	root.CompletionOptions.DisableDefaultCmd = true
	flags := root.PersistentFlags()
	flags.StringVar(&options.path, "config", "", "Explicit global configuration path")
	flags.StringVar(&options.profile, "profile", "default", "Named local profile")
	flags.BoolVar(&options.json, "json", false, "Emit stable JSON without presentation text")
	flags.StringVar(&options.model, "model", "", "Override this command's agent model (reviewer or feedback handler)")
	flags.StringVar(&options.effort, "reasoning-effort", "", "Override reasoning effort for this command")
	configure := &cobra.Command{Use: "config", Short: "Inspect effective configuration"}
	offline := false
	explain := &cobra.Command{Use: "explain <owner/repo>", Short: "Explain effective settings and provenance", Args: cobra.ExactArgs(1)}
	explain.Flags().BoolVar(&offline, "offline", false, "Skip trusted repository preferences; report the incomplete layer explicitly")
	explain.RunE = func(command *cobra.Command, args []string) error {
		document, path, err := options.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		overrides := options.overrides(command)
		defaults, err := config.ResolveDefaults(document, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		var identity *github.Identity
		preferences := preferenceReport{State: "not_checked", Note: "Offline: repository preferences were not consulted; this is not a full run configuration."}
		var trusted *config.RepositoryPreferences
		if !offline {
			client, err := deps.GitHub(command.Context(), defaults.String("github.host"))
			if err != nil {
				return failure(1, err.Error()+"; use --offline for operator-only settings")
			}
			actor, err := client.Identity(command.Context())
			if err != nil {
				return failure(1, err.Error())
			}
			identity = &actor
			source, err := client.DefaultPreferences(command.Context(), args[0])
			if err != nil {
				return failure(1, err.Error())
			}
			preferences = preferenceReport{State: "absent", Branch: source.Branch, BaseSHA: source.BaseSHA, Note: "Project explanation uses the default branch; PR jobs use their exact target-base SHA."}
			trusted = source.Preferences
			if trusted != nil {
				preferences.State = "present"
			}
		}
		resolved, err := config.Resolve(document, args[0], trusted, overrides)
		if err != nil {
			return failure(2, err.Error())
		}
		presented := presentConfiguration(resolved)
		report, masked, err := safeReport(explainReport{SchemaVersion: 1, GitHubIdentity: identity, RepositoryPreferences: preferences, Configuration: presented})
		if err != nil {
			return err
		}
		report.Redacted = masked || report.Configuration.Redacted
		preferences = report.RepositoryPreferences
		identity = report.GitHubIdentity
		presented = report.Configuration
		if options.json {
			return writeJSON(command.OutOrStdout(), report)
		}
		if identity != nil {
			fmt.Fprintf(command.OutOrStdout(), "GitHub: %s on %s\n", identity.Login, presented.Values["github.host"])
		}
		fmt.Fprintf(command.OutOrStdout(), "Configuration: %s\nRepository: %s\nRepository preferences: %s", redact(path), presented.Repository, preferences.State)
		if preferences.BaseSHA != "" {
			fmt.Fprintf(command.OutOrStdout(), " (%s@%s)", preferences.Branch, preferences.BaseSHA)
		}
		fmt.Fprintln(command.OutOrStdout())
		if preferences.Note != "" {
			fmt.Fprintln(command.OutOrStdout(), preferences.Note)
		}
		if report.Redacted {
			fmt.Fprintln(command.OutOrStdout(), "Credential-shaped strings are redacted; this display is not a frozen job snapshot.")
		}
		values, origins := presented.Values, presented.Provenance
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, _ := json.Marshal(values[key])
			fmt.Fprintf(command.OutOrStdout(), "%s = %s [%s]\n", key, value, origins[key].Layer)
		}
		return nil
	}
	configure.AddCommand(explain)
	root.AddCommand(configure)
	root.AddCommand(options.doctorCommand(deps), options.initCommand(deps))
	root.AddCommand(options.reviewCommand(deps), options.showCommand(), options.inboxCommand(deps), options.syncCommand(deps), options.followCommand(true), options.followCommand(false), options.watchCommand(deps))
	root.AddCommand(options.decisionCommand(true), options.decisionCommand(false), options.projectPauseCommand(true), options.projectPauseCommand(false), options.snoozeCommand(), options.unsnoozeCommand())
	root.AddCommand(options.skillCommand("feedback", deps))
	root.AddCommand(options.cancelCommand(), options.openCommand(deps))
	root.AddCommand(options.recoveryCommand(), options.cleanupCommand())
	completion := &cobra.Command{Use: "completion <bash|zsh|fish>", Short: "Print shell completion without installing it", Args: cobra.ExactArgs(1)}
	completion.RunE = func(command *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return root.GenBashCompletionV2(command.OutOrStdout(), true)
		case "zsh":
			return root.GenZshCompletion(command.OutOrStdout())
		case "fish":
			return root.GenFishCompletion(command.OutOrStdout(), true)
		default:
			return failure(2, "supported completion shells are bash, zsh and fish")
		}
	}
	root.AddCommand(completion)
	if err := root.ExecuteContext(ctx); err != nil {
		code := 2
		var owned *commandError
		if errors.As(err, &owned) {
			code = owned.code
		}
		if ctx.Err() != nil {
			code = 130
		}
		fmt.Fprintln(diagnostics, redact(err.Error()))
		return code
	}
	if ctx.Err() != nil {
		return 130
	}
	return options.resultCode
}
func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return failure(1, "unable to write command output")
	}
	return nil
}

// The shared report boundary covers metadata as well as payload fields. Its
// typed presentation copy never replaces private snapshots or application state.
func safeReport[T any](value T) (T, bool, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		var empty T
		return empty, false, failure(1, "unable to prepare command output")
	}
	safe := redact(string(encoded))
	if safe == string(encoded) {
		return value, false, nil
	}
	var presented T
	if err := json.Unmarshal([]byte(safe), &presented); err != nil {
		return presented, false, failure(1, "unable to prepare safe command output")
	}
	return presented, true, nil
}
func (e *execution) overrides(command *cobra.Command) map[string]any {
	result := map[string]any{}
	if command.Flags().Changed("model") {
		result["reviewer.model"] = e.model
	}
	if command.Flags().Changed("reasoning-effort") {
		result["reviewer.reasoning_effort"] = e.effort
	}
	return result
}
func (e *execution) configPath() (string, error) {
	if !profilePattern.MatchString(e.profile) || e.profile == "." || e.profile == ".." {
		return "", errors.New("profile must be an exact local name using letters, digits, dots, hyphens or underscores")
	}
	path := e.path
	if path == "" {
		path = os.Getenv("REVIEWD_CONFIG")
	}
	if path == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return "", errors.New("user configuration directory is unavailable; supply --config")
		}
		if !filepath.IsAbs(root) {
			return "", errors.New("user configuration directory must be absolute; correct XDG_CONFIG_HOME or supply --config")
		}
		path = filepath.Join(root, "reviewd", "config.toml")
		if e.profile != "default" {
			path = filepath.Join(root, "reviewd", "profiles", e.profile, "config.toml")
		}
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Abs(path)
}
func (e *execution) load(command *cobra.Command, allowMissing bool) (*config.Document, string, error) {
	path, err := e.configPath()
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if !allowMissing && (e.path != "" || os.Getenv("REVIEWD_CONFIG") != "") {
			return nil, path, errors.New("explicit configuration file is missing; run reviewd init with the same --config path")
		}
		return nil, path, nil
	}
	if err != nil {
		return nil, path, errors.New("configuration file cannot be read; check its path and permissions")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, path, errors.New("configuration must be a regular readable file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return nil, path, errors.New("configuration file cannot be read; check its path and permissions")
	}
	document, err := config.Parse(data, path)
	return document, path, err
}
func stateDirectory(snapshot config.Snapshot, configPath string) (string, error) {
	if selected := snapshot.String("state.directory"); selected != "" {
		return filepath.Abs(selected)
	}
	// Profiles use canonical configuration location, not config contents: reload
	// must never move a profile's durable inbox or scheduler lock.
	canonical, err := canonicalLocation(configPath)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(canonical))
	profileID := hex.EncodeToString(hash[:8])
	root := os.Getenv("XDG_STATE_HOME")
	if runtime.GOOS == "darwin" {
		root = ""
	}
	if root != "" && !filepath.IsAbs(root) {
		return "", errors.New("XDG_STATE_HOME must be an absolute directory")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if runtime.GOOS == "darwin" {
			root = filepath.Join(home, "Library", "Application Support")
		} else {
			root = filepath.Join(home, ".local", "state")
		}
	}
	return filepath.Join(root, "reviewd", "profiles", profileID), nil
}

// Resolve the nearest existing ancestor as well as the missing suffix. A
// profile keeps its state identity before and after init creates the file.
func canonicalLocation(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

var profilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func redact(text string) string { return activity.Redact(text) }
