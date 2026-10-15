package containercmd

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// freezeHelper copies checksum-bound installed bytes into the common owner.
// A repository/PATH executable or native Mach-O cannot become the Linux helper.
func freezeHelper(root, selected, expected, architecture string) (string, error) {
	if !filepath.IsAbs(selected) || filepath.Clean(selected) != selected || len(expected) != 64 {
		return "", errors.New("container commands require a checksum-bound installed Linux helper")
	}
	file, err := os.OpenFile(selected, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("installed container helper is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 64<<20 {
		return "", errors.New("installed container helper is not a bounded executable")
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	sum := sha256.Sum256(data)
	if err != nil || int64(len(data)) != info.Size() || hex.EncodeToString(sum[:]) != expected {
		return "", errors.New("installed container helper checksum differs from its package")
	}
	binary, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return "", errors.New("container helper must be a static Linux executable")
	}
	defer binary.Close()
	machine := elf.EM_X86_64
	if architecture == "arm64" {
		machine = elf.EM_AARCH64
	} else if architecture != "amd64" {
		return "", errors.New("container helper architecture is unsupported")
	}
	if binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB || binary.Machine != machine || (binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN) {
		return "", errors.New("container helper architecture differs from the selected guest")
	}
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			return "", errors.New("container helper must not depend on an image dynamic loader")
		}
	}
	build, err := buildinfo.Read(bytes.NewReader(data))
	if err != nil || build.Path != "github.com/faustbrian/reviewd/cmd/reviewd-command-host" || build.Main.Path != "github.com/faustbrian/reviewd" {
		return "", errors.New("container helper does not identify the expected application")
	}
	settings := map[string]string{}
	for _, setting := range build.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != architecture || settings["CGO_ENABLED"] != "0" {
		return "", errors.New("container helper build metadata differs from the supported guest")
	}
	frozen, err := os.CreateTemp(root, "container-helper-")
	if err != nil {
		return "", err
	}
	name := frozen.Name()
	good := false
	defer func() {
		if !good {
			_ = os.Remove(name)
		}
	}()
	count, writeErr := frozen.Write(data)
	modeErr := frozen.Chmod(0500)
	syncErr := frozen.Sync()
	closeErr := frozen.Close()
	if writeErr != nil || modeErr != nil || syncErr != nil || closeErr != nil || count != len(data) {
		return "", errors.New("cannot freeze installed container helper")
	}
	good = true
	return name, nil
}
