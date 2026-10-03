# perf-selfexcl report (v1.8.3 performance fix phase)

Branch `claude/perf-selfexcl` (from `claude/perf-int` 9a04bd50, merged with `claude/perf-int`
f46501da before the final runs). Scope: close every gap in pg_sage's exclusion of its own
statements and sessions from its analysis (v1.8.3 stops hiding pg_sage from
pg_stat_statements), the withheld-admission counting bug, a bounded `/value`, the config key
counts, the web bundle, and (coordinator addition) the five remaining perf gate offenders.

## What was built

### 1. Self-exclusion sweep (read-site table below)

The shared helpers are perf-api's `selfmonitor.StatementExclusionSQL` (statement text: the
`pg_sage` tag or a `sage.` reference) and `selfmonitor.ActivityExclusionSQL` (session:
application_name). Added `selfmonitor.IsApplicationName`, the Go twin of the activity
predicate (same ILIKE semantics, `_` = any character), for log-derived sources. Four inline
copies of the statement predicate (collector top statements, tuner candidates, auto_explain
candidates, the JSONB lint) now use the helper.

Fixed reads (each with a test that failed before, see Before / after):

- Collector (`internal/collector/queries_sessions.go`, new): system stats active_backends and
  idle_in_transaction, the locks snapshot, connection states and churn, and the load circuit
  breaker leave pg_sage out. active_backends and churn are now per database (fleet mode).
- Analyzer lock chains: a pg_sage waiter starts no chain.
- Executor: the write-latency regression check (`rollback_eval.go`), before-state
  active_backends, runaway blocker counts.
- Migration DDL risk: active queries and pending locks on the table.
- Tuner system context and the briefing's `active` count.
- auto_explain plans from provider logs: rejected by session name or statement tag
  (`autoexplain.ErrSelfStatement`, rejection reason `pg_sage_self`).
- SRE probes, all now v2: lock_chains, lock_graph, long_transactions, lwlock_waits,
  temp_spill_statements (perf-api's known gap: it read `pg_stat_statements(false)`, so it
  could not filter; it now reads the text, never returns it, and only on a temp-file incident).

The DB-backed proof is a shared workload, `internal/testsupport/selfload`: the same holder
(idle in transaction with an ACCESS EXCLUSIVE lock), waiter (queued behind the application's
lock) and sleeper run from application sessions and from sessions configured exactly like
pg_sage's pools (`selfmonitor.ConfigurePool`: application_name pg_sage, every statement
tagged). Each analysis input is asserted to count the application's sessions and statements
only (exact counts where the input is per database; minimum-of-8-samples deltas where it is
server-wide).

### 2. Withheld admissions (`internal/executor/load_admission_record.go`)

`recordWithheldAdmission` now returns `(bool, error)`; `admitIndexBuild` logs a failure with
the finding key, reason and database and does not log the "withheld" line. A lost connection
(SQLSTATE 57P0x, class 08, EOF, closed conn) is retried, at most the pool's size times.

### 3. Bounded reads and HOT updates (perf gate)

One new idempotent migration, `internal/schema/selfexcl_index_migration.go` (decision-ledger
loop: catalog checked first, INVALID rebuilt, built under the bootstrap lock; retired indexes
dropped only when `pg_indexes` still has them), registered in `bootstrap.go`:

- `idx_action_log_value_credit (executed_at) INCLUDE (action_type, toil_minutes_saved) WHERE
  outcome = 'success' AND toil_minutes_saved IS NOT NULL`: `/value` (`value/read.go`) now
  sums by day and type in SQL over it; window bounds are `COALESCE($n, ±infinity)` ranges.
- `idx_action_log_drop_index (executed_at) WHERE action_type = 'drop_index'`: the
  app-managed-index check (the gate's last action_log offender).
- `idx_action_log_rolled_back (measured_at) WHERE outcome = 'rolled_back'`: RCA's rollback
  history read every rolled-back action (6,667 rows a call in the fixture).
- `idx_sre_investigations_queue (deployment_id, database_id, state, created_at)` replaces
  `sre_investigation_queue` (..., updated_at); `sre_investigation_retention` (..., updated_at)
  is retired. Their CREATE statements were removed from the old SRE migrations. Investigation
  retention (`sre/postgres_retention.go`) is a range of `idx_sre_investigations_created`
  (updated_at >= created_at), oldest created first.
- The actions list's capped total walks `idx_action_log_time` (`ORDER BY executed_at DESC`).
- Retention: the decision purges' verification check and the verification purge's
  credited-action check are `OFFSET 0` subqueries, i.e. correlated index probes, not hash
  anti joins over the whole table.

### 4. Web (`sidecar/web`)

`src/lib/listPaging.js` (`formatTotal`, `withCursor`, `useCursorPages`) and
`src/components/LoadMore.jsx`. Findings and executed Actions show "1000+" when
`total_capped` and page with `next_cursor` ("Load more"; a failed page shows its error).
Rebuilt `internal/api/dist`; `node_modules` deleted.

### 5. Config key counts

`TestConfigConsistency_AllowedKeyCount` and `_ConfigToMapKeyCount` (116) pass on the merged
tree (PG17 full run). The api package has no key-count tripwire on this tree.

## Read-site table

Verdict rule (product decision 1): pg_sage is excluded where it would be judged as workload
(statements to tune, sessions counted as load, leaks, waiters, long transactions, spills); it
is kept where it is the cause of a resource effect on the application (lock blocker, xmin
holder, temp file holder, connection slots), because that cost is real and visible by
application_name, and the action layer already protects its sessions.

| # | Read site | Source | Feeds | Verdict |
|---|---|---|---|---|
| 1 | `collector/queries.go` queryStatsFrom (top statements) | pgss | optimizer, analyzer slow-query rules, query store, SLO latency proxy, forecaster, fingerprints | Excluded already (perf-api); now uses `StatementExclusionSQL` instead of an inline copy |
| 2 | `collector/queries_sessions.go` system stats: active_backends | activity | forecaster, tuner prompt, advisor prompt, causal model | **FIXED**: pg_sage excluded; now scoped to this database (fleet) |
| 3 | same: idle_in_transaction | activity | RCA idle-in-tx tree | **FIXED**: pg_sage excluded |
| 4 | same: total_backends | activity | RCA connections_high, advisor max_connections floor, forecaster | Kept: capacity (slots are real; perf-api decision 3) |
| 5 | same: cache hit, deadlocks, blk times | pg_stat_database | rules, RCA | Database-wide counters; not attributable per session; accepted |
| 6 | `collector` locksSQL (snapshot locks) | locks+activity | RCA vacuum-blocked tree, lock rules | **FIXED**: pg_sage sessions' locks excluded |
| 7 | `collector` loadRatioSQL | activity | collector circuit breaker | **FIXED**: pg_sage excluded (cluster-wide kept) |
| 8 | `collector/queries_config.go` connection states | activity | advisor connection prompt | **FIXED** |
| 9 | same: connection churn | activity | RCA connection storm, advisor | **FIXED**: excluded and scoped to this database |
| 10 | `collector` stats epoch, pgss.max, block-time column probe | catalog | reset detection | No session/statement data; exempt |
| 11 | `analyzer/analyzer_checks.go` connection leaks | activity | connection_leak finding | Excluded already (perf-api) |
| 12 | `analyzer/clone_activity.go` sessions/locks | locks+activity | clone family | Excluded already (perf-api) |
| 13 | `analyzer/rules_lockchain.go` lockChainQuery | activity+blocking pids | lock_chain finding | **FIXED**: pg_sage waiters start no chain; pg_sage as blocker kept (safe process, never kill SQL) |
| 14 | `analyzer/rules_workmem_promotion.go` | pgss join sage.query_hints | work_mem promotion | Driven by pg_sage's hints for app queries (tuner excludes self); exempt |
| 15 | `analyzer/index_builds.go` | pg_stat_progress_create_index | suppress invalid-index findings | Must include pg_sage's own builds; inclusive by design |
| 16 | `analyzer` statsEpoch, xid diagnostic text | pg_stat_database / text | epochs, operator diagnostic | Exempt |
| 17 | `analyzer/rules_system.go` pgss capacity | snapshot queries | stat_statements_pressure | Capacity (pg_sage entries occupy slots); uses collector count (noted) |
| 18 | `autonomy/schema_statements.go` | pgss | schema guard related statements | Excluded already (perf-api) |
| 19 | `autonomy/schema_family_postgres.go` | locks+activity | schema family in-use | Excluded already (perf-api) |
| 20 | `autonomy/freeze_postgres.go` oldest xmin blocker | activity | freeze custodian cancel target | Excluded already (application_name filter) |
| 21 | `optimizer/self_queries.go` | snapshot queries | optimizer, plan capture | Excluded already (perf-api) |
| 22 | `tuner/tuner.go` candidateSQL | pgss | hint candidates | Excluded already; now uses the shared helper |
| 23 | `tuner/context.go` active backends | activity | work_mem hint prompt | **FIXED** |
| 24 | `tuner/revalidate.go` by queryid | pgss | hint revalidation | Lookup of pg_sage's hint targets by id; exempt |
| 25 | `autoexplain/collector.go` candidateSQL | pgss | on-demand plan capture | Excluded already; now uses the shared helper |
| 26 | `autoexplain/provider_log.go` + `providerobs/log_sink.go` | auto_explain log plans | explain cache, plan regressions | **FIXED**: pg_sage session or tagged text rejected (`pg_sage_self`) |
| 27 | `logwatch/classifier.go` | server log | RCA log signals | Excluded already (application_name pg_sage; deadlocks kept, flagged self-inflicted) |
| 28 | `executor/rollback_eval.go` writeLatencySQL | pgss | post-action regression -> rollback | **FIXED** (real bug: `INSERT /* pg_sage */ ...` matched `INSERT%`) |
| 29 | `executor/rollback_eval.go` cacheHitRatioSQL, config_outcome temp files | pg_stat_database | regression, config outcome | Database-wide counters; accepted |
| 30 | `executor/action_record.go` active_backends | activity | before_state evidence | **FIXED** |
| 31 | `executor/runaway_detector.go` active queries | activity | runaway policy | Excluded already in Go (isSafeRunawayProcess) |
| 32 | `executor/runaway_detector.go` blocker counts | activity+blocking pids | runaway escalation | **FIXED**: pg_sage waiters not counted |
| 33 | `executor` cancel/terminate/signal guards, custodian xmin verify, unused_evidence epoch | activity / pg_stat_database | action identity checks | Identity/verification by pid; protected already; exempt |
| 34 | `migration/detector.go` DDL activity | activity | DDL risk findings | Excluded already |
| 35 | `migration/risk_queries.go` active queries | activity | DDL risk score | **FIXED** |
| 36 | `migration/risk_queries.go` pending locks | locks | DDL risk score | **FIXED**: pg_sage waiters excluded |
| 37 | `briefing/briefing.go` active | activity | LLM briefing | **FIXED**; `connections` kept (capacity) |
| 38 | `cases/incident_projector.go`, `cases/vacuum_autopilot.go`, `rca/trees.go`, `rca/log_trees.go` | SQL text | operator diagnostics in cases/incidents | Text shown to the operator, not executed by analysis; exempt |
| 39 | `rca/stale.go` live backends by pid | activity | incident staleness | Identity lookup; exempt |
| 40 | `sre/probes` lock_chains | activity+blocking pids | SRE evidence | **FIXED** (v2) |
| 41 | `sre/probes` lock_graph | activity+locks | SRE hypotheses, cancel target derivation | **FIXED** (v2): pg_sage waiter edges could lead to cancelling an app session |
| 42 | `sre/probes` long_transactions | activity | SRE lock evidence | **FIXED** (v2) |
| 43 | `sre/probes` lwlock_waits | activity | causal LWLock model | **FIXED** (v2) |
| 44 | `sre/probes` temp_spill_statements | pgss(false) | causal temp model | **FIXED** (v2): known gap from perf-api; text now read (not returned) to filter |
| 45 | `sre/probes` xmin_horizon, temp_file_holders, standby longest query, vacuum progress | activity | causal causes | Kept: pg_sage as holder of a resource is a real cause, labeled, protected from actions |
| 46 | `sre/probes` connection_saturation | activity | connection family | Capacity, grouped by application_name; kept |
| 47 | `sre/probes` signal_target, recovery_sample, backend_identity | activity | approved cancel | Identity; recovery_sample excluded already |
| 48 | `sre/slo/proxy.go` connections | activity | SLO connection proxy | Capacity (perf-api decision 3) |
| 49 | `sre/slo/proxy_errors.go`, `verify/io_*`, `sre/changefeed` | pg_stat_database / pgss_info | SLO error rate, IO admission, reset events | Database/cluster counters, no per-session data; accepted |
| 50 | `schema/lint/llm_jsonb.go` slowQuerySQL | pgss | LLM JSONB lint | Excluded already; now uses the shared helper |
| 51 | `api/cases_handlers.go` hint text by queryid | pgss | UI annotation | Display; exempt |
| 52 | `cmd/pg_sage_sidecar/prometheus.go` connections by state | activity | Prometheus metric | Monitoring metric, not analysis; exempt |
| 53 | `selfcost/selfcost.go` | pgss | self-cost metric/finding | Reads pg_sage's own entries on purpose (inverse filter) |
| 54 | `startup/checks.go`, `fleet/capabilities.go`, `api/database_helpers.go` | pgss/extension | capability checks | Exempt |
| 55 | forecaster, query store, RCA signals, causal models (from snapshots) | collector snapshot | forecasts, latency windows | Downstream of rows 1-9 |

## Product decisions

1. **Exclude pg_sage as workload, keep it as a cause.** See the verdict rule. A pg_sage
   blocker in a lock chain still produces a finding (never kill SQL, `isSafeProcess`).
2. **active_backends and churn are per database.** Both were cluster-wide while
   idle_in_transaction and connection states were per database; in fleet mode each database's
   snapshot counted every database's sessions. The circuit breaker and the tuner's work_mem
   context stay server-wide (they protect server resources).
3. **temp_spill_statements reads query text.** It is the only way to filter by the tag. The
   probe still returns no text and runs only on a temp-file incident; the catalog's
   no-query-text guard now ignores exactly the self-exclusion predicate.
4. **The withhold upsert retries on lost connections only, bounded by the pool size.** A
   statement on a terminated session did not run; any other error is returned, never counted
   as new.
5. **Retention keep checks are correlated probes.** Each candidate costs one index probe. With
   the gate fixture (every decision has a verification, so 8,000 old withheld decisions are kept
   forever) that is 8,000 index-only probes per decision purge run instead of a 20,000-row seq
   scan: sage.verification index scans rose from 212 to 32,196 in the steady phase, with no
   heap fetches. In production withheld decisions have no verification, so candidates are
   deleted and the work is bounded by the batch.
6. **No expression index on COALESCE(last_seen_at, created_at)** (coordinator suggestion):
   last_seen_at is rewritten by every repeat of a withheld decision; indexing it would make
   those updates non-HOT. The COALESCE was not the cause of the verification scan.

## Before / after

### Perf gate, small scale (PG17 ag6, `PG_SAGE_PERF_SCALE=small`, the CI command)

| measure | before (perf-int 9a04bd50) | after (this branch, merged f46501da) |
|---|---|---|
| verdict | FAIL, 5 offenders | **PASS, 0 offenders** |
| sage.action_log seq scans / rows, steady | 9 / 161,001 | 0 / 0 |
| sage.action_log seq scans / rows, warmup | 1 / 20,000 | 0 / 0 |
| sage.verification seq scans / rows, steady | 4 / 80,000 | 0 / 0 |
| sage.verification seq scans / rows, warmup | 2 / 40,000 | 0 / 0 |
| sage.sre_investigations HOT updates, steady | 43 % (3 of 7) | 57 % (4 of 7) |

An intermediate run after the first fixes had 2 offenders (action_log, 7 seq scans), which the
gate attributed to the reconcile's verifying read. Capturing pg_stat_statements during a gate
run and replaying each action_log statement 7 times on seeded history found the real source:
the app-managed-index check (7 seq scans, 140,000 rows). Its normalized text
(`interval '180 days'` becomes `interval $10`) cannot be explained, so the gate could not name
it. The reconcile read never scanned (its generic plan only scans when `$n` replaces the
`'verifying'` literal); it keeps a regression test. Investigation state transitions stay
non-HOT by necessity (state is in the live-trigger unique index and the queue index).

The 13 generic-plan suspects (not offenders) were left: they are custom-plan index reads whose
normalized text plans differently; none was a seq scan in the run.

### Self-exclusion (new tests on the pre-fix tree 9cc013d5, then on this branch)

| test | before | after |
|---|---|---|
| collector system stats (app: 1 idle in tx, 2 active) | idle_in_transaction 2, active 4 | 1, 2 |
| collector locks snapshot | 3 pg_sage sessions present | none |
| collector connection states | active 5, idle in tx 2 | 2, 1 |
| collector churn after 6 pg_sage sessions | 5 -> 11 | unchanged |
| circuit breaker active sessions (+8 pg_sage) | 2 -> 10 | unchanged |
| analyzer lock chain of the app holder | 2 blocked (incl. pg_sage) | 1 |
| migration active queries / pending locks | 2 / 2 | 1 / 1 |
| tuner active backends (+8 pg_sage) | 3 -> 12 | unchanged |
| briefing active (+8 pg_sage) | 4 -> 11 | unchanged |
| SRE lock_chains / lock_graph / long_transactions / lwlock_waits | 2 blocked / pg_sage edge / 3 pg_sage listed / 4 backends | 1 / none / none / 2 |
| SRE temp_spill_statements | pg_sage's spill listed | absent, app's present |
| retention generic plans | seq scans of action_log (verification purge), verification (both decision purges) | none |
| schema migration | indexes missing, two updated_at indexes | present; retired |
| withholds on 4 killed pooled connections, 16 concurrent | lost (single retry still lost 2: 14/16) | 16/16, one log line |
| /value (20,000 uncredited + 3 credited) | generic plan Seq Scan; every credited row returned to Go | index-only range, 0 seq scans, <= 3 fetched |
| write latency with a 300 ms pg_sage insert | `INSERT /* pg_sage */` matched `'INSERT%'` | the application's mean only |

The executor, value, api, sre and recommendation packages could not compile the new tests on
the pre-fix tree (new API: `recordWithheldAdmission`'s signature, `realizedSQL`,
`cappedCountSQL`, `agedIDsSQL`); their failing-before evidence is the scratch measurements
above and the mechanism each test pins.

## Bugs found

1. [BUG] `executor/rollback_eval.go` writeLatencySQL: pg_sage's own INSERT/UPDATE statements
   (tagged after the first keyword) matched `'INSERT%'`, so pg_sage's writes to sage tables
   could make a verified action look like a write regression and roll it back.
2. [BUG] SRE lock_graph: a pg_sage waiter's edge could make an application session the cancel
   target derived from the graph.
3. [BUG] Runaway detector: pg_sage waiters counted toward a query's blocked sessions (an
   escalation input).
4. [BUG] `recordWithheldAdmission` swallowed its upsert error and reported a new record. Root
   cause of the "1 of 8 not counted" flake: not an upsert race (INSERT ... ON CONFLICT is
   atomic; 9,600 concurrent withholds on an idle server lost none), but a failed upsert on a
   pooled connection terminated by another session (the loaded run's server log on pgsage-ag2
   has 2,265 "terminating connection due to administrator command"). Reproduced
   deterministically by killing the pool's idle connections.
5. [BUG] Collector active_backends and churn were cluster-wide (fleet leak), unlike their
   per-database neighbors.
6. [BUG] Perf gate offenders: the app-managed-index check seq-scanned action_log every
   analyzer cycle; the decision purges hashed all of sage.verification; the verification purge
   scanned action_log; the actions list's capped count seq-scanned action_log; /value's
   generic plan scanned action_log; sre_investigations updates were non-HOT because two
   indexes keyed updated_at.
7. [TEST BUG] `TestCatalog_HasM6ReactiveProbes` hardcoded v1 instead of the shared expected
   version table; `TestCatalog_NeverSelectsQueryText` matched a WHERE-only predicate. Both
   corrected (explained in commit dab047bd). My Actions paging test matched SQL text the
   collapsed row does not render (fixed in 0fd61005); my capped-count test left a 20,000-row
   ledger that changed later plan tests (cleanup added); the temp-spill test retries after a
   foreign package's unscoped `pg_stat_statements_reset()`.

## Test Results

**Command:** `go test -p 2 -count=1 -cover -v ./...` (PG17, pgsage-ag6, repo root mounted in
`golang:1.25`, `--cpus=2`), merged tree through 95f09874
**Total:** 11,620 passed, 3 failed, 21 skipped (86 packages ok, 2 FAIL)

### Failures (load-only; each package passes alone)
- `internal/analyzer` `TestPhase2_ResolveCleared_ResolvesInactive`: `schema bootstrap failed:
  acquiring advisory-lock connection: context deadline exceeded` (30 s pool acquire). The PG14
  run showed the cause: `dial error: timeout` to host.docker.internal under load. The package
  passed alone on PG17 and in the PG14 run.
- `internal/sre/probes` `TestCatalog_XIDRunwayCountsConsumption` (probe deadline exceeded) and
  `TestCatalog_WraparoundTablesUseTheTableMaximum` (`create: context deadline exceeded`). The
  package passed alone (55 s) and on PG14 and PG18.

### Other runs (`-count=1`)
- e2e `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok, 78 passed, 0 failed, 13 skipped.
- Perf gate, small: PASS (0 offenders); fixture databases dropped.
- PG18 (:55418), touched packages: all ok except `internal/analyzer`
  `TestOpenIndexRecommendationTables_FiltersByCategoryAndStatus` (the same 30 s bootstrap
  pool-acquire timeout).
- PG14 (:55414), touched packages: all ok except `internal/schema/lint`, whose fixture setup
  could not dial the server (`dial error: timeout`). The generic-plan tests skip below PG16 by
  design.
- `-race` (PG17): selfmonitor, collector, analyzer, migration, sre/probes, value, providerobs
  ok; executor failed `TestApplyLockCeilingCapsCycleAnalyze` ("control without a ceiling took
  774 ms"). **Pre-existing**: the same family fails on `claude/perf-int` f46501da in a full
  executor package run (`TestApplyLockCeilingCapsOperatorAnalyze`, 1.07 s), passes alone 3/3
  on both trees, and the full executor package passed on this branch in the same comparison.
- Web: `npx vitest run`: 52 files, 275 tests passed; eslint clean on the changed files.
- golangci-lint v2.11.4 (the local Windows binary the rules prescribe; the v2.1.6 image is not
  present and was not pulled): 0 issues. gofmt clean; added lines <= 100 characters.

**Coverage (touched packages, PG17 full run; analyzer and probes from their solo rerun):**

| package | coverage |
|---|---|
| internal/briefing | 95.0 % |
| internal/value | 94.9 % |
| internal/sre/probes | 93.1 % |
| internal/analyzer | 91.1 % |
| internal/selfmonitor | 89.8 % |
| internal/collector | 88.9 % |
| internal/providerobs | 88.1 % |
| internal/sre | 87.5 % |
| internal/retention | 86.2 % |
| internal/recommendation | 85.7 % |
| internal/tuner | 85.7 % |
| internal/executor | 85.6 % |
| internal/autoexplain | 84.7 % |
| internal/schema | 83.1 % |
| internal/schema/lint | 80.6 % |
| internal/api | 78.7 % |
| internal/migration | 78.7 % |
| internal/store | 75.9 % |

`internal/testsupport/selfload` is test support (no tests of its own; exercised by 30 tests).

### Skipped Tests (must be zero or justified)
None in the new tests (the GENERIC_PLAN tests skip below PG16 by design). The 21 suite-wide
skips need an external resource or an opt-in flag: live provisioning (AWS RDS, Cloud SQL,
Lakebase, Azure x2, gauntlet x3), live LLM x3, HA/restart containers x3, PgBouncer x3, a
Windows-only path test, the RCA child-process helper, plan fixture regeneration and the
incident bench. e2e: 13 live-LLM skips (`SAGE_LLM_API_KEY` not set; `PG_SAGE_LIVE_LLM` stays
unset by rule).

### Coverage Gaps
All packages meet coverage thresholds (lowest: store 75.9 %, api and migration 78.7 %).

### Manual Checks Remaining
- CHECK-M1: MANUAL — the Findings and Actions pages against a live sidecar with more than
  1,000 rows: "1000+" and "Load more" (covered by vitest with mocked responses).

## Post-test audit

- **Mutation evidence.** Every self-exclusion test failed on the pre-fix tree (the "no filter"
  mutant), with the counts in the table above. Further mutants: a single retry instead of
  pool-size retries (killed: 14/16); `connectionLost` always false (killed); `/value` without
  the partial index (killed by the generic-plan test); the old updated_at indexes recreated
  (killed by the retire test); the app-managed check without its index (7 seq scans).
- **Inputs not tested:** a pg_sage session in the middle of a lock chain (app -> pg_sage ->
  app) is covered by reasoning only (the recursion is unfiltered, the anchor is); a log line
  without application_name is caught only by the statement tag.
- **Assertions that could pass when broken:** the server-wide counts (circuit breaker, tuner,
  briefing) compare minimums of 8 samples before and after 8 pg_sage sessions and fail at +4.
  Other packages only add sessions, so a broken filter (+8) cannot hide; a correct filter could
  only fail if 4+ more foreign active sessions were present in all 8 samples.
- **Test doubles:** none for SQL. Every exclusion runs against PostgreSQL 14, 17 and 18 with
  real pg_sage-configured sessions (the production tagger).

## What is left / coordinator items

1. `TestApplyLockCeiling*` flakes in full executor package runs on `claude/perf-int` too (the
   uncapped control ends at ~0.5-1 s, as if a session-level lock_timeout leaked into a pooled
   connection). Not touched here.
2. The analyzer's `phase2Pool` bootstrap times out acquiring a connection under full-suite load
   (host.docker.internal dial timeouts); perf-api saw the same.
3. Decision purge keep check: 8,000 index probes per run on the gate fixture (decision 5). If
   withheld decisions with verifications exist in practice, a watermark would bound it.
4. `stat_statements_pressure` counts the collector's filtered, LIMITed top statements, so it
   undercounts pg_stat_statements use (pg_sage's entries occupy slots too). Out of scope.
5. `loadRecentlyCreatedIndexes` reads 7 days of successful actions through
   `(outcome, executed_at)` (2,333 rows a call in the fixture): bounded, not an offender.
6. Ten leftover fixture databases on pgsage-ag6 from interrupted runs were dropped.
