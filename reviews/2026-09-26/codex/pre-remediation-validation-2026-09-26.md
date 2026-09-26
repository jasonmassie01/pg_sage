# pg_sage: readiness for remediation

**Date:** 2026-09-26. **Source:** v1.5.0 / master,
`b396595059d2b1b312a22231fdbfd1d2cfef9530`.

## Decision

**The focused validation pass is complete enough to begin remediation. The product
is not ready for a clean release or expanded autonomous authority.** The additional
work converted critical source findings into reproducible failing contracts, found
additional recovery and evidence defects, and proved a useful working end-to-end path.
Further open-ended review is less valuable than fixing the confirmed defects now.

This pass did not implement fixes. The canonical checkout and existing services were
preserved. Audit tests and scripts were staged in independent clones before execution;
failed attempts and fixture corrections remain available. Real-model requests and
cloud provisioning were not performed. Tests used task-owned disposable PostgreSQL.

The broad original audit remains the feature inventory and improvement roadmap.
This addendum supersedes its earlier uncertainty where an executable proof is now
available, and updates the coverage/UI validation status without erasing historical runs.

## What the extra work established

| Area | New evidence | Meaning for remediation |
|---|---|---|
| Retention physical identity | Production Go Apply deletes 2 rows with batch limit 1, including a future row | R01 is a production-path data-loss defect, not merely suspicious SQL |
| Retention authority | Production constructor and schema guard ignore stop, observation, manual mode and disabled executor | R02 needs a shared runtime admission path, not four local checks |
| Retention evidence | 30-day dry-run evidence authorizes a changed 1-day contract | R03 needs relation and contract revision binding |
| Retention outcome durability | Injected audit-write failure leaves committed deletion and no applied record | New P1: mutation and durable outcome are not atomic |
| Mounted HTTP MCP | Authenticated viewer performs writes denied through REST; standing stop control also tested | SURF-01 confirmed; caller authorization and standing policy are separate requirements |
| OIDC identity | Local configured issuer with false/missing email_verified logs into existing admin | SURF-02 confirmed under its configured-issuer precondition; not anonymous OAuth bypass |
| Revert recovery | Real backend termination and process exit after durable verdict leave an unreverted index outside the due queue | C15 confirmed across actual failure boundaries |
| Concurrent verification | Two overlapping workers finalize one durable watch | New P2 concurrency contract: enforce exclusive durable ownership and idempotent completion; no data loss demonstrated |
| RCA recovery | Separate processes duplicate a continuing incident or leave an old active incident orphaned | R04 confirmed across process restart, not only object reconstruction |
| Manual RCA resolution | Stale concurrent persist overwrites committed resolution | R04 needs versioned/monotonic state transitions |
| Telemetry epochs | Real statistics reset followed by counter regrowth is certified as valid | R10 strengthened; monotonic counters alone do not establish a valid observation interval |
| Query identity | Actual role and top-level/nested cohorts share a query ID and yield incorrect window arithmetic | Prior hypothesis promoted to confirmed measurement defect |
| Evidence freshness | Real collector failure followed by reanalysis advances finding count and last_seen from the same snapshot | Prior hypothesis promoted to confirmed finding-integrity defect |
| AgentDB boundary | Invalid schema request leaves an actual schema without registration; spoofed approval actor persists | Validate before effects, reconcile partial outcomes, derive actor from authenticated context |
| Working application path | Real Chromium plus metadata fleet: detect → case → queue → approve → ANALYZE → restart → remove | This path works; broad claims that all wiring is broken would be wrong |

Existing C01 cache units, C02 suppression resurrection and C04 stale inverse SQL were
reproduced again. These are confirmations, not additional unique bug counts. Role and
top-level query problems share an identity-model cause. Several test failures therefore
map to one repair, and repeated/parent test events must not be counted as new bugs.

## Claims that remain deliberately narrower

- Synthetic `Calls=100, MeanExecTime=0` creates an infinite planning ratio and fails JSON
  persistence. A real failed-execution control produced `calls=0` and was correctly
  ignored. Production reachability of the positive-call/zero-mean case remains unproven.
- Unknown HA state admits at the legacy policy evaluator, but no test demonstrated a
  mutation through the complete standing-policy path during actual primary failover.
- The retention stop probes block **new work before it starts**. Behavior and guarantees
  for already-running operations need an explicit contract and implementation tests.
- Source inventory candidates are a review aid, not proof that every external side effect
  is enumerated or authorized. Dynamic SQL, indirect interfaces and external tools need
  their own enforcement and integration tests.
- A successful ANALYZE and its catalog timestamp prove execution. They do not establish
  improved service latency or general durable verification of other action types.

## Verification results and coverage

| Run | Result | Qualification |
|---|---|---|
| Clean uncached Go baseline | 7,148 pass; 0 fail; 8 skip | Named tests/subtests include parents; cloud/model/OS skips explicitly listed |
| Retention regressions | 1 pass; 8 fail; 0 skip | Seven failed leaf contracts plus one failed parent; defects intentionally remain |
| Linux real-binary standalone/configured fleet | 38 pass; 0 fail; 0 skip | Selected local suites with child-binary coverage |
| Real metadata fleet + browser | 14 checks pass | Includes real approval, catalog effect, restart, failed-create cleanup and worker removal |
| Security, durability and telemetry adversarial probes | Confirmed failing contracts | Exact selected runs, repeated attempts and coverage in their appendices |
| Historical-state diagnostic SQL | Validated on seeded disposable state | Transaction is REPEATABLE READ READ ONLY; no live installation queried |

The clean unit-only startup/orchestration package remains **43.5%** covered. Adding
actual process coverage raises its exact source-block union to **73.78% (1,548/2,098
statements)**. All measured production packages meet the combined coverage thresholds;
utility commands use their 50% floor. This is a combined Windows/Linux measurement,
not proof that each platform independently meets the floor.

The distinction matters: missing subprocess instrumentation accounted for a substantial
part of the original gap, but passing coverage does not remove failing safety contracts.
AgentDB fleet reconciliation, fleet log startup, external provider paths and several
failure branches remain uncovered. See preflight-verification.md for every package,
skip, exact command, harness correction and still-uncovered function.

## Historical data and upgrade readiness

`evidence/preflight-state-diagnostics.sql` provides eighteen named read-only diagnostic
sections for each monitored database and its metadata database. It avoids SQL text,
credentials, provider payloads and secrets in output. It was executed against deliberately
inconsistent fixture data, with and without optional AgentDB tables.

| Persisted cohort | Required migration/repair decision |
|---|---|
| Suppressed and open copies of one finding | Retain user suppression, preserve references, choose a canonical identity; no blanket deletion |
| Forward and inverse SQL disagreement | Quarantine affected queued work; regenerate/revalidate both from current catalog and immutable plan revision |
| Terminal verification with unreverted target | Inspect actual object identity and operation outcome before retry; restore durable pending intervention state |
| Duplicate/orphan active incidents | Preserve history and user resolution; establish stable identity/version; do not infer resolution from age alone |
| Old cache units or counters without epoch/cohort identity | Version observation schema; mark history non-comparable or rebuild valid windows; do not invent missing identity |
| Incomplete provision/cleanup claims | Reconcile local/provider resource identity and receipts before resuming, adopting or removing resources |
| Unrecorded historical retention deletion | Database contents alone cannot identify deleted rows; use retained logs/backups/PITR evidence if available |

Some past facts cannot be reconstructed from the current tables: an overwritten manual
resolution reason, the original pairing of mutable forward/inverse SQL, missing query
cohort identity, and unlogged deleted rows. Remediation must retain **unknown** as a valid
outcome. A heuristic candidate query is not authorization for automatic repair.

No active named pg_sage service/monitor process was found in the local process inventory.
The checkout contains several local/test/cloud configurations, without an authoritative
live deployment selected for this task. Existing application databases and the unrelated
review-test container were not used. Installation-specific diagnostics therefore remain
a **pre-deployment gate**, not a claim that existing installations have clean state.

## Remediation order and acceptance gates

These gates refine the original roadmap's waves; they do not remove its existing-feature
improvements or renumber the planned SRE releases.

### Gate A: destructive authority and caller identity (roadmap Wave 0)

1. Route retention through the same authoritative runtime checks as other mutations.
   Require the authenticated actor where relevant, current standing policy, stop state,
   operating mode, executor enablement, primary/unknown-state policy and bounded limits.
2. Fix partition/inheritance row identity using both relation and tuple identity or an
   equivalent stable key, retain the eligibility predicate, and prove the batch limit.
3. Bind dry-run evidence to relation identity, time column, interval, contract version,
   policy and expiry. Re-evaluate after any change.
4. Make retention intent/outcome durable with the database effect; reconcile uncertain
   commit results before retry.
5. Enforce mounted MCP tool roles and verified OIDC issuer/subject identities. Preserve
   existing valid login and standing-policy behavior; make account linking deliberate.

**Gate:** all staged retention and mounted authorization contracts pass; positive controls
still pass; no policy widening to satisfy tests; test partitioned and inherited tables,
policy changes, unavailable stop/role evidence and concurrent admission explicitly.

### Gate B: durable completion, recovery and existing state (roadmap Waves 0–1)

1. Separate measured verdict from intervention state. A decision to revert is not a
   completed revert. Track pending, claimed, executing, uncertain and confirmed outcomes.
2. Add leased ownership/fencing or another durable exclusive claim for verifications.
   A process mutex is insufficient across processes. Reconcile after worker death.
3. Make incident identity and manual resolution persistent and versioned. Reject stale
   writes that erase newer user decisions; hydrate or reconcile active incidents at startup.
4. Pair finding identity, suppression, forward SQL and inverse SQL atomically. Include
   staged C01/C02/C04 contracts and repair historical affected state where evidence permits.
5. Validate AgentDB requests before DDL/provider effects; use durable operation IDs and
   explicit compensation/reconciliation for partial outcomes. Bind recorded actor identity.

**Gate:** actual process-exit, lost-connection and competing-worker tests pass; repeated
recovery is idempotent; diagnostic candidates have documented dispositions; existing
queue/incident references survive schema upgrades. Never blindly replay old rollback SQL.

### Gate C: trustworthy evidence and incomplete wiring (roadmap Waves 1–2)

1. Carry database, role and top-level cohort identity plus statistics/reset epoch through
   collector, store, analysis, verification and API layers. Define aggregation explicitly.
2. Version observation units and preserve source time/freshness. Process an observation
   idempotently; unavailable or incomparable evidence must not become a passing verdict.
3. Repair unwired runtime features from the original inventory, including deferred advisor
   work, hint lifecycle, forecast history, fleet cleanup, rollout and rehearsal evidence.
4. Add a real trigger-to-visible-outcome test for each repaired feature. Preserve the
   demonstrated metadata fleet and Actions approval workflow.

**Gate:** reset/regrowth, interior-reset, mixed-role/nested-statement and stale-snapshot
tests pass; windows with missing identity are explicitly rejected; no invented verification.

### Gate D: release commissioning, then AI SRE (roadmap Waves 3–5)

Run the supported PostgreSQL-major matrix, physical replica/promotion tests, sustained
fleet load, real-model negative cases and each supported provider's create/observe/restore/
cleanup path in designated environments. Complete installation-specific diagnostics before
deployment. These do not need to delay Wave 0 coding, but they do gate release claims.

Start Sage Incident Investigator with the previously specified read-only scope after
the evidence and incident-persistence repairs. Add its 35 proposed acceptance checks;
they remain a future implementation contract, not tests that passed in this audit.

## Ready/not-ready checklist

CHECK-READY-01: PASS — frozen source baseline and canonical checkout preserved.
CHECK-READY-02: PASS — critical findings have staged, failing reproductions and controls.
CHECK-READY-03: PASS — actual restart, backend failure and concurrent recovery exercised.
CHECK-READY-04: PASS — real backend/browser approval journey verified.
CHECK-READY-05: PASS — subprocess coverage measured; combined threshold accounting explicit.
CHECK-READY-06: PASS — read-only historical diagnostics executed on seeded fixture states.
CHECK-READY-07: FAIL — product safety, identity and durable-recovery regressions remain unfixed.
CHECK-READY-08: MANUAL — live installation state diagnostics and migration rehearsal.
CHECK-READY-09: MANUAL — supported-major/HA/provider/real-model/soak commissioning.
CHECK-READY-10: PASS — sufficient evidence and acceptance criteria to start remediation now.

## Supporting artifacts

- preflight-retention-runtime.md — production retention and actual service/browser proofs.
- preflight-mutations-security.md — mutation inventory, mounted authorization and AgentDB probes.
- preflight-durability-state.md — crash/retry/concurrency, RCA and diagnostic validation.
- preflight-evidence-contracts.md — telemetry epoch, identity, freshness and negative controls.
- preflight-verification.md — uncached baseline, per-package coverage, skips and runtime counts.
- evidence/preflight-runtime-tests.patch — root staged regression/harness additions.
- evidence/preflight-state-diagnostics.sql — read-only installation diagnostic artifact.

The consolidated full audit embeds these reports so findings and proposed remediation
remain available in one file. Raw logs, staged patches, coverage profiles and screenshots
are retained separately for replay and review.
