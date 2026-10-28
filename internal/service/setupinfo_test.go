package service

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
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

func TestConfigProblemNamesTheRejectedField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := config.Default()
	c.Listen.Management = "127.0.0.1:" + strconv.Itoa(c.Runtime.Port)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	lr, err := config.NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if lr.Source != "default" {
		t.Fatalf("source %q", lr.Source)
	}
	got := configProblem(lr)
	if !strings.Contains(got, "listen.management") || !strings.Contains(got, "already used by runtime.port") {
		t.Fatalf("problem %q does not say which field and why", got)
	}
	fields := configProblemFields(lr)
	if len(fields) != 2 || fields[0].Field != "listen.management" || fields[1].Field != "runtime.port" {
		t.Fatalf("structured fields: %+v", fields)
	}
	if configProblem(config.LoadResult{Source: "primary"}) != "" {
		t.Fatal("a clean load reported a problem")
	}
}

func TestModelFileOnUnreachableShareIsUnknown(t *testing.T) {
	missing := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	if err := classifyModelFile(`\\nas\models\x.gguf`, missing); !errors.Is(err, config.ErrFileUnknown) {
		t.Fatalf("offline share: %v", err)
	}
	dir := t.TempDir()
	reachable := func(p string) (os.FileInfo, error) {
		if p == `\\nas\models\` {
			return os.Stat(dir)
		}
		return nil, os.ErrNotExist
	}
	if err := classifyModelFile(`\\nas\models\x.gguf`, reachable); err == nil || errors.Is(err, config.ErrFileUnknown) {
		t.Fatalf("missing file on a reachable share must be a hard error: %v", err)
	}
	if err := classifyModelFile(`D:\models\x.gguf`, missing); err == nil || errors.Is(err, config.ErrFileUnknown) {
		t.Fatalf("missing file on a local path must be a hard error: %v", err)
	}
}

func TestFileExistsIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if checkModelFile(f) != nil || checkModelFile(dir) == nil || checkModelFile(filepath.Join(dir, "nope.gguf")) == nil {
		t.Fatal("fileExists wrong")
	}
}

func TestModelFilesListsNamesWithoutReadingHeaders(t *testing.T) {
	p := ggufFixture(t, 1<<20)
	dir := filepath.Dir(p)
	names := modelFiles(dir)
	if len(names) != 1 || names[0].Path != p || names[0].Architecture != "" {
		t.Fatalf("%+v", names)
	}
	if full := listModels(dir); len(full) != 1 || full[0].Architecture != "llama" {
		t.Fatalf("%+v", full)
	}
}
