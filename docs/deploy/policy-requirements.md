# Policy requirements on managed Windows machines

Benchwarmer runs unsigned executables (its own and the llama.cpp runtime).
On a machine managed by Group Policy or Intune, check these before installing.

## Required when ASR rule 01443614 is in Block mode

Defender attack surface reduction rule `01443614-cd74-433a-b99e-2ecdc07bfc25`
("Block executable files from running unless they meet a prevalence, age, or
trusted list criterion") blocks `benchwarmer.exe`, `bwtray.exe`, and
`llama-server.exe`. Add an ASR-only exclusion:

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
