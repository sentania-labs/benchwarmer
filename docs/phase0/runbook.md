# Phase 0 runbook: measuring the target PC

This produces the evidence Benchwarmer's telemetry choice and thresholds are
based on. Total hands-on time is about 90 minutes, most of it playing or
idling while the probe records.

## What you need

- The gaming PC (Windows 11, RX 9060 XT 16 GB), with an administrator account.
- `bwprobe.exe` from the CI build artifact `bwprobe-windows-amd64` (or
  `make windows` on the dev box).
- Two llama.cpp Windows builds from the llama.cpp GitHub releases page:
  the **HIP/ROCm** build (`llama-*-bin-win-hip-*.zip`) and the **Vulkan**
  build (`llama-*-bin-win-vulkan-x64.zip`). Unzip to
  `C:\Benchwarmer\llama-hip\` and `C:\Benchwarmer\llama-vulkan\`.
- The intended model, e.g. gpt-oss-20b MXFP4 GGUF, at
  `C:\Benchwarmer\models\`. Paths outside the user profile matter: the
  service-identity test runs as an account that cannot read `C:\Users\...`.
- Put `bwprobe.exe` in `C:\Benchwarmer\` and open a terminal there.

Every step writes a `.jsonl` file into the current directory. Zip the whole
folder at the end and bring it back.

## 1. Environment report (1 minute)

```powershell
.\bwprobe.exe env -out env-user.jsonl
```

Look at the output for: the RX 9060 XT adapter and its LUID, whether
`perf.temperature_c` is non-zero (E6), and `gpu_sample.process_instances`
greater than zero (E4). If the wrong adapter was chosen, pass
`-adapter 9060` to later commands.

## 2. Runtime start/stop and VRAM release (10 minutes per build)

```powershell
.\bwprobe.exe runtime -exe C:\Benchwarmer\llama-vulkan\llama-server.exe -model C:\Benchwarmer\models\<model>.gguf -cycles 4 -out runtime-vulkan.jsonl
.\bwprobe.exe runtime -exe C:\Benchwarmer\llama-hip\llama-server.exe    -model C:\Benchwarmer\models\<model>.gguf -cycles 4 -out runtime-hip.jsonl
```

Each cycle loads the model inside a Job Object, runs a normal and a streaming
completion, then kills the process tree: odd cycles while idle, even cycles
one second into a streaming response. It measures kill-to-exit, kill-to-VRAM
released, and checks for leftover `llama-server.exe` processes. Answers E1,
E3, E5. Per-cycle runtime logs land in `runtime-cycleNN.log`.

If a build never becomes ready, the summary shows its stderr tail. Try
`-args "-ngl 999 -c 4096"` before concluding the build does not work.

## 3. Service identity (5 minutes, elevated terminal)

Run from an **Administrator** terminal, using whichever build worked in step 2:

```powershell
.\bwprobe.exe svctest -account virtual -exe C:\Benchwarmer\llama-vulkan\llama-server.exe -model C:\Benchwarmer\models\<model>.gguf
.\bwprobe.exe svctest -account virtual -perfmon-group -exe C:\Benchwarmer\llama-vulkan\llama-server.exe -model C:\Benchwarmer\models\<model>.gguf
.\bwprobe.exe svctest -account system -exe C:\Benchwarmer\llama-vulkan\llama-server.exe -model C:\Benchwarmer\models\<model>.gguf
```

Each registers a temporary service named `BenchwarmerProbe`, runs the
environment report and two runtime cycles from session 0 under that identity,
then deletes the service and removes the folder grants it added. Results land
in `C:\ProgramData\BenchwarmerProbe\`; copy that folder into the bundle.
Answers E2 and E7: does the GPU work from a service, can a least-privileged
account read GPU counters and other users' process paths.

## 4. Scenario recordings (about 60 minutes)

Each command records one scenario. Leave it running for the stated time. While
it runs you can type a note and press Enter to drop a timestamped marker
("game menu", "match started", "alt-tabbed out"). Ctrl+C ends early.

Start the runtime in a second terminal first for scenarios marked *(runtime
loaded)*:

```powershell
C:\Benchwarmer\llama-vulkan\llama-server.exe -m C:\Benchwarmer\models\<model>.gguf -ngl 999 -c 8192 --port 18081
```

| # | Label | Duration | What to do |
|---|---|---|---|
| a | `idle-desktop` | 3 min | Nothing running but the desktop. Do not touch the mouse. |
| b | `runtime-idle` | 3 min | *(runtime loaded)* No requests. |
| c | `runtime-loading` | 2 min | Start recording, then start llama-server; stop after it is ready. |
| d | `runtime-inference` | 3 min | *(runtime loaded)* In a third terminal run `.\bwprobe.exe load -duration 3m`. |
| e | `launcher-idle` | 3 min | Steam (and Epic, if installed) open at the library page, no game. |
| f | `game-launch` | 3 min | Mark, then launch a game from the launcher; mark again at the main menu. |
| g | `game-active` | 10 min | Actually play. Two different games if possible, one label each (`game-active-<name>`). |
| h | `game-plus-runtime` | 3 min | *(runtime loaded, idle)* Play the game. Shows VRAM contention directly. |
| i | `game-plus-inference` | 2 min | *(runtime loaded)* Play while `bwprobe load` runs. Note stutter, if any, with a marker. |
| j | `browser-video` | 3 min | 1080p or 4K YouTube in the browser, fullscreen part of the time. |
| k | `obs-or-discord` | 3 min | OBS recording, or Discord screen share, if either is used on this PC. |
| l | `sleep-resume` | 5 min | Start recording, put the PC to sleep, wake it after a minute. |

Command for each, with the label from the table:

```powershell
.\bwprobe.exe telemetry -label idle-desktop -duration 3m
```

A driver reset (E9) is optional. If you want it: with recording running,
press **Win+Ctrl+Shift+B** (resets the display driver stack, screen flashes)
and label the run `driver-reset`.

## 5. Bring it back

```powershell
Compress-Archive -Path C:\Benchwarmer\*.jsonl, C:\Benchwarmer\*.log, C:\ProgramData\BenchwarmerProbe\*.jsonl -DestinationPath C:\Benchwarmer\phase0-results.zip
```

On the dev box:

```sh
bwprobe summarize *.jsonl > docs/phase0/findings-raw.md
```

The summary feeds `docs/phase0/findings.md` and the threshold table.

## Privacy

The probe records process names, paths, foreground window process names,
input idle time, and GPU counters. It does not record window titles,
keystrokes, prompts, or model output. The test prompt is fixed and is not
written to the output.
