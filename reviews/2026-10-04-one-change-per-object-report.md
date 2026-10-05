# One self-initiated change per object until its verification concludes (2026-10-04)

Branch `claude/fix-one-change-per-object` (from `origin/master` = v1.10.0, `62a12d1b`).

## Why

lifeos, v1.10.0, 2026-10-04 UTC:

- 17:55 action 6407 `ALTER SYSTEM SET work_mem = '9MB'`; 18:34 action 6409
  `ALTER SYSTEM SET work_mem = '10MB'` from a new finding while 6407's evidence was still
  being judged.
- 18:45 action 6410 `CREATE INDEX idx_memories_live_status_type_quality ON public.memories
  (status, fact_type, quality_score) WHERE valid_to IS NULL AND deleted_at IS NULL AND
  quality_score IS NOT NULL`; 18:55 action 6411 `idx_memories_status_type_quality_current`
  (same keys, wider predicate, subsumes 6410) while 6410 was being verified.

Two overlapping changes on one object make both verdicts meaningless. Verification is what
earns pg_sage its autonomy, so this was a trust bug, not a tuning one.

## Design

Deterministic, in the policy gate (never from LLM text or evidence).

### Object identity (`internal/executor/change_object.go`, `internal/policy/change_table.go`)

- **GUC**: the setting name, whatever the scope. `ALTER SYSTEM`, `ALTER DATABASE x SET`
  and `ALTER ROLE x [IN DATABASE y] SET` of `work_mem` are one object (`guc:work_mem`).
  `RESET ALL` names none.
- **Table**: every other change is the table it touches (`table:public.memories`). The
  relation names come from the statement (`statementTarget`, the operator lease's reading,
  plus `statisticsTable` for CREATE STATISTICS) and the request's targets (a recommendation
  identity `schema.table|definition` counts as its table). `policy.ResolveChangeTables`
  resolves them with the lease resolver's catalog query (`resolveTargetsSQL`): an index is
  its table, quoted/unqualified spellings collapse to one canonical name, a qualified name
  with no catalog object keeps its own name.
- An in-flight change is matched by its statement, its targets **and its rollback SQL**:
  a dropped index is gone from the catalog, but the kept definition its rollback re-creates
  names the table (`CREATE INDEX ... ON public.memories`).
- Changes with no object identity (query hints, backend signals) are not serialized.

### In flight (`internal/executor/verification_wait.go`, one statement)

1. executed actions still watched: `sage.action_log.outcome IN ('monitoring', 'pending',
   'interrupted', 'rolling_back')` (served by `idx_action_log_outcome`);
2. executed actions whose `sage.action_outcome.verdict = 'pending'`, unless rolled back,
   reverted or failed (served by the new partial index `idx_action_outcome_pending`);
3. self-initiated execute authorizations younger than the hold horizon (2 x apply timeout,
   the budget's) with no action row and not released (`idx_decision_created`), excluding
   the request's own earlier authorization (same SQL and targets). This is what makes two
   proposals for one object in the same instant serialize: the gate already runs budget-
   spending authorizations inside one transaction holding the per-database advisory lock
   (W2-D), and the lookup reads in that transaction.

### Gate (`internal/policy/verification_wait.go`)

`GateConfig.Verification` (a `VerificationTracker`) is consulted last, after trust, mode,
budgets, windows and the ledger; it can only restrict:

- execute -> **park**, reason `awaiting_verification`, detail
  `awaiting verification of action 6407 (until 2026-10-04T18:10:00Z) on guc:work_mem`
  (each change in flight named; an unrun authorization reads "the change authorized by
  decision N"). `until` is the first window's end, or the hard deadline once that passed.
- queue_approval -> stays queued (a person decides), the wait appended to the detail.
- operator approval -> executes; detail `overrides pending verification of action N`,
  recorded (`Decision.VerificationWait.Overridden`).
- exempt (tracker not consulted): read-only, owner-declared (retention under its
  contract), and every `policy.BudgetBypassFor` case: rollback of pg_sage's own change
  (including the in-flight action's own rollback), revert of a created index, critical XID
  freeze, critical-disk space-freeing action.
- hard deadline: a wait whose deadline passed (inclusive) is **released** and recorded; it
  never parks forever. Hard deadline = executed_at + verification cap
  (`verify.window_max_minutes`, 72 h; `verify.drop_window_hours`, 168 h, for an index drop)
  + 1 h grace. An unrun authorization holds only for the hold horizon.
- unreadable in-flight state: self-initiated fails closed (`policy_unavailable`); an
  operator approval proceeds with the gap noted in the detail.

Resume is automatic: a parked candidate is evaluated every cycle (W2-D: once per cycle)
and executes once the verification concludes.

### Surfaces

- **Decision log** (`sage.decision`): reason `awaiting_verification`; evidence
  `verification_wait` (action/decision id, object, until, hard deadline),
  `verification_wait_detail`, `verification_override`, `verification_wait_released`.
  Reserved keys: request evidence can neither set nor erase them.
- **Approval card**: `verification_wait` (ids, objects, until, line) read live (not in the
  content hash), a `awaiting_verification` reason, the chat text and the UI line
  "Awaiting verification of action 6410 (until ...): approving overrides pending
  verification of action 6410". Wired in the API card loader and the chat notifier.
- **Prometheus**: `pg_sage_policy_parks_total{database,reason}` (every park, every reason,
  one per evaluation) and `pg_sage_verification_wait_releases_total{database,cause}`
  (`operator_override`, `hard_deadline`).
- **Docs**: `docs/configuration.md#one-change-per-object`; CHANGELOG `## Unreleased`.

### Files

- policy: `verification_wait.go`, `change_table.go` (new); `gate.go`, `types.go` (+2 fields
  each, additive).
- executor: `change_object.go`, `verification_wait.go`, `verification_wait_ledger.go` (new);
  `standing_policy.go` (wiring, ledger stamp), `budget_lock.go` (counter).
- schema: `verification_wait_migration.go` (new, checked index loop, idempotent), one line
  in `bootstrap.go`.
- approvalcard: `verification_wait.go` (new); `card.go`, `loader.go`, `assemble.go`,
  `text.go` (small additive edits). api: one line in `approval_card_handlers.go`. cmd:
  `park_metrics.go` (new), one line each in `prometheus.go`, `approval_cards_runtime.go`.
- web: `ApprovalCard.jsx` (WaitLine), dist rebuilt.

## Product calls (made by the AI-DBA lens; please confirm)

1. **A queued approval is still queued, not parked**: a person decides, and the card tells
   them what they would override. Parking it would hide the decision from them.
2. **One GUC across scopes**: `ALTER DATABASE/ROLE ... SET work_mem` and `ALTER SYSTEM SET
   work_mem` move the same queries, so they share one verdict window.
3. **Table granularity for everything else**: an index, a reloption, VACUUM, ANALYZE and
   extended statistics on a table all move the same plans. A partition and its parent are
   different tables (not linked); see open questions.
4. **'interrupted' and 'rolling_back' count as in flight** (a restart-interrupted monitor
   resumes; a rollback in progress is a change in progress).
5. **An index drop holds its table for its business cycle** (7 days by default) because its
   verification only concludes then (or earlier on a regression). Consequence: a
   performance index on a table where a redundant index was just dropped waits up to a
   week, unless an operator approves it (override recorded). See open questions.
6. **Operator authorizations are not holds**: they are never released (Apply releases only
   gate-authorized holds), so counting them held the object for the hold horizon even when
   the operator's change ran nothing. A person's change runs at once and holds its object
   through its action row from then on; the remaining gap (seconds, GUCs only, since tables
   are also leased) is accepted.
7. **Fail closed** on an unreadable in-flight state for pg_sage's own changes; an operator
   approval proceeds with the gap recorded.
8. **The park counter counts every park reason**, not only the new one: budget and DDL
   conflict parks were invisible to Prometheus too.
9. Unnamed (embedded) executors are not exported to `/metrics` (no `database=""` series).

## Test Results

**Command:** `go test -count=1 -v -cover -p 2 -timeout 3000s ./...` (golang:1.25,
`--cpus=2`, `--label owner=onechange`, PG17 `pgsage-ag6` :55476)
**Total:** 13257 passed, 4 failed (all load/environment, see Failures), 22 skipped.
After the run, two commits fixed what the cmd package re-run found (test fixture holds and
`database=""` metrics); the affected packages were re-run green (below).

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/policy | 88.4% |
| internal/executor | 88.2% |
| internal/approvalcard | 90.0% |
| internal/schema | 84.5% |
| internal/api | 79.4% |
| cmd/pg_sage_sidecar | 80.0% (re-run after the fixes) |

Other gates:

- **e2e** `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok (237.7 s, 78 passed, 13
  skipped, all LLM-gated).
- **Perf gate** `PG_SAGE_PERF_SCALE=small go test -tags=perfgate -run '^TestPerfGate$'
  ./cmd/pg_sage_sidecar`: **PASS**, 0 offenders. `sage.action_log` (20 000 rows) 0 seq
  scans, `sage.decision` (25 000 rows) 0 seq scans; `sage.action_outcome` is empty in the
  perf seed (seq scans read 0 rows, the planner's choice for an empty table, as in W1a).
  `TestOneChange_InFlightLookupUsesIndexes` proves the lookup has an index path on every
  table it reads (EXPLAIN with sequential scans priced out shows no Seq Scan on
  action_log, action_outcome or decision).
- **PG14** (:55414) policy, executor, approvalcard, schema, api: ok (88.4 / 88.2 / 90.0 /
  84.5 / 79.3%).
- **PG18** (:55418) same packages: ok; api failed `TestEventBrokerPollOncePublishesAction
  QueueChanges` once (event type "findings": the shared matrix database had other agents'
  findings activity); `-count=3` of that test and a full api re-run on PG18 passed (79.4%).
- **-race** policy, executor, approvalcard, schema: no data races; policy, approvalcard
  and schema ok; executor failed `TestApplyLockCeilingCapsOperatorAnalyze` once (its
  control lock wait took 1.9 s instead of about 4 s: a server-clock timing test under
  the race detector's load); `-race -count=3` of it plus every one-change test: ok.
- **golangci-lint** `run ./...`: 0 issues.
- **Web**: `npm ci`, vitest 76 files / 446 tests passed, `npm run build`, dist committed;
  run inside the node:22 container's own filesystem, no `node_modules` left in the repo.

### Skipped Tests (must be zero or justified)
None in touched code. All 22 environment-gated: live cloud provisioning (AWS RDS, Cloud SQL,
Lakebase, 3 AgentDB gauntlets, 2 Azure), live LLM (`TestChatLive_RealProvider`,
`TestChatWithToolsLive_RealProvider`, `TestTier2Live_RealGemini`, `TestLiveModelArm`),
container failover/restart/promotion fixtures (3), PgBouncer (3), `TestPGIncidentBench`
(own CI step), `TestGeneratePlanFixtures` (regeneration only), `TestRCAChildProcessFixture`
(helper process), `TestResolveLogDir_AbsoluteWindows` (Windows only).

### Failures (if any)
Full run, none in touched code:
- `cmd/pg_sage_sidecar TestMetaReconcileAddsOutOfBandRecordOnce`: dial timeout to the test
  database under load; package re-run ok.
- `internal/collector TestCollectQueries_AppliesConfiguredStatementAndLockTimeouts`:
  statement timeout under load; re-run ok (88.3%).
- `internal/sre/probes` (a different probe test each run: deadline_exceeded): passes when
  run alone (93.1%). Another agent owns probe deadlines.
- `internal/mcp TestToolReferenceDocsMatchSchemas`: pre-existing on Windows checkouts:
  `docs/mcp.md` is CRLF under `core.autocrlf=true` and the marker constant ends in `\n`.
  Untouched here; passes on Linux CI checkouts.

The cmd re-run (before the fixes) also failed `TestInstallGrandfathersTheRampAutonomy`
(fixed, bug 4), `TestValueMetricsFleetModeEmitsPerDatabase` (fixed, bug 3) and
`TestFleetReloadAddsDatabaseWithFullRuntime` (the known load-sensitive fleet-reload family);
after the fixes the whole package passed.

### Coverage Gaps (packages below threshold)
All touched packages meet their thresholds (lowest: api 79.4%). Untouched utility packages
below 70% are unchanged (`cmd/reset_admin_for_test` 50.0%, `cmd/create_admin` 51.5%,
`cmd/sigstore_trusted_root` 56.1%, `internal/testsupport/pgssepoch` 58.3%, `sre-bench`
65.7%; utility floor 50% met).

### Bugs Found This Session
1. [BUG] (own code, caught by the phase-1 tests) CREATE STATISTICS forms the strict
   pg_sage parser refuses named no table; the verifier's `statisticsTable` reading is now
   also used.
2. [DESIGN] operator authorizations are never released by Apply, so counting them as
   holds held the object for the hold horizon after a change that ran nothing (found by
   the full executor run: two coverage tests parked behind an earlier test's operator
   authorization). Excluded (product call 6), test `TestOneChange_OperatorAuthorizationIsNotAHold`.
3. [BUG] (own code) parks of an unnamed executor were exported as `database=""`.
4. [TEST] cmd tests evaluate custodian proposals on `public.orders` and never run them;
   with holds, a later test found the table held. The two helpers now name their own
   table.
5. [TEST LOGIC] (own tests) helper name collisions, the policy fixture's fake validator
   (admits only CONCURRENTLY), and the concurrency test reading the winner before knowing
   it; fixed in `4bf03808` without weakening any assertion.

### Mutation testing (object matching and exemptions)
23 mutants, each run against the tests named for it on PG17. **23/23 KILLED.**

| # | Mutant | # | Mutant |
|---|---|---|---|
| M01 | rollback/revert/emergency bypass not exempt | M13 | pending verdict branch ignored |
| M02 | owner-declared not exempt | M14 | rolled-back action with a pending verdict still holds |
| M03 | hard deadline exclusive | M15 | 'interrupted' not in flight |
| M04 | execute not parked | M16 | no grace on the hard deadline |
| M05 | operator override not recorded | M17 | drop judged like a create |
| M06 | unreadable state fails open | M18 | settings carry their targets ("instance") |
| M07 | expired wait still parks | M19 | spoofed evidence kept |
| M08 | any GUC matches any GUC | M20 | every verdict counted as a park |
| M09 | an index is not its table | M21 | card shows released waits |
| M10 | rollback SQL not used for identity | M22 | ALTER DATABASE/ROLE scope ignored |
| M11 | unrun authorizations ignored (concurrency) | M23 | operator authorizations hold |
| M12 | own earlier authorization not excluded | | |

### Post-test audit
- *Inputs not tested*: a partition vs its parent (treated as different tables, product
  call 3); a table renamed while a change is in flight (names are canonical, not OIDs, so a
  rename separates them; OIDs would break on a drop and re-create); clock skew between the
  sidecar and the database (deadlines are computed in Go from `executed_at`).
- *Assertions that pass when broken*: none found; every rule is killed by a mutant above,
  and every DB test first proves its control (`requireExecutable`: the same request would
  execute without the in-flight change).
- *Fakes hiding failures*: the policy tests use a fake tracker; every rule is also
  exercised through the real gate, ledger and Postgres (`verification_wait_*_db_test.go`),
  including both lifeos sequences and a RunCycle park-then-resume.

### Manual Checks Remaining
- CHECK-M1: MANUAL, the approval card's wait line in a browser (component tests only).
- CHECK-M2: MANUAL, after the next lifeos upgrade: a second work_mem finding during a
  verification is parked with `awaiting_verification`, and `pg_sage_policy_parks_total`
  shows it.

## Open questions

1. **Drops hold their table for a week** (product call 5). Should a hygiene drop's
   business-cycle wait block performance changes on the same table, or should a drop hold
   the object only for its first window (`trust.rollback_window_minutes`) with the
   soft-drop re-create still watching the cycle?
2. **Partitions**: should a change to a partition wait for one on its parent (and vice
   versa)? Today they are separate objects.
3. The tuning agent (#115) dedupes subsumed proposals at the source; this gate rule is the
   backstop when two still arrive.

## Follow-up: owner decisions on PR #122

All four product calls listed for confirmation (1, 2, 7, 8) were confirmed as made. Open questions answered and
implemented (tests first, `5ef639a5`; implementation `fix(executor)` after it):

1. **A drop holds its table only until its first window concludes**, not its 7-day cycle:
   the wait ends `trust.rollback_window_minutes` after the drop ran (the monitor's first
   window; the drop monitor records no earlier verdict, so the first window's end is the
   first point a verdict could be due). The release is recorded on the decision
   (`verification_wait_released[].release_reason = "drop's first window concluded"`), in
   the detail, as `pg_sage_verification_wait_releases_total{cause="drop_first_window"}`,
   and on approval cards ("Wait released: verification of action N: drop's first window
   concluded (the drop's soft-drop monitoring continues over its business cycle)"). The
   drop's own monitoring and soft-drop re-create are unchanged. `VerificationWindows`
   lost its now-unused drop window field.
2. **A partition tree is one object for index, extended-statistics and reloption
   changes**: `policy.ResolveChangeTables` returns each table's `pg_partition_root`
   (pg_inherits; NULL outside a tree), and such changes also meet on
   `partition_tree:<root>`, so a change to the parent, a partition or one of their indexes
   waits for an in-flight change to another, both directions. VACUUM, ANALYZE and
   settings keep their own object. The root comes from the catalog in the same single
   resolution query (catalog indexes only; no sage table read added).

Tests: `verification_wait_partition_db_test.go` (drop parks 30 s before its first-window
end and is released 30 s after, the drop still `monitoring`; parent -> partition index and
partition reloption; partition index (REINDEX of a partition's index) -> parent index and
parent statistics; ANALYZE of a partition and an unrelated table unaffected; the root
resolution itself), policy boundary at the first window's end (inclusive), release
causes, ledger `release_reason`, counters and the card line. Requirement-driven test
changes: the drop rows of `TestWaitTimesFollowTheVerificationWindows` (168 h -> first
window) and the dropped-index identity test's age (1 h -> 5 min, inside the first window).

Mutation check of the new rules: 9 mutants (drop held to the cap, drop release reason
lost, drop release counted as hard deadline, card hides the drop release, no partition
root, partition scope ignored, VACUUM/ANALYZE partition-scoped, index not its table under
the new query, plus M01/M10 re-run): **all killed**.

Merged `origin/release/v2.1.0` (`ff068cbf`): CHANGELOG conflict resolved by putting this
bullet first under the release branch's `### Fixed` (released sections unchanged); dist
conflicts taken from the release branch, then the dist rebuilt from the merged sources
(vitest 83 files / 483 tests passed).

Follow-up test results (after the merge, golang:1.25, `--cpus=2`, `-p 2`, PG17 :55476):
policy 88.4%, executor 88.3%, approvalcard 90.4%, schema 84.5%, api 78.9%,
cmd/pg_sage_sidecar 79.8%: all ok, no failures, no skips in touched packages. Small perf
gate: **PASS**, 0 offenders; `sage.action_log` and `sage.decision` 0 seq scans
(`sage.action_outcome` empty in the seed, 0 rows read). golangci-lint: 0 issues.
