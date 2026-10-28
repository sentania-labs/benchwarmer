package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/gguf"
	"github.com/sentania-labs/benchwarmer/internal/modelplan"
)

// SetupInfo is what the dashboard offers to pick from, so an administrator
// configures the runtime and certificate without typing paths.
type SetupInfo struct {
	DataDir   string `json:"data_dir"`
	ModelsDir string `json:"models_dir"`
	// Models are the *.gguf files in ModelsDir.
	Models []ModelFile `json:"models"`
	// Runtimes are the llama.cpp builds installed with Benchwarmer.
	Runtimes []RuntimeInstall `json:"runtimes"`
	// Certificates are the LocalMachine\My certificates that can serve
	// HTTPS; CertificatesError says why the list is empty when it could not
	// be read.
	Certificates      []StoreCertificate `json:"certificates"`
	CertificatesError string             `json:"certificates_error,omitempty"`
}

// ModelFile is a model the runtime could load.
type ModelFile struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	SizeBytes    int64     `json:"size_bytes"`
	Modified     time.Time `json:"modified"`
	Architecture string    `json:"architecture,omitempty"`
}

// RuntimeInstall is an installed llama-server build.
type RuntimeInstall struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	LlamaTag      string `json:"llama_cpp_tag,omitempty"`
	OllamaVersion string `json:"ollama_version,omitempty"`
}

// StoreCertificate is a certificate the inference listener could use.
type StoreCertificate struct {
	Thumbprint string    `json:"thumbprint"`
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	DNSNames   []string  `json:"dns_names"`
	NotAfter   time.Time `json:"not_after"`
	SelfSigned bool      `json:"self_signed"`
}

func (h *handler) setup(w http.ResponseWriter, r *http.Request) {
	if h.o.Setup == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "setup information is not available")
		return
	}
	writeJSON(w, http.StatusOK, h.o.Setup())
}

// Model inspection statuses. Only StatusOK carries metadata and a plan; every
// other status leaves the configured model untouched and says why.
const (
	InspectOK          = "ok"
	InspectNotFound    = "not_found"
	InspectUnreadable  = "unreadable"
	InspectNotGGUF     = "not_gguf"
	InspectUnsupported = "unsupported"
	InspectInvalid     = "invalid"
	// InspectNotAvailable is the one answer for a path outside the models
	// folder and the saved model path. It does not say whether the file exists.
	InspectNotAvailable = "not_available"
)

// ModelInspection answers GET /api/v1/setup/model: what the header of one
// model file says, and the settings that follow from it. Nothing is applied;
// the UI shows the proposals and the owner accepts or declines each.
type ModelInspection struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// Metadata and Plan are set only when Status is "ok".
	Metadata *gguf.Metadata  `json:"metadata,omitempty"`
	Plan     *modelplan.Plan `json:"plan,omitempty"`
	// GPUTotalMiB is the VRAM the plan used; 0 when telemetry had none.
	GPUTotalMiB int `json:"gpu_total_mib"`
	// Compatibility with the installed llama.cpp build. Benchwarmer never
	// runs a model or the runtime to find out, so this is "unknown" unless
	// a build is proven unable to load the file.
	Compatibility Compatibility `json:"compatibility"`
}

// Compatibility is whether the selected runtime can load the model.
type Compatibility struct {
	// Status is "unknown" or "incompatible". Only proven incompatibility
	// is "incompatible".
	Status  string `json:"status"`
	Message string `json:"message"`
}

// maxInspectPath bounds the path query parameter.
const maxInspectPath = 1024

// inspectSlots bounds concurrent header reads so a burst of requests cannot
// pile up disk reads.
var inspectSlots = make(chan struct{}, 2)

func (h *handler) inspectModel(w http.ResponseWriter, r *http.Request) {
	if h.o.InspectModel == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "model inspection is not available")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" || len(path) > maxInspectPath {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "path is required and must be under 1024 characters")
		return
	}
	ctx := 0
	if v := r.URL.Query().Get("context"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1<<30 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "context must be a whole number of tokens")
			return
		}
		ctx = n
	}
	select {
	case inspectSlots <- struct{}{}:
		defer func() { <-inspectSlots }()
	default:
		writeError(w, http.StatusTooManyRequests, CodeRejected, "another model is being read; try again in a moment")
		return
	}
	writeJSON(w, http.StatusOK, h.o.InspectModel(path, ctx))
}

// Restarting answers POST /api/v1/service/restart.
type Restarting struct {
	Restarting bool `json:"restarting"`
}

func (h *handler) restart(w http.ResponseWriter, r *http.Request) {
	if h.o.Restart == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "restart is not available")
		return
	}
	if err := h.o.Restart(); err != nil {
		writeError(w, http.StatusConflict, CodeRejected, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, Restarting{Restarting: true})
}
