# Phase 3 "Cheap, self-aware observer" report (2026-10-04)

Branch `claude/p3-observer`, from origin/master = v1.10.0 (62a12d1b). Roadmap:
`reviews/2026-10-02-ai-next/ROADMAP.md`, Phase 3, "Cheap, self-aware observer"
(change-only catalog storage, partitioned history, history optionally outside the
monitored DB, a declared CPU/IO/storage budget with a self-cost metric). Release
target v2.1.0 (CHANGELOG bullet under `## Unreleased`).

## 1. Trust endpoint regression (fixed)

**Symptom.** The small perf gate failed intermittently on `GET /api/v1/trust` (1.2-2.1 s
against a 1 s budget) on branches and on master under load. A quiet run measured
199.7 ms.

**Root cause.** No single statement was slow. The costliest one, the family safety read,
took 4-8 ms. The problem was that the view sent **17 statements one after another**
(17 pool acquires per call). There were no pool waits and no CPU throttling of the
container. On the loaded, shared Docker host each round trip costs 10-360 ms
(`SELECT 1` over the same path with nothing else of pg_sage running: p50 11 ms, p99
152 ms, max 363 ms). pg_stat_statements showed statements with about 0 ms of execution
that the client saw take up to 3.5 s. So the Trust page paid the host's round-trip
latency 17 times.

Two reads also grew with history:
- The family safety read fetched every outcome in the 30-day window (20,000 in the gate)
  to count the harmful ones, and it ran twice per view (once for the view, once for the
  effective levels).
- The shadow summary read `sage.shadow_decision` in full.

**Fix** (`d9b89e60`, `857032ec`, `ec11cda6`):
- The ledger's 10 reads go on one pgx batch, sent as one pipelined round trip after the
  proposal-expiry write (`internal/earned/reads.go`, `trust_view.go`). The set reads
  accept a `reads` sink (run now or queue), so the per-pair reads and the grid read still
  share the same SQL.
- `AnnotateTrust` reuses the safety record the view already read. A view that carries no
  record still reads it.
- New indexes: a partial index on harmful/unsafe outcomes
  (`idx_sre_autonomy_outcomes_harmful`) and a covering index for the shadow summary
  (`idx_shadow_decision_class_summary`). Both are in a new idempotent migration,
  `internal/schema/observer_index_migration.go`.
- Gate measurement: one warm-up call per endpoint, then 5 interleaved rounds. The gate
  charges the median, fails on any non-200, and reports the max. The 1000 ms budget is
  unchanged.

**Numbers.** Small fixture with the runtime running, 18 calls each, same host and same
hour:

| | before (origin/master) | after |
|---|---|---|
| round trips (pool acquires) per call | 17 | 7 |
| p50 | 71 ms | 21 ms |
| p90 | 631 ms | 611 ms |
| max | 949 ms | 678 ms |

The remaining tail is the host's per-round-trip latency, now paid 7 times instead of 17.
The 7 are: session lookup, expiry, the batch, 2 for the SLO budget and 2 for the HA
status. The gate's median absorbs the tail.

**Found along the way (gate A, fixed in `857032ec`).** The gate never seeded
`sage.action_outcome`, which gets one row per verified action. Once seeded, the gate
caught 2 seq scans of 20,000 rows per steady phase. auto_explain on the gate's database
named the statement: the shadow scorer's read of recent actions hash-joined every
outcome for a 6-day window. That read, the scorer's queue read and three
trust-reconciler reads now fetch each verdict by primary key (a scalar subquery).
Measured: 2 seq scans (40,000 rows) dropped to 0.

**Small gate, 5 consecutive runs on PG17 (ag1), after the series:**

| run | result | offenders | trust median / max ms | sidecar CPU ms/cycle |
|---|---|---|---|---|
| 1 | PASS | 0 | 18.4 / 303.2 | 218.2 |
| 2 | PASS | 0 | 14.3 / 45.1 | 200.8 |
| 3 | PASS | 0 | 11.2 / 19.3 | 185.7 |
| 4 | PASS | 0 | 12.1 / 14.3 | 231.4 |
| 5 | PASS | 0 | 21.9 / 261.6 | 240.3 |

In those runs other endpoints' worst single call reached 670 ms (`/api/v1/value`). The
median absorbs that.

## 2. Declared budget for pg_sage itself (done)

`5b088b71`, `9b4fddfc`, plus the gate in `ec11cda6`.
- **Config `self_budget`** (YAML only, restart-bound):
  - `cpu_ms_per_cycle`: the sidecar process's CPU per collector cycle. Default 600 ms,
    which is 1% of a core at 60 s.
  - `db_time_ms_per_hour`: default 0, see product calls.
  - `blocks_per_hour`: default 18,000,000 shared blocks, i.e. 5,000 a second.
  - `storage_mb`: the sage schema's size. Default 10240.
  - 0 disables a resource. Each has a bound and is refused at load when out of range.
- **`internal/selfbudget`**:
  - `Check` flags a resource only when known usage is strictly above an enabled budget.
  - `FromCost` converts per-cycle DB time and blocks to per hour.
  - `CPUMeter` reads the process CPU through getrusage, or `GetProcessTimes` on Windows.
  - `Loops` tracks busy time per loop. The collector and analyzer loops report to it.
- **`selfcost`** keeps pg_sage's five costliest statements per window, with the tag
  stripped and the text cut to 160 characters.
- **Analyzer**: one `sage_self_budget` warning per database. It names every resource
  over budget with what it used, the top statements (DB time, calls, blocks) and the
  busiest loops.
  - While an enabled resource is unmeasured, the category stays unknown and an open
    finding is kept.
  - The finding is about pg_sage on purpose, so it redacts its own name from statement
    text. That keeps the self-monitoring filter from dropping it.
- **/metrics**:
  - `pg_sage_self_cpu_ms_per_cycle` (process level)
  - `pg_sage_self_db_time_ms_per_hour{database}`
  - `pg_sage_self_blocks_per_hour{database}`
  - `pg_sage_self_budget{resource}`
  - `pg_sage_self_budget_exceeded{database,resource}`
  - `pg_sage_self_loop_busy_seconds_total{loop}` and `pg_sage_self_loop_runs_total{loop}`
- **Perf gate G** (the queued follow-up): the sidecar process's CPU per collector cycle
  in the steady phase. The CPU used by API calls is measured and subtracted. Budget
  600 ms; measured 186-310 ms.
- **Mutation testing**: 35 mutations of the budget checks, meter, loops, rule, config
  bounds, gates E and G, pprof, the new indexes and the batched reads. All 35 were
  killed (one more mutation did not apply because its target line is in another file).

## 3. Authenticated pprof (done)

`ca340451`, `b1afd3fd`.
- `debug.pprof_enabled` defaults to false and needs a restart to change. It serves Go's
  profiler at `/api/v1/debug/pprof/` on the API mux, behind the session middleware.
- Access:
  - admins only; operators and viewers get 403
  - no session, or an MCP token, gets 401
  - when the setting is off the path does not exist (404)
  - never on the Prometheus listener
- Responses are `Cache-Control: no-store`.
- `profile` and `trace` accept `seconds=1-25` (default 10), which keeps them inside the
  30 s request deadline. Enabling the setting turns on light block and mutex sampling.
- **Security review**: `cmdline` is not served (404). pg_sage takes credentials as
  flags (`--pg-url`, `--pg-password`, `--encryption-key`).
- Documented in `docs/configuration.md` ("Profiling the sidecar").

## 4. History outside the monitored database (not built: product call)

**Verified current state.**
- Meta-db mode already moves the control plane to the metadata database: logins,
  sessions, MCP tokens, notifications, standing policy, Sage SRE, and the
  earned-trust ledger. Verified in `installAutonomy` (`rt.spec.ControlPool`) and in the
  SRE store (`d.control`).
- History stays in each monitored database (collector on `rt.spec.Pool`).
- `schema.Bootstrap` creates the full schema everywhere.

**Why not now.** Moving history is not a configuration switch:
- The history tables have no database column, so a shared history database would mix
  databases.
- Several reads join history with the monitored database's catalog and
  pg_stat_statements: explain_cache with pg_stat_statements, query_hints,
  table_contract with pg_class, and the query_store insert joins explain_cache.
- The action log, its outcomes, the decision ledger, verification and recommendations
  reference each other through foreign keys.
- About 15 readers of `sage.snapshots` alone, including verify and the forecaster,
  would each need rewiring. A partial move would silently feed empty history to the
  ones missed.

The storage cost the roadmap item targets is already bounded: delta snapshots (about
11x smaller), day partitions, `retention.snapshots_max_pct`, and now
`self_budget.storage_mb`.

**Delivered instead.** `docs/configuration.md` "Where pg_sage keeps its data" (placement
per mode, and why history stays). The CHANGELOG says history outside the monitored
database is not supported yet.

**Proposed design** (its own PR): a `history_dsn` for standalone mode first:
- a history pool threaded through the snapshot writer and every snapshot reader
- `database_name` added to snapshots before fleet sharing
- the explain_cache/query_store pair moved only together, with the catalog-join reads
  split in two
- a test that the monitored database then has no history rows

## 5. Change-only catalog storage (verified, gaps closed)

`72198e0c`. Research confirmed the delta store covers every catalog-like category the
collector persists:
- keyed lists: tables, indexes, sequences, foreign_keys, partitions, queries, io
- object document: config_data

`system`, `locks` and `replication` stay full, each for a stated reason. New
`snapstore.Coverage` records the decision. A collector test fails when a new category is
persisted without one. The stale comments that called io and config_data full-only are
corrected.

## Product calls

1. **Database time has one budget per breach.** `self_budget.db_time_ms_per_hour`
   defaults to 0, which means "inherit": `analyzer.self_cost_budget_ms` (per cycle,
   `sage_self_cost`) stays the database-time budget, so one breach never raises two
   findings. An explicit value is checked by `sage_self_budget`. /metrics exports the
   effective per-hour budget either way.
2. **CPU is a process budget.** In fleet mode each database's finding reports the same
   process CPU. Dividing the budget among databases would misstate where the CPU went.
3. **Loop attribution is wall-clock busy time** for the collector and analyzer. Go does
   not attribute CPU to goroutines. For real CPU attribution, use pprof (section 3).
4. **self_budget and debug are YAML-only, restart-bound.** They are not API overrides
   and not in the Settings UI; `web/src/generated/config_meta.json` was not
   regenerated, so the web dist is untouched.
5. **pprof never serves cmdline**, and it is opt-in, admin-only and session-bound.
6. **History relocation deferred** (section 4). This is the main open item of the phase.
7. **Gate budgets**: endpoint stays at 1000 ms (median of 5); sidecar CPU 600 ms per 15 s
   cycle, about 2x the measured value.

## Test Results

**Command:** `go test -p 2 -cover -count=1 ./...` (sidecar, golang:1.25 `--cpus=2`, PG17
ag1 :55471), then the failing packages again after the fixes; e2e
`go test -tags=e2e -count=1 -timeout 900s ./e2e/`; touched packages with `-race` (PG17)
and on PG14 :55414 and PG18 :55418; small perf gate 5x
(`-tags=perfgate`, `PG_SAGE_PERF_SCALE=small`).

**Total (full suite, PG17):** 13,283 passed, 10 failed, 22 skipped. Of the 10:
- 6 were caught by this branch and fixed in `9b4fddfc`: 5 `cmd/gen_config_meta` tests
  (doc tags over 200 characters) and `internal/store`
  TestConfigConsistency_AllowedKeysMatchStruct (new keys had no override decision).
  Packages rerun after the fix: gen_config_meta ok 86.4%, store ok 76.1%, config ok
  91.4%.
- 3 are load flakes on the shared host. Each passes when rerun alone, and none touches
  this change:
  - `cmd/pg_sage_sidecar` TestMetaStartupRegistersOnlyEnabledDatabases: pool close hit
    its 30 s deadline in cleanup.
  - `internal/sre/probes` TestRunner_OKResultIsTypedAndStamped: the probe hit its 500 ms
    deadline.
  - `internal/earned` TestReconcileWithoutNewEvidenceDoesNotRewriteLedgerState: a
    statistics flush landed after the baseline read. The package passed twice in full
    on its own.
- 1 is an environment failure: `internal/mcp` TestToolReferenceDocsMatchSchemas, from
  the CRLF checkout of `docs/mcp.md`. It is pre-existing and passes on LF (CI).

**e2e (PG17):** 78 passed, 0 failed, 13 skipped (all live-LLM tests: `SAGE_LLM_API_KEY`
unset by rule).
**-race, touched packages (PG17):** 4,225 passed, 0 data races, 2 failed. Both pass when
rerun alone with `-race`:
- TestMetaReconcileConcurrentPassesPublishOneRuntime: dial timeout to
  `host.docker.internal`
- TestSnapshotAPI_OrphanDeltaReadsNull: passed 3 of 3 runs alone
**PG14, touched packages:** 4,222 passed, 0 failed, 5 skipped (version-gated, below).
**PG18, touched packages:** 4,227 passed, 0 failed, 0 skipped.
**Perf gate, small, 5 consecutive runs:** 5 PASS, 0 offenders (table in section 1).
**Lint:** golangci-lint 0 issues (default and `--build-tags perfgate`).

**Coverage (touched packages, full PG17 run):**

| package | coverage |
|---|---|
| internal/selfbudget | 98.9% |
| internal/selfcost | 97.1% |
| internal/snapstore | 98.6% |
| internal/analyzer | 91.6% |
| internal/testsupport/perfgate | 91.5% |
| internal/config | 91.4% |
| internal/earned | 90.1% |
| internal/collector | 88.7% |
| internal/shadow | 87.7% |
| cmd/gen_config_meta | 86.4% (after fix) |
| internal/schema | 84.2% |
| cmd/pg_sage_sidecar | 80.3% |
| internal/api | 79.4% |
| internal/store | 76.1% (after fix) |
| internal/testdb (utility) | 73.1% |

### Skipped Tests (must be zero or justified)
- PG17 full suite, 22 skips. All are environment-gated and none is in a package this
  branch touches:
  - live clouds: AWS RDS, Cloud SQL, Lakebase, Azure, agent-DB gauntlets (8)
  - live LLM (3)
  - HA standby, restartable server, causal standby, PgBouncer (6)
  - Windows path test on Linux (1)
  - RCA child-process helper (1)
  - plan-fixture regeneration (1)
  - PGIncidentBench and live arm, run in their own CI steps (2)
- PG14, 5 skips (version-gated):
  - EXPLAIN (GENERIC_PLAN) needs PG16+: earned
    TestReconcilerFactReadsNeverScanOutcomes, api
    TestActionLogCappedCountWalksTheTimeIndex
  - pg_stat_io needs PG16+: 2 collector tests
  - pg_stat_force_next_flush needs PG15+: 1 analyzer test
- e2e, 13 live-LLM skips.

### Failures (if any)
- Fixed: gen_config_meta doc length; store override-registry consistency (both
  `9b4fddfc`).
- Not caused by this change: the 3 load flakes, 1 CRLF environment failure and 2
  `-race` load flakes listed above.

### Coverage Gaps (packages below threshold)
- No touched package is below its threshold.
- Untouched packages below 70% in the full run:
  - test fixtures and tools: internal/testsupport/snapfixture 0%,
    cmd/reset_admin_for_test 50%, cmd/create_admin 51.5%, cmd/sigstore_trusted_root
    56.1%, internal/testsupport/pgssepoch 58.3%
  - sre-bench 65.7%, unchanged

### Bugs Found This Session
See "Bugs found" below (2 performance, 1 security in new code, 1 test environment).

### Manual Checks Remaining
- MANUAL: the large-scale nightly perf gate (5,000 tables, 150k rows) was not run here.
- MANUAL: the Windows `ProcessCPU` path is compiled by lint only; it is not executed by
  the Linux test containers.

## Post-test audit

- **Untested inputs.**
  - `writeSelfBudgetFromFleet` (fleet wiring of the metrics) has no test of its own; the
    writer it feeds is tested.
  - The Windows `ProcessCPU` is only compiled on Windows (lint); tests run on Linux.
  - The shadow summary keeps a seq scan of `sage.shadow_decision` when the table has many
    unvacuumed updates and few rows: 6 scans of 2,000 rows per steady phase in the gate.
    That is below gate A's 5,000-row threshold, and the plan test on a vacuumed table
    uses the index.
- **Assertions that would pass if broken.** None found. The mutation run killed every
  applied mutation, including the removal of each new index and of the batching.
- **Fakes that hide failures.**
  - The analyzer tests use a scripted process-CPU reader; the real reader is tested
    against the process in `selfbudget`.
  - The trust view's limiter uses fake budget and HA sources (as before), so the 4 round
    trips they cost in production are not counted by the round-trip test.
- **Added after the audit.** `TestCheckSelfBudget_NamesTheBusiestLoop`: top loops come
  from the process tracker between two analyzer cycles.

## Bugs found

1. [PERF] The Trust view made 17 sequential round trips and read the safety record twice
   per view (`internal/earned/trust_view.go`). Fixed.
2. [PERF] The shadow scorer's action read hash-joined all of `sage.action_outcome`
   (`internal/shadow/scorer_facts.go`). Fixed. The perf gate had been blind to it
   because it never seeded that table.
3. [SECURITY, new code] pprof `cmdline` would have exposed `--pg-url` / `--encryption-key`
   to admins. Fixed before merge.
4. [TEST ENV] `TestToolReferenceDocsMatchSchemas` (internal/mcp) fails on a Windows CRLF
   checkout because `docs/mcp.md` has CRLF line endings and the marker includes `\n`. It
   is pre-existing and unrelated to this change; it passes on Linux CI.

## What is left

- History outside the monitored database (section 4 design).
- Per-component CPU attribution beyond the two tracked loops (use pprof today).
- The large-scale gate (nightly) was not run here. With the summary's covering index,
  shadow_decision at 15,000 rows is expected to use the index.
