# ADR 0007: Authentication

Status: Accepted (2026-09-23)

## Context

The spec asks for localhost-by-default management, authentication for
non-loopback management, a separate inference credential, and a generated
token being sufficient for a trusted LAN.

## Decision

- Three independent bearer tokens, generated at install, stored in
  `%ProgramData%\Benchwarmer\secrets\` with an ACL limited to the service
  account and Administrators:
  - **management**: `/api/v1/*` state-changing calls and config reads.
  - **inference**: `/v1/*` through the proxy. Optional on loopback.
  - **agent**: held by the tray / session agent; readable by interactive
    users. Allowed for session reports, reading status and health, and
    changing the manual mode (Auto, Pause AI, AI Priority), because the
    local user must be able to pause or prioritise from the tray without
    administrator rights. Not allowed for configuration, events, drain,
    reload, or metrics.
- Loopback management requests are allowed without a token only for read-only
  endpoints and only when `security.loopback_trust` is true (default true
  during setup). Writes always require the management token, so a
  malicious web page cannot drive the API through the user's browser (no
  cookies are used, so CSRF does not apply; the web UI keeps the token in
  memory after the user pastes it once, or is served a short-lived token by
  the tray).
- **Amendment (issue 30): a tokenless Pause AI.** The person at the PC can
  pause the GPU, or return to Auto, from the web UI without a token. This is
  the only tokenless write and is scoped to `PUT /api/v1/mode` with `pause` or
  `auto`; AI Priority, config, applications, drain, reload, restart and every
  remote request still need a token. It requires `security.loopback_trust`, a
  loopback TCP peer, and a loopback `Host`, plus a cross-site barrier, because
  those first two checks do not stop a hostile page from sending a request to
  `http://localhost:<port>` itself: the request must be `application/json`
  (which a page cannot send cross-origin without a CORS preflight, and no
  preflight is ever granted), `Origin` must be exactly this listener's origin
  when present (`null` is refused), and `Sec-Fetch-*` must say same-origin.
  With neither `Origin` nor `Sec-Fetch-*` (a native client) the request is
  allowed on the remaining checks. The barrier keeps other web pages out; it
  is not authentication. Local processes and users are trusted by design and
  can forge any header, as they can already read the agent token. The change is
  recorded as `set_by: local` with `principal: local` and obeys the same
  duration limits and safety overrides as any other mode change.
- Tokens are compared in constant time, never logged, and redacted from
  config reads as `"<redacted>"`.
- Config holds token *file references*, not values.

## Consequences

- Any interactive user can switch modes through the tray. That matches the
  product intent (the human at the PC owns the GPU); hard safety and
  critical-contention rules still apply in every mode.

- No cookie auth, so no CSRF machinery. If cookie auth is ever added, it must
  come with CSRF protection per spec section 13.
