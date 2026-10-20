//go:build darwin || linux

package lease

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObserveExistingLeaseDoesNotCreateOrRetainOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot.lock")
	if busy, err := Busy(path); err != nil || busy {
		t.Fatalf("missing lease: %v %v", busy, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("observer created a lock")
	}
	held, err := Acquire(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := Busy(path); err != nil || !busy {
		t.Fatalf("live lease: %v %v", busy, err)
	}
	held.Close()
	if busy, err := Busy(path); err != nil || busy {
		t.Fatalf("released lease: %v %v", busy, err)
	}
	next, err := Acquire(path, false)
	if err != nil {
		t.Fatal("observer retained ownership", err)
	}
	next.Close()
	alias := path + ".symlink"
	os.Symlink(path, alias)
	if _, err := Busy(alias); err == nil {
		t.Fatal("followed symlink for lease observation")
	}
}
