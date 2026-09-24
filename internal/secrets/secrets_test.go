package secrets

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

func TestEnsureCreatesAndReloads(t *testing.T) {
	dir := t.TempDir()
	f := FilesFrom(config.Default().Security) // Windows-style relative refs
	a, err := EnsureTokens(dir, f)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"management": a.Management, "inference": a.Inference, "agent": a.Agent} {
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != tokenBytes {
			t.Fatalf("%s token is not 32 base64url bytes: len=%d err=%v", name, len(raw), err)
		}
		path := filepath.Join(dir, "secrets", name+".token")
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms %v", name, st.Mode().Perm())
		}
	}
	b, err := EnsureTokens(dir, f)
	if err != nil || b != a {
		t.Fatalf("reload changed tokens: err=%v", err)
	}
}

func TestLoadExistingTrimsNewline(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "m"), []byte("  handwritten-token-0123456789\r\n"), 0o600)
	tok, err := EnsureTokens(dir, Files{Management: "m", Inference: "i", Agent: "a"})
	if err != nil || tok.Management != "handwritten-token-0123456789" {
		t.Fatalf("tok=%q err=%v", tok.Management, err)
	}
}

func TestRejectsShortAndShared(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "short"), []byte("abc\n"), 0o600)
	_, err := EnsureTokens(dir, Files{Management: "short", Inference: "i", Agent: "a"})
	if err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("short token: err=%v (must not echo the value)", err)
	}
	if _, err := EnsureTokens(dir, Files{Management: "same", Inference: "i", Agent: "same"}); err == nil {
		t.Fatal("shared management/agent file accepted")
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	if got, want := Resolve(dir, `secrets\agent.token`), filepath.Join(dir, "secrets", "agent.token"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	abs := filepath.Join(dir, "x.token")
	if got := Resolve("/elsewhere", abs); got != abs {
		t.Fatalf("absolute path rewritten: %q", got)
	}
}
