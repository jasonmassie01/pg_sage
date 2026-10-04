# W2b: shadow mode as the default way to earn trust (roadmap 1.4), report (2026-10-04)

Branch `claude/w2b-shadow-mode` (from `origin/claude/w2a-one-trust-system`). Roadmap item 1.4:
*"Below the earned level, pg_sage records every action it would take with its predicted
effect and scores it later (HypoPG/replay, what the human did, what happened). Thousands of
scored shadow decisions instead of 3-20 hand reviews."*

## What was missing

Under the unified ledger (W2a) a self-initiated class below L3 either produced a manual
script (L1) or a one-click handoff (L2). Neither left a record of what pg_sage would have
done, so the only evidence a class could earn was real executed actions, which a class at
L1 rarely gets (operators seldom run the scripts): the chicken-and-egg the roadmap names.

## Design

```
 finding / custodian proposal ──► policy.Gate ──► verdict (observe-only, handoff, ...)
                                      │  Decision.Trusted: the verdict had the class been L3
                                      ▼
            executor.recordShadow (granted level < L3, verdict withheld for want of trust)
                                      │  SQL, rollback, prediction, evidence, both verdicts
                                      ▼
                           sage.shadow_decision (monitored db, pending)
                                      │  shadow.Scorer, every reconcile interval
                                      ▼
   operator decision > same change via pg_sage > same change outside pg_sage > HypoPG > none
                                      │  scored (correct / incorrect / neutral / unscored)
                                      ▼
    earned.Reconciler shadow pass: counted scores ──► sage.trust_shadow_evidence (control db)
                                      ▼
            ClassRecord: real and shadow successes ──► promotion bar ──► admin approves
```

### 1. Recording (`internal/executor/shadow_record.go`, `internal/policy/autonomy_trusted.go`)

- The gate's `restrictAutonomy` now also says what it would have decided had the pair been
  trusted at L3 (`policy.Decision.Trusted`): execute, or a handoff the ceiling or an L3
  condition (one target, the window) would still require. Same inputs, no extra reads, never
  changes the verdict. Nil when the pair is already L3.
- `processFinding` (and a withheld custodian proposal at its first authorization) calls
  `recordShadow` with the gate's decision. A shadow is recorded when:
  - the verdict withholds the action for want of trust: observe-only (trust level or ledger),
    queue for approval (ceiling or ledger handoff), or blocked by a disabled tier flag; hard
    stops (e-stop, executor off, replica, provider, change class, limits, windows, park) are
    not shadowed;
  - the request is a self-initiated class (family tuning or hygiene);
  - the class's granted level is below L3 (`earned.Limiter.GrantedLevel`). Promoted to L3:
    shadow stops (also when the ceiling still queues it); demoted: it resumes.
- Dedupe: `Store.Seen` (one statement) bumps a pending decision of the same fingerprint
  (class, object, normalized SQL) or reports one recorded inside the 24 h window; only then
  does the cycle compute the prediction. A partial unique index keeps one pending decision
  per (database, fingerprint); `Record` is `INSERT ... ON CONFLICT DO UPDATE`, so two cycles
  recording the same finding at once leave one decision with both sightings counted.
- What is recorded: the exact SQL; the rollback (for a config change the one that restores
  the captured prior value, via the same `prepareConfigChange` as real actions); the
  prediction (`predictEffect`, split out of `predictAction`: the same model, targets and
  maintenance baseline, without freezing a verification baseline); evidence (finding
  category/severity/detail, before-state, policy detail, evidence id); the gate's verdict
  and reason; the verdict had the class been trusted (or the ceiling's verdict when the gate
  decided before the ledger); the granted level; the decision id.
- Cost: no EXPLAIN, no HypoPG; reads only catalogs (`pg_settings`, `pg_class`, `pg_index`),
  statistics views and sage tables, so it takes no lock on a user object (tested with
  ACCESS EXCLUSIVE held on the target).

### 2. Scoring (`internal/shadow`)

`Scorer.RunOnce` (per database, on `sre.autonomy.reconcile_interval`) reads the oldest 200
pending decisions, the operator decisions and actions since the oldest one (one indexed read
each, matched in Go by `Shape`), then per decision:

| Order | Source | Evidence | Score | Counted |
|---|---|---|---|---|
| a | `operator` | approval item of the same shape decided after the decision | rejected: incorrect; approved: by its action's verdict | no (real outcome already) |
| b | `applied` | the same shape run later through pg_sage (operator or pg_sage) | by its verdict and lifecycle | no (real outcome already) |
| b | `external` | index create/drop: an index of the same shape on the table, or the index gone (catalog); then verified with `verify.DecideTargets` over query_store windows around [last proposed, detected] | by the verdict | yes |
| c | `hypopg` | index create, after `ScoreAfter` (24 h), targets ran since recording, HypoPG available, 10 per pass | the optimizer's what-if bar (`llm.optimizer.hypopg_min_improvement_pct`): verified correct, rejected incorrect | yes |
| d | `none` | nothing by the horizon (7 days) | unscored | no |

Verdict to score by the family rule: improved correct; regressed or an operator rollback
incorrect (except the no-gain revert of a neutral index create); neutral correct for hygiene
when it held, else neutral; insufficient/unverifiable unscored. Matched evidence still being
verified is waited for, up to 21 days. A score is written once (`WHERE status = 'pending'`),
so racing scorers score a decision once. Never from LLM text.

### 3. Trust ledger (`internal/earned`)

- Reconciler shadow pass (`selfinit_shadow_reconcile.go`): counted scores from the monitored
  database into `sage.trust_shadow_evidence` (control database), one row per decision, own
  cursor (`trust_ledger_state.shadow_cursor`). It never demotes.
- `ClassRecord` (one query, `selfinit_store.go`): real and shadow apart. Shadow successes and
  uncredited count **distinct decisions** since the last demerit (a decision re-recorded each
  day cannot inflate the bar). An incorrect shadow decision is a demerit for the count
  (`last_demerit: shadow_incorrect`), never a demotion.
- Promotion bar (`selfinit_shadow.go`): L2 counts real + shadow (shadow alone can earn it).
  L3: `class_successes` counts real + at most `min_successes_l3 - 3` shadow (7 of 10), and a
  new check `class_real_successes` needs **>= 3 real verified successes** (`MinRealSuccessesL3`;
  when fast elevation lowers `min_successes_l3` below 3 the minimum follows it and shadow
  fills nothing). The success rate counts every decided outcome, real and shadow.
- Proposals show the split: the class record in the evidence carries
  `shadow_successes_since_demerit`, and `class_successes` reads e.g. `10 (3 real, 7 shadow;
  16 more shadow over the cap)`. Trust rows carry `shadow_correct/incorrect/neutral`.

### 4. Surfaces

- `GET /api/v1/shadow-decisions?database=&class=&status=&score=&limit=` (new route file
  `api/shadow_routes.go`, every signed-in role): per database the per-class summary and the
  newest decisions.
- `GET /api/v1/trust` carries each database's per-class shadow summary as `shadow`.
- Trust page: per class, shadow counts by score and "What pg_sage would have done" (SQL,
  rollback, prediction, verdict had it been trusted, score, source, reason), loaded on demand.
- Approval card: `shadow_history` (class summary and this proposal's own shadow decision);
  not part of the card hash.
- Prometheus: `pg_sage_shadow_decisions_total{database,class,verdict}` and
  `pg_sage_shadow_scores_total{database,class,score,source}`.
- Retention: scored decisions age out on `retention.actions_days`, pending ones are kept;
  `trust_shadow_evidence` is exempt promotion evidence (like `sre_autonomy_outcomes`).
- Perf gate: seeds shadow history (HistoryRows/10 decisions, the ledger's counted copy).
- Docs: `docs/configuration.md` "Shadow mode"; CHANGELOG bullet under Unreleased.

### Schema

`internal/schema/shadow_mode_migration.go`, registered after `ddlTrustLedger()`:
`sage.shadow_decision` (checks on verdicts, status/score/source consistency, counted only
for external/hypopg), `sage.trust_shadow_evidence` (unique per decision), the cursor column
(added only when the catalog lacks it), six indexes through the checked index loop.

## Product calls (made under the product principle; please confirm)

1. **Shadow covers the self-initiated classes** (tuning, hygiene): findings and withheld
   custodian work. Incident remediations keep their own evidence (bench, packet reviews) and
   are not shadowed here.
2. **"Below the earned level" is the granted level below L3.** A class promoted to L3 is not
   shadowed even when the ceiling (`execution_mode: approval`, `advisory` for MODERATE) still
   queues it; a granted L3 that a downgrade signal holds back is not shadowed either. A class
   capped below L3 (retention, irreversible: L1) is shadowed forever; its evidence can never
   promote it, but "what pg_sage would have done" stays visible.
3. **Shadow records under every trust ceiling, including the default `trust.level:
   observation`**, so a new install earns trust from day one. The verdict "had the class been
   trusted" is then the ceiling's own (observe-only), which tells the operator that
   trust.level, not the class, holds it. Hard stops and parks are not shadowed (trust is not
   what withholds those). Nothing is shadowed with `sre.autonomy.enforce: false` (no ledger
   level to be below), in `manual` mode or with the executor off (the cycle does not run).
4. **No double counting.** Operator and applied (through pg_sage) scores are shown and scored
   but do not count as shadow evidence: the ledger already counts the same action as a real
   outcome (its verdict, rollback or rejection). Only changes applied outside pg_sage and
   what-ifs count, the evidence only shadow mode produces.
5. **Distinct decisions, not sightings.** One decision per fingerprint per 24 h, one pending at
   a time, and the ledger counts shadow successes by distinct fingerprint since the last
   demerit, so an unapplied proposal re-recorded daily cannot fill the bar by itself.
6. **L2 from shadow alone; L3 needs >= 3 real verified successes** (`MinRealSuccessesL3`),
   with shadow filling at most `min_successes_l3 - 3` (7 of 10). L2 still puts a person on
   every action, which is where real outcomes come from. When fast elevation lowers
   `min_successes_l3` below 3, the real minimum follows (never a shadow-only L3).
7. **An incorrect shadow decision resets the streak** (cause `shadow_incorrect`) and never
   demotes; only real regressions, rollbacks and rejections demote.
8. **The success rate counts all decided shadow outcomes** (uncapped): it measures decision
   quality, which is what shadow evidence is about.
9. **External detection covers index creates and drops** (the catalog says when an index of
   the same shape appears or the wanted one disappears). Configuration changes made outside
   pg_sage are not detected (open question).
10. **HypoPG only after a day and only while the targets still run**, at the optimizer's own
    bar, 10 per pass. The what-if plans the targeted queries (AccessShareLock on their
    tables, 5 s statement timeout, the optimizer's session code); it never builds anything.
    Recording itself never calls it.
11. **Shadow windows are constants** (dedupe 24 h, what-if after 24 h, horizon 7 d, matched
    evidence waited for up to 21 d); external verification uses `verify.*`. No new config
    keys.
12. **The Trust API carries the per-class shadow summary**, so the Trust page stays one
    request; the decision list loads on demand from `/api/v1/shadow-decisions`.
13. **Shadow history is not part of an approval card's hash**: it changes as decisions score,
    the action does not.

## Tests

Tests were written first (`559916d1`) and run afterwards. Integration tests run against real
Postgres (PG17 `pgsage-ag8`) with real HypoPG, pg_stat_statements, real indexes created "by a
migration" and seeded `sage.query_store` series.

| Requirement | Where |
|---|---|
| shadow recorded below the level, never executed, prediction, rollback, verdicts | `executor/shadow_record_db_test.go` (`TestShadowRecordedBelowTheEarnedLevel`, `...CarriesTheRollbackOfAConfigChange`, `...UnderTheObservationCeiling`, `...WithheldCustodianWork`) |
| no lock on user objects | `TestShadowRecordingTakesNoLockOnUserObjects` (ACCESS EXCLUSIVE held on the target) |
| promoted: stops; demoted: resumes | `TestNoShadowAtL3AndShadowResumesAfterDemotion`, `TestNoShadowForATrustedClassHeldByTheCeiling` |
| approval queue unchanged, shadow beside it | `TestShadowRecordedAlongsideTheApprovalQueue` |
| concurrency: two cycles, one finding | `TestConcurrentCyclesRecordOneShadow`, `shadow/TestConcurrentCyclesRecordOneDecision` |
| dedupe per fingerprint per window | `shadow/TestSeenDedupesPerFingerprintPerWindow`, `TestNoSecondShadowInsideTheWindowAfterScoring` |
| every scoring source and precedence | `shadow/score_test.go` (pure `judge`), `scorer_db_test.go` (operator rejected, approved, waiting; decided before recording; applied by shape; operator over applied), `scorer_sources_db_test.go` (external create verified; external window; external drop held and regressed; HypoPG verified and rejected; idle targets; budget; applied over a passing what-if) |
| concurrency: racing scorers | `TestConcurrentScorersScoreOnce` |
| ledger counting, shadow vs real, cap, real minimum | `earned/selfinit_shadow_test.go` (3/7/10 boundaries, capped, too few real, lowered bar, rate), `selfinit_shadow_db_test.go` (counted only, distinct, incorrect resets but never demotes, L2 proposal from shadow and admin approval, L3 never from shadow alone, proposal shows the split, concurrent reconcilers, trust view) |
| gate verdict had the class been trusted | `policy/gate_shadow_test.go` |
| API, trust summary, approval card | `api/shadow_routes_test.go`, `approvalcard/shadow_history_db_test.go` |
| schema, retention, metrics | `schema/shadow_mode_migration_test.go`, `retention/shadow_retention_test.go`, `shadow/metrics_test.go`, `cmd/pg_sage_sidecar/shadow_runtime_test.go` |
| UI | `web/src/pages/trust/ShadowDecisions.test.jsx`, `TrustPage.shadow.test.jsx`, `actions/ShadowHistory.test.jsx`, `actions/ApprovalCard.shadow.test.jsx` |
| e2e | `CHECK-A10`: no shadow decision for a class trusted at L3 in the binary pipeline |

### Mutation check (scorer, gate verdict, recording, promotion counting)

38 mutants, each run against the tests named for it on PG17 (script kept outside the repo):
**37 killed, 1 equivalent (code removed)**.

| ID | Rule broken | ID | Rule broken |
|---|---|---|---|
| S01 | operator decision ignored | P01 | no shadow cap at L3 |
| S02 | applied change ignored | P02 | no real minimum |
| S03 | what-if before the settle delay | P03 | real-success check dropped |
| S04 | what-if for any class | P04 | shadow counted per row, not per decision |
| S05 | operator scores counted | P05 | incorrect shadow not a demerit |
| S06 | external scores not counted | P06 | uncounted scores copied to the ledger |
| S07 | tuning neutral is correct | P07 | shadow neutral credited |
| S08 | operator rollback not incorrect | P08 | L2 needs real outcomes |
| S09 | no-gain revert incorrect | P09 | rate ignores shadow neutrals |
| S10 | horizon off by one | P10 | shadow successes not in the total |
| S11 | matched evidence waited for forever | G01 | trusted verdict ignores the ceiling's approval |
| S12 | operator decision before recording counts | G02 | trusted verdict at L3 |
| S13 | applied change matched without shape | G03 | L3 blocker ignored |
| S14 | score written twice by racing scorers | E01 | hard stops shadowed |
| S15 | inconclusive external verification final early | E02 | L3 classes shadowed |
| S16 | dedupe window ignored | E03 | per-window dedupe skipped |
| S17 | concurrent sightings not counted | E04 | no trusted verdict under the ceiling |
| S18 | CONCURRENTLY kept in the shape (equivalent) | E05 | config rollback not captured |
| S19 | idle targets get a what-if | | |
| S20 | what-if budget ignored | | |

First round: S12 survived (the pass reads decisions from its oldest pending one, which hid
the check; the test now has an older pending decision: killed); S18 survived as an equivalent
mutant (everything before ON is skipped anyway; the redundant code was removed); S19 and E03
first failed to compile (rewritten: killed). An incident-family check in `recordShadow` was
equivalent too (the family resolver already excludes incident families) and was removed.

## Test Results

**Command:** `go test -v -count=1 -cover -p 2 ./...` (Docker golang:1.25, `--cpus=2`, PG17
`pgsage-ag8` :55478), on the final implementation
**Total:** 12183 passed, 1 failed (load flake, passed on rerun), 21 skipped
**Coverage (touched packages):** shadow 87.7%, policy 89.7%, earned 88.7%, executor 87.1%,
api 79.1%, approvalcard 88.4%, schema 84.4%, retention 86.9%, optimizer 88.4%,
testsupport/perfgate 91.0%, cmd/pg_sage_sidecar 81.6%

### Skipped Tests (must be zero or justified)
- 21 environment-gated, the same set as the base branch: live cloud provisioning (AWS RDS,
  Cloud SQL, Lakebase, 3 AgentDB gauntlets, 2 Azure), live LLM providers (2 chat, Tier2
  Gemini), container failover/restart/promotion fixtures (3), PgBouncer (3),
  `TestPGIncidentBench` (own CI step), `TestGeneratePlanFixtures` (regeneration only),
  `TestRCAChildProcessFixture` (child-process helper), `TestResolveLogDir_AbsoluteWindows`
  (Windows only). None in this branch's code; the HypoPG shadow tests ran on PG14, 17 and 18
  (they skip with a stated reason only where HypoPG or pg_stat_statements is missing).

### Failures (if any)
- `cmd/pg_sage_sidecar` `TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident`:
  "incident ... was not persisted: context deadline exceeded" under full-suite load; and
  `internal/analyzer` passed its tests but its fixture cleanup timed out connecting. Both
  packages rerun alone: ok (81.6%, 91.6%). Neither touches shadow mode.
- PG18 (touched packages): `TestFleetReloadThroughWatchedYAMLFile` (connect timeout, the known
  load-sensitive fleet test) and `policy` `TestLeaseQueueTimeoutReleasesPosition` (timing;
  passed 3x on rerun). `shadow` `TestScorerWhatIfIsBudgetedPerPass` failed on PG18 because of a
  test bug, fixed (`test(shadow): look up the what-if target's queryid by its own text`): the
  fixture's LIKE also matched its own pg_stat_statements lookup, which PG18 returned first.
  After the fix: shadow ok on PG14, 17 and 18 (and `-count=3` on PG18).

### Coverage Gaps (packages below threshold)
- None among touched packages. Untouched packages below 70%: `cmd/reset_admin_for_test`
  50.0%, `cmd/create_admin` 51.5%, `cmd/sigstore_trusted_root` 56.1%,
  `internal/testsupport/pgssepoch` 58.3%, `sre-bench` 62.4% (tools and test support; the 50%
  utility floor is met).

### Bugs Found This Session
1. [BUG] `policy` test asked `policy.Gate` for `Explain` (the method lives on
   `policy.Explainer`); fixed in the test, noted in the feat commit.
2. [BUG] `cmd` test read the what-if bar from `cfg.Optimizer` (it is `cfg.LLM.Optimizer`);
   fixed in the test.
3. [BUG] `executor` test built two rigs in one test: each holds the package's cross-package
   advisory lock on its own connection, so the second deadlocked (10-minute timeout); one rig
   now.
4. [BUG] `executor`: a helper name collided with a test helper (`policyVerdict`); renamed
   `gateVerdictOf`.
5. [BUG] `shadow` fixture picked its own pg_stat_statements lookup as the what-if target on
   PG18 (above).
6. [CODE] Two redundant checks found by equivalent mutants were removed (index-create shape,
   incident-family filter in `recordShadow`).

### Manual Checks Remaining
- CHECK-M1: MANUAL, the Trust page shadow rows and "What pg_sage would have done" list, and the
  approval card's shadow line, in a browser against a live sidecar (covered by vitest only).
- CHECK-M2: MANUAL, a week of shadow scoring on a dogfood database (external index changes and
  what-if scores at real scale).

### Other runs
| Run | Result |
|---|---|
| e2e (`-tags=e2e ./e2e`, PG17) | ok 183 s, 78 PASS, 13 skipped (LLM-gated); CHECK-A10 (new) PASS |
| Perf gate (`-tags perfgate`, `PG_SAGE_PERF_SCALE=small`) | PASS 161 s, 0 offenders; `sage.shadow_decision` 2000 rows, 0 seq scans (35 index scans, the scorer scored 31 seeded decisions); `sage.trust_shadow_evidence` 0 seq scans, the reconciler copied 975 counted scores (inserts 0.05 ms mean) |
| PG14 :55414 (touched packages) | all ok |
| PG18 :55418 (touched packages) | ok after the fixture fix; two known timing flakes ok on rerun |
| `-race` (touched packages, PG17) | all ok, no data races |
| golangci-lint | 0 issues |
| Web | `npm ci`, vitest 62 files / 327 tests passed, eslint clean on changed files, dist rebuilt and committed; `node_modules` deleted afterwards |

### After merging the W2a follow-ups (`29483aef`)

`origin/claude/w2a-one-trust-system` moved by 7 commits (set-based Trust view, class trust
on approval cards). Conflicts: the class record moved to W2a's set-based read
(`store_evidence_records.go`): the shadow counts and the shadow demerit now live there; the
trust handler; the web dist (rebuilt). Afterwards on PG17: earned 89.6%, approvalcard 89.3%,
shadow 87.7%, api 79.1%, executor 87.0%, policy 89.7%, schema 84.4%, retention 86.9% all ok;
`cmd/pg_sage_sidecar` ok except `TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident`
(under concurrent lint load; 3/3 ok alone, untouched code); e2e ok (187 s); perf gate PASS
(160 s, 0 offenders); vitest 62 files / 329 tests; lint 0 issues; the ten ledger mutants
(P01-P10) re-run against the merged code: 10/10 killed.

### Post-test audit
- *Inputs not covered:* a migration that creates an index of the same shape under a
  different predicate spelling the planner normalizes (`a > 1` vs `1 < a`): shapes differ, the
  decision stays pending to the horizon (unscored, never wrongly scored); a queue item whose
  SQL was edited by an operator before approval (shape differs: not matched).
- *Assertions that pass when broken:* the mutation table above; the applied-change test was
  strengthened (a shape-blind matcher passed it before).
- *Test doubles hiding failures:* the policy tests use a fake limiter; every ledger rule also
  has a real-Postgres test, the executor tests use the real `earned.Service`, the scorer tests
  use real HypoPG, pg_stat_statements, real indexes and query_store series.

## Open questions

- Should operator/applied scores count as shadow evidence *instead of* the real outcomes they
  rest on (today: shown, never counted, to avoid counting one action twice)?
- External detection of configuration changes made outside pg_sage (GUC in `pg_settings`,
  reloptions in `pg_class`): worth adding, or are they too often set for unrelated reasons?
- Should a what-if score weigh less than an observed one (a separate, smaller cap for
  `hypopg` within the shadow share)?
- Expose the shadow windows (dedupe, settle delay, horizon) as configuration, or keep them
  constants?
- `GET /api/v1/trust` now also reads each database's shadow summary (one aggregate over
  `sage.shadow_decision`, bounded by retention); the W2a question of a cached trust view
  applies to it too.
- Shadow mode is per self-initiated class; should incident families move from hand-reviewed
  packets to the same scorer (their "applied" evidence is the recovery verdict)?
