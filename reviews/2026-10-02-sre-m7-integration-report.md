# Sage SRE M7 integration report (earned autonomy on M5 + M6)

Date: 2026-10-02. Branch `claude/sre-m7` (worktree `pg_sage-m7`), from `origin/claude/sre-m6`
(`e6f130d`: M3 + M4 + M5 actions + M5 SLO + M6 reactive, runways and runbooks, graph
`causal-v3`). Not pushed. Test database: my PG17 `pgsage-ag7` (:55477, repo root mounted);
shared matrix PG14 (:55414) and PG18 (:55418). M7 design, product calls (D1–D16) and the
gate-enforcement table are in `reviews/2026-10-02-sre-m7-autonomy-report.md` (merged with the
branch).

## What was merged

| Commit | What |
|---|---|
| `2938a6d` | merge `claude/sre-m7-autonomy` (`f193624`) |
| `c42e33e`, `e1e98c6`, `efd6bc7` | tests first for the wiring; audit test; table split (formatting) |
| `b69484a` | M5 `ActionOutcome` carries the executed cancel's `action_log_id` |
| `7b571f2` | earned: M5 budget adapter (`earned/slobudget`), reconciler records every executed family action, `ClassForActionType`, `RecordOutcomeOnce` |
| `8c92542` | runbook action proposals name their family and autonomy class |
| `ca26908` | sidecar wiring: budget per database, M5 outcome feed, bench shard walk |
| `a38b951`, `a74f760` | docs, config doc tag, generated config metadata, web dist, CHANGELOG |
| `250a6da` | web test uses `globalThis` (eslint `no-undef`) |

No rebase, no force-push. After the merge: conflict-marker scan
(`git grep '^<<<<<<< \|^>>>>>>> \|^=======$'`: none), `go build ./...`, `go vet ./...`.

## Conflicts and resolutions (both sides kept)

- **`config/sre.go`**: `SREConfig` has SLO, ChangeEvents, Actions, Runways (M5/M6) and
  Autonomy (M7). The defaults keep `AutomaticStart: true` (M4) plus every section, and
  validation chains SLO, ChangeEvents, Runways, then Autonomy.
- **`schema/bootstrap.go`**: `... ddlRunwaySamples, ddlSRERunbooks, ddlSREAutonomy,
  ddlSREAutonomyCarryOver`. The M7 DDL depends only on `sre_deployments`,
  `sage.sre_append_only()` and `rollout_run`, all of which are created earlier.
- **MCP** (`server.go`, `principal.go`, `production_backend.go`, `mcp_runtime.go`): the tool
  list is intent + SRE + M5 action + signal + runbook + autonomy. Dispatch adds the autonomy
  tools after the signal and action tools. `sre_downgrade_autonomy` joins M5's mutating
  tools. The backend has Signals, Actions and Autonomy.
- **`api/router.go`**: signal routes and autonomy routes. **Name collision**: M6 runbooks
  declared `decodeBody` and `actorOf` in package `api` (with different signatures). The
  autonomy helpers are renamed `decodeAutonomyBody` / `autonomyActor`.
- **`retention/cleanup.go`**, **`store/config_consistency_test.go`**: union of the
  exemptions (runway samples plus the eight M7 tables) and of the YAML-only keys (`sre.slo.*`,
  `sre.change_events.*`, `sre.actions.*`, `sre.runways.*`, `sre.autonomy.*`).
- **Web** (`App.jsx`, `Layout.jsx`): SLOs, Runbooks and Earned autonomy routes and nav.
- **CHANGELOG, docs/configuration.md**: union (the earned-autonomy section follows runways).
- **Generated**: `config_meta.json` and `config-lifecycles.md` were regenerated with
  `gen_config_meta`. Both sides' dist bundles were discarded; after `npm ci`, the dist was
  rebuilt from the merged sources. `index.html` references `assets/index-B7u3leqJ.js` and
  `assets/index-CTgYmBWX.css`, the only two files in `dist/assets`.

## Wiring the M7 report listed as left

1. **M5 error budget.** `earned/slobudget.New(rt.sloEngine)` is each database's
   `Binding.Budget`. The SLO engine is built with the investigator in `startMonitoring`, so
   it exists before `startExecution` installs the ledger. The mapping is the coordinator's:
   - a page-level burn of any SLO, proxies included, downgrades;
   - a non-empty `UnknownApp` downgrades;
   - an unknown proxy alone is no downgrade;
   - SLOs disabled (nil engine) means no budget source, so no downgrade.

   The adapter refuses a summary whose database is not the bound one. Tested on the **real
   SLO engine** (`slobudget_db_test.go`): unknown proxy only; unknown app beside an unknown
   proxy; proxy fast burn from seeded samples (20x); app fast burn; no SLOs; another
   database.
2. **M5 `cancel_backend`.** It is mitigation-only (M5's contract), capped at L2 in the class
   table and by the contract's rollback class, and not carried over. A ledger row forced to
   L3 still acts at L2 (`TestCancelBackendIsApprovalOnly`). M5 cancels run only after human
   approval (`operator_approved`), so the ledger never restricts them.
   - **How runs reach the ledger.** An outcome feed (`autonomy_action_feed.go`, every
     reconcile interval, 30-day lookback) records approved runs as **L2 outcomes** of
     `lock_blocking` or `connection_pressure` / `backend_cancel`.
   - **What counts.** A recovery that pg_sage caused is a verified recovery (promotion
     evidence). A failed run or a recovery that did not happen counts as not recovered.
   - **What is skipped:** recoveries caused by someone else or with an unknown cause,
     inconclusive or still-running verifications, refusals, and runs without an action_log
     row. Each run is recorded once, keyed by action_log id.
3. **Custodian and runway remediations.** The M6 runway advisor already explains custodian
   proposals through `custodianRequest`, which carries `IncidentFamily` (freeze →
   `wraparound_runway`, WAL bound → `wal_retention`), so its verdicts are the ledger's.
   - The reconciler now records **every executed self-initiated family action**: L3 runs at
     L3 (notified) and mandatory deadline overrides at **L1**. L1 outcomes never count as
     promotion evidence, but a harmful one is a family safety regression that demotes the
     family's earned pairs.
   - Operator approvals are left to the handoff path; an audit test was added after
     mutation I12 survived.
4. **Runbook proposals.** A typed action proposal records `family` (the diagnosis family) and
   `autonomy_class` (`earned.ClassForActionType`; `vacuum_table` outside a freeze is
   `vacuum`, and diagnostics have none). Runbook proposals are still never executed; a
   request for one is judged under that pair.
5. **M6 families.** All 11 families are in the ledger and the view, with their classes
   (`TestViewListsTheM6Families`).
   - **Bench shards.** `bench_results_path` is walked three levels deep, so pointing it at
     CI's `pgincidentbench/` ingests the core, reactive and runway shard reports. Each family
     reads the newest report that scored it (`TestBenchShardReportsFeedTheirFamilies`,
     using reports written by `srebench.WriteReport`).
6. **Event types.** M7's history is in its own table, `sre_autonomy_events`, whose check
   was rebuilt as `sre_autonomy_events_type_v2` (union, 11 types). M7 adds nothing to
   `sre_events`. The M6 union test (`schema/sre_event_types_union_test.go`, which reads the
   Go `Event*` constants of `internal/sre` and the action `typ` literals) passes unchanged:
   20 types and a single check.

## Mutation testing (new wiring)

| Mutant | Change | Result |
|---|---|---|
| I1 | budget copies AppFastBurning as FastBurning (proxy burn lost) | killed |
| I2 | UnknownApp copied from Unknown (proxy unknown downgrades) | killed |
| I3 | deadline overrides recorded at L3 | killed |
| I4 | deadline overrides notified as L3 | killed |
| I5 | external recoveries credited | killed |
| I6 | M5 runs recorded at L1 | killed |
| I7 | shard subdirectories skipped | killed |
| I8 | binding budget not passed to the limiter | killed |
| I9 | runbook proposal family dropped | killed |
| I10 | every action type mapped to freeze | killed |
| I11 | M5 feed reads one hour instead of the safety window | killed |
| I12 | operator approvals recorded as self-initiated | survived → test `e1e98c6` → killed |

## Test Results

**Command:** `go test -count=1 -cover -timeout 60m -v ./...` (Docker `golang:1.25`, repo root
mounted, PG17 `pgsage-ag7`); matrix `go test -count=1 -cover -v` over the touched packages on
PG14 and PG18; `go test -race -count=1` over the touched packages on PG17.
**Total (full suite, PG17):** 10121 passed, 3 failed, 13 skipped. All three failures pass on a
package rerun (see Failures).
**Coverage (touched packages, PG17):**

| Package | Coverage | Package | Coverage |
|---|---|---|---|
| internal/earned | 87.4% | internal/api | 76.0% |
| internal/earned/slobudget | 85.7% | internal/mcp | 79.7% |
| internal/earned/hasource | 100.0% | internal/config | 89.1% |
| internal/policy | 89.7% | internal/retention | 100.0% |
| internal/executor | 83.0% | internal/store | 74.5% |
| internal/schema | 81.6% | cmd/pg_sage_sidecar | 72.4% |
| internal/sre | 86.9% (rerun) | internal/sre/action | 82.4% |
| internal/rollout | 83.1% | internal/gameday | 87.6% |
| internal/ha | 100.0% | sre-bench | 62.2% without the gated bench (as on M6: 60.1%) |

All business packages meet the 70% threshold. `sre-bench` is below 70% only because the gated
bench is skipped (`SAGE_BENCH_RUN`), as on the M6 base. Its M7 game-day code is covered by
its own tests.

**PG14 / PG18 (touched packages, 17 packages):**
- PG14: 4148 passed, 0 failed, 1 skipped.
- PG18: 4147 passed, 1 failed, 1 skipped. The failure is `TestStore_StaleWorkerCannotCommit`
  (the 300 ms lease flake; fixed in PR #61, which is not on this base).
- The one skip is `TestPGIncidentBench`.

**Race (PG17):** earned/..., policy, executor, schema, api, mcp, retention, store,
cmd/pg_sage_sidecar and sre/action all pass, with no `DATA RACE`.
**Lint:** `golangci-lint run ./...` 0 issues.
**Web:** `npx vitest run` 46 files / 246 tests passed; `npx eslint src` clean; `npm run build`
ok.

### Skipped Tests (must be zero or justified)
13, all pre-existing and gated by environment variables: `TestAWSRDSLiveProvisioning`,
`TestCloudSQLLiveProvisioning`, `TestLakebaseLiveProvisioning`, three
`TestAgentDBLiveGauntlet*`, two `TestAzureLive*` (`PG_SAGE_LIVE_*`),
`TestChatWithToolsLive_RealProvider`, `TestTier2Live_RealGemini` (`PG_SAGE_LIVE_LLM`),
`TestRCAChildProcessFixture` (subprocess helper), `TestResolveLogDir_AbsoluteWindows` (Linux
container), `TestPGIncidentBench` (`SAGE_BENCH_RUN`).

### Failures
- **Full suite (PG17), timing under load from other agents' suites:**
  - `internal/sre` `TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep`: the
    300 ms lease, the known flake listed in the M6 report.
  - `internal/sre/probes` `TestCatalog_XIDRunwayCountsConsumption` and
    `TestRunner_SessionSettingsAreFixed`: the probes' 500 ms statement timeout.
  - A package rerun of both passes (sre 86.9%, probes 92.5%). M7 changes neither path (its
    `internal/sre` edit is the runbook proposal's two fields).
- **Merge-time run:** `cmd/pg_sage_sidecar` `TestRuntimeParityEquivalentDatabase`
  (yaml-fleet runtime drain exceeded 20 s under load) passed alone in 4 s, and passed in the
  full suite.

### Coverage Gaps (packages below threshold)
None among business packages; see the `sre-bench` note above.

### Bugs Found This Session
1. [BUG, merge] `api` helpers `decodeBody`/`actorOf` were declared by both M6 runbooks and
   M7 (build break). The M7 helpers were renamed.
2. [BUG, wiring] `bench_results_path` read only `*.json` in the top directory, so CI's shard
   layout (`pgincidentbench/{core,reactive,runway}/pgincidentbench.json`) would have
   ingested nothing. It now walks three levels deep.
3. [GAP, fixed] Mandatory deadline overrides and other non-L3 family executions were not
   live outcomes, so a harmful red-wraparound freeze would not have regressed its family.
4. [GAP, fixed] M5 outcomes had no action_log id, so the ledger could not record each run
   exactly once.
5. [TEST] eslint `no-undef` on `global` in the autonomy page test.

### Manual Checks Remaining
- CHECK-M7-UI: MANUAL. The Earned autonomy page next to the SLOs and Runbooks pages in a
  browser (component tests and the production build pass).

## Post-test audit

- **Inputs not covered:** an M5 run whose verification later flips is recorded by its first
  decided state only; the M5 store does not change a decided recovery, so this cannot
  happen today. A shard report larger than 8 MiB is rejected, as any report is.
- **Fakes:** the M5 outcome feed is tested with a fake outcome source against the real
  ledger. The M5 `ActionService.Outcomes` query, now with `action_log_id`, is covered by M5's
  own tests. The budget is tested against the real SLO engine.
- **Gate-enforcement table:** unchanged and green. The policy package, including the
  18,144-combination matrix and the deadline tests, passed in the full suite.

## Unresolved / for the coordinator

1. **Final base update (done).** Fetched again before finishing: `origin/claude/sre-m6` had
   been pushed with the runway surge fix (`edcbaf0`), the WAL-bound reload fix (`1bfd9ba`),
   the lease test fix (PR #61, via master) and the LWLock composed-test fix. They are merged
   in `02a9701` with no conflicts and no web or dist changes. After the merge:
   - the conflict-marker scan finds none;
   - `go build ./...`, `go vet ./...` and `golangci-lint` pass, with 0 issues;
   - the touched packages pass on PG17: earned/..., policy, executor (83.2%), schema, sre
     (87.1%), sre/causal, sre/action, cmd/pg_sage_sidecar, api, mcp, config, retention and
     store. There are no failures.
2. Load-sensitive tests on the shared machine (lease, probe statement timeout, runtime
   drain) are as listed in the M6 report.
