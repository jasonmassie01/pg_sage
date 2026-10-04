# W3-D: One case-driven tuning agent per database (roadmap 2.2)

Branch `claude/w3d-tuning-agent` (from `origin/release/v1.10.0`, stacked on #112).
Agent report, 2026-10-04.

## What was built

One tuning agent per monitored database (`sidecar/internal/tuning`) replaces the
optimizer, tuner (hint prescriber) and advisor (vacuum, memory) prompts. It runs once
per analyzer cycle as the analyzer's tuning producer:

1. **Classify the workload, deterministically** (`classify.go`, `sqlrefs.go`): every
   table is app, test or tenant schema; confirmed binding facts (migration-owned,
   test fixture, append-only, table windows) attach to the tables they bind; each
   statement is workload, diagnostic (pg_sage's own, catalog, `EXPLAIN`, maintenance)
   or batch-window. Test schemas are recognised by name (`facts.LooksLikeTestSchema`)
   or a confirmed `test_fixture` fact; an operator-rejected fixture fact overrides the
   name heuristic.
2. **Detect cases** (`cases.go`, `cases_write.go`): top statements by call-weighted
   time over the snapshot interval (share and call floors), regressions (interval mean
   vs prior mean, with call and total-time floors) and write amplification (indexes
   that cost writes but serve no case statement, unused or covered). Diagnostic,
   test and binding-fact tables never open a case of their own.
3. **Ask the model only about cases**, at most `tuning.max_cases_per_cycle`, inside a
   per-database budget of tokens and requests per cycle (`budget.go`). No cases, no
   call. A case the model already answered with nothing useful is skipped until its
   numbers change (`memory.go`); operator-rejected SQL from the last 30 days is never
   re-proposed.
4. **Read-only tools** (`tools*.go`, `rehearsal.go`): `statement`, `table`, `explain`
   (READ ONLY transaction, `GENERIC_PLAN` on PG16+, no plan for parameterized
   statements before 16, multi-statement refused), `what_if` (HypoPG, at most 3 per
   case), `write_cost`, `extended_stats`, `rehearse` (clone rehearsal, once per cycle,
   only when a clone provider is configured). Results are bounded (8 KB per tool
   result, 12 KB packet) and the lead statement is prefetched.
5. **Typed proposals only** (`proposal.go`, `validate*.go`): `index_create`,
   `index_drop`, `guc`, `reloption`, `create_statistics` (the #110 form, `sage_stx_*`,
   2-8 plain columns, parsed by `extstats.ParseCreate`) and `query_hint`. Each must
   cite evidence the agent actually showed the model, name a table in the case, and
   (except drops) carry a W1-A prediction (class, method, metric, target queryids,
   expected change). Each then goes through the same deterministic gate the old path
   used: index creates through the optimizer's what-if admission and rejection memory;
   drops only for covered indexes or indexes unused for 7+ days of stats; GUCs through
   `advisor.GateConfigFindings` (validation, host-memory guard, allowlist, cloud
   transform); hints through `tuner.ProposeHint` (syntax validation, allowlist,
   clamp); binding facts redirect to a source-fix packet or drop the proposal.
6. **Calibrated confidence** (`calibration.go`): decided outcomes from
   `sage.action_outcome` per action class and prediction method, binned by predicted
   |change| (0-10, 10-25, 25-50, 50+ %), Wilson lower bound (z = 1.96) of "tolerance
   met". A proposal takes its bin's confidence, else its class pool; below
   `tuning.calibration_min_outcomes` it is `uncalibrated` and no number is shown.
   A calibrated confidence below the approval threshold marks the finding
   `approval_required`; uncalibrated has no effect either way.
7. **Rank and cap** (`rank.go`): calibrated-and-confident first, then uncalibrated,
   then low-confidence; by predicted magnitude within a tier; at most
   `llm.optimizer.max_new_per_table` new index proposals per table and
   `tuning.max_proposals_per_cycle` new findings.

Findings keep the categories the executor, approval cards and metrics already know
(`index_*`, `memory_tuning`, `vacuum_tuning`, `table_tuning`, plus
`tuning_index_drop` and `query_create_statistics`); detail carries `producer`,
`case_id`, `case_kind`, `proposal_type`, `rationale`, `evidence`, `predicted_effect`
(target queryids as decimal strings; the executor now reads them exactly) and
`confidence_calibration`.

Also: `GET /api/v1/tuning/calibration?database=` and a calibration table on the Trust
page; approval cards say "N of M comparable actions improved (25-50% predicted)" or
"uncalibrated (n of 5 outcomes)".

Config: new `tuning:` block (`enabled`, `max_cases_per_cycle` 3,
`max_requests_per_cycle` 12, `max_tokens_per_cycle` 60000, `max_turns_per_case` 6,
`max_proposals_per_cycle` 10, `calibration_min_outcomes` 5, `calibration_window_days`
180), restart lifecycle, YAML-only.

## What was removed

- `internal/optimizer`: `prompt.go`, `optimizer_cycle.go`, `confidence.go`
  (fixed-weight confidence), `circuitbreaker.go`, `openrecs.go` (open-recommendation
  re-verification prompt), `rejection_streak.go`, `facts.go` (prompt facts), and the
  `Confidence`/`ActionLevel` fields. `Admit`, `ColdStart`, what-if, rejection memory,
  the detectors (include, partial, join pairs, JSON/vector/PostGIS) and the table
  context builder (now `table_context.go`) stay and are what the agent calls.
- `internal/tuner`: `llm_prescriber.go`, `prompt.go`, `context.go`, `WithLLM`,
  `LLMEnabled`. Hint validation moved to `hint_validate.go`; the deterministic hint
  rules are unchanged; `ProposeHint` is the agent's entry point.
- `internal/advisor`: `vacuum.go`, `memory.go` (their prompts). WAL, connections,
  query rewrites and bloat advice stay, now table-driven.
- `internal/analyzer`: `optimizer_stats.go`; the optimizer slot is now a
  `TuningProducer`.
- `cmd/pg_sage_sidecar`: `newOptimizer`, tuner LLM client wiring.
- e2e: `tuner_llm_test.go`, `optimizer_multiquery_test.go` (prompt paths gone);
  live prompt tests replaced by `TestTuningAgentLive` (skipped without a key, never
  run here).

Net diff vs base: about 6.6k lines added, 12.2k removed.

## Product calls made

1. The agent replaces the optimizer prompt, the tuner LLM prescriber and the advisor
   vacuum and memory prompts. Advisor WAL, connections, rewrites and bloat remain
   separate (see open questions).
2. Existing switches map to proposal types so no config breaks:
   `llm.optimizer.enabled` → index create/drop; `advisor.memory_enabled` → GUC;
   `advisor.vacuum_enabled` → reloption (both also need `advisor.enabled`);
   `tuner.enabled` and `tuner.llm_enabled` → hints; statistics always allowed.
3. The agent uses the `index_optimization` purpose client, with the general client as
   fallback on provider errors when `fallback_to_general` is set and it is distinct.
   A 429 or exhausted budget stops the cycle; an empty, malformed or tool-only answer
   counts as wasted for case memory.
4. Name-heuristic test schemas are excluded unless the operator rejected the fixture
   fact.
5. The approval threshold is applied to the Wilson lower bound, not the point
   estimate; uncalibrated never gates or promotes.
6. Open agent findings are re-emitted while their case lives; legacy optimizer
   findings follow their table; legacy advisor memory/vacuum findings without a
   producer resolve on the first agent cycle.
7. Index drops are proposed only for indexes covered by another index or unused
   for 7+ days of stats.
8. GUC rollback is `RESET` when the current value is the default, else the current
   value in base units (the executor still re-captures before applying).
9. Metric names (`optimizer_*`) are kept so dashboards keep working; they now count
   the agent.
10. Fact loading degrades open (WARN, cycle continues with classification only);
    an open-findings read failure fails the cycle's categories (no resolve storm).

## Test results

**Command:** `go test -json -count=1 -cover -p 2 ./...` (PG17 :55478, golang:1.25 in Docker)
**Total:** 9832 passed, 3 failed, 22 skipped (top-level tests). The 3 failures are in
packages this branch does not touch beyond `api`'s new route, and pass alone and as
whole packages on rerun (`go test -count=1 -cover -p 1 ./internal/api/ ./internal/autonomy/
./internal/earned/` → ok 79.2% / 84.0% / 90.0%): load-sensitive timing under `-p 2`.

**Coverage (touched packages):** tuning 90.3%, optimizer 90.8%, tuner 85.0%,
advisor 81.9%, analyzer 92.1%, api 79.2%, approvalcard 89.7%, executor 87.9%,
facts 86.1%, config 91.4%, cmd/pg_sage_sidecar 80.6%.

Other runs:

| Run | Result |
|---|---|
| e2e `-tags=e2e` on PG17 | 21 passed, 0 failed, 7 skipped (live-LLM tests, no key) |
| `-race` tuning, optimizer, tuner, advisor, approvalcard, analyzer, executor (PG17) | all ok, no races |
| Perf gate `PG_SAGE_PERF_SCALE=small -tags=perfgate TestPerfGate` (PG17) | ok (176.7s) |
| PG14 :55414 tuning, api (+ touched packages earlier) | ok (tuning 90.4%, api 79.1%) |
| PG18 :55418 tuning, api (+ touched packages earlier) | ok after the queryid test fix (tuning 90.3%, api 79.1%) |
| golangci-lint (default, `e2e,perfgate` tags) | 0 issues |
| vitest | 73 files, 409 tests passed; dist rebuilt; node_modules deleted |
| Golden corpus (fake model) | 8 cases pass |

### Skipped Tests (must be zero or justified)
- agentdb: 6 live cloud provisioning/gauntlet tests — need cloud credentials.
- azure: 2 live server-parameter tests — need an Azure server.
- llm: `TestChatLive_RealProvider`, `TestChatWithToolsLive_RealProvider` — live LLM
  calls are forbidden here.
- rca: `TestTier2Live_RealGemini` (live LLM); `TestRCAChildProcessFixture` (helper run
  only as a child process).
- ha, sre/causal: 3 container restart/failover tests — need a disposable server.
- sre/pooler: 3 PgBouncer tests — no PgBouncer in the test image.
- logwatch `TestResolveLogDir_AbsoluteWindows` — Windows-only path test on Linux.
- tuner `TestGeneratePlanFixtures` — fixture generator, opt-in.
- sre-bench `TestPGIncidentBench`, `TestLiveModelArm` — opt-in bench / live model.
- e2e: 6 `TestLLM*` and `TestTuningAgentLive` — live LLM.
None of the skips is in code this branch adds, except `TestTuningAgentLive` (live).

### Failures (if any)
- None outstanding. Full-run flakes, green on rerun: api
  `TestEventBrokerPollOncePublishesActionQueueChanges` (event ordering), autonomy
  `TestHistoryReadIsFlatOnALegacyFloodLedger` (252 ms vs 200 ms bound under load),
  earned `TestReconcileWithoutNewEvidenceDoesNotRewriteLedgerState`.

### Coverage Gaps (packages below threshold)
- None in touched packages. Below 70% repo-wide, all untouched utilities/test
  support: cmd/create_admin 51.5, cmd/reset_admin_for_test 50.0,
  cmd/sigstore_trusted_root 56.1, testsupport/pgssepoch 58.3, testsupport/snapfixture
  0.0 (fixtures only), sre-bench 65.7.

### Bugs Found This Session
1. [BUG] tuning explain: pgx's extended protocol refuses `EXPLAIN (GENERIC_PLAN)` of a
   `$1` statement with no arguments → run it through the simple protocol in a READ ONLY
   transaction (`store_pg.go` `rawSingleValue`).
2. [BUG] single-statement check treated a quoted identifier containing `;` as two
   statements (`validate_index.go` `hasSemicolon`).
3. [BUG] `predicted_effect.target_queryids` lost precision as JSONB numbers (float64)
   → written as decimal strings; executor reads them exactly (`exactTargetIDs`).
4. [BUG] tool-result dedupe across all tools let a repeated `what_if` skip the
   per-case cap → dedupe only the `statement` tool.
5. [BUG] calibration endpoint used a zero window/minimum from configs built without
   defaults (empty classes) and answered 200 without `?database=` → defaults for unset
   values, database required.
6. Test-logic fixes (explained in commits): cases fixtures gave statements accidental
   regressions; admission rejection fixture lacked a table size; DB tests matched
   pg_stat_statements by text, which fails on PG18 (relation names are jumbled, so an
   earlier schema's text is kept) and after other packages reset it; PG14/15 cannot
   EXPLAIN a parameterized statement generically.

### Manual Checks Remaining
- CHECK-M1: MANUAL — Trust page calibration table and approval-card wording in a
  browser (covered by vitest, not eyeballed).
- CHECK-M2: MANUAL — one cycle against a real model (live calls forbidden here;
  `TestTuningAgentLive` is the harness).

## Mutation testing

Classifier, calibration and validation: 40 hand-made mutants, 38 killed. The 2
survivors are equivalent: `splitFacts` status filter (`facts.Bind` already requires a
confirmed fact) and the statistics minimum column count (`extstats.ParseCreate`
refuses fewer than 2 before the agent's check).

## Open questions

1. Advisor WAL, connections, rewrites and bloat still have their own prompts. Fold
   them into the agent as more proposal types, or keep them as fleet-level advice?
2. Bench: the agent is benched by the golden corpus (8 recorded cases with a fake
   model). There is no sre-bench arm for it yet; should 2.2 get one before release?
3. `tuner.ProposeHint` records the hint when the proposal is validated, before the
   per-cycle cap; a hint cut by the cap is still recorded. Acceptable, or move the
   hint write after ranking?
4. Host memory is not wired in production (`host_memory_bytes` 0), so the memory
   guard leaves `shared_buffers` proposals advisory-only. Wire it from the provider?
