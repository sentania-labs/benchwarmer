#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Removes a Benchwarmer install made with install.ps1. Keeps
  C:\ProgramData\Benchwarmer (config, models, history, tokens) unless -Purge
  is given.
.DESCRIPTION
  For an MSI install, uninstall from Settings > Apps or with
  `msiexec /x benchwarmer-X.Y.Z-x64.msi /qn` instead; this script refuses.
  After an MSI uninstall, `uninstall.ps1 -Purge` still removes the data
  folder.
#>
[CmdletBinding()]
param([switch] $Purge)
$ErrorActionPreference = 'Stop'
$Name = 'Benchwarmer'
$Prog = Join-Path $env:ProgramFiles 'Benchwarmer'
$Data = Join-Path $env:ProgramData 'Benchwarmer'

$msi = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' -ErrorAction SilentlyContinue |
  Where-Object { $_.DisplayName -eq 'Benchwarmer' -and $_.WindowsInstaller -eq 1 }
if ($msi) {
  throw "Benchwarmer $($msi.DisplayVersion) is installed from the MSI. Uninstall it from Settings > Apps or with msiexec /x."
}

if (Get-Service $Name -ErrorAction SilentlyContinue) {
  $exe = Join-Path $Prog 'benchwarmer.exe'
  if (Test-Path $exe) {
    # Stops the service (the runtime drains or stops with it) and deletes it.
    & $exe service remove
    if ($LASTEXITCODE -ne 0) { throw "benchwarmer.exe service remove failed ($LASTEXITCODE)" }
  } else {
    Stop-Service $Name -Force
    sc.exe delete $Name | Out-Null
  }
}
Get-Process bwtray -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run' -Name 'Benchwarmer Tray' -ErrorAction SilentlyContinue
# Make sure no runtime survived before deleting its files.
Get-Process llama-server -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Prog\*" } | Stop-Process -Force
if (Test-Path $Prog) { Remove-Item -Recurse -Force $Prog }
if ($Purge) {
  if (Test-Path $Data) { Remove-Item -Recurse -Force $Data }
  Write-Host "Removed $Prog and $Data."
} else {
  Write-Host "Removed $Prog. Kept $Data (use -Purge to delete config, models, history, and tokens)."
}
