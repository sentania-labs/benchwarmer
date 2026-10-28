package service

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestGGUFArchitecture(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GGUF")
	write := func(v any) {
		t.Helper()
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) { write(uint64(len(s))); b.WriteString(s) }
	write(uint32(3))
	write(uint64(0))
	write(uint64(2))
	str("general.name")
	write(uint32(8))
	str("not the architecture")
	str("general.architecture")
	write(uint32(8))
	str("llama")
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ggufArchitecture(path); got != "llama" {
		t.Fatalf("architecture %q", got)
	}
	if err := os.WriteFile(path, b.Bytes()[:20], 0600); err != nil {
		t.Fatal(err)
	}
	if got := ggufArchitecture(path); got != "" {
		t.Fatalf("truncated metadata: %q", got)
	}
}
