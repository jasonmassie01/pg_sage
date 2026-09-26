# pg_sage verification report — 2026-09-26



All database tests targeted the disposable audit server. Counts include both parent tests and named subtests; they are not independent feature counts.



# go-unit

## Test Results

**Command:** `go test -cover -count=1 -json -timeout 300s ./...`
**Total:** 7126 passed, 16 failed, 14 skipped (named tests and subtests; exit 1).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 33.2% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.6% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.3% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 74.0% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 59.6% |
| internal/verify | 85.9% |

### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/optimizer: TestHypoPGNamespaceSizeAndTimeoutIsolation` — hypopg_session_test.go:62: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGNormalizedWorkloadParameters` — hypopg_session_test.go:86: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGFailureAndEmptyQueriesCleanSession` — hypopg_session_test.go:100: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGSizeDoesNotAcquireAnotherSession` — hypopg_session_test.go:121: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGConcurrentSessionsAndAvailability` — hypopg_session_test.go:170: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGCleanupFailureDiscardsConnection` — hypopg_session_test.go:204: HypoPG server extension unavailable; session integration not verified
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

### Failures (if any)

- `cmd/pg_sage_sidecar: TestFleetCollectionStatusUsesManagedCollector` — fleet_collection_status_test.go:80: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaLifecycleCreateReplaceDelete` — meta_lifecycle_integration_test.go:92: create: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaLifecycleFailedReplacementPreservesOld` — meta_lifecycle_integration_test.go:156: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaBootstrapKeepsEncryptionKeyAcrossRestart` — meta_lifecycle_integration_test.go:178: validation failed: password is required
- `internal/vectorlab: TestPostgresMissingExtension` — boundary_test.go:64: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestCommandRealReportAndInconclusiveExit` — command_test.go:51: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestCommandOutputFailure` — command_test.go:82: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresPermissionDenialAndRLS` — permissions_test.go:43: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresRejectsUnsafeIdentityAndView` — permissions_test.go:66: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresReadOnlyTransactionRejectsMutation` — permissions_test.go:91: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresFilteredEvidenceAndLocalSettings` — postgres_test.go:66: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresNoIndexEmptyAndMissingTable` — postgres_test.go:86: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresCancellationAndConcurrentSessions` — postgres_test.go:116: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresLockTimeoutIsBounded` — postgres_test.go:141: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `cmd/pg_sage_sidecar: TestMetaReconnectReplacesFailedGenerationAndHonorsStop` — meta_reconnect_integration_test.go:17: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaStartupRegistersOnlyEnabledDatabases` — meta_reconnect_integration_test.go:50: validation failed: password is required

### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 33.2% — inspect uncovered runtime/fixture branches.
- `internal/vectorlab`: 59.6% — inspect uncovered runtime/fixture branches.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.

Fixture limitation: this initial run lacked pgvector/HypoPG and a password in the test DSN. Failures are retained for transparency; corrected run follows.

# go-unit-complete

## Test Results

**Command:** `go test -cover -count=1 -json -timeout 300s ./...`
**Total:** 7148 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.6% |
| internal/api | 71.9% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

### Failures (if any)

- None.

### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


# go-integration

## Test Results

**Command:** `go test -cover -count=1 -json -tags=integration -timeout 300s ./...`
**Total:** 7295 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 72.3% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 93.4% |
| internal/fleet | 77.5% |
| internal/forecaster | 92.7% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 97.9% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.4% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

### Failures (if any)

- None.

### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


# go-race-linux

## Test Results

**Command:** `go test -race -cover -count=1 -json -timeout 300s ./...`
**Total:** 7124 passed, 12 failed, 8 skipped (named tests and subtests; exit 1).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 82.2% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 77.9% |
| internal/providerobs | 88.5% |
| internal/querystore | 83.3% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1
- `internal/logwatch: TestResolveLogDir_AbsoluteWindows` — detect_test.go:91: windows absolute path test not applicable on unix
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test

### Failures (if any)

- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP/docs/configuration.md` — wave5_contracts_test.go:19: read /docs/configuration.md: open /docs/configuration.md: no such file or directory
- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP/sidecar/config.example.yaml` — wave5_contracts_test.go:19: read /sidecar/config.example.yaml: open /sidecar/config.example.yaml: no such file or directory
- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP` — 
- `internal/config: TestWave5GeneratedLifecycleReferenceMatchesRegistry` — wave5_contracts_test.go:42: read /docs/generated/config-lifecycles.md: open /docs/generated/config-lifecycles.md: no such file or directory
- `internal/config: TestWave5CurrentLifecycleClaimsUseTypedRegistry` — wave5_contracts_test.go:73: read /sidecar/config.example.yaml: open /sidecar/config.example.yaml: no such file or directory
- `internal/config: TestWave5AgentDBDocsDescribeImplementedAuthorityAndMonitoring` — wave5_contracts_test.go:82: read /docs/agent-db-deployments.md: open /docs/agent-db-deployments.md: no such file or directory
- `internal/startup: TestWave5GolangCILintUsesPinnedV2Contract` — tooling_contract_wave5_test.go:34: read lint workflow: open /.github/workflows/test.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures/test.yml` — tooling_contract_wave5_test.go:55: read workflow: open /.github/workflows/test.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures/ci.yml` — tooling_contract_wave5_test.go:55: read workflow: open /.github/workflows/ci.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures` — 
- `internal/startup: TestWave5ComposeSupportsParallelBrowserVerification` — tooling_contract_wave5_test.go:99: read test compose file: open /docker-compose.test.yml: no such file or directory
- `internal/startup: TestWave5BrowserFixtureUsesLocalLLMMock` — tooling_contract_wave5_test.go:116: read browser fixture config: open /test-fixtures/config.test.yaml: no such file or directory

### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


# go-race-complete

## Test Results

**Command:** `go test -race -cover -count=1 -json -timeout 300s ./...`
**Total:** 7136 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.3% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 77.9% |
| internal/providerobs | 88.5% |
| internal/querystore | 83.3% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1
- `internal/logwatch: TestResolveLogDir_AbsoluteWindows` — detect_test.go:91: windows absolute path test not applicable on unix
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test

### Failures (if any)

- None.

### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


# go-e2e

## Test Results

**Command:** `go test -cover -count=1 -json -tags=e2e -timeout 900s ./e2e`
**Total:** 73 passed, 0 failed, 13 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|

### Skipped Tests (must be zero or justified)

- `e2e: TestLLMBasicChat` — llm_integration_test.go:80: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMBriefingGeneration` — llm_integration_test.go:122: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMOptimizerIndexRecommendation` — llm_integration_test.go:182: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMAdvisorVacuumRecommendation` — llm_integration_test.go:307: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMMarkdownWrappedJSON` — llm_integration_test.go:408: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMTokenBudgetTracking` — llm_integration_test.go:480: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestOptimizerMultiQueryConsolidation` — optimizer_multiquery_test.go:29: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_MergeJoin` — tuner_llm_test.go:258: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_IndexOnlyScan` — tuner_llm_test.go:331: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_BitmapScan` — tuner_llm_test.go:384: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_NestLoop` — tuner_llm_test.go:439: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_Parallel` — tuner_llm_test.go:511: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_NoSeqScan` — tuner_llm_test.go:568: SAGE_LLM_API_KEY not set, skipping live LLM test

### Failures (if any)

- None.

### Coverage Gaps (packages below threshold)

All packages with measured statements meet coverage thresholds; actual percentages are listed above. Test-only packages have no statements.

### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


## Verification summary and checklist

Test environment: Windows Go 1.26.1; Linux race toolchain from golang:1.25;
PostgreSQL 17.9 with pg_stat_statements 1.11, pgvector 0.8.6, HypoPG 1.4.3,
pg_hint_plan 1.7.1; Node 24.13.0; disposable database port 55439.
The database was separate from all existing application databases.

| Final run | Passed test/subtest results | Failed | Skipped |
|---|---:|---:|---:|
| Unit with correct fixture | 7,148 | 0 | 8 |
| Integration | 7,295 | 0 | 8 |
| Linux race with complete repository mount | 7,136 | 0 | 8 |
| Binary end-to-end | 73 | 0 | 13 |
| Frontend component tests | 89 | 0 | 0 |
| Mocked browser tests | 54 | 0 | 0 |

The first race run's 12 failures were missing repository docs/workflow files in the
container mount, not reported races. The corrected full run passed with zero race reports.
Initial unit fixture failures were missing vector support and passwordless DSN fields.
Both initial failed runs are preserved above; no test assertion was weakened.

Coverage release gate remains **FAIL**: cmd/pg_sage_sidecar = 43.5%, below 70%.
Missing areas include process startup/shutdown, full mode-specific feature construction,
reconnect/removal and the real routes between generation, persistence and execution.
E2E was run without a merged child-binary coverage profile; do not inflate that percentage.
The requested review reports this debt; it does not claim completed coverage remediation.

CHECK-AUDIT-01: PASS — source revision and clean original checkout verified against remote HEAD.
CHECK-AUDIT-02: PASS — Go build, vet and golangci-lint (0 issues).
CHECK-AUDIT-03: PASS — frontend lint/build; build retains an 874 KB bundle warning.
CHECK-AUDIT-04: PASS — corrected unit/integration/race suite has no test failures.
CHECK-AUDIT-05: PASS — local binary end-to-end checks; 13 real-model tests explicitly skipped.
CHECK-AUDIT-06: PASS — 89 component tests and 54 browser tests with mocked APIs.
CHECK-AUDIT-07: FAIL — sidecar entry-point coverage below business-logic threshold.
CHECK-AUDIT-08: FAIL — three targeted regression probes reproduce unfixed contract defects.
CHECK-AUDIT-09: FAIL — transactional retention reproduction deletes a noneligible partition row.
CHECK-AUDIT-10: PASS — proposed four-table SRE DDL accepted inside BEGIN/ROLLBACK.
CHECK-AUDIT-11: MANUAL — hosted provider lifecycle, real model behavior and real-backend UI commissioning.
CHECK-AUDIT-12: MANUAL — supported PostgreSQL-major/HA/failover matrix and long-running soak.
CHECK-AUDIT-13: FAIL — npm audit: 3 moderate package entries for one development-tool advisory.

The product validation is **not a passing release gate**, despite passing baseline tests.
The report is complete; product remediation is still required.

Post-test audit: demonstrated failures reveal gaps in collector-to-rule units,
finding suppression/inverse-SQL identity, partitioned deletion and mounted-route authority.
Mocks hide provider execution and browser/backend integration; source-unwired components
can retain high package coverage. Add full trigger-to-outcome tests for each repair before
raising autonomy. Proposed SRE CHECK-01..35 are future acceptance criteria, not these results.
No production source was changed to make a baseline or regression test pass.

