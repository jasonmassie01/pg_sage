# Sage SRE M7: earned autonomy (R3)

Branch `claude/sre-m7-autonomy` (based on the M3 branch, PR #58, at `86a5290`). Spec:
`reviews/2026-09-26/AI-SRE-SPEC.md` §4 R3, §7.2, §7.3, §12 (CHECK-40) and §13 M7. Decisions are
in `~/.claude/tasks/todo-2026-10-02-m7-autonomy.md` and are repeated below.

## What was built

1. **Autonomy ledger** (`internal/earned`). Levels L0 to L4 are kept per incident family and
   action class, in the control pool (the meta database when one is configured), keyed by
   deployment. The current level, proposals, append-only history and append-only live outcomes
   are all durable. Every pair starts at L1. Caps are enforced twice:
   - in the ledger, by class: irreversible classes stay at L1; mitigation-only classes and
     `config_guc` stay at L2;
   - in the gate, by the contract's own rollback class.

   L4 is reserved and can never be reached. A schema CHECK limits levels to 0 to 3.
2. **Promotion from evidence.** The service proposes one step at a time, and only when the
   evidence meets the spec:
   - **Bench:** the newest PGIncidentBench report that scored the family, on every gated arm
     cell. It needs top-1 ≥ 0.80 with n ≥ 10, mechanism precision ≥ 0.90 and no forbidden
     actions, and the report must be ≤ 30 days old.
   - **Shadow:** at least 30 days, with ≥ 20 reviewed packets and ≥ 95% accepted.
   - **Safety:** no violations in the family in 30 days.
   - **For L3, additionally:** Safe Pass ≥ 0.95 with n ≥ 10, a game-day Safe Pass ≥ 0.95 if any
     game days ran, ≥ 50 verified L2 recoveries, and no harmful outcome ever for the pair.

   Only a human admin can approve, and the approver is recorded. pg_sage, the system, MCP and
   stdio actors are refused. Approval re-checks the evidence, and proposals expire after 7
   days. The thresholds are fixed in code.
3. **Automatic downgrade to ≤ L1 (CHECK-40).** The limiter evaluates the signals at
   authorization time, and only when the granted level is ≥ L2:
   - **Error budget** (the M5 summary adapter): any page-level burn, database proxies
     included; or a registered app SLO whose state is unknown; or a source error.
   - **HA:** role not primary, safe mode, a role change within 30 minutes, or no HA source.
   - **Evidence:** missing, older than 5 minutes, or dated more than 1 minute in the future.
   - **Concurrency:** another active lease on the object, an action executed on it within 15
     minutes, or an error / no target.
   - **Safety:** a harmful or safety-violation outcome in the family within 30 days.

   Each cap and each clear is logged once per transition and database (`capped` /
   `cap_cleared`). A harmful outcome also durably demotes every class of the family to L1, so
   re-promotion needs the evidence again.
4. **Gate enforcement** (`policy.restrictAutonomy`). The ledger can only restrict:
   - L0/L1 → `observe_only` (reason `autonomy_level`, or `autonomy_downgraded`).
   - L2 → `queue_approval` (`autonomy_handoff`). Executor custodians turn this into an
     `autonomy_handoff` finding plus an action_queue item, deduplicated against pending items
     and against rejects within 24 hours, and send one `approval_needed` notification.
   - L3 → execute (`autonomy_l3`), only for a reversible action on exactly one target with both
     the standing-policy window and the configured window open. A deadline override does not
     stand in for the window. Each L3 execution sends one `auto_executed` notification.

   Operator trust settings and the standing policy remain the outer bound.
5. **Game days** (`internal/gameday`, `sre-bench/gameday.go`). PGIncidentBench fault programs
   run on a disposable clone: the clone provider (DLE or snapshot), or `local_dsn` as a dev
   fallback that refuses any monitored database. Only the deterministic arm runs. A forbidden
   action is recorded as a `safety_violation` outcome. The clone DSN is never stored. Game
   days are off by default and single-flight.
6. **Fleet canary** (`internal/rollout`):
   - An admin starts it from a verified action on one database.
   - It applies to the canary databases first, verifies and measures them, then widens.
   - A target is applied only where that database has its own open finding with the same SQL,
     executed through `ExecuteManual`, so it is the operator's approval on that database's
     gate.
   - It halts and rolls back every applied instance in reverse order on a failed
     verification, an aggregate canary regression, or a regression of any instance after the
     canary. Rollback runs under `context.WithoutCancel`.
7. **API, MCP and UI:**
   - API: `/api/v1/sre/autonomy`, with view, history, proposals, evaluate, approve (admin),
     reject, downgrade, reviews, outcomes, bench upload (admin), game days and rollouts.
   - MCP: `sre_get_autonomy` and `sre_downgrade_autonomy` (operator). There is no approval
     tool.
   - UI: **Advanced > Earned autonomy**.

## Product decisions (recorded)

| # | Decision | Why |
|---|---|---|
| D1 | The ledger is deployment-wide per family×class, stored in the control pool. | One trust record per fleet. Operator per-database settings stay the outer bound. |
| D2 | It governs only self-initiated requests that carry `IncidentFamily`. Operator-approved requests and read-only diagnostics are not restricted, and family-less optimizer actions stay on the trust ramp. | Approval is already the human step, and the trust ramp is the existing contract for index work. |
| D3 | Custodians are tagged: freeze, freeze_blocker and autovacuum_tuning → `wraparound_runway`; wal → `wal_retention`. **Default L1.** | Conservative default. **Behavior change**: a custodian that auto-executed on the trust ramp now writes a script until promoted. |
| D4 | `sre.autonomy.enforce` defaults to true. Setting it to false is an explicit opt-out with a startup warning. It is YAML-only and cannot be changed over the API. | The safe default must be hard to switch off silently. |
| D5 | Caps: reversible / no-rollback-needed → L3; mitigation_only → L2; everything else → L1. `wal_bound` is mitigation_only. `config_guc` is capped at L2. | An invalidated slot cannot be undone, and global GUCs were excluded through R2. |
| D6 | L3 needs exactly one target and an open window, and a deadline override does not substitute for the window. | The spec says "reversible, bounded actions in the window". |
| D7 | Downgrade signals are transient and evaluated only at ≥ L2. A harmful outcome causes a durable family-wide demotion. | L0/L1 already never execute. |
| D8 | Error-budget mapping (coordinator instruction): configured = `AppSLOs > 0 \|\| FastBurning`; downgrade on `FastBurning` (proxy burns included) or a non-empty `UnknownApp`. An unknown proxy SLO does not downgrade, and neither does a nil budget source or having no SLOs. | Proxy SLOs are often unknown for structural reasons and would otherwise pin every database at L1. |
| D9 | Promotion is one step at a time, approved by an admin human only, with evidence re-checked at approval and a 7-day TTL. Thresholds are not configurable. | The approver is the trust step, and the evidence bar is the spec's. |
| D10 | Game days run only the deterministic arm, on a clone or a non-monitored local DSN. They are off by default. | No model tokens are spent on drills, and customer databases are never touched. |
| D11 | The canary is admin-started. Each target must have its own matching open finding and executes through `ExecuteManual`. | Never apply a fix a database did not itself recommend. |
| D12 | MCP gets read access and operator downgrade, and no approval tool. | Promotion stays with a human in the UI or API. |
| D13 | The M7 tables are exempt from age-based retention. | They hold promotion evidence, and the "no harmful ever" check needs it. |

## CHECKs covered

| CHECK | Status | Tests |
|---|---|---|
| CHECK-40: autonomy auto-downgrades on burn, failover, stale evidence and concurrent action | PASS | `TestEachDowngradeSignalCapsAtL1` (14 signal cases), `TestSummaryBudgetDowngradesThroughTheLimiter`, `TestDowngradeBoundariesAreExclusive`, `TestMissingSignalSourcesFailClosed`, `TestCapTransitionsAreLoggedOnce`, `TestAutonomyDowngradedNeverExecutes`, `TestSafetyRegressionDemotesTheFamilyDurably` |
| §13 M7: promotion only with evidence | PASS | `TestAssess*` (9), `TestNoEvidenceNoProposal`, `TestApprovalRechecksEvidence`, `TestOnlyAHumanCanApprove`, `TestPromotionIsOneStepAndStopsAtTheCap`, `TestAutonomyAPI_PromotionIsProposedThenApprovedByAnAdmin` |
| §13 M7: game days on clones | PASS | `TestGameDay*` (9), `TestLocalProviderRefusesMonitoredDatabases`, `TestRunGameDayProducesAnIngestibleReport` |
| §13 M7: fleet canary halt and rollback | PASS | `TestCanary*` (6), `TestAggregateRegressionRollsBackTheCanaries`, `TestInstanceRegressionAfterTheCanaryHaltsAndRollsBack`, `TestRollbacksSurviveCancellation` |

## Gate-enforcement tests: no premature autonomy

| Test | Proves |
|---|---|
| `policy.TestAutonomyGateNeverExecutesPrematurely` | Exhaustive matrix of **18,144 combinations**: level {-1,0,1,2,3,4,9} × downgraded × 8 rollback classes × 3 trust levels × 3 execution modes × 3 risk tiers × window open/closed × 0/1/2 targets. An action executes **iff** L ≥ 3, not downgraded, reversible or no-rollback-needed, the operator allows it, the window is open and there is exactly one target. Every execution carries reason `autonomy_l3`. |
| `policy.TestAutonomyLevelsMapToVerdicts` | L0/L1 → observe_only, L2 → queue_approval, L3 → execute. |
| `policy.TestAutonomyL1WithholdsTheHandoff` | L1 never queues an approval, even where the operator's base verdict was queue_approval. |
| `policy.TestAutonomyDowngradedNeverExecutes` | Downgraded at a granted L3 → observe_only (`autonomy_downgraded`). |
| `policy.TestAutonomyGateCapsByReversibility` | A ledger that wrongly reports L3 still cannot execute irreversible, forward-fix, application or unknown classes, and mitigation_only stops at L2. |
| `policy.TestAutonomyReservedAndBogusLevels` | L4 and L99 act as L3 (reason `autonomy_l3`), negative levels as observe-only. |
| `policy.TestAutonomyNeverWidensTheOperatorBound` | At L3: approval mode only queues, manual mode and observation trust observe, and the trust ramp, emergency stop, replica and disallowed change class all still block. |
| `policy.TestAutonomyL3RequiresTheWindow` / `TestAutonomyL3IgnoresDeadlineOverride` / `TestAutonomyL3RequiresExactlyOneTarget` / `TestAutonomyL3ClearsTheOffWindowFlag` | L3 needs the window (an override does not count), exactly one object, and keeps no off-window flag. |
| `policy.TestAutonomyLedgerFailureFailsClosed` | A ledger error → blocked (`autonomy_unavailable`). |
| `policy.TestAutonomyExplainMatchesAuthorize` | Explain and Authorize agree. |
| `earned.TestIrreversibleClassesNeverExceedL1` / `TestProductCallCaps` | Class caps by reversibility (every class), product-call caps; the schema CHECK refuses a level above 3 (`TestSREMigrationM7_LevelBoundsAreEnforced`). |
| `earned.TestAssessL4IsNeverMet` / `TestPromotionIsOneStepAndStopsAtTheCap` | No evidence reaches L4, and no proposal goes beyond the cap. |
| `earned.TestCapsApplyBeforeAnyGrant` / `TestDecayedEvidenceLowersTheEffectiveLevel` | The cap and the evidence decay bound the effective level. |
| `executor.TestWithAutonomyRestrictsTheStandingGate` / `TestL1VerdictIsAManualScriptNotAHandoff` / `TestL2VerdictQueuesOneApprovalHandoff` | Custodians execute nothing at L1 and only queue at L2. |
| `cmd.TestInstallAutonomyRestrictsCustodiansByDefault` / `TestInstallAutonomyFailsClosedWithoutALedger` | The default wiring restricts, and a missing ledger blocks family actions. |
| `executor.TestReauthorizationUnderTheLeaseSaysSo` | The executor's own lease is not counted as a concurrent action. Another holder's lease is (`TestPostgresConcurrencyCountsOtherWriters`). |

## Test Results

**Command:** `go test -cover -count=1 ./...` (PG17, `pgsage-ag7` :55477, Docker golang:1.25), run with `-v` for accounting.
**Total:** 9075 passed, 3 failed, 13 skipped

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/earned | 86.5% |
| internal/earned/hasource | 100.0% |
| internal/policy | 89.5% |
| internal/executor | 82.6% |
| internal/ha | 100.0% |
| internal/schema | 81.6% |
| internal/gameday | 87.6% |
| internal/rollout | 83.1% |
| internal/api | 74.4% |
| internal/mcp | 80.7% |
| internal/config | 87.0% |
| internal/retention | 100.0% |
| internal/store | 74.2% |
| cmd/pg_sage_sidecar | 72.3% |
| sre-bench | 70.1% |

All touched packages meet the coverage thresholds.

**Other runs:**
- Touched packages on PG14 (:55414) and PG18 (:55418): 3421 passed, 0 failed, 1 skipped each, with the same coverage.
- `-race` on the touched packages (PG17): all 15 packages ok, with no data race reported.
- Web: `npm test` gives 204 passed in 40 files, and `npm run build` succeeds. The rebuilt dist is committed.
- `golangci-lint run ./...` reports 0 issues.

### Skipped Tests (must be zero or justified)
All 13 are pre-existing and gated by environment variables. None is in an M7 package.
- `TestAWSRDSLiveProvisioning`, `TestCloudSQLLiveProvisioning`, `TestLakebaseLiveProvisioning`, and the 3 `TestAgentDBLiveGauntlet*` tests need live cloud credentials.
- `TestAzureLiveServerParameter` and `TestAzureLiveRestartBoundParameter` need `PG_SAGE_LIVE_AZURE`.
- `TestChatWithToolsLive_RealProvider` and `TestTier2Live_RealGemini` need `PG_SAGE_LIVE_LLM`.
- `TestResolveLogDir_AbsoluteWindows` only applies on Windows.
- `TestRCAChildProcessFixture` is a helper that only runs as a child process.
- `TestPGIncidentBench` needs `SAGE_BENCH_RUN=1`; CI runs it in its own step. This is also the 1 skip on PG14 and PG18.

### Failures (if any)
- internal/sre: `TestStore_StaleWorkerCannotCommit`, `TestModelProbe_ResumedRunWithTurnsUsedFallsBack`, `TestCollect_ResumeAfterNeedsEvidenceConcludes`. These are **pre-existing timing flakes, not M7**:
  - M7 does not change `internal/sre`.
  - They depend on a 300 ms lease TTL, and the machine was loaded by four parallel milestone agents.
  - The same tests fail intermittently on the pre-M7 code (4 runs: `TestCoordinator_ResumesAnOrphaned…` failed twice).
  - `go test -count=1 ./internal/sre/` alone on this branch passes (88.0%).
- `executor.TestApplyLockCeiling*` was intermittent earlier in the session (timing under load). It passed in the full run.

### Coverage Gaps (packages below threshold)
None. All touched packages are at least 70%.

### Bugs Found This Session
1. [BUG] `earned/store_evidence.go` `LatestBench`: when two reports tied on time, it could pick one that did not score the family. It now takes the family and filters with JSON containment. Found by `TestSafetyRegressionDemotesTheFamilyDurably`.
2. [BUG] `retention/cleanup.go`: the eight M7 tables had neither a purge rule nor an exemption. Found by the full suite (G7-B12 guard) and fixed in `07cadee`.
3. [BUG] The new `sre.autonomy.*` keys were not registered as YAML-only in the config-override allowlist. Found by the full suite (`TestConfigConsistency_AllowedKeysMatchStruct`) and fixed in `24d9ac5`.
4. [BUG] `earned` ↔ `ha` import cycle (ha tests → executor → earned → ha). The HA adapter moved to `earned/hasource`.
5. [GAP] Mutation P6: no test failed when an L3 execution kept a deadline override's `off_window_ok` flag in its evidence. Pinned by `TestAutonomyL3ClearsTheOffWindowFlag` (`e108912`).
6. [TEST BUG, explained in commits]
   - The gameday fixture clock was not shared with the ledger (`5de6199`).
   - Policy tests expected a windowless document to be valid, but the gate fails closed on it (`a61d073`).
   - An executor test called `requireDB` twice and deadlocked on its advisory lock (`a3a8d16`).
7. [CONFIG] An `Enforce` doc tag over 200 characters broke `gen_config_meta`, and the lifecycle doc had drifted. Both are fixed and regenerated.

### Mutation testing
13 hand-written mutants on the gate, ledger, limiter, reconciler and budget adapter. 12 were killed on the first run. P6 survived, so a test was added, and the mutant is now killed:

| Mutant | What it changed | Result |
|---|---|---|
| P1 | L3 target check `!= 1` → `> 1` | killed |
| P2 | Downgraded clamp L1 → L2 | killed |
| P3 | mitigation_only cap L2 → L3 | killed |
| P4 | Configured window ignored | killed |
| P5 | L3 widens an operator queue_approval to execute | killed |
| P6 | L3 keeps the off-window flag | survived → test added → killed |
| E1 | slot_drop cap L1 → L2 | killed |
| E2 | L3 recoveries 50 → 49 | killed |
| E3 | Shadow volume 20 → 19 | killed |
| E4 | Any non-empty approver accepted | killed |
| E5 | Stale-evidence boundary +1 s | killed |
| E6 | `rolled_back` not harmful | killed |
| E7 | Proxy-unknown counted as unknown | killed |

### Manual Checks Remaining
- CHECK-M7-UI: MANUAL. The Earned autonomy page in dark mode, and the approve/downgrade flows against a live sidecar. Component tests cover the behavior, but the look needs a browser.

## Post-test audit

1. **What input would break this that is untested?**
   - A bench report larger than 8 MiB is rejected by size, but reports under 8 MiB that
     arrive in volume are not rate-limited. They are deduplicated by hash.
   - HA role history before the sidecar started is not known. The monitor only sees flips it
     observes, so a failover just before a restart is invisible to the 30-minute cooldown.
2. **Behavior with no assertion.** The UI's game-day and rollout lists are rendered but only
   asserted empty. The API tests cover their payloads.
3. **Assertions that would pass if broken?** None found. Every gate test asserts the verdict
   and the reason, and the matrix asserts the reason on every execution. The reconciler tests
   assert the outcome rows and the demotion.
4. **Mocks that hide failures.**
   - Gate tests use `fakeLimiter`. The real limiter is covered against Postgres in
     `limiter_db_test`.
   - Game-day tests use a fake provider. The real local provider and `sre-bench` `RunGameDay`
     run against Postgres, but DLE and snapshot clones are not exercised live, since there is
     no provider locally.
   - The canary target is tested against a real database (`TestFleetCanaryTargetReadsTheDatabase`).
     The full fleet path through `ExecuteManual` is covered by the canary service tests with a
     fake target.
5. **Shared-file limits.** Additive edits pushed `mcp/server.go` `callTool` from 56 to 59
   lines; it was already over 50 before M7. `router.go`, `executor.go` and `bootstrap.go` were
   already over 500 lines. All new files are ≤ 500 lines and all new functions are ≤ 50 lines.

## What's left

- **M5 budget wiring:** `earned.NewSummaryBudget` expects a source with the same fields as
  `slo.BudgetSummary`. Integration is a field copy over `inst.SLO.BudgetSummary(ctx)` into
  `Binding.Budget`. Until then the budget source is nil, which means "no SLOs" (no downgrade).
- **M5 actions:** `cancel_backend` needs rollback class `mitigation_only` from M5
  (`policy.RollbackMitigationOnly`). The ledger already maps `backend_cancel` to L2.
  M5's `request_execution` and recovery paths should set `IncidentFamily` and call
  `RecordOutcome`, so their live outcomes count.
- **M6 families** (checkpoint_storm, temp_file_explosion, replication_lag, lwlock_contention,
  wraparound_runway, disk_wal_runway, sequence_runway) are already in the family table. Their
  remediations need to tag `IncidentFamily` when they land.
- **Game days** use the bench's own fixture tables inside the clone. Parameterized faults on
  customer tables are a follow-up.
- **`sre_eval_runs`** can hold reports up to 8 MiB each. Reports are deduplicated, but a
  retention policy for superseded reports is a follow-up.

## Coordinator decisions needed

1. **Custodian behavior change (D3).** Autonomous custodians stop auto-executing until a pair
   is promoted. The alternative is to seed the custodian pairs at L3 on upgrade. That would
   grant autonomy without evidence, so I did not do it.
2. **Enforce opt-out (D4).** Default true. Setting it to false restores the trust ramp, with a
   startup warning.
3. **Budget unknown (D8).** Implemented as instructed: only an unknown app SLO downgrades, and
   a proxy fast burn does downgrade.
