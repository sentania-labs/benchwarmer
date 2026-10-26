package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

// suspendRig runs the assembled service with a real runtime process
// (fakellama) and no host: the test plays the control handler by sending the
// events the winsvc host would send and waiting on Event.Ack.
type suspendRig struct {
	t       *testing.T
	svc     *Service
	evs     chan winsvc.Event
	runDone chan error
}

func newSuspendRig(t *testing.T, runtimeArgs []string, mod func(*config.Config)) *suspendRig {
	t.Helper()
	dir := t.TempDir()
	fake := buildFakeLlama(t, dir)
	model := filepath.Join(dir, "model.gguf")
	_ = os.WriteFile(model, []byte("fake"), 0o644)
	simCtl := filepath.Join(dir, "sim.json")
	_ = os.WriteFile(simCtl, []byte("{}"), 0o644)
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o755)

	c := config.Default()
	c.Runtime.Executable, c.Runtime.ModelPath = fake, model
	c.Runtime.Args = runtimeArgs
	c.Runtime.Port = freePort(t)
	c.Runtime.RunAs = config.RunAsService
	// Ctrl+C on a shared Windows CI console would also reach the test runner
	// (see TestEndToEndLifecycle); the graceful stop is measured off Windows.
	if goruntime.GOOS == "windows" {
		c.Runtime.StopMode = config.StopKill
	}
	c.Listen.Inference = "127.0.0.1:" + strconv.Itoa(freePort(t))
	c.Listen.Management = "127.0.0.1:" + strconv.Itoa(freePort(t))
	c.Recovery.StartupCooldown = config.Duration(time.Second)
	c.Schedules[0].Enabled = false
	if mod != nil {
		mod(&c)
	}
	b, _ := config.Marshal(c)
	if err := os.WriteFile(filepath.Join(data, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{DataDir: data, SimulateGPU: true, SimControl: simCtl, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	r := &suspendRig{t: t, svc: svc, evs: make(chan winsvc.Event, 4), runDone: make(chan error, 1)}
	go func() { r.runDone <- svc.Run(context.Background(), r.evs) }()
	t.Cleanup(func() {
		select {
		case r.evs <- winsvc.Event{Kind: winsvc.Stop, Deadline: time.Now().Add(10 * time.Second)}:
		default:
		}
		select {
		case <-r.runDone:
		case <-time.After(15 * time.Second):
			t.Error("service did not stop")
		}
	})
	return r
}

func (r *suspendRig) waitState(want state.State, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if r.svc.Controller().Status().State == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	st := r.svc.Controller().Status()
	r.t.Fatalf("state %s, want %s (%s)", st.State, want, st.Summary)
}

// suspend sends a Suspend event as the host would and returns how long the
// host would have held Windows' suspend notification.
func (r *suspendRig) suspend(budget time.Duration) time.Duration {
	r.t.Helper()
	t0 := time.Now()
	done := make(chan struct{})
	r.evs <- winsvc.Event{Kind: winsvc.Suspend, Deadline: t0.Add(budget), Done: done}
	select {
	case <-done:
	case <-time.After(budget + 5*time.Second):
		r.t.Fatal("suspend never acknowledged")
	}
	return time.Since(t0)
}

// event finds the newest event of a type. Events reach the database
// through an asynchronous sink, so it waits a moment for one.
func (r *suspendRig) event(typ events.Type) (events.Event, bool) {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evList, err := r.svc.Controller().Events(apiQuery(200))
		if err != nil {
			r.t.Fatal(err)
		}
		for _, e := range evList { // newest first
			if e.Type == typ {
				return e, true
			}
		}
		if time.Now().After(deadline) {
			return events.Event{}, false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestSuspendStopsRuntimeBeforeAcknowledging is the simulated measurement
// behind ADR 0013: with the runtime serving, how long does the stop take
// from the suspend event to the acknowledgement, and is the runtime gone
// when the acknowledgement happens? Run with -v to see the number.
func TestSuspendStopsRuntimeBeforeAcknowledging(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	r := newSuspendRig(t, []string{"-load-delay", "200ms", "-spawn-child"}, nil)
	r.waitState(state.Ready, 15*time.Second)

	held := r.suspend(winsvc.DefaultSuspendBudget)
	st := r.svc.Controller().Status()
	t.Logf("suspend held %s of a %s budget; state after ack: %s", held.Round(time.Millisecond), winsvc.DefaultSuspendBudget, st.State)
	if st.State.RuntimeRunning() {
		t.Fatalf("runtime still %s when the suspend was acknowledged", st.State)
	}
	if held >= winsvc.DefaultSuspendBudget {
		t.Fatalf("stop took %s, the whole budget", held)
	}
	e, ok := r.event(events.PowerSuspendStop)
	if !ok || e.Data["stopped"] != true {
		t.Fatalf("power_suspend_stop %+v (found %v)", e, ok)
	}
	if s, ok := r.event(events.RuntimeStopped); !ok || s.Rule != "safety.suspend" {
		t.Fatalf("runtime_stopped %+v (found %v)", s, ok)
	}
}

// A runtime that ignores Ctrl+C is not hard-killed to meet the budget: the
// suspend is released at the budget with the stop still in flight, and the
// stop finishes on its own terms (the graceful timeout, then the fallback
// kill), which is the resume-side cleanup.
func TestSuspendDoesNotHardKillAtTheBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	if goruntime.GOOS == "windows" {
		t.Skip("needs a graceful stop, which a shared CI console cannot deliver")
	}
	r := newSuspendRig(t, []string{"-load-delay", "200ms", "-ignore-interrupt"}, func(c *config.Config) {
		c.Runtime.GracefulStopTimeout = config.Duration(3 * time.Second)
	})
	r.waitState(state.Ready, 15*time.Second)

	const budget = 700 * time.Millisecond
	held := r.suspend(budget)
	st := r.svc.Controller().Status()
	t.Logf("suspend held %s of a %s budget; state after ack: %s", held.Round(time.Millisecond), budget, st.State)
	if held < budget-50*time.Millisecond || held > budget+time.Second {
		t.Fatalf("held %s for a %s budget", held, budget)
	}
	if !st.State.RuntimeRunning() {
		t.Fatalf("runtime was killed inside the suspend window (state %s)", st.State)
	}
	if e, ok := r.event(events.PowerSuspendStop); !ok || e.Data["stopped"] != false {
		t.Fatalf("power_suspend_stop %+v (found %v)", e, ok)
	}

	// After "wake": the stop completes and is verified; nothing loads
	// before the resume cooldown.
	r.svc.Controller().Resumed()
	r.waitState(state.Cooldown, 10*time.Second)
	if _, ok := r.event(events.RuntimeStopped); !ok {
		t.Fatal("stop never verified after resume")
	}
}
