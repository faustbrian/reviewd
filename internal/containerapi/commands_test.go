package containerapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"github.com/faustbrian/reviewd/internal/commandcapture"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureImage = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// The API peer is inert: no command, container or process is created.
func TestContainerCommandLifecycleUsesOwnedRestrictedRequests(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	capture := t.TempDir()
	if err := os.Chmod(capture, 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("inert helper fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	t.Setenv("REVIEWD_TEST_HOST_SECRET", "outside-command-environment")
	var created, started, removed atomic.Bool
	endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
		if metadata(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/images/"+fixtureImage+"/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": fixtureImage, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{"Volumes": map[string]any{}, "Env": []string{"PATH=/opt/fixture-tools/bin:/usr/bin"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/create":
			var request struct {
				Image, WorkingDir    string
				User                 string
				Cmd, Entrypoint, Env []string
				Labels               map[string]string
				HostConfig           struct {
					LogConfig struct {
						Type   string
						Config map[string]string
					}
					NetworkMode                            string
					ReadonlyRootfs, Privileged, AutoRemove bool
					CapDrop, SecurityOpt                   []string
					Init                                   bool
					Mounts                                 []struct {
						Type, Source, Target string
						ReadOnly             bool
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "bad request", 400)
				return
			}
			if r.URL.Query().Get("name") != "reviewd-"+ownerID+"-"+resourceID || r.URL.Query().Get("platform") != "linux/arm64" || request.Image != fixtureImage || request.WorkingDir != "/workspace" || strings.Join(request.Cmd, "|") != commandcapture.GuestHelper+"|--protocol=1|--nonce="+resourceID+"|--|printf|hello space" || request.Entrypoint == nil || len(request.Entrypoint) != 0 {
				t.Error("command identity/argv or guest platform was not sent exactly")
			}
			if request.Labels["io.reviewd.owner"] != ownerID || request.Labels["io.reviewd.resource"] != resourceID || request.HostConfig.NetworkMode != "none" || !request.HostConfig.ReadonlyRootfs || request.HostConfig.Privileged || request.HostConfig.AutoRemove || strings.Join(request.HostConfig.CapDrop, ",") != "ALL" || strings.Join(request.HostConfig.SecurityOpt, ",") != "no-new-privileges:true" {
				t.Error("command request omitted its owned isolation boundary")
			}
			if request.User != strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()) || request.HostConfig.LogConfig.Type != "none" || len(request.HostConfig.LogConfig.Config) != 0 {
				t.Error("command did not retain private workspace ownership or disable daemon log retention")
			}
			if len(request.HostConfig.Mounts) != 3 || request.HostConfig.Mounts[0].Type != "bind" || request.HostConfig.Mounts[0].Source != workspace || request.HostConfig.Mounts[0].Target != "/workspace" || !request.HostConfig.Mounts[0].ReadOnly {
				t.Error("command omitted its read-only private workspace")
			}
			if len(request.HostConfig.Mounts) == 3 {
				grant := request.HostConfig.Mounts[1]
				if grant.Source != helper || grant.Target != commandcapture.GuestHelper || !grant.ReadOnly {
					t.Error("helper grant differs")
				}
				grant = request.HostConfig.Mounts[2]
				if grant.Source != capture || grant.Target != commandcapture.GuestDirectory || grant.ReadOnly {
					t.Error("private capture grant differs")
				}
			}
			hasKey := false
			for _, entry := range request.Env {
				if entry == commandcapture.KeyEnvironment+"="+hex.EncodeToString(key) {
					hasKey = true
				}
			}
			if !hasKey || request.HostConfig.Init {
				t.Error("protected helper startup contract differs")
			}
			if strings.Contains(strings.Join(request.Env, "\n"), "outside-command-environment") {
				t.Error("host environment leaked into the command request")
			}
			hasGuestPath := false
			for _, entry := range request.Env {
				if entry == "PATH=/opt/fixture-tools/bin:/usr/bin" {
					hasGuestPath = true
				}
			}
			if !hasGuestPath {
				t.Error("selected guest toolchain PATH was replaced")
			}
			created.Store(true)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": containerID, "Warnings": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/"+containerID+"/json":
			if !created.Load() || removed.Load() {
				http.NotFound(w, r)
				return
			}
			ownedContainer(w)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/"+containerID+"/start":
			started.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/"+containerID+"/wait":
			if !started.Load() || r.URL.Query().Get("condition") != "not-running" {
				t.Error("wait bypassed start or selected another lifetime")
			}
			_, _ = w.Write([]byte(`{"StatusCode":7}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.44/containers/"+containerID:
			if r.URL.Query().Get("force") != "true" || r.URL.Query().Get("v") != "true" {
				t.Error("owned removal omitted complete cleanup")
			}
			removed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected command API request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	})
	client, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resource := ownedResource(endpoint)
	resource.ContainerID = ""
	id, err := client.Create(ctx, resource, CommandSpec{Image: fixtureImage, Workspace: workspace, Helper: helper, Capture: capture, Key: key, Argv: []string{"printf", "hello space"}})
	if err != nil || id != containerID || !created.Load() {
		t.Fatalf("restricted owned command was not created: %s %v", id, err)
	}
	resource.ContainerID = id
	if err := client.Start(ctx, resource); err != nil {
		t.Fatal(err)
	}
	exit, err := client.Wait(ctx, resource)
	if err != nil || exit != 7 {
		t.Fatalf("terminal nonzero result was not preserved: %d %v", exit, err)
	}
	if err := client.Remove(ctx, resource); err != nil || !removed.Load() {
		t.Fatalf("owned command scope removal was not proven: %v", err)
	}
}

func TestImagePreflightRejectsAmbiguousOrWritableSearchPaths(t *testing.T) {
	for _, search := range []string{"relative/bin:/usr/bin", "/usr/bin:", "/workspace/bin:/bin", "/tmp/tools:/bin", "/usr/../tmp/tools:/bin", "/bin\n/other"} {
		t.Run(search, func(t *testing.T) {
			var mutation atomic.Bool
			endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
				if metadata(w, r) {
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/v1.44/images/"+fixtureImage+"/json" {
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": fixtureImage, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{"Env": []string{"PATH=" + search}}})
					return
				}
				mutation.Store(true)
				http.Error(w, "unexpected", 400)
			})
			client, err := New(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.CheckImage(context.Background(), fixtureImage); err == nil {
				t.Fatal("unsafe guest tool lookup passed preflight")
			}
			if mutation.Load() {
				t.Fatal("image preflight mutated the runtime")
			}
		})
	}
}

func TestContainerUserRejectsRootBeforeAnyGrant(t *testing.T) {
	for _, identity := range []struct {
		uid, gid int
		allowed  bool
	}{
		{501, 20, true}, {1000, 1000, true}, {0, 0, false}, {0, 1000, false}, {-1, 1000, false}, {1000, -1, false},
	} {
		user, err := containerUser(identity.uid, identity.gid)
		if identity.allowed {
			if err != nil || user != strconv.Itoa(identity.uid)+":"+strconv.Itoa(identity.gid) {
				t.Fatalf("non-root identity lost: %q %v", user, err)
			}
		} else if err == nil || user != "" {
			t.Fatalf("invalid/root identity granted: %q %v", user, err)
		}
	}
}

func TestRootIdentityNeverReachesContainerCreation(t *testing.T) {
	for _, uid := range []int{0, 501} {
		t.Run(strconv.Itoa(uid), func(t *testing.T) {
			var creates, starts atomic.Int32
			endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
				if metadata(w, r) {
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1.44/images/"+fixtureImage+"/json":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": fixtureImage, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/create":
					creates.Add(1)
					var request struct{ User string }
					_ = json.NewDecoder(r.Body).Decode(&request)
					if request.User != strconv.Itoa(uid)+":20" {
						t.Error("host ownership mapping changed")
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": containerID})
				default:
					if strings.HasSuffix(r.URL.Path, "/start") {
						starts.Add(1)
					}
					http.Error(w, "unexpected", 400)
				}
			})
			client, err := New(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			workspace, capture := t.TempDir(), t.TempDir()
			_ = os.Chmod(workspace, 0700)
			_ = os.Chmod(capture, 0700)
			helper := filepath.Join(t.TempDir(), "helper")
			if err := os.WriteFile(helper, []byte("inert fixture"), 0500); err != nil {
				t.Fatal(err)
			}
			resource := ownedResource(endpoint)
			resource.ContainerID = ""
			spec := CommandSpec{Image: fixtureImage, Workspace: workspace, Capture: capture, Helper: helper, Key: make([]byte, 32), Argv: []string{"printf", "literal"}}
			id, err := client.create(context.Background(), resource, spec, uid, 20)
			if uid == 0 {
				if err == nil || id != "" || creates.Load() != 0 || starts.Load() != 0 {
					t.Fatalf("root identity reached execution grant: error=%v creates=%d starts=%d", err, creates.Load(), starts.Load())
				}
			} else if err != nil || id != containerID || creates.Load() != 1 || starts.Load() != 0 {
				t.Fatalf("non-root owner mapping rejected: %v", err)
			}
		})
	}
}
