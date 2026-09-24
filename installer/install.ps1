#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Installs or upgrades Benchwarmer from the release zip (ADR 0012,
  "Installing by hand"). Idempotent; safe to re-run.
.DESCRIPTION
  Run from an elevated PowerShell in the unzipped folder. It:
    1. stops the service, the tray, and any runtime if present;
    2. copies the release files (binaries, runtime\vulkan, licenses) to
       C:\Program Files\Benchwarmer (the Defender ASR exclusion path);
    3. registers the service with `benchwarmer.exe service install`, the
       same registration the MSI makes;
    4. registers the tray for every user session (HKLM Run);
    5. starts the service and checks that it answers health.

  The service creates and secures C:\ProgramData\Benchwarmer (folders, ACLs,
  tokens, default config) itself on every start, so this script does not
  touch it. Creates no firewall rules: inbound LAN access is a GPO change.

  Use the MSI or this script, not both. If the MSI is installed, upgrade
  with a newer MSI instead.
#>
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$Name = 'Benchwarmer'
$Src = $PSScriptRoot
$Prog = Join-Path $env:ProgramFiles 'Benchwarmer'
$Data = Join-Path $env:ProgramData 'Benchwarmer'
$Health = 'http://127.0.0.1:8481/api/v1/health'

function Step($m) { Write-Host "==> $m" }

# 0. Refuse to mix install routes: the MSI owns its files and service entry,
# and a later MSI uninstall or upgrade would fight this script's copy.
$msi = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue |
  Where-Object { $_.DisplayName -eq 'Benchwarmer' -and $_.WindowsInstaller -eq 1 }
if ($msi) {
  throw "Benchwarmer $($msi.DisplayVersion) is installed from the MSI. Upgrade it with a newer MSI, or uninstall it (Settings > Apps) before using install.ps1."
}

# The release file set this script installs.
$files = 'benchwarmer.exe', 'bwtray.exe', 'bwprobe.exe', 'VERSION', 'config.example.json'
$dirs = 'runtime\vulkan', 'licenses'
foreach ($f in $files + $dirs) {
  if (-not (Test-Path (Join-Path $Src $f))) { throw "Missing $f next to install.ps1; run it from the unzipped release folder." }
}
if (-not (Test-Path (Join-Path $Src 'runtime\vulkan\llama-server.exe'))) { throw 'Missing runtime\vulkan\llama-server.exe in the release folder.' }

# 1. Stop what is running so files can be replaced.
$svc = Get-Service $Name -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Stopped') {
  Step 'Stopping service'
  Stop-Service $Name -Force
  $svc.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
}
Get-Process bwtray -ErrorAction SilentlyContinue | Stop-Process -Force
Get-Process llama-server -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Prog\*" } | Stop-Process -Force

# 2. Files. runtime\vulkan and licenses are replaced as a whole so files
# dropped by a newer llama.cpp release do not linger. Other folders under
# runtime\ (a runtime added by hand) are left alone.
Step "Installing files to $Prog"
New-Item -ItemType Directory -Force $Prog, (Join-Path $Prog 'runtime') | Out-Null
foreach ($f in $files) {
  Copy-Item (Join-Path $Src $f) $Prog -Force
}
foreach ($d in $dirs) {
  $dest = Join-Path $Prog $d
  if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }
  Copy-Item -Recurse (Join-Path $Src $d) $dest
}

# 3. Service registration: creates the service, or updates it in place on
# an upgrade. LocalSystem is the only supported identity (ADR 0006).
Step 'Registering service'
& (Join-Path $Prog 'benchwarmer.exe') service install --account system --data $Data
if ($LASTEXITCODE -ne 0) { throw "benchwarmer.exe service install failed ($LASTEXITCODE)" }

# 4. Tray for every user session.
Step 'Registering tray app'
Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run' -Name 'Benchwarmer Tray' -Value "`"$(Join-Path $Prog 'bwtray.exe')`""

# 5. Start and verify. The service provisions its data folder as it starts.
Step 'Starting service'
Start-Service $Name
$h = $null
$deadline = (Get-Date).AddSeconds(60)
while (-not $h -and (Get-Date) -lt $deadline) {
  try { $h = Invoke-RestMethod -TimeoutSec 2 $Health } catch { Start-Sleep -Seconds 1 }
}
if (-not $h) { throw "Service did not answer on $Health within 60 s; see the Application event log and $Data\logs." }
Write-Host "Service healthy (version $($h.version))."

# 6. Policy prerequisites.
try {
  $excl = (Get-MpPreference).AttackSurfaceReductionOnlyExclusions
  if (-not ($excl | Where-Object { $_.TrimEnd('\') -ieq $Prog.TrimEnd('\') })) {
    Write-Warning "No Defender ASR exclusion for $Prog was found. If ASR rule 01443614-cd74-433a-b99e-2ecdc07bfc25 is in Block mode, the runtime and tray will be blocked. See docs/deploy/policy-requirements.md."
  }
} catch {
  Write-Warning "Could not read Defender settings ($_). Check the ASR exclusion for $Prog by hand; see docs/deploy/policy-requirements.md."
}

if (-not (Get-ChildItem (Join-Path $Data 'models') -Filter *.gguf -ErrorAction SilentlyContinue)) {
  Write-Host "Next: copy a GGUF model into $Data\models\ and choose it in the dashboard's Configuration page. Until then the service reports Setup required."
}
Write-Host 'Done. Dashboard: http://127.0.0.1:8481/'
