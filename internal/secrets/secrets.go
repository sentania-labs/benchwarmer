// Package secrets creates and loads Benchwarmer's bearer tokens (ADR 0007).
// Token values live only in their files and in memory; they are never
// logged, returned by the API, or written to the config.
//
// File permissions here are POSIX 0600 and the directory 0700, which is what
// Go can express portably. On Windows those bits do not restrict access; the
// service sets the ACLs itself at start (internal/provision, ADR 0012:
// SYSTEM and Administrators full control, interactive users read on the
// agent token only). This package does not manage Windows ACLs.
package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

// tokenBytes is the random entropy in a generated token.
const tokenBytes = 32

// minTokenLen rejects hand-written tokens too short to resist guessing.
const minTokenLen = 16

// Files are the token file references, relative to the data directory
// unless absolute.
type Files struct {
	Management string
	Inference  string
	Agent      string
}

// FilesFrom takes the token file references from the config.
func FilesFrom(s config.Security) Files {
	return Files{Management: s.ManagementTokenFile, Inference: s.InferenceTokenFile, Agent: s.AgentTokenFile}
}

// Tokens are loaded token values.
type Tokens struct {
	Management string
	Inference  string
	Agent      string
}

// Resolve returns the path of a token file reference. Config values use
// Windows separators; both \ and / are accepted so tests and tooling work on
// any platform.
func Resolve(dataDir, ref string) string {
	p := filepath.FromSlash(strings.ReplaceAll(ref, `\`, "/"))
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return p
	}
	return filepath.Join(dataDir, p)
}

// EnsureTokens loads each token, creating any missing file with a fresh
// random token. The three tokens must differ, so the agent or inference
// credential can never act as the management credential.
func EnsureTokens(dataDir string, f Files) (Tokens, error) {
	var t Tokens
	var err error
	if t.Management, err = ensure(Resolve(dataDir, f.Management)); err != nil {
		return Tokens{}, fmt.Errorf("management token: %w", err)
	}
	if t.Inference, err = ensure(Resolve(dataDir, f.Inference)); err != nil {
		return Tokens{}, fmt.Errorf("inference token: %w", err)
	}
	if t.Agent, err = ensure(Resolve(dataDir, f.Agent)); err != nil {
		return Tokens{}, fmt.Errorf("agent token: %w", err)
	}
	if t.Management == t.Inference || t.Management == t.Agent || t.Inference == t.Agent {
		return Tokens{}, errors.New("management, inference, and agent tokens must be different (check that the token files are distinct)")
	}
	return t, nil
}

// Generate returns a new random token: 32 bytes, base64url without padding.
func Generate() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Load reads a token file. Surrounding whitespace (such as a trailing
// newline from an editor) is ignored. Errors name the file, never its
// contents.
func Load(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := string(bytes.TrimSpace(b))
	if len(tok) < minTokenLen {
		return "", fmt.Errorf("%s: token is shorter than %d characters", path, minTokenLen)
	}
	if strings.ContainsAny(tok, " \t\r\n") {
		return "", fmt.Errorf("%s: token contains whitespace", path)
	}
	return tok, nil
}

func ensure(path string) (string, error) {
	tok, err := Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return tok, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tok, err = Generate()
	if err != nil {
		return "", err
	}
	// O_EXCL: if another process created the file first, use its token.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return Load(path)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("%s: write token: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("%s: write token: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return tok, nil
}
