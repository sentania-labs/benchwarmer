# ADR 0004: State model and policy architecture

Status: Accepted (2026-09-23)

## Context

Several components (proxy, runtime, telemetry, API, UI) observe and influence
the lifecycle. If each keeps its own notion of state, they drift. The spec
wants every decision explainable and table-testable.

## Decision

- **One state machine, one writer.** `internal/state` defines the internal
  states (STOPPED, COOLDOWN, LOADING, READY, BUSY, DRAINING, PREEMPTING,
  SUPPRESSED, DISABLED, ERROR) and the only legal transitions. The controller
  goroutine is the only code that changes state. Everyone else reads
  snapshots.
- **Three user conditions.** Available (LOADING, READY, BUSY), Yielding
  (DRAINING, PREEMPTING), Unavailable (everything else). The mapping is a
  function in `internal/state`, not repeated in the UI.
- **Policy is a pure function.** `policy.Evaluate(Snapshot, Config) Decision`.
  The snapshot holds every fact (telemetry, signals, runtime state, request
  counts, time, schedule, mode, cooldown and suppression deadlines). No I/O,
  no clock reads; time comes in the snapshot. Rules run in the spec's
  precedence order and the first that fires wins; lower rules are still
  evaluated and reported as "also matched" for explanation.
- **Decision drives the controller.** The decision's desired behavior is one
  of RUN, HOLD (do not load, but do not stop a running runtime), DRAIN
  (stop admitting, grace), PREEMPT (stop admitting, kill now). The controller
  translates desired behavior plus current state into transitions and
  side effects, and emits an event for every transition carrying the decision.
- **Admission is a gate owned by the controller.** The proxy consults an
  atomic gate; the controller closes it before any drain or preempt begins,
  so no request is accepted after the decision to yield.

## Consequences

- Policy tests are tables of snapshots and expected decisions.
- Hysteresis windows (e.g. "above 60% for 3 s") are computed by the signal
  layer into snapshot facts, keeping the evaluator stateless.
