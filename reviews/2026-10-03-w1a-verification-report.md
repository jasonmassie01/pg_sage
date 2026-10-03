# W1a: real verification (roadmap Phase 1.3), report (2026-10-03)

Branch `claude/w1a-real-verification` (from master `e9feff49`). Roadmap item: *"Call-weighted
before/after deltas for the queries the action targets, windows that cover a business cycle
for drops (soft-drop: keep the definition, re-create on first miss), a variance-aware test,
and predicted-vs-observed recorded for every action."*

## What was wrong (from `autonomy.md` / `tuning.md`, re-checked in code)

- Drops and GUC changes without queryids were "verified" by a 15-minute compare of the
  database-wide cumulative cache-hit ratio and the unweighted mean of `INSERT%/UPDATE%`
  statements (`executor/rollback_eval.go`). Cumulative counters barely move in 15 minutes.
- The durable index-create engine compared bare means: it reverted when *any* target got
  worse and kept the index when *any* target got better, with no noise model.
- VACUUM/ANALYZE (no rollback SQL) were marked `success` the moment they returned.
- No action carried a predicted effect, so nothing could say whether an action did what it
  was supposed to do.

## Design

One verifier per path, both on one statistics core (no parallel system):

| Path | Who | Window | Decision |
|---|---|---|---|
| Durable engine (`verify.Engine`) | autonomous CREATE INDEX | `verify.window_minutes` doubling to `verify.window_max_minutes` | `verify.DecideTargets` |
| Rollback monitor (`MonitorAndRollback`, now a loop) | drops, GUC/reloption, hints, manual creates | min = `trust.rollback_window_minutes`, extended to `verify.window_max_minutes` while evidence accrues; drops: `verify.drop_window_hours` | `verify.DecideTargets` + class metric (`combineVerdict`) |
| Immediate (`verifyImmediate`) | VACUUM, ANALYZE, anything without rollback | right after the action | dead tuples / `n_mod_since_analyze` |
| Retention run | retention batches | the batch | deleted vs reviewed candidates |

### 1. Predicted effect (before the action runs)

`executor.predictAction` stamps `before_state.predicted_effect` and, after the action row is
written, a pending `sage.action_outcome` row (`recordPrediction`). A producer can supply a
`predicted_effect` in its finding detail; otherwise:

| Class | Metric | Expected | Method | Targets |
|---|---|---|---|---|
| index_create | mean_exec_time | -`estimated_improvement_pct` | `hypopg` when the what-if gate verified it, else `model` | finding queryids |
| index_drop | mean_exec_time | 0 (reads unchanged) | rule | top 10 statements on the table by calls; those whose captured plans used the index are recorded |
| guc / reloption | temp_spills -50 / dead_tuples -20 / hot_updates +5 | per metric | rule | work_mem: top temp-spilling statements; other GUCs: costliest statements; reloption: statements on the table |
| guc without a metric (e.g. random_page_cost) | — | — | **none** | as above (regression guard only) |
| vacuum / analyze | dead_tuples / n_mod_since_analyze | -100 | rule | — (baseline value recorded) |
| query_hint | mean_exec_time | producer's estimate if any | model / none | the hinted queryid |
| retention | rows_deleted | -min(candidates, bound)/candidates | rule | — |

"No prediction" is explicit (`method: none` with a note) and is never credited.

The targets' pre-action measurement is **frozen** in `before_state.verify_baseline` (window:
the class minimum, doubled up to the cap until the targets have `verify.min_samples` calls;
for drops the whole business cycle before the drop), so a 7-day watch never compares with a
baseline `query_store` retention already removed.

### 2. Call-weighted, variance-aware before/after (`internal/verify/stats.go`)

- `sage.query_store` stores cumulative counters per sample (written when they move). The
  verifier reads the **interval deltas** (one index range of `idx_query_store_qid_time`),
  summed into ~48 time buckets per window (5 s to 6 h each).
- `Summarize`: call-weighted mean = Σtime/Σcalls; standard error from the linearized
  variance of that ratio estimator across buckets, `n/(n-1)·Σ(T_i − μ·C_i)²/C²`. Buckets are
  clusters, so a window whose means wander stays uncertain however many calls it has (the
  per-call stddev would make every 1 ms wobble "significant" on a busy query).
- `Compare`: Welch's t on the two means (Welch–Satterthwaite df over buckets, exact t
  quantiles for df < 3, Cornish–Fisher otherwise). A change counts only when it is **both
  significant and beyond its bar**: regressed if the 95% CI is above 0 and the change is
  above the regression bar; improved if the CI is below 0 and the gain reaches
  `verify.min_gain_pct`; **neutral** only if the whole CI sits inside the bars; anything else
  (too few calls, < 3 buckets a side, or a CI too wide to tell) is
  **insufficient_evidence**. A statistics reset inside a window still voids it (R10).
- `DecideTargets`: each target is tested on its own with a Bonferroni-corrected alpha (one
  marginal query among 20 does not flip the verdict); a regression of any target is a
  regression of the action; otherwise the **call-weighted pool** of the targets measured on
  both sides decides (the old "any target gained" kept indexes that helped a rare query).
- Windows sized to traffic: first verdict after the minimum window, and only with enough
  evidence; insufficient evidence extends the watch until the cap (`verify.window_max_minutes`,
  72 h default). Unverifiable (e.g. no prediction) and regressed are final at once.

### 3. Index drops: business cycle and soft drop

- `verify.drop_window_hours` (default 168, 1–8760). Lower values are reported by
  `LoweredElevation`, so the startup `FAST ELEVATION` WARN, `/api/v1/sre/autonomy` and the
  Earned-autonomy badge list it, like the other fast-trust settings. YAML-only.
- A drop is checked from the start every `clamp(window/8, 5 min, 1 h)`; its verdict is final
  only after the full cycle (or regressed earlier).
- **Soft drop**: the definition is kept (`rollback_sql`, `pg_get_indexdef` at analysis) and
  re-created `CONCURRENTLY` through the existing rollback path on the **first miss**:
  1. *a targeted query regresses* (the Welch test above, on the top statements of the
     table — a plan that used the index and lost it shows up as that query getting slower),
     or
  2. *an active pg_sage hint names the index* (`sage.query_hints`, status active): its
     plan directive now silently fails.
  Why these: "a failed plan" has no direct signal in Postgres — a plan that wanted a missing
  index silently picks another one; the only observable consequence is latency, which (1)
  measures with noise control. (2) is the one case where pg_sage itself holds a plan that
  names the index. Reading every captured plan for the index name would scan
  `sage.explain_cache`; instead the captured plans of the targets are checked once at drop
  time (index range per target) and recorded as `plan_referenced_queryids` evidence.
- A drop whose reads held for the whole cycle is **improved** (product call, below).

### 4. Recorded outcome

`sage.action_outcome` (migration `action_outcome_migration.go`, idempotent: the table is
created only when the catalog lacks it, so a re-run takes no lock on `sage.action_log`; its
index goes through the checked index loop):

| Column | Meaning |
|---|---|
| `action_log_id` PK, FK → `action_log(id)` ON DELETE CASCADE | the action (retention prunes both) |
| `database_id` | copied from the action |
| `action_class` | index_create, index_drop, guc, reloption, vacuum, analyze, query_hint, retention |
| `predicted` jsonb, `prediction_method` | the immutable prediction (`verify.Prediction`) and hypopg / model / rule / none |
| `verdict` | pending, improved, neutral, regressed, insufficient_evidence, unverifiable |
| `tolerance` | met (≥ 50% of the predicted change, or within ±10 points of a predicted 0), partial (same direction, > 10% of it), missed, no_prediction, unmeasured |
| `observed` jsonb | metric, before, after, change_pct |
| `evidence` jsonb | the comparison (calls, means, standard errors, buckets, CI, t, df), per-target comparisons, the class metric, soft-drop trigger/definition/re-created, rollback outcome |
| `reason`, `window_start`, `window_end`, `created_at`, `decided_at` | |

Index `idx_action_outcome_class_decided (action_class, decided_at DESC)` serves the trust
reads. API: `verification_outcome` on `GET /api/v1/actions` and `/api/v1/actions/{id}`;
`GET /api/v1/action-outcomes?database=&class=&verdict=&since=&limit=` (newest first, limit
1–1000). UI: the Verification column shows the verdict; the action detail shows predicted vs
observed, tolerance, targets, evidence with CI, soft-drop and whether it counts toward trust.
MCP: executed actions are not exposed over MCP today (`get_ledger` reads `sage.decision`), so
no MCP field was added (open question below).

### 5. Earned autonomy and credit

- `earned.classifyWithVerdict`: improved → `verified_recovery`; regressed → `harmful`;
  neutral, insufficient_evidence, unverifiable → `unverified` (no credit, no harm); a failed
  action is never a recovery; rows without a verdict keep the P0-6 rules.
- `sage.verification.verdict` is `success` only for improved (`revert` for regressed,
  `unverifiable` otherwise), so value credit and every existing consumer of
  `verdict='success'` follow automatically. `action_log.outcome` stays a lifecycle field:
  `success` = kept and measured (improved or neutral), `unverifiable` = kept, could not tell.
- Regressed takes the existing rollback path (`rollbackRegressedAction`, emergency stop and
  standing-policy authorization unchanged; engine creates: `Revert`).

### Removed (replaced)

`evaluateRegression` (global cache-hit / write-latency check), `perQueryRegression`,
`isQueryRegressed`, `actionTargetQueries`, `settleConfigOutcome`, the binary config judges,
the engine's `anyGain/anyRegression/sampleCount`, the window first/last-sample measurement and
`RollbackMonitorConfig.Delay`. `updateActionSuccess` stays only for custodians, whose own
post-check verifies them.

## Product decisions

1. **A drop whose reads held for a full business cycle is "improved".** Its predicted effect
   is "reads unchanged, index space and write cost freed"; observing that is delivering the
   prediction. Without traffic on the table it is insufficient evidence, not improved.
2. **A neutral index create is still reverted** (`no_gain`), as before: an index that does not
   help still costs writes. Its verdict is neutral (not harmful) in the trust ledger.
3. **No prediction is never credited**, even when nothing regressed (e.g. `random_page_cost`
   from the LLM advisor). Such changes are still watched for regressions on the costliest
   statements and rolled back if one regresses.
4. **Config changes keep being watched while evidence can still accrue** (autovacuum has not
   run yet, too few calls), up to `verify.window_max_minutes`; a metric that can never be
   judged (nothing spilled before the change) stops waiting at once.
5. **Regression bar for the monitor stays `trust.rollback_threshold_pct`** (10%), the engine's
   stays `verify.regress_pct` (15%); the gain bar is `verify.min_gain_pct` (20%) for both.

## Tests

Tests were written first (`e67a0797`) and run afterwards. Integration tests run against real
Postgres (PG17 `pgsage-ag6`) with a seeded `sage.query_store`, so the SQL, the bucketing and
the verdict all execute for real.

| Required test | Where |
|---|---|
| index create that improves → improved | `executor/verification_outcome_db_test.go` `TestOutcome_IndexCreateImprovingTargetsIsImproved` (durable engine, real index) |
| drop that regresses → regressed → rollback | `TestOutcome_DropRegressingTargetIsRecreated` (index re-created from the kept definition, `soft_drop.recreated` evidence) |
| GUC with no effect → neutral | `TestOutcome_GUCWithoutMeasurableEffectIsNeutral` (work_mem, real temp spills before and after) |
| too few calls → insufficient | `TestOutcome_TooFewCallsIsInsufficientEvidence` |
| noisy samples do not flip the verdict | `verify/stats_test.go` `TestCompareNoisySamplesDoNotFlipVerdict`, `verify/engine_test.go` noisy-extends tests |
| soft-drop re-create | `TestOutcome_SoftDropRecreatesOnHintReference`, `TestOutcome_DropRegressingTargetIsRecreated`; held / not-yet-decided: `TestOutcome_DropHeldThroughBusinessCycleIsImproved`, `TestOutcome_DropIsNotDecidedBeforeBusinessCycle` |
| predicted vs observed persisted and served | `verify/outcome_store_db_test.go` (pending prediction, immutability, cascade, concurrent verdicts), `api/action_outcome_api_test.go` (list, detail, ledger filters, bad input, unknown database) |
| VACUUM / FREEZE verified | `executor/verification_maintenance_db_test.go` |
| earned autonomy counts only improved | `earned/reconcile_outcome_test.go` |
| migration idempotent, no lock on action_log on re-run | `schema/action_outcome_migration_test.go` |
| UI | `web/src/components/VerificationOutcome.test.jsx`, `web/src/pages/Actions.outcome.test.jsx` |

### Mutation check of the decision logic

Each mutation breaks one rule on purpose, then the named tests run (script kept outside the
repo). **16/16 killed.**

| # | Mutation | Result |
|---|---|---|
| M01 | regression without significance (drop `CILow > 0`) | KILLED |
| M02 | gain boundary `>=` → `>` | KILLED |
| M03 | unweighted mean instead of Σtime/Σcalls | KILLED |
| M04 | no Bonferroni correction across targets | KILLED |
| M05 | ignore the minimum bucket count | KILLED |
| M06 | neutral band always true | KILLED |
| M07 | a regressed target ignored when the pool improves | KILLED |
| M08 | tolerance always `met` | KILLED |
| M09 | no-prediction credited as improved | KILLED |
| M10 | drop that held for the cycle not counted improved | KILLED |
| M11 | final verdict before the minimum window | KILLED |
| M12 | soft drop ignores a hint naming the index | KILLED |
| M13 | neutral mapped to `success` in sage.verification | KILLED |
| M14 | earned autonomy credits neutral | KILLED |
| M15 | config metric regression ignored | KILLED |
| M16 | VACUUM always improved | KILLED |

## Test Results

**Command:** `go test -v -count=1 -cover -p 2 ./...` (golang:1.25, `--cpus=2`, PG17 `:55476`)
**Total:** 11773 passed, 2 failed (both flaky under load, see below), 21 skipped
**Coverage (touched packages):** verify 88.4%, executor 86.6%, earned 88.7%, api 78.7%,
config 91.0%, schema 83.8%, retention 86.9%, store 75.9%. Every package in the repo is at or
above its floor (lowest: cmd/reset_admin_for_test 50.0%, cmd/create_admin 51.5%, both
utilities).

Other gates:

- `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: **ok** (169.9 s; 13 LLM tests skipped,
  no `SAGE_LLM_API_KEY`).
- Perf gate, `PG_SAGE_PERF_SCALE=small`: **PASS** (160.7 s). `sage.action_log` 0 seq scans
  (the outcome lookups add none); `sage.action_outcome` showed 91 seq scans
  reading 0 tuples (an empty table, the planner's right choice); `/api/v1/actions` 13.7 ms.
- Touched packages on PG14 (`:55414`) and PG18 (`:55418`): **ok**, all 8 packages (verify, executor, earned, api,
  schema, config, retention, store) on both versions; coverage within 0.3 points of PG17.
- Touched packages with `-race`: **ok**, all 8 packages, no data races.
- `golangci-lint run ./...`: **0 issues**.
- Web: `npm ci && npm test` 54 files / 282 tests passed; `npm run build` committed
  (`internal/api/dist`); `node_modules` deleted.

### Skipped Tests (must be zero or justified)
None of the skips are in code this branch touched; all are environment-gated:
- Live cloud provisioning (AWS RDS, Cloud SQL, Lakebase, AgentDB gauntlet x3, Azure x2): need
  cloud credentials.
- LLM live tests (`TestChatLive_RealProvider`, `TestChatWithToolsLive_RealProvider`,
  `TestTier2Live_RealGemini`, 13 in e2e): need an API key.
- `internal/ha` container tests x3: `SAGE_TEST_HA_STANDBY_URL` not set (no disposable standby).
- `TestPgBouncer_*` x3: no PgBouncer in the test environment.
- `TestResolveLogDir_AbsoluteWindows`: Windows-only path test on Linux.
- `TestGeneratePlanFixtures`: runs only with `PG_SAGE_REGEN_PLAN_FIXTURES=1`.
- `TestPGIncidentBench`: full bench runs in its own CI step.
- `TestRCAChildProcessFixture`: child-process fixture, reports a skip when run as a parent.

### Failures (if any)
- `cmd/pg_sage_sidecar TestFleetReloadUnreachableChangeKeepsRunningRuntime` (34.8 s under load,
  3.1 s alone) and `internal/testsupport/perfgate TestReadTableStatsCountsPartitionsNotTheParent`
  failed once in the final full run while other agents' suites shared the Docker VM. Both
  passed in the previous full run, alone, and in a full re-run of both packages (`ok`, 212.9 s
  and 67.6 s). Neither touches verification code. Flagged as load-sensitive, not fixed here.

### Coverage Gaps (packages below threshold)
All packages meet coverage thresholds (business logic ≥ 70%, utilities ≥ 50%); numbers above.

### Bugs Found This Session
1. [BUG] engine retain path recorded `tolerance: no_prediction` for creates that had one;
   `engineOutcome` now reads the prediction stored in `before_state`.
2. [BUG] a tiny move in the predicted direction scored `partial`; partial now needs > 10% of
   the predicted change.
3. [BUG] 1-minute bucket floor collapsed short windows into one bucket (always insufficient);
   floor is now 5 s.
4. [BUG] a retained cleanup (verdict without an outcome) lost its value credit; mapped to
   improved.
5. [BUG] `sage.action_outcome` had no retention rule or exemption; exempted (cascades with its
   action).
6. [BUG] `verify.drop_window_hours` was missing from the store's YAML-only key registry (the
   config API already refused it; the consistency test caught the missing registration).

### Manual Checks Remaining
- CHECK-M1: MANUAL, the action detail panel (predicted vs observed, evidence, soft drop) in a
  browser against a live sidecar; covered by component tests only.
- CHECK-M2: MANUAL, a real 7-day drop cycle on a dogfood database (tests shorten the window).

### Post-test audit
- *Inputs not tested:* a query whose queryid changes mid-window (pg_stat_statements reset is
  covered by R10, a plan change keeps the queryid); targets reached only through views.
- *Behaviour not asserted:* the exact `checkEvery` cadence of a live 7-day loop (unit-tested
  through `monitorPlan`, not timed).
- *Assertions that could pass when broken:* none found; the mutation check above kills every
  decision rule.
- *Test doubles hiding failures:* none on the verdict path: verification tests use real
  Postgres, real indexes, real `work_mem` spills and real VACUUM; only the clock of the
  business cycle is shortened.

## Open questions

- MCP has no executed-actions tool; should `get_ledger` entries carry the verdict of the
  action a decision produced, or should a `get_action_outcomes` tool expose the ledger?
- Drop targets come from `pg_stat_statements` text matching the table name; a query that
  reaches the table only through a view or function is missed. Should the analyzer record
  plan-derived relation usage per queryid?
- Rare jobs (month-end) that fall out of `query_store`'s sampled set cannot be measured;
  should the verifier pin its targets into the sampled set for the window?
- The trust system (next wave) should decide whether `neutral` drops/creates count toward
  promotion volume (they prove safety, not benefit).
