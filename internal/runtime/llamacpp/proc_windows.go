//go:build windows

package llamacpp

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// terminate opens pid, re-checks its image path with verify (guarding
// against PID reuse since the snapshot), terminates it, and waits up to wait
// for it to exit.
func terminate(pid uint32, verify func(path string) bool, wait time.Duration) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// The path may be unreadable while termination is still allowed;
		// verify then decides on the name match alone.
		h, err = windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
		if err != nil {
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				return nil // already gone
			}
			return fmt.Errorf("OpenProcess: %w", err)
		}
	}
	defer windows.CloseHandle(h)
	if !verify(imagePath(h)) {
		return errNoLongerMatches
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("TerminateProcess: %w", err)
	}
	ev, err := windows.WaitForSingleObject(h, uint32(wait/time.Millisecond))
	if err != nil {
		return fmt.Errorf("WaitForSingleObject: %w", err)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		return errors.New("process still running after TerminateProcess")
	}
	return nil
}

func imagePath(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// processAlive reports whether pid exists and has not exited.
func processAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
