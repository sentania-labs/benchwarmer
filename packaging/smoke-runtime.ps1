<#
.SYNOPSIS
  Starts the bundled llama-server with every flag Benchwarmer passes and
  checks that it answers /health.
.DESCRIPTION
  Ollama patches llama.cpp, so each flag is proven against this binary, not
  assumed from upstream. One server is started per case (a tiny GGUF model
  keeps that to seconds) so a failure names the flag. The environment is the
  one internal/runtime/llamacpp.Env builds: GGML_BACKEND_PATH at
  vulkan\ggml-vulkan.dll and that folder on PATH.

  Flags covered: -m --host --port -c -ngl (always), -np, --kv-unified, -fa,
  -ctk/-ctv, --n-cpu-moe, and --jinja. Each case also serves one
  completion. The server's own TLS flags are not covered: Benchwarmer never
  passes them (it terminates TLS itself and runs llama-server on loopback),
  and Ollama's build has no SSL support.

  What this cannot prove: the GitHub runner has no GPU, so the model runs on
  the CPU backend. The script checks that the Vulkan backend DLL is found and
  loaded from the runtime folder when the machine has a Vulkan loader
  (vulkan-1.dll); it only warns otherwise. Serving
  on a GPU needs a real machine.
.PARAMETER RuntimeDir
  The runtime folder containing llama-server.exe (runtime\vulkan).
.PARAMETER Model
  A GGUF model. Tiny is fine. Default: the model pinned in ollama-runtime.json.
.PARAMETER LogDir
  Where each case's server log is written.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)] [string] $RuntimeDir,
  [string] $Model,
  [string] $LogDir = '.',
  [string] $Pin = (Join-Path $PSScriptRoot 'ollama-runtime.json')
)
$ErrorActionPreference = 'Stop'
$RuntimeDir = (Resolve-Path $RuntimeDir).Path
New-Item -ItemType Directory -Force $LogDir | Out-Null
$LogDir = (Resolve-Path $LogDir).Path
if (-not $Model) {
  # The pinned tiny model, verified by SHA-256.
  $pinData = Get-Content $Pin -Raw | ConvertFrom-Json
  $Model = Join-Path $LogDir 'smoke-model.gguf'
  Invoke-WebRequest $pinData.smoke_model_url -OutFile $Model -MaximumRetryCount 3 -RetryIntervalSec 5
  $got = (Get-FileHash $Model -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($got -ne $pinData.smoke_model_sha256) { throw "SHA-256 mismatch for the smoke model: want $($pinData.smoke_model_sha256), got $got" }
}
$Model = (Resolve-Path $Model).Path
$Exe = Join-Path $RuntimeDir 'llama-server.exe'
$Backend = Join-Path $RuntimeDir 'vulkan\ggml-vulkan.dll'

function Step($m) { Write-Host "==> $m" }
function Fail($m) { throw "RUNTIME SMOKE FAIL: $m" }

foreach ($f in $Exe, (Join-Path $RuntimeDir 'libllama-server-impl.dll'), $Backend) {
  if (-not (Test-Path $f)) { Fail "missing $f (the lib\ollama layout must be kept)" }
}

# Same environment as llamacpp.Env.
$env:GGML_BACKEND_PATH = $Backend
$env:PATH = (Split-Path $Backend) + ';' + $env:PATH

function Get-FreePort {
  $l = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
  $l.Start(); $p = $l.LocalEndpoint.Port; $l.Stop(); $p
}

function Show-Log([string] $Path) {
  if (Test-Path $Path) { Write-Host "---- $Path"; Get-Content $Path -Tail 60 | ForEach-Object { Write-Host $_ } }
}

# Backends: ggml must find the Vulkan backend through GGML_BACKEND_PATH. The
# CPU backend needs no log check: every case below serves a completion on it.
# At the default log level ggml prints no "loaded" lines, so the evidence is
# a listed Vulkan device (a machine with a GPU) or the error ggml prints when
# it loaded vulkan\ggml-vulkan.dll from the runtime folder but found no GPU.
Step 'Backend discovery (--list-devices)'
$out = Join-Path $LogDir 'list-devices.log'
$p = Start-Process $Exe -ArgumentList '--list-devices' -NoNewWindow -Wait -PassThru -RedirectStandardOutput $out -RedirectStandardError "$out.err"
$text = (Get-Content $out, "$out.err" -ErrorAction SilentlyContinue) -join "`n"
Set-Content $out $text
Show-Log $out
if ($p.ExitCode -ne 0) { Fail "llama-server --list-devices exited $($p.ExitCode)" }
$vkLoader = Test-Path (Join-Path $env:windir 'System32\vulkan-1.dll')
if ($text -match 'Vulkan\d+:') {
  Write-Host 'Vulkan device listed.'
} elseif ($text -match 'initialize backend from .*ggml-vulkan\.dll') {
  Write-Host 'Vulkan backend DLL found and loaded from the runtime folder; no GPU on this runner to initialize it.'
} elseif ($vkLoader) {
  Fail 'this machine has a Vulkan loader but the Vulkan backend was not found (GGML_BACKEND_PATH / vulkan\ layout)'
} else {
  Write-Host '::warning::No Vulkan loader (vulkan-1.dll) on this runner; the Vulkan backend could not be exercised. Check it on a machine with a GPU.'
}

function Test-Case([string] $Name, [string[]] $Extra) {
  Step "llama-server $Name"
  $port = Get-FreePort
  $log = Join-Path $LogDir ("case-" + ($Name -replace '[^A-Za-z0-9]+', '_') + '.log')
  $srvArgs = @('-m', $Model, '--host', '127.0.0.1', '--port', "$port", '-c', '256', '-ngl', '0') + $Extra
  $proc = Start-Process $Exe -ArgumentList $srvArgs -NoNewWindow -PassThru -RedirectStandardOutput $log -RedirectStandardError "$log.err"
  $base = "http://127.0.0.1:$port"
  $ok = $false
  try {
    $deadline = (Get-Date).AddSeconds(90)
    while ((Get-Date) -lt $deadline) {
      if ($proc.HasExited) { break }
      try {
        $r = Invoke-WebRequest "$base/health" -TimeoutSec 3
        if ($r.StatusCode -eq 200) { $ok = $true; break }
      } catch { }
      Start-Sleep -Milliseconds 500
    }
    if ($ok) {
      $body = @{ prompt = 'Once'; n_predict = 2 } | ConvertTo-Json
      $c = Invoke-RestMethod "$base/completion" -Method Post -Body $body -ContentType 'application/json' -TimeoutSec 60
      if ($null -eq $c.content) { $ok = $false }
    }
  } finally {
    if (-not $proc.HasExited) { & taskkill.exe /T /F /PID $proc.Id | Out-Null }
    Set-Content $log ((Get-Content $log, "$log.err" -ErrorAction SilentlyContinue) -join "`n")
  }
  if (-not $ok) {
    Show-Log $log
    Fail "case '$Name' ($($Extra -join ' ')) did not answer /health and serve a completion"
  }
}

Test-Case 'base' @()
Test-Case 'np' @('-np', '2')
Test-Case 'kv-unified' @('-np', '2', '--kv-unified')
Test-Case 'flash-attn' @('-fa', 'on')
Test-Case 'cache-type' @('-fa', 'on', '-ctk', 'f16', '-ctv', 'f16')
Test-Case 'n-cpu-moe' @('--n-cpu-moe', '1')
Test-Case 'jinja' @('--jinja')
Test-Case 'all' @('-np', '2', '--kv-unified', '-fa', 'on', '-ctk', 'f16', '-ctv', 'f16', '--n-cpu-moe', '1', '--jinja')
Step 'Runtime smoke passed'
