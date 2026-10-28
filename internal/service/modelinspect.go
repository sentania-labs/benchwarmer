package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/gguf"
	"github.com/sentania-labs/benchwarmer/internal/modelplan"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

const compatUnknown = "Whether the installed llama.cpp build can load this model is not checked: Benchwarmer never runs a model or the runtime to find out. If the runtime fails to start after a save, a newer llama.cpp build is the first thing to try."

// inspectModel reads the GGUF header of path and proposes settings for the
// current GPU. It only opens the file for reading and parses bytes.
func (s *Service) inspectModel(path string, currentContext int) api.ModelInspection {
	gpu := s.ctl.Status().GPU
	g := modelplan.GPU{TotalMiB: gpu.VRAMTotalMiB, Reliable: gpu.Confidence != policy.ConfidenceNone}
	return inspectModelFile(path, currentContext, g)
}

func inspectModelFile(path string, currentContext int, gpu modelplan.GPU) api.ModelInspection {
	out := api.ModelInspection{Path: path, Compatibility: api.Compatibility{Status: "unknown", Message: compatUnknown}}
	if gpu.Reliable {
		out.GPUTotalMiB = gpu.TotalMiB
	}
	fail := func(status, msg string) api.ModelInspection {
		out.Status, out.Message = status, msg
		return out
	}
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return fail(api.InspectInvalid, "The model path must be a full path, such as C:\\ProgramData\\Benchwarmer\\models\\model.gguf.")
	}
	if !strings.EqualFold(filepath.Ext(path), ".gguf") {
		return fail(api.InspectNotGGUF, "This is not a .gguf file.")
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
