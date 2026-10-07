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
  │     pg_sequences, pg_locks, pg_stat_replication, pg_stat_bgwriter,
  │     pg_stat_checkpointer, pg_stat_activity, pg_database_size()
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

Gathers snapshots every 60s (configurable) from 10+ catalog views. Stores raw data in `sage.snapshots` as JSONB. Uses a circuit breaker to back off during database crises.

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
metadata. These artifacts are shown in Cases and Actions so teams can review
schema work through their normal change-control process.

Incident playbooks follow the same typed-action model. Read-only diagnostics
can inspect blocker graphs, runaway queries, connection pressure,
WAL/replication state, standby conflicts, and vacuum pressure. Backend
cancel/terminate actions require exact PID evidence and approval. Sequence
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

Provider readiness uses adapters for Postgres, Cloud SQL, AlloyDB, RDS, and
Aurora. Adapters describe extension enablement paths, log access, managed
service limitations, and supported action families before policy evaluation
decides whether a specific database can execute, queue, or only observe.

### Alerting

Monitors new findings and routes notifications to Slack, PagerDuty, or custom webhooks based on severity. Supports quiet hours, cooldown periods, and per-severity routing rules. Event types: `finding_critical`, `action_executed`, `action_failed`, `approval_needed`, `query_rewrite_suggested`.

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
  pool; readers find the store from the pool they already hold
  (`histstore.Resolve`), so no reader can be wired to the wrong database.
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

Metrics endpoint on `:9187`. Exports findings count by severity, circuit breaker state, connection stats, cache hit ratio, database size, and LLM usage.

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
