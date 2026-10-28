package api

import (
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

func TestDeriveOwner(t *testing.T) {
	now := time.Date(2026, 9, 24, 19, 46, 0, 0, time.UTC)
	at := func(m int) *time.Time { x := now.Add(time.Duration(m) * time.Minute); return &x }
	rule := func(r string, ev ...policy.Evidence) policy.Decision {
		return policy.Decision{Rule: r, Evidence: ev}
	}
	ev := func(n, v string) policy.Evidence { return policy.Evidence{Name: n, Value: v} }
	cooldown := 5 * time.Minute

	cases := []struct {
		name     string
		s        Status
		kind     string
		severity string
		cause    string
		check    func(t *testing.T, o OwnerStatus)
	}{
		{"ready", Status{State: state.Ready, Decision: rule(policy.RuleRun)}, OwnerAvailable, OwnerSeverityOK, "ready", nil},
		{"busy", Status{State: state.Busy}, OwnerAvailable, OwnerSeverityOK, "busy", nil},
		{"loading", Status{State: state.Loading}, OwnerStarting, OwnerSeverityOK, "loading", nil},
		{"about to load", Status{State: state.Stopped, Decision: rule(policy.RuleRun)}, OwnerWaiting, OwnerSeverityWait, "starting", nil},
		{"game", Status{State: state.Cooldown, Decision: rule(policy.RuleGameConfirmed, ev("process", "game.exe"))},
			OwnerWaiting, OwnerSeverityWait, "game", func(t *testing.T, o OwnerStatus) {
				if o.App != "game.exe" || o.ResumeDelaySeconds != 300 || o.ReturnAt != nil {
					t.Errorf("game wait: %+v", o)
				}
			}},
		{"game, stepping aside", Status{State: state.Draining, Decision: rule(policy.RuleGameConfirmed)},
			OwnerWaiting, OwnerSeverityWait, "game", func(t *testing.T, o OwnerStatus) {
				if !o.Yielding {
					t.Error("draining must be yielding")
				}
			}},
		{"heavy gpu use", Status{State: state.Cooldown, Decision: rule(policy.RuleCriticalVRAMExt)}, OwnerWaiting, OwnerSeverityWait, "gpu_busy", nil},
		{"cooldown after a game", Status{State: state.Cooldown, Timers: TimerStatus{NextLoadAt: at(5)},
			Decision: rule(policy.RuleCooldown, ev("last_competing_rule", policy.RuleGameConfirmed))},
			OwnerWaiting, OwnerSeverityWait, "cooldown", func(t *testing.T, o OwnerStatus) {
				if o.After != "game" || o.ReturnAt == nil || !o.ReturnAt.Equal(*at(5)) {
					t.Errorf("cooldown: %+v", o)
				}
			}},
		{"cooldown without a known end promises nothing", Status{State: state.Cooldown, Decision: rule(policy.RuleCooldown)},
			OwnerWaiting, OwnerSeverityWait, "cooldown", func(t *testing.T, o OwnerStatus) {
				if o.ReturnAt != nil || o.After != "other" {
					t.Errorf("cooldown: %+v", o)
				}
			}},
		{"startup recovery is a normal wait", Status{State: state.Cooldown, Timers: TimerStatus{NextLoadAt: at(2)},
			Decision: rule(policy.RuleRecovery, ev("recovery_reason", "startup"))}, OwnerWaiting, OwnerSeverityWait, "startup", nil},
		{"resume recovery", Status{State: state.Cooldown, Decision: rule(policy.RuleRecovery, ev("recovery_reason", "resume"))}, OwnerWaiting, OwnerSeverityWait, "resume", nil},
		{"crash backoff is a problem", Status{State: state.Error, Timers: TimerStatus{NextLoadAt: at(16)},
			Decision: rule(policy.RuleRecovery, ev("recovery_reason", "crash backoff (attempt 4)"))},
			OwnerProblem, OwnerSeverityProblem, "crash", func(t *testing.T, o OwnerStatus) {
				if o.ReturnAt == nil {
					t.Error("a retry time is known")
				}
			}},
		{"readings back, waiting out the delay, is a normal wait", Status{State: state.Cooldown, Timers: TimerStatus{NextLoadAt: at(3)},
			Decision: rule(policy.RuleRecovery, ev("recovery_reason", "telemetry recovery (loss 2)"))},
			OwnerWaiting, OwnerSeverityWait, "telemetry_recovered", func(t *testing.T, o OwnerStatus) {
				if o.ReturnAt == nil {
					t.Error("a retry time is known")
				}
			}},
		{"other telemetry recovery reasons stay a problem", Status{State: state.Error, Decision: rule(policy.RuleRecovery, ev("recovery_reason", "telemetry lost"))}, OwnerProblem, OwnerSeverityProblem, "no_gpu_readings", nil},
		{"driver reset is a problem", Status{State: state.Error, Decision: rule(policy.RuleRecovery, ev("recovery_reason", "GPU driver reset (1)"))}, OwnerProblem, OwnerSeverityProblem, "gpu_fault", nil},
		{"unrecognised recovery stays a wait", Status{State: state.Cooldown, Decision: rule(policy.RuleRecovery, ev("recovery_reason", "something new"))}, OwnerWaiting, OwnerSeverityWait, "recovering", nil},
		{"anti-thrash", Status{State: state.Suppressed, Timers: TimerStatus{NextLoadAt: at(20)}, Decision: rule(policy.RuleSuppressed)}, OwnerWaiting, OwnerSeverityWait, "suppressed", nil},
		{"not enough free memory", Status{State: state.Stopped, Decision: rule(policy.RuleVRAMInsufficient)}, OwnerWaiting, OwnerSeverityWait, "low_vram", nil},
		{"required memory beyond the card is a setting problem", Status{State: state.Stopped, GPU: GPUStatus{VRAMTotalMiB: 12288},
			Decision: rule(policy.RuleVRAMInsufficient, policy.Evidence{Name: "vram_free_mib", Value: "9000", Threshold: "13312"})}, OwnerProblem, OwnerSeverityProblem, "vram_too_small", nil},
		{"memory that fits the card is still a wait", Status{State: state.Stopped, GPU: GPUStatus{VRAMTotalMiB: 24576},
			Decision: rule(policy.RuleVRAMInsufficient, policy.Evidence{Name: "vram_free_mib", Value: "9000", Threshold: "13312"})}, OwnerWaiting, OwnerSeverityWait, "low_vram", nil},
		{"too hot, idle", Status{State: state.Cooldown, Decision: rule(policy.RuleTemperature)}, OwnerProblem, OwnerSeverityProblem, "too_hot", nil},
		{"too hot, stopping the model", Status{State: state.Preempting, Decision: rule(policy.RuleTemperature)}, OwnerProblem, OwnerSeverityProblem, "too_hot", nil},
		{"setup", Status{State: state.Stopped, Decision: policy.Decision{Rule: policy.RuleSetupRequired, Reason: "Setup required: no model file"}},
			OwnerProblem, OwnerSeverityProblem, "setup", func(t *testing.T, o OwnerStatus) {
				if o.Detail != "no model file" {
					t.Errorf("detail %q", o.Detail)
				}
			}},
		{"no gpu readings", Status{State: state.Error, Decision: rule(policy.RuleTelemetryLost)}, OwnerProblem, OwnerSeverityProblem, "no_gpu_readings", nil},
		{"device lost", Status{State: state.Error, Decision: rule(policy.RuleDeviceLost)}, OwnerProblem, OwnerSeverityProblem, "gpu_fault", nil},
		{"going to sleep", Status{State: state.Preempting, Decision: rule(policy.RuleSuspend)}, OwnerWaiting, OwnerSeverityWait, "sleeping", nil},
		{"unknown rule stays unknown", Status{State: state.Stopped, Decision: rule("future.rule")}, OwnerUnknown, OwnerSeverityUnknown, "unknown", nil},
		{"empty status stays unknown", Status{}, OwnerUnknown, OwnerSeverityUnknown, "unknown", nil},
		{"error state with a run decision is a problem", Status{State: state.Error, Decision: rule(policy.RuleRun)}, OwnerProblem, OwnerSeverityProblem, "crash", nil},
		{"running on a fallback config needs attention", Status{State: state.Ready, ConfigSource: "last_good"},
			OwnerAvailable, OwnerSeverityAttention, "ready", func(t *testing.T, o OwnerStatus) {
				if !o.ConfigFallback {
					t.Error("fallback flag missing")
				}
			}},
		{"a fallback config does not hide a normal wait", Status{State: state.Cooldown, ConfigSource: "default", Decision: rule(policy.RuleCooldown)},
			OwnerWaiting, OwnerSeverityWait, "cooldown", func(t *testing.T, o OwnerStatus) {
				if !o.ConfigFallback {
					t.Error("fallback flag missing")
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := DeriveOwner(c.s, cooldown)
			if o.Kind != c.kind || o.Severity != c.severity || o.Cause != c.cause {
				t.Fatalf("got %s/%s/%s, want %s/%s/%s", o.Kind, o.Severity, o.Cause, c.kind, c.severity, c.cause)
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}

func TestDeriveOwnerPause(t *testing.T) {
	now := time.Date(2026, 9, 24, 19, 46, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	pause := policy.Decision{Rule: policy.RulePause}

	// A pause promises the return time the policy computed, whatever it is.
	o := DeriveOwner(Status{State: state.Disabled, Decision: pause, Mode: ModeStatus{Mode: policy.ModePause, Until: &until}, Timers: TimerStatus{NextLoadAt: &until}}, 0)
	if o.Kind != OwnerPaused || o.Severity != OwnerSeverityPaused || o.ReturnAt == nil || !o.ReturnAt.Equal(until) {
		t.Errorf("timed pause: %+v", o)
	}
	// No computed time means no promise: an open-ended pause has none.
	o = DeriveOwner(Status{State: state.Disabled, Decision: pause, Mode: ModeStatus{Mode: policy.ModePause}}, 0)
	if o.Kind != OwnerPaused || o.ReturnAt != nil {
		t.Errorf("open-ended pause: %+v", o)
	}
}
