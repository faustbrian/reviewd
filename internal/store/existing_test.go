package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingNeverManufacturesLostOwnershipEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	missing := filepath.Join(root, "missing-profile")
	if s, err := OpenExisting(ctx, missing); err == nil {
		s.Close()
		t.Fatal("missing profile was manufactured")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing profile directory was created", err)
	}
	empty := filepath.Join(root, "empty-profile")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenExisting(ctx, empty); err == nil {
		s.Close()
		t.Fatal("missing database was manufactured")
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatal("failed existing open wrote profile artifacts", err)
	}
	s, owner, run := privateOwnerRun(t)
	defer owner.Close()
	reopened, err := OpenExisting(ctx, s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	actual, err := reopened.Get(ctx, run.ID)
	if err != nil || actual.OwnerID != owner.ID || actual.WorkKey != run.WorkKey {
		t.Fatal("existing authoritative run was not reopened", err)
	}
}

func TestReconcileOnePrivateOwnerRequiresAuthoritativeClosure(t *testing.T) {
	ctx := context.Background()
	s, owner, run := privateOwnerRun(t)
	live, err := s.ReconcilePrivateOwner(ctx, owner.ID)
	if err != nil || live.State != "live" {
		t.Fatalf("live owner lost: %+v %v", live, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	settled, err := s.ReconcilePrivateOwner(ctx, owner.ID)
	if err != nil || settled.State != "already_recovered" {
		t.Fatalf("settled owner not reconciled: %+v %v", settled, err)
	}
	actual, err := s.Get(ctx, run.ID)
	if err != nil || actual.State != Failed || !actual.OwnerClosed {
		t.Fatal("lost owner was not terminalized with proven closure", err)
	}
	unknown, err := s.ReconcilePrivateOwner(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || unknown.State != "unknown" {
		t.Fatalf("absent metadata became shutdown proof: %+v %v", unknown, err)
	}
	missing, liveOwner, liveRun := privateOwnerRun(t)
	defer liveOwner.lease.Close() // No child was launched by this corruption fixture.
	if _, err := missing.db.ExecContext(ctx, "DELETE FROM private_owners WHERE id=?", liveOwner.ID); err != nil {
		t.Fatal(err)
	}
	unproven, err := missing.ReconcilePrivateOwner(ctx, liveOwner.ID)
	if err != nil || unproven.State != "unknown" {
		t.Fatalf("missing active owner record became closure proof: %+v %v", unproven, err)
	}
	untouched, err := missing.Get(ctx, liveRun.ID)
	if err != nil || untouched.State != Reviewing || untouched.OwnerClosed {
		t.Fatal("unproven work was interrupted", err)
	}
	if _, err := os.Stat(liveOwner.Root); err != nil {
		t.Fatal("unproven private storage was removed", err)
	}
}
