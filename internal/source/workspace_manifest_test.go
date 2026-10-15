package source

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MAC-07: reject unsupported trees before any repository-controlled pathname
// reaches Git checkout, regardless of what the text-review projection omitted.
func TestFeedbackWorkspaceRejectsUnsupportedManifestBeforeExport(t *testing.T) {
	for _, entry := range []Entry{
		{Path: "../outside", Mode: "100644", Type: "blob", OID: strings.Repeat("b", 40)},
		{Path: ".GiT/config", Mode: "100644", Type: "blob", OID: strings.Repeat("b", 40)},
		{Path: "submodule", Mode: "160000", Type: "commit", OID: strings.Repeat("b", 40)},
		{Path: "huge", Mode: "100644", Type: "blob", OID: strings.Repeat("b", 40), Size: 1<<30 + 1},
	} {
		t.Run(entry.Path, func(t *testing.T) {
			bare := t.TempDir()
			if err := os.Mkdir(filepath.Join(bare, "objects"), 0700); err != nil {
				t.Fatal(err)
			}
			snapshot := &Snapshot{root: bare, HeadSHA: strings.Repeat("a", 40), Manifests: map[string][]Entry{"head": {entry}}}
			parent := t.TempDir()
			workspace, err := snapshot.PrepareFeedbackWorkspace(context.Background(), parent, "feature/existing-pr")
			if err == nil {
				workspace.Close()
				t.Fatal("unsupported complete tree was exported for checkout")
			}
			files, err := os.ReadDir(parent)
			if err != nil || len(files) != 0 {
				t.Fatal("rejected tree left workspace files", err)
			}
		})
	}
}

func TestFeedbackWorkspaceValidatesCompleteMaterializedTree(t *testing.T) {
	for _, algorithm := range []string{"sha1", "sha256"} {
		for _, mutation := range []string{"exact", "same-size bytes", "mode", "symlink", "missing", "extra", "directory alias"} {
			t.Run(algorithm+"/"+mutation, func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal("owned fixture setup", err)
					}
				}
				root := t.TempDir()
				must(os.Mkdir(filepath.Join(root, ".git"), 0700))
				must(os.Mkdir(filepath.Join(root, "nested"), 0700))
				data := []byte{0, 255, 13, 10, 128, 0}
				must(os.WriteFile(filepath.Join(root, "nested", "binary"), data, 0600))
				must(os.WriteFile(filepath.Join(root, "executable"), []byte("fixture\n"), 0700))
				must(os.Symlink("../outside-secret", filepath.Join(root, "link")))
				oid := func(data []byte) string {
					encoded := append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)
					if algorithm == "sha256" {
						h := sha256.Sum256(encoded)
						return hex.EncodeToString(h[:])
					}
					h := sha1.Sum(encoded)
					return hex.EncodeToString(h[:])
				}
				snapshot := &Snapshot{HeadSHA: strings.Repeat("a", len(oid(nil))), Manifests: map[string][]Entry{"head": {
					{Path: "nested/binary", Mode: "100644", Type: "blob", OID: oid(data), Size: int64(len(data))},
					{Path: "executable", Mode: "100755", Type: "blob", OID: oid([]byte("fixture\n")), Size: 8},
					{Path: "link", Mode: "120000", Type: "blob", OID: oid([]byte("../outside-secret")), Size: 17},
				}}}
				if err := snapshot.ValidateFeedbackWorkspace(context.Background(), root); err != nil {
					t.Fatal("exact binary/mode/symlink tree rejected", err)
				}
				switch mutation {
				case "exact":
					return
				case "same-size bytes":
					must(os.WriteFile(filepath.Join(root, "nested", "binary"), []byte{1, 255, 13, 10, 128, 0}, 0600))
				case "mode":
					must(os.Chmod(filepath.Join(root, "executable"), 0600))
				case "symlink":
					must(os.Remove(filepath.Join(root, "link")))
					must(os.Symlink("../different-link", filepath.Join(root, "link")))
				case "missing":
					must(os.Remove(filepath.Join(root, "nested", "binary")))
				case "extra":
					must(os.WriteFile(filepath.Join(root, "unexpected"), []byte("undeclared"), 0600))
				case "directory alias":
					must(os.Remove(filepath.Join(root, "nested", "binary")))
					must(os.Remove(filepath.Join(root, "nested")))
					must(os.Symlink(t.TempDir(), filepath.Join(root, "nested")))
				}
				if err := snapshot.ValidateFeedbackWorkspace(context.Background(), root); err == nil {
					t.Fatal("changed/incomplete checkout certified")
				}
			})
		}
	}
}

func TestFeedbackWorkspacePreservesDistinctNamesOrRejectsFilesystemAliases(t *testing.T) {
	for _, names := range [][2]string{{"File", "file"}, {"café", "cafe\u0301"}} {
		t.Run(names[1], func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			entries := []Entry{}
			for i, name := range names {
				data := []byte(fmt.Sprintf("independent source %d", i))
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
				h := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...))
				entries = append(entries, Entry{Path: name, Mode: "100644", Type: "blob", OID: hex.EncodeToString(h[:]), Size: int64(len(data))})
			}
			a, err := os.Stat(filepath.Join(root, names[0]))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(filepath.Join(root, names[1]))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := &Snapshot{HeadSHA: strings.Repeat("a", 40), Manifests: map[string][]Entry{"head": entries}}
			err = snapshot.ValidateFeedbackWorkspace(context.Background(), root)
			if os.SameFile(a, b) {
				if err == nil {
					t.Fatal("case/Unicode alias silently certified as distinct complete source")
				}
			} else if err != nil {
				t.Fatal("distinct exact entries rejected", err)
			}
		})
	}
}
