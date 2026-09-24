package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListModelsAndRuntimes(t *testing.T) {
	dir := t.TempDir()
	models := filepath.Join(dir, "models")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(models, "gpt-oss"), 0o755))
	must(os.WriteFile(filepath.Join(models, "b.GGUF"), []byte("x"), 0o644))
	must(os.WriteFile(filepath.Join(models, "gpt-oss", "a.gguf"), []byte("xy"), 0o644))
	must(os.WriteFile(filepath.Join(models, "notes.txt"), []byte("x"), 0o644))
	got := listModels(models)
	if len(got) != 2 || got[0].Name != "b.GGUF" || got[1].Name != "a.gguf" || got[1].SizeBytes != 2 {
		t.Fatalf("models %+v", got)
	}
	if n := len(listModels(filepath.Join(dir, "missing"))); n != 0 {
		t.Fatalf("missing folder listed %d", n)
	}

	must(os.MkdirAll(filepath.Join(dir, "runtime", "vulkan"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "runtime", "empty"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "runtime", "vulkan", "llama-server.exe"), []byte("x"), 0o755))
	rt := listRuntimes(dir)
	if len(rt) != 1 || rt[0].Name != "vulkan" {
		t.Fatalf("runtimes %+v", rt)
	}
}
