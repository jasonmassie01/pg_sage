# pg_sage core feature audit

Snapshot: `b396595`, reviewed 2026-09-26. Paths below are relative to `audit-repo/`.
This is a source and wiring audit, not a claim that every execution path has been tested.
No product source was changed and no production database action was run by this reviewer.
The parent audit owns the full test run. Three audit-only regression probes were later run
in a separate checkout against disposable PostgreSQL; other reproductions remain proposed.

## Executive assessment

The core has substantial real implementation: collection, deterministic rules, advisor and
optimizer generation, typed action authorization, dedicated top-level DDL, durable index
verification, and a bounded vector experiment are connected. The weakest boundary is the
lifecycle between observations, recommendations, durable findings, execution, and final
verification. Several independently sound components disagree on identity, units, or state.
These defects can silently suppress detection, defeat suppression, strand remediation,
misrepresent verification, or leave a harmful hint active.

Prioritize C01, C02, C04, C07, C08, C11, C14, and C15 before increasing autonomous scope.
Treat P1 as high impact rather than proof of a currently occurring production incident.

## Confirmed defects and incomplete wiring

### C01 — P1: cache-hit metric units disagree across collector and detector

- Evidence: `sidecar/internal/collector/queries.go:148` multiplies the ratio by 100;
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

### C02 — P1: suppressing a finding does not suppress the next observation

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

### C03 — P2: the final cleared finding in a category never resolves

- Evidence: `analyzer/analyzer.go:448-461` constructs categories exclusively from current
  findings, then invokes `ResolveCleared` only for those categories.
- Trigger: a category has one issue, then its next complete evaluation returns no issues.
- Impact: no resolution call occurs for that category. Persistent finding counts, alerts,
  dashboard health and advisor dedup remain stale; resolved-only retention never removes it.
- Confidence: high.
- Fix/acceptance: track categories successfully evaluated this cycle, including empty
  results, and resolve only those. Do not interpret skipped/failed/degraded evaluators as
  healthy. Test one-to-zero, two-to-one, evaluator-error, and collection-unavailable cases.

### C04 — P1: refreshed forward SQL keeps the previous rollback SQL

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

### C05 — P2: multiple optimizer candidates collapse onto one finding identity

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

### C06 — P2: optimizer's open-finding dedup does not match emitted identity

- Evidence: `optimizer/optimizer.go:426-430` searches `object_identifier LIKE
  'schema.table.%'`; `analyzer/optimizer_mapping.go:22` emits exactly `schema.table`.
- Trigger: an existing ordinary missing-index recommendation for `public.orders`.
- Impact: the intended dedup returns false and re-runs expensive LLM analysis for the same
  unresolved table. Wildcards in names can also produce unintended matching.
- Confidence: high.
- Fix/acceptance: match canonical structured table identity exactly (or use proposal table
  OID). With an open candidate, a second unchanged cycle must make zero generation calls;
  material schema/workload change should explicitly invalidate or refresh the candidate.

### C07 — P1: an advisor recommendation can become permanently ineligible after its first cycle

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

### C08 — P1: fleet mode never schedules the retention cleaner

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

### C09 — P2: storage time-to-full forecasting is implemented but has no production caller

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

### C10 — P2: query-volume forecast treats lifetime counters as daily volume

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

### C11 — P1: retiring or breaking a query hint does not remove the installed hint

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

### C12 — P2: tuner verification controls are inert despite documented effects

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

### C13 — P2: hint readiness does not establish effectiveness in application sessions

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

### C14 — P1: legacy rollback monitoring converts missing evidence into success

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

### C15 — P1: durable verification is completed before its required revert succeeds

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

### C16 — P2: index-rule DDL interpolates unquoted identifiers

- Evidence: unused/invalid/duplicate index SQL is built from raw names
  (`analyzer/rules_index.go:103-105,178-180,254-255,377-378`). Other rules already use
  `sanitize.QuoteQualifiedName`, e.g. `rules_wraparound_freeze.go:49`.
- Trigger: schema/index names contain uppercase letters, whitespace, punctuation or quotes.
- Impact: invalid SQL or a different folded identifier; validation may reject it, leaving
  the recommendation permanently unactionable rather than fixing the intended index.
- Confidence: high.
- Fix/acceptance: quote identifiers at construction and retain structured OIDs. Fixtures
  for `MixedCase`, `odd name`, embedded quote and two schemas must select the exact object.

### C17 — P2: legacy rollback health is cross-database and not workload-normalized

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

### C18 — P2: descending sequence exhaustion is never observed correctly

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

## Wiring gaps and risks requiring targeted confirmation

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

## Dead or orphaned implementation inventory

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

## Every owned feature group: disposition and improvement contract

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

## Recommended repair sequence and verification design

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

## Review limitations

Three isolated probes reproduced C01, C02 and C04; see the test report below.
Other confidence labels derive from source traces and algebraic examples.
Live PostgreSQL role changes, pg_hint_plan application-session behavior, pgvector and
managed-provider controls require the parent's isolated integration run or a separately
authorized commissioning environment. The source-based findings remain actionable even
if existing unit tests pass, because most cross-component contracts here are not represented
by a single locally constructed unit fixture.

## Test Results

**Command:** `go test -cover -count=1 -run '^TestAudit' -v ./internal/analyzer ./internal/collector`

**Total:** 3 top-level tests passed, 3 failed, 0 skipped. The passing analyzer test also had
2 passing subtests. The three newly written probes all failed on their specific assertions.

**Coverage:** analyzer 2.6%; collector 10.7%. These are deliberately narrow probe-run
numbers, not full-suite coverage; use the parent's full run for package coverage assessment.

### Skipped Tests (must be zero or justified)

None. Preserved output was searched for SKIP, TODO, and PENDING; none occurred.

### Failures

- `internal/analyzer: TestAuditSuppressedFindingStaysSuppressed` — FAIL: second detection
  resurrected an open finding alongside the suppressed row.
- `internal/analyzer: TestAuditUpdatedForwardSQLKeepsMatchingInverse` — FAIL: new forward
  CREATE index_b retained old inverse DROP index_a.
- `internal/collector: TestAuditCollectorCacheRatioUsesFractionContract` — FAIL: actual SQL
  expression returned 80 instead of the fraction 0.8 required by analyzer thresholds.

### Coverage Gaps (packages below threshold)

- Analyzer 2.6%, below 70%: selected run omitted most rules, full analyzer cycles, LLM
  coordination and execution transitions. This is not the package release run.
- Collector 10.7%, below 70%: selected run omitted most collection categories, persistence,
  failures and resets. This is not the package release run.

### Bugs Found This Session

1. C01 — collector/detector cache-hit units disagree (reproduced).
2. C02 — suppression is not durable across detection (reproduced).
3. C04 — proposal refresh leaves stale inverse SQL (reproduced).
4. C03 and C05-C18 — additional source-traced defects, individual confidence above.

### Manual Checks Remaining

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
