# ADR 0006: Service identity

Status: Accepted (2026-09-24), from target measurements.

## Context

Least privilege says a virtual service account (`NT SERVICE\Benchwarmer`).
Unknowns: whether llama-server gets the GPU from session 0 under that
account, whether the account can read GPU performance counters, and whether
it can read image paths of the user's processes (needed for path-based
application rules).

## Evidence (target PC, 2026-09-24)

| Identity | llama-server on the GPU | GPU performance counters | User-session process list and paths |
|---|---|---|---|
| Virtual account `NT SERVICE\<name>` | Works (Vulkan, all layers on the RX 9060 XT) | Denied (`PdhAddEnglishCounter` 0xC0000BC8) | Session 1 not visible; 1 of 118 paths readable |
| LocalSystem | Works | Full (per-process engines and memory, temperature) | Full |
| LocalSystem service, runtime as privilege-stripped LocalService | Works; token verified as `NT AUTHORITY\LOCAL SERVICE` holding only `SeChangeNotifyPrivilege` | Full (read by the service) | Full |

Without counters and the process list, detection has nothing to work with,
so the virtual account cannot run the service.

## Decision

- The service runs as **LocalSystem**, the only identity that can read the
  GPU counters and the interactive user's processes.
- The runtime does **not** inherit that: by default (`runtime.run_as =
  localservice`) the service logs on `NT AUTHORITY\LocalService` and starts
  llama-server with a restricted copy of that token (every privilege
  removed except `SeChangeNotifyPrivilege`, integrity lowered from System to
  Medium), still created suspended inside the Job Object. If the token cannot
  be obtained the runtime does not start; there is no fallback to the
  service's own rights. A flaw in llama-server's parsing of network-sourced input
  then yields a low-privilege account, not SYSTEM.
- `runtime.run_as = service` exists for development and is not
  recommended. The installer supports only LocalSystem, and the service
  raises `service_misconfigured` if it is not LocalSystem while
  `run_as = localservice`.
- Known limit: LocalService is shared by other Windows services, so the
  runtime is isolated from SYSTEM, not from them. A per-runtime restricting
  SID would tighten this later.
- The runtime directory (Program Files) and the models directory must be
  readable by Users, which the installer ensures.

## Consequences

- The service's own attack surface (Go proxy and API) runs as SYSTEM; the
  inference listener is loopback by default and requires TLS and a token
  when exposed (ADR 0007, ADR 0010).
- Superseded text: the provisional virtual-account default below no longer
  applies.

## Original provisional decision (superseded)

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
