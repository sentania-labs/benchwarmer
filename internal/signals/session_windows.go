//go:build windows

package signals

import (
	"errors"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	moduser32                        = windows.NewLazySystemDLL("user32.dll")
	procGetForegroundWindow          = moduser32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId     = moduser32.NewProc("GetWindowThreadProcessId")
	procGetWindowRect                = moduser32.NewProc("GetWindowRect")
	procMonitorFromWindow            = moduser32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW              = moduser32.NewProc("GetMonitorInfoW")
	procGetLastInputInfo             = moduser32.NewProc("GetLastInputInfo")
	procGetShellWindow               = moduser32.NewProc("GetShellWindow")
	procGetDesktopWindow             = moduser32.NewProc("GetDesktopWindow")
	modshell32                       = windows.NewLazySystemDLL("shell32.dll")
	procSHQueryUserNotificationState = modshell32.NewProc("SHQueryUserNotificationState")
	modkernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount64               = modkernel32.NewProc("GetTickCount64")
)

// ErrSession0 means the caller runs in the services session, where the
// user's foreground window and input are not observable (ADR 0005).
var ErrSession0 = errors.New("signals: session signals are not observable from session 0")

type rect struct{ Left, Top, Right, Bottom int32 }

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

type lastInputInfo struct {
	Size uint32
	Time uint32
}

// ReadSessionSignals reads foreground, fullscreen, and idle facts. It must run
// inside the interactive session; from session 0 it reports nothing useful.
func ReadSessionSignals() (SessionSignals, error) {
	s := SessionSignals{Time: time.Now()}
	var sid uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid)
	s.SessionID = sid
	if sid == 0 {
		return s, ErrSession0
	}

	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd != 0 {
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		s.ForegroundPID = pid
		if path := ProcessPath(pid); path != "" {
			s.ForegroundPath = path
			s.ForegroundName = filepath.Base(path)
		}
		s.Fullscreen = isFullscreen(hwnd)
	}

	var st uint32
	if r, _, _ := procSHQueryUserNotificationState.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		s.NotificationState = notificationState(st)
	}

	lii := lastInputInfo{Size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r != 0 {
		now, _, _ := procGetTickCount64.Call()
		// LASTINPUTINFO.dwTime is a 32-bit tick count; compare modulo 2^32.
		idle := uint32(now) - lii.Time
		s.IdleSeconds = float64(idle) / 1000
	}
	return s, nil
}

func isFullscreen(hwnd uintptr) bool {
	shell, _, _ := procGetShellWindow.Call()
	desk, _, _ := procGetDesktopWindow.Call()
	if hwnd == shell || hwnd == desk {
		return false
	}
	var wr rect
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr))); r == 0 {
		return false
	}
	const monitorDefaultToNearest = 2
	mon, _, _ := procMonitorFromWindow.Call(hwnd, monitorDefaultToNearest)
	if mon == 0 {
		return false
	}
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return false
	}
	m := mi.Monitor
	return wr.Left <= m.Left && wr.Top <= m.Top && wr.Right >= m.Right && wr.Bottom >= m.Bottom
}

func notificationState(v uint32) string {
	switch v {
	case 1:
		return "not_present"
	case 2:
		return "busy"
	case 3:
		return "d3d_fullscreen"
	case 4:
		return "presentation"
	case 5:
		return "accepts_notifications"
	case 6:
		return "quiet_time"
	case 7:
		return "app"
	}
	return "unknown"
}

var procOpenInputDesktop = moduser32.NewProc("OpenInputDesktop")
var procCloseDesktop = moduser32.NewProc("CloseDesktop")

// WorkstationLocked reports whether the interactive desktop is locked: the
// input desktop cannot be opened while the secure (lock) desktop is active.
func WorkstationLocked() bool {
	const desktopSwitchDesktop = 0x0100
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopSwitchDesktop)
	if h == 0 {
		return true
	}
	procCloseDesktop.Call(h)
	return false
}
