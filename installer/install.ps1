#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Installs or upgrades Benchwarmer (ADR 0009). Idempotent; safe to re-run.
.DESCRIPTION
  Copies binaries to C:\Program Files\Benchwarmer (the Defender ASR exclusion
  path), prepares C:\ProgramData\Benchwarmer with restrictive ACLs, generates
  missing API tokens, registers the Windows service, registers the tray app
  for every user session, starts the service, and verifies it responds.
  Creates no firewall rules: inbound LAN access is a GPO change.
.PARAMETER Account
  Service identity: "system" (LocalSystem, default; the runtime itself runs as a
  privilege-stripped LocalService, ADR 0006) or "virtual" (NT SERVICE\Benchwarmer,
  which cannot read GPU counters on the target and is not recommended).
.PARAMETER RuntimeZip
  Optional llama.cpp release zip to unpack into ...\runtime\<RuntimeName>.
#>
[CmdletBinding()]
param(
  [ValidateSet('system', 'virtual')] [string] $Account = 'system',
  [string] $RuntimeZip,
  [string] $RuntimeName = 'vulkan'
)
$ErrorActionPreference = 'Stop'
$Name = 'Benchwarmer'
$Src = $PSScriptRoot
$Prog = Join-Path $env:ProgramFiles 'Benchwarmer'
$Data = Join-Path $env:ProgramData 'Benchwarmer'
$Secrets = Join-Path $Data 'secrets'
$SvcSid = if ($Account -eq 'virtual') { "NT SERVICE\$Name" } else { 'NT AUTHORITY\SYSTEM' }

function Step($m) { Write-Host "==> $m" }

# 1. Stop the service if present so binaries can be replaced.
$svc = Get-Service $Name -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Stopped') {
  Step 'Stopping service'
  Stop-Service $Name -Force
  $svc.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
}
Get-Process bwtray -ErrorAction SilentlyContinue | Stop-Process -Force

# 2. Binaries.
Step "Installing binaries to $Prog"
New-Item -ItemType Directory -Force $Prog, (Join-Path $Prog 'runtime') | Out-Null
foreach ($f in 'benchwarmer.exe', 'bwtray.exe', 'VERSION') {
  Copy-Item (Join-Path $Src $f) $Prog -Force
}
if ($RuntimeZip) {
  $dest = Join-Path $Prog "runtime\$RuntimeName"
  Step "Unpacking runtime to $dest"
  Expand-Archive -Force $RuntimeZip $dest
}

# 3. Data directory.
Step "Preparing $Data"
New-Item -ItemType Directory -Force $Data, $Secrets, (Join-Path $Data 'models'), (Join-Path $Data 'logs') | Out-Null

# 4. Config: keep an existing one; seed the example on first install.
$cfg = Join-Path $Data 'config.json'
if (-not (Test-Path $cfg) -and (Test-Path (Join-Path $Src 'config.example.json'))) {
  Copy-Item (Join-Path $Src 'config.example.json') $cfg
}

# 5. Service registration. This comes before the ACLs: the virtual account
# NT SERVICE\Benchwarmer only resolves to a SID once the service exists.
Step "Registering service ($SvcSid)"
& (Join-Path $Prog 'benchwarmer.exe') service install --account $Account --data $Data
if ($LASTEXITCODE -ne 0) { throw "service install failed ($LASTEXITCODE)" }

# 6. ACLs. Every icacls call is checked: a silent failure would leave
# secrets with the permissive ProgramData defaults.
function Set-Acl-Checked([string] $Path, [string[]] $IcaclsArgs) {
  & icacls $Path @IcaclsArgs | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "icacls failed on $Path ($LASTEXITCODE)" }
}
# Data (config, database, history): service and administrators only. The
# config can carry runtime arguments with secrets.
Set-Acl-Checked $Data @('/inheritance:r', '/grant:r', 'SYSTEM:(OI)(CI)F', 'Administrators:(OI)(CI)F', "${SvcSid}:(OI)(CI)M")
# Logs contain no secrets or prompts; readable by local users for support.
Set-Acl-Checked (Join-Path $Data 'logs') @('/grant', 'Users:(OI)(CI)RX')
# Models: readable by users (large, not secret).
Set-Acl-Checked (Join-Path $Data 'models') @('/grant', 'Users:(OI)(CI)RX')

# 7. Tokens (32 random bytes, base64url), only if missing.
function New-Token {
  $b = New-Object byte[] 32
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
  [Convert]::ToBase64String($b).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}
foreach ($t in 'management', 'inference', 'agent') {
  $p = Join-Path $Secrets "$t.token"
  if (-not (Test-Path $p)) {
    Step "Generating $t token"
    Set-Content -Path $p -Value (New-Token) -NoNewline -Encoding ascii
  }
}
# The tray runs as the interactive user and needs the agent token only. It
# must traverse the data and secrets folders to reach it.
Set-Acl-Checked $Data @('/grant', 'INTERACTIVE:(X)')
Set-Acl-Checked $Secrets @('/grant', 'INTERACTIVE:(X)')
Set-Acl-Checked (Join-Path $Secrets 'agent.token') @('/grant', 'INTERACTIVE:R')

# 8. Tray for every user session.
Step 'Registering tray app'
Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run' -Name 'Benchwarmer Tray' -Value "`"$(Join-Path $Prog 'bwtray.exe')`""

# 9. Start and verify.
Step 'Starting service'
Start-Service $Name
$ok = $false
for ($i = 0; $i -lt 30 -and -not $ok; $i++) {
  Start-Sleep -Seconds 1
  try { $h = Invoke-RestMethod -TimeoutSec 2 'http://127.0.0.1:8481/api/v1/health'; $ok = $true } catch { }
}
if (-not $ok) { throw 'Service did not answer on http://127.0.0.1:8481/api/v1/health within 30 s; see the event log and logs folder.' }
Write-Host "Service healthy (version $($h.version))."

# 10. Policy prerequisites.
$excl = (Get-MpPreference).AttackSurfaceReductionOnlyExclusions
if (-not ($excl | Where-Object { $_.TrimEnd('\') -ieq $Prog.TrimEnd('\') })) {
  Write-Warning "No Defender ASR exclusion for $Prog was found. If ASR rule 01443614-cd74-433a-b99e-2ecdc07bfc25 is in Block mode, the runtime and tray will be blocked. See docs/deploy/policy-requirements.md."
}
Write-Host 'Done. Dashboard: http://127.0.0.1:8481/'
