// Package policy decides what the worker should do from a snapshot of facts.
// Evaluate is a pure function: no I/O, no clock reads, no global state. The
// signal layer computes rolling windows into the snapshot; the controller
// turns decisions into state transitions (ADR 0004).
package policy

import (
	"time"

	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Mode is the manual operating mode.
type Mode string

// Operating modes (spec section 5).
const (
	ModeAuto       Mode = "auto"
	ModePause      Mode = "pause"
	ModeAIPriority Mode = "ai_priority"
)

// Confidence is how far GPU telemetry can be trusted (ADR 0003).
type Confidence string

// Telemetry confidence levels.
const (
	ConfidenceHigh     Confidence = "high"
	ConfidenceDegraded Confidence = "degraded"
	ConfidenceNone     Confidence = "none"
)

// Snapshot is every fact policy evaluates. Durations such as "above the
// critical threshold for" are computed by the signal layer from a rolling
// window; the evaluator only compares them with configured windows.
type Snapshot struct {
	Now time.Time `json:"now"`

	Mode    ModeFacts    `json:"mode"`
	Runtime RuntimeFacts `json:"runtime"`
	GPU     GPUFacts     `json:"gpu"`
	Apps    AppFacts     `json:"apps"`
	Session SessionFacts `json:"session"`
	Power   PowerFacts   `json:"power"`
	Timers  TimerFacts   `json:"timers"`
}

// ModeFacts describes the manual mode in force.
type ModeFacts struct {
	Mode Mode `json:"mode"`
	// Until is when a temporary mode expires; nil means indefinite. The
	// controller reverts expired modes to auto before building a snapshot.
	Until *time.Time `json:"until,omitempty"`
	SetBy string     `json:"set_by,omitempty"` // "tray", "api", "ui"
}

// RuntimeFacts describes the managed runtime.
type RuntimeFacts struct {
	State state.State `json:"state"`
	PID   int         `json:"pid,omitempty"`
	// ActiveRequests and OldestRequestAge come from proxy accounting.
	ActiveRequests   int           `json:"active_requests"`
	OldestRequestAge time.Duration `json:"oldest_request_age_ns"`
	// DrainingSince is when the current drain began (zero if not draining).
	DrainingSince time.Time `json:"draining_since,omitzero"`
	// FootprintMiB is the runtime's measured dedicated VRAM once loaded, or
	// the last load's figure; used to infer external VRAM when attribution
	// is degraded.
	FootprintMiB int `json:"footprint_mib"`
}

// GPUFacts are telemetry-derived facts for the target adapter.
type GPUFacts struct {
	Confidence Confidence `json:"confidence"`
	// NoneFor is how long confidence has been none (0 if not none).
	NoneFor time.Duration `json:"none_for_ns"`
	// HealthyFor is how long confidence has been high or degraded; used for
	// the telemetry-recovery cooldown.
	HealthyFor time.Duration `json:"healthy_for_ns"`

	TotalUtilPct    float64 `json:"total_util_pct"`
	OwnUtilPct      float64 `json:"own_util_pct"`
	ExternalUtilPct float64 `json:"external_util_pct"`
	// ExternalUtilTrusted is false when utilization could not be separated
	// (degraded confidence while the runtime is busy or loading).
	ExternalUtilTrusted bool `json:"external_util_trusted"`

	// Hysteresis facts from the rolling window. AboveCriticalFor is how
	// long external utilization has continuously exceeded the critical
	// threshold; AboveSoftFor the active profile's soft threshold;
	// BelowSoftFor how long it has continuously been below soft.
	AboveCriticalFor time.Duration `json:"above_critical_for_ns"`
	AboveSoftFor     time.Duration `json:"above_soft_for_ns"`
	BelowSoftFor     time.Duration `json:"below_soft_for_ns"`

	VRAMTotalMiB    int `json:"vram_total_mib"`
	VRAMUsedMiB     int `json:"vram_used_mib"`
	VRAMFreeMiB     int `json:"vram_free_mib"`
	OwnVRAMMiB      int `json:"own_vram_mib"`
	ExternalVRAMMiB int `json:"external_vram_mib"`
	// ExternalVRAMGrowthMiB is the change in external VRAM over the
	// critical window; positive means something is allocating.
	ExternalVRAMGrowthMiB int `json:"external_vram_growth_mib"`

	TemperatureC *float64 `json:"temperature_c,omitempty"`
}

// AppMatch is one running process with a classification.
type AppMatch struct {
	PID   uint32 `json:"pid"`
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	Class string `json:"class"` // config.Class*
	// Rule names the matching rule; "inferred:launcher-child" for an
	// unclassified GPU-using child of a launcher.
	Rule       string        `json:"rule"`
	GPUUtilPct float64       `json:"gpu_util_pct"`
	VRAMMiB    int           `json:"vram_mib"`
	Foreground bool          `json:"foreground"`
	Fullscreen bool          `json:"fullscreen"`
	RunningFor time.Duration `json:"running_for_ns"`
	ParentName string        `json:"parent_name,omitempty"`
}

// AppFacts groups classified processes. Ignored processes are omitted.
type AppFacts struct {
	Games     []AppMatch `json:"games,omitempty"`
	Launchers []AppMatch `json:"launchers,omitempty"`
	// Ordinary holds classified non-game GPU users (OBS, browsers) with
	// measurable GPU use.
	Ordinary []AppMatch `json:"ordinary,omitempty"`
	// LauncherChildren are unclassified processes whose parent is a
	// launcher and that use the GPU: likely unlisted games.
	LauncherChildren []AppMatch `json:"launcher_children,omitempty"`
	// ProcessListOK is false when the process snapshot failed.
	ProcessListOK bool `json:"process_list_ok"`
}

// SessionFacts come from the session agent (ADR 0005).
type SessionFacts struct {
	// Known is false when no fresh agent report exists; all other fields
	// are then meaningless.
	Known          bool   `json:"known"`
	UserLoggedOn   bool   `json:"user_logged_on"`
	Locked         bool   `json:"locked"`
	ForegroundName string `json:"foreground_name,omitempty"`
	ForegroundPath string `json:"foreground_path,omitempty"`
	ForegroundPID  uint32 `json:"foreground_pid,omitempty"`
	// ForegroundClass is the foreground process's classification, or ""
	// when unclassified.
	ForegroundClass string        `json:"foreground_class,omitempty"`
	Fullscreen      bool          `json:"fullscreen"`
	IdleFor         time.Duration `json:"idle_for_ns"`
}

// PowerFacts describe power and device events.
type PowerFacts struct {
	// Suspending is set from the suspend notification until resume.
	Suspending bool `json:"suspending"`
	// ShuttingDown is set when the service is stopping.
	ShuttingDown bool `json:"shutting_down"`
	// DeviceLost is set while the adapter is missing or reset.
	DeviceLost bool `json:"device_lost"`
}

// TimerFacts are the controller's recorded timestamps and deadlines. Expiry
// is computed by policy from these plus the active profile, so a profile
// change (school hours starting, AI Priority) takes effect immediately.
type TimerFacts struct {
	// LastCompetingAt is the last time a yield rule matched. Cooldown ends
	// at LastCompetingAt + active profile cooldown.
	LastCompetingAt   time.Time `json:"last_competing_at,omitzero"`
	LastCompetingRule string    `json:"last_competing_rule,omitempty"`
	// Preemptions are recent externally caused unloads (drains and forced
	// preemptions, not manual pauses), for anti-thrashing.
	Preemptions []time.Time `json:"preemptions,omitempty"`
	// SuppressedAt starts an anti-thrash suppression; it ends at
	// SuppressedAt + anti_thrash.suppress_for.
	SuppressedAt time.Time `json:"suppressed_at,omitzero"`
	// RecoveryUntil is an absolute deadline from startup, resume, crash
	// backoff, telemetry recovery, or device loss; RecoveryReason says which.
	RecoveryUntil  time.Time `json:"recovery_until,omitzero"`
	RecoveryReason string    `json:"recovery_reason,omitempty"`
}

// Action is the desired worker behavior.
type Action string

// Actions.
const (
	// ActionRun: keep running; load if stopped.
	ActionRun Action = "run"
	// ActionHold: do not load; a running runtime keeps running.
	ActionHold Action = "hold"
	// ActionDrain: stop admitting; allow active requests Grace; then unload.
	ActionDrain Action = "drain"
	// ActionPreempt: stop admitting and unload now.
	ActionPreempt Action = "preempt"
)

// Yields reports whether the action stops admission.
func (a Action) Yields() bool { return a == ActionDrain || a == ActionPreempt }

// Tier is the precedence tier (spec section 7), 1 = highest.
type Tier int

// Precedence tiers.
const (
	TierSafety     Tier = 1
	TierPause      Tier = 2
	TierCritical   Tier = 3
	TierGaming     Tier = 4
	TierAIPriority Tier = 5
	TierSchedule   Tier = 6
	TierDefault    Tier = 7
	// TierEligibility gates loading when every tier above says run.
	TierEligibility Tier = 8
)

// Severity ranks how disruptive a decision is, for display and alerting.
type Severity string

// Severities.
const (
	SeverityInfo     Severity = "info"
	SeverityNotice   Severity = "notice"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Evidence is one observed value relevant to a decision.
type Evidence struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Threshold string `json:"threshold,omitempty"`
	// Source names where the value came from (e.g. "pdh", "agent",
	// "process-list", "timer").
	Source string `json:"source,omitempty"`
}

// Decision is the policy verdict for one snapshot.
type Decision struct {
	Action   Action   `json:"action"`
	Rule     string   `json:"rule"`
	Tier     Tier     `json:"tier"`
	Severity Severity `json:"severity"`
	// Reason is a short human-readable explanation.
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence,omitempty"`

	// Profile is the profile whose parameters applied; ProfileSource says
	// why ("default", "schedule:<name>", "ai_priority").
	Profile       string `json:"profile"`
	ProfileSource string `json:"profile_source"`
	Mode          Mode   `json:"mode"`

	// Grace applies to ActionDrain: the maximum time active requests may
	// continue from the start of the drain. The controller never extends a
	// drain deadline, only shortens it.
	Grace time.Duration `json:"grace_ns,omitempty"`
	// Competing is true when the winning rule represents a competing
	// workload; the controller records LastCompetingAt, which (re)starts
	// the cooldown.
	Competing bool `json:"competing"`
	// CountsAsPreemption is true when an unload caused by this decision
	// counts toward anti-thrashing.
	CountsAsPreemption bool `json:"counts_as_preemption"`
	// Suppress is true when an unload now would reach the anti-thrash limit;
	// the controller then records SuppressedAt.
	Suppress bool `json:"suppress"`
	// IdleState is the state the controller should use while the runtime
	// is not running under this decision.
	IdleState state.State `json:"idle_state"`
	// NextLoadAt is the earliest time a load could be allowed, when known
	// (cooldown, suppression, recovery). Zero when no timer applies.
	NextLoadAt time.Time `json:"next_load_at,omitzero"`
	// AlsoMatched lists lower-precedence rules that also matched, for
	// explanation.
	AlsoMatched []string `json:"also_matched,omitempty"`
}
