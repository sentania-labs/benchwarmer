package winsvc

import (
	"context"
	"time"
)

// request is one SCM control request, as the control handler saw it.
type request struct {
	cmd, eventType, sessionID uint32
	at                        time.Time
	// done is set for suspend requests: the control handler waits on it,
	// and the Handler closes it through Event.Ack.
	done chan struct{}
}

// phase is the service state reported to the SCM.
type phase int

const (
	phaseStartPending phase = iota + 1
	phaseRunning
	phaseStopPending
	phaseStopped
)

// status is one report to the SCM.
type status struct {
	phase      phase
	checkpoint uint32
	waitHint   uint32 // milliseconds
	// failed marks a stopped service as failed, so the SCM's recovery
	// actions apply.
	failed bool
}

// plane connects the SCM's control handler to the Handler. The SCM side
// calls control from the dispatcher's thread; supervise runs the service.
// Nothing here is Windows-specific, so the suspend contract is tested
// everywhere.
type plane struct {
	o             Options
	suspendBudget time.Duration
	reqs          chan request
	stopped       chan struct{} // closed when supervise returns
	now           func() time.Time
}

func newPlane(o Options) *plane {
	return &plane{o: o.withDefaults(), suspendBudget: DefaultSuspendBudget, reqs: make(chan request, EventBuffer), stopped: make(chan struct{}), now: time.Now}
}

// control is the body of the SCM control handler. It returns as soon as the
// request is queued, except for a suspend notification: Windows proceeds to
// sleep once the handler returns, so that call waits until the Handler has
// acknowledged it or the suspend budget has passed. Waiting here is the only way
// to stop the runtime before the system sleeps (ADR 0013).
func (p *plane) control(cmd, eventType, sessionID uint32) {
	r := request{cmd: cmd, eventType: eventType, sessionID: sessionID, at: p.now()}
	if cmd == ctlPowerEvent && eventType == pbtAPMSuspend {
		r.done = make(chan struct{})
	}
	select {
	case p.reqs <- r:
	case <-p.stopped:
		return
	}
	if r.done == nil {
		return
	}
	t := time.NewTimer(p.suspendBudget)
	defer t.Stop()
	select {
	case <-r.done:
	case <-t.C:
	case <-p.stopped:
	}
}

// supervise runs h, reports the service state through report, and returns
// once h has returned, after reporting the service stopped: failed is true
// when h returned an error without having been asked to stop.
func (p *plane) supervise(h Handler, report func(status)) (failed bool) {
	defer close(p.stopped)
	defer func() { report(status{phase: phaseStopped, failed: failed}) }()
	report(status{phase: phaseStartPending, waitHint: 10000})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, EventBuffer)
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, events) }()
	last := status{phase: phaseRunning}
	report(last)

	var (
		stopping   bool
		deadline   time.Time
		checkpoint uint32
		ticker     *time.Ticker
		tick       <-chan time.Time
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	pending := func() {
		checkpoint++
		last = status{phase: phaseStopPending, checkpoint: checkpoint, waitHint: waitHint(deadline, time.Now())}
		report(last)
	}
	for {
		select {
		case err := <-done:
			return err != nil && !stopping
		case <-tick:
			pending()
		case r := <-p.reqs:
			if r.cmd == ctlInterrogate {
				report(last)
				continue
			}
			ev, ok := translate(r.cmd, r.eventType, r.sessionID, r.at, p.o, p.suspendBudget)
			if !ok {
				continue
			}
			ev.Done = r.done
			if ev.Kind == Suspend && stopping {
				// The runtime is already being stopped for good.
				ev.Ack()
				continue
			}
			if ev.Kind == Stop || ev.Kind == Shutdown {
				if stopping {
					if ev.Deadline.Before(deadline) {
						// A shutdown during a stop can only shorten it.
						deadline = ev.Deadline
						deliver(events, ev)
					}
					continue
				}
				stopping, deadline = true, ev.Deadline
				pending()
				deliver(events, ev)
				cancel()
				ticker = time.NewTicker(time.Second)
				tick = ticker.C
				continue
			}
			if !deliver(events, ev) {
				ev.Ack()
			}
		}
	}
}

// deliver queues ev for the Handler, or drops it when the buffer is full.
func deliver(events chan<- Event, ev Event) bool {
	select {
	case events <- ev:
		return true
	default:
		return false
	}
}
