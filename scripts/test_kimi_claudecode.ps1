# Live test kimi-k3 via Claude Code CLI against serve :8080.
# Usage: pwsh scripts/test_kimi_claudecode.ps1
#   -H :8080  override host:port
#   -Model    override model id (default: moonshotai/kimi-k3)
#   -Prompt   override task prompt
#   -Log      override serve log path
param(
    [string]$ServerHost = "localhost",
    [int]$Port   = 8080,
    [string]$Model = "moonshotai/kimi-k3",
    [string]$Prompt = "Find the file internal/provider/nvidia/coalesce.go, read the coalesceSSEEvents function, then read internal/provider/nvidia/coalesce_empty_test.go. Report any TODO/FIXME comments. Be brief.",
    [string]$Log = "C:\Users\Admin\AppData\Local\Temp\serve.log"
)

$ErrorActionPreference = "Stop"

Write-Host "=== environment ===" -ForegroundColor Cyan
Write-Host "Host    : $ServerHost"
Write-Host "Port    : $Port"
Write-Host "Model   : $Model"
Write-Host "Claude  : $((Get-Command claude -ErrorAction SilentlyContinue).Source)"
Write-Host ""

# Verify serve is up
try {
    $health = Invoke-WebRequest -Uri "http://${ServerHost}:${Port}/v1/models" -UseBasicParsing -TimeoutSec 5
    Write-Host "serve :$Port status=$($health.StatusCode)" -ForegroundColor Green
} catch {
    Write-Host "FAIL: serve :$Port not reachable: $_" -ForegroundColor Red
    exit 2
}
Write-Host ""

# Write the prompt to a temp file because shell quoting + Chinese/Vietnamese
# through -Prompt is fragile.
$promptFile = New-TemporaryFile
@"
$Prompt
"@ | Out-File -FilePath $promptFile.FullName -Encoding utf8 -NoNewline

Write-Host "=== prompt ===" -ForegroundColor Cyan
Get-Content $promptFile.FullName
Write-Host ""

# Run Claude Code non-interactive against our local serve.
# BASE_URL + ANTHROPIC_AUTH_TOKEN point Claude Code at the OpenAI-compat surface.
$env:ANTHROPIC_BASE_URL   = "http://${ServerHost}:${Port}"
$env:ANTHROPIC_AUTH_TOKEN = "dummy"
$env:ANTHROPIC_MODEL      = $Model

$stdoutFile = New-TemporaryFile
$stderrFile = New-TemporaryFile

Write-Host "=== claude --print run ===" -ForegroundColor Cyan
$claudeBin = (Get-Command claude -ErrorAction SilentlyContinue).Source
$promptText = Get-Content $promptFile.FullName -Raw
$args = @(
    "--print",
    "--model", $Model,
    "--output-format", "text",
    "--dangerously-skip-permissions",
    $promptText
)
$proc = Start-Process -FilePath "powershell" `
    -ArgumentList @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $claudeBin) + $args `
    -NoNewWindow -Wait -PassThru `
    -RedirectStandardOutput $stdoutFile.FullName `
    -RedirectStandardError  $stderrFile.FullName

$exitCode = $proc.ExitCode
Write-Host "claude exit=$exitCode" -ForegroundColor $(if ($exitCode -eq 0) { "Green" } else { "Red" })
Write-Host ""
Write-Host "=== stdout (last 60 lines) ===" -ForegroundColor Cyan
if (Test-Path $stdoutFile.FullName) {
    Get-Content $stdoutFile.FullName -Tail 60
}
Write-Host ""
if (Test-Path $stderrFile.FullName -and (Get-Item $stderrFile.FullName).Length -gt 0) {
    Write-Host "=== stderr ===" -ForegroundColor Yellow
    Get-Content $stderrFile.FullName -Tail 30
}
Write-Host ""
Write-Host "=== serve log tail ($Log) ===" -ForegroundColor Cyan
if (Test-Path $Log) {
    Get-Content $Log -Tail 30
} else {
    Write-Host "(no log file at $Log)" -ForegroundColor DarkYellow
}

Remove-Item $promptFile.FullName, $stdoutFile.FullName, $stderrFile.FullName -Force -ErrorAction SilentlyContinue

if ($exitCode -ne 0) { exit $exitCode }
