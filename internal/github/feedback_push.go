package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/faustbrian/reviewd/internal/source"
)

// PushFeedbackCommit is application-only: the caller must first run normal
// pre-push hooks in the credential-free workspace and durably record its
// attempted effect. It never executes hooks, config or repository code on the
// host, and is deliberately not a general model-visible Git/network tool.
//
// Smart HTTP's ordinary old/new/ref command provides server-side compare-and-
// swap without force options. A concurrent rewind is rejected as well as a
// concurrent advance. Git itself builds and validates the additive object pack.
func (c *Client) PushFeedbackCommit(ctx context.Context, target FeedbackTarget, snapshot *source.Snapshot, workspace, commit, tree string, contents []byte) (FeedbackTarget, error) {
	var empty FeedbackTarget
	canonical, _, err := c.repositoryScope(target.HeadRepository)
	if err != nil || canonical != target.HeadRepository || target.HeadRepositoryID <= 0 || snapshot == nil || snapshot.HeadSHA != target.Pull.HeadSHA || snapshot.TargetBaseSHA != target.Pull.TargetBaseSHA || !feedbackPushBranch(target.Pull.HeadRef) || !objectPattern.MatchString(commit) || commit == target.Pull.HeadSHA {
		return empty, errors.New("feedback push requires the exact existing unprotected PR head and additive commit")
	}
	current, err := c.FeedbackTarget(ctx, target.Pull.Repository, target.Pull.Number)
	if err != nil {
		return empty, err
	}
	if current.HeadRepository != target.HeadRepository || current.HeadRepositoryID != target.HeadRepositoryID || !SameReviewContext(current.Pull, target.Pull) {
		return empty, errors.New("feedback push target changed before transport")
	}
	pack, err := snapshot.FeedbackPushPack(ctx, workspace, commit, tree, contents)
	if err != nil {
		return empty, err
	}
	remote := url.URL{Scheme: "https", Host: c.host, Path: "/" + strings.TrimPrefix(canonical, c.host+"/") + ".git"}
	// New only accepts HTTP on owned loopback fixtures. This test adapter is
	// application-selected, never repository config or a server-provided URL.
	if c.endpoint.Scheme == "http" {
		remote.Scheme, remote.Host = c.endpoint.Scheme, c.endpoint.Host
	}
	discovery := remote
	discovery.Path += "/info/refs"
	discovery.RawQuery = "service=git-receive-pack"
	advertisement, err := c.feedbackGitRequest(ctx, http.MethodGet, discovery, nil, "advertisement", 4<<20)
	if err != nil {
		return empty, err
	}
	ref := "refs/heads/" + target.Pull.HeadRef
	capabilities, err := feedbackPushAdvertisement(advertisement, ref, target.Pull.HeadSHA)
	if err != nil {
		return empty, err
	}
	command := fmt.Sprintf("%s %s %s\x00%s\n", target.Pull.HeadSHA, commit, ref, capabilities)
	request := io.MultiReader(strings.NewReader(fmt.Sprintf("%04x%s0000", len(command)+4, command)), bytes.NewReader(pack))
	endpoint := remote
	endpoint.Path += "/git-receive-pack"
	status, transportErr := c.feedbackGitRequest(ctx, http.MethodPost, endpoint, request, "result", 1<<20)
	if transportErr == nil {
		transportErr = feedbackPushStatus(status, ref)
	}
	// Even a successful report-status does not certify PR identity. Conversely,
	// a lost/malformed response after acceptance is reconciled without replay.
	delivered, readErr := c.FeedbackTarget(ctx, target.Pull.Repository, target.Pull.Number)
	if readErr == nil {
		comparison := delivered.Pull
		comparison.HeadSHA, comparison.CommitCount, comparison.ChangedFiles = target.Pull.HeadSHA, target.Pull.CommitCount, target.Pull.ChangedFiles
		if delivered.HeadRepository == target.HeadRepository && delivered.HeadRepositoryID == target.HeadRepositoryID && delivered.Pull.HeadSHA == commit && SameReviewContext(comparison, target.Pull) {
			return delivered, nil
		}
	}
	return empty, errors.Join(transportErr, readErr, errors.New("feedback push is not canonically confirmed on the same PR; preserve the attempted effect and do not replay it"))
}

func (c *Client) feedbackGitRequest(ctx context.Context, method string, endpoint url.URL, body io.Reader, result string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, errors.New("invalid scoped Git transport request")
	}
	request.SetBasicAuth("x-access-token", c.token)
	request.Header.Set("User-Agent", "reviewd")
	request.Header.Set("Accept", "application/x-git-receive-pack-"+result)
	request.Header.Set("Cache-Control", "no-cache")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	}
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("scoped Git transport response is unknown; reconcile canonical head before any retry")
	}
	defer response.Body.Close()
	contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || parseErr != nil || contentType != "application/x-git-receive-pack-"+result {
		return nil, errors.New("scoped Git transport rejected the request or omitted its protocol response")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("scoped Git transport response is incomplete or exceeds its bound")
	}
	return data, nil
}

func feedbackPacket(data *[]byte) (string, bool, error) {
	if len(*data) < 4 {
		return "", false, errors.New("incomplete Git packet")
	}
	n, err := strconv.ParseUint(string((*data)[:4]), 16, 16)
	if err != nil || (n != 0 && n < 4) || n > 65520 || n > uint64(len(*data)) {
		return "", false, errors.New("invalid Git packet")
	}
	if n == 0 {
		*data = (*data)[4:]
		return "", true, nil
	}
	value := string((*data)[4:n])
	*data = (*data)[n:]
	return strings.TrimSuffix(value, "\n"), false, nil
}

func feedbackPushAdvertisement(data []byte, ref, old string) (string, error) {
	service, flush, err := feedbackPacket(&data)
	if err != nil || flush || service != "# service=git-receive-pack" {
		return "", errors.New("Git receive-pack discovery is unavailable")
	}
	_, flush, err = feedbackPacket(&data)
	if err != nil || !flush {
		return "", errors.New("Git discovery omitted its service boundary")
	}
	capabilities := map[string]bool{}
	found := false
	seen := map[string]bool{}
	for first := true; ; first = false {
		line, flush, err := feedbackPacket(&data)
		if err != nil {
			return "", err
		}
		if flush {
			if len(data) != 0 {
				return "", errors.New("Git discovery has trailing data")
			}
			break
		}
		if first {
			var raw string
			var ok bool
			line, raw, ok = strings.Cut(line, "\x00")
			if !ok || strings.ContainsRune(raw, 0) {
				return "", errors.New("Git discovery omitted capabilities")
			}
			for _, cap := range strings.Fields(raw) {
				capabilities[cap] = true
			}
		} else if strings.ContainsRune(line, 0) {
			return "", errors.New("Git discovery has conflicting capabilities")
		}
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !objectPattern.MatchString(oid) || len(oid) != len(old) || name == "" || seen[name] {
			return "", errors.New("Git discovery has invalid or duplicate references")
		}
		seen[name] = true
		if name == ref {
			if oid != old {
				return "", errors.New("existing feedback branch moved before push")
			}
			found = true
		}
	}
	if !found || !capabilities["report-status"] {
		return "", errors.New("Git server omitted the exact existing branch or mandatory status capability")
	}
	format := "sha1"
	if len(old) == 64 {
		format = "sha256"
	}
	for capability := range capabilities {
		if strings.HasPrefix(capability, "object-format=") && capability != "object-format="+format {
			return "", errors.New("Git object formats differ")
		}
	}
	if format == "sha256" && !capabilities["object-format=sha256"] {
		return "", errors.New("Git server omitted SHA-256 support")
	}
	selected := "report-status"
	if capabilities["object-format="+format] {
		selected += " object-format=" + format
	}
	return selected, nil
}

func feedbackPushStatus(data []byte, ref string) error {
	unpack, flush, err := feedbackPacket(&data)
	if err != nil || flush || unpack != "unpack ok" {
		return errors.New("Git server did not confirm object unpacking")
	}
	status, flush, err := feedbackPacket(&data)
	if err != nil || flush || status != "ok "+ref {
		return errors.New("Git server did not confirm the exact branch update")
	}
	_, flush, err = feedbackPacket(&data)
	if err != nil || !flush || len(data) != 0 {
		return errors.New("Git branch update status is incomplete or conflicting")
	}
	return nil
}

func feedbackPushBranch(branch string) bool {
	if branch == "" || branch == "HEAD" || branch == "main" || branch == "develop" || len(branch) > 1024 || !utf8.ValidString(branch) || strings.HasPrefix(branch, "-") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return false
	}
	for _, c := range branch {
		if c <= 32 || c == 127 || strings.ContainsRune(`~^:?*[\`, c) {
			return false
		}
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
