// Package controller is the single writer of Benchwarmer's lifecycle state.
// Each step it gathers facts, asks the pure policy evaluator for a decision,
// and turns the decision into state transitions and side effects: opening or
// closing the admission gate, starting or stopping the runtime, cooldown and
// suppression bookkeeping, and an event for every lifecycle change
// (ADR 0004).
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/humantime"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/proxy"
	"github.com/sentania-labs/benchwarmer/internal/runtime"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

// TelemetrySource yields GPU samples. *telemetry.WindowsCollector satisfies it.
type TelemetrySource interface {
	Collect() telemetry.Sample
	Rebuild() error
}

// FactInput is one tick's raw observations for the fact builder.
type FactInput struct {
	Sample       *telemetry.Sample // nil when no source is configured
	Procs        []signals.Process // nil when the snapshot failed
	OwnPIDs      []int
	RuntimeBusy  bool // busy or loading: own utilization cannot be separated without attribution
	FootprintMiB int
	Profile      config.Profile
	Config       config.Config
}

// FactBuilder turns raw observations into policy facts, keeping the rolling
// windows used for hysteresis.
type FactBuilder interface {
	Build(now time.Time, in FactInput) (policy.GPUFacts, policy.AppFacts, policy.SessionFacts)
	AgentReport(now time.Time, r api.AgentReport)
	AgentStatus(now time.Time) api.AgentStatus
}

// ConfigStore persists configuration atomically. *config.Store satisfies it.
type ConfigStore interface {
	Save(config.Config) error
}

// EventReader serves event history for the API.
type EventReader interface {
	Events(q api.EventQuery) ([]events.Event, error)
}

// Persisted is the controller state that survives a service restart.
type Persisted struct {
	Mode            policy.ModeFacts  `json:"mode"`
	ModeSetAt       time.Time         `json:"mode_set_at,omitzero"`
	UntilReboot     bool              `json:"until_reboot,omitempty"`
	Timers          policy.TimerFacts `json:"timers"`
	CrashCount      int               `json:"crash_count"`
	FootprintMiB    int               `json:"footprint_mib"`
	LastLoadSeconds float64           `json:"last_load_seconds"`
}

// Persist saves and loads Persisted.
type Persist interface {
	SaveControllerState(Persisted) error
	LoadControllerState() (Persisted, bool, error)
}

// Metrics receives lifecycle measurements. All methods must be cheap.
type Metrics interface {
	SetState(state.State)
	IncTransition(from, to state.State)
	IncLoad(result string)
	ObserveLoadSeconds(float64)
	SetActiveRequests(int)
	IncDrain(outcome string)
	IncPreemption(rule string, forced bool)
	IncCooldown()
	IncSuppression()
	IncRuntimeCrash()
	IncTelemetryFailure()
	ObserveVRAMReleaseSeconds(float64)
	SetNextLoadSeconds(float64)
	IncOrphansKilled(int)
	SetGPU(policy.GPUFacts)
}

// Deps are the controller's collaborators. Optional ones may be nil.
type Deps struct {
	Config       config.Config
	ConfigSource string // "primary", "last_good", "default"
	ConfigStore  ConfigStore
	Adapter      runtime.Adapter
	Telemetry    TelemetrySource // optional
	Processes    func() ([]signals.Process, error)
	Facts        FactBuilder
	Gate         *proxy.Gate
	Events       events.Sink
	EventReader  EventReader // optional
	Persist      Persist     // optional
	Metrics      Metrics     // optional
	Now          func() time.Time
	Log          *slog.Logger
	Version      string
}

type loadResult struct {
	inst runtime.Instance
	err  error
}

type stopResult struct {
	res runtime.StopResult
	err error
}

// Controller owns lifecycle state. All fields are guarded by mu.
type Controller struct {
	mu sync.Mutex
	d  Deps

	cfg           config.Config
	cfgSource     string
	restartNeeded bool

	st         state.State
	inst       runtime.Instance
	loadStart  time.Time
	loadedAt   time.Time
	lastLoadS  float64
	footprint  int
	crashCount int
	baseline   int // adapter VRAM used before the current load

	readyCh chan loadResult
	stopCh  chan stopResult
	wake    chan struct{}

	drainStart    time.Time
	drainDeadline time.Time
	yield         policy.Decision // the decision that started the current yield
	yieldManual   string          // "drain" or "reload" for API-requested yields
	vram          vramCheck

	timers      policy.TimerFacts
	mode        policy.ModeFacts
	modeSetAt   time.Time
	untilReboot bool
	power       policy.PowerFacts

	gpu     policy.GPUFacts
	apps    policy.AppFacts
	session policy.SessionFacts
	sample  telemetry.Sample

	procs        []signals.Process
	procsOK      bool
	lastProcScan time.Time

	decision      policy.Decision
	lastEvaluated time.Time
	telemetryLost bool
	manualPending string // "drain" or "reload" requested by the API
	persisted     []byte
	recentErrors  []events.Event
}

type vramCheck struct {
	active     bool
	since      time.Time
	usedBefore int
	footprint  int
}

// New builds a controller. Call Start before Run.
func New(d Deps) *Controller {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Events == nil {
		d.Events = events.SinkFunc(func(events.Event) {})
	}
	return &Controller{
		d: d, cfg: d.Config, cfgSource: d.ConfigSource, st: state.Stopped,
		readyCh: make(chan loadResult, 1), stopCh: make(chan stopResult, 1), wake: make(chan struct{}, 1),
		mode: policy.ModeFacts{Mode: policy.ModeAuto},
	}
}

// Start restores persisted state, reconciles orphaned runtimes, applies the
// startup cooldown, and records the service start.
func (c *Controller) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	if c.d.Persist != nil {
		if p, ok, err := c.d.Persist.LoadControllerState(); err != nil {
			c.d.Log.Warn("could not load controller state", "err", err)
		} else if ok {
			c.timers = p.Timers
			c.timers.RecoveryUntil, c.timers.RecoveryReason = time.Time{}, ""
			c.crashCount, c.footprint, c.lastLoadS = p.CrashCount, p.FootprintMiB, p.LastLoadSeconds
			// A mode "until reboot" does not survive a restart; an expired
			// temporary mode reverts to auto.
			if !p.UntilReboot && (p.Mode.Until == nil || p.Mode.Until.After(now)) && p.Mode.Mode != "" {
				c.mode, c.modeSetAt = p.Mode, p.ModeSetAt
			}
		}
	}
	c.emit(events.Event{Time: now, Type: events.ServiceStarted, State: c.st, Condition: c.st.Condition(),
		Message: "Benchwarmer service started", Data: map[string]any{"version": c.d.Version, "config_source": c.cfgSource}})
	if c.cfgSource != "" && c.cfgSource != "primary" {
		c.emit(events.Event{Time: now, Type: events.ConfigRecovered, Severity: policy.SeverityWarning,
			Message: "Configuration file was unusable; running on the " + c.cfgSource + " configuration"})
	}
	c.reconcile(now)
	c.setRecovery(now, c.cfg.Recovery.StartupCooldown.D(), "startup")
	c.persist()
}

// Run steps the controller until ctx ends, then shuts the runtime down.
func (c *Controller) Run(ctx context.Context) {
	cfg, _ := c.Config()
	interval := cfg.Telemetry.SampleInterval.D()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var gateCh <-chan struct{}
	if c.d.Gate != nil {
		gateCh = c.d.Gate.Changed()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.Step(true)
		case <-gateCh:
			c.Step(false)
		case <-c.wake:
			c.Step(false)
		}
	}
}

// Step runs one evaluation. collect=false reuses the last observations
// (used when woken by a request finishing or a runtime operation completing).
func (c *Controller) Step(collect bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step(c.d.Now(), collect)
}

func (c *Controller) signalWake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) emit(e events.Event) {
	if e.Time.IsZero() {
		e.Time = c.d.Now()
	}
	if e.State == "" {
		e.State = c.st
		e.Condition = c.st.Condition()
	}
	if e.Profile == "" {
		e.Profile = c.decision.Profile
	}
	if e.Mode == "" {
		e.Mode = c.mode.Mode
	}
	if e.RuntimePID == 0 && c.inst != nil {
		e.RuntimePID = c.inst.PID()
	}
	if e.Severity == policy.SeverityCritical || e.Type == events.RuntimeCrashed || e.Type == events.KillFailed || e.Type == events.LoadFailed {
		c.recentErrors = append(c.recentErrors, e)
		if len(c.recentErrors) > 5 {
			c.recentErrors = c.recentErrors[len(c.recentErrors)-5:]
		}
	}
	c.d.Events.Emit(e)
	c.d.Log.Info("event", "type", e.Type, "state", e.State, "rule", e.Rule, "message", e.Message)
}

// transition changes state if legal and records it with the decision.
func (c *Controller) transition(now time.Time, to state.State, d policy.Decision, msg string, data map[string]any) bool {
	from := c.st
	if from == to {
		return true
	}
	if !state.CanTransition(from, to) {
		c.d.Log.Error("illegal transition refused", "from", from, "to", to, "rule", d.Rule)
		return false
	}
	c.st = to
	if c.d.Metrics != nil {
		c.d.Metrics.IncTransition(from, to)
		c.d.Metrics.SetState(to)
	}
	// Ready and Busy flip per request; they are metrics, not audit events.
	if (from == state.Ready && to == state.Busy) || (from == state.Busy && to == state.Ready) {
		return true
	}
	if msg == "" {
		msg = d.Reason
	}
	e := events.FromDecision(events.StateChanged, now, d, c.gpu)
	e.PrevState, e.State, e.Condition, e.Message, e.Data = from, to, to.Condition(), msg, data
	c.emit(e)
	return true
}

func (c *Controller) setRecovery(now time.Time, d time.Duration, reason string) {
	until := now.Add(d)
	if until.After(c.timers.RecoveryUntil) {
		c.timers.RecoveryUntil, c.timers.RecoveryReason = until, reason
		if d > 0 {
			c.emit(events.Event{Time: now, Type: events.RecoveryScheduled, Message: fmt.Sprintf("Next load not before %s (%s)", humantime.Clock(until, now, c.cfg.Timezone), reason),
				Data: map[string]any{"until": until.Format(time.RFC3339), "reason": reason}})
		}
	}
}

func (c *Controller) persist() {
	if c.d.Persist == nil {
		return
	}
	p := Persisted{Mode: c.mode, ModeSetAt: c.modeSetAt, UntilReboot: c.untilReboot, Timers: c.timers,
		CrashCount: c.crashCount, FootprintMiB: c.footprint, LastLoadSeconds: c.lastLoadS}
	b, _ := json.Marshal(p)
	if string(b) == string(c.persisted) {
		return
	}
	if err := c.d.Persist.SaveControllerState(p); err != nil {
		c.d.Log.Warn("could not persist controller state", "err", err)
		return
	}
	c.persisted = b
}

func (c *Controller) reconcile(now time.Time) {
	if c.d.Adapter == nil {
		return
	}
	orphans, err := c.d.Adapter.Reconcile(c.cfg.Runtime)
	if err != nil {
		c.d.Log.Warn("orphan reconciliation failed", "err", err)
	}
	killed := 0
	for _, o := range orphans {
		if o.Killed {
			killed++
		}
		c.emit(events.Event{Time: now, Type: events.OrphanKilled, Severity: policy.SeverityWarning,
			Message: fmt.Sprintf("Found runtime process %d not owned by this worker; killed=%v", o.PID, o.Killed),
			Data:    map[string]any{"pid": o.PID, "path": o.Path, "killed": o.Killed, "error": o.Err}})
	}
	if killed > 0 && c.d.Metrics != nil {
		c.d.Metrics.IncOrphansKilled(killed)
	}
}

// errNotRunning is returned for manual actions that need a runtime.
var errNotRunning = errors.New("runtime is not running")
