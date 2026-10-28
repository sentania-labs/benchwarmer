# ADR 0009: Installer and packaging

Status: Accepted (2026-09-23); packaging superseded by [ADR 0012](0012-product-packaging.md) (2026-09-24)

## Context

The service must register itself, run without a logged-in user, start at
boot, and be removable cleanly. The target is a single domain-joined PC whose
GPOs block unknown executables outside an ASR-excluded path and disable local
firewall rule merging. Releases are built by CI from Linux and Windows
runners.

## Decision

- Release artifact: a zip per version containing `benchwarmer.exe`,
  `bwtray.exe`, `install.ps1`, `uninstall.ps1`, an example config, and a
  `VERSION` file naming the exact tag. No MSI for now.
- `install.ps1` (run elevated) is idempotent and does, in order:
  1. Stop the service if present.
  2. Copy binaries to `C:\Program Files\Benchwarmer\` (admin-only write; the
     ASR exclusion path). The llama.cpp runtime lives in `...\runtime\`.
  3. Create `C:\ProgramData\Benchwarmer\` with `models\`, `secrets\`, `logs\`,
     set ACLs: the service account and Administrators full; `secrets\agent.token`
     readable by interactive users; the management and inference tokens
     readable only by the service account and Administrators.
  4. Generate missing tokens (32 random bytes, base64url).
  5. Register the service via `benchwarmer.exe service install` (delayed
     automatic start, failure recovery actions, the chosen account).
  6. Add `bwtray.exe` to `HKLM\...\Run` so it starts for every user session.
  7. Start the service and verify `GET /api/v1/health` responds.
  8. Print the GPO requirements (ASR exclusion, optional firewall rule) and
     whether the ASR exclusion appears to be in effect.
- `uninstall.ps1` stops and removes the service, the Run key, and the
  binaries; it keeps `ProgramData\Benchwarmer` (config, models, history)
  unless `-Purge` is given.
- Firewall: the installer creates no rules. Inbound LAN access is a GPO
  change because local rule merge is disabled on the target; documented in
  the GPO requirements.

## Consequences

- One auditable PowerShell script instead of an MSI database; it is easy to
  read before running, which matters on a GPO-managed machine.
- Signing: the executables this repo builds, and the MSI, are Authenticode
  signed through the lab's Azure Artifact Signing profile; the bundled runtime
  is Ollama's signed build and keeps its signature. The ASR path exclusion
  is expected to be no longer required, and stays documented as a recommended
  fallback until a fresh install is verified under rule 01443614 in Block mode
  with no exclusion.
- An MSI can be added later without changing the service, since service
  registration lives in `benchwarmer.exe`.
