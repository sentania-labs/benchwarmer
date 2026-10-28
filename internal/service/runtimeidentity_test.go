package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeIdentityRequiresMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "llama-server.exe")
	dll := filepath.Join(dir, "libllama-server-impl.dll")
	content := []byte("test binary")
	for _, p := range []string{exe, dll} {
		if err := os.WriteFile(p, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256.Sum256(content)
	m := runtimeManifest{LlamaTag: "b11901", OllamaVersion: "v0.40.12"}
	for _, p := range []string{exe, dll} {
		m.Files = append(m.Files, struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		}{filepath.Base(p), hex.EncodeToString(hash[:])})
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "benchwarmer-runtime.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if tag, _ := runtimeIdentity(dir, exe); tag != "b11901" {
		t.Fatalf("matched tag %q", tag)
	}
	if err := os.WriteFile(dll, []byte("changed binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if tag, _ := runtimeIdentity(dir, exe); tag != "" {
		t.Fatalf("changed DLL identified as %q", tag)
	}
	if tag, _ := runtimeIdentity(dir, filepath.Join(dir, "other.exe")); tag != "" {
		t.Fatalf("other executable identified as %q", tag)
	}
}
