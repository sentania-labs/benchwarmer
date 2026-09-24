package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/proxy"
	"github.com/sentania-labs/benchwarmer/internal/runtime"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

// ---- fakes ----

type fakeInst struct {
	pid     int
	url     *url.URL
	ready   chan error
	exited  chan struct{}
	once    sync.Once
	exitErr error
	stopErr error
	mu      sync.Mutex
	stopped bool
	started time.Time
}

func (f *fakeInst) PID() int          { return f.pid }
func (f *fakeInst) Members() []int    { return []int{f.pid} }
func (f *fakeInst) BaseURL() *url.URL { return f.url }
func (f *fakeInst) WaitReady(ctx context.Context) error {
	select {
	case err := <-f.ready:
		return err
	case <-f.exited:
		return errors.New("exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeInst) Stop(time.Duration) (runtime.StopResult, error) {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	already := false
	select {
	case <-f.exited:
		already = true
	default:
	}
	f.once.Do(func() { close(f.exited) })
	f.mu.Lock()
	err := f.stopErr
	f.mu.Unlock()
	return runtime.StopResult{AlreadyExited: already, RootExit: time.Millisecond, TreeEmpty: 2 * time.Millisecond}, err
}
func (f *fakeInst) Exited() <-chan struct{} { return f.exited }
func (f *fakeInst) ExitErr() error          { return f.exitErr }
func (f *fakeInst) Diagnostics() string     { return "fake diagnostics" }
func (f *fakeInst) Started() time.Time      { return f.started }
func (f *fakeInst) crash(err error) {
	f.exitErr = err
	f.once.Do(func() { close(f.exited) })
}

type fakeAdapter struct {
	mu         sync.Mutex
	url        *url.URL
	insts      []*fakeInst
	reconciles int
	startErr   error
	autoReady  bool
}

func (a *fakeAdapter) Validate(config.Runtime) error { return nil }
func (a *fakeAdapter) Start(context.Context, config.Runtime) (runtime.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startErr != nil {
		return nil, a.startErr
	}
	i := &fakeInst{pid: 1000 + len(a.insts), url: a.url, ready: make(chan error, 1), exited: make(chan struct{})}
	if a.autoReady {
		i.ready <- nil
	}
	a.insts = append(a.insts, i)
	return i, nil
}
func (a *fakeAdapter) Reconcile(config.Runtime) ([]runtime.Orphan, error) {
	a.mu.Lock()
	a.reconciles++
	a.mu.Unlock()
	return nil, nil
}
func (a *fakeAdapter) last() *fakeInst {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.insts) == 0 {
		return nil
	}
	return a.insts[len(a.insts)-1]
}
func (a *fakeAdapter) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.insts) }

// fakeFacts returns whatever the test sets, adding own VRAM while running.
type fakeFacts struct {
	mu   sync.Mutex
	gpu  policy.GPUFacts
	apps policy.AppFacts
}

func (f *fakeFacts) Build(_ time.Time, in FactInput) (policy.GPUFacts, policy.AppFacts, policy.SessionFacts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.gpu
	if len(in.OwnPIDs) > 0 {
		g.OwnVRAMMiB = 12500
		g.VRAMUsedMiB += 12500
		g.VRAMFreeMiB -= 12500
	}
	return g, f.apps, policy.SessionFacts{}
}
func (f *fakeFacts) AgentReport(time.Time, api.AgentReport)               {}
func (f *fakeFacts) AgentStatus(time.Time, time.Duration) api.AgentStatus { return api.AgentStatus{} }
func (f *fakeFacts) set(fn func(g *policy.GPUFacts, a *policy.AppFacts)) {
	f.mu.Lock()
	fn(&f.gpu, &f.apps)
	f.mu.Unlock()
}

type sink struct {
	mu sync.Mutex
	ev []events.Event
}

func (s *sink) Emit(e events.Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }
func (s *sink) types() []events.Type {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.Type, len(s.ev))
	for i, e := range s.ev {
		out[i] = e.Type
	}
	return out
}
func (s *sink) find(t events.Type) (events.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.ev) - 1; i >= 0; i-- {
		if s.ev[i].Type == t {
			return s.ev[i], true
		}
	}
	return events.Event{}, false
}
func (s *sink) count(t events.Type) int {
	n := 0
	for _, x := range s.types() {
		if x == t {
			n++
		}
	}
	return n
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// upstream is a fake runtime HTTP server whose streams finish on demand.
type upstream struct {
	srv     *httptest.Server
	release chan struct{}
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{release: make(chan struct{})}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"n\":0}\n\n")
		fl.Flush()
		select {
		case <-u.release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// ---- rig ----

var chicago, _ = time.LoadLocation("America/Chicago")

type rig struct {
	t       *testing.T
	c       *Controller
	clk     *clock
	ad      *fakeAdapter
	facts   *fakeFacts
	ev      *sink
	gate    *proxy.Gate
	proxy   *httptest.Server
	up      *upstream
	cfg     config.Config
	persist *memPersist
}

type memPersist struct {
	mu sync.Mutex
	p  Persisted
	ok bool
}

func (m *memPersist) SaveControllerState(p Persisted) error {
	m.mu.Lock()
	m.p, m.ok = p, true
	m.mu.Unlock()
	return nil
}
func (m *memPersist) LoadControllerState() (Persisted, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.p, m.ok, nil
}

func quietGPU() policy.GPUFacts {
	return policy.GPUFacts{Confidence: policy.ConfidenceHigh, HealthyFor: time.Hour, TotalUtilPct: 2, ExternalUtilPct: 2,
		ExternalUtilTrusted: true, BelowSoftFor: time.Hour, VRAMTotalMiB: 16304, VRAMUsedMiB: 900, VRAMFreeMiB: 15404, ExternalVRAMMiB: 900}
}

func newRig(t *testing.T, persist *memPersist) *rig {
	t.Helper()
	cfg := config.Default()
	cfg.Recovery.StartupCooldown = config.Duration(2 * time.Minute)
	r := &rig{t: t, cfg: cfg, clk: &clock{t: time.Date(2026, 9, 23, 19, 0, 0, 0, chicago)}, ev: &sink{}, persist: persist}
	r.up = newUpstream(t)
	u, _ := url.Parse(r.up.srv.URL)
	r.ad = &fakeAdapter{url: u, autoReady: true}
	r.facts = &fakeFacts{gpu: quietGPU(), apps: policy.AppFacts{ProcessListOK: true}}
	r.gate = proxy.NewGate(r.clk.Now)
	r.proxy = httptest.NewServer(proxy.New(proxy.Options{Gate: r.gate, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(r.proxy.Close)
	d := Deps{Config: cfg, ConfigSource: "primary", Adapter: r.ad, Facts: r.facts, Gate: r.gate, Events: r.ev, Now: r.clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if persist != nil {
		d.Persist = persist
	}
	r.c = New(d)
	r.c.Start()
	return r
}

func (r *rig) state() state.State {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.st
}

// stepUntil steps (with real time for goroutines) until the state is want.
func (r *rig) stepUntil(want state.State) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.c.Step(true)
		if r.state() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.t.Fatalf("state %s, want %s; decision %q (%s); events %v", r.state(), want, r.c.decision.Rule, r.c.decision.Reason, r.ev.types())
}

func (r *rig) toReady() {
	r.t.Helper()
	r.c.Step(true)
	if r.state() != state.Cooldown {
		r.t.Fatalf("startup: state %s (%s)", r.state(), r.c.decision.Rule)
	}
	r.clk.Advance(2*time.Minute + time.Second)
	r.stepUntil(state.Ready)
}

type stream struct {
	resp *http.Response
	done chan streamResult
}

type streamResult struct {
	sawDone bool
	err     error
}

func (r *rig) startStream() *stream {
	r.t.Helper()
	resp, err := http.Post(r.proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		r.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		r.t.Fatalf("stream status %d: %s", resp.StatusCode, b)
	}
	s := &stream{resp: resp, done: make(chan streamResult, 1)}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil { // first chunk arrived: request is active
		r.t.Fatal(err)
	}
	go func() {
		defer resp.Body.Close()
		b, err := io.ReadAll(br)
		s.done <- streamResult{sawDone: strings.Contains(string(b), "[DONE]"), err: err}
	}()
	return s
}

func (r *rig) post() int {
	resp, err := http.Post(r.proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func game(name string) policy.AppMatch {
	return policy.AppMatch{PID: 7000, Name: name, Class: config.ClassGame, Rule: "Steam library"}
}

// ---- tests ----

// The first meaningful milestone: load, serve, detect a competing workload,
// stop admitting, drain, unload, release VRAM, cool down, and reload, with
// every decision visible in events and status.
func TestMilestoneLoop(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	if !r.gate.IsOpen() {
		t.Fatal("gate closed while ready")
	}

	s := r.startStream()
	r.c.Step(true)
	if r.state() != state.Busy {
		t.Fatalf("state %s, want BUSY", r.state())
	}

	// A game starts.
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("eldenring.exe")} })
	r.c.Step(true)
	if r.state() != state.Draining {
		t.Fatalf("state %s, want DRAINING", r.state())
	}
	if code := r.post(); code != 503 {
		t.Fatalf("new request got %d during drain, want 503", code)
	}
	st := r.c.Status()
	if st.Condition != state.Yielding || st.Timers.GraceUntil == nil || st.Decision.Rule != policy.RuleGameProcess {
		t.Fatalf("status during drain: %+v", st)
	}

	// The active turn finishes within grace.
	close(r.up.release)
	if res := <-s.done; !res.sawDone {
		t.Fatalf("active request should complete within grace: %+v", res)
	}
	r.stepUntil(state.Cooldown)
	for _, want := range []events.Type{events.DrainStarted, events.DrainCompleted, events.RuntimeStopped, events.CooldownStarted} {
		if r.ev.count(want) == 0 {
			t.Fatalf("missing %s in %v", want, r.ev.types())
		}
	}
	if r.ev.count(events.Preempted) != 0 {
		t.Fatal("request finished within grace but a forced preemption was recorded")
	}
	r.c.Step(true)
	if _, ok := r.ev.find(events.VRAMReleased); !ok {
		t.Fatalf("VRAM release not confirmed: %v", r.ev.types())
	}

	// Game still running for a while: stays unavailable, cooldown keeps resetting.
	r.clk.Advance(10 * time.Minute)
	r.c.Step(true)
	if r.state() != state.Cooldown || r.ad.count() != 1 {
		t.Fatalf("reloaded while game running: %s", r.state())
	}

	// Game exits; the 5 minute normal cooldown runs from that moment.
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = nil })
	r.clk.Advance(time.Second)
	r.c.Step(true)
	st = r.c.Status()
	if st.Decision.Rule != policy.RuleCooldown || st.Timers.NextLoadAt == nil {
		t.Fatalf("expected cooldown with next load time: %+v", st.Decision)
	}
	if !strings.Contains(st.Summary, "next load") {
		t.Fatalf("summary lacks next load time: %q", st.Summary)
	}
	r.clk.Advance(4 * time.Minute)
	r.c.Step(true)
	if r.state() != state.Cooldown {
		t.Fatalf("loaded before cooldown expired: %s", r.state())
	}
	r.clk.Advance(time.Minute)
	r.stepUntil(state.Ready)
	if r.ad.count() != 2 {
		t.Fatalf("instances started: %d", r.ad.count())
	}
	if r.ad.reconciles < 3 { // startup + before each load
		t.Fatalf("reconcile calls %d", r.ad.reconciles)
	}
}

func TestGraceExpiryForcesPreemption(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	s := r.startStream()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.c.Step(true)
	if r.state() != state.Draining {
		t.Fatalf("state %s", r.state())
	}
	r.clk.Advance(14 * time.Second)
	r.c.Step(true)
	if r.state() != state.Draining {
		t.Fatalf("preempted before grace: %s", r.state())
	}
	r.clk.Advance(2 * time.Second)
	r.stepUntil(state.Cooldown)
	if res := <-s.done; res.sawDone {
		t.Fatal("request should have been cut")
	}
	e, ok := r.ev.find(events.Preempted)
	if !ok || !strings.Contains(e.Message, "grace expired") {
		t.Fatalf("preempted event: %+v", e)
	}
	if !r.ad.last().stopped {
		t.Fatal("runtime not stopped")
	}
}

func TestHardContentionBeatsGrace(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	s := r.startStream()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.c.Step(true)
	r.clk.Advance(2 * time.Second)
	r.facts.set(func(g *policy.GPUFacts, _ *policy.AppFacts) { g.ExternalVRAMMiB = 4000 })
	r.stepUntil(state.Cooldown)
	if res := <-s.done; res.sawDone {
		t.Fatal("request should have been cut by hard contention")
	}
	e, ok := r.ev.find(events.Preempted)
	if !ok || e.Rule != policy.RuleCriticalVRAMExt {
		// Reported once as flaky by another worker; not reproduced in 300+
		// runs. Print the full trail if it recurs.
		t.Fatalf("preempted event found=%v rule=%q; events %v", ok, e.Rule, r.ev.types())
	}
}

func TestIdleYieldStopsImmediately(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.c.Step(true)
	if s := r.state(); s != state.Preempting && s != state.Cooldown {
		t.Fatalf("idle yield should skip draining, got %s", s)
	}
	r.stepUntil(state.Cooldown)
	if r.ev.count(events.DrainStarted) != 0 {
		t.Fatal("drain started with no active request")
	}
}

func TestRepeatedPreemptionSuppresses(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	for i := 0; i < 2; i++ {
		r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
		r.c.Step(true)
		if i == 0 {
			r.stepUntil(state.Cooldown)
			r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = nil })
			r.clk.Advance(5*time.Minute + time.Second)
			r.stepUntil(state.Ready)
		}
	}
	r.stepUntil(state.Suppressed)
	if _, ok := r.ev.find(events.SuppressionStarted); !ok {
		t.Fatalf("no suppression event: %v", r.ev.types())
	}
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = nil })
	r.clk.Advance(10 * time.Minute)
	r.c.Step(true)
	st := r.c.Status()
	if r.state() != state.Suppressed || st.Timers.SuppressedUntil == nil || st.Decision.Rule != policy.RuleSuppressed {
		t.Fatalf("suppression not held/visible: %s %+v", r.state(), st.Timers)
	}
	r.clk.Advance(21 * time.Minute)
	r.stepUntil(state.Ready)
}

func TestCrashUsesBoundedBackoff(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.ad.last().crash(errors.New("exit status 3"))
	r.stepUntil(state.Error)
	if _, ok := r.ev.find(events.RuntimeCrashed); !ok {
		t.Fatal("no crash event")
	}
	st := r.c.Status()
	if st.Decision.Rule != policy.RuleRecovery || st.Timers.RecoveryUntil == nil {
		t.Fatalf("no backoff: %+v", st.Decision)
	}
	first := st.Timers.RecoveryUntil.Sub(r.clk.Now())
	if first != 30*time.Second {
		t.Fatalf("first backoff %s", first)
	}
	r.clk.Advance(29 * time.Second)
	r.c.Step(true)
	if r.ad.count() != 1 {
		t.Fatal("restarted before backoff elapsed")
	}
	r.clk.Advance(2 * time.Second)
	r.stepUntil(state.Ready)
	r.ad.last().crash(errors.New("exit status 3"))
	r.stepUntil(state.Error)
	if d := r.c.Status().Timers.RecoveryUntil.Sub(r.clk.Now()); d != time.Minute {
		t.Fatalf("second backoff %s, want 1m", d)
	}
}

func TestLoadFailureBacksOff(t *testing.T) {
	r := newRig(t, nil)
	r.ad.autoReady = false
	r.c.Step(true)
	r.clk.Advance(2*time.Minute + time.Second)
	r.stepUntil(state.Loading)
	r.ad.last().ready <- errors.New("model file corrupt")
	r.stepUntil(state.Error)
	if _, ok := r.ev.find(events.LoadFailed); !ok {
		t.Fatal("no load failure event")
	}
	if r.gate.IsOpen() {
		t.Fatal("gate opened on failed load")
	}
}

func TestPauseAndResume(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	if _, err := r.c.SetMode(api.ModeRequest{Mode: policy.ModePause, Duration: "1h", SetBy: "tray"}); err != nil {
		t.Fatal(err)
	}
	r.stepUntil(state.Disabled)
	if r.post() != 503 {
		t.Fatal("paused worker admitted a request")
	}
	if r.ev.count(events.CooldownStarted) != 0 || len(r.c.timers.Preemptions) != 0 {
		t.Fatal("manual pause counted as competing or preemption")
	}
	r.clk.Advance(time.Hour)
	r.stepUntil(state.Ready)
	if _, ok := r.ev.find(events.ModeExpired); !ok {
		t.Fatal("no mode expiry event")
	}
}

func TestAIPriorityStillYieldsToCriticalContention(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	if _, err := r.c.SetMode(api.ModeRequest{Mode: policy.ModeAIPriority, Duration: "2h"}); err != nil {
		t.Fatal(err)
	}
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.c.Step(true)
	if r.state() != state.Ready {
		t.Fatalf("AI priority should tolerate an unconfirmed game: %s", r.state())
	}
	r.facts.set(func(g *policy.GPUFacts, _ *policy.AppFacts) { g.ExternalVRAMMiB = 4000 })
	r.stepUntil(state.Cooldown)
}

func TestTelemetryLossReleasesGPU(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.facts.set(func(g *policy.GPUFacts, _ *policy.AppFacts) {
		g.Confidence, g.NoneFor, g.ExternalUtilTrusted = policy.ConfidenceNone, 11*time.Second, false
	})
	r.stepUntil(state.Error)
	if _, ok := r.ev.find(events.TelemetryLost); !ok {
		t.Fatal("no telemetry lost event")
	}
	r.facts.set(func(g *policy.GPUFacts, _ *policy.AppFacts) { *g = quietGPU() })
	r.c.Step(true)
	st := r.c.Status()
	if st.Timers.RecoveryUntil == nil || !strings.Contains(r.c.timers.RecoveryReason, "telemetry") {
		t.Fatalf("no telemetry recovery cooldown: %+v", st.Timers)
	}
	r.clk.Advance(2*time.Minute + time.Second)
	r.stepUntil(state.Ready)
}

func TestSuspendAndResume(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.c.PrepareSuspend(2 * time.Second)
	if r.state().RuntimeRunning() {
		t.Fatalf("runtime running after suspend: %s", r.state())
	}
	r.c.Resumed()
	r.c.Step(true)
	if r.state().RuntimeRunning() {
		t.Fatal("loaded immediately after resume")
	}
	r.clk.Advance(3*time.Minute + time.Second)
	r.stepUntil(state.Ready)
}

func TestShutdownStopsRuntime(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.c.Shutdown(2 * time.Second)
	if !r.ad.last().stopped || r.state().RuntimeRunning() {
		t.Fatalf("runtime not stopped on shutdown: %s", r.state())
	}
}

func TestRestartRestoresTimersAndAppliesStartupCooldown(t *testing.T) {
	p := &memPersist{}
	r := newRig(t, p)
	r.toReady()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.stepUntil(state.Cooldown)
	if _, err := r.c.SetMode(api.ModeRequest{Mode: policy.ModePause, Duration: "4h"}); err != nil {
		t.Fatal(err)
	}
	r.c.Step(true)

	// A new controller on the same persisted state.
	r2 := newRig(t, p)
	r2.clk.t = r.clk.Now()
	if r2.c.mode.Mode != policy.ModePause || len(r2.c.timers.Preemptions) != 1 {
		t.Fatalf("state not restored: mode=%s preemptions=%d", r2.c.mode.Mode, len(r2.c.timers.Preemptions))
	}
	if r2.c.timers.RecoveryReason != "startup" {
		t.Fatalf("no startup cooldown: %q", r2.c.timers.RecoveryReason)
	}
}

func TestUntilRebootModeDoesNotSurviveRestart(t *testing.T) {
	p := &memPersist{}
	r := newRig(t, p)
	if _, err := r.c.SetMode(api.ModeRequest{Mode: policy.ModePause, Duration: "until_reboot"}); err != nil {
		t.Fatal(err)
	}
	r2 := newRig(t, p)
	if r2.c.mode.Mode != policy.ModeAuto {
		t.Fatalf("until_reboot survived: %s", r2.c.mode.Mode)
	}
}

func TestRuntimeConfigChangeReloads(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	nc := config.Clone(r.cfg)
	nc.Runtime.ContextSize = 4096
	im, err := r.c.UpdateConfig(context.Background(), nc, "test")
	if err != nil || !im.RuntimeReload {
		t.Fatalf("impact %+v err %v", im, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(r.ad.count() == 2 && r.state() == state.Ready) {
		r.c.Step(true)
		time.Sleep(2 * time.Millisecond)
	}
	if r.ad.count() != 2 || r.state() != state.Ready {
		t.Fatalf("expected a reload, instances=%d", r.ad.count())
	}
	if len(r.c.timers.Preemptions) != 0 {
		t.Fatal("reload counted as preemption")
	}
}

func TestInvalidConfigRejectedAndAudited(t *testing.T) {
	r := newRig(t, nil)
	nc := config.Clone(r.cfg)
	nc.Runtime.Host = "0.0.0.0"
	if _, err := r.c.UpdateConfig(context.Background(), nc, "test"); err == nil {
		t.Fatal("accepted unsafe config")
	}
	if _, ok := r.ev.find(events.ConfigRejected); !ok {
		t.Fatal("rejection not audited")
	}
	if cfg, _ := r.c.Config(); cfg.Runtime.Host != "127.0.0.1" {
		t.Fatal("unsafe config applied")
	}
}

func TestEveryStateChangeCarriesRule(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.stepUntil(state.Cooldown)
	r.ev.mu.Lock()
	defer r.ev.mu.Unlock()
	for _, e := range r.ev.ev {
		if e.Type == events.StateChanged && (e.Rule == "" || e.Message == "" || e.PrevState == "") {
			t.Errorf("state change without rule/explanation: %+v", e)
		}
	}
}

func TestFailedKillIsRetriedAndBlocksNewRuntime(t *testing.T) {
	r := newRig(t, nil)
	r.toReady()
	inst := r.ad.last()
	inst.mu.Lock()
	inst.stopErr = errors.New("descendants still present after timeout")
	inst.mu.Unlock()
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = []policy.AppMatch{game("x.exe")} })
	r.stepUntil(state.Error)
	if _, ok := r.ev.find(events.KillFailed); !ok {
		t.Fatal("no kill_failed event")
	}
	// The game leaves and every timer expires, but the old runtime is
	// unverified: nothing new may start.
	r.facts.set(func(_ *policy.GPUFacts, a *policy.AppFacts) { a.Games = nil })
	r.clk.Advance(6 * time.Minute)
	for i := 0; i < 20; i++ {
		r.c.Step(true)
		time.Sleep(2 * time.Millisecond)
	}
	if r.ad.count() != 1 {
		t.Fatalf("started a second runtime while the first was unverified (%d)", r.ad.count())
	}
	if st := r.c.Status(); st.Decision.Rule != "safety.runtime_unverified" {
		t.Fatalf("decision %q", st.Decision.Rule)
	}
	// The kill starts working; the retry verifies it and loading resumes.
	inst.mu.Lock()
	inst.stopErr = nil
	inst.mu.Unlock()
	r.clk.Advance(5 * time.Minute)
	r.stepUntil(state.Ready)
	if r.ad.count() != 2 {
		t.Fatalf("instances %d", r.ad.count())
	}
}
