package api

import (
	"context"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
)

// Backend is what the API needs from the running service. The controller
// implements it; tests use a fake. Methods must be safe for concurrent use
// and must not block on the runtime (drain and reload are requests the
// controller acts on asynchronously).
type Backend interface {
	Status() Status
	SetMode(req ModeRequest) (ModeStatus, error)
	// RequestDrain asks the worker to drain and unload now, as a manual
	// action (not counted toward anti-thrashing). The worker reloads when
	// policy allows.
	RequestDrain(reason string) error
	// RequestReload asks the worker to restart the runtime (e.g. after a
	// runtime config change). It drains first.
	RequestReload(reason string) error
	// Config returns the active config (unredacted; the API redacts) and
	// its source.
	Config() (config.Config, string)
	// UpdateConfig validates, persists atomically, applies, and audits a
	// new config, returning what the change requires.
	UpdateConfig(ctx context.Context, c config.Config, actor string) (config.Impact, error)
	AgentReport(r AgentReport)
	Events(q EventQuery) ([]events.Event, error)
}

// EventQuery filters GET /api/v1/events.
type EventQuery struct {
	Limit    int
	BeforeID int64
	Types    []events.Type
}
