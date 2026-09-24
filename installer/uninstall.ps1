#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Removes Benchwarmer. Keeps C:\ProgramData\Benchwarmer (config, models,
  history) unless -Purge is given.
#>
[CmdletBinding()]
param([switch] $Purge)
$ErrorActionPreference = 'Stop'
$Name = 'Benchwarmer'
$Prog = Join-Path $env:ProgramFiles 'Benchwarmer'
$Data = Join-Path $env:ProgramData 'Benchwarmer'

$exe = Join-Path $Prog 'benchwarmer.exe'
if (Test-Path $exe) {
  & $exe service remove
} elseif (Get-Service $Name -ErrorAction SilentlyContinue) {
  Stop-Service $Name -Force
  sc.exe delete $Name | Out-Null
}
Get-Process bwtray -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run' -Name 'Benchwarmer Tray' -ErrorAction SilentlyContinue
# Verify no runtime survived before deleting binaries.
Get-Process llama-server -ErrorAction SilentlyContinue | Where-Object { $_.Path -like "$Prog\*" } | Stop-Process -Force
Remove-Item -Recurse -Force $Prog -ErrorAction SilentlyContinue
if ($Purge) {
  Remove-Item -Recurse -Force $Data
  Write-Host "Removed $Prog and $Data."
} else {
  Write-Host "Removed $Prog. Kept $Data (use -Purge to delete config, models, and history)."
}
