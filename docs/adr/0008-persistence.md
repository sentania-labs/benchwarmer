# ADR 0008: Persistence

Status: Accepted (2026-09-23)

## Context

Persist versioned config, application rules, manual override expiry, recent
events, and rolling statistics, on one machine, with bounded growth and a
recovery path from corruption.

## Decision

- SQLite (`modernc.org/sqlite`) in WAL mode at
  `%ProgramData%\Benchwarmer\benchwarmer.db` for events, statistics, and
  override state.
- The active config is **also** written as `config.json` beside the database
  using write-temp, fsync, rename. The last validated config is kept as
  `config.last-good.json`. On startup: load `config.json`; if it fails
  validation, load last-good, emit a `config_recovered` event, and surface it
  in status. The database is never the only copy of the config, so a corrupt
  database cannot leave the service unconfigurable.
- Saved configuration versions (the last 20 saves, with actor, time, and a
  redacted diff) are kept in `config.json.history` beside the config, written
  with the same atomic replace and mode 0600. A save first records a pending
  entry, then replaces `config.json`; after a crash the entry counts only if
  `config.json` matches it (`internal/config/history.go`). Restore creates a
  new version and fails with a conflict if `config.json` changed since the
  preview.
- Retention: events pruned by age (`retention.events_days`) and count
  (`retention.events_max`), checked hourly. Stats are aggregated to hourly
  rows.

## Consequences

- Config is JSON (no extra parser dependency, same format as the API), human-readable and diffable; the API writes it, and an operator
  can recover by hand.
