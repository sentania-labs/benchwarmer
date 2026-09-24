package policy

import (
	"fmt"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/humantime"
	"github.com/sentania-labs/benchwarmer/internal/schedule"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Rule identifiers. Stable strings: they appear in events, metrics and the UI.
const (
	RuleShutdown         = "safety.shutdown"
	RuleSuspend          = "safety.suspend"
	RuleDeviceLost       = "safety.device_lost"
	RuleTemperature      = "safety.temperature"
	RuleTelemetryLost    = "safety.telemetry_lost"
	RulePause            = "manual.pause"
	RuleCriticalVRAMFree = "contention.critical_vram_free"
	RuleCriticalVRAMExt  = "contention.critical_external_vram"
	RuleCriticalUtil     = "contention.critical_external_util"
	RuleGameConfirmed    = "gaming.confirmed"
	RuleLauncherChildGPU = "gaming.launcher_child"
	RuleFullscreenGPU    = "interactive.fullscreen_gpu"
	RuleGameProcess      = "profile.game_process"
	RuleAmbiguous        = "profile.ambiguous_interactive"
	RuleSoftContention   = "profile.soft_contention"
	RuleRecovery         = "eligibility.recovery"
	RuleSuppressed       = "eligibility.suppressed"
	RuleCooldown         = "eligibility.cooldown"
	RuleNoTelemetry      = "eligibility.telemetry_unavailable"
	RuleNoProcessList    = "eligibility.process_list_unavailable"
	RuleVRAMInsufficient = "eligibility.vram_insufficient"
	RuleRun              = "profile.run"
	RuleAIPriorityRun    = "mode.ai_priority"
)

type match struct {
	action    Action
	rule      string
	tier      Tier
	severity  Severity
	reason    string
	evidence  []Evidence
	competing bool
	preempt   bool // counts toward anti-thrashing
	idle      state.State
}

// Evaluate returns the decision for s under c. It is deterministic: the same
// inputs always give the same decision.
func Evaluate(s Snapshot, c config.Config) Decision {
	e := newEval(s, c)
	matches := e.yieldRules()

	var win match
	if len(matches) > 0 {
		win = matches[0]
	} else if e.running {
		win = e.runMatch()
	} else if g, ok := e.eligibility(); ok {
		win = g
	} else {
		win = e.runMatch()
	}

	d := Decision{
		Action:        win.action,
		Rule:          win.rule,
		Tier:          win.tier,
		Severity:      win.severity,
		Reason:        win.reason,
		Evidence:      win.evidence,
		Profile:       e.profileName,
		ProfileSource: e.profileSource,
		Mode:          e.mode,
		Competing:     win.competing,
		IdleState:     win.idle,
		NextLoadAt:    e.nextLoadAt(win.competing),
	}
	for _, m := range matches {
		if m.rule != win.rule {
			d.AlsoMatched = append(d.AlsoMatched, m.rule)
		}
	}
	if !e.running && d.Action.Yields() {
		// Nothing to stop: a yield rule while idle means "do not load".
		d.Action = ActionHold
	}
	if d.Action == ActionDrain {
		d.Grace = e.grace()
	}
	if e.running && d.Action.Yields() && win.preempt {
		d.CountsAsPreemption = true
		d.Suppress = e.wouldSuppress()
	}
	if d.IdleState == "" {
		d.IdleState = state.Stopped
	}
	// While suppressed, a competing workload that is still present does not
	// hide the suppression: the idle state stays SUPPRESSED so the UI shows
	// why reload is blocked and until when.
	if end := e.suppressEnd(); !end.IsZero() && s.Now.Before(end) && d.IdleState == state.Cooldown {
		d.IdleState = state.Suppressed
	}
	return d
}

type eval struct {
	s             Snapshot
	c             config.Config
	p             config.Profile
	profileName   string
	profileSource string
	profileTier   Tier
	mode          Mode
	running       bool
	softUtil      float64
	softVRAM      int
}

func newEval(s Snapshot, c config.Config) *eval {
	e := &eval{s: s, c: c, mode: s.Mode.Mode, running: s.Runtime.State.RuntimeRunning()}
	if e.mode == "" {
		e.mode = ModeAuto
	}
	if e.mode == ModeAIPriority {
		e.profileName, e.profileSource, e.profileTier = c.AIPriorityProfile, "ai_priority", TierAIPriority
	} else {
		e.profileName, e.profileSource = schedule.Active(c, s.Now)
		e.profileTier = TierDefault
		if strings.HasPrefix(e.profileSource, "schedule:") {
			e.profileTier = TierSchedule
		}
	}
	p, ok := c.Profiles[e.profileName]
	if !ok {
		p = c.Profiles[c.DefaultProfile]
	}
	e.p = p
	e.softUtil, e.softVRAM = p.ExternalUtilSoftPct, p.ExternalVRAMSoftMiB
	if s.GPU.Confidence == ConfidenceDegraded {
		// Without attribution, react earlier (ADR 0003).
		f := 1 - float64(c.Telemetry.DegradedMarginPct)/100
		e.softUtil *= f
		e.softVRAM = int(float64(e.softVRAM) * f)
	}
	return e
}

// clock renders an instant for people in the configured zone.
func (e *eval) clock(t time.Time) string { return humantime.Clock(t, e.s.Now, e.c.Timezone) }

func ev(name string, value any, threshold any, source string) Evidence {
	x := Evidence{Name: name, Value: fmt.Sprint(value), Source: source}
	if threshold != nil {
		x.Threshold = fmt.Sprint(threshold)
	}
	return x
}

func dur(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }

func (e *eval) telemetryUsable() bool { return e.s.GPU.Confidence != ConfidenceNone }

// yieldRules returns every matching rule that makes the worker yield or hold
// for a reason other than load eligibility, in precedence order.
func (e *eval) yieldRules() []match {
	s, c, g := e.s, e.c, e.s.GPU
	var out []match
	add := func(m match) { out = append(out, m) }

	// Tier 1: hard safety.
	if s.Power.ShuttingDown {
		add(match{action: ActionPreempt, rule: RuleShutdown, tier: TierSafety, severity: SeverityNotice,
			reason: "Service is stopping", idle: state.Stopped})
	}
	if s.Power.Suspending {
		add(match{action: ActionPreempt, rule: RuleSuspend, tier: TierSafety, severity: SeverityNotice,
			reason: "System is going to sleep", idle: state.Stopped})
	}
	if s.Power.DeviceLost {
		add(match{action: ActionPreempt, rule: RuleDeviceLost, tier: TierSafety, severity: SeverityCritical,
			reason: "GPU device lost or reset", idle: state.Error})
	}
	if t := g.TemperatureC; t != nil {
		if *t >= c.Safety.GPUTempCriticalC {
			add(match{action: ActionPreempt, rule: RuleTemperature, tier: TierSafety, severity: SeverityCritical,
				reason:   fmt.Sprintf("GPU temperature %.0f C is at or above the %.0f C limit", *t, c.Safety.GPUTempCriticalC),
				evidence: []Evidence{ev("gpu_temperature_c", fmt.Sprintf("%.1f", *t), c.Safety.GPUTempCriticalC, "d3dkmt")}, idle: state.Cooldown})
		} else if !e.running && *t >= c.Safety.GPUTempResumeC {
			add(match{action: ActionHold, rule: RuleTemperature, tier: TierSafety, severity: SeverityWarning,
				reason:   fmt.Sprintf("GPU temperature %.0f C must fall below %.0f C before loading", *t, c.Safety.GPUTempResumeC),
				evidence: []Evidence{ev("gpu_temperature_c", fmt.Sprintf("%.1f", *t), c.Safety.GPUTempResumeC, "d3dkmt")}, idle: state.Cooldown})
		}
	}
	if g.Confidence == ConfidenceNone && g.NoneFor >= c.Telemetry.LossGrace.D() {
		add(match{action: ActionPreempt, rule: RuleTelemetryLost, tier: TierSafety, severity: SeverityWarning,
			reason:   "GPU telemetry unavailable; releasing the GPU until it recovers",
			evidence: []Evidence{ev("telemetry_unavailable_for", dur(g.NoneFor), dur(c.Telemetry.LossGrace.D()), "telemetry")}, idle: state.Error})
	}

	// Tier 2: manual pause.
	if e.mode == ModePause {
		until := "indefinitely"
		if s.Mode.Until != nil {
			until = "until " + e.clock(*s.Mode.Until)
		}
		add(match{action: ActionDrain, rule: RulePause, tier: TierPause, severity: SeverityNotice,
			reason: "Paused manually " + until, evidence: []Evidence{ev("mode", e.mode, nil, s.Mode.SetBy)}, idle: state.Disabled})
	}

	// Tier 3: critical external contention.
	if e.telemetryUsable() {
		k := c.Contention
		if e.running && g.VRAMTotalMiB > 0 && g.VRAMFreeMiB < k.VRAMFreeCriticalMiB && g.ExternalVRAMGrowthMiB > 0 {
			add(match{action: ActionPreempt, rule: RuleCriticalVRAMFree, tier: TierCritical, severity: SeverityCritical,
				reason: fmt.Sprintf("Free VRAM %d MiB is below %d MiB and another process is allocating", g.VRAMFreeMiB, k.VRAMFreeCriticalMiB),
				evidence: []Evidence{
					ev("vram_free_mib", g.VRAMFreeMiB, k.VRAMFreeCriticalMiB, "pdh"),
					ev("external_vram_growth_mib", g.ExternalVRAMGrowthMiB, "> 0", "pdh"),
				}, competing: true, preempt: true, idle: state.Cooldown})
		}
		if g.ExternalVRAMMiB >= k.ExternalVRAMCriticalMiB {
			add(match{action: ActionPreempt, rule: RuleCriticalVRAMExt, tier: TierCritical, severity: SeverityCritical,
				reason:    fmt.Sprintf("Other processes are using %d MiB of VRAM (limit %d MiB)", g.ExternalVRAMMiB, k.ExternalVRAMCriticalMiB),
				evidence:  append([]Evidence{ev("external_vram_mib", g.ExternalVRAMMiB, k.ExternalVRAMCriticalMiB, "pdh")}, e.topApps()...),
				competing: true, preempt: true, idle: state.Cooldown})
		}
		if g.ExternalUtilTrusted && k.ExternalUtilCriticalPct > 0 && g.AboveCriticalFor > 0 && g.AboveCriticalFor >= k.CriticalWindow.D() {
			add(match{action: ActionPreempt, rule: RuleCriticalUtil, tier: TierCritical, severity: SeverityCritical,
				reason: fmt.Sprintf("External GPU use %.0f%% has exceeded %.0f%% for %s", g.ExternalUtilPct, k.ExternalUtilCriticalPct, dur(g.AboveCriticalFor)),
				evidence: append([]Evidence{
					ev("external_util_pct", fmt.Sprintf("%.0f", g.ExternalUtilPct), k.ExternalUtilCriticalPct, "pdh"),
					ev("above_critical_for", dur(g.AboveCriticalFor), dur(k.CriticalWindow.D()), "pdh"),
				}, e.topApps()...), competing: true, preempt: true, idle: state.Cooldown})
		}
	}

	// Tier 4: confirmed gaming or strong interactive demand.
	for _, a := range s.Apps.Games {
		why := e.gameConfirmation(a)
		if why == "" {
			continue
		}
		add(match{action: ActionDrain, rule: RuleGameConfirmed, tier: TierGaming, severity: SeverityWarning,
			reason:   fmt.Sprintf("Game %s is running (%s)", a.Name, why),
			evidence: appEvidence(a), competing: true, preempt: true, idle: state.Cooldown})
		break
	}
	for _, a := range s.Apps.LauncherChildren {
		if a.Foreground || a.Fullscreen || a.VRAMMiB >= e.p.GameConfirmVRAMMiB {
			add(match{action: ActionDrain, rule: RuleLauncherChildGPU, tier: TierGaming, severity: SeverityWarning,
				reason:   fmt.Sprintf("%s was started by launcher %s and is using the GPU", a.Name, a.ParentName),
				evidence: appEvidence(a), competing: true, preempt: true, idle: state.Cooldown})
			break
		}
	}
	ss := s.Session
	if ss.Known && ss.Fullscreen && ss.ForegroundClass == "" && g.ExternalUtilTrusted && g.ExternalUtilPct >= e.softUtil {
		add(match{action: ActionDrain, rule: RuleFullscreenGPU, tier: TierGaming, severity: SeverityWarning,
			reason: fmt.Sprintf("Unclassified fullscreen app %s with external GPU use %.0f%%", ss.ForegroundName, g.ExternalUtilPct),
			evidence: []Evidence{
				ev("foreground", ss.ForegroundName, nil, "agent"),
				ev("fullscreen", true, nil, "agent"),
				ev("external_util_pct", fmt.Sprintf("%.0f", g.ExternalUtilPct), e.softUtil, "pdh"),
			}, competing: true, preempt: true, idle: state.Cooldown})
	}

	// Tiers 5-7: the active profile's tunable rules. Under AI Priority the
	// AI profile's parameters apply (by default, all drain_on_* are off).
	pt := e.profileTier
	if e.p.DrainOnGameProcess && len(s.Apps.Games) > 0 {
		a := s.Apps.Games[0]
		add(match{action: ActionDrain, rule: RuleGameProcess, tier: pt, severity: SeverityNotice,
			reason:   fmt.Sprintf("Game %s started (early warning)", a.Name),
			evidence: appEvidence(a), competing: true, preempt: true, idle: state.Cooldown})
	}
	if e.p.DrainOnAmbiguous {
		if ss.Known && ss.Fullscreen && ss.ForegroundClass == "" {
			add(match{action: ActionDrain, rule: RuleAmbiguous, tier: pt, severity: SeverityNotice,
				reason:    fmt.Sprintf("Unclassified fullscreen app %s in the foreground", ss.ForegroundName),
				evidence:  []Evidence{ev("foreground", ss.ForegroundName, nil, "agent"), ev("fullscreen", true, nil, "agent")},
				competing: true, preempt: true, idle: state.Cooldown})
		} else if len(s.Apps.LauncherChildren) > 0 {
			a := s.Apps.LauncherChildren[0]
			add(match{action: ActionDrain, rule: RuleAmbiguous, tier: pt, severity: SeverityNotice,
				reason:   fmt.Sprintf("%s started by launcher %s is using the GPU", a.Name, a.ParentName),
				evidence: appEvidence(a), competing: true, preempt: true, idle: state.Cooldown})
		}
	}
	if e.p.DrainOnSoftContention && e.telemetryUsable() {
		if m, ok := e.softContention(); ok {
			m.tier = pt
			add(m)
		}
	}
	return out
}

// gameConfirmation says why a running game counts as confirmed, or "".
func (e *eval) gameConfirmation(a AppMatch) string {
	switch {
	case a.Foreground && e.s.Session.Fullscreen:
		return "fullscreen in the foreground"
	case a.Foreground:
		return "in the foreground"
	case a.Fullscreen:
		return "fullscreen"
	case a.GPUUtilPct >= e.p.GameConfirmUtilPct:
		return fmt.Sprintf("GPU use %.0f%%", a.GPUUtilPct)
	case a.VRAMMiB >= e.p.GameConfirmVRAMMiB:
		return fmt.Sprintf("%d MiB VRAM", a.VRAMMiB)
	case e.s.GPU.Confidence != ConfidenceHigh:
		// Without per-process attribution a game process is taken at its
		// word (ADR 0003 fallback).
		return "GPU attribution unavailable"
	}
	return ""
}

func (e *eval) softContention() (match, bool) {
	g, p := e.s.GPU, e.p
	var evd []Evidence
	var why []string
	if g.ExternalUtilTrusted && g.AboveSoftFor > 0 && g.AboveSoftFor >= p.SoftWindow.D() {
		why = append(why, fmt.Sprintf("external GPU use %.0f%% above %.0f%% for %s", g.ExternalUtilPct, e.softUtil, dur(g.AboveSoftFor)))
		evd = append(evd, ev("external_util_pct", fmt.Sprintf("%.0f", g.ExternalUtilPct), fmt.Sprintf("%.0f", e.softUtil), "pdh"),
			ev("above_soft_for", dur(g.AboveSoftFor), dur(p.SoftWindow.D()), "pdh"))
	} else if !e.running && g.ExternalUtilTrusted && g.BelowSoftFor < e.c.Contention.ClearWindow.D() && e.utilWasCompeting() {
		// Hysteresis while idle: after utilization-driven contention,
		// external load must stay below the soft threshold for the clear
		// window before it counts as gone (loading screens dip).
		why = append(why, fmt.Sprintf("external GPU use below %.0f%% for only %s", e.softUtil, dur(g.BelowSoftFor)))
		evd = append(evd, ev("below_soft_for", dur(g.BelowSoftFor), dur(e.c.Contention.ClearWindow.D()), "pdh"))
	}
	if g.ExternalVRAMMiB >= e.softVRAM {
		why = append(why, fmt.Sprintf("external VRAM %d MiB at or above %d MiB", g.ExternalVRAMMiB, e.softVRAM))
		evd = append(evd, ev("external_vram_mib", g.ExternalVRAMMiB, e.softVRAM, "pdh"))
	}
	if len(why) == 0 {
		return match{}, false
	}
	if e.s.GPU.Confidence == ConfidenceDegraded {
		evd = append(evd, ev("telemetry_confidence", "degraded", nil, "telemetry"))
	}
	return match{action: ActionDrain, rule: RuleSoftContention, severity: SeverityNotice,
		reason:   "Competing GPU workload: " + strings.Join(why, "; "),
		evidence: append(evd, e.topApps()...), competing: true, preempt: true, idle: state.Cooldown}, true
}

// utilWasCompeting reports whether the last competing workload was detected
// by external GPU utilization, so the clear-window hysteresis applies. It
// does not apply at startup or after process-only triggers.
func (e *eval) utilWasCompeting() bool {
	switch e.s.Timers.LastCompetingRule {
	case RuleSoftContention, RuleCriticalUtil, RuleFullscreenGPU:
		return true
	}
	return false
}

func appEvidence(a AppMatch) []Evidence {
	x := []Evidence{ev("process", a.Name, nil, "process-list"), ev("class", a.Class, nil, a.Rule)}
	if a.Path != "" {
		x = append(x, ev("path", a.Path, nil, "process-list"))
	}
	if a.GPUUtilPct > 0 {
		x = append(x, ev("process_gpu_util_pct", fmt.Sprintf("%.0f", a.GPUUtilPct), nil, "pdh"))
	}
	if a.VRAMMiB > 0 {
		x = append(x, ev("process_vram_mib", a.VRAMMiB, nil, "pdh"))
	}
	if a.Foreground {
		x = append(x, ev("foreground", true, nil, "agent"))
	}
	return x
}

// topApps names the heaviest known external GPU users as evidence.
func (e *eval) topApps() []Evidence {
	var best *AppMatch
	for _, list := range [][]AppMatch{e.s.Apps.Games, e.s.Apps.LauncherChildren, e.s.Apps.Ordinary} {
		for i := range list {
			a := &list[i]
			if best == nil || a.GPUUtilPct > best.GPUUtilPct || (a.GPUUtilPct == best.GPUUtilPct && a.VRAMMiB > best.VRAMMiB) {
				best = a
			}
		}
	}
	if best == nil || (best.GPUUtilPct == 0 && best.VRAMMiB == 0) {
		return nil
	}
	return []Evidence{ev("top_external_process", fmt.Sprintf("%s (%.0f%%, %d MiB)", best.Name, best.GPUUtilPct, best.VRAMMiB), nil, "pdh")}
}

// eligibility gates a load when no yield rule matched and the runtime is
// idle. It returns the first failing gate.
func (e *eval) eligibility() (match, bool) {
	s, c := e.s, e.c
	now := s.Now
	t := s.Timers
	if !t.RecoveryUntil.IsZero() && now.Before(t.RecoveryUntil) {
		idle := state.Cooldown
		switch {
		case strings.HasPrefix(t.RecoveryReason, "crash"), strings.HasPrefix(t.RecoveryReason, "device"),
			strings.HasPrefix(t.RecoveryReason, "telemetry"), strings.HasPrefix(t.RecoveryReason, "kill"),
			strings.HasPrefix(t.RecoveryReason, "GPU"):
			idle = state.Error
		}
		return match{action: ActionHold, rule: RuleRecovery, tier: TierEligibility, severity: SeverityInfo,
			reason:   fmt.Sprintf("Recovery wait (%s) until %s", t.RecoveryReason, e.clock(t.RecoveryUntil)),
			evidence: []Evidence{ev("recovery_reason", t.RecoveryReason, nil, "timer"), ev("recovery_until", e.clock(t.RecoveryUntil), nil, "timer")},
			idle:     idle}, true
	}
	if end := e.suppressEnd(); !end.IsZero() && now.Before(end) {
		return match{action: ActionHold, rule: RuleSuppressed, tier: TierEligibility, severity: SeverityNotice,
			reason: fmt.Sprintf("Reload suppressed after %d preemptions within %s, until %s",
				c.AntiThrash.MaxPreemptions, c.AntiThrash.Window.D(), e.clock(end)),
			evidence: []Evidence{ev("suppressed_at", e.clock(t.SuppressedAt), nil, "timer"), ev("suppressed_until", e.clock(end), nil, "timer")},
			idle:     state.Suppressed}, true
	}
	if end := e.cooldownEnd(); !end.IsZero() && now.Before(end) {
		return match{action: ActionHold, rule: RuleCooldown, tier: TierEligibility, severity: SeverityInfo,
			reason: fmt.Sprintf("Cooling down after %s; %s cooldown (%s profile) ends %s",
				t.LastCompetingRule, e.p.Cooldown.D(), e.profileName, e.clock(end)),
			evidence: []Evidence{ev("last_competing_at", e.clock(t.LastCompetingAt), nil, "timer"),
				ev("last_competing_rule", t.LastCompetingRule, nil, "timer"), ev("cooldown", e.p.Cooldown.D(), nil, "profile:"+e.profileName)},
			idle: state.Cooldown}, true
	}
	if !e.telemetryUsable() {
		return match{action: ActionHold, rule: RuleNoTelemetry, tier: TierEligibility, severity: SeverityWarning,
			reason: "Waiting for GPU telemetry before loading", idle: state.Stopped}, true
	}
	if !s.Apps.ProcessListOK {
		return match{action: ActionHold, rule: RuleNoProcessList, tier: TierEligibility, severity: SeverityWarning,
			reason: "Process list unavailable; cannot rule out a running game", idle: state.Stopped}, true
	}
	if need := c.Runtime.RequiredFreeVRAMMiB; need > 0 && s.GPU.VRAMFreeMiB < need {
		return match{action: ActionHold, rule: RuleVRAMInsufficient, tier: TierEligibility, severity: SeverityInfo,
			reason:   fmt.Sprintf("Free VRAM %d MiB is below the %d MiB the model needs", s.GPU.VRAMFreeMiB, need),
			evidence: append([]Evidence{ev("vram_free_mib", s.GPU.VRAMFreeMiB, need, "pdh")}, e.topApps()...),
			idle:     state.Stopped}, true
	}
	return match{}, false
}

func (e *eval) runMatch() match {
	m := match{action: ActionRun, rule: RuleRun, tier: e.profileTier, severity: SeverityInfo,
		reason: fmt.Sprintf("GPU available (%s profile)", e.profileName), idle: state.Stopped}
	if e.mode == ModeAIPriority {
		m.rule = RuleAIPriorityRun
		m.reason = "AI Priority: GPU available under the permissive profile"
		if e.s.Mode.Until != nil {
			m.reason += " until " + e.clock(*e.s.Mode.Until)
		}
	}
	return m
}

func (e *eval) cooldownEnd() time.Time {
	if e.s.Timers.LastCompetingAt.IsZero() {
		return time.Time{}
	}
	return e.s.Timers.LastCompetingAt.Add(e.p.Cooldown.D())
}

func (e *eval) suppressEnd() time.Time {
	if e.s.Timers.SuppressedAt.IsZero() {
		return time.Time{}
	}
	return e.s.Timers.SuppressedAt.Add(e.c.AntiThrash.SuppressFor.D())
}

// nextLoadAt is the latest active timer; zero when no timer blocks loading.
// While a competing workload is still present the cooldown has not started
// counting, so it gives no bound: only recovery and suppression deadlines do.
func (e *eval) nextLoadAt(competing bool) time.Time {
	var next time.Time
	timers := []time.Time{e.s.Timers.RecoveryUntil, e.suppressEnd()}
	if !competing {
		timers = append(timers, e.cooldownEnd())
	}
	for _, t := range timers {
		if t.After(e.s.Now) && t.After(next) {
			next = t
		}
	}
	return next
}

// grace is the drain allowance, shortened while soft pressure is present.
func (e *eval) grace() time.Duration {
	g := e.s.GPU
	pressure := e.telemetryUsable() && ((g.ExternalUtilTrusted && g.ExternalUtilPct >= e.softUtil) || g.ExternalVRAMMiB >= e.softVRAM)
	if pressure {
		return e.p.GraceUnderPressure.D()
	}
	return e.p.Grace.D()
}

// wouldSuppress reports whether one more preemption now reaches the limit.
func (e *eval) wouldSuppress() bool {
	a := e.c.AntiThrash
	n := 1
	for _, t := range e.s.Timers.Preemptions {
		if !t.After(e.s.Now) && e.s.Now.Sub(t) <= a.Window.D() {
			n++
		}
	}
	return n >= a.MaxPreemptions
}
