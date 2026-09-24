package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/proxy"
	"github.com/sentania-labs/benchwarmer/internal/runtime"
	"github.com/sentania-labs/benchwarmer/internal/schedule"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

func (c *Controller) step(now time.Time, collect bool) {
	c.absorbAsync(now)
	c.checkGPUResets(now)
	c.expireMode(now)
	if collect || c.lastEvaluated.IsZero() {
		c.observe(now)
	}
	c.trackTelemetryHealth(now)

	snap := c.snapshot(now)
	d := policy.Evaluate(snap, c.cfg)
	d = c.applyManual(d)
	d = c.applyZombie(d)
	d = c.applyLoadBlocked(d)
	if d.Competing {
		if c.st == state.Cooldown && c.decision.Rule == policy.RuleCooldown {
			// A competing workload reappeared while the cooldown was
			// counting down: the cooldown restarts from its disappearance.
			c.emit(events.FromDecision(events.CooldownReset, now, d, c.gpu))
		}
		c.timers.LastCompetingAt, c.timers.LastCompetingRule = now, d.Rule
	}
	if d.Rule != c.decision.Rule && !c.st.RuntimeRunning() && d.IdleState == c.st {
		// Same idle state, new reason (e.g. game -> cooldown): worth one event.
		c.emit(events.FromDecision(events.DecisionChanged, now, d, c.gpu))
	}
	c.decision = d
	c.lastEvaluated = now

	c.act(now, d)
	c.checkVRAMRelease(now)
	c.checkCrashReset(now)
	c.publish(now)
	c.persist()
}

// absorbAsync handles results from load and stop goroutines and unexpected
// runtime exits.
func (c *Controller) absorbAsync(now time.Time) {
	select {
	case r := <-c.readyCh:
		c.onLoadResult(now, r)
	default:
	}
	for more := true; more; {
		select {
		case r := <-c.stopCh:
			if r.cleanup {
				c.onZombieStopped(now, r)
			} else {
				c.onStopped(now, r)
			}
		default:
			more = false
		}
	}
	c.retryZombie(now)
	if c.inst != nil && c.st != state.Preempting {
		select {
		case <-c.inst.Exited():
			c.onCrash(now)
		default:
		}
	}
}

func (c *Controller) expireMode(now time.Time) {
	if c.mode.Until != nil && !now.Before(*c.mode.Until) {
		prev := c.mode.Mode
		c.mode, c.untilReboot = policy.ModeFacts{Mode: policy.ModeAuto}, false
		c.emit(events.Event{Time: now, Type: events.ModeExpired, Mode: policy.ModeAuto,
			Message: fmt.Sprintf("%s expired; back to Auto", modeName(prev)), Data: map[string]any{"previous": prev}})
	}
}

func (c *Controller) observe(now time.Time) {
	in := FactInput{Config: c.cfg, FootprintMiB: c.footprint}
	if c.d.Telemetry != nil {
		s := c.d.Telemetry.Collect()
		c.sample = s
		in.Sample = &s
	}
	if c.d.Processes != nil && (c.lastProcScan.IsZero() || now.Sub(c.lastProcScan) >= c.cfg.Signals.ProcessInterval.D()) {
		procs, err := c.d.Processes()
		c.procs, c.procsOK, c.lastProcScan = procs, err == nil, now
	}
	if c.procsOK {
		in.Procs = c.procs
	}
	if c.inst != nil {
		in.OwnPIDs = c.inst.Members()
	}
	// Busy from the gate, not the state: the state only moves to Busy in
	// act(), after this observation, so a request in flight would otherwise
	// let the runtime's own utilization count as trusted external demand.
	active := 0
	if c.d.Gate != nil {
		active, _ = c.d.Gate.Active()
	}
	in.RuntimeBusy = c.st == state.Loading || c.st == state.Draining || c.st == state.Preempting || active > 0
	name, _ := c.activeProfile(now)
	in.Profile = c.cfg.Profiles[name]
	if c.d.Facts != nil {
		c.gpu, c.apps, c.session = c.d.Facts.Build(now, in)
	}
	// Until the release of our own VRAM is confirmed (or times out), memory
	// the driver has not freed yet is ours, not a competing workload.
	if c.vram.active && c.vram.footprint > 0 && !c.st.RuntimeRunning() {
		c.gpu.ExternalVRAMMiB = max(0, c.gpu.ExternalVRAMMiB-c.vram.footprint)
		c.gpu.ExternalVRAMGrowthMiB = 0
	}
	if c.d.Metrics != nil {
		c.d.Metrics.SetGPU(c.gpu)
	}
}

func (c *Controller) activeProfile(now time.Time) (string, string) {
	if c.mode.Mode == policy.ModeAIPriority {
		return c.cfg.AIPriorityProfile, "ai_priority"
	}
	return schedule.Active(c.cfg, now)
}

// trackTelemetryHealth emits loss/recovery events and applies the recovery
// cooldown after a sustained loss.
func (c *Controller) trackTelemetryHealth(now time.Time) {
	g := c.gpu
	if g.Confidence == policy.ConfidenceNone && g.NoneFor >= c.cfg.Telemetry.LossGrace.D() && !c.telemetryLost {
		c.telemetryLost = true
		if c.d.Metrics != nil {
			c.d.Metrics.IncTelemetryFailure()
		}
		c.emit(events.Event{Time: now, Type: events.TelemetryLost, Severity: policy.SeverityWarning,
			Message: "GPU telemetry lost for " + g.NoneFor.Round(time.Second).String(), Data: map[string]any{"errors": c.sample.Errors}})
		if c.d.Telemetry != nil {
			// A driver reset can change the adapter identity; re-enumerate.
			_ = c.d.Telemetry.Rebuild()
		}
	}
	if c.telemetryLost && g.Confidence != policy.ConfidenceNone {
		c.telemetryLost = false
		// Escalate: a load that itself breaks the counters must not cycle
		// load, loss, recover, load forever. Reset after a stable run.
		c.telemetryLosses++
		d := c.cfg.Recovery.TelemetryRecoveryCooldown.D()
		for i := 1; i < c.telemetryLosses && d < c.cfg.Recovery.CrashBackoffMax.D(); i++ {
			d *= 2
		}
		d = min(d, c.cfg.Recovery.CrashBackoffMax.D())
		c.emit(events.Event{Time: now, Type: events.TelemetryRecovered,
			Message: fmt.Sprintf("GPU telemetry recovered (confidence %s); waiting %s before loading (loss %d)", g.Confidence, d, c.telemetryLosses)})
		c.setRecovery(now, d, fmt.Sprintf("telemetry recovery (loss %d)", c.telemetryLosses))
	}
}

func (c *Controller) snapshot(now time.Time) policy.Snapshot {
	n, oldest := 0, time.Duration(0)
	if c.d.Gate != nil {
		n, oldest = c.d.Gate.Active()
	}
	s := policy.Snapshot{
		Now:  now,
		Mode: c.mode,
		Runtime: policy.RuntimeFacts{State: c.st, ActiveRequests: n, OldestRequestAge: oldest,
			DrainingSince: c.drainStart, FootprintMiB: c.footprint},
		GPU: c.gpu, Apps: c.apps, Session: c.session, Power: c.power, Timers: c.timers,
	}
	if c.inst != nil {
		s.Runtime.PID = c.inst.PID()
	}
	return s
}

// applyManual layers API-requested drains and reloads over the policy
// decision. They never override a yield the policy already wants.
func (c *Controller) applyManual(d policy.Decision) policy.Decision {
	if c.manualPending == "" || d.Action.Yields() || d.Action == policy.ActionHold && !c.st.RuntimeRunning() {
		return d
	}
	if !c.st.RuntimeRunning() {
		c.manualPending = ""
		return d
	}
	m := d
	m.Action, m.Competing, m.CountsAsPreemption, m.Suppress = policy.ActionDrain, false, false, false
	m.Severity, m.Tier = policy.SeverityNotice, policy.TierPause
	if c.manualPending == "reload" {
		m.Rule, m.Reason, m.IdleState = "manual.reload", "Reload requested: restarting the runtime", state.Stopped
	} else {
		m.Rule, m.Reason, m.IdleState = "manual.drain", "Drain requested: unloading the model", state.Cooldown
	}
	m.Grace = c.cfg.Profiles[d.Profile].Grace.D()
	return m
}

func (c *Controller) act(now time.Time, d policy.Decision) {
	switch c.st {
	case state.Stopped, state.Cooldown, state.Suppressed, state.Disabled, state.Error:
		if d.Action == policy.ActionRun {
			c.startLoad(now, d)
		} else {
			c.transition(now, d.IdleState, d, "", nil)
		}
	case state.Loading:
		if d.Action.Yields() {
			c.closeGate(d)
			c.beginStop(now, d, "stopping model load")
		}
	case state.Ready, state.Busy:
		var n int
		if d.Action.Yields() {
			// Close first and take the count under the gate's lock, so a
			// request admitted a moment ago is counted.
			n = c.closeGate(d)
		} else {
			if c.d.Gate != nil && c.inst != nil && !c.d.Gate.IsOpen() {
				// Loaded and allowed to run: admit. The gate opens here,
				// after this step's decision, not when loading finishes.
				c.d.Gate.Open(c.inst.BaseURL())
			}
			n, _ = c.d.Gate.Active()
		}
		switch d.Action {
		case policy.ActionPreempt:
			if n > 0 {
				c.forceClose(now, d, "hard contention")
			}
			c.beginStop(now, d, "")
		case policy.ActionDrain:
			if n == 0 {
				c.beginStop(now, d, "no request active; unloading now")
				return
			}
			c.yield, c.yieldManual = d, manualKind(d.Rule)
			c.drainStart, c.drainDeadline = now, now.Add(d.Grace)
			c.transition(now, state.Draining, d, fmt.Sprintf("%s. Stopped admitting; %d active request(s) get up to %s", d.Reason, n, d.Grace),
				map[string]any{"grace_ms": d.Grace.Milliseconds(), "active_requests": n, "grace_until": c.drainDeadline.Format(time.RFC3339)})
			c.emit(withData(events.FromDecision(events.DrainStarted, now, d, c.gpu), map[string]any{
				"grace_ms": d.Grace.Milliseconds(), "active_requests": n, "request_ids": c.d.Gate.ActiveIDs()}))
		default:
			if n > 0 {
				c.transition(now, state.Busy, d, "", nil)
			} else {
				c.transition(now, state.Ready, d, "", nil)
			}
		}
	case state.Draining:
		n, _ := c.d.Gate.Active()
		switch {
		case n == 0:
			if c.d.Metrics != nil {
				c.d.Metrics.IncDrain("completed")
			}
			c.emit(withData(events.FromDecision(events.DrainCompleted, now, c.yield, c.gpu), map[string]any{
				"drain_ms": now.Sub(c.drainStart).Milliseconds()}))
			c.beginStop(now, c.yield, "active requests finished within grace")
		case d.Action == policy.ActionPreempt:
			c.forceClose(now, d, "hard contention during grace")
			c.beginStop(now, d, "")
		case d.Action == policy.ActionDrain && c.drainStart.Add(d.Grace).Before(c.drainDeadline):
			old := c.drainDeadline
			c.drainDeadline = c.drainStart.Add(d.Grace)
			c.emit(withData(events.FromDecision(events.GraceShortened, now, d, c.gpu), map[string]any{
				"previous_until": old.Format(time.RFC3339), "grace_until": c.drainDeadline.Format(time.RFC3339)}))
			c.maybeGraceExpired(now, d)
		default:
			c.maybeGraceExpired(now, d)
		}
	case state.Preempting:
		// Waiting for the stop goroutine; absorbAsync handles completion.
	}
}

func (c *Controller) maybeGraceExpired(now time.Time, d policy.Decision) {
	if now.Before(c.drainDeadline) {
		return
	}
	if c.d.Metrics != nil {
		c.d.Metrics.IncDrain("grace_expired")
	}
	why := c.yield
	why.Reason = fmt.Sprintf("Grace of %s expired", c.drainDeadline.Sub(c.drainStart).Round(time.Second))
	c.forceClose(now, why, "grace expired")
	c.beginStop(now, c.yield, "")
}

func manualKind(rule string) string {
	switch rule {
	case "manual.drain":
		return "drain"
	case "manual.reload":
		return "reload"
	}
	return ""
}

func withData(e events.Event, data map[string]any) events.Event {
	e.Data = data
	return e
}

// closeGate stops admission and returns how many requests are still active.
func (c *Controller) closeGate(d policy.Decision) int {
	if c.d.Gate == nil {
		return 0
	}
	cond := state.Yielding
	if !c.st.Admitting() {
		cond = state.Unavailable
	}
	return c.d.Gate.Close(proxy.Closed{Condition: cond, Reason: d.Reason, RetryAfter: d.NextLoadAt})
}

func (c *Controller) forceClose(now time.Time, d policy.Decision, why string) {
	ids := c.d.Gate.ForceCloseAll()
	if len(ids) == 0 {
		return
	}
	if c.d.Metrics != nil {
		c.d.Metrics.IncPreemption(d.Rule, true)
	}
	e := events.FromDecision(events.Preempted, now, d, c.gpu)
	e.Message = fmt.Sprintf("Force-terminating %d active request(s): %s (%s)", len(ids), why, d.Reason)
	e.Data = map[string]any{"request_ids": ids, "why": why}
	if !c.drainStart.IsZero() {
		e.Data["drain_ms"] = now.Sub(c.drainStart).Milliseconds()
	}
	c.emit(e)
}

// beginStop moves to Preempting and terminates the runtime asynchronously.
func (c *Controller) beginStop(now time.Time, d policy.Decision, msg string) {
	if c.yield.Rule == "" || c.st != state.Draining {
		c.yield, c.yieldManual = d, manualKind(d.Rule)
	}
	c.closeGate(d)
	if !c.transition(now, state.Preempting, d, joinMsg(d.Reason, msg), nil) {
		return
	}
	c.vram = vramCheck{usedBefore: c.gpu.VRAMUsedMiB, footprint: c.footprint}
	inst := c.inst
	if inst == nil {
		c.stopCh <- stopResult{res: runtime.StopResult{AlreadyExited: true}}
		return
	}
	timeout := c.cfg.Runtime.KillVerifyTimeout.D()
	go func() {
		r, err := inst.Stop(timeout)
		c.stopCh <- stopResult{res: r, err: err}
		c.signalWake()
	}()
}

func joinMsg(a, b string) string {
	if b == "" {
		return a
	}
	return a + ". " + strings.ToUpper(b[:1]) + b[1:]
}

func (c *Controller) onStopped(now time.Time, r stopResult) {
	pid := 0
	inst := c.inst
	if inst != nil {
		pid = inst.PID()
	}
	c.inst = nil
	c.drainStart, c.drainDeadline = time.Time{}, time.Time{}
	y := c.yield
	c.yield = policy.Decision{}
	manual := c.yieldManual
	c.yieldManual = ""
	c.manualPending = ""
	if r.err != nil {
		c.emit(events.Event{Time: now, Type: events.KillFailed, Severity: policy.SeverityCritical, RuntimePID: pid,
			Message: "Runtime termination could not be verified: " + r.err.Error() + "; retrying"})
		c.zombie, c.zombieAttempts = inst, 1
		c.zombieRetryAt = now.Add(c.zombieBackoff())
		c.setRecovery(now, c.cfg.Recovery.DeviceLostCooldown.D(), "kill failure")
		c.transition(now, state.Error, y, "Runtime termination could not be verified", nil)
		return
	}
	c.emit(events.Event{Time: now, Type: events.RuntimeStopped, RuntimePID: pid, Rule: y.Rule,
		Message: "Runtime process tree terminated and verified empty",
		Data: map[string]any{"root_exit_ms": r.res.RootExit.Milliseconds(), "tree_empty_ms": r.res.TreeEmpty.Milliseconds(),
			"already_exited": r.res.AlreadyExited, "graceful": r.res.Graceful, "kill_fallback": r.res.KillFallback}})
	if r.res.KillFallback {
		msg := "Runtime did not exit on Ctrl+C within the graceful timeout; it was hard-killed (risk of a GPU driver hang on AMD, ADR 0011)"
		if r.res.InterruptErr != "" {
			msg = "Ctrl+C could not be delivered (" + r.res.InterruptErr + "); the runtime was hard-killed (risk of a GPU driver hang on AMD, ADR 0011)"
		}
		c.emit(events.Event{Time: now, Type: events.KillFailed, Severity: policy.SeverityWarning, RuntimePID: pid, Message: msg})
	}
	c.vram.active, c.vram.since = true, now
	if y.CountsAsPreemption {
		c.timers.Preemptions = append(pruneTimes(c.timers.Preemptions, now, c.cfg.AntiThrash.Window.D()), now)
		if c.d.Metrics != nil {
			c.d.Metrics.IncPreemption(y.Rule, false)
		}
	}
	idle := y.IdleState
	switch {
	case y.Suppress:
		c.timers.SuppressedAt = now
		idle = state.Suppressed
		if c.d.Metrics != nil {
			c.d.Metrics.IncSuppression()
		}
		end := now.Add(c.cfg.AntiThrash.SuppressFor.D())
		c.emit(withData(events.FromDecision(events.SuppressionStarted, now, y, c.gpu), map[string]any{
			"suppressed_until": end.Format(time.RFC3339), "preemptions_in_window": len(c.timers.Preemptions)}))
	case y.Competing:
		if c.d.Metrics != nil {
			c.d.Metrics.IncCooldown()
		}
		c.emit(withData(events.FromDecision(events.CooldownStarted, now, y, c.gpu), map[string]any{
			"cooldown": c.cfg.Profiles[y.Profile].Cooldown.D().String()}))
	case manual == "drain":
		c.setRecovery(now, c.cfg.Profiles[y.Profile].Cooldown.D(), "manual drain")
	}
	if idle == "" || idle.RuntimeRunning() {
		idle = state.Stopped
	}
	c.transition(now, idle, y, "Runtime unloaded", nil)
}

func pruneTimes(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := ts[:0:0]
	for _, t := range ts {
		if now.Sub(t) <= window {
			out = append(out, t)
		}
	}
	return out
}

// checkVRAMRelease confirms, as far as telemetry allows, that the runtime's
// VRAM came back after termination.
func (c *Controller) checkVRAMRelease(now time.Time) {
	v := &c.vram
	if !v.active || c.gpu.Confidence == policy.ConfidenceNone {
		if v.active && now.Sub(v.since) > c.cfg.Runtime.VRAMReleaseTimeout.D() {
			v.active = false
			c.emit(events.Event{Time: now, Type: events.VRAMNotReleased, Severity: policy.SeverityWarning,
				Message: "Could not confirm VRAM release: telemetry unavailable"})
		}
		return
	}
	tol := c.cfg.Runtime.VRAMReleaseToleranceMiB
	used := c.gpu.VRAMUsedMiB
	released := used <= c.baseline+tol || (v.footprint > 0 && used <= v.usedBefore-v.footprint*9/10)
	if released {
		v.active = false
		ms := now.Sub(v.since).Milliseconds()
		if c.d.Metrics != nil {
			c.d.Metrics.ObserveVRAMReleaseSeconds(float64(ms) / 1000)
		}
		c.emit(events.Event{Time: now, Type: events.VRAMReleased, Message: fmt.Sprintf("VRAM released: %d MiB in use (was %d MiB before stop)", used, v.usedBefore),
			Data: map[string]any{"release_ms": ms, "vram_used_mib": used, "vram_before_stop_mib": v.usedBefore, "baseline_mib": c.baseline}})
		return
	}
	if now.Sub(v.since) > c.cfg.Runtime.VRAMReleaseTimeout.D() {
		v.active = false
		c.emit(events.Event{Time: now, Type: events.VRAMNotReleased, Severity: policy.SeverityWarning,
			Message: fmt.Sprintf("VRAM not confirmed released after %s: %d MiB in use, baseline %d MiB", c.cfg.Runtime.VRAMReleaseTimeout.D(), used, c.baseline),
			Data:    map[string]any{"vram_used_mib": used, "baseline_mib": c.baseline}})
	}
}

func (c *Controller) checkCrashReset(now time.Time) {
	if c.st.Admitting() && !c.loadedAt.IsZero() && now.Sub(c.loadedAt) >= c.cfg.Recovery.CrashResetAfter.D() {
		c.crashCount, c.telemetryLosses, c.gpuResets = 0, 0, 0
	}
	// Learn the loaded footprint in the first 30 s after loading. With
	// attribution, own VRAM is measured directly; without it, the growth of
	// adapter usage over the pre-load baseline is the best estimate (own VRAM
	// is otherwise capped at the stored footprint and could never grow).
	if c.st.Admitting() && now.Sub(c.loadedAt) < 30*time.Second {
		candidate := 0
		switch c.gpu.Confidence {
		case policy.ConfidenceHigh:
			candidate = c.gpu.OwnVRAMMiB
		case policy.ConfidenceDegraded:
			candidate = c.gpu.VRAMUsedMiB - c.baseline
		}
		if candidate > c.footprint {
			c.footprint = candidate
		}
	}
}

func (c *Controller) startLoad(now time.Time, d policy.Decision) {
	if c.st != state.Stopped && !c.transition(now, state.Stopped, d, "Eligible to load", nil) {
		return
	}
	if c.d.Adapter == nil {
		return
	}
	// Never start a second runtime: clear anything left behind first.
	c.reconcile(now)
	c.baseline = c.gpu.VRAMUsedMiB
	inst, err := c.d.Adapter.Start(context.Background(), c.cfg.Runtime)
	if err != nil {
		c.loadFailed(now, d, err, "")
		return
	}
	c.inst, c.loadStart = inst, now
	c.transition(now, state.Loading, d, "Loading model", map[string]any{"pid": inst.PID()})
	c.emit(events.Event{Time: now, Type: events.LoadStarted, Rule: d.Rule, RuntimePID: inst.PID(),
		Message: "Starting runtime and loading model", Data: map[string]any{"vram_baseline_mib": c.baseline}})
	timeout := c.cfg.Runtime.LoadTimeout.D()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := inst.WaitReady(ctx)
		c.readyCh <- loadResult{inst: inst, err: err}
		c.signalWake()
	}()
}

func (c *Controller) onLoadResult(now time.Time, r loadResult) {
	if r.inst != c.inst || c.st != state.Loading {
		return // stale: the load was cancelled
	}
	if r.err != nil {
		diag := r.inst.Diagnostics()
		c.inst = nil
		c.zombie, c.zombieAttempts, c.zombieRetryAt = r.inst, 0, now
		c.retryZombie(now)
		c.closeGate(c.decision)
		c.loadFailed(now, c.decision, r.err, diag)
		return
	}
	c.lastLoadS = now.Sub(c.loadStart).Seconds()
	c.loadedAt = now
	if c.d.Metrics != nil {
		c.d.Metrics.IncLoad("ok")
		c.d.Metrics.ObserveLoadSeconds(c.lastLoadS)
	}
	c.transition(now, state.Ready, c.decision, fmt.Sprintf("Model ready after %.1f s", c.lastLoadS), map[string]any{"load_ms": now.Sub(c.loadStart).Milliseconds()})
	c.emit(events.Event{Time: now, Type: events.LoadCompleted, Message: fmt.Sprintf("Model loaded in %.1f s", c.lastLoadS),
		Data: map[string]any{"load_ms": now.Sub(c.loadStart).Milliseconds()}})
	// The gate opens in act() once this step's decision confirms the worker
	// may run; a game that appeared during loading must win first.
}

func (c *Controller) loadFailed(now time.Time, d policy.Decision, err error, diag string) {
	if c.d.Metrics != nil {
		c.d.Metrics.IncLoad("failed")
	}
	c.emit(events.Event{Time: now, Type: events.LoadFailed, Severity: policy.SeverityWarning,
		Message: "Model load failed: " + err.Error(), Data: map[string]any{"diagnostics_tail": tail(diag, 2048)}})
	c.backoff(now)
	c.transition(now, state.Error, d, "Model load failed", nil)
}

func (c *Controller) onCrash(now time.Time) {
	inst := c.inst
	c.inst = nil
	c.closeGate(policy.Decision{Reason: "runtime exited unexpectedly"})
	if ids := c.d.Gate.ForceCloseAll(); len(ids) > 0 {
		c.d.Log.Info("requests cut by runtime crash", "count", len(ids))
	}
	exit := "unknown"
	if err := inst.ExitErr(); err != nil {
		exit = err.Error()
	}
	if c.d.Metrics != nil {
		c.d.Metrics.IncRuntimeCrash()
	}
	c.emit(events.Event{Time: now, Type: events.RuntimeCrashed, Severity: policy.SeverityWarning, RuntimePID: inst.PID(),
		Message: "Runtime exited unexpectedly: " + exit, Data: map[string]any{"exit": exit, "diagnostics_tail": tail(inst.Diagnostics(), 2048), "state": c.st}})
	// Make sure nothing of the tree survives before anything else starts.
	c.zombie, c.zombieAttempts, c.zombieRetryAt = inst, 0, now
	c.retryZombie(now)
	c.drainStart, c.drainDeadline, c.yield = time.Time{}, time.Time{}, policy.Decision{}
	c.backoff(now)
	c.transition(now, state.Error, c.decision, "Runtime crashed", nil)
}

// checkGPUResets reacts to a GPU driver reset: the runtime is preempted at
// once (the next hang can be a blue screen) and loading waits for an
// escalating cooldown. The device-lost flag clears once the runtime is gone;
// the recovery timer then holds loading.
func (c *Controller) checkGPUResets(now time.Time) {
	if c.d.GPUResets != nil {
		resets := c.d.GPUResets()
		// Persisted at most every 30 s (the persist step compares JSON).
		if now.Sub(c.lastGPUCheck) >= 30*time.Second {
			c.lastGPUCheck = now
		}
		// One incident can surface as several dumps (both folders, a full
		// dump still being written): count resets within 2 minutes once.
		if len(resets) > 0 {
			// Move the watermark past every dump just handled so a restart
			// never reports the same incident again. (The dump may be
			// stamped slightly after now while it is still being written.)
			c.lastGPUCheck = now
			for _, r := range resets {
				if r.Time.After(c.lastGPUCheck) {
					c.lastGPUCheck = r.Time
				}
			}
		}
		if len(resets) > 0 && !c.lastGPUReset.IsZero() && now.Sub(c.lastGPUReset) < 2*time.Minute {
			c.power.DeviceLost = c.power.DeviceLost || c.st.RuntimeRunning()
			resets = nil
		}
		if len(resets) > 0 {
			c.lastGPUReset = now
			c.gpuResets++
			c.power.DeviceLost = true
			files := make([]string, 0, len(resets))
			for _, r := range resets {
				files = append(files, r.File)
			}
			d := c.cfg.Recovery.DeviceLostCooldown.D()
			for i := 1; i < c.gpuResets && d < 24*time.Hour; i++ {
				d *= 2
			}
			d = min(d, 24*time.Hour)
			c.emit(events.Event{Time: now, Type: events.DeviceLost, Severity: policy.SeverityCritical,
				Message: fmt.Sprintf("GPU driver reset detected (%d since the last stable run); stopping the runtime and waiting %s", c.gpuResets, d),
				Data:    map[string]any{"watchdog_dumps": files, "resets": c.gpuResets, "cooldown": d.String()}})
			c.setRecovery(now, d, fmt.Sprintf("GPU driver reset (%d)", c.gpuResets))
		}
	}
	if c.power.DeviceLost && !c.st.RuntimeRunning() && c.inst == nil && c.zombie == nil {
		c.power.DeviceLost = false
	}
}

// retryZombie starts (or re-starts, after backoff) terminating an
// unverified runtime.
func (c *Controller) retryZombie(now time.Time) {
	if c.zombie == nil || c.zombieStopping || now.Before(c.zombieRetryAt) {
		return
	}
	c.zombieStopping = true
	inst := c.zombie
	timeout := c.cfg.Runtime.KillVerifyTimeout.D()
	go func() {
		r, err := inst.Stop(timeout)
		c.stopCh <- stopResult{res: r, err: err, cleanup: true}
		c.signalWake()
	}()
}

func (c *Controller) onZombieStopped(now time.Time, r stopResult) {
	c.zombieStopping = false
	if c.zombie == nil {
		return
	}
	pid := c.zombie.PID()
	if r.err != nil {
		c.zombieAttempts++
		c.zombieRetryAt = now.Add(c.zombieBackoff())
		c.emit(events.Event{Time: now, Type: events.KillFailed, Severity: policy.SeverityCritical, RuntimePID: pid,
			Message: fmt.Sprintf("Runtime termination still not verified (attempt %d): %v", c.zombieAttempts, r.err)})
		return
	}
	c.zombie = nil
	c.emit(events.Event{Time: now, Type: events.RuntimeStopped, RuntimePID: pid,
		Message: "Previous runtime process tree verified terminated",
		Data:    map[string]any{"root_exit_ms": r.res.RootExit.Milliseconds(), "tree_empty_ms": r.res.TreeEmpty.Milliseconds(), "already_exited": r.res.AlreadyExited}})
}

func (c *Controller) zombieBackoff() time.Duration {
	d := 10 * time.Second
	for i := 1; i < c.zombieAttempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// applyZombie refuses to load while a previous runtime is unverified.
func (c *Controller) applyZombie(d policy.Decision) policy.Decision {
	if c.zombie == nil || c.st.RuntimeRunning() {
		return d
	}
	d.IdleState = state.Error
	if d.Action != policy.ActionRun {
		return d
	}
	d.Action, d.Rule, d.Tier, d.Severity = policy.ActionHold, "safety.runtime_unverified", policy.TierSafety, policy.SeverityWarning
	d.Reason = fmt.Sprintf("Previous runtime (pid %d) not yet confirmed terminated; retrying", c.zombie.PID())
	d.IdleState = state.Error
	return d
}

// RuleSecretsUnprotected holds the worker while Deps.LoadBlocked is set.
const RuleSecretsUnprotected = "safety.secrets_unprotected"

// applyLoadBlocked replaces the decision while loading is blocked, so the
// status names the blocking problem rather than whatever the policy would
// otherwise wait for. The runtime is never started in that case, so there
// is nothing running to yield.
func (c *Controller) applyLoadBlocked(d policy.Decision) policy.Decision {
	if c.d.LoadBlocked == "" || c.st.RuntimeRunning() {
		return d
	}
	return policy.Decision{Action: policy.ActionHold, Rule: RuleSecretsUnprotected, Tier: policy.TierSafety,
		Severity: policy.SeverityCritical, Reason: c.d.LoadBlocked, Profile: d.Profile, ProfileSource: d.ProfileSource,
		Mode: d.Mode, IdleState: state.Error}
}

// backoff schedules a bounded exponential retry after a crash or failed load.
func (c *Controller) backoff(now time.Time) {
	c.crashCount++
	d := c.cfg.Recovery.CrashBackoffInitial.D()
	for i := 1; i < c.crashCount && d < c.cfg.Recovery.CrashBackoffMax.D(); i++ {
		d *= 2
	}
	d = min(d, c.cfg.Recovery.CrashBackoffMax.D())
	c.setRecovery(now, d, fmt.Sprintf("crash backoff (attempt %d)", c.crashCount))
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// publish updates the gate's 503 details and metrics.
func (c *Controller) publish(now time.Time) {
	if c.d.Gate != nil && !c.st.Admitting() {
		c.d.Gate.UpdateClosed(proxy.Closed{Condition: c.st.Condition(), Reason: c.decision.Reason, RetryAfter: c.decision.NextLoadAt})
	}
	if c.d.Metrics != nil {
		n := 0
		if c.d.Gate != nil {
			n, _ = c.d.Gate.Active()
		}
		c.d.Metrics.SetActiveRequests(n)
		next := 0.0
		if !c.decision.NextLoadAt.IsZero() {
			next = c.decision.NextLoadAt.Sub(now).Seconds()
		}
		c.d.Metrics.SetNextLoadSeconds(next)
	}
}

func modeName(m policy.Mode) string {
	switch m {
	case policy.ModePause:
		return "Pause AI"
	case policy.ModeAIPriority:
		return "AI Priority"
	}
	return "Auto"
}
