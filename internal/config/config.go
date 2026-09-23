// Package config defines Benchwarmer's versioned configuration schema,
// defaults, validation, change-impact classification, secret redaction, and
// atomic persistence with a last-known-good copy (ADR 0008).
package config

// SchemaVersion is the current configuration schema version.
const SchemaVersion = 1

// Config is the complete service configuration.
type Config struct {
	SchemaVersion int `json:"schema_version"`

	Runtime    Runtime    `json:"runtime"`
	Listen     Listen     `json:"listen"`
	Telemetry  Telemetry  `json:"telemetry"`
	Safety     Safety     `json:"safety"`
	Contention Contention `json:"contention"`

	// Profiles by name. DefaultProfile applies outside schedules;
	// AIPriorityProfile applies while the AI Priority mode is active.
	Profiles          map[string]Profile `json:"profiles"`
	DefaultProfile    string             `json:"default_profile"`
	AIPriorityProfile string             `json:"ai_priority_profile"`
	Schedules         []Schedule         `json:"schedules"`
	// Timezone is an IANA zone name; schedules are evaluated in it, so
	// daylight-saving transitions follow the zone's rules.
	Timezone string `json:"timezone"`

	Applications []AppRule  `json:"applications"`
	AntiThrash   AntiThrash `json:"anti_thrash"`
	Recovery     Recovery   `json:"recovery"`
	Modes        Modes      `json:"modes"`
	Signals      Signals    `json:"signals"`
	Security     Security   `json:"security"`
	Retention    Retention  `json:"retention"`
	Metrics      Metrics    `json:"metrics"`
	Logging      Logging    `json:"logging"`
}

// Runtime configures the managed llama-server.
type Runtime struct {
	Executable string `json:"executable"`
	ModelPath  string `json:"model_path"`
	// Args are extra llama-server arguments. Benchwarmer supplies -m,
	// --host, --port, -c and -ngl itself; those must not appear here.
	Args        []string `json:"args"`
	ContextSize int      `json:"context_size"`
	GPULayers   int      `json:"gpu_layers"`
	// Host and Port are the private loopback listener for the runtime.
	Host string `json:"host"`
	Port int    `json:"port"`

	LoadTimeout             Duration `json:"load_timeout"`
	RequiredFreeVRAMMiB     int      `json:"required_free_vram_mib"`
	KillVerifyTimeout       Duration `json:"kill_verify_timeout"`
	VRAMReleaseTimeout      Duration `json:"vram_release_timeout"`
	VRAMReleaseToleranceMiB int      `json:"vram_release_tolerance_mib"`
	// MaxRequestDuration bounds any single proxied inference request.
	MaxRequestDuration Duration `json:"max_request_duration"`
	// DiagnosticsTailBytes bounds retained runtime stdout/stderr.
	DiagnosticsTailBytes int `json:"diagnostics_tail_bytes"`
}

// Listen configures Benchwarmer's own listeners.
type Listen struct {
	// Inference is the proxy listener (OpenAI-compatible /v1/*). This is the
	// only listener intended for LAN exposure.
	Inference string `json:"inference"`
	// Management serves /api/v1/*, /metrics and the web UI.
	Management string `json:"management"`
}

// Telemetry configures GPU sampling.
type Telemetry struct {
	// Adapter selects the GPU by LUID or name substring; empty picks the
	// discrete adapter with the most dedicated memory.
	Adapter        string   `json:"adapter"`
	SampleInterval Duration `json:"sample_interval"`
	// LossGrace is how long telemetry may be unavailable before the worker
	// yields the GPU.
	LossGrace Duration `json:"loss_grace"`
	// DegradedMarginPct tightens soft thresholds by this percentage when
	// per-process attribution is unavailable.
	DegradedMarginPct int `json:"degraded_margin_pct"`
}

// Safety holds hard limits. Crossing one terminates the runtime immediately
// in every mode, including AI Priority.
type Safety struct {
	GPUTempCriticalC float64 `json:"gpu_temp_critical_c"`
	GPUTempResumeC   float64 `json:"gpu_temp_resume_c"`
}

// Contention holds critical (hard) external-demand thresholds. These bypass
// grace in every profile and mode.
type Contention struct {
	VRAMFreeCriticalMiB     int      `json:"vram_free_critical_mib"`
	ExternalVRAMCriticalMiB int      `json:"external_vram_critical_mib"`
	ExternalUtilCriticalPct float64  `json:"external_util_critical_pct"`
	CriticalWindow          Duration `json:"critical_window"`
	// ClearWindow is how long signals must stay below soft thresholds
	// before contention counts as gone (hysteresis).
	ClearWindow Duration `json:"clear_window"`
}

// Profile holds the tunable, non-safety policy for a period of time.
type Profile struct {
	Description string `json:"description,omitempty"`
	// Grace is how long an active request may continue once draining starts.
	Grace Duration `json:"grace"`
	// GraceUnderPressure replaces Grace when soft external pressure is
	// present during the drain.
	GraceUnderPressure Duration `json:"grace_under_pressure"`
	// Cooldown is measured from the last moment a competing workload was
	// observed.
	Cooldown Duration `json:"cooldown"`

	ExternalUtilSoftPct float64  `json:"external_util_soft_pct"`
	SoftWindow          Duration `json:"soft_window"`
	ExternalVRAMSoftMiB int      `json:"external_vram_soft_mib"`

	// DrainOnGameProcess drains as soon as a classified game process runs,
	// before it is confirmed by foreground or GPU use (early warning).
	DrainOnGameProcess bool `json:"drain_on_game_process"`
	// DrainOnAmbiguous drains on ambiguous interactive signals: an
	// unclassified fullscreen foreground app, or a GPU-using child of a
	// launcher, without GPU corroboration.
	DrainOnAmbiguous bool `json:"drain_on_ambiguous"`
	// DrainOnSoftContention drains when soft external GPU/VRAM thresholds
	// are exceeded for SoftWindow.
	DrainOnSoftContention bool `json:"drain_on_soft_contention"`

	// GameConfirmUtilPct / GameConfirmVRAMMiB: a game process using at least
	// this much counts as confirmed gaming even without foreground data.
	GameConfirmUtilPct float64 `json:"game_confirm_util_pct"`
	GameConfirmVRAMMiB int     `json:"game_confirm_vram_mib"`
}

// Schedule activates a profile on given weekdays between Start and End
// (local wall-clock "HH:MM", End exclusive) in Config.Timezone.
type Schedule struct {
	Name    string   `json:"name"`
	Profile string   `json:"profile"`
	Days    []string `json:"days"` // "mon".."sun"
	Start   string   `json:"start"`
	End     string   `json:"end"`
	Enabled bool     `json:"enabled"`
}

// App classes.
const (
	ClassGame     = "game"
	ClassLauncher = "launcher"
	ClassIgnore   = "ignore"
	ClassOrdinary = "ordinary"
)

// AppRule classifies processes. Exactly one matcher must be set. Matching is
// case-insensitive and treats / and \ alike. Glob matches the full path, where
// * matches any run of characters (including separators) and ? matches one.
// Rules are evaluated in order; the first match wins.
type AppRule struct {
	Name       string `json:"name,omitempty"`
	Exe        string `json:"exe,omitempty"`
	Path       string `json:"path,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
	Glob       string `json:"glob,omitempty"`
	Class      string `json:"class"`
}

// AntiThrash suppresses reloads after repeated preemptions.
type AntiThrash struct {
	Window         Duration `json:"window"`
	MaxPreemptions int      `json:"max_preemptions"`
	SuppressFor    Duration `json:"suppress_for"`
}

// Recovery holds conservative cooldowns and backoff after abnormal events.
type Recovery struct {
	StartupCooldown           Duration `json:"startup_cooldown"`
	ResumeCooldown            Duration `json:"resume_cooldown"`
	CrashBackoffInitial       Duration `json:"crash_backoff_initial"`
	CrashBackoffMax           Duration `json:"crash_backoff_max"`
	TelemetryRecoveryCooldown Duration `json:"telemetry_recovery_cooldown"`
	DeviceLostCooldown        Duration `json:"device_lost_cooldown"`
	// CrashResetAfter: a runtime that stays up this long resets crash backoff.
	CrashResetAfter Duration `json:"crash_reset_after"`
}

// Modes configures manual-mode choices offered in the tray and UI.
type Modes struct {
	PauseDurations      []Duration `json:"pause_durations"`
	AIPriorityDurations []Duration `json:"ai_priority_durations"`
	// MaxAIPriority caps any AI Priority request.
	MaxAIPriority Duration `json:"max_ai_priority"`
}

// Signals configures non-GPU signal handling.
type Signals struct {
	// SessionStaleAfter: session-agent facts older than this are unknown.
	SessionStaleAfter Duration `json:"session_stale_after"`
	ProcessInterval   Duration `json:"process_interval"`
}

// Security configures credentials (ADR 0007). Values are file references;
// token values never appear in the config.
type Security struct {
	// LoopbackTrust allows read-only management calls from loopback without
	// a token. Writes always need the management token.
	LoopbackTrust         bool   `json:"loopback_trust"`
	ManagementTokenFile   string `json:"management_token_file"`
	InferenceTokenFile    string `json:"inference_token_file"`
	AgentTokenFile        string `json:"agent_token_file"`
	RequireInferenceToken bool   `json:"require_inference_token"`
}

// Retention bounds persisted data.
type Retention struct {
	EventsDays int `json:"events_days"`
	EventsMax  int `json:"events_max"`
	LogMaxMB   int `json:"log_max_mb"`
	LogFiles   int `json:"log_files"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Enabled bool `json:"enabled"`
}

// Logging configures the service log.
type Logging struct {
	Level string `json:"level"` // debug, info, warn, error
	// Dir is the log directory; empty means the data directory.
	Dir string `json:"dir"`
}
