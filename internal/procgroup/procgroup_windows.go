//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platform struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
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
	cmd.SysProcAttr = &windows.SysProcAttr{
		// Suspended so the process is inside the job before it can run or
		// spawn anything. No console window, and its own process group so a
		// console control event aimed at us never reaches it by accident.
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
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
	go func() { g.setExit(cmd.Wait()) }()
	return g, nil
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
