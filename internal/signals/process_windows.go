//go:build windows

package signals

import (
	"errors"
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

var procLookupPrivilegeNameW = windows.NewLazySystemDLL("advapi32.dll").NewProc("LookupPrivilegeNameW")

// ProcessIdentity reports the account a process runs as and every privilege
// present in its token (enabled or not), for verifying the runtime's reduced identity (ADR 0006).
func ProcessIdentity(pid uint32) (user string, privileges []string, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", nil, err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return "", nil, err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", nil, err
	}
	acct, dom, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		user = tu.User.Sid.String()
	} else {
		user = dom + `\` + acct
	}
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenPrivileges, nil, 0, &n)
	if n == 0 {
		return user, nil, nil
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenPrivileges, &buf[0], n, &n); err != nil {
		return user, nil, nil
	}
	tp := (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	for _, p := range tp.AllPrivileges() {
		name := make([]uint16, 64)
		l := uint32(len(name))
		if r, _, _ := procLookupPrivilegeNameW.Call(0, uintptr(unsafe.Pointer(&p.Luid)), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&l))); r != 0 {
			privileges = append(privileges, windows.UTF16ToString(name[:l]))
		}
	}
	return user, privileges, nil
}

// ProcessIntegrity returns the process's integrity level: "Low", "Medium",
// "High", "System", or the raw SID.
func ProcessIntegrity(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, nil, 0, &n)
	if n == 0 {
		return "", errors.New("no integrity level")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], n, &n); err != nil {
		return "", err
	}
	sid := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0])).Label.Sid.String()
	switch sid {
	case "S-1-16-4096":
		return "Low", nil
	case "S-1-16-8192":
		return "Medium", nil
	case "S-1-16-12288":
		return "High", nil
	case "S-1-16-16384":
		return "System", nil
	}
	return sid, nil
}
