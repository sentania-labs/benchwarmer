// Package api serves the versioned management API (/api/v1/*). This file is
// the wire contract shared with the web UI and tray; handlers live beside it.
// Times are RFC 3339 with offset; clients render them in local time.
package api

import (
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Health is GET /api/v1/health. It reports whether the service itself is
// working, not whether inference is available.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	// Problems lists service-level faults (config fallback, store errors,
	// telemetry collector down).
	Problems []string `json:"problems,omitempty"`
}

// Status is GET /api/v1/status.
type Status struct {
	Time      time.Time       `json:"time"`
	Version   string          `json:"version"`
	Condition state.Condition `json:"condition"`
	State     state.State     `json:"state"`
	// Summary is one sentence for the tray tooltip and dashboard header.
	Summary string `json:"summary"`

	Mode     ModeStatus      `json:"mode"`
	Profile  ProfileStatus   `json:"profile"`
	Runtime  RuntimeStatus   `json:"runtime"`
	GPU      GPUStatus       `json:"gpu"`
	Decision policy.Decision `json:"decision"`
	Timers   TimerStatus     `json:"timers"`
	Agent    AgentStatus     `json:"agent"`
	// Trigger names the competing application or metric, when known.
	Trigger string `json:"trigger,omitempty"`
	// RecentErrors are the last few error-severity events.
	RecentErrors []events.Event `json:"recent_errors,omitempty"`
	// ConfigSource is "primary", "last_good", or "default"; anything but
	// primary is a problem the UI must show.
	ConfigSource string `json:"config_source"`
}

// ModeStatus describes the manual mode.
type ModeStatus struct {
	Mode  policy.Mode `json:"mode"`
	Until *time.Time  `json:"until,omitempty"`
	SetBy string      `json:"set_by,omitempty"`
	SetAt time.Time   `json:"set_at,omitzero"`
}

// ProfileStatus describes the active profile.
type ProfileStatus struct {
	Active     string    `json:"active"`
	Source     string    `json:"source"` // "default", "schedule:<name>", "ai_priority"
	Timezone   string    `json:"timezone"`
	NextChange time.Time `json:"next_change,omitzero"`
}

// RuntimeStatus describes the managed runtime.
type RuntimeStatus struct {
	PID              int        `json:"pid,omitempty"`
	Ready            bool       `json:"ready"`
	Model            string     `json:"model"` // file name only
	ActiveRequests   int        `json:"active_requests"`
	OldestRequestSec float64    `json:"oldest_request_seconds"`
	LoadedAt         *time.Time `json:"loaded_at,omitempty"`
	LastLoadSeconds  float64    `json:"last_load_seconds,omitempty"`
	FootprintMiB     int        `json:"footprint_mib,omitempty"`
	RestartCount     int        `json:"crash_count"`
}

// GPUStatus is current telemetry.
type GPUStatus struct {
	Adapter         string            `json:"adapter"`
	Confidence      policy.Confidence `json:"confidence"`
	TotalUtilPct    float64           `json:"total_util_pct"`
	OwnUtilPct      float64           `json:"own_util_pct"`
	ExternalUtilPct float64           `json:"external_util_pct"`
	VRAMTotalMiB    int               `json:"vram_total_mib"`
	VRAMUsedMiB     int               `json:"vram_used_mib"`
	VRAMFreeMiB     int               `json:"vram_free_mib"`
	OwnVRAMMiB      int               `json:"own_vram_mib"`
	ExternalVRAMMiB int               `json:"external_vram_mib"`
	TemperatureC    *float64          `json:"temperature_c,omitempty"`
	TopExternal     []policy.AppMatch `json:"top_external,omitempty"`
	SampledAt       time.Time         `json:"sampled_at,omitzero"`
}

// TimerStatus holds deadlines relevant to the current state.
type TimerStatus struct {
	GraceUntil      *time.Time `json:"grace_until,omitempty"`
	CooldownUntil   *time.Time `json:"cooldown_until,omitempty"`
	SuppressedUntil *time.Time `json:"suppressed_until,omitempty"`
	RecoveryUntil   *time.Time `json:"recovery_until,omitempty"`
	// NextLoadAt and NextLoadReason answer "when is the next load attempt
	// allowed, and why not sooner".
	NextLoadAt     *time.Time `json:"next_load_at,omitempty"`
	NextLoadReason string     `json:"next_load_reason,omitempty"`
}

// AgentStatus describes the session agent connection.
type AgentStatus struct {
	Connected  bool      `json:"connected"`
	LastReport time.Time `json:"last_report,omitzero"`
	SessionID  uint32    `json:"session_id,omitempty"`
}

// ModeRequest is PUT /api/v1/mode. Duration is a Go duration string; empty
// means indefinite. Special values: "until_reboot", "until_tomorrow".
type ModeRequest struct {
	Mode     policy.Mode `json:"mode"`
	Duration string      `json:"duration,omitempty"`
	SetBy    string      `json:"set_by,omitempty"`
}

// ConfigResponse is GET /api/v1/config (secrets redacted) and the success
// body of PUT /api/v1/config.
type ConfigResponse struct {
	Config config.Config  `json:"config"`
	Source string         `json:"source"`
	Impact *config.Impact `json:"impact,omitempty"`
}

// ApplicationsBody is GET/PUT /api/v1/applications.
type ApplicationsBody struct {
	Applications []config.AppRule `json:"applications"`
}

// EventsResponse is GET /api/v1/events?limit=&before_id=&type=.
type EventsResponse struct {
	Events []events.Event `json:"events"`
	// NextBeforeID pages backwards; zero when there are no older events.
	NextBeforeID int64 `json:"next_before_id,omitempty"`
}

// AgentReport is POST /api/v1/agent/report from the session agent.
type AgentReport struct {
	SessionID      uint32  `json:"session_id"`
	ForegroundPID  uint32  `json:"foreground_pid,omitempty"`
	ForegroundName string  `json:"foreground_name,omitempty"`
	ForegroundPath string  `json:"foreground_path,omitempty"`
	Fullscreen     bool    `json:"fullscreen"`
	Notification   string  `json:"notification_state,omitempty"`
	IdleSeconds    float64 `json:"idle_seconds"`
	Locked         bool    `json:"locked"`
}

// Error is every non-2xx JSON body, including the proxy's 503.
type Error struct {
	Error   string              `json:"error"`
	Code    string              `json:"code"` // machine-readable, e.g. "worker_unavailable", "invalid_config"
	Details []config.FieldError `json:"details,omitempty"`
	// For worker_unavailable: the condition, reason, and when to retry.
	Condition  state.Condition `json:"condition,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	RetryAfter *time.Time      `json:"retry_after,omitempty"`
}
