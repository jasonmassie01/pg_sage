# Legacy optimizer index advice: what-if gate, stale rollbacks, withheld retries

Date: 2026-10-03. Branch `claude/fix-legacy-index-whatif` (merged with `origin/master` after
v1.8.4, PR #94). Evidence came from the lifeos dogfood on v1.8.3 (autonomous trust).

## What happened on lifeos

| Action | Finding | What went wrong |
|---|---|---|
| 6383 | 12905 `covering_index`, created 2026-06-11 | Built `idx_graph_nodes_node_type_name_covering` unattended. The finding had no `what_if_verdict`, and the v1.8.2 what-if gate only checked category `missing_index`. |
| 6385, 6399 | 18007 `partial_index` | `rollback_sql` dropped `idx_memories_active_partial` while the DDL creates `idx_memories_active_query_opt`. The identity check withheld it correctly, but it retried every cycle and each withheld row failed the ledger self-audit (`missing_verification`). |
| 6400 | legacy `graph_nodes (node_type, name)` | Rollback named another index (withheld). The index was also already covered by the index from 6383. |

## Root causes

1. **Categories (a).** Before v1.8.0 (`05c3479a`), the optimizer stored the LLM's own label as the
   finding category. The prompt offered `missing_index|covering_index|partial_index|composite_index`,
   but the LLM wrote the label freely. v1.8.0 fixed the category to `missing_index`, but old rows
   kept their labels. Two things only looked at `missing_index`: the what-if gate
   (`requireApprovalWithoutWhatIf`) and the re-verification of open recommendations
   (`optimizer/openrecs.go`, PR #85). Both missed those rows. No producer emits the legacy
   categories any more, so the analyzer never resolved them. On 2026-10-02, `MigrateLegacy`
   turned them into live recommendations.
2. **Stale rollback, finding 18007 (c).** The stale value was in `sage.findings.rollback_sql`.
   Releases before v1.8.0 stored the LLM's `drop_ddl` as `rollback_sql`. Before `6e9d9f92`
   (C04, 2026-09-26), the refresh path also rewrote `recommended_sql` and `detail` without
   `rollback_sql`. Detail `drop_ddl` was refreshed, so it shows the correct name. The rollback
   column kept the old one. Sibling finding 12887 has the same bad pair. The finding was last
   refreshed on 2026-09-12, before C04, so it was never fixed. On 2026-10-02, `MigrateLegacy`
   copied the stale pair verbatim into `recommendation_revision.inverse_sql` (recommendation
   546, revision 1). The executor builds the finding's rollback from that column
   (`findingFromCandidate`). Read-only queries confirmed this: the findings row, the revision
   and `action_log` 6385 all carry `idx_memories_active_partial`.
3. **Retries.** `exceedsMaxRetries` stops a finding only after 3 failed rows. Nothing remembered
   that the refusal depended only on the finding's content.
4. **Coverage.** `checkDuplicate` matched exact shapes only. `(node_type, name)` beside
   `(node_type, name) INCLUDE (id)` passed the check, for current findings as well as legacy ones.

## What was built

| File | Change |
|---|---|
| `internal/executor/whatif_gate.go` | Every background `CREATE INDEX` needs a verified what-if verdict unless its category is a deterministic rule. Only `missing_fk_index` is a rule, and only when the finding has no optimizer markers. Unknown categories fail closed. The decision is documented in the code. |
| `internal/optimizer/legacy_categories.go` | `Categories()` returns `missing_index` plus `covering_index`, `partial_index` and `composite_index`. `IsOptimizerFinding` recognises a finding by category, or by the `llm_rationale`, `plan_source`, `hypopg_validated` or `what_if_verdict` markers. |
| `internal/optimizer/openrecs.go` | Open legacy findings of an analyzed table also go through the reload path: canonicalize (derived rollback), validate (including coverage) and HypoPG re-verify. They are re-emitted as `missing_index` with the current identity and the LLM label kept as `index_category`. The legacy row is then retired. Rows that have been acted on are never retired. |
| `internal/optimizer/covered.go`, `validate.go` | `checkDuplicate` also rejects a candidate that a valid existing index covers: same method and predicate, the candidate's keys are a btree prefix of the existing keys (exact keys for other methods), and its INCLUDE columns are carried by the existing index. |
| `internal/recommendation/index_inverse.go`, `migrate.go`, `types.go` | `InverseDropsCreatedIndex` and `DerivedIndexInverse`. `MigrateLegacy` repairs stale `CREATE INDEX` rollbacks in two places: first in open findings, then in unapproved live revisions (a new `migrated` revision through the normal revise path). The count is in `MigrationReport.InversesRepaired`, which is logged at startup. |
| `internal/executor/manual_rollback_guard.go`, `manual.go` | `ExecuteManual` refuses (`ErrRollbackMismatch`) a named `CREATE INDEX` whose rollback does not drop exactly that index. Nothing is recorded or run. |
| `internal/executor/action_record.go`, `apply_finding.go` | `logRefusedAction`: refusals before execution (backend signal, managed config, denied lease, unverifiable `CREATE INDEX`) are closed at once with a terminal `failed` verification. Its reason starts with `not executed:`. |
| `internal/executor/withheld_park.go`, `apply_finding.go` | Content-bound withholds are parked. The withheld row's `before_state.withheld_content` holds a hash of the forward SQL, rollback and target queries. Until that content changes, the finding is skipped before the gate, so it writes no decision and no action row. |
| `cmd/pg_sage_sidecar/database_runtime_recommendation.go` | Logs the repaired-rollback count. |
| `CHANGELOG.md` | Added a `## Unreleased` / `### Fixed` bullet. |

## Product decisions

- **Fail closed with an allowlist, not a blocklist (a).** The legacy LLM category was free text,
  so a list of LLM categories can never be complete. The gate therefore exempts only proven
  deterministic producers. Categories I found:
  - LLM / optimizer: `missing_index`, `covering_index`, `partial_index`, `composite_index`, plus
    any other label the LLM wrote. These are recognised by markers. All are gated.
  - Deterministic: `missing_fk_index` (`analyzer/rules_fk_index.go`). Exempt and unchanged.
  - `migration_safety` creates indexes, but its safe alternative can come from the LLM fallback
    (`migration/llm_fallback.go`). It is gated.
  - The custodian index path uses its own request, not `findingRequest`, so it is unaffected.
  - Schema lint, runway and the autonomy schema guard do not produce `CREATE INDEX` findings.
- **(b) Normalize on load through the reverify path.** I chose this over rewriting categories in
  place:
  - The unique `(category, object_identifier) WHERE open` index would collide when several legacy
    rows share a table.
  - Recommendation identities include the category, so a rewrite would orphan them.
  - Reloading reuses PR #85 unchanged and also derives the rollback and applies coverage.
  - The retired legacy row makes the executor supersede its recommendation through the existing
    freshness check ("finding no longer open").
  - Legacy rows on tables the optimizer does not visit stay open, but the gate keeps them behind
    an approval.
- **Approved content is never rewritten (c).** An approval pins exactly what was approved (C04).
  An approved stale pair stays as approved. `ExecuteManual` then refuses it instead of running
  it, and an operator must re-propose.
- **Withheld audit: option (b), a terminal verification.** I chose this over (a), no action row,
  and (c), exempting the audit:
  - Execution failures are already recorded as `failed` rows with a terminal `failed`
    verification. Refusals now match that.
  - The verdict CHECK needs no schema change.
  - Earned-autonomy classification is unchanged, because it keys on `outcome = 'failed'`.
  - The audit query is untouched, so it stays strict for actions that ran.
- **Park scope.** Only withholds that verified-index admission derives purely from the finding
  are parked: no name, missing or mismatched rollback, `IF NOT EXISTS`, `UNIQUE`, no target
  queries. State-dependent ones are not parked (index already exists, load admission, lease
  conflict). The park lifts on new content, which a new revision provides. A parked finding is
  not queued for approval either; its single action row shows the reason.

## Spec CHECKs and lifeos scenarios covered

- CHECK-L01 PASS: a legacy `covering_index` without a verdict is queued (pending), never runs,
  and gets a `queue_approval/approval_required` decision (`TestLegacyCoveringIndexWithoutVerdictIsQueuedNotRun`).
- CHECK-L02 PASS: the same finding with a verified verdict runs once (`TestLegacyCoveringIndexWithVerifiedVerdictRuns`).
- CHECK-L03 PASS: the FK-index finding is authorized by the gate and not routed to approval
  (`TestFKIndexFindingNotRoutedToApproval`, `TestFindingRequest_DeterministicFKIndexUnaffected`).
- CHECK-L04 PASS: the stale-rollback create is withheld and not run (`TestStaleRollbackIsWithheldNotRun`).
  The finding row and the already-migrated revision are repaired
  (`TestMigrateLegacyRepairsStaleFindingRollback`, `TestMigrateLegacyRevisesAlreadyMigratedStaleInverse`).
  Approved content is left alone (`TestMigrateLegacyLeavesApprovedContentAlone`). The operator path
  refuses a mismatch (`TestExecuteManual_RefusesRollbackOfAnotherIndex`).
- CHECK-L05 PASS: the withheld row has a completed `failed` verification and is not in the
  self-audit. An executed action without a verification still is
  (`TestWithheldActionHasTerminalVerification`, `TestAuditStillReportsExecutedActionWithoutVerification`).
- CHECK-L06 PASS: over 3 cycles, a withheld create leaves exactly one action row and no new
  decisions, then runs once after a revision fixes the rollback
  (`TestUnverifiableCreateIsParkedAcrossCycles`, `TestParkedCreateRunsAfterContentChanges`).
- CHECK-L07 PASS: a covered candidate is rejected, and the covered legacy finding is retired
  (`TestCheckDuplicate_RejectsCandidateAnExistingIndexCovers`, `TestOpenRecommendations_RetiresLegacyCandidateNowCovered`).
- CHECK-L08 PASS: legacy categories, and an unlisted LLM label, are reloaded, re-verified,
  re-emitted as `missing_index` with a derived rollback, and retired
  (`TestOpenRecommendations_ReloadsAndRetiresLegacyCategories`, `..._ReloadsUnlistedLegacyLabel`).

## Test Results

**Command:** `go test -cover -count=1 -p 2 ./...` (PG17 `pgsage-ag4`, Docker `golang:1.25 --cpus=2`)

**Total:**
- Run 2, final code: 89 packages, 88 ok, 1 failed.
  - `internal/analyzer` `TestPhase2_Cycle_NilSnapshot`: "schema bootstrap failed: acquiring
    advisory-lock connection: context deadline exceeded".
  - This is bootstrap lock contention between parallel packages. The package passed in run 1 and
    alone (below), and it is not touched by this change.
- Run 1, before the fixture fixes, 90 packages:
  - Flake: `cmd/pg_sage_sidecar` `TestPreflightRetentionHonorsRuntimeControls`, the same
    advisory-lock timeout. It passed alone: 81.5%, 228 s.
  - 3 real failures in `internal/executor`, fixed (see "Failures").
- Touched packages, `-v`, PG17: 2161 passed, 0 failed, 0 skipped.
- PG14: 1612 passed, 0 failed, 2 skipped. PG18: 1614 passed, 0 failed, 0 skipped.
- `-race` on executor, optimizer, recommendation and ledger: all ok, no races.
- e2e (`-tags=e2e`): ok, 78 passed, 0 failed, 13 skipped. 84 CHECK lines PASS, 0 FAIL.
- Lint (golangci-lint v2): 0 issues.

**Coverage (touched packages, PG17):** analyzer 91.5%, executor 86.1%, optimizer 88.4%,
recommendation 86.0%, ledger 88.2%, cmd/pg_sage_sidecar 81.5%. All packages meet the
coverage thresholds.

### Skipped Tests (must be zero or justified)
- e2e, 13 tests (`TestLLM*`, `TestTunerLLM_*`, `TestOptimizerMultiQueryConsolidation`):
  `SAGE_LLM_API_KEY` is not set. Live LLM tests are opt-in by design.
- PG14 `TestGenericPlanCapturesUnboundParameters`, `TestVerifyingEffectsReachActionsByIndex`:
  `GENERIC_PLAN` requires PostgreSQL 16+.

### Failures
- `executor` `TestCoverage_RunCycle_HysteresisBlocks`: its fixture reached the autonomous path
  through an unverified `CREATE INDEX` with a made-up category, which is exactly the gap closed
  here. The fixture now carries a verified verdict; the test's intent (hysteresis) is unchanged.
- `executor` `TestExecuteRetention*`, 2 tests: these were parked by `blast_radius_exceeded`. My new
  fixtures touched about 10 fresh tables, which pushed the shared test database past the
  20-table limit of the unattended profile. The fixtures now age their action rows out of the
  24-hour usage window on cleanup.
- Test logic corrected (committed separately with the reason): the first park test assumed one
  execute decision per attempt. One attempt writes three: the gate's decision plus Apply's
  re-authorizations. The test now compares the count after the first cycle with the count
  after three.

### Coverage Gaps
None: all touched packages are above 70%.

### Bugs Found This Session
1. [BUG] `executor/whatif_gate.go`: the what-if gate only checked `missing_index`. Legacy LLM index
   findings ran unattended (lifeos action 6383).
2. [BUG] `optimizer/openrecs.go`: legacy-category findings were never reloaded, re-verified or
   resolved.
3. [BUG] `recommendation/migrate.go`: stale `CREATE INDEX` rollbacks were copied verbatim from
   legacy findings into revisions (finding 18007).
4. [BUG] `executor/manual.go`: an operator path could record, and later run, a rollback that
   drops a different index.
5. [BUG] `executor/apply_finding.go`: refused or withheld rows had no verification, which
   tripped the ledger self-audit (action 6385).
6. [BUG] `executor/apply_finding.go`: content-bound withholds were retried every cycle
   (actions 6385, 6399, 6400).
7. [BUG] `optimizer/validate.go`: a candidate covered by an existing index was not rejected
   (action 6400 next to 6383).
8. Observation, not changed: `standingUsageSQL` counts refused or failed action rows toward the
   24-hour blast radius and rate limits.

### Mutation testing
Each key piece of logic was broken on purpose. All 22 mutants were killed:
- **What-if gate:** gate only on `missing_index`; drop the marker check; drop the FK exemption.
- **Withheld audit:** no terminal verification.
- **Manual guard:** guard off.
- **Rollback matching:** ignore the schema.
- **Migration repairs:** no revision repair; repair approved revisions; no finding repair.
- **Legacy reload:** match only listed categories; empty legacy list; no retire; retire current
  rows; retire acted-on rows; label lost.
- **Park:** park off; park ignores content; withhold not marked.
- **Coverage:** coverage off; prefix allowed for any method; INCLUDE ignored; predicate ignored.

The first pass left two survivors: the marker SQL and the acted-on guard. I added
`TestOpenRecommendations_ReloadsUnlistedLegacyLabel` and `TestRetireLegacyFinding_SkipsActedOnRow`,
and both mutants are now killed.

### Post-test audit
- **Untested inputs:**
  - Legacy rows whose DDL `ParseIndexDDL` rejects. The reload drops them and they are retired;
    this is covered indirectly by the "targets another table" test.
  - `DROP INDEX` rollbacks with comments or several names. These are rejected by the regex.
    Tested: `two indexes`, `two statements`.
- **Assertions that pass when broken:** none found. Every test asserts state: queue rows,
  `action_log` counts, index existence, decision verdict counts, verification rows, finding
  status and revision content.
- **Fakes:**
  - The fixtures use the real standing gate, action queue, recommendation store and Postgres.
    Index verification and load admission are faked, as in the existing stale-approval
    fixture.
  - HypoPG is unavailable in the legacy optimizer tests on purpose (verdict stays unverified).
    Real HypoPG re-verification is covered by PR #85's `TestOpenRecommendations_ReverifiesWithRealHypoPG`.

## What happens on lifeos after upgrading

1. **Startup.** `MigrateLegacy` repairs the rollbacks of open findings 18007 and the 6400 finding,
   and appends a corrected revision to recommendation 546. The startup log prints
   "N stale index rollbacks repaired".
2. **First executor cycle.** The legacy rows have no verdict, so the gate queues them for
   approval instead of running them.
3. **When the optimizer visits `public.memories` / `public.graph_nodes`:**
   - The legacy rows are reloaded and HypoPG-verified, then re-emitted as `missing_index` or
     dropped.
   - The `(node_type, name)` candidate is dropped as covered by
     `idx_graph_nodes_node_type_name_covering`.
   - The legacy rows are retired and their recommendations superseded.
4. **The index from action 6383 stays.** It is in `verifying`, and the per-query latency
   verification decides whether to keep or revert it. This change does not touch it.

## Left for the coordinator
- Approve or reject any queued approvals for legacy index advice that appear after the upgrade.
- Decide whether refused actions should count toward the blast radius and rate limits
  (bug 8). That is a policy question and is out of scope here.
- The advisory-lock bootstrap flake under `-p 2` reproduced in two packages across two runs.
  It is worth a separate look.
