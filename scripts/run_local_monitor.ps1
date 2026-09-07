$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$launcher = Join-Path $PSScriptRoot "start_local_monitor.py"
$binary = Join-Path $repoRoot "sidecar\pg_sage_sidecar_qa.exe"
$logDir = Join-Path $repoRoot "logs"
$stdoutLog = Join-Path $logDir "local_monitor.current.out.log"
$stderrLog = Join-Path $logDir "local_monitor.current.err.log"

if (-not (Test-Path -LiteralPath $launcher)) {
    throw "Missing local-monitor launcher: $launcher"
}
if (-not (Test-Path -LiteralPath $binary)) {
    throw "Missing local-monitor binary: $binary"
}

$llmKey = [Environment]::GetEnvironmentVariable("LLM_API_KEY", "Process")
if (-not $llmKey) {
    $llmKey = [Environment]::GetEnvironmentVariable("LLM_API_KEY", "User")
}
if (-not $llmKey) {
    $llmKey = [Environment]::GetEnvironmentVariable("GEMINI_API_KEY", "Process")
}
if (-not $llmKey) {
    $llmKey = [Environment]::GetEnvironmentVariable("GEMINI_API_KEY", "User")
}
if (-not $llmKey) {
    throw "LLM_API_KEY or GEMINI_API_KEY is required in the process or user environment"
}

$python = (Get-Command python.exe -ErrorAction Stop).Source
New-Item -ItemType Directory -Path $logDir -Force | Out-Null
$env:LLM_API_KEY = $llmKey

& $python $launcher 1>> $stdoutLog 2>> $stderrLog
exit $LASTEXITCODE
