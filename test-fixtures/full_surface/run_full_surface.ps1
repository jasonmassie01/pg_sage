param(
  [switch]$NoStartSidecar,
  [switch]$PreserveSageState,
  [switch]$SkipActionLifecycle,
  [int]$PollSeconds = 90,
  [string]$ConfigPath = ""
)

$ErrorActionPreference = "Stop"

$Repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$Sidecar = Join-Path $Repo "sidecar"
$Binary = Join-Path $Sidecar "pg_sage_verify_sidecar.exe"
$Config = if ($ConfigPath) {
  (Resolve-Path -LiteralPath $ConfigPath).Path
} else {
  Join-Path $PSScriptRoot "verify_config.yaml"
}
$Reset = Join-Path $PSScriptRoot "reset_sage_state.sql"
$Seed = Join-Path $PSScriptRoot "seed_full_surface.sql"
$Workload = Join-Path $PSScriptRoot "workload_full_surface.sql"
$StdoutLog = Join-Path $PSScriptRoot "verify_sidecar.out.log"
$StderrLog = Join-Path $PSScriptRoot "verify_sidecar.err.log"

$Targets = @(
  @{ Name = "testdb"; Container = "pg_sage-pg-target-1"; Database = "testdb"; User = "postgres" },
  @{ Name = "testdb2"; Container = "pg_sage-pg-target-2-1"; Database = "testdb2"; User = "postgres" },
  @{ Name = "health_test"; Container = "health_pg"; Database = "health_test"; User = "postgres" }
)

function Invoke-PsqlFile($Target, $Path) {
  $remote = "/tmp/" + [IO.Path]::GetFileName($Path)
  docker cp $Path "$($Target.Container):$remote" | Out-Null
  docker exec $Target.Container psql -v ON_ERROR_STOP=1 -U $Target.User -d $Target.Database -f $remote
}

function Invoke-PsqlText($Target, $Sql) {
  docker exec $Target.Container psql -v ON_ERROR_STOP=1 -U $Target.User -d $Target.Database -Atc $Sql
}

function Get-MissingExpectedCases($Target) {
  $expectedCategories = @(
    "duplicate_index",
    "high_total_time",
    "invalid_index",
    "missing_fk_index",
    "query_tuning",
    "sequence_exhaustion",
    "slow_query",
    "sort_without_index",
    "table_bloat",
    "unused_index",
    "xid_wraparound"
  )
  $categoryValues = ($expectedCategories | ForEach-Object { "('$_')" }) -join ","
  $missingCategories = Invoke-PsqlText $Target @"
WITH expected(category) AS (VALUES $categoryValues)
SELECT e.category
  FROM expected e
 WHERE NOT EXISTS (
       SELECT 1 FROM sage.findings f
        WHERE f.status = 'open'
          AND f.category = e.category
 );
"@

  $expectedRuleIds = @(
    "lint_char_usage",
    "lint_fk_type_mismatch",
    "lint_jsonb_in_joins",
    "lint_low_cardinality_index",
    "lint_no_primary_key",
    "lint_nullable_unique",
    "lint_overlapping_index",
    "lint_sequence_overflow",
    "lint_serial_usage",
    "lint_timestamp_no_tz",
    "lint_varchar_255",
    "lint_wide_table"
  )
  $ruleValues = ($expectedRuleIds | ForEach-Object { "('$_')" }) -join ","
  $missingRuleIds = Invoke-PsqlText $Target @"
WITH expected(rule_id) AS (VALUES $ruleValues)
SELECT e.rule_id
  FROM expected e
 WHERE NOT EXISTS (
       SELECT 1 FROM sage.findings f
        WHERE f.status = 'open'
          AND f.rule_id = e.rule_id
 );
"@

  $hintCount = Invoke-PsqlText $Target @"
SELECT count(*) FROM sage.query_hints WHERE status = 'active';
"@

  $missing = @()
  if ($missingCategories) {
    $missing += $missingCategories | ForEach-Object { "category:$($_)" }
  }
  if ($missingRuleIds) {
    $missing += $missingRuleIds | ForEach-Object { "rule_id:$($_)" }
  }
  if ([int]$hintCount -lt 1) {
    $missing += "active_query_hint"
  }
  return $missing
}

function Wait-ExpectedCases($Target, $TimeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $lastMissing = @()
  do {
    $lastMissing = @(Get-MissingExpectedCases $Target)
    if ($lastMissing.Count -eq 0) {
      Write-Host "  expected cases present for $($Target.Name)"
      return
    }
    Start-Sleep -Seconds 5
  } while ((Get-Date) -lt $deadline)

  throw "Full-surface verification failed for $($Target.Name): missing $($lastMissing -join ', ')"
}

function Invoke-VerificationApi($Method, $Path, $Body = $null) {
  $params = @{
    UseBasicParsing = $true
    Uri = "http://127.0.0.1:18085$Path"
    Method = $Method
    WebSession = $session
  }
  if ($null -ne $Body) {
    $params.ContentType = "application/json"
    $params.Body = ($Body | ConvertTo-Json -Depth 10)
  }
  $response = Invoke-WebRequest @params
  if (-not $response.Content) {
    return $null
  }
  return $response.Content | ConvertFrom-Json
}

function Get-PendingActions {
  $result = Invoke-VerificationApi "Get" "/api/v1/actions/pending"
  return @($result.pending)
}

function Wait-PendingActionsForFleet($TimeoutSeconds) {
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  do {
    $pending = @(Get-PendingActions)
    $missing = @()
    foreach ($target in $Targets) {
      $dbPending = @($pending | Where-Object { $_.database_name -eq $target.Name })
      if ($dbPending.Count -eq 0) {
        $missing += $target.Name
      }
    }
    if ($pending.Count -gt 0 -and $missing.Count -eq 0) {
      return $pending
    }
    Start-Sleep -Seconds 5
  } while ((Get-Date) -lt $deadline)

  throw "Expected pending action rows for every fleet database; missing $($missing -join ', ')"
}

function Assert-ExecutedAction($DatabaseName, $ActionLogID, $FindingID, $ExpectedSqlFragment) {
  $db = [Uri]::EscapeDataString($DatabaseName)
  $actions = Invoke-VerificationApi "Get" "/api/v1/actions?database=$db&limit=100"
  $action = @($actions.actions) | Where-Object {
    [int64]$_.id -eq [int64]$ActionLogID
  } | Select-Object -First 1
  if (-not $action) {
    throw "Executed action log $ActionLogID was not returned for $DatabaseName"
  }
  if ([int64]$action.finding_id -ne [int64]$FindingID) {
    throw "Executed action log $ActionLogID finding mismatch: got $($action.finding_id), want $FindingID"
  }
  if ($action.outcome -ne "success") {
    throw "Executed action log $ActionLogID outcome mismatch: got $($action.outcome), want success"
  }
  if (-not $action.sql_executed.Contains($ExpectedSqlFragment)) {
    throw "Executed action log $ActionLogID SQL did not include expected fragment '$ExpectedSqlFragment'"
  }
}

function Assert-ResolvedFinding($DatabaseName, $FindingID, $ActionLogID) {
  $db = [Uri]::EscapeDataString($DatabaseName)
  $resolved = Invoke-VerificationApi "Get" "/api/v1/findings?status=resolved&database=$db&limit=200"
  $finding = @($resolved.findings) | Where-Object {
    [int64]$_.id -eq [int64]$FindingID
  } | Select-Object -First 1
  if (-not $finding) {
    throw "Finding $FindingID was not returned as resolved for $DatabaseName"
  }
  if ([int64]$finding.action_log_id -ne [int64]$ActionLogID) {
    throw "Resolved finding $FindingID action_log_id mismatch: got $($finding.action_log_id), want $ActionLogID"
  }
}

function Assert-NoChildBigintIndexNoise($DatabaseName) {
  $db = [Uri]::EscapeDataString($DatabaseName)
  $open = Invoke-VerificationApi "Get" "/api/v1/findings?status=open&database=$db&limit=200"
  $residual = @($open.findings) | Where-Object {
    ($_.category -eq "missing_fk_index" -or $_.category -eq "duplicate_index") -and
    (($_.category + " " + $_.title + " " + $_.object_identifier + " " + ($_.detail | ConvertTo-Json -Compress -Depth 8)).ToLower().Contains("child_bigint_fk"))
  }
  if ($residual.Count -gt 0) {
    $ids = ($residual | ForEach-Object { "$($_.id):$($_.category)" }) -join ", "
    throw "Approved child_bigint_fk create-index action reappeared as open index noise for ${DatabaseName}: $ids"
  }
}

function Assert-ActionLifecycle($TimeoutSeconds) {
  Write-Host ""
  Write-Host "Action lifecycle assertions:"
  $pending = @(Wait-PendingActionsForFleet $TimeoutSeconds)
  Write-Host "  pending rows present across fleet: $($pending.Count)"

  foreach ($target in $Targets) {
    $create = @($pending | Where-Object {
      $_.database_name -eq $target.Name -and
      $_.proposed_sql.Contains("CREATE INDEX CONCURRENTLY ON sage_verify.child_bigint_fk")
    }) | Select-Object -First 1
    if (-not $create) {
      throw "Expected pending child_bigint_fk create-index action for $($target.Name)"
    }

    $db = [Uri]::EscapeDataString($create.database_name)
    $approve = Invoke-VerificationApi "Post" "/api/v1/actions/$($create.id)/approve?database=$db" @{}
    if (-not $approve.ok -or -not $approve.executed -or [int64]$approve.action_log_id -le 0) {
      throw "Approve action $($create.id) for $($target.Name) did not execute successfully: $($approve | ConvertTo-Json -Compress)"
    }
    Assert-ExecutedAction $create.database_name $approve.action_log_id $create.finding_id "CREATE INDEX CONCURRENTLY ON sage_verify.child_bigint_fk"
    Assert-ResolvedFinding $create.database_name $create.finding_id $approve.action_log_id
    Write-Host "  approved create-index action $($create.id) for $($target.Name); action_log_id=$($approve.action_log_id)"
  }

  foreach ($target in $Targets) {
    $pending = @(Get-PendingActions)
    $reject = @($pending | Where-Object {
      $_.database_name -eq $target.Name -and
      $_.proposed_sql.StartsWith("DROP INDEX CONCURRENTLY")
    }) | Select-Object -First 1
    if (-not $reject) {
      throw "Expected pending rejectable DROP INDEX action for $($target.Name)"
    }

    $db = [Uri]::EscapeDataString($reject.database_name)
    $rejectResult = Invoke-VerificationApi "Post" "/api/v1/actions/$($reject.id)/reject?database=$db" @{
      reason = "full-surface verification reject"
    }
    if (-not $rejectResult.ok -or $rejectResult.status -ne "rejected") {
      throw "Reject action $($reject.id) for $($target.Name) did not succeed: $($rejectResult | ConvertTo-Json -Compress)"
    }
    $afterReject = @(Get-PendingActions)
    $stillPending = @($afterReject | Where-Object {
      [int64]$_.id -eq [int64]$reject.id -and $_.database_name -eq $reject.database_name
    })
    if ($stillPending.Count -gt 0) {
      throw "Rejected action $($reject.id) for $($target.Name) still appears in pending queue"
    }
    Write-Host "  rejected pending action $($reject.id) for $($target.Name)"
  }

  Start-Sleep -Seconds 12
  foreach ($target in $Targets) {
    Assert-NoChildBigintIndexNoise $target.Name
    Write-Host "  no child_bigint_fk missing/duplicate index noise after refresh for $($target.Name)"
  }
}

Write-Host "Applying full-surface schema fixtures..."
foreach ($target in $Targets) {
  if (-not $PreserveSageState) {
    Write-Host "  reset sage state $($target.Name)"
    Invoke-PsqlFile $target $Reset
  }
  Write-Host "  seed $($target.Name)"
  Invoke-PsqlFile $target $Seed
}

if (-not $NoStartSidecar) {
  Write-Host "Building verification sidecar..."
  Push-Location $Sidecar
  try {
    $env:GOCACHE = Join-Path $Repo ".gocache"
    go build -o $Binary ./cmd/pg_sage_sidecar
  } finally {
    Pop-Location
  }

  $existing = Get-NetTCPConnection -LocalPort 18085 -State Listen -ErrorAction SilentlyContinue
  if (-not $existing) {
    Write-Host "Starting verification sidecar on 127.0.0.1:18085..."
    if (Test-Path $StdoutLog) { Remove-Item -LiteralPath $StdoutLog -Force }
    if (Test-Path $StderrLog) { Remove-Item -LiteralPath $StderrLog -Force }
    Start-Process -FilePath $Binary -ArgumentList @("-config", $Config) -WorkingDirectory $Sidecar -WindowStyle Hidden -RedirectStandardOutput $StdoutLog -RedirectStandardError $StderrLog
    Start-Sleep -Seconds 8
  } else {
    Write-Host "Verification sidecar already listening on 18085."
  }
}

Write-Host "Resetting verification admin password..."
Push-Location $Sidecar
try {
  $env:GOCACHE = Join-Path $Repo ".gocache"
  go run ./cmd/reset_admin_for_test -dsn "postgres://postgres:test@127.0.0.1:5433/testdb?sslmode=disable" -email admin@pg-sage.local -password "CodexVerify123!"
} finally {
  Pop-Location
}

Write-Host "Applying query workload and explain-cache seeds..."
foreach ($target in $Targets) {
  Write-Host "  workload $($target.Name)"
  Invoke-PsqlFile $target $Workload
}

Write-Host "Polling for analyzer/linter output for $PollSeconds seconds..."
Start-Sleep -Seconds ([Math]::Min(5, $PollSeconds))

Write-Host ""
Write-Host "Finding summary by database:"
foreach ($target in $Targets) {
  Write-Host "[$($target.Name)]"
  Invoke-PsqlText $target @"
SELECT category || '|' || status || '|' || count(*)
  FROM sage.findings
 WHERE (detail->>'schema_name' = 'sage_verify'
        OR object_identifier LIKE 'sage_verify.%'
        OR category IN ('slow_query','seq_scan_heavy','sort_without_index','query_tuning','high_total_time','high_plan_time','cache_hit_ratio','xid_wraparound','table_bloat','sequence_exhaustion','missing_fk_index','duplicate_index'))
 GROUP BY category, status
 ORDER BY category, status;
"@
}

Write-Host ""
Write-Host "Expected full-surface case assertions:"
foreach ($target in $Targets) {
  Wait-ExpectedCases $target $PollSeconds
}

Write-Host ""
Write-Host "Verification API smoke:"
$body = @{ email = "admin@pg-sage.local"; password = "CodexVerify123!" } | ConvertTo-Json
$session = $null
$login = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:18085/api/v1/auth/login" -Method Post -ContentType "application/json" -Body $body -SessionVariable session
Write-Host "login=$($login.StatusCode)"
if (-not $SkipActionLifecycle) {
  Assert-ActionLifecycle $PollSeconds
}
$fleet = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:18085/api/v1/databases" -WebSession $session
Write-Host "databases=$($fleet.Content)"
$findings = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:18085/api/v1/findings?status=open" -WebSession $session
Write-Host "open_findings_sample=$($findings.Content.Substring(0, [Math]::Min(2000, $findings.Content.Length)))"
