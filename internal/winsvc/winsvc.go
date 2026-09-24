// Package winsvc hosts the service under the Windows service control manager
// and, for development, as a console process. The Handler sees the same
// events either way: stop, shutdown with its deadline, suspend, resume, and
// session changes. Install and Remove register the service with its
// identity (ADR 0006) and failure-recovery actions.
package winsvc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind is a service lifecycle event.
type Kind int

// Event kinds.
const (
	// Stop: the service was asked to stop (SCM stop, or Ctrl+C / SIGTERM
	// in console mode).
	Stop Kind = iota + 1
	// Shutdown: the system is shutting down (pre-shutdown or shutdown).
	Shutdown
	// Suspend: the system is about to sleep.
	Suspend
	// Resume: the system woke up.
	Resume
	// SessionChange: an interactive session changed state.
	SessionChange
)

func (k Kind) String() string {
	switch k {
	case Stop:
		return "stop"
	case Shutdown:
		return "shutdown"
	case Suspend:
		return "suspend"
	case Resume:
		return "resume"
	case SessionChange:
		return "session_change"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// SessionKind is the kind of session change.
type SessionKind string

// Session change kinds (WTS_* reasons).
const (
	SessionConsoleConnect    SessionKind = "console-connect"
	SessionConsoleDisconnect SessionKind = "console-disconnect"
	SessionRemoteConnect     SessionKind = "remote-connect"
	SessionRemoteDisconnect  SessionKind = "remote-disconnect"
	SessionLogon             SessionKind = "logon"
	SessionLogoff            SessionKind = "logoff"
	SessionLock              SessionKind = "lock"
	SessionUnlock            SessionKind = "unlock"
)

// Event is one lifecycle notification.
type Event struct {
	Kind Kind
	// Deadline, for Stop and Shutdown, is when the OS may terminate the
	// process; the handler should have released the GPU by then.
	Deadline time.Time
	// Session and SessionID are set for SessionChange.
	Session   SessionKind
	SessionID uint32
}

// Handler is the service body.
type Handler interface {
	// Run runs until ctx is cancelled and then returns once shut down. ctx
	// is cancelled right after a Stop or Shutdown event is queued on
	// events, so the event's deadline is available when ctx ends. Returning
	// an error while not stopping marks the service as failed, which lets
	// the SCM's recovery actions restart it.
	Run(ctx context.Context, events <-chan Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, events <-chan Event) error

// Run implements Handler.
func (f HandlerFunc) Run(ctx context.Context, events <-chan Event) error { return f(ctx, events) }

// Options configures the runner.
type Options struct {
	// Name is the service name registered with the SCM.
	Name string
	// StopTimeout is the budget for a stop request; it sets the Stop
	// event's deadline and the wait hints reported while stopping.
	StopTimeout time.Duration
	// ShutdownTimeout is the budget after a plain shutdown notification
	// (Windows allows services only a few seconds).
	ShutdownTimeout time.Duration
	// PreshutdownTimeout must match the value given to Install; it is the
	// budget after a pre-shutdown notification.
	PreshutdownTimeout time.Duration
}

// Defaults for zero Options fields.
const (
	DefaultStopTimeout        = 20 * time.Second
	DefaultShutdownTimeout    = 5 * time.Second
	DefaultPreshutdownTimeout = 30 * time.Second
)

// EventBuffer is how many events may queue before the handler reads them.
// Beyond that, non-stop events are dropped; stop is still signalled by ctx.
const EventBuffer = 64

func (o Options) withDefaults() Options {
	if o.StopTimeout <= 0 {
		o.StopTimeout = DefaultStopTimeout
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.PreshutdownTimeout <= 0 {
		o.PreshutdownTimeout = DefaultPreshutdownTimeout
	}
	return o
}

// Raw SCM control codes and event types. Declared here, not taken from
// x/sys/windows, so the translation is testable off Windows.
const (
	ctlStop          = 0x1
	ctlShutdown      = 0x5
	ctlPowerEvent    = 0xD
	ctlSessionChange = 0xE
	ctlPreShutdown   = 0xF

	pbtAPMSuspend         = 0x4
	pbtAPMResumeSuspend   = 0x7
	pbtAPMResumeAutomatic = 0x12

	wtsConsoleConnect    = 0x1
	wtsConsoleDisconnect = 0x2
	wtsRemoteConnect     = 0x3
	wtsRemoteDisconnect  = 0x4
	wtsSessionLogon      = 0x5
	wtsSessionLogoff     = 0x6
	wtsSessionLock       = 0x7
	wtsSessionUnlock     = 0x8
)

// PowerEvent maps a PBT_* power broadcast to an event kind. Resume arrives
// as PBT_APMRESUMEAUTOMATIC always and PBT_APMRESUMESUSPEND additionally
// when a user is present; both map to Resume, so handlers must tolerate a
// duplicate.
func PowerEvent(eventType uint32) (Kind, bool) {
	switch eventType {
	case pbtAPMSuspend:
		return Suspend, true
	case pbtAPMResumeAutomatic, pbtAPMResumeSuspend:
		return Resume, true
	}
	return 0, false
}

// SessionEvent maps a WTS_* session-change reason.
func SessionEvent(reason uint32) (SessionKind, bool) {
	switch reason {
	case wtsConsoleConnect:
		return SessionConsoleConnect, true
	case wtsConsoleDisconnect:
		return SessionConsoleDisconnect, true
	case wtsRemoteConnect:
		return SessionRemoteConnect, true
	case wtsRemoteDisconnect:
		return SessionRemoteDisconnect, true
	case wtsSessionLogon:
		return SessionLogon, true
	case wtsSessionLogoff:
		return SessionLogoff, true
	case wtsSessionLock:
		return SessionLock, true
	case wtsSessionUnlock:
		return SessionUnlock, true
	}
	return "", false
}

// translate maps one SCM control request to an event. sessionID is only
// meaningful for session changes.
func translate(cmd, eventType, sessionID uint32, now time.Time, o Options) (Event, bool) {
	o = o.withDefaults()
	switch cmd {
	case ctlStop:
		return Event{Kind: Stop, Deadline: now.Add(o.StopTimeout)}, true
	case ctlPreShutdown:
		return Event{Kind: Shutdown, Deadline: now.Add(o.PreshutdownTimeout)}, true
	case ctlShutdown:
		return Event{Kind: Shutdown, Deadline: now.Add(o.ShutdownTimeout)}, true
	case ctlPowerEvent:
		if k, ok := PowerEvent(eventType); ok {
			return Event{Kind: k}, true
		}
	case ctlSessionChange:
		if s, ok := SessionEvent(eventType); ok {
			return Event{Kind: SessionChange, Session: s, SessionID: sessionID}, true
		}
	}
	return Event{}, false
}

// waitHint is the SCM wait hint in milliseconds for a pending stop: the time
// left before deadline, at least 2 s so the SCM never gives up between two
// checkpoint updates.
func waitHint(deadline, now time.Time) uint32 {
	d := deadline.Sub(now)
	if d < 2*time.Second {
		d = 2 * time.Second
	}
	return uint32(d / time.Millisecond)
}

// Service accounts accepted by Install.
const (
	// AccountVirtual runs as the virtual account NT SERVICE\<name> (ADR 0006
	// default).
	AccountVirtual = "virtual"
	// AccountLocalSystem runs as LocalSystem.
	AccountLocalSystem = "LocalSystem"
)

// startName resolves an account choice to the SCM's service start name and
// whether the service SID must be unrestricted (required for a virtual
// account's SID to be usable in ACLs).
func startName(account, service string) (name string, serviceSID bool, err error) {
	switch account {
	case "", AccountVirtual:
		if service == "" {
			return "", false, errors.New("winsvc: a virtual account needs the service name")
		}
		return `NT SERVICE\` + service, true, nil
	case AccountLocalSystem, "system":
		return "LocalSystem", false, nil
	}
	return "", false, fmt.Errorf("winsvc: unsupported account %q (use %q or %q)", account, AccountVirtual, AccountLocalSystem)
}

// RecoveryStep is one SCM failure action.
type RecoveryStep struct {
	Restart bool
	Delay   time.Duration
}

// RecoveryResetPeriod is how long without failures before the SCM resets
// its failure count.
const RecoveryResetPeriod = 24 * time.Hour

// RecoveryPlan is the failure-recovery sequence: restart after 30 s, then
// after 2 min, then stop trying until the reset period passes. The service
// has its own crash backoff for the runtime; this only covers the service
// process itself failing.
func RecoveryPlan() []RecoveryStep {
	return []RecoveryStep{
		{Restart: true, Delay: 30 * time.Second},
		{Restart: true, Delay: 2 * time.Minute},
		{Restart: false},
	}
}
