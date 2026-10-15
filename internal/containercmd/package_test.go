package containercmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func packageSum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func TestInstalledPackageHelperIsBoundToNativeBinaryAndGuest(t *testing.T) {
	for _, mode := range []string{"valid", "missing-helper", "changed-helper", "changed-cli", "wrong-guest", "wrong-protocol", "traversal", "missing-manifest"} {
		t.Run(mode, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "paired install with spaces")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(directory, "reviewd")
			helper := filepath.Join(directory, "reviewd-command-host")
			cliData, helperData := []byte("owned native executable"), []byte("owned guest executable")
			if err := os.WriteFile(executable, cliData, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(helper, helperData, 0755); err != nil {
				t.Fatal(err)
			}
			selection := map[string]any{"file": "reviewd-command-host", "target": "linux/arm64", "protocol": 1, "sha256": packageSum(helperData)}
			switch mode {
			case "missing-helper":
				_ = os.Remove(helper)
			case "changed-helper":
				_ = os.WriteFile(helper, []byte("different helper"), 0755)
			case "changed-cli":
				_ = os.WriteFile(executable, []byte("different native binary"), 0755)
			case "wrong-guest":
				selection["target"] = "darwin/arm64"
			case "wrong-protocol":
				selection["protocol"] = 2
			case "traversal":
				selection["file"] = "../reviewd-command-host"
			}
			metadata := map[string]any{"schema_version": 1, "repository": "github.com/faustbrian/reviewd", "version": "owned-build", "binary_sha256": packageSum(cliData), "container_helper": selection}
			data, _ := json.Marshal(metadata)
			if err := os.WriteFile(filepath.Join(directory, "BUILD.json"), data, 0644); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-manifest" {
				_ = os.Remove(filepath.Join(directory, "BUILD.json"))
			}
			actual, err := FromPackage(executable, "arm64")
			if mode != "valid" {
				if err == nil {
					t.Fatal("incompatible or unpaired helper selected")
				}
				return
			}
			canonicalHelper, canonicalErr := filepath.EvalSymlinks(helper)
			if err != nil || canonicalErr != nil || actual.Path != canonicalHelper || actual.SHA256 != packageSum(helperData) {
				t.Fatal("paired installed helper not resolved", err)
			}
		})
	}
}
