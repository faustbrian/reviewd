package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

var normalFeedbackHooks = []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "pre-push"}

// CaptureCommitPolicy reads only the selected global operator Git settings in
// app-owned storage. Neither a PR's config nor its commands select a host
// signer, key, executable or read grant. Values are never ambient diagnostics.
func CaptureCommitPolicy(ctx context.Context, root string) (policy CommitPolicy, err error) {
	policy.Environment = signingEnvironment(os.Environ())
	environment := []string{}
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok && (key == "PATH" || key == "HOME" || key == "XDG_CONFIG_HOME" || key == "GIT_CONFIG_GLOBAL") {
			environment = append(environment, item)
		}
	}
	read := func(key string, boolean bool) (string, error) {
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		args := []string{"config", "--global", "--null"}
		if boolean {
			args = append(args, "--bool")
		}
		args = append(args, "--get", key)
		command := exec.CommandContext(bounded, "git", args...)
		command.Dir = root
		command.Env = environment
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = io.Discard
		runErr := lifetime.Run(bounded, command)
		if runErr != nil {
			var exit *exec.ExitError
			if errors.As(runErr, &exit) && exit.ExitCode() == 1 && output.Len() == 0 {
				return "", nil
			}
			return "", errors.Join(errors.New("cannot read normal operator Git policy"), runErr)
		}
		if output.Len() > 64<<10 {
			return "", errors.New("operator Git policy exceeds its supported bound")
		}
		return strings.TrimSuffix(output.String(), "\x00"), nil
	}
	for _, field := range []struct {
		key string
		out *string
	}{{"user.name", &policy.Name}, {"user.email", &policy.Email}, {"core.hooksPath", &policy.HooksPath}, {"gpg.format", &policy.SigningFormat}, {"user.signingKey", &policy.SigningKey}, {"gpg.ssh.program", &policy.SigningProgram}} {
		*field.out, err = read(field.key, false)
		if err != nil {
			return policy, err
		}
	}
	sign, err := read("commit.gpgSign", true)
	if err != nil {
		return policy, err
	}
	policy.Sign = sign == "true"
	if policy.Name == "" || policy.Email == "" {
		return policy, errors.New("feedback delivery requires configured global Git user.name and user.email")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return policy, err
	}
	expand := func(value string) string {
		if strings.HasPrefix(value, "~/") {
			return filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
		return value
	}
	policy.HooksPath = expand(policy.HooksPath)
	policy.SigningKey = expand(policy.SigningKey)
	policy.RequiredHooks = map[string]string{}
	if filepath.IsAbs(policy.HooksPath) {
		for _, name := range normalFeedbackHooks {
			path := filepath.Join(policy.HooksPath, name)
			info, statErr := os.Stat(path)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
				return policy, errors.New("operator Git hook is unavailable or unsupported")
			}
			if info.Mode().Perm()&0111 == 0 {
				continue
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				return policy, openErr
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, io.LimitReader(file, (16<<20)+1))
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return policy, errors.Join(copyErr, closeErr)
			}
			policy.RequiredHooks[name] = hex.EncodeToString(hash.Sum(nil))
		}
	}
	if policy.Sign {
		if policy.SigningFormat != "ssh" {
			return policy, errors.New("required signing is not scoped SSH signing; preserve work for an explicitly supported signer")
		}
		if strings.HasPrefix(policy.SigningKey, "ssh-") || strings.HasPrefix(policy.SigningKey, "ecdsa-") {
			key, createErr := os.CreateTemp(root, "reviewd-signing-public-*")
			if createErr != nil {
				return policy, createErr
			}
			_, writeErr := key.WriteString(policy.SigningKey + "\n")
			if err = errors.Join(writeErr, key.Close()); err != nil {
				return policy, err
			}
			policy.SigningKey = key.Name()
		}
		if !filepath.IsAbs(policy.SigningKey) {
			return policy, errors.New("SSH signing requires an absolute operator key path or configured public key")
		}
		if policy.SigningProgram == "" {
			policy.SigningProgram = "ssh-keygen"
		}
		policy.SigningProgram, err = exec.LookPath(policy.SigningProgram)
		if err != nil {
			return policy, errors.New("configured SSH signing program is unavailable")
		}
		policy.SigningProgram, err = filepath.Abs(policy.SigningProgram)
		if err != nil {
			return policy, err
		}
	}
	return policy, nil
}
