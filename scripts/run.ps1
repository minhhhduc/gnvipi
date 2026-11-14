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

# Terminate a Windows process tree without an unbounded wait. taskkill handles
# descendants (for example Docker plugins); Process.Kill is the fallback.
function Stop-ProcessTreeBounded {
  param($Process, [int]$TimeoutMs = 2000)
  if (-not $Process) { return }
  $taskkill = Join-Path $env:SystemRoot 'System32\taskkill.exe'
  try {
    if (Test-Path $taskkill) {
      $killInfo = New-Object System.Diagnostics.ProcessStartInfo
      $killInfo.FileName = $taskkill
      $killInfo.Arguments = "/PID $($Process.Id) /T /F"
      $killInfo.UseShellExecute = $false
      $killInfo.CreateNoWindow = $true
      $killInfo.RedirectStandardOutput = $true
      $killInfo.RedirectStandardError = $true
      $killer = [System.Diagnostics.Process]::Start($killInfo)
      try {
        if (-not $killer.WaitForExit($TimeoutMs)) { try { $killer.Kill() } catch {} }
      } finally {
        $killer.Dispose()
      }
    }
  } catch {}
  try { if (-not $Process.HasExited) { $Process.Kill() } } catch {}
  $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
  while ((Get-Date) -lt $deadline) {
    try { if ($Process.HasExited) { break } } catch { break }
    Start-Sleep -Milliseconds 50
  }
}

# Run a command with a bounded timeout and suppressed output. Returns $true on
# exit code 0. Output is drained to prevent verbose children from blocking, but
# never echoed because it may contain connection details.
function Invoke-Bounded {
  param([string]$FilePath, [string[]]$ArgumentList, [int]$TimeoutMs = 5000)
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $FilePath
  $psi.Arguments = ($ArgumentList | ForEach-Object {
    if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ }
  }) -join ' '
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  $proc = $null
  $needsStop = $true
  try {
    $proc = [System.Diagnostics.Process]::Start($psi)
    $stdout = $proc.StandardOutput.ReadToEndAsync()
    $stderr = $proc.StandardError.ReadToEndAsync()
    if (-not $proc.WaitForExit($TimeoutMs)) { return $false }
    if (-not $stdout.Wait(1000) -or -not $stderr.Wait(1000)) { return $false }
    $needsStop = $false
    return $proc.ExitCode -eq 0
  } catch {
    return $false
  } finally {
    if ($proc) {
      if ($needsStop) { Stop-ProcessTreeBounded $proc }
      $proc.Dispose()
    }
  }
}

# PostgreSQL readiness. DB mode is expected when a DSN or master key is set;
# file-backed mode skips the check. psql validates the configured target. When
# psql is absent, Docker is safe only for the matching local Compose database;
# a bare TCP listener is never treated as PostgreSQL readiness.
$dbUrl = $env:GNVIPI_DATABASE_URL
$expectDb = [bool]$dbUrl -or [bool]$env:GNVIPI_MASTER_KEY
if ($expectDb) {
  if (-not $dbUrl) {
    $dbUrl = 'postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
  }
  $dbHost, $dbPort, $dbUser, $dbName, $dbPassword, $dbSslMode = 'localhost', 5432, $null, $null, '', ''
  $dbConfigValid = $true
  try {
    $dsnMatch = [regex]::Match($dbUrl, '^(?i:postgres(?:ql)?)://(?:[^@/?#]*@)?(?:\[[^\]]+\]|[^:/?#]+)(?::(?<port>[0-9]+))?(?:[/?#]|$)')
    if (-not $dsnMatch.Success) { throw 'invalid PostgreSQL URL' }
    $dbUri = [Uri]$dbUrl
    if (-not $dbUri.IsAbsoluteUri -or $dbUri.Scheme -notin @('postgres', 'postgresql') -or -not $dbUri.DnsSafeHost) {
      throw 'invalid PostgreSQL URL'
    }
    $dbHost = $dbUri.DnsSafeHost
    if ($dsnMatch.Groups['port'].Success) {
      $parsedDbPort = 0
      if (-not [int]::TryParse($dsnMatch.Groups['port'].Value, [ref]$parsedDbPort)) {
        throw 'invalid PostgreSQL port'
      }
      $dbPort = $parsedDbPort
    } else {
      $dbPort = 5432
    }
    if ($dbUri.UserInfo) {
      $parts = $dbUri.UserInfo -split ':', 2
      if ($parts[0]) { $dbUser = [Uri]::UnescapeDataString($parts[0]) }
      if ($parts.Count -gt 1) { $dbPassword = [Uri]::UnescapeDataString($parts[1]) }
    }
    $name = $dbUri.AbsolutePath.Trim('/')
    if ($name) { $dbName = [Uri]::UnescapeDataString($name) }
    if ($dbUri.Query -match '(?:^\?|&)sslmode=([^&]+)') {
      $dbSslMode = [Uri]::UnescapeDataString($Matches[1])
    }
  } catch {
    $dbConfigValid = $false
    Write-Host "could not parse GNVIPI_DATABASE_URL" -ForegroundColor Yellow
  }
  if ($dbPort -lt 1 -or $dbPort -gt 65535) { $dbConfigValid = $false }

  $dbReady = $false
  $psql = Get-Command psql -ErrorAction SilentlyContinue
  if ($dbConfigValid -and $psql) {
    # Connection values travel in the child environment, never in process
    # arguments. --no-password prevents an interactive prompt.
    $probeNames = @('PGHOST', 'PGPORT', 'PGUSER', 'PGDATABASE', 'PGPASSWORD', 'PGSSLMODE')
    $oldProbeEnv = @{}
    foreach ($name in $probeNames) {
      $oldProbeEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    try {
      $env:PGHOST = $dbHost
      $env:PGPORT = "$dbPort"
      # Values the DSN omits keep their libpq environment defaults (for example
      # PGPASSWORD or PGSSLMODE) instead of being scrubbed.
      if ($null -ne $dbUser) { $env:PGUSER = $dbUser }
      if ($null -ne $dbName) { $env:PGDATABASE = $dbName }
      if ($dbPassword) { $env:PGPASSWORD = $dbPassword }
      if ($dbSslMode) { $env:PGSSLMODE = $dbSslMode }
      $dbReady = Invoke-Bounded $psql.Source @('--no-password', '-tAc', 'select 1')
    } catch {
      $dbReady = $false
    } finally {
      foreach ($name in $probeNames) {
        if ($null -eq $oldProbeEnv[$name]) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
        else { [Environment]::SetEnvironmentVariable($name, $oldProbeEnv[$name], 'Process') }
      }
    }
  } elseif ($dbConfigValid) {
    $composePort = 5432
    if ($env:GNVIPI_DB_PORT) {
      $parsedComposePort = 0
      if ([int]::TryParse($env:GNVIPI_DB_PORT, [ref]$parsedComposePort)) { $composePort = $parsedComposePort }
      else { $composePort = 0 }
    }
    $composeUser = if ($env:GNVIPI_DB_USER) { $env:GNVIPI_DB_USER } else { 'gnvipi_dev' }
    $composePassword = if ($env:GNVIPI_DB_PASSWORD) { $env:GNVIPI_DB_PASSWORD } else { 'gnvipi_dev_only' }
    $composeName = if ($env:GNVIPI_DB_NAME) { $env:GNVIPI_DB_NAME } else { 'gnvipi' }
    $localHost = $dbHost -in @('localhost', '127.0.0.1', '::1')
    $matchesCompose = $localHost -and $dbPort -eq $composePort -and
      $dbUser -ceq $composeUser -and $dbPassword -ceq $composePassword -and $dbName -ceq $composeName
    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if ($matchesCompose -and $docker) {
      $oldPgPassword = [Environment]::GetEnvironmentVariable('PGPASSWORD', 'Process')
      $oldPgSslMode = [Environment]::GetEnvironmentVariable('PGSSLMODE', 'Process')
      try {
        $env:PGPASSWORD = $dbPassword
        if ($dbSslMode) { $env:PGSSLMODE = $dbSslMode }
        else { Remove-Item Env:PGSSLMODE -ErrorAction SilentlyContinue }
        $dockerArgs = @('compose', 'exec', '-T', '-e', 'PGPASSWORD')
        if ($dbSslMode) { $dockerArgs += @('-e', 'PGSSLMODE') }
        $dockerArgs += @('db', 'psql', '--no-password', '-h', '127.0.0.1', '-p', '5432', '-U', $dbUser, '-d', $dbName, '-tAc', 'select 1')
        $dbReady = Invoke-Bounded $docker.Source $dockerArgs
      } catch {
        $dbReady = $false
      } finally {
        if ($null -eq $oldPgPassword) { Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue }
        else { $env:PGPASSWORD = $oldPgPassword }
        if ($null -eq $oldPgSslMode) { Remove-Item Env:PGSSLMODE -ErrorAction SilentlyContinue }
        else { $env:PGSSLMODE = $oldPgSslMode }
      }
      if ($dbReady) {
        $client = New-Object System.Net.Sockets.TcpClient
        try {
          $async = $client.BeginConnect($dbHost, $dbPort, $null, $null)
          $dbReady = $async.AsyncWaitHandle.WaitOne(2000)
          if ($dbReady) { $client.EndConnect($async) }
        } catch {
          $dbReady = $false
        } finally {
          $client.Close()
        }
      }
    } else {
      Write-Host "psql is required to check the configured PostgreSQL target" -ForegroundColor Yellow
    }
  }
  if (-not $dbReady) {
    throw "PostgreSQL is not ready. Start it with: docker compose up -d db"
  }
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
  # The binary embeds web/admin/{index.html,app.js,app.css}, so their mtimes
  # must trigger a rebuild too, not just Go sources.
  $sources = @(Get-ChildItem -Recurse -Include *.go -Path (Join-Path $root 'cmd'), (Join-Path $root 'internal'))
  $sources += Get-ChildItem -File -Path (Join-Path $root 'web\admin') |
    Where-Object { $_.Extension -in '.go', '.html', '.js', '.css' }
  $newestSrc = $sources | Sort-Object LastWriteTime -Descending | Select-Object -First 1
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

# Run in background for Claude mode and restore the caller's environment even
# when Claude or the gateway exits abnormally.
$proc = $null
$claudeEnvNames = @('ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY')
$oldClaudeEnv = @{}
foreach ($name in $claudeEnvNames) {
  $oldClaudeEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
try {
  try {
    $proc = Start-Process -FilePath $exe `
      -ArgumentList $serveArgs `
      -NoNewWindow -PassThru -ErrorAction Stop
  } catch {
    Write-Host "serve.exe error: $_ - skipping" -ForegroundColor Yellow
    return
  }

  # Wait for gateway to become healthy, then launch Claude Code pointing to it.
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
} finally {
  foreach ($name in $claudeEnvNames) {
    if ($null -eq $oldClaudeEnv[$name]) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
    else { [Environment]::SetEnvironmentVariable($name, $oldClaudeEnv[$name], 'Process') }
  }
  if ($proc) {
    try { if (-not $proc.HasExited) { Stop-ProcessTreeBounded $proc } } catch {}
    if ($proc -is [System.Diagnostics.Process]) { $proc.Dispose() }
  }
}
