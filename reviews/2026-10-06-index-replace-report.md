# Report: executor "replace" action (index_replace), v2.3.0

Branch `claude/v23-index-replace` (from `origin/master` 72646ab1). Plan:
`reviews/2026-10-06-index-replace-plan.md`.

## What was built

- **Statement pair** (`internal/optimizer/indexreplace.go`): a replacement travels as
  `CREATE INDEX CONCURRENTLY new ...;\nDROP INDEX CONCURRENTLY old;` with the undo
  `CREATE INDEX CONCURRENTLY <old definition>;\nDROP INDEX CONCURRENTLY IF EXISTS new;`.
  The finding, the approval queue row, the card and its content hash (approve-expecting)
  therefore bind to both statements. Nothing ever runs the pair as one string.
- **Executor** (`internal/executor/index_replace*.go`, `contract_replace.go`):
  - `ParseIndexReplace`: both pairs split; every statement passes `ValidateExecutorSQL`;
    the new index is named, non-unique, no `IF NOT EXISTS`, on a schema-qualified table; the
    old index is in that schema and is not the new one; the undo re-creates exactly the old
    index on that table and drops exactly the new one.
  - Contract `replace_index` (moderate, reversible, `approval_required` guardrail, drop kind
    derivable, change class index). `findingRefusal` refuses it in the executor cycle; it
    runs from the approval queue or Take Action (`ExecuteManual` and `RunApprovedAction`
    route to `ExecuteIndexReplace`).
  - Preflight (catalog): the old index backs no constraint and enforces no uniqueness
    (`pg_constraint.conindid`, `indisunique/primary/exclusion`), keeps its recorded OID and
    definition (also equal to the undo), sits on the table, is subsumed by the new index,
    and every foreign key it supports stays supported.
  - Two durable steps under one `Apply` (table + old index operator lease held across both):
    record `creating` → build → check valid/ready → `created` → re-check the old index →
    `dropping` → drop → `completed`. Failed build: INVALID remnant dropped, `create_failed`,
    nothing dropped. Failed drop: `drop_failed`, action outcome `partial`, both indexes stay.
    Old index changed between the steps: the build is undone (`rolled_back`).
  - `ResumeIndexReplaces` (every cycle, not on replicas): `resumeStepFor(state, catalog)`
    decides; the drop resumes under a fresh operator-approved authorization and lease
    (refused: retried next cycle); a committed drop completes; a missing/invalid new index
    restores the original state; undo steps continue idempotently.
  - Undo (`RollbackIndexReplace`, `RollbackAction` routes to it): re-create the old index
    (dropping an INVALID remnant of its name first), then drop the new one only when its
    OID is the one built; otherwise `rollback_failed` and the index is kept.
  - Verification (`index_replace_verify.go`, `index_replace_act.go`): targeted queries
    (finding queryids) and guarded queries (the table's most-called statements, i.e. the
    old index's users) against one frozen baseline. Phase judging: targets or guarded
    regressed, or a hint names the old index → full rollback; improved → kept (credited);
    no gain → rollback; no verdict at the cap → rollback (unverifiable). Phase watch (until
    `verify.drop_window_hours`): guarded regression or hint → soft-drop re-create of the old
    index only (`old_restored`, `after_state.soft_drop`). Phase is durable; restarts resume.
- **Schema**: `sage.index_replace` (new idempotent migration, registered once; no FK to
  `action_log`, by the existing pattern). Retention: rows with nothing to resume or watch
  age out with `actions_days`.
- **Earned** (`class.go`, `selfinit.go`, `service.go`, `trust_view.go`, `grandfather.go`):
  class `index_replace` (reversible, cap L2), tuning family, outcome class `index_replace`.
  Composite classes: the cap binds the self-initiated pair too; the level is the lowest of
  the components' (`index_create`/tuning, `index_drop`/hygiene) and its own row; not
  grandfathered; the Trust view applies the same rule as per-pair reads.
- **Tuning**: a candidate that subsumes exactly one existing index becomes a
  `tuning_index_replace` finding with the pair, undo and the old index's OID/definition.
  Still refused: subsuming an in-flight index or several existing ones, unknown OID,
  unique/invalid old index, old index proposed for a drop this cycle. Queued/open
  replacements count as in flight by their build; a replacement whose old index is gone is
  stale. Prompt rule 6 updated. Calibration lists include the class (agent and API).
- **Facts**: `replace_index` is DDL (migrations-owned table → source fix with the
  two-statement migration and down) and an index drop (append-only → keep).
- **Approval card**: SQL and rollback are the pairs; lock text names the SHARE UPDATE
  EXCLUSIVE build and the brief ACCESS EXCLUSIVE on the old index; targets include the old
  index; legacy untyped rows are typed from the pair.
- Docs: `docs/configuration.md` (tuning agent), CHANGELOG `## Unreleased`.
- No new config keys (windows reuse `trust.rollback_window_minutes`,
  `verify.window_max_minutes`, `verify.drop_window_hours`), so `key_classes.txt` and the
  generated config metadata are unchanged.

## Product decisions

1. **Cap L2, never unattended.** The brief's default; the class model caps reversible
   self-initiated classes at L3 by reversibility, but a new composite with partial states
   and no live record has earned nothing. The cap binds the self-initiated pair
   (only composites use this), and the contract's approval guardrail queues it even
   without a ledger. Level = min(components, own row) so a replace card appears exactly
   where creates and drops are already handed to a human.
2. **Name-based drop with identity re-check.** v2.1's superseded-cleanup path refused to
   drop because `DROP INDEX CONCURRENTLY` cannot bind to an OID. The replace re-reads the
   old index's OID and definition immediately before the drop under the table lease; the
   residual race is external DDL in that instant, the same as any name-based drop, and the
   kept definition re-creates it.
3. **Crash between steps resumes forward** (the operator approved the pair), re-authorized
   as an operator approval with a fresh lease; refusals (e-stop, facts) leave both indexes
   (safe) and retry.
4. **No gain rolls back** (as for `index_create`): a wider index without a measured gain
   costs more writes than the one it replaced.
5. **Soft-drop miss after a kept verdict re-creates only the old index**; the targets'
   verified verdict stands and the miss is recorded on the action.
6. **Composites are not grandfathered** (they never ran under the time ramp).

## Test Results

**Command:** `go test -count=1 -p 2 -cover ./...` (PG17, golang:1.25 in Docker, `--cpus=2`)
plus touched packages on PG14 and with `-race`, and `go test -tags=e2e ./e2e/`.

**Total (touched packages, PG17, `-json`):** 4070 passed, 0 failed, 0 skipped.
Full suite PG17: 107 packages ok; 4 failures, none in replace code (see below).
PG14 touched packages: 10/10 ok. `-race` touched packages: 10/10 ok, no races.
e2e: ok (109.8 s). `go build ./...`, `go vet ./...`, golangci-lint: 0 issues.

**Coverage (PG17 / PG14):** executor 87.5% / 87.6%, earned 90.2% / 90.2%, tuning 91.1% /
91.2%, approvalcard 90.6% / 90.6%, facts 86.5% / 86.5%, optimizer 91.2% / 90.9%, schema
84.5% / 84.5%, retention 86.9%(run with fix: 87.3%) / 87.3%, verify 89.5% / 89.9%, api 79.2%
/ 79.1%. All packages meet coverage thresholds.

### Skipped Tests
- None in the touched packages.

### Failures (full PG17 run)
- internal/retention: TestRetentionRules_CoverEveryTimeSeriesTable: the new
  `sage.index_replace` had no purge rule. Fixed (rule added); package passes on PG17/PG14.
- internal/executor: TestApplyLockCeilingCapsCustodian: timing assertion (500 ms ceiling
  took 4.3 s) under full-suite load; passes alone and in the PG14 and -race runs. Not
  related to this change.
- internal/mcp: TestToolReferenceDocsMatchSchemas: known CRLF-checkout failure (lessons).
- internal/sre/runbook: fixture cleanup could not connect (Docker DNS timeout) after its
  tests passed; passes on rerun.

### Bugs found this session
1. [BUG] tuning `overlap` flattened indexes to definitions, losing the name and identity
   needed to replace one; restructured (`existingOverlap`).
2. [BUG] retention: the new table lacked a purge rule (caught by the coverage test).
3. [BUG] earned: the Trust view read levels in bulk and disagreed with per-pair reads for
   the composite (caught by TestTrustViewAgreesWithPerPairReads); shared `composedLevel`.
4. [BUG] earned: grandfathering would seed the composite (caught by the grandfather tests);
   composites are excluded.
5. Test bugs (fixed, explained in commit 563916a0): two fixtures in one test deadlocked on
   the cross-package lock; readiness expectation was wrong.

### Post-test audit
- Mutation checks (each made the named tests fail): neutral verdict kept; unattended
  refusal removed; composite cap raised to L3; resume of an invalid build drops the old
  index; tuning replacing two indexes; unique new index accepted; constraint check
  removed (DB); new-index identity check in the undo removed (DB, test added in audit).
- Not covered by a live test: an exclusion-constraint FK edge where the new index stops
  supporting a foreign key (`supportsForeignKey` is unit-tested; subsumption makes the
  live case unreachable); a crash *during* `DROP INDEX CONCURRENTLY` that leaves the old
  index INVALID (decision table covers it: the drop is re-issued); the background watch
  loop's timer (checks are tested by calling `checkIndexReplace` directly).
- HypoPG measures the wider index with the old one present, not hidden; a
  `hypopg_hide_index` what-if for replacements is future work.

## What is left
- Web: the Actions list shows the raw type `replace_index` (no label); approval cards are
  data-driven and show both statements.
- HypoPG what-if with the old index hidden.
