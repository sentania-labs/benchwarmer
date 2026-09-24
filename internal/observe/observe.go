// Package observe turns raw observations (a telemetry sample, a process
// snapshot, the runtime's PIDs, the session agent's report) into the policy
// facts documented in internal/policy/types.go. It keeps the rolling windows
// and hysteresis timers that ADR 0004 assigns to the signal layer, so the
// evaluator stays stateless.
//
// Observer makes no OS calls and never reads the clock: every tick carries
// its own time, which keeps it deterministic and table-testable.
package observe

import (
	"cmp"
	"slices"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/classify"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

const mib = 1 << 20

// Fixed tuning values. They decide what counts as "measurable" GPU use for
// fact inclusion, not what makes the worker yield (the policy thresholds do
// that, and those are all in config).
const (
	// Ordinary apps are listed only with measurable GPU use.
	ordinaryMinUtilPct = 1.0
	ordinaryMinVRAMMiB = 64
	// An unclassified launcher child needs a little more to count.
	launcherChildMinUtilPct = 1.0
	launcherChildMinVRAMMiB = 128
	// Attribution is consistent (high confidence) when the per-process
	// dedicated sum is within the larger of these of the adapter figure.
	// Per-process counters miss kernel and compositor allocations, so some
	// gap is normal.
	consistencyToleranceMiB = 1024
	consistencyTolerancePct = 25.0
	// A gap between ticks longer than gapFactor sample intervals (at least
	// minGap) breaks every continuity claim: nothing was observed meanwhile.
	gapFactor = 3
	minGap    = 3 * time.Second
)

// RuleLauncherChild names the inferred rule for an unclassified GPU-using
// child of a launcher.
const RuleLauncherChild = "inferred:launcher-child"

// AgentReport is the latest session-agent report (ADR 0005). It mirrors the
// fields of the management API's AgentReport that policy needs.
type AgentReport struct {
	SessionID      uint32
	ForegroundPID  uint32
	ForegroundName string
	ForegroundPath string
	Fullscreen     bool
	IdleSeconds    float64
	Locked         bool
}

// Input is one tick's raw observations.
type Input struct {
	// Sample is the latest telemetry sample; nil when none was collected.
	Sample *telemetry.Sample
	// Processes is the process snapshot; nil when the snapshot failed.
	Processes []signals.Process
	// OwnPIDs are the runtime's current members (empty when not running).
	OwnPIDs []int
	// RuntimeActive is true while the runtime is loading or serving a
	// request, i.e. when its GPU use cannot be told apart without
	// per-process attribution.
	RuntimeActive bool
	// FootprintMiB is the runtime's measured loaded footprint, or the last
	// load's figure; 0 when never measured.
	FootprintMiB int

	Profile config.Profile
	Config  config.Config

	// Agent is the latest session-agent report and AgentAt when it was
	// received; nil when no report has ever arrived.
	Agent   *AgentReport
	AgentAt time.Time
}

// Facts are the policy facts derived from one tick.
type Facts struct {
	GPU     policy.GPUFacts
	Apps    policy.AppFacts
	Session policy.SessionFacts
}

type vramPoint struct {
	t   time.Time
	mib int
}

// Observer carries the state that spans ticks. It is not safe for
// concurrent use; the controller goroutine owns it.
type Observer struct {
	lastTick time.Time
	lastConf policy.Confidence

	noneSince    time.Time
	healthySince time.Time

	aboveCritSince time.Time
	aboveSoftSince time.Time
	belowSoftSince time.Time

	vram []vramPoint

	cls *classify.Classifier
}

// New returns an Observer with no history.
func New() *Observer { return &Observer{} }

// Observe derives facts for the tick at now.
func (o *Observer) Observe(now time.Time, in Input) Facts {
	cfg := in.Config
	if o.cls == nil || !slices.Equal(o.cls.Rules(), cfg.Applications) {
		o.cls = classify.New(cfg.Applications)
	}
	if !o.lastTick.IsZero() {
		gap := max(gapFactor*cfg.Telemetry.SampleInterval.D(), minGap)
		if now.Before(o.lastTick) || now.Sub(o.lastTick) > gap {
			o.resetWindows()
			o.healthySince = time.Time{}
		}
	}
	o.lastTick = now

	own := make(map[uint32]bool, len(in.OwnPIDs))
	for _, p := range in.OwnPIDs {
		own[uint32(p)] = true
	}
	gpu, perPID := o.gpu(now, in, own)
	sess, fresh := o.session(now, in)
	return Facts{GPU: gpu, Apps: o.apps(now, in, own, perPID, fresh), Session: sess}
}

func (o *Observer) resetWindows() {
	o.aboveCritSince, o.aboveSoftSince, o.belowSoftSince = time.Time{}, time.Time{}, time.Time{}
	o.vram = o.vram[:0]
}

// gpu computes GPUFacts and returns per-PID usage for app facts (nil when
// the sample carries no trustworthy per-process data).
func (o *Observer) gpu(now time.Time, in Input, own map[uint32]bool) (policy.GPUFacts, map[uint32]telemetry.ProcGPU) {
	var g policy.GPUFacts
	s := in.Sample
	if s != nil && s.TemperatureC != nil {
		// Temperature is a separate source and stays valid when the
		// counters are incomplete.
		t := *s.TemperatureC
		g.TemperatureC = &t
	}
	g.Confidence = confidence(in, own)
	o.trackConfidence(now, g.Confidence, &g)
	if g.Confidence == policy.ConfidenceNone {
		// Zeros in an incomplete sample mean unknown (ADR 0003). No
		// hysteresis window may be extended or satisfied by it.
		o.resetWindows()
		return g, nil
	}

	d := telemetry.Split(*s, own)
	g.TotalUtilPct = d.TotalUtilPct
	g.VRAMTotalMiB = int(s.DedicatedTotalBytes / mib)
	g.VRAMUsedMiB = int(s.DedicatedUsedBytes / mib)
	g.VRAMFreeMiB = int(d.FreeDedicatedBytes / mib)
	running := len(own) > 0

	if g.Confidence == policy.ConfidenceHigh {
		g.OwnUtilPct = d.OwnUtilPct
		g.ExternalUtilPct = d.ExternalUtilPct
		g.ExternalUtilTrusted = true
		g.OwnVRAMMiB = int(d.OwnDedicatedBytes / mib)
		g.ExternalVRAMMiB = int(d.ExternalDedicatedBytes / mib)
	} else {
		// Degraded (ADR 0003 fallback). Utilization cannot be split, so the
		// whole figure counts as external, and it is trusted only while the
		// runtime is idle, when its own share is negligible.
		g.OwnUtilPct = d.OwnUtilPct
		g.ExternalUtilPct = d.TotalUtilPct
		g.ExternalUtilTrusted = !in.RuntimeActive
		// External VRAM = adapter used minus the runtime's footprint.
		if running {
			fp := in.FootprintMiB
			if measured := int(d.OwnDedicatedBytes / mib); fp == 0 && measured > 0 {
				fp = measured
			}
			if fp == 0 {
				// First load, nothing measured yet: the configured model
				// requirement is the best estimate. Counting the loading
				// model as external would make the worker preempt itself.
				fp = in.Config.Runtime.RequiredFreeVRAMMiB
			}
			g.OwnVRAMMiB = min(fp, g.VRAMUsedMiB)
		}
		g.ExternalVRAMMiB = max(g.VRAMUsedMiB-g.OwnVRAMMiB, 0)
	}

	if o.lastConf != g.Confidence {
		// A change of method (measured vs inferred) is not growth.
		o.vram = o.vram[:0]
	}
	o.lastConf = g.Confidence
	g.ExternalVRAMGrowthMiB = o.growth(now, g.ExternalVRAMMiB, in.Config.Contention.CriticalWindow.D())
	o.hysteresis(now, &g, in)

	var perPID map[uint32]telemetry.ProcGPU
	if len(s.Processes) > 0 {
		perPID = make(map[uint32]telemetry.ProcGPU, len(s.Processes))
		for _, p := range s.Processes {
			perPID[p.PID] = p
		}
	}
	return g, perPID
}

// confidence grades the sample (ADR 0003): none when missing or incomplete;
// high when per-process data is present, the runtime (if running) is visible
// in it, and the per-process dedicated sum matches the adapter figure;
// otherwise degraded.
func confidence(in Input, own map[uint32]bool) policy.Confidence {
	s := in.Sample
	if s == nil || !s.Complete {
		return policy.ConfidenceNone
	}
	if len(s.Processes) == 0 {
		return policy.ConfidenceDegraded
	}
	var sum uint64
	ownSeen := false
	for _, p := range s.Processes {
		sum += p.DedicatedBytes
		if own[p.PID] {
			ownSeen = true
		}
	}
	if len(own) > 0 && !ownSeen {
		return policy.ConfidenceDegraded
	}
	adapter := float64(s.DedicatedUsedBytes) / mib
	diff := adapter - float64(sum)/mib
	if diff < 0 {
		diff = -diff
	}
	if diff > max(consistencyToleranceMiB, adapter*consistencyTolerancePct/100) {
		return policy.ConfidenceDegraded
	}
	return policy.ConfidenceHigh
}

func (o *Observer) trackConfidence(now time.Time, c policy.Confidence, g *policy.GPUFacts) {
	if c == policy.ConfidenceNone {
		o.healthySince = time.Time{}
		if o.noneSince.IsZero() {
			o.noneSince = now
		}
		g.NoneFor = now.Sub(o.noneSince)
		return
	}
	o.noneSince = time.Time{}
	if o.healthySince.IsZero() {
		o.healthySince = now
	}
	g.HealthyFor = now.Sub(o.healthySince)
}

// hysteresis maintains the continuous above/below durations for external
// utilization. Only trusted ticks count: an untrusted tick resets all three
// to zero, so it neither extends an "above" run nor counts as "below".
func (o *Observer) hysteresis(now time.Time, g *policy.GPUFacts, in Input) {
	if !g.ExternalUtilTrusted {
		o.aboveCritSince, o.aboveSoftSince, o.belowSoftSince = time.Time{}, time.Time{}, time.Time{}
		return
	}
	soft := in.Profile.ExternalUtilSoftPct
	if g.Confidence == policy.ConfidenceDegraded {
		// Same tightening the evaluator applies, so AboveSoftFor measures
		// the threshold policy compares against.
		soft *= 1 - float64(in.Config.Telemetry.DegradedMarginPct)/100
	}
	crit := in.Config.Contention.ExternalUtilCriticalPct
	u := g.ExternalUtilPct
	g.AboveCriticalFor = run(&o.aboveCritSince, crit > 0 && u >= crit, now)
	g.AboveSoftFor = run(&o.aboveSoftSince, soft > 0 && u >= soft, now)
	g.BelowSoftFor = run(&o.belowSoftSince, !(soft > 0 && u >= soft), now)
}

// run advances a continuous-condition timer and returns its duration.
func run(since *time.Time, cond bool, now time.Time) time.Duration {
	if !cond {
		*since = time.Time{}
		return 0
	}
	if since.IsZero() {
		*since = now
	}
	return now.Sub(*since)
}

// growth records ext and returns the change since the start of the window.
// The newest point at or before now-window is kept as the anchor, so the
// figure spans the full window once enough history exists.
func (o *Observer) growth(now time.Time, ext int, window time.Duration) int {
	o.vram = append(o.vram, vramPoint{now, ext})
	cut := now.Add(-window)
	i := 0
	for i+1 < len(o.vram) && !o.vram[i+1].t.After(cut) {
		i++
	}
	o.vram = o.vram[i:]
	return ext - o.vram[0].mib
}

func (o *Observer) session(now time.Time, in Input) (policy.SessionFacts, *AgentReport) {
	r := in.Agent
	if r == nil || in.AgentAt.IsZero() || now.Sub(in.AgentAt) >= in.Config.Signals.SessionStaleAfter.D() {
		return policy.SessionFacts{}, nil
	}
	f := policy.SessionFacts{
		Known:          true,
		UserLoggedOn:   true, // the agent runs inside a logged-on session
		Locked:         r.Locked,
		ForegroundName: r.ForegroundName,
		ForegroundPath: r.ForegroundPath,
		ForegroundPID:  r.ForegroundPID,
		Fullscreen:     r.Fullscreen,
		IdleFor:        time.Duration(r.IdleSeconds * float64(time.Second)),
	}
	if r.ForegroundName != "" || r.ForegroundPath != "" {
		f.ForegroundClass, _, _ = o.cls.Match(r.ForegroundName, r.ForegroundPath)
	}
	return f, r
}

func (o *Observer) apps(now time.Time, in Input, own map[uint32]bool, perPID map[uint32]telemetry.ProcGPU, agent *AgentReport) policy.AppFacts {
	var a policy.AppFacts
	if in.Processes == nil {
		return a
	}
	a.ProcessListOK = true

	type classified struct {
		p           signals.Process
		class, rule string
	}
	byPID := make(map[uint32]classified, len(in.Processes))
	for _, p := range in.Processes {
		class, rule, _ := o.cls.Classify(p)
		byPID[p.PID] = classified{p, class, rule}
	}
	build := func(c classified) policy.AppMatch {
		m := policy.AppMatch{PID: c.p.PID, Name: c.p.Name, Path: c.p.Path, Class: c.class, Rule: c.rule}
		if u, ok := perPID[c.p.PID]; ok {
			m.GPUUtilPct = u.MaxUtil()
			m.VRAMMiB = int(u.DedicatedBytes / mib)
		}
		if !c.p.CreateTime.IsZero() && now.After(c.p.CreateTime) {
			m.RunningFor = now.Sub(c.p.CreateTime)
		}
		if agent != nil && agent.ForegroundPID != 0 && agent.ForegroundPID == c.p.PID {
			m.Foreground = true
			m.Fullscreen = agent.Fullscreen
		}
		return m
	}

	for _, p := range in.Processes {
		if own[p.PID] {
			continue
		}
		c := byPID[p.PID]
		switch c.class {
		case config.ClassGame:
			a.Games = append(a.Games, build(c))
		case config.ClassLauncher:
			a.Launchers = append(a.Launchers, build(c))
		case config.ClassOrdinary:
			if m := build(c); m.GPUUtilPct >= ordinaryMinUtilPct || m.VRAMMiB >= ordinaryMinVRAMMiB {
				a.Ordinary = append(a.Ordinary, m)
			}
		case config.ClassIgnore:
		default:
			parent, ok := byPID[p.PPID]
			if !ok || p.PPID == p.PID || parent.class != config.ClassLauncher || own[p.PPID] {
				continue
			}
			// Guard against PID reuse: a real parent predates its child.
			if !parent.p.CreateTime.IsZero() && !p.CreateTime.IsZero() && parent.p.CreateTime.After(p.CreateTime) {
				continue
			}
			m := build(c)
			if m.GPUUtilPct < launcherChildMinUtilPct && m.VRAMMiB < launcherChildMinVRAMMiB {
				continue
			}
			// Inferred: likely an unlisted game started by the launcher.
			m.Class, m.Rule, m.ParentName = config.ClassGame, RuleLauncherChild, parent.p.Name
			a.LauncherChildren = append(a.LauncherChildren, m)
		}
	}
	for _, l := range [][]policy.AppMatch{a.Games, a.Launchers, a.Ordinary, a.LauncherChildren} {
		slices.SortStableFunc(l, byDemand)
	}
	return a
}

// byDemand orders the heaviest GPU user first, then by PID for determinism.
func byDemand(x, y policy.AppMatch) int {
	if c := cmp.Compare(y.GPUUtilPct, x.GPUUtilPct); c != 0 {
		return c
	}
	if c := cmp.Compare(y.VRAMMiB, x.VRAMMiB); c != 0 {
		return c
	}
	return cmp.Compare(x.PID, y.PID)
}
