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

# `docker compose up -d` returns before PostgreSQL is healthy, so the documented
# bootstrap must wait for the healthcheck before migrating.
$readme = Get-Content -Raw -Path (Join-Path $root 'README.md')
Assert-DevEnvironment ($readme -match 'docker compose up -d --wait db') 'the README bootstrap must wait for PostgreSQL health before migrating'

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
  'PGHOST', 'PGPORT', 'PGUSER', 'PGDATABASE', 'PGPASSWORD', 'PGSSLMODE',
  'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY',
  'MODEL_OPUS', 'MODEL_SONNET', 'MODEL_HAIKU', 'MODEL_FABLE', 'GNVIPI_TEST_GO_LOG',
  'GNVIPI_TEST_TOOL_LOG', 'GNVIPI_TEST_PSQL_EXIT', 'GNVIPI_TEST_DOCKER_EXIT',
  'GNVIPI_TEST_PSQL_SPAM', 'GNVIPI_TEST_PSQL_HOLD', 'GNVIPI_TEST_CLAUDE_THROW'
)
$savedEnv = @{}
foreach ($name in $envNames) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }

$global:GnvipiTestObservedPorts = @()
$global:GnvipiTestClaudeToken = $null
$global:GnvipiTestClaudeBaseURL = $null
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
  $global:GnvipiTestClaudeBaseURL = $env:ANTHROPIC_BASE_URL
  if ($env:GNVIPI_TEST_CLAUDE_THROW -eq '1') { throw 'claude test failure' }
}

function Clear-RunEnvironment {
  foreach ($name in @(
    'GNVIPI_GATEWAY_TOKEN', 'GNVIPI_DATABASE_URL', 'GNVIPI_MASTER_KEY',
    'GNVIPI_DB_PORT', 'GNVIPI_DB_USER', 'GNVIPI_DB_PASSWORD', 'GNVIPI_DB_NAME',
    'PGHOST', 'PGPORT', 'PGUSER', 'PGDATABASE', 'PGPASSWORD', 'PGSSLMODE',
    'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN', 'ANTHROPIC_API_KEY',
    'GNVIPI_TEST_PSQL_EXIT', 'GNVIPI_TEST_DOCKER_EXIT', 'GNVIPI_TEST_PSQL_SPAM',
    'GNVIPI_TEST_PSQL_HOLD', 'GNVIPI_TEST_CLAUDE_THROW'
  )) { Remove-Item "Env:$name" -ErrorAction SilentlyContinue }
  $global:GnvipiTestObservedPorts = @()
  $global:GnvipiTestClaudeToken = $null
  $global:GnvipiTestClaudeBaseURL = $null
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
using System.Diagnostics;
using System.IO;
using System.Threading;

public static class NativeToolStub {
    public static int Main(string[] args) {
        string name = Path.GetFileNameWithoutExtension(Environment.GetCommandLineArgs()[0]).ToUpperInvariant();
        string log = Environment.GetEnvironmentVariable("GNVIPI_TEST_TOOL_LOG");
        if (!String.IsNullOrEmpty(log)) {
            string probe = "|PGHOST=" + Environment.GetEnvironmentVariable("PGHOST") +
                "|PGPORT=" + Environment.GetEnvironmentVariable("PGPORT") +
                "|PGUSER=" + Environment.GetEnvironmentVariable("PGUSER") +
                "|PGDATABASE=" + Environment.GetEnvironmentVariable("PGDATABASE") +
                "|PGSSLMODE=" + Environment.GetEnvironmentVariable("PGSSLMODE") +
                "|PGPASSWORD_SET=" + (String.IsNullOrEmpty(Environment.GetEnvironmentVariable("PGPASSWORD")) ? "0" : "1");
            File.AppendAllText(log, name + "|" + String.Join("|", args) + probe + Environment.NewLine);
        }
        if (Environment.GetEnvironmentVariable("GNVIPI_TEST_" + name + "_HOLD") == "1") {
            var child = new ProcessStartInfo(Environment.GetEnvironmentVariable("COMSPEC") ?? "cmd.exe", "/c ping -n 30 127.0.0.1");
            child.UseShellExecute = false;
            Process.Start(child);
            Thread.Sleep(30000);
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
  Assert-DevEnvironment ($dbDownLog -match 'PGHOST=db\.example\.test\|PGPORT=6543\|PGUSER=alice\|PGDATABASE=app\|PGSSLMODE=verify-full') 'psql must check the configured host, port, user, database, and TLS mode without prompting'

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
  $env:ANTHROPIC_BASE_URL = 'https://original.example'
  $env:ANTHROPIC_AUTH_TOKEN = 'original-token'
  $env:ANTHROPIC_API_KEY = 'original-key'
  $valid = Invoke-RunScenario @{ Claude = $true; Port = 18081; NoCaptcha = $true }
  Assert-DevEnvironment ($null -eq $valid.Error) 'valid -Claude flow must reach Claude when the configured DB is ready'
  Assert-DevEnvironment ($global:GnvipiTestClaudeToken -eq $secret) '-Claude must pass GNVIPI_GATEWAY_TOKEN as ANTHROPIC_AUTH_TOKEN'
  Assert-DevEnvironment ($global:GnvipiTestClaudeBaseURL -eq 'http://localhost:18081') '-Claude must point Claude at the explicitly selected gateway port'
  Assert-DevEnvironment ($valid.Output -notmatch [regex]::Escape($secret)) 'run.ps1 output must never disclose the gateway token'
  Assert-DevEnvironment ($env:ANTHROPIC_BASE_URL -eq 'https://original.example' -and $env:ANTHROPIC_AUTH_TOKEN -eq 'original-token' -and $env:ANTHROPIC_API_KEY -eq 'original-key') 'Claude environment variables must be restored after launch'
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
  Assert-DevEnvironment ($ipv6Log -match 'PGHOST=[0-9a-f:]+\|PGPORT=6543\|' -and $ipv6Log -notmatch '\[') 'IPv6 probe environment must be normalized without URI brackets'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice%40tenant:secret@db.example.test:6543/my%20db?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $encoded = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  $encodedLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($null -eq $encoded.Error) 'percent-encoded PostgreSQL user and database names must be probed'
  Assert-DevEnvironment ($encodedLog -match 'PGUSER=alice@tenant\|PGDATABASE=my db\|') 'PostgreSQL user and database names must be URI-decoded'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://db.example.test:6543?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $omitted = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  $omittedLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($null -eq $omitted.Error) 'PostgreSQL URLs with omitted user and database must use client defaults'
  Assert-DevEnvironment ($omittedLog -match 'PGUSER=\|PGDATABASE=\|' -and $omittedLog -notmatch '\|-(?:U|d)\|') 'omitted PostgreSQL user and database must not receive Compose defaults'

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
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:PGPASSWORD = 'inherited-secret'
  $env:PGSSLMODE = 'require'
  $env:GNVIPI_DATABASE_URL = 'postgres://alice@db.example.test:6543/app'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $inherited = Invoke-RunScenario @{ Claude = $true; Port = 18083; NoCaptcha = $true }
  $inheritedLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($null -eq $inherited.Error) 'a DSN without a password or TLS mode must use the inherited client environment'
  Assert-DevEnvironment ($inheritedLog -match 'PGPASSWORD_SET=1') 'a DSN without a password must keep the inherited PGPASSWORD'
  Assert-DevEnvironment ($inheritedLog -match 'PGSSLMODE=require') 'a DSN without sslmode must keep the inherited PGSSLMODE'

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
  Assert-DevEnvironment ($composeLog -match 'DOCKER\|compose\|exec\|-T\|-e\|PGPASSWORD\|-e\|PGSSLMODE\|db\|psql\|--no-password\|') 'Docker fallback must run an authenticated psql query, not pg_isready'

  Clear-RunEnvironment
  $env:PATH = "$dockerOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $closedPort = Get-ClosedLoopbackPort
  $env:GNVIPI_DB_PORT = "$closedPort"
  $env:GNVIPI_DATABASE_URL = "postgres://gnvipi_dev:gnvipi_dev_only@127.0.0.1:$closedPort/gnvipi?sslmode=disable"
  $env:GNVIPI_TEST_DOCKER_EXIT = '0'
  $unpublished = Invoke-RunScenario @{ Claude = $true; Port = 18087; NoCaptcha = $true }
  $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  Assert-DevEnvironment ($unpublished.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'a Compose database with an unreachable published endpoint must fail readiness'
  Assert-DevEnvironment (-not (Test-Path $goLog)) 'a failed published-endpoint check must fail before a build is started'

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

  Clear-RunEnvironment
  $env:PATH = "$dockerOnlyDir;$env:SystemRoot\System32;$env:SystemRoot"
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=require'
  $env:GNVIPI_TEST_DOCKER_EXIT = '1'
  $composeTLS = Invoke-RunScenario @{ Claude = $true; Port = 18084; NoCaptcha = $true }
  $env:PATH = "$toolDir;$env:SystemRoot\System32;$env:SystemRoot"
  $composeTLSLog = if (Test-Path $toolLog) { Get-Content -Raw $toolLog } else { '' }
  Assert-DevEnvironment ($composeTLS.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'incompatible Compose TLS mode must fail readiness'
  Assert-DevEnvironment ($composeTLSLog -match '-e\|PGPASSWORD\|-e\|PGSSLMODE\|db\|psql' -and $composeTLSLog -match 'PGSSLMODE=require') 'Docker fallback must forward the configured TLS mode to psql'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@db.example.test:6543/app?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_HOLD = '1'
  $holdStarted = Get-Date
  $heldProbe = Invoke-RunScenario @{ Claude = $true; Port = 18085; NoCaptcha = $true }
  $holdElapsed = ((Get-Date) - $holdStarted).TotalSeconds
  Assert-DevEnvironment ($heldProbe.Error -eq 'PostgreSQL is not ready. Start it with: docker compose up -d db') 'timed-out database probes must fail readiness'
  Assert-DevEnvironment ($holdElapsed -lt 12) 'timed-out probes and descendants must be terminated within a bounded cleanup window'

  Clear-RunEnvironment
  $env:GNVIPI_GATEWAY_TOKEN = $secret
  $env:GNVIPI_DATABASE_URL = 'postgres://alice:secret@db.example.test:6543/app?sslmode=verify-full'
  $env:GNVIPI_TEST_PSQL_EXIT = '0'
  $env:GNVIPI_TEST_CLAUDE_THROW = '1'
  $env:ANTHROPIC_BASE_URL = 'https://original.example'
  $env:ANTHROPIC_AUTH_TOKEN = 'original-token'
  $env:ANTHROPIC_API_KEY = 'original-key'
  $claudeFailure = Invoke-RunScenario @{ Claude = $true; Port = 18086; NoCaptcha = $true }
  Assert-DevEnvironment ($null -eq $claudeFailure.Error) 'Claude command failures must be handled by run.ps1'
  Assert-DevEnvironment ($env:ANTHROPIC_BASE_URL -eq 'https://original.example' -and $env:ANTHROPIC_AUTH_TOKEN -eq 'original-token' -and $env:ANTHROPIC_API_KEY -eq 'original-key') 'Claude environment variables must be restored after command failure'
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
  Remove-Variable GnvipiTestObservedPorts, GnvipiTestClaudeToken, GnvipiTestClaudeBaseURL -Scope Global -ErrorAction SilentlyContinue
}

if ($failures.Count -gt 0) {
  throw (("{0} of {1} dev-environment checks failed:`n - " -f $failures.Count, $checks) + ($failures -join "`n - "))
}
Write-Host ("PASS: {0} dev-environment checks" -f $checks)
