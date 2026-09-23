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
  - **agent**: session agent reports only; readable by interactive users.
- Loopback management requests are allowed without a token only for read-only
  endpoints and only when `security.loopback_trust` is true (default true
  during setup). Writes always require the management token, so a
  malicious web page cannot drive the API through the user's browser (no
  cookies are used, so CSRF does not apply; the web UI keeps the token in
  memory after the user pastes it once, or is served a short-lived token by
  the tray).
- Tokens are compared in constant time, never logged, and redacted from
  config reads as `"<redacted>"`.
- Config holds token *file references*, not values.

## Consequences

- No cookie auth, so no CSRF machinery. If cookie auth is ever added, it must
  come with CSRF protection per spec section 13.
