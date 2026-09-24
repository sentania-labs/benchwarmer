<#
.SYNOPSIS
  Install, verify, uninstall, verify: the release smoke test (ADR 0012).
.DESCRIPTION
  Runs on a clean Windows machine (the GitHub-hosted runner) as an
  administrator. Installs Benchwarmer by one route, checks that the service
  is registered exactly as specified, runs, answers health within 60 s, and
  has provisioned its data folder; then uninstalls and checks that the
  service, the Run value and Program Files\Benchwarmer are gone while
  ProgramData\Benchwarmer is kept.

  -Mode msi installs the MSI given by -Path with msiexec /qn.
  -Mode zip runs install.ps1 / uninstall.ps1 from the unpacked zip folder
  given by -Path, under Windows PowerShell 5.1 as an admin would.

  With -UpgradeFrom (msi only), that older MSI is installed and verified
  first, then -Path is installed over it as a major upgrade.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)] [ValidateSet('msi', 'zip')] [string] $Mode,
  [Parameter(Mandatory)] [string] $Path,
  [string] $UpgradeFrom,
  [string] $LogDir = '.'
)
$ErrorActionPreference = 'Stop'
$Name = 'Benchwarmer'
$Prog = Join-Path $env:ProgramFiles 'Benchwarmer'
$Data = Join-Path $env:ProgramData 'Benchwarmer'
$Health = 'http://127.0.0.1:8481/api/v1/health'
$RunKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run'
# Must match cmd/benchwarmer `service install` and packaging/wix/Benchwarmer.wxs.
$Description = 'Runs a local LLM on the GPU only while it is otherwise idle; yields to games and interactive use.'

function Step($m) { Write-Host "==> $m" }
function Fail($m) { throw "SMOKE FAIL: $m" }

$Path = (Resolve-Path $Path).Path
New-Item -ItemType Directory -Force $LogDir | Out-Null
$LogDir = (Resolve-Path $LogDir).Path

function Invoke-Msiexec([string] $Action, [string] $Msi, [string] $Log) {
  Step "msiexec $Action $(Split-Path $Msi -Leaf) (log $Log)"
  $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList @($Action, "`"$Msi`"", '/qn', '/l*v', "`"$Log`"")
  if ($p.ExitCode -ne 0) { Fail "msiexec $Action exited $($p.ExitCode); see $Log" }
}

function Invoke-Script([string] $Script) {
  Step "powershell -File $Script"
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $Script
  if ($LASTEXITCODE -ne 0) { Fail "$Script exited $LASTEXITCODE" }
}

function Get-Arp {
  @(Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue |
      Where-Object { $_.DisplayName -eq 'Benchwarmer' })
}

function Assert-Installed([string] $ExpectVersion) {
  Step 'Checking service registration'
  $svc = Get-CimInstance Win32_Service -Filter "Name='$Name'"
  if (-not $svc) { Fail 'service Benchwarmer is not registered' }
  $want = "$(Join-Path $Prog 'benchwarmer.exe') run --service --data $Data"
  $got = $svc.PathName -replace '"', ''
  if ($got -ne $want) { Fail "service command line is '$($svc.PathName)', want '$want' (quotes aside)" }
  if ($svc.StartName -ne 'LocalSystem') { Fail "service account is '$($svc.StartName)', want LocalSystem" }
  if ($svc.StartMode -ne 'Auto') { Fail "service start mode is '$($svc.StartMode)', want Auto" }
  if ($svc.DisplayName -ne 'Benchwarmer') { Fail "service display name is '$($svc.DisplayName)'" }
  if ($svc.Description -ne $Description) { Fail "service description is '$($svc.Description)'" }

  Step "Waiting for $Health (60 s)"
  $deadline = (Get-Date).AddSeconds(60)
  $h = $null
  while (-not $h -and (Get-Date) -lt $deadline) {
    try { $h = Invoke-RestMethod -TimeoutSec 2 $Health } catch { Start-Sleep -Seconds 1 }
  }
  if (-not $h) { Fail "no answer from $Health within 60 s" }
  Write-Host "health: $($h | ConvertTo-Json -Compress -Depth 5)"
  if ((Get-Service $Name).Status -ne 'Running') { Fail "service is $((Get-Service $Name).Status), want Running" }
  # Informational until the setup_required reason lands (ADR 0012).
  try { Write-Host "status: $(Invoke-RestMethod -TimeoutSec 5 'http://127.0.0.1:8481/api/v1/status' | ConvertTo-Json -Compress -Depth 6)" } catch { Write-Host "status: unavailable ($_)" }

  Step 'Checking files'
  foreach ($f in 'benchwarmer.exe', 'bwtray.exe', 'bwprobe.exe', 'VERSION', 'runtime\vulkan\llama-server.exe', 'runtime\vulkan\ggml-vulkan.dll', 'licenses\LICENSE-llama.cpp') {
    if (-not (Test-Path (Join-Path $Prog $f))) { Fail "missing $Prog\$f" }
  }
  Write-Host "VERSION: $((Get-Content (Join-Path $Prog 'VERSION') -Raw).Trim())"

  $tok = Join-Path $Data 'secrets\management.token'
  if (-not (Test-Path $tok)) { Fail "missing $tok" }

  $run = (Get-ItemProperty $RunKey -Name 'Benchwarmer Tray' -ErrorAction SilentlyContinue).'Benchwarmer Tray'
  $wantRun = "`"$(Join-Path $Prog 'bwtray.exe')`""
  if ($run -ne $wantRun) { Fail "Run value 'Benchwarmer Tray' is '$run', want '$wantRun'" }

  if ($Mode -eq 'msi') {
    $arp = Get-Arp
    if ($arp.Count -ne 1) { Fail "expected one Programs and Features entry, found $($arp.Count)" }
    if ($arp[0].Publisher -ne 'sentania labs') { Fail "ARP publisher is '$($arp[0].Publisher)'" }
    if ($ExpectVersion -and $arp[0].DisplayVersion -ne $ExpectVersion) { Fail "ARP version is '$($arp[0].DisplayVersion)', want $ExpectVersion" }
  }
  Step 'Installed state OK'
}

function Assert-Removed {
  Step 'Checking removal'
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Service $Name -ErrorAction SilentlyContinue) -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 1 }
  if (Get-Service $Name -ErrorAction SilentlyContinue) { Fail 'service still registered after uninstall' }
  if (Test-Path $Prog) { Fail "$Prog still exists: $((Get-ChildItem -Recurse $Prog | Select-Object -ExpandProperty FullName) -join ', ')" }
  if ((Get-ItemProperty $RunKey -ErrorAction SilentlyContinue).'Benchwarmer Tray') { Fail 'Run value still present' }
  if (-not (Test-Path (Join-Path $Data 'secrets\management.token'))) { Fail "$Data was not kept" }
  if ($Mode -eq 'msi' -and (Get-Arp).Count -ne 0) { Fail 'Programs and Features entry still present' }
  Step 'Removed state OK (ProgramData kept)'
}

function Get-MsiVersion([string] $Msi) {
  # The MSI file name carries the ProductVersion: benchwarmer-X.Y.Z-x64.msi.
  if ((Split-Path $Msi -Leaf) -match '^benchwarmer-(\d+\.\d+\.\d+)-x64\.msi$') { return $Matches[1] }
  return $null
}

if ($Mode -eq 'msi') {
  if ($UpgradeFrom) {
    $old = (Resolve-Path $UpgradeFrom).Path
    Invoke-Msiexec '/i' $old (Join-Path $LogDir 'msi-upgrade-from.log')
    Assert-Installed (Get-MsiVersion $old)
  }
  Invoke-Msiexec '/i' $Path (Join-Path $LogDir 'msi-install.log')
  Assert-Installed (Get-MsiVersion $Path)
  Invoke-Msiexec '/x' $Path (Join-Path $LogDir 'msi-uninstall.log')
  Assert-Removed
} else {
  Invoke-Script (Join-Path $Path 'install.ps1')
  Assert-Installed $null
  # Re-running must be safe (idempotent upgrade path).
  Invoke-Script (Join-Path $Path 'install.ps1')
  Assert-Installed $null
  Invoke-Script (Join-Path $Path 'uninstall.ps1')
  Assert-Removed
}
Write-Host "Smoke test passed ($Mode)."
