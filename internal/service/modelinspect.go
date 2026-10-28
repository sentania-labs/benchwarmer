package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/gguf"
	"github.com/sentania-labs/benchwarmer/internal/modelplan"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

const compatUnknown = "Whether the installed llama.cpp build can load this model is not checked: Benchwarmer never runs a model or the runtime to find out. If the runtime fails to start after a save, a newer llama.cpp build is the first thing to try."

// inspectDeadline bounds one header read so a file that never answers cannot
// hold an inspection slot.
var inspectDeadline = 10 * time.Second

// inspectModel reads the GGUF header of path and proposes settings for the
// current GPU. It only opens the file for reading and parses bytes.
func (s *Service) inspectModel(path string, currentContext int) api.ModelInspection {
	gpu := s.ctl.Status().GPU
	g := modelplan.GPU{TotalMiB: gpu.VRAMTotalMiB, Reliable: gpu.Confidence != policy.ConfidenceNone}
	cfg, _ := s.ctl.Config()
	allowed := []string{cfg.Runtime.ModelPath}
	for _, m := range modelFiles(filepath.Join(s.o.DataDir, "models")) {
		allowed = append(allowed, m.Path)
	}
	return inspectModelFile(path, currentContext, g, allowed)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// withDeadline runs fn and gives up waiting after d. A stuck fn is abandoned,
// not stopped, but the caller is released.
func withDeadline(d time.Duration, fn func() api.ModelInspection, timedOut func() api.ModelInspection) api.ModelInspection {
	done := make(chan api.ModelInspection, 1)
	go func() { done <- fn() }()
	select {
	case r := <-done:
		return r
	case <-time.After(d):
		return timedOut()
	}
}

// inspectModelFile reads path only if it is one of allowed: the discovered
// models and the saved model path. Anything else is answered without
// touching the disk.
func inspectModelFile(path string, currentContext int, gpu modelplan.GPU, allowed []string) api.ModelInspection {
	out := api.ModelInspection{Path: path, Compatibility: api.Compatibility{Status: "unknown", Message: compatUnknown}}
	if gpu.Reliable {
		out.GPUTotalMiB = gpu.TotalMiB
	}
	base := out
	fail := func(status, msg string) api.ModelInspection {
		r := base
		r.Status, r.Message = status, msg
		return r
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return fail(api.InspectInvalid, "The model path must be a full path, such as C:\\ProgramData\\Benchwarmer\\models\\model.gguf.")
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return fail(api.InspectInvalid, "Network and device paths are not read. Copy the model into the models folder.")
	}
	if !strings.EqualFold(filepath.Ext(path), ".gguf") {
		return fail(api.InspectNotGGUF, "This is not a .gguf file.")
	}
	known := false
	for _, a := range allowed {
		if a != "" && samePath(a, path) {
			known = true
			break
		}
	}
	if !known {
		return fail(api.InspectNotAvailable, "This path can be checked after the configuration is saved with it, or if the file is in the models folder.")
	}
	return withDeadline(inspectDeadline, func() api.ModelInspection { return readModel(path, currentContext, gpu, base, fail) },
		func() api.ModelInspection {
			return fail(api.InspectUnreadable, "Reading this file took too long, so it was skipped. Check that the path is a local file.")
		})
}

func readModel(path string, currentContext int, gpu modelplan.GPU, out api.ModelInspection, fail func(status, msg string) api.ModelInspection) api.ModelInspection {
	if li, err := os.Lstat(path); err == nil && !li.Mode().IsRegular() {
		return fail(api.InspectInvalid, "This path is not a regular file.")
	}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fail(api.InspectNotFound, "No file exists at this path.")
	case errors.Is(err, fs.ErrPermission):
		return fail(api.InspectUnreadable, "Benchwarmer is not allowed to read this file. Move it into the models folder or grant the service account read access.")
	case err != nil:
		return fail(api.InspectUnreadable, "This file could not be opened.")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return fail(api.InspectInvalid, "This path is not a regular file.")
	}
	out.SizeBytes = fi.Size()
	md, err := gguf.Parse(f)
	switch {
	case errors.Is(err, gguf.ErrNotGGUF):
		return fail(api.InspectNotGGUF, "The file does not start with a GGUF header, so it is not a model llama.cpp can load.")
	case errors.Is(err, gguf.ErrUnsupportedVersion):
		return fail(api.InspectUnsupported, "This GGUF file uses a format version Benchwarmer cannot read ("+err.Error()+"). It may still load; no settings were proposed.")
	case err != nil:
		return fail(api.InspectInvalid, "The GGUF header could not be read ("+err.Error()+"). The file may be damaged or still downloading.")
	}
	out.Status = api.InspectOK
	out.Metadata = &md
	plan := modelplan.Compute(modelplan.Input{Meta: md, FileSize: fi.Size(), GPU: gpu, CurrentContext: currentContext})
	out.Plan = &plan
	return out
}
