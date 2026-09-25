package api

import (
	"net/http"
	"time"
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
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	Modified  time.Time `json:"modified"`
}

// RuntimeInstall is an installed llama-server build.
type RuntimeInstall struct {
	Name string `json:"name"`
	Path string `json:"path"`
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
