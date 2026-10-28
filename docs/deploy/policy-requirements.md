# Policy requirements on managed Windows machines

The executables Benchwarmer ships are Authenticode signed (`benchwarmer.exe`,
`bwtray.exe`, `bwprobe.exe`, and the MSI); the bundled llama-server runtime is
Ollama's own build, signed by Ollama Inc. On a machine managed by Group Policy or Intune,
check these before installing.

## Recommended when ASR rule 01443614 is in Block mode

Defender attack surface reduction rule `01443614-cd74-433a-b99e-2ecdc07bfc25`
("Block executable files from running unless they meet a prevalence, age, or
trusted list criterion") blocked the old unsigned llama.cpp `llama-server.exe`
(event 1121, "Access is denied" from CreateProcess). The bundled runtime is now
Ollama's Authenticode-signed build and Benchwarmer's own executables and MSI
are org-signed, so the exclusion is expected to be no longer required. A
signature does not guarantee the rule allows a file, so it stays documented as a
recommended fallback until a fresh install is verified on a machine with the
rule in Block mode and no exclusion. If `benchwarmer.exe`, `bwtray.exe` or the
runtime is blocked, add an ASR-only exclusion:

- Setting: *Microsoft Defender Antivirus > Microsoft Defender Exploit Guard >
  Attack Surface Reduction > Exclude files and paths from Attack Surface
  Reduction Rules*
- Value: `C:\Program Files\Benchwarmer\` = `0`

Exclude only that path. It is writable only by Administrators and SYSTEM, so
the exclusion does not give standard users a place to run arbitrary code.
Never exclude a folder under `C:\` that inherits the root's permissions
(Authenticated Users can create files there).

Check: `(Get-MpPreference).AttackSurfaceReductionOnlyExclusions`; blocks show
as event 1121 in *Microsoft-Windows-Windows Defender/Operational*.
`install.ps1` warns when the exclusion is missing; the MSI cannot check.

## Required only for LAN access to the inference endpoint

By default both listeners are on loopback and no firewall rule is needed. To
serve other machines, set `listen.inference` to a LAN address and allow
inbound TCP on that port for `%ProgramFiles%\Benchwarmer\benchwarmer.exe`,
scoped to the calling hosts. If Group Policy disables local firewall rule
merging, the rule must come from Group Policy; neither the MSI nor
`install.ps1` creates one.
Set `security.require_inference_token` to `true` when exposing it.

The management listener (`listen.management`) should stay on loopback.

## Usually already satisfied

- *Log on as a service*: the virtual account `NT SERVICE\Benchwarmer` is
  covered when the right includes `NT SERVICE\ALL SERVICES` (S-1-5-80-0).
- *Restricted Groups*: if Performance Monitor Users is managed centrally, add
  `NT SERVICE\Benchwarmer` there only if the service cannot read GPU counters
  (see ADR 0006).
- Controlled Folder Access does not cover `ProgramData`, where Benchwarmer
  writes.
