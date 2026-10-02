# Dogfood lifeos-1: pg_sage v1.8.0 on a real database

Branch `claude/dogfood-lifeos-1` (from master = v1.8.0). lifeos is a real, production-like
PG16 database with 160 leaked test schemas: 12,043 sequences, 35,439 indexes, 15,301 tables,
50,901 `pg_index` rows. Every lifeos measurement below is read-only (`SELECT`/`EXPLAIN`, with
`default_transaction_read_only=on` and a statement timeout); nothing was written there and
the `pgsage-lifeos` container was not touched.

The brief had 3 bugs; the coordinator added findings 4-9c during the run. All are fixed,
tests first.

## What was built

| # | Problem on lifeos | Fix | Files |
|---|---|---|---|
| 1 | `sequence_runway` timed out (500 ms budget) every minute, so no sequence runway was ever measured | v2 query, scan cap, coverage, background budget, own cadence | `sre/probes/catalog_sequences.go`, `decode_sequences.go`, `runner.go`, `types.go`, `registry.go`; `runway/sequence_cadence.go`, `monitor.go`, `options.go`, `series.go`; `config/runway.go` |
| 2 | 143 open incidents for months; "Idle-in-transaction PID 6686" stayed open with the PID long gone | stale and gone-subject resolution, applied to hydrated incidents | `rca/stale.go`, `lifecycle.go`, `cycle.go`, `events.go`, `rca.go`, `trees.go`; `config/rca.go` |
| 3 | 129 duplicate open "Autovacuum falling behind" incidents, legacy rows without identity | identity backfill and merge at hydration, plus a partial index migration | `rca/reconcile.go`, `store_load.go`; `schema/incident_open_identity.go` |
| 4 | Forecaster expanded every sequence of every snapshot (> 70 s, 108% CPU, pool exhausted) | per-day sampling, bounded collector sequence list | `forecaster/datasource.go`; `collector/queries.go`, `collect_catalog.go` |
| 8 | Collector index query timed out and failed the whole snapshot | oid keyset paging, per-category degradation | `collector/collect_steps.go`, `collector.go`, `collector_helpers.go`, `snapshot.go`; `analyzer/rules.go`, `cycle.go` |
| 5 | "schema guard scan failed ... index verification unavailable" | verification is wired; the refusal names the missing precondition and is parked | `executor/verified_index_identity.go`; `cmd/pg_sage_sidecar/autonomy_park.go` |
| 6 | "custodian proposal withheld by policy: blast_radius_exceeded" as ERROR every cycle | parked route, reported once at info | `autonomy/supervisor.go`; `cmd/pg_sage_sidecar/autonomy_runtime.go` |
| 7 | 477 of 478 open `duplicate_index` findings in leaked copies of one schema | clone-family collapse | `analyzer/clone_schemas.go` |
| 9a | Monitors from June resumed in October and ran rollback DDL on a stale baseline | `verification_expired` | `executor/rollback_expiry.go`, `rollback_runtime.go` |
| 9b | Rollback `CREATE INDEX` failed with 42P07 (the app had recreated it) | `already_restored` | `executor/rollback_restored.go`, `rollback.go` |
| 9c | `public.idx_thesis_allocation_run` dropped 8 times in 40 minutes, recreated by the app each time | app-managed index memory | `analyzer/app_managed_index.go` |

Docs: `docs/configuration.md` (two new keys), `docs/generated/config-lifecycles.md` and
`web/src/generated/config_meta.json` (regenerated), `CHANGELOG.md` (Unreleased).

### 1. sequence_runway

- v2 SQL reads `pg_sequence_last_value()` directly. The old query joined `pg_sequence` to the
  `pg_sequences` view by name and scanned `pg_depend` whole. v2 skips never-called sequences
  and other sessions' temp sequences (reading those raises an error). It looks owners up
  through the `pg_depend` depender index and renders names only for the 50 returned rows. It
  orders nearest the limit first (ties by oid).
- Every row reports coverage: `sequences_total`, `sequences_scanned`, `sequences_used` and
  `sequences_unreadable` (no privilege; those were previously indistinguishable from unused).
  Truncation uses the runner's existing `Truncated` flag. `probes.SequenceCoverageOf` decodes
  it.
- Scan cap `min(SequenceScanCap = 20000, a quarter of the shared lock table)`, where the lock
  table is `max_locks_per_transaction x (max_connections + max_prepared_transactions)`. Reading
  a last value locks the sequence until the probe's transaction ends: measured 12,104 locks for
  12,100 sequences in one probe transaction. With a fixed 20,000 cap, the PG14/PG18 matrix
  (max_connections 100) failed other sessions with "out of shared memory", so the cap now
  follows the server. lifeos (256 x 200 / 4 = 12,800) is read whole. Past the cap, the
  sequences with the smallest catalog capacity are read first (effective limit minus min,
  owner column type included). That catches the classic "bigint sequence feeding an int
  column".
- Background budget: `Spec.BackgroundTimeout`, bounded by `MaxBackgroundStatementTimeout = 2 s`
  and only used by `Runner.RunBackground`. Investigations keep the 500 ms ceiling. Only
  `sequence_runway` declares one.
- The runway monitor reads sequences every `sre.runways.sequence_interval_seconds` (default
  600; the effective period is clamped between `interval_seconds` and
  `lookback/(min_samples-1)`). It goes through `RunBackground` and samples only fresh readings
  (re-sampling a cached value would flatten the trend). A failed read is reported once and
  waits for the next due time. Coverage is logged once per change: a capped scan is WARN, a
  truncated listing is INFO, unreadable sequences are WARN.
- All probes now run with `jit = off`. On lifeos JIT took 10.6 ms of 187 ms.

### 2. Stale and gone-subject incidents

- `rca.stale_after_hours` (default 24, range 1-8760, at least the dedup window). An open
  incident not re-detected within the window resolves as `pg_sage:stale`, with a reason that
  names the window and the last detection. This runs after the cycle's detections merge,
  applies to hydrated incidents, and ignores the restart grace period (it is measured from
  stored detections). A zero-value config gets the default, never "resolve immediately".
- Backend subject: a `vacuum_blocked` incident naming an idle-in-transaction session now
  stores the holder's pid and `backend_start` in its chain (`Blocker`). Legacy rows carry
  only "PID N in state: idle in transaction". Before this cycle's detections merge, the
  engine lists those pids in `pg_stat_activity`:
  - pid gone, or present with a different `backend_start` (reused): resolved as
    `pg_sage:subject_gone`;
  - pid present without a comparable start: kept (stale handles it later);
  - lookup failure: nothing resolved, WARN.
- Quiet resolutions: resolving an incident last seen longer ago than the stale window sends
  no `incident_resolved` notification. lifeos would otherwise have sent ~140 notifications
  about June incidents.
- Operator semantics are unchanged and tested. `syncResolved` adopts an operator resolution,
  `updateIncident` never overwrites one, and a recurrence becomes a new linked incident.

### 3. Legacy duplicates

`rca.ReconcileOpenIncidents` runs at hydration. It needs the engine's database name, which a
schema migration does not have.

1. It backfills `identity_key` (the Go `identityKey`, re-implemented in SQL with `sha256`;
   parity is tested, including unsorted signals and non-ASCII objects) and `database_name`
   for open legacy rows. Rows without a name are adopted, as hydration always did.
2. It merges each identity's open rows into the earliest. The survivor gets the summed
   occurrences, the latest `last_detected_at`, the worst severity and the earliest
   escalation. The others are resolved as `pg_sage:merged` with "merged into <id>".

It touches open rows only, in batches (1,000 rows, 100 identities), under a per-database
advisory lock, so concurrent sidecars merge once. Running it twice changes nothing. On
failure it logs and hydration continues. Migration `ddlIncidentOpenIdentity` (registered in
`bootstrap.go`) adds the partial index `idx_incidents_identity_open (database_name,
identity_key) WHERE resolved_at IS NULL`. It also ensures `identity_key`, because registered
migrations run before the lifecycle migration adds it.

### 4 and 8. Bounded collection

- Forecaster: both daily aggregations read only the first and last non-empty snapshot of
  each day. Empty snapshots are told apart with `pg_column_size(data) > 12`, which needs no
  detoast. For monotonic counters the query totals are identical: the per-day deltas
  telescope (tested against the exhaustive form). Sequences under 1% are skipped.
- Collector:
  - tables and indexes page by oid (`relid` / `indexrelid > $1 ORDER BY oid LIMIT $2`)
    instead of a name keyset that sorted the whole catalog for every page;
  - sequences stored per snapshot are the used ones at or above 1%, plus the 100 most used,
    at most 1,000;
  - a failing catalog category (queries, tables, indexes, foreign keys, locks, sequences) is
    recorded in `Snapshot.Unavailable`, logged as WARN and not persisted. Only system stats
    still fail the snapshot.
- Analyzer: `unused_indexes` needs indexes and foreign keys; `missing_fk_indexes` needs
  foreign keys and indexes; table rules need tables. The index optimizer is skipped when
  indexes are unavailable. A missing category never reads as "nothing exists".
- On the "collector's 12000 cap": there is no cap in the code. lifeos has 12,043 sequences,
  and the 43 in the `sage` schema are excluded, which leaves exactly 12,000.

### 5 and 6. Parked custodian proposals

Verification is wired for every executor (`configureIndexVerification` in `New`), and a test
now pins that. The lifeos error came from schema-guard FK-index proposals that had no target
queries (no workload on the table). Changes:

- `verifiedActionForFinding` now names the missing precondition (index name, table,
  rollback, target queries).
- The cmd router turns `ErrCustodianProposalWithheld` and `ErrVerificationUnavailable` into
  a `schemaguard.ParkedRoute`. The schema guard records a parked remediation in the decision
  ledger, which is what the operator sees. The router logs it once per target and reason at
  INFO.
- The supervisor reports a parked route once at info, never as an error. Real failures stay
  errors.

### 7. Clone schema families

A family is at least 5 schemas whose names end in a generated suffix (hex, digits or
separators; at least 6 characters, with a digit) and that have the same set of table names.
Its findings collapse into one `clone_schemas` finding. That finding gives the counts by
category and example schemas, and recommends dropping the schemas if they are unused. No
names are hard-coded. `public` and hand-named schemas never collapse.

### 9a, 9b, 9c. Rollback monitor

- 9a: a resumed monitor whose window ended more than one window ago (grace = max(window,
  1 h)) is marked `verification_expired` with why. It gets no regression check, no rollback,
  no credit, and verification is recorded as `unverifiable`. Inside the horizon it still
  checks once, immediately.
- 9b: before running a `CREATE INDEX` rollback, the target is compared with the existing
  index of that name (`pg_get_indexdef`, normalized). Same definition: `already_restored`,
  verification `revert`, credit zeroed, no DDL. Different definition: `rollback_failed`
  naming the conflict, and the existing index is left alone. The check runs again after a
  failed DDL, to cover the race.
- 9c: `sage.action_log` drops of the last 180 days whose index exists again with the
  dropped definition make the index app-managed. Drops that pg_sage rolled back itself do
  not count. The index's `duplicate_index` and `unused_index` findings are replaced by one
  `app_managed_index` finding without SQL. Because the drop category is evaluated and no
  longer emitted, the durable drop recommendation resolves and is never executed again.

## Product decisions

1. **Background budget 2 s, separate from the 500 ms incident ceiling.** Spec §11's 500 ms
   cap is an investigation budget. Slow-cadence sampling at 2 s every 600 s costs less
   backend time than the old 500 ms every 60 s, and investigations keep 500 ms.
2. **Resolution actors stay `pg_sage:<mechanism>`** (`pg_sage:stale`,
   `pg_sage:subject_gone`, `pg_sage:merged`), matching `pg_sage:auto_resolve` and
   `pg_sage:superseded`. The brief said `resolved_by='pg_sage'`; a bare `pg_sage` would
   break the existing convention. **Coordinator: confirm.** Reasons start with `stale:` /
   `merged into` / `backend N is gone`.
3. **Quiet resolutions.** An incident last seen longer ago than the stale window is resolved
   without a notification. Telling an on-call engineer about a months-old incident is noise,
   and the resolution stays in the record.
4. **The stale window cannot be disabled** (1-8760 h). Under the AI-DBA principle,
   incidents that pg_sage itself no longer sees must not stay open forever.
5. **Legacy merge at hydration, not in the migration.** The identity includes the engine's
   database name, which legacy rows lack and a migration cannot know. The migration adds
   the index the merge relies on.
6. **App-managed indexes get a finding, not an approval gate.** Requiring approval would keep
   proposing a drop the application will undo. pg_sage records why and stops proposing.
7. **Sequences and query volume in the forecaster.** Sampling per day changes the work from
   O(snapshots) to O(days) with the same daily totals for monotonic counters. Mid-day
   counter resets are approximated.
8. **Per-category degradation.** A category that cannot be read is unknown, not empty. Rules
   that would read absence as a fact do not run, so their findings are neither raised nor
   resolved.

## Evidence from lifeos (read-only)

| Measurement | Before (v1.8.0) | After (this branch's SQL, run read-only on lifeos) |
|---|---|---|
| `sequence_runway` | 2,117 / 2,379 / 2,899 ms (plan: `pg_depend` x `pg_attribute` hash + external sort 5.8 MB); 52 `statement_timeout` WARNs in 50 min | 454 / 374 ms cold, then 198-228 ms (final SQL: 212 / 198 / 201 ms); 12,043 total, 12,043 read, 1,149 used, 0 unreadable |
| Forecaster `seqAggsSQL` (30-day lookback) | > 70 s (coordinator), 1,999 snapshots x 12,000 elements, 498 MB of jsonb | 20 ms |
| Forecaster `queryAggsSQL` | 4,597 ms (2,012 snapshots x ~496 queries) | 29 ms |
| Collector index page (1,000 rows) | 127 ms, sorting the whole catalog each page (36 pages) | 22 ms (oid keyset) |
| Collector table page (1,000 rows) | joined `pg_class` by `relname` | 97 ms (by `relid`) |
| Collector sequences per snapshot | 12,000 rows (265 KB jsonb each) | 100 rows, 210 ms |
| Open incidents | 144 (143 legacy + 1; 14 identities, 140 not seen in 24 h) | Projected: reconcile merges 130, leaving 14; stale resolves 11 (the 3 seen in the last 24 h stay open); idle-in-transaction PIDs no longer in `pg_stat_activity` resolve as `subject_gone` |
| `public.idx_thesis_allocation_run` | 8 drops on 2026-06-12; exists now with the dropped definition; `duplicate_index` proposed again | Becomes `app_managed_index`, no drop SQL (history query run read-only on lifeos) |
| Open `duplicate_index` | 478 (477 in `test_*` schemas) | 477 collapse into clone-family findings (`test_memory_*` and others) |

v1.8.0 itself backfilled identity keys on update during the run. By 18:24 UTC no open row
lacked one, but the 144 open rows still had 14 identities, and v1.8.0 superseded only one
duplicate per cycle.

## Spec CHECKs touched

- CHECK-21 (probe caps): extended with the background budget ceiling and its validation. The
  500 ms incident ceiling is unchanged.
- CHECK-07: probe version bumped (`sequence_runway` v2).
- R2 runways (§4): sequence runways are measured on lifeos for the first time.
- R04/SURF-19 (incident lifecycle): stale, subject-gone and merged resolutions keep the
  never-overwrite rule.
- CHECK-40 (autonomy safety): 9a/9c stop autonomous actions that rest on stale evidence or
  fight the application.

## Test Results

**Command:** `go test -cover -count=1 ./...` (golang:1.25 in Docker, repo root mounted,
`SAGE_TEST_DATABASE_URL` = `pgsage-ag3` PG17 :55473, `GEMINI_API_KEY` unset). Touched
packages were also run on PG14 (:55414), PG18 (:55418) and with `-race` on PG17.

**Total:**
- **Full suite on PG17 (final run):** 80 packages with tests. 76 ok; 4 failed with
  `dial error: timeout` to `host.docker.internal` (Docker networking under host load; other
  agents were running their suites). Those 4 (`cmd/pg_sage_sidecar`, `internal/analyzer`,
  `internal/sre`, `internal/sre/action`) pass on rerun.
- **Touched packages (PG17, `-v`):** 3,619 passed, 0 failed, 2 skipped.
- **PG14 and PG18:** 12/12 touched packages ok on each (after the lock-budget fix; before it,
  the scale fixture exhausted the matrix servers' lock table, see Bugs found 5).
- **`-race`:** 12/12 touched packages ok.
- **Web (`npm test`):** 46 files, 246 tests passed. Only the generated config metadata
  changed; dist is not tracked.
- **Lint:** `golangci-lint run ./...` reports 0 issues.

**Coverage (PG17):**

| Package | Coverage |
|---|---|
| sre/probes | 92.6% |
| runway | 89.4% |
| config | 89.2% |
| schema | 81.6% |
| rca | 95.6% |
| collector | 87.6% |
| forecaster | 87.4% |
| analyzer | 86.4% (PG18) / 86.1% |
| executor | 83.5% |
| autonomy | 80.3% |
| store | 74.5% |
| cmd/pg_sage_sidecar | 72.6% |

### Skipped Tests (must be zero or justified)
- rca: `TestRCAChildProcessFixture`: a helper process for the restart test; it runs only
  when that test spawns it.
- rca: `TestTier2Live_RealGemini`: live LLM test, gated behind `PG_SAGE_LIVE_LLM=1` (rules).

### Failures (if any)
None in the touched packages. Infrastructure-only failures, all passing on rerun:
- Full suite, last run: 4 packages hit Docker dial timeouts.
- Earlier runs (pre-existing timing tests, under load):
  - `TestRunner_OneProbePerDatabase` (593 ms for 3 x 250 ms sleeps, which shows clock or
    timer distortion under load);
  - `internal/sre/changefeed` (cluster-wide interference).

### Coverage Gaps (packages below threshold)
All touched packages meet the thresholds (70% business, 50% utilities). The lowest is
cmd/pg_sage_sidecar at 72.6%.

### Bugs Found This Session
See "Bugs found while testing" below. Fixed in the branch: lock-table exhaustion (5), legacy
merge (1), per-signal resolution (2), `relname` join (3), batch 0 (4).

### Manual Checks Remaining
- MANUAL: deploy to `pgsage-lifeos` (coordinator's call) and confirm the hydration log line
  "reconciled open incidents for "lifeos": N given an identity, 130 duplicates merged".

## Bugs found while testing (not in the brief)

1. An `UPDATE` of a legacy incident only ever merged the first matching open row; the
   others stayed open (bug 3's root cause in the engine).
2. `rca` auto-resolution is per signal: one table with dead tuples kept every
   vacuum_blocked incident open. Staleness now bounds that.
3. The collector's table query joined `pg_class` by `relname` (not oid).
4. `collectTables` with batch size 0 collected no tables (LIMIT 0). Paging now uses the
   default batch.
5. Reading a sequence's last value holds a lock until transaction end. One probe held
   12,104 locks for 12,100 sequences. With a fixed 20,000 cap, this exhausted the shared lock
   table on the default-sized PG14/PG18 matrix: other packages' migrations failed with
   "out of shared memory". Fixed with a server-derived lock budget (above).
6. Five tests encoded the old behaviour. Each was corrected in its own commit, with the
   reason in the commit message:
   - the name-order pagination tests;
   - the never-called sequence listed with an unknown value;
   - a stale-snapshot test that relied on whole-snapshot failure;
   - a config rule that rejected valid configurations;
   - a severity max taken over text.

## Post-test audit

- **Mutation tests.** Each check was broken on purpose and the named test failed:
  - resume expiry: TestResume_Expired;
  - merge order: TestReconcile_Merges;
  - stale boundary `<=`→`<`: TestStale_Resolves;
  - backend_start comparison: TestBackendGone;
  - gone-subject pass: TestSubjectGone_ResolvesBefore;
  - never-used filter: TestCatalog_SequenceRunwayFindsTheBindingLimit;
  - forecaster day sampling: TestSeqAndQueryAggs (EXPLAIN loops);
  - persist skip: TestCollect_OneFailed;
  - rolled-back exclusion: TestLoadAppManaged;
  - sequence cadence: TestMonitorRead_;
  - parked dedupe: TestDatabaseCycle_Parked.

  One mutant survived: removing the already-restored pre-check, because the post-error
  re-check also settled the action. `TestRollback_AlreadyRestoredRunsNoDDL` now kills it.
- **Fakes.**
  - The runway cadence tests use a fake runner, but a DB test
    (`TestMonitorTick_SamplesSequencesOnTheirCadence`) checks the real probe and real
    samples.
  - The subject-gone ordering test uses a backend-lookup seam, but
    `TestSubjectGone_ResolvesWhenTheBackendExits` uses a real session and real
    `pg_stat_activity`.
  - The collector degradation test injects one failing category; per-page bounds use a
    real 120-schema fixture.
- **Load-sensitive tests.** The full suite runs next to other agents on one host. Three
  timing assertions failed only under that load. They were hardened, each with the reason
  in the commit message:
  - the scale probe gets up to three runs (190 ms standalone, 533 ms under load);
  - a client deadline counts as the budget expiring;
  - the evidence test re-captures after a cross-package `pg_stat_statements` reset.
- **Untested inputs:**
  - a sequence owned by two columns (the lowest attnum wins, same as v1);
  - quoted mixed-case index names in the app-managed history (`lower()` key);
  - descending sequences, which are still not in the runway, unchanged from v1.

## What is left

- `sage.snapshots` on lifeos holds 3.3 GB of `indexes` and 1 GB of `tables` rows. The
  collector stores the full index list every minute (35,439 rows). Analyzers need the full
  list, so bounding it needs a design: change-only storage or a longer interval for catalog
  categories. Recommended as the next finding.
- `TestRunner_OneProbePerDatabase` (pre-existing) and `internal/sre/changefeed`
  (pre-existing) failed once each under host load and pass standalone. The changefeed
  failure looks like cluster-wide interference: other packages create roles and reset
  `pg_stat_statements`.
- Deploying this branch to `pgsage-lifeos` would apply the reconcile and stale resolution
  for real. That is the coordinator's call (I did not touch the container).
