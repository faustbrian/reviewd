// Package source acquires Git objects in disposable bare storage. It never
// checks out or executes a repository's files, attributes, hooks or tools.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

const (
	maxBlob     = 8 << 20
	maxText     = 256 << 20
	maxManifest = 32 << 20
)

type Boundary struct {
	TargetBaseSHA, HeadSHA string
	Number, ChangedFiles   int
}
type Options struct {
	RemoteURL, Authorization, TempRoot, Executable string
	// Only an application-owned test seam; never configurable by a PR or user
	// repository preferences. Production callers always acquire through HTTPS.
	AllowLocalFixture bool
}
type Entry struct {
	Path         string `json:"path"`
	Mode         string `json:"mode"`
	Type         string `json:"type"`
	OID          string `json:"oid"`
	Size         int64  `json:"size"`
	Availability string `json:"availability"`
}
type Change struct {
	Status       string `json:"status"`
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
}
type Gap struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Reason   string `json:"reason"`
}
type Snapshot struct {
	TargetBaseSHA, MergeBaseSHA, HeadSHA string
	HeadTreeSHA                          string
	CommitCount                          int
	Sources                              map[string]map[string]string
	Manifests                            map[string][]Entry
	Changes                              []Change
	Gaps                                 []Gap
	Diff, Commits                        string
	root                                 string
	closeOnce                            sync.Once
	closeErr                             error
}

func (s *Snapshot) Close() error {
	s.closeOnce.Do(func() {
		if err := os.RemoveAll(s.root); err != nil {
			s.closeErr = errors.New("could not remove owned source snapshot")
		}
	})
	return s.closeErr
}

// GitEnvironment removes all ambient Git configuration, helpers, proxies and
// SSH authority. Credentials are an app-owned Git-only environment header,
// never a remote URL, command argument, source document or worker environment.
func GitEnvironment(inherited []string, root, authorization string) []string {
	env := []string{}
	for _, item := range inherited {
		key, _, ok := strings.Cut(item, "=")
		if ok && (key == "PATH" || key == "LANG" || key == "LC_ALL") {
			env = append(env, item)
		}
	}
	env = append(env, "HOME="+root, "XDG_CONFIG_HOME="+root, "TMPDIR="+root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+filepath.Join(root, "hooks"), "GIT_CONFIG_KEY_1=credential.helper", "GIT_CONFIG_VALUE_1=", "GIT_CONFIG_KEY_2=http.followRedirects", "GIT_CONFIG_VALUE_2=false", "GIT_CONFIG_KEY_3=http.extraHeader", "GIT_CONFIG_VALUE_3="+authorization, "GIT_CONFIG_KEY_4=protocol.allow", "GIT_CONFIG_VALUE_4=never")
	return env
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("source evidence exceeds supported size")
	}
	return b.Buffer.Write(data)
}

type runner struct {
	executable, root string
	env              []string
}

func (r runner) run(ctx context.Context, input []byte, limit int, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, r.executable, append([]string{"--no-pager"}, args...)...)
	command.Dir = r.root
	command.Env = r.env
	command.Stdin = bytes.NewReader(input)
	output := &boundedBuffer{limit: limit}
	command.Stdout = output
	command.Stderr = io.Discard
	err := lifetime.Run(ctx, command)
	if errors.Is(err, lifetime.ErrCleanupUncertain) {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.New("immutable Git acquisition failed; check Git installation, repository access, disk space and the current review boundary")
	}
	return output.Bytes(), nil
}

var objectID = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func Acquire(ctx context.Context, options Options, boundary Boundary) (snapshot *Snapshot, err error) {
	remote, parseErr := url.Parse(options.RemoteURL)
	if parseErr != nil || remote.User != nil || remote.RawQuery != "" || remote.ForceQuery || remote.Fragment != "" || remote.RawPath != "" || (remote.Scheme != "https" && !(options.AllowLocalFixture && remote.Scheme == "file" && remote.Host == "")) || (remote.Scheme == "https" && (remote.Host == "" || remote.Port() != "")) || strings.ContainsAny(options.Authorization, "\r\n\x00") {
		return nil, errors.New("source remote must be a canonical HTTPS repository without credentials or URL decorations")
	}
	if !objectID.MatchString(boundary.TargetBaseSHA) || !objectID.MatchString(boundary.HeadSHA) || boundary.Number < 1 || boundary.ChangedFiles < 0 {
		return nil, errors.New("source acquisition requires an immutable target base, head and complete changed-file count")
	}
	root, err := os.MkdirTemp(lifetime.Root(ctx, options.TempRoot), "reviewd-source-")
	if err != nil {
		return nil, errors.New("could not create owned source storage")
	}
	snapshot = &Snapshot{TargetBaseSHA: boundary.TargetBaseSHA, HeadSHA: boundary.HeadSHA, root: root, Sources: map[string]map[string]string{}, Manifests: map[string][]Entry{}}
	owned := snapshot
	defer func() {
		if err != nil && !errors.Is(err, lifetime.ErrCleanupUncertain) {
			err = errors.Join(err, owned.Close())
		}
	}()
	if err = os.Mkdir(filepath.Join(root, "hooks"), 0700); err != nil {
		return nil, errors.New("could not initialize owned source storage")
	}
	executable := options.Executable
	if executable == "" {
		executable = "git"
	}
	r := runner{executable: executable, root: root, env: GitEnvironment(os.Environ(), root, options.Authorization)}
	protocol := "https"
	if options.AllowLocalFixture && remote.Scheme == "file" {
		protocol = "file"
	}
	r.env = append(r.env, "GIT_ALLOW_PROTOCOL="+protocol)
	if _, err = r.run(ctx, nil, 1<<20, "init", "--bare", "--template=", root); err != nil {
		return nil, err
	}
	if _, err = r.run(ctx, nil, 1<<20, "fetch", "--no-tags", "--no-write-fetch-head", options.RemoteURL, boundary.TargetBaseSHA, "+refs/pull/"+strconv.Itoa(boundary.Number)+"/head:refs/reviewd/head"); err != nil {
		return nil, err
	}
	return inspectSnapshot(ctx, r, boundary, snapshot)
}

// RetainedDirectory identifies app-owned immutable objects, not a new checkout.
// The feedback owner must pin its filesystem identity before persisting it.
func (s *Snapshot) RetainedDirectory() string { return s.root }

// ReopenFeedbackSnapshot reads the same original source object storage. It
// performs no fetch, checkout, hook, remote operation or source execution.
func ReopenFeedbackSnapshot(ctx context.Context, ownerRoot, directory string, boundary Boundary) (*Snapshot, error) {
	owner, err := filepath.EvalSymlinks(ownerRoot)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(directory)
	info, statErr := os.Lstat(directory)
	if err != nil || statErr != nil || canonical != directory || filepath.Dir(directory) != owner || !strings.HasPrefix(filepath.Base(directory), "reviewd-source-") || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("retained immutable source must be the original owner's private child")
	}
	if !objectID.MatchString(boundary.TargetBaseSHA) || !objectID.MatchString(boundary.HeadSHA) || boundary.Number < 1 || boundary.ChangedFiles < 0 {
		return nil, errors.New("retained immutable source boundary is invalid")
	}
	snapshot := &Snapshot{TargetBaseSHA: boundary.TargetBaseSHA, HeadSHA: boundary.HeadSHA, root: directory, Sources: map[string]map[string]string{}, Manifests: map[string][]Entry{}}
	r := runner{executable: "git", root: directory, env: append(GitEnvironment(os.Environ(), directory, ""), "GIT_ALLOW_PROTOCOL=never")}
	return inspectSnapshot(ctx, r, boundary, snapshot)
}

func inspectSnapshot(ctx context.Context, r runner, boundary Boundary, snapshot *Snapshot) (*Snapshot, error) {
	head, err := r.run(ctx, nil, 1<<20, "rev-parse", "--verify", "refs/reviewd/head^{commit}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(head)) != boundary.HeadSHA {
		return nil, errors.New("pull-request head changed while acquiring source; reassess the current boundary")
	}
	base, err := r.run(ctx, nil, 1<<20, "rev-parse", "--verify", boundary.TargetBaseSHA+"^{commit}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(base)) != boundary.TargetBaseSHA {
		return nil, errors.New("target-base object identity mismatch")
	}
	merge, err := r.run(ctx, nil, 1<<20, "merge-base", "--all", boundary.TargetBaseSHA, boundary.HeadSHA)
	if err != nil {
		return nil, err
	}
	snapshot.MergeBaseSHA = strings.TrimSpace(string(merge))
	if !objectID.MatchString(snapshot.MergeBaseSHA) {
		return nil, errors.New("comparison has no unique immutable merge base; complete review evidence is unavailable")
	}
	names, err := r.run(ctx, nil, maxManifest, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--name-status", "-z", snapshot.MergeBaseSHA, boundary.HeadSHA, "--")
	if err != nil {
		return nil, err
	}
	snapshot.Changes, err = parseChanges(names)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Changes) != boundary.ChangedFiles {
		return nil, errors.New("GitHub changed-file count does not match the complete immutable comparison")
	}
	diff, err := r.run(ctx, nil, 64<<20, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--full-index", snapshot.MergeBaseSHA, boundary.HeadSHA, "--")
	if err != nil {
		return nil, err
	}
	snapshot.Diff = string(diff)
	commits, err := r.run(ctx, nil, maxManifest, "log", "--format=commit %H%nparents %P%nauthor %an <%ae>%n%B", snapshot.MergeBaseSHA+".."+boundary.HeadSHA, "--")
	if err != nil {
		return nil, err
	}
	snapshot.Commits = string(commits)
	count, err := r.run(ctx, nil, 1024, "rev-list", "--count", snapshot.MergeBaseSHA+".."+boundary.HeadSHA, "--")
	if err != nil {
		return nil, err
	}
	snapshot.CommitCount, err = strconv.Atoi(strings.TrimSpace(string(count)))
	if err != nil || snapshot.CommitCount < 0 {
		return nil, errors.New("complete commit comparison count was unavailable")
	}
	objects := map[string]Entry{}
	headTree, err := r.run(ctx, nil, 1024, "rev-parse", "--verify", boundary.HeadSHA+"^{tree}")
	if err != nil {
		return nil, err
	}
	snapshot.HeadTreeSHA = strings.TrimSpace(string(headTree))
	if !objectID.MatchString(snapshot.HeadTreeSHA) || len(snapshot.HeadTreeSHA) != len(boundary.HeadSHA) {
		return nil, errors.New("immutable head tree identity is unavailable")
	}
	for revision, sha := range map[string]string{"base": snapshot.MergeBaseSHA, "head": boundary.HeadSHA, "target": boundary.TargetBaseSHA} {
		tree, treeErr := r.run(ctx, nil, maxManifest, "ls-tree", "-rlz", "--full-tree", sha)
		if treeErr != nil {
			return nil, treeErr
		}
		entries, treeErr := parseTree(tree)
		if treeErr != nil {
			return nil, treeErr
		}
		snapshot.Manifests[revision] = entries
		for _, entry := range entries {
			if entry.Type == "blob" {
				objects[entry.OID] = entry
			}
		}
	}
	var wanted bytes.Buffer
	total := int64(0)
	for oid, entry := range objects {
		if entry.Size <= maxBlob {
			total += entry.Size
			fmt.Fprintln(&wanted, oid)
		}
	}
	if total > maxText {
		return nil, errors.New("complete source text exceeds 256 MiB; review cannot silently truncate its source")
	}
	blobs, err := r.run(ctx, wanted.Bytes(), maxText+maxManifest, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	contents, err := parseBlobs(blobs, objects)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, change := range snapshot.Changes {
		changed[change.Path] = true
		if change.PreviousPath != "" {
			changed[change.PreviousPath] = true
		}
	}
	for revision, entries := range snapshot.Manifests {
		files := map[string]string{}
		for i, entry := range entries {
			reason := ""
			switch {
			case !utf8.ValidString(entry.Path) || path.Clean(entry.Path) != entry.Path || path.IsAbs(entry.Path) || strings.HasPrefix(entry.Path, "../") || strings.ContainsAny(entry.Path, "\\\x00:"):
				reason = "source path is outside the supported sealed text-reader format"
			case entry.Type == "commit":
				reason = "submodule content is external to this repository"
			case entry.Size > maxBlob:
				reason = "source blob exceeds the 8 MiB read limit"
			default:
				content, ok := contents[entry.OID]
				if !ok {
					return nil, errors.New("source object collection was incomplete")
				}
				if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
					reason = "binary or non-UTF-8 content requires separate inspection"
				} else {
					files[entry.Path] = content
					if strings.HasPrefix(content, "version https://git-lfs.github.com/spec/v1\n") {
						reason = "Git LFS object content is external to this repository"
					}
				}
			}
			entries[i].Availability = "text"
			if reason != "" {
				entries[i].Availability = reason
			}
			if changed[entry.Path] && reason != "" && revision != "target" {
				snapshot.Gaps = append(snapshot.Gaps, Gap{Path: entry.Path, Revision: revision, Reason: reason})
			}
		}
		snapshot.Manifests[revision] = entries
		snapshot.Sources[revision] = files
	}
	sort.Slice(snapshot.Gaps, func(i, j int) bool {
		a, b := snapshot.Gaps[i], snapshot.Gaps[j]
		if a.Revision != b.Revision {
			return a.Revision < b.Revision
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Reason < b.Reason
	})
	return snapshot, nil
}

func parseChanges(data []byte) ([]Change, error) {
	result := []Change{}
	parts := bytes.Split(data, []byte{0})
	for i := 0; i < len(parts)-1; {
		status := string(parts[i])
		i++
		if status == "" || i >= len(parts)-1 || !strings.ContainsRune("AMDRCTUXB", rune(status[0])) {
			return nil, errors.New("invalid immutable changed-file manifest")
		}
		change := Change{Status: status, Path: string(parts[i])}
		i++
		if status[0] == 'R' || status[0] == 'C' {
			if i >= len(parts)-1 {
				return nil, errors.New("incomplete rename or copy evidence")
			}
			change.PreviousPath = change.Path
			change.Path = string(parts[i])
			i++
		}
		result = append(result, change)
	}
	return result, nil
}
func parseTree(data []byte) ([]Entry, error) {
	result := []Entry{}
	for _, line := range bytes.Split(data, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		if !ok || len(fields) != 4 || !objectID.MatchString(fields[2]) {
			return nil, errors.New("invalid immutable source tree")
		}
		size := int64(0)
		var err error
		if fields[1] == "blob" {
			size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil || size < 0 {
				return nil, errors.New("invalid source blob size")
			}
		} else if fields[1] != "commit" {
			return nil, errors.New("unsupported source tree entry")
		}
		result = append(result, Entry{Path: string(name), Mode: fields[0], Type: fields[1], OID: fields[2], Size: size})
	}
	return result, nil
}
func parseBlobs(data []byte, objects map[string]Entry) (map[string]string, error) {
	result := map[string]string{}
	for len(data) > 0 {
		header, rest, ok := bytes.Cut(data, []byte{'\n'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || fields[1] != "blob" {
			return nil, errors.New("invalid immutable blob collection")
		}
		entry, ok := objects[fields[0]]
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if !ok || err != nil || size != entry.Size || size > maxBlob || size < 0 || int64(len(rest)) < size+1 || rest[size] != '\n' {
			return nil, errors.New("incomplete or mismatched source blob")
		}
		if _, duplicate := result[fields[0]]; duplicate {
			return nil, errors.New("duplicate immutable source blob")
		}
		result[fields[0]] = string(rest[:size])
		data = rest[size+1:]
	}
	for oid, entry := range objects {
		if entry.Size <= maxBlob {
			if _, ok := result[oid]; !ok {
				return nil, errors.New("immutable source collection is incomplete")
			}
		}
	}
	return result, nil
}
