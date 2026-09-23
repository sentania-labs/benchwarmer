# Phase 0 plan and specification analysis

Status: in progress (started 2026-09-23)

**Bottom line:** the specification is buildable as written. Two items need a
design answer the spec does not give (session-0 visibility of user signals, and
AI Priority vs. game detection); both are resolved below without changing
scope. Every threshold and the telemetry source choice depend on measurements
from the target PC, which the development workstation cannot reach, so Phase 0
ships a probe tool (`bwprobe`) and a runbook that produce those measurements,
and the rest of the system is built against fakes with provisional defaults.

## 1. Components and contracts

| Component | Package | Contract it owns or consumes |
|---|---|---|
| Service lifecycle | `cmd/benchwarmer`, `internal/winsvc` | Windows SCM stop/shutdown/power events in, controller commands out |
| Controller (state machine driver) | `internal/controller` | Owns `state.State`; the only writer of lifecycle state |
| Policy evaluator | `internal/policy` | `policy.Snapshot` in, `policy.Decision` out; pure function |
| Signal collection | `internal/telemetry`, `internal/signals` | Produces the fact half of `policy.Snapshot` |
| App classification | `internal/classify` | Config rules in, per-process class out |
| Schedule | `internal/schedule` | Config + clock in, active profile out |
| Runtime adapter | `internal/runtime` | `runtime.Adapter`: validate, start, ready, stop, kill, exit notification |
| Process ownership | `internal/procgroup` | Job Object on Windows, process group on Unix (dev only) |
| Reverse proxy | `internal/proxy` | Admission gate; request accounting feeds the snapshot |
| Persistence | `internal/store` | SQLite: config versions, overrides, events, stats |
| Events / audit | `internal/events` | `events.Event`, durable and explainable |
| Management API | `internal/api` | `/api/v1/*` JSON, OpenAPI document |
| Metrics | `internal/metrics` | Prometheus text on `/metrics` |
| Web UI | `internal/webui` | Static assets embedded in the service binary |
| Tray | `cmd/bwtray` | Talks only to the management API |
| Session agent | `cmd/bwtray` (same binary) | Reports foreground, fullscreen, idle time to the service |
| Installer | `installer/` | Service registration, firewall rule, ACLs |

## 2. Requirements that need the target machine

Each is answered by a `bwprobe` scenario in the runbook
([runbook.md](runbook.md)); results land in `docs/phase0/findings.md`.

| # | Question | Why it cannot be assumed | Probe |
|---|---|---|---|
| E1 | Does llama-server run on the RX 9060 XT (gfx1200) with the HIP/ROCm build, the Vulkan build, or both? | RDNA4 support on Windows HIP is recent; Vulkan is the common fallback | `bwprobe runtime` per build |
| E2 | Does llama-server get the GPU when launched from session 0 (service context), under LocalSystem and under a virtual service account? | Some GPU stacks refuse or fall back to CPU without an interactive desktop | `bwprobe svctest` |
| E3 | Does Job Object termination release VRAM, and how fast? | Driver cleanup of a killed process is asynchronous | `bwprobe runtime` cycles |
| E4 | Are per-process `GPU Engine` and `GPU Process Memory` counters present, attributable, and consistent with adapter totals on this driver? | Counters exist on all WDDM drivers; accuracy varies | `bwprobe telemetry` |
| E5 | Which engine type does llama-server use (Compute vs 3D vs Copy)? | Decides how own vs. game load separate | `bwprobe telemetry` during inference |
| E6 | Is GPU temperature available through D3DKMT adapter perf data? | Driver-optional field | `bwprobe env`, `telemetry` |
| E7 | Can a service account read full image paths of the user's processes and the GPU counters? | Path rules and attribution depend on it | `bwprobe svctest` |
| E8 | What do idle desktop, launcher, game, browser video, and OBS/Discord look like in these counters? | Thresholds come from this | `bwprobe telemetry --label ...` |
| E9 | What happens across sleep/resume and a driver reset (TDR)? | Counter instances and the runtime may vanish | `bwprobe telemetry` spanning the event |

## 3. ADRs

| ADR | Decision | State |
|---|---|---|
| [0001](../adr/0001-implementation-stack.md) | Go, single service binary, pure-Go SQLite | Accepted |
| [0002](../adr/0002-process-ownership.md) | Job Object with kill-on-close, created suspended | Accepted, VRAM timing pending E3 |
| [0003](../adr/0003-telemetry-sources.md) | PDH GPU counters + D3DKMT perf data, multi-signal fallback | Provisional pending E4-E8 |
| [0004](../adr/0004-state-and-policy.md) | Pure policy evaluator, single-writer controller, internal/external state mapping | Accepted |
| [0005](../adr/0005-session-signals.md) | User-session agent reports foreground/idle; service works without it | Accepted |
| [0006](../adr/0006-service-identity.md) | Virtual service account if E2/E7 pass, LocalSystem fallback | Provisional pending E2, E7 |
| [0007](../adr/0007-authentication.md) | Bearer tokens, separate management and inference credentials | Accepted |
| [0008](../adr/0008-persistence.md) | SQLite (modernc.org/sqlite), config also kept as last-known-good file | Accepted |
| 0009 | Installer strategy | Deferred to Phase 2 |

## 4. Parallel work after contracts land

Contracts (`state`, `policy` types, `events`, `config` schema, `runtime.Adapter`,
API shapes) are committed by the lead first. After that:

- **Windows/runtime:** `procgroup` Windows impl, llama.cpp adapter, PDH/D3DKMT
  collectors, Windows service wrapper, power events, session agent.
- **Policy/API:** evaluator and table tests, schedule, controller timers,
  store, events, API, metrics.
- **UI/packaging:** starts once the API is stable: web UI, tray, installer.

## 5. Contradictions and gaps, with resolutions

1. **Session 0 cannot see the user's desktop.** A Windows service cannot call
   `GetForegroundWindow` or `GetLastInputInfo` for the logged-in user, yet the
   spec lists foreground, fullscreen, and input idle as signals and requires
   the service to work with nobody logged in. Resolution (ADR 0005): process
   list and GPU telemetry, which are visible from session 0, are the primary
   signals. Foreground/fullscreen/idle come from a small agent in the user
   session (the tray binary). If the agent is absent, those facts are
   `unknown` and the policy does not depend on them for safety.

2. **AI Priority vs. game detection.** Section 5 says AI Priority never
   bypasses hard safety or critical contention, implying it may bypass other
   things; section 7 ranks confirmed gaming above AI Priority. Resolution: the
   precedence list governs. AI Priority suppresses early-warning signals
   (a game process appearing, ambiguous interactive signals, soft pressure)
   and gives longer grace and shorter cooldown, but *confirmed* gaming (game
   process plus foreground/fullscreen or measurable external GPU load) still
   yields.

3. **Graceful runtime shutdown from a service.** llama-server's graceful path
   is Ctrl+C, and a service has no console to deliver it through. Since the
   worker only stops the runtime when no request is active or grace has
   expired, graceful shutdown buys nothing the caller can observe.
   Resolution (ADR 0002): terminate the job, verify exit, verify VRAM release.

4. **"Required free VRAM" has no stated source.** Resolution: a config value
   per model, defaulting to a provisional figure, with the measured VRAM
   footprint of the last successful load recorded as a statistic for tuning.

5. **ROCm vs. Vulkan.** The spec says "confirm ROCm operation". The adapter is
   backend-agnostic (the backend is whichever llama-server build the config
   points at), so Phase 0 tests both and the findings pick the default.

None of these change scope.

## 6. Provisional thresholds

See [thresholds.md](thresholds.md). Every value is a config field; none are
compiled-in constants except schema bounds used for validation.
