# Debt, part 1: retention through Apply, operator typed-target lease, serialize_mode queue

Branch `claude/debt-exec` (from master `fce3674`). Agent DB `pgsage-ag7` (PG17, :55477).
Not pushed. Tests were written and committed first (`bddd09e`), then implemented.

## What was built

### 1. Retention delete through `Executor.Apply`

Before: the D5 retention batch called `Executor.AuthorizeRetention` (an `AuthorizeOnly`
intent that stopped after the first authorization) and then ran the delete itself, outside
the pipeline: no lease, no slot, no re-authorization after waits, no action record, no
verification.

Now `Executor.ExecuteRetention` runs the batch inside Apply:
authorize (standing gate) → typed-target lease on the table → DDL slot → re-authorize →
execute (action_log row written *before* the delete, then the batch) → verify.

- **Contract binding kept.** The batch still re-verifies, under `LOCK TABLE ... ROW
  EXCLUSIVE` in the delete transaction, that the relation OID, the declared column's attnum
  and type, and the contract id/version are the ones the reviewed dry run was bound to. A
  contract re-declared between dry run and execution is refused inside the transaction
  (nothing deleted, action recorded `failed`).
- **Dry-run bound.** A reviewed dry run now authorizes a bounded number of rows in total:
  its drift ceiling (2 × its candidate count + 100, the same bound the drift guard already
  used), less the rows already deleted by batches under it (`retention_run.dry_run_id`).
  When used up, a new dry run is recorded and deletion parks until it passes review. Each
  batch deletes at most min(batch limit, remaining bound).
- **Verification before commit.** The delete returns, from the deleted rows themselves, how
  many there were, how many do not satisfy `column < cutoff` and how many are outside the
  contracted relation (or its partitions, `pg_partition_tree`). Any such row, or more rows
  than the cap, rolls the batch back (`ErrRetentionVerification`).
- **Verification after commit.** The executor independently checks the batch report
  against the cap and against the durable `sage.retention_run` row (applied, same row count,
  same `action_id`), then records `success` + a `sage.verification` row, or `failed` +
  `unverifiable` (`ErrRetentionUnverified`).
- **Lock timeout** is the decision's (policy lock ceiling, 3 s in shipped profiles), never
  above the batch's own 2 s; set with `set_config` (parameterized).
- **E-stop**: refused at the first authorization, and again at the re-authorization when it
  is pressed while the batch waits for its lease or slot.
- **Lease conflict** parks the delete (`ParkedRoute`, parked `ddl_conflict` decision, no
  action row). Queue mode waits.
- `AuthorizeRetention` and `ActionIntent.AuthorizeOnly` are removed (no other user).

Files: `executor/retention_apply.go`, `executor/retention_run.go` (replace
`retention_authorization.go`), `autonomy/retention_batch.go`, `autonomy/retention_review.go`,
`autonomy/retention_postgres.go`, `autonomy/retention_identity.go`,
`autonomy/schema_postgres.go`, `cmd/pg_sage_sidecar/autonomy_runtime.go`.

### 2. Operator typed-target lease

Before: `ExecuteManual` took no lease at all (only a time lease on the recommendation row),
and leases were keyed by a name string, so `"orders"`, `orders` and a renamed table were
different objects; an index and its table never collided.

Now:
- `policy.ResolveTypedTargets` resolves names through the catalog (`to_regclass`, so
  unqualified names follow `search_path`) to `TypedTarget{Kind, Schema, Name, OID}`. An index
  adds its table. A qualified name with no object is kept by name; an unqualified one is an
  error (a change must never run unleased).
- `PostgresLeaseManager.AcquireTyped` takes the canonical name key (so every existing
  name-based lease still collides) and an `oid:<n>` key (rename- and spelling-proof). Lease
  rows record `actor`, `object_type`, `object_oid`, `object_name`. A conflict is a
  `LeaseConflictError` naming the holder and its decision.
- Operator actions lease the objects their SQL changes (`statementTarget`, shared with the
  protected-schema check). The holder is `operator:user:<id>`. Operator requests now carry
  `TargetObjs` in the ledger.
- Findings and custodians use the same typed leases; custodian `VACUUM` (freeze) now takes
  one too, so an operator `ALTER TABLE` and a running freeze on one table are serialized.
- A conflict refuses an operator action with `ErrTargetLeased` naming the holder, records a
  `blocked` / `ddl_conflict` decision (`evidence.refused_decision_id`, `lease_holder`), and the
  API returns **409** (was 500 "execution failed"). In queue mode the operator waits.

Files: `policy/typed_target.go`, `policy/lease_conflict.go`, `policy/postgres_lease.go`,
`executor/apply_lease.go`, `executor/lease_park.go`, `executor/statement_target.go`,
`executor/operator_lease.go`, `executor/manual.go`, `executor/operator_decision.go`,
`executor/validate.go`, `api/action_handlers.go`.

### 3. `serialize_mode: queue`

Before: `SerializeMode` was parsed and validated but no code read it; `queue` behaved
exactly like `park`.

Now execute decisions carry the policy's serialize mode (with the lock ceiling,
`withDocumentBounds`), and in queue mode the lease goes through `policy.LeaseQueue`:
- **Durable FIFO per object.** `sage.lease_queue` entries hold the lease keys; a waiter is
  granted only when no earlier live entry shares a key and the lease is free. A newcomer
  never jumps the line (it queues when anyone waits for its objects).
- **Resumed after restart.** A request is identified by kind + intent + keys. A restarted
  sidecar resubmitting it (next cycle, next custodian tick, re-approved action) takes over
  its entry (stale heartbeat, other instance), keeping its place and its original deadline
  (`resumed_count`). A live duplicate is refused.
- **Bounded.** At most 8 waiting per object and 64 per database (`ErrLeaseQueueFull`); the
  wait is bounded per entry (deadline in the row). An abandoned entry expires at its
  deadline, swept in the same statement that stops counting it.
- **Timeout → park/refuse per policy.** All queue outcomes wrap `ErrLeaseConflict`: a
  self-initiated action parks (parked `ddl_conflict`, `evidence.lease_queue = timeout|full|
  duplicate`, no failed row, no retry/abandon count, no rate budget); an operator action is
  refused (`ErrTargetLeased`, 409).
- `park` (both shipped profiles' default) is unchanged.

Files: `policy/lease_queue.go`, `policy/lease_queue_wait.go`, `policy/lock_ceiling.go`,
`policy/types.go`, `policy/gate.go` (one call), `policy/gate_operator.go`.

### Schema and retention

`schema/debt_exec_migration.go` (`ddlDebtExec`, registered in `bootstrap.go`), idempotent:
change_lease identity columns, `sage.lease_queue` (+ unique waiting request, GIN on keys),
`retention_run.action_id`/`dry_run_id`. `retention/cleanup.go`: `lease_queue` purged after
`actions_days`, waiting entries never.

## Product decisions (and why)

1. **Retention deletes are capped by what was reviewed, cumulatively.** A dry run that
   described N rows authorizes at most 2N+100 deletions in total; then a fresh dry run must
   pass review. The deletion is irreversible, so pg_sage claims only the authority the
   evidence gives it. Cost: a high-inflow table deletes in review-sized steps (the new dry
   run records the larger backlog, so the step grows with it).
2. **Verify inside the transaction, not only after.** For an irreversible action the only
   useful verification is one that can still roll back. Post-commit verification confirms
   the durable record and marks the action.
3. **A typed lease covers name and OID; an index lease covers its table.** Changes to a
   table and its indexes are serialized (an index build and a reloption change on the same
   table park/queue instead of queuing on `ACCESS EXCLUSIVE`). Name keys are kept so the
   earned-autonomy concurrency count and any legacy name lease still see the writer.
4. **VACUUM takes a lease** (custodian freeze): it holds a table for minutes and blocks an
   operator's DDL; it is now a visible writer (also to M7's concurrent-action downgrade,
   CHECK-40). ANALYZE, settings and signals do not.
5. **Operators are refused, not parked**, with the holder named and HTTP 409: a person
   decided and is waiting; "retry in N minutes" beats a silent park.
6. **Queue bounds: 2 min for pg_sage, 30 s for an operator, 8 per object, 64 per database.**
   Constants (`policy.DefaultLeaseQueueConfig`), not config: the queue is off by default
   (`park`) and no install uses it yet. The operator bound also caps the slot an operator
   action holds while it waits (it takes its DDL slot before Apply, so a long wait could
   starve a custodian that holds the lease and needs a slot; 30 s bounds that).
7. **"Resumed after restart" means the entry keeps its place, not that pg_sage re-runs the
   SQL on its own.** Re-execution without the caller's context (verification, approval,
   HTTP caller) would be unsafe; every caller already resubmits (cycle, custodian tick,
   approved queue item), and the resubmission takes over its entry.

## Spec CHECKs and decision tests covered

- CHECK-40 (autonomy downgrades on concurrent action): custodian VACUUM and retention now hold
  visible leases; `LeaseHeld` is set from the actual acquisition.
- D1 memo T8 (park, no failed row) kept; T9 (queue: second waits, then executes) now real:
  `TestQueueModeSecondWriterRunsAfterFirst`, `TestSecondOperatorQueuedRunsAfterFirst`.
- D5 memo tests 6-10 kept passing through the pipeline; new end-to-end
  `TestRetentionContractChangedBeforeExecutionRefused`, `TestRetentionStopDuringWaitDeletesNothing`,
  `TestRetentionBoundExhaustedNeedsNewDryRun`.
- Rules: every action through `Executor.Apply` (retention was the last exception);
  irreversible class unchanged (`retention_delete` stays `not_reversible`, moderate, owner
  declaration required).

## Test Results

**Command:** `go test -cover -count=1 ./...` (repo root mounted, `golang:1.25`, PG17 :55477)
**Total:** 77 packages ok, 3 failed (none touched by this branch; see Failures), 0 skipped
packages. Touched packages also on PG14 (:55414), PG18 (:55418) and with `-race`.

| Package | PG17 | PG14 | PG18 | -race |
|---|---|---|---|---|
| internal/policy | 89.1% | 89.4% | 89.2% | pass |
| internal/executor | 83.4% | FAIL once (timing flake, see Failures); 4/4 on rerun | 83.4% | pass |
| internal/autonomy | 80.4% | 81.0% | 81.0% | pass |
| internal/schema | 81.6% | 81.6% | 81.6% | pass |
| internal/retention | 100.0% | 100.0% | 100.0% | pass |
| internal/api | 76.1% | 76.1% | 76.1% | pass |
| cmd/pg_sage_sidecar | 72.6% | 72.5% | 72.5% | pass |

Touched packages on PG17 (`-json`, separate run): **2774 passed, 0 failed, 0 skipped**.

### Skipped Tests (must be zero or justified)
None in the touched packages (0 of 2774 tests skipped).

### Failures (if any)
- `internal/collector` `TestCollectQueries_AppliesConfiguredStatementAndLockTimeouts`
  (statement timeout), `internal/sre` `TestModelProbe_FailedFinalReviewKeepsVerifiedFirstReview`
  and `internal/sre/action` `TestActionHandoffBlockedWhileMetadataIsDegraded` (bootstrap
  advisory-lock connection timeout) failed in the PG17 full run while six other agents'
  suites ran on the host. This branch does not touch these packages. Rerun alone on PG17: all three packages pass (collector 85.6%, sre 87.1%, sre/action 82.4%).
- `internal/executor` `TestApplyLockCeilingCapsCycleAnalyze` (pre-existing test, ANALYZE
  path, takes no lease) failed twice under host load (control wait 2.9 s < 3 s) and passed
  4/4 alone on PG14 and in the PG17 and PG18 package runs. Timing flake, not this change.

### Coverage Gaps (packages below threshold)
All touched packages meet the 70% business-logic threshold (lowest: cmd 72.5%).

### Bugs Found This Session
1. [BUG] `executor/validate.go` protected-schema check read the wrong object for
   `REINDEX ... CONCURRENTLY x` (took `CONCURRENTLY`), `CREATE INDEX ... ON ONLY x`,
   `ALTER TABLE ONLY x` and `VACUUM (A, B) x`, so `sage.*`/`pg_catalog.*` targets in those
   forms passed the check (the cgo parse-tree layer may still catch some). Fixed by one
   `statementTarget` used by the check and the operator lease;
   `TestValidateRejectsConcurrentReindexOfProtectedSchema`.
2. [BUG] `serialize_mode: queue` was never read (queue == park).
3. [BUG] Operator actions took no change lease; a custodian and an operator (or two
   operators) could change one table at once.
4. [BUG] Retention deletes ran outside Apply: no lease, no re-authorization after waits, no
   action record or verification.
5. [BUG, found while implementing] typed resolution first dropped an unqualified name it
   could not resolve, which would have let a finding run unleased; the existing
   `TestApplyParksLeaseConflictAndRecordsOtherLeaseErrors` caught it. Now an error.
6. [BUG, found while implementing] the in-transaction relation check assumed
   `pg_partition_tree` lists a plain table; it lists nothing, so every plain-table batch
   would have rolled back (fail closed). Caught by the end-to-end tests.

### Manual Checks Remaining
- CHECK-M1: MANUAL. Cases panel / Actions page show the 409 message for a leased target
  (API returns `{error: "...held by custodian (decision N)"}`; no UI change was made).

## Post-test audit

- **Mutation testing** (each mutant built, run, reverted): killed 19/20 on first pass. The
  survivor ("batch ignores the reviewed bound", masked by the pipeline's row cap) got
  `TestRetentionBatchStopsAtReviewedBound`; then killed. Mutants: drop the delete predicate
  (both places; the victims-only drop is an equivalent mutant since the recheck filters),
  skip predicate/relation/count verification (autonomy and executor), skip record matching,
  bound not cumulative, pipeline row cap and lock timeout ignored, identity recheck skipped,
  retention/operator take no lease, operator conflict parks, queue mode ignored, no OID key,
  index without its table, queue not FIFO, no resume, no depth bound, no deadline.
- Mutation testing also showed failing tests could hang (a held table lock under the
  cleanup that drops the table; unbounded queue waits); fixed so they fail fast.
- **Inputs not tested:** a lease target in a schema the sidecar role cannot see
  (`to_regclass` returns NULL, so it is treated as missing); a partition being attached or
  detached during a batch (the relation lock blocks DETACH; ATTACH CONCURRENTLY not tested).
- **Fakes:** executor retention tests use a stub batch (it writes a real `retention_run` row
  and the executor verifies against the database); the real batch runs end to end in
  `autonomy/retention_pipeline_test.go` against the real executor and standing gate.

## What is left

- Queue bounds are constants; expose them (e.g. `executor.lease_queue.*`) when an install
  opts into `queue`.
- `RollbackAction` (operator rollback) and the backend-cancel path take no typed lease.
- ALTER SYSTEM / ALTER DATABASE (settings) have no typed target; a custodian WAL bound and an
  operator setting change are not serialized.
- Leases still use `database_id` NULL (as before); meta-db fleet leases are per database
  schema.
- A crashed lease holder leaves its `change_lease` row active until its TTL (pre-existing).
