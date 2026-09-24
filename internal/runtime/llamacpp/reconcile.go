package llamacpp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/runtime"
	"github.com/sentania-labs/benchwarmer/internal/signals"
)

// Reconcile terminates runtime processes this adapter does not own (ADR 0002
// decision 5): any process whose full image path equals cfg.Executable
// (case-insensitive, cleaned), or, only when its path is unreadable, whose
// executable name matches. Members of instances this adapter currently owns
// are never touched. It returns every candidate with the outcome; the error
// is non-nil only when the process list itself could not be read.
func (a *Adapter) Reconcile(cfg config.Runtime) ([]runtime.Orphan, error) {
	procs, err := signals.Snapshot()
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{normPath(cfg.Executable): true}
	if real, err := filepath.EvalSymlinks(cfg.Executable); err == nil {
		targets[normPath(real)] = true
	}
	name := strings.ToLower(filepath.Base(cfg.Executable))
	owned := a.ownedPIDs()
	self := uint32(os.Getpid())

	var out []runtime.Orphan
	for _, p := range procs {
		if p.PID == 0 || p.PID == self || owned[p.PID] {
			continue
		}
		byName := p.Path == "" && strings.EqualFold(p.Name, name)
		if !byName && (p.Path == "" || !targets[normPath(p.Path)]) {
			continue
		}
		verify := func(path string) bool {
			if path == "" {
				return byName
			}
			return targets[normPath(path)]
		}
		if byName && !processAlive(p.PID) {
			// On Unix an unreadable path usually means a zombie awaiting
			// its parent; it holds nothing and cannot be killed again.
			continue
		}
		o := runtime.Orphan{PID: p.PID, Path: p.Path, CreateTime: p.CreateTime}
		if o.Path == "" {
			o.Path = p.Name
		}
		if err := terminate(p.PID, verify, orphanKillWait); err != nil {
			if errors.Is(err, errNoLongerMatches) {
				continue // exited and the PID was reused since the snapshot
			}
			o.Err = err.Error()
		} else {
			o.Killed = true
		}
		out = append(out, o)
	}
	return out, nil
}

// normPath compares image paths case-insensitively with / and \ alike.
func normPath(p string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(p, `\`, string(filepath.Separator)))))
}
