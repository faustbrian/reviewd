// Package containerapi owns the local Docker Engine control-plane boundary.
// It grants no model tool, registry, shell, or remote publication capability.
package containerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("local container runtime is unavailable")
var ErrUncertain = errors.New("container resource state is unproven")

const apiVersion = "1.44"
const maxResponse = 1 << 20

var shortID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var fullID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Client struct {
	endpoint string
	http     *http.Client
}
type Identity struct{ DaemonID, APIVersion, Architecture string }

// Resource is application-owned durable identity, never a model-supplied selector.
// An empty ContainerID represents an unresolved create intent, not absence.
type Resource struct {
	Endpoint    string `json:"endpoint"`
	DaemonID    string `json:"daemon_id"`
	OwnerID     string `json:"owner_id"`
	ResourceID  string `json:"resource_id"`
	ContainerID string `json:"container_id"`
}

// Valid checks durable selectors without connecting to the runtime.
func (r Resource) Valid() bool {
	_, _, err := endpointPath(r.Endpoint)
	return err == nil && identityValue(r.DaemonID) && shortID.MatchString(r.OwnerID) && shortID.MatchString(r.ResourceID) && (r.ContainerID == "" || fullID.MatchString(r.ContainerID))
}

// Present never certifies completion: even an exited container must be removed
// before its owner can rely on canonical absence. No resource is mutated here.
type Observation struct {
	ContainerID string
	Present     bool
}

func endpointPath(endpoint string) (string, string, error) {
	if len(endpoint) > 4096 {
		return "", "", ErrUnavailable
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) || filepath.Clean(u.Path) != u.Path || u.Path == "/" {
		return "", "", ErrUnavailable
	}
	return u.Path, (&url.URL{Scheme: "unix", Path: u.Path}).String(), nil
}
func New(endpoint string) (*Client, error) {
	path, canonical, err := endpointPath(endpoint)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", path)
	}}
	return &Client{endpoint: canonical, http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Close() error  { c.http.CloseIdleConnections(); return nil }
func (r Resource) Name() string { return "reviewd-" + r.OwnerID + "-" + r.ResourceID }
func (c *Client) get(ctx context.Context, path string, target any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return 0, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	// Never expose arbitrary daemon error bodies or follow authentication/remote redirects.
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return 0, err
	}
	if len(body) > maxResponse || json.Unmarshal(body, target) != nil {
		return 0, errors.New("invalid bounded container control response")
	}
	return response.StatusCode, nil
}
func versionParts(value string) (int, int, bool) {
	pieces := strings.Split(value, ".")
	if len(pieces) != 2 {
		return 0, 0, false
	}
	major, a := strconv.Atoi(pieces[0])
	minor, b := strconv.Atoi(pieces[1])
	if a != nil || b != nil || major < 1 || minor < 0 || strconv.Itoa(major) != pieces[0] || strconv.Itoa(minor) != pieces[1] {
		return 0, 0, false
	}
	return major, minor, true
}
func supports(version, minimum string) bool {
	major, minor, ok := versionParts(version)
	if !ok || major != 1 || minor < 44 {
		return false
	}
	lowMajor, lowMinor, ok := versionParts(minimum)
	return ok && lowMajor == 1 && lowMinor <= 44
}
func architecture(value string) string {
	switch value {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	}
	return ""
}
func identityValue(value string) bool {
	return value != "" && len(value) <= 256 && strings.IndexFunc(value, func(r rune) bool { return r <= 32 || r == 127 }) < 0
}
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var version struct {
		API     string `json:"ApiVersion"`
		Minimum string `json:"MinAPIVersion"`
		OS      string `json:"Os"`
		Arch    string `json:"Arch"`
	}
	status, err := c.get(ctx, "/version", &version)
	if err != nil || status != http.StatusOK || !supports(version.API, version.Minimum) || version.OS != "linux" || architecture(version.Arch) == "" {
		return Identity{}, errors.Join(ErrUnavailable, err)
	}
	var info struct {
		ID   string `json:"ID"`
		OS   string `json:"OSType"`
		Arch string `json:"Architecture"`
	}
	status, err = c.get(ctx, "/v"+apiVersion+"/info", &info)
	if err != nil || status != http.StatusOK || !identityValue(info.ID) || info.OS != "linux" || architecture(info.Arch) != architecture(version.Arch) {
		return Identity{}, errors.Join(ErrUnavailable, err)
	}
	return Identity{DaemonID: info.ID, APIVersion: apiVersion, Architecture: architecture(info.Arch)}, nil
}
func (c *Client) Observe(ctx context.Context, resource Resource) (Observation, error) {
	_, endpoint, err := endpointPath(resource.Endpoint)
	if err != nil || endpoint != c.endpoint || !resource.Valid() {
		return Observation{}, ErrUncertain
	}
	before, err := c.Identity(ctx)
	if err != nil || before.DaemonID != resource.DaemonID {
		return Observation{}, errors.Join(ErrUncertain, err)
	}
	selector := resource.ContainerID
	if selector == "" {
		selector = resource.Name()
	}
	var value struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	status, err := c.get(ctx, "/v"+apiVersion+"/containers/"+selector+"/json", &value)
	if err != nil {
		return Observation{}, errors.Join(ErrUncertain, err)
	}
	observation := Observation{ContainerID: resource.ContainerID}
	switch status {
	case http.StatusNotFound:
		// A create request can still finish after owner loss. Name-only absence
		// cannot certify that an unbound resource will never appear later.
		if resource.ContainerID == "" {
			return Observation{}, ErrUncertain
		}
	case http.StatusOK:
		if !fullID.MatchString(value.ID) || (resource.ContainerID != "" && value.ID != resource.ContainerID) || value.Name != "/"+resource.Name() || value.Config.Labels["io.reviewd.owner"] != resource.OwnerID || value.Config.Labels["io.reviewd.resource"] != resource.ResourceID {
			return Observation{}, ErrUncertain
		}
		observation = Observation{ContainerID: value.ID, Present: true}
	default:
		return Observation{}, fmt.Errorf("%w: container inspection status %d", ErrUncertain, status)
	}
	after, err := c.Identity(ctx)
	if err != nil || after != before {
		return Observation{}, errors.Join(ErrUncertain, err)
	}
	return observation, nil
}
