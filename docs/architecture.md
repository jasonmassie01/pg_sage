# Architecture

pg_sage is a Go sidecar that connects to any PostgreSQL 14-18 over the network. The former C extension was removed (see the `c-extension-final` tag).

## Why a Sidecar

1. **No installation friction**: No `shared_preload_libraries`, no PostgreSQL restart, no matching PG major-version binaries. Works on managed services (RDS, Cloud SQL, AlloyDB, Aurora) where custom extensions are restricted or prohibited.

2. **Single implementation**: All collector, analyzer, optimizer, and executor logic lives in Go. No dual C/Go maintenance burden.

3. **Feature velocity**: The optimizer, web UI, briefing worker, and Prometheus exporter ship as a single binary with no database-side dependency.

---

## Pipeline

```
pg_sage sidecar (single Go binary)
  ├── Collector        [every 60s]
  │     pg_stat_statements, pg_stat_user_tables, pg_stat_user_indexes,
  │     pg_sequence (paged), pg_locks, pg_stat_replication,
  │     pg_stat_bgwriter, pg_stat_checkpointer, pg_stat_activity,
  │     pg_stat_io (PG16+), pg_database_size() (every 15 min)
  │
  ├── Analyzer         [every 600s]
  │   ├── Tier 1: Rules engine (25+ deterministic checks)
  │   └── Tier 2: Tuning agent (one per database; case-driven, read-only tools,
  │       typed proposals, optimizer admission + HypoPG, calibrated confidence)
  │
  ├── Executor         [trust-gated]
  │   ├── CONCURRENTLY DDL on raw pgx connection
  │   ├── DDL lock/rewrite/live-risk preflight + PR/CI script output
  │   ├── Incident playbooks for locks, runaway queries, connections, WAL,
  │   │   replication, and sequence exhaustion
  │   ├── Vacuum/bloat/freeze autopilot with IO and XID guardrails
  │   ├── Query tuning artifacts beyond hints: rewrites, statistics,
  │   │   parameterization, hint retirement, and role work_mem promotion
  │   ├── Rollback monitor (read + write latency regression)
  │   └── Emergency stop via sage.config
  │
  ├── API + Dashboard  [:8080]  REST API + React SPA (web UI)
  └── Prometheus       [:8187]  Metrics endpoint
```

---

## Component Details

### Collector

Gathers a snapshot every 60s (configurable) from the catalog and statistics views, under a
circuit breaker that skips cycles while the database is in trouble (active client sessions
over `max_connections` above `safety.cpu_ceiling_pct`). A cycle reads the catalog categories
in order (`catalogSteps`: `queries`, `tables`, `indexes`, `partitioned_indexes`,
`foreign_keys`, `system`, `locks`, `sequences`), then `replication` and the extra categories
(`extraSteps`: `io`, `prepared_xacts`, `archiver`, `partitions`, `index_bloat`,
`workload_sessions`, `wait_events`, `subscriptions`, `cdc_heartbeats` when
`replication.cdc_heartbeats` is set, `config_data`).

**Health.** A skipped cycle is logged with its cause: `load ratio 0.93 > ceiling 0.90
(active sessions / max_connections)`, or `load query failed: <error>` when the load query
itself fails (a permission or pooler fault, not load; it still counts toward dormancy).
`Collector.Status()` reports the breaker state (`closed`, `skipping`, `dormant`), the skip
reason and counts, the last load ratio, the last success and the last error, the latest
snapshot's unavailable categories, persist and `query_store` failure counts, and the
current capabilities. `GET /api/v1/databases` carries it per database as `collector`.

**Bounded reads.** Every catalog read runs under a statement and lock timeout and is
bounded by the size of the catalog it reads, not by the cycle:

- `tables` and `indexes` page through `pg_stat_user_tables` and `pg_stat_user_indexes` by
  oid. Index definitions (`pg_get_indexdef`) come from a cache and are read only for new
  or changed indexes, with a full refresh at least hourly.
- Only the 100 largest relations and the 100 largest indexes are sized exactly each cycle
  (`ExactSizeTopN`); every other size is `relpages` x block size.
- `sequences` reads `pg_sequence` in pages of 1,000 sequences per catalog transaction, never
  more than a quarter of the server's lock table, and at most 20,000 sequences a cycle;
  past that it rotates through the catalog across cycles (reading every sequence in one
  transaction held one lock per sequence and exhausted the lock table).
- `pg_database_size()` (a `stat()` of every file of the database) is measured every 15
  minutes and carried between measurements.

**Storage.** Snapshots are written through `internal/snapstore`: a category is stored as a
full row (a keyframe) when it has no usable keyframe, the keyframe is 6 hours old, or the
change is too large, and otherwise as a delta row on its keyframe or on the latest
checkpoint (changed fields by element identity, counters as increments). Every reader goes
through `sage.snapshot_data()`, which returns the document as the collector wrote it.
`sage.snapshots` and `sage.query_store` are range-partitioned by UTC day; the collector
creates each day's partition ahead, and retention (`retention.snapshots_days`,
`retention.query_store_days`, `retention.snapshots_max_pct`) drops whole days.

**Query store.** Each cycle the collector also writes per-query samples (calls, total time,
rows, statistics epoch, plan fingerprint) to `sage.query_store` through a
`querystore.Recorder`, which writes a query only when its counters or epoch moved since its
last sample, and at least once an hour (a keyframe). Readers take an absent sample as
"unchanged": a window starts at the query's newest sample before it, within two hours
(`AnchorLookback`). Samples of different statistics epochs (a counter decrease, a changed
`pg_stat_statements_info.stats_reset`, or an unknown epoch next to a known one) are never
differenced (R10). Both rules live once, in `internal/querystore` (`QueryIntervals` for
verification, `CaptureDeltas` for the SLO latency proxy).

**The workload rule.** The collector keeps every statement. Advice paths (findings, the
tuning agent, plan capture, verification targets, the briefing) read only application
workload as `internal/workload` classifies it: pg_sage's own tagged statements,
`EXPLAIN`, maintenance, statistics resets and backup `COPY ... TO` are not workload, and
neither are utility statements (transaction control such as `BEGIN`, `COMMIT`,
`SAVEPOINT`; `SET`, `RESET`, `SHOW`, `DISCARD`, `DEALLOCATE`, `LISTEN`/`NOTIFY`, `LOCK`)
or DDL and privilege changes (`CREATE` other than `CREATE INDEX`, `ALTER`, `DROP`,
`TRUNCATE`, `COMMENT`, `GRANT`, `REVOKE`): no index or hint fixes a slow `COMMIT`.
Procedures (`CALL`, `DO`) stay workload, so a slow one is still reported. Capacity, I/O
and incident rules (RCA, SRE probes) still see every statement. The one check for what a
planner may `EXPLAIN` (plan capture, the HypoPG what-if, the perf gate) is
`workload.Plannable`: one `SELECT`, `VALUES`, `TABLE`, `WITH`, `INSERT`, `UPDATE`,
`DELETE` or `MERGE` statement after any leading comments; the what-if plans only the
read-only ones.

Each snapshot holds one row per category: `queries`, `tables`, `indexes`,
`partitioned_indexes` (the partitioned tables' own indexes, relkind `I`), `foreign_keys`,
`system`, `locks`, `sequences`, `replication`, `io` (PostgreSQL 16 and later), `partitions`,
`prepared_xacts` (this database's two-phase-commit transactions from `pg_prepared_xacts`),
`archiver` (the server's one `pg_stat_archiver` row with the current WAL file and
`wal_segment_size`, for the WAL archiver findings),
`index_bloat` (the largest btree indexes' bloat estimated from planner statistics, at most 200),
`workload_sessions` (this database's client sessions by role, application and state, at most
200 groups, its 50 longest-running sessions with their sqlcommenter tags, and per-role
statement time and cluster-wide session counts, which workload budgets read),
`wait_events` (this database's active client sessions grouped by wait class and event, a
session without a wait event counted as `CPU`: one aggregate of `pg_stat_activity` per
cycle, with no query text and no pid, and without pg_sage's own sessions; it is sampled
each collector cycle, not active session history, and is the workload explorer's wait-load
chart),
`subscriptions` (this database's logical replication subscriptions, their apply workers and,
from PostgreSQL 15, their error counters; `subconninfo` is never read; at most 200)
and `config_data` (the advisor's settings, reloptions, connection and WAL snapshot, collected only
when an advisor will consume it). A category that could not be read in a cycle is
**unknown, not empty**: it is listed in the snapshot's `unavailable` map with the error, no
row is written for it, and rules that would read its absence as a fact stay quiet for that
cycle. Only the `system` category, the snapshot's spine, fails the whole snapshot. A category
pg_sage does not collect (`io` below PostgreSQL 16, `config_data` without an advisor) is not
collected, which is different from unavailable.

The `tables`, `indexes`, `foreign_keys`, `partitions` and reloptions reads leave out temporary
relations (`relpersistence = 't'`: other sessions' and pg_sage's own), which no other session
can vacuum, analyze or index; unlogged tables are still collected.

`queries` admits 2 x `collector.max_queries` (N) statements of this database on their
`pg_stat_statements(false)` counters, without texts, through four arms, each recorded in the
statement's `admitted_by`: `total`, the top statements by total time; `mean`, the K slowest
by mean time with two calls or more (a 20 s report run 50 times a day ranks below hundreds
of OLTP statements by total time); `io`, the K heaviest by blocks read plus temp blocks
written; and `pinned`, the queryids something depends on: the targets of open index
verifications and in-memory watches, at most 64 a cycle, oldest first. K is N/10, at least 1
and at most 50; the total arm keeps 2N - 2K - pins rows, never fewer than N, so the row
count, and the text read after it, stay at 2N. The mean and io arms leave out the queryids
the text cache already knows as pg_sage's own. Pinned rows, then the other arms' rows, are
kept first when the list is cut to N, and the list is published by total time. An N below 2
uses the total arm alone. These bounds are internal constants, not configuration.
`pg_stat_statements(true)` loads the
whole query-text file whatever rows are kept, so each collector keeps the texts in a cache
of its own, keyed by `queryid` of its own database (no sharing between databases, also in
fleet mode). The cache is bounded at 4 x `collector.max_queries` entries and 32 MiB of text
per collector, evicting the least recently seen. A cycle reads texts, in one statement
tagged `/* pg_sage text */` for exactly the missing ids, only when a candidate is new to the
cache or its text is an hour old: a steady workload reads them about once an hour. A new
statistics epoch (`pg_stat_statements_reset()`, a restart) clears the cache, and rising
`pg_stat_statements_info.dealloc` refreshes it on the next cycle. pg_sage's own statements
are left out in Go on the texts (`selfmonitor.IsStatementText`, the same predicate as the
SQL filter), then the list is cut to `collector.max_queries`; a candidate whose text is NULL
or gone cannot be attributed and is left out. A failed text read makes `queries`
unavailable: its counters are never published without their texts. Near
`pg_stat_statements.max`, the utility and `COPY ... TO STDOUT` classification (which reads
every text) runs at most hourly, and only when the entry or deallocation count moved.

The catalog categories and the extras (`io`, `prepared_xacts`, `archiver`, `partitions`,
`config_data`) share one retry budget per cycle: a read that hit a statement or lock timeout
is read once more, and at most two reads are retried per cycle. `config_data` is all or nothing: if any of
its sub-reads (`pg_settings`, `set_privileges`, `reloptions`, `connection_states`,
`wal_position`, `extensions`, `database_owner`) fails or returns a row that does not
scan, the whole category is unavailable and the reason names the sub-read. On a hot
standby the WAL position is the last WAL received (or replayed), since `pg_current_wal_lsn()` fails there.

`prepared_xacts` is stored as changes on a keyframe, paired by `gid`; a database with no
prepared transaction stores `[]`, which differs from the missing row of an unknown cycle.

`foreign_keys` holds one element per constraint: `schemaname`, `table_name`,
`ref_schemaname`, `referenced_table`, `constraint_name`, the constraint `oid` and `columns`,
the referencing columns in declared (`conkey`) order. Partition clones (`conparentid <> 0`)
are left out, so a key on a partitioned table, or one referencing a partitioned table, is one
element naming the partitioned parent. The category is stored as changes on a keyframe,
paired by schema, table and constraint name; no keys stores `[]`. Rows written before this
shape held one element per column with no schema; `/api/v1/snapshots/latest` regroups such a
row per constraint, with an empty schema.

### Analyzer (Tier 1 -- Rules Engine)

25+ deterministic rules across 6 categories. No LLM required:

| Category | Rules |
|----------|-------|
| Index health | unused_index, invalid_index, duplicate_index, missing_fk_index |
| Query performance | slow_query, high_plan_time, query_regression, seq_scan_heavy |
| Sequences | sequence_exhaustion |
| Maintenance | table_bloat, xid_wraparound |
| System | connection_leak, cache_hit_ratio, checkpoint_pressure |
| Replication | replication_lag, inactive_slot, slow_replication_slot, subscription_errors, subscription_lag, cdc_consumer_stalled, cdc_heartbeat_stale |

Each rule produces findings with severity (critical/warning/info), recommended SQL, and rollback SQL.

**Seasonal baselines (`internal/baseline`).** Fixed thresholds cannot tell a report that is slow every Monday at 09:00 from a real slowdown. Each cycle the analyzer folds what the collector already read (statement interval means, active and total backends, WAL bytes per second, lock-graph sessions per wait class) into one of 168 hour-of-week buckets per week in `sage.baseline_samples`, keyed by database, and scores the new value against the same bucket in the previous `baseline.weeks` weeks with a robust z over the median and MAD. `workload_anomaly` opens after 2 consecutive anomalous cycles and resolves after 2 normal ones; a bucket with fewer than 3 weeks is learning. `query_regression` judges against the bucket median when it is active. Scoring is deterministic (no LLM) and adds no query on the monitored database; the fixed-threshold rules remain the floor. A critical anomaly is a Sage SRE trigger (`workload_anomaly`), one read-only investigation per episode.

### Tuning agent (Tier 2)

Lives in `internal/tuning/` (roadmap 2.2). One agent per database replaces the former
optimizer, advisor vacuum/memory and tuner hint prompts:

- **Workload classification** (deterministic): application, tenant-family, test-fixture,
  diagnostic and pg_sage statements; confirmed facts give each table its routes.
- **Cases**: a top statement by interval time (at least 5% of the workload), a regression
  (mean time at least doubled), or write amplification (index maintenance on an unused or
  covered index, or dead-tuple churn). No case, no model call.
- **Read-only tools**: statement, table, explain (cache or `EXPLAIN` without `ANALYZE`,
  generic plans on PG16+), whatif_index (HypoPG), write_cost, extended_stats, and rehearse
  (clone provider only). Bounded results, wrapped as untrusted data, numbered as evidence.
- **Typed proposals only**: index create/drop, server setting, storage parameter,
  `CREATE STATISTICS` (`sage_stx_*`), query hint, each with cited evidence and a predicted
  effect. pg_sage generates every statement.
- **Validation**: index creates go through the optimizer's admission (`internal/optimizer`:
  canonical form, 8 validators, rejection memory in `sage.optimizer_rejection`, HypoPG
  what-if; a verified measurement replaces the model's estimate); configuration through the
  advisor's allowlists and ranges; hints through the tuner; confirmed facts redirect bound
  changes to source-fix packets; operator rejections are not re-proposed.
- **Calibrated confidence**: per action class and prediction method from
  `sage.action_outcome` (reliability bins by predicted improvement, Wilson intervals);
  "uncalibrated" below `tuning.calibration_min_outcomes`; a lower bound under the
  confidence threshold needs operator approval.
- **Budgets**: per database per cycle (`tuning.max_requests_per_cycle`,
  `tuning.max_tokens_per_cycle`), at most `tuning.max_cases_per_cycle` cases; a case whose
  recent answers were all wasted is not asked again until it changes.

### Executor (Tier 3 -- Trust-Gated)

| Trust Level | Timeline | Allowed Actions |
|-------------|----------|----------------|
| **observation** | Configured | No actions -- cases and recommendations only |
| **advisory** | Configured | Auto may execute eligible SAFE actions; higher risk queues |
| **autonomous** | Configured | Auto may execute eligible SAFE and MODERATE actions; HIGH queues |

HIGH-risk actions always require manual approval. Every action carries a typed
contract: risk tier, guardrails, expiration, rollback or mitigation, policy
decision, lifecycle state, and verification state. Execution outcomes are
logged to `sage.action_log`; pending work and approval outcomes are tracked in
the action queue.

Execution mode is an independent operator control. `manual` disables all
background queueing and execution at every trust level. `approval` queues
supported actions whenever trust is advisory or autonomous. `auto` executes
eligible SAFE actions at advisory trust, executes eligible SAFE and MODERATE
actions at autonomous trust, and queues higher-risk supported actions. Neither
trust nor its ramp can promote `manual` to `auto`. Emergency Stop and a
per-database disabled executor are hard mutation blocks.

High-risk schema changes are handled as migration-safety cases before direct
execution. The case projector attaches deterministic DDL preflight evidence,
including lock/rewrite classification plus live table-size, activity, pending
lock, replica-lag, and lock-timeout checks. It also attaches generated
migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI
metadata.

The live values come from the migration advisor's risk assessment, written
into the finding detail when the DDL is detected (`table_size_bytes`,
`estimated_rows`, `stats_known`, `active_queries`, `longest_query_sec`,
`pending_locks`, `replication_lag`, `estimated_lock_ms`, `lock_level`,
`requires_rewrite`, `affected_table`, `measured_at`). They are
detection-time values, not a fresh probe: the preflight notes "measured at
detection" and the dashboard shows their age. A live read that failed is left
out, so its check reads `unknown` rather than passing on a zero (with every
read failed there is no `measured_at` and no lock-hold estimate), and table
size is `unknown` when the table was never analysed (`stats_known = false`).
Findings written before these keys existed keep rendering `unknown`. Lock
level and rewrite come from the finding first (the candidate's own
`CONCURRENTLY` SQL takes its own lock), then from the one DDL rule catalog
(`internal/migration/ddlrules`) that the classifier also uses. A measured
warning (for example an active query on the table) blocks the candidate for
review. `active_queries` leaves out the detected DDL's own backend, which is
not load it competes with; its own queued lock still counts in
`pending_locks`. `estimated_lock_ms` is a heuristic estimate, so its check
says "estimated at detection", never "measured".

An in-flight DDL is analysed once per backend and statement but re-evaluated
on every activity poll, and each poll re-persists its finding, so a DDL still
running after 24 hours keeps its finding open. The detector adds no blocking
query of its own: it reads the lock-chain fast path's latest observation
(the `pg_blocking_pids` walk, which keeps each session's direct waiters) and,
while a tracked DDL waits on `Lock`, nudges the fast path (at most once per
10 s). When `migration.queue_waiters_threshold` (default 3) sessions wait
directly behind the DDL in an observation at most two fast-path intervals old,
the finding turns critical with the DDL's backend identity and an
approval-only, HIGH-risk guarded cancel of the DDL, followed by a retry under
`SET lock_timeout = '5s'`; the lock-chain finding and the RCA lock-contention
incident for that queue keep their evidence but no longer offer a signal to
the root reader. An unqualified table name resolves through the DDL role's
`search_path` (`pg_db_role_setting`, else the server default `"$user", public`),
then `public`, then a unique relation name; several matches are reported as
ambiguous with statistics unknown. These artifacts are shown in Cases and
Actions so teams can review
schema work through their normal change-control process.

Migration enrichment is asynchronous. Detection never waits for a model:
both detectors store the deterministic finding first, then queue the model
work on one worker per database, with a queue of 16 that drops the oldest
request when full (`pg_sage_migration_enrich_dropped_total{database}`). The
worker asks for a migration script for a classified statement, or runs the
LLM fallback for one no rule classified; the fallback's finding is stored only
when it scores above the 0.3 threshold. One answer per statement identity
(sha256 of the rule, the sanitized statement and the table's schema text) is
cached for 24 h, up to 256 entries, so the activity and log detectors share
one call and a schema change asks again. Schema context is read through the
bounded catalog reader with `to_regclass(format('%I.%I', ...))`, so
mixed-case names resolve.

Model text is shown as model output and is never `recommended_sql`: only a
rule's deterministic template (`RecommendedSQLSource = deterministic`) is,
and only as one statement the executor runs alone. A model script that
pg_sage validates is `detail.llm_script` (the fallback's alternative is
`detail.llm_safe_alternative`); one it cannot validate, including one that
runs `CONCURRENTLY` inside `BEGIN`/`COMMIT`, is `detail.llm_unvalidated_text`
with the reason in `detail.llm_script_invalid`. A re-detection keeps these
`llm_` keys. RCA Tier 2 diagnostic SQL is kept only once proven read-only
(see [Model-authored SQL](security.md#model-authored-sql)).

Incident playbooks follow the same typed-action model. Read-only diagnostics
can inspect blocker graphs, runaway queries, connection pressure,
WAL/replication state, standby conflicts, vacuum pressure, and freeze
blockers. Each diagnostic proposes the SQL of the Sage SRE catalog probes
for its family (lock_graph; long_transactions and temp_file_holders;
connection_saturation; replication_slots and replication_lag;
standby_replay_state; xmin_horizon and wraparound_tables; stat_tables,
xmin_horizon and vacuum_progress), rendered with the probe's row cap as a
literal LIMIT and its `/* pg_sage sre:<probe> <version> */` comment, so it is
bounded and scoped to `current_database()`. Run it read-only with a
statement_timeout; temp_file_holders reads `pg_ls_tmpdir()`, which needs
`pg_monitor`. The playbook is chosen from the incident's signal ids through one
table that records a decision, "none" included, for every signal RCA, the log
classifier and the SRE detector emit; the root-cause wording never selects
one. Analyzer and executor findings route the same way: replication lag and
inactive slots get the read-only WAL diagnostic (never a slot drop), a
sequence near its limit gets the reviewed migration script, and a runaway
query gets the diagnostic while its cancel stays with the executor's runaway
policy. Backend
cancel/terminate candidates come only from the structured blocker identity
RCA records on a lock_contention incident (pid, backend_start, query_start
and a hash of the first 200 characters of the query), never from a PID in
incident text, and only when the incident has exactly one root blocker.
They are script only: the script re-checks that identity, the session state,
`datname = current_database()` and the protected-session guards the executor
uses, so it signals zero sessions once the backend has moved on. An active
blocker gets a cancel (moderate) and, on a critical incident, a terminate
(high); an idle-in-transaction blocker gets a terminate only, because
`pg_cancel_backend` does not end an idle session. Idle-in-transaction
saturation, runaway-query and older incidents without that identity show the
diagnostic and a blocked cancel. Sequence
capacity changes are treated as forward-fix migrations with script and
verification output rather than autonomous DDL.

Vacuum, bloat, and freeze autopilot turns maintenance findings into bounded
actions. The analyzer's `table_bloat` rule proposes a guarded `VACUUM` (none
while IO is saturated) and its `autovacuum_tuning` rule a per-table scale
factor; its wraparound rules only diagnose. The freeze custodian is the single
producer of freeze actions. On each database's autonomy cycle (every
`analyzer.interval_seconds`, at least a minute apart) it answers a red table
with a ladder: a response to the xmin holder that pins it (a session signal
that always waits for approval, or a parked plan), then `VACUUM (FREEZE)`, a
parked plan for red system catalogs, and reviewed `pg_repack` plans for large
bloated heaps instead of `VACUUM FULL`. Concurrent reindex handles index-bloat
remediation. See
[Vacuum, bloat and freeze autopilot](configuration.md#vacuum-bloat-and-freeze-autopilot).

Query tuning has multiple paths. `pg_hint_plan` remains useful for reversible
planner experiments, but pg_sage can also generate application-query rewrite
artifacts, retire broken hint experiments, recommend `CREATE STATISTICS`, draft
parameterization changes, and promote repeated per-role `work_mem` patterns
into reviewed role settings. Those actions are modeled as typed cases with
rollback class, verification steps, and PR/script output so they can move
through change control like schema work.

The executor checks: trust level, trust ramp, per-tier toggles, maintenance window, emergency stop flag, and replica status before acting.

### API + Dashboard (Web UI)

REST API and embedded React SPA on `:8080` (configurable). The v1 UI is
organized around Overview, Cases, Actions, Fleet, and Settings. The legacy
`#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and
`#/incidents` routes open Cases with the appropriate source context. The API
includes case projection, shadow decisions, action queue, provider readiness,
findings, snapshots, config, forecasts, query hints, alerts, emergency stop,
and fleet management. UI and `/api/v1/*` routes are session-authenticated.

**Cases ranking and urgency.** Cases from every database are ranked by
severity first; no warning ever outranks a critical. Within a severity, a case
whose producer reports its limit already reached (a zero `days_until_full`,
`days_to_warn_pct` or `seconds_to_limit`) comes first, still showing no number;
then a case with an assessed time to impact, the nearest impact first, then
cases without one, then the most recent observation, then database and id.
Each case carries `rank_basis`, for example "severity warning; time to impact
about 3 days". The time to impact (`urgency`) is read only from the fields the
producer wrote, never computed by Cases: `days_until_full` with `r_squared` and
`data_points` (storage growth), `days_to_warn_pct` with `r_squared` (connection
saturation) and `seconds_to_limit` with `rate_per_s`, `samples`, `span_s` and
`r2` (disk, WAL slot, sequence and wraparound runways). The case shows the
formula as its basis sentence, for example "About 3 days until the disk is
full (seconds_to_limit 259200 from 48 samples over 6 h, R² 0.97)". A missing,
non-numeric, non-finite or non-positive value, a zero rate, or a storage or
connection regression fit below R² 0.5 leaves the case "Urgency not assessed";
it never shows a reassuring number. XID wraparound and sequence exhaustion
findings that carry no rate show their headroom instead ("xid_age 1.2B of
the 1.6B critical threshold") and stay unassessed. A recurring incident (one with
`previous_incident_id`) carries `recurrence`: the previous incident, the
number of occurrences in its chain and when the previous one was resolved,
read in one batched lookup per database, shown as "4th occurrence; previous
resolved 2026-11-02". When that lookup fails the case says the history is
unavailable rather than calling it a first occurrence.

**Cases and the recommendation lifecycle.** A finding case carries
`recommendation`, the newest `sage.recommendation` head of its finding (read
in one query per database over the `recommendation_finding` index): its id,
state, revision, verdict, reason, attempt count, `next_attempt_at`, when it
was applied, `current_content` (the head carries the content hash the case
proposes now), a one-line `next_step` and `url`
(`/api/v1/recommendations/{id}?database=...`). The case's next step follows
the head: "pg_sage applied this on 2026-11-10 and reverted it: <reason>",
"Stopped after 3 failed attempts", "Retrying after 14:05 UTC" and so on. A
reverted or abandoned head of the same content blocks the candidate as
`recommendation_reverted` or `recommendation_abandoned` (the store never
re-proposes that content); a failed head in backoff sets the candidate's
`retrying_after`. A head of other content blocks nothing and says the current
proposal differs. When the heads cannot be read, every finding case of that
database is marked `recommendation_unknown`, never "no recommendation", and
the response is still 200. A candidate past its `expires_at` is blocked as
`evidence_expired` ("Evidence expired; waiting for a fresh observation"),
replacing any other restriction except a recommendation refusal; the policy's
own reason stays in `policy_decision`. Each candidate shows how long its
evidence is valid. A head state the dashboard does not know is shown as
"state unknown", never as an attached change.

**Header and context strip.** The header says which database you are looking
at, its mode and what pg_sage may do there right now. With one database
selected, the trust badge shows its level, and its tooltip lists the action
families that run without approval now ("Auto-executes now: analyze_table"),
or "Nothing auto-executes right now". The list comes from each family's
policy decision, which the server takes from the executor's gate on every
status read, so it already accounts for the trust ramp, a disabled executor,
replicas and the emergency stop. A database that is emergency-stopped never
lists a family. In the all-databases view, a fleet on one level shows that
level, and a fleet on several shows every level in use, for example
"Mixed · 9 autonomous · 1 observation" (a database that has not reported a
level counts as "unknown", never as autonomous). The tooltip then names, per
family, the databases that run it unattended. The context strip under the
header repeats this in words ("Can run without approval now: create_index,
analyze_table. Everything else queues for approval or is blocked by policy.")
and, in the all-databases view, says how many databases are emergency-stopped.
While some databases have not reported their action policy, the strip and the
fleet tooltip add "2 databases have not reported their action policy" and the
strip limits its claim to the databases that did, since one that has not
reported may still auto-execute.

**Approval cards.** Each pending action in "Needs your review" is a card
(`GET /api/v1/approvals`): why it needs a person, the cited evidence, the
rationale, the predicted effect, the exact SQL and rollback, and the risk. A
`pg_hint_plan` card (`apply_query_hint`, `retire_query_hint`) also carries
`hint_evidence`, the plan comparison that gated the hint, with the finding's key
names: "Plan comparison: changed, cost 412.0 to 254.6 (-38.2%)", the reason when
the verdict is not `changed`, and the plans without and with the hint in
collapsed blocks (see [hint verification](hint-verification.md)). A stored plan
over 64 KB is left out and named in `plain_plan_omitted` or
`hinted_plan_omitted`; a malformed `hint_evidence` shows no section and logs one
WARN with the finding id. The section is read-only: the card hash, which binds
an approval, and the chat and Slack card text do not change.

The pending-approvals badge counts the selected database, or the whole fleet
in the all-databases view, the same scope as the Actions list. When a
database could not be counted (not connected, or its read failed) the badge
shows "3+" and its tooltip names the databases left out.

**Data freshness.** The header shows how current the page is. "Live" means the
`/api/v1/events` stream is open and pages refresh as changes are pushed.
"Polling, updated 40 s ago" means the stream is not live yet, or dropped; pages
keep their normal 30-second polling and the age is that of the last good fleet
poll. "Offline, data from 14:02" means three reconnects in a row failed; the
data on screen is from that time. Each tab keeps one stream for every database,
also in fleet mode. The server ends each stream after 45 seconds and the browser
reconnects on its own. When a reconnect is refused (for example a proxy 502 or
504 while the sidecar restarts) the browser gives up, so the dashboard opens a
new stream after 3 s, doubling up to 60 s with 20% jitter, and goes back to
"Live" when it connects, without a page reload.

**Background tabs and outages.** A dashboard tab that is not shown sends no
polls: each page fetches once when the tab is shown again, so the
pending-approvals badge in a background tab catches up when you return to it
(notification channels still carry urgent approvals). While an endpoint keeps
failing, its polls slow down to 2x, 4x and so on of the normal interval, up to
5 minutes, and return to normal after one success; Retry buttons still fetch at
once. Live events refresh a page only for the database it shows (or for all of
them in the all-databases view), and a burst of events refreshes it once, 2 s
after the last one. The "Getting started" checklist polls every 10 s until setup is
complete, then every 60 s.

Provider readiness uses adapters for Postgres, Cloud SQL, AlloyDB, RDS, and
Aurora. Adapters describe extension enablement paths, log access, managed
service limitations, and supported action families before policy evaluation
decides whether a specific database can execute, queue, or only observe.

### Alerting

pg_sage delivers notifications through one engine, the notification outbox (alerting-06b).
Channels and rules come from two places, both stored in `sage.notification_channels` and
`sage.notification_rules` of the control database: the Notifications page (`managed_by =
api`) and the config file's `alerting:` block, which the alerting owner reconciles into
`config:<kind>:<name>` channels and `finding_opened`/`finding_resolved` rules (`managed_by =
config`) at startup and on every live change ([alerting](configuration.md#alerting)). Each
finding therefore posts once per channel and pages PagerDuty under one key, with the outbox's
retries, cooldown, quiet hours and reminders. Config rows show a "From config file" badge and
the API refuses to change them (409 `managed_by_config`).

`alerting.engine: legacy` keeps the old per-database alerting manager (below) for one
release instead; at startup it removes the config rows a notify run left, so the block is
delivered once, and it posts a second time to any destination the Notifications page routes to, because the two stacks share no state and their dedup keys
differ. It is removed by alerting-06d.

| | Notify engine (default) | Legacy engine (`engine: legacy`) |
|---|---|---|
| Configured in | the Notifications page and the reconciled `alerting:` block | the config file's `alerting:` block |
| Destinations | Slack, PagerDuty, email, Telegram, webhooks, Microsoft Teams, Opsgenie, incident.io | Slack, PagerDuty, webhooks |
| Triggers | the event types below, each rule with a `min_severity` | every new finding at or above the route's severity, polled per database |
| Retries | queued in `sage.notification_outbox`, up to 6 attempts | up to 3 attempts within one check |
| Cooldown, quiet hours, reminders | the `alerting.*` values, for the finding events `finding_critical`, `finding_opened` and `finding_resolved` ([notification delivery](configuration.md#notification-delivery)) | `cooldown_minutes`, `quiet_hours_*`, `repeat_minutes` |
| Log | `sage.notification_log`, the Notifications page's Log tab | `sage.alert_log` of each database, `GET /api/v1/alert-log` (deprecated) |

**One notification log.** Every delivery the notify engine makes is a row of
`sage.notification_log` naming the database it was for and the event's dedup key; a
fleet-wide event, and every row written before the upgrade that added the column, has no
database and shows as `unknown`.
The Notifications page's Log tab, open to admins and operators, lists them for the database
selected in the header (all databases when none is), through
`GET /api/v1/notifications/log?database=<name>`. The old Alert log is folded into it: the
`#/alerts` dashboard URL redirects to the Log tab, keeping `?database=`, and
`GET /api/v1/alert-log` still serves `sage.alert_log` history (rows age out as before) with
`Deprecation: true` and a `Link` to its successor. Channels and rules stay admin-only.

**Cutover.** After the switch to the notify engine, each database runtime resolves the
PagerDuty pages the legacy engine opened on it (newest `sage.alert_log` row `sent` on the
`pagerduty` channel, no resolve since) through the reconciled PagerDuty channel, under the
legacy key `<database>:<category>:<object>`, once the finding resolves. Each attempt is
logged in `sage.alert_log` (`resolved` or `resolve_error`, at most 5 failures per finding);
the sweep runs every minute and stops when no legacy page is left.

#### Notifications (channels and rules)

Routes events to the channels of the matching rules. A `webhook` channel posts each event as
JSON (`event`, `severity`, `subject`, `body`, `database`, `dedup_key`, `resolve`, `data`,
`sent_at`) to its `webhook_url`, with its optional `headers`, a JSON object of header names to
values (`Host` cannot be set). Both are sealed like every channel secret, and the URL passes
the notification target policy when the channel is saved and again at connect time. Event types: `finding_opened` and
`finding_resolved` (any finding that opens, reopens or escalates, and its resolution, at the
finding's own severity, with the dedup key `finding:<database>:<category>:<object>`),
`finding_critical`, `action_executed`, `action_failed`, `approval_needed`, `query_rewrite_suggested`, `incident_detected`, `incident_escalated`, `incident_resolved`, `custodian_escalation` (critical: a red custodian proposal exhausted its failure backoff, raised once per failure streak and recorded as a `custodian_backoff` decision), `fact_expired` (a confirmed binding fact expired and no longer binds; see [facts](facts.md#expiry-and-re-verification)), `canary_interrupted` (warning: a sidecar restart interrupted a fleet canary, which is not resumed; see [configuration](configuration.md)). A Sage SRE diagnosis is not a rule event: `investigation_concluded` goes only to the channels an incident's page reached, once per incident (threaded under the Telegram page, never to PagerDuty; see [configuration](configuration.md#sage-sre-investigations)).

**Destinations.** Every channel type is a `notify.Sender`, registered once in
`notify.NewDefaultDispatcher`, and delivered through the outbox with its retries:

| Type | Protocol | Open and resolve |
|---|---|---|
| `slack`, `telegram`, `email` | incoming webhook, Bot API, SMTP | a resolve is an ordinary message |
| `webhook` | JSON post | `resolve: true` with the same `dedup_key` |
| `pagerduty` | Events API v2 | trigger and resolve on the dedup key |
| `teams` | Workflows webhook, Adaptive Card | a resolve is a green "Resolved:" card |
| `opsgenie` | Alert API v2 (`us` or `eu`) | create with `alias` = dedup key, close by alias |
| `incidentio` | HTTP alert source | `status` `firing`, then `resolved`, on one `deduplication_key` |

Jira is not a destination but a case link: a `jira` channel holds the sealed credentials,
`sage.case_tickets` links one case to one issue, and a bounded worker reads the issue status
back as evidence ([case tickets](configuration.md#case-tickets-jira)).

A `Retry-After` on a 429 or 5xx answer holds the next attempt back by that long, at most an
hour ([configuration](configuration.md#notification-channel-types)).

**PagerDuty lifecycle.** Each pg_sage incident is one PagerDuty incident with the
`dedup_key` `incident:<database>:<incident id>`. `incident_detected` and
`incident_escalated` trigger it (an escalation joins the open PagerDuty incident instead of
opening a second one) and `incident_resolved`
resolves it, so a PagerDuty channel must have rules for all three events; without an
`incident_resolved` rule the page stays open. A recurrence is a new incident id and opens a
new PagerDuty incident. The resolution of an incident last seen longer ago
than `rca.stale_after_hours` sends no notification, so close its PagerDuty incident by hand. A
key longer than PagerDuty's 255-character limit is shortened to its head plus a hash, so
the trigger and the resolve still match. A critical finding pages with the key
`finding_critical:<database>:<category>:<object>`. When the analyzer resolves the finding (it
is no longer detected), it sends a resolve on that key as a `finding_critical` event, so the
rule that paged also closes the page; a re-opened finding pages again. Slack, Telegram and
webhooks receive the resolves as ordinary messages ("Incident resolved: ...",
"Resolved: Critical finding: ..."). The channel's `dedup_key_prefix`
is still prepended to every key. PagerDuty incidents opened before v2.5.1 used other keys and
do not auto-resolve: close them by hand once after the upgrade.

#### Legacy alerting engine (`alerting.engine: legacy`)

Polls each monitored database for new findings and routes them by severity to Slack,
PagerDuty and webhooks, with cooldown, quiet hours (a deferred alert is delivered when they
end) and reminders for findings that stay open.

**Alerting manager keys.** The `alerting.*` manager runs once per monitored database and
names that database in every Slack header, PagerDuty `source`/`component` and webhook
`database` field. Its PagerDuty `dedup_key` is `<database>:<category>:<object>`, for the
trigger and the resolve alike, so identical schemas in a fleet (shards, tenants, staging
and prod) that share a routing key get one PagerDuty incident each, and one database's
resolve never closes another's page. The database name is kept verbatim (a `:` inside it
is fine; keys are opaque), and a key over 255 characters is shortened to its head plus a
hash. Without a database name the key stays `<category>:<object>`. Cooldown, quiet hours
and `sage.alert_log.dedup_key` (`<category>:<object>`) are unchanged: each database keeps
its own `sage` schema and its own throttle. Alerts open at upgrade time used the old key,
so they re-page once under the new key (standalone included) and the old PagerDuty
incident must be closed by hand.

### AutoExplain Collector

Stores plans for slow statements in `sage.explain_cache`, each with its true
`source`:

- `explain_estimate`: an on-demand plain `EXPLAIN` (never `ANALYZE`) in a
  read-only, rolled-back transaction bounded by a 5 s statement timeout and
  the probe lock timeout. It is an estimate, with no execution time.
  Parameterized statements are planned with `GENERIC_PLAN` on PostgreSQL 16+
  (`generic_plan`) and skipped on older servers. A statement that already has
  a plan from the last day is not explained again.
- `auto_explain_log`: the plan of a statement that ran, as `auto_explain`
  wrote it to the server log (with actual rows and timing only when
  `auto_explain.log_analyze` is on, which adds overhead to every statement), where pg_sage reads that log: the
  Supabase log sink, and on self-managed servers the cluster's log watcher
  (`logwatch.enabled` with RCA or migration log detection on), where each
  database subscribes to its own records. A plan needs
  `auto_explain.log_format = 'json'` and a query identifier
  (`compute_query_id = on` and `auto_explain.log_verbose = on`: only verbose
  output prints it). The log watcher stores at most
  `auto_explain.max_plans_per_cycle` plans per poll and keeps plan records of
  up to 1 MiB. Records
  that are not stored are counted by reason (`invalid_json_plan`,
  `missing_query_identifier`, `pg_sage_self`, `other_database`,
  `over_cycle_cap`, `store_failed`) and logged with the setting to change.
- `lakebase_plan_history`: Lakebase Advanced Telemetry plans, kept apart.

Readers that choose between cached plans prefer an executed plan over a newer
estimate. Rows stored as `auto_explain` by versions before rca-04 were
estimates and are renamed to `explain_estimate` on upgrade.

### Forecaster

Analyzes historical trends to predict disk growth, connection exhaustion, sequence depletion, and cache ratio degradation. Generates proactive findings before problems occur.

The runway monitor (`internal/runway`) samples the series a hard limit is approached along
(XID and multixact age, WAL, slots, disk, sequences) on the collector tick and projects each
against its limit. In fleet mode a process-wide `runway.ClusterOwners` elects, per cluster
(system identifier and postmaster start), the one database runtime that reports the
cluster-scope series; the others sample them too but report only their own tables and
sequences, and close their copies once the owner has reported. The forecaster's connection
and checkpoint forecasts use the same election per system identifier and address. See
[Cluster runways and forecasts](walkthrough-fleet.md#cluster-runways-and-forecasts).

### Tuner

Per-query optimization via `pg_hint_plan` (if available). Detects plan-level symptoms (disk sorts, hash spills, bad joins) and applies per-query GUC overrides without modifying application queries. The tuning agent's hints go through it: it validates the pg_hint_plan syntax, clamps `Set(work_mem)`, records the hint and keeps one hint per statement.

### Retention

Automatic cleanup of aged snapshots, findings, actions, and explain plans based on configurable retention windows.

### History store

Telemetry history (`sage.snapshots`, `sage.query_store`) lives in each monitored
database by default. With `history.store: meta` (meta-db mode) it lives in the
metadata database, each row tagged with the database's meta-db record id.

- Every history statement is written once with scope markers (`{db:alias}`,
  `{dbcol}`, `{dbval}`) and runs through `internal/histstore`, which binds them for
  the database's store. A marker is not SQL, so a statement sent without a store
  fails instead of reading an empty or mixed history; a static test checks that every
  statement naming the two tables carries a marker.
- Each runtime resolves its store before it starts and registers it for its monitored
  pool; readers find the store from the pool they already hold (`histstore.Resolve`).
  With `history.store: meta` the process refuses a pool no runtime registered: every
  statement on it fails with `histstore.ErrNotRegistered` instead of reading that
  database's own (wrong) history, so no reader can be wired to the wrong database.
- Reads that joined history with the monitored database's catalog are split: the
  query store's plan fingerprints are read from the monitored `sage.explain_cache`
  and stored with the samples; verification reads history in the store and
  `pg_index` on the monitored database; the first-run check reads `sage.config` on the
  monitored database and the newest snapshot in the store.
- A runtime refuses to start when its history would be split (rows in the old place
  that the migration did not copy); `pg_sage history migrate` copies them.
- Retention of the store runs once per process; the snapshot cap is the sum of every
  registered database's cap.

### Prometheus Exporter

Metrics endpoint on `:8187`. Exports findings count by severity, the LLM circuit breaker
state (`pg_sage_llm_circuit_open`), connection stats, cache hit ratio, database size, and
LLM usage. Collector health is exported per database (label `database`) in every mode: the
collector circuit breaker state (`pg_sage_collector_breaker_state`, 0 closed, 1 skipping,
2 dormant), `pg_sage_collector_breaker_skips_total{reason}` (`load`, `load_query_error`),
`pg_sage_collector_last_success_timestamp`,
`pg_sage_collector_category_unavailable{category}`,
`pg_sage_collector_persist_failures_total` and
`pg_sage_collector_query_store_failures_total`. pg_sage's own database time per hour by
the component its statement tag names is
`pg_sage_self_component_db_time_ms_per_hour{database,component}` (see
[sage_self_cost](findings.md#footprint-api)). Every `pg_sage_self_*` family (what pg_sage
itself costs, and its budgets) is listed under
[pg_sage's own metrics](#pg_sages-own-metrics).

---

## Own Footprint and Self-Configuration

pg_sage runs on the database it guards, so it meters what it costs, holds itself to declared
budgets, and sizes its own cadences and deadlines from what it measures. Code:
`internal/selfmonitor` (tag and exclusion), `internal/selfcost` (meter),
`internal/selfbudget` (budgets and the process watch), `internal/selfconfig` (derived
settings).

### The statement tag

Every pool and connection pg_sage opens goes through `selfmonitor.ConfigurePool` (or
`ConfigureConn`): it sets `application_name` to `pg_sage` (`pg_sage action:<decision id>`
for an executor action's session) and wraps the network connection with
`selfmonitor.TagConn`. The wrapper rewrites the protocol's Query and Parse messages, so
every statement from every code path, pgx's own included, carries the tag:

- The tag sits right after the statement's first keyword (`SELECT /* pg_sage */ ...`),
  or after the opening parenthesis of a parenthesized query, because PostgreSQL 18's
  `pg_stat_statements` drops comments in front of the first token. Each statement of a
  multi-statement Query gets its own tag; a leading pg_sage comment is moved there.
- The tag names the component that sent the statement (`/* pg_sage collector */`,
  `/* pg_sage sre:probe v1 */`), so the meter can charge each component.
- pg_sage does not hide from `pg_stat_statements`: a DBA sees and prices its work there.

pg_sage's own readers leave that work out of the workload they analyze with one rule in
SQL and in Go. `selfmonitor.StatementExclusionSQL` keeps a statement whose text has no
`pg_sage` tag (`NOT ILIKE '%pg_sage%'`) and no reference to the `sage` schema
(`selfmonitor.IsStatementText` is the Go twin); `selfmonitor.ActivityExclusionSQL` keeps
a `pg_stat_activity` session whose `application_name` is not pg_sage's
(`selfmonitor.IsApplicationName`). Reading a statistics view is not pg_sage's signature:
another monitoring agent's `SELECT ... FROM pg_stat_statements` stays in the workload.
Findings about pg_sage's objects are left out of advice; the three findings about its
cost (`sage_self_cost`, `sage_self_budget`, `sage_footprint`) are always kept. See
[which statements and findings are pg_sage's own](configuration.md#collection-analysis).

### The meter, the budgets and the three findings

Each analyzer cycle reads the tagged `pg_stat_statements` entries of its database through
the bounded catalog reader (`safety.query_timeout_ms` and `safety.lock_timeout_ms`) and turns the
change since the previous cycle into a rate per cycle of that database's own collector
(`selfcost.Cost`): database time (execution plus planning), calls and shared blocks,
rows read and written in `sage` tables, the `sage` schema's size, and the time per
component the tag names. The first cycle after a start, a statistics reset or a missing
`pg_stat_statements` leave a figure unknown: an open finding stays open and a metric is
left out, never reported as `0`.

| Finding | Measures | Budget (default) | Unit |
| --- | --- | --- | --- |
| [`sage_self_cost`](findings.md#sage_self_cost) | database time of pg_sage's statements | `analyzer.self_cost_budget_ms` (3000, 5% of one core at a 60 s cycle) | ms per collector cycle |
| [`sage_self_budget`](findings.md#sage_self_budget) | database time | `self_budget.db_time_ms_per_hour` (0: inherit `analyzer.self_cost_budget_ms` at the database's collector period) | ms per hour |
| | shared blocks hit or read | `self_budget.blocks_per_hour` (18,000,000) | 8 KB blocks per hour |
| | size of the `sage` schema, plus the database's share of a separate history store | `self_budget.storage_mb` (10240) | MB |
| | sidecar process CPU, once for all databases | `self_budget.cpu_ms_per_cycle` (600, 1% of one core at 60 s) | ms per global collector cycle |
| [`sage_footprint`](findings.md#sage_footprint) | `sage` schema's share of the database | `retention.sage_size_warning_pct` (10, and at least 256 MB) | percent |

`0` disables a budget. `sage_self_budget` names the top consumers: the five costliest
statements of the window and the busiest loops (`component:database`, busy time without
LLM waits). `GET /api/v1/self-cost` serves the same footprint per database
([Footprint API](findings.md#footprint-api)) and **Settings > General > Footprint** shows
it.

### pg_sage's own metrics

`/metrics` exports the meter and the budgets per database (label `database`) unless
noted:

| Family | Meaning |
| --- | --- |
| `pg_sage_self_db_time_ms_per_cycle` | database time of pg_sage's statements per collector cycle (ms) |
| `pg_sage_self_statements_per_cycle` | statement executions per collector cycle |
| `pg_sage_self_blocks_per_cycle` | shared blocks hit or read per collector cycle |
| `pg_sage_self_rows_read_per_cycle` | rows read from `sage` tables per collector cycle |
| `pg_sage_self_rows_written_per_cycle` | rows inserted, updated or deleted in `sage` tables per collector cycle |
| `pg_sage_self_schema_bytes` | size of the `sage` schema (tables, TOAST, indexes) in bytes |
| `pg_sage_self_db_time_budget_ms` | `analyzer.self_cost_budget_ms`, no label (absent when `0`) |
| `pg_sage_self_db_time_ms_per_hour` | database time per hour (ms) |
| `pg_sage_self_blocks_per_hour` | shared blocks per hour |
| `pg_sage_self_component_db_time_ms_per_hour{component}` | database time per hour by the component the tag names |
| `pg_sage_self_cpu_ms_per_cycle` | sidecar CPU per global collector cycle (ms), no label |
| `pg_sage_self_budget{resource}` | declared `self_budget` per resource, no `database` label |
| `pg_sage_self_budget_database{resource}` | budget a database inherits (`db_time` at its own collector period) |
| `pg_sage_self_budget_exceeded{resource}` | 1 when pg_sage used more than its budget of the resource (`cpu` has no `database` label) |
| `pg_sage_self_loop_busy_seconds_total{loop}` | wall time of pg_sage's loops, LLM waits excluded (counter; `database` only on a per-database loop) |
| `pg_sage_self_loop_runs_total{loop}` | runs of pg_sage's periodic loops (counter; `database` only on a per-database loop) |
| `pg_sage_self_loop_errors_total{loop}` | runs that failed or produced nothing, such as a skipped or failed collection (counter, 0 included; `database` only on a per-database loop) |
| `pg_sage_self_loop_last_success_timestamp_seconds{loop}` | Unix time the loop's last successful run ended (gauge, absent until the first success; `database` only on a per-database loop) |

[sage_self_cost](findings.md#sage_self_cost) and
[sage_self_budget](findings.md#sage_self_budget) describe the windows and labels in full.

### Derived settings (self-configuration)

For a few `derivable` keys the operator leaves unset, pg_sage derives a value per database
(`internal/selfconfig`, run at startup before any worker reads the key and then every
hour):

- **Keys.** `collector.interval_seconds` (live: its reader re-reads it every cycle),
  `safety.query_timeout_ms`, `sre.runways.sequence_interval_seconds`,
  `sre.detectors.temp_file_mb` and `sre.detectors.lwlock_waiters` (restart-bound: a value
  promoted while running is reported as pending restart and applies at the next start).
- **Evidence.** The cost rules read the collector's measured statement time (p90 of its
  cycles' statement time and of their slowest statement, and the statements a deadline
  cut off), cited with source `collector`; before the collector's first cycle they read
  timed probes of the catalog and `pg_stat_statements`, cited with source `probe`. The
  other rules read the sequence scan time, the temp-file rate and `max_connections`.
- **Bounds.** A value never goes past the product default in the direction that spends
  more or widens authority, except where a fixed ceiling is recorded with its reason.
- **Shadow and soak.** A new value is recorded in shadow and soaks for
  `self_config.soak_hours` (default 24); it is promoted only when the comparison with the
  active value's measured outcome is not worse.
- **The operator wins.** A key set in YAML, a fleet field or an API override is never
  derived; an admin can pin or unpin a derived value. Every derivation, promotion, hold
  and pin is recorded in `sage.config_derivation`. `self_config.enabled: false` turns it
  off.

The rules, their bounds and evidence are generated in
[derived settings](generated/derived-settings.md); `GET /api/v1/derived-settings` and the
Configuration page show each database's values.

### The performance gate

`TestPerfGate` (build tag `perfgate`, `cmd/pg_sage_sidecar`) runs the real runtime, every
component once per cycle, against a large synthetic catalog with pre-seeded `sage`
history, and fails the build on any offender. Its limits live in
`internal/testsupport/perfgate/budgets.go`: (A) no sequential scan of a `sage` table over
5,000 rows; (B) a 100 ms mean per statement and 3000 ms of pg_sage database time per
cycle; (C) at most 250 rows written per `sage` table per cycle; (D) 500 ms for the slowest
catalog statement, and none cut off by a timeout; (E) 1000 ms per API list endpoint; (F) at
least 50% HOT updates on a `sage` table updated 5 times or more; (G) 600 ms of sidecar CPU
per cycle; (H) the product meter within 10% (at least 5 ms) of the database time the gate
measures, and its cycle within 10% of the collector period. Pull requests run the small
scale and the nightly run the large one (`PG_SAGE_PERF_SCALE`). `TestPerfGateFirstLook`
times the catalog-only first look.

---

## Data Flow

1. **Collector** gathers `pg_stat_statements`, `pg_stat_user_tables`, `pg_stat_user_indexes` every 60s into `sage.snapshots` (in the metadata database with `history.store: meta`).
2. **Analyzer** runs rules every 600s, then the **tuning agent** if an LLM is usable.
3. The agent classifies the workload, detects cases and asks the model about each case
   with read-only tools.
4. The model answers with typed proposals; pg_sage generates and validates the SQL
   (optimizer admission and HypoPG, configuration gates, tuner hint checks, facts).
5. Each admitted proposal gets a confidence calibrated on the outcome ledger.
6. Findings are persisted to `sage.findings`.
7. **Case projection** combines findings, incidents, and action state into DBA
   cases and shadow-mode proof.
8. **Executor** queues, blocks, approves, or executes typed actions based on
   trust level, policy, evidence freshness, maintenance window, and guardrails.

---

## Schema Bootstrap

On startup, pg_sage acquires advisory lock `710190109` (`hashtext('pg_sage')`), then creates the `sage` schema and tables if they do not exist. See [SQL Reference](sql-reference.md) for the full schema.

---

## C Extension (Removed)

The C extension, frozen at v0.6.0-rc3, was removed from the repository. Its last source is
preserved at the `c-extension-final` tag. The sidecar needs only the catalog views,
`pg_stat_statements`, and optional extensions such as HypoPG and pg_hint_plan.
