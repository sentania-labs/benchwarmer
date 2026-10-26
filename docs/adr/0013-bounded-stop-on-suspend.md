# ADR 0013: Stop the runtime inside the suspend notification

Status: Provisional (2026-10-26). Simulated only; the budget and the effect
on the AMD host are to be confirmed on the target (see Open).

## Context

Issue #12. On the target (SS8510) the runtime was still loaded when Windows
entered S3, and was found hard-killed (`kill_failed`) after wake, on
2026-09-25 and again on 2026-10-02. On 2026-10-02: suspend received 10:55:31,
S3 at 10:55:32, resume 10:57:50, GPU watchdog dumps (`0x141`) at 10:57:54 and
10:58:02, "runtime PID 6532 had not exited within the 10-second verification
timeout" at 10:58:03, bugcheck `0x116` in `amdkmdag.sys` at 10:58:42. Which
part of that (holding the GPU context across sleep, the teardown, or the
driver's own D3 defect, ADR 0011) starts the hang is not isolated.

Two defects in the service, not the driver, kept the stop from happening
before sleep:

1. **The suspend work started after the notification was over.** Windows
   delivers `PBT_APMSUSPEND` to a service through its control handler and
   proceeds once the handler returns. `x/sys/windows/svc` returns as soon as
   the request is queued for the service's `Execute` loop, so the service
   began `PrepareSuspend` while the machine was already going down. Nothing
   the service did afterwards could be inside the window.
2. **The controller stalled behind a graceful stop.** `Instance.Members`
   took the lock `Instance.Stop` holds for the whole Ctrl+C wait. The
   controller calls `Members` every tick under its own lock, so while a
   graceful stop ran (up to `runtime.graceful_stop_timeout`, 5 s) every
   other controller call, including `PrepareSuspend`'s wait, blocked behind
   it. A "5 second" wait could not return in less than the stop itself.

## Decision

1. **Hold the notification until the runtime is stopped.** `internal/winsvc`
   hosts the service itself instead of through `x/sys/windows/svc`. Its
   control handler queues every request and returns at once, except
   `PBT_APMSUSPEND`: that call waits until the service has handled the
   event (`Event.Ack`) or the suspend budget passes. The service starts the
   graceful stop on receipt, waits for the runtime to be gone, then
   acknowledges. One host, one place where the wait is defined, tested off
   Windows (`supervisor_test.go`).
2. **Budget: 1.5 seconds** (`winsvc.DefaultSuspendBudget`). The budget is
   the longest the system is held back from sleeping. Windows documents
   about two seconds for a power-broadcast recipient and no figure for a
   service control handler; 1.5 s leaves half a second of margin inside that
   documented ceiling for queueing and the acknowledgement itself. The real
   limit for a service control handler is still open and must be confirmed
   on the AMD host. The graceful stop exited in 0.8 s on the target
   (ADR 0011), so the budget covers it with margin. Simulated, the stop of
   the fake runtime and its child, from event to acknowledgement, takes
   20-65 ms (see Verification).
3. **No hard kill to meet the budget.** If the runtime is still alive when
   the budget ends, the notification is released and the stop that is
   already running continues on its own terms (Ctrl+C was delivered; the
   graceful timeout, then the fallback kill, then verification). An
   unconditional kill is rejected: the earlier teardown hangs on this driver
   followed hard kills (ADR 0011), and the 2026-10-02 evidence does not show
   that a kill before sleep is safer than a graceful stop. This is the
   existing resume-side cleanup, kept as the fallback.
4. **Make the outcome visible.** `PrepareSuspend` emits `power_suspend_stop`
   with `stopped`, `late`, `elapsed_ms`, and `budget_ms`; a stop that ran
   out of budget, a stop first seen complete after the budget ended
   (`late`, `stopped` false: the host had already released the notification,
   so the system may have slept with the model loaded), or a runtime whose
   termination is still unverified (a retained `kill_failed` process, named
   by PID), is a warning. The existing
   `runtime_stopped` event keeps the per-stage timings (`root_exit_ms`,
   `tree_empty_ms`, `graceful`).
5. **Unblock the controller.** `Instance.Members` no longer waits for a stop
   in progress.

The host also reads the session id of a session-change notification inside
the control handler, where the SCM guarantees the memory, instead of after
the request has been queued (the "best effort" read the old code noted).

## Consequences

- The runtime is normally gone, and its VRAM released, before S3. Sleep is
  delayed by the stop time, at most the budget.
- With the budget exhausted, behavior is what it was before, plus the
  `power_suspend_stop` warning that says so.
- The service owns its SCM glue (about 100 lines in `run_windows.go`, a port
  of the parts of `x/sys/windows/svc` the service used). That code only runs
  as a service and is exercised by the CI service smoke tests, not by unit
  tests.
- `winsvc.Event` gained `Done` and `Ack`; a Suspend must be acknowledged on
  every path.

## Verification

Simulated (Linux, `fakellama` as the runtime, real process tree and Ctrl+C
as SIGINT; `internal/service/suspend_test.go`, run with `-v`):

| Case | Result |
|---|---|
| Runtime serving, graceful stop | acknowledgement 20-65 ms after the event, runtime gone, `power_suspend_stop` `stopped=true` |
| Runtime ignores Ctrl+C, budget 700 ms | released at 707 ms with the runtime still alive (not killed); stop completes after "resume" via the fallback; nothing reloads before the resume cooldown |

The control handler's hold, release at the budget, drop handling, and
stop-during-suspend are covered in `internal/winsvc/supervisor_test.go`.

## Open (needs the AMD host)

- Whether Windows actually waits for a service's handler on `PBT_APMSUSPEND`
  and for how long; the budget may need to move.
- Real stop time under the service (no console of its own, Ctrl+C through
  the console the service allocates, ADR 0011) with a model loaded and
  requests active.
- Whether releasing VRAM before S3 changes the resume-side GPU hang. This
  ADR fixes the service's timing defect; it does not claim to fix the driver
  defect or the crash that followed the 2026-10-02 resume.
