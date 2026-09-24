//go:build windows

package signals

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func snapshot() ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []Process
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		p := Process{PID: pe.ProcessID, PPID: pe.ParentProcessID, Name: windows.UTF16ToString(pe.ExeFile[:])}
		var sid uint32
		if windows.ProcessIdToSessionId(p.PID, &sid) == nil {
			p.SessionID = sid
		}
		if p.PID != 0 {
			fillFromHandle(&p)
		}
		out = append(out, p)
	}
	return out, nil
}

func fillFromHandle(p *Process) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.PID)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
		p.Path = windows.UTF16ToString(buf[:n])
	}
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) == nil {
		p.CreateTime = time.Unix(0, c.Nanoseconds())
	}
}

// ProcessPath returns the full image path of a process, or "" if unreadable.
func ProcessPath(pid uint32) string {
	p := Process{PID: pid}
	fillFromHandle(&p)
	return p.Path
}

// Sessions lists logon sessions.
func Sessions() ([]Session, error) {
	var infos *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &infos, &count); err != nil {
		return nil, err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(infos)))
	console := windows.WTSGetActiveConsoleSessionId()
	var out []Session
	for _, s := range unsafe.Slice(infos, count) {
		out = append(out, Session{
			ID:      s.SessionID,
			Name:    windows.UTF16PtrToString(s.WindowStationName),
			State:   wtsState(s.State),
			Console: s.SessionID == console,
		})
	}
	return out, nil
}

func wtsState(s uint32) string {
	switch s {
	case windows.WTSActive:
		return "active"
	case windows.WTSConnected:
		return "connected"
	case windows.WTSConnectQuery:
		return "connect_query"
	case windows.WTSShadow:
		return "shadow"
	case windows.WTSDisconnected:
		return "disconnected"
	case windows.WTSIdle:
		return "idle"
	case windows.WTSListen:
		return "listen"
	case windows.WTSReset:
		return "reset"
	case windows.WTSDown:
		return "down"
	case windows.WTSInit:
		return "init"
	}
	return "unknown"
}

// BootTime returns when the system booted (to the second).
func BootTime() (time.Time, error) {
	ms, _, _ := procGetTickCount64.Call()
	return time.Now().Add(-time.Duration(ms) * time.Millisecond).Truncate(time.Second), nil
}
