param(
  [switch]$SkipTests,
  [switch]$RestoreFullSurface,
  [string]$AdminPassword = "CodexVerify123!"
)

$ErrorActionPreference = "Stop"

$Repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$Sidecar = Join-Path $Repo "sidecar"
$E2E = Join-Path $Repo "e2e"
$Binary = Join-Path $Sidecar "pg_sage_verify_sidecar.exe"
$Config = Join-Path $PSScriptRoot "verify_config_walkthrough.yaml"
$FullSurfaceConfig = Join-Path $PSScriptRoot "verify_config_approval.yaml"
$Seed = Join-Path $PSScriptRoot "seed_full_surface.sql"
$StdoutLog = Join-Path $PSScriptRoot "walkthrough_sidecar.out.log"
$StderrLog = Join-Path $PSScriptRoot "walkthrough_sidecar.err.log"

$Meta = @{
  Container = "pg_sage-pg-target-1"
  Database = "pgsage_meta"
  User = "postgres"
}
$Targets = @(
  @{
    Name = "production"
    Container = "pg_sage-pg-target-1"
    Database = "app_production"
    Port = "5433"
    Password = "test"
  },
  @{
    Name = "staging"
    Container = "pg_sage-pg-target-2-1"
    Database = "app_staging"
    Port = "5434"
    Password = "test"
  }
)

function Stop-VerificationSidecar {
  $conn = Get-NetTCPConnection -LocalPort 18085 -State Listen -ErrorAction SilentlyContinue |
    Select-Object -First 1
  if ($conn) {
    Stop-Process -Id $conn.OwningProcess -Force
    Start-Sleep -Seconds 2
  }
}

function Reset-Database($Container, $Database) {
  docker exec $Container dropdb --if-exists --force -U postgres $Database
  docker exec $Container createdb -U postgres $Database
}

function Invoke-PsqlFile($Container, $Database, $Path) {
  $remote = "/tmp/" + [IO.Path]::GetFileName($Path)
  docker cp $Path "$($Container):$remote" | Out-Null
  docker exec $Container psql -v ON_ERROR_STOP=1 -U postgres -d $Database -f $remote
}

Write-Host "Resetting encrypted walkthrough databases..."
Stop-VerificationSidecar
Reset-Database $Meta.Container $Meta.Database
foreach ($target in $Targets) {
  Reset-Database $target.Container $target.Database
  Invoke-PsqlFile $target.Container $target.Database $Seed
}

Write-Host "Building sidecar..."
Push-Location $Sidecar
try {
  $env:GOCACHE = Join-Path $Repo ".gocache"
  go build -o $Binary ./cmd/pg_sage_sidecar
} finally {
  Pop-Location
}

Write-Host "Starting encrypted walkthrough sidecar..."
if (Test-Path $StdoutLog) { Remove-Item -LiteralPath $StdoutLog -Force }
if (Test-Path $StderrLog) { Remove-Item -LiteralPath $StderrLog -Force }
Start-Process -FilePath $Binary `
  -ArgumentList @("-config", $Config) `
  -WorkingDirectory $Sidecar `
  -WindowStyle Hidden `
  -RedirectStandardOutput $StdoutLog `
  -RedirectStandardError $StderrLog
Start-Sleep -Seconds 8

Write-Host "Resetting walkthrough admin password..."
Push-Location $Sidecar
try {
  $env:GOCACHE = Join-Path $Repo ".gocache"
  go run ./cmd/reset_admin_for_test `
    -dsn "postgres://postgres:test@127.0.0.1:5433/pgsage_meta?sslmode=disable" `
    -email admin@pg-sage.local `
    -password $AdminPassword
} finally {
  Pop-Location
}

if (-not $SkipTests) {
  Write-Host "Running 124-check Playwright walkthrough..."
  Push-Location $E2E
  try {
    $env:PG_SAGE_ADMIN_EMAIL = "admin@pg-sage.local"
    $env:PG_SAGE_ADMIN_PASS = $AdminPassword
    $env:PG_SAGE_E2E_BASE_URL = "http://127.0.0.1:18085"
    $env:PG_SAGE_E2E_FIXTURES = "1"
    $env:PG_SAGE_E2E_PROD_PASS = $Targets[0].Password
    $env:PG_SAGE_E2E_STAGING_PASS = $Targets[1].Password
    $env:PG_SAGE_E2E_IMPORT_PASS = $Targets[0].Password
    .\node_modules\.bin\playwright.cmd test walkthrough.spec.ts --workers=1
  } finally {
    Pop-Location
  }
}

if ($RestoreFullSurface) {
  Write-Host "Restoring standard three-database full-surface sidecar..."
  Stop-VerificationSidecar
  powershell -ExecutionPolicy Bypass -File `
    (Join-Path $PSScriptRoot "run_full_surface.ps1") `
    -ConfigPath $FullSurfaceConfig `
    -PollSeconds 40
}
