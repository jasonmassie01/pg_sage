# Probe deadlines and flaky bench faults (2026-10-04)

Branch `claude/fix-probe-deadlines` (from v1.10.0). Problem 1: cheap catalog probes and
collector reads on lifeos failing with `deadline_exceeded` / SQLSTATE 57014 while the
database was idle. Problem 2: two PGIncidentBench scenarios that failed the v1.8.5 and
v1.10.0 tag builds. Plus the coordinator's audit of pg_stat_statements text reads.

## Root causes

### Problem 1: probe deadlines on lifeos

Evidence (read-only: sidecar logs, `pg_stat_activity`, `pg_stat_statements`):

- lifeos sidecar logs: 1.9.0 `xid_runway` deadline_exceeded at 11:47:17 and 18:12:37,
  collector `indexes` 57014 at 14:21:31; 1.10.0 `checkpoint_activity` 18:14:58,
  `wraparound_tables` 18:18:53, collector `queries` 57014 18:19:52. None at the startup
  burst instant (18:13:35) and nothing else logged around them.
- `deadline_exceeded` is the **client** deadline (statement timeout + 1 s = 1.5 s); a
  server-side timeout would be `statement_timeout`. The server ran xid_runway in 0.1 ms.
- That 1.5 s client deadline started **before the probe had a connection**: `pool.BeginTx`
  (pool acquire) ran under it, and the first probe's `SHOW server_version_num` ran on
  `pool.QueryRow` under a 500 ms deadline including acquire. lifeos runs pg_sage standalone
  with `max_connections: 3` for the whole sidecar (~20 loops); all 3 connections were open.
- Measured with the new runner against lifeos (read-only, one connection, 30 runs each):
  execution p50 10-16 ms but max 293-380 ms for queries the server runs in 0.1-20 ms, i.e.
  ~5 round trips through `host.docker.internal` (Docker Desktop's port proxy) stalling by
  hundreds of ms under host load. Pool acquire p50 3 us.
- Reproduced in the bench on the old code: `inject: runway sample 4: xid_runway: probe
  xid_runway error: deadline_exceeded` (PG17, disk_wal_runway, repeat 7 of 10).
- Collector 57014s are server-side (500 ms `safety.query_timeout_ms`): the `queries` read
  aggregates every entry with its text (see the audit below), the `indexes` read pages a
  35k-index catalog; both normally take 10-90 ms on lifeos and hit 500 ms once in hours.

Root causes: (1) pool wait and queue wait spent the execution budget; (2) host/network
stalls on the multi-round-trip execution with no retry for an idempotent read;
(3) for the collector, rare server-side spikes with no retry.

### Problem 2(i): disk-slow-consumer-fill "does not retain the written WAL"

- The logical slot retains exactly the WAL written after its creation: 18 x 1 MiB
  messages plus record/page headers. Measured margin over the 18 MiB check: 67-155 KB in
  14 runs. Physical slots (the inactive-fill scenario) retain from the last checkpoint's
  redo, so they have megabytes of margin, which is why only the logical one flaked.
- The wraparound programs (same CI shard, run first) burned XIDs with
  `set_config('synchronous_commit', 'off', false)`: **session-level** on a pooled
  connection. A later `emitWAL` on that connection committed asynchronously, so
  `pg_current_wal_lsn()` (the write position the manifest reads) lagged the inserted WAL:
  2.6 MB unwritten right after 6 MiB on PG17 (0 after 400 ms when the walwriter ran).
- Reproduced exactly on the old code with a slow walwriter (`wal_writer_delay = 10s` on a
  throwaway PG17, families wraparound_runway + disk_wal_runway, 3 repeats): 2 of 6
  slow-consumer runs failed with the CI message, in both arms. CI's walwriter is slow under
  the bench's WAL volume; locally it rarely is (0 manifest failures in 37 unstressed
  old-code runs).
- Not the cause: restart_lsn advancing without feedback (it never moved in 14 traced slot
  lifetimes), wal_sender_timeout (no termination in the CI server logs), other packages
  (the bench step runs alone).

### Problem 2(ii): seq-cycling-near-limit on PG14 "wraparound_tables: statement_timeout"

- `wraparound_tables` reads per-table statistics (`pg_stat_get_dead_tuples` ...). On PG14 a
  backend's first statistics access waits for the stats collector to write a fresh file,
  inside the statement. On an idle throwaway PG14 under host load: 10 ms typically,
  **2,659 ms** once in 8 reads. 500 ms then fails the runway sample, and the bench's inject.

## What changed

Probe runner (`internal/sre/probes`):
- Separate budgets: limiter queue (5 s), pool acquire (5 s, `pool.Acquire`), execution
  (statement timeout + 1 s) on the acquired connection; server version read on the probe's
  own connection.
- `Result.Phase` (`queue_wait`, `pool_acquire`, `server_execution`, `lock_wait`) and
  `Result.Timing` (queue, acquire, execution, attempts); `UnavailableError` carries both,
  so the runway monitor and reactive detector log lines now read e.g.
  `probe xid_runway error: deadline_exceeded in pool_acquire (queue 0 ms, pool acquire
  5001 ms, server execution 0 ms, 1 attempt(s))`. `ElapsedMS` is execution time only.
- Retry: a `statement_timeout`, `lock_timeout` or execution-phase `deadline_exceeded` is
  retried once after a 25-100 ms jittered pause, only when the caller's deadline leaves a
  whole attempt (statement + 1 s + 100 ms). Pool/queue timeouts are not retried (they had
  their own budget); permanent errors never.
- `wraparound_tables` has the 2 s background budget; the runway monitor samples it with
  `RunBackground` (investigations keep 500 ms).
- `temp_spill_statements` v3 reads `pg_stat_statements(showtext => false)` with an
  `own_role` marker (see the audit).

Bounded reads and collector (`internal/catalogread`, `internal/collector`):
- Timed-out reads are wrapped in `catalogread.PhaseError` (pool acquire + begin time,
  server execution time, `lock_wait` for 55P03); other errors (including `pgx.ErrNoRows`)
  are returned unchanged. `catalogread.Retryable`.
- The collector reads a category that hit a server-side timeout once more per cycle, at
  most 2 retries a cycle; permanent failures never.
- New `SystemStats.StatStatements` usage: true entry count each cycle; near capacity
  (>= 80% of max) utility / COPY ... TO STDOUT counts at most every 15 min, track_utility,
  pg_stat_statements_info.dealloc.

Analyzer: the capacity rule uses the true entry count (it counted the collector's
top-500, so it never fired on lifeos at 98%) and, when utility statements are at least
half, recommends `pg_stat_statements.track_utility = off` (reload) or a higher
`pg_stat_statements.max` (restart caveat), citing the counts and deallocations.

Bench (`sre-bench`): `burnXIDs` sets synchronous_commit per transaction; `emitWAL` commits
each message with `SET LOCAL synchronous_commit = on`; the manifest error reports the bytes
retained. The 18 MiB check is unchanged.

### pg_stat_statements audit (coordinator request)

| Read | Before | After | 45k entries / 14 MB text |
|---|---|---|---|
| probe `temp_spill_statements` | text for every entry + ILIKE/regex self-exclusion | `showtext => false`, `own_role` marker | 85-102 ms -> 42-49 ms |
| collector `queries` | every entry with text sorted on disk (16 MB external merge), regex on all | rank top 2xlimit on counters, text/regex/aggregate only for candidates (materialized) | 265-325 ms -> 156-178 ms |
| collector usage classification (new) | - | reads all texts, so at most every 15 min near capacity | 38 ms on lifeos |
| stats epoch, `pg_stat_statements_info`, settings, entry count | no text | unchanged | - |

On lifeos (4.9k entries, 1.2 MB) the differences are within noise (text load 5-7 ms); the
gains show at scale. Every probe now passes `TestCatalog_NeverSelectsQueryText` without the
old exemption: no probe reads query text.

## Product decisions

- Pool and queue waits get their own budgets rather than a bigger statement timeout: the
  500 ms ceiling bounds load on the monitored server and stays as is.
- One retry, server-side failures only: an idempotent read-only catalog probe can be
  repeated safely; doubling at most one 500 ms statement is the price of not losing a
  sample to a transient spike. No retry on a saturated pool (it would double the wait).
- `own_role` is a marker, not a filter: lifeos runs pg_sage as the application's role
  (`lifeos`), so filtering by role would hide the application's statements. The causal
  graph is unchanged; pg_sage's own catalog reads rarely spill.
- The track_utility advice is advice only: no SQL for the executor; a server setting on
  the user's observability extension is the DBA's call.
- No startup jitter: the 1.10.0 failures were 85 s-6 min after start, not at the burst,
  and with separate budgets a burst can no longer spend the execution budget
  (`TestRunner_StartupBurstOnBusyPool`). The limiter is a FIFO channel semaphore already.
- Deployment advice for lifeos (not changed, user's call): run `pgsage-lifeos` on the same
  Docker network as `lifeos_postgres` and connect by container name instead of
  `host.docker.internal`, avoiding Docker Desktop's port proxy (probe p50 10-16 ms and
  300+ ms spikes for sub-millisecond queries); and consider `max_connections: 5`.

## lifeos verification (read-only)

pg_stat_statements: 4,910 of 5,000 entries, 4,177 utility, 3,700 `COPY ... TO stdout`,
dealloc 100, track_utility on (the new finding would be critical and recommend
track_utility = off). Phase timings above. The fix itself is not deployed on lifeos.

## Loop runs (PGIncidentBench, runway shard: wraparound_runway, disk_wal_runway, sequence_runway)

Each repeat runs both live arms (causal-graph, causal-graph+llm).

| Code | Server | Condition | Repeats | disk-slow-consumer-fill | seq-cycling-near-limit |
|---|---|---|---|---|---|
| old (origin/master) | PG17 | disk_wal_runway only | 10 | 20/20 manifest ok; 1 inject failure `xid_runway: deadline_exceeded` (problem 1) | - |
| old | PG17 | wraparound + disk, walwriter 10 s | 3 | **2/6 failed: "does not retain the written WAL"** (the CI error) | - |
| new | PG14 | normal | 10 | 20/20 | 20/20 |
| new | PG14 | normal (final code) | 10 | 20/20 | 20/20 |
| new | PG17 | runway shard, walwriter 10 s | 10 | 20/20 | 20/20 |
| new | PG17 | normal (final code) | 10 | 20/20 | 20/20 |

New code: 40 repeats (80 live-arm runs) of each target scenario across PG14 and PG17, 0
failures, 0 contaminated retries. One unrelated decoy failed once on PG17 (repeat 8):
`disk-slot-keeping-up: slot consumer to confirm: not reached in 15 s`. Its wait races the
bgwriter's 15 s standby-snapshot interval (a logical restart_lsn only moves to a
running_xacts record); it does not use emitWAL or burnXIDs. Filed as a separate task.

## Phase timings before / after

- Before: `probe xid_runway error: deadline_exceeded` (no phase; 1.5 s spent somewhere
  between the limiter, the pool and the server).
- After (tests against PostgreSQL): pool held 1.8 s, probe ok, acquire 1.8 s, execution
  under 1 s, `elapsed_ms` under 1000 (`TestRunner_PoolWaitDoesNotConsumeStatementBudget`);
  pool held past a 300 ms acquire budget: `deadline_exceeded in pool_acquire`, 1 attempt;
  a table lock: `lock_timeout in lock_wait`, 2 attempts; `pg_sleep`: `statement_timeout in
  server_execution`, 2 attempts.
- After, lifeos (read-only, 30 runs each): acquire p50 3-4 us; execution p50 10-16 ms,
  max 293-380 ms; 0 failures.

## Test Results

**Command:** `go test -p 2 -count=1 -cover -json -timeout 3600s ./...` (PG17, pgsage-ag4,
golang:1.25 --cpus=2), then the touched packages again after the last fixes.
**Total:** 10,219 passed, 2 failed, 22 skipped (top-level tests); 99 packages ok.

### Failures
- internal/mcp TestToolReferenceDocsMatchSchemas: environment, not this branch. Its marker
  ends in a newline and the Windows checkout of `docs/mcp.md` is CRLF; it fails the same way
  on an origin/master worktree and passes in CI (LF).
- internal/sre/probes TestRunner_StartupBurstOnBusyPool (first version): it kept the pool
  saturated at a 100% duty cycle; under the loaded host the fourth queued probe of a runner
  passed the 5 s queue budget. Rewritten to a bounded 1.8 s hold (d62b2e4f; the
  deadline-split mutation still kills it); probes passes on ag4, PG14 and PG18 since.
- internal/executor timed out after 1 h in the full run (TestStatisticsThatFixEstimates
  AreImproved hung 54 min under full-suite load). The package passes alone in 564 s and the
  test in 5 s. Not this branch.

### Skipped Tests
- internal/agentdb, internal/azure: live cloud (opt-in credentials).
- internal/llm, internal/rca TestTier2Live_RealGemini: live LLM (opt-in).
- internal/rca TestRCAChildProcessFixture: subprocess helper.
- sre-bench TestPGIncidentBench, TestLiveModelArm: SAGE_BENCH_RUN / live model (the bench
  ran separately above).
- internal/ha, internal/sre/causal container failover tests, internal/sre/pooler PgBouncer
  tests: need Docker-in-test or PgBouncer.
- internal/tuner TestGeneratePlanFixtures (generator), internal/logwatch
  TestResolveLogDir_AbsoluteWindows (Windows only).
None added by this branch.

### Coverage (touched packages)
| Package | PG17 | PG14 -race | PG18 -race |
|---|---|---|---|
| internal/sre/probes | 93.3% | 93.3% | 93.2% |
| internal/catalogread | 93.8% | 93.8% | 93.8% |
| internal/collector | 87.8% | 88.1% | 88.4% |
| internal/analyzer | 91.7% | 91.6% | 91.7% |
| internal/runway | 91.1% | 91.1% | 91.1% |
| internal/sre/causal | 94.3% | 94.3% | 94.3% |
| sre-bench | 66.1% (harness, excluded as documented) | new tests ok | new tests ok |

### Coverage Gaps
All touched business packages meet the 70% threshold. Below threshold elsewhere (not
touched): cmd/create_admin 51.5%, cmd/reset_admin_for_test 50.0%,
cmd/sigstore_trusted_root 56.1%, internal/testsupport/pgssepoch 58.3%,
internal/testsupport/snapfixture 0%.

### Other gates
- e2e (`-tags=e2e`, ag4): ok, 0 skips.
- Performance gate (perfgate, small, ag4): run 1 had 4 offenders (an action_log page query,
  an incidents update, /api/v1/cases and /api/v1/trust latency) with 4 test containers
  running; origin/master under the same load also failed (1 offender: the collector's
  `queries unavailable ... statement timeout`, i.e. problem 1); run 2 on this branch:
  **0 offenders, PASS**.
- golangci-lint: 0 issues. gofmt clean on touched files. No new file over 500 lines, no new
  function over 50, no new line over 100.

### Mutation tests (all killed)
Deadline split (acquire under the statement budget), ElapsedMS including acquire, version
read on the pool, attempt budget (3 attempts), no retry, caller-deadline check removed,
retry of pool_acquire, lock_wait phase, collector cycle budget, collector permanent-error
retry, catalogread lock_wait, catalogread begin-phase retry, utility half boundary, true
entry count ignored, burnXIDs session-level, emitWAL asynchronous, monitor foreground
wraparound read, spec without background budget, near-capacity boundary, copy_out keyword,
COPY counted as application, burst test against the deadline split. The COPY mutation first
survived (cluster-wide counts move with other sessions) and led to the fixed-text
classification test.

### Bugs Found This Session
1. [BUG] probes/runner.go: pool acquire and the first server-version read ran under the
   statement deadline (lifeos deadline_exceeded).
2. [BUG] analyzer/rules_system.go: the pg_stat_statements capacity rule counted the
   top-500 read and never fired near the real max.
3. [BUG] sre-bench/env_runway.go: burnXIDs leaked synchronous_commit = off into the pool
   (disk-slow-consumer-fill flake).
4. [BUG] runway: wraparound_tables sampled with the 500 ms investigation budget though the
   PG14 stats collector wait can take seconds (seq-cycling-near-limit flake).
5. [PERF] temp_spill_statements and the collector's queries read loaded or matched every
   pg_stat_statements text.

### Manual Checks Remaining
- MANUAL: deploy to lifeos and confirm the log lines name a phase (not done: read-only).

## Post-test audit

- Inputs not tested: a PG14 stats-collector stall itself (non-deterministic; covered by the
  measured 2.66 s stall, the background-budget tests and 20 PG14 loop repeats); pgx closing
  a connection mid-query on a client deadline (covered only through classification).
- Assertions that could pass when broken: the burst test now also asserts that a probe
  actually waited on the held pool; the ranked-read test asserts both that an application
  statement is in and that pg_sage's is out.
- Fakes hiding failures: none on the database paths (real pools held by the tests, real
  locks, real pg_stat_statements); the analyzer rule tests use snapshots by design.
- Known gap: the causal graph does not consume own_role; with a shared role pg_sage's own
  spilling statement could be a candidate (rare: its reads are catalog reads).

## What is left

- disk-slot-keeping-up decoy flake (task filed).
- internal/executor TestStatisticsThatFixEstimatesAreImproved can hang under heavy parallel
  load (seen once; passes alone).
- lifeos deployment advice (Docker network instead of host.docker.internal, pool size).
