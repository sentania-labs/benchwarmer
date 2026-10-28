package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Owner kinds: what the person at the PC needs to know first.
const (
	OwnerAvailable = "available" // AI is on
	OwnerStarting  = "starting"  // AI is loading or about to
	OwnerWaiting   = "waiting"   // normal wait; nothing to do
	OwnerPaused    = "paused"    // the owner paused it
	OwnerProblem   = "problem"   // something needs attention
	OwnerUnknown   = "unknown"   // Benchwarmer cannot tell
)

// Owner severities, from calm to urgent. A client chooses colors and icons
// from these, never from the kind or the condition.
const (
	OwnerSeverityOK        = "ok"
	OwnerSeverityWait      = "wait"
	OwnerSeverityPaused    = "paused"
	OwnerSeverityAttention = "attention" // AI works, but something needs fixing
	OwnerSeverityProblem   = "problem"
	OwnerSeverityUnknown   = "unknown"
)

// OwnerStatus is the owner-facing reading of a Status. It carries facts and
// codes, not sentences: the client words them in local time.
type OwnerStatus struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	// Cause says why: ready, busy, loading, starting, game, gpu_busy,
	// cooldown, startup, resume, drained, recovering, suppressed, low_vram,
	// stopping, sleeping, paused, too_hot, setup, crash, gpu_fault,
	// no_gpu_readings, no_process_list, vram_too_small, or unknown.
	Cause string `json:"cause"`
	// After names what a cooldown follows: game, gpu_busy, heat, or other.
	After string `json:"after,omitempty"`
	// App is the application that caused a wait, when one is named.
	App string `json:"app,omitempty"`
	// Detail is service-written text for the setup cause.
	Detail string `json:"detail,omitempty"`
	// Yielding is set while AI is stepping aside (draining or stopping).
	Yielding bool `json:"yielding,omitempty"`
	// ReturnAt is when AI can next start, only when a timer decides it. It
	// is a bound, not a promise: later checks (GPU memory, a game starting
	// again) can still hold it back, so clients must qualify it.
	ReturnAt *time.Time `json:"return_at,omitempty"`
	// ResumeDelaySeconds is how long AI waits after the GPU is free again,
	// for waits with no end time because the cause is still present.
	ResumeDelaySeconds int `json:"resume_delay_seconds,omitempty"`
	// ConfigFallback is set while the service runs on a fallback config.
	ConfigFallback bool `json:"config_fallback,omitempty"`
}

// DeriveOwner reads a Status the way its owner would. cooldown is the active
// profile's cooldown. It is a pure function of its inputs, and anything it
// does not recognise is "unknown" rather than a guess.
func DeriveOwner(s Status, cooldown time.Duration) OwnerStatus {
	o := classify(s, cooldown)
	if s.ConfigSource != "" && s.ConfigSource != "primary" {
		o.ConfigFallback = true
		if o.Severity == OwnerSeverityOK {
			o.Severity = OwnerSeverityAttention
		}
	}
	return o
}

func classify(s Status, cooldown time.Duration) OwnerStatus {
	d := s.Decision
	yielding := s.State == state.Draining || s.State == state.Preempting
	switch s.State {
	case state.Ready:
		return OwnerStatus{Kind: OwnerAvailable, Severity: OwnerSeverityOK, Cause: "ready"}
	case state.Busy:
		return OwnerStatus{Kind: OwnerAvailable, Severity: OwnerSeverityOK, Cause: "busy"}
	case state.Loading:
		return OwnerStatus{Kind: OwnerStarting, Severity: OwnerSeverityOK, Cause: "loading"}
	}
	o := OwnerStatus{Kind: OwnerWaiting, Severity: OwnerSeverityWait, Yielding: yielding}
	bad := func(cause string) OwnerStatus {
		o.Kind, o.Severity, o.Cause = OwnerProblem, OwnerSeverityProblem, cause
		return o
	}
	wait := func(cause string) OwnerStatus {
		o.Cause = cause
		return o
	}
	timed := func(cause string) OwnerStatus {
		o.Cause, o.ReturnAt = cause, s.Timers.NextLoadAt
		return o
	}
	switch rule := d.Rule; {
	case rule == policy.RuleShutdown:
		return wait("stopping")
	case rule == policy.RuleSuspend:
		return wait("sleeping")
	case rule == policy.RuleDeviceLost:
		return bad("gpu_fault")
	case rule == policy.RuleTemperature:
		return bad("too_hot")
	case rule == policy.RuleTelemetryLost, rule == policy.RuleNoTelemetry:
		return bad("no_gpu_readings")
	case rule == policy.RuleNoProcessList:
		return bad("no_process_list")
	case rule == policy.RuleSetupRequired:
		o = bad("setup")
		o.Detail = strings.TrimPrefix(d.Reason, "Setup required: ")
		return o
	case rule == policy.RulePause:
		o.Kind, o.Severity, o.Cause = OwnerPaused, OwnerSeverityPaused, "paused"
		o.ReturnAt = s.Timers.NextLoadAt
		return o
	case strings.HasPrefix(rule, "contention."), rule == policy.RuleSoftContention:
		o.ResumeDelaySeconds = int(cooldown.Seconds())
		o.App = evidenceValue(d, "process", "top_external_process")
		return wait("gpu_busy")
	case strings.HasPrefix(rule, "gaming."), rule == policy.RuleFullscreenGPU, rule == policy.RuleGameProcess, rule == policy.RuleAmbiguous:
		o.ResumeDelaySeconds = int(cooldown.Seconds())
		o.App = evidenceValue(d, "process", "foreground")
		return wait("game")
	case rule == policy.RuleSuppressed:
		return timed("suppressed")
	case rule == policy.RuleCooldown:
		o.After = cooldownAfter(evidenceValue(d, "last_competing_rule"))
		return timed("cooldown")
	case rule == policy.RuleVRAMInsufficient:
		// Memory another program holds comes back; memory the GPU never
		// had does not, so that is a setting to fix, not a wait.
		if need := evidenceThreshold(d, "vram_free_mib"); need > 0 && s.GPU.VRAMTotalMiB > 0 && need > s.GPU.VRAMTotalMiB {
			return bad("vram_too_small")
		}
		return wait("low_vram")
	case rule == policy.RuleRecovery:
		return recovery(o, s, evidenceValue(d, "recovery_reason"))
	case rule == policy.RuleRun, rule == policy.RuleAIPriorityRun:
		if s.State == state.Error {
			return bad("crash")
		}
		return wait("starting")
	}
	return OwnerStatus{Kind: OwnerUnknown, Severity: OwnerSeverityUnknown, Cause: "unknown"}
}

// recovery maps a recovery wait onto what the owner sees: routine waits
// after startup or wake are normal, faults are not.
func recovery(o OwnerStatus, s Status, reason string) OwnerStatus {
	o.ReturnAt = s.Timers.NextLoadAt
	fault := func(cause string) OwnerStatus {
		o.Kind, o.Severity, o.Cause = OwnerProblem, OwnerSeverityProblem, cause
		return o
	}
	switch {
	case strings.HasPrefix(reason, "startup"):
		o.Cause = "startup"
	case strings.HasPrefix(reason, "resume"):
		o.Cause = "resume"
	case strings.HasPrefix(reason, "manual drain"):
		o.Cause = "drained"
	case strings.HasPrefix(reason, "crash"), strings.HasPrefix(reason, "kill"):
		return fault("crash")
	case strings.HasPrefix(reason, "GPU"), strings.HasPrefix(reason, "device"):
		return fault("gpu_fault")
	case strings.HasPrefix(reason, "telemetry"):
		return fault("no_gpu_readings")
	default:
		o.Cause = "recovering"
	}
	return o
}

// cooldownAfter says what a cooldown follows, from the rule that last made
// AI yield.
func cooldownAfter(rule string) string {
	switch {
	case strings.HasPrefix(rule, "gaming."), rule == policy.RuleFullscreenGPU, rule == policy.RuleGameProcess, rule == policy.RuleAmbiguous:
		return "game"
	case strings.HasPrefix(rule, "contention."), rule == policy.RuleSoftContention:
		return "gpu_busy"
	case rule == policy.RuleTemperature:
		return "heat"
	}
	return "other"
}

// evidenceThreshold is the numeric threshold of the first evidence named
// name, or 0 when absent or not a number.
func evidenceThreshold(d policy.Decision, name string) int {
	for _, e := range d.Evidence {
		if e.Name == name {
			n, _ := strconv.Atoi(e.Threshold)
			return n
		}
	}
	return 0
}

// evidenceValue is the first evidence value whose name is one of names.
func evidenceValue(d policy.Decision, names ...string) string {
	for _, e := range d.Evidence {
		for _, n := range names {
			if e.Name == n {
				return e.Value
			}
		}
	}
	return ""
}
