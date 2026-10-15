package containerapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/commandcapture"
)

// CommandSpec is app-owned; repository/model input never selects these grants.
// Image is a locally present immutable image ID, never a mutable tag or pull.
type CommandSpec struct {
	Image           string
	Workspace       string
	Argv            []string
	Writable        bool
	Helper, Capture string
	Key             []byte
}

func (c *Client) commandRequest(ctx context.Context, method, path string, value any, limit int, wait bool) (int, []byte, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return 0, nil, err
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *c.http
	if wait {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 21*time.Minute {
			return 0, nil, ErrUncertain
		}
		// A repository check may legitimately exceed the metadata timeout.
		// Its caller must supply the bounded command lifetime instead.
		client.Timeout = 0
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, errors.Join(ErrUncertain, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent {
		return response.StatusCode, nil, nil // Never expose arbitrary error bodies.
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || len(data) > limit {
		return 0, nil, errors.Join(ErrUncertain, err)
	}
	return response.StatusCode, data, nil
}
func (c *Client) expectedIdentity(ctx context.Context, resource Resource) (Identity, error) {
	_, endpoint, err := endpointPath(resource.Endpoint)
	if err != nil || endpoint != c.endpoint || !resource.Valid() {
		return Identity{}, ErrUncertain
	}
	identity, err := c.Identity(ctx)
	if err != nil || identity.DaemonID != resource.DaemonID {
		return Identity{}, errors.Join(ErrUncertain, err)
	}
	return identity, nil
}
func (c *Client) ownedPresent(ctx context.Context, resource Resource) error {
	if resource.ContainerID == "" {
		return ErrUncertain
	}
	observation, err := c.Observe(ctx, resource)
	if err != nil || !observation.Present || observation.ContainerID != resource.ContainerID {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}

// CheckImage establishes a locally present immutable toolchain before a
// command intent is reserved. It never pulls or runs an image.
func (c *Client) CheckImage(ctx context.Context, imageID string) (Identity, error) {
	if _, err := containerUser(os.Getuid(), os.Getgid()); err != nil {
		return Identity{}, err
	}
	identity, _, err := c.checkImage(ctx, imageID)
	return identity, err
}

func imageSearchPath(environment []string) (string, error) {
	search := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	found := false
	for _, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			if found {
				return "", ErrUnavailable
			}
			found = true
			search = strings.TrimPrefix(entry, "PATH=")
		}
	}
	if len(search) > 8192 || strings.ContainsAny(search, "\x00\r\n") {
		return "", ErrUnavailable
	}
	for _, directory := range strings.Split(search, ":") {
		if !path.IsAbs(directory) || path.Clean(directory) != directory {
			return "", ErrUnavailable
		}
		for _, writable := range []string{"/workspace", commandcapture.GuestDirectory, "/tmp", "/dev", "/proc", "/sys"} {
			if directory == writable || strings.HasPrefix(directory, writable+"/") {
				return "", ErrUnavailable
			}
		}
	}
	// The operator selects the immutable image. Preserve its installed toolchain
	// lookup, never the host's environment or a repository-controlled PATH.
	return search, nil
}

func (c *Client) checkImage(ctx context.Context, imageID string) (Identity, string, error) {
	if !strings.HasPrefix(imageID, "sha256:") || !fullID.MatchString(strings.TrimPrefix(imageID, "sha256:")) {
		return Identity{}, "", ErrUnavailable
	}
	before, err := c.Identity(ctx)
	if err != nil {
		return Identity{}, "", err
	}
	var image struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		Config       *struct {
			Volumes map[string]json.RawMessage
			Env     []string
		}
	}
	status, err := c.get(ctx, "/v"+apiVersion+"/images/"+imageID+"/json", &image)
	if err != nil || status != http.StatusOK || image.ID != imageID || image.OS != "linux" || architecture(image.Architecture) != before.Architecture || image.Config == nil || len(image.Config.Volumes) != 0 {
		return Identity{}, "", errors.Join(ErrUnavailable, err)
	}
	search, err := imageSearchPath(image.Config.Env)
	if err != nil {
		return Identity{}, "", err
	}
	after, err := c.Identity(ctx)
	if err != nil || after != before {
		return Identity{}, "", errors.Join(ErrUncertain, err)
	}
	return before, search, nil
}

// Create requires a previously reserved application identity. The caller must
// durably Bind the returned ID before Start; create never starts or pulls.
// A valid returned ID can accompany an error and must still be reconciled.
func (c *Client) Create(ctx context.Context, resource Resource, spec CommandSpec) (string, error) {
	return c.create(ctx, resource, spec, os.Getuid(), os.Getgid())
}

func containerUser(uid, gid int) (string, error) {
	if uid <= 0 || gid < 0 {
		return "", errors.Join(ErrUnavailable, errors.New("container commands require a non-root host identity"))
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
}

// Identity is supplied only by Create's host observation, never CommandSpec.
func (c *Client) create(ctx context.Context, resource Resource, spec CommandSpec, uid, gid int) (string, error) {
	user, userErr := containerUser(uid, gid)
	if userErr != nil {
		return "", userErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if resource.ContainerID != "" || !strings.HasPrefix(spec.Image, "sha256:") || !fullID.MatchString(strings.TrimPrefix(spec.Image, "sha256:")) || len(spec.Argv) == 0 || len(spec.Argv) > 256 {
		return "", ErrUncertain
	}
	total := 0
	for _, arg := range spec.Argv {
		total += len(arg)
		if strings.ContainsRune(arg, 0) {
			return "", ErrUncertain
		}
	}
	info, err := os.Lstat(spec.Workspace)
	if err != nil || !filepath.IsAbs(spec.Workspace) || filepath.Clean(spec.Workspace) != spec.Workspace || !info.IsDir() || info.Mode().Perm()&0077 != 0 || total > 128<<10 {
		return "", ErrUncertain
	}
	captureInfo, captureErr := os.Lstat(spec.Capture)
	helperInfo, helperErr := os.Lstat(spec.Helper)
	if len(spec.Key) != 32 || captureErr != nil || helperErr != nil || !filepath.IsAbs(spec.Capture) || filepath.Clean(spec.Capture) != spec.Capture || !captureInfo.IsDir() || captureInfo.Mode().Perm()&0077 != 0 || !filepath.IsAbs(spec.Helper) || filepath.Clean(spec.Helper) != spec.Helper || !helperInfo.Mode().IsRegular() || helperInfo.Mode().Perm()&0022 != 0 || helperInfo.Mode().Perm()&0111 == 0 {
		return "", ErrUncertain
	}
	identity, err := c.expectedIdentity(ctx, resource)
	if err != nil {
		return "", err
	}
	imageIdentity, search, err := c.checkImage(ctx, spec.Image)
	if err != nil {
		return "", errors.Join(ErrUnavailable, err)
	}
	if imageIdentity != identity {
		return "", ErrUncertain
	}
	after, err := c.expectedIdentity(ctx, resource)
	if err != nil || after != identity {
		return "", errors.Join(ErrUncertain, err)
	}
	// Sharing can report a different guest owner even though the caller owns
	// this private host checkout. Trust only that exact application-bound mount,
	// in this command environment; never change global policy or trust wildcards.
	request := map[string]any{
		"Image": spec.Image, "User": user, "Cmd": append([]string{commandcapture.GuestHelper, "--protocol=1", "--nonce=" + resource.ResourceID, "--"}, spec.Argv...), "Entrypoint": []string{}, "WorkingDir": "/workspace", "Tty": false,
		"Labels": map[string]string{"io.reviewd.owner": resource.OwnerID, "io.reviewd.resource": resource.ResourceID},
		"Env":    []string{"PATH=" + search, commandcapture.KeyEnvironment + "=" + hex.EncodeToString(spec.Key), "HOME=/tmp", "TMPDIR=/tmp", "GOCACHE=/tmp/go-build", "GOMODCACHE=/tmp/go-modules", "GOTMPDIR=/tmp", "GOPATH=/tmp/go", "GOWORK=off", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/workspace"},
		"HostConfig": map[string]any{
			"NetworkMode": "none", "ReadonlyRootfs": true, "Privileged": false, "AutoRemove": false,
			"CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}, "PidsLimit": 256,
			"PidMode": "", "IpcMode": "private", "CgroupnsMode": "private", "Memory": int64(2 << 30),
			"Init":      false,
			"Tmpfs":     map[string]string{"/tmp": "rw,nosuid,nodev,size=1073741824,mode=1777"},
			"Mounts":    []map[string]any{{"Type": "bind", "Source": spec.Workspace, "Target": "/workspace", "ReadOnly": !spec.Writable}, {"Type": "bind", "Source": spec.Helper, "Target": commandcapture.GuestHelper, "ReadOnly": true}, {"Type": "bind", "Source": spec.Capture, "Target": commandcapture.GuestDirectory, "ReadOnly": false}},
			"LogConfig": map[string]any{"Type": "none", "Config": map[string]string{}},
		},
	}
	query := url.Values{"name": {resource.Name()}, "platform": {"linux/" + identity.Architecture}}
	status, data, err := c.commandRequest(ctx, http.MethodPost, "/v"+apiVersion+"/containers/create?"+query.Encode(), request, maxResponse, false)
	if err != nil || status != http.StatusCreated {
		return "", errors.Join(ErrUncertain, err)
	}
	var created struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	if json.Unmarshal(data, &created) != nil || !fullID.MatchString(created.ID) {
		return "", ErrUncertain
	}
	if len(created.Warnings) != 0 {
		return created.ID, ErrUncertain
	}
	return created.ID, nil
}
func (c *Client) Start(ctx context.Context, resource Resource) error {
	if err := c.ownedPresent(ctx, resource); err != nil {
		return err
	}
	status, _, err := c.commandRequest(ctx, http.MethodPost, "/v"+apiVersion+"/containers/"+resource.ContainerID+"/start", nil, maxResponse, false)
	if err != nil || status != http.StatusNoContent {
		return errors.Join(ErrUncertain, err)
	}
	return nil // An acknowledgement, never a terminal result or shutdown proof.
}
func (c *Client) Wait(ctx context.Context, resource Resource) (int, error) {
	if err := c.ownedPresent(ctx, resource); err != nil {
		return 0, err
	}
	status, data, err := c.commandRequest(ctx, http.MethodPost, "/v"+apiVersion+"/containers/"+resource.ContainerID+"/wait?condition=not-running", nil, maxResponse, true)
	var result struct {
		StatusCode *int
		Error      *json.RawMessage
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(data, &result) != nil || result.StatusCode == nil || result.Error != nil || *result.StatusCode < 0 || *result.StatusCode > 255 {
		return 0, errors.Join(ErrUncertain, err)
	}
	if err := c.ownedPresent(ctx, resource); err != nil {
		return 0, err
	}
	return *result.StatusCode, nil
}
func (c *Client) Remove(ctx context.Context, resource Resource) error {
	if resource.ContainerID == "" {
		return ErrUncertain
	}
	observed, err := c.Observe(ctx, resource)
	if err != nil {
		return err
	}
	if observed.Present {
		status, _, err := c.commandRequest(ctx, http.MethodDelete, "/v"+apiVersion+"/containers/"+resource.ContainerID+"?force=true&v=true", nil, maxResponse, false)
		if err != nil || (status != http.StatusNoContent && status != http.StatusNotFound) {
			return errors.Join(ErrUncertain, err)
		}
	}
	observed, err = c.Observe(ctx, resource)
	if err != nil || observed.Present {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}
