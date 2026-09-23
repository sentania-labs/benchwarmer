# ADR 0003: GPU telemetry sources and fallback

Status: Provisional (2026-09-23), pending Phase 0 E4-E8.

## Context

Policy needs aggregate GPU use, external (non-Benchwarmer) GPU use, VRAM
used/free, and temperature. Windows exposes per-process GPU data through the
WDDM performance counters on every driver, but accuracy is driver-dependent.
AMD's ADLX SDK is C++-only and would force cgo or a sidecar.

## Decision

Sources, in order of use:

| Fact | Primary source | Fallback |
|---|---|---|
| Per-process engine utilization | PDH `\GPU Engine(*)\Utilization Percentage` | none (see below) |
| Per-process dedicated memory | PDH `\GPU Process Memory(*)\Dedicated Usage` | none |
| Adapter dedicated memory in use | PDH `\GPU Adapter Memory(*)\Dedicated Usage` | DXGI budget |
| Total VRAM, adapter identity | DXGI `IDXGIAdapter1::GetDesc1` | config |
| Temperature, power, fan | D3DKMT `KMTQAITYPE_ADAPTERPERFDATA` (what Task Manager uses) | none: temperature rule reports unknown |

External demand = per engine, sum of non-owned processes; adapter figure = the
busiest engine (Task Manager's method). Owned = the Job Object's PID list.
External VRAM = max(sum of non-owned process usage, adapter usage minus owned
usage).

**Confidence.** Every snapshot carries `telemetry_confidence`:

- `high`: per-process data present, owned processes visible in it, and the
  per-process sum is consistent with the adapter total.
- `degraded`: adapter totals only, or per-process data inconsistent.
- `none`: collection failing.

**Fallback model when confidence is degraded** (conservative multi-signal):

1. External VRAM is inferred as adapter usage minus the runtime's recorded
   post-load footprint (the footprint is stable once loaded).
2. External utilization is trusted only while the runtime is idle (no active
   request, not loading); while busy, utilization cannot be split, so policy
   relies on VRAM growth and process/foreground signals.
3. A classified game process running counts as confirmed gaming without
   needing GPU corroboration.
4. Soft contention thresholds are tightened by `degraded_margin_pct`.

**When confidence is none** for longer than `telemetry.loss_grace`, the worker
yields (spec principle 6) and retries collection with backoff.

## Consequences

- No AMD SDK dependency. If E6 shows no temperature through D3DKMT, add an
  ADLX sidecar as a separate decision.
- GPU Engine counters can be expensive to collect; E8 measures it and sets the
  default sample interval.
