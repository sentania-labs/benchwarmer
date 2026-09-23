# Phase 0 findings

**Status: target-machine measurements pending.** The development workstation
has no access to the target PC; the measurements are collected with
[runbook.md](runbook.md). This file records what is already established
(from Windows CI and design review) and holds the slots the target results
fill.

## Established without the target PC

| Question | Finding | Evidence |
|---|---|---|
| Can a Job Object own the runtime tree and kill it as a unit? | Yes: created suspended, assigned, resumed; `TerminateJobObject` kills root and descendants | `internal/procgroup` tests on the CI Windows runner |
| Does the tree die if the owner is killed abruptly? | Yes, via `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` | `TestOwnerDeathKillsTree` on the CI Windows runner |
| Does the probe's start/ready/serve/kill loop work? | Yes against `fakellama` on Linux, including mid-stream kill with the client seeing a truncated stream | `bwprobe runtime` with `cmd/fakellama` |
| Is the counter parsing and own/external split correct? | Yes for synthetic counter sets, including JSON round-trip | `internal/telemetry` tests |

## Pending (target PC)

| # | Question | Result | Decision it drives |
|---|---|---|---|
| E1 | HIP vs Vulkan build works on gfx1200 | pending | Default runtime build in docs |
| E2 | GPU available to llama-server from session 0 (virtual account / LocalSystem) | pending | ADR 0006 service identity |
| E3 | Kill-to-VRAM-release time | pending | `runtime.vram_release_timeout` |
| E4 | Per-process counters present and consistent with adapter totals | pending | ADR 0003 attribution confidence |
| E5 | Engine type used by llama-server | pending | Own/external separation by engine class |
| E6 | Temperature via D3DKMT | pending | Safety source; fallback if absent |
| E7 | Service account can read counters and user process paths | pending | ADR 0006; path rules vs name rules |
| E8 | Scenario profiles | pending | thresholds.md |
| E9 | Sleep/resume and driver reset behavior | pending | Recovery rules |
| - | PDH collection cost per sample | pending | Sampling interval default |
