# pg_sage: full feature audit, improvement roadmap, and AI SRE specification

**Date:** 2026-09-26. **Baseline:** v1.5.0, master,
`b396595059d2b1b312a22231fdbfd1d2cfef9530`.
The canonical checkout was clean and matched remote HEAD. Review and tests used an
independent snapshot. Original source and running services were preserved.

## Decision

**Do not expand autonomous production authority yet.** The product has valuable
working capabilities, but the audit found destructive retention defects, authorization
gaps, broken lifecycles, misleading verification claims and unfinished production paths.
This is a completed audit and design deliverable, not a claim that pg_sage is repaired
or bug-free.

The most important finding is reproduced data loss: retention selected one expired row
and deleted two rows across partitions, including a future-dated row. Independent review
confirmed this path bypasses the central executor gate and can continue after application
emergency stop when retention preconditions are met. Disposable fixture changes were
rolled back; no customer data was involved.

**Recommended new feature: Sage Incident Investigator.** Extend existing RCA and Cases
into a persistent investigation that gathers fresh evidence, tests competing hypotheses,
explains uncertainty, proposes supported actions through existing authority, and verifies
recovery independently. Start with read-only investigations for blocking, connection
pressure and WAL/replication retention. Add approved interventions after repairing the
relevant safety and lifecycle defects.

## Contents and coverage

This consolidated file includes the enriched brief, all three audit reports, independent
cross-review, research, SRE specification, implementation contracts, exact verification
report and review limitations. Companion files remain available for focused reading.
Source citations refer to the pinned snapshot, not a moving branch.

Feature matrices cover collection and individual rules, all six advisors, optimization,
hints, actions/verification, RCA/logs, schema/migrations, policies/custodians, fleet,
AgentDB/providers, API/auth/UI, value/shadow, alerts/metrics, deployment and vector lab.
An entry with no additional confirmed defect is not exhaustive proof of correctness.

## Highest-priority findings

| Priority | Finding | Evidence and next action |
|---|---|---|
| P0 | Retention deletes noneligible rows across partitions | R01, executed SQL reproduction; exact physical identity and strict batch bounds |
| P1 | Retention bypasses stop and central policy | R02/R03, independently traced; one authorized execution path and bound evidence |
| P1 | HTTP MCP write tools omit caller-role checks | SURF-01; trusted principal and viewer-denial tests on the real mounted router |
| P1 | OIDC links existing users by unverified email | SURF-02; issuer-dependent takeover risk; issuer/subject identity and deliberate linking |
| P1 | Suppression resurrects findings; new SQL retains old inverse | C02/C04, failing regression probes; stable identity and atomic forward/inverse updates |
| P1 | Rollback accepts missing evidence or finishes before revert | C14/C15; separate measurement verdict from intervention completion |
| P1 | Real cache percentages are treated as fractions | C01, actual collector SQL probe; canonical units across all consumers |
| P1 | Incident restart/manual resolution is not durable | R04; one state owner, version checks and restart recovery |
| P1 | Fleet omits internal retention cleanup | C08; unified per-database runtime construction |
| P2 | Value loses actual activity; success/dry run become verified | SURF-03/06/12/14; correct storage topology, identity and proof states |

Other substantiated gaps include stranded advisor work, optimizer identity collisions,
installed hints surviving retirement, descending-sequence blindness, false query-volume
growth, corrupted query IDs, unavailable provider readiness despite existing dependencies,
discarded Terraform content, partial AgentDB monitoring, lost alert delivery, dead UI
paths, unwired forecasting/adaptive monitoring/fleet rollout and empty-workload rehearsal.
Detailed entries supply triggers, evidence, limitations and acceptance tests.

## Repair and investment order

| Wave | Scope | Completion evidence | Value measure |
|---|---|---|---|
| 0: authority/data safety | R01-R03, SURF-01/02/17, C04/C14/C15 | Stop/role/partition/crash fixtures prevent forbidden writes; exact actor/payload; restart-safe reverts | Zero wrong-target writes or lost reversals |
| 1: identity/truth | C01-C07, C17/C18, R04/R05/R10, SURF-03/09/11/12/13 | Stable IDs, canonical units, preserved observation times, explicit unknown state | Fewer stale/duplicate cases and false verified outcomes |
| 2: complete runtime paths | C08-C13, R06-R09, SURF-04/07/08/14/15 | Real-binary parity across runtime modes; working result or visible unsupported disposition | Promised workflows completing end to end |
| 3: improve existing features | Full matrices below; SURF-05/06/10/16/18/20 | Workload-aware advice, credential refresh, restore proof, complete Cases controls, reliable alerts | Operator task time, precision and delivery success |
| 4: SRE R1 | Three read-only incident families, durable evidence and Cases | Prerequisite repairs, replay benchmark, budget/permission/freshness tests | Useful packets and measured operator minutes saved |
| 5: SRE R1.1/R2 | Approved cancel handoff, SLI/change context, vector proof, rehearsal | Per-action admission and independently verified recovery | Recovery without harmful intervention; cohort quality |

These are dependency waves, not a promise to ship everything in one release. Delete dead
facades with no current purpose. Complete partial features only when the outcome remains
valuable. Distinguish unsupported, unavailable, unconfigured and degraded states.

## Verification interpretation

The appendix records all runs, including initial fixture failures and corrected results.
Baseline suites are broad and mostly green; focused boundary probes still reproduce bugs.
Package coverage is not proof of a complete workflow.

**The sidecar entry-point package is at 43.5% statement coverage**, below the 70% business
threshold. Corrected unit/integration runs meet thresholds for internal business packages.
This entry-point gap is an unresolved release gate. Audit probes are not a replacement
coverage run, and production remediation is not claimed complete.

Build, vet and Go lint pass. Frontend lint/build pass with a bundle-size warning.
Component tests: 89 passed. Mocked browser tests: 54 passed. These do not prove real
backend/provider commissioning. Dependency audit reports three moderate package entries
for one Vitest development-tool advisory, with actual scope explained in the runtime audit.

No live cloud provisioning or real LLM-quality validation occurred. Credential-dependent
tests are explicitly skipped. No complete legacy C-extension matrix, PostgreSQL 14-18
cross-version fleet, long soak or provider failover was exercised. Those remain gates.

## Research judgment

Research uses 31 linked primary sources: PostgreSQL behavior, operator issue reports,
providers, competitors and vector papers. Generic AI RCA is crowded and partly present
already. The opportunity is Postgres-specific evidence, bounded investigation, honest
abstention and proof of recovery under comparable workload. Vendor scope is distinguished
from independently demonstrated capability; social and market-validation gaps are listed.

The spec includes incident walkthroughs, eight scoped data entities, APIs/UX, worker and
lease recovery, probe/model budgets, permission boundaries, replay evaluation, 35 acceptance
checks and rollout. Targets are proposed, not measured. Investigation and recovery have
separate time budgets. The implementation appendix makes the first slice concrete.

## Review method and limits

Three parallel review/research tracks were combined with coordinating runtime review,
disposable-database probes and independent cross-review of destructive paths.
The pinned Gemini review was attempted but not executed: automatic approval review
rejected external upload of authentication source because that sharing was not authorized.
No alternate upload was attempted; local independent review continued.

Original checkout unchanged. Reports and audit-only failing regression probes are the
deliverables. Product fixes and the SRE feature have not been implemented.



## Detailed contents

- [Part 1: Enriched assignment](#part-1-enriched-assignment)
- [Part 2: Core feature audit](#part-2-core-feature-audit)
- [Part 3: API UI security and AgentDB audit](#part-3-api-ui-security-and-agentdb-audit)
- [Part 4: Runtime safety and lifecycle audit](#part-4-runtime-safety-and-lifecycle-audit)
- [Part 5: Independent runtime cross-review](#part-5-independent-runtime-crossreview)
- [Part 6: Independent security cross-review](#part-6-independent-security-crossreview)
- [Part 7: Primary-source research](#part-7-primarysource-research)
- [Part 8: AI SRE build specification](#part-8-ai-sre-build-specification)
- [Part 9: R1 implementation contracts](#part-9-r1-implementation-contracts)
- [Part 10: Verification results](#part-10-verification-results)
- [Part 11: Gemini review status](#part-11-gemini-review-status)

## Part 1: Enriched assignment

### Enriched assignment: pg_sage completion audit and AI SRE design

Act as a skeptical staff engineer, database reliability engineer, security reviewer,
product designer and researcher. Use the current repository, not historical claims.

1. Establish revision, remote freshness, working-tree state, runtime modes and actual
   feature groups. Read prior decisions without treating old test results as current.
2. Trace every group through trigger, config, startup, dependencies, database identity,
   persistence, API, UI, permissions, failure, restart and visible outcome.
   Tests or exported symbols without production callers do not establish delivery.
3. Find concrete bugs, unreachable paths, inert controls, incomplete integrations,
   misleading success/health/value claims and lifecycle transitions that lose work.
4. Try to disprove serious findings. Cite source and enabling conditions; distinguish
   reproduction, source confirmation, plausible risk and missing evidence.
5. Run uncached tests, coverage, static checks, builds, integration and browser checks
   using disposable targets. Preserve exact failures, skips and fixture limitations.
   Add targeted regression probes that can fail when the existing suite passes.
6. Assess every feature for a useful improvement, first slice, priority, dependencies,
   acceptance criteria, safety boundary and measurable value.
7. Research demand and alternatives using current primary sources. Include Postgres,
   managed providers, AI SRE, other database ecosystems, vector quality and agent DBs.
   Separate vendor claims, independent evidence and product judgment.
8. Specify an AI SRE capability beyond existing RCA: personas, incident examples,
   architecture, data model, API, UX, state machine, budgets, authority, recovery,
   tests, rollout and value. Model reasoning must not grant permissions.
9. Challenge missing questions: what refutes the diagnosis; who owns the action;
   what if pg_sage causes harm; what survives restart; what is actually reversible;
   what proves customer recovery; what would make the product unnecessary?
10. Deliver one complete report, supporting evidence and a buildable specification.
    Clearly distinguish proposed checks from executed verification.

Scope is review, reproduction, research and specification. Preserve original source
and live services. Do not publish, contact others, provision paid cloud resources,
or silently start optimization. Repair proposals must be concrete and reviewable.



## Part 2: Core feature audit

### pg_sage core feature audit

Snapshot: `b396595`, reviewed 2026-09-26. Paths below are relative to `audit-repo/`.
This is a source and wiring audit, not a claim that every execution path has been tested.
No product source was changed and no production database action was run by this reviewer.
The parent audit owns the full test run. Three audit-only regression probes were later run
in a separate checkout against disposable PostgreSQL; other reproductions remain proposed.

#### Executive assessment

The core has substantial real implementation: collection, deterministic rules, advisor and
optimizer generation, typed action authorization, dedicated top-level DDL, durable index
verification, and a bounded vector experiment are connected. The weakest boundary is the
lifecycle between observations, recommendations, durable findings, execution, and final
verification. Several independently sound components disagree on identity, units, or state.
These defects can silently suppress detection, defeat suppression, strand remediation,
misrepresent verification, or leave a harmful hint active.

Prioritize C01, C02, C04, C07, C08, C11, C14, and C15 before increasing autonomous scope.
Treat P1 as high impact rather than proof of a currently occurring production incident.

#### Confirmed defects and incomplete wiring

##### C01 — P1: cache-hit metric units disagree across collector and detector

- Evidence: [sidecar/internal/collector/queries.go:148](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/collector/queries.go#L148) multiplies the ratio by 100;
  `collector/collector.go:408-428` scans it without normalization.
  `analyzer/rules_system.go:60-66` compares it with fractional thresholds, default 0.95
  (`config/defaults.go:26`), and multiplies it by 100 again for display at line 76.
  `rca/signals.go:110-118` makes the same fractional comparison.
- Trigger: a real 80% cache hit ratio becomes `80`, which is greater than `0.95` and is
  incorrectly treated as healthy. Near-total cache failure is required to trigger.
- Impact: deterministic detection and RCA miss substantial cache pressure. Forecasting
  currently expects percentages (`forecaster/rules.go:122`), so fixing only the producer
  would introduce a second bug. This needs one explicit canonical unit across consumers.
- Confidence: reproduced: actual collector SQL returned 80 for 800 hits / 1000 accesses.
- Fix/acceptance: persist a versioned fraction, migrate or normalize historical rows, and
  test actual collector SQL through detector/RCA/forecast for 0%, 80%, 94%, 95%, and 99%.
  Unit fixtures that construct `CacheHitRatio: 0.80` alone do not exercise this defect.

##### C02 — P1: suppressing a finding does not suppress the next observation

- Evidence: API moves the row from open to suppressed (`api/handlers.go:324-325`).
  `analyzer/finding.go:48-52` searches only open rows, then inserts an open row at 116-131.
  The unique index also covers only open rows (`schema/bootstrap.go:456-458`).
- Trigger: suppress an ongoing slow-query/index/system finding; run the next analyzer cycle.
- Impact: a new open row for the same category and object is created. Notifications and
  automatic action eligibility can return despite operator suppression. Unsuppressing the
  old row can then conflict with the new open row's unique key.
- Confidence: reproduced: the second observation yielded open=1 and suppressed=1.
- Fix/acceptance: maintain one active identity with explicit suppression policy; honor
  indefinite and expiring suppression in the upsert and executor. Verify three subsequent
  cycles produce no open duplicate, and expiry/reopen produces exactly one identity.

##### C03 — P2: the final cleared finding in a category never resolves

- Evidence: `analyzer/analyzer.go:448-461` constructs categories exclusively from current
  findings, then invokes `ResolveCleared` only for those categories.
- Trigger: a category has one issue, then its next complete evaluation returns no issues.
- Impact: no resolution call occurs for that category. Persistent finding counts, alerts,
  dashboard health and advisor dedup remain stale; resolved-only retention never removes it.
- Confidence: high.
- Fix/acceptance: track categories successfully evaluated this cycle, including empty
  results, and resolve only those. Do not interpret skipped/failed/degraded evaluators as
  healthy. Test one-to-zero, two-to-one, evaluator-error, and collection-unavailable cases.

##### C04 — P1: refreshed forward SQL keeps the previous rollback SQL

- Evidence: the existing-row branch refreshes `recommended_sql` but omits `rollback_sql`
  in `analyzer/finding.go:64-83`; inserts do persist rollback SQL at 119-125.
- Trigger: an advisor/optimizer changes the recommendation for the same category/object.
- Impact: persisted findings can pair new forward SQL with an old inverse. Any later
  approval/manual execution consuming that pair can restore the wrong setting/index.
- Confidence: reproduced: CREATE index_b persisted with DROP index_a. Runtime impact
  depends on the execution surface and is covered by the parent action-API audit.
- Fix/acceptance: version the complete proposal atomically, including inverse, evidence,
  identity and preconditions. Upsert SQL A/inverse A, then SQL B/inverse B; assert every
  read/approval surface returns B/B and invalidates approval for A.

##### C05 — P2: multiple optimizer candidates collapse onto one finding identity

- Evidence: optimizer accepts up to three new candidates per table
  (`optimizer/optimizer.go:25,250-254`); mapping uses `rec.Category` plus `rec.Table`
  (`analyzer/optimizer_mapping.go:19-22`); finding uniqueness is category/object.
- Trigger: two valid same-category indexes are proposed for one table.
- Impact: the second upsert overwrites the first proposal; SQL/inverse mismatch C04 is
  amplified. In-memory findings still contain both but execution shares a finding ID and
  per-object cooldown, so the displayed and executed candidate can disagree.
- Confidence: high.
- Fix/acceptance: use a stable normalized index-definition fingerprint as candidate
  identity and retain table identity separately. Two candidates must persist independently,
  have correct inverse SQL, and obey an explicit per-table portfolio budget.

##### C06 — P2: optimizer's open-finding dedup does not match emitted identity

- Evidence: `optimizer/optimizer.go:426-430` searches `object_identifier LIKE
  'schema.table.%'`; `analyzer/optimizer_mapping.go:22` emits exactly `schema.table`.
- Trigger: an existing ordinary missing-index recommendation for `public.orders`.
- Impact: the intended dedup returns false and re-runs expensive LLM analysis for the same
  unresolved table. Wildcards in names can also produce unintended matching.
- Confidence: high.
- Fix/acceptance: match canonical structured table identity exactly (or use proposal table
  OID). With an open candidate, a second unchanged cycle must make zero generation calls;
  material schema/workload change should explicitly invalidate or refresh the candidate.

##### C07 — P1: an advisor recommendation can become permanently ineligible after its first cycle

- Evidence: advisor returns no findings between intervals (`advisor/advisor.go:76-77`) and
  skips each sub-advisor if an unacted open finding exists (94-183,255-274). Analyzer replaces
  its in-memory findings with only this cycle's results (`analyzer/analyzer.go:429-432`).
  Executor reads that in-memory list (`executor/executor.go:460-462`).
- Trigger: a valid config recommendation arrives during observation mode or outside the
  maintenance window; a later cycle clears it from memory; policy subsequently permits it.
- Impact: the durable finding remains open, blocking regeneration, while the executor never
  sees it again. Merely advancing trust or entering the window does not execute it.
- Confidence: high for advisor path. Optimizer's separate broken dedup C06 currently makes
  its behavior different; do not generalize this exact trigger to optimizer until C06 is fixed.
- Fix/acceptance: execute from a durable candidate queue with freshness/precondition checks,
  or re-emit cached actionable recommendations explicitly. Test observe → autonomous and
  outside-window → inside-window without needing a new LLM recommendation.

##### C08 — P1: fleet mode never schedules the retention cleaner

- Evidence: production search for `retention.New` and `cleaner.Run` finds only standalone
  setup/run (`cmd/pg_sage_sidecar/main.go:889,1033`). Fleet orchestrator at 1828-1884 runs
  executor, briefing and health updates, but no cleaner. Dynamic workers do not instantiate
  this package either. Cleaner covers snapshots, resolved findings, actions, explain cache,
  and query store (`retention/cleanup.go:30-43`).
- Trigger: any long-running fleet instance with normal collection.
- Impact: configured metadata retention is not enforced and telemetry grows without bound
  on monitored fleet databases. This eventually increases storage and scan cost.
- Confidence: high, complete call-site search.
- Fix/acceptance: register per-instance cleanup in the same supervised lifecycle as all
  other workers. Seed expired/live records in two DBs, run lifecycle cleanup, verify bounded
  deletion and isolation; removal/restart must cancel old workers. Extend retention ownership
  to newer evidence/event tables rather than claiming this cleaner covers every sage table.

##### C09 — P2: storage time-to-full forecasting is implemented but has no production caller

- Evidence: `forecaster/forecaster.go:90` defines `ForecastGrowth`; the only production
  `RecordSizeHistory` call is inside it (100). No production call to `ForecastGrowth` exists.
  Analyzer calls only `Forecast` (which runs six legacy forecast functions, lines 41-86).
  API reads `sage.size_history` (`api/handlers_v09.go:532`), which no active producer fills.
  `AlertHorizons` is copied into config but growth severity is hardcoded to 30/7/3 days
  (`forecaster/growth.go:235-240,287-297`).
- Trigger: enable forecaster and configure disk capacity/horizons, then run normal collection.
- Impact: basic disk-growth-rate findings work, but the advertised capacity forecast graph
  and configured time-to-full controls cannot receive live evidence through this path.
- Confidence: high, no-call-site proof.
- Fix/acceptance: connect one measured size producer per DB, use configured horizons and
  a minimum elapsed history span, populate metric/object identity (also currently absent
  on the `LinearForecast` constructed at growth.go:97-104), and verify API output after
  ordinary startup. Avoid an unrelated standalone test that calls ForecastGrowth directly.

##### C10 — P2: query-volume forecast treats lifetime counters as daily volume

- Evidence: `forecaster/datasource.go:92-106` aggregates daily `max(calls)` and sums it.
  `forecaster/rules.go:227-243` computes week-over-week growth on those values.
- Trigger: one query executes exactly 100 calls per day for 14 days since reset. The input
  series becomes 100,200,...,1400 instead of fourteen values of 100. First-week sum=2800,
  second-week sum=7700, so reported growth is 175% despite constant traffic.
- Impact: false critical workload-growth forecasts and poor sizing decisions. Resets,
  statement eviction and missing days further distort results.
- Confidence: high, algebraic reproduction directly from aggregation semantics.
- Fix/acceptance: derive reset-aware deltas at the native sample grain, then bucket to day;
  carry coverage/epoch metadata. Constant, doubling, reset, eviction and missing-day fixtures
  must report 0%, 100%, explicit epoch handling and insufficient evidence respectively.

##### C11 — P1: retiring or breaking a query hint does not remove the installed hint

- Evidence: `tuner/tuner.go:816-824` generates mutation of `hint_plan.hints` for executor
  installation. Revalidator selects active `sage.query_hints`, then only updates that metadata
  (`tuner/revalidate.go:55-68,291-310`). No DELETE of `hint_plan.hints` occurs on this path.
- Trigger: a previously installed hint ages out, references a dropped index, loses activity,
  or crosses the hardcoded observational-retirement condition.
- Impact: dashboard marks it retired/broken while PostgreSQL can continue applying it.
  The revalidator stops considering the row because it is no longer active. A later insert
  uses WHERE NOT EXISTS and will not replace the stale installed hint.
- Confidence: high, producer/consumer trace.
- Fix/acceptance: explicit proposed → applied → retiring → retired/failed states; route
  removal through the same policy executor; reconcile installed extension rows against
  durable metadata. Verify a retired installed hint is absent in the extension and actual
  EXPLAIN no longer applies it, with rollback/removal failures remaining retryable.

##### C12 — P2: tuner verification controls are inert despite documented effects

- Evidence: `VerifyAfterApply`, `RevalidationKeepRatio`, `RevalidationRollbackRatio`, and
  `RevalidationExplainTimeoutMs` are defined/copied, with no behavioral reads in tuner.
  `config/config.go:416,421-423` promises disabling the loop, cost comparison and EXPLAIN
  timeouts. Startup unconditionally calls `StartRevalidationLoop` for enabled tuner
  (`main.go:678-679`); the loop only checks interval (`tuner/revalidate.go:340-348`).
- Trigger: set verify_after_apply=false or change any cost ratio/EXPLAIN timeout.
- Impact: revalidation still runs; the promised hinted/unhinted comparison does not happen.
  The current heuristic may retire an effective hint simply because it makes the query fast.
- Confidence: high, full production reference search.
- Fix/acceptance: either implement documented cost/observational verification with controls
  or remove unsupported controls and correct documentation. Toggle tests must observe real
  scheduling and decision changes, not merely assert config deserialization.

##### C13 — P2: hint readiness does not establish effectiveness in application sessions

- Evidence: `tuner/detector.go:27-39,91-109` accepts successful `LOAD 'pg_hint_plan'` on
  one sidecar connection. `checkHintTable` tests only table readability (113-125).
  Production has no `enable_hint_table` check. BuildInsertSQL assumes query_id schema.
- Trigger: extension loadable only in pg_sage's session, table accessible, but application
  sessions do not load the module or enable the hint table; or an older hint-table schema.
- Impact: installation can be reported without changing user query plans, or fail at SQL
  execution after readiness was declared.
- Confidence: high for missing prerequisite checks; actual impact is deployment-dependent.
- Primary corroboration: upstream describes hint-table activation and query_id schema at
  https://github.com/ossc-db/pg_hint_plan/blob/master/docs/hint_table.md ; older schema uses
  norm_query_string at https://github.com/ossc-db/pg_hint_plan/blob/master/pg_hint_plan--1.3.0.sql .
- Fix/acceptance: distinguish module availability, table-schema compatibility, writer
  privilege, target-role settings and demonstrated application-session plan effect.
  Live tests must cover unloaded client sessions and disabled hint-table use.

##### C14 — P1: legacy rollback monitoring converts missing evidence into success

- Evidence: `executor/rollback.go:177-179,193-195` returns no regression on missing baseline
  or query error. Per-query monitoring skips missing/error windows then returns false
  (`rollback.go:319-337`). `MonitorAndRollback` marks success whenever that bool is false
  (`rollback.go:145-149`).
- Trigger: collector/query-store fails during the monitoring window, target query has no
  executions, database reads time out, or counters cannot produce a valid window.
- Impact: config/hint and other legacy non-index actions can be labelled successful with
  no evidence. Durable index verification already models insufficient data explicitly;
  the older path has incompatible semantics.
- Confidence: high.
- Fix/acceptance: tri-state or typed verdict (success/regression/unverifiable), minimum
  execution evidence, bounded extension, durable resumption. Induce missing baseline,
  timeout and zero executions; none may produce success or verified value credit.

##### C15 — P1: durable verification is completed before its required revert succeeds

- Evidence: `verify/engine.go:62-70` marks state completed and persists a revert verdict
  before `executor/verified_index_lifecycle.go:113-138` executes Retain/Revert.
  `verify/postgres.go:261-262` loads only pending/extended watches. Revert failure or policy
  withholding updates action outcome (`executor/index_verification_runtime.go:111-130`)
  but does not return the completed watch to a retryable state.
- Trigger: regression is observed, then DROP INDEX fails, emergency stop is active, or the
  process crashes between verdict persistence and revert execution.
- Impact: the harmful index remains while the durable verifier no longer resumes its
  cleanup. A retryable execution failure has become a terminal verification state.
- Confidence: high, durable ordering trace.
- Fix/acceptance: separate decision from effect completion, persist remediation intent,
  and reconcile idempotently after restart. Inject failure at every step between verdict,
  policy check, DDL and outcome persistence; recovery must either perform the authorized
  inverse or expose a durable actionable blocked state without falsely claiming reversion.

##### C16 — P2: index-rule DDL interpolates unquoted identifiers

- Evidence: unused/invalid/duplicate index SQL is built from raw names
  (`analyzer/rules_index.go:103-105,178-180,254-255,377-378`). Other rules already use
  `sanitize.QuoteQualifiedName`, e.g. `rules_wraparound_freeze.go:49`.
- Trigger: schema/index names contain uppercase letters, whitespace, punctuation or quotes.
- Impact: invalid SQL or a different folded identifier; validation may reject it, leaving
  the recommendation permanently unactionable rather than fixing the intended index.
- Confidence: high.
- Fix/acceptance: quote identifiers at construction and retain structured OIDs. Fixtures
  for `MixedCase`, `odd name`, embedded quote and two schemas must select the exact object.

##### C17 — P2: legacy rollback health is cross-database and not workload-normalized

- Evidence: `executor/executor.go:1179-1182,1199-1202` captures all-database cache-hit and
  unweighted statement means. `executor/rollback.go:183-189,214-217` repeats those aggregate
  reads without current-db/table constraints. New verifier's write observation explicitly
  ignores its table argument (`verify/postgres.go:86-112`) and divides database write time
  by snapshot count, not by writes or elapsed time.
- Trigger: another DB changes workload during a non-index action, or write rate/collection
  spacing changes during index verification while per-write latency is unchanged.
- Impact: unrelated workload can trigger a rollback or hide a local regression. Increasing
  total write time is not evidence that the new index increased per-write latency.
- Confidence: high for metric definition; outcome depends on workload.
- Fix/acceptance: bind observations to target DB and statement/table cohorts, compare
  deltas normalized by work, distinguish missing track_io_timing from zero, and retain
  workload mix/sample coverage. Cross-DB load must not change this DB's verdict.

##### C18 — P2: descending sequence exhaustion is never observed correctly

- Evidence: `collector/queries.go:189-193` calculates pct_used as last_value/max_value
  only when max_value is positive, returning zero otherwise. PostgreSQL's ordinary
  descending sequence has a negative maximum. `analyzer/rules_sequence.go:9-12` claims
  descending support but consumes only pct_used; at 25-27 it discards values below 75.
- Trigger: a descending sequence approaches its configured negative minimum.
- Impact: no warning or critical finding is emitted near sequence exhaustion. Custom
  nonzero minimums also make last/max an incorrect consumption fraction.
- Confidence: high, producer-to-consumer trace; not live-reproduced in this sub-review.
- Fix/acceptance: collect minimum, start, increment and cycle semantics; calculate consumed
  range in the direction of travel. Positive/negative increments and shifted ranges at
  74%, 75%, 89%, 90% must yield equivalent normalized severity.

#### Wiring gaps and risks requiring targeted confirmation

These are not promoted to confirmed unsafe execution without the remaining runtime proof.

1. **Stale snapshot reuse.** Collector retains its previous latest snapshot on failures and
   breaker skips (`collector.go:69-88`); analyzer only checks for nil (`analyzer.go:237-243`),
   so repeated old evidence refreshes findings/occurrences and feeds LLM work. Add a distinct
   stale-data state and evidence-age gate. Confirm every action adapter's fresh catalog
   precondition before claiming stale evidence can mutate the wrong object.
2. **HA unknown defaults to primary.** `ha/ha.go:46-52` returns `wasReplica` on failure,
   initially false, and reuses an old primary role on later errors. Standing policy may add
   further checks; verify that every mutation path fails closed on unknown role and that
   rollback checks role rather than passing literal false.
3. **Counters with PostgreSQL identity dimensions.** Query snapshots/keyed structures use
   queryid while PostgreSQL statistics may also differ by user/top-level status. Verify
   aggregation for the same query executed under two roles before asserting accurate
   windows. Root is reviewing query-store details.
4. **Plan-time division by zero.** `analyzer/rules_query.go:79` divides by MeanExecTime.
   A positive planning time and zero execution mean produces infinity; JSON cannot encode
   it and UpsertFindings returns early. Reproduce with an allowed real statistics fixture.
5. **Restart duration/freshness.** Analyzer and collector interval scheduling are independent
   rather than causally sequenced. Executor's comment that it runs "after each analyzer
   cycle" is not an event contract. Verify cancellation and long LLM cycles explicitly.

#### Dead or orphaned implementation inventory

- `forecaster.ForecastGrowth`, `RecordSizeHistory`, capacity/horizon path: orphaned production
  path C09. Keep only when wired and end-to-end demonstrated.
- `optimizer.AnalyzeDecay` / `ComputeDecayPct` (`optimizer/decay.go:14,24`): no production
  caller outside that file. It compares scan counters, not equal-window usage rates.
  Either build a reset-aware index-value retirement feature around it or remove it.
- `executor.NewRollbackMonitor` and `RollbackMonitor` (`executor/rollback.go:18-33`):
  constructor/type have no production consumer; actual monitoring uses free functions.
  Remove the unused facade when consolidating durable verification.
- `retention.cleanStaleFirstSeen` (`retention/cleanup.go:97-179`): comments explicitly say
  first_seen config keys are legacy orphaned data. Its index JSON extraction reads an
  object field from array-shaped snapshot data. Replace with an idempotent migration and
  remove recurring runtime queries instead of maintaining this dead feature lifecycle.
- Duplicate PG_CANCEL_BACKEND case in `executor/executor.go:688,692`: second branch is
  unreachable. Remove while consolidating typed action classification.
- Tuner verification/cost controls in C12: valid-looking configuration with no effect.
- Advisor degraded finding (`advisor/advisor.go:73-74,220`) is unreachable from normal
  startup when LLM is disabled because startup only constructs advisor with enabled LLM
  (`main.go:584-586`). Expose disabled/unavailable state independently of analyzer presence.

#### Every owned feature group: disposition and improvement contract

The matrix covers all core groups assigned to this reviewer. Other reviewers cover APIs,
UI, auth, AgentDB, notifications, schema intelligence, migration, RCA, policy/custodians,
rollout and provider integrations. "No additional defect confirmed" is a limited audit
disposition, not an assurance of correctness.

| Feature group | Current disposition / evidence | Improvement | Acceptance criterion |
|---|---|---|---|
| Collection and backpressure | Wired in standalone/fleet; catalog timeout wrapper and paging exist. C01 and stale-snapshot risk. | Typed metric units, per-category freshness/completeness, health independent of collection. | Partial category failure is visible; stale evidence cannot renew health or mutation eligibility. |
| Query telemetry and reset detection | pg_stat_statements snapshots and query-store recording wired (`collector.go:108-128`); cumulative semantics feed C10. | Persist stats epoch and user/top-level aggregation policy; reset-aware rates. | Reset, eviction and two-role workload produce correct nonnegative interval rates. |
| Unused/invalid indexes | Registered in AllRules; PK/unique and sole FK support protected. C16. | Persist observation coverage across restart, distinguish unused from unobserved, validate constraints and index validity immediately before mutation. | An index serving monthly/replica-only workload is not called unused after an observation gap. |
| Duplicate/subset indexes | Registered, parser considers btree definitions and subset risk; C16. | Portfolio assessment covering predicates, INCLUDE, collation/opclass, FK, partition and replica workloads. | Counterexample fixtures cannot recommend dropping the only required access path. |
| Missing FK index / seq-scan watchdog | Wired in rule and explicit analyzer call. | Rank by parent-delete/child-write evidence and actual scan cost; connect read-only recommendations to verifier-compatible query evidence. | Each recommendation shows attributable workload and explains why existing index prefixes do not suffice. |
| Slow queries / high planning / total-time burden | AllRules and historical regression paths wired; C03 common lifecycle. | Recent-window rates and p95 where available; separate latency versus total resource cost. | Old historical mean cannot hide a recent regression; high-frequency inexpensive query is ranked separately. |
| Query regression and plan-change narrative | Explicit analyzer calls, narrator optional. | Plan fingerprints and parameter-sensitive cohorts; confidence tied to statistics resets and deploy timing. | Same plan with different bind distribution is distinguished from a plan regression. |
| Sort-without-index and work_mem promotion | Explicit analyzer methods; tuner coordination present. | Account for sort concurrency, hash_mem_multiplier, parallel workers and role/session overrides. | Proposed per-role memory envelope stays within measured concurrent budget and is reversible. |
| Bloat / autovacuum / stale statistics | Snapshot rules plus advisor subfeatures wired. | Separate dead tuples from physical bloat; use vacuum progress, relation age and actual free-space evidence; avoid blanket VACUUM FULL. | Evidence names measured quantity; action resolves target without blocking DDL surprises. |
| Wraparound/freeze and XID monitoring | Registered freeze rule and explicit XID check; quoted SQL exists. | Deadline estimation from transaction burn, multi-XID age, blockers and partition coverage; share one custodian decision. | Deadline and blocker fixtures trigger before emergency threshold without duplicate competing jobs. |
| Cache/checkpoint/capacity rules | Wired, but cache C01 invalidates health detection. | Unified units; checkpoint rates divided by true elapsed time; saturation headroom. | Collector SQL fixtures exercise the same values consumed by rules and UI. |
| Sequences | Rule wired; descending observation broken by C18. | Account for cycle, increment, min/start, cache, identity column range and consumption rate. | Descending/shifted-range fixtures match ascending severity; cyclic behavior is qualified. |
| Replication lag and inactive slots | Registered; replication collection errors are nonfatal. | Per-slot safe WAL headroom, consumer ownership and provider constraints; no irreversible default slot drop. | Missing telemetry is unknown, not zero lag; retained-WAL alert includes last-consumer evidence. |
| Lock chains and runaway detection | Lock detector and supplemental runaway detector wired. | Backend identity includes start time, role, application and transaction impact; cancellation before termination. | PID reuse cannot target another query; critical owner workloads require explicit policy. |
| LLM index optimizer / HypoPG | Fully constructed, captures plans, validates SQL and scores confidence. C05/C06. | Stable candidate portfolio, explain confidence components, actual parameter cohorts, negative HypoPG evidence as a hard signal. | Repeated unchanged cycles spend no new generation tokens; each candidate independently traceable to measured gain. |
| JSON / spatial / vector index context | Optimizer has dedicated workload extraction files. No additional defect confirmed in sampled context. | Opclass/operator compatibility tests, write/storage cost and index-build budget; experimental candidates validated on representative cohorts. | Known unsupported expression/opclass combinations are rejected with clear reasons. |
| Vacuum configuration advisor | Wired through Advisor.Analyze; shared lifecycle C07. | Use actual autovacuum progress/budget, per-table overrides and measured worker contention before global changes. | Recommendation identifies current bottleneck and demonstrates a bounded effect with rollback state. |
| WAL/checkpoint configuration advisor | Wired, but prompt asks for requested-checkpoint share and WAL generation rate while context largely gives total checkpoint delta and one WAL position (`advisor/wal.go:17-23,67-101`). | Supply requested/timed deltas, WAL-byte delta/time, recovery objectives and storage headroom. | No specific max_wal_size recommendation when required measurements are absent; label unknowns. |
| Connections configuration advisor | Wired; deterministic minimum max_connections floor exists. | Actual reserved slots, recent peak/percentile open connections, pooling mode, burst churn and memory envelope. | Proposed limit remains above measured peak plus configured reserves/headroom across tenant scopes. |
| Memory configuration advisor | Wired; range grounding present. Prompt context includes cumulative spill counters, not daily rates or verified host RAM (`advisor/memory.go`). | Provide RAM/cgroup/provider memory, concurrency distribution, interval spill rate and pending restart. | Never infer RAM from existing GUCs or claim spills/day from lifetime counters. |
| Query rewrites and bloat remediation advisor | Wired as advisory findings, cloud transform applied. | Equivalence proof/workload replay for rewrites; lock/storage budgets and clone rehearsal for bloat changes. | Rewrites stay advisory without semantic equivalence; destructive storage rewrites never masquerade as online maintenance. |
| Per-query tuner / hints | Findings and SQL generated; C11-C13 show lifecycle and activation gaps. | One durable hint state machine with applied proof, retirement executor and counterfactual comparison. | UI active means demonstrated installed effect; retirement removes effect and is restart-safe. |
| Action executor / trust | Typed contract, repeated authorization, top-level DDL and queue modes implemented. C04/C07 are feeder defects. | Durable candidate discovery; immutable approvals; freshness and object-version preconditions; machine-readable blocked reasons. | Policy transition re-evaluates old fresh candidates; stale approval cannot run new SQL. |
| Index verify-and-revert | Durable watch and bounded sample extension exist; C15/C17. | Separate measurement verdict from action completion; idempotent recovery; normalized workload cohorts. | Restart/failure at every boundary leaves consistent desired/actual state and no lost revert. |
| Legacy rollback / config / hint verification | Real monitor executes, but C14/C17 make success ambiguous. | Migrate all actions onto typed durable verification criteria appropriate to each action class. | Missing data never means success; restart resumes verification; rollback scope matches target. |
| HA safety | Standalone/fleet role monitors wired; unknown-role behavior needs gate proof. | Tri-state role, freshness deadline, topology generation and promotion stabilization. | First probe failure and post-promotion flaps authorize zero autonomous mutations until fresh primary evidence. |
| Retention / internal footprint | Standalone connected; fleet C08; incomplete modern-table coverage. | Central retention catalog by data class, batch/time budget, partitioning, size alarms and legal/audit holds where configured. | Every growing table has an owner/window; tests verify bounded storage without deleting needed rollback evidence. |
| Capacity forecasting | Legacy functions run; C09/C10. | Real interval rates, minimum time span, seasonality, uncertainty bands, missingness and change-point reset. | Constant load gives no growth alert; predictions include observation coverage and calibrated backtest error. |
| Vector lab | Explicit CLI wired (`main.go` → `vector_lab.go` → Command); bounded read-only repeatable-read experiment, exact ground truth, plan proof, recall, underfill and tie rejection. No additional correctness defect confirmed. | Persist/import evidence, workload slices, independent holdout, cardinality sensitivity and warm/cold labels; keep adaptive production settings separate from advisory experiment. | Repeated manifest yields auditable report with source versions; no qualifying candidate cannot be mistaken for success; validate on real pgvector >=0.8. |

#### Recommended repair sequence and verification design

1. Repair shared evidence contracts first: cache units, identity, suppression, complete
   proposal upsert and successfully-evaluated category resolution. These fixes reduce false
   behavior across most features at once.
2. Introduce a durable recommendation state machine separate from findings/notifications.
   Carry evidence freshness, action identity, forward/inverse SQL, policy version, expected
   effect, retry budget, conflict group and lifecycle state. This solves advisor liveness
   and gives future AI SRE work an appropriate foundation.
3. Make every action completion evidence-backed and recoverable. A verdict is not a
   successful side effect. Include missing data, cancellation, process kill and failed
   rollback in the integration matrix.
4. Repair hint lifecycle and readiness, then complete forecast and retention wiring.
5. Add composed contract tests: collector SQL → serialized snapshot → analysis → durable
   proposal → policy → mutation → measurement → rollback/recovery → UI outcome. Existing
   tests that manually construct intermediate structs will miss C01 and many lifecycle bugs.

Suggested release gates (not executed by this reviewer):

- CHECK-CORE-01: suppression remains effective across three cycles and a restart.
- CHECK-CORE-02: empty successful category evaluation resolves its final finding.
- CHECK-CORE-03: cache hit 80% triggers the correct deterministic and RCA signal.
- CHECK-CORE-04: new forward/inverse proposal pair stays atomic through approval.
- CHECK-CORE-05: two optimizer candidates for one table remain independent.
- CHECK-CORE-06: policy/window transition acts on an existing fresh advisor proposal.
- CHECK-CORE-07: fleet retention removes only expired records in the correct DB.
- CHECK-CORE-08: ordinary startup produces live storage forecast history.
- CHECK-CORE-09: constant query traffic reports zero week-over-week growth.
- CHECK-CORE-10: retiring a hint removes actual extension effect, not just its UI row.
- CHECK-CORE-11: disabled hint verification schedules no revalidation worker.
- CHECK-CORE-12: missing post-action evidence never produces verified success.
- CHECK-CORE-13: failed/crashed revert is resumed or durably blocked after restart.
- CHECK-CORE-14: unrelated DB workload cannot change local rollback verdict.
- CHECK-CORE-15: quoted index names always address the intended catalog object.
- CHECK-CORE-16: stale/unknown role and evidence states cannot authorize new mutations.

#### Review limitations

Three isolated probes reproduced C01, C02 and C04; see the test report below.
Other confidence labels derive from source traces and algebraic examples.
Live PostgreSQL role changes, pg_hint_plan application-session behavior, pgvector and
managed-provider controls require the parent's isolated integration run or a separately
authorized commissioning environment. The source-based findings remain actionable even
if existing unit tests pass, because most cross-component contracts here are not represented
by a single locally constructed unit fixture.

#### Test Results

**Command:** `go test -cover -count=1 -run '^TestAudit' -v ./internal/analyzer ./internal/collector`

**Total:** 3 top-level tests passed, 3 failed, 0 skipped. The passing analyzer test also had
2 passing subtests. The three newly written probes all failed on their specific assertions.

**Coverage:** analyzer 2.6%; collector 10.7%. These are deliberately narrow probe-run
numbers, not full-suite coverage; use the parent's full run for package coverage assessment.

##### Skipped Tests (must be zero or justified)

None. Preserved output was searched for SKIP, TODO, and PENDING; none occurred.

##### Failures

- `internal/analyzer: TestAuditSuppressedFindingStaysSuppressed` — FAIL: second detection
  resurrected an open finding alongside the suppressed row.
- `internal/analyzer: TestAuditUpdatedForwardSQLKeepsMatchingInverse` — FAIL: new forward
  CREATE index_b retained old inverse DROP index_a.
- `internal/collector: TestAuditCollectorCacheRatioUsesFractionContract` — FAIL: actual SQL
  expression returned 80 instead of the fraction 0.8 required by analyzer thresholds.

##### Coverage Gaps (packages below threshold)

- Analyzer 2.6%, below 70%: selected run omitted most rules, full analyzer cycles, LLM
  coordination and execution transitions. This is not the package release run.
- Collector 10.7%, below 70%: selected run omitted most collection categories, persistence,
  failures and resets. This is not the package release run.

##### Bugs Found This Session

1. C01 — collector/detector cache-hit units disagree (reproduced).
2. C02 — suppression is not durable across detection (reproduced).
3. C04 — proposal refresh leaves stale inverse SQL (reproduced).
4. C03 and C05-C18 — additional source-traced defects, individual confidence above.

##### Manual Checks Remaining

- CHECK-CORE-01 through CHECK-CORE-16 remain the product-level regression gates; only
  the local assertions for C01/C02/C04 were executed here.
- MANUAL: application-session hint effectiveness and provider/HA behavior require a
  supported isolated commissioning environment.

Probes were written and staged before running in `core-probe-repo`, a separate checkout
of the same commit. Only two new test files were added there; product source was unchanged.
Evidence: `reports/evidence/core-regression-probes.log` and the reusable staged patch
`reports/evidence/core-regression-probes.patch`. Sources are at
`core-probe-repo/sidecar/internal/analyzer/audit_regression_test.go` and
`core-probe-repo/sidecar/internal/collector/audit_regression_test.go`.

Post-test audit: assertions compare actual database values and would not pass for empty
implementations. The collector probe substitutes only catalog input while executing the
real SQL expression; it does not prove the complete collection loop. Missing coverage is
explicitly tracked, not silently skipped. Failures remain intact because this is a review
and specification task; no product-code repair was made by this reviewer.


## Part 3: API UI security and AgentDB audit

### pg_sage feature surface and completion audit

Audit date: 2026-09-26. Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Repository: `audit-repo`. All references below are relative to its root.
Scope: API/UI/auth/config/fleet, Cases/value, alerts/metrics/MCP and AgentDB.

This is a source-trace audit. Findings labeled confirmed mean the relevant
production call chain and data contract establish the defect; they do not mean a
live exploit or cloud mutation was executed. No source fixes, live provider
operations, or external notifications were performed. The coordinating audit
owns test execution and its test-results report. `CLAUDE.md` was absent from this
snapshot. The independent Gemini review could not run; see `gemini-review.md`.

#### Overall assessment

The repository has substantial working machinery, including authenticated REST
routes, policy-gated actions, live provider runners, cleanup claims and durable
verification tables. It also has a recurring completion problem: a carefully
tested package can exist without its production caller, or the UI can present a
stronger claim than the persisted evidence supports. The first release priority
should be identity/authorization, truthful verification, and complete runtime
wiring before adding autonomous SRE scope.

The highest-risk defects are the HTTP MCP role bypass, unverified OIDC email
linking, and value attribution/read failures. The most visible unfinished
surfaces are provider readiness, Terraform template semantics, adaptive
monitoring, incident value credit, and the replacement of actionable legacy
pages with a read-only Cases view.

Priorities: P1 before production expansion; P2 functional defect; P3 robustness.
This audit does not certify unlisted code as bug-free.

#### Findings

##### SURF-01 — P1: HTTP MCP bypasses the viewer role boundary

**Confirmed, high confidence.** [sidecar/internal/api/router.go:158](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/api/router.go#L158) mounts
`POST /api/v1/mcp` directly, without `RequireRole`. Session auth still applies,
but every logged-in role can reach the tools. In contrast,
[sidecar/internal/api/policy_handlers.go:49](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/api/policy_handlers.go#L49) explicitly rejects viewers when
creating a policy proposal through REST.

[sidecar/internal/mcp/production_backend.go:75](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/mcp/production_backend.go#L75) forwards proposal writes;
[sidecar/internal/mcp/postgres_access.go:200](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/mcp/postgres_access.go#L200) persists them with a generic actor.
The mutation chain in `production_backend.go:117`, `intent_adapters.go:13`, and
`production_intent_executor.go:68` does not consume the authenticated user's
role. A permissive standing policy can therefore authorize a viewer's real
table-contract or slot-consumer INSERT/UPDATE via `postgres_access.go:18` and
`:46`. Standing action policy is not a substitute for caller authorization.

**Trigger/reproduction:** enable HTTP MCP, log in as viewer, call
`propose_policy_change` with a valid policy delta; compare with REST's 403.
With executor enabled, auto mode, sufficient trust and a policy permitting the
class, call `register_consumer` or `declare_table_contract` and inspect the
target table. No live reproduction was performed.

**Fix/acceptance:** bind a trusted principal to MCP requests; enforce per-tool
read/write roles before planning or persistence; preserve actor ID in evidence.
Viewer mutation calls must be denied before any DB write under every policy
profile. Test the real mounted router, not just a mocked backend.

##### SURF-02 — P1: OIDC email linking accepts an unverified identity

**Confirmed, high confidence; exploitability depends on the configured issuer.**
[sidecar/internal/auth/oauth.go:329](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/auth/oauth.go#L329) decodes only `email`, ignores
`email_verified`, and returns any nonempty email. The exchange does not retain
issuer/subject as the durable user key. `sidecar/internal/auth/auth.go:361-375`
looks up the existing user solely by email and returns that user's role,
including admin, without an explicit account-linking step.

**Trigger/reproduction:** configure an OIDC test issuer whose userinfo returns
an existing admin's email with `email_verified:false`; complete its otherwise
valid code exchange. The local user lookup grants the existing admin identity.
An issuer that always verifies emails may reduce exploitability, but the
application does not enforce its own requirement.

**Fix/acceptance:** use issuer+subject identity, require verified email for any
email-based linkage, and require deliberate authorized linking of existing
password accounts. Reject missing/false verification; test subject change,
issuer change, duplicate emails, unverified email and concurrent first login.

##### SURF-03 — P2: Value reporting loses normal action attribution and can fail

**Confirmed data-contract mismatch, high confidence; integration validation
belongs in the coordinating test report.**
[sidecar/internal/executor/executor.go:1243](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/executor/executor.go#L1243) and
[sidecar/internal/executor/manual.go:325](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/executor/manual.go#L325) insert action logs without
`database_id`. [sidecar/internal/schema/ddl_agent_value.go:42](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/schema/ddl_agent_value.go#L42) makes this column
nullable; no production repair/trigger was found. Crediting can subsequently
populate toil minutes through `executor/rollback.go:380` and
`executor/index_verification_runtime.go:90`.

`sidecar/internal/value/postgres.go:139-163` LEFT JOINs `sage.databases`, selects
`d.name` without COALESCE and scans it into a nonnullable string. A credited
row with NULL database attribution therefore fails the entire all-databases
Value response; filtering by database instead silently omits it.

There is a second topology mismatch: `sidecar/internal/api/router.go:183-184`
constructs Value from the auth/meta pool only, while normal fleet executors write
to each monitored database's pool. Unlike Cases, Value does not aggregate
selected fleet pools. Depending on topology it can show empty/incomplete value
even when target databases contain verified credit.

**Trigger/reproduction:** execute a normal action through the actual executor,
complete verification, inspect its database_id, then read `/api/v1/value` with
and without a database filter. Repeat with separate meta DB and two targets.

**Fix/acceptance:** pick one explicit ledger storage topology, stamp canonical
database identity at write time, migrate old rows, handle unknown identity
without failing, and use it consistently in API/MCP/metrics. A two-database
fixture must yield correct disjoint per-DB and summed fleet totals.

##### SURF-04 — P2: Provider readiness never receives its runtime dependencies

**Confirmed, high confidence.**
[sidecar/internal/api/agent_db_provider_handlers.go:12](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/api/agent_db_provider_handlers.go#L12) calls
`ProviderReadinessList(r.Context())` without options.
[sidecar/internal/agentdb/provider_readiness.go:34](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/agentdb/provider_readiness.go#L34) consequently always chooses
the no-runtime branch, marking every cloud provider unavailable with
`runtime_dependencies_missing`. The subrouter already has a runtime registry
and authority, but passes neither to this handler.

[sidecar/web/src/pages/AgentDBsPage.jsx:139](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/web/src/pages/AgentDBsPage.jsx#L139) consumes this endpoint, and
`pages/agentdb/AgentDBProvisioningPanels.jsx:284-285` displays missing/readiness
from the resulting `found` flag.

**Reproduction:** configure any live runner plus valid effective policy and GET
`/api/v1/agent-dbs/providers`; it still reports missing runtime dependencies.
**Acceptance:** endpoint must derive readiness from the same registry,
credential checks and policy snapshot used by execution, with current reasons,
version/hash and timestamp. Test enabled/disabled/expired-credential transitions.

##### SURF-05 — P2: Terraform template provisioning does not use template content

**Confirmed, high confidence.**
`sidecar/internal/agentdb/provision_links.go:97-146` loads the approved template
but uses only its status and ID. It constructs a generic provider profile from
the new request and calls `BuildProvisionPlan`; `template.Files` and
`template.Manifest` do not affect the plan. Uploaded infrastructure settings
such as networking, retention, extensions or instance class are therefore not
the settings that this flow provisions.

**Reproduction:** approve two templates with meaningfully different resource
settings and submit identical provision parameters to each; the effective
provider plan is identical apart from template provenance metadata.

**Acceptance:** either implement a constrained parsed-template-to-plan contract
and show an exact reviewed semantic diff, or explicitly name this feature
template storage/review and remove provisioning claims. Bind approval to an
immutable content hash and require supported attributes to survive into the
execution plan. Arbitrary Terraform execution is not recommended as a shortcut.

##### SURF-06 — P2: Dry-run backup checks claim verified backup evidence

**Confirmed, high confidence.**
`sidecar/internal/agentdb/backup_assurance.go:17-39` executes the command-runner
path then records `Status: "verified"` unconditionally after a zero exit code.
The default registry's runner is a dry run (`provider_runner.go:101`), and live
runners without the legacy command interface also fall back to dry run.
`provider_runner.go:147` even returns a verified status for its dry-run backup
method. `BackupAssurancePanel.jsx:12-14` renders verified green without exposing
execution mode.

**Reproduction:** with no cloud runner configured, register a planned cloud
deployment and click Check backups. No provider backup has been observed, but a
verified record is stored. This alone is not the restore-required deletion
bypass: teardown separately requires `restore_verified`; keep that distinction.

**Acceptance:** dry runs produce only `planned`/`not_checked`; verified requires
provider or artifact evidence including ID, observed time, integrity status and
provenance. The UI must show plan-only, provider-observed and restore-tested as
different states. Backup retention settings alone do not prove a usable backup.

##### SURF-07 — P2: AgentDB monitoring is partial and not reconciled on removal

**Confirmed, high confidence.**
`sidecar/cmd/pg_sage_sidecar/agentdb_fleet.go:87-106` only adds eligible
deployments and skips any already registered. It never removes inactive,
archived, expired-secret or deleted deployments, nor replaces a changed
connection/credential. `:110-155` attaches only a Collector; analyzer, executor,
per-target safety/readiness initialization and full pipeline are absent.

The secret resolver in `agentdb_secret_ref.go:25` supports only `env:` URIs.
For example AWS runners return AWS Secrets Manager ARNs
(`agentdb/aws_rds_runner.go:376`), so successful provider creation cannot
automatically become a monitored database through that path. Cloud SQL and
Lakebase also have endpoint-to-usable-credentials gaps that need explicit
commissioning rather than an assumption of readiness.

**Reproduction:** register/monitor one inline or env-backed deployment; archive
it or expire/change its secret; run the reconciler and inspect fleet instances
and active pools. Separately provision an ARN-backed RDS deployment and inspect
its absent fleet instance. No cloud operations were performed here.

**Acceptance:** desired-state reconciliation must add/update/remove instances
and cancel/join workers, with credential version/expiry handling. Clearly label
collector-only mode or install the full desired pipeline with read-only defaults.
Resolve provider secrets just in time without persisting credentials. Demonstrate
create -> connect -> collect -> analyze -> candidate -> approved action -> verify
-> expire -> stop -> cleanup for each supported provider.

##### SURF-08 — P2: Adaptive monitoring exists only as uncalled storage primitives

**Confirmed, high confidence; documented limitation.**
[sidecar/internal/agentdb/monitoring_schedule.go:17](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/agentdb/monitoring_schedule.go#L17) and
`monitoring_claims.go:46` have no production callers. No runtime worker completes
these claims. `docs/agent-db-deployments.md:252-258` admits this explicitly, so it
is unfinished scope rather than a hidden claim in that guide.

The scheduling query also always orders the first 10,000 deployment IDs and
selects at most 100 targets by count/key, without using due time or rotating a
cursor. If wired unchanged, the same targets can monopolize passes and others
starve. `state.next_due_at` is written but not used to select eligible targets.

**Acceptance:** wire a bounded scheduler and probe worker, claim fencing,
completion/retry/dead-letter states, last-success freshness and shutdown. Test
101+ targets and 10,001+ deployments across repeated passes to prove eventual
service, due-time behavior, provider/tenant budgets and stale claim recovery.

##### SURF-09 — P2: Query-hint IDs are corrupted before generated cleanup SQL

**Confirmed, high confidence.**
[sidecar/internal/api/cases_handlers.go:643](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/api/cases_handlers.go#L643) converts an int64 `queryid` through
float64. PostgreSQL query IDs commonly exceed 2^53, where that conversion loses
integer precision. The exact helper `int64Value` already exists nearby, but this
projection does not use it. `cases/query_hint_projector.go:29` propagates the
rounded identifier into case identity, and `cases/query_actions.go:41-47`
propagates it into cleanup/verification scripts.

**Reproduction:** project a broken hint with queryid `9007199254740993`; the
generated identity/SQL uses `9007199254740992`, targeting no row or the wrong row.
**Acceptance:** preserve int64 end-to-end and serialize identifiers as strings
at JavaScript boundaries. Test positive/negative 64-bit limits and neighbors
around 2^53 through DB -> API -> browser -> action, not only isolated helpers.

##### SURF-10 — P2: The Cases replacement removed existing operator workflows

**Confirmed, high confidence.** `sidecar/web/src/App.jsx:152-196` routes Findings,
advanced Findings, Incidents, forecasts, schema health and query hints to
`CasesPage`. That page (`CasesPage.jsx:56`) only fetches, filters and displays
evidence/scripts; it has no suppression, unsuppression, incident resolution,
historical status view, or candidate-to-approval command.

Those actual actions survive in unimported `Findings.jsx:346-459` and
`IncidentsPage.jsx:170`, along with unreachable old `ForecastsPage`,
`QueryHintsPage`, `SchemaHealthPage` and `DatabaseSettingsPage` components.
The separate Actions page remains reachable and can handle already-queued
actions; the gap is specifically Cases and the replaced lifecycle workflows.

**Reproduction:** browse Findings explorer or Incidents as operator and try to
suppress/unsuppress or resolve an incident; no control exists despite active API
routes and legacy component code.

**Acceptance:** restore the needed workflows in Cases with source IDs and DB
context, provide resolved/suppressed history and case-to-action navigation, then
delete superseded components. Route tests must exercise actual operator outcomes
and viewer denial, not just page headings or fixture rendering.

##### SURF-11 — P2: Cases advertise ranking and freshness they do not preserve

**Confirmed, high confidence.** `CasesPage.jsx:81` says ranked work items and
`:152-153` displays impact/urgency. `cases/case.go:52` has no score fields;
`api/cases_handlers.go:90-140` concatenates findings, incidents and hints per
database with no final global ranking. A critical incident can appear after
hundreds of informational findings. SourceFinding also omits original timestamps,
so `cases/case.go:155-158` assigns the projection time to old finding cases.

Candidate expiry is similarly recalculated from `time.Now()` on each projection
(`cases/query_actions.go:10`, `:27`, `:57`, `:71`), making the displayed freshness
window slide with every read. This does not prove executor stale-evidence bypass,
but it makes the review artifact itself misleading.

**Acceptance:** preserve observation time and immutable candidate generation /
evidence expiry, define reproducible global ordering, and either populate scores
with explained formulas or remove the labels. Test mixed-source, multi-DB
ranking and stale candidates across repeated reads.

##### SURF-12 — P2: Execution success is mislabeled as outcome verification

**Confirmed, high confidence.**
`sidecar/internal/api/cases_handlers.go:486-502` returns `verified` for any action
with `outcome == "success"`, even with no `measured_at` or durable verification
record. The Value service correctly has a stronger contract: completed success
verification is required (`value/service.go:51-54`). These surfaces disagree.

**Reproduction:** project an action-log row `{outcome:"success", measured_at:nil}`;
Cases says verified. **Acceptance:** join/use the authoritative verification
record and show applied/pending verification/inconclusive/verified/reverted
separately. A successful SQL return must never by itself assert benefit or safety.

##### SURF-13 — P2: Shadow savings ignore the policy decision for candidates

**Confirmed, high confidence.** [sidecar/internal/cases/shadow.go:52](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/cases/shadow.go#L52) increments
WouldAutoResolve for `safe` and an empty blocked reason even when its attached
policy decision is `queue_for_approval` or another nonexecute verdict. The proof
row can simultaneously display approval required while the totals claim auto
resolution. There is also no candidate expiry check in this counting path.

**Reproduction:** build a safe candidate with no blocked reason but
PolicyDecision=`queue_for_approval`; inspect inconsistent aggregate and proof.
**Acceptance:** explicitly evaluate a named hypothetical auto-safe policy against
current capabilities/freshness or use the provided authoritative decision; count
only eligible unique work, and show uncertainty/model version for toil estimates.

##### SURF-14 — P2: Value evidence links are dead and incident credit has no producer

**Confirmed, high confidence.** [sidecar/web/src/pages/ValuePage.jsx:207](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/web/src/pages/ValuePage.jsx#L207) links
to `#/ledger?evidence_id=...`; App's exact switch in `App.jsx:146-216` has no
ledger route or query parsing, so it opens Page not found.

The incident-avoided value card/metric is backed by `sage.incident_avoided`, but
[sidecar/internal/value/postgres.go:110](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/value/postgres.go#L110)'s `RecordIncident` has no production
caller, and no other production INSERT into that table was found. The UI works
with hand-seeded rows, while actual custodians do not produce that output.

**Acceptance:** provide a routable evidence/decision/action/verification detail
view, integrate qualifying incident credit from verified outcomes with unique
evidence IDs, and label counterfactual prevention separately from observed
recovery. End-to-end tests must start from a real supported detection/action
path and verify both the value row and its clickable evidence.

##### SURF-15 — P2: Alert delivery failure can permanently consume the event

**Confirmed, high confidence.**
`sidecar/internal/alerting/alerting.go:106-135` queries findings since lastCheck,
then advances lastCheck to the time after dispatch regardless of delivery
failure. Not recording throttle on all-channel failure (`:185-191`) does not
retry the row because the next query excludes it unless its last_seen changes.
Findings arriving between query snapshot and post-dispatch time can also fall
behind the advanced watermark without being read.

The newer notification dispatcher (`notify/dispatcher.go:47-55`) logs per-rule
errors and returns nil, and has no durable delivery retry queue. Both paths need
one clearly defined reliability contract.

**Reproduction:** create one finding, make sender fail once, recover sender
without updating the finding, run two evaluation cycles; the row is not retried.
Add a finding while a slow send is blocked to reproduce the watermark window.

**Acceptance:** durable outbox and per-channel delivery state with bounded
retry/backoff, idempotency and dead-letter visibility. Commit a read watermark
captured before the query and only advance safely; queue delivery independently.
Test partial channel failure, restart, quiet hours, slow sender and concurrent
arrivals. Never claim a page was delivered from enqueue success alone.

##### SURF-16 — P2: Login limiter capacity can be bypassed by its allow path

**Confirmed, high confidence.** [sidecar/internal/api/auth_handlers.go:117](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/internal/api/auth_handlers.go#L117)
stores an empty slice for a new email during `allow`. On failure `record` sees
that key already exists (`:129`) and returns before its 10,000-entry eviction
logic. Distinct failed emails therefore grow the map past the claimed bound
between cleanup passes. The non-atomic allow -> authenticate -> record sequence
also permits a simultaneous burst above the per-email attempt limit.

**Reproduction:** call allow/record for 10,001 different emails; map size exceeds
loginMaxEntries. Submit simultaneous failed requests for one account to test
reservation semantics. **Acceptance:** atomically reserve bounded attempts at
entry, normalize identity, enforce capacity in all insertion paths, and test the
actual request sequence plus concurrency. Keep global/IP controls as well.

##### SURF-17 — P2: Approval audit identities are accepted from request JSON

**Confirmed, high confidence; this is attribution spoofing, not a live authority
bypass.** `api/agent_db_terraform_template_handlers.go:42,65` and
`api/agent_db_blueprint_handlers.go:39,63` take `created_by` and `approved_by`
from user-supplied JSON. Any authorized operator can record another person's
name as template/blueprint approver. The separate live execution authority has
stronger server-owned checks and should not be conflated with this defect.

**Acceptance:** derive audit actor from authenticated user context, keep any
caller annotation in a separate field, and bind immutable revision/hash to the
approval. Two operators submitting forged actor strings must still record their
real identities. Race an update against approval to verify atomic validation.

##### SURF-18 — P3: Prometheus declares reversible savings as a counter

**Confirmed, high confidence.**
[sidecar/cmd/pg_sage_sidecar/value_metrics.go:17](https://github.com/jasonmassie01/pg_sage/blob/b396595059d2b1b312a22231fdbfd1d2cfef9530/sidecar/cmd/pg_sage_sidecar/value_metrics.go#L17) declares
`pg_sage_toil_minutes_saved_total` a counter, but derives it by summing current
successful action credits. Rollback zeroes credit (`value/postgres.go:98`), so
this series can decrease. Prometheus rates interpret that as a reset and can
report false positive savings. Query failures also silently emit no samples.

**Acceptance:** expose current net value as a gauge, and separate monotonic
earned/retracted counters if needed. Add scrape-error/freshness metrics and
prove a rollback does not create a bogus rate spike.

##### SURF-19 — P3: Incident resolution reason is discarded

**Confirmed, high confidence.** `api/handlers_v09.go:436` assigns reason to `_`
after updating resolved_at. Its comment claims it is logged, but this function
does not persist or log it. **Acceptance:** record resolver, reason, source
evidence and time in an immutable event; surface it in resolved case history.

##### SURF-20 — P3: Cloud SQL provisioning uses a permanently captured access token

**Confirmed lifecycle limitation, high confidence.**
`agentdb/runtime_registry.go:45-58` captures `PG_SAGE_GCP_ACCESS_TOKEN` into
`staticToken`; registries are created at startup/router construction and never
refresh that token. A long-lived sidecar loses Cloud SQL status/cleanup access
when the configured short-lived OAuth token expires.

**Acceptance:** use a supported refreshable ADC/workload identity token source,
report credential expiry/readiness, and demonstrate create/status/cleanup across
token rotation without restarting. Keep preflight honest: the CloudSQL runner's
Preflight validates configuration but does not make a credential-validation call.

#### Existing feature improvements and acceptance criteria

The matrix covers the groups owned by this audit. The companion execution audit
covers collector/analyzer/optimizer/advisor internals, tuning, forecasting,
executor, custodians, clones, migration runtime and other engine logic.

| Feature group | Current path and completion boundary | Highest-value improvement | Acceptance criteria |
|---|---|---|---|
| Auth/password/users | Login -> session cookie -> per-request user lookup; admin protection exists; limiter has SURF-16 | Strong bounded login admission, session/device management, explicit role matrix | Concurrent login flood bounded; revoked/deleted/demoted user loses capability immediately; last-admin invariant survives races |
| OAuth/OIDC | Discovery -> code exchange -> email -> local account; SURF-02 | Issuer+subject identity and deliberate account linking | False/missing verification denied; changed issuer/subject cannot inherit admin; human-readable linking history |
| REST/API | Session middleware, roles on most mutation routes, JSON/body/deadline controls | Generate API capability inventory from route table and test authorization per endpoint | Every route has owner, required role, request schema, error contract and nonmock request test; unsupported features report explicit reason |
| Dashboard navigation | Embedded React SPA; source pages and compiled assets; multiple dead legacy pages | One complete Cases experience with task-oriented routes and route health tests | Every visible link resolves; every operator workflow has an outcome; legacy components deleted; asset build matches source |
| Cases queue | Findings/incidents/hints -> projections -> read-only cards; SURF-09/10/11/12 | Ranked work, preserved source identity/time, evidence drilldown, act/suppress/resolve controls | Global mixed-source ranking; exact IDs; no freshening on GET; full case-to-action and status history |
| DDL/PR/script artifacts | Cases shows preflight, script, rollback, verification SQL and PR metadata | Bind artifacts to source schema revision and evidence expiry; distinguish generated artifact from executed migration | Stale artifact visibly invalidated; scripts explain irreversible forward-fix; generated PR text never implies applied DDL |
| Actions review/audit | Pending/history/action routes, approvals, rejection, rollback; reachable Actions UI | Deep links from Cases with immutable target, actor, policy and verification timeline | Same ID on different DB cannot cross-route; race/retry behavior is visible; success/verified/reverted remain distinct |
| Shadow mode | Cases-derived policy/toil proof; embedded in Settings; SURF-13 | Explicit hypothetical policy version and measured adoption readiness | Proof rows reconcile exactly with aggregates; blocked/expired/manual candidates excluded from auto totals |
| Value/ROI | Root page + value repository + Prometheus; SURF-03/14/18 | Complete target attribution and evidence lineage, editable versioned toil model | Real verified action creates appropriate value in standalone/meta/fleet; rollback retracts it; every claim has a working evidence link |
| Config and hot reload | Desired/active snapshots, typed lifecycles, persistent overrides, audit and restart flow | Show owner/applicability/restart behavior per setting, surface partial activation | Readback shows desired vs active and effective per DB; invalid batch is atomic; restart-only changes never look active early |
| Fleet/database management | Managed database CRUD/import and per-DB runtimes; AgentDB separate path | One reconciliation architecture for normal and agent-created databases, isolation/freshness first | Remove/readd/rotate credentials while polling without stale pools; per-DB mode/budget/config ownership maintained |
| Provider capability observability | Provider facts and action readiness; live APIs and target SQL have distinct limits | Normalize capability with source, observation time and degraded reasons | Provider outage = unknown/stale rather than supported/healthy; action readiness follows verified capability |
| AgentDB inventory/identity/lease | Register and token ping -> leases -> archive/delete; durable metadata | Clear tenant/agent ownership and idempotent lifecycle state machine | Repeated pings do not evade policy; revoked credential stops probes; lease expiry, backup gates and cancellation survive restart |
| Local schema/database provisioning | Actual SQL provisioning separate from cloud planned instances | Provide tested handoff credentials, resource quotas, isolation proof and workload bootstrap | New role can access only intended objects; reconnect and cleanup verified; failed partial creation has a recoverable record |
| Cloud runners (RDS/CloudSQL/Lakebase/Neon/Supabase) | Live registry/gates -> create/status/destroy; no live commissioning in this audit | Credential refresh, scope proof, ambiguous-operation recovery and full usable-connection handoff | Disposable canaries prove create -> usable DB -> backup evidence -> cleanup per provider; duplicate requests create one resource |
| Provider readiness | Rich evaluator exists, endpoint omits dependencies; SURF-04 | Use execution's exact dependencies and policy snapshot | Readiness changes with actual runtime/policy/credential state and explains every disabled state |
| AgentDB blueprints | LLM strict JSON -> normalized spec -> policy findings -> approve -> generic plan | Constrain generated intent to supported semantics and immutable reviewed diff | Networking/backup/class/region values survive generation -> review -> provision; uncertain requirements stay unresolved |
| Terraform intake | Zip/inline -> regex policy -> stored template -> generic provision; SURF-05 | Typed supported-template parser and semantic plan binding, or explicit review-only scope | Two differing templates yield corresponding effective settings; unsupported blocks rejected; aggregate zip expansion capped |
| Backup/restore assurance | Record backup, check provider/dry run, plan drill, manually mark restored; SURF-06 | Automated disposable restore with integrity/business probes and signed evidence | Simulation never marks verified; restore-tested state names real artifact/target/checks/RPO/RTO; stale proof cannot authorize deletion |
| Cost/budget | Manual cost samples plus hardcoded approximate provider estimates | Region/HA/storage/backup/network-aware estimates with source/version and observed billing reconciliation | Unknown components visible and conservative; estimate cannot become high confidence without evidence; enforce user budget over lifecycle |
| Cleanup/lifecycle | Scheduled archive/live status/teardown with restore claim checks | Reconcile DB, provider resource and monitoring workers as one lifecycle | Crash after provider accepts create/destroy resumes safely; archived/deleted records do not leave active collectors or orphaned billable resources |
| Adaptive monitoring | Scheduler/claim tables and functions exist; no runtime caller; SURF-08 | Budgeted adaptive worker with freshness and fairness | >100 physical targets eventually observed; due times respected; claims expire/recover; worker output feeds Cases |
| Agent query recommendations/feedback | Record recommendations and agent feedback in deployment scope | Connect recommendations to evidence and verified application effects | Recommendation accepted/applied/rejected differs; agent feedback does not equal verification; corrected plan carries traceable version |
| Promotion/deploy requests | Stores reviewed promotion intentions and plan artifacts | Explicit source/target revision, data policy and executable staged plan | Promotion cannot silently cross tenant/provider; preview matches apply; rollback/forward-fix is tested for supported modes |
| Alerting/notifications | YAML alert channels plus DB-backed notification rules; SURF-15 | One durable outbox/retry model and clear source-specific routing | Simulated outage/restart/partial delivery recovers without missing pages or duplicate floods; test button and real event use same path |
| Prometheus | Exports process/DB/action/LLM/value series | Correct metric types, bounded labels, separate absent/zero/degraded | Rollback doesn't reset false counters; disappeared DB removed; scrape failure and freshness independently visible |
| MCP agent tools | HTTP/stdio -> deterministic intent -> policy -> target adapters; SURF-01 | Principal-bound tools, consistent protocol/error semantics and capability discoverability | Viewer cannot mutate; stdio shutdown/oversize requests bounded; unsupported tool path explains gating; durable outcomes linked to source request |
| Deployment/documentation | Binary/Docker, embedded UI, provider docs and manual/live tests | A generated capability manifest stating implemented/wired/verified/unsupported for each platform | Fresh install plus upgrade smoke per supported mode; every "works on" claim linked to actual provider receipts and limitations |

#### Meta findings and additional questions the product should answer

1. **Where is the source of truth?** There are overlapping action history,
   decision, verification, queued action, value and incident records across
   target and meta databases. Define authoritative ownership before adding SRE
   incident state. Every displayed result should resolve to one immutable chain.
2. **What counts as done?** Compilation and package fixtures do not prove a
   feature's entry point reaches its outcome. Maintain an executable feature
   manifest: route/command -> principal -> policy -> runtime owner -> storage ->
   side effect -> verifier -> API/UI evidence -> failure/restart path.
3. **Which claims are observations versus estimates?** Verified, restored,
   ready, avoided incident, savings and ranked are operational promises. Encode
   their evidence requirements as schemas and shared query views; do not let
   each UI projector invent a weaker definition.
4. **Who may delegate authority?** Human API roles, standing operational policy,
   agent identity, tenant scope and database credentials are separate checks.
   Adding an AI SRE should not collapse them into model-supplied claims.
5. **Does permission expire with evidence?** Bind approval to revision, policy,
   target identity, preconditions and time. Projection time must never refresh
   safety evidence. Revalidate immediately before execution.
6. **What happens after an uncertain external result?** Create/destroy timeout,
   token expiration, lost response and process restart need reconciliation,
   stable operation IDs and operator-visible uncertainty; never retry blindly.
7. **What does an agent-created DB receive?** Provisioned is not connected,
   monitored, analyzed, protected or backed up. Expose each commissioning gate
   rather than a single green state.
8. **How is benefit measured honestly?** Time saved is a model; incident avoided
   is a counterfactual. Separate these from observed latency/error/recovery
   improvements, include uncertainty and prevent double credit.

#### Recommended implementation order

1. Repair SURF-01/02 identity and authorization, then add real router-level
   regression coverage.
2. Repair Value's storage/attribution contract and truthful verification states.
3. Restore actionable Cases navigation, ID integrity and evidence freshness.
4. Fix provider readiness and complete or explicitly narrow Terraform/backup
   claims; establish provider commissioning receipts.
5. Unify fleet desired-state reconciliation and finish adaptive monitoring,
   secret refresh and lifecycle cleanup.
6. Add durable alert delivery and audit actor provenance.
7. Build the proposed AI SRE feature on these completed primitives, with causal
   evidence, scoped authority and independently verified recovery.

#### Verification limitations

- This sub-audit did not run Go, browser or provider test suites; do not count it
  as a passing test run. The coordinating report owns test totals/coverage/skips.
- No production data, live user identities, credentials or cloud resources were
  used to demonstrate a vulnerability.
- Source-only absence-of-caller checks excluded tests and compiled JS bundles.
  Public APIs intentionally available to embedders are not automatically dead
  code; unfinished runtime features above have explicit advertised product paths.
- Gemini review was blocked before execution by automatic approval review, which
  rejected sending authentication source to the external Gemini service. No
  workaround or alternate-model upload was attempted.



## Part 4: Runtime safety and lifecycle audit

### Runtime, safety, evidence, and lifecycle audit

Source: pg_sage `master`, `b396595059d2b1b312a22231fdbfd1d2cfef9530`, 2026-09-26.
All source references are relative to the isolated `audit-repo` snapshot. Original
checkout and live services were not changed. P0 means possible data loss; P1 means
release-blocking safety/security or important broken workflow; P2 means correctness
or incomplete capability. A static confirmation establishes the source path, not
that the user's live installation has exercised it.

#### R01 — P0: retention can delete live rows in other partitions

**Confirmed by executable SQL reproduction.** `sidecar/internal/autonomy/retention_postgres.go`
`deleteBatch` selects `ctid` from the parent, then deletes using only equality on
`ctid`. The detector explicitly includes partitioned parents (`schema_postgres.go`,
`unboundedAppendSQL`, `relkind IN ('r','p')`). Physical row locations repeat across
partitions. One expired row in partition A and one future row in B both had `(0,1)`;
with batch limit 1 the production-equivalent query returned **DELETE 2**, including
the future row. All fixture changes were rolled back.

Evidence: [SQL reproduction](evidence/retention-partition-repro.sql),
[captured result](evidence/retention-partition-repro.txt).
[PostgreSQL system-column documentation](https://www.postgresql.org/docs/current/ddl-system-columns.html)
confirms `ctid` is a location within a table and `tableoid` distinguishes partitions.

**Fix:** bind victims and deletion to both `tableoid` and `ctid`, or process one
validated leaf relation at a time; keep a cutoff predicate at the deletion boundary.
Test partitioned/inherited tables, duplicate physical locations, concurrent updates,
row movement, RLS, and a strict total batch bound. Acceptance: no noneligible row is
ever deleted; total deleted rows never exceeds the authorized batch size.

#### R02 — P1: retention deletes bypass the central execution policy gate

**Confirmed source path.** `cmd/pg_sage_sidecar/autonomy_runtime.go` starts a schema
custodian alongside the executor. `autonomy/schema_postgres.go:28-39` enables retention
planning and injects a `postgresRetentionEnforcer` directly. Its `Apply` method checks
a prior dry run and whether the standing document lists the retention class. It does
not call `policy.Gate.Authorize` or the executor. It then calls `pool.Exec` directly.
Consequently the execution path does not inspect emergency stop, executor enabled,
observation/manual mode, approval-required classes, maintenance windows, current
blast-radius usage, or a mutation lease. Those checks exist in `policy/gate.go` and
are bypassed here. An authorized retention class plus a prior dry run is the enabling
condition; defaults alone do not establish that every deployment will delete rows.

**Fix:** retention must submit a typed destructive action through one authoritative
gate and execution mechanism. Check stop state immediately before each batch, use
bounded query/lock timeouts and leases, and record durable intent before mutation.
Accept only if stop, observation, disabled executor, required approval, closed window,
exceeded budget, replica, and stale policy all produce zero deleted rows.

#### R03 — P1: retention guesses the meaning of time and accepts stale dry runs

**Confirmed contract gap.** `schema_postgres.go` picks the first date/time attribute
using a preference for `created_at`, then `occurred_at`, then `updated_at`. The
`schemaguard.TableContract` includes a duration but no explicit retention column.
`requireDurableDryRun` accepts any historical dry run for schema/table, without binding
it to relation identity, policy version, time column, duration, or candidate population.
A changed contract can delete a different population using unrelated old dry-run evidence.

**Fix:** explicit owner-selected column, relation OID plus generation, contract hash,
policy version, candidate count/sample, expiry, and row limit. Changing any semantic
input invalidates the dry-run evidence. No production deletion on inferred semantics.

#### R04 — P1: incident state is not durable across restart or manual resolution

**Confirmed source path.** `rca/rca.go:127-136` initializes an empty in-memory incident
list. There is no production read/hydration path for unresolved persisted incidents.
After restart, existing rows are never visited by auto-resolution and a continuing
condition receives a new UUID. Conversely the API resolver (`api/handlers_v09.go:419-436`)
sets only the database row, while `PersistIncidents` upserts its old in-memory
`ResolvedAt` value (`rca.go:370-407`). The next persistence cycle can reopen a manual
resolution. The supplied resolution reason is discarded.

**Fix:** one incident repository/state transition owner, startup recovery, versioned
compare-and-swap updates, explicit reoccurrence semantics, and persisted actor/reason.
Acceptance: restart preserves incident identity; manual resolve survives subsequent
collection; a genuinely recurring incident is linked and distinguished from reopening.

#### R05 — P2: RCA database identity and explanation provenance are incomplete

`Incident.DatabaseName` is persisted but normal deterministic/LLM constructors never
populate it. The adapter lacks a database identity field. API aggregation patches
display context from the pool, but stored evidence and self-action matching lack a
strict incident identity (empty names are treated as matching). Do not infer a proven
cross-database exploit: normal per-database pools constrain many calls.

Tier 2 uses a fixed confidence of 0.6 (`rca/tier2.go`) and attaches chain descriptions
to signal IDs by array position; chain evidence text is empty. This is an explanation
quality limitation, not a calibrated probability. The new SRE specification must
require explicit evidence references, competing explanations, and disproof.

**Improve:** attach stable database ID and observation epoch at construction; persist
evidence hashes, timestamps, version, confidence rationale, and model/prompt provenance.
Require every causal statement to reference an actual observation. Missing evidence
must downgrade or reject the explanation rather than fill it with positional labels.

#### R06 — P1: fleet rollout is implemented as components but never activated

`cmd/pg_sage_sidecar/rollout_runtime.go:13-24` declares a runtime factory and returns
`ErrRuntimeDeferred` when nil. No production assignment to the factory exists. The
periodic scheduler starts, but all non-test paths remain deferred. `rollout.NewRuntime`
has tests and useful policy/cohort code but no assembled production constructor.

**Fix:** either visibly label the capability unavailable or wire concrete prior
evidence, cohort, policy, run store, actuator, verification, and rollback dependencies.
Acceptance: an actual binary run against three disposable databases selects one
canary, verifies it, halts on regression, survives restart, and never touches a
noncohort database. Unit tests of the rollout engine are not proof of this feature.

#### R07 — P1/P2: schema structural remediation ends in a successful no-op

`autonomy/schema_postgres.go:254-259` returns nil when a structural rehearsal router is
missing or SQL is empty. `executorProposalRouter` implements normal and verified-index
routing only; no production implementation of `RouteStructuralRehearsal` exists. The
schema custodian counts this nil result as routed and records a decision. Several
invariant enum/classifier branches also have no production detector in the schema
custodian (the runtime scans only FK indexes, append retention, and text pathologies).
Some similar findings exist in the separate schema lint engine; that does not complete
this specific custodian lifecycle.

**Fix:** return a typed unavailable/recommend-only result that reaches Cases, with a
reason and next action. Wire a supported rehearsal adapter only after proof requirements
are met. Acceptance: every emitted invariant has a visible terminal disposition;
unavailable dependencies cannot increment a successful routing metric.

#### R08 — P1: clone rehearsal can claim promotion without workload proof

`migration/runtime/postgres_rehearsal.go:48-69` measures command duration and database
size but never populates `AffectedQueries`. `migration/rehearsal/orchestrator.go` checks
regression only by iterating `AffectedQueries`; an empty slice passes and returns
`promote_expand`. Command wall time is labeled `MaxLockDuration` although it is not a
measurement of held-lock duration. There is no meaningful workload-regression test on
this production path. Policy still applies afterward, so this is an invalid evidence
gate rather than proof of unconditional execution.

The same orchestrator ignores clone-destruction errors, allowing apparent success
with a leaked resource. `mcp_migration_runtime.go:22-27` additionally provides a managed
snapshot factory that always returns unavailable; the DLE path is the concrete path.

**Fix:** require named affected workload, paired pre/post observations, representative
parameters, plan and latency evidence, freshness, coverage and variance checks. Empty
or unrepresentative workload => inconclusive, never promote. Record cleanup failure
durably and reconcile resource teardown. Report DLE versus managed snapshot readiness
honestly. Test interrupted cleanup and partial migration steps.

#### R09 — P2: FK-index detector accepts unusable coverage

The `missingFKIndexSQL` subquery in `autonomy/schema_postgres.go` checks whether FK
attribute numbers are contained anywhere in `idx.indkey`. It does not require usable
leading key columns, exclude INCLUDE-only attributes, or reason about a partial-index
predicate. Thus an FK can be labeled covered by an index that cannot serve the relevant
lookup. This is a false-negative performance/safety diagnosis, not constraint loss.

**Fix:** use actual key attributes (`indnkeyatts`), accepted access method and operator
class, leading-prefix compatibility, and a predicate proven applicable to the workload.
Acceptance: INCLUDE-only and noncovering partial indexes cannot hide a missing FK index;
test composite FK order, multicolumn indexes, partitions, invalid indexes and expressions.

#### R10 — P2: query evidence needs reset identity and workload comparability

`querystore/querystore.go` correctly computes differences rather than lifetime means
and rejects decreasing endpoint counters. However it records only query ID and totals,
without server/statistics reset epoch, user identity, top-level identity, or schema/plan
version. A reset followed by enough new calls can exceed the old endpoint and look
monotonic. A query fingerprint alone does not prove comparable before/after workload.

**Improve:** capture statistics epoch and grouping identity; use normalized interval
deltas; require comparable cohorts and minimum effective samples; invalidate evidence
on reset, failover, schema drift, or changed query shape. PostgreSQL documents reset
and snapshot semantics in its [statistics reference](https://www.postgresql.org/docs/current/monitoring-stats.html).
Treat absent or reset data as unknown, never zero load or verified improvement.

#### Feature dispositions and improvements for this review slice

| Feature group | Runtime disposition | Next improvement / acceptance |
|---|---|---|
| RCA deterministic trees | Wired; incident lifecycle defective | Durable state owner; recovery/resolution tests R04 |
| RCA LLM and self-action correlation | Wired; identity/provenance incomplete | Explicit cited hypotheses; no fixed score presented as probability |
| Log ingestion, parser, fanout | Wired to RCA and migration observers | Durable cursor and observable loss/freshness; rotation/truncation/restart replay |
| On-demand EXPLAIN / auto_explain support | Wired; on-demand path captures estimated plans | Clearly label estimated versus actual plans; distinguish session activation from application coverage |
| Query-store history | Wired; fleet pruning gap in core report | Reset-aware interval identities; bounded retention and drift invalidation |
| Schema lint | Wired deterministic/optional LLM runner | Show severity, owner, scope and remediation evidence; measure false positives |
| Schema guard / table contracts | Partial; R01-R03/R07/R09 | Safety gate first, explicit contracts and visible blocked dispositions |
| Freeze custodian | Wired through typed proposals | Deadline uncertainty, owner exemptions, freshness and hard-stop regression suite |
| WAL/slot custodian | Wired; auto-drop deliberately disabled | Owner heartbeat and retained-WAL evidence; opt-in/manual drop, never guess abandoned |
| Migration detection and PR-ready scripts | Wired; backend produces scripts/metadata | Versioned preflight identity; CI artifact must match reviewed SQL digest |
| Online migration / clone rehearsal | DLE path exists; proof incomplete R08 | Real workload replay, durable step journal, explicit expand/contract ownership |
| Managed snapshot clone | Adapter interface only in live construction | Provider-specific readiness and tested cost/TTL cleanup; mark unavailable until built |
| Decision ledger / self-audit | Wired worker and repository | Capture every mutation before execution; repair retention bypass; link readable evidence from UI |
| Standing policy / leases | Implemented central gate; incomplete path coverage | Prove all writers call the gate; one authoritative database generation and lease |
| Fleet cohort rollout | Components and periodic loop; factory unwired | Compose production runtime with canary, verification and recovery R06 |
| Legacy C extension | Frozen reference, not Go runtime | Archive/package boundary and docs; do not sell `sage.diagnose()` as sidecar functionality |

#### Maintainability and release hygiene

The production entry point is about 2,840 lines, far above the 500-line project limit;
several initialization functions also exceed the 50-line limit. Duplicated standalone,
static-fleet, metadata-managed, and AgentDB registration paths explain feature parity
drift. Refactor only after pinning lifecycle behavior with integration tests: one
database runtime constructor with explicit optional capabilities is the useful seam.
Do not refactor blindly while changing action semantics.

The roadmap still describes shipped alerting/plan history as future and contains old
test/coverage claims; the original ignored CLAUDE notes specify older Go versions and
feature counts. `sidecar/go.mod` requires Go 1.25.0. Generate a capability manifest from
runtime registration and use it to drive docs, readiness endpoints, and contract tests.

Frontend build/lint pass but the generated JS bundle is about 874 KB before gzip and
produces a size warning. Split advanced feature pages and large chart code; measure
first usable Cases/Value rendering on a constrained connection before choosing targets.

Dependency audit reports three moderate package entries for the same Vitest mocker
advisory, not three distinct production-server exploits. It concerns dev-server file
read behavior under stated reachability/plugin preconditions. Upgrade supported Vitest
and matching coverage packages, then rerun frontend checks; no blanket `audit fix --force`.
[Primary advisory](https://github.com/advisories/GHSA-82fw-gwwq-j7x9).

#### Verification boundary

This audit does not certify zero bugs. Confirmed paths and one destructive-query
reproduction are documented above; complete suite counts, skips and coverage are in
the verification report. No customer data was used, no cloud resources were provisioned,
and no product remediation was silently applied as part of the review.



## Part 5: Independent runtime cross-review

### Independent cross-review of runtime findings R01-R04

Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`. Reviewed 2026-09-26.
Source-only review by the surface-audit agent. No full tests or live mutations.

#### R01: Confirmed; no counterevidence found

The query in `sidecar/internal/autonomy/retention_postgres.go:116-119` selects
only ctid and deletes using only ctid equality. The detector explicitly includes
partitioned parents. The coordinator's transactional reproduction establishes the
cross-partition effect. This also applies to inherited relations with overlapping
physical tuple locations. LIMIT bounds selected tuple locations, not deleted
rows; FOR UPDATE SKIP LOCKED cannot make ctid globally unique.

P0 is justified under the report's definition of possible data loss. Preserve the
retention contract/policy enabling conditions; do not imply default deletion.

#### R02: Confirmed; emergency stop does not cancel this worker

The full production chain is:

1. `cmd/pg_sage_sidecar/main.go:756` starts standalone autonomy. Fleet starts it
   at `main.go:1542` even after SetExecutorEnabled at line 1541. The autonomy
   constructor does not consume the flag.
2. `cmd/pg_sage_sidecar/autonomy_runtime.go:61-98` constructs the schema custodian.
3. `autonomy/schema_postgres.go:28-37` sets AllowRetentionApply=true and installs
   postgresRetentionEnforcer directly.
4. `autonomy/supervisor.go:143-177` runs schema work on ticks and DDL triggers.
   runSchemaGuard checks context and a concurrency semaphore, not stop state.
5. `schemaguard/custodian.go:91-116` plans and routes. Its policy checks contract,
   exemption and prior history, not executor mode, trust, replica or stop state.
6. `autonomy/schema_postgres.go:241-246` routes retention directly to Apply;
   `retention_postgres.go:40-47` checks old dry-run existence and the allowed
   retention class, then executes DELETE directly.

Crucial counterevidence check: `sidecar/internal/fleet/manager.go:239-293`
intentionally leaves monitoring goroutines running on emergency stop. It persists
the stop flag and changes inst.Stopped but does not invoke Cancel. Retention reads
neither state. An enabled retention contract with dry-run history and permitted
retention class can still delete rows after the API emergency-stop operation.
Process shutdown/instance removal can cancel context, but are separate controls.

Precision: a true PostgreSQL hot standby rejects writes itself. Missing application
replica gating does not imply bypass of PostgreSQL read-only enforcement.

#### R03: Confirmed; distinguish dry-run evidence from human approval

The detector selects a timestamp/date column by name/order; TableContract contains
a duration but no explicit owner-selected column. requireDurableDryRun accepts a
historical row for schema/table only. History also counts decisions by target text
and invariant kind, with no contract hash, relation identity or expiry.

No upstream semantic/version check was found. Drop/recreate under the same table
name or changing duration can retain eligibility from old evidence. Replace
"unrelated old approval" in R03 with "unrelated old dry-run evidence": this path
does not require human approval of that dry run in the first place. A scheduled
first dry run creates its own history and a later cycle becomes Apply if standing
policy permits retention, without separate operator approval after seeing count.

#### R04: Confirmed; manual resolution overwrite is a production chain

`rca/rca.go:125` starts with an empty incident slice; no hydration/manual-resolve
callback was found. `cmd/pg_sage_sidecar/rca_adapter.go:31-35` only forwards persist.
The API updates only the database resolved_at (`api/handlers_v09.go:419-436`) and
discards reason. The RCA upsert sets resolved_at=EXCLUDED.resolved_at and iterates
all tracked incidents. A still-active memory incident therefore overwrites a
manually resolved row with NULL on the next persistence cycle. A continuing signal
keeps it active; no unusual race beyond the next normal persist is needed.

If memory independently auto-resolves first, that cycle does not reopen the row;
this does not negate the demonstrated path. After restart, old unresolved rows
are absent from memory, cannot be auto-resolved or deduplicated there, and later
detections can create new UUIDs while old rows remain unresolved.

#### Recommendation

R01 and R02 form a destructive-path release blocker. Keep retention mutation
disabled until central policy, leases, exact target identity and evidence binding
are mandatory. R03 and R04 also remain substantiated. No severity downgrade is
recommended, with the enabling-condition and PostgreSQL-read-only qualifications.


## Part 6: Independent security cross-review

### Surface findings independent challenge pass

Date: 2026-09-26. Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Scope: independently challenge SURF-01, SURF-02 and SURF-03 in
`surface-audit.md` by tracing production source. References are relative to
`audit-repo`. No tests, source changes, live identity flow or live mutations
were performed in this pass. Root owns runtime verification results.

#### SURF-01: retain P1, with deployment and policy prerequisites

**Confirmed.** `sidecar/internal/api/router.go:155-159` mounts the HTTP MCP
handler without `RequireRole`, while `policy_handlers.go:49` role-gates REST
policy proposal creation. The middleware wiring in
`cmd/pg_sage_sidecar/wire.go` supplies session authentication but does not turn
it into an MCP role check. `internal/mcp/production_backend.go:75` routes a
proposal to `postgres_access.go:200`, which persists it with a generic actor.
The typed intent planner and executor also do not consume the authenticated
user's role. The runtime has no later viewer-role check that closes this gap.

Counterevidence and limits:

- Authentication is still required: this is an authorization gap for a valid
  viewer session, not anonymous access.
- `internal/config/defaults.go:194-195` defaults MCP to `stdio`. The affected
  HTTP surface requires an explicit transport configuration.
- Policy proposal creation does not ratify the policy. The proposal path must
  not be described as direct policy activation or unrestricted execution.
- Actual table-contract and slot-consumer mutations still pass the standing
  policy gate and runtime checks. `internal/mcp/production_intent_executor.go:68`
  checks for an executable verdict; `internal/policy/gate.go` checks runtime,
  mode, allowed classes, approvals and other restrictions. These checks limit
  impact but do not establish caller authorization.

**Judgment:** P1 remains justified for HTTP MCP deployments because roles are
an advertised authorization boundary and the bypass reaches both persistent
proposals and permitted mutations. Retain the concrete policy prerequisites
already present in the original finding. Add default-stdio scope to avoid
implying every default installation exposes it.

#### SURF-02: retain conditional P1; no OAuth protocol bypass established

**Confirmed.** `internal/auth/oauth.go:303-340` decodes the OIDC userinfo into
an email-only structure and accepts any nonempty email, ignoring verification
and durable issuer/subject identity. `internal/auth/auth.go:349-390` selects
an existing local user by that email and preserves the existing role. The
callback in `internal/api/auth_handlers.go:479-537` then creates a session for
that user. No intervening verified-email or authorized-linking check was found.

Counterevidence and limits:

- The authorization-code exchange and bound, single-use state validation are
  present in `oauth.go:149-171`. The finding does not bypass those checks.
- The attacker needs a valid identity/token accepted through the configured
  issuer whose email claim can collide with a privileged local account.
  Exploitability is therefore issuer-dependent, as the original report states.
- OAuth is disabled by default (`internal/config/config.go:933` initializes
  the default role but leaves enabled false).
- The GitHub fallback email path explicitly requires primary and verified
  email (`oauth.go:419-429`). Do not generalize this OIDC finding into a claim
  that every provider's every email path accepts unverified addresses.

**Judgment:** retain P1 with the stated issuer precondition. Persist
issuer+subject as the identity key and require deliberate authorized account
linking. Verified email is necessary for email-based linking but alone should
not grant arbitrary cross-issuer linkage. No evidence here supports weakening
the finding merely because CSRF or code-exchange checks exist.

#### SURF-03: retain defect; recommend P2 for both reporting failures

**Confirmed.** Normal executor inserts in `internal/executor/executor.go:1243`
and `manual.go:325` omit database identity; the added column is nullable with
no default (`internal/schema/ddl_agent_value.go:42-44`). No attribution trigger
or production repair write was found. `internal/value/postgres.go:68-82` stamps
credit without repairing identity. Its realized-value read at `:139-163`
LEFT JOINs the missing identity and scans `d.name` into a string. The resulting
NULL scan error propagates through `value/service.go` and becomes an HTTP 500
at `internal/api/value_handlers.go:30-34`. A database filter instead excludes
that unattributed row.

The distinct topology issue is also supported: `internal/api/router.go:183-184`
constructs Value using one supplied auth/meta pool; `cmd/pg_sage_sidecar/wire.go`
selects that pool independently of target executor pools. Value has no fleet
pool aggregation analogous to the Cases handler. Separate-meta deployments
can therefore omit credit persisted on targets even after NULL scanning is
fixed.

Counterevidence and limits:

- Not every normal action immediately breaks the Value endpoint. The query
  requires both successful outcome and non-NULL toil credit. Credit itself
  requires a completed successful verification and a valid toil model.
- Manual inserts also omit `decision_id`; normal verification finalization
  requires that ID. A generic manual action is therefore not a reliable
  reproduction of the credited-row scan defect. Use a standing-policy action
  that actually completes and receives credit, or a clearly labeled fixture.
- With a distinct meta pool, the API may read no target row and show incomplete
  data rather than produce the NULL scan error. These are separate failure
  modes whose prerequisites should remain explicit.
- This finding demonstrates reporting failure and incorrect attribution; it
  does not itself show unsafe remediation, data loss, or an auth bypass.

**Judgment:** recommend splitting into two P2 findings (nullable attribution
read and fleet storage/read topology), or retaining one P2 with both causes.
P1 is warranted only with a documented critical billing/compliance dependency
or a release-specific requirement that makes a Value outage a blocker. The
original report's P1 classification is stronger than the demonstrated impact
relative to the two identity/authorization findings.

#### Result

No counterevidence overturns any of the three source findings. Scope SURF-01
to HTTP deployments, preserve SURF-02's issuer-dependent exploitation condition,
and reduce SURF-03 to a functional/reporting priority unless additional product
criticality is documented. The original source files and original finding
report were not modified.


## Part 7: Primary-source research

### pg_sage AI SRE research

Research date: 2026-09-26. Decision artifact; proposed functionality is not implemented.
Companion: `ai-sre-spec.md`. The main audit owns the full defect and test inventory.

#### Executive Recommendation

Build **Sage Incident Investigator**: an evidence-driven investigation and recovery workflow
inside Cases. It should collect bounded diagnostic evidence, compare competing explanations,
show the next discriminating check, hand an approved typed action to the existing executor,
and verify whether the incident actually improved. The product promise is fewer operator
minutes spent proving what happened and what to do next, measured against today's pg_sage.

This is an extension of existing RCA, not a new name for it. Current source already contains
decision trees, LLM correlation, log ingestion, incident lifecycle, and self-action correlation.
The useful new unit is a durable investigation with evidence provenance, explicit uncertainty,
customer-impact context, bounded tool use, and a recoverable verification contract.

Source baseline: clean `master`, commit `b396595059d2b1b312a22231fdbfd1d2cfef9530`,
tag `v1.5.0`; remote HEAD matched during the parent audit. Analysis used the independent
`audit-repo` clone in this workspace. No production mutation or cloud commissioning occurred.

| Rank | Recommendation | Impact | Feasibility | Safety risk | Differentiation |
| --- | --- | --- | --- | --- | --- |
| 1 | Repair identity, hydration, and workflow wiring before adding authority | High | High | Reduces risk | Foundational |
| 2 | Ship read-only evidence investigations for blockers, connection pressure, WAL | High | High | Low if budgeted | Medium-high |
| 3 | Prove recovery with freshness-aware, customer-impact verification | High | Medium | Medium | High |
| 4 | Attach Vector Lab proof to Cases with identity, cohort, and expiry | High for RAG users | High | Low | High |
| 5 | Rehearse incident and migration hypotheses in disposable agent DBs | Medium-high | Medium | Medium | High |

These rankings are product judgments, not measured market share or demonstrated ROI.
Start with a focused developer/platform-team buyer managing several PostgreSQL databases.
Use successful investigations and verified operator-time savings as adoption signals.
Do not optimize for the number of AI messages, automatically closed incidents, or SQL actions.

#### Current-State Assessment

The following is grounded in the inspected source snapshot, not only roadmap prose.

| Workflow | Existing implementation | Increment worth building |
| --- | --- | --- |
| Detect/correlate | `internal/rca/rca.go`, `signals.go`, `trees.go`, `log_trees.go` | Stable evidence IDs and versioned investigation state |
| LLM explanation | `internal/rca/tier2.go` correlates uncovered signals | Multiple hypotheses, disproof, validated references, bounded next checks |
| Self-induced failure | `internal/rca/self_action.go` links actions and signal families | Object-specific evidence and pre/post causal tests |
| Cases/actions | `internal/cases/case.go`, `incident_projector.go` | Investigation panel and reusable evidence packet |
| Execute | Existing typed actions, policy, trust, approval, emergency stop | Reuse these contracts; no second mutation engine |
| Verify | `internal/verify/types.go` measures query/write latency and load | Incident-specific predicates, service SLIs, confounders |
| Provider telemetry | `internal/providerobs/` and runtime wiring | Explicit freshness/completeness and capability reasons |
| Vector proof | `internal/vectorlab/`, September 4 product guide | Attach existing experiment evidence to a case |
| Agent databases | `internal/agentdb/`, deployment guide | Bounded rehearsal tied to an investigation |

Tier 2 currently assigns confidence `0.6` to LLM incidents. It converts an arrow-delimited
causal-chain string into links and assigns signal IDs by position; those links do not carry
validated evidence for each claim. This is insufficient to call the number a calibrated
probability or to treat a narrative as proof. The new workflow should retain useful hypotheses
while clearly labeling what was observed, inferred, contradicted, or not measured.

The parent audit identified prerequisites: RCA restart hydration is absent; incident database
identity is not bound through the engine; the structural rehearsal router lacks a production
implementation; the fleet rollout factory is not assigned in production; managed snapshot
creation defaults to unavailable. Treat those as repair gates, not available SRE infrastructure.
Verify fixes through executable runtime paths before depending on them.

Repository documents can be stale: `META.json` describes the C extension and version `0.5.0`;
`docs/architecture.md` identifies the Go sidecar as the product and the C extension as frozen.
Earlier RCA specifications remain labeled draft even where source exists. Current code and
runtime tests must decide support. The proposal does not revive the C-extension agent path.

#### Evidence Map

Primary sources were retrieved on 2026-09-26 unless a publication date is stated. Product
documentation verifies advertised behavior, not independent efficacy. Issue reports establish
concrete pain and reproducible questions; they do not establish prevalence or current bugs.

| ID | Source | Type and supported claim | Relevance | Confidence |
| --- | --- | --- | --- | --- |
| S01 | [PostgreSQL statistics](https://www.postgresql.org/docs/current/monitoring-stats.html) | Official: cumulative counters and observation semantics need careful interpretation | Normalize intervals and reset epochs | High |
| S02 | [pg_stat_statements](https://www.postgresql.org/docs/current/pgstatstatements.html) | Official: planning/execution statistics update on successful operations; identifiers have stability limits | SQL stats cannot substitute for customer error-rate SLI | High |
| S03 | [Routine vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html) | Official: vacuum and freezing preserve space reuse and XID safety | Diagnose horizons before maintenance advice | High |
| S04 | [Replication settings](https://www.postgresql.org/docs/current/runtime-config-replication.html) | Official: slot retention can be unlimited; retention caps can remove needed WAL | WAL mitigation needs replication-loss analysis | High |
| S05 | [Explicit locking](https://www.postgresql.org/docs/current/explicit-locking.html) | Official: operations acquire incompatible locks and can block one another | Build a blocker graph and distinguish holder from waiter | High |
| S06 | [Administration functions](https://www.postgresql.org/docs/current/functions-admin.html) | Official: cancellation and termination are distinct functions with permissions | Preserve exact action semantics and target checks | High |
| S07 | [Google SRE alerting](https://sre.google/workbook/alerting-on-slos/) | Primary SRE guidance: multi-window burn alerts balance precision and response time | Customer-impact priority, not symptom-count priority | High |
| S08 | [PgBouncer issue 1284](https://github.com/pgbouncer/pgbouncer/issues/1284) | Operator report: pool/server disconnect incident and timeout interaction | Include pool evidence and timeout ordering | Medium; one case |
| S09 | [PgBouncer configuration](https://www.pgbouncer.org/config.html) | Official: queue, pool, and timeout controls have different scopes | Avoid treating max_connections as the only lever | High |
| S10 | [Supabase pooling](https://supabase.com/docs/guides/database/connecting-to-postgres/pooling-and-limits) | Provider: client/backend limits and independent poolers differ | Explain managed-service headroom and pool topology | High |
| S11 | [pganalyze Index Advisor](https://pganalyze.com/docs/index-advisor/getting-started) | Vendor: whole-workload recommendations account for read/write tradeoffs | Index suggestions alone are already competitive territory | High for documented scope |
| S12 | [pganalyze docs](https://pganalyze.com/docs) | Vendor: VACUUM Advisor and alerting are offered | Generic vacuum dashboards are not a new category | High for documented scope |
| S13 | [AWS DevOps Guru for RDS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/devops-guru-for-rds.html) | Provider: telemetry-based reactive/proactive insights and investigation suggestions | Cross-signal diagnostics already exist | High for documented scope |
| S14 | [Cloud SQL Index Advisor](https://docs.cloud.google.com/sql/docs/postgres/use-index-advisor) | Provider: recommendations include estimated impact/storage; entitlement requirements apply | Capability gating must include edition and telemetry availability | High |
| S15 | [Azure automatic tuning](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql) | Provider: apply, verify improvement, and revert harmful tuning | Verification is expected product behavior | High |
| S16 | [Oracle automatic indexing](https://docs.oracle.com/en/database/oracle/oracle-database/26/admin/managing-indexes.html) | Provider: automatic index management follows workload changes | Consider workload lifecycle, not one-off suggestions | High |
| S17 | [Datadog investigations](https://docs.datadoghq.com/bits_ai/bits_investigation/investigate_issues/) | Vendor: iterative hypothesis checks, telemetry tools, inconclusive outcomes | A chat summary is below the competitive bar | High for documented scope |
| S18 | [Datadog configuration](https://docs.datadoghq.com/bits_ai/bits_investigation/configure/) | Vendor: investigation rate limits, RBAC, tool-call audit | Investigation cost and permissions need first-class controls | High |
| S19 | [incident.io AI SRE](https://incident.io/solution/ai-sre) | Vendor: telemetry/change/history correlation; reviewed PR is described change path | Human review remains a credible product model | High for stated boundary |
| S20 | [Rootly AI SRE](https://rootly.com/ai-sre) | Vendor: incident/code/telemetry context and hypothesis-based investigation | Compete on Postgres depth and proof, not generic RCA | High for advertised scope |
| S21 | [PagerDuty engineering](https://www.pagerduty.com/eng/inside-pagerdutys-sre-agent-how-we-built-deep-incident-investigation/) | Vendor engineering, June 2026: incident context and investigation system design | Evaluation and context quality are core engineering work | Medium-high |
| S22 | [pgvector README](https://github.com/pgvector/pgvector) | Maintainer: ANN/filter/build/query tradeoffs and iterative scans | Version-aware candidate generation | High |
| S23 | [pgvector issue 244](https://github.com/pgvector/pgvector/issues/244) | Historical report: dead tuples and ANN recall loss | Include maintenance/churn in vector incidents | Medium; not current defect proof |
| S24 | [pgvector issue 776](https://github.com/pgvector/pgvector/issues/776) | Reporter corrected initial assumptions about iterative scans and subqueries | Inspect actual plans; do not repeat issue title as fact | Medium-high |
| S25 | [pgvector issue 785](https://github.com/pgvector/pgvector/issues/785) | Report: filtered ordering/limits create difficult performance tradeoffs | Query-shape/cohort tests over blanket settings | Medium |
| S26 | [pgvectorscale README](https://github.com/timescale/pgvectorscale) | Maintainer: StreamingDiskANN, label filters, post-filtering, query controls | Add a separate adapter after pgvector evidence attachment | High |
| S27 | [Filtered ANN study](https://arxiv.org/abs/2602.11443) | Research, Feb 2026: planner/filter strategies produce workload-dependent outcomes | Benchmark exact and ANN candidates under filters | Medium; workload-specific |
| S28 | [RCheck study](https://arxiv.org/abs/2608.25185) | Research, Aug 2026: search-quality differences across groups warrant evaluation | Report cohort-level recall, not only global mean | Medium; not independently reproduced |
| S29 | [Neon schema-only branches](https://neon.com/blog/instant-branches-schema-only-or-with-data-the-choice-is-yours) | Provider: branching can separate schema from production data | Synthetic rehearsal can minimize sensitive-data transfer | High for concept; verify entitlement |
| S30 | [Neon masked environments](https://neon.com/blog/environments-masked-production-data) | Provider: masking workflows complement branches | A branch is not automatically anonymized | High |
| S31 | [BranchBench](https://arxiv.org/abs/2604.17180) | Research, Apr 2026: branch operations and data operations have different costs | Measure rehearsal setup and fidelity separately | Medium; benchmark-specific |

#### Community Pain

**Slow queries and plan changes.** Operators need the relevant plan, parameter distribution,
schema/statistics change, and workload shift together. An average from a cumulative counter
can hide the incident interval. Derive interval deltas only inside a stable statistics epoch;
carry query/database/user identity and plan provenance. Separate expensive, frequent queries
from low-volume tail regressions. S01 and S02 establish why aggregate SQL statistics alone
cannot prove end-user availability or p95 latency. Product hypothesis: evidence collection
and before/after comparison save more time than another generic tuning explanation.

**Index selection and write overhead.** Whole-workload tradeoffs are established in pganalyze
and cloud advisors, not an unexplored opportunity. Improve pg_sage by retiring stale advice,
recording counterfactual plan assumptions, revalidating after schema changes, and proving
read benefit alongside write/storage cost. If a rollout or verification path is unavailable,
show that concretely rather than converting an estimate into a success badge. [S11–S16]

**Vacuum, bloat, XID and replication slots.** A vacuum symptom does not establish that more
vacuum is the appropriate immediate action. Identify old transactions, prepared transactions,
replication horizons, progress and provider limits. For WAL, distinguish production rate,
retained bytes, archiver delay, and consumer lag. Changing retention or deleting a slot can
trade disk pressure for lost replication continuity. The new value is an owner-aware recovery
plan with evidence and escalation, not an autonomous slot-drop shortcut. [S03, S04]

**Locks and migrations.** Show the root blocking backend, waiters, lock mode, object identity,
transaction age, application owner, and recent migration. Check whether the supposed blocker
still exists before offering intervention. A cancellation that returns success is not proof
that an idle transaction released its locks. Teach the UI to distinguish an observed wait
chain, a suspected triggering deployment, and a verified recovery. [S05, S06]

**Connection storms and poolers.** Backend saturation, a client queue, connection churn, and
an application pool leak are different phenomena. The PgBouncer issue is useful precisely
because its timeout interaction is not captured by one database connection percentage.
Supabase documents distinct client/backend limits and independent pooler ceilings. Collect
topology and bounded pool metrics when available; otherwise state the missing evidence.
Increasing `max_connections` must not become the default response. [S08–S10]

**Managed Postgres.** Permission, log access, edition, provider, database version, and telemetry
freshness determine what is possible. A denied log query is a capability limitation, not proof
of no errors. The first release should succeed with catalog observations while making stronger
claims only when independent provider or application evidence is present. [S10, S13, S14]

**Vector/RAG quality incidents.** Returning fewer results and returning less useful results are
different. Filter shape, dead tuples, version, ordering, and search budgets influence what
the operator sees. Issue 776 is particularly instructive: the author revised the premise of
the original report. A trustworthy assistant preserves this correction, investigates the plan,
and supplies a reproducible experiment instead of treating the issue title as settled law.
These are sampled support cases, not a quantitative claim about all users. [S23–S25]

#### Competitive Landscape

All entries below describe public documentation reviewed this run. None is an independent
production comparison. Pricing is treated as an access/deployment signal, not a dollar quote;
commercial plans, usage charges and entitlement should be verified before procurement.

| Product | Audience and workflow | Automation/evidence/safety | Access signal and pg_sage design choice |
| --- | --- | --- | --- |
| pganalyze | Postgres specialists; workload and vacuum analysis | Workload-aware index advice and visibility into recommendation reasoning | Commercial monitoring product; win through verified incident work in the local sidecar, not more missing-index suggestions [S11, S12] |
| AWS DevOps Guru for RDS | RDS operators; anomalous load and proactive risks | Correlated telemetry, findings and corrective guidance; docs advise testing changes | AWS managed integration; offer consistent incident artifacts across providers [S13] |
| Cloud SQL Index Advisor | Cloud SQL query optimization | Estimated improvement/storage and SQL; Enterprise Plus/Query Insights prerequisites | Edition-dependent access; model capability evidence explicitly [S14] |
| Azure SQL automatic tuning | Azure SQL/Fabric database users | Continuous tuning with performance verification and reversal | Managed engine feature; adopt the proof loop as the bar, not novelty [S15] |
| Oracle automatic indexing | Oracle workload administration | Automatic index lifecycle responds to workload | Engine-specific capability; make Postgres lifecycle evidence inspectable [S16] |
| Datadog Bits Investigation | Teams with observability/service context | Tool-based hypothesis refinement and an inconclusive outcome; RBAC/rate limits/audit | Commercial platform and credits; preserve useful operation with local PG evidence [S17, S18] |
| incident.io Investigations | Incident response teams | Correlates telemetry, changes and history; described system change is a reviewed PR | Commercial incident platform; export a compact PG evidence packet [S19] |
| Rootly AI SRE | On-call/incident platform users | Parallel hypothesis checks and probable causes from service/change context | Commercial platform; differentiate with exact Postgres action constraints [S20] |
| PagerDuty SRE Agent | Incident-response and on-call users | Engineering material describes a dedicated investigation/context system | Commercial platform; integrate through existing alerting when authorized [S21] |

The defensible position is **PostgreSQL-specific evidence and recovery contracts with a
portable deployment boundary**. This is a design choice, not a claim that competitors lack
all these features. A small team should not attempt to outbuild the integrations, service
catalogs, paging systems and organization-wide telemetry of the larger incident platforms.

Useful optional output is a signed/versioned incident packet consumable by an existing
incident platform: scope, evidence, freshness, alternatives, proposed action, approval and
verification. Sending that packet needs the configured operator policy; drafting it does not.

#### Vector Search Opportunities

The current pgvector README retrieved this run uses a `v0.8.6` installation example. Treat
that as documentation context, not the version on a user's server. Read actual extension,
server and index versions before constructing a candidate. Existing pg_sage Vector Lab
requires pgvector at least 0.8.0 and explicitly excludes several advanced shapes.

Maintainer guidance: HNSW exposes `m` and `ef_construction` at build time, `ef_search` at
query time; higher search/build effort trades resource use for recall. Filtering can reduce
returned rows; iterative scans add bounded work. Filter indexes, partial indexes and
partitioning serve different selectivities. `halfvec` and quantization change representation
and need their own validation. Prefer bounded transaction-local settings before considering
rebuilds. This source-derived summary is deliberately limited; the experiment below is our
proposed design, not a guarantee from the extension. [S22]

Proposed evaluation contract:

1. Persist query cohort, tenant/filter selectivity, index/schema/embedding identity, extension
   version, distance, k, settings, row-identity type and snapshot. Never silently reuse an old
   exact baseline after a corpus change.
2. Compute exact top-k truth on the same authorized rows, RLS context and consistent snapshot.
   Prefer an approved clone for heavy scans. Verify the baseline plan is actually exact.
   If exact truth exceeds its budget, report unmeasured; do not label ANN output ground truth.
3. Record tie handling, zero/NULL-vector exclusions, underfill and the denominator
   `min(k, eligible_ground_truth_count)`. Empty eligible sets are not 100% recall successes.
4. Measure recall@k and underfill per cohort/query, latency distribution, timeout fraction,
   I/O, memory, index size, build duration, WAL and insert/update cost. Separate warm and cold
   experiments and repeat order randomly. A faster mean must not hide a bad minority cohort.
5. Compare exact-filter-first against existing HNSW candidates; later add IVFFlat lists/probes,
   half precision with full-precision reranking, and compatible DiskANN configurations.
6. Recommendation requires latency AND recall AND resource constraints for every required
   cohort. Low sample count means provisional evidence. Freeze evaluation queries before
   tuning and hold out an independent set to avoid fitting the benchmark.
7. Persist a case attachment with a content hash and explicit expiry, never raw vectors or
   sensitive filter values. An embedding-model, dimensionality, corpus or index change
   invalidates the certificate and schedules a bounded recheck.

pgvectorscale deserves a separate adapter: its README describes StreamingDiskANN,
label-based filtering and arbitrary WHERE post-filtering, plus `query_search_list_size` and
`query_rescore`. Label and arbitrary-filter paths are not interchangeable; settings can be
transaction-local. Rebuild and compression choices need new fidelity/resource tests. Do not
copy vendor benchmark multipliers into a cross-provider recommendation. [S26]

Recent research strengthens the need for measurement, not a universal tuning recipe.
The filtered-ANN study reports workload-specific planner/filter tradeoffs; RCheck examines
quality differences across groups. Neither was independently reproduced here. The actionable
inference is to evaluate query shape and minority cohorts, with exact truth and held-out
queries, before applying advice. [S27, S28]

**Smallest ship:** attach the already implemented Vector Lab JSON to Cases and the decision
ledger with workload/schema identity and expiration. Next add exact-search as an eligible
winner and representative cohort sampling. DiskANN, halfvec, partition roots, hybrid fusion
and reranking should each be separate measured releases. Distinguish ANN recall from
application relevance, grounded-answer quality, permission correctness and freshness.

#### Agent-Created Databases

pg_sage already provisions local schemas/databases and has gated cloud workflows, leases,
budgets, ownership, backups and cleanup. The opportunity is a **rehearsal attached to an
incident or change**: generate a minimum schema, constraints, synthetic seeds and a bounded
workload; reproduce a hypothesized failure; compare a proposed intervention; destroy the lab;
attach both successful and failed evidence. This connects existing modules to an operator job.

Schema-only branches and masking illustrate useful infrastructure, but a branch is not
inherently sanitized. Provider support and entitlements require target-side verification.
BranchBench reports tradeoffs between branching and data operation costs; that is a reason
to measure lab startup and runtime independently, not declare one platform superior. [S29–S31]

Require separate lab credentials without production write access; explicit target allowlists;
egress restrictions; owner/run IDs; pre-reserved compute/storage/query budgets; TTL and
independent reaper; teardown receipts; and a durable audit. Budget estimates must include
provisioning, idle compute, storage, snapshots, I/O and cleanup failures. Unknown price means
no automatic provisioning. Never send production rows to an LLM to make seed data.

Separate fidelity levels: (A) schema-only synthetic functional proof, (B) masked statistical
reproduction, (C) isolated production-shaped replay. A successful A proves semantics, not
production p95 or disk headroom. Preserve skew, hot keys, concurrent transactions and failure
timing where relevant. Application side effects, external services and client retry logic may
still be absent. Surface those gaps next to the result.

Generated migrations must pass existing DDL safety and policy. Generated workloads have a
positive operation allowlist; SQL comments and database text cannot expand authority. Lab
failure is a result worth keeping. Production promotion always revalidates schema identity,
provider readiness and approvals. The current missing rehearsal/router/rollout wiring is a
hard implementation prerequisite, so cloud rehearsals are excluded from the first release.

#### Backlog

Scoring: 1–5 ordinal judgments. Pain/differentiation/feasibility/evidence higher is better;
safety risk higher is worse. Scores organize discussion rather than imply measured precision.

| Item | Pain | Differentiation | Feasibility | Risk | Evidence | First slice |
| --- | --- | --- | --- | --- | --- | --- |
| Evidence investigator | 5 | 4 | 4 | 2 | 5 | Three catalog-based incident families, read-only |
| Recovery verifier | 5 | 5 | 3 | 3 | 5 | Blocker/queue recovery with missing-data states |
| Vector evidence attachment | 4 | 4 | 5 | 1 | 5 | Import existing report and invalidate stale identity |
| Application SLO linkage | 4 | 3 | 3 | 2 | 5 | One read-only Prometheus-compatible connector |
| Change attribution | 4 | 4 | 3 | 2 | 4 | Signed change event plus object/window overlap |
| Agent DB rehearsal | 4 | 5 | 2 | 3 | 4 | Local synthetic lab; no production data |
| DiskANN/quantized evaluation | 3 | 4 | 2 | 2 | 4 | Adapter behind explicit capability probe |
| Incident learning/replay | 4 | 4 | 4 | 2 | 4 | Redacted incident bundles and withheld test set |

For the investigator, verify known blocker graphs, stale snapshots, malformed model output,
wrong-tenant evidence, missing privileges and investigation time budgets. Success metric:
operator time to a usable evidence packet and adjudicated cause accuracy, not confidence text.

For recovery, inject apparent success with continuing symptoms and traffic disappearing.
Success metric: independently verified recoveries divided by attempted interventions,
with inconclusive, failed and harmful outcomes reported separately.

For vector attachment, test mismatched schema/corpus/role, boundary ties, empty truth,
corrupted manifests and permission leakage. Success metric: qualified recommendations
reproduced by a second run and quality regressions caught before rollout.

For SLO/change linkage, test stale counters, counter resets, duplicate/out-of-order events,
low traffic, clock skew and deployment correlation without causation. Success metric:
useful-priority precision and reduced duplicate notifications without missed incidents.

For labs, inject provisioning timeout, cost uncertainty, failed cleanup and skew mismatch.
Success metric: reproducible experiments inside budget, verified teardown, and explicit
fidelity; not the number of databases created. All thresholds in the spec are design targets.

#### Questions Not Asked

1. Who owns the page and budget: application developer, DBA, platform engineer, or managed service?
2. Which three recurring incidents consume the most actual operator time today?
3. Is the acceptable first win diagnosis quality, faster recovery, or verified toil reduction?
4. Which customer-facing service and SLI map to each database, and who maintains that mapping?
5. What data can leave the host: query fingerprints, normalized SQL, plans, logs or none?
6. Which evidence must remain available when the monitored database is completely unavailable?
7. How should missing logs/host metrics affect diagnosis, authority and user-visible confidence?
8. Who may approve intervention, and what action classes require a second approver?
9. What is the tolerated blast radius when cancellation cannot be undone?
10. Which provider/edition/version combinations will be continuously commissioned in CI?
11. How will actual operator time saved be sampled without rewarding premature closure?
12. Who owns vector relevance ground truth, tenant fairness, and embedding/corpus versioning?
13. Can lab data be exported or cloned, and who funds resources left after a failed teardown?
14. How are incorrect incident conclusions corrected and removed from future retrieved examples?
15. Should pg_sage be the incident workspace or a specialist that feeds an existing platform?

#### Research Gaps

No authenticated X/LinkedIn scan was performed. No claim about absent social discussion is
made. Search surfaced public forum material, but technical conclusions here rely on primary
docs, maintainer issues, papers and inspected code. This is not an exhaustive community census.
PostgreSQL mailing-list and Stack Overflow searches did not yield sufficiently focused,
verified material for this report; they remain follow-up research, not implied coverage.

No competitor trial, pricing quote, independent vendor benchmark, customer interview, or
production incident replay was executed. Market willingness to pay and comparative diagnosis
quality remain unproven. Papers were inspected at abstract/documentation level and need
artifact reproduction before engineering decisions depend on numeric benchmark claims.

The next evidence to collect is five to ten redacted real incidents from intended users,
their current investigation steps and time, data boundaries, and actual provider permissions.
Pair these with deliberately misleading replay cases. The feature should earn broader
authority only after it can abstain correctly, survive missing telemetry, and prove recovery.


## Part 8: AI SRE build specification

### Sage Incident Investigator: build specification

Status: proposed, not implemented. Date: 2026-09-26. Baseline: pg_sage v1.5.0,
`b396595059d2b1b312a22231fdbfd1d2cfef9530`. Research and sources: `sre-research.md`.
All budgets and success thresholds below are initial product targets, not measured results.

#### 1. Product contract and enriched brief

When PostgreSQL is behaving badly, an operator should receive a concise, inspectable answer:
what is affected, what evidence is fresh, what probably explains it, what contradicts that
explanation, which bounded check is worth doing next, and what would prove recovery.
An approved intervention must use the existing policy/executor path. Success requires
independent recovery evidence. A plausible explanation or successful SQL call is insufficient.

Investigations extend existing RCA, Cases, typed actions, ledger and verification. Keep
deterministic detection functioning without the LLM. The model selects useful diagnostic
questions from a constrained catalog and synthesizes evidence; it does not grant permissions,
invent telemetry, calculate the canonical SLI, choose its own cost cap, or execute arbitrary SQL.

Evaluate the feature against current pg_sage, a deterministic runbook, and a human operator.
Ask whether it actually reduces a database incident's operator work without making the
incident worse. Include negative evidence, missing telemetry, misleading changes, correlated
failures and pg_sage-caused incidents. Treat uncertainty and refusal to act as useful outcomes.

Primary persona: the engineer responsible for several production PostgreSQL databases who
has enough permissions to inspect them but incomplete DBA context. Secondary: the DBA who
needs a reusable evidence packet. The UI is the existing Cases workspace, not a new dashboard.

#### 2. Scope and release cuts

**Prerequisite repair gate:** fix database identity in RCA; hydrate active incidents after
restart; ensure one production proposal-to-policy-to-queue-to-executor path; expose missing
provider telemetry explicitly. Structural rehearsal, fleet rollout factory, and managed
snapshot defaults identified by the audit remain unavailable until separately commissioned.
Do not require those unfinished paths for the first investigation release.

**R1, read-only release:** persistent investigations for (a) lock/transaction blocking,
(b) connection pressure, and (c) WAL/replication retention. Automatic start is off by default.
Provide case-triggered or operator-triggered bounded catalog checks, competing hypotheses,
timeline, evidence links, supported-action proposals, verification after an operator reports
an intervention, and redacted export. Existing alerts still fire without an investigation.

**R1.1, approval beta:** opt-in handoff of one supported backend-cancel proposal through
the existing approval/executor path, with exact target checks and no automatic termination.
Add one read-only Prometheus-compatible SLI connector and signed change-event ingestion.
R1.1 must pass separate safety and service-mapping gates; R1 can ship without it.

**R2:** vector evidence attachments and quality incidents; local disposable rehearsal;
incident-library replay; more provider log/metric adapters; carefully selected reversible
actions. Every action family needs its own admission and recovery contract.

Excluded from R1/R1.1: autonomous arbitrary SQL; shell or Kubernetes execution; failover;
restarts; changing max_connections; global GUC changes; dropping replication slots;
VACUUM FULL; index rebuilding; application deployment; cloud provisioning; automatic PR
publication; on-call scheduling; general log storage; declaring application RCA from PG data
alone. A reviewable script may describe an unsupported action without claiming it executed.

#### 3. Concrete incident walkthroughs

##### A. A migration appears to cause checkout timeouts

At 14:02 an existing lock case opens. The investigator captures the wait graph, application
names, backend start times, transaction states, lock modes and affected objects. A registered
change event reports a migration at 14:00. Hypotheses: migration lock; old application
transaction; connection capacity. The migration timestamp alone is merely correlation.

A catalog check shows the migration waiting behind a 40-minute idle transaction, with live
checkout queries behind it. The original migration-as-root explanation is revised. The UI
shows the wait edges and evidence timestamps, flags missing service error telemetry, and
offers a precise operator handoff. Cancelling an idle backend has no active query to cancel
and may not release transaction locks; the system does not offer cancel as a proven cure.

In R1 the operator resolves the transaction through their normal process and records the
external action. In R1.1 a running, approved migration query may be a supported cancellation
target if its exact identity still matches. Termination remains reviewed manual procedure.
Recovery requires the wait chain to clear, workload to remain present, and any available
checkout SLI to recover. The historical initiating defect remains a separate follow-up.

##### B. A deploy coincides with connection exhaustion

The investigation considers a connection leak, a blocked-query backlog, increased traffic,
and pool fan-out. It obtains backend counts/states and application/role distribution.
If pool metrics exist, it compares client queue, server occupancy and timeout ordering.
A fresh deploy event raises one hypothesis; it cannot prove the deploy caused the problem.

If blockers are absent and each of many application replicas opens its own idle pool,
the evidence supports pool fan-out. The proposed recovery is an operator-owned app/pool
change, with database settings left to the existing policy. Missing pool metrics produces
"database pressure confirmed; pool cause not established" and a discriminating next check.
After an external fix, falling traffic alone cannot satisfy the recovery predicate.

##### C. A CDC slot threatens the WAL volume

The investigator compares current/restart/confirmed-flush positions, consumer activity,
WAL generation and available provider disk metrics. It distinguishes an offline consumer,
slow consumer, write surge and archiver failure. A slot owner label links to the operator.
Without real volume capacity/free-space data, remaining disk runway is unknown; database
relation size is not a proxy for free filesystem space.

The output proposes restoring the consumer and escalating to its owner. Slot deletion or
retention caps are reviewed actions with explicit replication-loss implications. Verification
checks consumer progress and retained-WAL trend over enough fresh observations; retained
files need not disappear immediately. No autonomous slot drop, checkpoint or consumer restart.

##### D. Optional R2: a tenant's vector results silently deteriorate

Application evidence reports retrieval underfill while latency looks healthy. The case links
existing Vector Lab evidence to corpus, index, role and query-cohort versions. Exact truth
shows eligible matches while one selective tenant cohort underfills. A bounded lab compares
query-local settings and exact-filter-first, retaining recall and latency together.
A successful experiment is a recommendation; application rollout requires separate policy.
If exact truth exceeds budget or data identity changed, the conclusion is unmeasured/stale.

#### 4. Architecture and existing integration points

Preserve the Go sidecar and Postgres metadata store. Add one cohesive `internal/sre` package
with small modules, not an autonomous service mesh. Reuse the existing LLM client, token
accounting, auth, config lifecycle, policy, ledger and notification infrastructure.

```text
Collector / logwatch / providerobs / existing RCA / approved external SLI
  -> typed trigger and evidence normalizer
  -> durable investigation + lease + diagnostic budget
  -> deterministic hypothesis templates + optional LLM next-check selection
  -> fixed read-only probe registry -> immutable evidence
  -> claim validator + alternative/counterevidence review
  -> Cases investigation panel + existing typed action proposal
  -> existing policy / approval / executor (R1.1 only)
  -> independent recovery evaluator -> timeline / audit / redacted export
```

| Area | Existing location | Planned addition or change |
| --- | --- | --- |
| RCA | `internal/rca/rca.go`, `tier2.go` | Trigger adapter; stable database identity; hydrate state; retain old deterministic detector |
| Probe evidence | `internal/collector/`, `logwatch/`, `providerobs/` | Typed adapters with observation, collection, freshness and capability fields |
| Coordinator | New `internal/sre/coordinator.go`, `worker.go` | Durable bounded work, stop/resume and lease fencing |
| Hypotheses | New `internal/sre/hypothesis.go`, `claims.go` | Structured facts/inferences, schema validation, references and disproof |
| Diagnostics | New `internal/sre/probes/` | Fixed versioned queries and catalog-defined resource budgets |
| Persistence | `internal/schema/bootstrap.go`; new DDL module and `internal/sre/postgres.go` | Additive idempotent metadata migrations and stores |
| Cases | `internal/cases/case.go`, `incident_projector.go` | Investigation reference and evidence attachment, no duplicate case queue |
| API | `internal/api/` | Scoped investigation routes; existing auth and role enforcement |
| UI | `sidecar/web/src/pages/CasesPage.jsx` | Detail panel, claim provenance, alternatives, approval and recovery states |
| Runtime | `cmd/pg_sage_sidecar/main.go`, `metadb.go` | Shared constructor used by standalone/config-fleet/metadata-fleet startup |
| Policy/action | `internal/policy/`, `executor/`, `store/action_lifecycle.go` | R1.1 adapter to existing typed action; never bypass approval |
| Evidence audit | `internal/ledger/types.go`, `service.go` | Investigation/evidence IDs and policy/model/template versions |
| Recovery | `internal/verify/` plus new `internal/sre/recovery.go` | Reuse persistence concepts; add incident predicates separately from latency gain |
| Config | `internal/config/` and config metadata/UI | Validated typed SRE fields with defaults and reload lifecycle |
| Cleanup | `internal/retention/` | Reference-aware retention and tombstones for retired evidence |

Do not run LLM/tool loops while holding `rca.Engine.mu`. Existing Tier 2 calls occur from the
locked Analyze path; the new coordinator consumes committed triggers asynchronously with
an inherited cancellation context. A bounded queue prevents an alert storm blocking collection.
Failed investigation work cannot prevent deterministic RCA, policy, or emergency stop.

Use the metadata database for durable coordination where configured. Standalone installations
that persist into the monitored database lose metadata availability with that target; advertise
this boundary. A small local redacted event spool can retain recent diagnostics during outage,
with OS file permissions, bounded disk use and no stored secrets/query literals. If spool or
metadata durability fails, enter degraded read-only reporting and prohibit action handoff.
Do not claim fleet-wide fault tolerance from an in-process queue.

#### 5. Data model and identity

New tables live in `sage`; every key is scoped by deployment UUID and stable database UUID.
Keep the existing fleet alias as display metadata. Map current numeric database IDs to UUIDs
once. Physical database identity, cluster epoch and provider resource ID are evidence fields;
where no cluster system ID is permitted, record an explicit identity-strength level.
Never coalesce databases because they share a name, query ID, IP address or incident number.

| Table | Required fields and invariant |
| --- | --- |
| `sre_investigations` | id, deployment_id, database_id, source_case_id, source_incident_id, trigger_fingerprint, state, version, started_at, updated_at, expires_at, lease_owner, lease_until, fence_token, budget_reserved, budget_used, summary, failure_code; one live trigger per scoped fingerprint |
| `sre_evidence` | id, investigation_id, deployment/database scope, source_kind, probe_version, observed_at, collected_at, valid_until, interval_start/end, reset_epoch, capability_state, payload_version, bounded redacted payload, SHA-256, classification; immutable rows |
| `sre_hypotheses` | id, investigation_id, revision, label, mechanism, status, supporting_ids, contradicting_ids, missing_checks, evidence_strength, model_version; references must resolve in scope |
| `sre_steps` | id, investigation_id, sequence, probe_id, canonical_args_hash, attempt, idempotency_key, status, start/end, outcome_evidence_ids, error_code, cost; unique step/attempt identity |
| `sre_recovery_checks` | id, investigation_id, action_ref or external_change_ref, predicate_version, baseline_evidence_ids, target_scope, earliest_at, deadline, min_samples, state, result_evidence_ids, confounders |
| `sre_events` | investigation_id, sequence, event_type, actor_id, observed_at, received_at, payload, previous_hash, hash; append-only application semantics |
| `sre_service_slos` | R1.1: id, service_id, database_id, SLI source/query identity, target, window, good/total definitions, owner, source_config_revision, enabled |
| `sre_change_events` | R1.1: source_id/event_id, scoped database/service/object refs, declared_at/received_at, change hash, external link, signature status; unique source/event |

Foreign keys include scope where practical; enforce scope again in store methods and API
authorization. Reject cross-database evidence references before model invocation. Index
investigations on `(database_id,state,updated_at)`, evidence on `(investigation_id,observed_at)`,
and steps/events on `(investigation_id,sequence)`. Use checked enums or CHECK constraints,
bounded JSON fields and nonnegative counters. Store durations/bytes with explicit units.

An investigation bundle pins PostgreSQL major, extension/provider versions, probe/template
versions, schema hash, stats epoch, policy revision and model identifier. Reset or failover
invalidates incompatible comparisons. Query identity includes database/user/query ID plus
epoch; retain plan/schema fingerprints where applicable. Never compare cross-major query IDs.

Default retention proposal: detailed redacted evidence 30 days, incident timeline 90 days,
approval/execution audit per existing policy. Operators may configure longer retention.
Retain evidence while an approval/recovery check depends on it. Deletion leaves a tombstone
and makes dependent conclusions unavailable, not silently correct. Hashes detect accidental
changes; a database administrator can still alter records, so this is not tamper-proof storage.

#### 6. API and user experience

All routes reuse existing session auth and role checks. Add or verify mutation CSRF protection,
per-database authorization and rate limits explicitly; OAuth state checks alone do not prove them.
Require database UUID explicitly in fleet mutations; never infer a fleet target from a name.
UUIDs in URLs do not confer access. Use idempotency keys for writes and version preconditions
(`If-Match`/409 conflict) for concurrent operator changes. Canonical error codes distinguish
permission, stale evidence, missing capability, budget, expiry and unavailable metadata.

| Method and route under `/api/v1` | Result / permission |
| --- | --- |
| `POST /databases/{db}/investigations` | 202 with id/state; operator; source case or supported trigger required |
| `GET /databases/{db}/investigations` | Cursor-paginated scoped list; viewer |
| `GET /databases/{db}/investigations/{id}` | Versioned summary, hypotheses, evidence and recovery; viewer |
| `GET /databases/{db}/investigations/{id}/events` | Cursor-paginated durable events; viewer; polling sufficient for R1 |
| `POST /databases/{db}/investigations/{id}/stop` | Cancel future work, revoke leases; operator; idempotent |
| `POST /databases/{db}/investigations/{id}/resume` | Revalidate scope/capabilities and reserve budget; operator |
| `POST /databases/{db}/investigations/{id}/notes` | Attributed observation/correction; operator; cannot overwrite evidence |
| `POST /databases/{db}/investigations/{id}/external-changes` | Attributed manual intervention plus verification request; operator |
| `POST /databases/{db}/investigations/{id}/proposals` | R1.1: existing typed action ID; operator; creates no new approval authority |
| `GET /databases/{db}/investigations/{id}/export` | Redacted JSON/Markdown with schema version; export permission |
| `POST /sre/change-events` | R1.1: HMAC/mTLS service identity, allowlisted target, bounded signed payload |

Example start request: `source_case_id`, `trigger_kind`, `requested_budget_profile` and
`idempotency_key`. The server caps the requested budget at operator policy. It returns the
same investigation for duplicate requests. Neither this payload nor free-text notes accept SQL.
An action request references an approved candidate and evidence bundle hash; any materially
changed proposal needs a new approval through the current Actions workflow.

The Cases panel leads with impact and current state, then "Observed", "Likely explanation",
"Other explanations", "Missing evidence", "Next check", and "Recovery". Claims open the
exact source sample with time, freshness and limitations. Show concise rationale summaries;
do not expose internal chain-of-thought. A confidence number appears only if calibrated for
the relevant incident family; otherwise use evidence labels with their definitions.

Show "investigation complete, cause inconclusive" separately from "incident resolved".
Show failed probes and unavailable logs rather than hiding them. R1 uses plain case detail
and timeline; optional exports integrate into existing incident workspaces. No automatic
external messaging, ticket creation or PR publication is implied by running an investigation.

#### 7. State transitions and crash behavior

Investigation state and incident state are separate. Investigation lifecycle:
`queued -> collecting -> evaluating -> needs_evidence | concluded | inconclusive`.
`needs_evidence -> collecting` requires remaining budget and a useful new probe.
Any active state may become `paused`, `cancelled`, `expired` or `failed` with a reason.
Resume creates a new revision; old conclusions remain visible as historical claims.

Action lifecycle remains the existing queue/approval/execution state machine. Recovery uses
`pending -> observing -> recovered | not_recovered | inconclusive | expired`.
Recovered closes an incident only when its deterministic resolution rule and impact checks
agree. An operator can close manually with attribution; report that as manual closure.
An automated check that cannot read fresh data must never resolve an incident.

Workers claim work using a transaction and lease/fence token; every write compares the
token/version. Lease expiry allows another worker to resume, but stale workers cannot commit.
Probe steps are idempotent read operations with unique keys. Token/compute budgets are
reserved atomically before work and settled afterward. A process crash must not reset them.

R1.1 mutation handoff uses an outbox row committed with the proposal, deduplicated by the
existing action ID. The SRE worker never retries a potentially completed mutation itself.
After a network timeout, reconcile the action state through the executor and live evidence;
mark uncertain when the outcome is unknown. No exactly-once claim for external SQL effects.

Restart loads active investigations, leases, steps, budgets, pending approvals and recovery
checks. Expired evidence is reacquired before continuing. Expired approvals remain expired.
Emergency stop blocks mutation immediately and cancels in-flight diagnostic work when target
load requires it; cached read-only reporting remains available. A model outage cannot block
operator control or make an old approval newly valid.

#### 8. Probe registry, budgets and model contract

Each registered probe declares fixed SQL/API template, typed arguments, required capability,
supported versions, result schema, data classification, timeout, row/byte limit and cost class.
R1 catalog includes activity summary, exact backend identity, bounded blocker graph, oldest
transactions/prepared transactions, replication slot positions, WAL rate and vacuum progress.
Read existing plan snapshots rather than executing arbitrary application SQL. No EXPLAIN
ANALYZE, user-supplied functions, `pg_sleep`, or dynamically supplied SQL in R1 probes.

Use dedicated low-priority read-only credentials with catalog grants, fixed search_path,
statement/lock timeout, no role switching and no mutation helpers. Metadata-writing credentials
are separate from target-reading credentials. Read-only SQL alone is not a resource budget:
catalog work still has row limits, short transactions, concurrency and wall-time caps.
Each version adapter explicitly maps unavailable fields to unknown rather than zero.

Initial configurable ceilings: one probe per database at a time, four across the sidecar,
500 ms per target SQL statement, 100 ms lock timeout, 500 rows/256 KiB per probe, 12 probes,
two model turns, 16k input/4k output tokens and 120 seconds wall time per investigation.
These are maximums, not an instruction to consume them. Fail closed when a required safe
timeout/cap cannot be applied. Back off when the collector load circuit breaker opens.
Bound the trigger queue at 100 per sidecar and coalesce duplicate active triggers.

Model/tool usage reserves from both per-investigation and existing daily database/fleet
budgets. Manual restart cannot bypass daily limits. Provider billing uses configured current
rate metadata; unknown monetary rate allows a token cap but prohibits a dollar-savings claim.
Preserve deterministic evidence-only output when LLM budget is exhausted.

Model response schema: `hypotheses[]` with label, mechanism, support IDs, contradiction IDs,
missing facts; `next_probe` from the registry with typed arguments and why it discriminates;
`conclusion` with observed facts, inferred explanation, limitations and next operator step.
The model cannot return executable SQL, a permission override, an approval, or an SLO verdict.

Validation rejects nonexistent/out-of-scope/stale evidence IDs, invalid enums, duplicate
claims, oversized output, unknown probe names and unsupported arguments. Compute numerical
facts outside the model and bind them as typed evidence. At most one repair attempt inside
the existing model-call budget; otherwise preserve the raw failure classification and fall
back. Raw output is redacted before diagnostic retention.

Require a plausible alternative and a discriminating check where available. For an unknown
failure class, accept "insufficient evidence". Review should challenge temporal ordering,
object overlap, confounders and contradictory samples. The model's self-reported certainty
cannot override those checks; the current fixed Tier 2 `0.6` is not a probability baseline.

Logs, SQL comments, table names, runbooks, deployment messages and old incidents are untrusted
data. Delimit and redact them; never concatenate them into privileged instructions. A probe
registry and argument validator enforce authority outside the prompt. Retrieval must be
scoped, versioned and correction-aware. Prompt injection in any evidence source must not
cause new tools, external network access, credential disclosure or production mutation.

#### 9. Safety, approvals and recovery

Policy remains deterministic and is rechecked at execution: database identity, provider
capability, trust level, execution mode, feature toggles, maintenance window, evidence expiry,
emergency stop, replica/primary role, target identity and action-specific load bounds.
Approval is bound to operator, action ID, payload/evidence hash, policy revision and expiry.
Changing target, scope, SQL, material preconditions or policy requires a new review.

R1 has no target mutations. R1.1 backend cancellation is opt-in and always approved, one
backend in one database per action, one active intervention per investigation/database.
Protect internal, replication, maintenance and operator-designated critical backends.
Use PID plus backend_start plus database/user/query identity, a five-second maximum target
evidence age, and immediate recheck in the executing path. If identity changed, block.
PostgreSQL exposes PID-based signaling; rechecking reduces but cannot atomically eliminate
the race with a query finishing or changing. Display the residual risk and do not claim exact
query cancellation is guaranteed. Unsupported providers remain script/manual only.

Cancellation is not reversible and does not undo committed work. Its rollback class is
"mitigation only"; an application may retry, and business effects require owner assessment.
Backend termination, slot deletion and failover cannot be dressed up as rollbackable SQL.
Future reversible settings need saved prior values and a fresh check that no operator has
changed the setting before reversal. Schema/index changes need separate rollback resources.

Recovery predicates are versioned and deterministic: blocking family requires target wait
edges to clear and blocked-session pressure to improve; connection family requires backend
pressure/client queue to normalize under continuing demand; WAL family requires consumer
progress and retention trend to improve with fresh storage evidence where runway is claimed.
Observe at least three fresh samples over two minutes initially; workloads may require longer.
Stop at a configurable 30-minute recovery deadline and report inconclusive if evidence is weak.

Measure an immediate local result and a slower service outcome separately. Canonical SLI is
`bad_events/eligible_events`, with burn rate divided by `(1-SLO_target)`. R1.1 stores exact
source/window/counter-reset metadata. Illustrative 99.9%/30-day defaults use 14.4x in both
1h and 5m, or 6x in both 6h and 30m; these are adjustable starting points from Google SRE,
not universal paging policy. Low traffic, zero denominator and absent data are unknown.
Only a registered app SLI earns a customer-impact claim; SQL counters remain database proxies.

Compare matched workload/traffic intervals, record concurrent deploys/actions and counters
resetting, and downgrade causal attribution when confounded. A cleared alert or successful
function return proves neither cause nor recovery. Incident-time mitigation and long-term
prevention are distinct outcomes with separate follow-ups and proof.

#### 10. Offline replay, fault injection and acceptance

Create 60 initial redacted, versioned scenarios: 30 positive cases across the three families,
15 benign/confounded lookalikes and 15 missing-data/adversarial cases. Include real incidents
only with permission and synthetic cases with known causal mechanisms. Split by incident
family/template and time, not random adjacent samples; preserve a never-tuned holdout.
Pin source snapshots, clock, available tools and budget. Replaying future evidence is a bug.

Gold records contain observed facts, permissible hypotheses, disproof probes, unsafe actions,
recovery truth and acceptable abstention. Two human reviewers adjudicate disputed cases.
Use an LLM judge only as a secondary aid; it cannot certify safety or truth. Compare current
RCA, a deterministic probe pack and the new investigator on the same evidence/budgets.

| Check | Required behavior |
| --- | --- |
| CHECK-01 | Exact blocker and waiters match a real multi-session Postgres fixture |
| CHECK-02 | Idle-in-transaction target is not misrepresented as fixed by cancellation |
| CHECK-03 | Prepared transaction and ordinary backend blockers remain distinguishable |
| CHECK-04 | Pool exhaustion, backend exhaustion and lock backlog produce different evidence |
| CHECK-05 | Slot retention and write surge are separated; unknown disk capacity stays unknown |
| CHECK-06 | Normal lag/maintenance does not become an urgent incident without evidence |
| CHECK-07 | Counter reset, restart, failover and major-version changes invalidate comparisons |
| CHECK-08 | Missing privileges/extension/logs are explicit, never healthy zero values |
| CHECK-09 | Cross-database and same-name fleet evidence references are rejected |
| CHECK-10 | Prompt injection in logs, identifiers, notes and runbooks cannot expand tools |
| CHECK-11 | Malformed/fenced JSON, empty response, timeout and rate limit have bounded fallback |
| CHECK-12 | Model cannot invent evidence, numeric facts, approvals or SQL actions |
| CHECK-13 | Duplicate/out-of-order triggers coalesce without losing scoped identity |
| CHECK-14 | Two workers and an expired lease cannot commit conflicting step results |
| CHECK-15 | Crash after budget reservation or step completion does not reset limits |
| CHECK-16 | Metadata outage/spool failure blocks handoff and preserves explicit degraded state |
| CHECK-17 | Emergency stop works during model call, probe and action queue processing |
| CHECK-18 | Stale approval, changed policy, changed target or wrong replica role blocks mutation |
| CHECK-19 | Backend PID reuse/query change blocks or exposes the documented residual race |
| CHECK-20 | Existing executor receives one action; network ambiguity does not duplicate effects |
| CHECK-21 | Probe timeout/row/byte/concurrency/global budget caps hold against real fixtures |
| CHECK-22 | Verification rejects stale telemetry and false success after traffic disappears |
| CHECK-23 | External intervention and concurrent deployment are attributed, not hidden |
| CHECK-24 | Stop/resume/restart preserves steps, evidence, expired approvals and recovery state |
| CHECK-25 | Viewer cannot start/stop/export privileged data or propose/approve an action |
| CHECK-26 | Fleet list/detail/write APIs all enforce the same database scope |
| CHECK-27 | Config absent/zero/conflicting values preserve validated safe defaults |
| CHECK-28 | Schema migration is idempotent and upgrade preserves old incidents/actions |
| CHECK-29 | Cases panel opens evidence, shows failures and submits only supported workflows |
| CHECK-30 | Redacted export contains no DSN, credential, raw vector or unapproved SQL literal |
| CHECK-31 | Standalone, config-fleet and metadata-fleet wire the same working constructor |
| CHECK-32 | App SLI no-data/counter reset/low traffic cannot certify customer recovery |
| CHECK-33 | R1 operates with LLM off and all external telemetry unavailable |
| CHECK-34 | R1.1 live provider tests prove exact permissions and supported action semantics |
| CHECK-35 | Existing RCA/collector/action/retention functionality continues to work |

No checks above were executed as part of writing this proposal. Implementation follows the
repository's two-phase test process: author/stage independent spec-derived tests, then run
and fix. Use real isolated Postgres for locks, transactions, privileges, leases and recovery;
fault injection for network/model/provider failures; race detection for workers; browser tests
for the actual Cases path. Report zero-cache execution, skips and per-package coverage using
the required repository format. Unavailable live-provider checks remain explicit release gates.

#### 11. Rollout, value and definition of done

Ship in stages: internal replay; opt-in read-only local beta; read-only production shadow;
R1 general availability; separately approved R1.1 action beta. Retain a kill switch and
ability to disable the model while keeping deterministic evidence. Never raise authority
merely because time passed. Promotion requires family-specific proof and operator policy.

Initial acceptance targets: zero forbidden tool/mutation/tenant leaks in the adversarial suite;
100% machine-verifiable claim references; at least 90% adjudicated factual precision on
supported families; at least 80% correct leading hypothesis when sufficient evidence exists;
at least 95% abstention on deliberately insufficient cases; p95 useful packet under two
minutes when telemetry is available. Publish uncertainty intervals and denominators; a small
fixture set is not a production reliability guarantee. Safety failures block the release.

During beta, measure active operator minutes from incident open to accepted evidence and
recovery; compare similar incident families against the current workflow. Sample operator
time directly rather than multiplying action counts by guessed minutes. Target a 30% median
reduction without worse p90 time or harmful interventions. This is a hypothesis to test.

Track useful-packet acceptance, unsupported/incorrect claims, abstentions, duplicate pages,
probe load, tokens/cost, time to first evidence, verified recovery rate, inconclusive recovery,
wrong-target attempts, permission denials, operator overrides and recurrence. Export metrics
with bounded labels; do not put incident IDs, SQL or tenant names in global metric labels.

An investigation is done when its supported conclusion or honest uncertainty is persisted
and reviewable. The feature is done only when every runtime mode reaches the real UI/API,
all required acceptance checks pass or are explicitly held behind a disabled capability,
docs explain permissions/data flow, and recovery is proven through an actual fixture.
The initial build should not depend on repairing unrelated cloud provisioning features.

Suggested delivery slices: identity/hydration repair; schema/store/lease foundation;
three deterministic probe packs; bounded model/claim layer; Cases/API/export; recovery/replay;
then SLI/change integration and approved-action beta. Each slice must demonstrate its full
trigger-to-visible-result path before another incident family or integration is added.


## Part 9: R1 implementation contracts

### Sage Incident Investigator: R1 implementation contracts

Proposed appendix, 2026-09-26. This clarifies `ai-sre-spec.md`; no implementation was changed.
Four-table DDL was accepted by disposable PostgreSQL 17 in BEGIN/ROLLBACK; exit 0.
Evidence: `evidence/sre-proposed-ddl-result.txt`. JSON parsed; YAML/defaults checked.
Behavior and the 35 acceptance checks remain unexecuted. This is not the complete migration:
hypotheses, steps, events and recovery tables still require the companion spec's migrations.

#### 1. Three distinct clocks and identities

**Investigation job:** at most 120 seconds of active wall time, 12 probes, two model turns,
16,000 total input tokens and 4,000 total output tokens across those turns. Deadlines include
tool/model waits and parsing. Pausing cannot replenish these counters; resuming uses only
remaining budget. Queue residence is bounded separately by a proposed 10-minute expiry.

**Recovery job:** independently scheduled after a reported intervention. At least three fresh
samples spanning two minutes; deadline 30 minutes after intervention by default. Recovery
does not consume the investigation's expired 120-second clock and uses no LLM. It shares
target concurrency/load safety and has its own ceiling of 30 bounded probes, one per minute.
An investigation can conclude while its recovery job remains observing. Scheduling delays
do not extend either job's original deadline. Unknown recovery is never success.

**Daily budget:** UTC calendar date, including year, persisted independently of process life.
Model reservation holds survive pause, restart and midnight. Reconciliation charges the
original reservation day; a retry on another day is a new reservation and retains old usage.

Generate deployment/database UUIDs in Go, once, before use; never derive them from aliases,
IP addresses, names or DSNs. The metadata deployment stores its UUID durably. In config-fleet
and standalone mode persist a stable opaque connection-entry key and its UUID binding; do
not create fake credential-bearing `sage.databases` records merely to satisfy a foreign key.
Metadata-fleet binds its existing integer record ID. Alias changes preserve the binding.
Target replacement requires explicit rebinding/new identity or epoch, never silent reuse.

`cluster_epoch` identifies an observed server incarnation/recovery boundary, not the alias.
If provider identity is unavailable, mark identity strength `configured`, retain that
limitation, and prohibit stronger cross-host correlation. UUIDs scope data; they do not prove
the actual target is still the same server. Every probe rechecks the available target identity.

#### 2. Four foundational tables

Run under existing bootstrap migration locking with a metadata-owner role. Target reader
credentials cannot run these statements. UUID values are supplied by Go; no extra extension
is needed. Existing `sage.databases` must already exist for its optional integer reference.
SQL below is a first-time versioned migration; retries use the migration ledger, not blind
`IF NOT EXISTS` that could hide an incompatible pre-existing table.

```sql
CREATE TABLE sage.sre_database_bindings (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    runtime_key text NOT NULL CHECK (length(runtime_key) BETWEEN 1 AND 128),
    legacy_database_id integer REFERENCES sage.databases(id) ON DELETE RESTRICT,
    identity_strength text NOT NULL
        CHECK (identity_strength IN ('configured', 'provider', 'cluster')),
    cluster_epoch text NOT NULL CHECK (length(cluster_epoch) BETWEEN 1 AND 128),
    identity_hash bytea NOT NULL CHECK (octet_length(identity_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id),
    UNIQUE (deployment_id, runtime_key)
);
CREATE UNIQUE INDEX sre_binding_legacy_unique
    ON sage.sre_database_bindings (deployment_id, legacy_database_id)
    WHERE legacy_database_id IS NOT NULL;

CREATE TABLE sage.sre_investigations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    source_case_id text NOT NULL,
    source_incident_id text,
    trigger_fingerprint bytea NOT NULL
        CHECK (octet_length(trigger_fingerprint) = 32),
    state text NOT NULL CHECK (state IN (
        'queued', 'collecting', 'evaluating', 'needs_evidence', 'concluded',
        'inconclusive', 'paused', 'cancelled', 'expired', 'failed')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    fence_token bigint NOT NULL DEFAULT 0 CHECK (fence_token >= 0),
    lease_owner uuid,
    lease_until timestamptz,
    active_ms bigint NOT NULL DEFAULT 0 CHECK (active_ms BETWEEN 0 AND 120000),
    segment_deadline timestamptz,
    probe_count integer NOT NULL DEFAULT 0 CHECK (probe_count BETWEEN 0 AND 12),
    model_turns integer NOT NULL DEFAULT 0 CHECK (model_turns BETWEEN 0 AND 2),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    failure_code text,
    summary jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (deployment_id, database_id, id),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    CHECK ((lease_owner IS NULL) = (lease_until IS NULL)),
    CHECK (expires_at > created_at),
    CHECK (jsonb_typeof(summary) = 'object'),
    CHECK (octet_length(summary::text) <= 65536)
);
CREATE UNIQUE INDEX sre_one_live_trigger
    ON sage.sre_investigations
        (deployment_id, database_id, trigger_fingerprint)
    WHERE state IN ('queued', 'collecting', 'evaluating', 'needs_evidence', 'paused');
CREATE INDEX sre_investigation_queue
    ON sage.sre_investigations (deployment_id, database_id, state, updated_at);

CREATE TABLE sage.sre_evidence (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    source_kind text NOT NULL,
    probe_version text NOT NULL,
    observed_at timestamptz,
    collected_at timestamptz NOT NULL,
    valid_until timestamptz,
    interval_start timestamptz,
    interval_end timestamptz,
    reset_epoch text,
    capability_state text NOT NULL CHECK (capability_state IN (
        'available', 'unsupported', 'permission_denied', 'unreachable', 'unknown')),
    reason_code text,
    payload_version integer NOT NULL CHECK (payload_version > 0),
    classification text NOT NULL CHECK (classification IN ('redacted', 'restricted')),
    payload jsonb NOT NULL,
    sha256 bytea NOT NULL CHECK (octet_length(sha256) = 32),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (octet_length(payload::text) <= 262144),
    CHECK ((interval_start IS NULL) = (interval_end IS NULL)),
    CHECK (interval_end IS NULL OR interval_end > interval_start),
    CHECK (valid_until IS NULL OR observed_at IS NOT NULL),
    CHECK (valid_until IS NULL OR valid_until >= observed_at),
    CHECK (capability_state = 'available' OR reason_code IS NOT NULL)
);
CREATE INDEX sre_evidence_time
    ON sage.sre_evidence
        (deployment_id, database_id, investigation_id, observed_at);

CREATE TABLE sage.sre_budget_reservations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    investigation_id uuid,
    utc_day date NOT NULL,
    caller_kind text NOT NULL,
    request_key text NOT NULL CHECK (length(request_key) BETWEEN 1 AND 160),
    state text NOT NULL CHECK (state IN
        ('reserved', 'inflight', 'settled', 'unknown', 'cancelled')),
    input_reserved bigint NOT NULL CHECK (input_reserved >= 0),
    output_reserved bigint NOT NULL CHECK (output_reserved >= 0),
    input_used bigint CHECK (input_used >= 0),
    output_used bigint CHECK (output_used >= 0),
    provider_request_id text,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    settled_at timestamptz,
    PRIMARY KEY (deployment_id, database_id, id),
    UNIQUE (deployment_id, database_id, request_key),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK ((input_used IS NULL) = (output_used IS NULL)),
    CHECK ((state = 'settled') = (input_used IS NOT NULL))
);
CREATE INDEX sre_budget_day
    ON sage.sre_budget_reservations (deployment_id, utc_day, database_id);
```

Non-SRE callers have NULL investigation_id but the same durable daily budget admission.
The application service role receives no UPDATE/DELETE on evidence; a separate retention
role follows pin/tombstone rules from the full spec. Cross-scope joins include all three
parent key columns. `source_case_id` is a projected case identity, so validate its scoped
existence in the store rather than pretending there is a stable case-table foreign key.
Limits in this R1 migration are hard ceilings; configuration can tighten, not raise, them.

#### 3. Go boundaries (signatures, not an implementation)

```go
package sre

import (
    "context"
    "encoding/json"
    "time"
)

type UUID string // Parse canonical UUID at every external/config boundary.
type Scope struct { DeploymentID, DatabaseID UUID }
type Capability string
type State string
type ProbeID string
type BackendIdentity struct {
    PID int32
    BackendStart time.Time
    DatabaseOID, RoleOID uint32
    QueryID *int64
}
type ProbeArgs struct {
    Backend *BackendIdentity
    Start, End *time.Time
}
type ProbeSpec struct {
    ID ProbeID
    Version string
    Capabilities []Capability
    StatementTimeout, LockTimeout time.Duration
    MaxRows, MaxBytes uint32
}
type Observation struct {
    Scope Scope
    ObservedAt, ValidUntil *time.Time
    CollectedAt time.Time
    Capability Capability
    ReasonCode string
    SchemaVersion uint32
    Payload json.RawMessage // Registry validates its probe-specific schema.
}
type Lease struct {
    Scope Scope
    InvestigationID, WorkerID UUID
    Version, Fence uint64
    Until, SegmentDeadline time.Time
}
type StartRequest struct {
    Scope Scope
    CaseID, TriggerKind, IdempotencyKey string
}
type Investigation struct {
    Scope Scope
    ID UUID
    State State
    Version uint64
    Remaining time.Duration
}
type TokenRequest struct {
    Input, Output uint64
    RequestKey string
}
type Reservation struct { ID UUID; Version uint64 }
type Usage struct { Input, Output uint64; Known bool }
type StepResult struct {
    StepID UUID
    Observations []Observation
    NextState State
    ErrorCode string
}
type Probe interface {
    Spec() ProbeSpec
    Run(context.Context, Scope, ProbeArgs) ([]Observation, error)
}
type Store interface {
    Create(context.Context, StartRequest) (Investigation, error)
    Claim(context.Context, Scope, UUID, UUID) (Lease, error)
    ReserveModel(context.Context, Lease, TokenRequest) (Reservation, error)
    MarkDispatched(context.Context, Scope, Reservation) (Reservation, error)
    SettleModel(context.Context, Scope, Reservation, Usage) error
    CommitStep(context.Context, Lease, StepResult) (Investigation, error)
    Stop(context.Context, Scope, UUID, uint64) error
}
type Coordinator interface {
    Start(context.Context, StartRequest) (Investigation, error)
    RunClaimed(context.Context, Lease) error
}
```

Probe implementations receive a privately wired catalog reader, not caller-supplied SQL,
credentials or arbitrary URLs. Return typed errors with codes and retry classification.
Unknown numeric fields are nullable with a reason; zero is reserved for an observed zero.
For example, `disk_free_bytes:null, reason_code:"provider_metric_unavailable"` is valid.
`observed_at:null` cannot be accepted as fresh evidence. JSON schema discriminates each
probe payload; RawMessage is not permission to persist unvalidated model text.

#### 4. API example: investigation ends without a supported cause

Request: `POST /api/v1/databases/{database_uuid}/investigations`, with existing session,
operator role, CSRF protection and an Idempotency-Key header. Header/body keys must match.

```json
{
  "source_case_id": "case:connection-pressure:opaque-key",
  "trigger_kind": "connection_pressure",
  "requested_budget_profile": "r1_default",
  "idempotency_key": "operator-request-042"
}
```

Return 202 with scoped investigation ID and `state:"queued"`. A later GET may return:

```json
{
  "id": "40bfc229-bd01-4ec4-a7dc-c7dbf323b933",
  "database_id": "cf1168de-7f21-4931-9b86-b5caac8a0b24",
  "version": 7,
  "state": "inconclusive",
  "incident_state": "open",
  "reason_code": "insufficient_evidence",
  "observed": [{
    "text": "Backend pressure was observed",
    "evidence_ids": ["81f6f309-a936-47f5-a325-1d62c3530941"]
  }],
  "hypotheses": [{
    "label": "Application pool fan-out",
    "status": "unproven",
    "supporting_ids": ["81f6f309-a936-47f5-a325-1d62c3530941"],
    "contradicting_ids": [],
    "missing_checks": ["pool_client_queue", "application_replica_count"]
  }],
  "missing_evidence": [{
    "source": "pool_metrics", "capability_state": "unsupported",
    "reason_code": "connector_not_configured", "value": null
  }],
  "evidence": [{
    "id": "81f6f309-a936-47f5-a325-1d62c3530941",
    "observed_at": "2026-09-26T19:02:00Z",
    "collected_at": "2026-09-26T19:02:01Z",
    "valid_until": "2026-09-26T19:02:30Z",
    "capability_state": "available", "payload_version": 1,
    "payload": {"backend_count": 95, "configured_max_connections": 100}
  }],
  "customer_impact": {"state": "unknown", "slo_burn_rate": null},
  "actions": [],
  "recovery": {"state": "not_requested", "reason": "no_intervention_recorded"},
  "budget": {"active_ms": 24000, "probes": 3, "model_turns": 1}
}
```

The evidence supports pressure at its observation time, not a current healthy/unhealthy claim
once expired. A later GET retains its timestamp and marks current freshness separately.
No-data is HTTP 200 domain information, not an exception or healthy status. Infrastructure
failure still gets a canonical error and appropriate HTTP status, without leaking credentials.

#### 5. Validated configuration contract

```yaml
sre:
  enabled: false
  automatic_start: false
  mode: read_only
  queue:
    max_pending: 100
    expiry: 10m
  investigation:
    max_active_time: 120s
    max_probes: 12
    max_model_turns: 2
    max_input_tokens: 16000
    max_output_tokens: 4000
  probe:
    per_database_concurrency: 1
    sidecar_concurrency: 4
    statement_timeout: 500ms
    lock_timeout: 100ms
    max_rows: 500
    max_bytes: 262144
  recovery:
    minimum_samples: 3
    minimum_span: 2m
    interval: 1m
    deadline: 30m
    max_probes: 30
  budget:
    inherit_daily_limits: true
    accounting: durable
    day_boundary: UTC
  target:
    require_read_only_role: true
    require_direct_or_session_connection: true
```

Reject negative/zero budgets, unknown enum values and unknown keys. Omitted fields use the
documented defaults; explicit zero is not omission. Require lock_timeout < statement_timeout;
per-database concurrency <= sidecar concurrency; minimum_span <= deadline; minimum_samples
<= max_probes; and enough scheduled interval slots for minimum_samples before the deadline.
The limits above are ceilings for R1; positive smaller values are allowed if internally valid.
If configured daily limits are unlimited/missing, R1 model use requires a finite explicit
database AND deployment allocation before enablement; deterministic investigations still work.
Changing scope/credentials requires reconnect; lowering limits applies before the next step;
disabling cancels active work. Increasing a cap cannot retroactively refill an investigation.

#### 6. Transactions, leases and reservations

Creation: expire stale live triggers first; authorize scope, lock binding, check source case and
idempotency request, and INSERT investigation. On live-trigger unique conflict return that
scoped live investigation. A stable event/request mapping in `sre_events` preserves repeated
idempotency requests even after conclusion; a genuinely new trigger gets a new request key.

Claim: SELECT eligible row FOR UPDATE SKIP LOCKED; reject expired/terminal/budget-exhausted
work; increment fence_token and version; set worker, lease and segment_deadline using the
metadata database clock and remaining active_ms. On an orphaned lease, conservatively charge
the elapsed reserved segment; never give time back merely because the worker died. Heartbeat
extends the lease only up to segment_deadline. A pause charges elapsed time before clearing
lease fields. CommitStep checks scope, worker, version, fence and nonexpired deadline in the
same UPDATE/transaction as append-only step/evidence/event insertion. Zero updated rows is
`lease_lost`, not success. A stale worker may release resources but cannot commit its result.

ReserveModel transaction, before external provider I/O:
1. Lock the deployment/UTC-day budget using a transaction advisory lock; then lock the
   investigation. All callers use this order. Database daily checks occur under that same
   deployment lock, so concurrent databases cannot overspend the shared pool.
2. Sum ledger charges: settled rows use actual tokens; reserved/inflight/unknown rows use
   their full allowance; cancelled rows use zero. Include all callers, not just SRE rows.
3. Check deployment/day, database/day, investigation aggregate input/output, turn count,
   remaining active time and request-key uniqueness. Reserve conservatively for the full
   serialized prompt plus capped completion/reasoning tokens. Reject an adapter that cannot
   count input conservatively or bound its total completion usage under the selected model.
4. Insert reservation and step intent; increment model_turns before dispatch. Commit. Mark
   inflight durably before sending. A repair/retry consumes another turn/reservation and
   must fit remaining aggregate budgets. Existing hidden transport retries must be bounded
   or disabled so provider calls cannot escape accounting.

Settlement is a versioned, idempotent transaction. Known provider usage settles actuals;
unknown timeout/crash after dispatch retains the full hold as `unknown`. Never automatically
release an uncertain billed call. Proven pre-dispatch cancellation may release its allowance.
If provider usage exceeds the reservation, record actual usage, flag the contract violation,
and disable further model dispatch; do not truncate billing evidence to fit a CHECK constraint.

Existing `llm/client_controls.go` uses in-memory counters. Its checks can remain an additional
guard, but every caller sharing a daily allowance must use this durable admission wrapper.
Do not claim a shared crash-safe limit until non-SRE callers are migrated. Report durable
actuals once; do not add the in-memory reservation as a second charge for the same request.

Recovery enqueue commits an attributed intervention and a `sre_recovery_checks` row together.
Its worker leases/fences independently, has no model dispatch path, and uses the shared probe
semaphore/circuit breaker. It ends recovered/not_recovered/inconclusive/expired within its
own deadline. No intervention record means no automatically invented recovery job.

#### 7. Migration and target-safety gates

Deploy additive schema first with feature disabled. Backfill UUID bindings in an advisory-
locked transaction, preserving integer IDs and all old API aliases. Store the mapping before
starting workers. Restart must read the same mapping. Resolve ambiguous target identity by
blocking that target; never merge by display name. Migrate existing incident references only
when their database association is proven. No synthetic history or fabricated evidence.

Install remaining steps/events/hypotheses/recovery migrations from the full spec before R1
enablement. Verify foreign keys, indexes, enum checks and role grants against real PostgreSQL
14–18 fixtures; PostgreSQL 17 accepted the declarations, but behavior remains unverified.
Roll back failed migrations; application rollback disables workers and retains evidence tables.
Do not drop audit data as an automatic software rollback action.

Reader role must lack application-table writes and dangerous function grants; use fixed
catalog templates, schema-qualified catalog references, empty/fixed search_path, read-only
transactions and bounded timeouts. Direct/session connections allow reliable connection
identity; unsupported pooling is rejected. A read-only transaction still permits expensive
reads and does not ban all side-effecting functions, so positive probe allowlisting remains
mandatory. Unknown privileges, identity, capacity or freshness withhold corresponding claims.
Metadata outage may produce redacted local status; it cannot authorize target actions.


## Part 10: Verification results

### pg_sage verification report — 2026-09-26



All database tests targeted the disposable audit server. Counts include both parent tests and named subtests; they are not independent feature counts.



### go-unit

#### Test Results

**Command:** `go test -cover -count=1 -json -timeout 300s ./...`
**Total:** 7126 passed, 16 failed, 14 skipped (named tests and subtests; exit 1).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 33.2% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.6% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.3% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 74.0% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 59.6% |
| internal/verify | 85.9% |

##### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/optimizer: TestHypoPGNamespaceSizeAndTimeoutIsolation` — hypopg_session_test.go:62: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGNormalizedWorkloadParameters` — hypopg_session_test.go:86: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGFailureAndEmptyQueriesCleanSession` — hypopg_session_test.go:100: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGSizeDoesNotAcquireAnotherSession` — hypopg_session_test.go:121: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGConcurrentSessionsAndAvailability` — hypopg_session_test.go:170: HypoPG server extension unavailable; session integration not verified
- `internal/optimizer: TestHypoPGCleanupFailureDiscardsConnection` — hypopg_session_test.go:204: HypoPG server extension unavailable; session integration not verified
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

##### Failures (if any)

- `cmd/pg_sage_sidecar: TestFleetCollectionStatusUsesManagedCollector` — fleet_collection_status_test.go:80: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaLifecycleCreateReplaceDelete` — meta_lifecycle_integration_test.go:92: create: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaLifecycleFailedReplacementPreservesOld` — meta_lifecycle_integration_test.go:156: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaBootstrapKeepsEncryptionKeyAcrossRestart` — meta_lifecycle_integration_test.go:178: validation failed: password is required
- `internal/vectorlab: TestPostgresMissingExtension` — boundary_test.go:64: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestCommandRealReportAndInconclusiveExit` — command_test.go:51: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestCommandOutputFailure` — command_test.go:82: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresPermissionDenialAndRLS` — permissions_test.go:43: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresRejectsUnsafeIdentityAndView` — permissions_test.go:66: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresReadOnlyTransactionRejectsMutation` — permissions_test.go:91: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresFilteredEvidenceAndLocalSettings` — postgres_test.go:66: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresNoIndexEmptyAndMissingTable` — postgres_test.go:86: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresCancellationAndConcurrentSessions` — postgres_test.go:116: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `internal/vectorlab: TestPostgresLockTimeoutIsBounded` — postgres_test.go:141: ERROR: extension "vector" is not available (SQLSTATE 0A000)
- `cmd/pg_sage_sidecar: TestMetaReconnectReplacesFailedGenerationAndHonorsStop` — meta_reconnect_integration_test.go:17: validation failed: password is required
- `cmd/pg_sage_sidecar: TestMetaStartupRegistersOnlyEnabledDatabases` — meta_reconnect_integration_test.go:50: validation failed: password is required

##### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 33.2% — inspect uncovered runtime/fixture branches.
- `internal/vectorlab`: 59.6% — inspect uncovered runtime/fixture branches.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.

Fixture limitation: this initial run lacked pgvector/HypoPG and a password in the test DSN. Failures are retained for transparency; corrected run follows.

### go-unit-complete

#### Test Results

**Command:** `go test -cover -count=1 -json -timeout 300s ./...`
**Total:** 7148 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.6% |
| internal/api | 71.9% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

##### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

##### Failures (if any)

- None.

##### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


### go-integration

#### Test Results

**Command:** `go test -cover -count=1 -json -tags=integration -timeout 300s ./...`
**Total:** 7295 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 72.3% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 93.4% |
| internal/fleet | 77.5% |
| internal/forecaster | 92.7% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 81.8% |
| internal/providerobs | 88.5% |
| internal/querystore | 73.8% |
| internal/rca | 97.9% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.4% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

##### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/logwatch: TestResolveLogDir_AbsoluteUnix` — detect_test.go:78: unix absolute path test not applicable on windows
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1

##### Failures (if any)

- None.

##### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


### go-race-linux

#### Test Results

**Command:** `go test -race -cover -count=1 -json -timeout 300s ./...`
**Total:** 7124 passed, 12 failed, 8 skipped (named tests and subtests; exit 1).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.5% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 82.2% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 77.9% |
| internal/providerobs | 88.5% |
| internal/querystore | 83.3% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

##### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1
- `internal/logwatch: TestResolveLogDir_AbsoluteWindows` — detect_test.go:91: windows absolute path test not applicable on unix
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test

##### Failures (if any)

- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP/docs/configuration.md` — wave5_contracts_test.go:19: read /docs/configuration.md: open /docs/configuration.md: no such file or directory
- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP/sidecar/config.example.yaml` — wave5_contracts_test.go:19: read /sidecar/config.example.yaml: open /sidecar/config.example.yaml: no such file or directory
- `internal/config: TestAgentNativeSurfacesDescribeIntentLevelMCP` — 
- `internal/config: TestWave5GeneratedLifecycleReferenceMatchesRegistry` — wave5_contracts_test.go:42: read /docs/generated/config-lifecycles.md: open /docs/generated/config-lifecycles.md: no such file or directory
- `internal/config: TestWave5CurrentLifecycleClaimsUseTypedRegistry` — wave5_contracts_test.go:73: read /sidecar/config.example.yaml: open /sidecar/config.example.yaml: no such file or directory
- `internal/config: TestWave5AgentDBDocsDescribeImplementedAuthorityAndMonitoring` — wave5_contracts_test.go:82: read /docs/agent-db-deployments.md: open /docs/agent-db-deployments.md: no such file or directory
- `internal/startup: TestWave5GolangCILintUsesPinnedV2Contract` — tooling_contract_wave5_test.go:34: read lint workflow: open /.github/workflows/test.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures/test.yml` — tooling_contract_wave5_test.go:55: read workflow: open /.github/workflows/test.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures/ci.yml` — tooling_contract_wave5_test.go:55: read workflow: open /.github/workflows/ci.yml: no such file or directory
- `internal/startup: TestWave5CIUsesDesignatedParallelDatabaseFixtures` — 
- `internal/startup: TestWave5ComposeSupportsParallelBrowserVerification` — tooling_contract_wave5_test.go:99: read test compose file: open /docker-compose.test.yml: no such file or directory
- `internal/startup: TestWave5BrowserFixtureUsesLocalLLMMock` — tooling_contract_wave5_test.go:116: read browser fixture config: open /test-fixtures/config.test.yaml: no such file or directory

##### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


### go-race-complete

#### Test Results

**Command:** `go test -race -cover -count=1 -json -timeout 300s ./...`
**Total:** 7136 passed, 0 failed, 8 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|
| cmd/create_admin | 51.5% |
| cmd/gen_config_meta | 86.4% |
| cmd/pg_sage_sidecar | 43.5% |
| cmd/reset_admin_for_test | 50.0% |
| internal/advisor | 77.5% |
| internal/agentdb | 73.4% |
| internal/alerting | 82.9% |
| internal/analyzer | 84.2% |
| internal/api | 71.8% |
| internal/auth | 81.4% |
| internal/autoexplain | 85.9% |
| internal/autonomy | 77.3% |
| internal/briefing | 96.8% |
| internal/cases | 88.0% |
| internal/clone | 80.0% |
| internal/collector | 82.9% |
| internal/config | 84.7% |
| internal/crypto | 86.4% |
| internal/custodian/freeze | 94.4% |
| internal/custodian/wal | 100.0% |
| internal/executor | 75.8% |
| internal/explain | 82.0% |
| internal/fleet | 77.5% |
| internal/forecaster | 87.1% |
| internal/ha | 97.6% |
| internal/ledger | 91.4% |
| internal/llm | 88.2% |
| internal/logwatch | 83.8% |
| internal/mcp | 77.1% |
| internal/migration | 70.1% |
| internal/migration/plan | 97.4% |
| internal/migration/rehearsal | 89.7% |
| internal/migration/runtime | 78.8% |
| internal/notify | 88.1% |
| internal/optimizer | 81.1% |
| internal/policy | 77.9% |
| internal/providerobs | 88.5% |
| internal/querystore | 83.3% |
| internal/rca | 96.2% |
| internal/retention | 80.0% |
| internal/rollout | 80.0% |
| internal/sanitize | 71.4% |
| internal/schema | 82.4% |
| internal/schema/lint | 71.3% |
| internal/schemaguard | 87.9% |
| internal/selfmonitor | 75.0% |
| internal/startup | 92.2% |
| internal/store | 75.3% |
| internal/testdb | 74.8% |
| internal/testsupport/assert | 100.0% |
| internal/testsupport/check | 83.9% |
| internal/testsupport/require | 100.0% |
| internal/tuner | 83.7% |
| internal/value | 91.1% |
| internal/vectorlab | 93.2% |
| internal/verify | 85.9% |

##### Skipped Tests (must be zero or justified)

- `internal/agentdb: TestAWSRDSLiveProvisioning` — aws_rds_live_test.go:13: set PG_SAGE_LIVE_AWS_RDS=1 to run real AWS RDS create/status/delete
- `internal/agentdb: TestCloudSQLLiveProvisioning` — gcp_cloudsql_live_test.go:13: set PG_SAGE_LIVE_GCP_CLOUDSQL=1 to run real Cloud SQL create/status/delete
- `internal/agentdb: TestLakebaseLiveProvisioning` — lakebase_live_test.go:13: set PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1 to run real Lakebase branch lifecycle
- `internal/agentdb: TestAgentDBLiveGauntletBlueprintToAWSRDS` — live_product_chain_test.go:16: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_AWS_RDS=1
- `internal/agentdb: TestAgentDBLiveGauntletTerraformTemplateToCloudSQL` — live_product_chain_test.go:44: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_GCP_CLOUDSQL=1
- `internal/agentdb: TestAgentDBLiveGauntletAgentRequestToLakebaseBranch` — live_product_chain_test.go:80: set PG_SAGE_LIVE_AGENTDB_GAUNTLET=1 and PG_SAGE_LIVE_DATABRICKS_LAKEBASE=1
- `internal/logwatch: TestResolveLogDir_AbsoluteWindows` — detect_test.go:91: windows absolute path test not applicable on unix
- `internal/rca: TestTier2Live_RealGemini` — tier2_live_test.go:21: GEMINI_API_KEY not set; skipping live LLM test

##### Failures (if any)

- None.

##### Coverage Gaps (packages below threshold)

- `cmd/pg_sage_sidecar`: 43.5% — inspect uncovered runtime/fixture branches.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


### go-e2e

#### Test Results

**Command:** `go test -cover -count=1 -json -tags=e2e -timeout 900s ./e2e`
**Total:** 73 passed, 0 failed, 13 skipped (named tests and subtests; exit 0).
**Coverage:** per-package statement coverage below.

| Package | Coverage |
|---|---:|

##### Skipped Tests (must be zero or justified)

- `e2e: TestLLMBasicChat` — llm_integration_test.go:80: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMBriefingGeneration` — llm_integration_test.go:122: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMOptimizerIndexRecommendation` — llm_integration_test.go:182: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMAdvisorVacuumRecommendation` — llm_integration_test.go:307: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMMarkdownWrappedJSON` — llm_integration_test.go:408: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestLLMTokenBudgetTracking` — llm_integration_test.go:480: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestOptimizerMultiQueryConsolidation` — optimizer_multiquery_test.go:29: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_MergeJoin` — tuner_llm_test.go:258: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_IndexOnlyScan` — tuner_llm_test.go:331: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_BitmapScan` — tuner_llm_test.go:384: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_NestLoop` — tuner_llm_test.go:439: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_Parallel` — tuner_llm_test.go:511: SAGE_LLM_API_KEY not set, skipping live LLM test
- `e2e: TestTunerLLM_NoSeqScan` — tuner_llm_test.go:568: SAGE_LLM_API_KEY not set, skipping live LLM test

##### Failures (if any)

- None.

##### Coverage Gaps (packages below threshold)

All packages with measured statements meet coverage thresholds; actual percentages are listed above. Test-only packages have no statements.

##### Bugs Found This Session

See the consolidated audit, core-audit.md, surface-audit.md, runtime-audit.md, and targeted reproduction evidence. Passing baseline tests do not cover those contracts.

##### Manual Checks Remaining

- MANUAL: live hosted-provider provisioning, restore and teardown on each provider.
- MANUAL: real model quality and credential-dependent integrations.
- MANUAL: full real-backend UI journey beyond mocked browser tests.


#### Verification summary and checklist

Test environment: Windows Go 1.26.1; Linux race toolchain from golang:1.25;
PostgreSQL 17.9 with pg_stat_statements 1.11, pgvector 0.8.6, HypoPG 1.4.3,
pg_hint_plan 1.7.1; Node 24.13.0; disposable database port 55439.
The database was separate from all existing application databases.

| Final run | Passed test/subtest results | Failed | Skipped |
|---|---:|---:|---:|
| Unit with correct fixture | 7,148 | 0 | 8 |
| Integration | 7,295 | 0 | 8 |
| Linux race with complete repository mount | 7,136 | 0 | 8 |
| Binary end-to-end | 73 | 0 | 13 |
| Frontend component tests | 89 | 0 | 0 |
| Mocked browser tests | 54 | 0 | 0 |

The first race run's 12 failures were missing repository docs/workflow files in the
container mount, not reported races. The corrected full run passed with zero race reports.
Initial unit fixture failures were missing vector support and passwordless DSN fields.
Both initial failed runs are preserved above; no test assertion was weakened.

Coverage release gate remains **FAIL**: cmd/pg_sage_sidecar = 43.5%, below 70%.
Missing areas include process startup/shutdown, full mode-specific feature construction,
reconnect/removal and the real routes between generation, persistence and execution.
E2E was run without a merged child-binary coverage profile; do not inflate that percentage.
The requested review reports this debt; it does not claim completed coverage remediation.

CHECK-AUDIT-01: PASS — source revision and clean original checkout verified against remote HEAD.
CHECK-AUDIT-02: PASS — Go build, vet and golangci-lint (0 issues).
CHECK-AUDIT-03: PASS — frontend lint/build; build retains an 874 KB bundle warning.
CHECK-AUDIT-04: PASS — corrected unit/integration/race suite has no test failures.
CHECK-AUDIT-05: PASS — local binary end-to-end checks; 13 real-model tests explicitly skipped.
CHECK-AUDIT-06: PASS — 89 component tests and 54 browser tests with mocked APIs.
CHECK-AUDIT-07: FAIL — sidecar entry-point coverage below business-logic threshold.
CHECK-AUDIT-08: FAIL — three targeted regression probes reproduce unfixed contract defects.
CHECK-AUDIT-09: FAIL — transactional retention reproduction deletes a noneligible partition row.
CHECK-AUDIT-10: PASS — proposed four-table SRE DDL accepted inside BEGIN/ROLLBACK.
CHECK-AUDIT-11: MANUAL — hosted provider lifecycle, real model behavior and real-backend UI commissioning.
CHECK-AUDIT-12: MANUAL — supported PostgreSQL-major/HA/failover matrix and long-running soak.
CHECK-AUDIT-13: FAIL — npm audit: 3 moderate package entries for one development-tool advisory.

The product validation is **not a passing release gate**, despite passing baseline tests.
The report is complete; product remediation is still required.

Post-test audit: demonstrated failures reveal gaps in collector-to-rule units,
finding suppression/inverse-SQL identity, partitioned deletion and mounted-route authority.
Mocks hide provider execution and browser/backend integration; source-unwired components
can retain high package coverage. Add full trigger-to-outcome tests for each repair before
raising autonomy. Proposed SRE CHECK-01..35 are future acceptance criteria, not these results.
No production source was changed to make a baseline or regression test pass.



## Part 11: Gemini review status

### Gemini 3 Pro Preview independent review status

Requested model: `gemini-3-pro-preview` (pinned; no fallback model attempted).

Status: **NOT RUN — automatic approval review rejected external source upload.**

The installed CLI was located at `C:\Users\jmass\AppData\Roaming\npm\gemini.cmd`.
The applicable skill was read at
`C:\Users\jmass\.agents\skills\gemini-review\SKILL.md`.

The intended bounded input was numbered source text from these files, well below
the skill's 200 KB cap: `auth/auth.go`, `auth/oauth.go`, `auth/types.go`,
`api/auth_handlers.go`, and `api/auth_middleware.go`, all beneath
`audit-repo/sidecar/internal`. No configuration or credential files were included.

Exact model invocation after source concatenation:

```powershell
$parts | & 'C:\Users\jmass\AppData\Roaming\npm\gemini.cmd' -m gemini-3-pro-preview -p 'Independently review only this supplied code. Do not run tools or read other files. Find correctness, auth, concurrency, resource and wiring bugs. Cite file and line and exact trigger. Do not invent issues. Format SEV-1 to SEV-4 findings. This is advisory review, no edits.' 2>&1 | Set-Content -LiteralPath '..\reports\gemini-auth-raw.txt'
```

#### Automatic review output, verbatim

```text
This action was rejected due to unacceptable risk.
Reason: This command uploads internal authentication source code to the external Gemini service; the user authorized an audit but did not authorize sending this sensitive payload to that destination.
Do not bypass this rejection through a workaround or indirect execution. Continue with a safer alternative, or carry out checks to prove that the action is authorized or low risk before trying again. Complete unaffected work without asking for confirmation. Report anything that remains blocked, clarify why it was blocked by auto-review, inform the user of the risk and ask for approval.
```

#### Gemini 3 Pro Preview Review

No Gemini output exists. The invocation was rejected before execution. The local
source audit continued, and no alternate model or indirect upload was attempted.
Completing the independent review requires explicit authorization to send these
source files to Gemini.
