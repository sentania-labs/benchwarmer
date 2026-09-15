# Opportunistic GPU Worker

## Product and Technical Specification

**Status:** Ready for implementation planning  
**Initial platform:** Windows 11 with AMD Radeon RX 9060 XT 16 GB  
**Initial inference runtime:** `llama.cpp` / `llama-server`  
**Primary use case:** Use an otherwise-idle gaming GPU for local inference without noticeably degrading the owner's interactive or gaming experience.

## 1. Executive summary

Build a lightweight Windows service that runs a local LLM on a gaming PC only while the GPU is available. The service owns the inference-runtime process, observes interactive use and external GPU demand, and automatically yields the GPU when a game or other demanding application needs it.

The service must:

- Start and stop `llama-server` automatically.
- Proxy its OpenAI-compatible inference API so unavailable states can reject new work cleanly.
- Detect likely interactive or gaming use using multiple Windows and GPU signals.
- Stop accepting new requests as soon as the machine should yield.
- Allow an active inference turn a configurable grace period to finish when resources permit.
- Terminate the runtime immediately when actual GPU or VRAM contention becomes urgent.
- Reload after the competing workload ends and a cooldown expires.
- Apply different configurable policies by schedule, including a less-strict school-hours policy from 7:00 AM–5:00 PM, Monday–Friday, in `America/Chicago`.
- Provide a local web UI, tray controls, API, metrics, and an explainable event log.

This is a **single-machine GPU lifecycle controller**, not an inference gateway, cluster scheduler, agent framework, or distributed retry system.

## 2. Product principles

1. **The human owns the GPU.** Human interactive use and games ultimately take priority.
2. **Drain on indication; evict on contention.** A likely game launch stops new inference. Actual pressure can terminate an active turn.
3. **Inference is disposable.** A caller must tolerate a dropped connection if the worker is forcibly preempted.
4. **Use multiple signals.** No individual Windows metric, executable name, or aggregate GPU percentage is authoritative.
5. **Be explainable.** Every load, rejection, drain, termination, cooldown, suppression, and error records the rule and evidence that caused it.
6. **Fail conservatively.** If GPU telemetry or runtime control becomes unreliable, release the GPU and remain unavailable until safely recoverable.
7. **Configuration over code changes.** Schedules, thresholds, executable classifications, grace periods, and cooldowns are editable through both UI and API.

## 3. Scope

### MVP includes

- Windows service with automatic startup.
- Lightweight local web UI and system-tray control.
- `llama-server` process ownership and process-tree termination.
- OpenAI-compatible reverse proxy to the managed runtime.
- Machine-wide and per-process GPU/VRAM telemetry where Windows and the installed driver expose it reliably.
- Process, path, foreground-window, fullscreen, user-input, and interactive-session signals.
- Configurable application classifications: game, launcher, ignore, and ordinary workload.
- Default and scheduled policy profiles.
- Graceful drain followed by forced preemption when required.
- Cooldown, anti-thrashing backoff, and manual operating modes.
- Persistent configuration and bounded event history.
- Health, status, configuration, control, event, and metrics APIs.
- Temperature and runtime-health safety limits.
- Correct behavior across boot, login/logout, lock/unlock, sleep/resume, shutdown, runtime crash, and telemetry failure.

### Explicitly out of scope

- Multi-node routing or load balancing.
- Retrying or resuming interrupted inference requests.
- KV-cache checkpointing or live inference migration.
- Waking a sleeping PC to provide capacity.
- Downloading, selecting, benchmarking, or updating models.
- General agent orchestration.
- Supporting several simultaneous model runtimes.
- Supporting Ollama, ComfyUI, Stable Diffusion, or non-LLM workloads in MVP.
- Automatically discovering every installed game with perfect accuracy.

## 4. User-visible operating model

The UI should avoid exposing the complete internal state machine. It presents three availability conditions:

| Condition | Meaning |
|---|---|
| **Available** | The model is loading or ready, and the worker can accept inference when ready. |
| **Yielding** | The worker accepts no new work and is allowing an active turn a limited time to finish. |
| **Unavailable** | The runtime is stopped because of gaming, external demand, cooldown, suspension, manual disablement, sleep, or error. |

The implementation may use internal states such as `STOPPED`, `COOLDOWN`, `LOADING`, `READY`, `BUSY`, `DRAINING`, `PREEMPTING`, `SUPPRESSED`, `DISABLED`, and `ERROR`. These are exposed in diagnostic data but are not the primary user mental model.

## 5. Operating modes

The tray UI and web UI provide these manual modes:

| Mode | Behavior |
|---|---|
| **Auto** | Normal policy evaluation. This is the default. |
| **Pause AI** | Drain and unload; remain unavailable for a chosen duration, until reboot, until tomorrow, or indefinitely. |
| **AI Priority** | Use a more permissive policy for a chosen duration, but never bypass hard safety rules or critical external GPU/VRAM contention. |

Manual mode changes must be available through the API. Temporary changes expire automatically and revert to `Auto`.

## 6. Policy profiles

Ship with two editable profiles:

### Normal

Designed to be deferential whenever the owner may be using the computer.

- A classified game or strong interactive-gaming signal immediately stops new requests.
- An active turn receives approximately 10–15 seconds to finish if pressure remains low.
- External GPU or VRAM pressure can shorten or bypass the grace period.
- Default cooldown after the competing workload disappears: 5 minutes.

### School hours

Active Monday–Friday from 7:00 AM through 5:00 PM in `America/Chicago`.

- The service is more tolerant of ambiguous signals.
- An active turn may receive up to 60 seconds to finish if pressure remains low.
- Actual external contention still wins immediately.
- Default cooldown: 2 minutes.

Schedules, timezone, profile assignment, and all thresholds are configurable. Daylight-saving transitions use the configured IANA timezone rather than a fixed UTC offset.

## 7. Signals and decisions

### Signals to collect

- Aggregate GPU utilization, memory use, temperature, and health.
- Per-process GPU engine utilization when available.
- Per-process dedicated GPU memory when available.
- Managed runtime PID tree so the service can separate its own demand from external demand.
- Free VRAM and change in external VRAM allocation over time.
- Running executable name, full path, signer or file identity where practical, and parent/child relationship.
- Foreground window and fullscreen/borderless-fullscreen hints.
- Time since last keyboard or mouse input.
- Presence and state of interactive user sessions.
- Runtime state, current request count, request duration, and model-load duration.
- Sleep/resume, display, login/logout, lock/unlock, reboot, and driver/device events.

### Detection requirements

- Do not treat the managed LLM's own GPU use as external pressure.
- Treat process appearance as an early-warning signal and actual resource contention as stronger evidence.
- Support configurable application rules by executable, exact path, path prefix, and optional wildcard.
- Classify applications as `game`, `launcher`, `ignore`, or `ordinary`.
- Launcher presence alone must not be treated as a running game.
- Account for non-game GPU users such as OBS, Discord streaming, browsers, video editors, and other compute applications.
- Use a rolling observation window and hysteresis so brief telemetry spikes do not cause unnecessary unloads.
- Hard safety conditions remain direct rules. Other ambiguous signals may be combined using a documented pressure score if that improves maintainability and tuning.

### Decision precedence

From highest to lowest priority:

1. Hard safety and health protection.
2. Manual `Pause AI`.
3. Critical external GPU or VRAM contention.
4. Confirmed gaming or strong interactive demand.
5. Temporary `AI Priority` behavior.
6. Active schedule profile.
7. Default profile.

Every decision must report the winning rule plus the relevant observed values.

## 8. Preemption behavior

When policy determines that the worker should yield:

1. Atomically mark the proxy unavailable for new requests.
2. Return a machine-readable `503 Service Unavailable` for newly arriving inference requests.
3. If no request is active, stop the runtime immediately.
4. If a request is active and no hard threshold is crossed, allow it to continue for the active profile's grace period.
5. Continue monitoring pressure throughout the grace period.
6. Stop the runtime immediately if a hard threshold is crossed.
7. At grace-period expiration, terminate the inference runtime and its entire process tree.
8. Verify that the process exited and GPU memory was released as far as telemetry can confirm.
9. Enter unavailable/cooldown state and record the full decision trail.

A force-preempted streaming response may end abruptly. The worker does not retry, resume, or reconstruct it.

The agent implementing this must propose initial threshold values based on the RX 9060 XT 16 GB and make every non-safety value easy to tune. Thresholds should not be silently hard-coded.

## 9. Reload, cooldown, and anti-thrashing

The worker may load the model only when:

- The active mode and schedule permit it.
- No classified game or strong competing workload remains.
- GPU telemetry is healthy.
- GPU temperature and other safety values are within limits.
- Required free VRAM is available.
- The cooldown and any anti-thrashing suppression have expired.

Default anti-thrashing policy:

- If two preemptions occur within a rolling 10-minute window, suppress reload for 30 minutes.
- A new competing event during cooldown resets the cooldown.
- Sleep/resume, GPU-driver recovery, runtime crash, and telemetry recovery apply a conservative configurable cooldown.
- The UI must show the exact time at which the next load attempt is allowed and why.

Model load time must be measured and recorded. Predictive idle-window logic is optional after MVP and must not delay the basic implementation.

## 10. Runtime management and proxy

Define a narrow internal runtime-adapter interface, but implement only a `llama.cpp` adapter in MVP.

The adapter must support:

- Validate executable and model configuration.
- Start the runtime with explicit arguments and a private loopback port.
- Determine readiness.
- Track the owned PID and descendants.
- Determine current request count through the worker proxy.
- Request graceful shutdown when supported.
- Terminate the complete process tree on timeout or emergency.
- Detect unexpected exit and retain bounded stdout/stderr diagnostics.

Only the worker's proxy listens on the configured LAN interface. `llama-server` listens on loopback and must not be directly reachable from other machines.

For compatible `/v1/*` inference paths, the proxy should pass requests and streaming responses through without interpreting prompt content. It must add useful response headers such as worker availability and request ID, enforce a configurable maximum request duration, and stop admitting requests atomically when yielding begins.

## 11. UI requirements

### Local web UI

The UI should make the service understandable at a glance:

- Available / Yielding / Unavailable.
- Active manual mode and policy profile.
- Model/runtime status.
- Whether a request is active and how long it has run.
- Aggregate and external GPU use, VRAM use/free, and temperature.
- Current competing application or trigger, when known.
- Grace, cooldown, or suppression countdown.
- The winning policy rule and supporting evidence.
- Recent lifecycle and preemption events.

Configuration pages:

- Runtime executable, arguments, model path, context size, and ports.
- Policy profiles and thresholds.
- Schedules and timezone.
- Application classifications and rules.
- Manual-mode defaults and duration choices.
- Retention, logging, metrics, and authentication/network settings.

### Tray UI

The tray UI should provide:

- Current condition and brief reason.
- `Auto`, `Pause AI`, and `AI Priority`.
- Common durations.
- Open dashboard.
- No requirement for the local user to understand models or infrastructure.

The Windows service must work without a logged-in desktop session. The tray process is optional presentation/control and communicates with the service.

## 12. API

Provide a versioned JSON API. Exact resource naming may change during implementation, but MVP must cover:

```text
GET  /api/v1/health
GET  /api/v1/status
GET  /api/v1/config
PUT  /api/v1/config
GET  /api/v1/events
GET  /api/v1/applications
PUT  /api/v1/applications
PUT  /api/v1/mode
POST /api/v1/drain
POST /api/v1/reload
GET  /metrics
```

`/status` must include:

- User-visible condition and internal state.
- Active profile, schedule, and manual override.
- Runtime/model readiness and active request count.
- Current GPU and external-demand telemetry.
- Current decision, winning rule, trigger evidence, and timestamps.
- Grace/cooldown/suppression expiration if relevant.
- Recent error summary.

Configuration writes must be schema-validated, reject unsafe or internally inconsistent values, persist atomically, and create an audit event. The service should apply safe changes without restart where practical and clearly mark changes that require runtime reload or service restart.

## 13. Security and networking

- Default the UI/API listener to localhost during initial setup.
- Allow an explicit LAN bind for later gateway access.
- Do not expose the private runtime port.
- Require authentication for non-loopback management operations.
- Permit a separate inference credential from the management credential.
- Never log prompts, generated content, bearer tokens, or model request bodies by default.
- Redact secrets in configuration reads, exports, diagnostics, and logs.
- Run under a dedicated least-privileged service identity where Windows GPU access permits it.
- Bind browser-facing state-changing endpoints to an anti-CSRF design when cookie-based authentication is used.
- Document Windows Firewall rules created by the installer.

For an initial trusted-LAN deployment, a generated API token is sufficient. Enterprise identity integration is out of scope.

## 14. Persistence, logs, and metrics

Persist:

- Versioned configuration.
- Application classification rules.
- Temporary/manual override expiration.
- Recent event/audit history.
- Basic rolling statistics such as model load duration, inference availability, and preemption counts.

Use a simple embedded store appropriate for one Windows machine; SQLite is preferred unless the implementation team identifies a concrete reason not to use it.

Event records should include timestamp, event type, before/after state, active profile, winning rule, relevant telemetry snapshot, affected runtime PID/request ID, and human-readable explanation.

Expose Prometheus-format metrics for lifecycle counts, readiness, current telemetry, model-load duration, active requests, request duration, graceful drains, forced preemptions, cooldowns, suppressions, runtime crashes, and telemetry failures.

Logs and database/event retention must be bounded and configurable.

## 15. Reliability and lifecycle behavior

- **Boot:** Start service, validate configuration, observe the machine, and apply startup cooldown before loading.
- **No logged-in user:** AI remains eligible under policy.
- **Login/unlock/user input:** Re-evaluate policy; do not necessarily unload solely because input occurred.
- **Lock/logout:** Re-evaluate but do not assume the GPU is idle until telemetry supports it.
- **Sleep:** Mark unavailable and stop or abandon the runtime cleanly before suspend when possible.
- **Resume:** Do not immediately load; verify device health and enter resume cooldown.
- **Shutdown/reboot:** Stop accepting work and terminate the runtime cleanly within Windows service deadlines.
- **Runtime crash:** Record diagnostics, re-evaluate eligibility, then use bounded exponential backoff. Avoid restart loops.
- **Worker restart:** Reconcile any orphaned runtime process safely before starting another.
- **GPU/telemetry failure:** Attempt to stop the runtime, enter error/unavailable state, and retry telemetry with backoff.
- **Driver reset/device removal:** Treat as a safety event and require stable telemetry plus cooldown before reload.
- **Configuration corruption:** Retain the last known-good configuration and provide a clear recovery path.

## 16. Technology guidance

The preferred implementation is a small Go service because it can produce a single Windows binary with low idle overhead and straightforward service/process management. This is guidance, not an absolute requirement; the implementation team may recommend a different stack if it materially improves reliable Windows GPU telemetry or tray/UI integration.

Expected components:

- Core Windows service and policy evaluator.
- Windows signal collectors.
- `llama.cpp` runtime adapter.
- Reverse proxy.
- Embedded SQLite persistence.
- HTTP API and Prometheus endpoint.
- Small web application served by the service.
- Separate lightweight tray application.
- Installer/uninstaller and Windows service registration.

Telemetry investigation should begin with Windows Performance Counters, documented Windows GPU APIs, and AMD-supported interfaces. The implementation must validate observed values on the target PC instead of assuming all per-process counters are reliable. If precise external-demand attribution is not available, degrade safely to multi-signal heuristics and report telemetry confidence.

## 17. Acceptance criteria

MVP is complete when all of the following are demonstrated on the target Windows/AMD machine:

1. The service starts at boot without an interactive login and loads the configured model when eligible.
2. The proxied OpenAI-compatible endpoint successfully completes both ordinary and streaming requests.
3. Starting a configured game makes the worker reject new inference immediately.
4. With low contention, an active inference turn is allowed to complete within the configured grace period and the model then unloads.
5. With simulated or real critical GPU/VRAM contention, the worker terminates the runtime before the grace period expires.
6. Forced preemption releases the owned runtime process tree and does not leave a second runtime or orphan behind.
7. The model reloads only after the workload disappears and the appropriate cooldown expires.
8. Normal and school-hours profiles produce measurably different grace/cooldown behavior using `America/Chicago` schedule evaluation.
9. Repeated preemption triggers anti-thrashing suppression and shows its reason/expiration in UI and API.
10. `Pause AI` and `AI Priority` work from the tray, web UI, and API; hard safety still overrides AI Priority.
11. The worker does not preempt itself because of `llama-server` GPU utilization.
12. A launcher left open does not independently cause permanent unavailability.
13. Sleep/resume, service restart, runtime crash, and invalid telemetry recover without a rapid restart loop.
14. Every lifecycle decision produces an event showing the winning rule and evidence.
15. Configuration survives reboot, validates atomically, and secrets are never returned or logged in plaintext.
16. The service and UI remain lightweight while idle and do not create noticeable input latency or meaningful CPU load.

## 18. Testing requirements

Include:

- Unit tests for schedule evaluation, precedence, grace periods, cooldown, hysteresis, suppression, and configuration validation.
- Table-driven policy tests that map telemetry/input facts to decisions.
- Runtime-adapter tests using a fake child process.
- Proxy tests for admission cutoff, streaming completion, forced connection loss, request limits, and secret-safe logging.
- Integration tests for service restart and orphan reconciliation.
- UI/API contract tests.
- Manual hardware validation script/checklist for the RX 9060 XT covering several games and at least one non-game GPU workload.
- A soak test that repeatedly loads, serves, drains, kills, cools down, and reloads without leaked processes or unbounded logs/database growth.

Record target-machine telemetry samples before selecting production defaults. Defaults must be documented as starting points, not universal truths.

## 19. Delivery phases

### Phase 0: Spike

- Confirm `llama.cpp` ROCm operation and reliable start/stop on the target PC.
- Evaluate available aggregate and per-process GPU/VRAM telemetry.
- Verify that process termination releases VRAM promptly.
- Capture sample telemetry for idle desktop, model loading, inference, launcher, game launch, active gaming, OBS/Discord/video, sleep/resume, and driver recovery if practical.
- Produce a brief decision note selecting telemetry sources and fallback behavior.

### Phase 1: Headless MVP

- Windows service, runtime ownership, proxy, basic telemetry, normal/school profiles, drain/preempt/cooldown, API, SQLite, logs, and metrics.
- Configuration can initially be managed through API plus a checked-in development config, but persistence and validation are required.

### Phase 2: Human controls

- Web UI, tray app, application-rule editor, schedule editor, manual modes, decision explanations, and installer.

### Phase 3: Hardening

- Hardware tuning, sleep/resume and driver-reset recovery, anti-thrashing, security review, soak testing, packaging, signed release artifacts, and documentation.

## 20. Repository and engineering expectations

- Use a conventional monorepo with clearly separated service, tray/UI, runtime adapter, telemetry, policy, persistence, API, and installer concerns.
- Include `README`, architecture decision records, example configuration, development setup, test instructions, troubleshooting guide, API schema, and release process.
- Use semantic versioning and CI for formatting, linting, tests, builds, dependency/security scans, and Windows release artifacts.
- Do not hide material design decisions in code. Record telemetry-source selection, authentication design, process-tree management, and installer/service identity choices as ADRs.
- Avoid premature abstractions beyond the single managed runtime, but preserve the narrow runtime-adapter seam.

## 21. Questions the implementation team must resolve during Phase 0

These are engineering investigations, not blockers requiring product redesign:

1. Which Windows/AMD data sources give sufficiently reliable per-process GPU engine and VRAM attribution on the actual RX 9060 XT driver?
2. What fallback decision model is safest when per-process attribution is missing or contradictory?
3. Can the service identity access the required GPU telemetry and ROCm runtime without granting excessive privileges?
4. What stop sequence most reliably releases VRAM: graceful runtime shutdown, console control event, job-object termination, or a combination?
5. Which initial thresholds avoid visible gaming impact while preserving useful inference availability?
6. Should the tray use a native toolkit or a minimal cross-platform shell, given packaging and idle-memory goals?

The team should recommend answers with evidence from the target system, then proceed unless a finding materially changes scope or user experience.

## 22. Suggested delegation plan

### Lead / architect

- Own Phase 0 and the architecture skeleton.
- Resolve telemetry sources, service identity, process ownership, API contract, persistence, and ADRs.
- Integrate all work and guard scope.

### Backend / Windows systems agent

- Build telemetry collectors, runtime adapter, Windows service lifecycle, job-object/process-tree management, and sleep/resume handling.
- Create target-hardware validation tooling.

### Policy / API agent

- Build the policy evaluator, schedules, precedence, grace/cooldown/suppression logic, persistence, event explanations, API, metrics, and comprehensive table-driven tests.

### UI / packaging agent

- Build the web dashboard, tray controls, configuration experience, installer/uninstaller, and Windows release packaging after API contracts stabilize.

Agents may work in parallel after the lead commits the schemas and contracts. One agent should retain integration ownership; avoid having several agents independently redefine the state model or API.
