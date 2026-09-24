package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/humantime"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/schedule"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

var _ api.Backend = (*Controller)(nil)

// Config returns the active configuration and its source.
func (c *Controller) Config() (config.Config, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return config.Clone(c.cfg), c.cfgSource
}

// SetMode changes the manual mode.
func (c *Controller) SetMode(req api.ModeRequest) (api.ModeStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	m := policy.ModeFacts{Mode: req.Mode, SetBy: req.SetBy}
	untilReboot := false
	switch req.Mode {
	case policy.ModeAuto:
	case policy.ModePause, policy.ModeAIPriority:
		switch req.Duration {
		case "":
			if req.Mode == policy.ModeAIPriority {
				u := now.Add(c.cfg.Modes.MaxAIPriority.D())
				m.Until = &u
			}
		case "until_reboot":
			untilReboot = true
			if req.Mode == policy.ModeAIPriority {
				u := now.Add(c.cfg.Modes.MaxAIPriority.D())
				m.Until = &u
			}
		case "until_tomorrow":
			u := nextLocalMidnight(now, c.cfg.Timezone)
			m.Until = &u
		default:
			d, err := time.ParseDuration(req.Duration)
			if err != nil || d <= 0 {
				return api.ModeStatus{}, fmt.Errorf("invalid duration %q", req.Duration)
			}
			u := now.Add(d)
			m.Until = &u
		}
		if req.Mode == policy.ModeAIPriority && m.Until != nil && m.Until.Sub(now) > c.cfg.Modes.MaxAIPriority.D() {
			u := now.Add(c.cfg.Modes.MaxAIPriority.D())
			m.Until = &u
		}
	default:
		return api.ModeStatus{}, fmt.Errorf("unknown mode %q", req.Mode)
	}
	prev := c.mode.Mode
	c.mode, c.modeSetAt, c.untilReboot = m, now, untilReboot
	msg := fmt.Sprintf("Mode changed from %s to %s", modeName(prev), modeName(m.Mode))
	if m.Until != nil {
		msg += " until " + humantime.Clock(*m.Until, now, c.cfg.Timezone)
	} else if untilReboot {
		msg += " until reboot"
	}
	data := map[string]any{"previous": prev, "set_by": req.SetBy}
	if m.Until != nil {
		data["until"] = m.Until.Format(time.RFC3339)
	}
	c.emit(events.Event{Time: now, Type: events.ModeChanged, Mode: m.Mode, Message: msg, Data: data})
	c.persist()
	c.signalWake()
	return api.ModeStatus{Mode: m.Mode, Until: m.Until, SetBy: m.SetBy, SetAt: now}, nil
}

func nextLocalMidnight(now time.Time, tz string) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
}

// RequestDrain unloads the model now; it reloads when policy allows after
// the active profile's cooldown.
func (c *Controller) RequestDrain(reason string) error {
	return c.requestManual("drain", reason)
}

// RequestReload restarts the runtime (drain first, then reload).
func (c *Controller) RequestReload(reason string) error {
	return c.requestManual("reload", reason)
}

func (c *Controller) requestManual(kind, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.st.RuntimeRunning() {
		return errNotRunning
	}
	if c.st == state.Draining || c.st == state.Preempting {
		return nil
	}
	c.manualPending = kind
	c.emit(events.Event{Type: events.DecisionChanged, Rule: "manual." + kind, Message: fmt.Sprintf("Manual %s requested: %s", kind, reason)})
	c.signalWake()
	return nil
}

// UpdateConfig validates, persists, applies, and audits a new config.
func (c *Controller) UpdateConfig(_ context.Context, nc config.Config, actor string) (config.Impact, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	if err := config.Validate(nc); err != nil {
		c.emit(events.Event{Time: now, Type: events.ConfigRejected, Severity: policy.SeverityNotice,
			Message: "Configuration change rejected: " + err.Error(), Data: map[string]any{"actor": actor}})
		return config.Impact{}, err
	}
	if c.d.ConfigStore != nil {
		if err := c.d.ConfigStore.Save(nc); err != nil {
			return config.Impact{}, fmt.Errorf("persist config: %w", err)
		}
	}
	im := config.Classify(c.cfg, nc)
	c.cfg, c.cfgSource = nc, "primary"
	if im.ServiceRestart {
		c.restartNeeded = true
	}
	if im.RuntimeReload && c.st.RuntimeRunning() && c.st != state.Draining && c.st != state.Preempting {
		c.manualPending = "reload"
	}
	c.emit(events.Event{Time: now, Type: events.ConfigChanged,
		Message: fmt.Sprintf("Configuration updated by %s: %s", actor, strings.Join(im.Changed, ", ")),
		Data:    map[string]any{"actor": actor, "changed": im.Changed, "runtime_reload": im.RuntimeReload, "service_restart": im.ServiceRestart}})
	c.signalWake()
	return im, nil
}

// AgentReport accepts a session-agent report.
func (c *Controller) AgentReport(r api.AgentReport) {
	if c.d.Facts == nil {
		return
	}
	c.d.Facts.AgentReport(c.d.Now(), r)
}

// Events returns event history.
func (c *Controller) Events(q api.EventQuery) ([]events.Event, error) {
	if c.d.EventReader == nil {
		return nil, errors.New("event history unavailable")
	}
	return c.d.EventReader.Events(q)
}

// Status assembles the /api/v1/status document.
func (c *Controller) Status() api.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	d := c.decision
	s := api.Status{
		Time: now, Version: c.d.Version, Condition: c.st.Condition(), State: c.st,
		Mode:         api.ModeStatus{Mode: c.mode.Mode, Until: c.mode.Until, SetBy: c.mode.SetBy, SetAt: c.modeSetAt},
		Decision:     d,
		ConfigSource: c.cfgSource,
		RecentErrors: append([]events.Event(nil), c.recentErrors...),
	}
	name, src := c.activeProfile(now)
	s.Profile = api.ProfileStatus{Active: name, Source: src, Timezone: c.cfg.Timezone}
	if c.mode.Mode != policy.ModeAIPriority {
		s.Profile.NextChange = schedule.NextChange(c.cfg, now)
	}
	n, oldest := 0, time.Duration(0)
	if c.d.Gate != nil {
		n, oldest = c.d.Gate.Active()
	}
	s.Runtime = api.RuntimeStatus{Ready: c.st.Admitting(), Model: filepath.Base(strings.ReplaceAll(c.cfg.Runtime.ModelPath, `\`, "/")),
		ActiveRequests: n, OldestRequestSec: oldest.Seconds(), LastLoadSeconds: c.lastLoadS, FootprintMiB: c.footprint, RestartCount: c.crashCount}
	if c.inst != nil {
		s.Runtime.PID = c.inst.PID()
	}
	if c.st.Admitting() && !c.loadedAt.IsZero() {
		t := c.loadedAt
		s.Runtime.LoadedAt = &t
	}
	g := c.gpu
	s.GPU = api.GPUStatus{Adapter: c.sample.AdapterName, Confidence: g.Confidence, TotalUtilPct: g.TotalUtilPct, OwnUtilPct: g.OwnUtilPct,
		ExternalUtilPct: g.ExternalUtilPct, VRAMTotalMiB: g.VRAMTotalMiB, VRAMUsedMiB: g.VRAMUsedMiB, VRAMFreeMiB: g.VRAMFreeMiB,
		OwnVRAMMiB: g.OwnVRAMMiB, ExternalVRAMMiB: g.ExternalVRAMMiB, TemperatureC: g.TemperatureC, SampledAt: c.sample.Time}
	s.GPU.TopExternal = topExternal(c.apps)
	s.Trigger = trigger(d)
	s.Timers = c.timerStatus(now, d)
	if c.d.Facts != nil {
		s.Agent = c.d.Facts.AgentStatus(now)
	}
	s.Summary = c.summary(now, s)
	return s
}

func (c *Controller) timerStatus(now time.Time, d policy.Decision) api.TimerStatus {
	var t api.TimerStatus
	ptr := func(x time.Time) *time.Time {
		if x.IsZero() || !x.After(now) {
			return nil
		}
		return &x
	}
	if c.st == state.Draining {
		t.GraceUntil = ptr(c.drainDeadline)
	}
	p := c.cfg.Profiles[d.Profile]
	if !c.timers.LastCompetingAt.IsZero() {
		t.CooldownUntil = ptr(c.timers.LastCompetingAt.Add(p.Cooldown.D()))
	}
	if !c.timers.SuppressedAt.IsZero() {
		t.SuppressedUntil = ptr(c.timers.SuppressedAt.Add(c.cfg.AntiThrash.SuppressFor.D()))
	}
	t.RecoveryUntil = ptr(c.timers.RecoveryUntil)
	if !c.st.RuntimeRunning() {
		t.NextLoadAt = ptr(d.NextLoadAt)
		if d.Action == policy.ActionHold {
			t.NextLoadReason = d.Reason
		}
	}
	return t
}

func topExternal(a policy.AppFacts) []policy.AppMatch {
	var out []policy.AppMatch
	for _, l := range [][]policy.AppMatch{a.Games, a.LauncherChildren, a.Ordinary} {
		out = append(out, l...)
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func trigger(d policy.Decision) string {
	for _, e := range d.Evidence {
		switch e.Name {
		case "process", "foreground", "top_external_process":
			return e.Value
		}
	}
	if d.Action.Yields() || d.Competing {
		for _, e := range d.Evidence {
			return e.Name + "=" + e.Value
		}
	}
	return ""
}

func (c *Controller) summary(now time.Time, s api.Status) string {
	clock := func(t *time.Time) string { return humantime.Clock(*t, now, c.cfg.Timezone) }
	switch c.st {
	case state.Ready:
		return "Available: model ready"
	case state.Busy:
		return fmt.Sprintf("Available: serving %d request(s)", s.Runtime.ActiveRequests)
	case state.Loading:
		return "Available: loading model"
	case state.Draining:
		left := ""
		if s.Timers.GraceUntil != nil {
			left = fmt.Sprintf(" (%s left)", s.Timers.GraceUntil.Sub(now).Round(time.Second))
		}
		return "Yielding: " + s.Decision.Reason + "; finishing active request" + left
	case state.Preempting:
		return "Yielding: stopping the model"
	}
	msg := "Unavailable: " + s.Decision.Reason
	switch {
	case s.Timers.NextLoadAt != nil:
		msg += "; next load " + clock(s.Timers.NextLoadAt)
	case s.Decision.Competing:
		// No fixed time exists while the workload persists.
		msg += fmt.Sprintf("; reloads %s after it stops", c.cfg.Profiles[s.Decision.Profile].Cooldown.D())
	}
	return msg
}

// PrepareSuspend releases the GPU before sleep, waiting up to timeout.
func (c *Controller) PrepareSuspend(timeout time.Duration) {
	c.mu.Lock()
	c.power.Suspending = true
	c.emit(events.Event{Type: events.PowerSuspend, Message: "System is suspending"})
	c.mu.Unlock()
	c.waitStopped(timeout)
}

// Resumed records a resume and applies the resume cooldown.
func (c *Controller) Resumed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	c.power.Suspending = false
	c.emit(events.Event{Time: now, Type: events.PowerResume, Message: "System resumed"})
	if c.d.Telemetry != nil {
		_ = c.d.Telemetry.Rebuild()
	}
	c.setRecovery(now, c.cfg.Recovery.ResumeCooldown.D(), "resume")
	c.signalWake()
}

// Shutdown stops admitting work and terminates the runtime within timeout.
func (c *Controller) Shutdown(timeout time.Duration) {
	c.mu.Lock()
	c.power.ShuttingDown = true
	c.emit(events.Event{Type: events.ServiceStopping, Message: "Benchwarmer service stopping"})
	c.mu.Unlock()
	c.waitStopped(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inst != nil {
		// Out of time: kill without waiting for policy.
		_, _ = c.inst.Stop(2 * time.Second)
		c.inst = nil
	}
	c.persist()
}

func (c *Controller) waitStopped(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.Step(false)
		c.mu.Lock()
		running := c.st.RuntimeRunning()
		c.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
