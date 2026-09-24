package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/controller"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/metrics"
	"github.com/sentania-labs/benchwarmer/internal/proxy"
	"github.com/sentania-labs/benchwarmer/internal/runtime/llamacpp"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/simgpu"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/store"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
	"github.com/sentania-labs/benchwarmer/internal/version"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

// Options configures a service instance.
type Options struct {
	// DataDir holds config.json, the database, secrets, and logs.
	DataDir string
	// SimulateGPU enables the development GPU simulator; SimControl is its
	// control file. Never used on the target.
	SimulateGPU bool
	SimControl  string
	Log         *slog.Logger
	// UI serves the web dashboard at "/" on the management listener.
	UI http.Handler
}

// Service is the assembled worker.
type Service struct {
	o      Options
	log    *slog.Logger
	store  *store.Store
	sink   *store.EventSink
	ctl    *controller.Controller
	cfg    config.Config
	gate   *proxy.Gate
	met    *metrics.Metrics
	tel    telemetry.Source
	auth   *api.Authenticator
	infTok string
}

// New loads configuration and opens persistence. It does not start anything.
func New(o Options) (*Service, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Service{o: o, log: o.Log}

	cs := config.NewStore(filepath.Join(o.DataDir, "config.json"))
	lr, err := cs.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if lr.PrimaryErr != nil {
		s.log.Warn("config.json unusable; using fallback", "source", lr.Source, "err", lr.PrimaryErr)
	}
	s.cfg = lr.Config

	st, err := store.Open(filepath.Join(o.DataDir, "benchwarmer.db"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s.store = st
	s.sink = store.NewEventSink(st, 1024)

	files := secrets.FilesFrom(s.cfg.Security)
	toks, err := secrets.EnsureTokens(o.DataDir, files)
	if err != nil {
		return nil, fmt.Errorf("tokens: %w", err)
	}
	s.auth = api.NewAuthenticator(toks)
	s.infTok = toks.Inference

	s.met = metrics.New()
	s.met.SetBuildInfo(version.Version)
	s.met.ObserveEventSink(s.sink.Dropped, s.sink.Failed)
	s.gate = proxy.NewGate(nil)

	procs := signals.Snapshot
	switch {
	case o.SimulateGPU:
		s.tel = simgpu.New(o.SimControl, s.cfg.Runtime.Executable, procs)
	default:
		t, err := newPlatformTelemetry(s.cfg.Telemetry.Adapter)
		if err != nil {
			// Not fatal: without telemetry the policy refuses to load and
			// the status explains why. Collection is retried on rebuild.
			s.log.Error("GPU telemetry unavailable", "err", err)
			s.tel = unavailableTelemetry{err: err}
		} else {
			s.tel = t
		}
	}

	facts := NewFacts()
	s.ctl = controller.New(controller.Deps{
		Config: s.cfg, ConfigSource: lr.Source, ConfigStore: auditingStore{cs: cs, st: st, met: s.met},
		Adapter: llamacpp.New(), Telemetry: s.tel, Processes: procs, Facts: facts, Gate: s.gate,
		Events: events.SinkFunc(s.sink.Emit), EventReader: eventReader{st}, Persist: persister{st},
		Metrics: s.met, Log: s.log, Version: version.Version, BootTime: signals.BootTime,
	})
	return s, nil
}

// Controller exposes the controller (for tests and the service runner).
func (s *Service) Controller() *controller.Controller { return s.ctl }

// Run serves until ctx ends or a stop event arrives, then releases the GPU
// within the event's deadline.
func (s *Service) Run(ctx context.Context, evs <-chan winsvc.Event) error {
	s.ctl.Start()
	infLn, err := net.Listen("tcp", s.cfg.Listen.Inference)
	if err != nil {
		return fmt.Errorf("inference listener %s: %w", s.cfg.Listen.Inference, err)
	}
	mgmtLn, err := net.Listen("tcp", s.cfg.Listen.Management)
	if err != nil {
		infLn.Close()
		return fmt.Errorf("management listener %s: %w", s.cfg.Listen.Management, err)
	}
	inf := &http.Server{Handler: proxy.New(proxy.Options{
		Gate:         s.gate,
		MaxDuration:  func() time.Duration { c, _ := s.ctl.Config(); return c.Runtime.MaxRequestDuration.D() },
		Token:        func() string { return s.infTok },
		RequireToken: func() bool { c, _ := s.ctl.Config(); return c.Security.RequireInferenceToken },
		Condition:    s.ctl.Condition,
		Observer:     requestObserver{s.met},
		Log:          s.log,
	}), ReadHeaderTimeout: 10 * time.Second}
	mux := http.NewServeMux()
	apiH := api.New(api.Options{Backend: s.ctl, Auth: s.auth, Metrics: s.met.Handler(), Version: version.Version, Logger: s.log})
	mux.Handle("/api/", apiH)
	mux.Handle("/metrics", apiH)
	if s.o.UI != nil {
		mux.Handle("/", s.o.UI)
	}
	mgmt := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	var wg sync.WaitGroup
	serve := func(srv *http.Server, ln net.Listener, name string) {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("listener failed", "listener", name, "err", err)
		}
	}
	wg.Add(2)
	go serve(inf, infLn, "inference")
	go serve(mgmt, mgmtLn, "management")
	s.log.Info("benchwarmer running", "inference", infLn.Addr().String(), "management", mgmtLn.Addr().String(), "version", version.Version)

	runCtx, cancel := context.WithCancel(context.Background())
	ctlDone := make(chan struct{})
	go func() { s.ctl.Run(runCtx); close(ctlDone) }()
	go s.housekeeping(runCtx)

	var deadline time.Time
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case e, ok := <-evs:
			if !ok {
				break loop
			}
			switch e.Kind {
			case winsvc.Stop, winsvc.Shutdown:
				deadline = e.Deadline
				break loop
			case winsvc.Suspend:
				s.ctl.PrepareSuspend(5 * time.Second)
			case winsvc.Resume:
				s.ctl.Resumed()
			case winsvc.SessionChange:
				s.log.Info("session change", "kind", e.Session, "session", e.SessionID)
			}
		}
	}

	// Release the GPU first: stop admitting, terminate the runtime. The
	// budget runs from the stop request, not from service start.
	if deadline.IsZero() {
		deadline = time.Now().Add(20 * time.Second)
	}
	budget := time.Until(deadline) - 2*time.Second
	if budget < time.Second {
		budget = time.Second
	}
	cancel()
	<-ctlDone
	s.ctl.Shutdown(budget)
	shCtx, shCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shCancel()
	_ = inf.Shutdown(shCtx)
	_ = mgmt.Shutdown(shCtx)
	wg.Wait()
	return s.Close()
}

// housekeeping prunes history hourly and records availability statistics.
func (s *Service) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	last := time.Now()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			st := s.ctl.Status()
			_ = s.store.AddAvailability(last, now, st.Condition == state.Available)
			last = now
			if now.Sub(lastPrune) >= time.Hour {
				c, _ := s.ctl.Config()
				if err := s.store.Prune(now, c.Retention); err != nil {
					s.log.Warn("prune failed", "err", err)
				}
				lastPrune = now
			}
		}
	}
}

// Close flushes events and closes the database.
func (s *Service) Close() error {
	if s.tel != nil {
		s.tel.Close()
	}
	s.sink.Close()
	return s.store.Close()
}

// ServiceArgs are the arguments the SCM passes to the service binary.
func ServiceArgs(dataDir string) []string { return []string{"run", "--service", "--data", dataDir} }

// DefaultDataDir is %ProgramData%\Benchwarmer on Windows, ./data elsewhere.
func DefaultDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Benchwarmer")
	}
	return "data"
}

// unavailableTelemetry reports incomplete samples so policy fails safe.
type unavailableTelemetry struct{ err error }

func (u unavailableTelemetry) Collect() telemetry.Sample {
	return telemetry.Sample{Time: time.Now(), Errors: []string{u.err.Error()}}
}
func (u unavailableTelemetry) Rebuild() error { return u.err }
func (u unavailableTelemetry) Close()         {}

// persister maps controller state onto the store's documents.
type persister struct{ st *store.Store }

func (p persister) SaveControllerState(c controller.Persisted) error {
	if err := p.st.SaveModeState(store.ModeState{Mode: c.Mode.Mode, Until: c.Mode.Until, SetBy: c.Mode.SetBy, SetAt: c.ModeSetAt, UntilReboot: c.UntilReboot}); err != nil {
		return err
	}
	if err := p.st.SaveTimers(store.Timers{Preemptions: c.Timers.Preemptions, SuppressedAt: c.Timers.SuppressedAt,
		LastCompetingAt: c.Timers.LastCompetingAt, LastCompetingRule: c.Timers.LastCompetingRule, CrashCount: c.CrashCount}); err != nil {
		return err
	}
	return p.st.SaveRuntimeMemo(store.RuntimeMemo{FootprintMiB: c.FootprintMiB, LastLoadSeconds: c.LastLoadSeconds, MeasuredAt: time.Now()})
}

func (p persister) LoadControllerState() (controller.Persisted, bool, error) {
	var out controller.Persisted
	m, okM, err := p.st.ModeState()
	if err != nil {
		return out, false, err
	}
	t, okT, err := p.st.Timers()
	if err != nil {
		return out, false, err
	}
	r, okR, err := p.st.RuntimeMemo()
	if err != nil {
		return out, false, err
	}
	if okM {
		out.Mode.Mode, out.Mode.Until, out.Mode.SetBy, out.ModeSetAt, out.UntilReboot = m.Mode, m.Until, m.SetBy, m.SetAt, m.UntilReboot
	}
	if okT {
		out.Timers.Preemptions, out.Timers.SuppressedAt = t.Preemptions, t.SuppressedAt
		out.Timers.LastCompetingAt, out.Timers.LastCompetingRule = t.LastCompetingAt, t.LastCompetingRule
		out.CrashCount = t.CrashCount
	}
	if okR {
		out.FootprintMiB, out.LastLoadSeconds = r.FootprintMiB, r.LastLoadSeconds
	}
	return out, okM || okT || okR, nil
}

type eventReader struct{ st *store.Store }

func (e eventReader) Events(q api.EventQuery) ([]events.Event, error) {
	return e.st.Events(store.EventQuery{Limit: q.Limit, BeforeID: q.BeforeID, Types: q.Types})
}

// auditingStore persists config and records the change in history.
type auditingStore struct {
	cs  *config.Store
	st  *store.Store
	met *metrics.Metrics
}

func (a auditingStore) Save(c config.Config) error {
	if err := a.cs.Save(c); err != nil {
		a.met.IncConfigChange("rejected")
		return err
	}
	a.met.IncConfigChange("applied")
	// The config is already persisted; a history failure must not make the
	// caller think the change was not applied (the file would then differ
	// from the running config after the next restart).
	if _, err := a.st.AppendConfigChange(time.Now(), "api", config.Redact(c), config.Impact{}); err != nil {
		slog.Warn("config history not recorded", "err", err)
	}
	return nil
}

type requestObserver struct{ m *metrics.Metrics }

func (r requestObserver) RequestFinished(_ string, d time.Duration, outcome string) {
	r.m.ObserveRequestSeconds(outcome, d.Seconds())
}
func (r requestObserver) RequestRejected() { r.m.IncRequestsRejected() }
