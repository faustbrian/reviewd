// Package github owns authenticated GitHub I/O. Tokens never enter reviewer
// configuration, snapshots, prompts, or worker environments.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
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
	host          string
	endpoint      *url.URL
	token         string
	http          *http.Client
	discoveryOnce sync.Once
	discoveryHTTP *http.Client
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
		retry := time.Duration(0)
		if seconds, err := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
			maximum := time.Duration(1<<63 - 1)
			if seconds > uint64(maximum/time.Second) {
				retry = maximum
			} else {
				retry = time.Duration(seconds) * time.Second
			}
		} else if deadline, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
			retry = time.Until(deadline)
			if retry < 0 {
				retry = 0
			}
		}
		return &HTTPError{Status: response.StatusCode, RetryAfter: retry, RateLimitRemaining: response.Header.Get("X-RateLimit-Remaining")}
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
	var tree struct {
		SHA       string                                    `json:"sha"`
		Truncated *bool                                     `json:"truncated"`
		Tree      *[]struct{ Path, Mode, Type, SHA string } `json:"tree"`
	}
	if err := c.get(ctx, prefix+"/git/trees/"+commit.Commit.Tree.SHA, &tree); err != nil {
		return source, err
	}
	if tree.Truncated == nil || *tree.Truncated || tree.Tree == nil || tree.SHA != commit.Commit.Tree.SHA {
		return source, errors.New("repository preference tree is incomplete or mismatched")
	}
	for _, entry := range *tree.Tree {
		if entry.Path != ".reviewd" {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") || !objectPattern.MatchString(entry.SHA) {
			return source, errors.New("trusted .reviewd must be a regular file, not a symlink or executable tool")
		}
		var blob struct {
			SHA      string `json:"sha"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
			Size     int64  `json:"size"`
		}
		if err := c.get(ctx, prefix+"/git/blobs/"+entry.SHA, &blob); err != nil {
			return source, err
		}
		if blob.SHA != entry.SHA || blob.Encoding != "base64" || blob.Size < 0 || blob.Size > 1024*1024 {
			return source, errors.New("trusted .reviewd blob is mismatched, oversized or has an unsupported encoding")
		}
		data, err := base64.StdEncoding.DecodeString(blob.Content)
		if err != nil || int64(len(data)) != blob.Size {
			return source, errors.New("trusted .reviewd blob is incomplete")
		}
		source.Preferences = &config.RepositoryPreferences{BaseSHA: source.BaseSHA, Data: data}
		return source, nil
	}
	return source, nil
}

var objectPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
