# ADR 0002: Runtime process ownership

Status: Accepted (2026-09-23). VRAM release timing pending Phase 0 E3.

## Context

The worker must terminate llama-server and every descendant, verify it, and
never leave an orphan holding VRAM, including when the worker itself crashes.
Tracking a parent PID is racy (PID reuse, grandchildren, the worker dying).
llama-server's graceful shutdown is Ctrl+C, which a service without a console
cannot deliver reliably.

## Decision

1. Create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` and no
   breakaway permission. The handle is non-inheritable.
2. Start llama-server `CREATE_SUSPENDED | CREATE_NO_WINDOW |
   CREATE_NEW_PROCESS_GROUP`, assign it to the job, then resume it. It cannot
   run a single instruction outside the job.
3. Stop = `TerminateJobObject`, then verify: root reaped, job process list
   empty, runtime counter instances gone, adapter dedicated memory back
   within tolerance of the pre-load baseline. Each verification has a timeout
   and emits an event with the measured duration.
4. No graceful-shutdown attempt. The worker only stops the runtime when no
   request is active or when grace has expired; in both cases a graceful
   stop gives the caller nothing. Revisit only if E3 shows VRAM release is
   slower after a hard kill than after a clean exit.
5. The worker's own death closes the job handle and the kernel kills the tree.
   On startup the worker still scans for processes whose image path equals the
   configured runtime executable and kills them (belt and braces for a
   previous worker that was killed while a handle leaked, or a runtime started
   by hand).

## Consequences

- Orphans are prevented by the kernel, not by worker bookkeeping.
- On Linux (dev only) a process group plus parent-death signal approximates
  this; it does not cover grandchildren if the owner dies.
- Startup reconciliation will kill a llama-server the user started by hand
  from the same path. Documented; the configured path is Benchwarmer's.
