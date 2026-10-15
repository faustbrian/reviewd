package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// commandExecutable resolves only the application-selected installation. Linux
// re-executes this native file inside bwrap, so granting a JS launcher or its
// parent installation directory is neither sufficient nor appropriately scoped.
func commandExecutable(ctx context.Context, selected string) (string, error) {
	path, err := exec.LookPath(selected)
	if err != nil {
		return "", errors.New("Codex CLI is required for enforced feedback command isolation")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("selected Codex executable is unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if filepath.Base(path) == "codex.js" {
		root := filepath.Dir(filepath.Dir(path))
		file, openErr := os.Open(filepath.Join(root, "package.json"))
		if openErr != nil {
			return "", errors.New("Codex npm installation metadata is unavailable; select its standalone native executable")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (64<<10)+1))
		closeErr := file.Close()
		var metadata struct {
			Name string `json:"name"`
			Bin  struct {
				Codex string `json:"codex"`
			} `json:"bin"`
		}
		if readErr != nil || closeErr != nil || len(data) > 64<<10 || json.Unmarshal(data, &metadata) != nil || metadata.Name != "@openai/codex" || metadata.Bin.Codex != "bin/codex.js" {
			return "", errors.New("unsupported Codex npm launcher; select its standalone native executable")
		}
		node, lookupErr := exec.LookPath("node")
		if lookupErr != nil {
			return "", errors.New("Node is required to resolve the selected Codex npm installation")
		}
		// Match the pinned official launcher's Node resolution without importing
		// or executing package code. No NODE_OPTIONS/NODE_PATH or credentials are
		// inherited. Optional dependencies may be nested or package-manager links.
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		program := `const {createRequire}=require('node:module'); const p=require('node:path'); const targets={'linux/x64':['linux-x64','x86_64-unknown-linux-musl'],'linux/arm64':['linux-arm64','aarch64-unknown-linux-musl'],'darwin/x64':['darwin-x64','x86_64-apple-darwin'],'darwin/arm64':['darwin-arm64','aarch64-apple-darwin']}; const target=targets[process.platform+'/'+process.arch]; if(!target) throw new Error('Unsupported Codex native platform'); const r=createRequire(process.argv[1]); let vendor; try {vendor=p.join(p.dirname(r.resolve('@openai/codex-'+target[0]+'/package.json')),'vendor')} catch {vendor=p.join(p.dirname(process.argv[1]),'..','vendor')} process.stdout.write(p.join(vendor,target[1],'bin','codex'));`
		command := exec.CommandContext(probe, node, "-e", program, path)
		command.Dir = root
		command.Env = []string{"PATH=" + os.Getenv("PATH")}
		output, probeErr := lifetime.Output(probe, command)
		if probeErr != nil {
			return "", errors.Join(errors.New("cannot resolve the selected Codex npm native executable"), probeErr)
		}
		if len(output) > 8192 || !filepath.IsAbs(string(output)) || strings.ContainsAny(string(output), "\x00\r\n") {
			return "", errors.New("Codex npm native executable resolution returned an invalid path")
		}
		path, err = filepath.EvalSymlinks(string(output))
		if err != nil {
			return "", errors.New("Codex npm native executable is missing; reinstall Codex or select a standalone native executable")
		}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("selected Codex native executable must be an executable regular file")
	}
	return path, nil
}
