//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platform struct {
	mu           sync.Mutex
	job          windows.Handle
	closed       bool
	shareConsole bool
}

var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW            = modadvapi32.NewProc("LogonUserW")
	procCreateRestrictedToken = modadvapi32.NewProc("CreateRestrictedToken")
	modkernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = modkernel32.NewProc("SetConsoleCtrlHandler")
)

var (
	procAllocConsole     = modkernel32.NewProc("AllocConsole")
	procGetConsoleWindow = modkernel32.NewProc("GetConsoleWindow")

	// consoleMu serialises Ctrl+C delivery with process creation: while
	// this process ignores Ctrl+C, a child created then would inherit the
	// ignore flag and could never be stopped gracefully.
	consoleMu sync.Mutex
	// swallowing is set once this process swallows all console events
	// itself (EnsureConsole), so no ignore window is needed.
	swallowing bool
)

// EnsureConsole gives a process without a console (a Windows service) a
// console for its runtime children, and makes this process swallow every
// console control event, so it can send Ctrl+C to a child without stopping
// itself. Service shutdown still arrives through the service manager.
// Not for interactive programs: their own Ctrl+C would be swallowed too.
func EnsureConsole() error {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if swallowing {
		return nil
	}
	if h, _, _ := procGetConsoleWindow.Call(); h == 0 {
		if r, _, err := procAllocConsole.Call(); r == 0 {
			return fmt.Errorf("procgroup: AllocConsole: %w", err)
		}
	}
	// Clear an ignore-Ctrl+C flag this process may have inherited:
	// children inherit it, and a runtime that ignores Ctrl+C can only be
	// hard-killed.
	procSetConsoleCtrlHandler.Call(0, 0)
	cb := windows.NewCallback(func(uint32) uintptr { return 1 })
	if r, _, err := procSetConsoleCtrlHandler.Call(cb, 1); r == 0 {
		return fmt.Errorf("procgroup: SetConsoleCtrlHandler: %w", err)
	}
	swallowing = true
	return nil
}

// HasConsole reports whether this process has a console to share.
func HasConsole() bool {
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}

// interrupt sends Ctrl+C to every process on our console.
func (g *Group) interrupt() error {
	if !g.plat.shareConsole {
		return errors.New("procgroup: Interrupt requires Spec.ShareConsole")
	}
	select {
	case <-g.done:
		return ErrNotRunning
	default:
	}
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if swallowing {
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
			return fmt.Errorf("procgroup: GenerateConsoleCtrlEvent: %w", err)
		}
		return nil
	}
	// Interactive caller (the probe in a terminal): ignore Ctrl+C for a
	// moment so only the child reacts. Held under consoleMu so no child is
	// created while the ignore flag is set.
	if r, _, err := procSetConsoleCtrlHandler.Call(0, 1); r == 0 {
		return fmt.Errorf("procgroup: SetConsoleCtrlHandler: %w", err)
	}
	err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
	time.Sleep(time.Second)
	procSetConsoleCtrlHandler.Call(0, 0)
	if err != nil {
		return fmt.Errorf("procgroup: GenerateConsoleCtrlEvent: %w", err)
	}
	return nil
}

func start(spec Spec) (*Group, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("procgroup: CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			// Kill the whole tree when the last job handle closes, which
			// includes this process dying for any reason. No breakaway flag
			// is set, so descendants cannot leave the job.
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
				windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("procgroup: SetInformationJobObject: %w", err)
	}

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.WaitDelay = pipeDrainDelay
	// Suspended so the process is inside the job before it can run or spawn
	// anything. By default: no console window, and its own process group so a
	// console control event aimed at us never reaches it by accident.
	flags := uint32(windows.CREATE_SUSPENDED)
	if !spec.ShareConsole {
		flags |= windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP
	}
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: flags, HideWindow: !spec.ShareConsole}
	switch spec.RunAs {
	case "":
	case RunAsLocalService:
		tok, err := localServiceToken()
		if err != nil {
			_ = windows.CloseHandle(job)
			// Never fall back to launching with this process's own rights.
			return nil, fmt.Errorf("procgroup: cannot start the runtime as LocalService (the service must run as LocalSystem; set runtime.run_as to \"service\" only for development): %w", err)
		}
		// The token is only needed for process creation.
		defer tok.Close()
		cmd.SysProcAttr.Token = syscall.Token(tok)
	default:
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("procgroup: unknown RunAs %q", spec.RunAs)
	}
	if spec.ShareConsole {
		consoleMu.Lock()
	}
	err = cmd.Start()
	if spec.ShareConsole {
		consoleMu.Unlock()
	}
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("procgroup: start: %w", err)
	}
	pid := cmd.Process.Pid

	fail := func(stage string, err error) (*Group, error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("procgroup: %s: %w", stage, err)
	}

	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fail("OpenProcess", err)
	}
	err = windows.AssignProcessToJobObject(job, ph)
	_ = windows.CloseHandle(ph)
	if err != nil {
		return fail("AssignProcessToJobObject", err)
	}
	if err := resumeProcess(uint32(pid)); err != nil {
		return fail("resume", err)
	}

	g := &Group{pid: pid, started: time.Now(), done: make(chan struct{})}
	g.plat.job = job
	g.plat.shareConsole = spec.ShareConsole
	go func() { g.setExit(cmd.Wait()) }()
	return g, nil
}

// localServiceToken logs on NT AUTHORITY\LocalService (only LocalSystem may
// do this without a password) and removes every privilege from the token
// except SeChangeNotify, so the runtime holds no special rights even if an
// input it parses is hostile.
func localServiceToken() (windows.Token, error) {
	const logon32LogonService, logon32ProviderDefault = 5, 0
	user, _ := windows.UTF16PtrFromString("LocalService")
	domain, _ := windows.UTF16PtrFromString("NT AUTHORITY")
	var tok windows.Token
	r, _, err := procLogonUserW.Call(uintptr(unsafe.Pointer(user)), uintptr(unsafe.Pointer(domain)), 0,
		logon32LogonService, logon32ProviderDefault, uintptr(unsafe.Pointer(&tok)))
	if r == 0 {
		return 0, fmt.Errorf("LogonUser: %w", err)
	}
	defer tok.Close()
	const disableMaxPrivilege = 0x1
	var restricted windows.Token
	r, _, err = procCreateRestrictedToken.Call(uintptr(tok), disableMaxPrivilege, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&restricted)))
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", err)
	}
	// Service logons run at System integrity; drop to Medium so the runtime
	// cannot write to or open objects that other services protect with
	// integrity labels.
	medium, err := windows.CreateWellKnownSid(windows.WinMediumLabelSid)
	if err != nil {
		restricted.Close()
		return 0, fmt.Errorf("medium integrity SID: %w", err)
	}
	tml := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: medium, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(restricted, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&tml)), tml.Size()); err != nil {
		restricted.Close()
		return 0, fmt.Errorf("set integrity level: %w", err)
	}
	return restricted, nil
}

// resumeProcess resumes the threads of a process created suspended. A freshly
// created suspended process has exactly one thread.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return oerr
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return errors.New("no threads found for process")
	}
	return nil
}

func (g *Group) members() ([]int, error) {
	g.plat.mu.Lock()
	defer g.plat.mu.Unlock()
	if g.plat.closed {
		return nil, nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORDs then ULONG_PTR[].
	const max = 256
	var buf struct {
		Assigned uint32
		InList   uint32
		IDs      [max]uintptr
	}
	err := windows.QueryInformationJobObject(g.plat.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf)), uint32(unsafe.Sizeof(buf)), nil)
	if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return nil, fmt.Errorf("procgroup: QueryInformationJobObject: %w", err)
	}
	out := make([]int, 0, buf.InList)
	for i := uint32(0); i < buf.InList && i < max; i++ {
		out = append(out, int(buf.IDs[i]))
	}
	return out, nil
}

func (g *Group) kill() error {
	g.plat.mu.Lock()
	defer g.plat.mu.Unlock()
	if g.plat.closed {
		return ErrNotRunning
	}
	if err := windows.TerminateJobObject(g.plat.job, 1); err != nil {
		return fmt.Errorf("procgroup: TerminateJobObject: %w", err)
	}
	return nil
}

func (g *Group) close() error {
	g.plat.mu.Lock()
	defer g.plat.mu.Unlock()
	if g.plat.closed {
		return nil
	}
	g.plat.closed = true
	return windows.CloseHandle(g.plat.job)
}
