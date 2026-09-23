// Package events defines the durable audit record that explains every
// lifecycle decision. Events never contain prompts, generated content,
// request bodies, or credentials.
package events

import (
	"time"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Type classifies an event. Stable strings: stored, exported, and shown.
type Type string

// Event types.
const (
	ServiceStarted  Type = "service_started"
	ServiceStopping Type = "service_stopping"
	StateChanged    Type = "state_changed"
	// DecisionChanged records a change of winning rule without a state
	// change (e.g. cooldown reason changes from game to soft contention).
	DecisionChanged Type = "decision_changed"

	LoadStarted   Type = "load_started"
	LoadCompleted Type = "load_completed"
	LoadFailed    Type = "load_failed"

	DrainStarted    Type = "drain_started"
	DrainCompleted  Type = "drain_completed" // requests finished within grace
	GraceShortened  Type = "grace_shortened"
	Preempted       Type = "preempted" // runtime force-terminated with requests active
	RuntimeStopped  Type = "runtime_stopped"
	RuntimeCrashed  Type = "runtime_crashed"
	KillFailed      Type = "kill_failed"
	VRAMReleased    Type = "vram_released"
	VRAMNotReleased Type = "vram_release_timeout"
	OrphanKilled    Type = "orphan_killed"

	CooldownStarted    Type = "cooldown_started"
	CooldownReset      Type = "cooldown_reset"
	SuppressionStarted Type = "suppression_started"
	RecoveryScheduled  Type = "recovery_scheduled"

	ModeChanged     Type = "mode_changed"
	ModeExpired     Type = "mode_expired"
	ConfigChanged   Type = "config_changed"
	ConfigRejected  Type = "config_rejected"
	ConfigRecovered Type = "config_recovered"

	TelemetryLost      Type = "telemetry_lost"
	TelemetryRecovered Type = "telemetry_recovered"
	TelemetryDegraded  Type = "telemetry_degraded"

	PowerSuspend      Type = "power_suspend"
	PowerResume       Type = "power_resume"
	SessionChange     Type = "session_change"
	AgentConnected    Type = "agent_connected"
	AgentDisconnected Type = "agent_disconnected"
	DeviceLost        Type = "device_lost"

	// RequestsRejected aggregates 503s during one unavailable period rather
	// than one event per request.
	RequestsRejected Type = "requests_rejected"
	// RequestForceClosed records a request cut off by preemption or the
	// maximum request duration (ID and duration only).
	RequestForceClosed Type = "request_force_closed"
)

// Telemetry is the compact telemetry snapshot stored with an event.
type Telemetry struct {
	Confidence      policy.Confidence `json:"confidence"`
	TotalUtilPct    float64           `json:"total_util_pct"`
	OwnUtilPct      float64           `json:"own_util_pct"`
	ExternalUtilPct float64           `json:"external_util_pct"`
	VRAMUsedMiB     int               `json:"vram_used_mib"`
	VRAMFreeMiB     int               `json:"vram_free_mib"`
	OwnVRAMMiB      int               `json:"own_vram_mib"`
	ExternalVRAMMiB int               `json:"external_vram_mib"`
	TemperatureC    *float64          `json:"temperature_c,omitempty"`
}

// TelemetryFrom extracts the stored subset from policy facts.
func TelemetryFrom(g policy.GPUFacts) *Telemetry {
	return &Telemetry{
		Confidence: g.Confidence, TotalUtilPct: g.TotalUtilPct, OwnUtilPct: g.OwnUtilPct,
		ExternalUtilPct: g.ExternalUtilPct, VRAMUsedMiB: g.VRAMUsedMiB, VRAMFreeMiB: g.VRAMFreeMiB,
		OwnVRAMMiB: g.OwnVRAMMiB, ExternalVRAMMiB: g.ExternalVRAMMiB, TemperatureC: g.TemperatureC,
	}
}

// Event is one audit record.
type Event struct {
	ID   int64     `json:"id"` // assigned by the store
	Time time.Time `json:"time"`
	Type Type      `json:"type"`

	PrevState state.State     `json:"prev_state,omitempty"`
	State     state.State     `json:"state,omitempty"`
	Condition state.Condition `json:"condition,omitempty"`

	Profile  string            `json:"profile,omitempty"`
	Mode     policy.Mode       `json:"mode,omitempty"`
	Rule     string            `json:"rule,omitempty"`
	Severity policy.Severity   `json:"severity,omitempty"`
	Evidence []policy.Evidence `json:"evidence,omitempty"`

	Telemetry *Telemetry `json:"telemetry,omitempty"`

	RuntimePID int    `json:"runtime_pid,omitempty"`
	RequestID  string `json:"request_id,omitempty"`

	// Message is the human-readable explanation.
	Message string `json:"message"`
	// Data holds type-specific measurements (durations in milliseconds as
	// *_ms, deadlines as RFC 3339). Never prompts or bodies.
	Data map[string]any `json:"data,omitempty"`
}

// FromDecision starts an event populated from a policy decision.
func FromDecision(t Type, now time.Time, d policy.Decision, g policy.GPUFacts) Event {
	return Event{
		Time: now, Type: t, Profile: d.Profile, Mode: d.Mode, Rule: d.Rule,
		Severity: d.Severity, Evidence: d.Evidence, Telemetry: TelemetryFrom(g), Message: d.Reason,
	}
}

// Sink receives events. Implementations must not block the controller for
// long; the store buffers and writes asynchronously.
type Sink interface {
	Emit(Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Event)

// Emit implements Sink.
func (f SinkFunc) Emit(e Event) { f(e) }
