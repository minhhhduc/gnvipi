<#
.SYNOPSIS
  Start the NVIDIA playground gateway for Claude Code.

.EXAMPLE
  .\run.ps1                                   # :8080, captcha pool on
  .\run.ps1 -Port 8099                        # another port (adjust ANTHROPIC_BASE_URL accordingly)
  .\run.ps1 -Haiku z-ai/glm-5.2               # haiku tier uses NVIDIA, others use Claude account
  .\run.ps1 -Build                            # build .\serve.exe and run
  .\run.ps1 -Proxy socks5://127.0.0.1:1080    # proxy for both Chrome captcha and upstream
  .\run.ps1 -Harness -Batch 6                 # nvpi mode: empty DOM, 1 Chrome borrow = 6 tokens
  .\run.ps1 -ChampionBudget 4m                # benchmark fastest playground URL
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
  [switch]$NoCaptcha,
  [switch]$Claude,
  [switch]$Harness,
  [int]$Batch = 6,
  [string]$ChampionBudget
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "go is not in PATH"
}

# Check if port is already in use
$busy = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($busy) {
  $owner = (Get-Process -Id $busy[0].OwningProcess -ErrorAction SilentlyContinue).ProcessName
  throw "Port $Port is in use by $owner (PID $($busy[0].OwningProcess)) - terminate it or specify another -Port"
}

# Configure model environment overrides
if ($Opus)   { $env:MODEL_OPUS = $Opus }     else { Remove-Item Env:MODEL_OPUS   -ErrorAction SilentlyContinue }
if ($Sonnet) { $env:MODEL_SONNET = $Sonnet } else { Remove-Item Env:MODEL_SONNET -ErrorAction SilentlyContinue }
if ($Haiku)  { $env:MODEL_HAIKU = $Haiku }   else { Remove-Item Env:MODEL_HAIKU  -ErrorAction SilentlyContinue }
if ($Fable)  { $env:MODEL_FABLE = $Fable }   else { Remove-Item Env:MODEL_FABLE  -ErrorAction SilentlyContinue }
if ($Proxy)  { $env:CHROME_PROXY = $Proxy }
$serveArgs = @('-addr', ":$Port", '-pool-size', "$PoolSize", '-pool-workers', "$PoolWorkers")
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

$needBuild = $Build -or -not (Test-Path .\serve.exe)
if (-not $needBuild) {
  $newestSrc = Get-ChildItem -Recurse -Include *.go -Path (Join-Path $PSScriptRoot 'cmd'), (Join-Path $PSScriptRoot 'internal') |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if ($newestSrc -and $newestSrc.LastWriteTime -gt (Get-Item .\serve.exe).LastWriteTime) { $needBuild = $true }
}
if ($needBuild) {
  Write-Host "building serve.exe..." -ForegroundColor DarkGray
  go build -o serve.exe ./cmd/serve
  if ($LASTEXITCODE -ne 0) { throw "build failed" }
}
if (-not $Claude) {
  # Run directly in foreground so Ctrl+C terminates properly
  & .\serve.exe @serveArgs
  return
}

# Run in background for Claude mode
try {
  $proc = Start-Process -FilePath (Join-Path $PSScriptRoot 'serve.exe') `
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
$env:ANTHROPIC_AUTH_TOKEN = "local-gateway"
$env:ANTHROPIC_API_KEY = ""
Write-Host "  claude  : base=$env:ANTHROPIC_BASE_URL (skip login)" -ForegroundColor Green
try {
  claude
} catch {
  Write-Host "claude error: $_ - skipping" -ForegroundColor Yellow
}
try { if ($proc -and -not $proc.HasExited) { $proc.Kill() } } catch {}
