# CI test isolation: cross-package interference (2026-10-03)

Branch `claude/ci-test-isolation`, from master `f8c8f1a8`.

CI runs every Go package in parallel against one PostgreSQL server. `internal/testdb`
gives each package its own database, but server-wide state is shared: `pg_stat_activity`,
`pg_stat_statements`, the statistics machinery (PG14's asynchronous collector, PG15+'s
deferred flush), autovacuum and the load. This branch fixes three fresh CI flakes at the
root, sweeps the suite for the same class of hazard, and checks the result under load.

## 1. The three failures

### 1.1 `TestSystemStatsCountApplicationSessionsOnly` (collector, PG18)

CI: `active_backends = 3, want 2 (application waiter and sleeper only)`.

**Root cause: a product imprecision.** `active_backends` and `idle_in_transaction` in the
system snapshot filtered by database and left pg_sage's own sessions out. They did not
filter by backend type, so every other backend carrying the database's name was counted.
That includes autovacuum workers, logical walsenders and parallel workers.

During a collector run an autovacuum worker was observed on the package database:
`autovacuum: VACUUM ANALYZE pg_catalog.pg_attribute`, state `active`. The package
creates thousands of catalog rows (one test builds 5,000 indexes).

**Reproduced.** A table whose autovacuum crawls (cost limit 1, delay 100 ms) was added to
the collector database, so a real autovacuum worker stays active there. The old code then
failed 35 of 40 runs with the exact CI message.

**Fixed (product).** Both counts are now limited to `backend_type = 'client backend'`, as
`connection_states` already was. `total_backends` still counts every backend, because it
measures capacity.

- The same gap existed in the executor's `before_state.active_backends`, which is evidence
  recorded with every action. It is fixed the same way.
- New test `TestSystemStatsCountClientSessionsNotTheirWorkers` uses a parallel scan as a
  deterministic stand-in for these backends: one application session with two parallel
  workers beside it. Without the fix the count is 3; with it, 1.
- New test `TestBeforeStateActiveBackendsAreClientSessions` does the same for the executor.
- Both tests use the new `selfload.StartParallel`.
- CHANGELOG: `## Unreleased` / `### Fixed`.

**Hardened (test support).** `selfload` now keeps one admin session for the life of the
workload and, at cleanup, makes sure every session it started is gone.

Under load, the cleanup check found two workload sessions still running 15 s after their
clients closed. The cause: when a `pg_sleep`'s cancel request is lost, the backend sleeps
on, because a sleeping backend does not notice its client is gone. The next test in the
package then counted it.

Cleanup now waits 2 s, terminates whatever is left (only the workload's own pids) and
waits again. Because one long-lived admin session does all the state reads, no helper
connection opens or closes between a test's two reads. Before this, one could, and it
moved connection churn by one.

### 1.2 `TestRealizedValueReadsCreditedActionsOnly` (value, PG14)

CI: `realized value read {Seq:0 SeqRead:0 Index:3 IndexFetch:5} ... want ... at most 3 rows
fetched`.

**Root cause: the fixture depended on VACUUM marking every page all-visible.** The read is
the expected index-only scan. It fetches heap rows only for pages whose visibility-map bit
is unset.

VACUUM cannot set those bits while any snapshot of the same database predates the rows.
Snapshots in other databases do not matter here; that was measured, not assumed. In CI,
the likely holder in the test's own fresh database is an autovacuum ANALYZE: it takes a
snapshot, and an ANALYZE is not exempt from the horizon the way a VACUUM is.

**Reproduced exactly.** Holding a REPEATABLE READ snapshot in the same database during the
fixture's VACUUM gives `{Index:3 IndexFetch:5}` and `relallvisible 0/213`.

Under the stress harness's stand-in for an autovacuum ANALYZE (`PAT`: a snapshot held
0.3 s in the test's database every 0.5 s), the old fixture failed 6 of 20 runs with the
CI signature. The new fixture failed 0 of 20.

**Fixed (test).** New helper `testdb.VacuumAllVisible` runs `VACUUM (ANALYZE)` until
`relallvisible` reaches `relpages`, with a 60 s bound. If it times out, it names the
sessions and slots holding the horizon. The assertion is unchanged: no seq scan, and at
most the credited rows fetched.

### 1.3 `TestFleetReloadRemovalLetsInFlightActionFinish` (cmd, PG18)

CI: `removal completed before the in-flight action finished`, with no drain timeout logged.

**Root cause: a measurement race in the test.** It compared `time.Now()` stamped by two
goroutines:

- the action's goroutine, after `Apply` returned;
- the reload's goroutine, after the reload returned.

The drain completes when `Apply` releases its DDL slot, which happens inside `Apply`,
before it returns. The reload's goroutine could therefore stamp its time first, even
though the product waited correctly.

**Reproduced.** The old test with the action's goroutine delayed 200 ms after `Apply`
returned (the product unchanged) failed 5 of 5 runs with the CI message.

**Fixed (test).**

- The action stays in flight until the test sees the drain parking new actions. That makes
  the drain provably start while the action runs; before, this was left to a 1.5 s
  `pg_sleep`.
- The action then runs one statement on the pool and sets a flag before releasing its
  slot.
- The reload's goroutine reads that flag the moment the reload returns.
- No clocks are compared.

**Mutation checks.** Each of these fails the new test (the drain is never seen parking
within 30 s):

- `Quiesce` that takes only the free slots and does not wait;
- a removal that does not drain at all.

## 2. Sweep: the same hazard class, fixed proactively

Two read-only sweeps covered the whole suite, including build-tagged files, `e2e`,
`sre-bench` and `cmd`. Every finding below was read and confirmed before it was fixed.

### (a) Session and activity counts

| Test | Hazard | Fix |
|---|---|---|
| `executor` `TestBeforeStateActiveBackendsAreApplicationSessions` | exact `active_backends == 2`; product counted autovacuum workers | product fix (§1.1) plus a parallel-worker test |
| `collector` `TestConnectionChurnIgnoresPgSageSessions` | `selfload` opened and closed a non-pg_sage admin connection between the two reads | `selfload` keeps one admin session |
| `collector` `TestConnectionStatesCountApplicationSessionsOnly`, `sre/probes` `TestLWLockWaitsProbeCountsApplicationBackendsOnly` | backends of an earlier test outliving their clients | `selfload` cleanup ends every session it started |
| `collector` `TestCollectLocksAndActivity_ScopedToCurrentDatabase` | snapshot count compared with a separate read | local count read before and after; an unstable window is measured again (up to 5) |

### (b) Statistics read right after writes

| Test | Hazard | Fix |
|---|---|---|
| `perfgate` `TestReadTableStatsCountsPartitionsNotTheParent` (CI PG17: `2430 live rows ... want 2160`) | VACUUM sets `n_live_tup`; a seeding session's still-pending inserts are added afterwards and count twice | `AnalyzeSage` closes the pool's idle sessions and waits for their exit, since exiting flushes, before it vacuums. New deterministic test `TestAnalyzeSageCountsPendingWritesOnce`: 2000 vs 1000 without the fix, on PG14 and PG17 |
| `collector` golden tables/indexes, `tuner` `TestLoadStaleStatsCache_MatchesLegacyQuery` | PG14: `pg_stat_force_next_flush()` does not exist and a sleep flushes nothing, so the fixture session's last counts arrived between the two compared reads | new `testdb.FlushStats` (below) |
| `analyzer` `TestFindingRefreshIsAHeapOnlyUpdate`, `rca` `TestPersist_UnchangedCausalChainWritesNoToast` | table counters from every session; pending writes in the shared pool's idle sessions are flushed by PG15+'s idle timer inside the window | `testdb.FlushIdleSessions(shared)` before the window |
| `partition` `TestConvert_ExclusiveWindowReadsNoHeap` | block reads from every backend: autovacuum of the 60,000-row fixture, or the fill's deferred flush, inside the window | fixture `autovacuum_enabled = off`; `FlushIdleSessions` before conversion |
| `perfgate` `TestStatementsCaptureAndExplain` | reset then `Calls == 1`; another package's `pg_stat_statements_reset()` or an eviction erased the entry | window repeated (up to 3) only when the epoch moved after its own reset |
| `sre/probes` `TestCatalog_TempFileActivityReadsThisDatabase` | probe's sample required ≥ a later `pg_stat_database` read | counters read first (they only grow) |
| `e2e` CHECK-B07b/B09b | `last_vacuum`/`last_analyze` read once (PG14's collector is async) | polled up to 15 s |

`testdb.FlushStats(conn)` works per PostgreSQL version:

- PG15+: calls `pg_stat_force_next_flush()`, which flushes as the statement ends.
- PG14: waits out the 500 ms report interval, then scans a freshly created sentinel table.
  The sentinel's counts are the report's last entry, so once the collector shows that
  scan, everything reported before it has been applied.

### (c) Autovacuum on fixtures

`value` (§1.2) and `partition` are covered above. The `migration` reltuples fakes were
reviewed and are safe: the tables are empty, so no autoanalyze can trigger.

### (d) Wall-clock budgets and goroutine ordering

| Test | Was | Now |
|---|---|---|
| `api` `TestEventsHandlerStreamsSubscribedEvent` | cancelled 30 ms after publishing. The handler's `select` picks randomly between a done context and a ready event, so the frame could be dropped | waits (≤10 s) for the frame through a mutex-guarded recorder |
| `sre/probes` `TestRunner_QueueWaitIsBounded` | 60 ms pause hoping the busy probe takes the only slot; 200 ms budget | the test holds the slot itself and frees it after 5 s; 2 s budget |
| `sre/probes` `TestRunner_StatementTimeout` | `pg_sleep(2)` vs 1.5 s budget | `pg_sleep(30)`, 10 s budget |
| `executor` `TestApplyLockCeiling*`, `autonomy` `TestRetentionBatchUsesPipelineLockTimeout` | fixed budgets (2 s, 1.5 s) that also absorbed the path's overhead | pass under the old bound or ≥1.5 s (1 s) under a same-path control run; ceiling control floor 2.5 s (see §4) |
| `schema` migration re-run tests | 900 ms; the held lock plus `lock_timeout` 1 s already rules out a wait | 10 s (hang guard only) |
| `schema` bootstrap lock tests | 2 s contexts for uncontended acquisitions with a dial; 2 s waiter poll (250 ms/query); 1 s for 150 ms timeouts | 10 s; 15 s (2 s/query); 5 s. A contender ignoring its timeout waits forever |
| `fleet` `TestWave2RemoveInternalBound*` | 250 ms for a 30 ms bound | 5 s (the production bound is 30 s) |
| `sre/slo` `TestPromClient_TimeoutAndUnreachable` | 250 ms; server answered at 300 ms | server answers at 30 s (or when the client hangs up); 3 s budget, below the 5 s default timeout a client ignoring its setting would use |
| `llm` `TestWave4ReconfigureAppliesNewRequestTimeout` | 1.3 s budget; server answered at 1.5 s | server answers at 30 s (or on hang-up); 4 s budget, below the replaced 5 s timeout |
| `optimizer` `TestHypoPGSizeDoesNotAcquireAnotherSession` | 2 s deadline also bounded the real validation | 10 s (it only has to end a blocked second checkout) |
| `executor` `TestApplyLockCeilingCapsOperatorAnalyze` | compared two zero durations and failed when its subtests skipped | skips with them |

Every budget stays below what the bug it guards against would take.

### Reviewed, left as is

| Test | Why it stays |
|---|---|
| `tuner`/`briefing` server-wide session counts | already sample the minimum over 8 reads with a loose bound |
| `sre-bench` `lockWaiters` | per-database count; autovacuum takes its locks conditionally and does not wait |
| `explain` `analyze_guard` 3 s per EXPLAIN | below the 5 s statement timeout it guards |
| plan-shape tests that run an explicit ANALYZE | autoanalyze of unchanged data gives the same statistics |
| e2e `pg_stat_statements_reset()` | the e2e step runs alone in CI |
| `executor` `custodian_incident` freeze ages | below `autovacuum_freeze_max_age` |

## 3. Mutation checks

Each converted or new test was run against deliberately broken product (or helper) code
and failed. Every break was then reverted.

| Test | Break | Result |
|---|---|---|
| `TestSystemStatsCountClientSessionsNotTheirWorkers` | no `backend_type` filter | `active_backends = 3, want 1` |
| `TestBeforeStateActiveBackendsAreClientSessions` | no `backend_type` filter | `= 3, want 1` |
| `TestRealizedValueReadsCreditedActionsOnly` | predicate the partial index cannot use | `{Seq:1 SeqRead:20003}` |
| `TestVacuumAllVisible*` | helper returns after one VACUUM | 2 tests fail |
| `TestFleetReloadRemovalLetsInFlightActionFinish` | `Quiesce` does not wait; removal does not drain | both fail |
| `TestAnalyzeSageCountsPendingWritesOnce` | no session flush | 2000 vs 1000 (PG14, PG17) |
| `TestFlushStats*` / `TestFlushIdleSessions*` | no-op helper | `n_tup_ins = 0` |
| `TestCollectLocksAndActivity_ScopedToCurrentDatabase` | idle-in-transaction cluster-wide | `11, want 0` |
| `TestConvert_ExclusiveWindowReadsNoHeap` | skip VALIDATE CONSTRAINT | fails, caught first by the product's own guard |
| `TestStatementsCaptureAndExplain` | harness statements not filtered | fails |
| `TestFindingRefreshIsAHeapOnlyUpdate` | `last_seen` indexed (the original bug) | `5 updates, 0 HOT` |
| `TestEventsHandlerStreamsSubscribedEvent` | frame type renamed | `stream missing event: findings` |
| `TestRunner_QueueWaitIsBounded` | limiter ignores ctx | waited 5 s |
| `TestApplyLockCeilingCapsCycleAnalyze` | ceiling ignored | 4.1 s vs control 4.1 s |
| `TestRetentionBatchUsesPipelineLockTimeout` | pipeline timeout ignored | 2.0 s vs control 2.0 s |
| `TestAdvisoryLock_TimesOutWhileAnotherSessionOwnsLock` | timeout ×40 | 6 s |
| `TestWave2RemoveInternalBoundClosesPoolWithStuckWorker` | internal bound ignored | 30 s |
| `TestPromClient_TimeoutAndUnreachable` | configured timeout ignored | 5 s |
| `TestUnusedIndex_ResetBetweenSamplesNeverYieldsEarlyDrop` | reset epoch ignored | early drop finding at reset+48h |
| `selfload` `TestAwaitGoneTerminatesALingeringSession` | cleanup does not terminate | session survived |
| `TestWave4ReconfigureAppliesNewRequestTimeout` | reconfigured timeout ignored | 5 s |

Not re-run against a product break:

- `TestCatalog_TempFileActivityReadsThisDatabase`: the change only reorders the reads, and
  the assertion's power depends on how many spills the package's earlier tests left.
- `TestHypoPGSizeDoesNotAcquireAnotherSession`: the deadline only ends a blocked second
  checkout.

## 4. Stress evidence

Harness: `scripts/ci/stress-load.sh`, committed. Base load on the server:

- 10 sessions active in `pg_sleep`;
- 10 sessions idle in a REPEATABLE READ snapshot;
- a `pg_stat_statements_reset()` every 3 s;
- one new connection a second.

Two optional modes: `PAT`, a snapshot holder in matching databases (the autovacuum ANALYZE
stand-in), and `AVPAT`, a crawling autovacuum worker in matching databases.

Converted tests, old (master) vs new (this branch), `-race -count=5`, under base load:

| Run | PG14 | PG18 |
|---|---|---|
| old (master `f8c8f1a8`) | 18/18 packages ok, 0 failures | 18/18 ok, 0 failures |
| new, first clean run | 1 failure: `TestApplyLockCeilingCapsCycleAnalyze`, control took 2.74 s | 1 failure: `TestApplyLockCeilingCapsCustodian`, control took 2.79 s |
| new, final (after `8884ac25`) | 18/18 ok, 0 failures | 18/18 ok, 0 failures |

How to read this table:

- Generic load alone rarely triggers the three CI root causes. Neither master nor this
  branch failed them in 5 iterations. That is why the targeted provocations below exist.
- Both new-code failures came from the 3 s control floor that master also has. The cause
  was the VM's realtime clock, not load (§7). Master passed it by chance in its run.
- A first attempt that ran old and new at the same time on one server is discarded. Two
  `go test` containers produced the same fixture database name (package plus PID, and PIDs
  repeat across containers), and the overloaded VM's connection proxy timed out dials.

Targeted provocations, which reproduce the CI root causes:

| Case | Server | Old (master) | New |
|---|---|---|---|
| collector session tests, crawling autovacuum (`AVPAT`), `-count=40` | PG17 | 35/40 `TestSystemStatsCount...` fail (`active_backends = 3`) | 0 fail in 239 runs, 1 skip (a ping timed out on the overloaded VM); an earlier non-verbose run had 1 failure in 240 |
| value, snapshot holder (`PAT`), `-count=20` | PG14 | 6/20 fail (`{Index:3 IndexFetch:5}`) | 0/20 |
| fleet, action goroutine late by 200 ms | PG18 | 5/5 fail | not applicable: the new test does not compare clocks |
| perfgate pending-write double count | PG14, PG17 | 3/3 fail (2000 vs 1000) | 0/3 |

Full suite (`go test -race -tags=integration -p 2 -count=1 ./...`):

| Run | PG14 | PG18 |
|---|---|---|
| under the base stress load (tree with every fix up to `8884ac25`) | 89 packages ok, 0 failures | 88 ok, 1 failure: `sre-bench` `TestReplayCorpus` (see below) |
| no extra load, but three suites at once on the VM (PG14, PG18, PG17) | 86 ok; 3 failures, all fixed since (see below) | 88 ok; 2 failures (see below) |

Failures in these runs and what became of them:

- **`analyzer` `TestUnusedIndex_ResetBetweenSamplesNeverYieldsEarlyDrop`** (PG14). It
  waited for `idx_scan > 0`, which one PG14 report can satisfy while the session still
  holds the rest. Those scans then arrived after `pg_stat_reset()`.
  - Fixed in `36e5b48a`: `FlushStats`, then all 5 scans must be visible.
  - Mutation (reset epoch ignored) fails it.
- **`sre/probes` `TestCatalog_WALDirectoryIsPrivilegedAndTyped`** (PG14): the probe
  returned `deadline_exceeded`, having run out of its 500 ms budget. `catalogRun` now
  repeats a budget-exhausted run (`d12bd207`).
- **`sre/probes` `TestRunner_OneProbePerDatabase`** (PG14): a 250 ms sleep hit the 400 ms
  statement timeout. The sleep probe now gets the 500 ms maximum (`d12bd207`).
- **`executor` `TestApplyLockCeilingCapsCycleAnalyze`** (PG18): the control ended after
  2.43 s. This is the VM clock step from §7. The floor is now 2 s (`d12bd207`).
- **`executor` `TestReadUnusedEvidence_RealStatistics`** (PG18): `dial error: timeout` to
  `host.docker.internal`. Docker Desktop's connection proxy saturates with three suites at
  once. This is the environment, not the test.
- **`sre-bench` `TestReplayCorpus`** (PG18, stressed): `conclude: ... violates check
  constraint "sre_investigations_active_ms_check"`. The active-time charge
  (`clock_timestamp() - lease_started_at`) goes negative when the realtime clock steps
  backwards. This is a real product edge case: NTP can step a production server's clock
  too. It is out of scope here and was handed off as a separate task, with a test-first
  fix: `GREATEST(0, ...)`.

## 5. Product decision

`active_backends` and `idle_in_transaction` are session counts, so they count client
sessions only.

- Autovacuum workers, walsenders and parallel workers do not take `max_connections`
  slots and are not the application's sessions.
- Counting them made a busy autovacuum or one parallel query look like application load
  to the connection advisor, the forecaster, lock analysis and the before-state evidence.
- Capacity (`total_backends`) still counts every backend.
- The circuit breaker's cluster-wide load ratio is unchanged.

## 6. Test Results

**Command:** `go test -count=1 -cover -p 2 -json ./...` on PG17 (`pgsage-ag3`, tree
`04e1c2aa`), plus `go test -tags=e2e -count=1 -timeout 900s ./e2e/` and golangci-lint
v2.11.4.

**Total:** 11,654 passed, 1 failed, 21 skipped. e2e: ok (223 s). Lint: 0 issues, also with
`--build-tags=e2e,integration,perfgate`.

**Coverage:** packages touched on this branch.

| Package | Coverage |
|---|---|
| `cmd/pg_sage_sidecar` | 81.4% |
| `internal/analyzer` | 91.2% |
| `internal/api` | 78.7% |
| `internal/autonomy` | 83.8% |
| `internal/collector` | 88.9% |
| `internal/executor` | 85.6% |
| `internal/fleet` | 83.7% |
| `internal/llm` | 93.7% |
| `internal/optimizer` | 88.2% |
| `internal/partition` | 83.5% |
| `internal/rca` | 95.6% |
| `internal/schema` | 83.1% |
| `internal/sre/probes` | 93.5% |
| `internal/sre/slo` | 88.7% |
| `internal/testdb` | 72.0% |
| `internal/testsupport/perfgate` | 91.2% |
| `internal/testsupport/selfload` | 82.1% (was 0%: no tests of its own) |
| `internal/tuner` | 85.7% |
| `internal/value` | 94.9% |

### Skipped tests (21, all environment-gated, none new)

| Package | Count | Reason |
|---|---|---|
| `agentdb` | 6 | live AWS/GCP/Databricks flags |
| `azure` | 2 | live Azure |
| `llm` | 2 | `PG_SAGE_LIVE_LLM` |
| `rca` | 2 | live Gemini; child-process helper |
| `sre/causal` | 2 | no restartable server or standby |
| `sre/pooler` | 3 | no PgBouncer |
| `ha` | 1 | no standby |
| `logwatch` | 1 | Windows path test on Linux |
| `tuner` | 1 | fixture regeneration flag |
| `sre-bench` | 1 | `TestPGIncidentBench` runs in its own CI step |

### Failures

`cmd/pg_sage_sidecar` `TestMetaReconcileConcurrentPassesPublishOneRuntime`:
`list databases: listing databases: context deadline exceeded`. This was a trivial query
with a 10 s timeout, while two other `-race` suites ran on the same Docker VM, whose
connection proxy was timing out dials (the same run produced dial timeouts elsewhere). The
test was not touched on this branch.

Rerun alone with `-race -count=10 -run TestMetaReconcile`: ok, all 10 passes.

The packages changed after `04e1c2aa` (`analyzer`, `sre/probes`, `executor`, `selfload`,
`testdb`, `collector`) were re-run with `-race` on PG14 and PG18 at the final tree: all ok.
One `testdb` run on PG14 first hit a 30 s dial timeout while another `-race` suite was
running; its re-run passed.

### Coverage gaps

All touched packages meet their thresholds: business packages are 78.7–95.6% against a
70% floor, and the test-support packages `testdb` (72.0%), `selfload` (82.1%) and
`perfgate` (91.2%) are above their 50% floor.

### Bugs found

1. **[BUG]** `collector/queries_sessions.go`: `active_backends` and `idle_in_transaction`
   counted autovacuum workers, walsenders and parallel workers. Fixed.
2. **[BUG]** `executor/action_record.go`: `before_state.active_backends` had the same gap.
   Fixed.
3. **[BUG, out of scope]** `sre/postgres_lease.go` `chargeSQL`: the charge goes negative
   when the clock steps back, which breaks the CHECK on `active_ms`. Handed off as a
   separate task.
4. **[TEST BUGS]** 26 tests that depended on shared server state or timing; see §1 and §2.

### Post-test audit

- **Assertions that would pass with the feature broken.** Every converted test was run
  against a product break (§3).
  - `TestCatalog_TempFileActivityReadsThisDatabase` stays weak: a probe returning
    cluster-wide sums would pass. That is pre-existing; this branch only reordered the
    reads.
- **Inputs left untested.** The helpers' PG14 paths (sentinel flush) are covered on PG14.
  `VacuumAllVisible` against a replication-slot holder is not tested; it names slots in its
  error but no fixture creates one.
- **Fakes that hide failures.** None were added. Every new test runs against real
  PostgreSQL (parallel workers, snapshot holders, pending statistics).
- **Tests-first.** Tests were committed before the product fixes for the collector,
  executor, perfgate and `testdb` helpers.
  - `FlushIdleSessions` and the `selfload` tests were committed together with their code;
    they were extractions and coverage for existing code.

## 7. Incidents and notes

- **I crash-restarted the shared matrix servers three times** while developing the
  harness: PG14 at 16:41 and 16:43, PG18 at 16:43.
  - The first harness ran its client loops inside the server containers with
    `docker exec`. There the postmaster is PID 1 and reaps orphaned clients. Killing those
    clients made it log `untracked child process ... terminated by signal 15` and
    reinitialize, which dropped every connection on the server at that moment.
  - Any other agent's run on those servers at those minutes may have failed spuriously.
  - The committed harness runs from its own client container and never signals processes
    inside a server container.
- **Docker Desktop's VM realtime clock jumps about 1.4 s forward every ~30 s** (measured
  against `CLOCK_BOOTTIME`). PostgreSQL times `lock_timeout` on the realtime clock, so
  locally a 4 s lock wait can end after about 2.7 s of monotonic time. The lock-ceiling
  bounds now tolerate this. GitHub runners are not expected to show it.
- **The local matrix servers keep `pg_stat_statements.max` at 5000**, while CI uses 50000.
  I did not restart shared servers to change it.
- **Two `go test` containers on one server can collide on fixture database names**, since
  the name is package plus PID and PIDs repeat across containers. This caused a false
  failure in my first evidence run. It cannot happen in CI (one server per job), so the
  evidence runs were done one at a time per server.
- **Leftover fixture databases.** Killed runs leave `pgsage_*` databases behind; several
  exist on the matrix servers. I dropped only those I could attribute to my helper tests
  (`pgsage_x_visibility_*`) and left the rest.
- **Merged with master after v1.8.4 was cut.** The changelog bullet stays under a new
  `## Unreleased`. After the merge, `go vet ./...`, lint and the touched packages
  (`perfgate`, `schema`, `collector`, `executor`, `testdb`, `selfload`, `value`,
  `cmd/pg_sage_sidecar`) were re-run on PG17: all ok.
- **Not done:** CI itself was not re-run on this branch before the PR. The PR's own run is
  the first CI check.
