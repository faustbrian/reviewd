package containerapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const ownerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const resourceID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const containerID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// This is an inert local HTTP peer, not a container/daemon or OS child. No
// command, background descendant, interruption or termination probe is run.
func enginePeer(t *testing.T, serve func(http.ResponseWriter, *http.Request)) string {
	t.Helper()
	// macOS limits Unix socket path length; use a short task-owned directory.
	directory, err := os.MkdirTemp("", "rd-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "engine.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(serve), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "unix://" + path
}
func metadata(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/version":
		_, _ = w.Write([]byte(`{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"linux","Arch":"arm64"}`))
	case "/v1.44/info":
		_, _ = w.Write([]byte(`{"ID":"owned-engine","OSType":"linux","Architecture":"aarch64"}`))
	default:
		return false
	}
	return true
}
func ownedResource(endpoint string) Resource {
	return Resource{Endpoint: endpoint, DaemonID: "owned-engine", OwnerID: ownerID, ResourceID: resourceID, ContainerID: containerID}
}
func ownedContainer(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"Id": containerID, "Name": "/reviewd-" + ownerID + "-" + resourceID, "Config": map[string]any{"Labels": map[string]string{"io.reviewd.owner": ownerID, "io.reviewd.resource": resourceID}}})
}
func TestLocalIdentityAndOwnedObservation(t *testing.T) {
	var requests atomic.Int32
	endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if metadata(w, r) {
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1.44/containers/"+containerID+"/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		ownedContainer(w)
	})
	client, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	identity, err := client.Identity(context.Background())
	if err != nil || identity.DaemonID != "owned-engine" || identity.APIVersion != "1.44" || identity.Architecture != "arm64" {
		t.Fatalf("usable local engine not established: %+v %v", identity, err)
	}
	observation, err := client.Observe(context.Background(), ownedResource(endpoint))
	if err != nil || !observation.Present || observation.ContainerID != containerID {
		t.Fatalf("owned container not recognized: %+v %v", observation, err)
	}
	if requests.Load() < 3 {
		t.Fatal("observation bypassed canonical runtime facts")
	}
}
func TestRejectsRemoteAndAmbiguousEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "tcp://127.0.0.1:2375", "http://localhost:2375", "ssh://host", "unix:relative", "unix://host/tmp/socket", "unix:///tmp/socket?query=x", "unix:///tmp/socket#fragment", "unix:///tmp/a\x00b"} {
		t.Run(endpoint, func(t *testing.T) {
			if client, err := New(endpoint); err == nil {
				_ = client.Close()
				t.Fatal("non-local or ambiguous endpoint accepted")
			}
		})
	}
}
func TestIdentityRejectsMissingOrUnsupportedFacts(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"ApiVersion":"1.43","Os":"linux","Arch":"arm64"}`, `{"ApiVersion":"1.54","MinAPIVersion":"1.45","Os":"linux","Arch":"arm64"}`, `{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"other","Arch":"arm64"}`, `{"ApiVersion":"1.54","MinAPIVersion":"1.44","Os":"linux","Arch":"unknown"}`} {
		t.Run(body, func(t *testing.T) {
			endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/version" {
					_, _ = w.Write([]byte(body))
					return
				}
				metadata(w, r)
			})
			client, err := New(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Identity(context.Background()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid runtime identity admitted: %v", err)
			}
		})
	}
}
func TestKnownAbsenceRequiresSameDaemonAndKnownContainer(t *testing.T) {
	endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
		if metadata(w, r) {
			return
		}
		http.Error(w, "not found", 404)
	})
	client, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	resource := ownedResource(endpoint)
	observation, err := client.Observe(context.Background(), resource)
	if err != nil || observation.Present || observation.ContainerID != containerID {
		t.Fatalf("known absent owned identity unresolved: %+v %v", observation, err)
	}
	resource.ContainerID = ""
	if _, err := client.Observe(context.Background(), resource); !errors.Is(err, ErrUncertain) {
		t.Fatal("absent unbound create intent became known absent")
	}
	resource = ownedResource(endpoint)
	resource.DaemonID = "replacement-engine"
	if _, err := client.Observe(context.Background(), resource); !errors.Is(err, ErrUncertain) {
		t.Fatal("replacement daemon certified old resource absence")
	}
}
func TestObservationRejectsUnownedOrMalformedContainer(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"Id":"` + containerID + `","Name":"/other","Config":{"Labels":{}}}`, `{"Id":"` + containerID + `","Name":"/reviewd-` + ownerID + `-` + resourceID + `","Config":{"Labels":{"io.reviewd.owner":"other","io.reviewd.resource":"` + resourceID + `"}}}`} {
		t.Run(body, func(t *testing.T) {
			endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
				if metadata(w, r) {
					return
				}
				_, _ = w.Write([]byte(body))
			})
			client, err := New(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Observe(context.Background(), ownedResource(endpoint)); !errors.Is(err, ErrUncertain) {
				t.Fatal("unproven container ownership accepted")
			}
		})
	}
}
func TestControlPlaneIsBoundedAndNeverFollowsRedirects(t *testing.T) {
	for _, mode := range []string{"oversized", "redirect", "untrusted-error", "truncated", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			var hits, redirected atomic.Int32
			redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusOK) }))
			defer redirectTarget.Close()
			entered := make(chan struct{}, 1)
			endpoint := enginePeer(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				switch mode {
				case "oversized":
					_, _ = w.Write([]byte(strings.Repeat(" ", (1<<20)+1)))
				case "redirect":
					http.Redirect(w, r, redirectTarget.URL+"/should-not-be-contacted", http.StatusFound)
				case "untrusted-error":
					http.Error(w, "private-runtime-value", 500)
				case "truncated":
					_, _ = w.Write([]byte(`{"ApiVersion":`))
				case "cancelled":
					entered <- struct{}{}
					<-r.Context().Done()
				}
			})
			client, err := New(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "cancelled" {
				returned := make(chan error, 1)
				go func() { _, callErr := client.Identity(ctx); returned <- callErr }()
				select {
				case <-entered:
					cancel()
				case <-ctx.Done():
					t.Fatal("inert peer never received request")
				}
				err = <-returned
			} else {
				_, err = client.Identity(ctx)
			}
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private-runtime-value") {
				t.Fatalf("unbounded/untrusted identity became usable: %v", err)
			}
			if redirected.Load() != 0 {
				t.Fatal("redirect leaked control-plane request to another endpoint")
			}
			if hits.Load() != 1 {
				t.Fatalf("unexpected redirect/retry: %d requests", hits.Load())
			}
		})
	}
}
