# Phase 0 findings

**Status: target-machine measurements in progress.** Remote access to SS8510
(the target PC) was provisioned on 2026-09-23 over WinRM. Measurements follow
[runbook.md](runbook.md). This file records what is established and holds the
slots the target results fill.

## Target machine

SS8510: Windows 11 Pro 10.0.26200, Ryzen 7 9700X, 31 GB RAM, AMD Radeon RX
9060 XT 16 GB (driver 32.0.31044.16) plus the Ryzen iGPU (driver
32.0.21043.5001). Domain-joined; managed by lab GPOs.

## Blocker found: Defender ASR blocks unsigned, low-prevalence executables

The lab GPO sets Defender attack surface reduction rule
`01443614-cd74-433a-b99e-2ecdc07bfc25` ("Block executable files from running
unless they meet a prevalence, age, or trusted list criterion") to Block.
It blocked `bwprobe.exe` on first run (Defender event 1121, 2026-09-23
6:49 PM). It applies to Benchwarmer's own binaries and to the llama.cpp
runtime regardless of which process launches them.

Resolution requested from the GPO owner: an ASR-only path exclusion for
`C:\Program Files\Benchwarmer\`, which only Administrators and SYSTEM can
write, so the exclusion does not open a user-writable bypass. Benchwarmer
installs its binaries and the llama.cpp runtime under that path; models live
in `C:\ProgramData\Benchwarmer\models\` (data, not executables). Code
signing alone would not cover the third-party runtime. The installer must
document this requirement (ADR 0009, installer strategy).

Also enabled on SS8510 and checked: Controlled Folder Access (does not cover
ProgramData), ASR rule `d1e49aac-...` for PsExec/WMI-launched processes (did
not fire for WinRM). Smart App Control is off; AppLocker has no rules.

## Established without the target PC

| Question | Finding | Evidence |
|---|---|---|
| Can a Job Object own the runtime tree and kill it as a unit? | Yes: created suspended, assigned, resumed; `TerminateJobObject` kills root and descendants | `internal/procgroup` tests, CI run 35935453119 (Windows job) |
| Does the tree die if the owner is killed abruptly? | Yes, via `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` | `TestOwnerDeathKillsTree`, same run |
| Does the probe's start/ready/serve/stop loop work on Windows? | Yes against `fakellama` (with a spawned child): tree empty 2 ms after kill, no leftovers, mid-stream kill seen by the client as a truncated stream | Same run, step "Probe runtime cycles" |
| Is the counter parsing and own/external split correct? | Yes for synthetic counter sets, including JSON round-trip and missing-adapter detection | `internal/telemetry` tests |

Not yet exercised anywhere: PDH array parsing against real GPU counter
instances, and D3DKMT perf data (the CI runner has no hardware GPU). Those
come from the target PC.

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
