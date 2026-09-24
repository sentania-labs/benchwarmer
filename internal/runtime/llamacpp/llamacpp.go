// Package llamacpp implements runtime.Adapter for llama.cpp's llama-server.
//
// The runtime runs inside a procgroup boundary (a Job Object on Windows, ADR
// 0002), listens only on the configured loopback port, and is stopped by
// terminating the whole tree; there is no graceful-shutdown attempt (ADR
// 0002 decision 4). Its stdout and stderr go to a bounded, redacted tail.
package llamacpp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/procgroup"
	"github.com/sentania-labs/benchwarmer/internal/runtime"
)

// Errors the controller can distinguish with errors.Is.
var (
	// ErrPortInUse: something already listens on the private port, so a
	// readiness probe could be answered by the wrong process.
	ErrPortInUse = errors.New("llamacpp: runtime port already in use")
	// ErrExited: the runtime exited before becoming ready.
	ErrExited = errors.New("llamacpp: runtime exited before ready")
	// ErrLoadTimeout: the runtime did not become ready within LoadTimeout.
	ErrLoadTimeout = errors.New("llamacpp: model load timed out")

	errNoLongerMatches = errors.New("process no longer matches the runtime executable")
)

const (
	pollInterval   = 100 * time.Millisecond
	healthTimeout  = 2 * time.Second
	orphanKillWait = 5 * time.Second
)

var (
	_ runtime.Adapter  = (*Adapter)(nil)
	_ runtime.Instance = (*Instance)(nil)
)

// Adapter starts llama-server instances and remembers which ones it owns,
// so Reconcile never kills them.
type Adapter struct {
	mu    sync.Mutex
	owned map[*Instance]struct{}
}

// New returns an Adapter that owns no instances.
func New() *Adapter { return &Adapter{owned: map[*Instance]struct{}{}} }

// Validate checks that the executable is a regular file, the model is a
// non-empty regular file, and the listener is loopback.
func (a *Adapter) Validate(cfg config.Runtime) error {
	var errs []error
	if fi, err := os.Stat(cfg.Executable); err != nil {
		errs = append(errs, fmt.Errorf("executable: %w", err))
	} else if !fi.Mode().IsRegular() {
		errs = append(errs, fmt.Errorf("executable %s is not a regular file", cfg.Executable))
	}
	if fi, err := os.Stat(cfg.ModelPath); err != nil {
		errs = append(errs, fmt.Errorf("model: %w", err))
	} else if !fi.Mode().IsRegular() {
		errs = append(errs, fmt.Errorf("model %s is not a regular file", cfg.ModelPath))
	} else if fi.Size() == 0 {
		errs = append(errs, fmt.Errorf("model %s is empty", cfg.ModelPath))
	}
	if !isLoopback(cfg.Host) {
		errs = append(errs, fmt.Errorf("host %q is not a loopback address", cfg.Host))
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d is out of range", cfg.Port))
	}
	return errors.Join(errs...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Args returns the llama-server argument list for cfg: Benchwarmer's own
// flags first, then the configured extras.
func Args(cfg config.Runtime) []string {
	a := []string{
		"-m", cfg.ModelPath,
		"--host", cfg.Host,
		"--port", strconv.Itoa(cfg.Port),
		"-c", strconv.Itoa(cfg.ContextSize),
		"-ngl", strconv.Itoa(cfg.GPULayers),
	}
	return append(a, cfg.Args...)
}

// Start validates cfg, refuses if the port is already bound, and launches
// llama-server inside a new process-tree boundary.
func (a *Adapter) Start(ctx context.Context, cfg config.Runtime) (runtime.Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.Validate(cfg); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrPortInUse, addr, err)
	}
	_ = ln.Close()

	i := &Instance{
		a:       a,
		cfg:     cfg,
		base:    &url.URL{Scheme: "http", Host: addr},
		out:     newTail(cfg.DiagnosticsTailBytes),
		secrets: secretValues(cfg.Args),
		client: &http.Client{
			Timeout:   healthTimeout,
			Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		},
	}
	g, err := procgroup.Start(procgroup.Spec{
		Path:   cfg.Executable,
		Args:   Args(cfg),
		Dir:    filepath.Dir(cfg.Executable),
		Stdout: i.out,
		Stderr: i.out,
	})
	if err != nil {
		return nil, err
	}
	i.g = g
	a.mu.Lock()
	a.owned[i] = struct{}{}
	a.mu.Unlock()
	return i, nil
}

func (a *Adapter) release(i *Instance) {
	a.mu.Lock()
	delete(a.owned, i)
	a.mu.Unlock()
}

// ownedPIDs lists every PID inside a boundary this adapter owns.
func (a *Adapter) ownedPIDs() map[uint32]bool {
	a.mu.Lock()
	insts := make([]*Instance, 0, len(a.owned))
	for i := range a.owned {
		insts = append(insts, i)
	}
	a.mu.Unlock()
	out := map[uint32]bool{}
	for _, i := range insts {
		out[uint32(i.g.PID())] = true
		m, _ := i.g.Members()
		for _, p := range m {
			out[uint32(p)] = true
		}
	}
	return out
}

// Instance is one llama-server process tree.
type Instance struct {
	a       *Adapter
	g       *procgroup.Group
	cfg     config.Runtime
	base    *url.URL
	out     *tail
	secrets []string
	client  *http.Client

	stopMu  sync.Mutex
	stopped bool
	stopRes runtime.StopResult
}

// PID returns the root process ID.
func (i *Instance) PID() int { return i.g.PID() }

// Members lists the PIDs in the boundary; empty once stopped.
func (i *Instance) Members() []int {
	i.stopMu.Lock()
	stopped := i.stopped
	i.stopMu.Unlock()
	if stopped {
		return nil
	}
	m, err := i.g.Members()
	if err != nil {
		select {
		case <-i.g.Done():
			return nil
		default:
			return []int{i.g.PID()}
		}
	}
	return m
}

// BaseURL returns the runtime's private loopback endpoint.
func (i *Instance) BaseURL() *url.URL {
	u := *i.base
	return &u
}

// Exited is closed when the root process has exited.
func (i *Instance) Exited() <-chan struct{} { return i.g.Done() }

// ExitErr describes how the root exited: nil while running, non-nil after
// any exit (including status 0, which is still unexpected for a server).
func (i *Instance) ExitErr() error {
	select {
	case <-i.g.Done():
	default:
		return nil
	}
	if err := i.g.ExitErr(); err != nil {
		return err
	}
	return errors.New("llamacpp: runtime exited with status 0")
}

// Started returns when the process was started.
func (i *Instance) Started() time.Time { return i.g.Started() }

// Diagnostics returns the retained output tail with secrets redacted.
func (i *Instance) Diagnostics() string { return redact(i.out.String(), i.secrets) }

// WaitReady polls GET /health until it returns 200. It fails promptly when
// the process exits, when ctx ends, or once LoadTimeout has elapsed since
// the process started.
func (i *Instance) WaitReady(ctx context.Context) error {
	var timeout <-chan time.Time
	if lt := i.cfg.LoadTimeout.D(); lt > 0 {
		t := time.NewTimer(time.Until(i.Started().Add(lt)))
		defer t.Stop()
		timeout = t.C
	}
	health := i.base.JoinPath("health").String()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-i.g.Done():
			return fmt.Errorf("%w: %v", ErrExited, i.ExitErr())
		default:
		}
		if i.healthy(ctx, health) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("%w after %s", ErrLoadTimeout, i.cfg.LoadTimeout.D())
		case <-i.g.Done():
			return fmt.Errorf("%w: %v", ErrExited, i.ExitErr())
		case <-tick.C:
		}
	}
}

func (i *Instance) healthy(ctx context.Context, u string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := i.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Stop terminates the whole tree and waits up to timeout for the root to
// exit and the boundary to empty. A successful Stop is final: later calls
// return the same result. After a failed Stop the instance stays owned, so
// a retry kills again.
func (i *Instance) Stop(timeout time.Duration) (runtime.StopResult, error) {
	i.stopMu.Lock()
	defer i.stopMu.Unlock()
	if i.stopped {
		return i.stopRes, nil
	}
	var res runtime.StopResult
	select {
	case <-i.g.Done():
		res.AlreadyExited = true
	default:
	}
	t0 := time.Now()
	if err := i.g.Kill(); err != nil && !errors.Is(err, procgroup.ErrNotRunning) {
		return res, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	if !res.AlreadyExited {
		select {
		case <-i.g.Done():
			res.RootExit = time.Since(t0)
		case <-deadline.C:
			return res, fmt.Errorf("llamacpp: root process %d did not exit within %s", i.g.PID(), timeout)
		}
	}
	for {
		m, err := i.g.Members()
		if err != nil {
			return res, err
		}
		if len(m) == 0 {
			res.TreeEmpty = time.Since(t0)
			break
		}
		select {
		case <-deadline.C:
			return res, fmt.Errorf("llamacpp: %d process(es) still in the runtime tree after %s", len(m), timeout)
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = i.g.Close()
	i.stopped, i.stopRes = true, res
	i.a.release(i)
	return res, nil
}
