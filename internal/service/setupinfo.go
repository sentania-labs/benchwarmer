package service

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/tlscert"
)

// setupInfo lists what the dashboard offers to pick from: model files in
// the models folder, llama.cpp builds installed next to the service, and
// certificates in the Windows store that can serve HTTPS.
func (s *Service) setupInfo() api.SetupInfo {
	models := filepath.Join(s.o.DataDir, "models")
	info := api.SetupInfo{DataDir: s.o.DataDir, ModelsDir: models,
		Models: listModels(models), Runtimes: listRuntimes(installDir()), Certificates: []api.StoreCertificate{}}
	certs, err := tlscert.ListStore()
	if err != nil {
		info.CertificatesError = err.Error()
	}
	for _, c := range certs {
		info.Certificates = append(info.Certificates, api.StoreCertificate(c))
	}
	return info
}

// installDir is the folder holding the running executable (Program
// Files\Benchwarmer when installed).
func installDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// listModels returns the GGUF files directly in dir and one level below
// (models are often kept in a folder per model), sorted by path.
func listModels(dir string) []api.ModelFile {
	out := []api.ModelFile{}
	add := func(p string, fi os.FileInfo) {
		if fi.Mode().IsRegular() && strings.EqualFold(filepath.Ext(p), ".gguf") {
			out = append(out, api.ModelFile{Name: filepath.Base(p), Path: p, SizeBytes: fi.Size(), Modified: fi.ModTime()})
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			sub, _ := os.ReadDir(p)
			for _, se := range sub {
				if fi, err := se.Info(); err == nil {
					add(filepath.Join(p, se.Name()), fi)
				}
			}
			continue
		}
		if fi, err := e.Info(); err == nil {
			add(p, fi)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path) })
	return out
}

// listRuntimes returns runtime\<name>\llama-server(.exe) under dir.
func listRuntimes(dir string) []api.RuntimeInstall {
	out := []api.RuntimeInstall{}
	if dir == "" {
		return out
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "runtime"))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		for _, exe := range []string{"llama-server.exe", "llama-server"} {
			p := filepath.Join(dir, "runtime", e.Name(), exe)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				out = append(out, api.RuntimeInstall{Name: e.Name(), Path: p})
				break
			}
		}
	}
	return out
}
