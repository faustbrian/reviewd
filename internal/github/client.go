// Package github owns authenticated GitHub I/O. Tokens never enter reviewer
// configuration, snapshots, prompts, or worker environments.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
)

type Options struct {
	Host string
	// BaseURL is an application/test adapter option, never repository input.
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}
type Client struct {
	host           string
	endpoint       *url.URL
	token          string
	http           *http.Client
	discoveryOnce  sync.Once
	discoveryHTTP  *http.Client
	discoveryCache *conditionalCache
}
type Identity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type PreferenceSource struct {
	Host, Repository, Branch, BaseSHA string
	Preferences                       *config.RepositoryPreferences
}
type HTTPError struct {
	Status             int
	RetryAfter         time.Duration
	RateLimitRemaining string
}

func (e *HTTPError) Error() string {
	switch e.Status {
	case http.StatusOK:
		return "GitHub GraphQL rate limit reached; wait for the server retry interval"
	case 401:
		return "GitHub authentication failed; run gh auth login for the configured host"
	case 403:
		return "GitHub denied access or rate-limited the request; check account permissions and retry guidance"
	case 404:
		return "GitHub resource unavailable; check the repository identity and account access"
	case 429:
		return "GitHub rate limit reached; wait for the server retry interval"
	default:
		return fmt.Sprintf("GitHub request failed with HTTP %d", e.Status)
	}
}
func New(options Options) (*Client, error) {
	host := strings.ToLower(options.Host)
	if _, err := config.Repository(host, "reviewd/validation"); err != nil {
		return nil, errors.New("GitHub host must be a hostname without scheme, path or credentials")
	}
	endpoint := options.BaseURL
	if endpoint == "" {
		if host == "github.com" {
			endpoint = "https://api.github.com"
		} else {
			endpoint = "https://" + host + "/api/v3"
		}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid GitHub API endpoint")
	}
	if parsed.Scheme != "https" {
		ip := net.ParseIP(parsed.Hostname())
		if parsed.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("GitHub API endpoint must use HTTPS; HTTP is only allowed for owned loopback fixtures")
		}
	}
	if strings.TrimSpace(options.Token) == "" || strings.ContainsAny(options.Token, "\r\n") {
		return nil, errors.New("GitHub authentication unavailable; run gh auth login for the configured host")
	}
	httpClient := http.Client{Timeout: 20 * time.Second}
	if options.HTTPClient != nil {
		httpClient = *options.HTTPClient
		if httpClient.Timeout == 0 {
			httpClient.Timeout = 20 * time.Second
		}
	}
	httpClient.CheckRedirect = func(request *http.Request, previous []*http.Request) error {
		if len(previous) >= 10 {
			return errors.New("GitHub redirect limit exceeded")
		}
		if request.URL.Host != parsed.Host || request.URL.Scheme != parsed.Scheme {
			return errors.New("GitHub redirected outside the authenticated API host")
		}
		return nil
	}
	return &Client{host: host, endpoint: parsed, token: options.Token, http: &httpClient}, nil
}
func FromCLI(ctx context.Context, host string) (*Client, error) {
	if _, err := config.Repository(host, "reviewd/validation"); err != nil {
		return nil, errors.New("invalid configured GitHub host")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, errors.New("GitHub CLI unavailable; install gh and run gh auth login for the configured host")
	}
	command := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host)
	// Capture credential output directly into memory, never diagnostics/logs.
	data, err := command.Output()
	if err != nil {
		return nil, errors.New("GitHub authentication unavailable; run gh auth login for the configured host")
	}
	if len(data) > 8192 {
		return nil, errors.New("GitHub credential exceeds supported size")
	}
	return New(Options{Host: host, Token: strings.TrimSpace(string(data))})
}
func (c *Client) get(ctx context.Context, path string, target any) error {
	endpoint := strings.TrimRight(c.endpoint.String(), "/") + "/" + strings.TrimLeft(path, "/")
	return c.readRequest(ctx, http.MethodGet, endpoint, nil, target)
}

func (c *Client) getPage(ctx context.Context, path string, target any) (http.Header, error) {
	endpoint := strings.TrimRight(c.endpoint.String(), "/") + "/" + strings.TrimLeft(path, "/")
	var headers http.Header
	err := c.readRequestWithHeaders(ctx, http.MethodGet, endpoint, nil, target, &headers)
	return headers, err
}

// readRequest is used only by application-owned GET endpoints and fixed
// GraphQL read queries. It is not a model-visible arbitrary request tool.
func (c *Client) readRequest(ctx context.Context, method, endpoint string, payload []byte, target any) error {
	return c.readRequestWithHeaders(ctx, method, endpoint, payload, target, nil)
}

func (c *Client) readRequestWithHeaders(ctx context.Context, method, endpoint string, payload []byte, target any, headers *http.Header) error {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid GitHub read request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "reviewd")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("GitHub read request failed; check network access and configured host")
	}
	defer response.Body.Close()
	if headers != nil {
		*headers = response.Header.Clone()
	}
	if response.StatusCode != http.StatusOK {
		return remoteHTTPError(response, time.Now())
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil {
		return errors.New("GitHub response was incomplete")
	}
	if len(data) > 8*1024*1024 {
		return errors.New("GitHub response exceeds 8 MiB; complete evidence is unavailable")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("GitHub returned invalid JSON evidence")
	}
	return nil
}

// remoteHTTPError preserves the same server guidance for reads and writes.
func remoteHTTPError(response *http.Response, now time.Time) *HTTPError {
	retry := time.Duration(0)
	if seconds, err := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
		maximum := time.Duration(1<<63 - 1)
		if seconds > uint64(maximum/time.Second) {
			retry = maximum
		} else {
			retry = time.Duration(seconds) * time.Second
		}
	} else if deadline, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
		retry = deadline.Sub(now)
		if retry < 0 {
			retry = 0
		}
	}
	remaining := response.Header.Get("X-RateLimit-Remaining")
	if remaining == "0" {
		if seconds, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && seconds > 0 {
			if delay := time.Unix(seconds, 0).Sub(now); delay > retry {
				retry = delay
			}
		}
	}
	if retry <= 0 && (response.StatusCode == http.StatusTooManyRequests || (response.StatusCode == http.StatusForbidden && remaining == "0")) {
		retry = time.Minute
	}
	if retry <= 0 && response.StatusCode == http.StatusForbidden && response.Body != nil {
		// Secondary limits may omit retry headers. Inspect only the bounded
		// classification message; never retain or print the remote error body.
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		var failure struct {
			Message string            `json:"message"`
			Errors  []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(data, &failure) == nil && (rateLimitMessage(failure.Message) || rateLimitErrors(failure.Errors)) {
			retry = time.Minute
		}
	}
	return &HTTPError{Status: response.StatusCode, RetryAfter: retry, RateLimitRemaining: remaining}
}

func rateLimitMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "secondary rate limit") || strings.Contains(message, "api rate limit exceeded")
}

func rateLimitErrors(failures []json.RawMessage) bool {
	for _, raw := range failures {
		var failure struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &failure) == nil && (failure.Type == "RATE_LIMITED" || rateLimitMessage(failure.Message)) {
			return true
		}
	}
	return false
}

// readInterruption keeps retry constraints out of unavailable-evidence prose.
// Permanent access gaps may remain review evidence; throttling must stop reads.
func readInterruption(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var remote *HTTPError
	if errors.As(err, &remote) && (remote.Status == http.StatusTooManyRequests || remote.RetryAfter > 0) {
		return err
	}
	return nil
}

// RetryAfter returns the longest server constraint across repository errors.
func RetryAfter(err error) time.Duration {
	var delay time.Duration
	pending := []error{err}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if limited, ok := current.(*HTTPError); ok && limited.RetryAfter > delay {
			delay = limited.RetryAfter
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return delay
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var identity Identity
	if err := c.get(ctx, "user", &identity); err != nil {
		return identity, err
	}
	if identity.ID <= 0 || strings.TrimSpace(identity.Login) == "" {
		return identity, errors.New("GitHub returned no authenticated identity")
	}
	return identity, nil
}
func (c *Client) DefaultPreferences(ctx context.Context, repository string) (PreferenceSource, error) {
	canonical, err := config.Repository(c.host, repository)
	if err != nil || !strings.HasPrefix(canonical, c.host+"/") {
		return PreferenceSource{}, errors.New("repository host does not match the authenticated GitHub host")
	}
	parts := strings.Split(canonical, "/")
	relative := parts[1] + "/" + parts[2]
	prefix := "repos/" + url.PathEscape(parts[1]) + "/" + url.PathEscape(parts[2])
	source := PreferenceSource{Host: c.host, Repository: canonical}
	var metadata struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, prefix, &metadata); err != nil {
		return source, err
	}
	if metadata.ID <= 0 || strings.ToLower(metadata.FullName) != relative {
		return source, errors.New("repository identity changed; update the configured repository scope")
	}
	if metadata.DefaultBranch == "" {
		return source, errors.New("repository has no default branch; initialize it before resolving repository preferences")
	}
	source.Branch = metadata.DefaultBranch
	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := c.get(ctx, prefix+"/commits/"+url.PathEscape(metadata.DefaultBranch), &commit); err != nil {
		return source, err
	}
	if !objectPattern.MatchString(commit.SHA) || !objectPattern.MatchString(commit.Commit.Tree.SHA) {
		return source, errors.New("GitHub returned no immutable default-branch boundary")
	}
	source.BaseSHA = commit.SHA
	source.Preferences, err = c.preferencesAtTree(ctx, prefix, source.BaseSHA, commit.Commit.Tree.SHA)
	return source, err
}

var objectPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
