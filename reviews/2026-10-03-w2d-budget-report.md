# W2-D: policy budget fairness, max_rows_rewritten, one evaluation per park

Branch `claude/w2d-budget-fairness` (from `origin/release/wave1`, merged again at
`cbed8d07`). Three findings from dogfooding lifeos, all in the standing-policy budget path.

## 1. What was built

### Finding 1: hygiene starved performance (separate budgets)

lifeos: about 20 redundant-index drops in leaked `test_*` schemas used the whole 24-hour
`max_tables_per_window = 20`, so two HypoPG-verified indexes (`public.events`,
`public.memories`) were parked for most of a day.

- `internal/policy/budget.go` (new): `BudgetKind` (`performance`, `hygiene`),
  `KindBudget{MaxTablesPerWindow, MaxChangesPerWindow}`, `BudgetKindFor(req)`,
  `Document.Budget(kind)`, and `limitDecision` (moved out of `gate.go`), which now picks
  the request's own budget. It also writes the park detail.
- Classification is deterministic and uses only the typed action contract:
  `drop_unused_index`, `vacuum_table` and `analyze_table` are hygiene. Everything else is
  performance: index builds, reindex, GUCs, autovacuum tuning, hints, reverts, retention.
  A request with no contract or an unknown action type is also performance (fail closed).
  Evidence, `Feature` and LLM output never decide the kind. The ledger overwrites any
  `budget_kind`, `rows_rewritten`, `budget_detail` or `budget_released` key a request
  puts in its evidence.
- `internal/policy/budget_document.go` (new): the wire format. The canonical form is
  `blast_radius: {max_rows_rewritten, performance: {max_tables_per_window,
  max_changes_per_window}, hygiene: {...}}`.
  - Legacy `blast_radius.max_tables_per_window` and
    `rate_limits.max_self_initiated_changes_per_window` are read as the performance
    budget.
  - If a document sets both the legacy field and the `performance` block, the values must
    match; otherwise parsing fails with a conflict error.
  - A missing hygiene block, or a missing field inside it, takes the hygiene default.
  - Negative values are rejected.
- Defaults: the profiles split today's envelope evenly, giving performance 10 tables / 25
  changes and hygiene 10 tables / 25 changes (20/50 in total, as before).
  `max_rows_rewritten` stays 5,000,000 and is shared by both kinds.
- The park reason names the full budget and when it frees. The reason code is unchanged
  (`blast_radius_exceeded` or `rate_limit_exceeded`). `Decision.Detail` and the recorded
  `evidence.budget_detail` read, for example: `hygiene budget full: 19 of 10 tables in
  the 24h window; next frees at 2026-10-04T18:00:02Z`. When nothing can free it, the
  detail says so: `it does not free until the policy limit is raised`.
- Concurrency:
  - `authorizationGate.budgetMu` makes the usage read and the recorded verdict of a
    budget-spending request one critical section.
  - `standingUsage` counts a recorded execute decision whose action has not run yet (an
    "in-flight hold"), so a second candidate sees the slot as taken.
  - `Apply` releases its holds when it returns. Paths that only authorize (rollback,
    created-index revert, superseded cleanup) release immediately.
  - If the sidecar crashes, a hold lasts at most 2 × apply timeout (12 min at defaults).
- `gate_batch.go`: `ExplainBatch` now reads usage once per kind, so hygiene families are
  explained against the hygiene budget.

### Finding 2: `max_rows_rewritten` was never filled

- `internal/executor/rows_rewritten.go` (new): `rewriteTarget(sql)` classifies statements
  from the SQL alone.
  - Rewrites: `VACUUM FULL` (keyword or enabled option), `CLUSTER`, `REINDEX` without
    `CONCURRENTLY`, and `ALTER TABLE` forms that rewrite the heap: column `TYPE`,
    `SET LOGGED`/`UNLOGGED`/`TABLESPACE`/`ACCESS METHOD`, and an added column that is
    generated, an identity or has a non-literal `DEFAULT`. Any function default counts
    as volatile; for example, `now()` is treated as a rewrite (conservative). Literals
    are masked first, so `DEFAULT 'random()'` stays a literal.
  - `estimateRowsRewritten` reads `GREATEST(reltuples, n_live_tup)` at decision time,
    summed over every leaf partition. For `REINDEX INDEX` it uses the index's table.
    `pg_partition_tree` returns nothing for a plain table, a bug the tests caught.
  - A rewrite whose table cannot be resolved is an error, never 0. The gate then blocks
    the change with `policy_unavailable`.
- `standingUsage` returns the window's rows plus the request's own estimate. The window
  rows are summed across both kinds from `evidence.rows_rewritten` on the action's
  decision, and recorded by `ledgerInput` from `Decision.RowsRewritten`.
- Today no self-initiated typed contract rewrites rows: `VACUUM FULL` has no contract and
  the AST validator rejects it, and the `ALTER TABLE` allowlist is `SET`/`RESET` plus
  non-rewriting migration steps. So the limit is defense in depth until a rewriting
  contract exists. The test exercises it through the real usage and ledger behind a gate
  whose SQL validation admits `VACUUM FULL`.

### Finding 3: parked candidates evaluated twice per cycle

- **Root cause**, confirmed read-only on lifeos:
  - Decision 395082 (parked, `blast_radius_exceeded`) was created and repeated 13 ms
    apart.
  - The parked events index collected 50 repeats over about 24 cycles.
  - `processFinding` returned only on `blocked` and `observe_only`. A `park` fell through
    to `Apply`, whose first authorization evaluated the same candidate again. Execute
    verdicts were evaluated three times: routing, Apply's first authorization, and the
    re-authorization.
- **Fix** (`apply_finding.go`, `apply.go`):
  - `processFinding` acts only on execute and queue-approval verdicts.
  - It passes its execute decision to Apply as `ActionIntent.FirstDecision`, which is
    used as the first authorization.
  - Result: a parked candidate is evaluated once per cycle; an executed one is evaluated
    once before its waits and once after them.
- **Tests that would have caught it:**
  - `TestParkedCandidateIsAuthorizedOncePerCycle`: counting gate, park, expects exactly
    one call.
  - `TestParkedCandidateRecordsOneEvaluationPerCycle`: real gate and ledger,
    `repeat_count` equals the number of cycles over 3 cycles.
  - `TestExecutedCandidateIsAuthorizedThenReauthorized`: expects exactly 2 calls.

### Files

- policy: `budget.go`, `budget_document.go` (new); `document.go`, `gate.go`,
  `gate_batch.go`, `types.go` (`Decision.BudgetKind/RowsRewritten`,
  `LimitUsage.RequestRowsRewritten` and `*FreeAt`).
- executor: `budget_usage.go`, `rows_rewritten.go` (new; `standingUsage` moved here from
  `policy_runtime.go`); `apply.go`, `apply_finding.go`, `standing_policy.go`,
  `rollback_runtime.go`, `retained_cleanup.go`, `verified_index_identity.go`.
- Tests:
  - New: `policy/budget_test.go`, `policy/budget_document_test.go`,
    `executor/rows_rewritten_test.go`, `executor/budget_usage_test.go`,
    `executor/budget_gate_test.go`, `executor/parked_once_test.go`.
  - Fixtures: `lifeosPolicy()` now parses lifeos's real stored (legacy) document. Fixtures
    that raise shared-database limits now also raise hygiene: `executor`,
    `api/approval_card_fixture_test.go` and `autonomy/retention_pipeline_test.go`, one
    additive line each.
- Docs: `docs/configuration.md#blast-radius-budgets`, plus the `CHANGELOG.md` bullet
  under `## Unreleased`.

## 2. Product decisions (made here, by the AI-DBA lens)

1. **Reindex is performance.** It is not in the owner's hygiene list, so it fails closed
   to performance. Created-index reverts and retention deletes are also performance.
2. **Rows rewritten is one shared budget.** It is a safety bound on I/O and locking, not a
   fairness one.
3. **Even split for the profiles** (10/25 per kind). This preserves the 20/50 total.
   Legacy documents keep 20/50 for performance and get hygiene 10/25 on top, as the owner
   specified, so their total grows to 30/75. Ratifying the canonical form restores any
   total the operator wants.
4. **Historic decisions without a kind count as performance**, from the fail-closed rule.
   For up to 24 h after the upgrade, lifeos's old drops still count against performance.
   On lifeos today that is moot: the drops are older than 24 h.
5. **A zero change limit keeps its old meaning** (the first change passes). A zero table
   limit admits no table.
6. **In-flight holds over a database-wide lock.**
   - The gate mutex serializes within one sidecar process. The recorded execute decision
     makes the hold visible to other processes.
   - Rollbacks and reverts never counted toward a budget before, and their
     authorizations still hold no slot.
7. **Not changed:** a full budget can still park a deadline (XID or disk) vacuum or a
   revert. That was true before; see open questions.

## 3. Spec CHECKs covered

Spec principles are covered: evidence-backed changes are bounded, the limits fail
closed, and no autonomy is granted. No new trust is granted. Hygiene gets a budget it
did not have, and performance keeps exactly what the stored policy says.

## 4. Test Results

**Command:** `go test -json -p 2 -count=1 -cover -timeout 1800s ./...` (Docker
golang:1.25, `--cpus=2`, own PG17 `pgsage-ag7` :55477), after merging `release/wave1`.
**Total:** 11,989 passed, 1 failed, 21 skipped (93 packages: 92 ok, 1 FAIL).

**Coverage (touched packages):**

| Package | Coverage |
|---|---|
| internal/policy | 90.3% |
| internal/executor | 87.2% |
| internal/api (fixture only) | 79.1% |
| internal/autonomy (fixture only) | 84.0% |

All packages meet the coverage thresholds.

**Other runs:**

- **e2e:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/` ok (263.6 s).
- **Perf gate:** `PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$'
  ./cmd/pg_sage_sidecar` ok (170.2 s).
- **Race:** `go test -race` on policy and executor: ok.
- **PG14** (:55414) policy and executor: ok (90.3% / 87.1%).
- **PG18** (:55418): policy ok. On the first run, executor failed one test
  (`TestQueueModeTimeoutParksSelfInitiated`, see Failures); the full executor package
  rerun on PG18 passed (87.1%).
- **Lint:** `golangci-lint run ./...`: 0 issues.

### Skipped Tests (all environment-gated, none in touched packages)

- agentdb: `TestAWSRDSLiveProvisioning`, `TestCloudSQLLiveProvisioning`,
  `TestLakebaseLiveProvisioning` and three `TestAgentDBLiveGauntlet*` tests. They need
  `PG_SAGE_LIVE_*` (live cloud).
- azure: `TestAzureLiveServerParameter` and `TestAzureLiveRestartBoundParameter`. They need
  `PG_SAGE_LIVE_AZURE`.
- llm: `TestChatLive_RealProvider` and `TestChatWithToolsLive_RealProvider`; rca:
  `TestTier2Live_RealGemini`. They need `PG_SAGE_LIVE_LLM` (live LLM, off by rule).
- ha: `TestContainer_FailoverWhileDownOpensTheCooldown`; sre/causal: two `TestContainer_*`
  tests. They need a disposable standby or restartable server URL.
- sre/pooler: three `TestPgBouncer_*` tests. No PgBouncer is configured.
- logwatch: `TestResolveLogDir_AbsoluteWindows`. Windows only.
- rca: `TestRCAChildProcessFixture`. This is a helper process, not a test.
- tuner: `TestGeneratePlanFixtures`. It runs only when regenerating plan fixtures.
- sre-bench: `TestPGIncidentBench`. It needs `SAGE_BENCH_RUN`; CI runs it in its own step.

### Failures

- `internal/snapstore TestBytesPerHour_5000Indexes`: `sage.snapshots reduction 9.3x, want
  >= 10x`. **Not caused by this branch.**
  - snapstore imports neither policy nor executor.
  - The test passed in the pre-merge full run.
  - It fails identically on a clean `origin/release/wave1` export run at 01:09 UTC.
  - It is likely time-of-day sensitive: snapstore starts a keyframe each UTC day, and the
    run crossed midnight UTC.
  - The owner of snapstore should look at it; I did not change it.
- PG18 executor, first run: `TestQueueModeTimeoutParksSelfInitiated` queued 119 ms
  against the 300 ms bound.
  - The test goes through `Apply` with `intent.Authorize` set, a path where none of my
    changes apply (no release, no `FirstDecision`).
  - It passed 5/5 on immediate rerun (`-count=5`) and on PG14 and PG17.
  - It is most likely interference on the shared PG18 matrix, from another agent's lease
    cleanup.

### Coverage Gaps

None: every touched package is above 70%.

### Bugs Found This Session

1. [BUG] `apply_finding.go` `processFinding`: a park fell through to `Apply`, which
   evaluated the candidate again, so there were two decisions per cycle. Execute verdicts
   were evaluated three times.
2. [BUG] `policy_runtime.go` `standingUsage`: `RowsRewritten` was never filled, so
   `max_rows_rewritten` was dead.
3. [BUG] `gate.go`: two concurrent candidates could both take the last budget slot. Usage
   counted only `action_log` rows, which are written after execution.
4. [BUG] (in my own first implementation, caught by the tests) `rowsRewrittenSQL` returned
   0 for plain tables, because `pg_partition_tree` returns no rows outside a hierarchy.
5. [OBSERVATION] Usage would error on any execute decision stored with `target_objects =
   null` (a nil target slice) once in-flight decisions are read. The query normalizes
   such targets, and a regression test covers it.

### Mutation testing

29 mutants were run against the key logic. 28 were killed and 1 is equivalent.

- **Policy (12/12 killed):** kind classification, the change and table boundaries (`>=`
  versus `>`), the gate mutex, hygiene reading the performance budget, `ExplainBatch`
  memoization, legacy parsing, the legacy/new conflict check, hygiene defaults, the rows
  check, decision stamping, and the free-time detail.
- **Executor (16/17 killed):** the in-flight CTE, the unknown kind being counted, ignoring
  `FirstDecision`, the request rows being dropped, spoofed evidence being kept, release as
  a no-op, the same-request exclusion, the `VACUUM FULL` classifier, the volatile default,
  the executed-after-hold dedupe, the kind stamp, `Apply` not releasing, the plain-table
  estimate, null targets, read-only holds, and target-less self-exclusion.
- **Equivalent mutant:** removing only the `processFinding` park return is masked by the
  second fix layer, because `Apply` reuses `FirstDecision` without a new gate call.
  Removing both layers is killed by all three park/evaluation tests.

### Post-test audit

- **Untested inputs:** dollar-quoted defaults are treated as rewrites, which is
  conservative. Two sidecar processes racing on one database are covered by the recorded
  hold, but the in-process mutex is not cross-process (see open questions).
- **Assertions that could pass when broken:** the free-time assertions use a ±2 min
  tolerance on a computed now()+N h, so they would catch a wrong anchor. The read-only
  hold test first passed even with the read-only filter removed, because the decisions
  shared SQL and were deduplicated. I changed it to use distinct SQL, and the mutant is
  now killed.
- **Fakes that hide failures:** the policy race test uses an in-memory ledger. The same
  race also runs through the real gate, executor usage and Postgres ledger
  (`TestLastBudgetSlotRaceAdmitsExactlyOne`).
- **Added after audit:** `TestStandingUsageToleratesNullTargetsAndSkipsReadOnly`.

### Manual Checks Remaining

- MANUAL: after the next lifeos upgrade, confirm that a hygiene park on lifeos shows its
  `budget_detail`, and that parked decisions gain one repeat per cycle.

## 5. Open questions for the coordinator / owner

1. **Deadline and revert vs budgets.** A full budget still parks a critical XID or disk
   VACUUM (hygiene) and a verified-index revert (performance). This was true before, but
   hygiene's smaller default makes it more likely. Should valid critical-deadline requests
   and reverts bypass the kind budgets? That would grant something, so I left it to the
   owner.
2. **Cross-process serialization.** The gate mutex is per process. If two sidecars ever
   authorize for the same database concurrently, they could still race between the usage
   read and the recorded decision. A `pg_advisory_lock` per database would close that gap.
   Is multi-writer HA in scope?
3. **Downgrade.** Saving a policy writes the canonical `performance`/`hygiene` form, which
   older sidecars reject (they fail closed with `policy_unavailable`). Acceptable, or
   should marshal also emit the legacy fields?
4. **Snapstore `TestBytesPerHour_5000Indexes`** fails near UTC midnight on the base branch
   too; it needs an owner.

## 6. Left undone

- None of the brief's items. The web UI does not edit blast-radius fields today, so it
  needed no change.

## 7. Commits

The merge with `release/wave1` sits on top of these commits:
`test(policy)` (tests first), `feat(policy)`, `fix(executor)` (kind charging, rows,
holds), `fix(executor)` (one evaluation per park), `test(executor)` (audit),
`docs(policy)`.
