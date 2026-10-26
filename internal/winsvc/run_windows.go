//go:build windows

package winsvc

import (
	"sync"
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
	return runService(o.withDefaults(), h)
}

// IsService reports whether the process runs under the service control
// manager. An error counts as not a service: callers use it to decide
// whether to touch the service's own registration and data ACLs.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// Pre-shutdown gives the configured PreshutdownTimeout instead of the few
// seconds a plain shutdown notification allows. Shutdown is accepted too,
// so a system that skips pre-shutdown still stops the runtime.
const accepts = windows.SERVICE_ACCEPT_STOP | windows.SERVICE_ACCEPT_SHUTDOWN | windows.SERVICE_ACCEPT_PRESHUTDOWN |
	windows.SERVICE_ACCEPT_POWEREVENT | windows.SERVICE_ACCEPT_SESSIONCHANGE

// host is the service registered with the SCM. A process hosts one service,
// so it is a package variable the SCM's callbacks can reach.
type host struct {
	name   *uint16
	plane  *plane
	h      Handler
	handle windows.Handle
}

var (
	theHost       *host
	initCallbacks sync.Once
	ctlCallback   uintptr
	mainCallback  uintptr
)

// runService hands the process to the SCM dispatcher and returns when the
// service has stopped. It replaces x/sys/windows/svc.Run, whose control
// handler returns before the service has seen the request; a suspend
// notification needs the handler held (ADR 0013).
func runService(o Options, h Handler) error {
	name, err := windows.UTF16PtrFromString(o.Name)
	if err != nil {
		return err
	}
	initCallbacks.Do(func() {
		ctlCallback = windows.NewCallback(ctlHandler)
		mainCallback = windows.NewCallback(serviceMain)
	})
	theHost = &host{name: name, plane: newPlane(o), h: h}
	table := []windows.SERVICE_TABLE_ENTRY{
		{ServiceName: name, ServiceProc: mainCallback},
		{ServiceName: nil, ServiceProc: 0},
	}
	return windows.StartServiceCtrlDispatcher(&table[0])
}

// ctlHandler is the SCM's control handler (HandlerEx). It runs on the
// dispatcher's thread, which serializes control requests, so a held suspend
// also delays the next request by at most DefaultSuspendBudget.
func ctlHandler(ctl, eventType, eventData, _ uintptr) uintptr {
	var sid uint32
	if uint32(ctl) == ctlSessionChange && eventData != 0 {
		// The SCM owns this memory and keeps it valid until the handler
		// returns, so it is read here and not later.
		n := *(**windows.WTSSESSION_NOTIFICATION)(unsafe.Pointer(&eventData))
		sid = n.SessionID
	}
	theHost.plane.control(uint32(ctl), uint32(eventType), sid)
	return uintptr(windows.NO_ERROR)
}

// serviceMain is the entry point the SCM calls once the service starts.
func serviceMain(_ uint32, _ **uint16) uintptr {
	h := theHost
	handle, err := windows.RegisterServiceCtrlHandlerEx(h.name, ctlCallback, 0)
	if err != nil {
		if errno, ok := err.(windows.Errno); ok {
			return uintptr(errno)
		}
		return uintptr(windows.ERROR_UNKNOWN_EXCEPTION)
	}
	h.handle = handle
	h.plane.supervise(h.h, h.report)
	return uintptr(windows.NO_ERROR)
}

// report sends one status to the SCM.
func (h *host) report(s status) {
	t := windows.SERVICE_STATUS{ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, CheckPoint: s.checkpoint, WaitHint: s.waitHint}
	switch s.phase {
	case phaseStartPending:
		t.CurrentState = windows.SERVICE_START_PENDING
	case phaseRunning:
		t.CurrentState, t.ControlsAccepted = windows.SERVICE_RUNNING, accepts
	case phaseStopPending:
		t.CurrentState = windows.SERVICE_STOP_PENDING
	case phaseStopped:
		t.CurrentState = windows.SERVICE_STOPPED
		if s.failed {
			// A service-specific exit code marks a failure, so the SCM's
			// recovery actions apply.
			t.Win32ExitCode, t.ServiceSpecificExitCode = uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR), 1
		}
	}
	_ = windows.SetServiceStatus(h.handle, &t)
}
