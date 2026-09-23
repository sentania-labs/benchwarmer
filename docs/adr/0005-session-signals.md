# ADR 0005: Interactive-session signals

Status: Accepted (2026-09-23)

## Context

A Windows service runs in session 0 and cannot observe the logged-in user's
foreground window, fullscreen state, or input idle time. The spec lists those
signals and also requires the service to work with nobody logged in.

## Decision

- The tray binary (`bwtray.exe`), started at user logon, doubles as the
  **session agent**. Every few seconds it posts foreground process, fullscreen
  flag, notification state, and idle seconds to the service's management API
  over loopback, authenticated with a per-machine agent token readable only by
  interactive users and administrators.
- The service treats these facts as optional. If no agent report is fresher
  than `signals.session_stale_after`, they are `unknown`.
- Primary detection does not depend on them: process list and GPU telemetry
  are visible from session 0.
- The service does not spawn processes into user sessions, which would
  require LocalSystem (SeTcbPrivilege) and a larger attack surface.

## Consequences

- If the user quits the tray, fullscreen and idle signals are lost; detection
  still works through processes and GPU contention. The UI shows the agent as
  disconnected.
- A malicious local user could feed false session facts. The worst outcome is
  inference yielding or loading under ordinary policy; hard contention rules
  use GPU telemetry, which the agent cannot influence.
