# ADR 0001: Implementation stack

Status: Accepted (2026-09-23)

## Context

The spec prefers a small Go service. The service must run as a Windows
service, own a child process tree, call Win32 GPU and session APIs, serve HTTP,
and stay light at idle. The development workstation is Linux.

## Decision

- Go for the service, the probe, and the tray/session agent.
- Win32 access through `golang.org/x/sys/windows` and direct DLL calls; no cgo.
- SQLite through `modernc.org/sqlite` (pure Go), so Windows binaries
  cross-compile from Linux and CI without a C toolchain.
- Web UI as static HTML/CSS/JS embedded in the service binary; no Node build
  step.
- Development and most tests run on Linux with fakes; Windows-only code is
  exercised on a GitHub-hosted Windows runner (the repo is public, so it costs
  nothing).

## Consequences

- One `benchwarmer.exe` plus one `bwtray.exe`; no runtime dependencies.
- Pure-Go SQLite is slower than the C build; irrelevant at this data volume.
- COM calls (DXGI) are hand-written vtable calls; kept to a minimum.
