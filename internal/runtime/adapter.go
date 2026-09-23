// Package runtime defines the narrow seam between the controller and the
// managed inference runtime. Only llama.cpp is implemented (spec section 10);
// the interface exists so the controller can be tested with a fake.
package runtime

import (
	"context"
	"net/url"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

// Adapter starts runtime instances.
type Adapter interface {
	// Validate checks the executable and model configuration without
	// starting anything.
	Validate(cfg config.Runtime) error
	// Start launches the runtime on the configured private loopback port,
	// inside a process-tree ownership boundary. It returns once the process
	// exists; readiness is reported by Instance.WaitReady.
	Start(ctx context.Context, cfg config.Runtime) (Instance, error)
	// Reconcile finds and terminates runtime processes not owned by this
	// worker (e.g. left by a previous worker), returning what it killed.
	Reconcile(cfg config.Runtime) ([]Orphan, error)
}

// Instance is one running runtime process tree.
type Instance interface {
	PID() int
	// Members lists every PID in the ownership boundary (for own-vs-external
	// GPU attribution).
	Members() []int
	// BaseURL is the runtime's private loopback endpoint.
	BaseURL() *url.URL
	// WaitReady blocks until the model is loaded and serving, the process
	// exits, or ctx ends.
	WaitReady(ctx context.Context) error
	// Stop terminates the whole tree and verifies that every process exited
	// within timeout. It is idempotent.
	Stop(timeout time.Duration) (StopResult, error)
	// Exited is closed when the root process exits for any reason.
	Exited() <-chan struct{}
	// ExitErr describes how the root exited (nil while running).
	ExitErr() error
	// Diagnostics returns the retained tail of runtime stdout/stderr, with
	// secrets redacted.
	Diagnostics() string
	Started() time.Time
}

// StopResult reports what a stop measured.
type StopResult struct {
	// AlreadyExited is true when the runtime had exited before Stop.
	AlreadyExited bool          `json:"already_exited"`
	RootExit      time.Duration `json:"root_exit_ns"`
	TreeEmpty     time.Duration `json:"tree_empty_ns"`
}

// Orphan is a runtime process found and killed during reconciliation.
type Orphan struct {
	PID        uint32    `json:"pid"`
	Path       string    `json:"path"`
	CreateTime time.Time `json:"create_time"`
	Killed     bool      `json:"killed"`
	Err        string    `json:"error,omitempty"`
}
