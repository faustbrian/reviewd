package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"github.com/faustbrian/reviewd/internal/review"
	"github.com/faustbrian/reviewd/internal/source"
	"github.com/faustbrian/reviewd/internal/store"
)

// The privileged source boundary reports uncertain shutdown with an actual
// live, registered child and private input. The caller must keep its ownership
// discoverable even though acquisition returns no Snapshot cleanup handle.
type uncertainPublicationSource struct {
	t     *testing.T
	root  string
	child *exec.Cmd
	stop  func()
}

func newUncertainPublicationSource(t *testing.T) *uncertainPublicationSource {
	return &uncertainPublicationSource{t: t}
}

func (s *uncertainPublicationSource) acquire(ctx context.Context, _ github.Pull) (*source.Snapshot, error) {
	s.t.Helper()
	if s.child != nil {
		s.t.Fatal("uncertain acquisition was repeated")
	}
	var err error
	s.root, err = os.MkdirTemp(lifetime.Root(ctx, s.t.TempDir()), "retained-source-")
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, "private-input"), []byte("source recovery canary"), 0600); err != nil {
		s.t.Fatal(err)
	}
	s.child = exec.Command("/bin/sleep", "60")
	s.child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if _, err := lifetime.Start(ctx, s.child); err != nil {
		s.t.Fatal(err)
	}
	stopped := false
	s.stop = func() {
		if !stopped {
			stopped = true
			_ = s.child.Process.Kill() // Only this unreaped fixture-owned child.
			_ = s.child.Wait()
		}
	}
	s.t.Cleanup(s.stop)
	return nil, lifetime.ErrCleanupUncertain
}

func (s *uncertainPublicationSource) assertRecovered(t *testing.T, original *store.Store, directory string, before store.Run) {
	t.Helper()
	if s.child == nil {
		t.Fatal("publication did not reach its source acquisition boundary")
	}
	assertRetained := func() {
		t.Helper()
		if err := syscall.Kill(s.child.Process.Pid, 0); err != nil {
			t.Fatalf("recovery interfered with the live owned child: %v", err)
		}
		if _, err := os.Stat(filepath.Join(s.root, "private-input")); err != nil {
			t.Fatalf("uncertain source input was removed: %v", err)
		}
	}
	assertRun := func() {
		t.Helper()
		after, err := original.Get(context.Background(), before.ID)
		if err != nil || after.State != before.State || after.PayloadID != before.PayloadID || !bytes.Equal(after.Prepared, before.Prepared) || after.OwnerID != before.OwnerID || after.OwnerClosed != before.OwnerClosed || after.ThreadID != before.ThreadID || after.TurnID != before.TurnID {
			t.Fatalf("source recovery changed prepared/run identity: before=%+v after=%+v err=%v", before, after, err)
		}
		var previous, current review.PublicationOutcome
		_ = json.Unmarshal(before.Outcome, &previous)
		_ = json.Unmarshal(after.Outcome, &current)
		previous.Failure, current.Failure = "", ""
		a, _ := json.Marshal(previous)
		b, _ := json.Marshal(current)
		if !bytes.Equal(a, b) {
			t.Fatalf("source recovery changed publication facts: before=%s after=%s", a, b)
		}
	}
	assertRetained()
	assertRun()
	// A new client must discover this allocation without a returned Snapshot.
	reopened, err := store.Open(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.RecoverPrivate(context.Background(), false)
	if err != nil || len(report.Owners) != 1 || report.Owners[0].State != "resources_pending" || len(report.Owners[0].RunIDs) != 0 {
		t.Fatalf("uncertain publication source is not durably recoverable: %+v %v", report, err)
	}
	assertRetained()
	assertRun()
	s.stop()
	report, err = reopened.RecoverPrivate(context.Background(), false)
	if err != nil || len(report.Owners) != 1 || report.Owners[0].State != "recovered" {
		t.Fatalf("stopped publication source was not recovered: %+v %v", report, err)
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatalf("recovery left private source storage: %v", err)
	}
	assertRun()
	report, err = reopened.RecoverPrivate(context.Background(), false)
	if err != nil || len(report.Owners) != 0 {
		t.Fatalf("repeated recovery was not settled: %+v %v", report, err)
	}
	assertRun()
}
