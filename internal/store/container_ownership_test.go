package store

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The real ownership/recovery boundary talks only to an inert local HTTP peer.
// No container, daemon, OS child, or termination probe is involved.
func TestContainerOwnershipRequiresCanonicalKnownAbsence(t *testing.T) {
	for _, mode := range []string{"known-absent", "present", "unknown-create", "changed-daemon", "changed-after-inspect", "unavailable-after-inspect"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			directory := filepath.Join(t.TempDir(), "state")
			durable, err := Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			owner, err := durable.BeginPrivateOwner(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			canary := filepath.Join(owner.Root, "retained-source")
			if err := os.WriteFile(canary, []byte("immutable fixture source"), 0600); err != nil {
				t.Fatal(err)
			}
			socketDir, err := os.MkdirTemp("", "rd-owner-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(socketDir)
			socket := filepath.Join(socketDir, "engine.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("b", 32)
			cid := strings.Repeat("c", 64)
			var inspected atomic.Bool
			server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/version":
					_, _ = w.Write([]byte(`{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"linux","Arch":"arm64"}`))
				case "/v1.44/info":
					if mode == "unavailable-after-inspect" && inspected.Load() {
						http.Error(w, "runtime unavailable", http.StatusServiceUnavailable)
						return
					}
					engine := "owned-engine"
					if mode == "changed-daemon" || (mode == "changed-after-inspect" && inspected.Load()) {
						engine = "replacement-engine"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ID": engine, "OSType": "linux", "Architecture": "aarch64"})
				default:
					inspected.Store(true)
					if mode != "present" {
						http.Error(w, "not found", 404)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": cid, "Name": "/reviewd-" + owner.ID + "-" + id, "Config": map[string]any{"Labels": map[string]string{"io.reviewd.owner": owner.ID, "io.reviewd.resource": id}}})
				}
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			if mode == "unknown-create" {
				cid = ""
			}
			descriptor := map[string]any{"kind": "docker", "resource": map[string]any{"endpoint": "unix://" + socket, "daemon_id": "owned-engine", "owner_id": owner.ID, "resource_id": id, "container_id": cid}}
			raw, err := json.Marshal(map[string]any{id: descriptor})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := durable.db.ExecContext(ctx, "UPDATE private_owners SET processes=? WHERE id=?", string(raw), owner.ID); err != nil {
				t.Fatal(err)
			}
			closeErr := owner.Close()
			if mode == "known-absent" {
				if closeErr != nil {
					t.Fatalf("canonical known container absence did not release owned storage: %v", closeErr)
				}
				if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
					t.Fatal("known absent container retained owner storage")
				}
				return
			}
			if closeErr == nil {
				t.Fatal("uncertain/live container owner was deleted")
			}
			if _, err := os.Stat(owner.Root); err != nil {
				t.Fatal("unproven container storage lost")
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "immutable fixture source" {
				t.Fatal("uncertain container cleanup changed retained source", err)
			}
			var retained string
			if err := durable.db.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", owner.ID).Scan(&retained); err != nil || retained != string(raw) {
				t.Fatal("uncertain container cleanup changed durable resource identity", err)
			}
			if err := durable.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			// Exercise the same mid-observation loss through recovery, not merely
			// an initial mismatch left behind by the earlier Close observation.
			inspected.Store(false)
			report, err := reopened.RecoverPrivate(ctx, false)
			if err != nil || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" {
				t.Fatalf("container ownership uncertainty lost on recovery: %+v %v", report, err)
			}
			if (mode == "changed-after-inspect" || mode == "unavailable-after-inspect") && !inspected.Load() {
				t.Fatal("recovery never reached inspection after the matching precheck")
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "immutable fixture source" {
				t.Fatal("uncertain recovery changed retained source", err)
			}
			if err := reopened.db.QueryRowContext(ctx, "SELECT processes FROM private_owners WHERE id=?", owner.ID).Scan(&retained); err != nil || retained != string(raw) {
				t.Fatal("uncertain recovery changed durable resource identity", err)
			}
		})
	}
}

func TestContainerRegistrationCannotReleaseBeforeBoundCanonicalAbsence(t *testing.T) {
	ctx := context.Background()
	durable, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	resource, err := owner.StartingContainer(ctx, "unix://"+filepath.Join(t.TempDir(), "not-created.sock"), "owned-engine")
	if err != nil {
		t.Fatalf("container intent was not durably reserved: %v", err)
	}
	reference, err := resource.Resource(ctx)
	if err != nil || reference.OwnerID != owner.ID || reference.ContainerID != "" || reference.ResourceID == "" {
		t.Fatalf("container intent lacks owned durable identity: %+v %v", reference, err)
	}
	// No request was started, but an unresolved intent is deliberately retained
	// until the application acknowledges its actual create/start outcome.
	if err := owner.Close(); err == nil {
		t.Fatal("unbound resource intent allowed owner deletion")
	}
	if _, err := os.Stat(owner.Root); err != nil {
		t.Fatal("unbound container intent lost storage")
	}
	if err := resource.Bind(ctx, strings.Repeat("x", 64)); err == nil {
		t.Fatal("invalid container identity was bound")
	}
	if err := resource.Finished(ctx); err == nil {
		t.Fatal("unacknowledged launch intent was cleared")
	}
}

func TestContainerBindingAndCompletionUseCanonicalOwnedResource(t *testing.T) {
	ctx := context.Background()
	durable, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	owner, err := durable.BeginPrivateOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	directory, err := os.MkdirTemp("", "rd-bind-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "engine.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var absent, wrongOwner atomic.Bool
	wrongOwner.Store(true)
	var resourceID atomic.Value
	resourceID.Store("")
	cid := strings.Repeat("c", 64)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("ownership observation mutated runtime")
			http.Error(w, "unexpected", 400)
			return
		}
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"linux","Arch":"arm64"}`))
		case "/v1.44/info":
			_, _ = w.Write([]byte(`{"ID":"owned-engine","OSType":"linux","Architecture":"aarch64"}`))
		default:
			if absent.Load() {
				http.Error(w, "not found", 404)
				return
			}
			ownerLabel := owner.ID
			if wrongOwner.Load() {
				ownerLabel = "unrelated-owner"
			}
			id := resourceID.Load().(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": cid, "Name": "/reviewd-" + owner.ID + "-" + id, "Config": map[string]any{"Labels": map[string]string{"io.reviewd.owner": ownerLabel, "io.reviewd.resource": id}}})
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	pending, err := owner.StartingContainer(ctx, "unix://"+socket, "owned-engine")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := pending.Resource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resourceID.Store(reference.ResourceID)
	if err := pending.Bind(ctx, cid); err == nil {
		t.Fatal("unrelated resource identity was bound")
	}
	wrongOwner.Store(false)
	if err := pending.Bind(ctx, cid); err != nil {
		t.Fatalf("canonical created container identity was not durably bound: %v", err)
	}
	reference, err = pending.Resource(ctx)
	if err != nil || reference.ContainerID != cid {
		t.Fatal("canonical binding not persisted", err)
	}
	if err := pending.Bind(ctx, strings.Repeat("d", 64)); err == nil {
		t.Fatal("created resource rebound to another identity")
	}
	if err := pending.Finished(ctx); err == nil {
		t.Fatal("present container was acknowledged as removed")
	}
	absent.Store(true)
	if err := pending.Finished(ctx); err != nil {
		t.Fatalf("canonical absence did not acknowledge container cleanup: %v", err)
	}
	if err := pending.Finished(ctx); err != nil {
		t.Fatalf("acknowledged cleanup was not idempotent: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("completed container retained owner storage: %v", err)
	}
	if _, err := os.Stat(owner.Root); !os.IsNotExist(err) {
		t.Fatal("completed container retained owned storage")
	}
}
