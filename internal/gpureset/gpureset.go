// Package gpureset detects GPU driver resets (TDRs) as they happen. When a
// GPU engine hangs, Windows writes a "live" watchdog dump before recovering
// the driver; on the target PC that dump appeared a full minute before a
// second hang escalated to a blue screen (bugcheck 0x116). Seeing the first
// dump lets the worker stop hammering the GPU before the second one.
package gpureset

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Reset is one detected GPU driver reset.
type Reset struct {
	File string    `json:"file"`
	Time time.Time `json:"time"`
}

// Watcher polls directories for new watchdog dumps.
type Watcher struct {
	mu   sync.Mutex
	dirs []string
	seen map[string]time.Time
}

// DefaultDirs are where Windows writes GPU watchdog live dumps.
func DefaultDirs() []string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return nil
	}
	return []string{filepath.Join(root, "LiveKernelReports", "WATCHDOG"), filepath.Join(root, "LiveKernelReports")}
}

// New starts watching dirs. Dumps already present are the baseline and are
// never reported: only resets that happen while the service runs count.
func New(dirs []string) *Watcher {
	w := &Watcher{dirs: dirs, seen: map[string]time.Time{}}
	for _, r := range w.scan() {
		w.seen[r.File] = r.Time
	}
	return w
}

func isWatchdogDump(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "watchdog") && strings.HasSuffix(n, ".dmp")
}

func (w *Watcher) scan() []Reset {
	var out []Reset
	for _, d := range w.dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !isWatchdogDump(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, Reset{File: filepath.Join(d, e.Name()), Time: info.ModTime()})
		}
	}
	return out
}

// Poll returns resets that appeared (or were rewritten) since the last call.
func (w *Watcher) Poll() []Reset {
	w.mu.Lock()
	defer w.mu.Unlock()
	var fresh []Reset
	for _, r := range w.scan() {
		if prev, ok := w.seen[r.File]; ok && !r.Time.After(prev) {
			continue
		}
		w.seen[r.File] = r.Time
		fresh = append(fresh, r)
	}
	return fresh
}
