<#
.SYNOPSIS
  One-time setup: create internal/models/playground_models.json from the
  committed default (the real file is gitignored because it holds local keys).

  No-op if the file already exists — safe to re-run anytime.

.EXAMPLE
  .\init.ps1           # copy default -> playground_models.json (if missing)
  .\init.ps1 -Force    # overwrite, resetting masks/providers to defaults
#>
[CmdletBinding()]
param(
  [switch]$Force
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

$target = Join-Path $PSScriptRoot 'internal\models\playground_models.json'
if ((Test-Path $target) -and -not $Force) {
  Write-Host "already there, skipping: $target" -ForegroundColor Green
  return
}

$default = Join-Path $PSScriptRoot 'internal\models\playground_models.default.json'
if (-not (Test-Path $default)) { throw "missing default file: $default" }

Copy-Item $default $target
$n = (Get-Content $target | ConvertFrom-Json)._meta.count
Write-Host "created $target ($n models) - add custom API keys via /admin if needed" -ForegroundColor Green
