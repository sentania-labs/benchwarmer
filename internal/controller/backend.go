package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
			if req.Mode == policy.ModePause {
				u = nextLocalMorning(now, c.cfg.Timezone)
			}
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
	if req.Principal != "" {
		data["principal"] = req.Principal
	}
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

// untilTomorrowHour is when a pause's "until tomorrow" ends: the start of the next
// day's usual hours, not midnight, so an evening pause does not hand the GPU
// back while the household is still asleep or at work.
const untilTomorrowHour = 8

// nextLocalMorning is 08:00 on the next calendar day in tz. time.Date keeps
// it at 08:00 wall-clock across a daylight-saving change.
func nextLocalMorning(now time.Time, tz string) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+1, untilTomorrowHour, 0, 0, 0, loc)
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

// modelCheckTimeout bounds the model file check: a stat on an unreachable
// network path can hang.
var modelCheckTimeout = 2 * time.Second

// checkModelFile runs CheckModelFile with a deadline. A check that cannot
// answer in time reports the file's availability as unknown.
func (c *Controller) checkModelFile(path string) error {
	if c.d.CheckModelFile == nil {
		return config.ErrFileUnknown
	}
	ch := make(chan error, 1)
	go func() { ch <- c.d.CheckModelFile(path) }()
	select {
	case err := <-ch:
		return err
	case <-time.After(modelCheckTimeout):
		return config.ErrFileUnknown
	}
}

// ValidateConfig checks a proposed change without applying it. Every field
// problem is reported together. A model file that is definitely missing or
// unusable is an error; one whose availability cannot be determined is a
// warning, so the save is allowed.
func (c *Controller) ValidateConfig(nc config.Config) ([]config.FieldError, error) {
	return c.validateConfig(nc, false)
}

// validateConfig is ValidateConfig with the model file also checked when
// checkModel is set. Otherwise only a changed path is checked, so an
// unrelated save is not blocked by a model that has gone missing since.
func (c *Controller) validateConfig(nc config.Config, checkModel bool) ([]config.FieldError, error) {
	var errs, warnings []config.FieldError
	if err := config.ValidateChange(nc); err != nil {
		var ve *config.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
		errs = ve.Errors
	}
	c.mu.Lock()
	changed := checkModel || nc.Runtime.ModelPath != c.cfg.Runtime.ModelPath
	c.mu.Unlock()
	reported := false
	for _, e := range errs {
		reported = reported || e.Field == "runtime.model_path"
	}
	if changed && !reported {
		switch err := c.checkModelFile(nc.Runtime.ModelPath); {
		case err == nil:
		case errors.Is(err, config.ErrFileUnknown):
			warnings = append(warnings, config.FieldError{Field: "runtime.model_path", Message: err.Error() + "; the model may not load until it is reachable"})
		default:
			errs = append(errs, config.FieldError{Field: "runtime.model_path", Message: err.Error()})
		}
	}
	if len(errs) > 0 {
		return warnings, &config.ValidationError{Errors: errs}
	}
	return warnings, nil
}

// UpdateConfig validates, persists, applies, and audits a new config.
func (c *Controller) UpdateConfig(ctx context.Context, nc config.Config, actor string) (config.Impact, error) {
	return c.updateConfig(ctx, nc, actor, "", "")
}

func (c *Controller) updateConfig(_ context.Context, nc config.Config, actor, expected, restoreOf string) (config.Impact, error) {
	// The model stat can hang on an unreachable network path, so validation
	// runs before the controller lock.
	_, verr := c.validateConfig(nc, restoreOf != "")
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	if restoreOf != "" && (expected == "" || expected != c.revisionLocked()) {
		return config.Impact{}, config.ErrHistoryConflict
	}
	if restoreOf != "" {
		if store, ok := c.d.ConfigStore.(historyStore); !ok {
			return config.Impact{}, errors.New("configuration history unavailable")
		} else if err := store.VerifyCurrent(c.cfg); err != nil {
			return config.Impact{}, err
		}
	}
	if verr != nil {
		c.emit(events.Event{Time: now, Type: events.ConfigRejected, Severity: policy.SeverityNotice,
			Message: "Configuration change rejected: " + verr.Error(), Data: map[string]any{"actor": actor}})
		return config.Impact{}, verr
	}
	if c.d.ConfigStore != nil {
		var err error
		if store, ok := c.d.ConfigStore.(historyStore); ok {
			var reset string
			reset, err = store.SaveVersion(c.cfg, nc, actor, now, restoreOf)
			if err == nil && reset != "" {
				c.emit(events.Event{Time: now, Type: events.ConfigChanged, Severity: policy.SeverityWarning,
					Message: "Configuration history was unreadable and has been reset; the old file was moved to " + reset,
					Data:    map[string]any{"actor": actor, "history_reset": true, "moved_to": reset}})
			}
		} else {
			err = c.d.ConfigStore.Save(nc)
		}
		if err != nil {
			return config.Impact{}, fmt.Errorf("persist config: %w", err)
		}
	}
	im := config.Classify(c.cfg, nc)
	changes := config.HistoryDiff(c.cfg, nc)
	c.cfg, c.cfgSource, c.cfgProblem, c.cfgProblemFields = nc, "primary", "", nil
	c.configRevision = rand.Text()
	c.schedCacheValid = false
	c.lastSetupCheck, c.setupResult = time.Time{}, nil // re-check the runtime and model files now, ignoring a check of the old paths
	if im.ServiceRestart {
		c.restartNeeded = true
	}
	if im.RuntimeReload {
		// A different model or context has a different footprint; relearn
		// it on the next load rather than trusting the old figure.
		c.footprint = 0
		if c.st.RuntimeRunning() && c.st != state.Draining && c.st != state.Preempting {
			c.manualPending = "reload"
		}
	}
	c.emit(events.Event{Time: now, Type: events.ConfigChanged,
		Message: fmt.Sprintf("Configuration updated by %s: %s", actor, strings.Join(im.Changed, ", ")),
		Data:    map[string]any{"actor": actor, "changed": im.Changed, "runtime_reload": im.RuntimeReload, "service_restart": im.ServiceRestart, "restore_of": restoreOf, "changes": changes}})
	c.signalWake()
	return im, nil
}

type historyStore interface {
	SaveVersion(config.Config, config.Config, string, time.Time, string) (string, error)
	History() ([]config.HistoryEntry, error)
	HistoryConfig(string) (config.Config, error)
	VerifyCurrent(config.Config) error
}

func configDigest(c config.Config) string {
	b, _ := config.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (c *Controller) revisionLocked() string {
	if c.configRevision == "" {
		c.configRevision = rand.Text()
	}
	return c.configRevision
}
func (c *Controller) ConfigHistory() ([]config.HistoryEntry, error) {
	store, ok := c.d.ConfigStore.(historyStore)
	if !ok {
		return nil, errors.New("configuration history unavailable")
	}
	return store.History()
}
func (c *Controller) PreviewConfigRestore(id string) (api.RestorePreview, error) {
	store, ok := c.d.ConfigStore.(historyStore)
	if !ok {
		return api.RestorePreview{}, errors.New("configuration history unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	target, err := store.HistoryConfig(id)
	if err != nil {
		return api.RestorePreview{}, err
	}
	return api.RestorePreview{ID: id, Revision: c.revisionLocked() + ":" + configDigest(target), Changes: config.HistoryDiff(c.cfg, target), Impact: config.Classify(c.cfg, target)}, nil
}
func (c *Controller) RestoreConfig(ctx context.Context, id, revision, actor string) (config.Impact, error) {
	if revision == "" {
		return config.Impact{}, config.ErrHistoryConflict
	}
	store, ok := c.d.ConfigStore.(historyStore)
	if !ok {
		return config.Impact{}, errors.New("configuration history unavailable")
	}
	target, err := store.HistoryConfig(id)
	if err != nil {
		return config.Impact{}, err
	}
	parts := strings.Split(revision, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] != configDigest(target) {
		return config.Impact{}, config.ErrHistoryConflict
	}
	return c.updateConfig(ctx, target, actor, parts[0], id)
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

// Condition returns the user-visible condition without taking the
// controller lock.
func (c *Controller) Condition() state.Condition {
	if v, ok := c.cond.Load().(state.Condition); ok {
		return v
	}
	return state.Unavailable
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
		ConfigSource: c.cfgSource, ConfigProblem: c.cfgProblem, ConfigProblemFields: slices.Clone(c.cfgProblemFields),
		ModeOptions: api.ModeOptions{PauseDurations: slices.Clone(c.cfg.Modes.PauseDurations), AIPriorityDurations: slices.Clone(c.cfg.Modes.AIPriorityDurations),
			UntilTomorrowAt: nextLocalMorning(now, c.cfg.Timezone)},
		RecentErrors: append([]events.Event(nil), c.recentErrors...),
	}
	name, src := c.activeProfile(now)
	s.Profile = api.ProfileStatus{Active: name, Source: src, Timezone: c.cfg.Timezone}
	if c.mode.Mode != policy.ModeAIPriority {
		s.Profile.NextChange = c.nextScheduleChange(now)
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
	s.GPU.ObservedGPUUsers = c.apps.ObservedGPUUsers
	if c.procsOK && !c.lastProcScan.IsZero() {
		age := max(now.Sub(c.lastProcScan).Milliseconds(), 0)
		s.GPU.ProcessScannedAt, s.GPU.ProcessScanAgeMS = c.lastProcScan, &age
	}
	s.Trigger = trigger(d)
	s.Timers = c.timerStatus(now, d)
	if c.d.Facts != nil {
		s.Agent = c.d.Facts.AgentStatus(now, c.cfg.Signals.SessionStaleAfter.D())
	}
	s.Summary = c.summary(now, s)
	s.Owner = api.DeriveOwner(s, c.cfg.Profiles[d.Profile].Cooldown.D())
	return s
}

// nextScheduleChange caches schedule.NextChange until the cached instant
// passes or the config changes; the full scan is too slow to run under the
// controller lock on every status read.
func (c *Controller) nextScheduleChange(now time.Time) time.Time {
	if c.schedCacheValid && (c.schedCache.IsZero() || now.Before(c.schedCache)) && now.Sub(c.schedCacheAt) < time.Hour {
		return c.schedCache
	}
	c.schedCache, c.schedCacheAt, c.schedCacheValid = schedule.NextChange(c.cfg, now), now, true
	return c.schedCache
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
	if !c.timers.LastCompetingAt.IsZero() && !d.Competing {
		// While the competing workload is present the cooldown has not
		// started; showing a rolling end time would contradict "no next load".
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

// PrepareSuspend releases the GPU before sleep: it stops admitting, starts
// the graceful stop at once, and waits up to budget for it to finish. It
// never escalates to a hard kill because of the budget (ADR 0013): a stop
// still running when the budget ends keeps going, and the resume side
// verifies it. The caller is the Windows control handler, which holds the
// system awake while this runs, so budget must be short. The outcome is
// the power_suspend_stop event: stopped (no runtime process remains, and the
// stop was observed inside the budget), late (the runtime was gone, but only
// seen so after the budget ended, when the host had already let the system
// sleep), elapsed_ms (how long the system was held back from sleeping), and
// budget_ms.
func (c *Controller) PrepareSuspend(budget time.Duration) {
	c.prepareSuspend(budget, time.Now, time.Sleep)
}

// prepareSuspend is PrepareSuspend with the clock and the poll sleep
// injectable, so a test can order a stop's completion against the deadline.
func (c *Controller) prepareSuspend(budget time.Duration, now func() time.Time, sleep func(time.Duration)) {
	t0 := now()
	c.mu.Lock()
	c.power.Suspending = true
	c.emit(events.Event{Type: events.PowerSuspend, Message: "System is suspending"})
	c.mu.Unlock()
	gone, late := c.waitStoppedWith(budget, now, sleep)
	stopped := gone && !late
	elapsed := now().Sub(t0)
	e := events.Event{Type: events.PowerSuspendStop, Message: "Runtime stopped before the system slept",
		Data: map[string]any{"stopped": stopped, "late": late, "elapsed_ms": elapsed.Milliseconds(), "budget_ms": budget.Milliseconds()}}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case late:
		e.Severity = policy.SeverityWarning
		e.Message = fmt.Sprintf("Runtime stopped only after the %s suspend budget ran out; the system may have slept with the model loaded", budget.Round(time.Millisecond))
	case stopped:
	case c.zombie != nil:
		e.Severity = policy.SeverityWarning
		e.RuntimePID = c.zombie.PID()
		e.Message = fmt.Sprintf("Runtime termination is unverified (pid %d, see kill_failed); the system may sleep with VRAM still held, and termination is retried after resume", e.RuntimePID)
	default:
		e.Severity = policy.SeverityWarning
		e.Message = fmt.Sprintf("Runtime still stopping when the %s suspend budget ran out; the system may sleep with the model loaded, and the stop continues and is verified after resume", budget.Round(time.Millisecond))
	}
	c.emit(e)
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
	inst := c.inst
	c.inst = nil
	c.persist()
	c.mu.Unlock()
	if inst != nil {
		// Out of time: kill without waiting for policy, outside the lock.
		// If the process exits anyway, the Job Object kills the tree.
		_, _ = inst.Stop(2 * time.Second)
	}
}

// waitStopped steps the controller until no runtime is running, no zombie
// verification is in flight, or timeout passes. It reports gone (no runtime
// process remains: none running and none retained as a zombie whose
// termination is unverified) and late (gone, but first seen after the
// deadline, so it cannot be claimed as done within the timeout). A stop
// that was already complete is never late. It always steps once, so the
// stop begins even with no time to wait.
func (c *Controller) waitStopped(timeout time.Duration) (gone, late bool) {
	return c.waitStoppedWith(timeout, time.Now, time.Sleep)
}

func (c *Controller) waitStoppedWith(timeout time.Duration, now func() time.Time, sleep func(time.Duration)) (gone, late bool) {
	deadline := now().Add(timeout)
	sawBusy := false
	for {
		c.Step(false)
		c.mu.Lock()
		busy := c.st.RuntimeRunning() || c.zombieStopping
		gone = !busy && c.inst == nil && c.zombie == nil
		c.mu.Unlock()
		sawBusy = sawBusy || busy
		expired := !now().Before(deadline)
		if !busy {
			return gone, gone && sawBusy && expired
		}
		if expired {
			return false, false
		}
		sleep(20 * time.Millisecond)
	}
}
