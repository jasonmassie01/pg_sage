# Fixes — executor & safety area (2026-09-26)

Branch `fix/2026-09-26-executor`, worktree `C:/Users/jmass/pgsr-fix-executor`.
Process: every assigned finding re-verified in code, regression tests committed first
(`b840e4f`, confirmed failing: compile failures for new safety APIs, runtime failures
elsewhere), then one fix commit per area. All paths relative to `sidecar/`.

## Decisions (fail-closed mappings)

- **Standing gate ramp mapping (G4-B02).** `policy.RuntimeState` now carries
  `Tier3Safe`, `Tier3Moderate`, `RampStart`, `InConfiguredWindow`; zero values fail closed.
  read_only: advisory/autonomous execute (no ramp). safe: needs `tier3_safe` + ramp ≥ 8 d,
  else `blocked(trust_ramp_not_satisfied)`. moderate: advisory queues; autonomous needs
  `tier3_moderate` + ramp ≥ 31 d, then both the policy-document window and the configured
  `trust.maintenance_window` (legacy grammar) must be open. high, `cancel_backend`,
  `terminate_backend`: always `queue_approval`. approval mode: always queue. Tier is decided
  before windows; an XID/disk deadline override can only lift the window restriction (B15).
  Consequence: with defaults (`tier3_moderate: false`, empty `maintenance_window`) moderate
  actions never auto-execute, matching the documented ramp and the startup warning.
- **Operator approvals (G4-B16)** are not gated by auto-execution eligibility (ramp, tier3
  flags, auto mode, empty trust window); hard stops, trust level, provider support and a
  *configured* trust window still apply.
- **Retention (G4-B04/R02)** is a typed `retention_delete` contract of risk **moderate**
  (not high: custodians have no approval queue, so high would silently disable the feature).
  It therefore needs autonomous trust, `tier3_moderate`, the 31-day ramp, an open window,
  the `retention` class, no emergency stop, executor enabled and a primary.
- **Verified-index revert** uses a new `revert_created_index` contract (moderate, no approval
  guardrail) after OID identity is proven; otherwise the B01 fix would have made every revert
  of pg_sage's own index wait for approval via the `drop_unused_index` contract.
- **Backend signals** never execute autonomously (gate + executor + custodian refusals).
- **Change classes (G4-B18):** new `backend_signal`, `query_hint`, `schema_change`; built-in
  profiles include them, stored pre-existing policy documents do not (fail closed).

## Results

| ID | Status | Commit | Test(s) | Notes |
|---|---|---|---|---|
| G4-B01 | FIXED | ad821fb | TestPolicyContractMapsApprovalRequiredGuardrail, TestStandingGateQueuesApprovalGuardedFindings, TestLegacyPolicyQueuesApprovalGuardedContract, TestIsApprovalRequiredGuardrailNormalizesSpelling | Normalized in both gate mapping and legacy policy |
| G4-B02 | FIXED | 2382ffb, ad821fb | TestGateRequiresTierFlagsAndRamp, TestGateModerateHonorsConfiguredWindow, TestStandingRuntimeStateCarriesTrustCeilings | Mapping above |
| G4-B03 | FIXED | 2382ffb, ad821fb, 4cd9317 | TestGateBackendSignalsAlwaysQueue, TestExecuteFindingNeverSignalsBackendAsRawSQL, TestCustodianBlockerProposalNeverSignals, TestBackendEvidenceRequiresBackendStart, TestSignalMatchingBackendRejectsDifferentBackendStart, TestSignalMatchingBackendSignalsExactIdentity, TestBackendSignalSQLExcludesProtectedBackends, TestRunawayFindingCarriesBackendStart, TestOldestXminBlockerExcludesProtectedApplications | pid+backend_start+query identity; current DB, client backends only, pg_dump/basebackup/restore/pg_sage excluded |
| G4-B04 | FIXED | 4cd9317, ad821fb | TestRetentionApplyRequiresAuthorization, TestRetentionApplyPassesIntentToAuthorizer, TestRetentionDeleteUsesLockTimeout | Gate-authorized per batch; 30 s statement / 2 s lock timeout. Delete is recorded in `sage.retention_run` + ledger decision, not `action_log` |
| R01 | FIXED | 4cd9317 | TestRetentionDeleteNeverTouchesOtherPartitions, TestRetentionDeleteStatementBindsIdentityAndCutoff | (tableoid, ctid) + cutoff re-check in DELETE |
| R02 | FIXED | 4cd9317 | TestRetentionApplyRequiresAuthorization | Same as B04 |
| R03 / G2-B12 | FIXED (partial) | 4cd9317 | TestRetentionDryRunMustMatchCurrentSemantics, TestRetentionDryRunNeedsReviewWindow, TestUnboundedAppendDoesNotGuessArbitraryTimeColumn | Dry run bound to relation+column+window, ≥24 h and ≤7 d old; column limited to created_at/occurred_at. An explicit owner-selected column needs `sage.table_contract.retention_column` (schema package) — cross-area |
| G4-B05 | FIXED | 4cd9317, f34e3d7 | TestWALBoundTargetKeepsHeadroomAboveRetained, TestWALBoundNeverInvalidatesLaggingSlot, TestWALBoundEscalatesForRegisteredConsumer, TestWALBoundEscalatesWhenDiskCannotHoldHeadroom, TestParseWALKeepSizeBytes | Bound = max(limit, 1.5×max retained); registered consumer or >90% disk → plan-only escalation; every path refuses keep sizes below headroom |
| G4-B06 | FIXED | ad821fb | TestConcurrentIndexRollbackSQL, TestMonitorRollbackIsConcurrentAndLockBounded | CREATE/DROP INDEX rollbacks rewritten to CONCURRENTLY; approved path authorizes (B11). Analyzer still emits non-concurrent rollback text (rewritten at execution) |
| G4-B07 | FIXED | 909a473 | TestValidateRejectsAlterTableSubcommandLists, TestValidateAcceptsSingleReloptionSubcommand, TestVacuumFullIsNeverClassifiedAsSafeVacuum, TestValidateRejectsSQLComments | Tokenizer-light normalization (not a full parser); SET TABLESPACE removed |
| G4-B08 | FIXED | ad821fb | TestVerifiedActionRejectsUnsafeCreateForms, TestPrepareVerifiedIndexRefusesExistingName, TestRevertRefusesIndexWithDifferentIdentity, TestRevertDropsOwnIndexAndCompletesVerification | IF NOT EXISTS rejected; name must not exist; OID recorded and re-checked before drop |
| G4-B09 | FIXED | ad821fb | TestPrepareVerifiedIndexQualifiesNameOutsideSearchPath | Quoted schema-qualified name |
| G4-B10 | FIXED | ad821fb | TestHysteresisFollowsFindingIdentityAcrossIDs | Hysteresis/retries by category+object; oscillation counts rollback outcomes |
| G4-B11 | FIXED | ad821fb | TestMonitorWithoutAuthorizerWithholdsRollback, TestManualRollbackAuthorizerHonorsExecutorAndTrust | Authorizer mandatory; nil withholds |
| G4-B12 | FIXED | ad821fb | TestMonitorDoesNotOverwriteOperatorRollback | Compare-and-set outcomes; RollbackAction claims `rolling_back` |
| G4-B13 | FIXED | ad821fb | TestMonitorReloadsConfigAfterAlterSystemRollback | Auto and manual GUC rollbacks reload |
| G4-B14 | FIXED | 4cd9317 | TestAutonomyRouterMarksReplicaProposals, TestAutonomyRouterWithoutReplicaCheckFailsClosed | HA probe per database (Check ‖ InSafeMode) |
| G4-B15 | FIXED | 2382ffb | TestDeadlineOverrideCannotBypassTrustModeOrRisk, TestDeadlineOverrideLiftsConfiguredWindowOnly | |
| G4-B16 | FIXED | ad821fb | TestApprovalReadinessIgnoresAutoExecutionEligibility, TestApprovalReadinessStillRefusesObservationTrust | Uses configured window only when set; does not consult policy-document windows (no ctx in readiness API) |
| G4-B17 | FIXED (partial) | ad821fb | TestStandingUsageCountsSelfInitiatedChangesInWindow | Rate limit + tables-per-window enforced (rolling 24 h). DEFERRED: RefusalSet / LockDurationCeilingMS / SerializeMode — enforcing `non_dup_object_drop`/`unrollbackable` would forbid unused-index drops and autovacuum tuning outright; product decision. RowsRewritten/StorageBytes usage not measured (0) |
| G4-B18 | FIXED | 144d606, ad821fb | TestFeatureForFindingDerivesChangeClassFromContract | |
| G4-B19 | FIXED | ad821fb | TestRecentActionsSafeForConcurrentUse | Mutex; no `-race` available (CGO off on host) |
| G4-B20 | FIXED | ad821fb | TestCustodianBacksOffAfterRepeatedFailures | Exponential backoff; stop after 3 failures / 6 h (durable via action_log) |
| G4-B21 | FIXED (partial) | 909a473, f34e3d7 | TestValidateRejectsSessionSafetyGUCs, TestCustodianRefusesUnparsableWALKeepSize, TestWALKeepSizeCheckAllowsHeadroom | Session-timeout GUCs removed; WAL keep-size bounded. Per-GUC value bounds for the other allowlisted GUCs remain in advisor/docground only |
| G4-B22 | FIXED | 909a473 | TestValidateRejectsSessionStateStatements | SET/RESET removed |
| G4-B23 | DEFERRED | — | — | Cross-area: `analyzer/rules_index.go` must quote identifiers (`pgx.Identifier{...}.Sanitize()`); analyzer is owned by another agent |
| G4-B24 | DEFERRED | — | — | Two grammars remain by design: policy-document windows use `policy.ParseWindow`, `trust.maintenance_window` uses its documented legacy grammar (presets, 1-hour cron). Unifying changes policy-document semantics (cron minute width, timezone field) — product decision |
| G4-B25 | FIXED | ad821fb | TestResumeOrphanedMonitorsFinishesInterruptedActions | Resumed once per executor on first RunCycle |
| G4-B26 | DEFERRED | — | — | Decision-row retention left to the collection agent (retention package) as instructed |
| G4-B27 | FIXED | ad821fb, 09fff77 | TestExecuteManualRecordsDecisionAndDatabase, TestSelfAuditIgnoresPendingAndHistoricRows | Operator actions record an `operator_approved` decision; audit skips in-flight rows, 24 h window |
| G4-B28 | FIXED | 0598b51, ad821fb | TestZeroWriteBaselineIsNotWriteRegression, TestPerQueryRegressionWithoutDataIsUnverifiable | |
| G4-B29 | FIXED | ad821fb | TestExecuteManualWaitsForSharedDDLSlot | One semaphore for RunCycle, manual, custodian, verified index, rollbacks |
| G4-B30 | FIXED | ad821fb | TestExecuteManualKeepsUnrelatedInvalidIndexes | Only an invalid remnant with the exact (or PostgreSQL-default) name is dropped |
| G4-B31 | FIXED | ad821fb | TestMonitorRollbackIsConcurrentAndLockBounded | Custodian ALTER TABLE and all rollbacks use lock_timeout |
| G4-B32 | FIXED | ad821fb | TestVerifiedIndexProposalReauthorizesAfterAdmission | |
| G4-B36 | FIXED | ad821fb | TestPolicySnapshotWaitsForHotReloadWriter | Also `TrustLevel()` |
| G4-B37 | FIXED / NOT A BUG | 6fc9800 | — | 6-hourly rollout warning removed with R06. The startup warning is accurate again after B02 (moderate needs a configured window), so it stays |
| G4-B38 | FIXED | 4cd9317 | TestFreezeRatesNeedTwoSamples, TestFreezeProposalOmitsDeadlineWhenRateUnknown | Snapshot-xmax XID rate, own multixact rate, no deadline override on first tick |
| G6-B03 | FIXED | ad821fb | TestExecuteManualSurvivesCallerCancellation | Caller ctx bounds only the DDL-slot wait; execution/logging detached with DDL deadline (also RollbackAction; covers G4-B33) |
| G3-B05 | FIXED | ad821fb | TestVerifiedActionRejectsUnsafeCreateForms | Rollback must target the created index; rollback re-derived deterministically |
| G3-B20 | FIXED (partial) | ad821fb | TestVerifiedActionRejectsUnsafeCreateForms | Executor rejects UNIQUE in verified creates. Binding DDL to the analyzed table belongs in `optimizer/validate.go` — cross-area |
| C14 | FIXED | ad821fb | TestMonitorMissingEvidenceIsNotSuccess, TestEvaluateRegressionTriState | Tri-state verdict; unverifiable is never credited |
| C15 | FIXED | 0598b51, ad821fb | TestRevertVerdictRemainsRetryableUntilEffectCompletes, TestResumedRevertReturnsStoredVerdictWithoutReobserving, TestFailedRevertKeepsVerificationRetryable | Revert verdict stays open (retry every 5 min) until the executor completes it |
| C17 | FIXED | ad821fb | TestLegacyHealthQueriesAreScopedToCurrentDatabase | Workload normalization of write latency not changed |
| SURF-03 / G2-B11 | FIXED (partial) | ad821fb, 09fff77 | TestExecuteManualRecordsDecisionAndDatabase, TestReadSnapshotToleratesUnattributedCredit | database_id stamped when known (meta-db mode); value query COALESCEs. Fleet aggregation of value across pools is in `api/router.go` — cross-area |
| R06 | FIXED | 6fc9800 | — (deletion) | Scheduler stub + RunPeriodic removed; engine/runtime kept (tested components, unwired) |
| G4-D03, D06, D20 | FIXED | e2fa96f, 909a473 | — | Dead code removed |

## Cross-area edits

- `cmd/pg_sage_sidecar/main.go`: removed the `startFleetRolloutScheduler()` call (R06).
- `internal/mcp/postgres_access_integration_test.go`: live-test gate fixture supplies the
  new `RuntimeState` fields.
- `internal/api/action_handlers_test.go`: rollback fixture uses `ANALYZE` instead of `SET`.
- Needed from other owners (not done): quote identifiers in `analyzer/rules_index.go`
  (B23, C16) and emit `CREATE INDEX CONCURRENTLY` rollbacks there (B06 at source); bind
  optimizer DDL to the analyzed table and derive `drop_ddl` (G3-B20/G3-B05 at source);
  lock-chain findings carry no `backend_start/query_start/query_id` evidence, so approved
  lock-chain cancels are refused as stale until `analyzer/rules_lockchain.go` records them;
  `sage.table_contract.retention_column` for explicit retention columns (R03); value API
  aggregation across fleet pools (G2-B11); decision retention (B26).

## Test Results

**Command:** `go test -count=1 -cover -v <pkgs>` and the same with `-tags=integration`,
`SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`,
pkgs = executor, policy, autonomy, custodian/..., verify, ledger, value, rollout, clone, mcp,
cmd/pg_sage_sidecar. Also `go test -count=1 [-tags=integration] ./internal/api ./internal/fleet`
(ok), `./internal/schemaguard ./internal/analyzer ./internal/optimizer` (ok).
`go build ./...` and `go vet ./...` clean.

**Total:** 1190 passed, 0 failed, 0 skipped (both unit+DB and integration runs).

**Coverage:**

| Package | Coverage |
|---|---|
| internal/executor | 78.2% |
| internal/policy | 83.9% |
| internal/autonomy | 79.6% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/verify | 85.8% |
| internal/ledger | 91.4% |
| internal/value | 91.1% |
| internal/rollout | 80.4% |
| internal/clone | 80.0% |
| internal/mcp | 77.1% |
| cmd/pg_sage_sidecar | 43.8% |

### Skipped Tests (must be zero or justified)
- None (grep of `--- SKIP` in both runs: 0).

### Failures (if any)
- None in the final runs. During the run one new test
  (`TestOldestXminBlockerCapturesBackendIdentity`) was order-sensitive: it assumed its
  session held the oldest xmin in the fixture DB. Fixed in `00df565` (test logic error,
  explained in the commit).

### Coverage Gaps (packages below threshold)
- cmd/pg_sage_sidecar: 43.8% (pre-existing; `main` wiring — startup/fleet functions of
  200–500 lines are untested). Only business-logic packages are held to 70%; all of those
  in scope meet it.

### Bugs Found This Session (beyond the review findings)
1. [BUG] verified-index revert would have been withheld forever once B01 was fixed (the
   revert DROP matched the approval-gated `drop_unused_index` contract) — fixed with the
   identity-bound `revert_created_index` contract.
2. [BUG] `updateActionSuccess` could flip `rolled_back`/`failed` rows to `success` and
   credit value — now compare-and-set.
3. [BUG] runaway detection scanned every database's backends, not the executor's own.
4. [BUG] several legacy coverage tests asserted autonomous execution of approval-guarded
   actions (REINDEX CONCURRENTLY, autovacuum ALTER TABLE) — they encoded G4-B01.

### Manual Checks Remaining
- CHECK-01: MANUAL — run `go test -race` for executor on a host with CGO (B19/B36 were
  verified structurally: concurrent-map stress test and hot-reload lock test).
- CHECK-02: MANUAL — review whether `created_at`/`occurred_at` inference is acceptable until
  the schema gains an explicit retention column.
