# Sage SRE lease test flakes: report (2026-10-02)

Branch `claude/sre-lease-test-flake` (based on master `e2f7c40`, Sage SRE M3).

## Summary

- **One production bug found and fixed:** the worker did not renew its lease while it
  waited between compared samples in connection and WAL investigations. With
  `sre.sample_interval_seconds` near its maximum of 30, the wait was as long as the
  30-second lease, so every attempt lost the lease. The investigation was then re-claimed
  and lost again until its 120-second budget ran out, and it ended as `failed`
  (`budget_exhausted`). The fix was test-first (commits `76a8257`, `9471f6b`).
- **The lease test flakes are fixed.** Tests no longer wait out a tiny TTL with a sleep. They
  hold leases with the default 30-second TTL and expire them on purpose with
  `expireLease`, which backdates the lease using parameterized SQL. The two tests that
  check heartbeats still use the real clock, with margins that survive load.
- **Under CPU load, with `-count=5` on PG17:** 8 lease failures before the fix and 0 after
  (925 passed, 0 failed, 0 skipped).

## Production bug (fixed)

`collect` (`internal/sre/worker.go`) called `c.sleep(ctx, SampleInterval)` without a
heartbeat. The last heartbeat came before the previous step's probes, so whenever the
previous step plus the sample interval was at least the lease TTL (30 s), the lease ran out.
The heartbeat after the wait then returned `ErrLeaseLost`, and `Investigate` stopped quietly.
On the next poll the investigation was re-claimed, charged for the whole orphaned segment,
and the same wait lost the lease again. Connection and WAL investigations configured with
`sample_interval_seconds` of about 25 to 30 could therefore never conclude.

Fix: a new function, `waitHeld`, runs the wait under the existing `keepAlive`, which renews
the lease every third of the remaining lease time, as it already does for model calls. If
the lease is lost during the wait (an operator stop), the wait now ends at the next
heartbeat, not after the full interval.

Tests, written and committed before the fix:
- `TestCollect_SampleWaitAsLongAsTheLeaseKeepsTheLease` uses a wait equal to the TTL, the
  largest ratio the configuration allows. Before the fix the investigation stayed
  `collecting` and was never concluded. After the fix it concludes with a single claim.
- `TestCollect_LeaseLostDuringSampleWaitEndsTheWait` stops the investigation during a 25 s
  wait. Before the fix the wait lasted 25.02 s. After the fix it ends at the next heartbeat
  (about 1 to 2 s), with only the first step's probes committed.

Product decision: this is a reliability fix only. It adds no autonomy and does not change
any action path.

I also checked the heartbeat interval. Production uses a 30 s TTL and heartbeats every
10 s, so the sidecar would have to stall for 30 s to lose a lease. That margin is enough,
so I made no change there.

## Per-test timing fixes (test isolation)

| Test | Wrong timing assumption | Now |
|---|---|---|
| `TestStore_StaleWorkerCannotCommit` | A 300 ms lease had to still be held when the rival claim ran | 30 s TTL, then `expireLease` |
| `TestStore_ExpiredLeaseCannotCommitEvenWithoutRival` | Slept 800 ms for a 200 ms lease (slow, and depends on clock skew) | `expireLease` |
| `TestStore_ActiveTimeIsChargedAndCapped` | Step B and claim C had to fit a 3 s budget, and the sleep had to outlast C's lease | 60 s budget. Both dead leases are expired by the helper. The 300 ms orphan charge and the exact-cap final charge are still asserted exactly |
| `TestStore_PendingFindsQueuedAndOrphanedWork` | The 300 ms TTL covered `evaluating()`'s claim and commit ("commit: lease lost") | 30 s TTL, then `expireLease` |
| `TestStore_ClaimPastTheBudgetFailsTheInvestigation` | Slept 1.3 s for a 1 s lease | `expireLease` (the whole 1 s budget is leased) |
| `TestStore_ConcludeNeedsTheCurrentLease` | Claim and commit had to fit a 1 s lease | 30 s TTL, then `expireLease` |
| `TestReserveModel_CrashAfterReservationKeepsTheHold` | Claim and reserve had to fit a 200 ms lease ("reserve: lease lost") | 30 s TTL, then `expireLease` |
| `TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep` | The dead worker's step and the whole resumed run used a 300 ms TTL | 30 s TTL, then `expireLease` |
| `TestCollect_ResumeAfterNeedsEvidenceConcludes` | Three commits had to fit 1 s, and the resumed run used a 1 s TTL | 30 s TTL, then `expireLease` |
| `TestModelProbe_ResumedRunWithTurnsUsedFallsBack` | Two reservations had to fit 1 s, and the resumed run used a 1 s TTL | 30 s TTL, then `expireLease` |
| `TestModelTurn_SlowCallKeepsTheLease` (still on the real clock) | A 1 s TTL. Instrumented runs showed one 3.6 s gap between 1 s heartbeat ticks under load | 6 s TTL and a 9 s call (1.5 TTLs). The test gets its own client with a 30 s HTTP timeout, because the shared fake client allows only 5 s |
| `TestModelTurn_NoTimeLeftSkipsTheModel` | The whole run had to fit a 4 s budget | `stepMargin + minModelTime - 1s` (5 s), the largest budget that still counts as "no time left" |
| `TestCoordinatorRun_AppliesRetention` (not a lease test) | It ran the loop for exactly 700 ms. Under load the first purge was cancelled and, with the hourly retention interval, never retried | The loop now runs until the row is gone (15 s deadline) |

How `expireLease` works (`lease_expiry_helper_test.go`): it moves `lease_started_at`,
`lease_until` and `segment_deadline` back by the same amount, using one `now()`, so that
`lease_until = now() - 1 ms`. The time an orphaned lease is charged
(`lease_until - lease_started_at`) therefore stays exactly the TTL that was granted. The
helper also asserts that it updated exactly one row: the held lease, matched by owner and
fence token.

## Evidence

Load: a CPU burner container (`--cpus=12`, 12 busy loops; it reported 1126 to 1191 % CPU)
plus the other agents' test containers, on a 20-CPU Docker VM. All runs used PG17 `pgsage-ag8`.

| Run | Command | Result |
|---|---|---|
| Before (master `e2f7c40`) | `go test -count=5 ./internal/sre/` under load | **FAIL**, 8 lease failures in 894 s: PendingFinds ×2 ("commit/conclude: lease lost"), ResumesAnOrphaned ×2 ("dead worker step: lease lost"), CrashAfterReservation ×1 ("reserve: lease lost"), ResumeAfterNeedsEvidence ×1 and ResumedRunWithTurnsUsed ×2 (the resumed run lost its 1 s lease and stopped before concluding) |
| Intermediate (`c72b12a`) | same, with `-v` | 6 failures: SlowCall ×5 (my 6 s call exceeded the 5 s client timeout, an error in my test), AppliesRetention ×1 (700 ms loop). Fixed in `df97e03` and `f3b42c0` |
| After (`f3b42c0`) | `go test -count=5 -v ./internal/sre/` under load | **ok**, 925 passed, 0 failed, 0 skipped, 0 "lease lost", 427 s |

Mutation checks. Each mutation was made on purpose, and in every case the tests failed as
they should:
- M1: removing `lease_until > clock_timestamp()` from `leaseGuard` makes
  `TestStore_ExpiredLeaseCannotCommitEvenWithoutRival` fail ("commit after lease expiry =
  <nil>").
- M2: if `expireLease` does not move `lease_started_at`, `ActiveTimeIsChargedAndCapped`
  fails ("charged 8 ms, want the whole 300 ms") and so does `ClaimPastTheBudget...`.
- M3: removing the fix itself (the pre-fix code) makes both new sample-wait tests fail, as
  shown above.
- M4: if `keepAlive` never heartbeats (interval of 1 hour), `SlowCallKeepsTheLease` fails,
  and so do both sample-wait tests.

## Test Results

**Command:** `go test -cover -count=1 ./...` (whole module, PG17 `:55478`, repo root mounted)
**Total:** 67 packages ok, 1 failed (`internal/analyzer`, not touched by this branch, see
below), 0 skipped tests in the touched packages.
**Coverage of touched packages:** `internal/sre` 88.4 %, `internal/sre/causal` 95.7 %,
`internal/sre/probes` 89.7 %. All packages meet the coverage thresholds.

| Check | Result |
|---|---|
| `go test -race -count=1 -v ./internal/sre/...` on PG17 | ok: 315 passed, 0 failed, 0 skipped, 0 data races |
| `go test -count=1 -v -cover ./internal/sre/...` on PG18 `:55418` | ok: 315 passed, 0 failed, 0 skipped |
| Same on PG14 `:55414` | `sre` and `causal` ok. `probes`: `TestCatalog_ReplicationProbesOnAPrimaryWithoutReplicas` failed because another agent had a streaming replication client attached to the shared PG14 at that moment (`replay_lag_bytes` 89 MB, `client_addr` 172.17.0.1). A rerun after the client left: ok, 89.7 % |
| `golangci-lint run ./...` (Windows) | 0 issues |
| `go vet`, `gofmt` on touched files | clean |

### Skipped Tests
None in the touched packages.

### Failures
- `internal/analyzer` `TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding` failed
  only in the full-module run, with `relation "pg_stat_statements" does not exist`. It
  passes alone and as a package (`ok`, 85.2 %). This looks like interference between
  packages that run in parallel against the same database, and it predates this branch,
  which does not touch `internal/analyzer`. Reported here; not fixed.

### Coverage Gaps
None. Every touched package is at or above 70 %.

### Bugs Found This Session
1. [BUG, fixed] `worker.go` `collect`: the wait between samples did not renew the lease,
   so with `sre.sample_interval_seconds` near 30 every connection and WAL investigation
   ended as failed (`budget_exhausted`).
2. [Test bug, fixed] Thirteen tests in `internal/sre` raced the wall clock: twelve against
   tiny lease TTLs or budgets, and one (`AppliesRetention`) against a fixed loop run time.
   See the table above.
3. [Not fixed, outside scope] Interference in the full-module test run (the analyzer test
   above, and the shared-matrix replication client on PG14).

## Post-test audit

- **Inputs not tested:** a WAL investigation's wait. It uses the same code path as the
  connection wait that is tested, and its plan has the same `sample: true` step. A
  heartbeat that fails with a transient store error rather than a lost lease during the
  wait: `keepAlive` treats it the same way as during a model call (the wait is cancelled
  and the error returned), and the existing model-call tests cover that branch.
- **Assertions that pass when the code is broken:** none known. The mutations above
  show that the expiry, orphan-charge and heartbeat assertions fail when that logic breaks.
- **Fakes that hide failures:** `expireLease` writes the lease columns directly rather than
  waiting for time to pass. M1 and M2 show that the expiry checks in production SQL
  (`clock_timestamp()` comparisons) are still the code under test. Two tests still use the
  real clock on purpose, because heartbeats are what they test.

## What is left

- The analyzer interference in the full-module run, and the risk that the shared matrix
  has replication clients attached. Both are outside this branch.
- `keepAlive` sets its heartbeat interval from `time.Until(lease.Until)`, which compares the
  host clock with the database clock. A database clock running ahead of the host by more
  than about 2 TTLs (60 s) would let the lease lapse. That is not realistic with NTP, so I
  made no change. It is noted here for the coordinator.
