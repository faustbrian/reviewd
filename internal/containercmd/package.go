package containercmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/faustbrian/reviewd/internal/commandcapture"
)

type HelperSelection struct{ Path, SHA256 string }

func packageBytes(name string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 0 || info.Size() > limit {
		return nil, errors.New("unsupported installed package file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, errors.New("incomplete installed package file")
	}
	return data, nil
}
func packageDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// FromPackage resolves only the helper paired with the running native binary.
// Missing companion files hold container commands, not ordinary native reviews.
// New additionally validates the executable's Linux ABI and freezes its bytes.
func FromPackage(nativeExecutable, architecture string) (HelperSelection, error) {
	unavailable := errors.New("paired container helper is unavailable; install reviewd, reviewd-command-host and BUILD.json from the same package")
	if !filepath.IsAbs(nativeExecutable) || (architecture != "arm64" && architecture != "amd64") {
		return HelperSelection{}, unavailable
	}
	executable, err := filepath.EvalSymlinks(nativeExecutable)
	if err != nil {
		return HelperSelection{}, unavailable
	}
	raw, err := packageBytes(filepath.Join(filepath.Dir(executable), "BUILD.json"), 64<<10)
	if err != nil {
		return HelperSelection{}, unavailable
	}
	var metadata struct {
		SchemaVersion int    `json:"schema_version"`
		Repository    string `json:"repository"`
		NativeSHA256  string `json:"binary_sha256"`
		Helper        struct {
			File, Target, SHA256 string
			Protocol             int
		} `json:"container_helper"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.SchemaVersion != 1 || metadata.Repository != "github.com/faustbrian/reviewd" || metadata.Helper.File != "reviewd-command-host" || metadata.Helper.Target != "linux/"+architecture || metadata.Helper.Protocol != commandcapture.Protocol {
		return HelperSelection{}, unavailable
	}
	native, err := packageBytes(executable, 64<<20)
	if err != nil || packageDigest(native) != metadata.NativeSHA256 {
		return HelperSelection{}, unavailable
	}
	helper := filepath.Join(filepath.Dir(executable), metadata.Helper.File)
	data, err := packageBytes(helper, 64<<20)
	if err != nil || packageDigest(data) != metadata.Helper.SHA256 {
		return HelperSelection{}, unavailable
	}
	return HelperSelection{Path: helper, SHA256: metadata.Helper.SHA256}, nil
}
