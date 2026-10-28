<#
.SYNOPSIS
  Fails unless every executable and DLL in the bundled runtime carries a valid
  Authenticode signature from the pinned signer (Ollama Inc.).
.DESCRIPTION
  The runtime is unpacked from Ollama's release zip, which is signed (the
  upstream llama.cpp Windows builds are not). A future pin that points at an
  unsigned or differently signed zip must fail the build, not ship. A valid
  signature does not guarantee that a Windows security policy permits it.
  Needs Windows (Get-AuthenticodeSignature).

  The check is on the signer in the signing certificate, not only on the
  signature being valid, and covers every .exe and .dll under -RuntimeDir
  including vulkan\, the Microsoft C++ runtime DLLs Ollama re-signs among
  them.
.PARAMETER RuntimeDir
  The runtime folder (runtime\vulkan in the staged file set or an install).
.PARAMETER Pin
  The pin file. Default: packaging\ollama-runtime.json.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)] [string] $RuntimeDir,
  [string] $Pin = (Join-Path $PSScriptRoot 'ollama-runtime.json')
)
$ErrorActionPreference = 'Stop'

$signer = (Get-Content $Pin -Raw | ConvertFrom-Json).signer
$RuntimeDir = (Resolve-Path $RuntimeDir).Path
$files = @(Get-ChildItem $RuntimeDir -Recurse -File | Where-Object { $_.Extension -in '.exe', '.dll' })
if ($files.Count -eq 0) { throw "VERIFY FAIL: no executables or DLLs under $RuntimeDir" }

# CN=<signer> as a whole relative distinguished name of the certificate subject.
$cn = '(^|, )CN=' + [regex]::Escape($signer) + '(,|$)'
$bad = @()
foreach ($f in $files) {
  $sig = Get-AuthenticodeSignature -LiteralPath $f.FullName
  $rel = $f.FullName.Substring($RuntimeDir.Length).TrimStart('\')
  $who = if ($sig.SignerCertificate) { $sig.SignerCertificate.Subject } else { '(no signer)' }
  if ($sig.Status -ne 'Valid') { $bad += "$rel : status $($sig.Status) ($who)"; continue }
  if ($who -notmatch $cn) { $bad += "$rel : signed by $who, want CN=$signer"; continue }
}
if ($bad.Count -gt 0) {
  $bad | ForEach-Object { Write-Host "::error::$_" }
  throw "VERIFY FAIL: $($bad.Count) of $($files.Count) runtime files are not validly signed by $signer"
}
Write-Host "All $($files.Count) runtime executables and DLLs under $RuntimeDir are validly signed by $signer."
