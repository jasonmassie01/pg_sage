# Executor "never evaluates" verified index on lifeos: report (2026-10-03)

Branch `claude/fix-executor-skips-verified` (from `origin/master` e9feff49, v1.8.4).

## Summary

The executor does evaluate finding 18569 every cycle. The brief's premise (no decision after
18:54) is wrong. The standing gate parks the finding as `blast_radius_exceeded`. The policy's
24-hour blast-radius window held **21 distinct targets against `max_tables_per_window: 20`**.
v1.8.3 used it up at 17:49-18:00, mostly with duplicate-index drops in leaked `test_*`
schemas. The finding is parked in `documentDecision` before `Apply` runs, so #91's
stale-approval supersede path is never reached.

Waiting is the correct safety behaviour. The limit itself had two counting bugs, now fixed:

1. **Off by one.** The gate parks when `TablesInWindow > limit`, but the usage counted only
   the tables already touched, without the table the request would touch. So
   `max_tables_per_window: 20` let a 21st table through. That is how lifeos reached 21/20.
2. **An index identity counted as a separate table.** `public.events|btree(user_id,started_at)`
   and `public.events` were two "tables". So every later index on a table already counted
   used up budget a second time.

## Evidence (read-only lifeos queries and `docker logs`)

| Hypothesis | Verdict | Evidence |
|---|---|---|
| (a) window budget exhausted / ordering | **Confirmed, as budget exhaustion** | Decision **392087**: feature `index`, target `public.events\|btree(user_id,started_at)`, verdict `parked`, reason `blast_radius_exceeded`. Created 19:05:05Z, `last_seen_at` 19:15:41Z, `repeat_count` 4. Same for memories (392085). The gate's own usage query on lifeos returns 23 self-initiated changes and **21 distinct targets** in the last 24 h (action_log 6383-6405: 17 `DROP INDEX` in `test_*` schemas, the covering index on `public.graph_nodes`, `ANALYZE public.audit_log`, failed builds on `public.memories`/`public.graph_nodes`, `work_mem` = `instance`). 21 > 20, so every non-read-only autonomous change is parked. The loop does not stop and ordering is not the cause: every candidate is evaluated and parked (16 `index` + 795 `fk_index` + 1 `config_guc` parked decisions since 18:54). |
| (b) pending queue item skips the finding before the gate | Refuted | `processFinding` has no pending-queue check before the gate. `pendingApproval` runs only inside `queueFinding`, after a `queue_approval` verdict. Decision 392087 shows the gate ran. |
| (c) cascade guard / in-memory cooldown | Refuted | `recentActions` was empty after the 18:54 restart. No `cascade guard: skipping` line in the logs. The gate ran (392087). |
| (d) candidate layer filters pending proposals | Refuted | `ListActionable` selects `state IN ('proposed','approved')` or due `failed` and does not join the queue. Recommendation 1512 (finding 18569) is `proposed`, revision 1, and reached the gate. |

Why the brief saw no new decision: the ledger dedupes repeated non-execute verdicts by
fingerprint. The fingerprint includes the verdict, so the change from `queue_approval` (326942,
v1.8.3) to `parked` (v1.8.4) opened a new row, **392087**. Row 326942 stopped at 18:52:32.

Why the verdict changed at 18:54: v1.8.4 (#91) feeds the finding's current, verified evidence
to the gate, so the approval requirement went away. `documentDecision` checks usage limits
right after the approval check, and the window was full.

## Fix

`sidecar/internal/executor/policy_runtime.go`: `standingUsage` now returns the window as it
would be if the request ran. That is the distinct tables of executed self-initiated actions
in the last 24 h plus the request's own `TargetObjs`. Both sides count a target by its part
before `|`, so a recommendation identity counts as its table. The gate is unchanged
(`TablesInWindow > limit` parks), and the result is:

- the window never holds more than `max_tables_per_window` tables (21/20 is no longer possible);
- a change on a table already counted does not widen the window (the rate limit still bounds
  the number of changes);
- a change on a new table waits until there is room. The parked decision records
  `blast_radius_exceeded`, as before. `max_tables_per_window: 0` now admits no table. Before,
  the first one got through.

`sidecar/internal/policy/types.go`: a doc comment on `LimitUsage.TablesInWindow` (no code).

Safety rules unchanged: blast radius, rate limit, leases, windows, approval, the trust ramp
and the emergency stop work exactly as before. The fix only makes the table limit stricter
at its edge, and stops it from counting one table twice.

### What happens on lifeos (v1.8.4 still running; nothing was written there)

- The window frees on **2026-10-04 at about 17:49:20 UTC**, when actions 6384-6398 age out.
  With v1.8.4 the events index runs in the first executor cycle after 17:49:19.95. With this
  fix it runs after 17:49:20.12, when `instance` also ages out. The first cycle after either
  time sees 7 tables in the window, so the index runs. Candidates are taken in recommendation
  id order, and 1512 is older than every pending drop (1531+).
- Queue items 2 and 3 expire at 05:57 UTC that morning, before the window frees. An expired
  item is left alone and the change still runs (`TestStaleApprovalExpiredIsUnchangedAndChangeRuns`).
- **To run it now:** approve queue item 3 (and 2 for memories) in the UI before 05:57 UTC.
  The operator path is not limited by the self-initiated blast radius, by design. This is
  the user's decision; I did not touch lifeos.

## Are the `test_*` drops counted sensibly? Should the index get priority?

- **Counting:** each `DROP INDEX` is counted under the index's name, not its table. On lifeos
  the 17 drops hit 17 different tables (3 per leaked schema), so the number is about right.
  Each one is a real change to a distinct table and should count. `ANALYZE` and failed builds
  also count. That is the conservative choice and I kept it. Two oddities I left alone:
  ALTER SYSTEM counts as a table named `instance`, and a drop counts under the index name,
  so two drops on one table count twice. Resolving a drop to its table needs the catalog
  when the decision is made, and history rows cannot be resolved after the drop. Not small.
- **Priority:** the budget is first come, first served across classes. On 10-03 it went to
  hygiene drops in leaked schemas while the index was still unverified, so no ordering would
  have changed that day. Within a cycle, the id order already puts the older index ahead of
  newer drops. **Recommendation (not implemented, not small):** keep a share of the window
  for evidence-backed performance changes. One way is to cap hygiene classes
  (duplicate/unused-index drops) at about half of `max_tables_per_window`. Another is to sort
  `ListActionable` by expected impact (verified improvement) when the window is nearly full.
  This is a policy-schema change and should go through the spec.
- **Root cause on the lifeos side:** the lifeos test suite leaks `test_*` schemas, and new ones
  keep appearing (18:10 batch). They generate an endless stream of drop proposals, and 795
  `fk_index` proposals from the custodian. Dropping the leaked schemas in lifeos's test
  teardown would remove the competition. That is the user's code, so I recommend it and did
  not change it.

## Tests (written first, commit 28d9f7be; failed before the fix)

- `executor/stale_approval_budget_test.go` uses an isolated database per test, so the test owns
  the whole 24-hour window:
  - `TestBudgetExhaustedVerifiedIndexWaitsThenSupersedesStaleApproval`: the lifeos state.
    21 executed targets (17 test-schema drops + graph_nodes, audit_log, memories, instance),
    an index queued as unverified and then verified, policy = lifeos's. At 21/20 and at 20/20
    it is parked, recorded as `blast_radius_exceeded`, the queue item stays pending, and
    nothing runs. At 19/20 the stale approval is superseded and the index is built once. The
    window then holds exactly 20. **Before the fix: failed at 20/20, executing the 21st table.**
  - `TestBlastRadiusRetouchingAWindowTableDoesNotWiden`: limit 2, window = the same table
    under an identity and under its name, plus one other table. The index builds.
    **Before: parked (3 > 2).**
  - `TestBlastRadiusNewTableBoundaries`: a new table at exactly the limit parks, and a zero
    limit in an empty window parks. **Before: both executed.**
- `executor/policy_usage_window_test.go` (`standingUsage`): request tables counted, already
  counted ones and identities not, duplicates once, empty target ignored. Operator actions
  excluded. 23h59m inside, 24h01m outside. Multi-target decision. Distinguishable errors (no
  pool / failed read). **Before: 3 of 4 failed** (the error test passed both ways).
- The #91 fixture now takes the policy document as a parameter (`newStaleFixtureOn`).
  Existing callers are unchanged.

## Test Results

**Command:** `go test -p 2 -count=1 -cover ./...` (golang:1.25, `--cpus=2`, PG17 `pgsage-ag5` :55475)
**Total:** 89 packages. 88 ok. `internal/executor` failed once, see below; it was fixed and
passes on re-run (1283 tests in executor + policy under `-v`: 1283 passed, 0 failed, 0 skipped).
**Coverage (touched packages):** `internal/executor` 86.2%, `internal/policy` 89.1-89.3%.
**Cross-version + race:** `go test -race -count=1 -cover ./internal/executor/ ./internal/policy/`
passes on PG14 (:55414) and PG18 (:55418).
**E2E:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok (198.7 s), 78 passed, 0 failed,
13 skipped.
**Lint:** golangci-lint v2.11.4 `run ./...`: 0 issues.

### Skipped Tests
- e2e: 13 live-LLM tests (`TestLLM*`, `TestTunerLLM_*`, `TestOptimizerMultiQueryConsolidation`).
  Skipped because `SAGE_LLM_API_KEY` is not set, which the rules require for live tests.
- executor and policy: none. (The full `./...` run was without `-v`, so skips in other
  packages were not listed.)

### Failures (if any)
- `internal/executor`: `TestExecuteRetentionBatchErrorRecordsFailure`. The fix made it
  `parked/blast_radius_exceeded`. It runs against the shared test database with the unattended
  profile (limit 20). At its point in the package order, the package's earlier tests have left
  exactly 20 tables in that database's window, so it had only passed because the old count let
  a 21st table through. The test is about retention batch errors, not the window. Its helper
  `withSerializeGate` now raises the window limits, as the #91 fixture already did, and the
  window itself is tested on isolated databases (commit message explains). Re-run: pass.
- `TestCustodianWALBoundCreditsAMeasuredDiskNearMiss` failed once (`statement_timeout`
  probing `replication_slots`). That was during a package run in parallel with the full suite.
  It passed in every other run, is unrelated to the change, and is load-sensitive.

### Coverage Gaps
All touched packages meet thresholds (executor 86.2%, policy 89.1%).

### Bugs Found This Session
1. [BUG] `executor/policy_runtime.go` `standingUsage`: the window was counted without the
   request's table, so `max_tables_per_window` admitted limit+1 tables (lifeos 21/20). A zero
   limit admitted the first table.
2. [BUG] Same query: an index identity `schema.table|btree(...)` counted as a separate table.
3. [TEST BUG] `withSerializeGate` relied on the shared test database's window staying under
   the limit.

### Mutation testing
Each mutation was applied to `standingUsageSQL` and the window tests re-run. Every mutation
was caught:
- request union removed: 4 tests fail (usage, edges, lifeos repro, boundaries);
- identity normalization removed: 3 tests fail (usage, multi-target, re-touch);
- normalization only on the window side: 3 tests fail.

### Post-test audit
- **Inputs not tested:** targets with whitespace (handled by `btrim`, and upstream already
  trims); a quoted identifier containing `|` (would be cut at the `|`, which only makes the
  count stricter or merges two names; the optimizer identity format already uses `|` as its
  separator, see `analyzer/optimizer_mapping.go`).
- **Weak assertions:** none. Each test asserts verdicts, queue state, action counts, index
  existence and the exact usage numbers.
- **Fakes:** index verification is faked (`fakeIndexVerifier`), as in #91. The gate, the
  ledger, the action queue, the recommendation store, the usage SQL and the DDL are real
  Postgres.

### Manual Checks Remaining
- MANUAL: on lifeos after 2026-10-04 ~17:50 UTC (or after the fix is deployed), confirm that a
  decision `execute/authorized` for `public.events|btree(user_id,started_at)` exists and that
  `idx_events_user_started_at` exists.

## Other observations (not changed)
- On lifeos, each parked candidate is authorized twice per cycle (decision ids skip one between
  candidates, and `repeat_count` rises by 2 per 10-minute cycle). In the test executor it is
  once per cycle. Harmless (deduped), cause not found. Worth a look.
- `LimitUsage.RowsRewritten` is never filled in, so `max_rows_rewritten` is not enforced.
- Usage limits are checked before the trust tier and window. So `blast_radius_exceeded` does
  not mean the action would otherwise run.

## Product decisions
- The index waits for the window. I did not loosen the limit to let it run today. The
  limit is the operator's safety bound, and lifeos really did touch 21 tables.
- I made the limit stricter where it was wrong (off by one) and fairer where it counted one
  table twice. Both follow from what `max_tables_per_window` says.
- I did not implement priority for performance actions over hygiene ones. It is a policy
  design change for the spec, not a small fix.

## What is left
- Lifeos: approve queue items 3/2 before 05:57 UTC 2026-10-04 to build the indexes now, or
  let them run when the window frees (~17:50 UTC). Fix the lifeos test suite's `test_*`
  schema leak.
- Spec work: a window share reserved for performance actions; resolve drop targets to tables;
  enforce `max_rows_rewritten`.
