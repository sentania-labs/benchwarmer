# Phase 0 findings

**Status: target measurements largely complete (2026-09-24); scenario profiles pending.** Remote access to the target
PC was provisioned on 2026-09-23 (Windows remote management). Measurements follow
[runbook.md](runbook.md). This file records what is established and holds the
slots the target results fill.

## Target machine

Windows 11 Pro 10.0.26200, Ryzen 7 9700X, 31 GB RAM, AMD Radeon RX
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

Also enabled on the target and checked: Controlled Folder Access (does not cover
ProgramData), ASR rule `d1e49aac-...` for PsExec/WMI-launched processes (did
not fire for remote management). Smart App Control is off; AppLocker has no rules.

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

## Target measurements (2026-09-24)

| # | Question | Result | Decision |
|---|---|---|---|
| E1 | Which llama.cpp build works on gfx1200 | **Vulkan** (b11149): all 25 layers on the RX 9060 XT, ~93 tokens/s, first token ~40 ms, load ~5 s (6.7 s cold). **ROCm** build: `--list-devices` shows none; it silently runs on the CPU at ~23 tokens/s | Vulkan is the default build. A loaded runtime with ~0 own VRAM should count as a failed load (CPU inference would load all cores during games) |
| E2 | GPU from session 0 | Works under LocalSystem, a virtual account, and LocalService (restricted token, Medium integrity) | ADR 0006 |
| E3 | Stop-to-VRAM-release | Hard kill: tree empty ~1.4-1.5 s, VRAM back ~1.0-1.4 s. Graceful Ctrl+C: exit 0.8 s, VRAM 0.8 s | `vram_release_timeout` 15 s is ample |
| E4 | Per-process counters | Present and attributable (21 processes, 8 engines at idle); a collection costs ~4 ms. A new engine instance once reported 3.7e14 %: impossible readings are now dropped | ADR 0003: PDH primary |
| E5 | Engines used by llama-server | Compute and Copy only; games use 3D | Own vs game separation by engine is clean |
| E6 | Temperature | Available via D3DKMT adapter perf data (31 C idle, ~62 C under sustained inference) | Safety source confirmed |
| E7 | Service identity visibility | Virtual account: GPU counters denied, user-session processes not visible. LocalSystem: full | ADR 0006: service as LocalSystem, runtime as restricted LocalService |
| - | Model footprint | gpt-oss-20b MXFP4, 8K context: 11,327 MiB owned (10,949 weights + 216 KV + 96 compute) | `required_free_vram_mib` ~12,500 |
| - | Stability | Four blue screens `0x116` in `amdkmdag.sys` with the display off, matching a known AMD idle/D3 VRAM eviction defect; hard kills of long-running runtimes triggered it, a graceful stop did not | ADR 0011 |

## Still pending

| # | Question | Needs |
|---|---|---|
| E8 | Game, launcher, browser, and streaming scenario profiles | A normal evening of use with passive recording |
| E9 | Sleep/resume | The target is set never to sleep; driver resets are covered by ADR 0011 |
