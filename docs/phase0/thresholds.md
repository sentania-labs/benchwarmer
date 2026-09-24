# Provisional thresholds

**Status: provisional.** These are starting values reasoned from the RX 9060 XT
16 GB's capacity and typical desktop and game behavior, not yet from target
measurements. Each maps to a config field (see `configs/example.json`); the
runbook scenarios that will confirm or replace each one are listed.

The dominant fact: a 20B-class model at MXFP4 with an 8K context occupies
roughly 12-13 GiB of the card's 16 GiB. No modern game coexists with that, so
the design goal is not coexistence but fast, clean handoff.

## Hard (safety) thresholds: immediate termination, bypass grace

| Config field | Value | Reasoning | Confirmed by |
|---|---|---|---|
| `safety.gpu_temp_critical_c` | 90 | Edge temperature for RDNA4 under sustained load typically sits 60-80 C; 90 leaves headroom before driver throttling | runtime-inference, game-active |
| `safety.gpu_temp_resume_c` | 80 | Hysteresis: reload blocked until the card cools this far | same |
| `contention.vram_free_critical_mib` | 512 | Below this, a game allocating textures will page to system memory and stutter visibly | game-plus-runtime |
| `contention.external_vram_critical_mib` | 3072 | External dedicated use this high while the model is loaded means a real 3D workload is fighting for memory | game-plus-runtime, game-active |
| `contention.external_util_critical_pct` over `contention.critical_window` | 60% over 3 s | A game rendering at 60%+ of the busiest engine is not a transient | game-active, browser-video (must stay below) |

## Soft thresholds: stop admitting, drain within grace

| Config field | Value | Reasoning | Confirmed by |
|---|---|---|---|
| `contention.external_util_soft_pct` over `contention.soft_window` | 25% over 10 s | Desktop compositing and video decode should stay below this | idle-desktop, browser-video, obs-or-discord |
| `contention.external_vram_soft_mib` | 2048 | Desktop plus browser is typically 0.5-1.5 GiB | idle-desktop, browser-video, launcher-idle |
| `contention.clear_window` | 30 s | Signals must stay below soft thresholds this long before the contention counts as gone | game-launch (loading screens dip) |

## Load eligibility

| Config field | Value | Reasoning |
|---|---|---|
| `runtime.required_free_vram_mib` | 13312 | Model plus KV cache plus margin; replaced by measured footprint + 10% after first load in runtime-* |
| `runtime.load_timeout` | 180 s | Cold load from NVMe of a 12 GiB model is expected well under a minute; generous for first-run shader compilation |

## Timing (profiles)

| Config field | Normal | School hours | Source |
|---|---|---|---|
| `grace` | 15 s | 60 s | Spec section 6 |
| `cooldown` | 5 min | 2 min | Spec section 6 |
| `ambiguous_signal_tolerance` | strict | tolerant | Spec section 6 |

## Anti-thrashing and recovery

| Config field | Value | Source |
|---|---|---|
| `antithrash.window` / `antithrash.max_preemptions` / `antithrash.suppress_for` | 10 min / 2 / 30 min | Spec section 9 |
| `recovery.resume_cooldown` | 3 min | Conservative: driver re-initialisation after resume |
| `recovery.crash_backoff_initial` / `max` | 30 s / 30 min, doubling | No restart loops |
| `recovery.telemetry_failure_cooldown` | 2 min after telemetry is stable again | Fail conservatively |
| `recovery.startup_cooldown` | 2 min | Spec section 15 (boot) |

## Process drain

| Config field | Value | Reasoning |
|---|---|---|
| `runtime.kill_verify_timeout` | 10 s | Job termination should complete in well under 1 s; anything longer is an error event |
| `runtime.vram_release_timeout` | 15 s | Replaced by measured kill-to-release p95 x 3 from runtime-* |
| `runtime.vram_release_tolerance_mib` | 256 | Driver bookkeeping jitter |
