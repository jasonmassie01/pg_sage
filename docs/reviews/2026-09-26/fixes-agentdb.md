# AgentDB fixes — 2026-09-26

Branch `fix/2026-09-26-agentdb` (worktree `C:/Users/jmass/pgsr-fix-agentdb`), base `9a3cac7`.
Scope: `sidecar/internal/agentdb`, `sidecar/internal/api/agent_db_*`,
`sidecar/cmd/pg_sage_sidecar/agentdb_fleet.go` + `agentdb_secret_ref.go`,
`sidecar/web/src/pages/AgentDBsPage.jsx` + `web/src/pages/agentdb/*`.
No real cloud APIs were called; the six live cloud tests remain skipped.
The embedded `internal/api/dist` was **not** rebuilt; that belongs to the api-web agent.

## Status table

| ID | Status | Commit(s) | Test(s) | Notes |
|---|---|---|---|---|
| G8-B01 | FIXED | aeaf46e / 741bcb6 | TestAgentPingCannotChangeLifecycleStatus, TestAgentPingDoesNotClearBudgetExceeded, TestPingedExpiredDeploymentIsStillClaimed | Ping writes only `last_ping_at` + validated `agent_status` (healthy/degraded/busy/idle; `active` = healthy). Lifecycle words are 400. |
| G8-B02 | FIXED | 5dff2d7 / 4a8fdcf | TestCleanupArchiveDoesNotExemptFromTeardown, TestArchivedBlockedDeploymentIsRetriedAfterRestoreVerified, TestReconcileContinuesAfterProviderError, TestManualArchiveLiveDeploymentIsTornDown, TestReconcileBlocksLiveDeploymentWithoutLiveRunner | Reconciler re-claims archived live rows every pass (bounded 100, blocked rows rotate); block reason persisted in `teardown_blocked_reason/at`; per-row errors never abort the batch; live rows never go to the dry-run runner. |
| G8-B03 | FIXED | aeaf46e / 741bcb6 | TestRegisterSameIDDoesNotResetLiveDeployment, TestRegisterSameIDDifferentTenantConflicts, TestRegisterSameOwnerCanReplanBeforeLive | Upsert only replaces same-owner pre-live plans; live/in-flight rows replay idempotently; other tenant/agent → 409. |
| G8-B04 | FIXED | 5dff2d7, 34ac003 / 4a8fdcf, dfe8172 | TestDestroyRefusesDerivedIdentityWithoutReceipt, TestDestroyRequiresLiveCreationReceipt, TestAWSDestroyRefusesEmptyRecordedID, TestAWSDestroyRefusesTagMismatch, TestCloudSQLDestroyVerifiesLabelsAndUnprotects, TestLakebaseDestroyRefusesEmptyRecordedID | Destroy requires `live_mode`, recorded id and matching live creation receipt; AWS tags / GCP labels verified before delete; Lakebase (no tag surface) requires the recorded id and never adopts. Collision-free naming not changed (not needed once ownership is verified). |
| G8-B05 | FIXED (agent principal) | 6231c2e / 27254e4 | TestAgentPrincipalCannotReadOtherTenant, TestAgentPrincipalRequestTenantComesFromToken, TestAgentPrincipalDeniedManagementAndBadTokens, TestAdminMintsAgentTokenBoundToIdentityTenant, TestRequestAllowedRegionsIgnoredFromBody | Tenant-bound agent tokens (admin-minted per identity) on `/api/v1/agent-api/`; tenant/agent from token; agents cannot approve/authorize/destroy/archive/mint. Human admin/operator remain global operators (documented in `docs/agent-db-deployments.md`). `allowed_regions` is server config. Per-user tenant scoping for humans is a product decision (DEFERRED). |
| G8-B06 | FIXED | 071f012, 34ac003 / 5871fa7, dfe8172 | TestAmbiguousCreateStaysUncertainAndIsReconciled, TestHostedStatusUnknownCreateIsNotRetryable, TestAWSCreateAmbiguousErrorIsUncertain, TestAWSStatusWithoutRecordedIDOnlyAdoptsUncertainCreateByTag | `create_operation_id` + `live_mode` persisted before the call; only definitive rejections become `failed`; `create_uncertain` refuses new creates and is resolved by tag-verified lookup (adopt + receipt) or confirmed absence. Hosted/Lakebase stay blocked for manual reconciliation (no name lookup). |
| G8-B07 | FIXED | 34ac003 / dfe8172 | TestAWSCreateRefusesRegionMismatch | Create refused when requested region ≠ SDK runner region. |
| G8-B08 | FIXED | 3d1555d / 8858cef | TestUnknownInstanceClassDeniedLive, TestLowConfidenceEstimateIsDoubledAgainstCeiling, TestIssueLiveRecordsAppliesDeploymentBudgetGate, TestExtendLeaseCappedByPolicyTTLAndBudget | Unknown class refused unless provider config `allow_unknown_instance_pricing`; doubled low-confidence compare; lease extension capped by provider TTL and re-estimated against budget. Reviewer≠requester (two-person review) DEFERRED (product decision). RDS `ttl` tag not updated on extend (DEFERRED). |
| G8-B09 / SURF-20 | FIXED (partial) | ed288cb, 34ac003 / be85c13, dfe8172 | TestCloudSQLDestroyVerifiesLabelsAndUnprotects, TestCloudSQLUnauthorizedIsTypedPermissionError, TestGCPTokenSourcesReportExpiryAndRefresh | Refreshing metadata-server token source (`PG_SAGE_GCP_TOKEN_SOURCE=metadata`); static token reports expiry via Token() and readiness; typed HTTP errors; deletion protection lifted inside the authorized destroy. Creating a DB user/credential for the agent DEFERRED. No new module dependency added (oauth2/ADC would change go.mod). |
| G8-B10 | FIXED (partial) | 3d1555d / 75532fa | TestOperatorApproveCannotOverridePolicyDeny, TestApprovedRequestIsSingleUse, TestApprovedBlueprintSpecWinsOverOverrides | Deny is terminal; approvals single-use (`consumed_deployment_id`); approved spec wins. Requiring an approved request for `POST /agent-dbs` cloud registers DEFERRED (breaks the documented register flow; product decision). |
| G8-B11 | FIXED | 3d1555d / 81c2bdc, f27fcd1, 6308143 | TestRecordBackupRestoreVerifiedRequiresDrillAndScope, TestAgentDBLifecycleEndpointsExposeCostBackupsAndHints (strengthened), TestPreflightSurfaceAgentDBSchemaLifecycle (adapted) | `restore_verified` only via admin `POST /{id}/backups/restore-drill` with evidence (recorded `restore_drill` attempt); backup upsert scoped to its deployment; UI button → "Attest restore drill (admin)". |
| G8-B12 | FIXED | 5dff2d7, 156ac43 / 4a8fdcf, 3335395 | TestRequireBackupFalseHonoredForAuthorizedDestroy, TestRegisterHonoursBackupPolicyOption | Direct destroy uses the effective policy; register honours `require_backup_before_destroy`. |
| G8-B13 | FIXED | 6308143 | AgentDBsPage.test.jsx "authorizes live execute and destroy before calling the provider", "does not execute live work when the operator declines the estimate", agentDBLiveActions.test.js | UI calls authorize-live, confirms the server estimate, sends only the server tuple. |
| G8-B14 | FIXED | ed288cb / be85c13, 4f4da33 | TestRegisterRejectsSecretRefOutsideAllowlist, TestAgentDBRegisterPersistsSecretRef | Register persists `secret_ref` restricted to `env:PG_SAGE_AGENTDB_*`; fleet resolver enforces the same allow-list. `PATCH /secret-ref` DEFERRED. |
| G8-B15 / SURF-04 | FIXED | ed288cb / be85c13 | TestAgentDBProviderReadinessUsesRuntimeRegistry | Readiness per provider from runtime registry, credential checks and the same global/persisted policy layers as execution. |
| G8-B16 | FIXED | 3d1555d / 8b57eb7 | TestEnsureIsMemoizedPerPool | Memoized per pool + `sage.agent_db_schema_version`; migrations run on version change. |
| G8-B17 | FIXED (partial) | 3d1555d, 156ac43 / 3335395 | TestLocalProvisioningDisabledByDefault, TestLocalProvisioningRefusesToAdoptExistingSchema, TestPreflightSurfaceAgentDBInvalidRegistrationLeavesNoSchema (auditor) | Local DDL requires `PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1`, honours emergency stop, validates before DDL, never adopts (42P06/42P04 → 409), compensating DROP on failed register. Scoped role/credentials and DROP on teardown DEFERRED. |
| G8-B18 | FIXED (partial) | 5dff2d7 / 4a8fdcf, 4f4da33 | TestEmergencyStopBlocksAgentDBMutations, TestAgentDBMutationGateHonoursFleetEmergencyStop | Store gate (persisted `emergency_stop`, fail closed) before live create, direct destroy, every provider destroy incl. TTL, and local DDL; reconciler also gates on in-memory fleet stop. API routes see the persisted flag only (in-memory fleet gate needs a router.go wiring edit — DEFERRED). Trust ramp not consulted (DEFERRED). |
| G8-B19 | FIXED | ed288cb / be85c13 | TestSupabaseProjectPasswordsAreDistinctPerProject | Per-project HMAC-derived password from the configured master secret (operator-recoverable, never shared). |
| G8-B20 | FIXED (partial) | 34ac003 / dfe8172, 75532fa, 8182d9f | TestAWSCreateHonorsApprovedSettings | Runner honours multi_az/engine_version (AWS) and availabilityType (GCP); refuses deletion_protection (AWS) and private_network+public; blueprint carries multi_az/private_network; Terraform renders identifier from a variable and the real deletion_protection. AWS subnet group / SG DEFERRED. |
| SURF-05 | FIXED (relabel) | 3d1555d / 8182d9f, 6308143 | TestTerraformTemplateProvisionIsReviewOnlyAndHashBound | Chose honest relabel: plan `template_semantics=review_only` + note; deployment records template `content_sha256`; UI tooltip. |
| SURF-06 | FIXED | 3d1555d / 81c2bdc | TestDryRunBackupCheckDoesNotRecordVerified, TestBackupAssuranceManagedProviderRecordsPlannedCheck | Dry-run/command check records `planned` + `execution_mode=dry_run`. |
| SURF-17 / G8-B26 | FIXED (actor); DEFERRED (audit errors) | f27fcd1 | TestPreflightSurfaceAgentDBApprovalActorCannotBeSpoofed (auditor), TestAgentDBDeployRequestEndpoints (strengthened) | Blueprint/template create+approve, deploy-request create+review use the session user; no session → 401. Surfacing `_ = s.audit(...)` errors DEFERRED (broad change). |
| G8-B21 / SURF-07 | FIXED (partial) | 4f4da33 | TestAgentFleetPlanRemovesInactiveAndReplacesChanged, TestAgentDeploymentToFleetConfig_Defaults | Desired-state sync: remove archived/deleted/destroyed/unresolvable, replace changed credentials; sslmode default `require`. Snapshot persistence in meta-DB, RDS secret ARN resolution, reconcile ctx in `connectMonitoredDB`, scale-to-zero cadence DEFERRED. |
| SURF-08 | FIXED (ordering); DEFERRED (wiring) | 70c6b91 / 85a2dba | TestMonitoringScheduleRotatesAcrossPasses | Due-time + least-recently-scheduled selection; no runtime worker wired. |
| G8-B22 | FIXED (partial) | 34ac003 / dfe8172, be85c13 | TestMapProviderErrorDoesNotTreatRateSubstringAsThrottle | "rate" substring fixed; Cloud SQL errors typed by HTTP status; unique RDS final snapshot id. Snapshot retention sweep DEFERRED. |
| G8-B23 | FIXED | 5871fa7 | TestReconcileResolvesDestroyingWithoutTeardownID, TestReconcileLiveProvisioning (counter) | destroying+NotFound → destroyed; destroy_pending w/o id reported; status refreshes counted as StatusChecked. Test written with the fix. |
| G8-B24 | FIXED (partial) | b5d0652 | TestPingTokenFailuresLimitedPerDeploymentAndPruned | Per-deployment failure cap across token hashes; 24h pruning. Per-IP limiter and `agent_db_pings`/audit retention DEFERRED. Test written with the fix. |
| G8-B25 | FIXED | 6308143 | agentDBLiveActions.test.js (pagination) | "Load more deployments" via `next_cursor`. |
| G8-B27 | FIXED | 5dff2d7 / 4a8fdcf | TestDestroyValidatesStateBeforeConsumingAuthorization, TestDestroyRefusesDerivedIdentityWithoutReceipt | All checks before `ClaimLiveExecution`; receipt always has the recorded id. |
| G8-B28 | FIXED | 3d1555d / 8182d9f, dbb5cfe | TestRenderedTerraformEscapesTemplateExpressions, TestTerraformZipCapsTotalDecompressedSize | `${`/`%{` escaped; zip capped by file count and total size. Zip test written with the fix. |
| G8-B29 | FIXED (partial) | 3d1555d / 8182d9f | TestNegativeCostSampleRejected, TestAgentPingDoesNotClearBudgetExceeded | Negative samples rejected; heartbeat no longer resets budget_exceeded. Budget hard-limit → stop/destroy proposal DEFERRED. |
| G8-B30 (readMap) | FIXED (partial) | 27254e4, 8858cef | — | `readJSONMap` rejects malformed JSON on request-create, agent API, lease and drill paths; other handlers unchanged. |
| G8-D01 | FIXED | 1335f26 | existing blueprint tests | Heuristic generator moved into `heuristic_blueprint_generator_test.go`. A separate `agentdbtest` package would create an import cycle with in-package tests; no api test uses it. |
| G8-D02 | FIXED (DELETE) | 1335f26 | lifecycle_concurrency tests via `directDestroyForTest` | `Store.DestroyProvisionLive` removed. |
| G8-D04 | FIXED (WIRE) | 8858cef | TestIssueLiveRecordsAppliesDeploymentBudgetGate | BudgetGate wired into live issuance. |
| G8-D05 | FIXED | f27fcd1 | — | `registerAgentDBRoutes`/`agentDBSubrouter` moved to `agent_db_router_helpers_test.go`. |

## Independent auditor tests

Copied verbatim in 156ac43: `sidecar/internal/api/preflight_surface_agentdb_test.go`,
`preflight_surface_helpers_test.go`. All four pass on this branch.

- `TestPreflightSurfaceAgentDBInvalidRegistrationLeavesNoSchema` — fixed by validating before DDL
  and compensating on failure (3335395).
- `TestPreflightSurfaceAgentDBApprovalActorCannotBeSpoofed` — fixed by session actors (f27fcd1).
- **Conflict, needs lead review:** `TestPreflightSurfaceAgentDBSchemaLifecycle` asserted that an
  operator can POST `status:"restore_verified"` to `/backups` (this is exactly G8-B11) and that
  local_postgres DDL runs without opt-in (G8-B17). I did not weaken either fix. The test now sets
  `PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1`, asserts the self-attestation is rejected (400), that an
  operator cannot use the drill endpoint (403), and that an admin drill attestation then allows the
  tombstone delete. Every other assertion is unchanged (f27fcd1).

## Cross-area edits

- `sidecar/internal/api/auth_middleware.go`: `shouldSkipAuth` skips session auth for
  `/api/v1/agent-api/` (those routes authenticate with agent tokens in `requireAgentPrincipal`).
- `sidecar/cmd/pg_sage_sidecar/main.go` `startAgentDBReconciler`: three lines applying
  `require_backup_before_destroy` and the fleet emergency-stop gate to the reconciler store.
- `docs/agent-db-deployments.md`: principal model section (operator/admin vs agent token vs ping token).
- Not changed but relevant: `internal/api/router.go` still builds the API store with `NewStore`;
  API routes get backup policy and local opt-in via `applyAgentDBStorePolicy`, but not the
  in-memory fleet emergency-stop gate (persisted flag only).

## Operational notes for the lead

- New env: `PG_SAGE_AGENTDB_LOCAL_PROVISIONING=1` (local_postgres DDL opt-in),
  `PG_SAGE_GCP_TOKEN_SOURCE=metadata`, `PG_SAGE_GCP_METADATA_URL`,
  `PG_SAGE_GCP_ACCESS_TOKEN_TTL_SECONDS`. `PG_SAGE_SUPABASE_DATABASE_PASSWORD` is now a master key.
- `secret_ref` values must be `env:PG_SAGE_AGENTDB_<NAME>`.
- New schema objects (via `safety_schema.go`, schema version 2026092601): deployment columns
  `agent_status`, `teardown_blocked_reason`, `teardown_blocked_at`, `create_operation_id`;
  request column `consumed_deployment_id`; tables `agent_db_agent_tokens`, `agent_db_schema_version`.
- New routes: `POST /api/v1/agent-dbs/identities/{agent_id}/tokens` (admin),
  `POST /api/v1/agent-dbs/{id}/backups/restore-drill` (admin), `/api/v1/agent-api/...` (agent token).
- `internal/api/dist` must be rebuilt by the api-web agent to ship the UI changes.

## Test Results

**Command:** `go test -cover -count=1 -v ./internal/agentdb ./internal/api ./cmd/pg_sage_sidecar`
(and again with `-tags=integration`), `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`;
`go build ./...` and `go vet ./...` clean.
Web: `npm ci`, `npm run lint` (clean), `npx vitest run src/pages` (71 passed); production bundle
verified with `vite build --outDir <scratch>` (dist intentionally not rebuilt).

**Total (unit run):** 1409 passed, 0 failed, 6 skipped (counts include subtests)
**Total (integration tag):** 1443 passed, 0 failed, 6 skipped

**Coverage:**
- internal/agentdb: 74.0%
- internal/api: 72.4% (72.9% with `-tags=integration`)
- cmd/pg_sage_sidecar: 44.0%

### Skipped Tests (must be zero or justified)
- internal/agentdb: TestAWSRDSLiveProvisioning, TestCloudSQLLiveProvisioning,
  TestLakebaseLiveProvisioning — SKIPPED: require real cloud credentials; live cloud calls are out of scope.
- internal/api: TestAgentDBLiveGauntletBlueprintToAWSRDS,
  TestAgentDBLiveGauntletTerraformTemplateToCloudSQL,
  TestAgentDBLiveGauntletAgentRequestToLakebaseBranch — SKIPPED: same (live cloud gauntlet).

### Failures (if any)
- None.

### Coverage Gaps (packages below threshold)
- cmd/pg_sage_sidecar: 44.0% (baseline 43.5%) — `main` package, dominated by process wiring
  (startup, fleet init, HTTP server). The AgentDB-owned files there are covered
  (fleet plan, gate, secret resolution); the gap is outside this area.
- internal/agentdb 74.0% and internal/api 72.4% meet the 70% threshold.

### Bugs Found This Session
1. [BUG] store.go Ping — ping token could set any lifecycle status / un-archive / clear budget_exceeded (G8-B01).
2. [BUG] lifecycle.go — archived live deployments never revisited; one provider error aborted the batch (G8-B02).
3. [BUG] queries.go registerSQL — re-register wiped live identity and could transfer tenant (G8-B03).
4. [BUG] AWS/GCP/Lakebase Destroy — derived-name destroy with no ownership check (G8-B04).
5. [BUG] execution.go — ambiguous create rewritten to retryable `failed` (G8-B06).
6. [BUG] aws_rds_runner.go — region allowlist not bound to the SDK region (G8-B07).
7. [BUG] provider_cost/policy — unknown class priced $100/mo; doubling never applied; unbounded lease extension (G8-B08).
8. [BUG] operations.go RecordBackup — self-attested restore_verified and cross-deployment backup rewrite (G8-B11).
9. [BUG] teardown SQL — require_backup_before_destroy=false ineffective and burned the authorization (G8-B12, G8-B27).
10. [BUG] schema.go Ensure — full DDL on every call including unauthenticated ping (G8-B16).
11. [BUG] schema.go — local DDL adopted existing schemas and left orphan schemas on rejected registration (G8-B17, auditor).
12. [BUG] provider_errors.go — "rate" substring matched "generate"/"operation" (G8-B22).
13. [BUG] monitoring_schedule.go — starvation beyond 100 targets (SURF-08).
14. [BUG] test isolation — t.Cleanup row deletes ran on already-closed pools (defer before cleanup), leaking rows into later reconcile tests; fixed in fixtures.

### Manual Checks Remaining
- CHECK-M1: MANUAL — AgentDB page after the api-web agent rebuilds `internal/api/dist`
  (authorize-live confirmation dialog, restore-drill prompt, "Load more deployments").
