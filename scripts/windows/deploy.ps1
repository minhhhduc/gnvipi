<#
.SYNOPSIS
  Deploy gnvipi on Windows via Docker Compose (db + migrate + serve + optional tunnel).
#>
[CmdletBinding()]
param(
  [switch]$NoBuild,
  [switch]$Down
)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
Set-Location $root

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
  throw 'docker is not installed or not in PATH.'
}
docker info *> $null
if ($LASTEXITCODE -ne 0) {
  throw 'Docker Desktop is not running. Start Docker Desktop and try again.'
}

$envArgs = @()
if (Test-Path (Join-Path $root '.env')) {
  $envArgs = @('--env-file', '.env')
} else {
  Write-Warning ".env not found in $root; using environment defaults."
}

if ($Down) {
  & docker compose @envArgs down
  exit $LASTEXITCODE
}

$upArgs = @('up', '-d')
if (-not $NoBuild) {
  $upArgs += '--build'
}

& docker compose @envArgs @upArgs
if ($LASTEXITCODE -ne 0) {
  exit $LASTEXITCODE
}

$port = if ($env:GNVIPI_PORT) { $env:GNVIPI_PORT } else { '8080' }
Write-Host "Deployed. Local Admin: http://127.0.0.1:$port/admin"
