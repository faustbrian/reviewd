package containercmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/containerapi"
	"github.com/faustbrian/reviewd/internal/store"
)

// This opt-in suite uses only owned foreground work and canonical metadata.
// It never starts detached/background helpers or interrupts a process/daemon.
func TestNativeOwnedContainerCommands(t *testing.T) {
	if os.Getenv("REVIEWD_CONTAINER_COMMAND_NATIVE") != "1" {
		t.Skip("explicit owned-container acceptance is not enabled")
	}
	parent := os.Getenv("REVIEWD_CONTAINER_FIXTURE_PARENT")
	endpoint := os.Getenv("REVIEWD_CONTAINER_ENDPOINT")
	image := os.Getenv("REVIEWD_CONTAINER_IMAGE")
	if parent == "" || endpoint == "" || image == "" {
		t.Fatal("explicit fixture parent, local endpoint and cached immutable image are required")
	}
	preflight, err := containerapi.New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	metadataContext, stopMetadata := context.WithTimeout(context.Background(), 10*time.Second)
	identity, metadataErr := preflight.Identity(metadataContext)
	stopMetadata()
	_ = preflight.Close()
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	helper, helperHash := buildFixtureHelper(t, identity.Architecture)
	root, err := os.MkdirTemp(parent, "native-container-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	durable, err := store.Open(ctx, filepath.Join(root, "profile"))
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		_ = durable.Close()
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	var hosts []*Host
	defer func() {
		var cleanupErr error
		for _, host := range hosts {
			cleanupErr = errors.Join(cleanupErr, host.Close())
		}
		cleanupErr = errors.Join(cleanupErr, owner.Close(), durable.Close())
		if cleanupErr != nil {
			t.Errorf("unproven owned fixture retained for reconciliation at %s: %v", root, cleanupErr)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error("cannot remove proven owned fixture", err)
		}
	}()
	workspace := filepath.Join(owner.Root, "checkout")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "source"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-canary")
	if err := os.WriteFile(outside, []byte("outside private checkout"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWD_NATIVE_CANARY", "host-only-fixture-value")
	database, err := sql.Open("sqlite", "file:"+filepath.Join(durable.Directory(), "reviewd.sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, writable := range []bool{false, true} {
		name := "read-only"
		if writable {
			name = "writable"
		}
		t.Run(name, func(t *testing.T) {
			host, err := New(ctx, owner, Options{Endpoint: endpoint, Image: image, Workspace: workspace, Writable: writable, Helper: helper, HelperSHA256: helperHash})
			if err != nil {
				t.Fatal(err)
			}
			hosts = append(hosts, host)
			release := ".release-" + name
			script := `test "$(cat source)" = before || exit 2
 test -z "$REVIEWD_NATIVE_CANARY$OPENAI_API_KEY$GH_TOKEN$GITHUB_TOKEN$SSH_AUTH_SOCK$CODEX_HOME$DOCKER_HOST$REVIEWD_PRIVATE_CAPTURE_KEY" || exit 3
 if cat /proc/1/environ >/dev/null 2>&1; then exit 8; fi
 test ! -e "$1" || exit 4
 test ! -S /var/run/docker.sock || exit 5
 i=0; while test ! -e "$2" && test "$i" -lt 100; do sleep 0.1; i=$((i+1)); done
 test -e "$2" || exit 6
 `
			if writable {
				script += `printf 'after\n' >source; printf 'fixture index' >.git/index; printf 'executable\n' >executable; chmod 755 executable; ln -s source source-link
 `
			} else {
				script += `if (printf changed >source) 2>/dev/null; then exit 7; fi
 `
			}
			script += `printf 'scratch\n' >/tmp/owned-scratch
 if (printf forbidden >/reviewd-output/forbidden) 2>/dev/null; then exit 9; fi
 if mkdir /reviewd-output/forbidden-dir 2>/dev/null; then exit 10; fi
 test "$(cat /reviewd-output/protected-canary)" = protected || exit 13
 if truncate -s 0 /reviewd-output/protected-canary 2>/dev/null; then exit 14; fi
 if rm /reviewd-output/protected-canary 2>/dev/null; then exit 15; fi
 ln -s /reviewd-output /tmp/output-alias
 if (printf forbidden >/tmp/output-alias/forbidden) 2>/dev/null; then exit 16; fi
 test "$(cat /reviewd-output/protected-canary)" = protected || exit 17
 if ln /tmp/owned-scratch /reviewd-output/forbidden-link 2>/dev/null; then exit 11; fi
 if mv /tmp/owned-scratch /reviewd-output/forbidden-move 2>/dev/null; then exit 12; fi
 printf 'shared scratch\n' >/dev/shm/owned-scratch
 go version`
			type outcome struct {
				result codex.CommandResult
				err    error
			}
			returned := make(chan outcome, 1)
			go func() {
				result, err := host.Run(ctx, []string{"/bin/sh", "-c", script, "owned-foreground-fixture", outside, release}, 30*time.Second)
				returned <- outcome{result, err}
			}()
			// Release normal foreground work even if grant inspection fails; this is
			// not a cancellation/termination experiment.
			releasePath := filepath.Join(workspace, release)
			grantErr := inspectNativeGrant(ctx, database, owner.ID, endpoint, workspace, writable)
			releaseErr := os.WriteFile(releasePath, []byte("finish normally"), 0600)
			observed := <-returned
			if releaseErr != nil {
				t.Fatal(releaseErr)
			}
			if observed.err != nil || observed.result.ExitCode != 0 || !strings.Contains(observed.result.Stdout, "linux/") {
				t.Fatalf("native foreground command failed: %+v %v; grant inspection: %v", observed.result, observed.err, grantErr)
			}
			if grantErr != nil {
				t.Fatal(grantErr)
			}
			for _, exit := range []int{0, 7} {
				literal := "printf 'exact stdout\\000é\\n'; printf 'exact stderr\\000ø\\n' >&2; exit " + strconv.Itoa(exit)
				result, err := host.Run(ctx, []string{"/bin/sh", "-c", literal}, 30*time.Second)
				if err != nil || result.ExitCode != exit || result.Stdout != "exact stdout\x00é\n" || result.Stderr != "exact stderr\x00ø\n" {
					t.Fatalf("separate literal output or exit lost: %+v %v", result, err)
				}
			}
			result, err := host.Run(ctx, []string{"/bin/sh", "-c", "exit 0"}, 30*time.Second)
			if err != nil || result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
				t.Fatalf("empty foreground output not complete: %+v %v", result, err)
			}
			if err := host.Close(); err != nil {
				t.Fatal(err)
			}
			expected := "before\n"
			if writable {
				expected = "after\n"
			}
			if data, err := os.ReadFile(filepath.Join(workspace, "source")); err != nil || string(data) != expected {
				t.Fatal("source read/write result differs", err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "outside private checkout" {
				t.Fatal("outside canary changed", err)
			}
			if writable {
				if data, err := os.ReadFile(filepath.Join(workspace, ".git", "index")); err != nil || string(data) != "fixture index" {
					t.Fatal("private metadata write missing", err)
				}
				if info, err := os.Stat(filepath.Join(workspace, "executable")); err != nil || info.Mode().Perm()&0111 == 0 {
					t.Fatal("executable mode lost", err)
				}
				if target, err := os.Readlink(filepath.Join(workspace, "source-link")); err != nil || target != "source" {
					t.Fatal("symlink data lost", err)
				}
			}
			t.Logf("foreground command complete in %s/%s; private mounts and canonical owner cleanup only", host.Environment().OS, host.Environment().Architecture)
		})
		if t.Failed() {
			return
		}
	}
}

func inspectNativeGrant(ctx context.Context, database *sql.DB, ownerID, endpoint, workspace string, writable bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "unix" {
		return errors.New("native fixture requires its explicit Unix endpoint")
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", parsed.Path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var raw string
		if err := database.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", ownerID).Scan(&raw); err != nil {
			return err
		}
		var entries map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &entries) != nil {
			return errors.New("native fixture ownership metadata is malformed")
		}
		for _, entry := range entries {
			var descriptor struct {
				Kind     string
				Resource containerapi.Resource
			}
			if json.Unmarshal(entry, &descriptor) != nil || descriptor.Kind != "docker" || descriptor.Resource.ContainerID == "" {
				continue
			}
			resource := descriptor.Resource
			if !resource.Valid() || resource.OwnerID != ownerID {
				return errors.New("native fixture resource identity mismatched")
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v1.44/containers/"+resource.ContainerID+"/json", nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			_ = response.Body.Close()
			if readErr != nil || len(body) > 1<<20 {
				return errors.New("native fixture inspect response incomplete")
			}
			if response.StatusCode == http.StatusNotFound {
				continue
			}
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("native fixture inspect status %d", response.StatusCode)
			}
			var observed struct {
				ID     string `json:"Id"`
				Name   string
				State  struct{ Running bool }
				Config struct {
					User   string
					Tty    bool
					Labels map[string]string
				}
				HostConfig struct {
					NetworkMode                string
					ReadonlyRootfs, Privileged bool
					CapDrop, SecurityOpt       []string
					PidsLimit, Memory          int64
					LogConfig                  struct{ Type string }
				}
				Mounts []struct {
					Type, Source, Destination string
					RW                        bool
				}
			}
			if json.Unmarshal(body, &observed) != nil {
				return errors.New("native fixture inspect payload invalid")
			}
			if !observed.State.Running {
				continue
			}
			if observed.ID != resource.ContainerID || observed.Name != "/"+resource.Name() || observed.Config.Labels["io.reviewd.owner"] != ownerID || observed.Config.Labels["io.reviewd.resource"] != resource.ResourceID {
				return errors.New("native runtime object is not the owned fixture")
			}
			if observed.HostConfig.NetworkMode != "none" || !observed.HostConfig.ReadonlyRootfs || observed.HostConfig.Privileged || observed.Config.Tty || observed.Config.User != strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()) || strings.Join(observed.HostConfig.CapDrop, ",") != "ALL" || observed.HostConfig.LogConfig.Type != "none" || observed.HostConfig.PidsLimit != 256 || observed.HostConfig.Memory != 2<<30 {
				return errors.New("actual native container grants differ from the restricted command scope")
			}
			noPrivileges := false
			for _, opt := range observed.HostConfig.SecurityOpt {
				if opt == "no-new-privileges" || opt == "no-new-privileges:true" {
					noPrivileges = true
				}
			}
			if !noPrivileges {
				return errors.New("actual native container lacks no-new-privileges")
			}
			found := false
			for _, mount := range observed.Mounts {
				if mount.Destination == "/tmp" && mount.Type == "tmpfs" {
					continue
				}
				if mount.Destination == "/reviewd-command-host" {
					if mount.Type != "bind" || mount.RW {
						return errors.New("actual helper is writable or not a bind")
					}
					continue
				}
				if mount.Destination == "/reviewd-output" {
					if mount.Type != "bind" || !mount.RW || !strings.HasPrefix(mount.Source, filepath.Dir(canonical)+string(filepath.Separator)) {
						return errors.New("actual capture mount escaped the private owner")
					}
					// App-owned finite canary, created before releasing the foreground
					// command. No hostile fill, interruption or process probe.
					if err := os.WriteFile(filepath.Join(mount.Source, "protected-canary"), []byte("protected\n"), 0600); err != nil {
						return err
					}
					continue
				}
				if mount.Type != "bind" || mount.Source != canonical || mount.Destination != "/workspace" || mount.RW != writable {
					return errors.New("actual native mount expands the private workspace boundary")
				}
				found = true
			}
			if !found {
				return errors.New("actual native private-workspace mount absent")
			}
			return nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return errors.New("native foreground fixture did not reach inspectable running state")
		}
	}
}
