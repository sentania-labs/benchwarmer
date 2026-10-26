package winsvc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recorder collects what the supervisor reports to the SCM.
type recorder struct {
	mu sync.Mutex
	st []status
}

func (r *recorder) report(s status) { r.mu.Lock(); r.st = append(r.st, s); r.mu.Unlock() }
func (r *recorder) all() []status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]status(nil), r.st...)
}

// runPlane starts supervise with h and returns the plane, the recorder, and
// a channel carrying supervise's result.
func runPlane(t *testing.T, o Options, h Handler) (*plane, *recorder, <-chan bool) {
	t.Helper()
	p := newPlane(o)
	rec := &recorder{}
	out := make(chan bool, 1)
	go func() { out <- p.supervise(h, rec.report) }()
	return p, rec, out
}

func within(t *testing.T, d time.Duration, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// goControl runs a control request on its own goroutine and closes the
// returned channel when the control handler returns.
func goControl(p *plane, cmd, evt, sid uint32) <-chan struct{} {
	done := make(chan struct{})
	go func() { p.control(cmd, evt, sid); close(done) }()
	return done
}

func stopRun(t *testing.T, p *plane, out <-chan bool) {
	t.Helper()
	p.control(ctlStop, 0, 0)
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after stop")
	}
}

// The control handler must not return from a suspend notification until the
// Handler has acknowledged it: that is what keeps Windows from sleeping
// before the runtime is stopped.
func TestSuspendIsHeldUntilAcknowledged(t *testing.T) {
	got := make(chan Event, 1)
	release := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		for {
			select {
			case e := <-ev:
				if e.Kind == Suspend {
					got <- e
					<-release
					e.Ack()
				}
			case <-ctx.Done():
				return nil
			}
		}
	})
	p, _, out := runPlane(t, Options{SuspendBudget: 5 * time.Second}, h)

	returned := goControl(p, ctlPowerEvent, pbtAPMSuspend, 0)
	e := <-got
	if e.Done == nil || e.Deadline.IsZero() {
		t.Fatalf("suspend event carries no ack or deadline: %+v", e)
	}
	select {
	case <-returned:
		t.Fatal("control returned before the Handler acknowledged the suspend")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	within(t, time.Second, "control to return after the ack", returned)
	stopRun(t, p, out)
}

// A Handler that never acknowledges cannot hold the system awake past the
// budget.
func TestSuspendIsReleasedAtTheBudget(t *testing.T) {
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		for {
			select {
			case <-ev: // never acknowledged
			case <-ctx.Done():
				return nil
			}
		}
	})
	p, _, out := runPlane(t, Options{SuspendBudget: 80 * time.Millisecond}, h)
	t0 := time.Now()
	within(t, 2*time.Second, "control to give up at the budget", goControl(p, ctlPowerEvent, pbtAPMSuspend, 0))
	if d := time.Since(t0); d < 80*time.Millisecond || d > time.Second {
		t.Fatalf("held for %s with an 80ms budget", d)
	}
	stopRun(t, p, out)
}

func TestOtherControlsReturnAtOnce(t *testing.T) {
	seen := make(chan Event, 8)
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		for {
			select {
			case e := <-ev:
				seen <- e
			case <-ctx.Done():
				return nil
			}
		}
	})
	p, _, out := runPlane(t, Options{SuspendBudget: 5 * time.Second}, h)
	for _, c := range []struct{ cmd, evt, sid uint32 }{
		{ctlPowerEvent, pbtAPMResumeAutomatic, 0},
		{ctlSessionChange, wtsSessionLock, 3},
		{ctlPowerEvent, 0xA, 0}, // power status change: ignored
	} {
		within(t, time.Second, "control to return", goControl(p, c.cmd, c.evt, c.sid))
	}
	if e := <-seen; e.Kind != Resume || e.Done != nil {
		t.Fatalf("resume event %+v", e)
	}
	if e := <-seen; e.Kind != SessionChange || e.Session != SessionLock || e.SessionID != 3 {
		t.Fatalf("session event %+v", e)
	}
	stopRun(t, p, out)
}

// A suspend that arrives while the service is already stopping needs no
// work from the Handler: the runtime is going away anyway.
func TestSuspendDuringStopIsAcknowledgedByTheHost(t *testing.T) {
	release := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-ctx.Done()
		<-release // slow to stop
		return nil
	})
	p, rec, out := runPlane(t, Options{SuspendBudget: 5 * time.Second, StopTimeout: 5 * time.Second}, h)
	within(t, time.Second, "stop to be queued", goControl(p, ctlStop, 0, 0))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if st := rec.all(); len(st) > 0 && st[len(st)-1].phase == phaseStopPending {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t0 := time.Now()
	within(t, 2*time.Second, "suspend during stop", goControl(p, ctlPowerEvent, pbtAPMSuspend, 0))
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("suspend during stop was held for %s", d)
	}
	close(release)
	<-out
}

// When the event buffer is full the suspend is dropped, and must not hold
// the system for the whole budget.
func TestDroppedSuspendIsAcknowledged(t *testing.T) {
	block := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-block
		return nil
	})
	p, _, out := runPlane(t, Options{SuspendBudget: 10 * time.Second}, h)
	for i := 0; i < EventBuffer; i++ {
		p.control(ctlSessionChange, wtsSessionLock, 1)
	}
	// Let the supervisor drain p.reqs into the full events buffer.
	time.Sleep(100 * time.Millisecond)
	t0 := time.Now()
	within(t, 3*time.Second, "dropped suspend to be released", goControl(p, ctlPowerEvent, pbtAPMSuspend, 0))
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("dropped suspend held for %s", d)
	}
	close(block)
	<-out
}

func TestHandlerExitReleasesHeldSuspend(t *testing.T) {
	exit := make(chan struct{})
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-exit
		return nil
	})
	p, _, out := runPlane(t, Options{SuspendBudget: 10 * time.Second}, h)
	returned := goControl(p, ctlPowerEvent, pbtAPMSuspend, 0)
	time.Sleep(50 * time.Millisecond)
	close(exit)
	within(t, 2*time.Second, "held suspend to be released when the Handler exits", returned)
	<-out
}

func TestSuperviseStopSequenceAndExitStatus(t *testing.T) {
	var gotStop Event
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-ctx.Done()
		gotStop = <-ev
		return nil
	})
	p, rec, out := runPlane(t, Options{StopTimeout: 7 * time.Second}, h)
	p.control(ctlInterrogate, 0, 0)
	stopRun(t, p, out)
	if gotStop.Kind != Stop || time.Until(gotStop.Deadline) < 5*time.Second {
		t.Fatalf("stop event %+v", gotStop)
	}
	st := rec.all()
	if st[0].phase != phaseStartPending || st[1].phase != phaseRunning {
		t.Fatalf("start reports %+v", st[:2])
	}
	var pendingSeen bool
	for _, s := range st {
		if s.phase == phaseStopPending && s.checkpoint > 0 && s.waitHint >= 2000 {
			pendingSeen = true
		}
	}
	if !pendingSeen {
		t.Fatalf("no stop-pending report: %+v", st)
	}
}

func TestSuperviseFailureOnlyWhenNotStopping(t *testing.T) {
	boom := HandlerFunc(func(context.Context, <-chan Event) error { return context.DeadlineExceeded })
	_, rec, out := runPlane(t, Options{}, boom)
	if failed := <-out; !failed {
		t.Fatal("handler error while running was not a failure")
	}
	if st := rec.all(); !st[len(st)-1].failed || st[len(st)-1].phase != phaseStopped {
		t.Fatalf("final report %+v", st[len(st)-1])
	}

	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-ctx.Done()
		return context.Canceled
	})
	p, _, out2 := runPlane(t, Options{}, h)
	p.control(ctlStop, 0, 0)
	if failed := <-out2; failed {
		t.Fatal("handler error while stopping was reported as a failure")
	}
}

func TestInterrogateReportsLastStatus(t *testing.T) {
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error { <-ctx.Done(); return nil })
	p, rec, out := runPlane(t, Options{}, h)
	p.control(ctlInterrogate, 0, 0)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(rec.all()) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	st := rec.all()
	if len(st) < 3 || st[2].phase != phaseRunning {
		t.Fatalf("interrogate did not re-report running: %+v", st)
	}
	stopRun(t, p, out)
}
