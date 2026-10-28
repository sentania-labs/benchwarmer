package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runtimeManifest struct {
	LlamaTag      string `json:"llama_cpp_tag"`
	OllamaVersion string `json:"ollama_version"`
	Files         []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}
type hashEntry struct {
	size     int64
	modified time.Time
	sum      string
}

var runtimeHashes = struct {
	sync.Mutex
	entries map[string]hashEntry
}{entries: map[string]hashEntry{}}

// hashRuntimeFile limits work and caches by file identity. Read errors or
// changes while reading leave identity unknown.
func hashRuntimeFile(path string) string {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 256<<20 {
		return ""
	}
	runtimeHashes.Lock()
	e, ok := runtimeHashes.entries[path]
	runtimeHashes.Unlock()
	if ok && e.size == fi.Size() && e.modified.Equal(fi.ModTime()) {
		return e.sum
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	if err != nil || n != fi.Size() {
		return ""
	}
	after, err := f.Stat()
	if err != nil || after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime()) {
		return ""
	}
	sum := hex.EncodeToString(h.Sum(nil))
	runtimeHashes.Lock()
	if len(runtimeHashes.entries) >= 512 {
		clear(runtimeHashes.entries)
	}
	runtimeHashes.entries[path] = hashEntry{fi.Size(), fi.ModTime(), sum}
	runtimeHashes.Unlock()
	return sum
}

// runtimeIdentity only trusts the installed package manifest. All shipped
// executable files must match, since the server is a stub loading DLLs.
func runtimeIdentity(dir, exe string) (string, string) {
	manifest, err := os.Open(filepath.Join(dir, "benchwarmer-runtime.json"))
	if err != nil {
		return "", ""
	}
	defer manifest.Close()
	b, err := io.ReadAll(io.LimitReader(manifest, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return "", ""
	}
	var m runtimeManifest
	if json.Unmarshal(b, &m) != nil || len(m.Files) == 0 || len(m.Files) > 256 {
		return "", ""
	}
	found := false
	seen := map[string]bool{}
	var total int64
	for _, file := range m.Files {
		p := filepath.FromSlash(file.Path)
		if filepath.IsAbs(p) || p == "." || strings.HasPrefix(filepath.Clean(p), ".."+string(filepath.Separator)) || filepath.Clean(p) == ".." {
			return "", ""
		}
		full := filepath.Join(dir, p)
		if seen[full] {
			return "", ""
		}
		seen[full] = true
		fi, err := os.Stat(full)
		if err != nil {
			return "", ""
		}
		total += fi.Size()
		if total > 256<<20 {
			return "", ""
		}
		if full == exe {
			found = true
		}
		if len(file.SHA256) != 64 || hashRuntimeFile(full) != strings.ToLower(file.SHA256) {
			return "", ""
		}
	}
	if !found || !seen[filepath.Join(dir, "libllama-server-impl.dll")] {
		return "", ""
	}
	return m.LlamaTag, m.OllamaVersion
}
