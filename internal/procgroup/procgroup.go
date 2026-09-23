// Package procgroup starts a child process inside an ownership boundary that
// covers its entire descendant tree, so the tree can be terminated as a unit
// and cannot outlive its owner.
//
// On Windows the boundary is a Job Object created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; the child is created suspended and
// assigned to the job before its first instruction runs (ADR 0002). On Unix,
// which is used only for development and tests, the boundary is a process
// group plus a parent-death signal.
package procgroup

import (
	"errors"
	"io"
	"sync"
	"time"
)

// Spec describes the process to start.
type Spec struct {
	Path   string
	Args   []string // arguments, not including argv[0]
	Dir    string
	Env    []string // nil means inherit
	Stdout io.Writer
	Stderr io.Writer
}

// ErrNotRunning is returned when an operation needs a live group.
var ErrNotRunning = errors.New("procgroup: process not running")

// Group is a started process tree.
type Group struct {
	pid     int
	started time.Time
	done    chan struct{}

	mu      sync.Mutex
	exitErr error
	exited  time.Time

	plat platform
}

// PID returns the root process ID.
func (g *Group) PID() int { return g.pid }

// Started returns when the root process was started.
func (g *Group) Started() time.Time { return g.started }

// Done is closed when the root process has exited and been reaped.
func (g *Group) Done() <-chan struct{} { return g.done }

// ExitErr returns the root process's exit error once Done is closed.
func (g *Group) ExitErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exitErr
}

// Exited returns when the root process exit was observed (zero if running).
func (g *Group) Exited() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exited
}

// Start launches spec inside a new ownership boundary.
func Start(spec Spec) (*Group, error) {
	if spec.Path == "" {
		return nil, errors.New("procgroup: empty path")
	}
	return start(spec)
}

// Members returns the PIDs currently inside the boundary, including the root.
func (g *Group) Members() ([]int, error) { return g.members() }

// Kill terminates every process in the boundary. It does not wait.
func (g *Group) Kill() error { return g.kill() }

// KillAndWait terminates the tree and waits up to timeout for the root to be
// reaped and the boundary to report no members. It returns how long that took.
func (g *Group) KillAndWait(timeout time.Duration) (time.Duration, error) {
	t0 := time.Now()
	if err := g.kill(); err != nil && !errors.Is(err, ErrNotRunning) {
		return time.Since(t0), err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case <-g.done:
	case <-deadline.C:
		return time.Since(t0), errors.New("procgroup: root process did not exit before timeout")
	}
	for {
		m, err := g.members()
		if err != nil {
			return time.Since(t0), err
		}
		if len(m) == 0 {
			return time.Since(t0), nil
		}
		select {
		case <-deadline.C:
			return time.Since(t0), errors.New("procgroup: descendants still present after timeout")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Close releases the boundary. On Windows, closing the last job handle kills
// anything still inside it.
func (g *Group) Close() error { return g.close() }

func (g *Group) setExit(err error) {
	g.mu.Lock()
	g.exitErr = err
	g.exited = time.Now()
	g.mu.Unlock()
	close(g.done)
}
