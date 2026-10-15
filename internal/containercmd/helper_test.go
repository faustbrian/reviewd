package containercmd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func buildFixtureHelper(t *testing.T, architecture string) (string, string) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "reviewd-command-host")
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-mod=readonly", "-o", output, "./cmd/reviewd-command-host")
	command.Dir = filepath.Join("..", "..")
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+architecture, "CGO_ENABLED=0", "GOWORK=off")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("owned helper build: %v %s", err, raw)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return output, hex.EncodeToString(sum[:])
}
