package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/faustbrian/reviewd/internal/lifetime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ownedSigningRequest() (CommitSigningScope, []byte) {
	scope := CommitSigningScope{Tree: strings.Repeat("a", 40), Parent: strings.Repeat("b", 40), Author: "Fixture <fixture@reviewd.invalid> 1792036800 +0000", Committer: "Fixture <fixture@reviewd.invalid> 1792036800 +0000", Message: "fix: retain the accepted behavior\n\nPreserve the established observable contract.\n"}
	payload := []byte("tree aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nparent bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\nauthor Fixture <fixture@reviewd.invalid> 1792036800 +0000\ncommitter Fixture <fixture@reviewd.invalid> 1792036800 +0000\n\nfix: retain the accepted behavior\n\nPreserve the established observable contract.\n")
	return scope, payload
}

func TestCommitSigningRequiresExactApplicationOwnedScope(t *testing.T) {
	scope, payload := ownedSigningRequest()
	if err := ValidateCommitSigningPayload(scope, payload); err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string][]byte{
		"tree":        bytes.Replace(payload, []byte("tree a"), []byte("tree c"), 1),
		"parent":      bytes.Replace(payload, []byte("parent b"), []byte("parent c"), 1),
		"merge":       bytes.Replace(payload, []byte("author Fixture"), []byte("parent cccccccccccccccccccccccccccccccccccccccc\nauthor Fixture"), 1),
		"author":      bytes.Replace(payload, []byte("author Fixture"), []byte("author Other"), 1),
		"committer":   bytes.Replace(payload, []byte("committer Fixture"), []byte("committer Other"), 1),
		"timestamp":   bytes.Replace(payload, []byte("1792036800"), []byte("1792036801"), 1),
		"message":     bytes.Replace(payload, []byte("accepted behavior"), []byte("unreviewed behavior"), 1),
		"extraheader": bytes.Replace(payload, []byte("\n\nfix:"), []byte("\nencoding UTF-8\n\nfix:"), 1),
		"nul":         append(append([]byte{}, payload...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCommitSigningPayload(scope, changed); err == nil {
				t.Fatal("foreign signing bytes accepted")
			}
		})
	}
	invalid := scope
	invalid.Author += "\nparent cccccccccccccccccccccccccccccccccccccccc"
	if err := ValidateCommitSigningPayload(invalid, payload); err == nil {
		t.Fatal("header injection in scope accepted")
	}
}

func TestSSHCommitSignerSignsOnlyScopedBytesWithoutProviderCredentials(t *testing.T) {
	executable, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Fatal("OpenSSH signing prerequisite unavailable")
	}
	root := t.TempDir()
	key := filepath.Join(root, "owned-key")
	if output, err := exec.Command(executable, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("owned signer setup: %v %s", err, output)
	}
	storage := filepath.Join(root, "signer-storage")
	if err := os.Mkdir(storage, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "signer-invocation")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	wrapper := filepath.Join(root, "scoped-signer")
	script := "#!/bin/sh\nset -eu\nif test -n \"${GH_TOKEN-}${OPENAI_API_KEY-}${CODEX_HOME-}${GIT_CONFIG_COUNT-}\"; then exit 91; fi\nprintf invoked > " + quote(marker) + "\nexec " + quote(executable) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	options := SSHSignOptions{Executable: wrapper, KeyFile: key, TempRoot: storage, Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GH_TOKEN=owned-gh-canary", "OPENAI_API_KEY=owned-provider-canary", "CODEX_HOME=owned-auth-canary", "GIT_CONFIG_COUNT=9"}}
	scope, payload := ownedSigningRequest()
	foreign := bytes.Replace(payload, []byte("tree a"), []byte("tree c"), 1)
	if _, err := SignSSHCommit(context.Background(), options, scope, foreign); err == nil {
		t.Fatal("foreign payload reached signer")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("signer invoked before payload scope validation")
	}
	result, err := SignSSHCommit(context.Background(), options, scope, payload)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	if result.PayloadSHA256 != hex.EncodeToString(hash[:]) || !bytes.HasPrefix(result.Signature, []byte("-----BEGIN SSH SIGNATURE-----")) {
		t.Fatal("signing result lost immutable payload binding")
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed, signature := filepath.Join(root, "allowed"), filepath.Join(root, "signature")
	if err := os.WriteFile(allowed, append([]byte("fixture "), public...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signature, result.Signature, 0600); err != nil {
		t.Fatal(err)
	}
	verify := func(input []byte) error {
		command := exec.Command(executable, "-Y", "verify", "-f", allowed, "-I", "fixture", "-n", "git", "-s", signature)
		command.Stdin = bytes.NewReader(input)
		return command.Run()
	}
	if err := verify(payload); err != nil {
		t.Fatalf("independent SSH signature verification failed: %v", err)
	}
	if err := verify(foreign); err == nil {
		t.Fatal("signature verified an unapproved tree")
	}
	if entries, err := os.ReadDir(storage); err != nil || len(entries) != 0 {
		t.Fatal("completed signer retained task-owned payload storage")
	}
}

func TestSSHCommitSignerCancellationStopsBeforeReturning(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "signer-storage")
	if err := os.Mkdir(storage, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "slow-signer")
	pidPath := filepath.Join(root, "signer.pid")
	script := "#!/bin/sh\necho $$ > " + "\"" + pidPath + "\"" + "\nexec /bin/sleep 60\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "owned-public-key")
	if err := os.WriteFile(key, []byte("owned fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scope, payload := ownedSigningRequest()
	done := make(chan error, 1)
	go func() {
		_, err := SignSSHCommit(ctx, SSHSignOptions{Executable: executable, KeyFile: key, TempRoot: storage, Environment: []string{"PATH=/usr/bin:/bin", "HOME=" + root}}, scope, payload)
		done <- err
	}()
	pid := 0
	for pid == 0 {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid != 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("signer ended before cancellation barrier: %v", err)
		case <-ctx.Done():
			t.Fatal("signer never reached owned barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("signer cancellation lost: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("signer cancellation did not finish")
	}
	proof, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if alive, err := lifetime.GroupAlive(proof, pid); err != nil || alive {
		t.Fatalf("signer returned before owned group stopped: alive=%t err=%v", alive, err)
	}

	if entries, err := os.ReadDir(storage); err != nil || len(entries) != 0 {
		t.Fatal("stopped signer retained task-owned storage")
	}
}

func TestSSHCommitSignerRejectsUnusableOrEscapingOutput(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "oversized", "escaping"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			storage := filepath.Join(root, "storage")
			if err := os.Mkdir(storage, 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("-----BEGIN SSH SIGNATURE-----\nowned-not-a-signature\n-----END SSH SIGNATURE-----\n"), 0600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(root, "signer")
			body := "#!/bin/sh\nset -eu\nfor argument do payload=$argument; done\n"
			switch kind {
			case "missing":
				body += "exit 0\n"
			case "malformed":
				body += "printf malformed > \"$payload.sig\"\n"
			case "oversized":
				body += "{ printf '%s\\n' '-----BEGIN SSH SIGNATURE-----'; dd if=/dev/zero bs=65537 count=1 2>/dev/null | tr '\\000' A; printf '\\n%s\\n' '-----END SSH SIGNATURE-----'; } > \"$payload.sig\"\n"
			case "escaping":
				body += "ln -s \"" + outside + "\" \"$payload.sig\"\n"
			}
			if err := os.WriteFile(executable, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			scope, payload := ownedSigningRequest()
			result, err := SignSSHCommit(context.Background(), SSHSignOptions{Executable: executable, KeyFile: outside, TempRoot: storage, Environment: []string{"PATH=/usr/bin:/bin"}}, scope, payload)
			if err == nil || len(result.Signature) != 0 {
				t.Fatal("unusable or foreign signer output accepted")
			}
			if entries, err := os.ReadDir(storage); err != nil || len(entries) != 0 {
				t.Fatal("proven stopped failure retained payload storage")
			}
		})
	}
}

func TestSSHCommitSignerRetainsPayloadWhenShutdownProofIsUnavailable(t *testing.T) {
	root := t.TempDir()
	storage, tools := filepath.Join(root, "storage"), filepath.Join(root, "tools")
	for _, path := range []string{storage, tools} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	pidPath := filepath.Join(root, "owned.pid")
	executable := filepath.Join(root, "slow-signer")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho $$ > \""+pidPath+"\"\nexec /bin/sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte("#!/bin/sh\nprintf 'invalid-metadata\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	actualPath := os.Getenv("PATH")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+actualPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	scope, payload := ownedSigningRequest()
	go func() {
		_, err := SignSSHCommit(ctx, SSHSignOptions{Executable: executable, KeyFile: filepath.Join(root, "key"), TempRoot: storage, Environment: []string{"PATH=/usr/bin:/bin"}}, scope, payload)
		done <- err
	}()
	barrier := time.NewTimer(5 * time.Second)
	defer barrier.Stop()
	pid := 0
	for pid == 0 {
		if data, err := os.ReadFile(pidPath); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid != 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("signer ended before barrier: %v", err)
		case <-barrier.C:
			t.Fatal("signer did not reach barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, lifetime.ErrCleanupUncertain) {
			t.Fatalf("shutdown uncertainty lost: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("signer failed bounded cancellation")
	}
	t.Setenv("PATH", actualPath)
	proof, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if alive, err := lifetime.GroupAlive(proof, pid); err != nil || alive {
		t.Fatalf("fixture cleanup unproven: %t %v", alive, err)
	}
	entries, err := os.ReadDir(storage)
	if err != nil || len(entries) != 1 {
		t.Fatal("unproven shutdown deleted signing payload")
	}
	retained, err := os.ReadFile(filepath.Join(storage, entries[0].Name(), "payload"))
	if err != nil || !bytes.Equal(retained, payload) {
		t.Fatal("retained signing evidence missing")
	}
}
