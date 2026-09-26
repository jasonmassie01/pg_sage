# Fixes: auditor contract tests, executor and retention (2026-09-26)

Branch `fix/2026-09-26-contracts-exec`, worktree `C:/Users/jmass/pgsr-fix-contracts-exec`.
Scope: the Codex (auditor) preflight regression tests for retention and verified-index
durability, run against the merged remediation. Paths are relative to `sidecar/`.

Process:
1. `6e960d3` copies the auditor tests verbatim.
2. `fa8311c` adapts only fixtures, with every behavioral assertion kept. Details are in the commit
   message and below.
3. Running the adapted tests left two failures, both real defects:
   - `24fb813` fixes retention atomicity.
   - `bfe23ef` adds failing tests for the durable claim, and `c4a4ee3` fixes it.
4. `2a17d56` and `3512963` are test-only isolation fixes found in repeated runs (`-count=5`) and
   full-package runs. They change no assertions.

## Results

| Test | Classification | Change | Commit |
|---|---|---|---|
| autonomy `TestPreflightRetentionPartitionIdentity` | FIXTURE | Dry run aged 25 h (created and cutoff times shifted together); an allow authorizer stands in for the production gate adapter. R01 was already fixed by (tableoid, ctid) in 4cd9317 | fa8311c |
| autonomy `TestPreflightRetentionInvalidatesChangedContract` | FIXTURE (shared setup only) | Already passed on the merged code. Still passes with the aged 30-day dry run: the 1-day contract does not match it, the delete is refused and 3 rows remain | fa8311c |
| autonomy `TestPreflightRetentionAuditFailureDoesNotSilentlyDelete` | FIXTURE + REAL DEFECT | After the fixture change, the DELETE committed before the `applied` row was written in a separate statement. The injected trigger failure left 1 row and no record. Fix: the audit insert runs inside the delete transaction, so both commit or neither does | fa8311c, 24fb813 |
| autonomy `TestPreflightRetentionNonpartitionedControl` | FIXTURE | Same aging and authorizer as above | fa8311c |
| cmd `TestPreflightRetentionHonorsRuntimeControls` (emergency_stop, observation, manual, disabled) | FIXTURE | Executor wired as startup does (`EnableStandingPolicy`). Autonomous trust, `tier3_moderate`, ramp and an open window configured. Dry run aged between cycle 1 and cycle 2. New `none` positive control proves the delete runs without a control. Cycle 2 must return `ErrCustodianProposalWithheld` naming the control's gate reason (`emergency_stop`, `observe_only`, `executor_disabled`) instead of nil. The zero-deletion assertions are unchanged. Unattended policy activated through propose/ratify (an earlier test leaves `staffed` active) | fa8311c, 3512963 |
| executor `TestPreflightDurableRevertHappyPath` | FIXTURE | Created-index OID identity recorded with the production `recordCreatedIndexIdentity` into `action_log.before_state`, as `executeFinding` does. `revert_created_index` requires it (G4-B08) | fa8311c |
| executor `TestPreflightDurableRevertCrashBeforeEffect` | FIXTURE | Identity as above. Recovery runs at a clock past the owner's claim lease (`verify.RevertRetryInterval`). New assertion: before the lease expires, recovery leaves the revert owed (index present, verdict `revert`, not completed). C15 was already fixed in the merge (0598b51, ad821fb) | fa8311c |
| executor `TestPreflightDurableRevertConnectionLossThenRestart` | FIXTURE | Identity made the real DROP reachable. Before that, the revert was refused before the DROP ran, so the "never reached lock wait" failure was a fixture issue. Recovery clock is past the lease | fa8311c |
| executor `TestPreflightConcurrentRecoveryClaimsEachWatchOnce` | FIXTURE + REAL DEFECT | After the identity fixture, both workers still finalized the watch (2 reverts). Fix: `StateStore.Claim` is a conditional `UPDATE ... WHERE completed_at IS NULL AND next_evaluation_at <= now RETURNING`, which sets a lease. ResumeDue acts only on rows it claimed, using the freshly returned state | fa8311c, c4a4ee3 |
| executor `TestPreflightVerifierProcessCrashResumesRollback` | FIXTURE | Identity and lease-expired recovery clock. The helper `TestVerifierCrashChildFixture` now returns unless the parent launched it; before, a whole-package run failed it | fa8311c, 3512963 |
| executor `preflight_diagnostics_test.go` | N/A | Needs the external `PREFLIGHT_DIAGNOSTIC_SQL` artifact and a fixed container name; not copied | none |
| verify `TestResumeDueSkipsWatchClaimedByAnotherWorker`, `TestPendingRevertClaimLeasesUntilRetry`, `TestPostgresClaimGrantsEachDueWatchOnce` (new) | REAL DEFECT regression | Stale-list second worker must skip. Eight concurrent PostgreSQL claims must grant exactly one | bfe23ef, c4a4ee3 |

## Decisions

- **Claim lease rather than liveness-based ownership.** A watch is owned until its
  `next_evaluation_at` lease, which is `RevertRetryInterval` (5 min). This covers both the Watch
  path (the revert verdict is persisted with that lease) and recovery claims. A restarted process
  cannot tell whether the old owner is still alive, so it resumes an owed revert after the lease
  lapses: at most 5 minutes late, never lost. The auditor fixtures froze the clock at the crash
  instant, so they now recover at a clock past the lease. The early-restart assertion documents
  that a live owner is not raced.
- **Withheld retention is an error.** A retention delete refused by the policy gate returns
  `ErrCustodianProposalWithheld` and is parked in the ledger. Every other custodian route behaves
  the same way. Returning nil would hide which control stopped the delete.

## Residual risks (not fixed, outside this scope)

- If a revert DROP outlives the 5-minute lease, a second worker can claim the watch and try again.
  The OID identity check and `DROP ... IF EXISTS` make the retry safe (the second attempt finds
  the index absent). Completion is not fenced by a claim token.
- The retain branch still persists `completed` before the retain effect (value credit and cleanup
  of the superseded index). A crash in between loses the credit and the cleanup, but no index is
  left wrongly installed. Deferred.
- `schemaguard.Custodian.Scan` stops at the first routing error. A withheld retention therefore
  ends that scan before later invariants are processed. This behavior predates this work; deferred.

## Cross-area edits

- `internal/autonomy/retention_safety_test.go`: two existing `deleteBatch` calls gain the new
  candidate-count argument. No assertion changes.
- `internal/verify/helpers_test.go`: the memory store implements `Claim`.

## Test Results

**Commands:**
- `go build ./...` and `go vet ./...`: clean.
- `go test -count=1 -cover -v ./internal/executor ./internal/autonomy ./internal/verify
  ./internal/policy ./internal/schemaguard ./internal/ledger ./cmd/pg_sage_sidecar`, run once
  as-is and once with `-tags=integration`.
- `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`.
- Repeat run: `go test -count=5 -run '^TestPreflight' ./internal/executor` passes.
- Full `go test -count=1 -cover ./...`: every package passes except the live
  `internal/rca TestTier2Live_RealGemini`, which calls the real Gemini model and returned no
  Tier 2 incident. It passed at baseline. RCA does not depend on any changed package, so this is
  model nondeterminism, not a regression.

**Total:**
- Unit + DB run: 1096 passed, 0 failed, 0 skipped.
- Integration-tag run: 1096 passed, 0 failed, 0 skipped.

**Coverage:**

| Package | Coverage |
|---|---|
| internal/executor | 79.1% |
| internal/autonomy | 80.1% |
| internal/verify | 86.2% |
| internal/policy | 83.9% |
| internal/schemaguard | 87.9% |
| internal/ledger | 91.4% |
| cmd/pg_sage_sidecar | 48.4% |

### Skipped Tests (must be zero or justified)
- None. Both runs were grepped for `--- SKIP`, `TODO` and `PENDING` and returned 0. The string
  `outcome=pending` in logs is action state, not a test marker.
- `TestVerifierCrashChildFixture` is a child-process entry point. It returns immediately unless
  its parent test launched it, so it asserts nothing in an ordinary run.

### Failures (if any)
- None in owned packages.
- Intermediate failures, all fixed:
  - `-count=5` hit duplicate `evidence_id` values in the auditor fixture (fixed in 2a17d56).
  - In a whole-package run the helper test failed and the `none` control ran under the `staffed`
    policy (fixed in 3512963).

### Coverage Gaps (packages below threshold)
- cmd/pg_sage_sidecar: 48.4%. The executor-fix report measured 43.8%. This is startup and orchestration
  wiring and was below threshold before this work. All business-logic packages in scope meet 70%.

### Bugs Found This Session
1. [BUG] `autonomy/retention_postgres.go` `deleteBatch`: the retention DELETE committed before
   its `applied` audit row. An audit failure left rows deleted with no durable outcome. Fixed in
   24fb813.
2. [BUG] `verify/engine.go` `ResumeDueResults` and `verify/postgres.go` `ListDue`: due watches
   were read with no durable claim. Overlapping workers evaluated and reverted one watch twice.
   Fixed in c4a4ee3.
3. [TEST] The auditor fixture was not re-runnable in one database, and its crash helper failed in
   full-package runs (2a17d56, 3512963).

### Manual Checks Remaining
- CHECK-01: MANUAL. Run `go test -race` for executor and verify on a host with CGO. The claim is
  a database-level race and is covered by the real concurrent PostgreSQL test instead.
