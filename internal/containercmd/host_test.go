package containercmd

import (
	"context"
	"database/sql"

	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/faustbrian/reviewd/internal/commandcapture"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faustbrian/reviewd/internal/codex"
	"github.com/faustbrian/reviewd/internal/containerapi"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/store"
)

const testImage = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
const testContainer = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// An inert Unix HTTP peer and real reopened SQLite exercise the application
// lifecycle. No container, daemon, model or operating-system child is used.
func TestOwnedCommandScopeRequiresTerminalOutputAndCanonicalRemoval(t *testing.T) {
	helper, helperHash := buildFixtureHelper(t, "arm64")
	for _, mode := range []string{"complete", "complete-empty", "exit-mismatch", "capture-missing", "capture-malformed", "capture-tampered", "capture-stale", "unknown-create", "start-ack-lost", "unknown-terminal", "remove-ack-only", "cancelled-before-dispatch", "cancelled-active", "close-active", "missing-image"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			directory := filepath.Join(t.TempDir(), "state")
			durable, err := store.Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			owner, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			workspace := filepath.Join(owner.Root, "checkout")
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(workspace, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(workspace, "source")
			if err := os.WriteFile(canary, []byte("retained source"), 0600); err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "reviewd.sqlite")+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			socketDirectory, err := os.MkdirTemp("", "rd-cmd-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(socketDirectory)
			socket := filepath.Join(socketDirectory, "engine.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var created, removed atomic.Bool
			var captureDirectory string
			var captureKey []byte
			var captureRequest commandcapture.Request
			var mutations atomic.Int32
			var mu sync.Mutex
			var name string
			var labels map[string]string
			started := make(chan struct{})
			waiting := make(chan struct{}, 1)
			var start, captureOnce sync.Once
			t.Cleanup(func() { start.Do(func() { close(started) }) })
			assertIntent := func(bound bool) {
				var raw string
				if err := database.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", owner.ID).Scan(&raw); err != nil {
					t.Error("durable resource missing before runtime mutation", err)
					return
				}
				var resources map[string]json.RawMessage
				if json.Unmarshal([]byte(raw), &resources) != nil {
					t.Error("invalid durable inventory")
					return
				}
				pending := false
				container := false
				for _, v := range resources {
					if string(v) == "0" {
						pending = true
					}
					var entry struct {
						Kind     string
						Resource struct {
							ContainerID string `json:"container_id"`
						}
					}
					_ = json.Unmarshal(v, &entry)
					if entry.Kind == "docker" && ((!bound && entry.Resource.ContainerID == "") || (bound && entry.Resource.ContainerID == testContainer)) {
						container = true
					}
				}
				if !pending || !container {
					t.Error("runtime mutation preceded durable command/container identity", raw)
				}
			}
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/version":
					_, _ = w.Write([]byte(`{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"linux","Arch":"arm64"}`))
				case r.Method == http.MethodGet && r.URL.Path == "/v1.44/info":
					_, _ = w.Write([]byte(`{"ID":"fixture-engine","OSType":"linux","Architecture":"arm64"}`))
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.44/images/"):
					if mode == "missing-image" {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": testImage, "Os": "linux", "Architecture": "arm64", "Config": map[string]any{"Volumes": map[string]any{}}})
				case r.Method == http.MethodPost && r.URL.Path == "/v1.44/containers/create":
					mutations.Add(1)
					assertIntent(false)
					var body struct {
						Labels     map[string]string
						Env, Cmd   []string
						HostConfig struct {
							Mounts []struct{ Source, Target string }
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					mu.Lock()
					name = r.URL.Query().Get("name")
					labels = body.Labels
					for _, entry := range body.Env {
						if strings.HasPrefix(entry, commandcapture.KeyEnvironment+"=") {
							captureKey, _ = hex.DecodeString(strings.TrimPrefix(entry, commandcapture.KeyEnvironment+"="))
						}
					}
					for _, mount := range body.HostConfig.Mounts {
						if mount.Target == commandcapture.GuestDirectory {
							captureDirectory = mount.Source
						}
					}
					if len(body.Cmd) < 4 {
						t.Error("helper invocation missing")
					} else {
						captureRequest = commandcapture.Request{Nonce: strings.TrimPrefix(body.Cmd[2], "--nonce="), Argv: body.Cmd[4:]}
					}
					mu.Unlock()
					created.Store(true)
					w.WriteHeader(http.StatusCreated)
					if mode == "unknown-create" {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": testContainer, "Warnings": []string{}})
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					if !created.Load() || removed.Load() {
						http.NotFound(w, r)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": testContainer, "Name": "/" + name, "Config": map[string]any{"Labels": labels}})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					mutations.Add(1)
					assertIntent(true)

					start.Do(func() { close(started) })
					if mode == "start-ack-lost" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait"):
					if mode == "cancelled-active" || mode == "close-active" {
						waiting <- struct{}{}
						<-r.Context().Done()
						return
					}
					if mode == "unknown-terminal" {
						_, _ = w.Write([]byte(`{}`))
					} else if mode == "complete-empty" {
						_, _ = w.Write([]byte(`{"StatusCode":0}`))
					} else {
						_, _ = w.Write([]byte(`{"StatusCode":7}`))
					}
				case r.Method == http.MethodDelete:
					mutations.Add(1)
					// Complete bytes become available only at removal, not at Wait.
					// A provider reading before cleanup cannot pass this positive case.
					captureOnce.Do(func() {
						if captureDirectory != "" && mode != "capture-missing" {
							request := captureRequest
							if mode == "capture-stale" {
								request.Nonce = strings.Repeat("b", 32)
							}
							session, err := commandcapture.New(captureDirectory, captureKey, request)
							if err != nil {
								t.Error("capture fixture", err)
							} else {
								if mode != "complete-empty" {
									_, _ = session.Stdout.Write([]byte("owned output\n"))
									_, _ = session.Stderr.Write([]byte("owned error\n"))
								}
								exit := 7
								if mode == "complete-empty" || mode == "exit-mismatch" {
									exit = 0
								}
								if err := session.Finish(exit, true); err != nil {
									t.Error(err)
								}
								if mode == "capture-malformed" {
									_ = os.WriteFile(filepath.Join(captureDirectory, "result.json"), []byte(`{"exit_code":0}`), 0600)
								}
								if mode == "capture-tampered" {
									_ = os.WriteFile(filepath.Join(captureDirectory, "stdout"), []byte("forged output"), 0600)
								}
							}
						}
					})
					if mode != "remove-ack-only" {
						removed.Store(true)
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected inert runtime request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
				}
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			host, err := New(ctx, owner, Options{Endpoint: "unix://" + socket, Image: testImage, Workspace: workspace, Writable: true, Helper: helper, HelperSHA256: helperHash})
			if mode == "missing-image" {
				if !errors.Is(err, containerapi.ErrUnavailable) || mutations.Load() != 0 {
					t.Fatalf("missing image admitted commands or lost prerequisite identity: %v %d", err, mutations.Load())
				}
				if err := owner.Close(); err != nil {
					t.Fatal("unavailable prerequisite left unresolved resources", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("container command host unavailable: %v", err)
			}
			defer host.Close()
			operation, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if mode == "cancelled-before-dispatch" {
				cancel()
			}
			var result codex.CommandResult
			var runErr error
			if mode == "close-active" {
				type outcome struct {
					result codex.CommandResult
					err    error
				}
				returned := make(chan outcome, 1)
				go func() {
					result, err := host.Run(operation, []string{"printf", "owned output\n"}, 2*time.Second)
					returned <- outcome{result, err}
				}()
				select {
				case <-waiting:
				case <-operation.Done():
					t.Fatal("command did not reach inert wait barrier")
				}
				if err := host.Close(); err == nil {
					t.Fatal("active unknown cancellation became successful quiescence")
				}
				observed := <-returned
				result = observed.result
				runErr = observed.err
			} else {
				result, runErr = host.Run(operation, []string{"printf", "owned output\n"}, time.Second)
			}
			if mode == "cancelled-before-dispatch" {
				if !errors.Is(runErr, context.Canceled) || mutations.Load() != 0 {
					t.Fatalf("pre-dispatch cancellation performed work: %v, mutations %d", runErr, mutations.Load())
				}
				if err := host.Close(); err != nil {
					t.Fatal(err)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if mode == "complete" || mode == "complete-empty" {
				expectedExit, expectedOutput, expectedError := 7, "owned output\n", "owned error\n"
				if mode == "complete-empty" {
					expectedExit, expectedOutput, expectedError = 0, "", ""
				}
				if runErr != nil || result.ExitCode != expectedExit || result.Stdout != expectedOutput || result.Stderr != expectedError || !removed.Load() {
					t.Fatalf("complete command facts lost: %+v %v", result, runErr)
				}
				environment := host.Environment()
				if environment.OS != "linux" || environment.Architecture != "arm64" || environment.Image != testImage || environment.Workspace != "/workspace" {
					t.Fatalf("guest environment was not recorded: %+v", environment)
				}
				if err := host.Close(); err != nil {
					t.Fatal(err)
				}
				if err := owner.Close(); err != nil {
					t.Fatalf("canonical completion retained owner: %v", err)
				}
				if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
					t.Fatal("completed owner retained storage")
				}
				return
			}
			if !errors.Is(runErr, lifetime.ErrCleanupUncertain) {
				t.Fatalf("unknown terminal/removal became completed: %+v %v", result, runErr)
			}
			beforeRetry := mutations.Load()
			if _, err := host.Run(ctx, []string{"printf", "must not run"}, time.Second); !errors.Is(err, lifetime.ErrCleanupUncertain) || mutations.Load() != beforeRetry {
				t.Fatal("uncertain command admitted subsequent execution", err)
			}
			if err := host.Close(); err == nil {
				t.Fatal("unknown execution allowed capture/quiescence")
			}
			if err := owner.Close(); err == nil {
				t.Fatal("unknown execution allowed owner deletion")
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "retained source" {
				t.Fatal("unknown execution lost source", err)
			}
			if err := durable.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			report, err := reopened.RecoverPrivate(ctx, false)
			if err != nil || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
				t.Fatalf("recovery forgot uncertain command: %+v %v", report, err)
			}
		})
	}
}
