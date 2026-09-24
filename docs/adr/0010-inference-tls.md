# ADR 0010: HTTPS on the inference listener

Status: Accepted (2026-09-24)

## Context

The inference proxy is the only listener meant for other machines. Its
bearer token and the prompts it carries must not cross the network in the
clear. The lab issues certificates from a Microsoft (AD CS) CA; the target is
domain-joined, so clients already trust that CA.

## Decision

- The inference listener serves HTTPS (TLS 1.2+) when
  `listen.inference_tls.enabled`; validation rejects a non-loopback
  inference listener without it.
- Certificates come from files: a PFX (the natural AD CS export, including
  the modern AES-encrypted format Windows produces) or PEM files. Paths may
  be relative to the data directory. *Amended 2026-09-24: PFX files are no
  longer read. The Windows certificate store (issue #3) is the deployment
  path, a PFX is imported there, and dropping PFX parsing removed the only
  use of `golang.org/x/crypto`. PEM files remain.*
- Files are re-checked every 30 seconds and swapped live on change; a bad
  renewal keeps the previous certificate and records an event. Expiry is
  warned 21 days ahead.
- A self-signed certificate can be generated for testing only; the log and
  events label it as such.
- The management listener stays loopback HTTP.

## Consequences

- Renewal is a file replacement, not a restart.
- Reading directly from the Windows certificate store (so AD CS
  autoenrollment renews with no export step) is not implemented; it needs
  CNG signing for non-exportable keys. Tracked as a follow-up issue.
