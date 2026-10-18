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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/containerapi"
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

// Obsolete definitions are retained only to decode historical run snapshots.
// They are rejected by live configuration and never appear in new snapshots.
var definitions = map[string]definition{
	"schema_version":                {int64(1), globalScope | repositoryScope},
	"github.host":                   {"github.com", globalScope | cliScope},
	"github.repositories":           {[]string{}, globalScope | cliScope},
	"github.watch_review_requests":  {true, globalScope | cliScope},
	"github.watch_own_prs":          {true, globalScope | cliScope},
	"watch.poll_interval":           {"60s", globalScope | cliScope},
	"watch.debounce":                {"30s", globalScope | projectScope | cliScope},
	"watch.include_drafts":          {false, globalScope | projectScope | cliScope},
	"agent.working_directory":       {"", globalScope | projectScope | cliScope},
	"feedback.instructions":         {"", globalScope | projectScope | cliScope},
	"reviewer.model":                {"gpt-6.1-sol", globalScope | projectScope | cliScope},
	"reviewer.backend":              {"codex", globalScope | projectScope | cliScope},
	"feedback.backend":              {"codex", globalScope | projectScope | cliScope},
	"feedback.executable":           {"", globalScope | projectScope | cliScope},
	"reviewer.reasoning_effort":     {"high", globalScope | projectScope | repositoryScope | cliScope},
	"reviewer.skill":                {"review", globalScope | projectScope | cliScope},
	"reviewer.skill_path":           {"", globalScope | projectScope | cliScope},
	"reviewer.executable":           {"codex", globalScope | projectScope | cliScope},
	"reviewer.auth_home":            {"", globalScope | cliScope},
	"reviewer.exclude_authors":      {[]string{"@me"}, globalScope | projectScope | cliScope},
	"feedback.authors":              {[]string{"@me"}, globalScope | projectScope},
	"feedback.action":               {"notify", globalScope | projectScope},
	"feedback.model":                {"gpt-6.1-sol", globalScope | projectScope | cliScope},
	"feedback.reasoning_effort":     {"high", globalScope | projectScope | cliScope},
	"feedback.skill":                {"handle-pr-feedback", globalScope | projectScope | cliScope},
	"feedback.skill_path":           {"", globalScope | projectScope | cliScope},
	"feedback.container_endpoint":   {"", globalScope | projectScope},
	"feedback.container_image":      {"", globalScope | projectScope},
	"feedback.metadata_validators":  {"", globalScope | projectScope},
	"feedback.metadata_profile":     {"", globalScope | projectScope},
	"feedback.label_catalog":        {"", globalScope | projectScope},
	"limits.feedback_timeout":       {"60m", globalScope | projectScope | cliScope},
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

// ReviewAuthorExcluded governs discovered review work, not explicit manual URL
// reviews or the independent feedback lane. @me follows stable account identity.
func (s Snapshot) ReviewAuthorExcluded(authorID int64, login string, actorID int64) bool {
	authors := s.Strings("reviewer.exclude_authors")
	if _, present := s.values["reviewer.exclude_authors"]; !present {
		authors = []string{"@me"} // Historical snapshots retain their exact bytes.
	}
	for _, author := range authors {
		if author == "@me" {
			if actorID > 0 && authorID == actorID {
				return true
			}
		} else if strings.EqualFold(author, login) {
			return true
		}
	}
	return false
}

// FeedbackAuthorAllowed selects PR authors, not the people leaving feedback.
// An empty replacement list disables discovery for this lane.
func (s Snapshot) FeedbackAuthorAllowed(authorID int64, login string, actorID int64) bool {
	for _, author := range s.Strings("feedback.authors") {
		if (author == "@me" && actorID > 0 && authorID == actorID) || (author != "@me" && strings.EqualFold(author, login)) {
			return true
		}
	}
	return false
}

// FeedbackMonitoringConfigured distinguishes the separately opted-in lane from
// historical incoming-feedback/private-review monitoring. Selecting an agent
// alone does not subscribe the operator to a new category of work.
func (s Snapshot) FeedbackMonitoringConfigured() bool {
	for _, name := range []string{"feedback.authors", "feedback.action"} {
		if origin, ok := s.origins[name]; ok && origin.Layer != "built-in" {
			return true
		}
	}
	return false
}

// FeedbackAction requires new explicit operator consent for automatic fixes.
// Historical action=auto requested only private reviewing and cannot acquire
// the distinct modifying capability after an upgrade.
func (s Snapshot) FeedbackAction(trigger string) string {
	if origin, ok := s.origins["feedback.action"]; ok && origin.Layer != "built-in" {
		return s.String("feedback.action")
	}
	action := s.String("actions." + trigger)
	if action == "auto" {
		return "ask"
	}
	if oneOf(action, "ignore", "notify", "ask") {
		return action
	}
	return "notify"
}

func feedbackSetting(key string) bool {
	return strings.HasPrefix(key, "feedback.") || key == "limits.feedback_timeout"
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
	// Accept only complete known generations. Never backfill serialized values:
	// old prepared payloads retain their exact fingerprint and encoded bytes.
	_, hasAuthors := wire.Values["reviewer.exclude_authors"]
	hasFeedback, feedbackCount := false, 0
	for key := range definitions {
		if feedbackSetting(key) && !backendSetting(key) && !containerSetting(key) && !metadataSetting(key) && key != "feedback.label_catalog" && key != "feedback.instructions" {
			feedbackCount++
			if _, present := wire.Values[key]; present {
				hasFeedback = true
			}
		}
	}
	_, hasContainerEndpoint := wire.Values["feedback.container_endpoint"]
	_, hasContainerImage := wire.Values["feedback.container_image"]
	hasContainer := hasContainerEndpoint || hasContainerImage
	_, hasMetadataValidators := wire.Values["feedback.metadata_validators"]
	_, hasMetadataProfile := wire.Values["feedback.metadata_profile"]
	hasMetadata := hasMetadataValidators || hasMetadataProfile
	_, hasLabelCatalog := wire.Values["feedback.label_catalog"]
	// Pre-release native snapshots used inner version 2. Public v1 and the
	// historical private workflow both use 1, so the native-only launch fields
	// distinguish their complete shapes without rewriting recorded identities.
	_, hasWorkingDirectory := wire.Values["agent.working_directory"]
	_, hasFeedbackInstructions := wire.Values["feedback.instructions"]
	launcher := wire.Values["schema_version"] == json.Number("2") || hasWorkingDirectory || hasFeedbackInstructions
	expected := len(definitions)
	backendCount := 0
	for key := range definitions {
		if backendSetting(key) {
			if _, ok := wire.Values[key]; ok {
				backendCount++
			}
		}
	}
	if backendCount == 0 {
		expected -= 3
	} else if backendCount != 3 || !launcher {
		return errors.New("configuration snapshot has an incomplete backend selection")
	}
	if launcher {
		for key := range definitions {
			if obsoleteSetting(key) {
				expected--
				if _, present := wire.Values[key]; present {
					return errors.New("launcher snapshot contains a removed workflow setting")
				}
			}
		}
		// The old generation checks below apply only to historical snapshots.
		hasAuthors, hasFeedback, hasContainer, hasMetadata, hasLabelCatalog = true, true, true, true, true
	} else {
		expected -= 2 // working_directory and feedback instructions did not exist.
	}
	if !hasAuthors {
		expected--
	}
	if !hasFeedback {
		expected -= feedbackCount
	}
	if !hasContainer {
		expected -= 2
	}
	if !hasMetadata {
		expected -= 2
	}
	if !hasLabelCatalog {
		expected--
	}
	if wire.SchemaVersion != 1 || len(wire.Values) != expected || len(wire.Provenance) != expected || (hasFeedback && !hasAuthors) || ((hasContainer || hasMetadata || hasLabelCatalog) && !hasFeedback) {
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
	validationValues := cloneValues(values)
	for key, definition := range definitions {
		if _, present := validationValues[key]; !present {
			legacyContainer := containerSetting(key) && !hasContainer
			legacyMetadata := metadataSetting(key) && !hasMetadata
			legacyLabels := key == "feedback.label_catalog" && !hasLabelCatalog
			legacyLauncher := !launcher && (key == "agent.working_directory" || key == "feedback.instructions")
			removedWorkflow := launcher && obsoleteSetting(key)
			if !backendSetting(key) && !legacyLauncher && !removedWorkflow && !legacyContainer && !legacyMetadata && !legacyLabels && key != "reviewer.exclude_authors" && (!feedbackSetting(key) || hasFeedback) {
				return errors.New("configuration snapshot is missing a required setting")
			}
			validationValues[key] = copyValue(definition.value)
			if key == "feedback.executable" {
				validationValues[key] = "codex"
			}
		}
	}
	if err := validate(validationValues); err != nil {
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
		if obsoleteSetting(key) {
			continue
		}
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
		delete(values, "schema_version") // Repository preferences have a separate, limited schema.
		apply(values, Origin{Layer: "repository", Source: ".reviewd", Revision: preferences.BaseSHA})
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
		if obsoleteSetting(key) {
			return result, fmt.Errorf("removed workflow setting: %s; configure the skill or prompt instead", safeKey(key))
		}
		normalized, err := normalize(key, value, cliScope)
		if err != nil {
			return result, err
		}
		result.values[key] = normalized
		result.origins[key] = Origin{Layer: "cli"}
	}
	result.backendDefaults()
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
	if allowed == globalScope && (!ok || version != 1) {
		return nil, nil, fmt.Errorf("%s: operator configuration requires schema_version=1", source)
	}
	if allowed == repositoryScope && (!ok || version != 1) {
		return nil, nil, fmt.Errorf("%s: repository preferences require schema_version=1", source)
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
			if obsoleteSetting(path) {
				return nil, fmt.Errorf("removed workflow setting: %s; configure the skill or prompt instead", safeKey(path))
			}
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
var skillPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(?::[A-Za-z0-9][A-Za-z0-9._-]{0,99})?$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var credentialPattern = regexp.MustCompile(`^(sk[-_]|gh[opusr]_|github_pat_)`)
var authorPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`)

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
	// Decode historical generations without inserting obsolete settings into a
	// live snapshot or rewriting historical values and their fingerprints.
	var temporary []string
	for key, definition := range definitions {
		if _, present := values[key]; !present && (backendSetting(key) || obsoleteSetting(key) || key == "agent.working_directory" || key == "feedback.instructions") {
			values[key] = copyValue(definition.value)
			if key == "feedback.executable" {
				values[key] = "codex"
			}
			temporary = append(temporary, key)
		}
	}
	defer func() {
		for _, key := range temporary {
			delete(values, key)
		}
	}()
	if values["schema_version"] != int64(1) && values["schema_version"] != int64(2) {
		return errors.New("unsupported configuration schema_version")
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
	for _, key := range []string{"reviewer.exclude_authors", "feedback.authors"} {
		authors := values[key].([]string)
		seenAuthors := map[string]bool{}
		for index, author := range authors {
			author = strings.ToLower(author)
			if author != "@me" && !authorPattern.MatchString(author) {
				return fmt.Errorf("%s must contain exact GitHub logins or @me", key)
			}
			if seenAuthors[author] {
				return fmt.Errorf("%s contains a duplicate author", key)
			}
			seenAuthors[author] = true
			authors[index] = author
		}
	}
	if !oneOf(values["feedback.action"].(string), "ignore", "notify", "ask", "auto") {
		return errors.New("feedback.action must be ignore, notify, ask or auto")
	}
	if endpoint := values["feedback.container_endpoint"].(string); endpoint != "" {
		// Construction validates the Unix selector without opening a socket.
		client, err := containerapi.New(endpoint)
		if err != nil {
			return errors.New("feedback.container_endpoint must be an absolute local unix:/// socket URL")
		}
		_ = client.Close()
	}
	if image := values["feedback.container_image"].(string); image != "" && !containerImagePattern.MatchString(image) {
		return errors.New("feedback.container_image must be a locally cached immutable sha256 image ID, not a tag")
	}
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
		"limits.feedback_timeout": {time.Minute, 24 * time.Hour}, "limits.review_timeout": {time.Minute, 24 * time.Hour}, "notifications.group_interval": {0, 5 * time.Minute},
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
	for _, key := range []string{"reviewer.reasoning_effort", "feedback.reasoning_effort"} {
		backend := values[strings.TrimSuffix(key, "reasoning_effort")+"backend"].(string)
		valid := oneOf(values[key].(string), "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra")
		if backend == "claude" {
			valid = oneOf(values[key].(string), "", "low", "medium", "high", "xhigh", "max")
		}
		if !valid {
			return fmt.Errorf("%s must be an advertised reasoning level", key)
		}
	}
	for _, key := range []string{"reviewer.skill", "reviewer.executable", "feedback.executable", "feedback.skill"} {
		if strings.TrimSpace(values[key].(string)) == "" || strings.ContainsRune(values[key].(string), 0) {
			return fmt.Errorf("%s must be a nonempty identifier or path", key)
		}
	}
	for _, key := range []string{"reviewer.skill", "feedback.skill"} {
		if value := values[key].(string); !skillPattern.MatchString(value) || credentialPattern.MatchString(value) {
			return fmt.Errorf("%s must be an installed skill name, not a path or credential", key)
		}
	}
	for _, key := range []string{"reviewer.model", "feedback.model"} {
		value := values[key].(string)
		backend := values[strings.TrimSuffix(key, "model")+"backend"].(string)
		if value == "" && backend == "claude" {
			continue
		}
		if !modelPattern.MatchString(value) || credentialPattern.MatchString(value) {
			return fmt.Errorf("%s must be a model identifier, not a credential", key)
		}
		if backend == "claude" && strings.HasPrefix(value, "gpt-") {
			return fmt.Errorf("%s selects a Codex model for Claude; select a Claude model or an empty native default", key)
		}
	}
	for _, prefix := range []string{"reviewer.", "feedback."} {
		if !oneOf(values[prefix+"backend"].(string), "codex", "claude") {
			return fmt.Errorf("%sbackend must be codex or claude", prefix)
		}
		if value := values[prefix+"executable"].(string); strings.ContainsRune(value, 0) {
			return fmt.Errorf("%sexecutable must not contain NUL", prefix)
		}
	}
	for _, key := range []string{"reviewer.skill_path", "feedback.skill_path", "reviewer.auth_home", "state.directory", "review.instructions", "feedback.instructions", "agent.working_directory"} {
		if strings.ContainsRune(values[key].(string), 0) {
			return fmt.Errorf("%s must not contain NUL", key)
		}
	}
	if directory := values["agent.working_directory"].(string); directory != "" && !filepath.IsAbs(directory) {
		return errors.New("agent.working_directory must be an absolute operator path")
	}
	for _, key := range []string{"feedback.metadata_validators", "feedback.metadata_profile", "feedback.label_catalog"} {
		if value := values[key].(string); value != "" && (!filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n")) {
			return fmt.Errorf("%s must be an absolute operator path", key)
		}
	}
	start, end := values["notifications.quiet_start"].(string), values["notifications.quiet_end"].(string)
	if start != "" || end != "" {
		startClock, err := time.Parse("15:04", start)
		if err != nil {
			return errors.New("notifications.quiet_start must be H:MM or HH:MM when quiet hours are enabled")
		}
		if endClock, err := time.Parse("15:04", end); err != nil || startClock.Equal(endClock) {
			return errors.New("notifications.quiet_end must be H:MM or HH:MM and differ from quiet_start by clock value")
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

func containerSetting(key string) bool {
	return key == "feedback.container_endpoint" || key == "feedback.container_image"
}

func metadataSetting(key string) bool {
	return key == "feedback.metadata_validators" || key == "feedback.metadata_profile"
}

var containerImagePattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

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

// Document captures the immutable validated operator document for one dispatch.
func (m *Manager) Document() *Document {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.document
}

func obsoleteSetting(key string) bool {
	switch key {
	case "reviewer.skill_path", "feedback.skill_path", "reviewer.auth_home", "actions.publish_review", "feedback.container_endpoint", "feedback.container_image", "feedback.metadata_validators", "feedback.metadata_profile", "feedback.label_catalog":
		return true
	}
	return false
}
