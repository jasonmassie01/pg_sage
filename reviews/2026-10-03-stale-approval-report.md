# Stale approval: approvals whose reason has gone away (2026-10-03)

Branch `claude/fix-stale-approval`, based on `origin/master` (d7b3a52b, v1.8.3).

## The dogfood evidence (lifeos, read-only)

- `sage.action_queue` ids 2 and 3 (findings 18036 memories, 18569 events): `pending`,
  queued at 05:57 with `expires_at` 24 h later, pinned to recommendations 1034 and 1512 at
  revision 1.
- `sage.findings` 18036 and 18569: `detail.what_if_verdict = verified`,
  `hypopg_validated = true` (the v1.8.3 re-verification ran at 12:28).
- `sage.recommendation_revision` (1034, 1) and (1512, 1): the evidence has **no**
  `what_if_verdict` and `hypopg_validated = false`. Both heads are still at revision 1 with the
  original content hash.
- `sage.decision` 326942 (events): `queue_approval / approval_required`, `repeat_count` 38,
  `last_seen_at` 12:37, after the finding was verified.

## Root cause

1. **The gate read stale evidence.** Since C07 the executor acts on durable recommendations.
   `findingFromCandidate` builds the gate request from the current revision's `evidence`. A
   revision is immutable, and evidence is deliberately outside the content hash (so fresh
   metrics never invalidate an approval). When HypoPG re-verified the index, the analyzer
   rewrote `sage.findings.detail`, but the recommendation only got a sighting (`touch`).
   `requireApprovalWithoutWhatIf` therefore kept seeing "no verdict", the gate kept answering
   `queue_approval / approval_required`, and `queueFinding` returned at `pendingApproval`.
   Nothing ran until the proposal expired.
2. **Nothing retired the pending proposal.** Even with a fresh verdict the Execute path never
   looked at the queue. The pending proposal would have stayed open beside the executed
   change, so an operator could still approve a duplicate.
3. **A latent bug on the same path (found by the new tests).** The change lease of a finding
   used its object identifier as a catalog name. An optimizer identity is
   `schema.table|<index definition>` (C05), which never resolves (`invalid identifier`). So the
   first authorized optimizer CREATE INDEX would have been recorded as a failed action instead
   of running. lifeos has never run an optimizer index since the lease code landed: its last
   CREATE INDEX actions date from June.

**Decision ledger.** It does not keep a stale verdict. The verdict is part of the
fingerprint, so a changed verdict opens its own row, and a repeat refreshes reason, risk tier
and evidence. Row 326942 was stale because the gate's input was stale, not because of the
upsert. The new DB test `TestChangedVerdictOpensNewRowAndKeepsTheOldOne` pins this. Not
changed: the ledger never writes the `guardrails` column (it is always `[]`). See follow-ups.

## Fix

- `internal/executor/stale_approval.go`: `currentGateEvidence` runs in `processFinding`
  before the gate. It overlays the gate keys (`what_if_verdict`, `what_if_reason`,
  `hypopg_validated`, `approval_required`) from the open finding's current `detail`, but only
  when that finding still proposes the same SQL. A verdict about other SQL never authorizes
  the old SQL. If a key has vanished from the finding, it is removed. That covers any
  producer's `approval_required` marker disappearing. If an operator rejected exactly this
  content, the change gets the approval guardrail, so autonomy never overrides a human "no".
  The normal re-queue rules (rejection cooldown) still apply. If either read fails, the
  overlay fails safe: the change requires approval.
- `internal/executor/stale_approval_queue.go`: `retireStaleApprovals` runs in
  `runAuthorizedFinding`, under the change lease and after Apply's re-authorization, so
  emergency stop, maintenance windows, cooldowns, the serialize mode and the DDL slot have
  already passed. In one transaction it locks the proposals for the finding, its
  recommendation or its SQL (`FOR UPDATE`, the same lock order as `Approve`). It then
  decides:
  - The change does not run if:
    - an approved or failed (approved, retrying) proposal exists;
    - an operator rejected this exact content;
    - a pending proposal is for different content;
    - the recommendation changed since the cycle read it (another worker took it).
  - Otherwise, the unexpired pending proposals of exactly this content become `superseded`,
    with reason `approval no longer required: what-if verified (decision N)` or
    `... the standing policy now authorizes it (decision N)`. The change then runs once
    through the unchanged claim/execute/verify path.
  - Expired proposals are left untouched.
- Generic case: any reason that disappears (what-if verified, trust raised from advisory,
  execution mode switched to auto, a producer clearing `approval_required`) is handled the
  same way. The supersede step does not care why approval was needed. It only acts when the
  gate now authorizes the same content.
- `internal/executor/apply_lease.go`: an optimizer finding's lease target is now its table
  (`analyzer.OptimizerFindingTable`). The gate request keeps the full identity for the ledger
  and the policy limits.

### Product calls

- **Operator rejection is sticky for autonomy.** Before this fix the rejection cooldown only
  stopped re-queueing. Once the verdict is fresh, a rejected but verified index would
  otherwise have run unattended five minutes later. A rejected change now only re-enters the
  approval queue.
- **A pending proposal for other content blocks autonomy.** This is fail-closed and bounded
  by the proposal's expiry. A revised recommendation already supersedes its old proposals, so
  this only affects legacy rows.
- **Superseded rows get no `decided_at`**, the same as the existing
  "recommendation revised" supersede.

## Tests (written first: commits ac10f949, 874386d5)

DB-backed, real sage schema, real standing gate (unattended profile) and ledger, real
`ActionStore`, the durable recommendation store and `RunCycle`. Only the index verifier is
faked.

| # | Test | Asserts |
|---|------|---------|
| 1 | `TestStaleApprovalSupersededWhenWhatIfVerified` | pending, then `superseded` with "approval no longer required: what-if verified"; 1 action; index built; recommendation claimed and linked; another cycle adds nothing |
| 2 | `TestStaleApprovalStillUnverifiedStaysPending`, `...LegacyFindingWithoutVerdictStaysPending` | row unchanged, 0 actions, no execute verdict |
| 3 | `...RejectedStaysRejected`, `...ApprovedIsLeftToTheOperatorPath`, `...ExpiredIsUnchangedAndChangeRuns` | rejected/approved/expired rows unchanged; rejected and approved run nothing (rejected is re-queued after the cooldown, still not run); expired runs once |
| 4 | `...RevisedContentRunsOnlyNewSQL`, `...VerdictForOtherSQLDoesNotAuthorize`, `...PendingForOtherContentBlocks` | old SQL never runs; the revision's own supersede reason is kept; another SQL's verdict authorizes nothing; an other-content proposal is untouched |
| 5 | `...ConcurrentCyclesExecuteOnce` (3 executors at once), `...LateWorkerDoesNotRunAgain` | superseded once, 1 action, 1 `applying` transition; a late worker records nothing |
| 6 | `...TrustRaisedSupersedes` | generic reason, advisory → autonomous, runs once |
| - | unit: overlay, `sameContent`, classification, reason, rejection guardrail; `TestLeaseSpecFor*` | pure logic |
| - | ledger: `TestChangedVerdictOpensNewRowAndKeepsTheOldOne` | changed verdict gets a new row; repeat refreshes the reason |

Two fixture corrections (commit 65fe3a9b, the tests' logic, not the product): the executor
config needed `maintenance_window: always` (moderate actions were blocked
`outside_maintenance_window`). The unattended profile's 24-hour blast radius (20 tables) is
shared with the package's other tests, so the fixture raises it.

**Mutation testing.** Each of these mutations made at least one test fail:
- removing the evidence overlay;
- making the supersede step a no-op;
- dropping the recommendation re-read;
- ignoring the SQL match;
- ignoring rejections;
- ignoring approved proposals;
- superseding other-content proposals;
- reverting the lease fix;
- a constant reason.

The re-read mutation initially survived. That gap is what `...LateWorkerDoesNotRunAgain`
was added to cover.

## Test Results

**Command:** `go test -count=1 -cover -p 2 -v ./...` (golang:1.25, `--cpus=2`, PG17 `pgsage-ag8` :55478)
**Total:** 11,659 passed, 0 failed, 21 skipped (`go vet ./...` clean)

**e2e:** `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: 78 passed, 0 failed, 13 skipped.

**Touched packages, `-race`, PG14 :55414 and PG18 :55418:** pass. New stale tests: `-race -count=10` pass.

**Lint:** golangci-lint v2.11.4 (the rules' scratchpad binary; `~/go/bin` has v1.64.8) on
`./internal/executor/... ./internal/ledger/...`: 0 issues.

**Coverage (touched):** executor 86.0%, ledger 88.2%. All meet thresholds.

### Skipped tests

All skips are gated on live cloud, a live LLM or external infrastructure; none are in touched packages:
- **Unit suite (21):**
  - AWS RDS / Cloud SQL / Lakebase / AgentDB gauntlet live provisioning
  - Azure live parameters
  - live LLM (`PG_SAGE_LIVE_LLM`) and real Gemini
  - PgBouncer and causal standby URLs not set
  - container failover/promotion fixtures
  - the Windows-only log dir test
  - the RCA child-process fixture (runs only as a subprocess)
  - plan fixture generator
  - PG incident bench
- **e2e (13):** all because `SAGE_LLM_API_KEY` is not set.

### Failures

None.

### Bugs found

1. `executor/recommendation.go` `findingFromCandidate`: the gate read immutable revision
   evidence, so a re-verified index was gated as unverified forever (the reported bug).
2. Execute path: a pending approval was never superseded when approval stopped being needed.
3. `executor/apply_lease.go` `leaseSpecFor`: optimizer leases used `schema.table|<def>` as a
   catalog name, so every authorized optimizer CREATE INDEX would fail at the lease.

## Follow-ups (not done, kept out of scope)

- Earned-autonomy L2 handoffs (`autonomy.go`, custodian proposals): when a handoff's class
  later reaches L3, the custodian path runs without looking at the pending handoff. It should
  get the same supersede step, keyed on `identity_key`.
- `buildApprovalProposalMetadata` labels queued rows with `ExplainAction`, which does not see
  the finding's evidence. On lifeos the rows show `policy_decision = parked` and no
  `approval_required` guardrail, although the gate queued them for approval.
- The ledger never writes `sage.decision.guardrails` (always `[]`), and a superseded
  queue_approval decision row stays open (`resolved_at` NULL; nothing resolves decisions).
- After deploying, lifeos queue items 2 and 3 should be superseded on the first cycle, and
  both indexes should build once, inside the maintenance window.
