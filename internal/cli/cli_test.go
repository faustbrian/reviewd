package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/github"
)

// QA-14/16/X06: the actual command boundary must expose effective policy and
// provenance without requiring provider auth for explicitly offline explain.
func TestOfflineExplainResolvesWithoutGitHubOrCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version = 1\n[projects.\"shipitfi/api\".actions]\nreview_requested = 'notify'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	githubCalls, codexCalls := 0, 0
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		githubCalls++
		return nil, errors.New("fixture must remain offline")
	}, Codex: func(context.Context, []config.Snapshot) []Diagnostic { codexCalls++; return nil }}
	var output, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"config", "explain", "shipitfi/api", "--offline", "--config", path, "--json", "--reasoning-effort", "medium"}, &output, &diagnostics, deps)
	if code != 0 {
		t.Fatalf("explain failed code=%d diagnostics=%s", code, diagnostics.String())
	}
	var result struct {
		SchemaVersion         int `json:"schema_version"`
		RepositoryPreferences struct {
			State string `json:"state"`
		} `json:"repository_preferences"`
		Configuration config.Snapshot `json:"configuration"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.RepositoryPreferences.State != "not_checked" || result.Configuration.String("actions.review_requested") != "notify" || result.Configuration.String("reviewer.reasoning_effort") != "medium" || result.Configuration.Provenance()["reviewer.reasoning_effort"].Layer != "cli" {
		t.Fatalf("command lost effective policy/provenance: %s", output.String())
	}
	if githubCalls != 0 || codexCalls != 0 || diagnostics.Len() != 0 {
		t.Fatal("offline explanation attempted prerequisites or mixed diagnostics into output")
	}
}

func TestExplainRedactsValidInstructionValuesWithoutChangingPrivateSnapshot(t *testing.T) {
	canary := "ghp_" + strings.Repeat("A", 36)
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte("schema_version=1\n[review]\ninstructions='Never expose " + canary + "'\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	private, err := config.Resolve(document, "owner/repo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"config", "explain", "owner/repo", "--offline", "--config", path}
		if asJSON {
			args = append(args, "--json")
		}
		var out, diagnostics bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostics, Dependencies{}); code != 0 {
			t.Fatalf("explain failed: %s", diagnostics.String())
		}
		if strings.Contains(out.String(), canary) || strings.Contains(diagnostics.String(), canary) || !strings.Contains(out.String(), "redacted") {
			t.Fatal("valid configuration presentation exposed a credential-shaped value")
		}
		if asJSON {
			var report struct {
				Configuration struct {
					Redacted    bool           `json:"redacted"`
					Fingerprint string         `json:"fingerprint"`
					Values      map[string]any `json:"values"`
				} `json:"configuration"`
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Configuration.Redacted || report.Configuration.Fingerprint != private.Fingerprint {
				t.Fatal("redacted explanation concealed its incomplete values or lost configuration identity")
			}
		}
	}
	if !strings.Contains(private.String("review.instructions"), canary) {
		t.Fatal("display redaction modified private review instructions")
	}
}

func TestOnlineExplainComposesPresentTrustedPreferencesAndRejectsInvalidOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[projects.\"owner/repo\".reviewer]\nreasoning_effort='medium'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base, tree, blob := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	canary := "sk-proj-" + strings.Repeat("B", 40)
	var invalid atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("explanation performed a mutation")
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","default_branch":"main"}`)
		case "/repos/owner/repo/commits/main":
			fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, base, tree)
		case "/repos/owner/repo/git/trees/" + tree:
			fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[{"path":".reviewd","mode":"100644","type":"blob","sha":%q}]}`, tree, blob)
		case "/repos/owner/repo/git/blobs/" + blob:
			preferences := "schema_version=1\n[reviewer]\nreasoning_effort='low'\n[review]\ninstructions='trusted guidance " + canary + "'\n"
			if invalid.Load() {
				preferences = "schema_version=1\n[actions]\npublish_review='auto'\n"
			}
			fmt.Fprintf(w, `{"sha":%q,"encoding":"base64","size":%d,"content":%q}`, blob, len(preferences), base64.StdEncoding.EncodeToString([]byte(preferences)))
		default:
			t.Errorf("unexpected source path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		return github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	}, Codex: func(context.Context, []config.Snapshot) []Diagnostic {
		t.Error("explanation started Codex")
		return nil
	}}
	var out, diagnostics bytes.Buffer
	args := []string{"config", "explain", "owner/repo", "--config", path, "--json", "--reasoning-effort", "high"}
	if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
		t.Fatalf("present explanation failed: %s", diagnostics.String())
	}
	var report struct {
		Configuration         presentedConfiguration `json:"configuration"`
		RepositoryPreferences preferenceReport       `json:"repository_preferences"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	settings := report.Configuration
	if report.RepositoryPreferences.State != "present" || settings.Values["reviewer.reasoning_effort"] != "high" || settings.Provenance["reviewer.reasoning_effort"].Layer != "cli" || settings.Provenance["review.instructions"].Layer != "repository" || settings.Provenance["review.instructions"].Revision != base || !strings.Contains(settings.Values["review.instructions"].(string), "trusted guidance") {
		t.Fatal("online CLI dropped trusted data or composed incorrect precedence/provenance")
	}
	if !settings.Redacted || strings.Contains(out.String(), canary) {
		t.Fatal("trusted instruction presentation exposed a credential-shaped value")
	}
	// Explicit redacted output must not masquerade as an immutable job snapshot.
	encoded, _ := json.Marshal(settings)
	var restored config.Snapshot
	if err := json.Unmarshal(encoded, &restored); err == nil {
		t.Fatal("masked display could be silently restored as private run policy")
	}
	invalid.Store(true)
	out.Reset()
	diagnostics.Reset()
	if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 2 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "disallowed") {
		t.Fatal("invalid present preferences silently became absent/operator-only policy")
	}
}

func TestOnlineExplainRedactsPreferenceMetadataInBothOutputModes(t *testing.T) {
	canary := "ghp_" + strings.Repeat("E", 36)
	base, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`)
		case "/repos/owner/repo":
			fmt.Fprintf(w, `{"id":1,"full_name":"owner/repo","default_branch":%q}`, canary)
		case "/repos/owner/repo/commits/" + canary:
			fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, base, tree)
		case "/repos/owner/repo/git/trees/" + tree:
			fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, tree)
		default:
			t.Error("unexpected request")
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		return github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	}}
	for _, asJSON := range []bool{false, true} {
		args := []string{"config", "explain", "owner/repo", "--config", path}
		if asJSON {
			args = append(args, "--json")
		}
		var out, diagnostics bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
			t.Fatalf("online explain status=%d", code)
		}
		if strings.Contains(out.String(), canary) || strings.Contains(diagnostics.String(), canary) || !strings.Contains(out.String(), "redacted") {
			t.Error("explanation metadata exposed a credential-shaped branch")
		}
		if asJSON {
			var report struct {
				Redacted    bool             `json:"redacted"`
				Preferences preferenceReport `json:"repository_preferences"`
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if !report.Redacted || report.Preferences.Branch != "<redacted>" {
				t.Error("metadata redaction was not explicit")
			}
		}
	}
}

func TestOnlineExplainAccountHeaderUsesPresentedHost(t *testing.T) {
	canary := "sk-" + strings.Repeat("f", 30)
	base, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[github]\nhost='"+canary+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`)
		case "/repos/owner/repo":
			fmt.Fprint(w, `{"id":1,"full_name":"owner/repo","default_branch":"main"}`)
		case "/repos/owner/repo/commits/main":
			fmt.Fprintf(w, `{"sha":%q,"commit":{"tree":{"sha":%q}}}`, base, tree)
		case "/repos/owner/repo/git/trees/" + tree:
			fmt.Fprintf(w, `{"sha":%q,"truncated":false,"tree":[]}`, tree)
		default:
			t.Error("unexpected request")
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()
	deps := Dependencies{GitHub: func(_ context.Context, host string) (*github.Client, error) {
		return github.New(github.Options{Host: host, BaseURL: peer.URL, Token: "fixture-token"})
	}}
	for _, asJSON := range []bool{false, true} {
		args := []string{"config", "explain", "owner/repo", "--config", path}
		if asJSON {
			args = append(args, "--json")
		}
		var out, diagnostics bytes.Buffer
		if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 0 {
			t.Fatalf("explanation failed %d", code)
		}
		if strings.Contains(out.String(), canary) || strings.Contains(diagnostics.String(), canary) || !strings.Contains(out.String(), "fixture-user") || !strings.Contains(out.String(), "owner/repo") {
			t.Error("account-header host bypassed presentation or useful identity was lost")
		}
	}
}

func TestProfileStateIdentityIsStableAcrossCreationAndPathAliases(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(alias, "nested", "config.toml")
	snapshot, err := config.ResolveDefaults(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stateDirectory(snapshot, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("schema_version=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := stateDirectory(snapshot, path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := stateDirectory(snapshot, filepath.Join(real, "nested", "config.toml"))
	if err != nil || before != after || after != other {
		t.Fatalf("profile inbox moved across config creation/alias: %q %q %q (%v)", before, after, other, err)
	}
}

func TestInvalidDoctorConfigurationStaysStructuredAndDoesNotProbeAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n[actions]\npublish_review='invalid'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		t.Error("invalid config contacted GitHub")
		return nil, errors.New("no")
	}, Codex: func(context.Context, []config.Snapshot) []Diagnostic {
		t.Error("invalid config started Codex")
		return nil
	}}
	var out, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--config", path, "--json"}, &out, &diagnostics, deps); code != 1 {
		t.Fatalf("invalid doctor code=%d", code)
	}
	var report diagnosticReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || len(report.Diagnostics) != 1 || report.Diagnostics[0].Correction == "" || diagnostics.Len() != 0 {
		t.Fatalf("invalid configuration did not remain actionable JSON: %s", out.String())
	}
}

func TestDoctorRedactsAllPresentationPathsIncludingInvalidConfiguration(t *testing.T) {
	canary := "ghp_" + strings.Repeat("C", 36)
	path := filepath.Join(t.TempDir(), canary, "config.toml")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return nil, errors.New("fixture offline") }, Codex: func(context.Context, []config.Snapshot) []Diagnostic { return nil }}
	for _, configuration := range []string{"schema_version=1\n", "schema_version=\n"} {
		if err := os.WriteFile(path, []byte(configuration), 0600); err != nil {
			t.Fatal(err)
		}
		for _, asJSON := range []bool{false, true} {
			args := []string{"doctor", "--config", path}
			if asJSON {
				args = append(args, "--json")
			}
			var out, diagnostics bytes.Buffer
			if code := Run(context.Background(), args, &out, &diagnostics, deps); code != 1 {
				t.Fatalf("doctor failure status=%d", code)
			}
			if strings.Contains(out.String(), canary) || strings.Contains(diagnostics.String(), canary) || !strings.Contains(out.String(), "redacted") {
				t.Error("diagnostic presentation exposed credential-shaped content")
			}
		}
	}
}

func TestCancelledDoctorRetainsStructuredDiagnosticsAndReturnsInterruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { cancel(); return nil, context.Canceled }, Codex: func(context.Context, []config.Snapshot) []Diagnostic { return nil }}
	var out, diagnostics bytes.Buffer
	if code := Run(ctx, []string{"doctor", "--config", path, "--json"}, &out, &diagnostics, deps); code != 130 {
		t.Fatalf("interrupted diagnostics returned %d instead of 130", code)
	}
	var report diagnosticReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Ready {
		t.Fatal("cancelled diagnostics were missing or claimed readiness")
	}
}

func TestMissingCodexAndSkillProduceIndependentCorrections(t *testing.T) {
	document, err := config.Parse([]byte("schema_version=1\n[reviewer]\nexecutable='/definitely/missing/reviewd-codex'\nskill_path='/definitely/missing/reviewd-skill.md'\n"), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := config.ResolveDefaults(document, nil)
	if err != nil {
		t.Fatal(err)
	}
	issues := probeCodex(context.Background(), []config.Snapshot{snapshot})
	if len(issues) != 2 {
		t.Fatalf("missing prerequisites lost independent corrections: %+v", issues)
	}
	for _, item := range issues {
		if item.Status != "error" || item.Correction == "" {
			t.Fatalf("missing prerequisite not actionable: %+v", item)
		}
	}
}

func TestInitUnavailableAccountsStillCreateSafeConfigWithVisibleWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return nil, errors.New("account unavailable") }, Codex: func(context.Context, []config.Snapshot) []Diagnostic {
		return []Diagnostic{{Check: "Codex account", Status: "error", Message: "No login", Correction: "Sign in."}}
	}}
	var out, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"init", "--config", path, "--json"}, &out, &diagnostics, deps)
	var report diagnosticReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.Created || report.Ready || report.Identity != nil {
		t.Fatalf("safe initialization conflated creation with readiness: %s (%d)", out.String(), code)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `publish_review = "ask"`) {
		t.Fatal("missing account enabled unsafe initialization")
	}
}

func TestInitPersistsExplicitModelAndEffortSelections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) { return nil, errors.New("fixture offline") }, Codex: func(_ context.Context, scopes []config.Snapshot) []Diagnostic {
		if scopes[0].String("reviewer.model") != "selected-model" || scopes[0].String("reviewer.reasoning_effort") != "medium" {
			t.Error("init did not diagnose selected settings")
		}
		return nil
	}}
	var out, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--config", path, "--model", "selected-model", "--reasoning-effort", "medium", "--json"}, &out, &diagnostics, deps); code != 0 {
		t.Fatalf("init failed: %s", diagnostics.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := config.ResolveDefaults(document, nil)
	if err != nil || snapshot.String("reviewer.model") != "selected-model" || snapshot.String("reviewer.reasoning_effort") != "medium" {
		t.Fatal("created configuration disagrees with initialization policy")
	}
}

func TestDoctorReturnsActionableFailureInJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("schema_version = 1\n[reviewer]\nmodel = 'unavailable-model'\n[state]\ndirectory = '"+filepath.ToSlash(t.TempDir())+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Error("doctor discovered or reviewed PRs")
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`)
	}))
	defer peer.Close()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		return github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	}, Codex: func(_ context.Context, scopes []config.Snapshot) []Diagnostic {
		if len(scopes) != 1 || scopes[0].String("reviewer.model") != "unavailable-model" {
			t.Error("doctor did not probe the configured model")
		}
		return []Diagnostic{{Check: "codex model", Status: "error", Message: "Selected model unavailable", Correction: "Choose an advertised model and reasoning effort."}}
	}}
	var output, diagnostics bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--config", path, "--json"}, &output, &diagnostics, deps)
	if code != 1 {
		t.Fatalf("blocking doctor result returned %d: %s", code, diagnostics.String())
	}
	var report struct {
		Ready       bool             `json:"ready"`
		Diagnostics []Diagnostic     `json:"diagnostics"`
		Identity    *github.Identity `json:"github_identity"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || report.Identity == nil || report.Identity.ID != 42 {
		t.Fatal("doctor conflated failure with readiness or lost posting identity")
	}
	found := false
	for _, item := range report.Diagnostics {
		if item.Check == "codex model" && item.Status == "error" && item.Correction != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("blocking failure has no concrete corrective action")
	}
}

func TestInitCreatesSafeConfigWithoutOverwritingExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned", "config.toml")
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":42,"login":"fixture-user"}`) }))
	defer peer.Close()
	deps := Dependencies{GitHub: func(context.Context, string) (*github.Client, error) {
		return github.New(github.Options{Host: "github.com", BaseURL: peer.URL, Token: "fixture-token"})
	}, Codex: func(context.Context, []config.Snapshot) []Diagnostic { return nil }}
	var output, diagnostics bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--config", path, "--json"}, &output, &diagnostics, deps); code != 0 {
		t.Fatalf("init failed %d: %s", code, diagnostics.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := config.Parse(data, path)
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := config.ResolveDefaults(document, nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.String("actions.publish_review") != "ask" || len(defaults.Strings("github.repositories")) != 0 {
		t.Fatal("initialization enabled automation or customer-repository monitoring")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration was not created privately")
	}
	if code := Run(context.Background(), []string{"init", "--config", path}, &output, &diagnostics, deps); code == 0 {
		t.Fatal("init claimed to overwrite an existing configuration")
	}
	preserved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, preserved) {
		t.Fatal("existing operator configuration was modified")
	}
}
