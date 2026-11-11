# Architecture

pg_sage is a Go sidecar that connects to any PostgreSQL 14-17 over the network. The former C extension was removed (see the `c-extension-final` tag).

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
  └── Prometheus       [:9187]  Metrics endpoint
```

---

## Component Details

### Collector

Gathers a snapshot every 60s (configurable) from the catalog and statistics views, under a
circuit breaker that skips cycles while the database is in trouble (active client sessions
over `max_connections` above `safety.cpu_ceiling_pct`). A cycle reads the catalog categories
in order (`catalogSteps`: `queries`, `tables`, `indexes`, `partitioned_indexes`,
`foreign_keys`, `system`, `locks`, `sequences`), then `replication` and the extra categories
(`extraSteps`: `io`, `prepared_xacts`, `partitions`, `config_data`).

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
`EXPLAIN`, maintenance, statistics resets and backup `COPY ... TO` are not workload.

Each snapshot holds one row per category: `queries`, `tables`, `indexes`,
`partitioned_indexes` (the partitioned tables' own indexes, relkind `I`), `foreign_keys`,
`system`, `locks`, `sequences`, `replication`, `io` (PostgreSQL 16 and later), `partitions`,
`prepared_xacts` (this database's two-phase-commit transactions from `pg_prepared_xacts`) and
`config_data` (the advisor's settings, reloptions, connection and WAL snapshot, collected only
when an advisor will consume it). A category that could not be read in a cycle is
**unknown, not empty**: it is listed in the snapshot's `unavailable` map with the error, no
row is written for it, and rules that would read its absence as a fact stay quiet for that
cycle. Only the `system` category, the snapshot's spine, fails the whole snapshot. A category
pg_sage does not collect (`io` below PostgreSQL 16, `config_data` without an advisor) is not
collected, which is different from unavailable.

The `tables`, `indexes`, `foreign_keys`, `partitions` and reloptions reads leave out temporary
relations (`relpersistence = 't'`: other sessions' and pg_sage's own), which no other session
can vacuum, analyze or index; unlogged tables are still collected.

`queries` ranks this database's top 2 x `collector.max_queries` statements on their
`pg_stat_statements(false)` counters, without texts. `pg_stat_statements(true)` loads the
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

The catalog categories and the extras (`io`, `prepared_xacts`, `partitions`, `config_data`)
share one retry budget per cycle: a read that hit a statement or lock timeout is read once
more, and at most two reads are retried per cycle. `config_data` is all or nothing: if any of
its sub-reads (`pg_settings`, `reloptions`, `connection_states`, `wal_position`, `extensions`,
`connection_churn`) fails or returns a row that does not scan, the whole category is
unavailable and the reason names the sub-read. On a hot standby the WAL position is the last
WAL received (or replayed), since `pg_current_wal_lsn()` fails there.

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
| Replication | replication_lag, inactive_slot |

Each rule produces findings with severity (critical/warning/info), recommended SQL, and rollback SQL.

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
actions. Small table-bloat cases can propose guarded `VACUUM`; IO-saturated
cases are blocked to script/review output; XID wraparound cases diagnose oldest
`backend_xmin` holders; concurrent reindex handles index-bloat remediation;
and high-blast-radius table bloat produces reviewed online remediation plans
instead of `VACUUM FULL` by default.

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
includes case projection, shadow report, action queue, provider readiness,
findings, snapshots, config, forecasts, query hints, alerts, emergency stop,
and fleet management. UI and `/api/v1/*` routes are session-authenticated.

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

Monitors new findings and routes notifications to Slack, PagerDuty, or custom webhooks based on severity. Supports quiet hours, cooldown periods, and per-severity routing rules. Event types: `finding_critical`, `action_executed`, `action_failed`, `approval_needed`, `query_rewrite_suggested`, `incident_detected`, `incident_escalated`, `incident_resolved`, `custodian_escalation` (critical: a red custodian proposal exhausted its failure backoff, raised once per failure streak and recorded as a `custodian_backoff` decision), `fact_expired` (a confirmed binding fact expired and no longer binds; see [facts](facts.md#expiry-and-re-verification)).

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

Detects and uses `auto_explain` (if available) to capture EXPLAIN plans for slow queries. Stores plans for optimizer and diagnostic use.

### Forecaster

Analyzes historical trends to predict disk growth, connection exhaustion, sequence depletion, and cache ratio degradation. Generates proactive findings before problems occur.

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

Metrics endpoint on `:9187`. Exports findings count by severity, the LLM circuit breaker
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
[sage_self_cost](findings.md#footprint-api)).

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
