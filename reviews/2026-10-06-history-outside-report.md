# Report: keep pg_sage's history outside the monitored database (v2.3.0)

Branch `claude/v23-history-outside`. Plan: `reviews/2026-10-06-history-outside-plan.md`.

## What shipped

An opt-in `history.store: meta` (default `monitored`: nothing changes for existing
installs). It requires meta-db mode. It moves **telemetry history**
(`sage.snapshots`, `sage.query_store`, most of pg_sage's bytes) to the metadata database.

| Area | Files |
|---|---|
| Config `history.store`, key class `operator_preference`, config meta regenerated | `internal/config/history.go`, `key_classes.txt`, `web/src/generated/config_meta.json`, `docs/generated/config-lifecycles.md` |
| Store schema: `database_id`, indexes, registry, migration progress and id map (meta database only) | `internal/schema/history_store_migration.go` |
| Store binding, registry, placement check, migration, cleanup, status, store share | `internal/histstore/*` |
| Readers and writers rewired | snapstore writer, collector partitions, querystore recorder and evidence, verify, analyzer regression baseline, optimizer cold start, forecaster, onboarding, API snapshots, SLO latency proxy, `plan_regressions` probe |
| Split catalog joins | recorder plan_hash (fingerprints from the monitored explain_cache), verify (`pg_index` on the monitored database), onboarding (`sage.config` there) |
| Retention store cleaner and summed cap | `internal/retention/history_store.go` |
| Footprint and self-budget | `internal/analyzer/history_store.go` |
| Runtime wiring, startup refusal, `pg_sage history migrate/status` | `cmd/pg_sage_sidecar/history_store_wiring.go`, `history_cmd.go` |
| Perf gate meta variant | `cmd/pg_sage_sidecar/perfgate_history_*_test.go`, `internal/testsupport/perfgate/history_store.go` |
| Docs | `docs/configuration.md` (the "not supported yet" note is gone), `architecture.md`, `deployment.md`, CHANGELOG `## Unreleased` |

## Product decisions

1. **Scope is the telemetry history only.** Findings, the action log, outcomes,
   verification, decisions and recommendations form one foreign-key graph. About 55 files
   read findings. Moving part of the graph across databases is impossible. Snapshots and
   the query store have no foreign keys and hold most of the bytes. The rest is deferred
   (below) and documented as staying in each monitored database.
2. **Identity is the meta-db record id**, not the name, because names can be reused.
   YAML fleet and agent databases have no record. They are refused in meta mode with a
   clear message rather than given a made-up id.
3. **Resolve from the monitored pool.** Every reader already holds the monitored pool, so
   a registry maps that pool to its store. Without it, about 15 constructors would need a
   new argument, and the observer report's risk was that one gets missed. Statements carry
   markers that are not SQL, so an unbound statement fails loudly. A static test scans the
   source and fails if any history statement lacks a marker.
4. **Key class `operator_preference`.** The key declares topology. It never widens
   authority and is never derived. Its safety comes from the startup refusal, and
   `meta_db` is already `safety_critical`.
5. **Snapshot cap in meta mode is the sum of the per-database caps** (5% of each
   database's size, with a 256 MB floor each). The cap is enforced store-wide, oldest day
   first, so every database keeps the same days. Retention windows are process-wide, so
   day drops are consistent. One store cleaner runs per process.
6. **`sage_footprint` keeps measuring the monitored schema.** That is what the guard
   protects, and in meta mode history no longer grows there. The finding says where
   history lives and points at `--cleanup` for leftover rows. `self_budget.storage_mb`
   adds the database's share of the store, because that storage is still pg_sage's.
7. **The query store migration mark is time-based** for the startup check (index only)
   and id-based for copying. A row backdated below the mark is not produced by pg_sage.
8. **The perf gate's meta variant uses the standalone runtime with the history moved.**
   Only the placement changes, so the comparison with the monitored gate is like for like.
   Meta-db control-plane queries have never been gated and are out of scope.

## Deferred

- Findings, the action log, verification, decisions and recommendations in the meta
  database. They form one FK graph and need their own release.
- explain_cache, size_history, health_history and briefings in the store.
- A `history_dsn` for standalone and YAML fleet (no meta-db record id there).

## Test Results

**Command:** `go test -cover -count=1 -p 2 ./...` (golang:1.25, `--cpus=2`, PG17
`pgsage/pg17-hint`, throwaway container)
**Total:** 107 packages ok, 5 failed in the full run; all 5 explained below and passing
when rerun alone. 0 skipped (`grep SKIP`: none).

Other runs:
- `-race` on 15 touched packages (PG17): all ok, 0 data races.
- PG14 (`postgres:14`), 14 touched DB packages: 13 ok. `collector`
  `TestCollect_OneFailedCategoryDoesNotFailTheSnapshot` fails identically on
  origin/master with PG14, so it predates this branch.
- e2e (`-tags=e2e`): ok.
- Perf gate (small): monitored PASS; meta PASS, 0 offenders in all four phases.
- golangci-lint (with and without `-tags perfgate`): 0 issues. `go vet`: clean.

**Coverage (touched packages):** histstore 81.1, schema 84.3, snapstore 98.6,
querystore 93.0, verify 89.6, optimizer 91.1, forecaster 88.9, onboarding 89.9,
sre/slo 89.1, sre/probes 93.2, retention 86.9, collector 88.6, analyzer 92.2, api 79.2,
config 92.8, testsupport/perfgate 89.4, cmd/pg_sage_sidecar 79.1.

### Skipped Tests
None.

### Failures (full run; each passes when rerun alone)
- cmd `TestFleetReloadConcurrentAppliesSerialize`: ping deadline under load.
- analyzer `TestHistoricalAverages_DedupeGolden`: bootstrap lock wait timed out under load.
- executor `TestApplyLockCeilingCapsCustodian`: lock-timeout timing under load.
- histstore: build failed because a test was renamed while the run was going; rerun ok.
- mcp `TestToolReferenceDocsMatchSchemas`: known failure in CRLF checkouts.

### Coverage Gaps
All touched packages meet their thresholds (business logic 70%, utilities 50%).

### Bugs Found This Session
1. [BUG] The phase 1 fixtures took `time.Now()` per placement, so the results differed
   by milliseconds. Fixed in the tests.
2. [BUG] Perf gate meta harness: a server-wide `pg_stat_statements_reset` wiped the
   store's warmup statements before they were read. Fixed.
3. [BUG] Monitored stores built only from a catalog handle (verify unit fakes) had no
   history handle. They now fall back to that handle.

### Post-test audit
- Mutation: making meta binding unscoped (`... OR true`) fails all 8 both-placement
  tests (querystore, forecaster x2, optimizer, api, probes, analyzer, onboarding).
- The fakes hide nothing on the history paths: every both-placement test runs on two
  real databases, with another database's colliding rows in the store.
- Still untested: a meta database that is also a monitored database in meta mode. The
  code path (`database_id IS NULL` scoping) is tested through `OpenMonitored` and the
  migration's legacy handling, but not end to end through a runtime.
- Still untested: a source whose sequence was reset after a migration (documented edge).

### Manual Checks Remaining
- None needed for this change (no UI changes).

## Code limits
New and changed functions are within 50 lines and lines within 100 columns, with one
exception: the `history.store` struct tag, which follows the config package's long `doc`
tags. Pre-existing violations in `config.go`, `handlers.go` and `metadb.go` are
untouched.
