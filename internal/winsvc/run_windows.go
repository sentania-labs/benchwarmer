//go:build windows

package winsvc

import (
	"context"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// Run runs h under the service control manager when the process was started
// as a service, and as a console process otherwise.
func Run(o Options, h Handler) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		return RunConsole(o, h)
	}
	return svc.Run(o.Name, &service{opts: o.withDefaults(), h: h})
}

// IsService reports whether the process runs under the service control
// manager. An error counts as not a service: callers use it to decide
// whether to touch the service's own registration and data ACLs.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type service struct {
	opts Options
	h    Handler
}

// Pre-shutdown gives the configured PreshutdownTimeout instead of the few
// seconds a plain shutdown notification allows. Shutdown is accepted too,
// so a system that skips pre-shutdown still stops the runtime.
const accepts = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown |
	svc.AcceptPowerEvent | svc.AcceptSessionChange

// Execute implements svc.Handler.
func (s *service) Execute(_ []string, r <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, EventBuffer)
	done := make(chan error, 1)
	go func() { done <- s.h.Run(ctx, events) }()
	st <- svc.Status{State: svc.Running, Accepts: accepts}

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
		st <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: waitHint(deadline, time.Now())}
	}
	for {
		select {
		case err := <-done:
			if err != nil && !stopping {
				// A service-specific exit code marks a failure, so the
				// SCM's recovery actions apply.
				return true, 1
			}
			return false, 0
		case <-tick:
			pending()
		case c := <-r:
			if c.Cmd == svc.Interrogate {
				st <- c.CurrentStatus
				continue
			}
			var sid uint32
			if c.Cmd == svc.SessionChange && c.EventData != 0 {
				// Read at once: the SCM owns this memory. Known x/sys
				// limitation: the control handler has already returned by
				// the time Execute sees the request, so the read is best
				// effort. The reason is in EventType; only the ID is read
				// from the pointer. EventData is reinterpreted in place
				// rather than converted from uintptr.
				n := *(**windows.WTSSESSION_NOTIFICATION)(unsafe.Pointer(&c.EventData))
				sid = n.SessionID
			}
			ev, ok := translate(uint32(c.Cmd), c.EventType, sid, time.Now(), s.opts)
			if !ok {
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
			deliver(events, ev)
		}
	}
}

func deliver(events chan<- Event, ev Event) {
	select {
	case events <- ev:
	default:
	}
}
