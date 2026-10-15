package containerapi

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

// This opt-in check reads local engine metadata only. It does not create,
// start, stop, delete, or interrupt any container or operating-system child.
func TestLocalEngineIdentity(t *testing.T) {
	if os.Getenv("REVIEWD_CONTAINER_NATIVE") != "1" {
		t.Skip("explicit local engine metadata acceptance is not enabled")
	}
	endpoint := os.Getenv("REVIEWD_CONTAINER_ENDPOINT")
	if endpoint == "" {
		t.Fatal("explicit local Unix endpoint is required")
	}
	client, err := New(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity, err := client.Identity(ctx)
	if err != nil {
		t.Fatalf("local engine metadata acceptance failed: %v", err)
	}
	if identity.DaemonID == "" || identity.Architecture != runtime.GOARCH {
		t.Fatal("local engine did not establish expected architecture and stable identity")
	}
	t.Logf("Local engine metadata: API=%s architecture=%s; no container operation performed", identity.APIVersion, identity.Architecture)
}
