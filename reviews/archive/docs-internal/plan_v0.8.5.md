# pg_sage v0.8.5 — Release Spec

**Status:** Draft
**Author:** jmass
**Target ship:** TBD (post v0.8.4 release 2026-04-07)
**Last updated:** 2026-04-11

---

## 1. Release theme

> **Every pg_sage recommendation now has a review cycle.**

v0.8.x has been a write-only advisor: it prescribes, persists, and walks away. Hints accumulate in `hint_plan.hints` forever. Findings pile up with no reverification. The nested-loop prescription path can pin a `HashJoin` hint for a problem that was really stale statistics, then never reassess. Settings fields get added without documentation and drift from the YAML example file.

v0.8.5 closes the loop. Every persistent artifact the sidecar produces — query hints, tuner diagnoses, config fields — gains either a revalidation path, a correct-by-construction mechanism, or both. Nothing about this release is a greenfield feature; all five items are paying off accumulated debt by finishing work that was started and orphaned, or correcting prescription logic that is currently wrong for the most common real-world case.

This is a **correctness and discipline release**, not a feature release. Marketing should describe it as "pg_sage now maintains its own recommendations so you don't have to."

---

## 2. Executive summary

| # | Item | Type | Tier |
|---|---|---|---|
| 1 | Hint revalidation loop (finish `verify_after_apply`) | Debt payoff | P0 |
| 2 | Stale-stats detection + scoped ANALYZE action | Correctness fix | P0 |
| 3 | `work_mem` role-promotion advisor | New advisor | P1 |
| 4 | Extension drift detector | Guardrail | P1 |
| 5 | Config tooltip infrastructure + tier 1-2 field docs | UX + discipline | P1 |

All five land in one release. #1 and #2 are interlocked (#2 is a prerequisite for #1's cost comparison step to be meaningful) and must ship together. #3, #4, and #5 are independent and can be parallelized once #1 and #2 are stable.

**Out of scope** (explicitly deferred to v0.9 with rationale in §14):
- Parameter-sensitive query detection
- `CREATE STATISTICS` advisor
- Index bloat / REINDEX CONCURRENTLY advisor
- `ALTER ROLE` as an executor action (work_mem advisor emits advisory finding only)
- LLM-driven multi-plan experimentation

---

## 3. Feature 1 — Hint Revalidation Loop

### 3.1 Problem

Once the tuner writes a hint to `sage.query_hints` and `hint_plan.hints`, there is no mechanism to reassess it. The hint outlives:

- Stats refresh (the underlying row-skew resolved itself; the hint is now redundant at best, harmful at worst)
- Index additions (the HashJoin hint is now worse than the nested loop + new index)
- Index drops (the `IndexScan(tbl idx)` directive references a nonexistent index)
- Schema changes (the referenced table was renamed or dropped)
- `work_mem` cluster-level tuning (the per-query `Set(work_mem "256MB")` hint is redundant once cluster-wide is ≥ 256MB)
- Queryid churn (the app deployed a rewrite; the original queryid is dead; the hint serves nothing)
- Workload shifts (the query that justified the hint hasn't been called in 30 days)
- PostgreSQL major version upgrade (planner behavior changes invalidate prior cost assumptions)

Today, the only removal path is a human noticing and manually deleting from `hint_plan.hints`. In a 100-hint deployment this is not realistic; in fleet mode across N databases it is not possible.

### 3.2 Current state — what's already scaffolded

This feature is **not greenfield**. The scaffolding exists and was orphaned:

- `internal/schema/bootstrap.go:386-388` — `sage.query_hints` declares `verified_at timestamptz` and `rolled_back_at timestamptz`. **Nothing writes to these columns.**
- `internal/config/config.go:617` — default config sets `verify_after_apply: true`.
- `internal/config/config.go:232` — `VerifyAfterApply bool` field exists in the config struct.
- `internal/config/watcher.go:277-281` — the config watcher reloads this field on YAML changes.
- `internal/tuner/types.go:56` — `TunerConfig.VerifyAfterApply` field exists.
- `cmd/pg_sage_sidecar/main.go:408,815,1113` — threaded from runtime config into the tuner in all three run modes.
- `internal/tuner/*.go` — **never reads `VerifyAfterApply`.** Confirmed by grep: only the struct field declaration matches. The config value is dead; integration test configs set it to `true` but nothing happens.

Every config file shipped since this field was added is lying to users. That's a latent bug, not a feature gap.

### 3.3 Design

Add a `Revalidate(ctx)` method to the tuner package. Scheduling is **not** tied to the analyzer cycle (which drives `Tune()` today at sub-minute cadence). Revalidation runs on a **separate, longer cadence** (default 24h, configurable via `tuner.revalidation_interval_hours`).

**Scheduler implementation:** a new `Tuner.StartRevalidationLoop(ctx)` spawns a dedicated goroutine with a `time.Ticker` sized to `RevalidationIntervalHours`. The loop is started exactly once per run mode from `cmd/pg_sage_sidecar/main.go` alongside the analyzer goroutine:

- **Standalone mode:** one call to `StartRevalidationLoop` with the single pool.
- **Fleet mode:** one call **per database**, each with the per-database tuner instance. No sidecar-global state.
- **One-shot mode:** `Revalidate()` is called once, synchronously, after the single `Tune()` pass and before process exit. No goroutine/ticker in one-shot.

When `VerifyAfterApply` is false, `StartRevalidationLoop` is never called. When true, the loop runs. One-shot mode calls `Revalidate()` unconditionally only if `VerifyAfterApply` is true; otherwise no-op.

**Concurrency with `Tune()`:** `Tuner` gains a `mu sync.Mutex`. Both `Tune()` and `Revalidate()` acquire it for the section that reads/writes `sage.query_hints` and `hint_plan.hints`. The read-only EXPLAIN portions of Check 4 run outside the mutex. This keeps the analyzer cycle fast and prevents the revalidation loop from stalling it.

For every row in `sage.query_hints WHERE status = 'active'`, run the four checks in order (cheap to expensive, short-circuit on first actionable outcome):

#### Check 1 — object existence (free)

Parse the hint directive into a structured form, then consult catalog:

**Directives the parser recognizes** (v0.8.5 exhaustive list):

| Directive | Extracted fields | Catalog lookup |
|---|---|---|
| `HashJoin(alias ...)` | aliases | none (alias is query-local) |
| `NestLoop(alias ...)` | aliases | none |
| `MergeJoin(alias ...)` | aliases | none |
| `IndexScan(alias idx1 idx2 ...)` | alias + index names | `pg_class` for each index |
| `NoIndexScan(alias)` | alias | none |
| `BitmapScan(alias idx ...)` | alias + index names | `pg_class` |
| `SeqScan(alias)` | alias | none |
| `NoSeqScan(alias)` | alias | none |
| `Leading((t1 t2 ...))` | alias list | none |
| `Set(param "value")` | param name, value | `pg_settings` for param existence |
| `Rows(alias ... #N)` / `Rows(alias ... +N)` | alias list | none |

The parser lives in a new file `internal/tuner/hint_parse.go` using strict regex-based matching. One regex per directive head (`^HashJoin\(|^IndexScan\(|...`), then field extraction. No external SQL parser dependency.

**Unparseable directives:** if a hint cannot be parsed (unknown directive head, malformed syntax), Check 1 **skips the hint and logs a `hint_unparseable` finding**. The hint is NOT marked broken — the parser failure is treated as our bug, not the user's. The hint proceeds to Check 2, 3, 4 as normal.

**Object-existence outcomes:** if Check 1 parses successfully and any referenced index is absent from `pg_class`, mark `status = 'broken'`, DELETE from `hint_plan.hints` (subject to race guard in §3.3.6), write `rolled_back_at = now()`, emit `hint_revalidation` finding with severity `warning`. Relation existence is NOT checked for alias-only directives like `HashJoin(a)` because aliases live in the query text, not the catalog.

#### Check 2 — dead queryid (near-free)

`pg_stat_statements` has **no** `last_call` column. Per-query last-call time must be inferred from `calls` deltas between revalidation cycles. Two new columns are added to `sage.query_hints` to track this:

```sql
calls_at_last_check   bigint,
last_revalidated_at   timestamptz
```

(DDL in §3.7; distinct from `verified_at`, which only advances when Check 4 passes with ratio ≥ 1.2.)

**Dead-queryid algorithm:**

1. Join `sage.query_hints` row against `pg_stat_statements` by `queryid`.
2. **Absent from pg_stat_statements at all** → candidate for retirement if `created_at < now() - HintRetirementDays` (protect against freshly-written hints that haven't been run yet). Otherwise defer.
3. **Present, `calls > calls_at_last_check`** → alive. Update `calls_at_last_check = current_calls`, `last_revalidated_at = now()`. Proceed to Check 3.
4. **Present, `calls == calls_at_last_check`, `last_revalidated_at < now() - HintRetirementDays`** → retired. Set `status = 'retired'`, delete from `hint_plan.hints` (race-guarded per §3.3.6).
5. **Present, `calls < calls_at_last_check`** → `pg_stat_statements_reset()` happened. Snap `calls_at_last_check = current_calls`, `last_revalidated_at = now()`, log `hint_revalidation_reset_detected`, proceed to Check 3. Do not retire on reset.
6. **Bootstrap / first cycle after upgrade (`calls_at_last_check IS NULL`)** → capture snapshot, do not retire this cycle. Retirement starts one full cycle after upgrade.

The `HintRetirementDays` field defaults to 14. Retirement emits an informational `hint_revalidation` finding; DELETE is subject to §3.3.6 race-guard.

#### Check 3 — `work_mem` redundancy special case (free)

For `Set(work_mem "NMB")` directives, compare the hint value against **both** the cluster default **and** every role-level override. The hint is only `superseded` if **every** role that could run this query would already get ≥ N MB.

Algorithm:

1. Parse N from the directive.
2. Read cluster default via `SHOW work_mem` on a fresh connection (no role switch).
3. Read role-level overrides:
   ```sql
   SELECT rolname, rolconfig
     FROM pg_roles
    WHERE rolconfig IS NOT NULL;
   ```
   Scan each `rolconfig` entry for `work_mem=...`. Parse the right-hand side (handle `64MB`, `65536kB`, `64000`, etc. — units per PG's `parse_int` with `GUC_UNIT_KB`).
4. Determine the **effective minimum** work_mem across all roles: cluster default, overridden downward by any role-level setting below the default. If **min >= N**, mark `superseded`. Otherwise the hint still has role(s) that need it — leave active, proceed to Check 4.
5. If the hint is marked superseded, DELETE from `hint_plan.hints` (race-guarded per §3.3.6), write `rolled_back_at = now()`, emit info finding. No EXPLAIN required.

**Database-level overrides** (`ALTER DATABASE … SET work_mem`) are **not** considered in v0.8.5. Rationale: they are rare and a false-negative here is preferable to a false-positive that deletes a needed hint. Document the limitation in the finding text.

#### Check 4 — cost comparison (the expensive one)

**Prepared-statement normalization gate.** `pg_stat_statements.query` stores the **normalized** query text with `$1`/`$2` placeholders for parameterized queries. `EXPLAIN` cannot plan a query text that contains placeholders without type information. Check 4 therefore:

1. If the normalized query text contains `$N` placeholders, **skip cost comparison** for this cycle. Mark the hint with `revalidation_method='deferred_parameterized'` in the finding detail. The hint still benefits from Check 1 (object existence), Check 2 (dead queryid), and Check 3 (work_mem) which do not require EXPLAIN.
2. Otherwise proceed with the two-plan comparison below.

**Two-plan comparison.** For the remaining hints, run two `EXPLAIN (FORMAT JSON)` calls on a dedicated revalidation connection:

1. With `SET LOCAL pg_hint_plan.enable_hint = on` (default) — captures current "with hint" cost.
2. With `SET LOCAL pg_hint_plan.enable_hint = off` — captures current "without hint" cost.

Both calls use `EXPLAIN` **without** `ANALYZE` (do not execute the query). Cost is estimator output, not runtime.

**Statement timeout.** Each EXPLAIN runs with `SET LOCAL statement_timeout = cfg.Tuner.RevalidationExplainTimeoutMs` (default 10000, 10s). Timeout → skip with `revalidation_method='deferred_explain_timeout'`, logged.

**EXPLAIN errors.** If either EXPLAIN returns an error (permission denied, table dropped, syntax drift), the hint is treated the same as a Check 1 broken-object case: mark `status='broken'`, DELETE subject to race guard, emit warning finding including the Postgres error text. The hint cannot function if its underlying query cannot be planned.

**Cost extraction.** Parse the `"Total Cost"` field of the root plan node from the JSON output. If either cost is ≤ 0 or `NaN`, skip with `revalidation_method='deferred_invalid_cost'` and log.

Three outcomes based on `ratio = without_cost / with_cost`:

| Ratio | Interpretation | Action |
|---|---|---|
| ratio ≥ 1.2 | Hint is still winning by ≥20% | Update `verified_at = now()`, leave active |
| 0.8 ≤ ratio < 1.2 | Hint is cost-neutral | `status = 'superseded'`, DELETE, info finding |
| ratio < 0.8 | Hint is actively harmful | `status = 'rolled_back'`, DELETE, `warning` finding with before/after costs |

The 1.2 / 0.8 thresholds are configurable under `tuner.revalidation_keep_ratio` and `tuner.revalidation_rollback_ratio`, defaults as shown.

#### Trust-level gating

- **`observation` mode:** run the full loop, emit findings for every case, but **never DELETE** from `hint_plan.hints` and never update `query_hints.status`. Observation mode is read-only by contract.
- **`advisory` mode:** emit findings and a pending recommendation in `sage.action_queue` for human approval. Do not execute the DELETE.
- **`autonomous` mode:** execute DELETEs directly, write `rolled_back_at`, update `status`, emit findings for the audit trail.

This matches the existing executor gating model in `internal/advisor/vacuum.go` and must reuse the same helper, not duplicate it.

#### 3.3.6 Shared-directive race guard

A single row in `hint_plan.hints` (keyed on `(norm_query_string, application_name)`) can be referenced by **multiple** `sage.query_hints` rows — two queryids that normalize to the same text (e.g., only differing in literal values) will share the same `hint_plan.hints` row. A blind DELETE when revalidating **one** queryid would silently kill the hint for the other.

**Guard procedure**, executed inside the tuner mutex (§3.3 "Concurrency with Tune()") for every DELETE path:

1. Before DELETE from `hint_plan.hints`, query:
   ```sql
   SELECT count(*) FROM sage.query_hints
    WHERE status = 'active'
      AND hint_text = $1
      AND id <> $2;
   ```
   where `$1` is the hint text and `$2` is the row being revalidated.
2. If count = 0, proceed with DELETE from `hint_plan.hints`.
3. If count > 0, **skip the DELETE**, still update the `sage.query_hints` row's `status` and `rolled_back_at`/`verified_at` as computed, and emit a finding with detail `{shared_directive: true, surviving_refs: N}`. The other queryid keeps the hint alive.

When the *last* remaining active reference eventually retires, its DELETE proceeds.

#### 3.3.7 Fleet-mode isolation

In fleet mode, each database runs its own `Revalidate()` goroutine on its own pool. Revalidation never reaches across databases:

- Each `Tuner` has a single pool, bound to one database. It reads `sage.query_hints` from **that** database's `sage` schema and writes `hint_plan.hints` to **that** database only.
- The `database_name` field in every emitted finding must be the target database, never `"all"` or `""`. This is a known failure pattern (CLAUDE.md §5 Fleet-mode leaks); explicit test coverage is mandatory (see §11).
- No global state is shared between per-database revalidation loops except the logger.

### 3.4 Caveat: estimates vs actuals

Check 4 compares **planner estimates**, not runtime cost. If statistics are stale, both "with hint" and "without hint" estimates are garbage and the ratio is meaningless. This is the direct dependency on Feature 2: stale-stats detection must run first, ANALYZE must fire if indicated, and revalidation runs on fresh statistics.

Implementation sequence within a single revalidation cycle:
1. For each active hint, look up the relations in the query plan.
2. Run Feature 2's stale-stats check against those relations.
3. If stale stats detected, emit ANALYZE finding, **skip revalidation for this hint until next cycle** (stats will be fresh by then). Log the skip explicitly — no silent skips per CLAUDE.md.
4. Otherwise proceed with Check 4.

### 3.5 Acceptance criteria

- CHECK-R01: `VerifyAfterApply=false` — `Revalidate()` is never called by the scheduler. Verified via log inspection in integration test.
- CHECK-R02: `VerifyAfterApply=true`, trust level `observation` — loop runs, findings emitted, zero rows modified in `sage.query_hints`, zero rows deleted from `hint_plan.hints`. Verified by row count diff.
- CHECK-R03: `VerifyAfterApply=true`, trust level `autonomous`, hint references dropped index — hint row has `status='broken'` and `rolled_back_at IS NOT NULL` after one cycle. `hint_plan.hints` row gone.
- CHECK-R04: `VerifyAfterApply=true`, trust level `autonomous`, `Set(work_mem "64MB")` hint, cluster `work_mem = 128MB` — status becomes `superseded`, DELETE executes. No EXPLAIN calls made (verified by `pg_stat_statements` delta on the test connection).
- CHECK-R05: `VerifyAfterApply=true`, autonomous, hint whose queryid has not been called in 20 days (configured retention = 14) — status becomes `retired`.
- CHECK-R06: `VerifyAfterApply=true`, autonomous, fresh stats, hint is cost-neutral — `superseded`, DELETE.
- CHECK-R07: `VerifyAfterApply=true`, autonomous, fresh stats, hint is harmful (without-hint cost < 0.8 × with-hint cost) — `rolled_back`, DELETE, warning finding.
- CHECK-R08: `VerifyAfterApply=true`, stale stats detected for a hint's relations — revalidation of that hint is skipped for this cycle, skip is logged with reason, next cycle re-attempts after ANALYZE has run.
- CHECK-R09: Two consecutive revalidation cycles with no hint state changes — second cycle updates `verified_at` but does not touch `hint_plan.hints`.
- CHECK-R10: Revalidation cycle interrupted mid-loop by context cancellation — partial progress is safe; interrupted hints retain prior state. No half-deleted hints.
- CHECK-R11: Shared-directive race guard — two `sage.query_hints` rows reference the same `(norm_query_string, application_name)` in `hint_plan.hints`. Revalidating the first to retirement must NOT DELETE from `hint_plan.hints`; status updates on the query_hints row only; surviving row still resolves the hint at query time. Verified by `hint_plan.hints` row count diff + live query that uses the hint.
- CHECK-R12: `pg_stat_statements_reset()` called mid-revalidation — `calls < calls_at_last_check` → snap snapshot, do not retire, emit `hint_revalidation_reset_detected` log. Verified by issuing `SELECT pg_stat_statements_reset()` between cycles.
- CHECK-R13: Bootstrap / first cycle after upgrade with `calls_at_last_check IS NULL` — revalidation captures snapshot, does not retire any hint this cycle. Verified by row-state assertion.
- CHECK-R14: Parameterized query (pg_stat_statements normalized text contains `$1`) — Check 4 is skipped with `revalidation_method='deferred_parameterized'`; Checks 1/2/3 still run; no EXPLAIN issued (verified via `pg_stat_statements` delta).
- CHECK-R15: Revalidation `Tune()` concurrency — fire `Tune()` and `Revalidate()` concurrently against the same DB; both complete without deadlock; `sage.query_hints` rowset is consistent (no torn writes, no lost updates).
- CHECK-R16: Unparseable hint directive (fabricated `BogusHint(x)` in `sage.query_hints.hint_text`) — parser logs `hint_unparseable`, hint is NOT marked broken, Checks 2/3/4 still run.
- CHECK-R17: `work_mem` hint 64MB, cluster default 128MB, one role has `ALTER ROLE ... SET work_mem = '32MB'` — hint is NOT marked superseded (at least one role needs it).
- CHECK-R18: EXPLAIN exceeds `revalidation_explain_timeout_ms` — hint deferred with `revalidation_method='deferred_explain_timeout'`, not retired, no state change.
- CHECK-R19: Fleet-mode isolation — 3 databases, one retires a hint, other two unaffected; every emitted finding has correct `database_name` (not "all", not empty).

### 3.6 New / modified files

- `internal/tuner/revalidate.go` — new. `Revalidate(ctx)` entry point, four check functions, cost comparison, cycle telemetry.
- `internal/tuner/revalidate_test.go` — new. Unit tests for state transitions, share-directive race guard, reset handling.
- `internal/tuner/revalidate_integration_test.go` — new, `//go:build integration`. Real PG via docker-compose.test.yml.
- `internal/tuner/hint_parse.go` — new. Regex-based directive parser for the eleven directives listed in Check 1.
- `internal/tuner/hint_parse_test.go` — new. Exhaustive directive parse table.
- `internal/tuner/scheduler.go` — new. `StartRevalidationLoop(ctx)` method on `Tuner`; owns the goroutine + ticker.
- `internal/tuner/types.go` — add `HintRetirementDays int`, `RevalidationKeepRatio float64`, `RevalidationRollbackRatio float64`, `RevalidationIntervalHours int`, `RevalidationExplainTimeoutMs int` to `TunerConfig`. Add `mu sync.Mutex` on `Tuner` struct.
- `internal/tuner/tuner.go` — acquire `mu` around `sage.query_hints` write sections of `Tune()`.
- `cmd/pg_sage_sidecar/main.go` — call `StartRevalidationLoop` once per run mode (standalone: once, fleet: per-database, one-shot: synchronous `Revalidate()` call, no loop).
- `internal/config/config.go` — add the five new config fields with defaults and `doc` tags (Feature 5).
- `internal/config/watcher.go` — reload the five new fields.
- `internal/schema/bootstrap.go` — migration for the two new columns on `sage.query_hints` (see §3.7).
- `config.example.yaml` — document all five under `tuner:`.

### 3.7 Schema changes

Two new columns on `sage.query_hints` to support the dead-queryid check in §3.3 Check 2:

```sql
ALTER TABLE sage.query_hints
    ADD COLUMN IF NOT EXISTS calls_at_last_check bigint,
    ADD COLUMN IF NOT EXISTS last_revalidated_at timestamptz;
```

Added via `internal/schema/bootstrap.go` DDL (idempotent `ADD COLUMN IF NOT EXISTS`). No backfill required — NULL is the bootstrap state and is handled explicitly in Check 2 step 6.

**Distinction from existing columns** (important, do not collapse):

| Column | Semantics | Written by |
|---|---|---|
| `created_at` | Hint first written | Tune() |
| `verified_at` | Last time Check 4 passed with ratio ≥ keep_ratio | Revalidate(), Check 4 success only |
| `last_revalidated_at` | Last time any revalidation check inspected this row | Revalidate(), every cycle |
| `calls_at_last_check` | `pg_stat_statements.calls` snapshot at `last_revalidated_at` | Revalidate(), Check 2 |
| `rolled_back_at` | Row removed from `hint_plan.hints` | Revalidate(), on retirement/rollback/superseded |

`verified_at` and `last_revalidated_at` intentionally diverge: a hint that gets inspected daily but only cost-verified when its relations' stats are fresh will have `last_revalidated_at` advance every day while `verified_at` advances more rarely.

**`status` vocabulary.** Existing: `'active'`. New values used: `'broken'`, `'retired'`, `'superseded'`, `'rolled_back'`. No CHECK constraint exists on `status` today (confirmed via bootstrap DDL review). If one is added in a future release, it must include these five values.

---

## 4. Feature 2 — Stale-stats detection + scoped ANALYZE action

### 4.1 Problem

The tuner currently detects `SymptomBadNestedLoop` (`internal/tuner/planscan.go:143-165`) when a Nested Loop node has `ActualRows > PlanRows * 10`, and prescribes `HashJoin(alias)` as a pg_hint_plan directive. The prescription is written to `hint_plan.hints` and persists permanently.

**This is wrong for the most common real-world case.** Row-estimate drift on a nested loop is usually caused by stale statistics, not by the planner fundamentally choosing the wrong strategy. The correct fix is `ANALYZE schema.table`, which is transient, cheap, and reversible. Instead, the tuner pins a HashJoin hint that:

- Does not fix the underlying problem (stats are still stale, other queries on the same table are still making bad choices)
- Cannot be un-pinned without revalidation (Feature 1) or manual intervention
- Can become actively wrong once fresh statistics land and the optimizer's natural choice becomes correct

The tuner has no ANALYZE action anywhere in the executor surface. The only ANALYZE-adjacent code is `internal/analyzer/rules_vacuum.go`, which detects bloat and XID wraparound — it does not feed back into query plan correctness.

### 4.2 Current state

Confirmed by grep in §Feature 1 investigation:

- `internal/tuner/types.go:6-15` — `SymptomKind` constants: `disk_sort`, `hash_spill`, `high_plan_time`, `bad_nested_loop`, `seq_scan_with_index`, `parallel_disabled`, `sort_limit`, `stat_temp_spill`. **No stale-stats symptom.**
- `internal/tuner/planscan.go:143-165` — `checkBadNestedLoop` is the only row-estimate skew check. Nested-loop only.
- `internal/tuner/rules.go:75-85` — prescription is `HashJoin(alias)`, no ANALYZE option.
- Tuner candidate fetch (`internal/tuner/tuner.go:216-226`) does not consult `pg_stat_user_tables` or `last_analyze`.

### 4.3 Design

#### 4.3.1 New symptom — `SymptomStaleStats`

Add to `internal/tuner/types.go`:

```go
SymptomStaleStats SymptomKind = "stale_stats"
```

Detection runs in `planscan.go` and fires when **all five** conditions hold for a relation referenced by a plan node:

1. `ActualRows > PlanRows * staleStatsEstimateSkewThreshold` (default 10, same as nested-loop check).
2. The relation is identifiable from the plan node (Seq Scan, Index Scan, Bitmap Heap Scan, or Nested Loop inner — walks all row-producing nodes, not just nested loops).
3. The relation's `pg_class.relkind` is in `{'r', 'm', 'p'}` (regular table, materialized view, partitioned table). **Excluded** relkinds: `'t'` (temp — session-scoped, cannot be analyzed by another backend), `'f'` (foreign — no local stats), `'v'` (view — has no storage), `'i'` (index — analyzed via its owning table), `'S'` (sequence).
4. Catalog lookup via `pg_stat_user_tables` shows `n_mod_since_analyze > reltuples * staleStatsModRatio` (default 0.1, i.e. 10% of rows modified since last analyze). For partitioned parent tables (`relkind='p'`), aggregate `n_mod_since_analyze` and `reltuples` across the partition hierarchy via `pg_inherits`.
5. `COALESCE(last_autoanalyze, '-infinity')` < `now() - staleStatsAgeMinutes` **AND** `COALESCE(last_analyze, '-infinity')` < `now() - staleStatsAgeMinutes` (default 60 minutes — both manual and auto must be stale, NULL treated as ancient).

All five gates must pass to emit the symptom. This is intentionally strict: the failure mode "ANALYZE fires on a 500GB table during peak traffic because of a false positive" is more damaging than "we miss some stale-stats cases and the existing HashJoin fallback fires instead."

**Catalog cache:** batched once per cycle. Before walking plan nodes, fetch:

```sql
SELECT c.oid, c.relnamespace::regnamespace::text AS schema, c.relname,
       c.relkind, c.reltuples, s.n_mod_since_analyze,
       s.last_analyze, s.last_autoanalyze
  FROM pg_class c
  LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
 WHERE c.relkind IN ('r','m','p');
```

Cache the result in a `map[string]staleStatsRow` keyed by canonical `schema.table` for the cycle only. Do not query per-hint. Rebuild on each cycle (stats are cheap to fetch; no cross-cycle caching).

#### 4.3.2 New prescription — full-table `ANALYZE`

Add to `internal/tuner/rules.go`:

```go
case SymptomStaleStats:
    return prescribeStaleStats(symptom, cfg)
```

Prescription produces:

1. `HintDirective` empty string — stale-stats is not a hint, it's an action.
2. New `AnalyzeTarget` field on `Prescription`: canonical `"schema.table"` string (no column list).
3. Finding's `RecommendedSQL` is `ANALYZE "schema"."table"` with identifiers passed through `pq.QuoteIdentifier`-equivalent (hand-rolled helper; the project has no pq dependency).
4. Rollback SQL is empty — ANALYZE has no undo.

**Column-scoped ANALYZE is deferred.** The spec initially considered `ANALYZE schema.table (col1, col2, ...)` to reduce cost on wide tables, but pg_sage has no SQL parser dependency (confirmed: `go.mod` contains no `pg_query_go` / `vitess` / similar). Regex-based predicate extraction is too error-prone for production, and an incorrect column list is worse than a full-table ANALYZE because it produces silently-skewed statistics. Full-table ANALYZE for v0.8.5; column-scoped deferred to v0.9 together with a real SQL parser dependency decision.

#### 4.3.3 Executor integration — reuse existing `analyze` categorization

`executor.categorizeAction()` at `internal/executor/executor.go:562-583` already maps SQL strings beginning with `ANALYZE` to the `analyze` action type. **No new `ActionKind` enum is introduced.** The tuner's ANALYZE path is:

1. Tuner's `prescribeStaleStats` builds a `Finding` with `RecommendedSQL = "ANALYZE \"schema\".\"table\""` and category `stale_statistics`.
2. The existing executor loop (`executor.RunCycle`) picks up the finding on its normal cadence, categorizes it as `analyze`, applies trust gating (`internal/executor/trust.go:17-58`), and dispatches to an execution path.
3. New file `internal/executor/analyze.go` implements the `executeAnalyze()` function called by the dispatch when the categorized action is `analyze` and the originating category is `stale_statistics` (to distinguish tuner-issued ANALYZE from any future user-issued ANALYZE).

**Connection handling.** ANALYZE cannot run inside a transaction block. Use the existing "dedicated raw connection" pattern from `internal/executor/ddl.go:44-91` (`pool.Acquire(ctx)`, SET conn-level timeouts, execute outside transaction, release). Do NOT use `ExecInTransaction` — it wraps the call in `BEGIN…COMMIT`.

**Execution sequence**, on the dedicated connection:

```sql
SET lock_timeout = '${cfg.Safety.LockTimeoutMs}ms';           -- conn-level
SET statement_timeout = '${cfg.Tuner.AnalyzeTimeoutMs}ms';    -- conn-level
ANALYZE "${schema}"."${table}";                               -- no parenthesized column list
SET lock_timeout = 0;
SET statement_timeout = 0;
```

On completion, release the connection back to the pool. `sage.action_log` records the action with `action_type='analyze'`, `outcome='applied'` on success. Rollback is a no-op: `rolled_back_at` is set to the execution completion time and `rollback_sql` is empty.

**Error handling.**

| Error | Action |
|---|---|
| `lock_timeout` reached | Mark `outcome='skipped_lock_contention'`, extend cooldown to `2 × AnalyzeCooldownMinutes`, emit info finding |
| `statement_timeout` reached | Mark `outcome='failed_timeout'`, extend cooldown to `2 × AnalyzeCooldownMinutes`, emit warning finding |
| Table dropped between detection and execution | Mark `outcome='failed_missing'`, warning finding, no cooldown extension |
| Any other PG error | Mark `outcome='failed'`, log error text, warning finding |

#### 4.3.4 Safety gates on ANALYZE

ANALYZE is safer than CREATE INDEX but it is not free. Gates are evaluated in this order, short-circuiting on first denial:

1. **Trust-level gating.** `observation` → finding only, no execution. `advisory` → enqueue in `sage.action_queue`, no execution until approved. `autonomous` → proceed to next gate.
2. **Table size cap.** `pg_relation_size(oid) > cfg.Tuner.AnalyzeMaxTableMB * 1024 * 1024` → skip, emit advisory finding with category `stale_statistics_deferred_large_table`. Default 10GB (`analyze_max_table_mb: 10240`).
3. **Maintenance window for medium/large tables.** `pg_relation_size(oid) > cfg.Tuner.AnalyzeMaintenanceThresholdMB * 1024 * 1024` AND current time not within `cfg.Trust.MaintenanceWindow` → defer, no finding (will retry next cycle). Default 1GB (`analyze_maintenance_threshold_mb: 1024`).
4. **Per-table cooldown.** Last ANALYZE for this table (from `pg_stat_user_tables.last_analyze` OR `sage.action_log` whichever is more recent) within `cfg.Tuner.AnalyzeCooldownMinutes` → skip silently, log only. Default 60.
5. **Concurrent ANALYZE cap.** `cfg.Tuner.MaxConcurrentAnalyze` (default 1) is enforced via a **single shared semaphore** created in `cmd/pg_sage_sidecar/main.go` at startup and passed to every per-database Executor instance. In fleet mode with 10 DBs and `max_concurrent_analyze=1`, at most one ANALYZE runs sidecar-wide at a time. The semaphore is a `chan struct{}` with buffer size `MaxConcurrentAnalyze`; executor acquires before execution, releases in `defer`.
6. **Lock-timeout pre-check.** Before execution, fail fast if `cfg.Safety.LockTimeoutMs == 0` (unlimited lock wait is forbidden for ANALYZE because autovacuum ANALYZE on the same table holds `ShareUpdateExclusiveLock` and could block indefinitely). Require explicit non-zero value; default 5000ms via existing `safety.lock_timeout_ms`.

#### 4.3.5 Remove the stale-stats HashJoin prescription

In `checkBadNestedLoop`, before returning `SymptomBadNestedLoop`, consult the stale-stats catalog cache for the nested-loop's inner relation. If stale stats are detected, return `SymptomStaleStats` instead. This is the correctness fix: we stop pinning hints for problems that ANALYZE will fix.

If stats are fresh and the row skew is still large, the HashJoin prescription is legitimate (the optimizer genuinely chose the wrong strategy) and fires as before.

### 4.4 Acceptance criteria

- CHECK-S01: Stats are stale on a test table (`n_mod_since_analyze = 50% of reltuples`, `last_analyze > 2h old`). Query with nested-loop row skew → tuner emits `SymptomStaleStats`, NOT `SymptomBadNestedLoop`. No HashJoin hint written to `hint_plan.hints`.
- CHECK-S02: Same setup, trust=`autonomous` — ANALYZE action executes, `pg_stat_user_tables.last_analyze` advances, tuner re-runs and no longer emits the symptom.
- CHECK-S03: Same setup, trust=`observation` — finding emitted, ANALYZE does not execute.
- CHECK-S04: Stats are fresh (`last_analyze < 5min ago`), row skew is large — tuner falls through to `SymptomBadNestedLoop`, prescribes `HashJoin`, writes hint.
- CHECK-S05: Table size exceeds `AnalyzeMaxTableMB` — finding is emitted with `severity=info` and a recommendation string, no ANALYZE executes, advisory finding persists for human action.
- CHECK-S06: ANALYZE cooldown — same table targeted twice within cooldown window, only first execution runs. Second emits a log message and is suppressed.
- CHECK-S07: Two tables targeted simultaneously in fleet mode with `MaxConcurrentAnalyze=1` — second is serialized behind the first. Verified via timing.
- CHECK-S08: ANALYZE fails mid-execution (simulated via `lock_timeout` trigger) — action is marked failed in `sage.action_log`, `rolled_back_at` set, error is logged with enough context to debug.
- CHECK-S09: Identifier quoting — relation named `"Orders.Archive"` (with period in name) → SQL is `ANALYZE "public"."Orders.Archive"` (round-trip through identifier quoter). Assert issued SQL byte-for-byte.
- CHECK-S10: Relkind filter — temp table (`relkind='t'`), foreign table (`relkind='f'`), view (`relkind='v'`) with plan-level row skew → no `SymptomStaleStats` emitted for any. Verified per table type.
- CHECK-S11: Maintenance window gating — 2GB table at 3pm (outside window) → defer, no finding, next cycle retries. Same table at 2am (inside window) → executes.
- CHECK-S12: Known failure pattern — ANALYZE attempted inside an existing transaction → detected at pre-flight, uses dedicated connection via `pool.Acquire`, does not fail. Assert the executor path did NOT call `pool.Begin`.
- CHECK-S13: Stat-based negative — `n_mod_since_analyze = 0` and `last_analyze = 1h ago` → no symptom, even if plan row skew is high (the issue isn't stats).
- CHECK-S14: Partitioned parent — parent table (`relkind='p'`) with 4 partitions, one partition has `n_mod_since_analyze / reltuples = 0.5` → aggregate exceeds threshold → `ANALYZE parent` fires, cascade to children via PG semantics. Verified by post-ANALYZE `last_analyze` on all children.
- CHECK-S15: `lock_timeout=0` forbidden — executor refuses to run ANALYZE when `safety.lock_timeout_ms = 0`, logs a configuration error finding.
- CHECK-S16: Shared semaphore fleet-mode — 3 databases, `max_concurrent_analyze=1`, three ANALYZE findings raised simultaneously → they serialize through the global semaphore (not per-database). Verified by `pg_stat_activity` concurrent count assertion.
- CHECK-S17: Cooldown source — if `pg_stat_user_tables.last_analyze` advanced due to autovacuum within the cooldown window, tuner-issued ANALYZE is suppressed. Verified by triggering an autovacuum ANALYZE and then asking the tuner to re-analyze the same table.

### 4.5 New / modified files

- `internal/tuner/stale_stats.go` — new. Catalog cache builder, `IsStale(schemaTable string) bool`, relkind filter, partition aggregation.
- `internal/tuner/stale_stats_test.go` — new unit tests (no PG).
- `internal/tuner/stale_stats_integration_test.go` — new, `//go:build integration`. Real PG + real `pg_stat_user_tables` state.
- `internal/tuner/rules.go` — add `prescribeStaleStats` (emits canonical-quoted `ANALYZE` SQL).
- `internal/tuner/ident.go` — new. Identifier quoting helper (no new dep).
- `internal/tuner/types.go` — `SymptomStaleStats` constant, `AnalyzeTarget` field on `Prescription`, eight new config fields (see §10).
- `internal/tuner/planscan.go` — `checkBadNestedLoop` consults stale-stats cache; returns `SymptomStaleStats` when stats are stale, `SymptomBadNestedLoop` otherwise.
- `internal/executor/analyze.go` — new. `executeAnalyze()` function, dedicated-connection pattern, safety gates, error mapping.
- `internal/executor/analyze_test.go` — new unit tests for gate logic.
- `internal/executor/analyze_integration_test.go` — new, `//go:build integration`. Exercises the full path against docker-compose PG.
- `internal/executor/executor.go` — route `analyze` + `stale_statistics` category into `executeAnalyze`; accept the shared semaphore via constructor.
- `cmd/pg_sage_sidecar/main.go` — create the shared `chan struct{}` sized to `MaxConcurrentAnalyze`, pass to every Executor instance.
- `config.example.yaml` — document new `tuner.analyze_*` and `tuner.stale_stats_*` fields.

### 4.6 Schema changes

No new tables. `sage.action_log` will record ANALYZE actions — verify its existing `action_type` column accepts new values, or add an enum/check update.

---

## 5. Feature 3 — `work_mem` role-promotion advisor

### 5.1 Problem

When the tuner prescribes `Set(work_mem "256MB")` for many queries on the same role, the user ends up with dozens of per-query hints all doing the same thing. This is fragile:

- Each hint must be individually revalidated (Feature 1 work scales linearly).
- A role-level default would be cheaper and clearer.
- Users who look at `hint_plan.hints` see noise that hides the actual custom-plan workarounds.

### 5.2 Design

Add a `work_mem_promotion` analyzer rule, runs in the existing analyzer cycle, not the tuner hot path. SQL:

```sql
SELECT
    r.rolname AS role_name,
    (regexp_match(h.hint_text, 'Set\(work_mem "(\d+)MB"\)'))[1]::int AS hint_mb,
    count(*) AS hint_count,
    max((regexp_match(h.hint_text, 'Set\(work_mem "(\d+)MB"\)'))[1]::int) AS max_mb
FROM sage.query_hints h
JOIN pg_stat_statements s USING (queryid)
JOIN pg_roles r ON r.oid = s.userid
WHERE h.status = 'active'
  AND h.hint_text ~ 'Set\(work_mem "\d+MB"\)'
GROUP BY r.rolname
HAVING count(*) >= $1;
```

Notes:
- Uses `pg_roles` (not `pg_user`) so NOLOGIN roles are reachable — a common pattern is workload role inheriting from group role.
- The ORIGINAL spec grouped on `(role, mb)`; the tightened version groups on `role only` and selects `max_mb`, implementing CHECK-W04's semantic (tiebreak by max) directly in SQL. Hints with different sizes for the same role are still counted.
- Regex uses `~` operator with anchored-capture form to survive cases where the hint_text contains other Set directives.
- Threshold parameter `$1` bound to `cfg.Analyzer.WorkMemPromotionThreshold`.

Emit a finding with category `work_mem_promotion`, severity `info`. The recommendation SQL must quote the role name:

```sql
ALTER ROLE "${quote_ident(role_name)}" SET work_mem = '${max_mb}MB';
```

`quote_ident` is implemented in the analyzer as a helper — no new dependency; wraps the identifier in double-quotes and doubles any embedded quotes. Mandatory because role names can contain mixed case, spaces, or reserved words.

Rollback:

```sql
ALTER ROLE "${quote_ident(role_name)}" RESET work_mem;
-- Then re-inspect sage.query_hints: affected queries may regenerate hints
```

The finding's `RecommendedSQL` is the ALTER ROLE statement. **It is advisory only in v0.8.5** — the executor does not gain `ALTER ROLE` as an action kind. Users apply it manually or via their own migration process. Adding ALTER ROLE as an autonomous action surface is deferred to v0.9.

Rationale for advisory-only: role-level changes affect every future query for that role, not just the ones the tuner has analyzed. Autonomous role-level changes need a much more careful design than v0.8.5 can accommodate.

### 5.3 Acceptance criteria

- CHECK-W01: Five `Set(work_mem "256MB")` hints for the same role → one finding emitted with correct `ALTER ROLE` SQL.
- CHECK-W02: Four hints → no finding (below threshold).
- CHECK-W03: Five hints split across two roles (3+2) → no finding.
- CHECK-W04: Five hints for same role but different `work_mem` values (128MB, 256MB, 256MB, 256MB, 512MB) → one finding, using the *maximum* value (512MB) because downgrading would re-trigger the hints.
- CHECK-W05: Finding persists across analyzer cycles until the role is updated or hints are retired. Verified by running three cycles and checking finding count stays at 1.
- CHECK-W06: After Feature 1 revalidation retires 4 of the 5 hints → work_mem finding drops below threshold and is auto-resolved on next analyzer cycle.
- CHECK-W07: Role name requires quoting — role `"Mixed Case"` (literal, with space) → recommendation SQL is `ALTER ROLE "Mixed Case" SET work_mem = '256MB'`, passes `pg_query_parse()` as valid SQL. Verified by running the emitted SQL against a live DB.
- CHECK-W08: NOLOGIN role inherits via group — `worker_login` role has no direct hints, inherits from `workers` group, hints are attributed to `workers` → finding emits for `workers`, not `worker_login`. Assert `role_name` field.
- CHECK-W09: Role name matches a reserved word (e.g., role literally named `user`) — `quote_ident` wraps it, emitted SQL parses.

### 5.4 New / modified files

- `internal/analyzer/rules_work_mem.go` — new.
- `internal/analyzer/rules_work_mem_test.go` — new.
- `internal/analyzer/analyzer.go` — wire the rule into the analyzer cycle.
- `internal/config/config.go` — add `Analyzer.WorkMemPromotionThreshold int` (default 5).
- `config.example.yaml` — document it.

---

## 6. Feature 4 — Extension drift detector

### 6.1 Problem

If `pg_hint_plan` is removed from `shared_preload_libraries` after sidecar startup, the hints in `hint_plan.hints` are no longer being applied by the planner. The tuner continues to write new hints, findings accumulate, the user believes the sidecar is working. It is silently useless.

Same for `pg_stat_statements` — if it is disabled or dropped, the tuner's candidate query (`internal/tuner/tuner.go:216-226`) returns zero rows, and the tuner reports "nothing to tune," which is indistinguishable from a healthy system.

This is a production-grade silent failure. A sidecar that reports success while doing nothing is worse than a sidecar that crashes.

### 6.2 Design

Add a per-cycle preflight check at the top of both `Tune()` and `Revalidate()`:

```go
func (t *Tuner) checkRequiredExtensions(ctx context.Context) error {
    required := []string{"pg_stat_statements"}
    optional := []string{"pg_hint_plan"} // required iff VerifyAfterApply or hint writes are active

    for _, ext := range required {
        if !t.extensionAvailable(ctx, ext) {
            return fmt.Errorf("required extension %s not available", ext)
        }
    }
    // pg_hint_plan check
    ...
}
```

`extensionAvailable` queries `pg_extension` and confirms the extension is installed in the current database. For `pg_hint_plan` specifically, also verify `shared_preload_libraries` contains it (via `SHOW shared_preload_libraries`), because installation alone is not sufficient.

**Managed-service scoping.** On RDS / Aurora / Cloud SQL / AlloyDB, `pg_stat_statements` and `pg_hint_plan` are installed **per-database**. A sidecar connected to database `app_prod` checks that extensions are present in `app_prod` only — it makes no claim about other databases on the same cluster. In fleet mode, each per-database Tuner instance runs its own preflight independently; one database missing `pg_stat_statements` does not disable the others (CHECK-E06). Document this in the finding text so operators understand why the finding names a specific database rather than the cluster.

When a required extension is missing:

1. Emit a `sage.findings` row with `category='extension_drift'`, `severity='critical'`, recommendation = "install or enable ${ext}".
2. Abort the current `Tune()` / `Revalidate()` cycle with a logged error.
3. Enter a throttled retry mode: check once per 5 minutes instead of every cycle, to avoid log spam.
4. On next successful check, emit a recovery finding and resume normal operation.

For `pg_hint_plan` specifically: if it has `shared_preload_libraries` drift but the extension is still installed, treat as critical (hints in `hint_plan.hints` are being ignored). If the extension is dropped entirely, treat as critical and **refuse to write new hints** until it returns.

### 6.3 Acceptance criteria

- CHECK-E01: `pg_stat_statements` absent at startup → sidecar starts, logs error, emits critical finding, tuner loop is a no-op until extension appears.
- CHECK-E02: `pg_stat_statements` present at startup, dropped mid-operation → next tune cycle emits the finding, subsequent cycles are throttled to 5-minute retry.
- CHECK-E03: `pg_stat_statements` recovers → recovery finding emitted, full cycle cadence resumes.
- CHECK-E04: `pg_hint_plan` installed but removed from `shared_preload_libraries` → finding emitted, `Tune()` refuses to write new hints, existing hints in `query_hints` still revalidate (because their effect is already zero).
- CHECK-E05: `pg_hint_plan` not installed at all, optional usage → informational finding, tuner proceeds without writing hints, ANALYZE actions still execute (not dependent on pg_hint_plan).
- CHECK-E06: Fleet mode with 3 databases, one missing `pg_stat_statements` → 2 databases run normally, 1 emits the finding and throttles. Verify isolation: the bad database does not affect the other two.
- CHECK-E07: Retry throttle — check that during the 5-minute backoff, the extension check is not re-issued (verified via pg_stat_activity query count delta).

### 6.4 New / modified files

- `internal/tuner/extensions.go` — new. Check logic, throttle state.
- `internal/tuner/extensions_test.go` — new.
- `internal/tuner/tuner.go` — preflight call at top of `Tune()`.
- `internal/tuner/revalidate.go` — preflight call at top of `Revalidate()`.
- `internal/analyzer/analyzer.go` — similar preflight, since analyzer also depends on `pg_stat_statements` for query identification.

---

## 7. Feature 5 — Config tooltip infrastructure

### 7.1 Problem

`SettingsPage.jsx` is 957 lines with dozens of form fields. `DatabaseSettingsPage.jsx` is 224 lines. Most fields have **zero inline documentation**. Users must cross-reference `config.example.yaml` in the repo to understand what a field means.

The YAML example file has hand-authored comments for most fields, but they are not surfaced in the UI. Fields added since the last YAML example update drift silently. There is no mechanism to prevent a developer from adding a new config field without documentation.

Several fields have non-obvious mode-dependent behavior that is not captured anywhere:
- `trust.tier3_high_risk` is "always false in standalone mode"
- `ramp_start: ""` auto-persists on first start
- Some settings apply globally in standalone but per-database in fleet
- `verify_after_apply` will go from "dead config" in v0.8.4 to "daily hint revalidation" in v0.8.5 — users upgrading must be told

Users hit these as production surprises.

### 7.2 Current state

- No tooltip component exists. Grep of `web/src/` for `tooltip|Tooltip|HelpCircle|InfoIcon` returns zero matches beyond `title=` attributes on three reset buttons.
- `config.example.yaml` has authored inline comments for ~60% of fields.
- `internal/config/config.go` is the canonical source for what fields exist.
- No tooltip primitive dependency (no Radix, no Floating UI) currently in `web/package.json`.

### 7.3 Design — single source of truth

**Author tooltip content in Go struct tags**, generate `config_meta.json` at build time, consume from React.

#### 7.3.1 Struct tag format

Extend `internal/config/config.go` structs:

```go
type TunerConfig struct {
    Enabled bool `yaml:"enabled" doc:"Enable per-query tuning. When false, no hints are written and Tune() is a no-op."`
    VerifyAfterApply bool `yaml:"verify_after_apply" doc:"Run daily hint revalidation. Removes hints that have become redundant, harmful, or broken. Requires pg_hint_plan." warning:"Enables a background cycle that issues EXPLAIN queries. Low impact but not zero."`
    // ...
}
```

Tag fields:
- `doc` (required): one-sentence plain description, 20–200 chars.
- `warning` (optional): callout text for dangerous or production-impacting fields.
- `mode` (optional): `"standalone-only"`, `"fleet-only"`, `"both"`. Defaults to `"both"`.
- `docs_url` (optional): path fragment appended to the docs base URL for "Read more →" link.
- `secret` (optional): `"true"` marks the field as sensitive — the generator never emits a `default` value for it and the tooltip UI renders a "sensitive — not shown in UI" badge. Required on every field whose name matches `/(api_key|password|secret|token|tls_(cert|key)|encryption_key)/i`. The drift test enforces this match.

#### 7.3.2 Generator

New file: `cmd/gen_config_meta/main.go` (build tool, not shipped in the sidecar binary).

Uses reflection over an instance populated by `config.DefaultConfig()`. Walks every struct field, extracts tags, emits JSON.

**Yaml tag parsing.** The yaml library supports comma-separated options (`yaml:"name,omitempty"`). The generator parses the tag value and takes only the **first** comma-separated token as the field name. Tokens beyond the first are ignored.

**Recursion rules.**
- **Value struct** → recurse, prepending the parent key with a dot.
- **Pointer to struct** → follow if non-nil, recurse as value; if nil, skip.
- **Slice of struct** → emit metadata for the element type once, using key suffix `[]`. Example: `databases[].name`. Slice length is not recursively indexed.
- **Slice of scalar** → emit as type `[]<scalar_type>`.
- **Map** → emit as type `map[K]V` opaque, do not recurse into map values. `doc` must describe the semantics of keys and values.
- **`yaml:"-"` tag or no yaml tag at all** → skip the field entirely (this excludes runtime-only fields like `ConfigPath`, `PGVersionNum`, and legacy env-var fields like `APIKey`, `TLSCert`, `TLSKey`).
- **Unexported field** → skip.

**Output schema:**

```json
{
  "tuner.enabled": {
    "type": "bool",
    "default": false,
    "doc": "Enable per-query tuning...",
    "warning": null,
    "mode": "both",
    "docs_url": "/configuration.md#tuner",
    "secret": false
  },
  "llm.api_key": {
    "type": "string",
    "default": null,
    "doc": "...",
    "secret": true
  },
  "databases[].name": {
    "type": "string",
    "default": "",
    "doc": "Logical name for this database in fleet mode",
    "mode": "fleet-only"
  },
  ...
}
```

**Determinism.** Struct field order is deterministic (declaration order via `reflect.Type.NumField()`), and the generator writes keys in sorted order to guarantee byte-stable output across runs. Assertion: two consecutive invocations produce byte-identical files (CHECK-T03).

**Output path:** `sidecar/web/src/generated/config_meta.json`. Committed to git so web builds have no dependency on the Go toolchain.

Wire via `go generate ./...` in the sidecar build process. Add a CI check (Feature 5's drift test) that the generated file matches what the generator would produce — fails the build on stale metadata.

#### 7.3.3 Tooltip React component

Dependency add: `@radix-ui/react-tooltip` (~15KB gzipped). Boring technology, WCAG-compliant out of the box, portal-based (no clipping).

New file: `sidecar/web/src/components/ConfigTooltip.jsx` (~60 lines):

```jsx
import * as Tooltip from '@radix-ui/react-tooltip';
import configMeta from '../generated/config_meta.json';

export function ConfigTooltip({ configKey, children }) {
  const meta = configMeta[configKey];
  if (!meta) return children;
  // Render Radix Tooltip with meta.doc, meta.warning callout,
  // meta.docs_url "Read more" link when present.
}
```

Usage in settings pages:

```jsx
<ConfigTooltip configKey="tuner.verify_after_apply">
  <label>Verify after apply</label>
</ConfigTooltip>
```

The `configKey` matches the key in `config_meta.json` (dot-path through the struct).

#### 7.3.4 Field tiers for v0.8.5

Not all ~80 fields get tooltips in v0.8.5. Tier 1-2 only.

**Tier 1 (must ship, can cause incidents when set wrong):**
- All `trust.*` fields
- `safety.query_timeout_ms`, `safety.lock_timeout_ms`, `safety.ddl_timeout_seconds`
- `safety.cpu_ceiling_pct`, `safety.backoff_consecutive_skips`
- `rollback_threshold_pct`, `rollback_window_minutes`, `rollback_cooldown_days`
- `trust.maintenance_window`
- `tuner.verify_after_apply` (must document the new behavior)

**Tier 2 (should ship):**
- All `tuner.*` fields including the new v0.8.5 additions
- Core `analyzer.*` thresholds: `unused_index_window_days`, `table_bloat_*`, `xid_wraparound_*`
- `llm.*` fields (API keys, model, fallback)
- Fleet-mode-specific field annotations

**Tier 3+ (incremental, post-v0.8.5):**
- Collector intervals, batch sizes
- Retention periods
- Notification channel details
- Read-only display fields (no tooltips)

v0.8.5 ships with tier 1 + tier 2 tooltips and the drift test ensuring no future field can be added without docs. Tier 3+ is filled in over subsequent releases as the drift test catches gaps.

#### 7.3.5 Drift test

New file: `internal/config/meta_drift_test.go`.

```go
func TestConfigMetaCoversAllFields(t *testing.T) {
    // 1. Walk Config{} via reflection
    // 2. For each yaml:"..." tag, assert a matching doc:"..." tag exists and is non-empty
    // 3. Assert sidecar/web/src/generated/config_meta.json has a matching entry
    // 4. For tier 1 fields (listed in test fixture), assert they exist in config_meta.json
    //    and their doc strings are > 20 chars and < 200 chars
}
```

Mandatory. Fails the build if someone adds a config field without documentation.

#### 7.3.6 Rendering test

New file: `sidecar/web/e2e/tooltips.spec.ts` (Playwright).

Navigates to `/settings`, tab-focuses each known tooltip trigger, asserts tooltip content becomes visible and contains the expected doc text. Catches:

- Tooltip inside a clipped container (Radix portal should prevent this, but test it)
- Missing `configKey` wiring (tooltip silently renders `children` with no doc)
- Doc text not matching `config_meta.json` (build drift)

### 7.4 Acceptance criteria

- CHECK-T01: `TestConfigMetaCoversAllFields` passes — every field in `Config{}` has a `doc` tag.
- CHECK-T02: Adding a new field to `Config{}` without a `doc` tag fails the test.
- CHECK-T03: `cmd/gen_config_meta` produces deterministic output (running it twice yields identical JSON).
- CHECK-T04: `config_meta.json` is committed; CI fails if the file is out of sync with `config.go` tags.
- CHECK-T05: `ConfigTooltip` component renders Radix tooltip when `configKey` has metadata, renders plain children when metadata is missing (graceful degradation).
- CHECK-T06: Tooltip opens on keyboard focus (Tab), not just mouse hover. Playwright test.
- CHECK-T07: Tooltip dismisses on Escape.
- CHECK-T08: Tooltip with `warning` field renders visually distinct from info-only tooltips (different color class).
- CHECK-T09: Tier 1 fields all have tooltips rendering in `SettingsPage.jsx` — verified by Playwright walking each field.
- CHECK-T10: Docs URL "Read more →" link points at a valid `docs/*.md` path (checked against filesystem in unit test).
- CHECK-T11: Secret field handling — `llm.api_key` has `secret:"true"` tag; `config_meta.json` entry has `default: null` and `secret: true`; drift test fails if any field matching the sensitive-name regex lacks the tag.
- CHECK-T12: Yaml tag option parsing — field with tag `yaml:"name,omitempty"` generates key `name` (not `name,omitempty`).
- CHECK-T13: Slice-of-struct handling — `databases[].name`, `databases[].host`, `databases[].port` all appear in `config_meta.json` with `mode: "fleet-only"`.
- CHECK-T14: Map handling — `webhooks[].headers` appears as `type: "map[string]string"`, no recursion into values.
- CHECK-T15: Skip-behavior for `yaml:"-"` — runtime fields (`ConfigPath`, `PGVersionNum`, `HasWALColumns`) do NOT appear in `config_meta.json`.
- CHECK-T16: Generator refuses to run if any `doc` tag is shorter than 20 or longer than 200 chars — fails with file:line of the offending field.

### 7.5 New / modified files

- `cmd/gen_config_meta/main.go` — new. Reflection-based generator.
- `cmd/gen_config_meta/main_test.go` — new.
- `internal/config/config.go` — add `doc`, `warning`, `mode`, `docs_url` tags to all existing fields. Large mechanical edit.
- `internal/config/meta_drift_test.go` — new.
- `sidecar/web/src/generated/config_meta.json` — new, generated, committed.
- `sidecar/web/src/components/ConfigTooltip.jsx` — new.
- `sidecar/web/package.json` — add `@radix-ui/react-tooltip` dependency.
- `sidecar/web/src/pages/SettingsPage.jsx` — wrap tier 1-2 field labels in `<ConfigTooltip>`. Largest diff of the release.
- `sidecar/web/src/pages/DatabaseSettingsPage.jsx` — same, smaller diff.
- `sidecar/web/e2e/tooltips.spec.ts` — new Playwright test.

### 7.6 Risk: tag edit is mechanical but massive

Adding `doc:"..."` tags to every field in `config.go` is a ~400-line diff in a single critical file. Mitigation:

1. Land the generator tool and the drift test first, in its own PR, with `doc` tags added for only 3-5 representative fields. Verify the pipeline works end to end.
2. Then land the bulk tag edit as a separate PR. Reviewers can skim it because the generator output diff (`config_meta.json`) is the actual review surface — they read the user-facing strings, not the Go tag syntax.
3. Then land the SettingsPage.jsx wiring changes in a third PR.

Three-PR sequence de-risks the large mechanical change.

---

## 8. Sequencing and dependencies

```
Feature 2 (stale-stats)  ──┐
                           ├──→ Feature 1 (revalidation) needs fresh stats for meaningful cost comparison
Feature 2 (ANALYZE action) ─┘

Feature 4 (extension drift) ──→ Independent. Land early; simplest.

Feature 3 (work_mem promo)  ──→ Independent. Land alongside Feature 1 — it reads from query_hints which Feature 1 also touches.

Feature 5 (tooltips)        ──→ Mostly independent. Must land AFTER Feature 1-2 config fields are added so their tooltips can be authored in the same release.
```

Suggested landing order:

1. **Week 1:**
   - Feature 4 (extension drift) — smallest, lowest risk, unblocks the rest by guaranteeing preflight health checks.
   - Feature 5 PR 1 (tooltip generator + drift test + 3 representative fields) — infrastructure only.
   - Feature 2 part A (stale-stats detection in planscan + symptom + prescription — no ANALYZE action yet).

2. **Week 2:**
   - Feature 2 part B (`ActionAnalyze` executor kind + safety gates + integration tests).
   - Feature 2 part C (remove HashJoin fallback for stale-stats cases — the correctness fix).
   - Feature 5 PR 2 (bulk `doc` tag edit in `config.go` + regenerated `config_meta.json`).

3. **Week 3:**
   - Feature 1 (revalidation loop — all four checks, scheduler wiring, trust-level gating).
   - Feature 3 (work_mem promotion advisor).
   - Feature 5 PR 3 (SettingsPage.jsx tooltip wiring + Playwright test).

4. **Week 4:**
   - Integration testing across all five features.
   - Release notes, CHANGELOG entry, version bump.
   - v0.8.5 tag.

This is a suggestion, not a commitment. Week numbers are ordering, not time budgets.

---

## 9. Schema changes (consolidated)

No new tables. Modifications to existing tables:

| Table | Change | Feature |
|---|---|---|
| `sage.query_hints` | `ADD COLUMN calls_at_last_check bigint`, `ADD COLUMN last_revalidated_at timestamptz` (idempotent `IF NOT EXISTS`). Start using `verified_at`, `rolled_back_at`, expand `status` vocabulary (`broken`, `retired`, `superseded`, `rolled_back`, `hint_unparseable` as a log category). | 1 |
| `sage.action_log` | `action_type` is already a free-form text column (no CHECK constraint per scout). New value `'analyze'` accepted by the existing `categorizeAction()`. No schema change required. | 2 |
| `sage.findings` | New categories: `hint_revalidation`, `hint_revalidation_reset_detected`, `hint_unparseable`, `stale_statistics`, `stale_statistics_deferred_large_table`, `work_mem_promotion`, `extension_drift`. All authored as strings; `category` column is text. | 1, 2, 3, 4 |

**Migration ordering:** the column additions on `sage.query_hints` run in `internal/schema/bootstrap.go` using idempotent DDL. They run on first startup after upgrade before the Tuner's first cycle, so Check 2 bootstrap state (NULL → snapshot-only) is well-defined.

**Downgrade:** the two new columns are additive and nullable. Downgrading to v0.8.4 leaves the columns present but unused. No data loss.

---

## 10. Config additions (consolidated)

New YAML fields, all under `tuner:` unless noted:

```yaml
tuner:
  verify_after_apply: true                    # existing field, now functional
  hint_retirement_days: 14                    # Feature 1
  revalidation_interval_hours: 24             # Feature 1
  revalidation_keep_ratio: 1.2                # Feature 1
  revalidation_rollback_ratio: 0.8            # Feature 1
  revalidation_explain_timeout_ms: 10000      # Feature 1
  stale_stats_estimate_skew: 10.0             # Feature 2
  stale_stats_mod_ratio: 0.1                  # Feature 2
  stale_stats_age_minutes: 60                 # Feature 2
  analyze_max_table_mb: 10240                 # Feature 2, 10GB
  analyze_cooldown_minutes: 60                # Feature 2
  analyze_maintenance_threshold_mb: 1024      # Feature 2, 1GB
  analyze_timeout_ms: 600000                  # Feature 2, 10min
  max_concurrent_analyze: 1                   # Feature 2

analyzer:
  work_mem_promotion_threshold: 5             # Feature 3
```

Every new field must have a `doc` tag (Feature 5). No field ships without a tooltip.

---

## 11. Testing plan

Per CLAUDE.md testing standards. Non-negotiable.

### 11.1 Two-phase test writing

Each feature is implemented in two passes:

1. **Phase 1 — Write tests.** Before implementation, author the tests for all required categories from the CLAUDE.md test table. Commit or stage. Do not run.
2. **Phase 2 — Implement and run.** Write the implementation, run the full suite, track bugs found, fix implementation (not tests).

No feature merges without both phases logged in its PR description. The PR template (add to `.github/pull_request_template.md`) gains two checkboxes:

- [ ] Phase 1 complete: tests committed before implementation (link commit)
- [ ] Phase 2 complete: implementation, `go test -cover -count=1 ./...`, bug list reported

### 11.2 Test harness tiers

pg_sage uses four test tiers, each with a distinct build tag and runner. v0.8.5 uses all four.

| Tier | Build tag | Purpose | Runner | Examples (v0.8.5) |
|---|---|---|---|---|
| **Unit** | none | Pure Go logic, no I/O | `go test -count=1 ./...` | `hint_parse_test.go`, `stale_stats_test.go` (map-mocked catalog) |
| **Integration** | `//go:build integration` | Real PG, single process | `docker-compose -f docker-compose.test.yml up -d pg-target && go test -count=1 -tags=integration ./...` | `revalidate_integration_test.go`, `analyze_integration_test.go` |
| **E2E** | `//go:build e2e` | Full sidecar binary against real PG + fleet topology | `sidecar/e2e/run.sh` driving compiled binary against `docker-compose.test.yml` | existing `fleet_test.go`, new `revalidation_e2e_test.go` |
| **Functional** | shell | Workload scripts + observation | `test-fixtures/workload-*.sh` + `test-fixtures/verify.sh` | new `workload-stale-stats.sh`, new `workload-dead-queryid.sh` |

**Web-side tests:**

| Tier | Runner | Scope |
|---|---|---|
| **Unit (Vitest)** | `npm test` in `sidecar/web/` | `ConfigTooltip.jsx` behavior with mock `config_meta.json` |
| **E2E (Playwright)** | `npx playwright test` in `sidecar/web/` | `tooltips.spec.ts`, existing walkthrough spec |

### 11.3 Test infrastructure reuse and additions

**Reused as-is (no changes):**

- `docker-compose.test.yml` — already exposes `pg-target` (5433) and `pg-target-2` (5434) with `pg_stat_statements` preloaded + `track_io_timing=on`. Supports fleet tests.
- `test-fixtures/llm-mock/main.go` — OpenAI-compatible mock on port 11434, supports mode-switch via `/setMode` endpoint, logs all requests. v0.8.5 reuses for any LLM-adjacent test path (Feature 1's revalidation does NOT invoke LLM; reuse is for regression testing that LLM paths still call `stripToJSON`).
- `test-fixtures/seed.sql` — customers (10k), products (500), orders (500k), bloat_target (100k), sequences.

**Additions (new files in `test-fixtures/`):**

- `seed_stale_stats.sql` — extends seed with: a wide_events table (1M rows with autovacuum disabled via `ALTER TABLE … SET (autovacuum_enabled = false)`), a partitioned_events table (`PARTITION BY RANGE(event_date)` with 4 partitions), a temp table in a long-lived session via `pg_dblink` or a helper psql backend, a foreign table via `postgres_fdw` pointing back at pg-target-2, a materialized view.
- `seed_work_mem_hints.sql` — seeds `sage.query_hints` with five `Set(work_mem "256MB")` hints for role `app_worker`, plus mixed-size fixture (CHECK-W04), plus quoted-role fixture ("Mixed Case"), plus NOLOGIN role pattern.
- `seed_dead_queryid.sql` — seeds `sage.query_hints` with a hint whose queryid corresponds to a query text that is **not** present in `pg_stat_statements` (simulate churn via `pg_stat_statements_reset()` after seed).
- `seed_shared_directive.sql` — seeds two `sage.query_hints` rows with distinct queryids but identical `(norm_query_string, application_name)` mapping into `hint_plan.hints`.
- `workload-stale-stats.sh` — bulk-updates 30% of wide_events rows, issues a join against orders that produces nested-loop row-skew. Driven by `psql` in a loop. Used for CHECK-S01, S02, S04.
- `workload-dead-queryid.sh` — runs query A for 10 rounds, then issues `pg_stat_statements_reset()`, then runs query B (different text) for 10 rounds. Establishes "hint exists for queryid that stopped being called."
- `workload-hint-revalidation.sh` — drives a full Tune → Revalidate cycle end-to-end: issue bad query, observe hint written, issue better data, observe hint retired.
- `workload-extension-drop.sh` — drops and recreates `pg_stat_statements` mid-cycle to trigger Feature 4 detector.

**Docker-compose changes:**

- Add a second postgres image variant in `docker-compose.test.yml` under a new profile `ext-drop-test`: identical to `pg-target` but with `pg_stat_statements` **not** preloaded at start. Used for CHECK-E01.
- Add an llm-mock mode `revalidation_noop` to the mock's known modes — returns 200 with empty text so the regression path can confirm no LLM call happens during revalidation. (Defense-in-depth: revalidation should never call LLM; this test ensures we catch a regression.)

**Playwright bootstrap (new — does not exist today):**

`sidecar/web/` has no Playwright setup today. Feature 5 adds:

- `sidecar/web/playwright.config.ts` — base URL `http://localhost:5173` (Vite dev server), headed=false, single browser (Chromium) for CI determinism.
- `sidecar/web/e2e/` directory (new).
- `sidecar/web/e2e/fixtures/auth.ts` — reuse the walkthrough auth setup from existing e2e if present; otherwise spin up a test user via the backend's `/api/auth/register` (exercised in v0.8.4 tests).
- `sidecar/web/e2e/tooltips.spec.ts` — new test covering T06, T07, T09.
- `sidecar/web/package.json` — add `@playwright/test` as a dev dependency, add `npm run test:e2e` script.
- `sidecar/web/README.md` — Playwright run instructions.

### 11.4 Per-feature test inventory — every CHECK maps to a concrete test

**Notation:** `path/to/file.go::TestFuncName` for Go, `path/to/spec.ts::it('name')` for Playwright/Vitest, `shell:path/to/script.sh` for functional.

#### Feature 1 — Hint revalidation

| CHECK | Test |
|---|---|
| R01 | `internal/tuner/scheduler_test.go::TestRevalidationLoopDisabledByConfig` |
| R02 | `internal/tuner/revalidate_test.go::TestObservationModeNoWrites` |
| R03 | `internal/tuner/revalidate_integration_test.go::TestCheck1DroppedIndexMarkedBroken` |
| R04 | `internal/tuner/revalidate_integration_test.go::TestCheck3WorkMemSuperseded` |
| R05 | `internal/tuner/revalidate_integration_test.go::TestCheck2DeadQueryidRetired` |
| R06 | `internal/tuner/revalidate_integration_test.go::TestCheck4NeutralSuperseded` |
| R07 | `internal/tuner/revalidate_integration_test.go::TestCheck4HarmfulRolledBack` |
| R08 | `internal/tuner/revalidate_integration_test.go::TestStaleStatsSkipsCheck4` |
| R09 | `internal/tuner/revalidate_integration_test.go::TestConsecutiveCyclesIdempotent` |
| R10 | `internal/tuner/revalidate_test.go::TestContextCancellationPreservesState` |
| R11 | `internal/tuner/revalidate_integration_test.go::TestSharedDirectiveRaceGuard` |
| R12 | `internal/tuner/revalidate_integration_test.go::TestPgStatStatementsResetDetection` |
| R13 | `internal/tuner/revalidate_integration_test.go::TestBootstrapFirstCycleNoRetire` |
| R14 | `internal/tuner/revalidate_integration_test.go::TestParameterizedQueryDeferred` |
| R15 | `internal/tuner/revalidate_integration_test.go::TestTuneRevalidateConcurrency` |
| R16 | `internal/tuner/hint_parse_test.go::TestUnparseableDirectiveNotMarkedBroken` |
| R17 | `internal/tuner/revalidate_integration_test.go::TestWorkMemRoleOverrideKeepsHint` |
| R18 | `internal/tuner/revalidate_integration_test.go::TestExplainTimeoutDeferred` |
| R19 | `sidecar/e2e/revalidation_fleet_e2e_test.go::TestFleetModeIsolation` |

#### Feature 2 — Stale-stats + ANALYZE

| CHECK | Test |
|---|---|
| S01 | `internal/tuner/stale_stats_integration_test.go::TestStaleStatsOverridesBadNestedLoop` |
| S02 | `internal/executor/analyze_integration_test.go::TestAutonomousAnalyzeExecutes` |
| S03 | `internal/executor/analyze_integration_test.go::TestObservationAnalyzeNoExec` |
| S04 | `internal/tuner/stale_stats_integration_test.go::TestFreshStatsFallsToHashJoin` |
| S05 | `internal/executor/analyze_integration_test.go::TestLargeTableSizeCap` |
| S06 | `internal/executor/analyze_integration_test.go::TestAnalyzeCooldown` |
| S07 | `internal/executor/analyze_integration_test.go::TestSharedSemaphoreFleetSerialization` |
| S08 | `internal/executor/analyze_integration_test.go::TestLockTimeoutRecovery` |
| S09 | `internal/tuner/ident_test.go::TestIdentifierQuotingEdgeCases` + `internal/executor/analyze_integration_test.go::TestQuotedTableNames` |
| S10 | `internal/tuner/stale_stats_test.go::TestRelkindFilter` (table-driven: r/m/p/t/f/v/i/S) |
| S11 | `internal/executor/analyze_integration_test.go::TestMaintenanceWindowGating` |
| S12 | `internal/executor/analyze_integration_test.go::TestDedicatedConnectionNotInTransaction` |
| S13 | `internal/tuner/stale_stats_test.go::TestStatBasedNegative` |
| S14 | `internal/tuner/stale_stats_integration_test.go::TestPartitionedParentAggregation` |
| S15 | `internal/executor/analyze_test.go::TestLockTimeoutZeroRefused` |
| S16 | `internal/executor/analyze_integration_test.go::TestSharedSemaphoreGlobalCap` + `shell:test-fixtures/verify_fleet_analyze_serialization.sh` |
| S17 | `internal/executor/analyze_integration_test.go::TestCooldownUsesAutovacuumLastAnalyze` |

Functional/e2e:

- `shell:test-fixtures/workload-stale-stats.sh` + `test-fixtures/verify.sh` asserting: `last_analyze` advanced AND no HashJoin hint in `hint_plan.hints` AND finding with category `stale_statistics` in `sage.findings`. Covers F2 end-to-end for release sign-off.

#### Feature 3 — work_mem promotion

| CHECK | Test |
|---|---|
| W01 | `internal/analyzer/rules_work_mem_integration_test.go::TestFiveHintsOneFinding` |
| W02 | `internal/analyzer/rules_work_mem_integration_test.go::TestFourHintsNoFinding` |
| W03 | `internal/analyzer/rules_work_mem_integration_test.go::TestSplitAcrossRoles` |
| W04 | `internal/analyzer/rules_work_mem_integration_test.go::TestMixedSizesUsesMax` |
| W05 | `internal/analyzer/rules_work_mem_integration_test.go::TestFindingPersistsAcrossCycles` |
| W06 | `internal/analyzer/rules_work_mem_integration_test.go::TestAutoResolveAfterRevalidation` (joint F1+F3) |
| W07 | `internal/analyzer/rules_work_mem_integration_test.go::TestQuotedRoleNameSQLValidates` |
| W08 | `internal/analyzer/rules_work_mem_integration_test.go::TestNoLoginRoleInheritsViaGroup` |
| W09 | `internal/analyzer/rules_work_mem_integration_test.go::TestReservedWordRoleName` |

#### Feature 4 — Extension drift

| CHECK | Test |
|---|---|
| E01 | `internal/tuner/extensions_integration_test.go::TestMissingPgStatStatementsAtStartup` (uses `ext-drop-test` docker profile) |
| E02 | `internal/tuner/extensions_integration_test.go::TestExtensionDroppedMidOperation` |
| E03 | `internal/tuner/extensions_integration_test.go::TestExtensionRecoveryFinding` |
| E04 | `internal/tuner/extensions_integration_test.go::TestHintPlanSharedPreloadDrift` |
| E05 | `internal/tuner/extensions_integration_test.go::TestHintPlanOptionalAbsent` |
| E06 | `sidecar/e2e/extension_drift_fleet_e2e_test.go::TestFleetModeOneDBAffected` |
| E07 | `internal/tuner/extensions_test.go::TestRetryThrottleNoExtraQueries` (uses mock pgx pool counter) |

Functional:

- `shell:test-fixtures/workload-extension-drop.sh` — starts sidecar, verifies finding absent, drops `pg_stat_statements`, waits 2 cycles, asserts critical finding present and tuner loop throttled.

#### Feature 5 — Config tooltips

| CHECK | Test |
|---|---|
| T01 | `internal/config/meta_drift_test.go::TestAllFieldsHaveDocTag` |
| T02 | `internal/config/meta_drift_test.go::TestNewFieldWithoutDocFails` (uses a negative fixture struct) |
| T03 | `cmd/gen_config_meta/main_test.go::TestDeterministicOutput` |
| T04 | `internal/config/meta_drift_test.go::TestGeneratedJSONMatchesCommitted` |
| T05 | `sidecar/web/src/components/ConfigTooltip.test.jsx::TestGracefulDegradation` (Vitest) |
| T06 | `sidecar/web/e2e/tooltips.spec.ts::it('opens on keyboard focus')` |
| T07 | `sidecar/web/e2e/tooltips.spec.ts::it('dismisses on Escape')` |
| T08 | `sidecar/web/e2e/tooltips.spec.ts::it('renders warning callout for warning fields')` |
| T09 | `sidecar/web/e2e/tooltips.spec.ts::it('tier 1 fields all have tooltips')` (iterates a test fixture list) |
| T10 | `internal/config/meta_drift_test.go::TestDocsURLsExist` (filesystem check against `docs/`) |
| T11 | `internal/config/meta_drift_test.go::TestSecretFieldsMarked` + `cmd/gen_config_meta/main_test.go::TestSecretDefaultNil` |
| T12 | `cmd/gen_config_meta/main_test.go::TestYamlTagOptionsParsing` |
| T13 | `cmd/gen_config_meta/main_test.go::TestSliceOfStructKeyFormat` |
| T14 | `cmd/gen_config_meta/main_test.go::TestMapOpaque` |
| T15 | `cmd/gen_config_meta/main_test.go::TestYamlDashSkipped` |
| T16 | `cmd/gen_config_meta/main_test.go::TestDocLengthBounds` |

### 11.5 Fault injection tests

Mandatory for every feature that touches the database. Implemented via a test helper `internal/testutil/faultinjector.go` (new) that wraps pgx calls and fails N-th call with a configured error. The helper supports:

- **Connection-reset mid-query** — inject `io.EOF` on query
- **`lock_timeout` simulation** — inject `SQLSTATE 55P03 lock_not_available`
- **`statement_timeout` simulation** — inject `SQLSTATE 57014 query_canceled`
- **Permission denied** — inject `SQLSTATE 42501`
- **Serialization failure** — inject `SQLSTATE 40001`

Required fault-injection tests for v0.8.5:

| Test | Asserts |
|---|---|
| `TestRevalidationHandlesEOFMidCheck4` | Connection drops between `EXPLAIN` with-hint and without-hint — row state unchanged, error logged, next cycle retries |
| `TestAnalyzeHandlesLockTimeout` | `55P03` during ANALYZE → outcome=`skipped_lock_contention`, extended cooldown, not marked failed-permanent |
| `TestAnalyzeHandlesStatementTimeout` | `57014` during ANALYZE → outcome=`failed_timeout`, finding emitted |
| `TestAnalyzeHandlesPermissionDenied` | `42501` → outcome=`failed_permission`, error preserved in finding detail |
| `TestExtensionCheckHandlesConnReset` | Connection lost during `pg_extension` query → throttle engaged, no critical finding (transient error) |
| `TestSharedDirectiveGuardHandlesSerializationFailure` | Concurrent retirement attempt returns `40001` on the count query → retry once, then bail |

### 11.6 Concurrency and race tests

Executed with `go test -race` enforced in CI. New required races:

- `TestTuneRevalidateConcurrency` (R15) — spawn two goroutines: one calling `Tune()`, one calling `Revalidate()`, each 100 iterations, assert no panics, no data races, final `sage.query_hints` state is one of the two deterministic end states.
- `TestSharedSemaphoreFleetSerialization` (S16) — three per-database tuners in goroutines, each requesting ANALYZE, assert at most `MaxConcurrentAnalyze` are in execution at any moment (checked via `pg_stat_activity` polling).
- `TestRevalidationCatalogCacheIsNotShared` — two per-database tuners in fleet mode build their own stale-stats cache; one's cache never leaks into the other.

### 11.7 LLM tests (via test-fixtures/llm-mock)

Feature 1 and Feature 2 do NOT add new LLM paths. However, because they modify the tuner package, regression tests verify that existing LLM paths still behave correctly:

- `TestLLMPrescribeStillCallsStripToJSON` — confirms `stripToJSON` is invoked when `tryLLMPrescribe` receives a markdown-wrapped response from the mock. Known failure pattern per CLAUDE.md §5.
- `TestRevalidationDoesNotCallLLM` — sets llm-mock to `revalidation_noop` mode, runs a full revalidation cycle, asserts the mock's request log has zero entries. Defense-in-depth.
- `TestLLMTimeoutFallsBackToFallbackClient` — existing test, re-run to ensure Feature 1's changes did not disrupt the fallback path.

### 11.8 Playwright tests (new setup)

New Playwright bootstrap described in §11.3. Initial specs for v0.8.5:

- `tooltips.spec.ts` — covers T06, T07, T08, T09. ~120 lines. Runs against a locally-spawned dev server (`npm run dev &`) plus a test user created via the API.
- Existing walkthrough spec (if present in v0.8.4) — re-run to ensure tooltip insertion did not break the settings page navigation.

CI hook: `sidecar/web/.github/workflows/playwright.yml` — runs on every web change, blocks merge on failure.

### 11.9 Required test categories per feature (CLAUDE.md table)

All eight CLAUDE.md categories must be exercised per feature:

| Category | F1 (files / tests) | F2 | F3 | F4 | F5 |
|---|---|---|---|---|---|
| **Happy path** | `revalidate_integration_test.go::TestCheck*` (R02-R07) | `stale_stats_integration_test.go::TestStaleStatsOverrides*` (S01-S04) | `rules_work_mem_integration_test.go::TestFiveHints*` (W01) | `extensions_integration_test.go::TestMissing*` (E01, E05) | `meta_drift_test.go::TestAllFieldsHaveDocTag` (T01) |
| **Invalid input** | malformed plan JSON, NaN cost, negative ratio; `revalidate_test.go::TestInvalidCostExtracted` | `stale_stats_test.go::TestMalformedCatalogRow` | `rules_work_mem_test.go::TestMalformedHintText` | `extensions_test.go::TestBogusExtensionName` | `gen_config_meta/main_test.go::TestMalformedYamlTag` |
| **Nil/empty/zero** | `revalidate_test.go::TestEmptyHintSetNoOp`; zero hints, nil pool | `stale_stats_test.go::TestZeroRowTable`, `TestNullLastAnalyze` | `rules_work_mem_test.go::TestNoHintsNoFinding` | `extensions_test.go::TestExtensionVersionNil` | `main_test.go::TestEmptyStructNoMeta` |
| **Error propagation** | EXPLAIN fails; `revalidate_integration_test.go::TestExplainErrorMarksBroken` | ANALYZE lock_timeout + statement_timeout + perm_denied (fault injector) | `rules_work_mem_test.go::TestPgStatQueryError` | `extensions_test.go::TestPgExtensionQueryFails` | `meta_drift_test.go::TestGenerationError` |
| **Boundary** | `revalidate_test.go::TestRatioExactly_08` + `TestRatioExactly_12`; `HintRetirementDays` exactly at now-14d | stats exactly at age limit; mod_ratio exactly at threshold | threshold boundary (4, 5, 6) | 5-minute retry window edge | `doc` string exactly 20 and 200 chars |
| **Concurrent** | `TestTuneRevalidateConcurrency` (R15) | `TestSharedSemaphoreFleetSerialization` (S16) | rule runs concurrently with tuner writing new hints | `TestRetryThrottleUnderConcurrentCycles` | n/a |
| **State transitions** | observation→advisory→auto (R02, implied) | stale→fresh→stale (S04 then S02) | below→above→below threshold (W02→W01→W06) | missing→recovered (E02→E03) | n/a |
| **Integration** | real PG, real hints, dockerized pg-target | real ANALYZE, real autovacuum interaction | real `sage.query_hints`, real `pg_stat_statements` | real `pg_extension` DROP | Playwright + real Vite dev server |

### 11.10 Coverage targets

Per `go test -cover -count=1 ./...`, reported in the CLAUDE.md format:

| Package | Floor | Rationale |
|---|---|---|
| `internal/tuner/` | 78% | Business logic; raised from v0.8.4 baseline |
| `internal/tuner/revalidate.go` (file-level proxy via func coverage) | 85% | New code, complex branching |
| `internal/tuner/stale_stats.go` | 85% | New, complex |
| `internal/tuner/hint_parse.go` | 90% | Pure regex logic, must be exhaustive |
| `internal/tuner/scheduler.go` | 70% | Goroutine plumbing, hard to cover entirely |
| `internal/executor/analyze.go` | 80% | Integration-heavy, 80% acceptable |
| `internal/executor/` overall | no regression from v0.8.4 | — |
| `internal/analyzer/rules_work_mem.go` | 85% | New, SQL + emit logic |
| `internal/config/` | 75% | Includes meta_drift_test coverage |
| `internal/config/meta_drift_test.go` | 100% (test file itself must exercise every branch via subtests) | Bulletproof |
| `cmd/gen_config_meta/` | 80% | — |
| `sidecar/web/src/components/ConfigTooltip.jsx` | 100% (statement coverage via Vitest) | Small file |

**Coverage gaps reporting is mandatory** per CLAUDE.md §5 "Coverage Gaps Are Mandatory Output" — every test session reports per-package coverage even when all floors are met.

### 11.11 Known failure patterns (regression coverage)

From CLAUDE.md §5. Every listed pattern has an explicit test in v0.8.5:

| Pattern | Test |
|---|---|
| **Default value masking** | `internal/config/defaults_test.go::TestAllV085FieldsHaveDefaults` — load empty YAML, assert every new field resolves to its documented default (not zero) |
| **Markdown-wrapped LLM responses** | `internal/tuner/llm_prescriber_test.go::TestStripJSONHandlesFencedResponse` (existing; re-run) |
| **Transaction scope errors** | `internal/executor/analyze_integration_test.go::TestDedicatedConnectionNotInTransaction` (CHECK-S12) |
| **Fleet mode leaks** | `sidecar/e2e/revalidation_fleet_e2e_test.go::TestFleetModeIsolation` (CHECK-R19) — asserts every emitted finding's `database_name` is one of the expected DB names, never `"all"` or `""` |
| **Confidence score boundaries** | Substituted with revalidation ratio boundary: `TestRatioExactly_08`, `TestRatioExactly_12` — deterministic outcome, no flip-flop |

### 11.12 Test audit (post-phase-2)

After every test session, answer per CLAUDE.md §5 Post-Test Audit:

1. What input would break this that I haven't tested?
2. What behavior is NOT covered by any assertion?
3. Are there assertions that would pass even if the feature was completely broken?
4. Are there mocks that hide real failure modes?

Specifically for v0.8.5 the audit must look for:

- Tests that only assert `err == nil` without asserting the resulting `sage.query_hints.status` value.
- Tests that mock `pg_stat_statements` in-process rather than exercising a real extension.
- Tests that create `hint_plan.hints` rows without verifying the hints are actually applied at query time (verified by running a hinted query and EXPLAIN'ing the result).
- Tests that pass because catalog cache is empty and the code "early exits" — indistinguishable from passing because it worked.

### 11.13 Verification checklist — release gate

Total CHECK-* items across all five features after tightening:

| Feature | CHECK range | Count |
|---|---|---|
| F1 Revalidation | R01–R19 | 19 |
| F2 Stale-stats | S01–S17 | 17 |
| F3 work_mem | W01–W09 | 9 |
| F4 Ext drift | E01–E07 | 7 |
| F5 Tooltips | T01–T16 | 16 |
| **Total** | | **68** |

At release time, run through every CHECK-* and report PASS/FAIL in the format from CLAUDE.md §5. Manual checks tagged `MANUAL:` are allowed but must be justified; every `MANUAL:` must name a reason for why it cannot be automated. A release with any FAIL blocks the tag.

Aggregated report format (condensed example):

```
## v0.8.5 Release Verification — 2026-??-??

**Total:** 68 checks (F1:19, F2:17, F3:9, F4:7, F5:16)

### PASS (XX)
- CHECK-R01: `TestRevalidationLoopDisabledByConfig`
- CHECK-R02: `TestObservationModeNoWrites`
...

### FAIL (must be zero to ship)
- (none)

### MANUAL (must be justified)
- CHECK-T09: walked 40 tier-1 fields in Chrome Canary (reason: Playwright visual-regression not wired for v0.8.5)
```

The release is blocked if any FAIL remains or if any MANUAL check lacks a justification.

---

## 12. Release notes outline

```
# pg_sage v0.8.5 — "Every recommendation has a review cycle"

## Headline changes

### Hint revalidation is now automatic
Previously, hints written by the per-query tuner persisted in `hint_plan.hints`
indefinitely. v0.8.5 adds a daily revalidation loop that checks every active
hint and removes those that have become redundant, broken, or actively harmful.
This closes a long-standing source of configuration drift.

**Upgrade note:** If your config has `verify_after_apply: true` (the default),
the new revalidation loop begins running on first startup. In observation or
advisory trust levels it only logs findings; in autonomous mode it deletes
hints directly. If you wish to defer enabling it, set `verify_after_apply: false`
in your config before upgrading.

### Stale statistics now trigger ANALYZE instead of hint pinning
Previously, the tuner prescribed HashJoin hints for queries with row-estimate
skew, even when the underlying cause was stale statistics. v0.8.5 detects the
stale-stats case and issues a scoped `ANALYZE schema.table (col1, col2)` action
instead, fixing the underlying problem rather than pinning a workaround.

ANALYZE is the first non-hint executor action in the tuner pipeline and is
gated by trust level, table size, cooldown, and maintenance window settings.

### work_mem promotion advisor
When the tuner has prescribed the same `Set(work_mem)` hint to five or more
queries for the same role, the analyzer now emits an advisory finding
suggesting `ALTER ROLE ... SET work_mem = ...` to promote the setting to the
role level.

### Extension drift detection
The sidecar now checks that `pg_stat_statements` and `pg_hint_plan` remain
available at the start of each cycle. If either is dropped or removed from
`shared_preload_libraries`, a critical finding is emitted and the affected
cycle is throttled instead of silently failing.

### Settings tooltips
Tier 1 and 2 settings in the web UI now have inline tooltip documentation
describing each field's effect, danger level, and mode-specific behavior.
Tooltip content is generated from Go struct tags, guaranteed consistent
with the config schema by a build-time drift test.

## Configuration additions

[list of new fields]

## Schema changes

None. New usage of existing columns in sage.query_hints.

## Breaking changes

None, but see the "verify_after_apply" upgrade note above.
```

Full release notes authored during week 4.

---

## 13. Risks and open questions

### 13.1 Technical risks

**R-1: Revalidation cost comparison uses estimates, not actuals.**
Mitigation: interlock with Feature 2 so stale stats are refreshed before revalidation runs. Document clearly that revalidation is a heuristic and that rare cases may retire a hint that actuals would have kept (or vice versa). Severity: medium.

**R-2: ANALYZE action on large tables during peak hours.**
Mitigation: table size cap + maintenance window gating + cooldown. Default size cap deliberately conservative (10GB). Severity: medium-high if mitigations fail.

**R-3: Bulk `doc` tag edit in `config.go` is a merge-conflict hazard.**
Mitigation: land generator + drift test first; bulk edit as its own PR with minimal other changes; avoid landing during a long-running feature branch. Severity: low-medium.

**R-4: Radix UI is a new frontend dependency.**
Mitigation: already battle-tested in thousands of projects, active maintenance, MIT license. Bundle size impact ~15KB gzipped. Severity: low.

**R-5: `pg_stat_statements.last_call` is not a real column. [RESOLVED]**
The dead-queryid check in Feature 1 uses `calls` delta between revalidation cycles, not a timestamp, because `pg_stat_statements` does not expose per-query last-call time. **Resolution:** add two columns to `sage.query_hints`: `calls_at_last_check bigint` and `last_revalidated_at timestamptz`. Bootstrap state (NULL) is handled in Check 2 step 6. Reset detection (`calls < calls_at_last_check`) is handled in Check 2 step 5. See §3.7 for schema and §3.3 Check 2 for the full algorithm.

**R-6: The revalidation cost comparison EXPLAIN may itself be expensive for complex queries.**
Mitigation: cap `EXPLAIN` time via `statement_timeout` on the revalidation connection (default 10s). Queries that can't be EXPLAIN'd in 10s get their revalidation skipped for this cycle with a logged reason.

### 13.2 Open questions

**Q-1:** Should `ActionAnalyze` record the pre-ANALYZE `reltuples` and post-ANALYZE `reltuples` in `sage.action_log.detail`? Would support a future "verify ANALYZE changed the stats" check. **Recommendation: yes, add both to the detail JSONB.**

**Q-2:** Should Feature 1's "retired" hints be tombstoned (kept with `status='retired'`) or hard-deleted? Retention would grow `sage.query_hints` over time. **Recommendation: tombstone with `status='retired'`, rely on the existing retention loop (`internal/retention/cleanup.go`) to garbage-collect after `retention.query_hints_days` (new retention config).**

**Q-3:** Does the tooltip infrastructure need i18n consideration? **Recommendation: no. Defer to when/if pg_sage gets a second locale. `doc` tags are English-only for v0.8.5.**

**Q-4:** Should Feature 3 (`work_mem` promotion) also consider `maintenance_work_mem`? Same pattern applies. **Recommendation: out of scope for v0.8.5. Feature 3 is `work_mem`-only; a future release can generalize.**

**Q-5:** Does the config tooltip Playwright test (CHECK-T06) require a signed-in user session? **Recommendation: yes; reuse the existing walkthrough Playwright fixture from the v0.8.4 e2e spec.**

### 13.3 Items explicitly out of scope for v0.8.5

Documented so they don't accidentally creep in:

- Parameter-sensitive query detection. Deferred to v0.9 agent mode — detection is hard without multi-plan history and the corrective actions are unreliable.
- `CREATE STATISTICS` advisor for correlated predicates. Natural follow-up to stale-stats detection; defer until v0.9 so the revalidation loop from v0.8.5 can measure whether extended stats would have helped.
- Index bloat / `REINDEX CONCURRENTLY` advisor. Adding another executor action kind while also adding ANALYZE is too many action surfaces in one release.
- `ALTER ROLE` as an executor action. Feature 3 is advisory-only in v0.8.5; autonomous role-level writes need a much more careful trust model.
- LLM-driven revalidation reasoning. Feature 1's cost comparison is deterministic; LLM involvement is a v0.9 concern.
- Tooltip tier 3-4 fields (collector, retention, notifications). Incremental post-v0.8.5.
- PG18 support. Still blocked on upstream changes unrelated to this release.

---

## 14. Definition of done

v0.8.5 ships when:

1. All 46 CHECK-* items report PASS (or explicit `MANUAL: reason` per CLAUDE.md).
2. `go test -cover -count=1 ./...` passes with no skipped tests outside the explicit skip list, and per-package coverage meets §11.3 targets.
3. CHANGELOG.md has a v0.8.5 entry with release notes from §12.
4. `config.example.yaml` is regenerated and documents every new field.
5. `sidecar/web/src/generated/config_meta.json` is committed and matches `config.go` tags (drift test green).
6. Playwright walkthrough spec passes, including new tooltip tests.
7. Manual verification of one full end-to-end scenario per feature on a real Postgres instance (not just unit tests):
   - Revalidation retires a superseded hint
   - ANALYZE fires and fixes a real stale-stats skew
   - `work_mem` promotion finding renders in dashboard
   - Extension drift is caught after dropping `pg_stat_statements`
   - Tooltip renders on tier 1 field with warning styling
8. README.md version bumped to v0.8.5; git tag created; release binary built and sanity-checked.
9. Post-release: a single PR follow-up to `roadmap.md` adding a v0.9 section that explicitly carries the deferred items from §13.3 so they do not get lost.

---

## 15. Version bump rationale

v0.8.4 → v0.8.5 (patch bump) is correct per semver interpretation in pg_sage:

- No breaking API changes
- No breaking config changes (new fields are additive with defaults)
- No schema migrations that require manual intervention
- `verify_after_apply: true` default activating new behavior is noted in release notes as an upgrade consideration, not a breaking change — the field was always set to true, just inert. Users who opt out before upgrading preserve v0.8.4 behavior exactly.

If during implementation any feature turns out to require a breaking change, escalate to v0.9.0 — do not paper over it with patch semantics.
