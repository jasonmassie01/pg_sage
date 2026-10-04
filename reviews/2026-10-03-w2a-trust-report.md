# W2a: one trust system (roadmap 1.2), report (2026-10-03)

Branch `claude/w2a-one-trust-system` (from `origin/release/wave1`). Roadmap item 1.2: *"Every
self-initiated action class gets a ledger pair per database. The time ramp becomes a floor,
not a grant. Rollbacks, regressions and operator rejections demote automatically. One Trust
page shows both, per database x family x class, with the evidence."*

## What was wrong

- Two trust systems. Self-initiated actions (index create/drop, GUC, reloption, vacuum,
  analyze, hints) ran on the elapsed-time ramp in `policy.tierDecision`: once
  `trust.ramp_*_hours` had passed, `trust.level` + `tier3_*` let them run unattended, and no
  outcome (regression, rollback, rejection) ever lowered that. The earned ledger
  (`internal/earned`, L0-L4, admin-approved promotions) restricted only requests carrying an
  `IncidentFamily`, i.e. the custodians (`policy/autonomy.go` `governedByAutonomy`).
- Nothing tied the Phase 1.3 verdicts (`sage.action_outcome`) to the level of the class that
  produced them.

## Design

One ledger (`sage.sre_family_autonomy`, per deployment x database x family x class), two
kinds of family:

| Kind | Families | Evidence | Demotion |
|---|---|---|---|
| incident (unchanged) | the 11 Sage SRE families | bench, shadow reviews, live L2 recoveries | harmful: every class of the family to L1 (M7); **new:** an operator rejection of a handoff: the pair one level |
| self-initiated (new) | `tuning`: index_create, config_guc (GUC), autovacuum_tuning (reloption), query_hint, statistics; `hygiene`: index_drop, vacuum, analyze, retention, reindex | `sage.action_outcome` verdicts, operator rollbacks and rejections, per database | regressed, operator rollback or rejection: the class one level, at once |

### Gate (`internal/policy`)

- `AutonomyScope` (optional on a limiter): `Governs(req)`. The earned limiter governs an
  incident-family request **or any self-initiated class** (`earned.Governs`). A limiter
  without it keeps the M7 contract (incident families only), so fakes/embedders are unchanged.
- Never governed: operator approvals, `ActionRequest.Rollback` (new: undoing pg_sage's own
  change: the monitor's `standingRollbackAuthorizer` and the verified-index revert set it),
  owner-declared requests without an incident family (retention under its table contract,
  D5), read-only diagnostics.
- For a governed self-initiated request, `tierDecision` no longer checks the ramp
  (`rampSatisfied(..., ledgerDecides)`): the ledger level decides. Trust level, execution
  mode, tier flags, the standing policy, refusal set and windows stay the ceiling (advisory
  never runs a MODERATE class unattended at L3; tier3 off still blocks). Incident-family
  requests keep the ramp as an extra restriction (M7 behaviour unchanged).
- L3 of a self-initiated class needs exactly one target; its window rule is its tier's
  (SAFE classes never needed a window; MODERATE ones are already held to it by the gate).
  Incident L3 keeps the strict window.
- `policy.RampAge` exported (the ledger's floor uses the gate's own rule).

### Ledger (`internal/earned`)

- `selfinit.go`: families, class table (tier, verification class), `FamilyForRequest`,
  `Governs`, `CapForPair` (a reversible self-initiated class can reach L3, incl. GUC;
  retention is irreversible: L1). `AllFamilies()` = incident + self; `Families()` stays the
  incident set (bench, game days, shadow reviews).
- Evidence (`selfinit_evidence.go`, `selfinit_store.go`): `ClassRecord` counts every outcome
  by verdict and the credited / uncredited decided outcomes **since the last demerit**.
  Promotion checks: `class_cap`, `observation_floor` (ramp start + `trust.ramp_safe_hours`
  for L2, `ramp_moderate_hours` for L3 of MODERATE classes, the gate's irreversible floor),
  `class_successes` (L2 >= 3, L3 >= 10), `class_success_rate` (L3 >= 80%). Each unmet check
  carries its instruction and, for the floor, an ETA. Unknown ramp start: never proposed.
- Reconciler self pass (`selfinit_reconcile.go`), every `reconcile_interval_seconds`, with
  per-kind cursors in `sage.trust_ledger_state`:
  - verdicts (`sage.action_outcome`, decided): improved -> credit; neutral -> credit for
    hygiene when the action was not rolled back, else nothing; regressed -> demerit;
    insufficient_evidence / unverifiable -> nothing. Incident-family decisions are skipped
    (their own path records them).
  - operator rollbacks (`action_log.outcome = rolled_back`): demerit, except the rollback of
    its own regression (already counted) and the Phase 1.3 no-gain revert of a neutral index
    create.
  - operator rejections (`action_queue` rejected by a user): demerit for the class (or the
    incident pair of an `autonomy:` handoff).
- Demotion (`demote.go`): one level (floor L1), compare-and-set, history event
  `auto_downgraded` with the cause, pending proposals superseded, operator notified
  (`DemotionNotifier`, wired to the database's notification rules as `action_failed`). A
  demerit observed before the pair's level was last set does not demote it again (so
  pre-upgrade history never punishes grandfathered levels, but resets the success streak).
  Carried-over incident levels are not auto-demoted (M7 decision kept).
- Limiter: self-initiated classes take the database-wide signals (error budget, HA role);
  not the stale-evidence signal (findings carry no observation time; the executor re-reads
  the finding before the gate), not the family safety window (demerits demote the class
  itself), and not the 15-minute concurrency signal (the executor's cascade cooldown and
  change leases serialize work on one object; e2e CHECK-A05 caught the cap withholding the
  autovacuum tuning that follows a vacuum of the same table).
  A self-initiated level is not re-derived from evidence at each authorization.
- Grandfathering (`grandfather.go`): on the first start of a database under the ledger, the
  level the ramp gate granted each self-initiated class (L3 unattended, L2 approval queue)
  is seeded with provenance `grandfathered` and what granted it, once (marker in
  `sage.trust_ledger_state`). The ramp elapsing later grants nothing; existing rows are never
  touched; grandfathered levels demote like any other.
- Trust view (`trust_view.go`): rows for every family x class of both kinds with level,
  effective level (limiter annotation), cap, provenance, evidence counts, last change
  (newest level-changing event) and the next level's assessment.

### Schema

`internal/schema/trust_ledger_migration.go` (registered after `ddlActionOutcome()`):
outcomes gain `verdict`, `observed_at`, `queue_id` (unique per queue item), result
`rejected`, source `rollback`; provenance `grandfathered`; event `grandfathered`;
`sage.trust_ledger_state`; indexes `idx_action_outcome_decided`, `idx_action_queue_rejected`
(checked loop). Constraints keep their names and are redefined in place only when they lack
the new values, so re-running the earlier M7 migrations never fails. Retention exemption
added for the state table.

### Wiring, API, UI

- `cmd/pg_sage_sidecar`: install hands the ledger `Executor.RampFloor` and grandfathers
  from `Executor.OperatorBound()` (now with the ramp); the fail-closed limiter governs the
  same scope; startup `TRUST:` lines explain the new meaning with the configured values (WARN
  when `sre.autonomy.enforce=false`); one log line per database when grandfathered;
  demotion notifications; `sre.autonomy.class_promotion` mapped to the bar.
- `GET /api/v1/trust[?database=]` (new route file `api/trust_routes.go`, every signed-in
  role): one view per database plus `enforced` and `meaning`. MCP `sre_get_autonomy` gains
  `trust` (additive). The SRE autonomy view and routes are unchanged (incident families).
- Web: **Trust** page (`#/trust`, nav "Trust"): per database, self-initiated and incident
  sections, level / cap / effective, provenance badge (grandfathered, carried over), evidence
  counts, last change and why, path to next level (unmet checks with ETA), Approve for a
  pending promotion (admin, existing endpoint).
- Config: `sre.autonomy.class_promotion.{min_successes_l2, min_successes_l3,
  min_success_rate_pct}` (3, 10, 80; YAML-only, restart-bound, lowered values are fast
  elevation). Doc strings of `trust.level`, `trust.ramp_*`, `sre.autonomy.enforce` say what
  they mean now; `docs/configuration.md` "One trust system"; generated config meta and
  lifecycle docs refreshed.

## Product calls (made under the product principle; please confirm)

1. **Two self-initiated families by goal** (`tuning`, `hygiene`) rather than one per class:
   the goal decides what a success is (the owner's rule), and the grid stays readable. Ledger
   class names stay the M7 ones (`config_guc` = GUC, `autovacuum_tuning` = reloption); the
   API row carries the verification class (`outcome_class`).
2. **L1 is the demotion floor** for one-level demotions (L0 and L1 both mean "manual script"
   at the gate; going to L0 would only add an approval step to get back to a script).
3. **A demerit known before the level was set does not demote it again.** Historical
   pre-upgrade rejections/regressions are recorded and reset the success streak, but do not
   take back grandfathered autonomy on the first reconcile.
4. **Self-initiated GUC changes can reach L3** (incident `config_guc` stays capped at L2):
   since Phase 1.3 they have prior-value capture, rollback SQL, read-back and a verdict. This
   also keeps the lifeos GUC autonomy that the ramp had granted (grandfathered L3).
5. **Retention is irreversible: L1 for pg_sage's own initiative**; owner-declared retention
   deletes keep running under the table contract (D5), outside the ledger, with their outcomes
   still recorded.
6. **Rollbacks of pg_sage's own changes are never withheld by the ledger** (a drop's
   re-create must not wait for index_create trust).
7. **Self-initiated L2/L3 bar:** L2 = 3 verified successes since the last demerit + safe
   ramp observed; L3 = 10 successes, >= 80% of decided outcomes, moderate ramp observed (safe
   ramp for SAFE classes). Successes from operator-run actions count (the only evidence a
   class at L1 can produce).
8. **Operator rejections demote incident handoff pairs too** (one level), as one system.
9. **Demotion notifications use the `action_failed` rule** (warning), so existing rules
   deliver them without a new event type.
10. **`sre.autonomy.enforce: false` stays** as a loudly logged legacy escape hatch (the ramp
    grants). Removing it is a separate decision.
11. **Incident families keep the ramp as an extra restriction and all their downgrade
    signals**; self-initiated classes take only the error budget and HA role (see Limiter).
12. **Custodian work without an incident family** (schema guard, FK index, analyze) is
    self-initiated and now governed by its trust family; its L2 handoff key names that
    family (`autonomy:hygiene:analyze:...`).

## Tests

Tests were written first (`95c245d1`) and run afterwards.

## Test Results

**Command:** `go test -v -count=1 -cover -p 2 ./...` (Docker golang:1.25, `--cpus=2`, PG17 :55476),
after the final merge of `origin/release/wave1` (f6f25009)
**Total:** 12087 passed, 1 failed (flaky, passed on rerun), 22 skipped
**Coverage (touched packages):** earned 88.7%, policy 89.6%, executor 87.0%, api 79.1%,
config 91.1%, schema 84.1%, retention 87.3%, store 76.1%, gameday 91.2%, notify 91.0%,
cmd/pg_sage_sidecar 81.5%

### Skipped Tests (must be zero or justified)
- 21 env-gated, same set as on the base branch: live cloud provisioning (AWS RDS, Cloud SQL,
  Lakebase, 3 AgentDB gauntlets, 2 Azure), live LLM providers (2 chat, Tier2 Gemini),
  container failover/restart/promotion fixtures (3), PgBouncer (3), `TestPGIncidentBench`,
  `TestGeneratePlanFixtures`, `TestRCAChildProcessFixture`, `TestResolveLogDir_AbsoluteWindows`
  (Windows only).
- `internal/collector` `TestCollectQueriesExcludesStatementsFromPgSagePools`: skipped in the
  full run because its 5 s ping timed out while the database was loaded; rerun alone: PASS.

### Failures (if any)
- `cmd/pg_sage_sidecar` `TestRuntimeParityEquivalentDatabase/persisted_stop=false/yaml-fleet`:
  "drain ... runtime: context deadline exceeded" in the same loaded minute as the ping
  timeout above. Rerun `-count=3 -run TestRuntimeParity`: PASS; package rerun: PASS (81.5%).
  The earlier full run's only failure was `TestFleetReloadThroughWatchedYAMLFile` (passed on
  rerun); PG18's first run failed `TestFleetReloadConcurrentRetriesConverge` on a connect
  timeout, passed on rerun. Load-sensitive fleet tests, not touched by this branch.

### Coverage Gaps (packages below threshold)
- None among touched packages. Untouched packages below 70%: `cmd/reset_admin_for_test`
  50.0%, `cmd/create_admin` 51.5%, `cmd/sigstore_trusted_root` 56.1%,
  `internal/testsupport/pgssepoch` 58.3%, `sre-bench` 62.4% (tools and test support;
  utility floor 50% met).

### Bugs Found This Session
All in this branch's own new code, caught by the tests before push.

1. [BUG] `earned/limiter.go`: the 15-minute concurrency signal capped a self-initiated class
   right after another action on the same table; e2e CHECK-A05 caught the autovacuum tuning
   that follows a vacuum being withheld. Fixed: self-initiated classes take only the error
   budget and HA signals (`9fb1fc67`).
2. [BUG] `executor/autonomy.go`: the L2 handoff key of a custodian action without an incident
   family had an empty family (`autonomy::analyze:...`), so a rejection could not be
   attributed. Fixed: the key uses `FamilyForRequest` (`autonomy:hygiene:analyze:...`).
3. [BUG] `earned/carryover.go`: `CapForPair` returning L1 for an unknown family would have
   let `effectiveCap` mis-cap carried incident levels; non-self families keep `CapFor`.
4. [TEST] the handoff-key test shared the package's 24-hour blast-radius window and was
   parked (`blast_radius_exceeded`) on PG14/PG18/race; now uses `unlimitedWindowPolicy`.

### Manual Checks Remaining
- MANUAL: Trust page visual check in a browser (rendering covered by vitest).

### Other runs
| Run | Result |
|---|---|
| e2e (`./e2e`, PG17) | ok 176 s, 78 PASS, 13 skipped (LLM-gated); CHECK-A05 PASS after fix 1 |
| Perf gate (`-tags perfgate`, `PG_SAGE_PERF_SCALE=small`) | PASS 180 s; `sage.trust_ledger_state` 1 row, negligible; the rollback-facts query on `sage.decision` appears among generic-plan suspects, not offenders |
| PG14 :55414 (touched packages) | all ok after the test fix; flaky reruns: retention `TestRun_DoesNotWaitForTheConversion` (timing, ok on rerun), executor `TestOperatorQueueTimeoutRefuses` (timing, ok on rerun) |
| PG18 :55418 (touched packages) | all ok after the test fix (cmd fleet connect timeout ok on rerun) |
| `-race` (touched packages, PG17) | all ok, no data races |
| golangci-lint | 0 issues (after the final merge) |
| Web | `npm ci`, vitest 58 files / 316 tests passed, eslint clean on new files, dist rebuilt and committed |

### Mutation check (rules of demotion and promotion)
25 mutations, each run against the tests named for it on PG17: **25/25 KILLED**.

| ID | Rule broken | ID | Rule broken |
|---|---|---|---|
| M01 | demote two levels | M14 | ramp still decides for the ledger |
| M02 | demotion floor L0 | M15 | rollbacks governed by the ledger |
| M03 | hygiene neutral never credit | M16 | grandfather MODERATE without the ramp |
| M04 | tuning neutral credit | M17 | grandfather twice |
| M05 | insufficient/unverifiable credit | M18 | self level re-derived from evidence |
| M06 | no-gain revert is a demerit | M19 | successes not reset by a demerit |
| M07 | operator rollback not a demerit | M20 | demotion not notified |
| M08 | old demerit re-demotes | M21 | self L3 needs a window |
| M09 | rejection not a demerit | M22 | incident-family demerit path |
| M10 | successes bar off by one | M23 | self takes every downgrade signal |
| M11 | floor ignored | M24 | self class without family |
| M12 | success-rate bar ignored | M25 | standing rollback not marked |
| M13 | cap ignored | | |

M12 and M20 first failed to compile (rewritten); M22 first survived as an equivalent mutant
(the condition was redundant: removed, mutant redefined).

### Post-test audit
- Inputs not covered: a verdict class `sage.action_outcome` may add later (unknown classes are
  dropped, tested); clock skew between the monitored database and the sidecar (cursors and
  "known before the level was set" both use database time).
- Assertions checked to fail on a no-op body: demotion, grandfathering, cursors and the trust
  view assert levels, events, counts and causes read back from Postgres, not just `err == nil`.
- Test doubles: limiter and notifier fakes are used only in gate/unit tests; every ledger rule
  also has a real-Postgres test (`*_db_test.go`), and the executor's ledger path runs through
  the real `earned.Service` (`trust_ledger_db_test.go`).

## Coordinator answers and follow-ups (2026-10-03)

Product calls 1-12 confirmed as made. Open questions answered:

- `trust.level` / `tier3_*` stay **permanently** as the operator's ceiling and kill switch:
  the ledger grants, the operator caps. The startup `TRUST:` lines, the `/api/v1/trust`
  `meaning`, the config doc strings and `docs/configuration.md` say exactly that.
- Demotion notification retry: left as is (the demotion persists; the Trust page shows it).
- `statistics` / `reindex` verification: queued separately, not in this PR.

**Follow-up 1: the Trust view costs a constant number of statements.** Every evidence read is
one set-based statement over a scope (one pair, or the whole database), in
`earned/store_evidence_set.go` and `store_evidence_records.go`. The bench reports are one
`LATERAL` over the families. The view reads, once each: levels, pending proposals, last
changes, grandfathering, bench reports, game days, shadow reviews, live records, family
safety and class records. The annotation reads the error budget and the HA role once, plus
every family's safety record in one statement. A pair's `Evidence` runs the same statements
filtered to that pair, so the per-pair and grid paths cannot disagree. The old per-pair
implementations are removed. Before: about 200 statements per database (7 per incident pair,
plus budget, HA and safety for each family). After: a fixed number (bounded at 12 by the
test), however many pairs or outcomes there are. Guards:

- `TestTrustViewReadsAConstantNumberOfStatements` uses a traced pool and checks the count
  against the bound, with empty and with populated data.
- `TestTrustViewAgreesWithPerPairReads` holds the view against the per-pair reads.
- `/api/v1/trust` joins the small perf gate's timed endpoints.

**Follow-up 2: class trust on approval cards.** `Card.trust` gives family, class, level,
effective level, cap, provenance, evidence counts, next level and the path to it (unmet
checks with their ETA). The trust also appears as one `Trust:` line in Slack and Telegram,
and as one line with a Trust page link in the UI. The loader reads the database's annotated
view once per request (one card, or the whole pending list) through `earned.Registry`; the
API and the executor/snooze notifiers are wired to it. An unreadable ledger shows as
"unavailable"; a database without a ledger shows nothing. The trust is not part of the card's
content hash. The action-to-pair mapping is `earned.PairForQueued`, the same mapping the
reconciler uses to attribute rejections.

### Follow-up test results

**Command:** `go test -v -count=1 -cover -p 2 ./internal/earned/... ./internal/approvalcard/
./internal/api/ ./internal/config/ ./internal/store/ ./internal/mcp/ ./internal/executor/
./internal/policy/ ./cmd/pg_sage_sidecar/` (Docker golang:1.25, PG17 :55476)
**Total:** 4251 passed, 0 failed, 0 skipped
**Coverage:** earned 89.7%, approvalcard 89.6%, api 79.1%, config 91.1%, store 76.1%,
mcp 79.5%, executor 87.0%, policy 89.3%, cmd/pg_sage_sidecar 81.5%. All touched packages are
above their thresholds.

- Skipped tests: none in the touched packages. e2e: 13 skipped, all LLM-gated.
- Failures: none on the final code. The first run (before the LATERAL rewrite) failed three
  tests:
  - `TestApprovalCardCarriesTheClassTrust`: a test logic error. It read `cards[0]`, but the
    shared database holds other tests' pending items. Fixed in `a1280ce8`.
  - Two timing tests under load: policy `TestLeaseQueueDisjointTargetsDoNotWait` and cmd
    `TestComposedSRE_M6_DetectorOpensAnLWLockInvestigation`. Neither package was changed by
    the follow-ups, and both passed in the final run.
- **Small perf gate:** the first run FAILED with 2 offenders: sequential scans of
  `sage.sre_autonomy_outcomes` and `sage.sre_autonomy_events` by the grouped Trust view reads,
  with `/api/v1/trust` at 175 ms. After the per-pair LATERAL rewrite (`065ffd4a`) the gate
  PASSES with 0 offenders and `/api/v1/trust` at 51 ms.
- e2e: ok (183 s, 78 passed, 13 LLM-gated skips).
- PG14 and PG18 (earned, approvalcard, api): ok.
- golangci-lint: 0 issues. Web: vitest 58 files / 318 tests passed, eslint clean, dist
  rebuilt.
- **Mutation check of the new rules: 5/5 killed.**
  - N1: the annotation reads each family's signals per family.
  - N2: the view reads each pair's evidence one by one.
  - N3: the pending card list reads trust per card.
  - N4: an unreadable ledger is not shown.
  - N5: a pair takes another class's live record. This one first survived, because the
    agreement test compared two paths that share code; the test now also checks each pair
    against the store's own single-pair reads.
- Statements: an annotated Trust view takes 11 statements however many pairs and outcomes
  there are; the test bounds it at 12.

## Open questions

- None outstanding from this PR (see the answers above).
