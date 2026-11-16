<#
.SYNOPSIS
  One-time setup: create an empty internal/models/playground_models.json.

  The NVIDIA catalog has no built-in defaults: in database mode it lives in
  PostgreSQL, and in file mode it starts empty until an owner/admin runs
  Rescan on the Models page. The file only holds per-machine preferences and
  custom endpoints.

  No-op if the file already exists - safe to re-run anytime.

.EXAMPLE
  .\init.ps1           # create an empty playground_models.json (if missing)
  .\init.ps1 -Force    # overwrite, resetting masks/providers to defaults
#>
[CmdletBinding()]
param(
  [switch]$Force
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot); Set-Location -Path $root

$target = Join-Path $root 'internal\models\playground_models.json'
if ((Test-Path $target) -and -not $Force) {
  Write-Host "already there, skipping: $target" -ForegroundColor Green
  return
}

$doc = @'
{
  "groups": [],
  "masks_id": [],
  "deleted_id": [],
  "schema_version": 2,
  "providers_v2": []
}
'@
[System.IO.File]::WriteAllText($target, $doc, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "created $target (empty catalog - run Rescan on the Models page)" -ForegroundColor Green
