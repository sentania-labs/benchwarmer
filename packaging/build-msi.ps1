<#
.SYNOPSIS
  Builds benchwarmer-X.Y.Z-x64.msi from the staged release file set (ADR 0012).
.DESCRIPTION
  Installs the pinned WiX Toolset as a .NET global tool if it is missing or a
  different version, then builds packaging\wix\Benchwarmer.wxs against
  StageDir (produced by `make stage` on Linux).

  The MSI ProductVersion must be numeric. It is taken from StageDir\VERSION:
  a release tag vX.Y.Z becomes X.Y.Z; anything else (an untagged CI build)
  becomes 0.0.0.

  Writes the MSI path to stdout.
.PARAMETER StageDir
  The staged file set. Default: dist\stage in the repository.
.PARAMETER OutDir
  Where the MSI is written. Default: dist in the repository.
.PARAMETER Version
  Override the ProductVersion (X.Y.Z). CI uses it to build a second, newer
  package from the same files to test a major upgrade.
#>
[CmdletBinding()]
param(
  [string] $StageDir = (Join-Path $PSScriptRoot '..\dist\stage'),
  [string] $OutDir = (Join-Path $PSScriptRoot '..\dist'),
  [string] $Version
)
$ErrorActionPreference = 'Stop'

# The WiX Toolset version. Change it here only; CI and release both call
# this script.
$WixVersion = '6.0.2'

$StageDir = (Resolve-Path $StageDir).Path
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path

$tag = (Get-Content (Join-Path $StageDir 'VERSION') -Raw).Trim()
$ver = $tag -replace '^v', ''
if ($ver -notmatch '^\d+\.\d+\.\d+$') { $ver = '0.0.0' }
if ($Version) {
  if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "Version must be X.Y.Z, got '$Version'" }
  $ver = $Version
}

# Global .NET tools land here; make sure this process can find them.
$tools = Join-Path $env:USERPROFILE '.dotnet\tools'
if (($env:PATH -split ';') -notcontains $tools) { $env:PATH = "$tools;$env:PATH" }

$have = $null
if (Get-Command wix -ErrorAction SilentlyContinue) {
  $have = ((& wix --version) -split '\+')[0].Trim()
}
if ($have -ne $WixVersion) {
  if ($have) {
    & dotnet tool uninstall --global wix | Out-Host
  }
  & dotnet tool install --global wix --version $WixVersion | Out-Host
  if ($LASTEXITCODE -ne 0) { throw "installing WiX $WixVersion failed ($LASTEXITCODE)" }
}

$msi = Join-Path $OutDir "benchwarmer-$ver-x64.msi"
$wxs = Join-Path $PSScriptRoot 'wix\Benchwarmer.wxs'
# The Util extension provides CloseApplication (closes the tray on upgrade).
& wix extension add --global "WixToolset.Util.wixext/$WixVersion" | Out-Host
if ($LASTEXITCODE -ne 0) { throw "wix extension add failed ($LASTEXITCODE)" }
& wix build -arch x64 -ext WixToolset.Util.wixext -d "ProductVersion=$ver" -d "StageDir=$StageDir" -o $msi $wxs | Out-Host
if ($LASTEXITCODE -ne 0) { throw "wix build failed ($LASTEXITCODE)" }
Write-Output $msi
