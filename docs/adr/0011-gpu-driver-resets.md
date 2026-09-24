# ADR 0011: GPU driver resets, graceful stop, and the AMD idle/D3 driver bug

Status: Accepted (2026-09-24), from target evidence.

## Context

On the target PC (RX 9060 XT, AMD driver 32.0.31044.16, dated 2026-09-08) four
blue screens occurred on 2026-09-24, all `0x116 VIDEO_TDR_FAILURE` in
`amdkmdag.sys`. Each was two GPU engine timeouts (live dump `0x141`) about 30-60
seconds apart; Windows recovered the first and bugchecked on the second. All
four happened with the display off and nobody using the PC:

| First hang | What had just happened |
|---|---|
| 05:36 | Probe hard-killed a runtime mid-stream (ROCm build, CPU inference, HIP runtime loaded) |
| 09:21 | Inference through the installed service; no kill recorded (event buffer may have been lost) |
| 10:07 | Service hard-killed a runtime that had served for 16 minutes (254 requests); dump 3 s after the kill |
| 10:33 | Service restarted while paused; no runtime running |

About 20 other stops of short-lived runtimes were clean. The same 15-minute
soak (473 requests) stopped with Ctrl+C exited in 0.8 s with no hang. There
were no GPU hangs in the 30 days before testing.

The pattern matches a reported AMD driver defect
([llama.cpp discussion #23443](https://github.com/ggml-org/llama.cpp/discussions/23443)):
in Adrenalin 26.5.1 and later, when the GPU enters D3 at idle or display
power-off, the driver does not declare which memory segments stay active, so
Windows evicts VRAM, followed by two TDRs and `0x116`. Reported workarounds:
roll back to 32.0.22042.14002 (Adrenalin 26.3.1); a fix was expected in
26.9.2. Registry and PCIe power tweaks were reported ineffective.

## Decision

1. **Detect driver resets in real time.** Windows writes a watchdog live dump
   at the first hang, before the fatal second one. The service watches
   `%SystemRoot%\LiveKernelReports` for new watchdog dumps; on one it
   preempts immediately (no grace), emits `device_lost`, and waits
   `recovery.device_lost_cooldown` (default 30 min), doubling per reset until
   a stable run.
2. **Stop the runtime gracefully.** `runtime.stop_mode = graceful` (default):
   Ctrl+C through a console the service allocates for itself, hard kill only
   after `runtime.graceful_stop_timeout`; a fallback kill raises an event.
   This reverses ADR 0002 decision 4.
3. **Driver version is a deployment prerequisite.** Run Benchwarmer only on an
   AMD driver without the idle/D3 eviction defect. Detection and graceful stop
   reduce the risk; they do not remove it, because the defect can trigger when
   anything touches the idle GPU.

## Consequences

- A hang that starts inside the driver during teardown cannot be interrupted
  from user space; detection alone did not prevent the 10:07 blue screen.
- The watchdog dump throttle must be off to capture full dumps for diagnosis
  (`HKLM\SYSTEM\CurrentControlSet\Control\CrashControl\FullLiveKernelReports`,
  `SystemThrottleThreshold` and `ComponentThrottleThreshold` = 0).
