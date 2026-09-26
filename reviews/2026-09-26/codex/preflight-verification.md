# Focused validation: verification results

## Test Results

**Commands:**

- Clean baseline (from audit-repo/sidecar): `go test -cover -covermode=atomic -coverprofile=../../reports/evidence/preflight-unit.cover -count=1 -json -timeout=300s ./...`
- Retention regressions: `go test -cover -count=1 -json -timeout=180s -run=TestPreflightRetention ./cmd/pg_sage_sidecar ./internal/autonomy`
- Linux binary: `go test -cover -count=1 -json -tags=e2e -run 'TestSmoke|TestFleet' -timeout=600s ./e2e`
- Real metadata fleet and Chromium UI (from workspace): `python validation-runtime-repo/scripts/preflight_runtime.py`

Replay prerequisites and exact instrumentation commands: preflight-replay.md.

**Total:** named Go test/subtest results; parents are included, not unique scenarios.

| Run | Passed | Failed | Skipped |
|---|---:|---:|---:|
| preflight-unit | 7148 | 0 | 8 |
| preflight-retention | 1 | 8 | 0 |
| preflight-binary | 38 | 0 | 0 |
| Real metadata fleet/UI checks | 14 | 0 | 0 |

Security, durability, and telemetry regression results are reported separately in their appendices.

**Coverage:** uncached baseline and union with actual subprocess coverage.

| Package | Baseline | Combined unit + runtime | Covered / statements |
|---|---:|---:|---:|
| cmd/create_admin | 51.5% | 51.52% | 17 / 33 |
| cmd/gen_config_meta | 86.4% | 86.39% | 127 / 147 |
| cmd/pg_sage_sidecar | 43.5% | 73.78% | 1548 / 2098 |
| cmd/reset_admin_for_test | 50.0% | 50.0% | 19 / 38 |
| internal/advisor | 77.5% | 77.54% | 504 / 650 |
| internal/agentdb | 73.4% | 73.46% | 2881 / 3922 |
| internal/alerting | 82.9% | 82.86% | 203 / 245 |
| internal/analyzer | 84.6% | 87.08% | 1301 / 1494 |
| internal/api | 71.9% | 76.36% | 3812 / 4992 |
| internal/auth | 81.4% | 81.41% | 289 / 355 |
| internal/autoexplain | 85.9% | 85.85% | 176 / 205 |
| internal/autonomy | 77.3% | 77.32% | 375 / 485 |
| internal/briefing | 96.8% | 96.79% | 151 / 156 |
| internal/cases | 88.0% | 87.96% | 570 / 648 |
| internal/clone | 80.0% | 80.0% | 96 / 120 |
| internal/collector | 82.9% | 86.02% | 363 / 422 |
| internal/config | 84.7% | 85.89% | 773 / 900 |
| internal/crypto | 86.4% | 86.44% | 51 / 59 |
| internal/custodian/freeze | 94.4% | 94.39% | 101 / 107 |
| internal/custodian/wal | 100.0% | 100.0% | 28 / 28 |
| internal/executor | 75.8% | 77.62% | 1706 / 2198 |
| internal/explain | 82.0% | 82.89% | 189 / 228 |
| internal/fleet | 77.5% | 85.26% | 561 / 658 |
| internal/forecaster | 87.1% | 87.07% | 276 / 317 |
| internal/ha | 97.6% | 97.62% | 41 / 42 |
| internal/ledger | 91.4% | 91.43% | 64 / 70 |
| internal/llm | 88.2% | 88.32% | 537 / 608 |
| internal/logwatch | 83.8% | 83.8% | 512 / 611 |
| internal/mcp | 77.1% | 77.12% | 337 / 437 |
| internal/migration | 70.1% | 70.12% | 399 / 569 |
| internal/migration/plan | 97.4% | 97.37% | 37 / 38 |
| internal/migration/rehearsal | 89.7% | 89.66% | 26 / 29 |
| internal/migration/runtime | 78.8% | 78.76% | 89 / 113 |
| internal/notify | 88.1% | 88.05% | 199 / 226 |
| internal/optimizer | 81.1% | 81.07% | 955 / 1178 |
| internal/policy | 81.8% | 82.81% | 559 / 675 |
| internal/providerobs | 88.5% | 88.51% | 262 / 296 |
| internal/querystore | 83.3% | 83.33% | 35 / 42 |
| internal/rca | 96.2% | 97.13% | 609 / 627 |
| internal/retention | 80.0% | 80.0% | 48 / 60 |
| internal/rollout | 80.0% | 80.0% | 176 / 220 |
| internal/sanitize | 71.4% | 71.43% | 10 / 14 |
| internal/schema | 82.4% | 82.45% | 155 / 188 |
| internal/schema/lint | 71.3% | 71.29% | 499 / 700 |
| internal/schemaguard | 87.9% | 87.88% | 87 / 99 |
| internal/selfmonitor | 75.0% | 84.38% | 27 / 32 |
| internal/startup | 92.2% | 92.22% | 83 / 90 |
| internal/store | 75.3% | 81.26% | 893 / 1099 |
| internal/testdb | 74.8% | 74.76% | 77 / 103 |
| internal/testsupport/assert | 100.0% | 100.0% | 58 / 58 |
| internal/testsupport/check | 83.9% | 83.89% | 151 / 180 |
| internal/testsupport/require | 100.0% | 100.0% | 83 / 83 |
| internal/tuner | 83.7% | 84.52% | 912 / 1079 |
| internal/value | 91.1% | 92.68% | 114 / 123 |
| internal/vectorlab | 93.2% | 93.22% | 344 / 369 |
| internal/verify | 85.9% | 88.24% | 225 / 255 |

Union method: same frozen production source; canonical source-block coordinates; maximum hit count
across Windows Go1.26.1 unit, Linux Go1.25 binary and Windows Go1.26.1 metadata-fleet profiles.
Five inspected one-statement return/assignment spans containing multiline raw strings had
different endpoint coordinates across Go versions. Byte-identical source files and matching
statement counts were verified; these five were mapped to unit-profile coordinates before union.
The explicit alias list is evidence/preflight-coverage-normalization.json. Main had zero aliases
and its 1,548/2,098 result is unchanged. The earlier draft counted five spans twice across
agentdb/cases/store; preflight-coverage-draft-invalid.json retains it as superseded evidence.
All remaining common spans match exactly. Counts are not additive. Platform-only files
are included separately. Do not interpret cross-platform union as each platform independently covered.
No production implementation or existing behavioral assertion was modified.

### Skipped Tests (must be zero or justified)

- preflight-unit: internal/agentdb::TestAWSRDSLiveProvisioning — === RUN   TestAWSRDSLiveProvisioning | aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete | --- SKIP: TestAWSRDSLiveProvisioning (0.00s)
- preflight-unit: internal/logwatch::TestResolveLogDir_AbsoluteUnix — === RUN   TestResolveLogDir_AbsoluteUnix | detect_test.go:78: unix absolute path test not applicable on windows | --- SKIP: TestResolveLogDir_AbsoluteUnix (0.00s)
- preflight-unit: internal/rca::TestTier2Live_RealGemini — === RUN   TestTier2Live_RealGemini | tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test | --- SKIP: TestTier2Live_RealGemini (0.00s)
- preflight-unit: internal/agentdb::TestCloudSQLLiveProvisioning — === RUN   TestCloudSQLLiveProvisioning | gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete | --- SKIP: TestCloudSQLLiveProvisioning (0.00s)
- preflight-unit: internal/agentdb::TestLakebaseLiveProvisioning — === RUN   TestLakebaseLiveProvisioning | lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle | --- SKIP: TestLakebaseLiveProvisioning (0.00s)
- preflight-unit: internal/agentdb::TestAgentDBLiveGauntletBlueprintToAWSRDS — === RUN   TestAgentDBLiveGauntletBlueprintToAWSRDS | live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1 | --- SKIP: TestAgentDBLiveGauntletBlueprintToAWSRDS (0.00s)
- preflight-unit: internal/agentdb::TestAgentDBLiveGauntletTerraformTemplateToCloudSQL — === RUN   TestAgentDBLiveGauntletTerraformTemplateToCloudSQL | live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1 | --- SKIP: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL (0.00s)
- preflight-unit: internal/agentdb::TestAgentDBLiveGauntletAgentRequestToLakebaseBranch — === RUN   TestAgentDBLiveGauntletAgentRequestToLakebaseBranch | live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 | --- SKIP: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch (0.00s)

The selected local binary subset has zero skips; live-model E2E and cloud provisioning
were not selected in this pass. Prior full-run skips remain open in verification.md.

### Failures (if any)

- preflight-retention: TestPreflightRetentionPartitionIdentity — preflight_retention_test.go:69: partition identity/batch limit violated: future rows=0 want=1, deleted=2 limit=1
- preflight-retention: TestPreflightRetentionInvalidatesChangedContract — preflight_retention_test.go:87: 30-day dry run reused for 1-day contract: err=<nil> remaining=2 want=3
- preflight-retention: TestPreflightRetentionAuditFailureDoesNotSilentlyDelete — preflight_retention_test.go:121: mutation committed without durable audit: remaining=1 want=2 applied=0
- preflight-retention: TestPreflightRetentionHonorsRuntimeControls/emergency_stop — preflight_retention_test.go:60: emergency_stop violated: remaining=1 want=2, applied=1 want=0
- preflight-retention: TestPreflightRetentionHonorsRuntimeControls/observation — preflight_retention_test.go:60: observation violated: remaining=1 want=2, applied=1 want=0
- preflight-retention: TestPreflightRetentionHonorsRuntimeControls/manual — preflight_retention_test.go:60: manual violated: remaining=1 want=2, applied=1 want=0
- preflight-retention: TestPreflightRetentionHonorsRuntimeControls/disabled — preflight_retention_test.go:60: disabled violated: remaining=1 want=2, applied=1 want=0
- preflight-retention: TestPreflightRetentionHonorsRuntimeControls — 

### Coverage Gaps (packages below threshold)

All measured production packages meet the **combined** coverage thresholds; exact numbers
are in the table above. Main is 73.78% (1,548/2,098 statements), create_admin 51.52%,
reset_admin_for_test 50.0%, and gen_config_meta 86.4%. Utility commands use the 50% floor.
The clean unit-only main value is still 43.5%, below 70%. Runtime coverage is additional
evidence, not an improvement to that unit-only result. Only packages explicitly reported as
having no statements omit denominators; testdb/testsupport helpers remain measured in the table.
Uncovered runtime functions include syncAgentDBsToFleet, startFleetLogWatcher, runFanoutDrain,
reconcileAgentDBsOnce, updateFleetStatus, writeExtensionMetrics, and configuredMCPMigrationRuntime.
This passing combined percentage does not remove the failing behavior gates.

Targeted-only percentages are not package-wide regression coverage:
retention probes cover main 0.7% and autonomy 9.3%. Other subset values appear in agent reports.

### Bugs Found This Session

See pre-remediation-validation-2026-09-26.md for deduplicated confirmed findings and risk priority.

### Manual Checks Remaining

- CHECK-PREFLIGHT-M01: MANUAL — paid hosted-provider create/restore/teardown, real model behavior.
- CHECK-PREFLIGHT-M02: MANUAL — PostgreSQL supported-version and physical HA/failover matrix.
- CHECK-PREFLIGHT-M03: MANUAL — sustained fleet load and live installation state diagnostics.

### Harness corrections and post-test audit

Initial baseline launch failed before tests because PowerShell split an unquoted coverage-profile
argument. The complete setup-failure log remains in evidence/preflight-unit-launch-failure.jsonl.
Initial real-browser script used Sign in instead of the UI's exact Sign In label and checked the
approval queue before its worker tick. Those fixture errors were corrected (exact label and bounded
polling), staged, and rerun; initial artifacts remain in evidence/preflight-runtime-attempt1/.
The corrected run kept substantive assertions and verified the database's last_analyze timestamp.
Retention regressions assert surviving row identity, bounded deletion, untouched data on audit
failure, and per-control non-execution. The positive nonpartitioned control passes.
Real UI smoke does not certify all UI interactions or durable action verification: ANALYZE's
catalog timestamp proves execution only. Failures intentionally remain pending remediation.
