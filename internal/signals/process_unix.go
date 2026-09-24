//go:build !windows

package signals

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// The Unix implementation reads /proc and exists for development only.
func snapshot() ([]Process, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range ents {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if l < 0 || r < l {
			continue
		}
		p := Process{PID: uint32(pid), Name: s[l+1 : r]}
		if f := strings.Fields(s[r+1:]); len(f) > 1 {
			pp, _ := strconv.ParseUint(f[1], 10, 32)
			p.PPID = uint32(pp)
		}
		if path, err := os.Readlink("/proc/" + e.Name() + "/exe"); err == nil {
			p.Path = path
			if i := strings.LastIndexByte(path, '/'); i >= 0 {
				p.Name = path[i+1:]
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// ErrUnsupported is returned for session signals off Windows.
var ErrUnsupported = errors.New("signals: not supported on this platform")

// ReadSessionSignals is Windows-only.
func ReadSessionSignals() (SessionSignals, error) { return SessionSignals{}, ErrUnsupported }

// Sessions is Windows-only.
func Sessions() ([]Session, error) { return nil, ErrUnsupported }

// WorkstationLocked is Windows-only; it reports false elsewhere.
func WorkstationLocked() bool { return false }
