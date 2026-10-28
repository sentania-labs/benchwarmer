# Troubleshooting

Start with the dashboard (`http://127.0.0.1:8481/`): the condition, the
winning rule, its evidence, and "next load allowed at ... because ..." answer
most questions. The Events page shows every decision in order.

| Symptom | Where to look | Usual cause |
|---|---|---|
| Unavailable, rule `eligibility.telemetry_unavailable` or `safety.telemetry_lost` | Status GPU card, `telemetry_lost` events | GPU counters unreadable (service identity, driver reset). See ADR 0006. |
| `telemetry_degraded` event, "Runtime GPU attribution incomplete" | The event message and `data` (`reasons`, `missing_gpu_pids`, `sample_errors`, `duration_ms`) | Per-process GPU counters did not cover the runtime. One event per change in the cause, then a `recovered` event with the duration. When only the job membership list lacked the runtime root and counters stayed `high` confidence (`identity_only`), it is logged at DEBUG. See ADR 0003. |
| Unavailable, rule `eligibility.vram_insufficient` | Evidence `vram_free_mib` | Another process holds VRAM, or `runtime.required_free_vram_mib` is too high for the model. |
| Unavailable, rule `eligibility.suppressed` | Timers card | Two preemptions within `anti_thrash.window`; waits `anti_thrash.suppress_for`. |
| ERROR with `eligibility.recovery` "crash backoff" | `runtime_crashed` / `load_failed` events, `diagnostics_tail` | Bad model path or args, driver problem. Backoff doubles up to `recovery.crash_backoff_max`. |
| ERROR with `safety.runtime_unverified` | `kill_failed` events | A runtime process could not be confirmed terminated; the service retries and will not start another meanwhile. |
| Runtime still loaded when the PC slept, or `kill_failed` after wake | `power_suspend_stop` event (`stopped`, `late`, `elapsed_ms`, `budget_ms`), then `runtime_stopped` / `kill_failed` | The stop did not finish inside the 1.5 s suspend budget (`late` means it finished only after the budget); it continues and is verified after resume. See ADR 0013. |
| Runtime never starts, "Access is denied" | Defender event 1121 | ASR prevalence rule; see [policy requirements](deploy/policy-requirements.md). |
| Yields while only a launcher is open | Evidence `process` and `class` | A launcher helper is unclassified; add it as `launcher` under Configuration > Applications. |
| A game is not detected | Status top external processes | Add an application rule (`game`) by exe name or path. |
| Session agent "not connected" | Tray running? | `bwtray.exe` starts at logon from the Run key; it needs read access to `secrets\agent.token`. |
| Config changes ignored after restart | `config_recovered` event, banner | `config.json` failed validation; the service runs on `config.last-good.json`. Fix the file or save from the UI. |

Logs: `C:\ProgramData\Benchwarmer\logs\benchwarmer.log` (rotated, size
bounded by `retention.log_max_mb` x `retention.log_files`). Logs never contain
prompts, request bodies, or tokens.

Development without a GPU: `benchwarmer run --data ./data --simulate-gpu
--sim-control sim.json`; write `{"external_vram_mib": 4000}` to `sim.json` to
simulate a competing workload.
