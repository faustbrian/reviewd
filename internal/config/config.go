// Package config resolves operator settings and immutable trusted-base
// preferences. It never reads repository working trees or authentication.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type scope uint8

const (
	globalScope scope = 1 << iota
	projectScope
	repositoryScope
	cliScope
)

type definition struct {
	value  any
	scopes scope
}

var definitions = map[string]definition{
	"schema_version":                {int64(1), globalScope | repositoryScope},
	"github.host":                   {"github.com", globalScope | cliScope},
	"github.repositories":           {[]string{}, globalScope | cliScope},
	"github.watch_review_requests":  {true, globalScope | cliScope},
	"github.watch_own_prs":          {true, globalScope | cliScope},
	"watch.poll_interval":           {"60s", globalScope | cliScope},
	"watch.debounce":                {"30s", globalScope | projectScope | cliScope},
	"watch.include_drafts":          {false, globalScope | projectScope | cliScope},
	"reviewer.model":                {"gpt-6.1-sol", globalScope | projectScope | cliScope},
	"reviewer.reasoning_effort":     {"high", globalScope | projectScope | repositoryScope | cliScope},
	"reviewer.skill":                {"requesting-code-review", globalScope | projectScope | cliScope},
	"reviewer.skill_path":           {"", globalScope | projectScope | cliScope},
	"reviewer.executable":           {"codex", globalScope | cliScope},
	"reviewer.auth_home":            {"", globalScope | cliScope},
	"limits.max_concurrent_reviews": {int64(2), globalScope | cliScope},
	"limits.review_timeout":         {"30m", globalScope | projectScope | cliScope},
	"actions.new_pull_request":      {"ignore", globalScope | projectScope | cliScope},
	"actions.review_requested":      {"ask", globalScope | projectScope | cliScope},
	"actions.head_updated":          {"ask", globalScope | projectScope | cliScope},
	"actions.review_submitted":      {"notify", globalScope | projectScope | cliScope},
	"actions.review_comment_added":  {"notify", globalScope | projectScope | cliScope},
	"actions.publish_review":        {"ask", globalScope | projectScope | cliScope},
	"notifications.desktop":         {true, globalScope | projectScope | cliScope},
	"notifications.terminal":        {true, globalScope | projectScope | cliScope},
	"notifications.quiet_start":     {"", globalScope | projectScope | cliScope},
	"notifications.quiet_end":       {"", globalScope | projectScope | cliScope},
	"notifications.timezone":        {"Local", globalScope | projectScope | cliScope},
	"notifications.group_interval":  {"5s", globalScope | projectScope | cliScope},
	"review.instructions":           {"", globalScope | projectScope | repositoryScope | cliScope},
	"state.directory":               {"", globalScope | cliScope},
	"state.retention_days":          {int64(30), globalScope | cliScope},
}

type Document struct {
	fields   map[string]any
	projects map[string]map[string]any
	source   string
}
type RepositoryPreferences struct {
	BaseSHA string
	Data    []byte
}
type Origin struct {
	Layer    string `json:"layer"`
	Source   string `json:"source,omitempty"`
	Revision string `json:"revision,omitempty"`
}
type Snapshot struct {
	Repository  string `json:"repository"`
	Fingerprint string `json:"fingerprint"`
	values      map[string]any
	origins     map[string]Origin
}

func (s Snapshot) Values() map[string]any { return cloneValues(s.values) }
func (s Snapshot) Provenance() map[string]Origin {
	result := make(map[string]Origin, len(s.origins))
	for key, value := range s.origins {
		result[key] = value
	}
	return result
}
func (s Snapshot) String(key string) string { value, _ := s.values[key].(string); return value }
func (s Snapshot) Bool(key string) bool     { value, _ := s.values[key].(bool); return value }
func (s Snapshot) Int(key string) int64     { value, _ := s.values[key].(int64); return value }
func (s Snapshot) Strings(key string) []string {
	value, _ := s.values[key].([]string)
	return append([]string{}, value...)
}
func (s Snapshot) Duration(key string) time.Duration {
	value, _ := time.ParseDuration(s.String(key))
	return value
}

type snapshotWire struct {
	SchemaVersion int               `json:"schema_version"`
	Repository    string            `json:"repository"`
	Fingerprint   string            `json:"fingerprint"`
	Values        map[string]any    `json:"values"`
	Provenance    map[string]Origin `json:"provenance"`
}

func (s Snapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(snapshotWire{1, s.Repository, s.Fingerprint, s.values, s.origins})
}
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	*s = Snapshot{}
	var wire snapshotWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return errors.New("invalid configuration snapshot JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("configuration snapshot contains trailing data")
	}
	if wire.SchemaVersion != 1 || len(wire.Values) != len(definitions) || len(wire.Provenance) != len(definitions) {
		return errors.New("configuration snapshot has an unsupported or incomplete schema")
	}
	identity, err := Repository("github.com", wire.Repository)
	if wire.Repository != "" && (err != nil || identity != wire.Repository) {
		return errors.New("configuration snapshot has a noncanonical repository identity")
	}
	values := make(map[string]any, len(definitions))
	for key, value := range wire.Values {
		if number, ok := value.(json.Number); ok {
			integer, err := number.Int64()
			if err != nil {
				return fmt.Errorf("snapshot %s must be an integer", safeKey(key))
			}
			value = integer
		}
		normalized, err := normalize(key, value, globalScope)
		if err != nil {
			return err
		}
		values[key] = normalized
		origin, ok := wire.Provenance[key]
		if !ok || !oneOf(origin.Layer, "built-in", "global", "repository", "project", "cli") || (wire.Repository == "" && (origin.Layer == "repository" || origin.Layer == "project")) {
			return errors.New("configuration snapshot has invalid provenance")
		}
		if origin.Layer == "repository" && !revisionPattern.MatchString(origin.Revision) {
			return errors.New("configuration snapshot repository provenance has no immutable revision")
		}
		allowed := map[string]scope{"built-in": globalScope, "global": globalScope, "repository": repositoryScope, "project": projectScope, "cli": cliScope}[origin.Layer]
		if definitions[key].scopes&allowed == 0 {
			return errors.New("configuration snapshot provenance grants a disallowed setting")
		}
	}
	if err := validate(values); err != nil {
		return err
	}
	fingerprint, err := snapshotHash(wire.Repository, values)
	if err != nil || fingerprint != wire.Fingerprint {
		return errors.New("configuration snapshot fingerprint does not match its values")
	}
	*s = Snapshot{Repository: wire.Repository, Fingerprint: fingerprint, values: values, origins: wire.Provenance}
	return nil
}
func snapshotHash(repository string, values map[string]any) (string, error) {
	encoded, err := json.Marshal(struct {
		Repository string
		Values     map[string]any
	}{repository, values})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func Parse(data []byte, source string) (*Document, error) {
	fields, projects, err := parse(data, source, globalScope)
	if err != nil {
		return nil, err
	}
	document := &Document{fields: fields, projects: make(map[string]map[string]any), source: source}
	base, err := ResolveDefaults(document, nil)
	if err != nil {
		return nil, err
	}
	for name, project := range projects {
		canonical, err := Repository(base.String("github.host"), name)
		if err != nil {
			return nil, errors.New("projects: expected an exact owner/repo or host/owner/repo identity")
		}
		if _, duplicate := document.projects[canonical]; duplicate {
			return nil, errors.New("projects: duplicate canonical repository identity")
		}
		document.projects[canonical] = project
		if _, err := Resolve(document, canonical, nil, nil); err != nil {
			return nil, err
		}
	}
	return document, nil
}

func Resolve(document *Document, repository string, preferences *RepositoryPreferences, overrides map[string]any) (Snapshot, error) {
	if repository == "" {
		return Snapshot{}, errors.New("repository identity is required")
	}
	return resolve(document, repository, preferences, overrides)
}
func ResolveDefaults(document *Document, overrides map[string]any) (Snapshot, error) {
	return resolve(document, "", nil, overrides)
}
func resolve(document *Document, repository string, preferences *RepositoryPreferences, overrides map[string]any) (result Snapshot, err error) {
	defer func() {
		if err != nil {
			result = Snapshot{}
		}
	}()
	result = Snapshot{values: make(map[string]any, len(definitions)), origins: make(map[string]Origin, len(definitions))}
	for key, definition := range definitions {
		result.values[key] = copyValue(definition.value)
		result.origins[key] = Origin{Layer: "built-in"}
	}
	apply := func(values map[string]any, origin Origin) {
		for key, value := range values {
			result.values[key] = copyValue(value)
			result.origins[key] = origin
		}
	}
	if document != nil {
		apply(document.fields, Origin{Layer: "global", Source: document.source})
	}
	if preferences != nil {
		if !revisionPattern.MatchString(preferences.BaseSHA) {
			return result, errors.New("repository preferences require an immutable trusted-base SHA")
		}
		values, _, err := parse(preferences.Data, ".reviewd", repositoryScope)
		if err != nil {
			return result, err
		}
		for key, value := range values {
			if key == "reviewer.reasoning_effort" && !oneOf(value.(string), "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra") {
				return result, errors.New(".reviewd: reviewer.reasoning_effort is invalid")
			}
			if key == "review.instructions" && strings.ContainsRune(value.(string), 0) {
				return result, errors.New(".reviewd: review.instructions must not contain NUL")
			}
		}
		apply(values, Origin{Layer: "repository", Source: ".reviewd", Revision: preferences.BaseSHA})
	}
	if err := validate(result.values); err != nil {
		return result, err
	}
	identityHost := result.String("github.host")
	if value, ok := overrides["github.host"].(string); ok {
		identityHost = value
	}
	if repository != "" {
		identity, err := Repository(identityHost, repository)
		if err != nil {
			return result, errors.New("repository: expected owner/repo or host/owner/repo")
		}
		result.Repository = identity
		if document != nil {
			apply(document.projects[identity], Origin{Layer: "project", Source: document.source})
		}
	}

	for key, value := range overrides {
		normalized, err := normalize(key, value, cliScope)
		if err != nil {
			return result, err
		}
		result.values[key] = normalized
		result.origins[key] = Origin{Layer: "cli"}
	}
	if err := validate(result.values); err != nil {
		return result, err
	}
	result.Fingerprint, err = snapshotHash(result.Repository, result.values)
	if err != nil {
		return result, err
	}
	return result, nil
}

func parse(data []byte, source string, allowed scope) (map[string]any, map[string]map[string]any, error) {
	if len(data) > 1024*1024 {
		return nil, nil, fmt.Errorf("%s: configuration exceeds 1 MiB", source)
	}
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		// TOML errors can quote raw credential-shaped values. Expose location,
		// not source snippets, even for unknown/disallowed configuration fields.
		var decode *toml.DecodeError
		if errors.As(err, &decode) {
			line, column := decode.Position()
			return nil, nil, fmt.Errorf("%s:%d:%d: invalid TOML", source, line, column)
		}
		return nil, nil, fmt.Errorf("%s: invalid TOML", source)
	}
	version, ok := root["schema_version"].(int64)
	if !ok || version != 1 {
		return nil, nil, fmt.Errorf("%s: schema_version must be 1", source)
	}
	projects := make(map[string]map[string]any)
	if raw, present := root["projects"]; present {
		if allowed != globalScope {
			return nil, nil, errors.New(".reviewd: projects are operator-controlled")
		}
		entries, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, errors.New("projects must be a table of exact repository settings")
		}
		for name, rawProject := range entries {
			table, ok := rawProject.(map[string]any)
			if !ok {
				return nil, nil, errors.New("project settings must be a table")
			}
			fields, err := flatten(table, "", projectScope)
			if err != nil {
				return nil, nil, err
			}
			projects[name] = fields
		}
		delete(root, "projects")
	}
	fields, err := flatten(root, "", allowed)
	return fields, projects, err
}
func flatten(table map[string]any, prefix string, allowed scope) (map[string]any, error) {
	result := make(map[string]any)
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := prefix + key
		if nested, ok := table[key].(map[string]any); ok {
			known := false
			for setting, definition := range definitions {
				if strings.HasPrefix(setting, path+".") && definition.scopes&allowed != 0 {
					known = true
					break
				}
			}
			if !known {
				return nil, fmt.Errorf("unknown or disallowed configuration table: %s", safeKey(path))
			}
			values, err := flatten(nested, path+".", allowed)
			if err != nil {
				return nil, err
			}
			for key, value := range values {
				result[key] = value
			}
		} else {
			value, err := normalize(path, table[key], allowed)
			if err != nil {
				return nil, err
			}
			result[path] = value
		}
	}
	return result, nil
}
func normalize(key string, value any, allowed scope) (any, error) {
	definition, ok := definitions[key]
	if !ok || definition.scopes&allowed == 0 {
		return nil, fmt.Errorf("unknown or disallowed configuration setting: %s", safeKey(key))
	}
	switch definition.value.(type) {
	case string:
		if _, ok := value.(string); ok {
			return value, nil
		}
	case bool:
		if _, ok := value.(bool); ok {
			return value, nil
		}
	case int64:
		if number, ok := value.(int64); ok {
			return number, nil
		}
		if number, ok := value.(int); ok {
			return int64(number), nil
		}
	case []string:
		if values, ok := value.([]string); ok {
			return append([]string{}, values...), nil
		}
		if values, ok := value.([]any); ok {
			strings := make([]string, 0, len(values))
			for _, value := range values {
				item, ok := value.(string)
				if !ok {
					return nil, fmt.Errorf("%s must contain strings", key)
				}
				strings = append(strings, item)
			}
			return strings, nil
		}
	}
	return nil, fmt.Errorf("%s has an invalid value type", key)
}
func cloneValues(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = copyValue(value)
	}
	return result
}
func copyValue(value any) any {
	if items, ok := value.([]string); ok {
		return append([]string{}, items...)
	}
	return value
}
func safeKey(key string) string {
	// Only declared identifiers are useful diagnostics; unknown tokens must
	// not become a secret-leaking echo surface.
	for _, part := range strings.Split(key, ".") {
		if len(part) > 40 || !keyPattern.MatchString(part) || secretKeyPattern.MatchString(part) {
			return "<unrecognized key>"
		}
	}
	return key
}

var secretKeyPattern = regexp.MustCompile(`^(gh[pousr]_|github_pat_|sk_)`)
var keyPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var credentialPattern = regexp.MustCompile(`^(sk[-_]|gh[opusr]_|github_pat_)`)

func Repository(host, name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) == 3 {
		host = parts[0]
		parts = parts[1:]
	}
	host = strings.ToLower(host)
	if !hostPattern.MatchString(host) || strings.Contains(host, "..") || len(parts) != 2 || !segmentPattern.MatchString(parts[0]) || !segmentPattern.MatchString(parts[1]) {
		return "", errors.New("invalid repository identity")
	}
	return host + "/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]), nil
}
func validate(values map[string]any) error {
	if values["schema_version"] != int64(1) {
		return errors.New("schema_version must be 1")
	}
	host, ok := values["github.host"].(string)
	if !ok || !hostPattern.MatchString(strings.ToLower(host)) || strings.Contains(host, "..") {
		return errors.New("github.host must be a hostname without scheme, path or credentials")
	}
	values["github.host"] = strings.ToLower(host)
	repositories := values["github.repositories"].([]string)
	canonical := make([]string, 0, len(repositories))
	seen := map[string]bool{}
	for _, repository := range repositories {
		identity, err := Repository(host, repository)
		if err != nil || !strings.HasPrefix(identity, strings.ToLower(host)+"/") {
			return errors.New("github.repositories must contain repository identities on github.host")
		}
		if seen[identity] {
			return errors.New("github.repositories contains a duplicate canonical identity")
		}
		seen[identity] = true
		canonical = append(canonical, identity)
	}
	values["github.repositories"] = canonical
	for _, key := range []string{"actions.new_pull_request", "actions.review_requested", "actions.head_updated", "actions.review_submitted", "actions.review_comment_added"} {
		if !oneOf(values[key].(string), "ignore", "notify", "ask", "auto") {
			return fmt.Errorf("%s must be ignore, notify, ask or auto", key)
		}
	}
	if !oneOf(values["actions.publish_review"].(string), "never", "ask", "auto") {
		return errors.New("actions.publish_review must be never, ask or auto")
	}
	for key, bounds := range map[string][2]time.Duration{
		"watch.poll_interval": {time.Second, 24 * time.Hour}, "watch.debounce": {0, time.Hour},
		"limits.review_timeout": {time.Minute, 24 * time.Hour}, "notifications.group_interval": {0, 5 * time.Minute},
	} {
		duration, err := time.ParseDuration(values[key].(string))
		if err != nil || duration < bounds[0] || duration > bounds[1] {
			return fmt.Errorf("%s must be a duration between %s and %s", key, bounds[0], bounds[1])
		}
	}
	if number := values["limits.max_concurrent_reviews"].(int64); number < 1 || number > 16 {
		return errors.New("limits.max_concurrent_reviews must be between 1 and 16")
	}
	if number := values["state.retention_days"].(int64); number < 1 || number > 3650 {
		return errors.New("state.retention_days must be between 1 and 3650")
	}
	if !oneOf(values["reviewer.reasoning_effort"].(string), "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra") {
		return errors.New("reviewer.reasoning_effort must be an advertised reasoning level")
	}
	for _, key := range []string{"reviewer.model", "reviewer.skill", "reviewer.executable"} {
		if strings.TrimSpace(values[key].(string)) == "" || strings.ContainsRune(values[key].(string), 0) {
			return fmt.Errorf("%s must be a nonempty identifier or path", key)
		}
	}
	if value := values["reviewer.skill"].(string); !segmentPattern.MatchString(value) || credentialPattern.MatchString(value) {
		return errors.New("reviewer.skill must be an installed skill name, not a path or credential")
	}
	if value := values["reviewer.model"].(string); !modelPattern.MatchString(value) || credentialPattern.MatchString(value) {
		return errors.New("reviewer.model must be a model identifier, not a credential")
	}
	for _, key := range []string{"reviewer.skill_path", "reviewer.auth_home", "state.directory", "review.instructions"} {
		if strings.ContainsRune(values[key].(string), 0) {
			return fmt.Errorf("%s must not contain NUL", key)
		}
	}
	start, end := values["notifications.quiet_start"].(string), values["notifications.quiet_end"].(string)
	if start != "" || end != "" {
		if _, err := time.Parse("15:04", start); err != nil {
			return errors.New("notifications.quiet_start must be HH:MM when quiet hours are enabled")
		}
		if _, err := time.Parse("15:04", end); err != nil || start == end {
			return errors.New("notifications.quiet_end must be HH:MM and differ from quiet_start")
		}
	}
	if _, err := time.LoadLocation(values["notifications.timezone"].(string)); err != nil {
		return errors.New("notifications.timezone must be Local or a valid IANA timezone")
	}
	return nil
}
func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

// Manager swaps only fully parsed/validated operator configuration. Existing
// snapshots contain independent values and are not rewritten by reload.
type Manager struct {
	mu       sync.RWMutex
	document *Document
	source   string
}

func NewManager(data []byte, source string) (*Manager, error) {
	document, err := Parse(data, source)
	if err != nil {
		return nil, err
	}
	return &Manager{document: document, source: source}, nil
}
func (m *Manager) Reload(data []byte) error {
	document, err := Parse(data, m.source)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.document = document
	return nil
}
func (m *Manager) Resolve(repository string, preferences *RepositoryPreferences, overrides map[string]any) (Snapshot, error) {
	m.mu.RLock()
	document := m.document
	m.mu.RUnlock()
	return Resolve(document, repository, preferences, overrides)
}
