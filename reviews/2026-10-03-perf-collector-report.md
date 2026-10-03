# Perf fix: collector and catalog reads (2026-10-03)

Branch `claude/perf-collector` (from `claude/perf-base`), worktree
`C:/Users/jmass/pg_sage-perf-collector`. Not pushed. Brief:
`reviews/2026-10-03-perf/FIX-BRIEF.md`. Scope: measured.md M1, M3, M4, M7 (and M13), static.md
F3/F11/F12 (catalog parts), gate offender 5 (sequence catalog reads at 5,000 sequences).

## 1. What changed

| # | Problem (evidence) | Fix | Files |
|---|---|---|---|
| 1 | **Safety.** The sequences query held one lock per sequence until its transaction ended: 12,012 locks every minute on lifeos (23% of its lock table). With PG defaults (64 x 100) it fails with "out of shared memory", and other sessions can fail too (M4) | Sequences are read in oid pages, **one transaction per page**, each page `LIMIT LEAST(1000, a quarter of max_locks_per_transaction x (max_connections + max_prepared_transactions))`, the same budget the SRE `sequence_runway` probe uses. Only used, readable sequences are kept, with the same arithmetic and selection as before (>= 1% or top 100, at most 1,000). At most 20,000 are read per cycle; past that the read resumes where it stopped and the rest come from earlier reads. Coverage (scanned, unreadable, used, complete) is on the snapshot | `collect_sequences.go`, `queries_catalog.go`, `catalog_cache.go` |
| 2 | The tables page rebuilt the grouped `pg_stat_user_tables` view for every table after the cursor, on every page, and called 3 size functions per table (114 ms per 1,000-row page on lifeos, 30% of pg_sage DB time; M1) | A `pg_class` oid keyset (`c.oid > $1 ORDER BY c.oid LIMIT $2`) calling the `pg_stat_get_*` functions the view is built from. The index aggregates come from a per-table LATERAL over `pg_index`. Sizes are `relpages x block_size` (heap + TOAST, plus summed index pages), `total = table + index`. The **100 largest tables and the 100 largest indexes** get exact sizes each cycle, best effort: a lock timeout keeps the estimates, so a table under `ACCESS EXCLUSIVE` no longer stalls the page | `queries.go`, `collect_tables.go` |
| 3 | `pg_get_indexdef` ran for all 35k indexes every minute (M3). Each call also leaves ~4 KB in the backend's catalog cache for good (measured: +16 MB for 3,600 indexes), and a full size pass grew a lifeos backend to 299 MB (F3) | Index definitions are cached in the collector, keyed by (index `pg_class` xmin, relfilenode, table xmin, schema xmin). A watermark catches renames that touch neither `pg_class` row (column renames: verified, neither xmin moves). The watermark is the sum of the `n_tup_upd` counters of `pg_attribute`, `pg_namespace` and `pg_proc`. There is also a 1-hour backstop for servers with `track_counts` off. Bulk definition fetches (>= 100 in one page) run on a connection hijacked from the pool and closed when the pass ends, so no pool backend keeps that cache. `pg_relation_size` per index is gone (relpages, exact for the top 100) | `collect_indexes.go`, `scratch_conn.go`, `catalog_cache.go` |
| 4 | `pg_database_size` ran every collector cycle and on every `/metrics` scrape, untimed (M7). It timed out on lifeos and failed the `system` step, which loses the whole snapshot | Measured every 15 min, apart from the system statement. A failure keeps the last size and retries at the next interval; it never fails `system`. An unknown size is stored as JSON `null`, so `avg()` ignores it. `/metrics` reports the collector's cached size, or omits the gauge when no size is known | `collect_system.go`, `system_stats_json.go`, `cmd/pg_sage_sidecar/prometheus.go` |
| 5 | Catalog transactions | Every collector catalog statement already ran under the configured `statement_timeout` / `lock_timeout` (verified: there is no other `c.pool` use in the collector except `query_store` writes, which are sage writes). They are now also `READ ONLY` with `max_parallel_workers_per_gather=0` and `jit=off`: catalog scans spawned 2 parallel workers each, which is why lifeos saw 5 pg_sage backends | `catalog_query.go` |
| 6 | The tuner's stale-stats cache joined the view by name and ran `pg_relation_size` per table each tuner cycle (98-350 ms, M13). The TOAST lint rule ran `pg_total_relation_size` up to 3x per table hourly (551 ms, F12). Both opened every relation in a pool backend | Both read `pg_stat_get_*` by oid and `relpages`. The TOAST rule is `LIMIT 200`. Both statements are tagged | `tuner/stale_stats.go`, `schema/lint/rule_toast_heavy.go` |

## 2. Before / after

**Per page, PG17, 6,068 tables / 18,164 indexes / 4,800 used sequences** (synthetic, warm, three runs,
literal cursor = custom plan; the old view numbers are with a warm relcache, which flatters them):

| statement | before | after |
|---|---:|---:|
| tables page (1,000 rows) | 28-31 ms (57-64 ms first run) | **5.3-6.0 ms**; generic plan 8.5 ms (bounded index scan on `pg_class_oid_index`) |
| indexes page (1,000 rows) | 19-21 ms (incl. def + size) | **12 ms**, generic plan 10 ms; definitions 6 ms per 1,000 *only when changed* |
| sequences | one transaction, 4,800 locks, 17-23 ms | **5 ms per 1,000-sequence page**, <= 1,000 locks per transaction |
| `pg_database_size` | 54-68 ms every cycle and every scrape | once per 15 min |

**lifeos projection (inferred from the above; lifeos was not touched):** tables 16 pages x ~6 ms
~= 0.1 s per cycle (was 1.8 s); indexes 36 pages ~= 0.4 s on the first cycle, then pages only
(was 1.1 s); sequences 13 transactions of <= 1,000 locks (was one of 12,012); size walk 4x/hour
(was 60x/hour + scrapes). All are well under the 50 ms/page target and the 500 ms catalog budget.

**Backend memory** (1,200 tables x 3 indexes, single-connection pool, two cycles): old SQL
+20 MB per backend; the rewrite before the scratch connection +16 MB (all `pg_get_indexdef`);
final < 6 MB (test bound; the top-100 exact sizes are ~2 MB of it).

**Perf gate, small scale** (500 tables / 1,500 indexes / 500 sequences, PG17 `pgsage-ag1`), steady phase:

| statement | base mean / max | branch mean / max |
|---|---:|---:|
| collector tables page | 30.5 / 43.3 ms | **4.4 / 6.4 ms** |
| collector indexes page | 16.7 / 26.2 ms | **6.8 / 11.1 ms** |
| exact sizes (top 100) | - | 3.6 / 6.3 ms |
| tuner stale stats | 3.5 / 6.5 ms | below the top-20 list |

Offenders: 15 before, 15 after. All are outside this scope: the live-update poll, retention
purges, decision reconcile, and the snapshot history reader (`WITH ranked`, analyzer/forecaster).
No collector statement was an offender at small scale before or after. Gate offender 5 (the
collector `sequences` timeout at 5,000 sequences) only shows at large scale. Per the brief, the
gate was not run at large scale. Instead it is proven directly: 5,200 sequences read in pages
holding at most 1,000 + 64 locks per transaction, at ~5 ms per page against a 500 ms timeout. The
SRE `sequence_runway v2` probe (616-773 ms at large scale) belongs to the SRE owner.

## 3. Decisions (product calls, recorded)

1. **Sizes are estimates outside the top 100.** Consumers use sizes for display, bloat MB, build-time
   estimates and prompts, never for safety gates. Index `relpages` is exact after VACUUM/ANALYZE.
   Heap `relpages` lags until autovacuum or autoanalyze (bounded by the 10% thresholds). Exact
   sizes for every relation cost a relation-cache entry per relation in every pool backend, and
   that is the defect being fixed. Side effect: sizes change less often, so snapshot dedupe (#78)
   stores less.
2. **Read every sequence each cycle up to 20,000 (lifeos: all 12k), in bounded transactions,
   rather than every 10 minutes.** This keeps the snapshot identical to v1.8.2 for consumers
   (golden test). Rotation with cached last reads only starts past 20,000.
3. **Bulk index-definition reads go to a throwaway connection.** This is the only way to get
   `pg_get_indexdef`'s catalog-cache cost back, since backends never shrink.
4. **A column/schema/function rename anywhere refreshes all cached definitions.** It is rare and
   runs on the scratch connection. On lifeos it costs ~0.45 s, spread over pages.
5. **No pool-wide `statement_timeout`.** In standalone mode the monitored pool also runs schema
   bootstrap (e.g. `CREATE INDEX CONCURRENTLY` on multi-GB sage tables), snapshot writes and
   executor DDL. A pool default at `query_timeout_ms` (500 ms) would break them. Every collector
   catalog read is under the configured timeouts. **Coordinator decision needed:** the remaining
   untimed catalog readers (`analyzer/analyzer_checks.go`, `optimizer/context_builder.go`,
   `migration/risk_queries.go`, the other lint rules) are bounded only by their context
   deadlines. Either wrap them in a read-only, timed transaction (the collector's
   `catalogQueryVia` is the pattern), or set a pool default once bootstrap and executor paths are
   audited for explicit overrides.
6. `selectSequences` breaks ties by schema, then name, in byte order. The old SQL used the database
   collation. This only matters for equal `pct_used` at the 100/1,000 cut.

## 4. Test Results

**Command (full suite, PG17 `pgsage-ag1` :55471, repo root mounted, `--cpus=2`):**
`go test -p 2 -count=1 -cover -timeout 2400s ./...`, then
`go test -tags=e2e -count=1 -timeout 900s ./e2e/`.
**Total:** 82 packages ok, 3 failed on the first run: the collector golden test (xid age raced,
fixed in `4fe51374`), the `/metrics` characterization (fixed, see Failures) and
`internal/api TestSREAPI_ExportAndPinNeedAnOperator` (flake, see Failures). After the fixes, the
collector and cmd packages pass on PG17 (also under `-race`), PG14 and PG18. e2e: ok (156 s).
**Touched packages:** PG14 all ok. PG18 all ok except one known fleet-reload flake (below).
`-race` on PG17: all ok.
**Lint:** `golangci-lint run ./...` 0 issues.

**Coverage (touched packages):**

| Package | Coverage |
|---|---|
| internal/collector | 88.3% (PG17), 88.6% (PG14), 89.0% (PG18) |
| internal/tuner | 85.7% (PG17/18), 84.0% (PG14) |
| internal/schema/lint | 77.9% |
| cmd/pg_sage_sidecar | 81.3% (PG14) |

### Skipped Tests (must be zero or justified)
- PG14 only: `TestCollectIO`, `TestPhase2_CollectIO_FieldsPopulated` (`pg_stat_io` is PG16+);
  7 tuner hint tests (no `pg_hint_plan` on the PG14 matrix server); `TestHintRemovalFindings_*`
  (same reason). All pre-existing.
- All versions: `TestGeneratePlanFixtures` (a fixture generator, opt-in). Pre-existing.

### Failures
- `internal/api TestSREAPI_ExportAndPinNeedAnOperator` (full PG17 run only): passes alone. The
  API package is untouched.
- PG18 `cmd TestFleetReloadWithoutDatabaseChangeKeepsRuntimes`: nil executor during reload under
  load. It passes 3/3 on rerun and is the same class as the fleet-reload flakes in the gate report.
- Fixed on this branch: the golden table tests compared `age(relfrozenxid)` exactly, which moves
  between two reads. The `/metrics` characterization pinned the per-scrape size walk this branch
  removes.

### Coverage Gaps
All touched packages meet their thresholds (business logic 70%).

### Bugs Found This Session
1. [SAFETY] `collector/queries.go` `sequencesSQL`: one transaction held a lock per sequence
   (pg_sequences -> `pg_sequence_last_value`).
2. [PERF] `collector/queries.go` `tableStatsSQL`: the grouped view was rebuilt per page, and
   there were 3 storage calls per table.
3. [PERF/MEMORY] `pg_get_indexdef` fills the backend catalog cache (~4 KB per index), which is
   never released. It was found by the memory test after the first rewrite. Fixed with the
   scratch connection.
4. [RELIABILITY] `pg_database_size` inside the system statement: a timeout lost the whole
   snapshot.
5. [PERF] `/metrics` walked the database on every scrape, untimed.
6. [PERF] The tuner's stale stats and the TOAST lint rule opened every relation.

## 5. Post-test audit

- **Mutation testing (12 mutants, all killed after the audit fix):** final-page detection
  (`<` to `<=`), the lock-budget clamp removed, the watermark ignored, table xmin dropped from the
  definition version, the scratch threshold disabled, the size cadence always due, top-N reversed,
  the cache range boundary (`<=` to `<`), the size never cached, the parallel-workers setting
  replaced, and the catalog cache lock removed (`-race`). One survived at first: `idx_tup_fetch`
  for a table without indexes. No fixture table had heap fetches without an index. The fixture now
  bitmap-scans through a since-dropped index (`8aef9d27`).
- **Assertions that could pass while broken:** golden tests compare whole JSON rows, not
  `err == nil`. The rotation test compares against a full legacy read after drops. The memory
  test compares against a warm backend (the scratch connection can replace the pool backend).
- **Fakes:** none. Every DB test runs real PostgreSQL 14/17/18, and the legacy SQL is the
  reference.
- **Not covered:** TOAST-index pages are not in the table estimate (small). Rotation past 20,000
  sequences is tested with a lowered cap, not 20k+ real sequences. lifeos itself was not measured
  (forbidden). Sequence coverage is on the snapshot but not persisted or shown in the API (API
  owner).

## 6. Left for others / coordinator

- Decision 5 (timeouts for the remaining catalog readers / pool default).
- SRE `sequence_runway` and `cluster_database_size` probes could reuse the collector's
  sequence pages and cached size (SRE owner); static.md F12's other lint rules (serial-usage
  `pg_get_serial_sequence` over all columns: 412 ms).
- Commits: `78c6861d` (tests), `b460475d` (test fixes), `4fdcebfa` (collector), `6ac7261a` /
  `11613b4f` (tuner/lint tests), `0a1ef4ce` (tuner/lint), `30c5bf37` (changelog), later test
  commits.
