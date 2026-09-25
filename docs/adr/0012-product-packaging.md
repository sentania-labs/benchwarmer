# ADR 0012: Product packaging, self-provisioning, and first-run setup

Status: Accepted (2026-09-24). Supersedes the packaging parts of ADR 0009.

## Context

Until now the target PC was set up by hand from a development box: binaries
copied over WinRM, the llama.cpp runtime unpacked separately, the config
edited through the API, the certificate imported with a script. That is
deployment work, and it leaves drift the product cannot account for.

From here Benchwarmer is a product. A release is a tag. Someone else installs
it, by Group Policy software installation, by `msiexec`, or by hand, and then
configures it through the local dashboard and the management API. Nothing
about a working install may depend on steps that exist only in a developer's
shell history.

Constraints:
- **GPO Software Installation** deploys only Windows Installer packages (MSI),
  per machine, silently, as SYSTEM, at boot.
- **Managed PCs** have local firewall rule merging disabled and ASR rules
  that block low-prevalence executables outside an excluded path
  (`docs/deploy/policy-requirements.md`).
- **Model downloading** is out of scope (spec section 3). The admin supplies
  the GGUF file.

## Decision

### Artifacts

Each tag `vX.Y.Z` publishes:

1. `benchwarmer-X.Y.Z-x64.msi`: per-machine Windows Installer package, the
   primary artifact. Suitable for GPO assignment and `msiexec /qn`.
2. `benchwarmer-vX.Y.Z-windows-amd64.zip`: the same files plus
   `install.ps1`/`uninstall.ps1`, for installing by hand without the MSI.
3. `SHA256SUMS`.

Both carry the same file set:

| Path under `C:\Program Files\Benchwarmer\` | What |
|---|---|
| `benchwarmer.exe` | Service, CLI |
| `bwtray.exe` | Tray and session agent |
| `bwprobe.exe` | Diagnostics (Phase 0 probe) |
| `runtime\vulkan\` | llama.cpp release pinned in `packaging/llama-cpp.json`, verified by SHA-256 at build time |
| `licenses\` | Benchwarmer and llama.cpp licenses |
| `VERSION` | The exact tag |

The MSI is built with WiX Toolset (pinned version, installed as a .NET tool)
on the GitHub-hosted Windows runner. CI builds it on every push, to validate
it; the release workflow builds and publishes it on a tag. Artifacts are
unsigned until a code-signing certificate exists; the ASR path exclusion is
what lets them run.

### What the MSI does

- Installs the files above.
- Registers the `Benchwarmer` service natively (ServiceInstall): LocalSystem,
  automatic start, command line `run --service --data
  "C:\ProgramData\Benchwarmer"`. Starts it on install; stops and removes it on
  uninstall.
- Adds `bwtray.exe` to `HKLM\Software\Microsoft\Windows\CurrentVersion\Run`.
- Major upgrades in place (fixed UpgradeCode); the service is stopped, the
  runtime drains or stops gracefully, files are replaced, and the service
  starts again.
- Uninstall keeps `C:\ProgramData\Benchwarmer` (config, models, history,
  tokens). Removing it is a documented manual step.
- Creates **no firewall rules** and changes no Defender settings: on managed
  PCs those are GPO settings (local rule merge is off), documented in
  `docs/deploy/`.
- Has no custom actions of its own and no required properties. It uses
  WiX's standard CloseApplication to close a signed-in user's tray during an
  upgrade or uninstall, so no restart is needed. The service start is not
  waited on, so a start that policy blocks (an ASR exclusion not yet
  applied) does not roll the install back; the service's recovery actions
  retry. Everything else is done by the service.

### What the service does for itself (every start)

So that an MSI install, a hand install, and an upgrade all converge on the
same state, the service provisions itself at start, idempotently:

- Creates the data folder and `secrets\`, `models\`, `logs\`, `tls\`.
- Applies ACLs:
  - **Data folder:** SYSTEM and Administrators only, not inherited from
    ProgramData.
  - **`logs\` and `models\`:** local users can also read.
  - **Agent token:** the interactive user can read it, with traverse rights on
    the folders above it.
  - A drifted ACL is repaired.
- Generates missing tokens (32 random bytes, base64url).
- Writes the default configuration if there is none. A `config.json` placed
  in the data folder before first start (for example by a GPO file
  preference) is used as-is, which is how a fleet can be pre-configured.
- Sets its own service recovery actions and delayed automatic start.

If provisioning fails the service still starts. It reports the problem in
status and events, and does not load a model while secrets could be exposed.

### First-run setup

- **Setup required.** A fresh install has no model, so the service reports
  **Setup required** instead of repeatedly failing to load. That is a
  distinct eligibility reason (`eligibility.setup_required`, Unavailable)
  whenever the runtime executable or model file is missing. It clears on its
  own when the file appears or the config changes.
- **Pickers.** The management API lists what the dashboard needs to pick
  from without typing paths:
  - models (`*.gguf`) in `models\`;
  - installed runtimes (`runtime\*\llama-server.exe`);
  - certificates in `LocalMachine\My` that are usable for server
    authentication, with subject, names, thumbprint and expiry.
- **Admin sign-in.** Changing settings needs the management token (ADR 0007).
  Reading a SYSTEM-only file by hand is poor UX. The tray gets **Sign in to
  change settings**:
  1. The tray makes a random single-use code and runs `benchwarmer.exe login
     --code <code>`, elevated through UAC.
  2. The elevated helper refuses unless it runs as the same account that is
     signed in to the session. An administrator typing credentials into a
     standard user's prompt would otherwise hand that user the session.
  3. The helper reads the management token and registers the code with the
     service (valid for 60 seconds, redeemable once, only from this PC).
  4. The unelevated tray opens the dashboard with the code in the URL
     fragment, so the browser never runs elevated.
  5. The dashboard removes the code from the address bar and redeems it for a
     **session token**. The session token acts as the management token but
     only from this PC, and it expires after 8 hours. The management token
     never leaves the elevated helper.

  Limits, accepted:
  - Another process running as the same user could read the code from the
    browser's command line and redeem it first. Same-user processes are
    inside the user's trust boundary anyway: they can read the tab's storage.
  - The single-use code can remain in the browser's history.
  - Redemption requires a JSON body, so a web page cannot submit guesses
    without a CORS preflight, which the API never grants.
- **Restart from the dashboard.** Changes that need a service restart
  (listeners, the HTTPS certificate selection, token files, logging) show a
  **Restart service now** button. It calls `POST /api/v1/service/restart`,
  which starts a detached `benchwarmer service restart`. That helper stops
  the service through the SCM, so the GPU is released the normal way, and
  starts it again. The page waits for the service to go down and come back.
  An in-process listener rebind was considered, but it would cover only the
  listeners, and the restart covers every such change with one path.

### Installing by hand

Unzip, then from an elevated PowerShell run `.\install.ps1`. It copies the
files and runs `benchwarmer.exe service install`, which registers the
service exactly as the MSI does. The service provisions itself on start as
above, so the script no longer creates folders, ACLs or tokens.

## Consequences

- **One install state.** A GPO deployment, a hand install and an upgrade end
  up the same. An admin repairs a broken ACL or a deleted token by restarting
  the service.
- **Release size.** The MSI carries the llama.cpp runtime, about 32 MB
  compressed. Upgrading llama.cpp is a change to `packaging/llama-cpp.json`
  and a new release, tested like any other change.
- **Model files** are still copied by an admin (or a GPO file preference) to
  `C:\ProgramData\Benchwarmer\models\`.
- **Unsigned MSI.** GPO accepts it. SmartScreen and ASR need the documented
  exclusions.
- **Fleet configuration** is either a pre-placed `config.json` or API calls.
  There is no ADMX template; adding one later would be additive.
- **The developer no longer installs** on the target. A release is verified
  on a clean Windows runner by installing the MSI and checking the service
  comes up in Setup required, answers health, and uninstalls cleanly.
