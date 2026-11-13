[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$runScript = Join-Path $PSScriptRoot 'run.ps1'
$composeFile = Join-Path $root 'docker-compose.yml'
$failures = [System.Collections.Generic.List[string]]::new()
$checks = 0

function Assert-DevEnvironment {
  param([bool]$Condition, [string]$Message)
  $script:checks++
  if (-not $Condition) { $script:failures.Add($Message) }
}

# Docker is not required for these contract checks. Keep this parser deliberately
# narrow: it validates the db service that `docker compose up -d db` selects.
$compose = Get-Content -Raw -Path $composeFile
$dbMatch = [regex]::Match($compose, '(?ms)^  db:\r?\n(?<body>.*?)(?=^  [A-Za-z0-9_-]+:|^volumes:)')
$dbBlock = $dbMatch.Groups['body'].Value
Assert-DevEnvironment $dbMatch.Success 'docker-compose.yml must define a db service'
Assert-DevEnvironment ($dbBlock -match '(?m)^    image: postgres:17-alpine\s*$') 'db must use postgres:17-alpine'
Assert-DevEnvironment ($dbBlock -match 'pg_isready') 'db must have a pg_isready healthcheck'
Assert-DevEnvironment ($dbBlock -match '(?m)^      - gnvipi_db:/var/lib/postgresql/data\s*$') 'db must mount the named gnvipi_db volume'
Assert-DevEnvironment ($compose -match '(?ms)^volumes:\r?\n  gnvipi_db:\s*$') 'compose must declare the gnvipi_db named volume'
Assert-DevEnvironment ($dbBlock -notmatch '8080') 'the db-only service must not publish or inspect port 8080'
Assert-DevEnvironment ($compose -notmatch 'GNVIPI_MASTER_KEY:\s*["'']?\$\{GNVIPI_MASTER_KEY:\?') 'db-only compose startup must not require a serve-only master key'

$modelsFile = Join-Path $root 'internal\models\playground_models.json'
$modelsDefault = Join-Path $root 'internal\models\playground_models.default.json'
$binDir = Join-Path $root 'bin'
$serveExe = Join-Path $binDir 'serve.exe'
$toolDir = Join-Path ([System.IO.Path]::GetTempPath()) ("gnvipi-run-test-" + [guid]::NewGuid().ToString('N'))
$goOnlyDir = Join-Path ([System.IO.Path]::GetTempPath()) ("gnvipi-run-go-only-" + [guid]::NewGuid().ToString('N'))
$dockerOnlyDir = Join-Path ([System.IO.Path]::GetTempPath()) ("gnvipi-run-docker-only-" + [guid]::NewGuid().ToString('N'))
$goLog = Join-Path $toolDir 'go.log'
$toolLog = Join-Path $toolDir 'tools.log'
$createdModels = -not (Test-Path $modelsFile)
$createdServe = -not (Test-Path $serveExe)
$savedServeTimestamp = if ($createdServe) { $null } else { (Get-Item $serveExe).LastWriteTime }
$createdBin = -not (Test-Path $binDir)
$savedLocation = Get-Location
$envNames = @(
  'PATH', 'GNVIPI_GATEWAY_TOKEN', 'GNVIPI_DATABASE_URL', 'GNVIPI_MASTER_KEY',
  'GNVIPI_DB_PORT', 'GNVIPI_DB_USER', 'GNVIPI_DB_PASSWORD', 'GNVIPI_DB_NAME',
  'PGPASSWORD', 'PGSSLMODE', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY',
  'MODEL_OPUS', 'MODEL_SONNET', 'MODEL_HAIKU', 'MODEL_FABLE', 'GNVIPI_TEST_GO_LOG',
  'GNVIPI_TEST_TOOL_LOG', 'GNVIPI_TEST_PSQL_EXIT', 'GNVIPI_TEST_DOCKER_EXIT',
  'GNVIPI_TEST_PSQL_SPAM'
)
$savedEnv = @{}
foreach ($name in $envNames) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }

$global:GnvipiTestObservedPorts = @()
$global:GnvipiTestClaudeToken = $null
function Get-NetTCPConnection {
  [CmdletBinding()]
  param([int]$LocalPort, [string]$State)
  $global:GnvipiTestObservedPorts += $LocalPort
}

function Start-Process {
  [CmdletBinding()]
  param([string]$FilePath, [object[]]$ArgumentList, [switch]$NoNewWindow, [switch]$PassThru)
  return [pscustomobject]@{ HasExited = $true }
}

function Invoke-RestMethod {
  [CmdletBinding()]
  param([Parameter(Position = 0)][string]$Uri, [int]$TimeoutSec)
  return @{ status = 'ok' }
}

function claude {
  $global:GnvipiTestClaudeToken = $env:ANTHROPIC_AUTH_TOKEN
}

function Clear-RunEnvironment {
  foreach ($name in @(
    'GNVIPI_GATEWAY_TOKEN', 'GNVIPI_DATABASE_URL', 'GNVIPI_MASTER_KEY',
    'GNVIPI_DB_PORT', 'GNVIPI_DB_USER', 'GNVIPI_DB_PASSWORD', 'GNVIPI_DB_NAME',
    'PGPASSWORD', 'PGSSLMODE', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY',
    'GNVIPI_TEST_PSQL_EXIT', 'GNVIPI_TEST_DOCKER_EXIT', 'GNVIPI_TEST_PSQL_SPAM'
  )) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
  $global:GnvipiTestObservedPorts = @()
  $global:GnvipiTestClaudeToken = $null
  Remove-Item $goLog, $toolLog -ErrorAction SilentlyContinue
}

function Invoke-RunScenario {
  param([hashtable]$Parameters)
  $errorMessage = $null
  $records = try {
    & $runScript @Parameters *>&1
  } catch {
    $errorMessage = $_.Exception.Message
    $_
  }
  $output = $records | Out-String
  return [pscustomobject]@{
    Output = $output
    Error = $errorMessage
    Ports = @($global:GnvipiTestObservedPorts)
  }
}

function Get-ClosedLoopbackPort {
  $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
  $listener.Start()
  $port = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
  $listener.Stop()
  return $port
}

try {
  New-Item -ItemType Directory -Force -Path $toolDir, $goOnlyDir, $dockerOnlyDir | Out-Null
  $goStub = "@echo off`r`necho invoked>>`"%GNVIPI_TEST_GO_LOG%`"`r`nexit /b 0"
  Set-Content -Path (Join-Path $toolDir 'go.cmd'), (Join-Path $goOnlyDir 'go.cmd'), (Join-Path $dockerOnlyDir 'go.cmd') -Encoding Ascii -Value $goStub
  $nativeStub = @'
using System;
using System.IO;

public static class NativeToolStub {
    public static int Main(string[] args) {
        string name = Path.GetFileNameWithoutExtension(Environment.GetCommandLineArgs()[0]).ToUpperInvariant();
        string log = Environment.GetEnvironmentVariable("GNVIPI_TEST_TOOL_LOG");
        if (!String.IsNullOrEmpty(log)) {
            File.AppendAllText(log, name + "|" + String.Join("|", args) + Environment.NewLine);
        }
        if (Environment.GetEnvironmentVariable("GNVIPI_TEST_" + name + "_SPAM") == "1") {
            Console.Out.Write(new String('o', 131072));
            Console.Error.Write(new String('e', 131072));
        }
        int exitCode;
        return Int32.TryParse(Environment.GetEnvironmentVariable("GNVIPI_TEST_" + name + "_EXIT"), out exitCode) ? exitCode : 0;
    }
}
'@
  Add-Type -TypeDefinition $nativeStub -OutputAssembly (Join-Path $toolDir 'psql.exe') -OutputType ConsoleApplication
  Copy-Item (Join-Path $toolDir 'psql.exe') (Join-Path $toolDir 'docker.exe')
  Copy-Item (Join-Path $toolDir 'docker.exe') (Join-Path $dockerOnlyDir 'docker.exe')
  $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_TEST_GO_LOG = $goLog
  $env:GNVIPI_TEST_TOOL_LOG = $toolLog
  if ($createdModels) { Copy-Item $modelsDefault $modelsFile }
  if ($createdServe) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $serveExe) | Out-Null
    [System.IO.File]::WriteAllBytes($serveExe, [byte[]]@())
  }
  (Get-Item $serveExe).LastWriteTime = (Get-Date).AddYears(10)

  Clear-RunEnvironment
  $missing = Invoke-RunScenario @{ Claude = $true }
  Assert-DevEnvironment ($missing.Error -match 'GNVIPI_GATEWAY_TOKEN') '-Claude without a token must fail with the token requirement'
  Assert-DevEnvironment ($missing.Output -match 'admin create-owner --username <owner> --with-gateway-token') 'missing-token output must document owner bootstrap'
  Assert-DevEnvironment ($missing.Ports -notcontains 8080) 'missing-token flow must not inspect the default gateway port'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = 'local-gateway'
  $legacy = Invoke-RunScenario @{ Claude = $true; Port = 18081 }
  Assert-DevEnvironment ($legacy.Error -match 'GNVIPI_GATEWAY_TOKEN') '-Claude must reject the legacy local-gateway token'
  Assert-DevEnvironment ($legacy.Output -match "'local-gateway' is rejected") 'legacy-token rejection must be explicit'

  Clear-RunEnvironment
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@db.example.test:6543/app?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_EXIT = '1'
  $env:GNVIPI_TEST_DOCKER_EXIT = '0'
  $dbDown = Invoke-RunScenario @{}
  Assert-DevEnvironment ($dbDown.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'DB-down flow must print the exact compose readiness message'
  Assert-DevEnvironment ($dbDown.Ports -notcontains 8080) 'DB readiness must fail before the default gateway port is inspected'
  Assert-DevEnvironment (-not (Test-Path $goLog)) 'DB readiness must fail before a build is started'
  $dbDownLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($dbDownLog -match '(?m)^PSQL\|') 'a configured DSN must be checked with psql when it is available'
  Assert-DevEnvironment ($dbDownLog -notmatch '(?m)^DOCKER\|') 'Docker presence must not override a psql check of the configured DSN'
  Assert-DevEnvironment ($dbDownLog -match 'PSQL\|--no-password\|-h\|db\.example\.test\|-p\|6543\|-U\|alice\|-d\|app\|') 'psql must check the configured host, port, user, and database without prompting'

  Clear-RunEnvironment
  $secret = 'gw_redaction_probe_7f28d9'
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:PATH = "$goOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
  $listener.Start()
  try {
    $listenerPort = ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    $env:GNVIPI_DATABASE_URL = "postgres://gnvipi_dev:gnvipi_dev_only@127.0.0.1:$listenerPort/gnvipi?sslmode=disable"
    $tcpOnly = Invoke-RunScenario @{ Claude = $true; Port = 18082; NoCaptcha = $true }
  } finally {
    $listener.Stop()
    $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  }
  Assert-DevEnvironment ($tcpOnly.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'an unrelated TCP listener must not count as PostgreSQL readiness'
  Assert-DevEnvironment ($null -eq $global:GnvipiTestClaudeToken) 'Claude must not launch after a TCP-only readiness false positive'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@db.example.test:6543/app?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $env:GNVIPI_TEST_DOCKER_EXIT = '1'
  $env:GNVIPI_TEST_PSQL_SPAM = '1'
  $valid = Invoke-RunScenario @{ Claude = $true; Port = 18081; NoCaptcha = $true }
  Assert-DevEnvironment ($null -eq $valid.Error) 'valid -Claude flow must reach Claude when the configured DB is ready'
  Assert-DevEnvironment ($global:GnvipiTestClaudeToken -eq $secret) '-Claude must pass GNVIPI_GATEWAY_TOKEN as ANTHROPIC_AUTH_TOKEN'
  Assert-DevEnvironment ($valid.Output -notmatch [regex]::Escape($secret)) 'run.ps1 output must never disclose the gateway token'
  Assert-DevEnvironment ($env:ANTHROPIC_BASE_URL -eq 'http://localhost:18081') '-Claude must point Claude at the explicitly selected gateway port'
  Assert-DevEnvironment ($valid.Ports -notcontains 8080) 'an explicit non-default port must not inspect port 8080'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'http://alice:secret@localhost:5432/app'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $wrongScheme = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  Assert-DevEnvironment ($wrongScheme.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'a non-PostgreSQL DSN scheme must fail closed'
  Assert-DevEnvironment (-not (Test-Path $toolLog)) 'an invalid DSN must not launch a database probe'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@localhost:0/app?sslmode=disable'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $zeroPort = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  Assert-DevEnvironment ($zeroPort.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'an explicit database port zero must fail closed'
  Assert-DevEnvironment (-not (Test-Path $toolLog)) 'an invalid database port must not launch a probe'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@[::1]:6543/app?sslmode=disable'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $ipv6 = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  $ipv6Log = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($null -eq $ipv6.Error) 'a valid IPv6 PostgreSQL DSN must reach the configured probe'
  Assert-DevEnvironment ($ipv6Log -match 'PSQL\|--no-password\|-h\|[0-9a-f:]+\|-p\|6543\|' -and $ipv6Log -notmatch '\[') 'IPv6 probe arguments must be normalized without URI brackets'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:PGPASSWORD = 'original-password'
  $env:PGSSLMODE = 'require'
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@localhost:5432/app?sslmode=disable'
  $env:GNVIPI_TEST_PSQL_EXIT = '1'
  $invalidProbeEnv = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  Assert-DevEnvironment ($invalidProbeEnv.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'failed probes must use the readiness message'
  Assert-DevEnvironment ($env:PGPASSWORD -eq 'original-password') 'PGPASSWORD must be restored after probe setup failure'
  Assert-DevEnvironment ($env:PGSSLMODE -eq 'require') 'PGSSLMODE must be restored after probe setup failure'

  Clear-RunEnvironment
  $env:PATH = "$dockerOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $composeListener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
  $composeListener.Start()
  try {
    $composePort = ([System.Net.IPEndPoint]$composeListener.LocalEndpoint).Port
    $env:GNVIPI_DB_PORT = "$composePort"
    $env:GNVIPI_DATABASE_URL = "postgres://gnvipi_dev:gnvipi_dev_only@127.0.0.1:$composePort/gnvipi?sslmode=disable"
    $env:GNVIPI_TEST_DOCKER_EXIT = '0'
    $composeReady = Invoke-RunScenario @{ Claude = $true; Port = 18084; NoCaptcha = $true }
  } finally {
    $composeListener.Stop()
    $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  }
  $composeLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($null -eq $composeReady.Error) 'an authenticated matching local Compose probe plus published TCP endpoint must pass'
  Assert-DevEnvironment ($composeLog -match 'DOCKER\|compose\|exec\|-T\|-e\|PGPASSWORD\|db\|psql\|--no-password\|') 'Docker fallback must run an authenticated psql query, not pg_isready'

  Clear-RunEnvironment
  $env:PATH = "$dockerOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://gnvipi_dev:GNVIPI_DEV_ONLY@localhost:5432/gnvipi?sslmode=disable'
  $env:GNVIPI_TEST_DOCKER_EXIT = '0'
  $caseMismatch = Invoke-RunScenario @{ Claude = $true; Port = 18084; NoCaptcha = $true }
  $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  Assert-DevEnvironment ($caseMismatch.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'case-only Compose credential mismatches must fail closed'
  Assert-DevEnvironment (-not (Test-Path $toolLog)) 'mismatched Compose credentials must not launch Docker'

  Clear-RunEnvironment
  $env:PATH = "$dockerOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
  $env:GNVIPI_TEST_DOCKER_EXIT = '1'
  $composeAuthFail = Invoke-RunScenario @{ Claude = $true; Port = 18084; NoCaptcha = $true }
  $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  Assert-DevEnvironment ($composeAuthFail.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'failed Compose authentication must fail readiness'
} finally {
  Set-Location $savedLocation
  foreach ($name in $envNames) {
    $value = $savedEnv[$name]
    if ($null -eq $value) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
    else { [Environment]::SetEnvironmentVariable($name, $value, 'Process') }
  }
  if ($createdServe) { Remove-Item $serveExe -Force -ErrorAction SilentlyContinue }
  elseif ($null -ne $savedServeTimestamp) { (Get-Item $serveExe).LastWriteTime = $savedServeTimestamp }
  if ($createdModels) { Remove-Item $modelsFile -Force -ErrorAction SilentlyContinue }
  if ($createdBin) { Remove-Item $binDir -Force -ErrorAction SilentlyContinue }
  Remove-Item $toolDir, $goOnlyDir, $dockerOnlyDir -Recurse -Force -ErrorAction SilentlyContinue
  Remove-Variable GnvipiTestObservedPorts, GnvipiTestClaudeToken -Scope Global -ErrorAction SilentlyContinue
}

if ($failures.Count -gt 0) {
  throw (("{0} of {1} dev-environment checks failed:`n - " -f $failures.Count, $checks) + ($failures -join "`n - "))
}
Write-Host ("PASS: {0} dev-environment checks" -f $checks)
