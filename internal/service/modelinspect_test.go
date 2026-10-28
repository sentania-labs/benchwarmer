package service

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/modelplan"
)

func ggufFixture(t *testing.T, size int64) string {
	t.Helper()
	var b bytes.Buffer
	w32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	w32(3)
	w64(0)
	w64(6)
	str("general.architecture")
	w32(8)
	str("llama")
	for _, kv := range []struct {
		k string
		v uint32
	}{{"llama.context_length", 131072}, {"llama.block_count", 32}, {"llama.embedding_length", 4096},
		{"llama.attention.head_count", 32}, {"llama.attention.head_count_kv", 8}} {
		str(kv.k)
		w32(4)
		w32(kv.v)
	}
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil && size > int64(b.Len()) {
		t.Fatal(err)
	}
	return p
}

func TestInspectModelFile(t *testing.T) {
	gpu := modelplan.GPU{TotalMiB: 24576, Reliable: true}
	p := ggufFixture(t, 4690<<20) // sparse: only the header is real
	got := inspectModelFile(p, 262144, gpu, []string{p})
	if got.Status != api.InspectOK || got.Metadata == nil || got.Plan == nil || got.Metadata.Arch != "llama" ||
		got.GPUTotalMiB != 24576 || got.Compatibility.Status != "unknown" {
		t.Fatalf("%+v", got)
	}
	if got.Plan.WeightsMiB != 4690 || len(got.Plan.Recommendations) == 0 {
		t.Fatalf("%+v", got.Plan)
	}
	if g := inspectModelFile(p, 8192, modelplan.GPU{}, []string{p}); g.Status != api.InspectOK || g.GPUTotalMiB != 0 || len(g.Plan.Recommendations) != 0 {
		t.Fatalf("no gpu info must give no memory proposals: %+v", g)
	}
}

func TestInspectModelFileFailuresReplaceNothing(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "t.gguf")
	os.WriteFile(text, []byte("hello, definitely not a model"), 0o644)
	bad := filepath.Join(dir, "bad.gguf")
	os.WriteFile(bad, append([]byte("GGUF"), 9, 0, 0, 0), 0o644)
	short := filepath.Join(dir, "short.gguf")
	os.WriteFile(short, []byte{'G', 'G', 'U', 'F', 3, 0, 0, 0, 0, 0}, 0o644)
	folder := filepath.Join(dir, "d.gguf")
	os.Mkdir(folder, 0o755)
	notModel := filepath.Join(dir, "a.bin")
	os.WriteFile(notModel, []byte("GGUF"), 0o644)
	cases := map[string]struct{ path, status string }{
		"missing":     {filepath.Join(dir, "none.gguf"), api.InspectNotFound},
		"text":        {text, api.InspectNotGGUF},
		"version":     {bad, api.InspectUnsupported},
		"truncated":   {short, api.InspectInvalid},
		"directory":   {folder, api.InspectInvalid},
		"wrong ext":   {notModel, api.InspectNotGGUF},
		"relative":    {"models/x.gguf", api.InspectInvalid},
		"nul in path": {filepath.Join(dir, "a\x00.gguf"), api.InspectInvalid},
		"unc":         {`\\\\host\\share\\m.gguf`, api.InspectInvalid},
		"device":      {`\\\\.\\pipe\\m.gguf`, api.InspectInvalid},
		"extended":    {`\\\\?\\C:\\m.gguf`, api.InspectInvalid},
		"slash unc":   {"//host/share/m.gguf", api.InspectInvalid},
	}
	for name, c := range cases {
		got := inspectModelFile(c.path, 8192, modelplan.GPU{TotalMiB: 24576, Reliable: true}, []string{c.path})
		if got.Status != c.status || got.Message == "" || got.Metadata != nil || got.Plan != nil {
			t.Errorf("%s: %+v", name, got)
		}
		if strings.Contains(got.Message, dir) {
			t.Errorf("%s: message leaks the folder: %q", name, got.Message)
		}
	}
}

func TestInspectModelFileUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced here")
	}
	p := ggufFixture(t, 0)
	os.Chmod(p, 0)
	if got := inspectModelFile(p, 0, modelplan.GPU{}, []string{p}); got.Status != api.InspectUnreadable {
		t.Fatalf("%+v", got)
	}
}

func TestInspectModelFileOnlyReadsAllowedPaths(t *testing.T) {
	gpu := modelplan.GPU{TotalMiB: 24576, Reliable: true}
	p := ggufFixture(t, 1<<20)
	other := ggufFixture(t, 1<<20)
	missing := filepath.Join(t.TempDir(), "none.gguf")

	if got := inspectModelFile(p, 0, gpu, []string{other}); got.Status != api.InspectNotAvailable || got.Metadata != nil || got.Plan != nil || got.SizeBytes != 0 {
		t.Fatalf("a real file outside the allowed set must not be read: %+v", got)
	}
	real, none := inspectModelFile(p, 0, gpu, nil), inspectModelFile(missing, 0, gpu, nil)
	if real.Status != none.Status || real.Message != none.Message {
		t.Fatalf("existing and missing files must be indistinguishable: %+v vs %+v", real, none)
	}
	if got := inspectModelFile(missing, 0, gpu, []string{missing}); got.Status != api.InspectNotFound {
		t.Fatalf("an allowed missing file is reported as missing: %+v", got)
	}
	if got := inspectModelFile(p, 0, gpu, []string{"", filepath.Dir(p) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(p)}); got.Status != api.InspectOK {
		t.Fatalf("an allowed path matches after cleaning: %+v", got)
	}
}

func TestInspectModelFileDotDotThroughSymlinkIsNotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	gpu := modelplan.GPU{TotalMiB: 24576, Reliable: true}
	models := t.TempDir()
	sep := string(filepath.Separator)
	allowed := filepath.Join(models, "small.gguf")
	// The target of the symlink holds a different, readable model that sits in
	// the parent of the symlink target, so "link/../x.gguf" cleans to an
	// allowed path but the OS resolves it to this one.
	outer := t.TempDir()
	secret := filepath.Join(outer, "small.gguf")
	if err := os.Rename(ggufFixture(t, 1<<20), secret); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(models, "link")); err != nil {
		t.Fatal(err)
	}
	got := inspectModelFile(models+sep+"link"+sep+".."+sep+"small.gguf", 0, gpu, []string{allowed})
	if got.Metadata != nil || got.Plan != nil || got.SizeBytes != 0 {
		t.Fatalf("a path through a symlink must not read the file it resolves to: %+v", got)
	}
}

func TestWithDeadlineReleasesAStuckRead(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	got := withDeadline(50*time.Millisecond,
		func() api.ModelInspection { <-block; return api.ModelInspection{Status: api.InspectOK} },
		func() api.ModelInspection { return api.ModelInspection{Status: api.InspectUnreadable} })
	if got.Status != api.InspectUnreadable || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v after %s", got, time.Since(start))
	}
	got = withDeadline(time.Second,
		func() api.ModelInspection { return api.ModelInspection{Status: api.InspectOK} },
		func() api.ModelInspection { return api.ModelInspection{Status: api.InspectUnreadable} })
	if got.Status != api.InspectOK {
		t.Fatalf("%+v", got)
	}
}
