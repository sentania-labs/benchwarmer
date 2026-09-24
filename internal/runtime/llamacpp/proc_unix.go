//go:build !windows

package llamacpp

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// terminate kills pid with SIGKILL after re-checking its image path with
// verify (guarding against PID reuse since the snapshot), then waits up to
// wait for it to be gone. A zombie counts as gone: it holds no resources.
func terminate(pid uint32, verify func(path string) bool, wait time.Duration) error {
	path, _ := os.Readlink("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/exe")
	if !verify(path) {
		return errNoLongerMatches
	}
	if err := syscall.Kill(int(pid), syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("kill: %w", err)
	}
	deadline := time.Now().Add(wait)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return errors.New("process still running after SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid uint32) bool {
	if syscall.Kill(int(pid), 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || !strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}
