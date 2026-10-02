# Sage SRE follow-ups B: failover between samples, failover while down, pooler exhaustion, fleet runway dedupe

Date: 2026-10-02. Branch `claude/sre-followups-b`, based on master `fce3674` (v1.8.0-to-be).
Test database `pgsage-ag6` (PG17, :55476). Spec: `reviews/2026-09-26/AI-SRE-SPEC.md`.
Earlier audits: `2026-10-02-sre-m4-ga-report.md` (CHECK-04 and CHECK-07 partial),
`2026-10-02-sre-m6-runways-report.md` and `2026-10-02-sre-m7-autonomy-report.md`.

**Graph version: `causal-v4`.** `git ls-remote --tags origin v1.8.0` returned `8c40cb9`
during this work, and v1.8.0 ships `causal-v3`. The graph changes on this branch therefore
bump the version (commit `52a2abb`).

## What was built

### 1. CHECK-07: restart or failover between samples

- **Server identity on compared probes.** `internal/sre/probes/identity.go` adds
  `ServerIdentity`, `IdentityOf` and one SQL fragment. The fragment reads the postmaster
  start, `pg_control_system().system_identifier`, the timeline, `pg_is_in_recovery()` and
  `server_version_num`. On a primary the timeline is the WAL insert timeline (taken from
  `pg_walfile_name(pg_current_wal_lsn())`), which is correct immediately after a promotion.
  On a standby it is the last checkpoint's timeline. Every function used is executable by
  PUBLIC. Three probes carry the identity on each row: `connection_saturation` v3,
  `wal_checkpoint` v2 and `replication_slots` v2 (`catalog_capacity.go`, `decode_m2.go`).
- **Comparison validity.** `internal/sre/causal/identity.go` returns the most specific
  reason two samples cannot be compared:
  - `server_replaced`: the system identifier changed;
  - `failover_between_samples`: the role or the timeline changed;
  - `major_version_changed`;
  - `server_restarted`;
  - `server_identity_unknown`: a field is known on one side only.

  A field unknown on both sides (evidence recorded before this change) is not compared.
- **Families.** `connection.go` uses this check in `compareConn`. `wal_identity.go`
  (`walComparable`) gates WAL, disk/WAL runway and replication lag. When the identity
  changed, every WAL series is cut back to its last sample, so no rate, slot growth or rising
  failure count is computed across the change. The change is recorded as missing evidence.
  The last sample is still judged on its own: an inactive slot or a pool fan-out on the new
  primary is still an observation.

### 2. HA: failover just before a restart

- **Persisted identity.** `internal/ha/identity.go` and `identity_store.go` store the node's
  last observed identity (role, timeline, system identifier, start) and when it last changed.
  `Monitor.WithIdentityStore(store, key)` enables this, and the data goes in
  `sage.ha_identity`, a new idempotent migration (`schema/ha_identity_migration.go`) with one
  line added to `bootstrap.go`.
- **Startup comparison.** The first successful check compares the stored identity with the
  current one:

  | What changed while pg_sage was down | Effect |
  |---|---|
  | Role | Opens the M7 failover cooldown and counts as a role flip |
  | Timeline or system identifier | Opens the cooldown, does not count toward flapping |
  | Only the postmaster start (a plain restart) | Nothing |
  | Nothing, but a change was recorded shortly before the restart | That change keeps its cooldown |
  | Stored identity cannot be read | Fails closed: the cooldown opens |

- **While running.** A timeline or system-identifier change under the same role now also
  opens the cooldown. This covers a failover behind a DSN or VIP that follows the primary,
  which the monitor could not see before.
- **Wiring.** `cmd/pg_sage_sidecar/ha_identity_wiring.go` adds `newPersistedHAMonitor` and
  `haIdentityKey`: `db:<id>`, or `name:<instance>` when there is no database ID. It is used
  only by the earned-autonomy limiter's monitor (`autonomy_wiring.go`). The fleet
  orchestrator and replica-probe monitors are unchanged.
- **Retention.** `sage.ha_identity` is exempt from age purges (`retention/cleanup.go`). It
  is current state, and the retention guard test requires a purge rule or an exemption.

### 3. CHECK-04: pool exhaustion behind PgBouncer

- **Config.** `sre.poolers` is defined in `internal/config/sre_pooler.go` and adds one field
  plus one validation call in `sre.go`. Each pooler has:
  - `name`;
  - `dsn` (tagged `secret:"true"`; use `${ENV}`) or `dsn_file` (a mounted secret);
  - `databases`: which instances it fronts (empty means all);
  - `pools`: a PgBouncer database filter;
  - `timeout_ms`: 100–2000, default 1000.

  Validation errors never quote the DSN. `config_meta.json` and
  `docs/generated/config-lifecycles.md` were regenerated (`sre.poolers` is restart-bound).
- **Probe** (`internal/sre/pooler`):
  - Runs only `SHOW POOLS` and `SHOW STATS`, using the simple query protocol with no
    statement cache.
  - Each pooler is bounded by its timeout and the probe row ceiling (500, with truncation
    flagged).
  - The admin console is never reported as a pool.
  - Failures are classified into reasons and never carry error text, so a DSN, host or user
    cannot leak: `pooler_unreachable`, `pooler_timeout`, `pooler_protocol_error`, or
    `pooler_auth_failed` (reported as `no_privilege`).
  - Unknown numbers are stored as null and decode as NaN.
- **Signal probe.** It is wired as the `pooler_pools` signal probe (`probes/pooler.go`, with
  `IsSignal` extended). The model can never propose or call it. `addPooler` adds it to both
  steps of a connection investigation only, and it is a series probe, so both samples are
  compared. Wiring: `cmd/pg_sage_sidecar/sre_pooler.go` plus one line in
  `database_runtime_sre.go`.
- **Causal node.** `pooler_saturation` (`causal/pooler.go`) is in the connection family and
  amplifies `blocked_backlog`. It is scored from the pool with the most waiting clients:

  | Evidence | Weight |
  |---|---|
  | Clients waiting | 0.3 |
  | Oldest wait ≥ 1 s | 0.2 |
  | Clients waiting in both samples | 0.2 |
  | No idle server connections | 0.1 |

  - Zero waiting clients rules it out. A short queue seen once (< 1 s) is not enough.
  - The pool's state is stated as an observed fact.
  - With a lock backlog, the backlog is the root and the pooler queue contributes.
  - Without pooler telemetry no hypothesis is added. The family behaves exactly as before,
    plus missing evidence `pooler_pools: pooler_telemetry_unavailable`.
- **Replay.**
  - The replay runner serves recorded signal probes (`replay/runner_signal.go`,
    `replay_run.go`), and validation accepts them.
  - Three cases were added under the `post_r1` tag: `conn-pooler-queueing` (positive),
    `conn-pooler-behind-lock-backlog` (backlog root, pooler contributing) and
    `conn-pooler-busy-no-queue` (decoy).
  - Two CHECK-07 cases were also added: `conn-failover-between-samples` and
    `wal-failover-between-samples`.
  - The R1 composition test counts only cases without `post_r1`, so R1's 10/5/5 split stays
    frozen.

### 4. Fleet WAL-runway database-size dedupe

- **Probe split.** `wal_runway` v2 no longer sums database sizes. It names its cluster
  instead: `system_identifier`, `server_started_at` and `role_name`. A new catalog probe,
  `cluster_database_size`, measures the size (`probes/catalog_runway.go`,
  `decode_runway.go`).
- **Shared measurement.** `runway.SizeShare` (`internal/runway/share.go`) holds one
  measurement per cluster per pass:
  - The key is the system identifier plus postmaster start plus role, so a standby, a
    restored clone or a role with different privileges never reuses another's reading.
  - The TTL is the runway interval.
  - Single-flight: runtimes that tick together wait for the one measurement in flight.
  - Failures are never reused.
  - Stale keys are pruned past 256 entries.
  - A standby measures nothing.
- **Wiring.** One process-wide share: `runwaySizeShare` in `database_runtime_runway.go`. The
  credit measurement (`MeasureDisk`) measures directly, because it runs per action, not per
  pass.

## Product decisions

1. **A restart invalidates WAL comparisons too.** WAL counters survive a clean restart, but
   CHECK-07 says a restart invalidates comparisons, and the conservative answer is "missing
   evidence". This costs at most one inconclusive WAL rate per restart.
2. **Identity gating also covers the disk/WAL runway and replication-lag families.** They
   share `measureRate`; leaving them out would have allowed a WAL rate across a failover.
3. **The last sample is still evidence after a failover.** Only cross-sample growth, rates
   and counts are refused.
4. **Pooler telemetry is a signal, not a catalog probe.** It holds credentials and talks to
   another system, so the model cannot request it. It is read only in connection
   investigations.
5. **Pooler queueing must be real to count.** It needs clients waiting plus a wait of at
   least 1 s, or a queue that persists across samples, following the PgBouncer runbook
   threshold (`maxwait` > 1 s). A pool that is busy but has no queue rules the hypothesis
   out.
6. **Pooler queues do not start investigations by themselves** (no new detector). Connection
   investigations start as before: from an RCA `connections_high`, an SLO burn or an
   operator. A pooler queue detector is listed under What's left.
7. **HA history fails closed.** Unreadable history opens the 30-minute cooldown, because
   pg_sage cannot rule out a failover it did not see. That fits the spec's trust-before-
   autonomy rule. A plain restart does not open the cooldown.
8. **A role change while down counts toward flapping; a timeline-only change does not.**
   Right after a promotion, the timeline is read from the WAL insert position, so an
   in-place promotion is a single role flip, not two changes.
9. **The size-share key includes the start time and the role** in addition to the system
   identifier (the brief said "keyed by system identifier"). Without them, a standby, a clone
   or a less-privileged role could reuse a reading that is wrong for it.

## Spec CHECKs covered

| CHECK | Status | Evidence |
|---|---|---|
| CHECK-07: counter reset, restart, failover and major version invalidate comparisons | **pass** (was partial) | `causal/identity_test.go`, `connection_failover_test.go`, `wal_failover_test.go`; live identity `probes/catalog_identity_db_test.go`; **real restart and real promotion between samples** `causal/identity_container_db_test.go`; replay `conn-failover-between-samples`, `wal-failover-between-samples` |
| CHECK-04: pool exhaustion, backend exhaustion and lock backlog produce different evidence | **pass** (was partial) | `causal/pooler_test.go`; `pooler/pooler_test.go`; **real PgBouncer 1.26** `pooler/pgbouncer_db_test.go` (an exhausted pool queues a client, which the probe reads; wrong password; nothing listening); replay `conn-pooler-*` |
| CHECK-40: autonomy downgrades on failover | strengthened | `ha/identity_test.go` (17 cases), `ha/identity_store_db_test.go`, **real promotion while the monitor is down** `ha/failover_container_db_test.go` |
| CHECK-21: probe caps | holds for the new probes | `cluster_database_size` is in the catalog with the standard ceilings; the pooler probe has a row cap and timeout tests |
| CHECK-08: missing evidence is explicit | holds | pooler telemetry unavailable, unreachable or no-privilege is stated as missing, never as healthy |

## Test Results

**Command (full suite, PG17 ag6):** `go test -count=1 -cover -timeout 40m ./...` (repo root
mounted in `golang:1.25`).

**First full run:** 59 packages ok, 22 FAIL. 18 of the 22 failed only because test-fixture
`DROP DATABASE` timed out. The host was saturated (ag6 checkpoint sync time 409 s
cumulative; several agents were running full suites at once). Of the other four, three were
also load timeouts: the replay corpus ran out of its time budget, a meta bootstrap timed out
in `cmd`, and a lock-ceiling timing test failed in `executor`. One failure was real:
`retention/TestRetentionRules_CoverEveryTimeSeriesTable` flagged the new `ha_identity` table.
That is fixed in `e636dc1`.

**Rerun of the 22 packages** (`-p 2`): **22/22 ok.**

**Touched packages, final** (`go test -count=1 -cover -v -p 2`):

| Run | Passed | Failed | Skipped |
|---|---|---|---|
| PG17 `-race` | 1855 | 0 | 7 |
| PG14 | 1854 | 1 | 7 |
| PG18 | 1855 | 0 | 7 |

The PG14 failure was `TestModelTurn_DisabledClientIsLoggedOnce`: its bootstrap could not get
the advisory-lock connection on the shared matrix server. Rerun alone: ok. No data races were
reported.

**Container tests** (disposable containers I started and removed:
`postgres:17-alpine` ×4 and `edoburu/pgbouncer` 1.26):
- `causal/TestContainer_RestartBetweenSamplesInvalidatesComparisons`: PASS
- `causal/TestContainer_PromotionBetweenSamplesInvalidatesComparisons`: PASS
- `ha/TestContainer_FailoverWhileDownOpensTheCooldown`: PASS
- `pooler/TestPgBouncer_*` (3): PASS. Pooler coverage with PgBouncer present was 94.5%.

**Lint:** `golangci-lint run ./...` reported 0 issues.

**Web:** `npm ci && npm test` ran 46 files and 246 tests; all passed. The only web change is
the regenerated `config_meta.json`. `node_modules` was removed afterwards.

**Coverage of touched packages (PG17):**

| Package | Coverage |
|---|---|
| internal/sre | 87.3% |
| internal/sre/causal | 94.3% |
| internal/sre/probes | 92.8% |
| internal/sre/pooler | 75.3% without a PgBouncer, 94.5% with one |
| internal/ha | 97.8% |
| internal/runway | 89.8% |
| internal/config | 89.2% |
| internal/schema | 81.6% |
| internal/retention | 100% |
| internal/earned/hasource | 100% |
| cmd/pg_sage_sidecar | 72.6% |
| sre-bench/replay | 94.8% |
| sre-bench (bench harness, utility) | 62.2% |

### Skipped Tests (must be zero or justified)
- `causal/TestContainer_RestartBetweenSamplesInvalidatesComparisons` and
  `causal/TestContainer_PromotionBetweenSamplesInvalidatesComparisons`: skipped without
  `SAGE_TEST_RESTARTABLE_SUPERUSER_URL` / `SAGE_TEST_CAUSAL_STANDBY_URL`. They restart or
  promote a server, so they need a disposable one. They ran and passed against disposable
  containers (see above).
- `ha/TestContainer_FailoverWhileDownOpensTheCooldown`: skipped without
  `SAGE_TEST_HA_STANDBY_URL`, which must be a disposable standby the test promotes. Ran and
  passed.
- `pooler/TestPgBouncer_*` (3): skipped without `SAGE_TEST_PGBOUNCER_ADMIN_URL` and
  `..._CLIENT_URL`. Ran and passed against a real PgBouncer.
- `sre-bench/TestPGIncidentBench`: skipped unless `PG_SAGE_BENCH_RUN=1`. This is existing
  behavior; CI runs it in its own step.

### Failures (if any)
- None outstanding. The load-induced failures and the PG14 flake are explained above.

### Coverage Gaps (packages below threshold)
- `internal/sre/pooler` is 75.3% without a PgBouncer: the pgx admin session is only reachable
  live. It meets the threshold either way.
- `sre-bench` is 62.2%. It is the bench harness (a utility, threshold 50%), and its coverage
  is unchanged from before this branch.
- All business packages meet the 70% threshold.

### Bugs Found This Session
1. [BUG] `pooler/read.go`: PgBouncer reports refused logins as SQLSTATE `08P01`
   ("SASL authentication failed"), not class 28. A wrong password was therefore reported as a
   protocol error. Found against the real PgBouncer 1.26 and fixed in `9d41063`, with a
   regression test.
2. [BUG, pre-existing gap] The HA monitor could not see a failover behind an address that
   follows the primary (timeline change, same role). It now opens the cooldown.
3. [BUG] The new `sage.ha_identity` table had no retention decision. The guard test caught it
   and it is fixed (exempt as current state).
4. [TEST] `conn-failover-between-samples` was authored with 11 idle backends. From the last
   sample alone that is a real pool fan-out, so the case now holds 8 (`d2c5976`).
5. [TEST] A `SizeShare` test asserted that a shared reading is re-stamped on each reuse. That
   was wrong: a reading keeps its measurement time (`c6b8ec3`).
6. [NOTE, not mine] `retention/cleanup.go` has a gofmt alignment deviation at `rollout_run`.
   It predates this branch and was left alone.

### Manual Checks Remaining
- None for this scope. No UI was changed: pooler facts and missing evidence render through
  the existing Cases panel.

## Post-test audit

1. **Inputs that are not tested.**
   - PgBouncer versions before 1.8 (no `maxwait_us`) are tested only with the fake.
   - Odyssey and PgCat consoles are not supported: the probe is PgBouncer-specific.
   - A cascading standby's timeline lags until its next restartpoint; on a standby,
     mutations are blocked anyway.
   - Whether `pg_upgrade` preserves the system identifier is not relied on: a major upgrade
     is caught by the version and restart checks.
   - Two poolers that report the same pool name are told apart by the pooler name.
2. **Assertions that could pass with the feature broken.** I ran 20 mutants over the
   identity check, WAL freezing, pooler scoring and boundaries, the plan, the size share, HA
   reconciliation and the pooler probe; 19 were killed. The survivor, "share caches
   failures", is equivalent: a failed reading carries no measurement time, so it is never
   fresh and never reused.
3. **Fakes that hide real failures.**
   - The pooler fake returns the Go types pgx decodes. The real PgBouncer test confirms them,
     and it found bug 1.
   - The HA `memStore` is backed by `identity_store_db_test` against real Postgres.
   - Replay serves recorded pooler telemetry. The real PgBouncer test covers the live read.
4. **Limits.**
   - All new and changed functions are ≤ 50 lines and all new files ≤ 500 lines.
   - `bootstrap.go` (994 lines) and `PersistTrustRampStart` were already over the limits.
   - Config doc-tag lines exceed 100 characters, following the existing config convention.

## What's left

- **A pooler queue detector.** Start a read-only connection investigation when
  `cl_waiting > 0` and `maxwait > 1 s` persist at a configured pooler. Today a pooler queue is
  diagnosed only when a connection investigation runs.
- Odyssey and PgCat telemetry.
- Persisting the HA identity for the fleet orchestrator's monitor too. Today only the
  earned-autonomy limiter's monitor persists; the orchestrator gates on safe mode only.
- A live `PG_SAGE_LIVE_LLM=1` replay of the new cases (the account has no credits).

## Coordinator decisions

1. **`causal-v4`.** v1.8.0 shipped `causal-v3`, so this branch bumps the graph. If another
   branch also changes the graph before release, merge both into v4; do not bump twice.
2. **Shared and generated files that may conflict on merge:**
   - `CHANGELOG.md` (one bullet);
   - `internal/schema/bootstrap.go` (the migration list line);
   - `internal/config/sre.go` (field and validation call);
   - `cmd/pg_sage_sidecar/autonomy_wiring.go` (HA monitor);
   - `database_runtime_sre.go`, `database_runtime_runway.go`;
   - `retention/cleanup.go` (one exemption);
   - the generated `web/src/generated/config_meta.json` and
     `docs/generated/config-lifecycles.md`. Regenerate these after merging rather than
     hand-merging.
3. **Probe version bumps:** `connection_saturation` v3, `wal_checkpoint` and
   `replication_slots` v2, `wal_runway` v2, and the new `cluster_database_size`. Stored
   evidence from older versions still decodes: unknown identity fields are not compared.

## Commits

```
e636dc1 fix(retention): exempt sage.ha_identity from age purges
9c9d2a2 style(ha): wrap a long test line
9d41063 fix(sre): recognize PgBouncer's own login refusals
923ea3c perf(runway): measure each cluster's database size once per pass
52a2abb feat(sre): bump the causal graph to causal-v4
7fb3ae5 feat(sre): diagnose pool exhaustion at an external pooler
57fa8f4 feat(ha): open the failover cooldown for a failover while pg_sage was down
af3497d feat(sre): refuse sample comparisons across a server identity change
f858426 test(sre): expect causal graph v4
d2c5976 test(sre): keep the failover replay case about growth only
10ac2cb test(probes): expect the identity and runway-size catalog changes
c6b8ec3 test(runway): keep a shared reading's measurement time
67a547a test(probes): move database-size assertions to cluster_database_size
4281c1b test(runway): add fleet database-size dedupe tests
0a31e08 test(sre): add CHECK-04 pooler exhaustion tests
6aeac34 test(ha): add failover-while-down identity persistence tests
648d3b4 test(sre): add CHECK-07 failover-between-samples tests
```
