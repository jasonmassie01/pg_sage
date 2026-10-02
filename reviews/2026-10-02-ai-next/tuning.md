# Tuning brain: analyzer, optimizer, advisor, tuner, plans, recommendations, LLM

Scope: `sidecar/internal/{analyzer, optimizer, advisor, tuner, autoexplain, explain, planhash,
querystore, sqlast, schemaguard, vectorlab, recommendation, value, llm}` at v1.8.1. All paths
below are relative to `sidecar/internal/`. Claims were checked in code unless marked *inferred*.

## 1. Inventory

| Feature | What it does today | Where | LLM? | Maturity | Evidence |
|---|---|---|---|---|---|
| Snapshot rules (index, vacuum, stats, query, system, sequence, replication) | Deterministic thresholds over one or two snapshots; a clean run resolves owned categories | `analyzer/rules.go:51-87`, `rules_*.go` | No | Production | 39 test files; dogfood dropped 19 indexes, none regressed |
| App-managed index memory | An index pg_sage dropped that came back with the same definition stops being proposed | `analyzer/app_managed_index.go` | No | Solid (new) | Fixes lifeos 8x re-drop; read-only check on lifeos |
| Clone-schema collapse | Schemas with generated suffixes and identical table sets (≥5) fold into one info finding | `analyzer/clone_schemas.go` | No | Solid (new) | 477/478 lifeos dup findings collapsed |
| Plan regression + narrator | Compares the last two `explain_cache` plans per queryid by estimated cost; LLM writes a 2-4 sentence "why" | `analyzer/rules_plan_diff.go`, `plan_narrative.go` | Yes, single-shot prose | Prototype | Unit tests on fixtures |
| Index optimizer | LLM proposes indexes per table; canonicalised, validated, HypoPG-checked, confidence-scored | `optimizer/` | Yes, JSON per table, repair | Solid | 28 test files, functional tests |
| Config advisors (memory, connection, vacuum, WAL, bloat, rewrite) | LLM proposes `ALTER SYSTEM` / reloptions, checked against a doc-grounded range table | `advisor/` | Yes, fully LLM-driven | Solid / prototype by advisor | 40 test files incl. e2e; lifeos proposed `work_mem='8MB'` |
| Query tuner | Plan symptoms → deterministic or LLM `pg_hint_plan` hints, revalidated and removed | `tuner/` | Yes (prescriber) | Prototype | 25 test files on plan fixtures |
| Plan capture | Periodic plain `EXPLAIN` (or `GENERIC_PLAN`) of top queries into `explain_cache` | `autoexplain/collector.go:185-219` | No | Solid | 11 test files |
| /explain + LLM explanation | EXPLAIN (ANALYZE) on demand, LLM narrative, cached | `explain/` | Yes | Solid | Injection integration tests |
| planhash / querystore | Plan fingerprint; windowed per-query latency history | `planhash/`, `querystore/` | No | Solid | Used by verify-and-revert (`executor/rollback_eval.go:80-107`) |
| sqlast | pg_query AST checks on generated DDL (cgo build) | `sqlast/check_cgo.go` | No | Solid | 1 test file |
| schemaguard | Schema invariants (FK index, etc.) and routing to park / rehearse | `schemaguard/` | No | Prototype | 8 test files |
| vectorlab | pgvector index lab: manifest, plan, evidence | `vectorlab/` | No | Prototype (*inferred*: not on the main loop) | 7 test files |
| Recommendation store | 11-state durable lifecycle, CAS transitions, content-hash revisions | `recommendation/state.go`, `transition.go` | No | Production | 8 test files |
| Value model | Credits "toil minutes" per action and avoided incident | `value/` | No | Prototype | Constants, e.g. `value/incident.go:24-25` |
| LLM client | OpenAI-compatible chat + tools, daily token budget, breaker, JSON repair, untrusted wrapping | `llm/` | n/a | Solid | 30 test files; gpt-6-luna on lifeos |

## 2. What is weak

### Evidence quality: the brain reasons over estimates
- **Plan regression is a regression in estimated cost.** `diffPlans` fires at a cost ratio ≥2.0
  even when the plan shape is identical (`analyzer/rules_plan_diff.go:118-147`). `plan_changed`
  is computed (`:111-112`) but never gates the finding, so data growth reads as a regression.
  `CurrentTime`/`PreviousTime` are scanned (`:373-374`) and never used.
- **"auto_explain" is plain EXPLAIN.** The capture path runs `EXPLAIN (FORMAT JSON)` without ANALYZE
  (`autoexplain/collector.go:217-219`). Real auto_explain plans are read only through the
  Supabase log sink. Row-estimate errors, the most useful evidence for hints and extended
  statistics, are therefore mostly missing.
- **query_store rows carry the latest plan's hash, not the hash of the plan that ran**
  (`querystore/querystore.go:30-38`). Plans are recaptured at most daily, so "latency changed with
  the plan" cannot be told apart from "latency changed".
- **Unused-index logic ignores replicas.** `ruleUnusedIndexes` reads primary `idx_scan` only
  (`analyzer/rules_index.go:67-106`). No code reads standby index usage (verified by grep), so an
  index used only by read replicas is a drop candidate. PG16+ `last_idx_scan` is not collected
  either. The observation window `FirstSeen` lives only in memory (`rules_index.go:113-116`).
- **Verify-after-drop is a 15-minute global check** (`config/defaults.go:52`). Without target
  queryids it compares the database cache-hit ratio and the mean latency of
  `query LIKE 'INSERT%' OR 'UPDATE%'`, which is case-sensitive (`executor/rollback_eval.go:30-33`).
  A weekly report that needed the index is never seen.

### Optimizer
- **HypoPG fails open.** If one workload query fails EXPLAIN inside the shared session, the whole
  run errors (`optimizer/hypopg_session.go:110-113`) and is treated as neutral. The candidate then
  skips the HypoPG gate (`optimizer/optimizer.go:300-306`).
- **Improvement is the unweighted mean across queries** (`optimizer/hypopg.go:79-92`). A 90% win
  on the one hot query out of ten averages to 9% and is rejected. Calls and total_time are
  ignored.
- **There is no write-cost model.** `checkWriteImpact` runs before HypoPG, uses the LLM's own
  estimate, gates at 15% while the prompt says 30% (`optimizer/prompt.go:27`), and computes write
  rate from lifetime counters.
- **Expression indexes are always rejected.** `extractColumnsFromDDL` stops at the first `)`
  (`optimizer/validate.go:241-251`). `checkDuplicate` ignores method, WHERE and INCLUDE.
  `CONCURRENTLY` is required, but it is invalid on partitioned parents. `lc_collate` was removed
  in PG16, so the collation always reads as `C`.
- **Dead code, mismatched prompt.** Most of `detection.go` and all of `cost.go` are unused. The
  prompt asks for work_mem/matview advice the schema cannot express and does not require index
  names, yet unnamed DDL is rejected. LLM `severity` passes unvalidated.
- **Confidence is a fixed weighted sum of data-availability signals**
  (`optimizer/confidence.go:20-25`). None of its inputs comes from past outcomes. `openrecs`
  blocks re-analysis of a table while any finding on it is open.

### Advisor and tuner
- **Validation covers about 15 GUCs. Unknown GUCs and reloptions pass**
  (`advisor/docground.go:155-159`). So `autovacuum_enabled=false` passes the advisor. The
  restart-required list is hardcoded (`advisor/validate.go:35-42`) and misses
  `autovacuum_max_workers` (pre-PG18). The collector already has `pg_settings.context`
  (`collector/snapshot.go:228`).
- **Platform detection only knows Cloud SQL** (`advisor/wal.go:123-131`). RDS, Aurora and AlloyDB
  restrictions exist in `restrictedSettings` but cannot trigger. `WithHostMemoryBytes`
  (`advisor/advisor.go:62`) has no caller, so shared_buffers advice runs without knowing host RAM.
- **`ALTER SYSTEM` is applied without read-back or RollbackSQL.** The monitor marks it a success
  immediately. No before/after metric (temp files, dead tuples, checkpoint rate) is recorded.
- One open finding in an advisor category blocks that category everywhere.
- **Tuner symptoms are noisy.** `checkSeqScan` flags every Seq Scan
  (`tuner/planscan.go:168-182`). `checkParallelDisabled` flags every scan, because
  `WorkersPlanned` sits on Gather nodes, not scans (`:213-231`). `prescribeBadNestedLoop` emits
  `HashJoin(<one alias>)`, or `HashJoin()` for join nodes without a relation
  (`tuner/rules.go:98-107`). pg_hint_plan needs two or more relations, so the hint is a no-op.
- **Tuner state is wrong around failures.** `query_hints` rows are marked active before the hint
  is applied and are never updated on rollback. The LLM's confidence is discarded. Tests use
  fixtures that hide all of these issues.

### Workload awareness
- **App-managed detection only reacts after a fight.** It needs one pg_sage drop followed by a
  recreate (`analyzer/app_managed_index.go:32-40`). It matches on the dropped name, so an app that
  recreates the index under a new name is not recognised. Migration tables
  (`schema_migrations`, `flyway_schema_history`, `alembic_version`, `__EFMigrationsHistory`) and
  DDL in `pg_stat_statements` are not consulted before the first drop.
- **Clone collapse can hide live tenants.** `cloneStem` treats any 6+ character
  hex/digit suffix as generated (`analyzer/clone_schemas.go:28-41`). So `tenant_000123`
  schema-per-tenant designs collapse, their real missing-index findings disappear, and the
  finding says "drop them" (`:149-152`). Activity (seq/idx scans, n_tup_ins, last
  vacuum/analyze) is not checked.
- **The recommendation loop is bounded only by oscillation 3 per 7 days**
  (`executor/executor.go:673-674`). Heads can stay in `verifying` forever. Retention deletes the
  refusal memory.

### LLM usage
- **One client, one model.** No per-task routing, response cache or dollar cost. The daily cap is per client and
  in memory, so a fleet gets N×2 budgets (`llm/client_controls.go:250-256`). Budget exhaustion is
  detected by string match, and the breaker lives in memory.
- **`neutralizeDataTags` indexes the lower-cased string with offsets taken from the original**
  (`llm/untrusted.go:56-72`). `strings.ToLower` changes byte length for some runes (the Kelvin
  sign U+212A, 3 bytes, becomes 1-byte `k`). Offsets then drift, which can panic or let a
  `</data` tag through. Escaped quotes in E-strings leak through redaction.
- **The plan narrator sends raw query text** without the untrusted wrapper
  (`analyzer/plan_narrative.go:72-92`). It overwrites `Recommendation` (`:65`), runs on every
  cycle for every persisting regression with no cache, and passes `"analyzer"` as the log level
  (`:52`).
- **`/explain` side effects.** It runs EXPLAIN ANALYZE in a READ ONLY transaction
  (`explain/explain.go:300-315`), but volatile functions such as `pg_terminate_backend` and
  `dblink` still execute. `timeout_ms: 0` means no limit, the cache key ignores PlanOnly, and LLM
  failure fallbacks are cached for 60 minutes.
- **sqlast misses unqualified DROP INDEX.** The protected-schema check applies only when the name
  is qualified (`sqlast/check_cgo.go:82`).

### Built but unused
- **`create_statistics` is half built.** It has an action class (`earned/class.go:89`), a contract
  (`executor/action_contract.go:794`) and a case candidate (`cases/query_actions.go:71`). No
  producer emits CREATE STATISTICS; the A4 comment in `rules_stale_statistics.go:15` promises one.
- **Case candidate confidences are constants** (0.76 / 0.74 / 0.68, `cases/query_actions.go`).
- **schemaguard gaps.** Clone Rehearsal is "routed" with no implementation; `external_reversion`
  and 4 kinds are never produced; one `sage.decision` row per invariant per cycle; the FK test
  `conkey <@ indkey` counts non-leading columns.
- **Value numbers are assumed constants** (`value/incident.go:24-25`), not measurements.

## 3. Iterate

| # | Change | Value | Effort | Risk | Guardrail |
|---|---|---|---|---|---|
| I1 | Gate `plan_regression` on `plan_changed` and on measured latency from querystore; keep the estimated-cost ratio as context | Removes false regressions from data growth | S | Low | Unchanged (finding only) |
| I2 | HypoPG: fail **closed** per query (EXPLAIN each query in a savepoint), weight improvement by `total_exec_time`, require ≥1 measured query | Real admission gate; hot-query wins pass | S | Low | Reject when unmeasured at ≥L2 |
| I3 | Write-cost model: index maintenance ≈ (n_tup_ins + n_tup_upd non-HOT + n_tup_del) rate × index count, using snapshot deltas, not lifetime counters; drop the LLM estimate from the gate | Stops write-heavy regressions | M | Low | Reject above budget |
| I4 | Fix the expression/partial/INCLUDE parser by validating with `sqlast` (pg_query) instead of string scans. Use `ON ONLY` + per-partition CONCURRENTLY for partitioned tables | Unlocks expression and partitioned indexes | M | Med | sqlast parse required |
| I5 | Unused index: collect `last_idx_scan` (PG16+); persist FirstSeen; poll standby `pg_stat_user_indexes` when replicas are configured; block drops when standby usage is unknown | Prevents replica-only index drops | M | Low | Unknown replica usage → no drop above L1 |
| I6 | Pre-emptive app-ownership: read migration tables, scan pg_stat_statements for `CREATE INDEX` text, and track the index→table OID plus definition (not the name). Index without drop history but with a migration fingerprint → `app_managed_index` and a "PR to your migrations" artifact | Ends migration fights before the first drop | M | Low | Finding only |
| I7 | Clone collapse: require inactivity (zero scans/writes since stats epoch, no live sessions) before saying "drop"; active families keep per-schema findings and get a "multi-tenant" label | Stops hiding tenant problems | S | Low | Finding only; schema drop never above L1 |
| I8 | Advisor: unknown GUC or reloption → reject (allowlist). Restart flag from `pg_settings.context`. Read back after `ALTER SYSTEM` + `pg_reload_conf()`. Store a RollbackSQL `RESET`/previous value. Capture a category metric before and after | Config actions become verified and reversible | M | Low | Executor refuses without RollbackSQL |
| I9 | Detect platform from `rds.*`/`cloudsql.*`/`alloydb.*`/`azure.*` settings; wire host RAM | Correct cloud limits | S | Low | `restrictedSettings` |
| I10 | Tuner: delete `checkParallelDisabled` and the blanket `checkSeqScan`, or gate them on actual-rows evidence. Generate join hints only with ≥2 aliases from the plan. Transition `query_hints` state on rollback. Keep the LLM's confidence | Removes no-op hints | S | Low | Hint validity check via `hint_parse` |
| I11 | LLM hygiene: fix `neutralizeDataTags` (scan runes case-insensitively on the original string); wrap narrator and justifier inputs in `<data>`; cache narratives by `(queryid, plan_hash pair)`; keep the deterministic recommendation and add the narrative in Detail | Safety and cost | S | Low | Untrusted-data contract |
| I12 | `/explain`: refuse ANALYZE of queries that call volatile functions (`pg_proc.provolatile = 'v'`, from the AST), force a default `statement_timeout`, add PlanOnly to the cache key | Closes a side-effect hole | S | Low | sqlast |
| I13 | Fleet-wide token and dollar budget persisted in `sage.llm_usage`; per-task model routing (`narrate`→cheap, `optimizer`/`advisor`→strong) | Predictable cost | M | Low | Budget refusal = deterministic fallback |

## 4. Expand

1. **Extended statistics producer.** Nodes misestimated ≥10x (real auto_explain or replica
   `EXPLAIN ANALYZE`) yield `CREATE STATISTICS` candidates on correlated predicate columns,
   verified by the estimate error after ANALYZE. Class, contract and case already exist.
2. **Index lifecycle ledger.** Per created index: idx_scan growth, target-query latency at
   1h/24h/7d, size, write amplification. Answers "did it pay for itself" (see §5).
3. **Workload classifier.** Label objects app-owned / migration-managed / tenant / leaked test /
   partition child / queue from naming, activity, migration tables and DDL history; every rule and
   prompt gets the label. Leaked schemas get a reversible quarantine rename; drop stays human.
4. **Real plan capture.** Sampled `auto_explain` read from every supported log sink, not only
   Supabase; fallback `EXPLAIN (ANALYZE, BUFFERS)` on a replica under a timeout.
5. **Index consolidation.** Replace overlapping A+B with one covering C, validated with
   `hypopg_hide_index` + hypothetical C on the full workload.
6. **Migration-aware PR output.** When an index or config is app-owned, emit a migration file in
   the framework the app uses (Rails/Alembic/Flyway/Prisma, detected from the migration table)
   instead of DDL.

## 5. AI-first redesign

**Shape.** One **tuning agent per database per cycle** works *cases* (a slow query family, a
bloated table, a config category) with tools, instead of single-shot prompts per package. The deterministic rules stay, but they become
**detectors that open cases** and **validators that close them**. They no longer write the
recommendation text.

**What the model decides**
- Which open cases to work, in priority order, within the per-cycle token budget.
- Hypotheses per case (missing index, correlated columns, plan cache mode, spill, bloat, app
  pattern).
- The candidate action set, including "do nothing; this is app-owned / test data / transient".
- What evidence it still needs, through tools.
- The verification plan: which queryids and metrics, for how long, and what counts as success
  and as regression.

**Tools** (MCP and `llm.ChatWithTools`, all read-only except via `Executor.Apply`):
`get_query_stats(queryid, window)`, `get_plan(queryid, analyze=replica_only)`,
`hypopg_whatif(create[], hide[], queryids[])` returning per-query cost weighted by calls,
`table_profile(table)` (size, write rates from deltas, HOT ratio, reloptions, partitioning),
`column_stats(table, cols)`, `index_usage(index, include_replicas)`,
`ownership(object)` (migration fingerprint, drop/recreate history, workload label),
`outcome_history(action_class, table)` (from the ledger), `guc_info(name)` (pg_settings context,
unit, bounds, platform restriction), and `propose_action(...)`. `propose_action` writes a
recommendation revision and never executes. Tool results are untrusted data.

**Validation.** The model's output is a typed `ActionProposal`. It must pass, deterministically
and in order:
1. sqlast parse and allowlist.
2. Object-existence and ownership checks. App-owned objects are refused and become a PR artifact.
3. HypoPG gate for indexes (fail closed) or the GUC allowlist plus bounds plus context for
   config.
4. Write-cost budget.
5. The existing `policy.Gate`, trust level and cooldown/oscillation checks.

The model cannot raise a risk tier or confidence above what the validator computes. Its
confidence is recorded but only calibrated (below), never trusted raw.

**Closing the loop.** Each executed action gets an **outcome ledger row**: hypothesis, predicted
effect (for example, "q123 mean −60%, writes +3%"), and measured effect at 15m/24h/7d from
querystore deltas, idx_scan, temp_bytes and dead tuples. Verdict: win, neutral, regressed, or
reverted-externally. Verify-and-revert stays the safety net. The ledger feeds:
- **Calibration.** Fit per action class and table profile how often predicted wins occur. That
  replaces the fixed weights in `optimizer/confidence.go` and the 0.74/0.76 constants.
- **Retrieval.** Similar past outcomes go into the prompt.
- **Earned autonomy.** Trust promotion uses measured win rate per class, not only accepted
  reviews. That also fixes "nothing was proposable" on lifeos.
- **Value.** The value model credits measured latency×calls saved, not toil constants.

**Evaluation**
- **Replay bench (PGIncidentBench-style).** Freeze snapshots, plans and querystore windows from
  dogfood and synthetic workloads (lifeos-1 included: leaked test schemas, an app-recreated index,
  a replica-only index, a write-heavy table). Score each model and prompt version on precision
  (no bad DDL), recall (known-good fixes found), refusal correctness (app-owned or test objects
  left alone) and tokens.
- **Shadow mode.** A new prompt or model runs beside production, proposals logged and diffed,
  never applied, until it beats production on the bench and a week of shadow.
- Every reverted action becomes a bench case.

**Cost control**
- Detectors decide whether there is anything to think about: no open case, no call.
- Cases are deduplicated by (case kind, object, evidence hash). An unchanged case is not
  re-reasoned. This fixes the narrator re-running every cycle.
- Model routing: a cheap model for triage and narratives, a stronger model only for proposals
  that reach validation.
- A fleet-wide persisted token and dollar budget, with deterministic rule-only fallback when it
  is exhausted.
- Tool-call caps per case and per cycle.

## 6. Top 5

**1. Outcome ledger + calibration (closes the loop).** Today the brain never learns. Optimizer
confidence is a fixed sum of data-availability signals (`optimizer/confidence.go:20-25`), case
confidences are constants, and verification only asks "did it get worse within 15 minutes". Record
the predicted effect per action, measure the actual per-query effect at 15m/24h/7d from querystore
and idx_scan, and drive confidence, prompts and trust promotion from measured win rates. It also gives
promotions an evidence path beyond "3 accepted reviews in 4 h".

**2. Fix the evidence the brain reasons on.** Plan regressions are estimated-cost ratios that fire
on data growth, captured plans are not ANALYZE plans, query_store rows carry the wrong plan hash,
and unused-index detection ignores replicas and `last_idx_scan`. Gate regressions on plan hash and measured latency, capture real
plans (auto_explain logs or replica ANALYZE), stamp the executed plan, and make replica usage a
hard precondition for drops.

**3. Workload classifier and pre-emptive ownership.** The lifeos migration fight and leaked-schema flood were patched
reactively: app-managed detection needs a lost fight first, and clone collapse would also swallow
tenant schemas. Classify objects up front from migration tables, DDL history, naming
plus activity, and partition trees. Give that label to every rule, prompt and validator. App-owned
fixes become migration PRs, and leaked schemas get a quarantine action.

**4. Make the optimizer gate real.** HypoPG fails open on one bad query, averages improvement
without weighting, has no write-cost model, and rejects every expression index. Fix these (per-query
savepoints, calls-weighted improvement, delta-based write cost, a pg_query-based parser) and the
optimizer's L2+ autonomy becomes defensible. 

**5. One tuning agent with tools, behind the existing gates, evaluated by replay and shadow.**
Today advisor, optimizer, tuner and narrator run separate single-shot prompts with uneven
validation (unknown GUCs pass; no-op hints ship). Consolidate them into a case-driven agent on `ChatWithTools` (used today by rca/sre, not by
any tuning producer). Add typed tools, a deterministic validator chain, model routing, a persisted
fleet budget, and replay + shadow evaluation before any prompt or model change ships. Do the safety fixes first, as they are small and independent of the redesign:
`neutralizeDataTags`, unwrapped raw SQL in the narrator and justifier, volatile functions under
`/explain` ANALYZE, and unqualified DROP INDEX in sqlast.
