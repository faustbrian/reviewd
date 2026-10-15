// Package commandcapture authenticates bounded command output independently of
// the live control transport. Its files belong to the existing private owner;
// they never authorize replay, owner closure or container removal.
package commandcapture

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

const Protocol = 1
const Limit = 1 << 20 // Per stream; two streams remain below the JSONL transport cap.
const KeyEnvironment = "REVIEWD_PRIVATE_CAPTURE_KEY"
const GuestDirectory = "/reviewd-output"
const GuestHelper = "/reviewd-command-host"

var ErrIncomplete = errors.New("private command output is incomplete or unauthenticated")

type Request struct {
	Nonce string
	Argv  []string
}
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}
type metadata struct {
	Protocol     int    `json:"protocol"`
	Nonce        string `json:"nonce"`
	Command      string `json:"command_sha256"`
	Complete     *bool  `json:"complete"`
	ExitCode     *int   `json:"exit_code"`
	StdoutBytes  *int   `json:"stdout_bytes"`
	StderrBytes  *int   `json:"stderr_bytes"`
	StdoutSHA256 string `json:"stdout_sha256"`
	StderrSHA256 string `json:"stderr_sha256"`
	MAC          string `json:"mac,omitempty"`
}

type boundedOutput struct {
	buffer bytes.Buffer
	failed bool
}

func (w *boundedOutput) Len() int      { return w.buffer.Len() }
func (w *boundedOutput) Bytes() []byte { return w.buffer.Bytes() }

func (w *boundedOutput) Write(data []byte) (int, error) {
	if w.failed {
		return 0, ErrIncomplete
	}
	remaining := Limit - w.Len()
	if len(data) > remaining {
		_, _ = w.buffer.Write(data[:remaining])
		w.failed = true
		return remaining, ErrIncomplete
	}
	return w.buffer.Write(data)
}

type Session struct {
	Stdout, Stderr  io.Writer
	out, diagnostic *boundedOutput
	directory       string
	key             []byte
	request         Request
	finished        bool
}

func validRequest(request Request) bool {
	nonce, err := hex.DecodeString(request.Nonce)
	if err != nil || len(nonce) != 16 || request.Nonce != strings.ToLower(request.Nonce) || len(request.Argv) == 0 || len(request.Argv) > 256 {
		return false
	}
	total := 0
	for _, argument := range request.Argv {
		total += len(argument)
		if strings.ContainsRune(argument, 0) {
			return false
		}
	}
	return total <= 128<<10
}
func commandDigest(argv []string) string {
	data, _ := json.Marshal(argv)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func authenticate(value metadata, key []byte) string {
	value.MAC = ""
	payload, _ := json.Marshal(value)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("reviewd-command-capture-v1\x00"))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
func New(directory string, key []byte, request Request) (*Session, error) {
	if len(key) != 32 || !validRequest(request) {
		return nil, ErrIncomplete
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, ErrIncomplete
	}
	out, diagnostic := &boundedOutput{}, &boundedOutput{}
	return &Session{Stdout: out, Stderr: diagnostic, out: out, diagnostic: diagnostic, directory: directory, key: append([]byte{}, key...), request: Request{Nonce: request.Nonce, Argv: append([]string{}, request.Argv...)}}, nil
}

// Finish signs the helper's own pipe observations, never a reread of files the
// repository command can alter. Tampering can deny output, not forge completeness.
// Call only after the foreground wait and both output pumps have joined.
func (s *Session) Finish(exit int, complete bool) error {
	if s.finished || exit < 0 || exit > 255 {
		return ErrIncomplete
	}
	s.finished = true
	complete = complete && !s.out.failed && !s.diagnostic.failed
	outLength, errLength := s.out.Len(), s.diagnostic.Len()
	value := metadata{Protocol: Protocol, Nonce: s.request.Nonce, Command: commandDigest(s.request.Argv), Complete: &complete, ExitCode: &exit, StdoutBytes: &outLength, StderrBytes: &errLength, StdoutSHA256: digest(s.out.Bytes()), StderrSHA256: digest(s.diagnostic.Bytes())}
	value.MAC = authenticate(value, s.key)
	data, err := json.Marshal(value)
	if err != nil {
		return ErrIncomplete
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return ErrIncomplete
	}
	defer root.Close()
	for _, entry := range []struct {
		name string
		data []byte
	}{{"stdout", s.out.Bytes()}, {"stderr", s.diagnostic.Bytes()}, {"result.json", data}} {
		file, err := root.OpenFile(entry.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return ErrIncomplete
		}
		count, writeErr := file.Write(entry.data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil || count != len(entry.data) {
			return ErrIncomplete
		}
	}
	return nil
}
func readFile(root *os.Root, name string, limit int) ([]byte, error) {
	// Nonblocking/no-follow open closes the special-file substitution window;
	// fstat validates the opened descriptor, not just a preceding path lookup.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrIncomplete
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > int64(limit) {
		return nil, ErrIncomplete
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, ErrIncomplete
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || len(data) > limit || int64(len(data)) != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, ErrIncomplete
	}
	return data, nil
}

// Read must be called only after canonical removal of the exact owned container.
// Stable files alone never constitute that control-plane proof.
func Read(directory string, key []byte, request Request, canonicalExit int) (Result, error) {
	if len(key) != 32 || !validRequest(request) || canonicalExit < 0 || canonicalExit > 255 {
		return Result{}, ErrIncomplete
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return Result{}, ErrIncomplete
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Result{}, ErrIncomplete
	}
	defer root.Close()
	raw, err := readFile(root, "result.json", 4096)
	if err != nil {
		return Result{}, ErrIncomplete
	}
	var value metadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Protocol != Protocol || value.Nonce != request.Nonce || value.Command != commandDigest(request.Argv) || value.Complete == nil || !*value.Complete || value.ExitCode == nil || *value.ExitCode != canonicalExit || value.StdoutBytes == nil || value.StderrBytes == nil {
		return Result{}, ErrIncomplete
	}
	provided, err := hex.DecodeString(value.MAC)
	expected, _ := hex.DecodeString(authenticate(value, key))
	if err != nil || !hmac.Equal(provided, expected) {
		return Result{}, ErrIncomplete
	}
	out, err := readFile(root, "stdout", Limit)
	if err != nil {
		return Result{}, ErrIncomplete
	}
	diagnostic, err := readFile(root, "stderr", Limit)
	if err != nil || len(out) != *value.StdoutBytes || len(diagnostic) != *value.StderrBytes || digest(out) != value.StdoutSHA256 || digest(diagnostic) != value.StderrSHA256 || !utf8.Valid(out) || !utf8.Valid(diagnostic) {
		return Result{}, ErrIncomplete
	}
	return Result{Stdout: string(out), Stderr: string(diagnostic), ExitCode: canonicalExit}, nil
}
