# ADR 0006: Service identity

Status: Provisional (2026-09-23), pending Phase 0 E2 and E7.

## Context

Least privilege says a virtual service account (`NT SERVICE\Benchwarmer`).
Unknowns: whether llama-server gets the GPU from session 0 under that
account, whether the account can read GPU performance counters, and whether
it can read image paths of the user's processes (needed for path-based
application rules).

## Decision

- Default: virtual account `NT SERVICE\Benchwarmer`, added by the installer to
  Performance Monitor Users if E7 shows that is required, with read/execute on
  the runtime and model directories and modify on `%ProgramData%\Benchwarmer`.
- If E2 shows the GPU is unavailable from session 0 under the virtual account
  but available under LocalSystem, use LocalSystem and record the specific
  reason here.
- If E7 shows paths are unreadable, application rules still match by
  executable name (always readable from the process snapshot) and path rules
  are reported as unevaluable in the UI rather than silently failing.

## Consequences

- The `bwprobe svctest` results decide this; the installer takes the account as
  a parameter so the choice is a config change, not a code change.
