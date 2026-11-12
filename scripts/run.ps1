<#
.SYNOPSIS
  Start the NVIDIA playground gateway for Claude Code.

.EXAMPLE
  .\scripts\run.ps1                                   # 127.0.0.1:8080, captcha pool on
  $env:GNVIPI_ADMIN_TOKEN='...'; .\scripts\run.ps1 -Lan  # :8080 on all interfaces; env token enables /admin
  .\scripts\run.ps1 -Port 8099                        # another port (adjust ANTHROPIC_BASE_URL accordingly)
  .\scripts\run.ps1 -Haiku z-ai/glm-5.2               # haiku tier uses NVIDIA, others use Claude account
  .\scripts\run.ps1 -Build                            # build .\bin\serve.exe and run
  .\scripts\run.ps1 -Proxy socks5://127.0.0.1:1080    # proxy for both Chrome captcha and upstream
  .\scripts\run.ps1 -Harness -Batch 6                 # nvpi mode: empty DOM, 1 Chrome borrow = 6 tokens
  .\scripts\run.ps1 -ChampionBudget 4m                # benchmark fastest playground URL
#>
[CmdletBinding()]
param(
  [int]$Port = 8080,
  [string]$Opus,
  [string]$Sonnet,
  [string]$Haiku,
  [string]$Fable,
  [string]$Proxy,
  [int]$PoolSize = 6,
  [int]$PoolWorkers = 3,
  [switch]$Build,
  [switch]$Lan,
  [switch]$NoCaptcha,
  [switch]$Claude,
  [switch]$Harness,
  [int]$Batch = 6,
  [string]$ChampionBudget
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot   # scripts/.. = repo root
Set-Location -Path $root

# Ensure the local model catalog exists (gitignored - holds per-machine keys).
$modelsFile = Join-Path $root 'internal\models\playground_models.json'
if (-not (Test-Path $modelsFile)) {
  Write-Host "playground_models.json missing - running init.ps1..." -ForegroundColor DarkGray
  & (Join-Path $PSScriptRoot 'init.ps1')
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "go is not in PATH"
}

# Check if port is already in use
$busy = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($busy) {
  $owner = (Get-Process -Id $busy[0].OwningProcess -ErrorAction SilentlyContinue).ProcessName
  throw "Port $Port is in use by $owner (PID $($busy[0].OwningProcess)) - terminate it or specify another -Port"
}

# -Claude authenticates with a real gateway token; the old hardcoded literal is
# rejected. Never print the token.
$gatewayToken = $env:GNVIPI_GATEWAY_TOKEN
if ($gatewayToken) { $gatewayToken = $gatewayToken.Trim() }
if ($Claude -and (-not $gatewayToken -or $gatewayToken -eq 'local-gateway')) {
  Write-Host "GNVIPI_GATEWAY_TOKEN is required for -Claude; 'local-gateway' is rejected." -ForegroundColor Red
  Write-Host "Create one with: .\bin\serve.exe admin create-owner --username <owner> --with-gateway-token"
  Write-Host "then set: `$env:GNVIPI_GATEWAY_TOKEN='gw_...'"
  throw "GNVIPI_GATEWAY_TOKEN is not set"
}

# Check PostgreSQL readiness before building or starting when a DSN is configured.
if ($env:GNVIPI_DATABASE_URL) {
  $dbHost, $dbPort = 'localhost', 5432
  try {
    $dbUri = [Uri]$env:GNVIPI_DATABASE_URL
    if ($dbUri.Host) { $dbHost = $dbUri.Host }
    if ($dbUri.Port -gt 0) { $dbPort = $dbUri.Port }
  } catch {
    Write-Host "could not parse GNVIPI_DATABASE_URL; skipping DB readiness check" -ForegroundColor Yellow
  }
  $ready = $false
  $client = New-Object System.Net.Sockets.TcpClient
  try {
    $async = $client.BeginConnect($dbHost, $dbPort, $null, $null)
    if ($async.AsyncWaitHandle.WaitOne(2000)) {
      $client.EndConnect($async)
      $ready = $true
    }
  } catch {
    $ready = $false
  } finally {
    $client.Close()
  }
  if (-not $ready) {
    throw "PostgreSQL is not ready. Start it with: docker compose up -d db"
  }
}

# Configure model environment overrides
if ($Opus)   { $env:MODEL_OPUS = $Opus }     else { Remove-Item Env:MODEL_OPUS   -ErrorAction SilentlyContinue }
if ($Sonnet) { $env:MODEL_SONNET = $Sonnet } else { Remove-Item Env:MODEL_SONNET -ErrorAction SilentlyContinue }
if ($Haiku)  { $env:MODEL_HAIKU = $Haiku }   else { Remove-Item Env:MODEL_HAIKU  -ErrorAction SilentlyContinue }
if ($Fable)  { $env:MODEL_FABLE = $Fable }   else { Remove-Item Env:MODEL_FABLE  -ErrorAction SilentlyContinue }
if ($Proxy)  { $env:CHROME_PROXY = $Proxy }
# Bind loopback like the serve default so the admin console stays usable;
# pass -Lan to expose the gateway on every interface (requires -admin-token
# before /admin opens up).
$bindHost = if ($Lan) { "" } else { "127.0.0.1" }
$serveArgs = @('-addr', "${bindHost}:$Port", '-pool-size', "$PoolSize", '-pool-workers', "$PoolWorkers")
if (-not $NoCaptcha) { $serveArgs = @('-auto') + $serveArgs }
if ($Harness) {
  $serveArgs += '-captcha-harness=true'
  if ($Batch -gt 1) { $serveArgs += @('-pool-batch', "$Batch") }
}
if ($ChampionBudget) { $serveArgs += @('-captcha-select-budget', $ChampionBudget) }

Write-Host ""
Write-Host "  gateway : http://localhost:$Port" -ForegroundColor Green
Write-Host "  models  : http://localhost:$Port/admin   (toggle models, add endpoints)" -ForegroundColor Green
Write-Host "  health  : http://localhost:$Port/healthz"
foreach ($t in 'MODEL_OPUS','MODEL_SONNET','MODEL_HAIKU','MODEL_FABLE') {
  $v = [Environment]::GetEnvironmentVariable($t)
  if ($v) { Write-Host "  $t = $v" -ForegroundColor DarkGray }
}
Write-Host "  Press Ctrl+C to stop"
Write-Host ""

$exe = Join-Path $root 'bin\serve.exe'
$needBuild = $Build -or -not (Test-Path $exe)
if (-not $needBuild) {
  $newestSrc = Get-ChildItem -Recurse -Include *.go -Path (Join-Path $root 'cmd'), (Join-Path $root 'internal') |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if ($newestSrc -and $newestSrc.LastWriteTime -gt (Get-Item $exe).LastWriteTime) { $needBuild = $true }
}
if ($needBuild) {
  Write-Host "building bin\serve.exe..." -ForegroundColor DarkGray
  New-Item -ItemType Directory -Force (Join-Path $root 'bin') | Out-Null
  go build -o $exe ./cmd/serve
  if ($LASTEXITCODE -ne 0) { throw "build failed" }
}
if (-not $Claude) {
  # Run directly in foreground so Ctrl+C terminates properly
  & $exe @serveArgs
  return
}

# Run in background for Claude mode
try {
  $proc = Start-Process -FilePath $exe `
    -ArgumentList $serveArgs `
    -NoNewWindow -PassThru -ErrorAction Stop
} catch {
  Write-Host "serve.exe error: $_ - skipping" -ForegroundColor Yellow
}

# Wait for gateway to become healthy, then launch Claude Code pointing to it
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Date) -lt $deadline) {
  try {
    Invoke-RestMethod "http://localhost:$Port/healthz" -TimeoutSec 2 | Out-Null
    break
  } catch { Start-Sleep -Milliseconds 500 }
}
$env:ANTHROPIC_BASE_URL = "http://localhost:$Port"
$env:ANTHROPIC_AUTH_TOKEN = $gatewayToken
$env:ANTHROPIC_API_KEY = ""
Write-Host "  claude  : base=$env:ANTHROPIC_BASE_URL (skip login)" -ForegroundColor Green
try {
  claude
} catch {
  Write-Host "claude error: $_ - skipping" -ForegroundColor Yellow
}
try { if ($proc -and -not $proc.HasExited) { $proc.Kill() } } catch {}
