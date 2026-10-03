# Schema guard: flat history read on a flooded decision ledger

Branch `claude/fix-guard-ledger-scan` (base `origin/master` d7b3a52b, v1.8.3).

## Problem

In the lifeos dogfood (v1.8.3), `pg_stat_statements` showed the schema guard's history read
(`WITH /* pg_sage */ guard AS MATERIALIZED (...)`) at **13.3 s mean, 14.9 s max** per call
(5 calls since the last reset, 93,209 buffers per call). The brief measured 12.7 s mean and
12.9 s max earlier. `sage.decision` holds 252,974 `schema_guard` rows. Almost all were
written in 9 hours by v1.8.1's flood (about 33,000 rows an hour between 21:00 and 06:00 UTC on
2026-10-02/03). They are 158,510 `type_tightening`, 57,640 `everything_text` (both
`recommend`) and 36,824 `missing_fk_index` (`park`). They cover 170 identities, and each row
names up to 71 leaked `test_*` schemas. None is a dry run, none has `external_reversion`.

The read selected every row whose `target_objects ?| <scan targets>` matched (nearly the
whole flood), then re-aggregated them all on every structural scan. The partial GIN index
`idx_decision_schema_guard_targets` only found them faster.

## What the read is used for

`HistoryIndex` feeds two places (`internal/schemaguard`):

| Value | Consumer | Used as |
|---|---|---|
| `LastHash[identity]` | `Custodian.recordChanged` | record a decision only when its hash differs from the identity's last recorded one (or the identity reappears) |
| `SuccessfulRetentionDryRuns` per (kind, table) | `planRetention` | `== 0` → dry run first, else apply |
| `ExternalReversions` per (kind, table) | `Plan` | `>= OscillationLimit` (3) → park |

The custodian only ever reads `LastHash` by `InvariantIdentity` of a scanned invariant. Counts
come only from rows with `disposition = 'dry_run'` or `external_reversion = 'true'`.
Recommendations and parks, which make up the whole flood, never count.

## Design chosen

The read asks only for what those consumers need. It is still one statement per scan.

- **`'k'` rows (last hash):** the scan's distinct identities are passed as `$2`. For each one
  there is a `LATERAL ... ORDER BY id DESC LIMIT 1` probe of the new partial btree
  `idx_decision_schema_guard_key ((evidence->>'invariant_key'), id DESC) WHERE feature =
  'schema_guard'`. The cost is one index probe per identity, however many rows each identity
  has (about 1,500 per identity on lifeos).
- **`'h'` rows (counts):** the same `?|` lookup by target as before, but limited to counted rows
  (`disposition = 'dry_run' OR external_reversion = 'true'`) through the new partial GIN
  `idx_decision_schema_guard_counted`. That index holds only those rows: 0 on lifeos, 16 kB
  in the 250k fixture.
- **Retired:** `idx_decision_schema_guard_targets` (a GIN over every schema guard row, 64 MB
  on the 250k fixture, 2.9 MB on lifeos) served only the old read. The new migration drops it
  once, and only while the catalog still has it. Its entry is removed from
  `decision_ledger_migration.go`.

Alternatives I rejected:

- *Bounding the read to a time window.* The newest hash of an identity that has been stable
  for months is older than any window. Dropping it would re-record every stable decision,
  the same flood in reverse.
- *An incremental per-identity state table.* It adds a second source of truth that must stay
  consistent with the ledger, which retention purges, on restart and in fleet mode. Two
  indexes give the same flat cost without new state.
- *Capping counts.* Not needed. The counted set is not part of the flood, and capping would
  change the values `History` returns.

### Semantics

The counts are identical: same rows, same filters, and family rows still count for every
listed member that is scanned. One intentional change, which is tested:

- **The last hash is the identity's newest row, whichever members it lists.** The old read
  took the newest row that named one of the scan's targets. Take a family whose membership went
  A → B → A, the way leaked test schemas come and go. The old read compared the A scan against
  the stale A row's hash and found it "unchanged", although the ledger's latest decision for
  that identity was B's. The change back went unrecorded. The new read compares against B's
  hash and records it. This matches the documented rule in `dedupe.go` ("the one last
  recorded for its identity").
- `LastHash` now holds only the scanned identities. Before, it also held any other identity
  that touched the same tables. No consumer reads those.

### Migration (`internal/schema/guard_history_index_migration.go`)

This is one idempotent migration, registered after `ddlSelfExclIndexes()`. It uses the same
checked loop as the decision ledger migration: the catalog is checked first, an INVALID index
is rebuilt, and the retired index is dropped only if `pg_indexes` still lists it.

**Deviation from the brief:** the build is a plain `CREATE INDEX` under the bootstrap advisory
lock, not `CREATE INDEX CONCURRENTLY`. The brief points at `decision_ledger_migration.go` as the
pattern, and that file deliberately avoids CONCURRENTLY. A concurrent build beside the bootstrap
deadlocked a fleet reload, and CONCURRENTLY cannot run inside the migration's `DO` block anyway.
I measured the lock cost on a 250,000-row, 490 MB copy of the flood shape on PostgreSQL 17:

| Index | Build (SHARE lock, blocks only writes to sage.decision) | Size |
|---|---|---|
| `idx_decision_schema_guard_key` | 481 ms | 16 MB |
| `idx_decision_schema_guard_counted` | 175 ms | 16 kB |
| (old) `idx_decision_schema_guard_targets`, for comparison | 7,393 ms | 64 MB |

This is a one-time pause of about 0.7 s for pg_sage's own writes to its own table, at startup.
A re-run takes no lock on `sage.decision` (tested under a held SHARE UPDATE EXCLUSIVE lock with
`lock_timeout = 1s`).

## Before / after

| Measurement | Before | After |
|---|---|---|
| lifeos `pg_stat_statements` (252,974 legacy rows) | 13.3 s mean, 14.9 s max | not deployed (lifeos untouched) |
| History read, 250,000-row legacy ledger, 27,206 invariants / 2,726 targets / 171 identities (best of 3, `--cpus=2`) | **51.5 s** | **15 to 37 ms** (two runs; PG14 and PG18: 11 ms) |
| Rows of `sage.decision` read by that statement | ~250,000 (bitmap heap over the whole flood) | **179** (8 counted + 171 newest rows), 0 seq |
| Statement execution time (EXPLAIN ANALYZE) | n/a | 2.0 ms |
| Whole guard scan (stub detector/router/recorder, real contract and history sources) | n/a (> 51 s) | **63 to 88 ms** |
| Small perf gate with the legacy-ledger fixture | **FAIL**: 1 offender, gate B, this statement at 139.6 ms mean | **PASS**: 0 offenders |
| Small perf gate on master without the fixture | PASS (164 s) | PASS (166 s with the fixture) |

The read time is mostly Go work: hashing 27,206 invariant identities and encoding 2,726
targets. The SQL executes in 2 ms.

## Decision retention of the legacy rows: left as is

I considered a "superseded" purge: delete schema guard rows whose identity has a newer row with
the same `decision_hash`. I did not do it, for these reasons:

- **The audit value is not clearly nil.** Since v1.8.2, an identity that disappears and comes
  back is deliberately recorded again with the same hash, and that row says "it came back at T".
  In the ledger a flood duplicate cannot be told apart from such a row (same key, same hash, same
  shape), so a superseded rule would also delete real audit facts.
- **The rows no longer cost the guard anything.** After this fix the read never touches them.
  What remains is disk (`sage.decision` is 199 MB in total on lifeos), and the existing rule already
  reclaims it: non-execute decisions age out after `retention.decisions_days` (30), and the
  flood rows are `observe_only` or `parked`, are not dry runs, and are not referenced, so they
  go around 2026-11-02. An operator who wants them gone sooner can lower `decisions_days`.

## Files

- `sidecar/internal/autonomy/schema_sources_postgres.go`: new `schemaHistorySQL` and
  `distinctIdentities`.
- `sidecar/internal/schema/guard_history_index_migration.go` (new), `bootstrap.go` (one
  registration line), `decision_ledger_migration.go` (old index entry and comment removed).
- Tests: `autonomy/schema_history_flood_test.go` (new), `autonomy/schema_history_postgres_test.go`,
  `schema/guard_history_index_migration_test.go` (new), `schema/decision_indexes_test.go`.
- Perf gate fixture: `testsupport/perfgate/legacy_guard.go` and `legacy_guard_test.go` (new),
  `catalog.go`, `history.go`. These add an idle family of 80 leaked `test_leak_*` copies, each
  with an all-text table, and HistoryRows/4 legacy rows naming 70 of them under the guard's real
  identity (5,000 rows small, 37,500 large). The small fixture grew from 199 MB to 219 MB.
- `CHANGELOG.md`: an `## Unreleased / ### Fixed` bullet.

## Tests

Tests were written and committed first (e4df1bb5), then run against the old code:

- `TestHistoryReadIsFlatOnALegacyFloodLedger`: seeds 250,000 rows set-based (160 schemas,
  70 targets a row, 170 identities, two drifting member windows, newest rows, dry runs and
  reversions). It checks exact `LastHash` and counts, a read under 200 ms, no seq scan, an
  `idx_decision_*` index path, at most 1,000 plan rows and index fetches (counter delta in one
  transaction), and a whole guard scan under 200 ms. **Before: FAIL (51.5 s). After: PASS.**
- `TestHistoryLastHashIsTheIdentitysNewestRowWhateverItsTargets`: **before FAIL** (returned
  `members-a`), after PASS.
- `TestHistoryReturnsOnlyTheScannedIdentities`: **before FAIL**, after PASS.
- `TestHistoryCountsExternalReversionsPerIntentAndTarget` and `TestHistoryReportsAFailedRead`:
  regression guards, passing before and after.
- The schema migration tests (creates, retires the old GIN, re-run takes no lock, INVALID
  rebuilt): **before FAIL**, after PASS.
- The perf gate fixture tests (one family with the guard's identity, leaked copies built,
  flood seeded): PASS.

Test corrections, each explained in its commit:

- `TestHistoryCountsFamilyRowsPerMemberAndKeepsTheNewestHash` recorded rows under an
  `invariant_key` that no invariant can have. It now records the family's real
  `InvariantIdentity`; the assertions are unchanged.
- `decision_indexes_test.go` exercised the checked loop through the retired index. The same
  three checks now use the counted-rows GIN index.
- fec46c29: the flood test first charged the connection's earlier, not yet flushed statements
  to the read (`pg_stat_xact_*` includes pending stats on PostgreSQL 15+). It now takes a delta.

- 3d728c6e: two environment faults. Parallel VACUUM outgrew the PG14 container's 64 MB
  `/dev/shm`, so the fixture now uses `PARALLEL 0`. Under `-race` the Go-heavy whole-scan check
  measured 257 to 281 ms, so the wall-clock budgets are scaled 5x only under the race
  detector. The rows-read assertions are unchanged.

### Mutation testing

I applied 9 mutants one at a time, and all 9 were killed:

| Mutant | Killed by |
|---|---|
| newest row → oldest (`ORDER BY id ASC`) | flood test (wrong LastHash) |
| drop the counted-rows filter (read every guard row) | flood test (rows read and time) |
| dry-run filter on the wrong disposition | flood test (counts) |
| look up identities by target instead of `InvariantIdentity` | flood test |
| reversion filter inverted | flood test (counts) |
| `LIMIT 2` per identity | flood test (an older hash wins) |
| migration no longer drops the retired GIN | `RetiresTheOldGINIndex` |
| key index without its `feature` predicate | `CreatesTheIndexes` |
| counted index without the reversion branch | `TestBootstrapCreatesThePartialGINIndex` |

## Test Results

**Command:** `go test -count=1 -cover -p 2 -timeout 1800s ./...` (golang:1.25 in Docker,
`--cpus=2`, PostgreSQL 17 `pgsage-ag1`)
**Total:** 11,649 passed, 0 failed, 21 skipped

**Coverage (touched packages):** `internal/autonomy` 84.0 %, `internal/schema` 83.4 %,
`internal/schemaguard` 94.1 % (untouched, its behavior tests still pass),
`internal/testsupport/perfgate` 91.8 %. All of them meet their thresholds.

**e2e:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: 78 passed, 0 failed, 13 skipped.

**Perf gate (small):** `PG_SAGE_PERF_SCALE=small go test ./cmd/pg_sage_sidecar -tags=perfgate
-run '^TestPerfGate$' -count=1 -timeout 1500s`: **PASS**, 0 offenders, 166 s. With the new
fixture and the old read it FAILS on this statement (139.6 ms mean against the 100 ms budget).

**Cross-version and race (touched tests):** PostgreSQL 14 and 18 pass (history read 11 ms,
scan 60 to 65 ms on 250k rows). `-race` on PostgreSQL 17 passes.

**Lint:** golangci-lint v2.11.4 (the scratchpad binary; `~/go/bin/golangci-lint` is v1.64.8)
reports 0 issues on `./...`, and 0 with `--build-tags perfgate`.

### Skipped tests (none in touched packages)

- Full suite (21): live cloud provisioning and gauntlets (AWS RDS, Cloud SQL, Lakebase,
  Azure: no credentials), live LLM providers (`TestChatLive_*`, `TestTier2Live_RealGemini`: no
  API key), container failover and promotion fixtures and PgBouncer probes (need their
  docker-compose services), `TestResolveLogDir_AbsoluteWindows` (Windows only),
  `TestRCAChildProcessFixture` (a helper process, not a test), `TestGeneratePlanFixtures` and
  `TestPGIncidentBench` (generators and benches behind env flags).
- e2e (13): live-LLM tests with `SAGE_LLM_API_KEY` unset.

### Failures

None.

### Coverage gaps

None below threshold in the touched packages.

### Bugs found this session

1. [BUG] `schema_sources_postgres.go` `schemaHistorySQL`: read and re-aggregated every schema
   guard row naming a scan target (13.3 s per scan on lifeos). Fixed.
2. [BUG] Same read: the last hash was the newest row naming a scan target, not the identity's
   newest row. A family that went A → B → A left the change back to A unrecorded. Fixed and
   tested.
3. [GAP] The perf gate seeded schema guard rows whose `target_objects` were objects, which
   `?|` never matches, so the gate could not see this read. The fixture now has the legacy
   shape.

### Post-test audit

- *Untested inputs:* identities with no rows (covered: no `'k'` row, so the hash is `""` as
  before); duplicate identities (deduplicated in Go); rows without `invariant_key` (pre-1.8.2;
  never looked up, as before).
- *Assertions that pass when broken:* the flood test checks exact hashes and counts for all
  27,206 invariants, and the rows-read bound is a counter delta, not just `err == nil`. One
  surviving behavior is equivalent: removing `t.target = ANY($1)` adds `ByTarget` entries for
  unscanned members, which `For()` never reads.
- *Fakes hiding failures:* the whole-scan check stubs the detector, router and recorder (the
  detector is not under test). The contract and history sources and all SQL are real
  PostgreSQL. The perf gate runs the real runtime.
- *Timing:* budgets are best-of-3 with about 3x headroom on `--cpus=2`, and are scaled under
  `-race`. The structural assertions (index path, no seq scan, at most 1,000 rows read) are
  the ones that do not depend on the machine.

## What is left

- Deploy to lifeos (not done here: lifeos must not be written to). At the first bootstrap
  of the new version, expect a one-time pause of about 0.7 s for pg_sage's own decision writes
  while the two indexes build and the old 2.9 MB GIN is dropped. The guard statement should then
  fall from about 13 s to milliseconds in `pg_stat_statements`.
- The legacy flood rows age out with `retention.decisions_days` (around 2026-11-02 at the
  default 30).
