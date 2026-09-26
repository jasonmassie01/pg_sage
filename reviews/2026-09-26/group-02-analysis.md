# Group 02 — Tier-1 analysis, findings & cases

Reviewer: G2 agent, 2026-09-26, worktree `claude/full-review-ai-sre-2026-09-26` @ `b396595`.
Source was treated read-only. Behavioural claims marked **CONFIRMED (exec)** were proven with
throw-away tests injected through `go test -overlay` (the repo tree was not modified; see §E).

## Scope

| Package | Non-test LOC | Notes |
|---|---:|---|
| `internal/analyzer` | 4,655 | rules engine, dedup, persistence (`finding.go`), index parser |
| `internal/forecaster` | 1,097 | daily-agg forecasts + v0.9 storage growth |
| `internal/schema/lint` | 2,541 | 16 registered + 4 unregistered rules, runner, LLM JSONB enhancer |
| `internal/cases` | 2,311 | stateless case projection over findings/incidents/hints |
| `internal/schemaguard` | 333 | invariant classifier/planner/custodian (detector lives in `autonomy`) |
| `internal/value` | 450 | DBA-hours-saved accounting |
| `internal/ledger` | 239 | decision ledger + self-audit |
| Wiring read | — | `cmd/pg_sage_sidecar/main.go` (standalone 580-900, fleet 1300-1620), `wire.go`, `rca_adapter.go`, `value_metrics.go`; consumers in `api/cases_handlers.go`, `api/handlers.go` (suppress), `api/router.go` (value), `autonomy/schema_postgres.go`, `autonomy/retention_postgres.go`, `executor/executor.go` (RunCycle), `collector/queries.go` |

Total reviewed: ~11.6k non-test LOC in group + ~2k LOC of wiring/consumers.

---

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G2-B01 | P1 | CONFIRMED (read) | `analyzer/finding.go:49-53`, `api/handlers.go:1712-1720` | Suppression is defeated: next cycle inserts a fresh `open` row, executor acts on it |
| G2-B02 | P1 | CONFIRMED (read) | `analyzer/analyzer.go:450-464` | Findings never auto-resolve when their category drops to zero findings |
| G2-B03 | P1 | CONFIRMED (exec) | `analyzer/rules_vacuum.go:147`, `selfmonitor/selfmonitor.go` `collectorCatalogRef` | `xid_wraparound` finding is classified as self-monitoring and never persisted |
| G2-B04 | P1 | CONFIRMED (exec) | `analyzer/rules_system.go:56-65` vs `collector/queries.go:145-150` | Cache-hit rule compares a percent (0-100) to a fraction (0.95): never fires; fires "critical 0.00%" on no-data |
| G2-B05 | P1 | CONFIRMED (exec) | `analyzer/dedup.go:246-285`, `rules_autovacuum_tuning.go:33-43` | GUC-conflict dedup is keyed on GUC only → only 1 table ever gets an autovacuum_tuning finding; rule ignores existing reloptions so the same table wins forever |
| G2-B06 | P1 | CONFIRMED (read) | `analyzer/analyzer.go:477-495` | Every critical finding re-dispatched to Slack/PagerDuty every analyzer cycle (no "new" check, no cooldown) |
| G2-B07 | P1 | CONFIRMED (exec) | `analyzer/rules_index.go:65-101` | Unused-index `FirstSeen` never cleared once the index is used → after `pg_stat_reset`/crash the DROP is recommended immediately |
| G2-B08 | P1 | CONFIRMED (exec) | `analyzer/rules_index.go:413-440`, `collector/queries.go:119-132` | Missing-FK-index rule: schema guessed by table name, composite FKs split per column, INCLUDE indexes not recognized → wrong/redundant `CREATE INDEX` and false negatives |
| G2-B09 | P1 | CONFIRMED (read) / exec path PLAUSIBLE | `analyzer/rules_index.go:142-187` | Invalid-index rule flags in-progress `CREATE INDEX CONCURRENTLY` / `REINDEX … _ccnew` and recommends `DROP INDEX CONCURRENTLY` |
| G2-B10 | P1 | CONFIRMED (read) | `schema/lint/rule_overlapping_index.go:22-43,83` | Overlapping-index lint recommends dropping UNIQUE/PK prefix indexes (no `indisunique`/constraint filter) |
| G2-B11 | P1 | CONFIRMED (read) | `value/postgres.go:131-151`, `executor/executor.go:1243`, `api/router.go:183` | Value report: `action_log.database_id` never written → NULL scan error (500) once any credit exists; `?database=` always 0; fleet/meta reads the wrong pool |
| G2-B12 | P1 | CONFIRMED (read) | `schemaguard/planner.go:78-95`, `autonomy/retention_postgres.go:84-98`, `autonomy/schema_postgres.go:319-333` | Retention "dry-run gate" is a one-cycle delay, not bound to column/window; retention column chosen by name heuristic (falls back to *any* date column) |
| G2-B13 | P2 | CONFIRMED (exec) | `analyzer/index_parser.go:19-24` | `ParseIndexDef` greedy regex swallows `INCLUDE`/`WHERE` (pg_get_indexdef always parenthesises WHERE) → full index reported as "subset" of a partial index; INCLUDE guard dead |
| G2-B14 | P2 | CONFIRMED (read) | `schema/lint/linter.go:67-70`, `schema/lint/runner.go:185-240` | A transiently failing lint rule resolves all its open findings; they re-open next scan as new rows |
| G2-B15 | P2 | CONFIRMED (read) | `cmd/pg_sage_sidecar/main.go:743` vs `:777-782`; fleet `:1510` vs `:1562-1567` | Data race: `WithDispatcher/WithDatabaseName/WithPlanNarrator` called after `anal.Run` goroutine started |
| G2-B16 | P2 | CONFIRMED (read), cost PLAUSIBLE | `analyzer/analyzer.go:609-666`, `forecaster/datasource.go:85-97` | Each analyzer cycle pulls ~7 days of full `queries` snapshots (≈10k jsonb rows) into Go to keep 100; forecaster explodes 30 days of query jsonb in SQL; no statement_timeout |
| G2-B17 | P2 | CONFIRMED (read) | `analyzer/rules_vacuum.go:110-130`, `collector/queries.go:20,35` | Per-query `blk_read/write_time` hardcoded 0 → `ioSaturated` always false; table_bloat I/O downgrade and case `BlockedReason` dead |
| G2-B18 | P2 | CONFIRMED (read) | `forecaster/rules.go:62-75` | Connection-saturation forecast uses *active* backends, not total, against `max_connections` |
| G2-B19 | P2 | CONFIRMED (exec) | `analyzer/optimizer_mapping.go:18-21`, `dedup.go:181-207` | Two optimizer recs for the same table+category collapse to one (ident = table) |
| G2-B20 | P2 | CONFIRMED (read) | `schema/lint/llm_jsonb.go:40-47` | JSONB LLM enhancer reads `pg_stat_statements` without `dbid` filter → other databases' queries sent to LLM and attributed here |
| G2-B21 | P2 | CONFIRMED (read) | `ledger/postgres.go:205-231` | Self-audit flags every failed/manual/in-flight action forever; reports the full list every tick |
| G2-B22 | P2 | CONFIRMED (read) | `analyzer/rules_index.go:103-106,178-181,254-256,432-435` | DDL built with unquoted identifiers; mixed-case names fail or case-fold onto a *different* index |
| G2-B23 | P3 | CONFIRMED (exec) | `analyzer/rules_query.go:18-28`, `config/config.go:648` | `slow_query_threshold_ms: 0` is valid → every query "critical, +Inf× threshold" |
| G2-B24 | P3 | CONFIRMED (read) | `cases/projector.go:75`, `cases/case.go:154-158` | Case `expires_at`/`observed_at` recomputed as `now()` on every request → expiry never happens, observed time meaningless |
| G2-B25 | P3 | CONFIRMED (read) | `value/postgres.go:228-242` | `trend_daily`/`by_database` built from map iteration → random order in UI table |
| G2-B26 | P3 | CONFIRMED (read) | `cmd/pg_sage_sidecar/main.go:1579` | Fleet lint runner gets `cfg.PGVersionNum` instead of per-DB `dbPGVersion` |
| G2-B27 | P3 | CONFIRMED (read) | `schema/lint/rule.go:28-48` | `exclude_schemas` entries with uppercase/hyphen silently ignored |
| G2-B28 | P3 | CONFIRMED (read) | `analyzer/rules_workmem_promotion.go:306-318` | work_mem promotion joins `pg_stat_statements` without `dbid`/`toplevel` → hint counts inflated across DBs |

### G2-B01 — Suppression is defeated every cycle (P1)
- **Failure scenario:** Operator suppresses finding #42 `unused_index public.idx_monthly_report`
  (`POST /findings/42/suppress` → `status='suppressed'`). Next analyzer cycle, `UpsertFindings`
  looks up `status = 'open'` (`finding.go:49-53`), finds none, `recentlyResolvedByAction` is false,
  and INSERTs a new open row #43. The UI shows it again; the executor's `lookupFindingID`
  (`executor.go:1037`) finds #43 with `acted_on_at IS NULL` and proceeds (auto mode + window →
  `DROP INDEX CONCURRENTLY`). `suppressed_until` is documented (`docs/sql-reference.md:42`) but
  the handler never sets it and nothing reads it for non-tuner rows. The unsuppress handler even
  works around the duplicate (`handlers.go:1722-1735` deletes the suppressed row on conflict).
- **Root cause:** dedup key is `(category, object_identifier) WHERE status='open'`; suppressed
  rows are invisible to it.
- **Fix:** in `UpsertFindings`, before insert, `SELECT 1 … WHERE category=$1 AND
  object_identifier=$2 AND status='suppressed' AND (suppressed_until IS NULL OR
  suppressed_until > now())` → skip. Suppress handler sets `suppressed_until` (default 30d,
  request param). Executor: filter in-memory findings against suppressed keys (one query per
  cycle). Escalate to P0 if operators use suppress as their "don't do this" control in auto mode.
- **Test:** integration: upsert → suppress → upsert again → assert 0 open rows and executor
  `RunCycle` issues no action; unit: suppression expiry re-opens.

### G2-B02 — Last finding of a category never auto-resolves (P1)
- **Scenario:** one `table_bloat` finding on `public.orders`; executor or DBA runs VACUUM; next
  cycle the rule emits nothing for `table_bloat`, so `activeByCategory` (`analyzer.go:451-457`)
  has no `table_bloat` key and `ResolveCleared` is never called for it. The finding (and its
  case) stays open forever. Same for any category whose last object clears
  (`xid_wraparound`, `replication_lag`, `connection_leak`, `forecast_*`, `sequence_exhaustion`…).
- **Root cause:** resolve loop iterates categories *present* this cycle only. The lint runner does
  this correctly by also querying open categories from the DB (`runner.go:205-225`).
- **Fix:** maintain an explicit set of analyzer-owned categories (static rule categories plus
  categories returned by each sub-producer that *succeeded* this cycle); call `ResolveCleared`
  with an empty set for owned categories absent from the output. Do **not** resolve categories of
  a producer that errored (advisor/forecaster/tuner) — track per-producer success.
- **Test:** unit with a fake store: cycle 1 emits `table_bloat:X`, cycle 2 emits nothing →
  assert X resolved; cycle where forecaster errors → assert `forecast_*` untouched.

### G2-B03 — XID wraparound finding is never persisted (P1)
- **Scenario (exec):** `ruleXIDWraparound(1.2e9)` → critical finding whose `RecommendedSQL` is a
  diagnostic `SELECT … FROM pg_stat_activity …` (`rules_vacuum.go:144-149`).
  `selfmonitor.IsQueryText` matches `collectorCatalogRef` (`from pg_stat_activity`) →
  `isSelfMonitoringFinding=true` → `UpsertFindings` skips it. The most catastrophic Postgres
  failure mode never appears in Findings, Cases (the `xid_wraparound` → freeze-diagnostic case
  mapping in `cases/vacuum_autopilot.go:14` is unreachable), or MCP. Only the in-memory
  dispatcher sees it.
- **Root cause:** self-monitoring heuristic applied to *recommended* SQL; any diagnostic query
  pg_sage suggests that reads a catalog view is treated as pg_sage's own workload.
- **Fix:** apply `IsQueryText` only to fields that carry *observed workload* (`detail.query`,
  `query_text`, …), not to `RecommendedSQL/RollbackSQL`; keep the `sage.` object check. Move the
  xid diagnostic query to `detail.diagnostic_sql` and leave `RecommendedSQL` empty (it is not an
  action).
- **Test:** `TestXIDFindingPersisted` asserting `isSelfMonitoringFinding(ruleXIDWraparound(...)[0])
  == false`; table test over all rules with a crafted snapshot asserting none self-classify.

### G2-B04 — Cache-hit-ratio unit mismatch (P1)
- **Scenario (exec):** collector stores `round(hit/(hit+read)*100,2)` (`queries.go:145-150`) →
  e.g. `60.0`. Rule: `60.0 >= 0.95` → no finding at a 60% hit ratio. When there is no data the
  `COALESCE(...,0)` yields `0` → "critical, Cache hit ratio 0.00%". Same fraction assumption in
  `rca/signals.go:110-118` (cross-group). The forecaster (×100) is the only correct consumer.
- **Fix:** collector emits a fraction (0-1) and `NULL`→`-1` for no data (the rule already treats
  `<0` as no data); or divide by 100 in the rule. Pick one unit and assert it in a collector test.
- **Test:** rule test with `CacheHitRatio: 0.60` and a collector-contract test asserting
  `0 <= ratio <= 1`.

### G2-B05 — autovacuum_tuning: only one table ever, and it re-fires (P1)
- **Scenario (exec):** 3 qualifying tables → `ruleAutovacuumTuning` emits 3 findings with
  `ALTER TABLE … SET (autovacuum_vacuum_scale_factor = …)`; `resolveGUCConflicts` groups by GUC
  name across *all objects* (`dedup.go:250-257`) → 1 survives (first in table order). The rule
  never reads `snap.ConfigData.TableReloptions`, so after the executor applies it, the same
  table re-qualifies; after the 2-min grace a new open row is inserted and re-applied until the
  oscillation limit trips, then it stays open forever — and keeps winning dedup, so no other
  table is ever tuned.
- **Fix:** key GUC conflicts by `(guc, ObjectIdentifier)` for `perTable` entries (global entries
  still conflict globally); skip tables whose reloptions already contain a scale factor ≤
  target.
- **Test:** dedup test with 3 per-table findings → 3 survive; rule test with reloptions present →
  no finding.

### G2-B06 — Alert storm: critical findings re-paged every cycle (P1)
- **Scenario:** `dispatchCriticalFindings` (`analyzer.go:477-495`) sends every `critical`
  finding each cycle; `notify.Dispatcher.Dispatch` has no dedup/cooldown
  (`notify/dispatcher.go:42-89`). Exact duplicate indexes are `critical`
  (`rules_index.go:240`) → 5 duplicate indexes = 5 PagerDuty events every 10 min (720/day).
  The comment says "new critical findings". Suppressed findings are also paged (B01).
- **Fix:** have `UpsertFindings` return which findings were *inserted* (or severity-escalated)
  and dispatch only those; downgrade `duplicate_index` to `warning`.
- **Test:** analyzer unit with fake dispatcher: two cycles with identical findings → 1 dispatch.

### G2-B07 — Unused index dropped right after a stats reset (P1)
- **Scenario (exec):** index seen at 0 scans at startup (`FirstSeen=T0`); used for weeks
  (`IdxScan>0` path `continue`s without clearing `FirstSeen`, `rules_index.go:81`); DBA runs
  `pg_stat_reset()` or the server crash-recovers → `idx_scan=0`, `now-T0 ≥ 7d` → finding with
  `DROP INDEX CONCURRENTLY` in the same cycle. Test output: `FirstSeen retained after use=true;
  findings right after reset=1`. Replica promotion has the same effect (counters reflect the
  standby's own reads). The unregistered lint rule already guarded with
  `pg_stat_database.stats_reset < now()-7d` (`lint/rule_unused_index.go:29-33`).
- **Fix:** `delete(extras.FirstSeen, ident)` when `IdxScan>0`; also require
  `now - max(stats_reset, pg_postmaster_start_time) ≥ window` (collect both).
- **Test:** the overlay test above as a permanent unit test + one with `stats_reset` inside window.

### G2-B08 — Missing-FK-index rule creates wrong/redundant indexes (P1)
- **Scenarios (exec):**
  1. *INCLUDE:* FK `orders.customer_id`, existing `btree (customer_id) INCLUDE (total)` →
     finding `CREATE INDEX CONCURRENTLY ON public.orders (customer_id);` (redundant index).
  2. *Composite FK* `(order_id, tenant_id)` with index `(order_id, tenant_id)` → collector
     returns one row per column; rule requires `tenant_id` as a leading column → recommends
     `CREATE INDEX … (tenant_id)`.
  3. *Schema-per-tenant:* `tenant_a.orders` and `tenant_b.orders`; collector's FK query has no
     schema (`queries.go:119-132`); rule picks the first table named `orders`
     (`rules_index.go:419-425`) → `tenant_b`'s missing index is a false negative (0 findings),
     and all tenants' FKs collapse onto one ident. `buildFKRequirements` (`:487`) has the same
     bug, so FK-support protection for unused-index drops is attributed to the wrong schema.
  `ActionRisk:"safe"`; the executor contract is `create_index_concurrently` (moderate).
  A second, catalog-correct detector exists in `autonomy/schema_postgres.go:381-395`
  (schemaguard) → two owners of the same invariant.
- **Fix:** collect `nspname`, `conkey` (ordered array) and index `indkey`/`indnkeyatts` in the
  collector; compare attnums, not parsed text; or delete the analyzer rule and let schemaguard
  own FK indexes (preferred — its SQL is catalog based; fix its `<@` containment to a
  leading-prefix check).
- **Test:** the three overlay scenarios as unit tests.

### G2-B09 — In-progress concurrent index builds flagged as invalid (P1)
- **Scenario:** app migration (or pg_sage's own executor) runs `CREATE INDEX CONCURRENTLY` on a
  large table; `indisvalid=false` for the whole build. Analyzer cycle → `invalid_index` finding
  "Drop the invalid index" with `DROP INDEX CONCURRENTLY` (`rules_index.go:170-184`). If approved
  (or auto-executed at autonomous/tier3_moderate in window), the DROP waits for the build's
  lock and then drops the freshly built index. `REINDEX CONCURRENTLY` `_ccnew` indexes likewise.
- **Fix:** exclude indexes whose table has a row in `pg_stat_progress_create_index`, or whose
  `indexrelid` is locked by another backend; require invalid across 2 cycles ≥ N minutes apart.
- **Test:** unit with snapshot flag `BuildInProgress`; integration with a slow CIC.

### G2-B10 — Lint recommends dropping unique/PK indexes (P1)
- **Scenario:** PK `orders_pkey (id)` and index `(id, created_at)` → overlapping-index SQL has no
  `NOT a.indisunique / NOT a.indisprimary` / `pg_constraint` filter → finding
  `DROP INDEX CONCURRENTLY public.orders_pkey;`. For a plain `CREATE UNIQUE INDEX` (not
  constraint-backed) the drop succeeds and silently removes uniqueness enforcement if a user
  approves the case (`actionTypeForSQL` → `drop_unused_index`, "queue_for_approval"). Also
  ignores `indoption` (DESC) and collation, and duplicates the analyzer's subset rule, which has
  the constraint check and size heuristics this rule lacks.
- **Fix:** add `AND NOT a.indisunique AND NOT a.indisprimary AND a.indoption = …prefix`; or delete
  this rule and keep the analyzer subset rule (after B13 is fixed).
- **Test:** lint integration test with a PK prefix → 0 findings.

### G2-B11 — Value accounting broken for database attribution and fleet (P1)
- **Scenario:** `ddl_agent_value.go:41-44` adds `action_log.database_id`; neither executor insert
  sets it (`executor.go:1243-1250`, `manual.go:325-330`). `readRealized` scans
  `d.name` (NULL via LEFT JOIN) into `string` (`value/postgres.go:148-151`) → pgx "cannot scan
  NULL" → `GET /api/v1/value` 500s as soon as one verified credit is stamped. With
  `?database=x` the `d.name=$1` predicate never matches → 0. In meta-db mode the handler uses the
  meta pool (`router.go:183`), while credits are stamped in each monitored DB's `action_log`
  (`executor/rollback.go:379`) → always 0; in fleet mode it reads only the primary DB. The
  Prometheus path (`value_metrics.go:18`) uses `COALESCE(d.name,'')` so metrics and API
  disagree. Integration tests insert `database_id` explicitly (`value/postgres_test.go:252`),
  hiding the real failure.
- **Fix:** `COALESCE(d.name, $db_label)`; write `database_id` at action insert (or drop the
  column and label by pool); aggregate the value report across `poolsForDatabaseSelection`.
- **Test:** integration inserting an action the way the executor does (no database_id) → report
  succeeds; fleet API test with 2 pools.

### G2-B12 — Retention dry-run gate is not a gate (P1)
- **Scenario:** user declares `sage.table_contract(append_only, retention_interval='90 days')`
  and grants `ChangeRetention`. Cycle 1: dry run recorded. Cycle 2: `planRetention`
  (`planner.go:88-95`) sees `SuccessfulRetentionDryRuns>0` → apply → deletes batches. Nobody
  reviews the dry-run count. If the user later shortens the window to `1 day`, or the table gains
  a `created_at` column (heuristic order `created_at, occurred_at, updated_at, else first
  date/timestamp column` — `schema_postgres.go:319-333`), deletion starts immediately with the
  new semantics because `requireDurableDryRun` only checks "any dry_run ever for this
  schema.table" (`retention_postgres.go:88-90`). A table without the preferred names uses e.g.
  `birth_date`.
- **Fix:** bind the dry run to `(retention_column, retention_interval, contract.updated_at)`;
  require explicit retention column in the contract (no heuristic); require an operator ack of
  the dry-run candidate count (or a max-delete-fraction guard) before first apply.
- **Test:** planner unit: contract change after dry run → `DispositionDryRun` again; integration:
  column heuristic removed → contract without column parks.

### G2-B13 — Index-definition parser is greedy (P2)
- **Exec:** `(a) INCLUDE (b)` → `Columns=["a) INCLUDE (b"]`, `IncludeCols=[]`;
  `(a, b) WHERE (deleted_at IS NULL)` → `Columns=["a","b) WHERE (deleted_at IS NULL"]`,
  `WhereClause=""`. Existing tests document the bug instead of failing
  (`coverage_boost_test.go:374-400`). Consequences: full `(a)` reported as subset of partial
  `(a,b) WHERE …` (verified), `IsSubset` INCLUDE guard never runs, B08 INCLUDE false positive.
- **Fix:** replace regex with a paren-depth scanner after `USING <am> (`; better, collect
  `indkey/indnkeyatts/indpred/indclass/indoption` and compare catalog data.
- **Test:** turn the two "documents actual behavior" tests into real assertions.

### G2-B14 — Lint rule error resolves its findings (P2)
- `Linter.Scan` logs and `continue`s on rule error (`linter.go:67-70`); `resolveCleared` then adds
  that rule's open rule_id with an empty active set (`runner.go:217-222`) → all resolved; next
  successful scan inserts new rows (history, case IDs, action links lost). Fix: return the set of
  failed rule IDs from `Scan` and skip them in `resolveCleared`. Test: fake rule that errors.

### G2-B15 — Analyzer configured after its goroutine starts (P2)
- Standalone: `go anal.Run` (`main.go:743`) precedes `WithDispatcher`/`WithDatabaseName`/
  `WithPlanNarrator` (`:777-782`); fleet: `startInstanceWorker(dbAnal.Run)` (`:1510`) precedes
  `:1562-1567`. The first cycle runs immediately → unsynchronized reads (race detector would
  flag), first-cycle events carry empty DB name and no narration. Fix: move the `With*` calls
  above `Run`. Test: `-race` test that constructs via the wiring helper.

### G2-B16 — Analyzer/forecaster pull huge history every cycle (P2)
- `buildHistoricalAverages` selects every `queries` snapshot `data` for `RegressionLookbackDays`
  (7d × 1440/day at 60s collector ≈ 10k rows, each up to 500 query entries incl. text) and
  downsamples to 100 in Go. `queryAggsSQL` runs `jsonb_array_elements` over 30 days of the same
  snapshots in the monitored DB every analyzer cycle. Neither has a `statement_timeout`
  (only the collector sets one). Fix: sample in SQL (`WHERE collected_at` bucketed hourly,
  `DISTINCT ON (date_trunc('hour', …))`) or maintain a daily rollup table. Test: benchmark /
  integration asserting row count fetched ≤ 100.

### G2-B17 — I/O-saturation downgrade is dead (P2)
- Collector hardcodes `0::float8 AS blk_read_time, blk_write_time` for pg_stat_statements
  (`queries.go:20,35`, PG17 column rename); `ioWaitRatio` sums those → always 0 → table_bloat
  never downgraded, `detail.io_saturated=false`, cases never set the "IO is saturated"
  BlockedReason. A second, differently-defined metric (`computeIOUtilPct`, DB-level blk time vs
  summed query exec time — different reset epochs) only affects categories containing "vacuum",
  i.e. not `table_bloat`. Fix: version-aware `shared_blk_read_time` (PG17) / `blk_read_time`,
  and one I/O-pressure function using deltas between snapshots.

### G2-B18 — Connection forecast uses active backends (P2)
- `forecastConnectionSaturation` regresses `MaxActiveBackends` against `max_connections`
  (`rules.go:62-75`); idle and idle-in-tx connections consume slots too. A pooler-less app with
  900 idle / 20 active of 1000 is reported healthy. Fix: use `MaxTotalBackends` (already
  aggregated). Test: aggs with high total/low active → finding.

### G2-B19 — Multiple index recommendations per table collapse (P2)
- Optimizer finding ident is `rec.Table` (`optimizer_mapping.go:21`); two recs of category
  `missing_index` on `public.orders` → `dedupSameCategory` keeps one (exec: `two recs -> 1`), and
  the DB unique key would collide anyway. Fix: ident `table + "|" + index columns` (or DDL hash).

### G2-B20 — Cross-database query text in LLM prompt (P2)
- `slowQuerySQL` has no `dbid = (SELECT oid FROM pg_database WHERE datname=current_database())`
  (`llm_jsonb.go:40-47`) → in multi-DB clusters (fleet), DB B's queries are sent to the LLM as
  DB A's evidence. Also table names are interpolated into regex unescaped (`:137-140`). Fix: add
  dbid filter; `regexp.QuoteMeta`-equivalent escaping.

### G2-B21 — Ledger self-audit is permanently red (P2)
- `FindAuditViolations` flags any `action_log` row not `reverted/rolled_back` lacking
  `decision_id` or a verification row (`ledger/postgres.go:208-214`). Manual executions never set
  `decision_id` (`manual.go:325`); `failed` actions never get verification; `monitoring` rows are
  flagged until verification completes. `runSelfAudit` reports the full, ever-growing list as an
  `error` every tick (`autonomy/supervisor.go:174-190`) → alarm fatigue masks real violations.
  Fix: scope to executor-originated `outcome IN ('success','monitoring')` older than the
  verification window, add `LIMIT`, and report counts by kind.

### G2-B22 — Unquoted identifiers in index DDL (P2)
- `DROP INDEX CONCURRENTLY %s.%s` with raw `SchemaName/IndexRelName` (unused, invalid,
  duplicate, subset) and `CREATE INDEX … ON %s.%s (%s)` for FKs. `"Sales"."IdxFoo"` → error;
  if both `IdxFoo` (target) and `idxfoo` exist, case folding drops the other one.
  Multi-statement injection is blocked by `executor/validate.go:351-365`. Fix: use
  `sanitize.QuoteQualifiedName` like the vacuum/analyze rules already do.

### G2-B23..B28 (P3) — brief
- **B23:** `slow_query_threshold_ms: 0` passes validation → ratio `+Inf`, every query critical
  (and paged via B06). Guard `threshold<=0 → default`.
- **B24:** `NewCase` sets `ObservedAt=now` and candidates `ExpiresAt=now+24h` per request;
  `IsExecutable` has no production caller. Use finding `created_at/last_seen`.
- **B25:** sort `dayRows` by day and `databaseRows` by hours desc.
- **B26:** pass `dbPGVersion` to `lint.NewRunner` / `migration.NewAdvisor` in fleet.
- **B27:** quote with `quote_literal` semantics instead of dropping non-lowercase names; log a
  warning when an exclude entry is ignored.
- **B28:** filter `dbid` and `toplevel` in the work_mem promotion join.

---

## B. Dead / unwired / half-built

From `raw-deadcode-prod.txt` (6 items in group; lint rules = 24 method lines → 4 rules):

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G2-D01 | `analyzer/dedup.go:48` | `DedupFindings` | DELETE | Thin wrapper; production dedup is `DeduplicateFindings` at `analyzer.go:432`. |
| G2-D02 | `cases/projector.go:342` | `ResolveIfEvidenceMissing` | DELETE | Cases are stateless projections of open findings (`api/cases_handlers.go:86-133`); a case disappears when its finding leaves `open`. The *real* gap is G2-B02 (findings not resolving). |
| G2-D03 | `schema/lint/rule_unused_index.go:13-45` | lint unused index | DELETE (port guard) | Analyzer owns `unused_index`; but port this rule's `pg_stat_database.stats_reset` guard into the analyzer (fixes B07). |
| G2-D04 | `schema/lint/rule_duplicate_index.go:13-61` | lint duplicate index | DELETE (port approach) | Analyzer owns `duplicate_index`; this rule's catalog comparison (`indkey/indclass/exprs/pred`) is strictly better than the analyzer's regex parser (B13) — port it, then delete. |
| G2-D05 | `schema/lint/rule_invalid_index.go:13-43` | lint invalid index | DELETE | Exact duplicate of analyzer `invalid_index`; both need the B09 in-progress guard. |
| G2-D06 | `schema/lint/rule_bloated_table.go:13-58` | lint physical (page-estimate) bloat | WIRE | *Not* a duplicate: analyzer `table_bloat` is a dead-tuple ratio that goes to 0 after VACUUM while the table stays physically bloated. This is the only physical-bloat signal; `cases/vacuum_autopilot.go:12` already maps `schema_lint:lint_bloated_table`. Register it (or move into analyzer as `table_physical_bloat`) feeding the `bloat_remediation`/pg_repack case. |

Found beyond the deadcode list (deadcode misses methods reachable "only through reflection";
verified with `deadcode -whylive` and grep):

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G2-D07 | `forecaster/forecaster.go:89-126`, `growth.go` (all) | `ForecastGrowth`, `RecordSizeHistory`, `QuerySizeHistory`, `GrowthFindings` — no production caller | WIRE | `sage.size_history` is never written, so `GET /api/v1/forecasts` (`api/handlers_v09.go:296`) and `web/src/pages/ForecastsPage.jsx` are always empty; config keys `forecaster.min_data_points/alert_horizons/disk_capacity_bytes/min_r_squared` do nothing. `disk_capacity_bytes: 0 = auto-detect` is not implemented. REVERSE_SPEC §2.3 claims it flows into `allFindings` — false. |
| G2-D08 | `value/postgres.go:110-126` | `RecordIncident` — no caller; `sage.incident_avoided` never written | WIRE (or remove UI card) | "Incidents avoided" on the Value page and `pg_sage_incidents_avoided_total` are always 0. |
| G2-D09 | `schema/ddl_agent_value.go:42` | `action_log.database_id` never written | WIRE | Root of B11. |
| G2-D10 | `config/config.go:158` | `analyzer.index_bloat_threshold_pct` (UI/API editable, default 30) — no consumer | WIRE | REVERSE_SPEC claim still true: no index-bloat/REINDEX rule. `cases/vacuum_autopilot.go:21` (`reindex_candidate`) and executor `reindex_concurrently` contract + toil model exist with no producer. |
| G2-D11 | `cases/vacuum_autopilot.go:21-24` | `reindex_candidate`, `blocked_vacuum` case mappings — no producer category | WIRE with D10 / delete `blocked_vacuum` | Half-built vacuum autopilot. |
| G2-D12 | `autonomy/schema_postgres.go:343` | `external_reversion` evidence key read, never written | WIRE | `schemaguard` oscillation park (`planner.go:45-49`) can never trigger. |
| G2-D13 | `cases/case.go:120-125` | `ActionCandidate.IsExecutable` | TEST-ONLY → DELETE or WIRE into approval | Only tests call it; `ExpiresAt` is cosmetic (B24). |
| G2-D14 | `analyzer/analyzer.go:166-172` | `SetFindings` | TEST-ONLY OK | Used by `e2e/pipeline_helpers_test.go:154`; fix the comment ("called by rule evaluation" is false). |
| G2-D15 | `api/handlers.go:1652,1697` / `analyzer/finding.go` | `suppressed_until` for non-tuner findings | WIRE | Displayed, documented, never set or honored (B01). |
| G2-D16 | `analyzer/rules_vacuum.go:126-130` | `ioSaturated` branch | WIRE | Dead in production (B17). |
| G2-D17 | `schema/lint/linter.go:115-121` | comment lists `ruleMissingFKIndex` | DELETE comment | Type no longer exists. |
| G2-D18 | `docs/reverse_spec/02-tier1-rules.md:174-178,309-312` | `agent_workload.go` "dead" | DOC FIX | File no longer exists — claim stale. |

**Verdict counts (deadcode list, 6 items):** WIRE 1 · DELETE 5 (2 port logic first) · TEST-ONLY 0.
**Additional unwired found:** 12 (WIRE 8 · DELETE/doc 2 · TEST-ONLY 2).

---

## C. Feature improvements (ranked Impact × Effort; H/M/L)

### Finding lifecycle (`analyzer/finding.go`, `analyzer.go`)
Current: per-finding SELECT+UPDATE/INSERT, open-only dedup, resolve by present categories,
2-min action grace, no suppression/expiry, in-memory findings drive the executor.
1. **G2-I01 (H×L)** Fix B01/B02/B06 together: `UpsertFindings` returns `{inserted, escalated,
   suppressed}`; resolve owned-but-absent categories; dispatch only inserted/escalated.
2. **G2-I02 (H×M)** Single-statement upsert: `INSERT … ON CONFLICT (category,
   object_identifier) WHERE status='open' DO UPDATE` (the partial unique index supports it) —
   removes 2N round trips per cycle and the SELECT→INSERT race between analyzer and lint.
3. **G2-I03 (M×L)** Derive the reopen grace from `max(analyzer, collector interval)×2` instead of
   the fixed 2 min (at default 600s the grace never covers the next cycle, so executed
   findings whose condition persists reopen as new rows).
4. **G2-I04 (M×M)** Stable finding identity across reopen (`finding_key` column + `first_seen`
   carried forward) so history/cases/actions survive resolve→reopen.

### Index rules (`rules_index.go`, `index_parser.go`)
1. **G2-I05 (H×M)** Collect catalog facts in the collector (`indkey`, `indnkeyatts`, `indclass`,
   `indoption`, `indpred IS NOT NULL`, `conkey`, `nspname` for FKs, `stats_reset`) and delete the
   regex parser. Fixes B07, B08, B13 and makes duplicate detection opclass/DESC/collation-aware.
2. **G2-I06 (H×M)** Index bloat / REINDEX rule (pgstattuple_approx or the btree bloat estimate) +
   `invalid_index` → `REINDEX INDEX CONCURRENTLY` instead of drop for leftover `_ccnew` of a
   still-wanted index. Consumes the dead `index_bloat_threshold_pct` and the existing
   `reindex_candidate` case mapping.
3. **G2-I07 (M×L)** Unused-index evidence: record `idx_scan` deltas per cycle (not just 0) and show
   "unused since <max(stats_reset, first_seen)>"; exclude indexes on partitions whose parent
   index is used.
4. **G2-I08 (M×L)** Single FK-index owner: retire analyzer `missing_fk_index` in favour of
   schemaguard (catalog-based, named index, verified route), fix schemaguard's `<@` check.

### Vacuum / autovacuum / freeze
1. **G2-I09 (H×L)** autovacuum_tuning reads reloptions; recommend `autovacuum_vacuum_threshold`
   + `insert_scale_factor` for append-heavy tables (PG13+), not only scale factor.
2. **G2-I10 (M×M)** Physical bloat (D06) and dead-tuple bloat as two distinct signals; route
   physical bloat to pg_repack script case.
3. **G2-I11 (M×L)** Consolidate wraparound: `xid_wraparound` (DB), `wraparound_freeze` (150M),
   `lint_txid_age` (500M), `lint_mxid_age` — one owner, one threshold ladder relative to
   `autovacuum_freeze_max_age` read from `pg_settings` (not hardcoded 200M).

### Query rules
1. **G2-I12 (H×M)** Regression detection on *interval deltas* (Δtotal_time/Δcalls between
   snapshots) instead of cumulative means — a regression after weeks of stable history barely
   moves the cumulative mean today, so the rule is insensitive exactly when it matters.
2. **G2-I13 (M×L)** Slow-query minimum calls and a total-time share floor to cut ad-hoc noise.

### System / cache / checkpoints
1. **G2-I14 (M×L)** After fixing units (B04), use *delta* hit ratio between snapshots (cumulative
   since stats reset hides current pressure) and make checkpoint forecast use
   `checkpoint_frequency_warning_per_hour` instead of a hardcoded 12.

### Forecaster
1. **G2-I15 (H×M)** Wire storage growth (D07): record size per collector cycle (db + top-N
   relations), auto-detect capacity where possible (provider API / `pg_settings` data dir is not
   enough → require config for managed), emit "days until full" with prediction intervals.
2. **G2-I16 (M×L)** Give every forecast finding an `ObjectIdentifier` (today 5 categories use
   `""`; the two cache findings share a key so one is always dropped by dedup) and gate on
   `MinRSquared` consistently.
3. **G2-I17 (M×M)** Handle day gaps (divide checkpoint deltas by actual elapsed hours), use
   `MaxTotalBackends` (B18), forecast from a daily rollup table (B16).

### Schema lint
1. **G2-I18 (H×L)** Fix B10/B14; register D06; add `sage` to default excludes explicitly (today
   filtered only at persistence).
2. **G2-I19 (M×L)** `lint_int_pk`: use the owning sequence's `last_value` (not `reltuples`) —
   deletes make row count a poor proxy for key exhaustion.
3. **G2-I20 (M×M)** Report lint rule health (last success, error) in the API so failures are
   visible instead of silently resolving findings.

### Cases
1. **G2-I21 (M×L)** Use finding `created_at/last_seen/occurrence_count` for `observed_at`/why-now;
   real expiry from finding age.
2. **G2-I22 (M×M)** Remove the silent 500-open-finding cap per DB (`cases_handlers.go:90`) or page
   it; group duplicate-object cases across categories (e.g. wraparound ×3, subset ×2).

### Schemaguard
1. **G2-I23 (H×M)** Retention: explicit column in contract, dry-run bound to contract version,
   operator ack or max-delete fraction per run (B12); write `external_reversion` (D12).

### Value / ledger
1. **G2-I24 (H×L)** Fix attribution (B11), sort outputs, aggregate across fleet pools.
2. **G2-I25 (M×M)** Wire incident credits (D08) from RCA/wraparound prevention with the
   evidence graph the table already requires.
3. **G2-I26 (M×L)** Self-audit scoped + paged (B21); expose via `/api/v1/ledger/audit` with counts.

---

## D. Questions the user isn't asking

1. **Is "suppress" a UI mute or a safety control?** Today it is neither (B01). Decide the
   semantics before users rely on it in auto mode.
2. **Who owns each invariant?** FK indexes (analyzer + schemaguard), duplicate/subset indexes
   (analyzer + lint overlapping), wraparound (3 rules), bloat (analyzer + unregistered lint +
   advisor `bloat_remediation`). Split ownership already produced B08/B10 and duplicate cases.
   A category-ownership registry would also fix B02.
3. **Why does `ActionRisk` on findings exist?** It is not persisted and the executor uses its own
   contract tiers; labels like `inactive_slot → pg_drop_replication_slot` = `"safe"` and
   `connection_leak → pg_terminate_backend` = `"safe"` mislead readers of in-memory findings/MCP.
4. **What is the analyzer's load budget on the monitored DB?** No statement_timeout on analyzer
   or forecaster queries; history scans grow with retention (B16). pg_sage stores its own
   telemetry in the monitored DB — is that still the right default?
5. **How do you know a rule is right?** Unit tests encode current behaviour (the parser tests
   literally document the bug). A fixture corpus of real `pg_get_indexdef`/catalog dumps with
   expected findings would catch B04/B08/B13 class bugs.
6. **What happens after `pg_stat_reset`, crash recovery, or failover?** Several rules assume
   monotonic cumulative counters (unused index, cache ratio, I/O ratio, checkpoint forecast).
7. **Are forecasts/Value numbers trustworthy enough to show a customer?** Forecasts page is empty,
   value 500s / zero in fleet, incidents avoided always 0 — the "prove value" story rests on
   unwired code.
8. **Should the deadcode report be trusted as complete?** No — it misses methods reachable via
   reflection (ForecastGrowth, RecordIncident, IsExecutable). Add a grep-based "no production
   caller" check for exported methods to CI.

---

## E. Verification notes

- **Ran:** `go vet` (clean) and `go test -count=1 -cover` on all 7 packages — all pass.
  Coverage: analyzer 70.7%, forecaster 71.3%, schemaguard 87.9%, cases 88.0%,
  schema/lint **23.4%**, value **24.4%**, ledger **47.1%** (below thresholds; the gap is
  DB-backed code). 513 PASS, **90 SKIP** (all require `SAGE_TEST_DATABASE_URL`, e.g.
  `TestPostgresRepositoryReadSnapshotFiltersHonestValue`, lint integration tests). No
  integration/e2e run per brief.
- **Overlay proofs (repo untouched):** `go test -overlay <scratch>/overlay.json -run TestReviewG2
  ./internal/analyzer/` with a scratch `zz_review_g2_test.go`. Output confirmed B03
  (`isSelfMonitoring=true`), B04 (60% → 0 findings; 0 → critical), B05 (3→1), B07 (finding
  immediately after reset), B08 (INCLUDE false positive, composite `tenant_id` index, tenant_b
  false negative), B13 (parser output), B19 (2→1), B23 (`+Infx threshold`), subset-of-partial.
- **deadcode:** `deadcode -whylive` shows `forecaster.QuerySizeHistory` "reachable only through
  reflection"; grep confirms no production callers for D07/D08/D13.
- **Could not verify (needs live PG):** lint SQL behaviour for B10 (read-verified; SQL has no
  uniqueness predicate), actual cost of B16, pgx NULL-scan error text for B11 (standard pgx v5
  behaviour for `string` targets), whether policy auto-executes the B09 drop under
  autonomous+tier3_moderate (executor group), RCA's use of cache ratio (`rca/signals.go:110`,
  cross-group — same unit bug).
- **Cross-group handoffs:** executor (suppression honouring, `database_id` on action insert,
  B09 policy path), collector (units, blk time columns, catalog facts for I05), api
  (value pool selection, suppress `suppressed_until`), rca (cache ratio units), autonomy
  (retention column heuristic, schemaguard FK `<@`), notify (dedup/cooldown).
