package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/faustbrian/reviewd/internal/lifetime"
)

// CommitSigningScope is captured by the application after reviewing the exact
// tree and commit proposal. It is not a model-selected signing request.
type CommitSigningScope struct {
	Tree, Parent, Author, Committer, Message string
}

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var commitIdentity = regexp.MustCompile(`^[^<>\x00-\x1f\x7f]+ <[^<>\x00-\x20\x7f]+> [0-9]+ [+-][0-9]{4}$`)

// ValidateCommitSigningPayload accepts only a canonical single-parent commit
// with the application-approved identity, dates, message and tree. Extra Git
// headers (including a second parent or existing signature) are not accepted.
func ValidateCommitSigningPayload(scope CommitSigningScope, payload []byte) error {
	if !objectID.MatchString(scope.Tree) || !objectID.MatchString(scope.Parent) || len(scope.Tree) != len(scope.Parent) ||
		!commitIdentity.MatchString(scope.Author) || !commitIdentity.MatchString(scope.Committer) ||
		strings.TrimSpace(scope.Message) == "" || strings.ContainsRune(scope.Message, 0) || len(scope.Message) > 1<<20 {
		return errors.New("commit signing requires a canonical application-owned single-parent scope")
	}
	expected := "tree " + scope.Tree + "\nparent " + scope.Parent + "\nauthor " + scope.Author + "\ncommitter " + scope.Committer + "\n\n" + scope.Message
	if !bytes.Equal(payload, []byte(expected)) {
		return errors.New("commit payload differs from the application-approved signing scope")
	}
	return nil
}

// SSHSignOptions is trusted operator/application configuration. The signer is
// never run inside the target repository and receives no Git/provider auth.
type SSHSignOptions struct {
	Executable, KeyFile, TempRoot string
	Environment                   []string
}

type SSHSignature struct {
	PayloadSHA256 string
	Signature     []byte
}

func SignSSHCommit(ctx context.Context, options SSHSignOptions, scope CommitSigningScope, payload []byte) (_ SSHSignature, err error) {
	// Own the bytes before validation and asynchronous execution.
	payload = bytes.Clone(payload)
	if err := ValidateCommitSigningPayload(scope, payload); err != nil {
		return SSHSignature{}, err
	}
	if !filepath.IsAbs(options.Executable) || !filepath.IsAbs(options.KeyFile) {
		return SSHSignature{}, errors.New("SSH signing requires explicit absolute executable and key paths")
	}
	if err := ctx.Err(); err != nil {
		return SSHSignature{}, err
	}
	root, err := os.MkdirTemp(lifetime.Root(ctx, options.TempRoot), "reviewd-signing-")
	if err != nil {
		return SSHSignature{}, err
	}
	defer func() {
		if !errors.Is(err, lifetime.ErrCleanupUncertain) {
			err = errors.Join(err, os.RemoveAll(root))
		}
	}()
	path := filepath.Join(root, "payload")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		return SSHSignature{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, options.Executable, "-Y", "sign", "-n", "git", "-f", options.KeyFile, path)
	command.Dir = root
	command.Env = signingEnvironment(options.Environment)
	// Signer output is not a tool response. Bound and discard diagnostic output
	// rather than exposing private key paths or unbounded signer messages.
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := lifetime.Run(ctx, command); err != nil {
		return SSHSignature{}, fmt.Errorf("scoped SSH commit signing failed: %w", err)
	}
	storage, err := os.OpenRoot(root)
	if err != nil {
		return SSHSignature{}, err
	}
	defer storage.Close()
	file, err := storage.Open("payload.sig")
	if err != nil {
		return SSHSignature{}, errors.New("SSH signer did not produce its scoped signature")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return SSHSignature{}, errors.New("SSH signer produced an invalid signature file")
	}
	signature, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(signature) > 64<<10 || !bytes.HasPrefix(signature, []byte("-----BEGIN SSH SIGNATURE-----\n")) || !bytes.HasSuffix(signature, []byte("-----END SSH SIGNATURE-----\n")) {
		return SSHSignature{}, errors.New("SSH signer produced an invalid signature envelope")
	}
	hash := sha256.Sum256(payload)
	return SSHSignature{PayloadSHA256: hex.EncodeToString(hash[:]), Signature: signature}, nil
}

func signingEnvironment(source []string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "SSH_AUTH_SOCK": true, "LANG": true, "LC_ALL": true}
	values := map[string]string{}
	for _, entry := range source {
		key, value, ok := strings.Cut(entry, "=")
		if ok && allowed[key] && !strings.ContainsRune(value, 0) {
			values[key] = value
		}
	}
	result := []string{}
	for _, key := range []string{"PATH", "HOME", "SSH_AUTH_SOCK", "LANG", "LC_ALL"} {
		if value, ok := values[key]; ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}
