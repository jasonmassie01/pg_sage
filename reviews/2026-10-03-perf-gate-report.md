# pg_sage performance gate: report (2026-10-03)

Branch `claude/perf-gate` (worktree `C:/Users/jmass/pg_sage-perf-gate`, from `origin/master`
49a088a9). Not pushed. Brief: `reviews/2026-10-03-perf/BRIEF.md` in the perf-review worktree.

**Requirement.** pg_sage must ship high performing: a DBA must never need to add an index, a
hint, a setting or a retention job to make pg_sage itself behave on a large, messy database.

**What exists now.** A permanent gate, `TestPerfGate` (build tag `perfgate`), that builds a large
synthetic monitored database, runs the real standalone runtime in-process against it with
`pg_stat_statements` tracking pg_sage's own sessions, and fails with a ranked offender list.
A dedicated CI step runs it on PostgreSQL 17 (small scale on pull requests, large scale
nightly). As required, it fails on master: the offender list below is the work queue. The
cheap, unambiguous offenders are fixed on this branch; the rest are ranked for the fix phase.

## 1. How the gate works

| Piece | Where |
|---|---|
| Orchestrator (fixture, runtime, phases, report) | `sidecar/cmd/pg_sage_sidecar/perfgate_test.go`, `perfgate_runtime_test.go`, `perfgate_harness_test.go` |
| Budgets (the one place) | `sidecar/internal/testsupport/perfgate/budgets.go` |
| Scale and timing | `.../perfgate/scale.go` |
| Catalog fixture | `.../perfgate/catalog.go` |
| Sage history fixture | `.../perfgate/history.go`, `history_sre.go`, `history_snapshots.go` |
| Measurements (pg_stat_statements, table counters) | `.../perfgate/measure.go` |
| Generic plans of captured statements | `.../perfgate/explain.go` |
| Gate rules and ranking | `.../perfgate/gates.go` |
| Markdown report | `.../perfgate/report.go` |
| CI step | `.github/workflows/ci.yml` ("Performance gate", artifact `perfgate-pg17`) |

**Fixture.** `testdb.CreateDatabase` makes a disposable database, force-dropped when the test
ends (pass or fail). Catalog: application schemas with distinct tables plus *identical clone
schemas* (leaked branch/test clones), every table with a bigserial key (one sequence each),
three indexes per table, ten populated hot tables. History: `HistoryRows` rows in each growing
sage table, generated server-side with `generate_series`:

| Scale | Tables / indexes / sequences | Schemas (clones) | History rows per table | DB size | Build |
|---|---|---|---|---|---|
| small (PRs, default) | 500 / 1,500 / 500 | 10 (4) | 20,000 | 199 MB | 23 s |
| large (nightly) | 5,000 / 15,000 / 5,000 | 100 (40) | 150,000 | 1,435 MB | 6 m 16 s |

200,000 rows (the brief's example) measured 1,771 MB and 10.5 min, over the 1.5 GB budget, so the
large scale uses 150,000. Overrides: `PG_SAGE_PERF_SCALE`, `PG_SAGE_PERF_TABLES`,
`PG_SAGE_PERF_HISTORY_ROWS`.

Seeded tables (24 at full row count): `snapshots` (written through the real `snapstore.Writer`
in the #78 keyframe+delta format, 240 one-minute cycles, then replicated back in time; every row
reads back through `sage.snapshot_data`), `query_store`, `decision`, `action_log`, `findings`,
`incidents`, `recommendation`, `recommendation_revision`, `recommendation_transition`,
`verification`, `size_history`, `health_history`, `alert_log`, `notification_log`,
`explain_cache`, `runway_samples`, `io_rate_sample`, `sre_steps`, `sre_evidence`, `sre_events`,
`sre_change_events`, `sre_sli_samples`, `sre_autonomy_events`, `sre_autonomy_outcomes`; plus
`sre_investigations` (1/4), `sre_hypotheses` (1/2), `sre_investigation_outcomes` (1/4). Rows are
spread over 60 days (inside every retention window, so steady state deletes almost nothing),
runway samples over 40 h, IO samples over 13 days, SRE history over 7 days. 70% of the
per-database SRE history is bound to the runtime's own identity: the gate pre-creates the
`sre_deployments` row and the `startup:<db>` binding the runtime then reuses. Seeded history is
finished history (terminal investigations, resolved incidents, terminal recommendations), so the
runtime has nothing live to act on; ledgers span 12 families and 4 action classes.

**Runtime.** The shipped default config (`config.DefaultConfig()`), LLM off, every periodic
component compressed to one interval (collector, analyzer, runways, sequence runways, SRE
trigger: 15 s; the autonomy supervisor's floor is 60 s), built through `initStandalone()` exactly
as the binary does: collector, analyzer with optimizer/advisor rules, forecaster, tuner, RCA,
Sage SRE investigator and actions, runway monitor, schema guard and autonomy supervisor,
retention and briefing orchestrator, provider observability, IO admission. The API is wired
with the production `wireRouter`; one live-update (SSE) subscriber stays connected for the
whole run (an open dashboard), and the list endpoints `/findings`, `/cases`, `/actions`,
`/investigations`, `/value`, `/incidents`, `/recommendations` are called mid-window with a real
session. A light application workload (tagged, excluded) runs on the hot tables.

**Phases.** *Warmup* (45 s: startup, first cycles, keyframes, baselines) and *steady* (90 s =
6 cycles). `pg_stat_statements` is reset at each phase start; `pg_stat_user_tables` is read at
each boundary; the runtime is drained and its pool closed before the last reading so every
backend flushes its counters.

**Gates** (budgets in `budgets.go`, all positive, validated):

| Gate | Rule | Budget | Phases |
|---|---|---|---|
| A | no sequential scan of a sage table holding more than N rows (confirmed by `seq_scan` deltas; attributed to statements whose `EXPLAIN (GENERIC_PLAN)` scans it) | N = 5,000 | both |
| B | mean execution time of every pg_sage statement | 100 ms | steady |
| B | total pg_sage DB time per collector cycle | 3,000 ms (5% of a core at the default 60 s interval) | steady |
| C | rows inserted+updated+deleted per sage table per cycle (O(changes), not O(objects)) | 250 | steady |
| D | slowest single execution of a catalog statement | 500 ms | both |
| D | statements cut off by a timeout (from the runtime log: `pg_stat_statements` never records a cancelled statement) | 0 | both |
| E | API list endpoint: HTTP 200 within | 1,000 ms | steady |

Offenders are ranked by gate, then by how far over budget. Generic plans that scan a large table
the counters did not confirm are listed as *suspects*, not offenders.

**Run it.**

```
cd sidecar
go test -tags=perfgate -run '^TestPerfGate$' -count=1 -v ./cmd/pg_sage_sidecar
# PG_SAGE_PERF_SCALE=large  PG_SAGE_PERF_REPORT=/path/report.md
# PG_SAGE_PERF_INTERVAL=15s PG_SAGE_PERF_WARMUP=45s PG_SAGE_PERF_WINDOW=90s
```

Requires PostgreSQL 16+ (generic plans; CI uses 17) with `pg_stat_statements` preloaded.

## 2. Why lifeos' pg_stat_statements shows almost no pg_sage statements

`silenceSelfStats` (`sidecar/cmd/pg_sage_sidecar/mode_init.go`) is every pool's `AfterConnect`
(`metadb.go` monitored and meta pools, `fleet_bootstrap.go`): it runs
`SET pg_stat_statements.track = 'none'` on each connection, so a superuser-connected pg_sage
is invisible to `pg_stat_statements`. The 7 tagged statements seen on lifeos come from
connections that do not go through those pools, or from a role that may not set this
superuser-only GUC, where the SET fails silently (inferred; not verified on lifeos, which is
read-only for this review).

How to measure anyway: the gate replaces `silenceSelfStats` (now a package variable) with
`SET pg_stat_statements.track = 'top'`. Production behaviour is unchanged.

**Product decision (mine, recorded):** keep silencing by default. pg_sage's statements would
otherwise crowd the operator's own top-N and pg_sage's own query store reads them back (a
feedback loop). The gate is the mechanism that keeps pg_sage's SQL honest. Recommendation for
the coordinator: a `self_monitoring.track_own_statements` switch for dogfooding deployments
like lifeos, so the next field review can read pg_sage's cost directly.

## 3. Offenders on master (the work queue as found)

Large scale on master code (run 1: 200k rows, before any fix; 23 offenders). That run used
the first fixture (history in 14 days, ledgers in one family) and started the API router
mid-window; master's live-update broker polls with or without a subscriber, so its scans
are in run 1 as well. Ranked:

| # | Gate | Table / statement | Measured (steady unless noted) | Cause (`file:line`) | Status |
|---|---|---|---|---|---|
| 1 | A | `sage.action_log` | 89 seq scans, 6.8M rows read | live-update poll `count(*)`/`max()` over action_log+action_queue every 2 s (`internal/api/events.go:232`); verification purge FK/NOT EXISTS; actions list `COUNT(*) OVER` (`internal/api/actions_ledger.go:27`) | partly fixed (see 4) |
| 2 | A | `sage.findings` | 88 scans, 6.4M rows | live-update poll (`events.go:219`); resolved-findings purge; open index findings `ILIKE ... NOT IN` (`internal/analyzer/analyzer_checks.go`) | partly fixed |
| 3 | A | `sage.health_history` | 46 scans, 4.6M rows | live-update poll `count(*)` (`events.go:247`) | fixed when no dashboard is open; open while one is |
| 4 | A | `sage.runway_samples` | 18 scans, 2.4M rows (+10 warmup, 1.4M) | `clock_timestamp()` cutoffs in prune/load (`internal/runway/store.go`) and the runway-trend probe (`internal/sre/probes/catalog_runway.go:208`) | fixed |
| 5 | A | `sage.verification` | 12 scans, 1.6M rows | purge by `created_at` (no index); `verify.ListDue` asks for 4 verdicts, partial index holds 2 (`internal/verify/postgres.go:240`); FK checks on `decision_id` | fixed |
| 6 | A | `sage.alert_log` | 4 scans (+2 warmup) | purge by `sent_at` (no index) (`internal/retention/cleanup.go:148`) | fixed |
| 7 | A | `sage.explain_cache` | 9 scans (+3 warmup) | purge by `captured_at` (no index); plan-diff and explain readers' `captured_at` windows (`internal/analyzer/rules_plan_diff.go:341`) | fixed (index) |
| 8 | A | `sage.incidents` | 3 scans warmup | startup; not attributed by generic plans (section 5, item 8) | open |
| 9 | A | `sage.decision` | 6 scans (+3 warmup) | earned-autonomy reconcile joins all `execute` decisions (`internal/earned/reconcile.go:186`); decision purge by `created_at` (no index) | purge fixed; reconcile open |
| 10 | A | `sage.sre_change_events` | 1 scan warmup | change-feed age-out by `received_at` (no index) | fixed |
| 11 | A | `sage.sre_investigations` | 2 scans | investigation retention `updated_at < clock_timestamp() - ...` (`internal/sre/postgres_retention.go:70`) and others | retention fixed; 1 unattributed scan remains |
| 12 | A | `sage.sre_autonomy_outcomes` | 2 scans warmup (small run) | fixture artifact: one family in the first fixture; gone with 12 families | not an offender |
| 13 | B | `sre:runway_trends v1` | 220 ms mean | full scan (volatile cutoff) plus per-series regression | scan fixed; 108 ms mean remains |
| 14 | B | analyzer query history `SELECT id, sage.snapshot_data(...) WHERE id = ANY($1)` (`internal/analyzer/query_history.go:93`) | 185 ms mean | rebuilding 100 delta snapshots per call in PL/pgSQL | open |
| 15 | B | forecaster daily history (`internal/forecaster/datasource.go:101`) | 179 ms mean | `row_number() OVER (PARTITION BY date_trunc('day', ...))` over the snapshot window + delta rebuild | open |
| 16 | B | `/api/v1/actions` list (`actions_ledger.go:27`) | 165 ms mean | `COUNT(*) OVER (PARTITION BY sql_executed)` over all of action_log before LIMIT | open |
| 17 | B | plan-diff explain reader (`rules_plan_diff.go:341`) | 105 ms mean | `ROW_NUMBER() OVER (PARTITION BY queryid)` over 7 days of explain_cache | index added; recheck |
| 18 | D | `sre:sequence_runway v2` (`internal/sre/probes/catalog_sequences.go:35`) | 773 ms max (warmup) | catalog query over 5,000 sequences | open |

Gates C (rows written per cycle) and E (endpoints) passed at both scales: since #78 the writers
are O(changes). The slowest endpoints at 200k were `/value` 789 ms and `/actions` 766 ms (under
the 1 s budget but growing with history).

## 4. What this branch fixed

| Commit | Fix | Evidence |
|---|---|---|
| `a12ed3ee` perf(schema) | New idempotent migration `internal/schema/perf_index_migration.go` (registered last in `bootstrap.go`): `explain_cache(captured_at)`, `alert_log(sent_at)`, `verification(created_at)`, `verification(decision_id)`, `findings(last_seen) WHERE status='resolved'`, `sre_change_events(deployment_id, received_at)`, and indexes on every foreign key a purge fires: `findings`, `decision`, `action_queue` `(action_log_id)`, `decision(queue_id)`, `change_lease(decision_id)`, `incident_avoided` (3 keys), `schema_baseline` (2 keys) | `TestPerfIndexMigration_*`; permanent guard `TestSageForeignKeysAreIndexed` (every sage FK indexed unless its parent is never-deleted configuration, exemptions listed with reasons) |
| `c0027fe4` perf(schema) | `verification(next_evaluation_at) WHERE completed_at IS NULL` (matches `ListDue`), `decision(created_at)` (purge) | same tests |
| `826b034c` perf(runway,sre) | `now()` instead of `clock_timestamp()` in 8 time-window predicates: runway prune and restart load, runway-trend, query-store and Sage-actions probes, auto-propose and abandoned-run sweeps, investigation retention | `TestRunwayStoreStatementsAreIndexServable`; permanent guard `TestNoVolatileTimeWindowPredicates` (scans all non-test sources) |
| `071d7e17` perf(api) | live-update poll runs only while a subscriber is connected | `TestEventBrokerPollsOnlyWhileWatched` |
| `2f2a9f31` perf(analyzer) | open index findings read `status = 'open'` (the domain is open/resolved/suppressed) through `idx_findings_category_status` | `TestOpenIndexRecommendationTables_OpenOnlyThroughPartialIndex` |

Before/after at large scale. Run 3 uses the final fixture (60-day history, 12 families,
150k rows) and keeps one live-update subscriber connected for the whole run, so the
live-update scans are charged in both phases; the counts are therefore conservative:

| | master (run 1, 200k) | after (run 3, 150k) |
|---|---|---|
| offenders | 23 | 16 |
| sage tables with confirmed seq scans | 11 | 6 (3 of them only from the live-update poll with a dashboard open) |
| unexplainable captured statements | 19 (pg_stat_statements normalization) | 0 (repaired: `interval $n`, `EXTRACT($n FROM`) |

Offenders remaining after this branch (run 3: large scale, 150k rows, all fixes; the full
ranked report with suspects, costliest statements and per-table activity is the CI artifact):

| # | Gate | Subject | Measured | Work-queue item |
|---|---|---|---|---|
| 1 | A | `sage.action_log` (steady) | 163 seq scans, 8.8M rows | 1, 2 |
| 2 | A | `sage.findings` (steady) | 154 seq scans, 7.7M rows | 1 |
| 3 | A | `sage.health_history` (steady) | 50 seq scans, 7.5M rows | 1 |
| 4-6 | A | the same three tables (warmup) | 68 / 67 / 21 seq scans | 1 |
| 7 | A | `sage.incidents` (warmup) | 3 seq scans, 450k rows | 8 |
| 8-9 | A | `sage.decision` (steady, warmup) | 6 + 3 seq scans | 3 |
| 10 | A | `sage.sre_investigations` (steady) | 1 seq scan, 37.5k rows | 8 |
| 11 | B | forecaster daily snapshot history | 164 ms mean | 4 |
| 12 | B | analyzer query history (`snapshot_data ... ANY`) | 131 ms mean | 4 |
| 13 | B | `sre:runway_trends v1` | 108 ms mean (was 220) | 6 |
| 14 | B | `/api/v1/actions` list query | 107 ms mean | 2 |
| 15 | D | `sre:sequence_runway v2` (warmup) | 616 ms max | 5 |
| 16 | D | collector `sequences` category cut off by its statement timeout (warmup) | 1 | 5 |

Gone after the fixes: every seq scan of `runway_samples`, `verification`, `explain_cache`,
`alert_log` and `sre_change_events`, and every seq scan the gate attributed to a retention
purge. Gates C (rows written per cycle: the largest steady writer was
`sage.findings` at 438 rows over 6 cycles) and E (slowest endpoint `/actions` 165 ms) pass.
Small scale after the fixes: 12 offenders, the same families.

## 5. Remaining work queue (fix phase), ranked

1. **Live-update poll while a dashboard is open** (`internal/api/events.go:194-250`): three
   `count(*)`/`max()` scans of `findings`, `action_log`+`action_queue` and `health_history` every
   2 s per database: 8.8M + 7.7M + 7.5M rows read in 90 s at 150k rows. Fix: an O(1) change
   signature (`n_tup_ins+n_tup_upd+n_tup_del` from `pg_stat_user_tables` for the three tables,
   or a `sage.change_seq` row bumped by the writers / `LISTEN/NOTIFY`); keep the payload shape
   (the UI only uses the event type, `web/src/hooks/useLiveEvents.jsx`). Proof: gate A on those
   tables with the subscriber connected.
2. **Actions list** (`internal/api/actions_ledger.go:27`, `internal/api/handlers.go:1753`):
   `COUNT(*) OVER (PARTITION BY sql_executed)` and an unbounded `COUNT(*)` over all of
   `action_log` per page (107-147 ms at 150k; lifeos-sized history is ~10x). Fix: compute attempts
   for the page's rows only (lateral count on an indexed `md5(sql_executed)` or a stored
   attempt counter) and a capped/estimated total. Proof: gate B + E.
3. **Earned-autonomy reconcile** (`internal/earned/reconcile.go:186`): hash-joins every
   `execute` decision with `action_log` each cycle. Fix: drive from
   `action_log.executed_at > now() - lookback` (indexed) and look decisions up by key;
   bound by `l.id` order. Proof: gate A on `sage.decision`.
4. **Snapshot history readers** (`internal/analyzer/query_history.go:93`,
   `internal/forecaster/datasource.go:101`): 131-191 ms mean rebuilding delta documents in
   PL/pgSQL. Fix (with the #78 owner): read one row per day by index (`DISTINCT ON
   (date_trunc('day'))` over `idx_snapshots_category`) and decode deltas in Go
   (`snapstore` already has the decoder), or cache the daily points. Proof: gate B.
5. **Sequence catalog reads at 5,000 sequences**: the `sre:sequence_runway v2` probe
   (`internal/sre/probes/catalog_sequences.go:35`, 616-773 ms) and the collector's
   `sequences` category (`internal/collector/collect_catalog.go:213`, cut off by its
   statement timeout once in warmup) exceed the 500 ms incident budget. Fix: page by
   `pg_sequence.seqrelid` like the collector's table and index paging, and pre-filter
   by `last_value`/`seqmax` before the joins. Proof: gate D (max and timeout).
6. **Runway trend probe** (`internal/sre/probes/catalog_runway.go:208`): 108 ms mean after the
   scan fix (regression over every series' window). Fix: regress only series whose last sample
   moved, or keep running sums per series. Proof: gate B.
7. **Runway restart load** (`internal/runway/store.go`, `loadLastSQL`): reads the whole 48 h
   window once at startup (all rows qualify, so a sequential scan is the right plan for that
   query). Fix: a recursive skip scan over `runway_samples_series_idx` or a `last sample per
   series` row. Proof: gate A in warmup.
8. **Unattributed seq scans**: `sage.incidents` (3 per startup) and `sage.sre_investigations`
   (1 per window) are confirmed by the counters but no captured statement's generic plan
   scans them (a custom plan, a foreign-key action or a non-top-level statement). Next step:
   one gate run with `auto_explain.log_min_duration = 0` and `log_nested_statements = on` on
   the fixture database to name them.
9. **Latent (not yet exercised by the gate):** `verify` watch lookup
   `WHERE baseline->>'watch_id' = $1` (`internal/verify/postgres.go:234`) has no expression
   index.

## 6. Limitations (stated so nobody over-reads the gate)

- Generic plans are planned from `pg_stat_statements` text, where every literal is a `$n`: a
  partial index whose predicate names a literal (`status = 'resolved'`) cannot match, so some
  "suspects" are false. That is why gate A is confirmed by `pg_stat_user_tables` deltas and
  generic-plan scans alone are never offenders.
- Statistics counters flush when a backend is idle (up to ~10 s later); the warmup/steady split
  can shift a few late flushes into the steady phase. Final readings are exact (pool closed).
- Intervals are compressed (15 s) so each component runs about once per cycle; gate B's
  per-cycle DB time is therefore per component run, not per wall-clock minute.
- Timing budgets on a shared Docker machine are noisy; budgets are generous (100 ms mean,
  500 ms catalog max) and the seq-scan and write gates are deterministic.
- Schema guard (owned by `claude/p0-schemaguard-load`) runs in the gate but was not touched.
  The new `sage.decision` indexes (`action_log_id`, `queue_id`, `created_at`) serve foreign-key
  actions and the retention purge, not the schema guard path.

## 7. Decisions for the coordinator

1. The CI step is **report-only** (`continue-on-error: true`) because master fails it by design;
   delete `continue-on-error` when the work queue is empty. A blocking gate today would turn
   every pull request red.
2. A nightly `schedule` trigger was added to `ci.yml` (06:17 UTC) so the large scale runs; it
   also runs the rest of the CI matrix nightly.
3. `silenceSelfStats` became a package variable (test seam). Production behaviour unchanged;
   see section 2 for the self-tracking recommendation.
4. The large scale uses 150k history rows, not 200k (size budget).

## 8. Test Results

**Command (full suite, PG17 `pgsage-ag5` :55475, repo root mounted, `--cpus=2`):**
`go test -p 2 -count=1 -cover -timeout 2400s ./...` then
`go test -p 2 -count=1 -cover -tags=e2e -timeout 900s ./e2e/`
**Total:** 84 packages ok, 0 failed (non-verbose run; per-test totals not collected); e2e ok
(173.8 s).

**Touched packages, verbose, on PG14 (:55414), PG18 (:55418) and PG17 with `-race`:**
all ok except two `cmd/pg_sage_sidecar` fleet-reload tests, one per run, both infrastructure
flakes that pass on rerun (see Failures).

**Performance gate** (`go test -tags=perfgate -run '^TestPerfGate$' ./cmd/pg_sage_sidecar`):
FAIL by design (16 offenders at large scale, 12 at small); see sections 3-5.

**Lint:** `golangci-lint run ./...` and `--build-tags=perfgate` on the gate packages: 0 issues.

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/testsupport/perfgate (new, utility) | 91.5% (87.1% on PG14: generic plans need PG16+) |
| internal/schema | 81.6% |
| internal/runway | 90.1% |
| internal/api | 77.6% |
| internal/analyzer | 86.0% |
| internal/verify | 86.8% |
| internal/sre | 87.5% |
| internal/sre/action | 82.4% |
| internal/sre/probes | 93.3% |
| cmd/pg_sage_sidecar | 81.2% |

### Skipped Tests (must be zero or justified)
- internal/testsupport/perfgate: none with a test DSN set. `TestStatementsCaptureAndExplain`
  skips only when `pg_stat_statements` is not preloaded (CI and every local server preload it).
- internal/sre/causal: `TestContainer_RestartBetweenSamplesInvalidatesComparisons`,
  `TestContainer_PromotionBetweenSamplesInvalidatesComparisons`: pre-existing, need a
  container runtime (not touched here).
- internal/sre/pooler: three `TestPgBouncer_*`: pre-existing, need a PgBouncer (not touched).

### Failures
- PG14: `TestFleetReloadRejectsDuplicateNames`: "create extra test database: connect designated
  test server: failed to connect" (the shared PG14 server refused a connection under load);
  passes on rerun.
- PG17 `-race`: `TestFleetReloadConcurrentRetriesConverge`: the reload saw a reconnect race in
  the loaded race run; passes 3/3 on rerun with `-race`. Neither test touches code changed here
  (the only `cmd` production change is `silenceSelfStats` becoming a variable).

### Coverage Gaps (packages below threshold)
All touched packages meet their thresholds (business logic 70%, utilities 50%).

### Bugs Found This Session
1. [PERF] `internal/runway/store.go`: runway prune and restart load bounded `sampled_at` with
   `clock_timestamp()` (volatile, so not an index bound) and read all of `runway_samples` each pass.
2. [PERF] 6 more `clock_timestamp()` window predicates in `internal/sre` (probes, action sweeps,
   investigation retention): same defect.
3. [PERF] `internal/verify/postgres.go:240` `ListDue` asked for 4 verdicts against a 2-verdict
   partial index: full scan each pass.
4. [PERF] Retention purges of `explain_cache`, `alert_log`, `verification`, resolved
   `findings`, `decision` and the change-feed age-out had no index on their time column.
5. [PERF] 12 foreign keys on purged parents had no index: each purged parent row scanned the
   child table (`findings`, `decision`, `action_queue`, `change_lease`, `incident_avoided`,
   `schema_baseline`, `verification`).
6. [PERF] `internal/api/events.go`: the live-update poll ran full-table aggregates every 2 s
   per database with nobody subscribed.
7. [PERF] `internal/analyzer/analyzer_checks.go`: open index findings excluded two states
   instead of naming the open one, defeating the open-findings partial index.
8. [PERF, open] items 1-9 of section 5.

### Manual Checks Remaining
- None for the code here. The CI step and the nightly schedule first run on the
  coordinator's push.

## 9. Post-test audit

- **Untested inputs.** Timing values that are not Go durations are rejected (tested). A scale
  with zero clone schemas is valid and covered only by `Validate`. `IsCatalogQuery` is a name
  list: a catalog view outside it would not be charged to gate D (it is still charged to gate
  B); `pg_stat_*` views are all covered.
- **Assertions that passed when broken.** Found two and fixed them (`d6fae579`): the live delta
  test passed on an absolute reading (an exact unit test now pins the subtraction), and
  doubling the rows-written budget survived mutation (a one-row-over case now kills it).
- **Mutation testing** of the gate logic (10 mutants: seq-scan threshold boundary, mean-time
  boundary, steady-phase guard, sage-schema filter in the plan walker, delta subtraction,
  rows-per-cycle budget, timeout dedupe count, endpoint status check, `interval $n` repair,
  cycle DB-time budget): 10/10 killed after the audit fix (9/10 before).
- **Fakes that hide failures.** The gate uses none: real PostgreSQL, the real runtime, the
  production router and a real session. The LLM is off by design (its SQL is the same; model
  latency is not pg_sage's database cost). The fixture's data distribution is synthetic: the
  first fixture put every ledger row in one family and all history in 14 days, which
  manufactured plausible seq scans; both were corrected (12 families, 60-day spread) and are
  pinned by tests.
- **What the gate cannot see.** A statement cancelled by a timeout is caught only through the
  runtime log (gate D-timeout); seq scans by foreign-key actions and nested statements are
  confirmed by the counters but not attributed to a statement (work-queue item 8).
