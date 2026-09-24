// Package proxy is the inference listener: an OpenAI-compatible reverse
// proxy in front of the managed runtime. Its Gate is the admission control
// the controller closes before any drain or preemption begins, so no request
// is ever admitted after the decision to yield.
package proxy

import (
	"context"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Closed describes why the gate is closed, for the 503 body.
type Closed struct {
	Condition  state.Condition
	Reason     string
	RetryAfter time.Time // zero when unknown
}

// Ticket is one admitted request.
type Ticket struct {
	ID       string
	Started  time.Time
	upstream *url.URL
	cancel   context.CancelCauseFunc
}

// Gate tracks admission and active requests. All admission decisions happen
// under one mutex, so once Close returns no further Admit can succeed.
type Gate struct {
	mu       sync.Mutex
	open     bool
	upstream *url.URL
	closed   Closed
	active   map[string]*Ticket
	seq      atomic.Uint64
	notify   chan struct{}
	now      func() time.Time
	rejected atomic.Uint64
}

// NewGate returns a closed gate.
func NewGate(now func() time.Time) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{
		active: map[string]*Ticket{},
		notify: make(chan struct{}, 1),
		now:    now,
		closed: Closed{Condition: state.Unavailable, Reason: "starting"},
	}
}

// Open starts admitting requests to upstream.
func (g *Gate) Open(upstream *url.URL) {
	g.mu.Lock()
	g.open, g.upstream = true, upstream
	g.mu.Unlock()
	g.signal()
}

// Close stops admission. It returns the number of requests still active.
func (g *Gate) Close(c Closed) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = false
	g.closed = c
	return len(g.active)
}

// UpdateClosed refreshes the 503 details while closed (e.g. a new retry time).
func (g *Gate) UpdateClosed(c Closed) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.closed = c
	}
}

// IsOpen reports whether requests are admitted.
func (g *Gate) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// admit registers a request if the gate is open.
func (g *Gate) admit(parent context.Context) (*Ticket, context.Context, Closed, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		g.rejected.Add(1)
		return nil, nil, g.closed, false
	}
	ctx, cancel := context.WithCancelCause(parent)
	t := &Ticket{ID: g.newID(), Started: g.now(), upstream: g.upstream, cancel: cancel}
	g.active[t.ID] = t
	return t, ctx, Closed{}, true
}

func (g *Gate) newID() string {
	return "bw-" + strconv.FormatInt(g.now().UnixMilli(), 36) + "-" + strconv.FormatUint(g.seq.Add(1), 36)
}

func (g *Gate) release(t *Ticket) {
	g.mu.Lock()
	delete(g.active, t.ID)
	g.mu.Unlock()
	t.cancel(nil)
	g.signal()
}

func (g *Gate) signal() {
	select {
	case g.notify <- struct{}{}:
	default:
	}
}

// Changed receives a value whenever a request finishes or the gate opens, so
// the controller can react to a drain completing without waiting for a tick.
func (g *Gate) Changed() <-chan struct{} { return g.notify }

// Active returns the number of active requests and the oldest one's age.
func (g *Gate) Active() (n int, oldest time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for _, t := range g.active {
		if a := now.Sub(t.Started); a > oldest {
			oldest = a
		}
	}
	return len(g.active), oldest
}

// ActiveIDs lists active request IDs.
func (g *Gate) ActiveIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.active))
	for id := range g.active {
		ids = append(ids, id)
	}
	return ids
}

// ErrPreempted is the cancellation cause for requests cut off by preemption.
var ErrPreempted = preemptedError{}

type preemptedError struct{}

func (preemptedError) Error() string { return "request terminated: worker preempted" }

// ForceCloseAll cancels every active request (their connections are cut)
// and returns their IDs. The gate should already be closed.
func (g *Gate) ForceCloseAll() []string {
	g.mu.Lock()
	ts := make([]*Ticket, 0, len(g.active))
	for _, t := range g.active {
		ts = append(ts, t)
	}
	g.mu.Unlock()
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		t.cancel(ErrPreempted)
		ids = append(ids, t.ID)
	}
	return ids
}

// Rejected returns the total number of requests refused while closed.
func (g *Gate) Rejected() uint64 { return g.rejected.Load() }
