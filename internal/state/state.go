// Package state defines Benchwarmer's lifecycle state machine: the internal
// states, the only legal transitions between them, and the mapping onto the
// three user-visible conditions (ADR 0004). The controller is the only writer
// of state; everything else reads it.
package state

// State is an internal lifecycle state. Exposed in diagnostics, not the
// primary user model.
type State string

// Internal states.
const (
	// Stopped: runtime not running and nothing is blocking a load except
	// eligibility (e.g. insufficient free VRAM). Loads when eligible.
	Stopped State = "STOPPED"
	// Cooldown: runtime not running; a competing workload is present or was
	// seen recently. Loads once the cooldown after it expires.
	Cooldown State = "COOLDOWN"
	// Loading: runtime process started, model loading, not yet ready.
	Loading State = "LOADING"
	// Ready: model loaded, admitting requests, none active.
	Ready State = "READY"
	// Busy: model loaded, admitting requests, one or more active.
	Busy State = "BUSY"
	// Draining: admission closed; active requests get the grace period.
	Draining State = "DRAINING"
	// Preempting: admission closed; the runtime tree is being terminated and
	// its exit and VRAM release verified.
	Preempting State = "PREEMPTING"
	// Suppressed: anti-thrashing hold after repeated preemptions.
	Suppressed State = "SUPPRESSED"
	// Disabled: manual Pause AI.
	Disabled State = "DISABLED"
	// Error: runtime crash, failed kill verification, telemetry loss, device
	// loss, or invalid configuration; recovery uses bounded backoff.
	Error State = "ERROR"
)

// All lists every state, for validation and metrics.
var All = []State{Stopped, Cooldown, Loading, Ready, Busy, Draining, Preempting, Suppressed, Disabled, Error}

// Condition is the user-visible availability.
type Condition string

// User-visible conditions (spec section 4).
const (
	Available   Condition = "Available"
	Yielding    Condition = "Yielding"
	Unavailable Condition = "Unavailable"
)

// Condition maps an internal state onto what the user sees.
func (s State) Condition() Condition {
	switch s {
	case Loading, Ready, Busy:
		return Available
	case Draining, Preempting:
		return Yielding
	default:
		return Unavailable
	}
}

// RuntimeRunning reports whether the runtime process is expected to exist.
func (s State) RuntimeRunning() bool {
	switch s {
	case Loading, Ready, Busy, Draining, Preempting:
		return true
	}
	return false
}

// Admitting reports whether new inference requests may be accepted. Loading
// is Available to the user but still rejects requests until ready.
func (s State) Admitting() bool { return s == Ready || s == Busy }

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, x := range All {
		if s == x {
			return true
		}
	}
	return false
}

// idle states are "runtime not running"; any of them may move to any other,
// since which one applies is decided by the winning policy gate.
var idle = []State{Stopped, Cooldown, Suppressed, Disabled, Error}

var transitions = func() map[State]map[State]bool {
	t := map[State]map[State]bool{}
	allow := func(from State, to ...State) {
		if t[from] == nil {
			t[from] = map[State]bool{}
		}
		for _, x := range to {
			t[from][x] = true
		}
	}
	for _, s := range idle {
		allow(s, idle...)
	}
	// Only an eligible idle state starts a load.
	allow(Stopped, Loading)
	// Loading can finish, fail (crash/timeout), or be cut short. There are no
	// requests while loading, so yielding goes straight to Preempting.
	allow(Loading, Ready, Preempting, Error)
	allow(Ready, Busy, Draining, Preempting, Error)
	allow(Busy, Ready, Draining, Preempting, Error)
	// A drain is a commitment to unload: it never re-opens admission. It
	// ends by terminating the runtime (after requests finish or grace ends).
	allow(Draining, Preempting, Error)
	// After termination is verified the runtime is idle; which idle state is
	// decided by policy. Failed verification is an error.
	allow(Preempting, idle...)
	return t
}()

// CanTransition reports whether from -> to is legal. Self-transitions are not
// transitions and return false.
func CanTransition(from, to State) bool { return transitions[from][to] }
