# Configuration

pg_sage uses three configuration sources with the following precedence (highest wins):

1. **CLI flags** (`--pg-url`, `--config`, `--prom-addr`, `--meta-db`, `--encryption-key`)
2. **Environment variables** (`SAGE_DATABASE_URL`, `SAGE_LLM_API_KEY`, etc.)
3. **YAML config file** (`config.yaml`)
4. **Built-in defaults**

The sidecar validates a complete candidate before publishing a YAML reload.
Every field has a typed lifecycle: `live_policy` swaps an immutable policy
snapshot, `reconfigure` tears down and rebuilds its named runtime owner, and
`restart` remains pending until process restart. A `reconfigure` or
`live_policy` field whose owner does not run in the current mode is treated
as `restart`. In-flight work keeps its original snapshot.

**Adding, removing and changing databases without a restart.** In YAML fleet
mode, editing `databases` (or `defaults`) in the watched config file is
applied at once: a new entry starts a runtime, a removed entry is retired, a
renamed entry is a removal plus an addition. Per-database `trust_level`,
`execution_mode`, `executor_enabled` and `tags` apply in place to the running
database; any other per-database change (connection, credentials, pool size,
intervals, `llm_enabled`, `verify.io_capacity`) rebuilds only that database's
runtime. A retired runtime first lets in-flight actions finish (up to 60
seconds; new actions on it park and are retried by the next runtime), then
stops its workers, Sage SRE investigator, executor and pool, and its metrics
and LLM budget share disappear. A reload is all-or-nothing: an invalid file,
an invalid per-database trust level or execution mode, or a changed database
whose new connection fails is rejected and the running runtimes keep going
(fix the file and save again). A newly added database that cannot connect is
shown as failed, as at startup. Removing or reconnecting the control database
(the first database that started, which holds logins and standing policy)
still needs a restart. In meta-db mode `sage.databases` is the source of
truth: the managed-database API applies changes immediately, and rows added,
removed, disabled or changed by another replica or by SQL are picked up within
30 seconds with the same rules.

The generated [per-field lifecycle reference](generated/config-lifecycles.md)
is the authoritative list. Regenerate it from the typed registry with:

```bash
cd sidecar
go run ./cmd/gen_config_meta -lifecycle-only \
  -lifecycle-out ../docs/generated/config-lifecycles.md
```

---

## CLI Flags

```bash
./pg_sage --pg-url "postgres://user:pass@host:5432/db" --config config.yaml
```

| Flag | Description |
|---|---|
| `--pg-url` | PostgreSQL connection string (overrides YAML and env) |
| `--config` | Path to YAML config file |
| `--prom-addr` | Prometheus listen address (e.g., `0.0.0.0:9187`) |
| `--meta-db` | Metadata database connection string (PostgreSQL URL for fleet mode) |
| `--encryption-key` | Encryption key for sensitive config values |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SAGE_DATABASE_URL` | (none) | PostgreSQL connection string |
| `SAGE_LLM_API_KEY` | (none) | API key for Gemini or any OpenAI-compatible LLM |
| `SAGE_OPTIMIZER_LLM_API_KEY` | (none) | Separate API key for the optimizer model (optional) |
| `SAGE_API_KEY` | (none) | Legacy config field; the current web/API path uses session login cookies |
| `SAGE_TLS_CERT` | (none) | Legacy/reserved; terminate TLS at a reverse proxy |
| `SAGE_TLS_KEY` | (none) | Legacy/reserved; terminate TLS at a reverse proxy |
| `SAGE_PROMETHEUS_PORT` | `9187` | Port for Prometheus metrics |
| `SAGE_RATE_LIMIT` | `60` | Max requests per minute per IP on REST API |
| `SAGE_PG_MAX_CONNS` | `2` | Max PostgreSQL connections in pool |

---

## YAML Config File

Full example (see also `sidecar/config.example.yaml`):

```yaml
mode: standalone

postgres:
  host: your-instance-ip
  port: 5432
  user: sage_agent
  password: ${PGPASSWORD}       # env var expansion supported
  database: postgres
  sslmode: require
  max_connections: 2

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 600

trust:
  level: observation             # observation | advisory | autonomous
  maintenance_window: "weeknights"  # see "Maintenance windows" below
  ramp_start: ""                 # Auto-persisted on first start; set to override

llm:
  enabled: true                  # default; false turns every LLM feature off
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 30
  token_budget_daily: 500000
  optimizer:
    enabled: true                # default
    min_query_calls: 100         # ignore ad-hoc queries below this threshold
    max_indexes_per_table: 10    # skip tables already at this index count
    max_include_columns: 3
    max_new_per_table: 3
    over_indexed_ratio_pct: 80
    write_heavy_ratio_pct: 70
  optimizer_llm:                 # optional second model for optimizer
    endpoint: ""
    model: ""
    api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}

api:
  listen_addr: "0.0.0.0:8080"
  # Web UI and /api/v1 endpoints are session-authenticated.
  # The first local admin is bootstrapped automatically.

alerting:
  enabled: false
  check_interval_seconds: 60
  slack_webhook_url: ${SAGE_SLACK_WEBHOOK}
  pagerduty_routing_key: ${SAGE_PAGERDUTY_KEY}
  routes:
    - severity: critical
      channels: [slack, pagerduty]

prometheus:
  listen_addr: "0.0.0.0:9187"

retention:
  snapshots_days: 90
  findings_days: 180
  actions_days: 365
  explains_days: 90
  sage_size_warning_pct: 10

briefing:
  schedule: "0 6 * * *"         # cron expression
```

---

## Key Settings Reference

### Core

| Parameter | Default | Description |
|---|---|---|
| `mode` | `standalone` | `standalone` (one database), `fleet` (YAML `databases`), or `meta` (inferred from `--meta-db`). The former `extension` mode was removed with the C extension and is rejected at startup |
| `postgres.max_connections` | `2` | Connection pool size |
| `postgres.sslmode` | `prefer` | SSL mode (`disable`, `prefer`, `require`, `verify-ca`, `verify-full`) |

### Collection & Analysis

| Parameter | Default | Description |
|---|---|---|
| `collector.interval_seconds` | `60` | Seconds between snapshot collections |
| `analyzer.interval_seconds` | `600` | Seconds between analysis cycles |
| `analyzer.self_cost_budget_ms` | `3000` | Raise a `sage_self_cost` finding when pg_sage's own statements (the `pg_stat_statements` entries carrying the `/* pg_sage */` tag after their first keyword) use more than this many milliseconds of database time per collector cycle, measured over each analyzer cycle. `0` disables the finding; the `pg_sage_self_*` Prometheus metrics are exported either way. `0`-`3600000` |
| `rca.lock_chain_interval_seconds` | `60` | Seconds between lock-chain fast-path checks. Each check opens or updates the `lock_contention` incident (with the root blocker's pid, `backend_start` and query identity) and sends `incident_detected` without waiting for the analyzer cycle. `0` disables the fast path; otherwise `10`-`3600`. Escalation and auto-resolution still count analyzer cycles. Restart to change |
| `rca.stale_after_hours` | `24` | Hours an open incident may go without being re-detected before pg_sage resolves it (`resolved_by` `pg_sage:stale`). Incidents about an idle-in-transaction session also resolve once that session is gone (`pg_sage:subject_gone`). Resolutions of incidents last seen longer ago than this send no notification. `1`-`8760`, and at least `rca.dedup_window_minutes` |

### Trust & Actions

| Parameter | Default | Description |
|---|---|---|
| `trust.level` | `observation` | Autonomy ceiling: `observation`, `advisory`, `autonomous`. The trust ledger decides each class's level; this is the most it may use, never a grant |
| `trust.maintenance_window` | (none) | When autonomous MODERATE actions may run; see [Maintenance windows](#maintenance-windows). Unset or `never` closes the window |
| `trust.ramp_start` | (auto) | When pg_sage began observing the database. Auto-persisted on first start; set to override |
| `trust.ramp_safe_hours` | `192` | Minimum hours observed before pg_sage may propose a promotion to L2 (and to L3 for SAFE classes), 8 days, `1`-`8760`. A floor, never a grant. Actions that cannot be rolled back always wait at least `192` |
| `trust.ramp_moderate_hours` | `744` | Minimum hours observed before pg_sage may propose a MODERATE class's promotion to L3, 31 days, `1`-`8760`, at least `ramp_safe_hours`. A floor, never a grant. Actions that cannot be rolled back always wait at least `744` |

The trust model controls what pg_sage is allowed to do:

| Trust Level | Actions Allowed |
|---|---|
| `observation` | Cases and recommendations only |
| `advisory` | With `auto`, execute eligible typed SAFE actions; queue higher risk |
| `autonomous` | With `auto`, execute eligible typed SAFE/MODERATE actions; queue HIGH |

Execution mode is independent from trust: `manual` disables background
queueing and execution, `approval` queues supported actions, and `auto`
applies the table above. Trust never promotes `manual` to `auto`.

HIGH-risk actions always require manual confirmation regardless of trust level.
Plain CREATE/DROP/REINDEX and `VACUUM FULL` do not satisfy the typed background
contracts; concurrent or non-FULL forms are required.

### One trust system

Every action class pg_sage runs on its own initiative has one level per database in the
trust ledger: the incident remediations (see [Sage SRE earned autonomy](#sage-sre-earned-autonomy))
and the self-initiated classes, grouped by their goal:

| Family | Classes | A success is |
|---|---|---|
| `tuning` | `index_create`, `config_guc` (GUC), `autovacuum_tuning` (reloption), `query_hint`, `statistics` | an `improved` verdict |
| `hygiene` | `index_drop`, `vacuum`, `analyze`, `retention`, `reindex` | an `improved` verdict, or a `neutral` one that held (the action was not rolled back) |

The levels are the earned-autonomy levels: L1 writes the script (the default), L2 hands the
action to one-click approval, L3 runs it unattended (SAFE classes at any time, MODERATE ones
inside the maintenance window, one object at a time). Irreversible classes (`retention`)
never exceed L1. `trust.level`, `execution_mode`, the `tier3_*` flags and the standing policy
stay the ceiling: `advisory` never runs a MODERATE class unattended whatever its level.

**Evidence.** The verification verdicts of sage.action_outcome are the evidence:
`insufficient_evidence` and `unverifiable` count neither way. A `regressed` verdict, an
operator's rollback of the action or an operator's rejection of its approval item demotes the
class one level at once, records the cause in the history and notifies an operator through
the notification rules for failed actions (`action_failed`). The automatic revert of a
neutral index create (no gain) is not a demerit. A demerit already known when the level was
set does not demote it again.

**Promotion.** pg_sage proposes one level up when the class has (defaults, configurable under
`sre.autonomy.class_promotion`):
- L2: `min_successes_l2` (3) verified successes since its last demerit, and the database
  observed for at least `trust.ramp_safe_hours`;
- L3: `min_successes_l3` (10) successes since the last demerit, `min_success_rate_pct` (80%)
  of decided outcomes since then successful, and the database observed for at least
  `trust.ramp_moderate_hours` (`trust.ramp_safe_hours` for SAFE classes).

An admin approves every promotion (the Trust page, or the earned-autonomy API). The ramp is
only a floor: it never grants anything by itself.

**Existing configurations.** On the first start of a database under the unified ledger, the
level the time ramp had already given each self-initiated class is kept as a
**grandfathered** level (L3 when it ran unattended, L2 when it queued for approval), once.
Grandfathered levels demote like any other; the ramp elapsing later grants nothing. The
startup log explains the new meaning of `trust.level` and the ramp, and lists what was
grandfathered. `sre.autonomy.enforce: false` keeps the legacy behaviour (the ramp grants)
and is logged as a warning. Rollbacks of pg_sage's own changes and owner-declared retention
deletes are never withheld by the ledger.

The **Trust** page (and `GET /api/v1/trust?database=`) shows every database x family x class
with its level, effective level, evidence counts, last change and why, and the path to the
next level. MCP `sre_get_autonomy` carries the same grid as `trust`.

### Verifying actions

Every action pg_sage takes records, before it runs, what it expects: the queries it
targets, the metric that should move and by how much, and how that was predicted (`hypopg`,
`model`, `rule`, or `none`). Afterwards it records what it observed and a verdict:

| Verdict | Meaning | Trust and credit |
|---|---|---|
| `improved` | the predicted effect was observed (a drop: reads held for the whole cycle) | counts toward earned autonomy and value |
| `neutral` | measured, no change beyond the bars | neither |
| `insufficient_evidence` | too few calls, too few sampling intervals, or too noisy to tell | neither |
| `unverifiable` | nothing measurable, or no prediction | neither |
| `regressed` | a targeted query (or the targeted metric) got significantly worse | rolled back; counts against trust |

The targeted queries are compared on their call-weighted mean execution time, before and
after, with Welch's test over the sampling intervals in `sage.query_store`: a change counts
only when it is both significant and beyond its bar. With several targets each is tested
with a Bonferroni-corrected significance and the call-weighted pool decides. A first verdict
comes after `trust.rollback_window_minutes` once there are `verify.min_samples` calls; without
enough evidence the watch extends up to `verify.window_max_minutes`.

An index drop is watched for its business cycle, `verify.drop_window_hours`, and checked
from the start. Its definition is kept, and the index is re-created as soon as a query on
the table regresses or an active pg_sage hint names it (soft drop). VACUUM and ANALYZE are
checked right after they run (dead tuples, rows modified since analyze, relfrozenxid age
for VACUUM FREEZE).

Each executed action carries `verification_outcome` in `GET /api/v1/actions` and
`/api/v1/actions/{id}`; `GET /api/v1/action-outcomes?database=&class=&verdict=&since=&limit=` lists the
outcome ledger (`sage.action_outcome`).

| Parameter | Default | Description |
|---|---|---|
| `trust.rollback_window_minutes` | `15` | Minimum minutes before a first verdict (not for drops) |
| `trust.rollback_threshold_pct` | `10` | Regression bar on a target's call-weighted mean |
| `verify.min_samples` | `30` | Calls each window needs |
| `verify.min_gain_pct` | `20` | Gain bar for `improved` |
| `verify.window_max_minutes` | `4320` | Longest a non-drop action is watched for evidence |
| `verify.drop_window_hours` | `168` | Business cycle an index drop is verified over, `1`-`8760`. See [Fast elevation](#fast-elevation-dogfood-databases) |

### Maintenance windows

`trust.maintenance_window` and the standing policy's `maintenance_windows`
use one grammar. An invalid value is rejected when the config is loaded,
reloaded or saved.

| Form | Example | Meaning |
|---|---|---|
| Always | `always`, `24x7` | Every minute |
| Preset | `nights`, `weeknights`, `weekends`, `weekdays`, `business-hours`, `off-hours` | `daily 22:00-06:00`, `weekdays 22:00-06:00`, all day Sat-Sun, all day Mon-Fri, `weekdays 09:00-17:00`, `daily 20:00-08:00` |
| Days | `sat,sun`, `Mon-Fri`, `daily` | All day on those days (names are case-insensitive; `fri-mon` wraps) |
| Time range | `22:00-02:00`, `weekdays 01:00-05:00` | A range that ends before it starts wraps midnight and belongs to the day it starts on: `weeknights` includes Friday night into Saturday morning, not Sunday night |
| Cron | `0 2 * * *`, `0 2 * * 1-5 @30m` | Five fields with lists, ranges and steps. Each matching minute opens a one-hour window unless `@<duration>` (whole minutes, `@1m` to `@24h`) says otherwise |
| Time zone | `weekdays 01:00-05:00 America/Chicago` | A trailing IANA zone evaluates the window on that zone's clock, including DST. Without one, the process clock (UTC in the container) is used. `Local` is refused as ambiguous |

`never`, `off`, `none` and `disabled` are accepted only for
`trust.maintenance_window`; a policy always names at least one window.

### LLM

LLM features are on by default, but they call a provider only once `llm.endpoint` and
`llm.api_key` (or `SAGE_LLM_ENDPOINT` and `SAGE_LLM_API_KEY`) are set. Until then every
feature runs its deterministic path, no LLM request is made, and startup logs one line
saying the LLM is not configured. `llm.token_budget_daily` caps spend across the general
LLM features. Turning a feature on grants no autonomy: what an LLM proposes executes only
through the policy gate, trust level and execution mode, like any other recommendation.

| Parameter | Default | Description |
|---|---|---|
| `llm.enabled` | `true` | Master switch for every LLM feature; `false` is a hot kill switch |
| `llm.endpoint` | (none) | OpenAI-compatible chat completions endpoint |
| `llm.model` | (none) | Model name |
| `llm.api_key` | (none) | API key (supports `${ENV_VAR}` expansion) |
| `llm.timeout_seconds` | `30` | Timeout for LLM API calls |
| `llm.token_budget_daily` | `500000` | Maximum tokens per day across general LLM features; `0` means no cap |
| `llm.optimizer.enabled` | `true` | LLM index optimizer (HypoPG-validated, confidence-scored) |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new indexes per table per cycle |
| `llm.optimizer_llm.enabled` | `false` | Dedicated optimizer model; adds a second client with its own `token_budget_daily` |
| `advisor.enabled` | `true` | LLM configuration advisor (vacuum, WAL, connections, memory, rewrites, bloat) |
| `tuner.llm_enabled` | `true` | Let the query tuner ask the LLM for pg_hint_plan hints (YAML only) |
| `explain.enabled` | `true` | `POST /api/v1/explain`; adds an LLM narrative when an LLM is configured |
| `rca.narration_enabled` | `true` | Let the LLM rewrite the summary on `incident_detected` and `incident_escalated` notifications. The model can only read the incident's own evidence: its causal chain (`E#`), the results of the fixed read-only catalog probes run for it (`P#`, also readable with `get_probe_result`) and the deterministic causal-graph hypotheses (`H#`). It writes no SQL and has no database access. It answers with claims; each must cite evidence ids, and every number in a claim must appear in the evidence that claim cites. Budget per narration: 2 model turns, 1,024 output tokens per turn, about 16k input tokens, 20 s per turn; at most 3 narrations per persistence cycle within 45 s. Any failure (LLM off, budget, rate limit, timeout, malformed or uncited output) uses the deterministic summary, which is always labeled. `llm.enabled=false` is the hot kill switch and cancels narrations in flight |

### Web UI and API Authentication

The embedded web UI and `/api/v1/*` endpoints require a session login. On first
start, pg_sage creates `admin@pg-sage.local` and prints a one-time initial
password to stderr. Use `/api/v1/auth/login` for API scripts and keep the
returned `sage_session` cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
```

### Agent-native autonomy

The `policy`, `verify`, `clone`, `custodian`, `value`, and `mcp` sections
configure the closed-loop autonomy stack. MCP is an intent-level JSON-RPC
surface: it exposes policy, change-request, and evidence-ledger tools, never
raw SQL execution. Caller claims are recorded as untrusted input and do not
grant authority.

The default `unattended` policy profile permits explicitly bounded deadline
overrides for XID and disk emergencies. Use `staffed` for narrow maintenance
windows without deadline overrides. Clone-backed migration rehearsal defaults
to disabled (`clone.provider: none`) and stale clones are recommendation-only.

The standing policy document also carries three safety fields, all enforced:

- `refusal_set`: actions pg_sage never runs on its own. A refused action is
  queued for approval with reason `refused_by_policy` (the token is in the
  detail); an operator approval runs it. Tokens: `rls_change` (row-level
  security or its policies), `grant_expansion` (GRANT, ALTER ROLE, OWNER TO,
  ALTER DEFAULT PRIVILEGES), `major_upgrade` (major or extension upgrades),
  `non_dup_object_drop` (dropping a table, column, constraint, sequence,
  schema or replication slot; index drops are rebuildable and are not
  refused) and `unrollbackable` (rollback class `not_reversible` or
  `forward_fix_only`, unless the action is a retention delete on the column
  the owner declared as the contract's `retention.column`; see [Retention
  contracts](#retention-contracts)). Unknown tokens are rejected.
- `lock_duration_ceiling_ms`: caps `lock_timeout` for DDL that runs inside a
  transaction at the smaller of the ceiling and `safety.lock_timeout_ms`.
  `CONCURRENTLY` builds keep `safety.lock_timeout_ms`, because they wait on
  older transactions by design. `0` means no ceiling.
- `serialize_mode`: `park` (both profiles). When another writer holds the
  DDL lease for the same object, the action is parked with reason
  `ddl_conflict` and retried next cycle. A park is not a failure: it does not
  count toward the retry limit or the self-initiated rate limit. `queue` is
  accepted and currently behaves like `park` (the action waits for the next
  cycle, not for the lease holder).

#### Retention contracts

The only way pg_sage deletes user rows is a retention contract declared with the
MCP tool `declare_table_contract`. The owner names both the window and the column
whose age defines it; pg_sage never infers the column:

```json
{"table": "public.events", "append_only": true,
 "retention": {"interval": "90 days", "column": "ingested_at"}}
```

- `retention.column` is required whenever `retention.interval` is set. It must
  exist, not be dropped, and be `timestamptz`, `timestamp` or `date`. On a
  partitioned table a column that is not the partition key is accepted with a
  warning: each retention batch then scans every partition.
- A contract without a usable column parks (deletes nothing). The ledger reason
  shows pg_sage's suggested column, for example `retention column not declared;
  suggested: created_at — re-declare the contract with retention.column to
  enable deletes`.
- The first cycle is always a dry run. Deletion starts only after a dry run at
  least 24 hours old (and at most 7 days) for the same table (by OID), column
  (by attnum and type), contract version and window. Renaming or swapping the
  column, rebuilding the table, re-declaring the contract, or the eligible row
  count growing past 2x + 100 of the reviewed dry run starts a new dry run.
- Each batch deletes at most 1,000 rows under a `ROW EXCLUSIVE` table lock, after
  re-checking the column identity in the same transaction, and requires standing
  policy consent to the `retention` change class.

### Load admission for autonomous index builds

Autonomous `CREATE INDEX CONCURRENTLY` and custodian index proposals (for
example FK supporting indexes) start only when load evidence says the host is
quiet. pg_sage measures IO itself from Postgres, every minute, with no cloud
credentials:

- Data IO: `pg_stat_io` reads, writes and extends of relation data (PG16+;
  PG18 byte columns). On PG14/15, `pg_stat_database.blks_read` plus the
  buffers written by the checkpointer, bgwriter and backends
  (`pg_stat_bgwriter`), times `block_size`. `blks_read` also counts reads served
  by the OS page cache, so on PG14/15 the rate over-states device reads; the
  learned baseline compares the database with itself, which keeps the bias
  consistent.
- WAL: `pg_stat_wal.wal_bytes` (PG14+). PostgreSQL 13 and older have no IO
  evidence, so admission stays withheld.

A statistics reset, or any counter that goes backwards, discards that interval.
It is never read as a quiet period.

Admission uses one of two evidence modes:

| Mode | When | Admits when |
|---|---|---|
| `declared_capacity` | `verify.io_capacity` (standalone) or `databases[].verify.io_capacity` (fleet) is set | data and WAL throughput are at or below `safety.data_io_ceiling_pct` / `safety.wal_io_ceiling_pct` of the declared MiB/s |
| `learned_baseline` | no declared capacity and `verify.io_baseline_days` > 0 | after that many days of observation, current data and WAL rates are at or below the learned median (p50) |

Host CPU is used when a provider reader supplies it (Supabase with
`SAGE_SUPABASE_OBSERVABILITY_TOKEN`) and must be at or below
`safety.cpu_ceiling_pct`. When CPU is unavailable, admission is granted only
while the maintenance window is open (`trust.maintenance_window` and the
standing policy's windows, evaluated as the policy gate does).

| Parameter | Default | Description |
|---|---|---|
| `verify.io_baseline_days` | `7` | Days of observation before the learned baseline can admit. `0` disables the learned baseline |
| `verify.io_baseline_hours` | `0` | Hours of observation instead, `1`-`8760`. When set it takes precedence over `io_baseline_days` (even `0`). `0` uses `io_baseline_days`. See [Fast elevation](#fast-elevation-dogfood-databases) |
| `verify.io_sample_retention_days` | `14` | Days of rate samples kept in `sage.io_rate_sample`; the rolling baseline covers this window. Must be at least `io_baseline_days` |
| `verify.io_capacity.read_write_mbps` | (none) | Standalone only. Declared data read+write throughput in MiB/s |
| `verify.io_capacity.wal_mbps` | (none) | Standalone only. Declared WAL throughput in MiB/s |
| `databases[].verify.io_capacity` | (none) | Fleet: the same attestation per database. A fleet-wide `verify.io_capacity` is rejected |
| `safety.data_io_ceiling_pct` | `70` | Data IO ceiling, percent of declared capacity. Independent of `cpu_ceiling_pct` |
| `safety.wal_io_ceiling_pct` | `70` | WAL IO ceiling, percent of declared capacity. Independent of `cpu_ceiling_pct` |

Declared capacity is an operator attestation and overrides the learned
baseline. Declare only what the volume really provides (for example the
provisioned throughput of the EBS/PD/Azure disk). Meta-mode databases use the
learned baseline only.

Each decision records the evidence mode, rates, capacity or baseline in
`sage.decision.evidence.load_admission`. A withheld build is recorded once per
finding and reason in `sage.admission_withheld` instead of a failed action on
every cycle. `GET /api/v1/admission` (optionally `?database=`) and
`GET /api/v1/admission/{name}` report each database's mode, reason and
baseline progress; the dashboard shows them on the Actions page and in the
Overview provider-readiness tab.

### Sage SRE investigations

By default (`sre.automatic_start: true`), pg_sage investigates each open RCA incident of the
lock, connection or WAL family and each open `plan_regression` finding, once. Investigations
are read-only and bounded (at most 12 catalog probes and 120 s of active time each), so they
start by themselves; set `sre.automatic_start: false` to start them only from a case or the
API. With the LLM off, investigations still run and conclude deterministically. An investigation runs
the fixed read-only catalog probes of its family (connection and WAL investigations sample
twice, `sre.sample_interval_seconds` apart), matches them against the deterministic causal
graph and stores the result: the likely explanation, contributing factors, alternatives and
ruled-out explanations, each citing its evidence, the evidence that could not be collected,
and an operator step. Nothing is executed. "Inconclusive" is a normal outcome.

Four more families are investigated the same way: **checkpoint storms**, **temp-file
explosions**, **replication lag** and **LWLock contention**. Their triggers are the RCA
incidents for "checkpoints are occurring too frequently", temp files, replication conflicts and
replication lag (`replication_lag_increasing`), plus a deterministic detector that samples the
database once per trigger poll (only while `sre.automatic_start` is on). The detector opens one
episode at a time per family, with conservative default thresholds (`sre.detectors.*`, below):
3 or more requested checkpoints within 5 minutes that outnumber timed ones; 1 GiB of temp
files in this database within 5 minutes; or 8 or more backends waiting on one modeled LWLock
in 3 consecutive polls. A new episode of a family waits 30 minutes after the previous one
started.

Each episode is an incident, like an RCA incident: a `warning` row in `sage.incidents`
(signal `sre_checkpoint_storm`, `sre_temp_file_explosion` or `sre_lwlock_contention`) with the
measurement and its threshold as evidence. It appears in the Cases panel, sends the usual
incident notifications (detected, escalated, resolved) and links its investigation, which an
operator can review for earned autonomy. One investigation per incident: a new episode while
its incident is still open counts as another occurrence (the RCA engine escalates an incident
after `rca.escalation_cycles` occurrences), and the incident resolves itself
`rca.resolution_cycles` analyzer cycles after the episodes stop. An open RCA incident of the
same family that already has at least warning severity (for example "checkpoints are occurring
too frequently" from the logs) is used instead of a second incident. An incident an operator
resolves is not reopened by the same episode. With `rca.enabled: false` there is no incident:
the investigation keeps its own case (`sre:detector:<family>:<database>`). A checkpoint investigation
compares samples 6 sample intervals apart (30 s by default). The live-temp-file probe needs
`pg_monitor` (or superuser), and per-statement spills need `pg_stat_statements`. Without them,
the investigation reports the evidence as unavailable instead of guessing. Standby-side
evidence (paused replay, standby queries holding replay back) is only visible when pg_sage
monitors the standby itself.

**Model turn (on by default whenever an LLM is configured).** After the causal graph has
scored the hypotheses, the configured LLM (`llm.*`) reviews the result. It may only:

- reorder the graph's own open hypotheses (shown separately as "model ranking", never merged
  into the graph's scores);
- while the graph is inconclusive, ask for one more catalog probe with typed arguments and a
  one-line reason. The probe counts against the 12-probe ceiling, and the graph then
  diagnoses again;
- write up to 5 claims, each citing the evidence it rests on (shown as "model-generated
  narrative").

Every reply is checked: known node ids only, catalog probes with valid arguments, evidence ids
of this investigation, every number found in the cited evidence, and a 16 KiB size limit. A
reply that fails gets one repair attempt. If the repair fails too, or the model times out, is
rate limited or is over budget, the investigation ends with the deterministic result and a
`model_rejected` event that gives the reason. Before concluding, a verifier rechecks the ranking
and every claim against the stored evidence. When the graph has a conclusive root cause, it
always wins: if the model ranks another hypothesis first, nothing the model said is kept and a
`model_disagreed` event records the disagreement. Each investigation is limited to 2 model
turns, 16k input and 4k output tokens, and its 120 s active time. The daily allocation is
`llm.token_budget_daily`.

With no LLM configured, investigations run deterministically, and the sidecar logs once that
the model turn is unavailable and why. To turn the model turn off while an LLM stays
configured for other features, set `sre.llm.enabled: false`. Reasoning models (Gemini
2.5+/3, OpenAI o-series, DeepSeek R1) work. Their thinking gets its own allowance of 16k tokens
per investigation (8k per turn), separate from the 4k answer limit. A turn requests 2k answer
plus 8k reasoning tokens, and the daily allocation counts both. If a provider reports more
reasoning than allowed, the usage is recorded as reported and no further turn is started.

| Parameter | Default | Description |
|---|---|---|
| `sre.automatic_start` | `true` | Start a read-only investigation for each incident, plan regression and reactive detector episode (checkpoint storm, temp-file explosion, LWLock contention). `false`: start them only on request; investigations already stored are still resumed and retained. Restart to change |
| `sre.trigger_interval_seconds` | `15` | Seconds between checks for new triggers and pending investigations, `5`-`600` |
| `sre.sample_interval_seconds` | `5` | Seconds between the two samples connection and WAL investigations compare, `1`-`30` |
| `sre.evidence_retention_days` | `30` | Days a finished, unpinned investigation keeps its probe evidence. The delete leaves a tombstone, and the investigation is shown as "evidence deleted by retention" |
| `sre.timeline_retention_days` | `90` | Days a finished, unpinned investigation is kept at all (hypotheses, steps, event chain), leaving a tombstone. At least `sre.evidence_retention_days`, at most `3650` |
| `sre.llm.enabled` | `true` | Model turn in investigations, used whenever an LLM is configured. `false` keeps investigations deterministic. Restart to change |
| `sre.detectors.window_seconds` | `300` | Seconds over which checkpoint and temp-file growth is measured, `60`-`3600`. Keep it at least twice `sre.trigger_interval_seconds`; a shorter window cannot see growth between two polls, and the sidecar warns at startup |
| `sre.detectors.checkpoint_requested` | `3` | Requested checkpoints within the window that are a checkpoint storm (they must also outnumber timed ones), `1`-`1000` |
| `sre.detectors.temp_file_mb` | `1024` | MiB of temp files this database writes within the window that are a temp-file explosion, `1`-`1048576` |
| `sre.detectors.lwlock_waiters` | `8` | Backends waiting on one modeled LWLock class that count as contention, `1`-`10000` |
| `sre.detectors.lwlock_polls` | `3` | Consecutive trigger polls the waiters must hold before an episode opens, `1`-`100` |
| `sre.detectors.cooldown_minutes` | `30` | Minutes after an episode starts before a new episode of the same family can open, `1`-`1440` |

Pinned investigations (Pin in the Cases panel, or `POST .../pin`) and running ones are never
deleted. Where the data lives: the `sage.sre_*` tables are in the meta database when one is
configured and in the monitored database otherwise; triggers are read from the monitored
database's `sage.incidents` and `sage.findings`. Probes run with a 500 ms statement timeout, at
most one probe per database and four per sidecar, and return identities, counts and ages, never
query text. Evidence, hypotheses, events and exports are redacted before they leave the store
(connection URIs, credentials, bearer tokens, SQL literals and raw vectors are removed).

Reading investigations (any signed-in role):
`GET /api/v1/investigations?database=<name|all>`, and per database
`GET /api/v1/databases/{db}/investigations`, `.../{id}`, `.../{id}/events`,
`.../{id}/evidence/{evidence_id}`. Operators and admins can also
`GET .../{id}/export` (JSON, or `?format=markdown`) and `POST .../{id}/pin` or `/unpin`.
With MCP enabled, agents get the read-only tools `sre_list_incidents`,
`sre_get_investigation` and `sre_get_evidence`.

### Sage SRE approved actions

When an investigation concludes that one active backend is the root of a lock or
connection-pressure incident (a statement holding a lock that a DDL or other sessions queue
behind), pg_sage proposes one action: `pg_cancel_backend` of that backend. Nothing else is
ever proposed. Termination is never proposed, an idle-in-transaction holder is not proposed
(a cancel does not end an idle transaction; the investigation says so), and a prepared
transaction or contention spread across many sessions is "not proposed" with the reason.

The proposal is derived only from the investigation's cited evidence and a fresh sample of
the target. It names the exact backend: pid, backend start, query start, database, user,
query hash and query id. It also carries its repair contract (mitigation only, one backend,
the conditions recovery is checked against, what is never done) and the policy verdict. A
proposal never executes by itself:

1. With `sre.actions.request_approval: true`, a proposal the policy would allow gets exactly
   one item in the database's existing approval queue, and is sent to ChatOps channels.
   Operators can also request approval in the Cases panel or with the MCP tool
   `sre_request_execution`.
2. A human approves it, on the Actions page, in the Cases panel or with a chat button. The
   approval is single use and records who approved.
3. pg_sage samples the backend again and compares the whole identity. The evidence may be at
   most `sre.actions.max_evidence_age_seconds` old, and the signalling statement checks the
   identity once more. If the backend finished, changed, stopped blocking or is protected,
   nothing is signalled, and the investigation timeline records why.
4. After the cancel, pg_sage samples the blocking every `sre.actions.recovery_sample_seconds`
   until `sre.actions.recovery_samples` fresh samples decide: recovered (the target's wait
   edges cleared and lock waits fell), not recovered, or inconclusive. The result is recorded
   on the timeline and closes the action's verification. Value is credited only on recovery.

The policy gate decides as for any action. `trust.level` must be `advisory` or `autonomous`
(under `observation` the proposal shows why it is withheld), and the emergency stop and the
execution mode apply. An approved cancel is incident mitigation, so maintenance windows do
not delay it. Replication connections, pg_sage's own sessions and `pg_dump`,
`pg_basebackup` and `pg_restore` sessions are never targeted. Superuser sessions are not
protected by default, so list roles or applications that must never be cancelled.

| Key | Default | Meaning |
|---|---|---|
| `sre.actions.proposals` | `true` | Propose automatically for concluded lock and connection investigations. Operators can always propose explicitly. Restart to change |
| `sre.actions.request_approval` | `true` | Queue a proposal the policy allows for approval and notify ChatOps channels at once |
| `sre.actions.approval_ttl_minutes` | `15` | Minutes an approval request stays open, `1`-`60` |
| `sre.actions.max_evidence_age_seconds` | `5` | Oldest identity evidence a cancel may act on, `1`-`5` |
| `sre.actions.recovery_sample_seconds` | `40` | Seconds between recovery samples, `1`-`300` |
| `sre.actions.recovery_samples` | `3` | Fresh samples a recovery verdict needs, `3`-`20` |
| `sre.actions.recovery_deadline_minutes` | `30` | Minutes before an unproven recovery is reported inconclusive, `1`-`240`, at least the sampling span |
| `sre.actions.chatops_tolerance_seconds` | `300` | Maximum age and clock skew of a signed Slack callback, `30`-`900` |
| `sre.actions.protected_roles` | `[]` | Roles whose sessions are never targeted |
| `sre.actions.protected_applications` | `[]` | `application_name` values whose sessions are never targeted |

Routes: `GET .../investigations/{id}/proposals` (any role), and for operators and admins
`POST .../investigations/{id}/proposals` (propose) and
`POST .../investigations/{id}/proposals/{proposal_id}/request`. Approval and denial use the
existing `POST /api/v1/actions/{queue_id}/approve` and `/reject`. With MCP enabled, operators
and admins get `sre_propose_action` and `sre_request_execution`. Neither tool executes.

**Approving in Slack or Telegram.** Approval requests are `approval_needed` notifications,
so add a notification rule for that event to the channel.

- *Slack*: in the channel's config, set `interactive: "true"` and the Slack app's
  `signing_secret`, plus `team_id` to accept only your workspace. Set the app's
  Interactivity Request URL to `https://<sidecar>/api/v1/chatops/slack/<channel id>`.
  Requests are verified with Slack's v0 signature within
  `sre.actions.chatops_tolerance_seconds`.
- *Telegram*: create a channel of type `telegram` with `bot_token`, a numeric `chat_id` and a
  `webhook_secret` (letters, digits, `_` and `-`). Register the webhook with
  `setWebhook?url=https://<sidecar>/api/v1/chatops/telegram/<channel id>&secret_token=<webhook_secret>`.
  Only button presses from that chat are accepted.

A chat user can decide only after an admin maps them to a pg_sage user:
`POST /api/v1/chatops/identities` with `{"provider": "slack", "team_id": "T...",
"external_user_id": "U...", "user_id": 7}` (Telegram: the numeric user id, no `team_id`).
The pg_sage user's current role must be operator or admin. Each callback is processed once,
and the decision goes through the same approval as the browser, attributed to the mapped
user. Bot tokens, signing secrets and webhook secrets are sealed at rest like other channel
secrets.

#### Runways and pre-incident investigations

A runway is the time left before a hard limit. Every `sre.runways.interval_seconds`, pg_sage
samples the series a limit is approached along into `sage.runway_samples`: the XID and
multixact counters, the WAL position, the WAL each replication slot retains, the size of the
databases, disk usage (databases plus `pg_wal`, which needs `pg_monitor`) and the sequences
nearest their limit. It fits a straight line through the last `sre.runways.lookback_hours` of
each series and projects when the series reaches its limit:

| Runway | Limit | Finding (category) | Investigation |
|---|---|---|---|
| XID or multixact age | the wraparound warning limit (2^31 - 1 - 40,000,000) | `forecast_wraparound_runway` (`xid`, `mxid`) | wraparound runway |
| A table 1.25 times past its freeze maximum | its effective `autovacuum_freeze_max_age` (the table's own setting when lower) | `forecast_wraparound_runway` (the table) | wraparound runway |
| Disk usage | `forecaster.disk_capacity_bytes` (only when you declare it; `0`, the default, means undeclared: nothing auto-detects capacity, so there is no disk runway and no disk-full credit) | `forecast_wal_runway` (`disk`) | disk/WAL runway |
| WAL a slot retains | `max_slot_wal_keep_size` when it is set, else the WAL custodian's 10 GiB ceiling | `forecast_wal_runway` (`slot:<name>`) | disk/WAL runway |
| A sequence's last value | the lower of its maximum and its owning integer column's maximum | `forecast_sequence_runway` (the sequence) | sequence runway |

A projection needs `sre.runways.min_samples` samples over at least
`sre.runways.min_span_minutes`. Disk and slot projections also need a steady trend (a line
that fits, r² at least 0.5), so a sawtooth never alarms. A runway inside its horizon opens a
forecast finding (critical inside the critical horizon) and, with `sre.runways.investigate`,
one read-only pre-incident investigation per finding and severity, whatever
`sre.automatic_start` says. A finding resolves when its runway clears. A standby samples
nothing (it is read-only).

A pre-incident investigation explains what drives the runway. Wraparound: a session,
prepared transaction or replication slot holding the xmin horizon, busy, disabled or
cancelled autovacuum (cancellations are read from logged incidents, so with no log source
their absence proves nothing), or a surge in XID use. Disk/WAL: an inactive slot, a slow
consumer, a failing archiver, a write surge or database growth. Sequences: which limit binds
(the sequence's type, a narrower owning column, or an explicit `MAXVALUE`). A concluded
investigation lists the existing custodian action that addresses it (the freeze custodian's
`VACUUM (FREEZE)` or blocker response, the WAL custodian's bound or its escalation plan) with
the standing policy gate's verdict. The investigation never executes it: the custodian
workers submit their proposals on their own schedule under your autonomy settings.
Sequences have no custodian action, because widening a column rewrites the table; the
investigation says so and gives the manual step.

| Parameter | Default | Description |
|---|---|---|
| `sre.runways.enabled` | `true` | Sample runways and open forecast findings |
| `sre.runways.investigate` | `true` | Open a read-only pre-incident investigation per runway finding and severity |
| `sre.runways.interval_seconds` | `60` | Seconds between samples, `15`-`3600` |
| `sre.runways.sequence_interval_seconds` | `600` | Seconds between sequence samples, `15`-`86400`. Sequences are consumed slowly and are the costliest series to read (one lock per sequence read), so they are read less often, through a separate 2 s background budget. The effective period is never shorter than `interval_seconds` and never so long that `min_samples` no longer fit in `lookback_hours` |
| `sre.runways.lookback_hours` | `6` | Hours a trend is fitted over, `1` to `sample_retention_hours` (at most `168`). Pre-incident investigations read the same window |
| `sre.runways.min_samples` | `10` | Fewest samples a projection needs, `3`-`1000` |
| `sre.runways.min_span_minutes` | `30` | Shortest span of samples a projection needs |
| `sre.runways.wraparound_horizon_hours` | `336` | Wraparound runway horizon (14 days) |
| `sre.runways.wraparound_critical_hours` | `72` | Critical wraparound runway |
| `sre.runways.disk_horizon_hours` | `72` | Disk and slot runway horizon |
| `sre.runways.disk_critical_hours` | `24` | Critical disk and slot runway |
| `sre.runways.sequence_horizon_days` | `30` | Sequence runway horizon |
| `sre.runways.sequence_critical_days` | `7` | Critical sequence runway |
| `sre.runways.sample_retention_hours` | `48` | Hours samples are kept, `lookback_hours` to `720` |

Changing these needs a restart.

### Sage SRE earned autonomy

pg_sage keeps an autonomy level per incident family and action class (for example
`wal_retention` / `wal_bound`, `wraparound_runway` / `freeze`). The level applies to actions
pg_sage starts on its own for an incident family, custodians included. Operator-approved
actions and read-only diagnostics are not restricted.

| Level | Meaning |
|---|---|
| L0 | Observe only |
| L1 | Diagnose and write the script; never executes (default for every known pair that was not autonomous before) |
| L2 | Hand off for one-click approval (a finding plus an approval-queue item) |
| L3 | Execute a reversible, single-object action inside the standing-policy window, then notify |

L4 is never reached. Irreversible classes (slot drop, backend terminate, schema change,
sequence migration, anything unclassified) never go above L1, and mitigation-only classes
(`backend_cancel`, `wal_bound`) and `config_guc` never go above L2. Operator trust settings
and the standing policy stay the outer bound: the ledger can only restrict them.

**Carried-over autonomy.** The ledger gates new autonomy and keeps what pg_sage already
had. When a database's ledger is first set up, four pairs are seeded at the level that
database's trust and execution settings already let run unattended:
- the custodian freeze and autovacuum tuning (`wraparound_runway`);
- the WAL bound (`wal_retention`);
- load-admitted index creation (`plan_regression`).

Settings with execution mode `auto` and the matching tier enabled give L3. Anything else
gives the default L1. The view shows these as "carried over", with the decision that
granted them. Irreversible classes are never carried above L1. A carried level does not
depend on promotion evidence. The downgrade signals below still cap it, and it resumes by
itself when they clear. An operator downgrade ends the carry-over, and the pair then has to
earn its level back with evidence.

**Approved actions and outcomes.** M5's `cancel_backend` is approval-only in the ledger: it
is mitigation-only, never above L2 and never carried over. Each approved cancel whose
recovery pg_sage verified is recorded as an L2 outcome of its family (`lock_blocking` or
`connection_pressure`), and a cancel that failed or did not recover is recorded as not
recovered. Executed custodian actions are recorded as well: an L3 execution as an L3 outcome
and a mandatory deadline override at L1, which is never promotion evidence. A harmful outcome
of any of them is a family safety regression. Runbook action proposals name their family and
autonomy class.

**Mandatory deadlines.** A critical XID or disk deadline that the standing policy lets
override (a red wraparound freeze, for example) is not held back by the ledger level or by
a downgrade, because waiting risks an outage. The emergency stop and the rest of the gate
still apply. The ledger records each such action once per object and deadline while the
sidecar runs.

**Promotion.** pg_sage proposes one level at a time, and only an admin approves; pg_sage, the
system and MCP clients cannot. The evidence is checked again at approval time, and a proposal
expires after `proposal_ttl_hours`. Promotion to L2 requires all of the following:

- PGIncidentBench top-1 accuracy of at least 80% over at least 10 runs, mechanism precision
  of at least 90%, and no forbidden actions, on every gated arm for the family. The report
  must be at most 30 days old.
- At least 30 days of shadow reviews, with at least 20 reviewed packets and at least 95% of
  them accepted.
- No safety violations in the family in the last 30 days.

Promotion to L3 also needs a Safe Pass rate of at least 95%, and at least 95% on game days if
any have run. It also needs at least 50 verified recoveries at L2, and the pair must never
have had a harmful outcome. These are the spec's thresholds and the defaults; the timing,
volume and accuracy values can be lowered under `sre.autonomy.promotion` (see
[Fast elevation](#fast-elevation-dogfood-databases)). The report age (30 days) and the
minimum bench run counts (10) are fixed.

**Downgrade.** At authorization time, any of the following caps an L2 or L3 pair at L1. The
change is logged once per transition.

- The database's SLO engine (`sre.slo`) reports an SLO burning its error budget at the page
  rate (database proxy SLOs included), or a
  registered app SLO's state is unknown. An unknown proxy SLO does not count, since it is
  often unknown for structural reasons (no standbys, no log access).
- The HA role is not primary, safe mode is on, or the role changed in the last
  `failover_cooldown_minutes`.
- The action's evidence is older than `max_evidence_age_seconds`.
- Another pg_sage action holds or recently touched the same object.
- The family had a harmful or unsafe outcome in the last `safety_window_days`.

A harmful or unsafe outcome also demotes every earned class of its family to L1 durably,
and re-promotion needs the evidence again. A carried-over pair is instead capped for
`safety_window_days`. Operators can downgrade a pair or a whole family at
any time.

| Parameter | Default | Description |
|---|---|---|
| `sre.autonomy.enforce` | `true` | The ledger restricts self-initiated incident-family actions. `false` returns them to the trust ramp; the sidecar warns at startup |
| `sre.autonomy.bench_results_path` | `""` | PGIncidentBench JSON report, or a directory searched three levels deep, ingested at startup and hourly. Point it at the CI `pgincidentbench/` artifact to read the core, M6 reactive and M6 runway shard reports; each family reads the newest report that scored it. A `<report>.sigstore.json` bundle is verified; a report for another pg_sage build is refused. Not needed for the reports a release ships. Empty: upload through the API |
| `sre.autonomy.evaluate_interval_minutes` | `60` | Minutes between promotion evaluations, `5`-`1440` |
| `sre.autonomy.reconcile_interval_seconds` | `60` | Seconds between recording live outcomes, `10`-`3600` |
| `sre.autonomy.max_evidence_age_seconds` | `300` | Older evidence caps an action at L1, `5`-`3600` |
| `sre.autonomy.concurrency_window_minutes` | `15` | A same-object action this recent caps at L1, `1`-`1440` |
| `sre.autonomy.safety_window_days` | `30` | Days a harmful outcome caps its family, `1`-`365` |
| `sre.autonomy.failover_cooldown_minutes` | `30` | Minutes after a role change autonomy stays at L1, `1`-`1440` |
| `sre.autonomy.proposal_ttl_hours` | `168` | Hours a promotion proposal waits for an admin, `1`-`720` |
| `sre.autonomy.report_retention_days` | `90` | Days a stored bench or game-day report is kept after it was ingested, `30`-`3650`. Never deleted: a report that is the evidence of a current autonomy level or a pending promotion, and the newest report of each family (per source, and per database for game days). Pruned with the other retention rules, in the database that holds the ledger (the meta database when one is configured) |
| `sre.autonomy.promotion.shadow_window_hours` | `720` | Hours of shadow reviews (from the first) a family needs for L2; reviewed and accepted packets are counted in this window. `1`-`8760` |
| `sre.autonomy.promotion.shadow_min_reviewed` | `20` | Reviewed packets needed inside the shadow window for L2, `3`-`1000` |
| `sre.autonomy.promotion.shadow_min_accepted_pct` | `95` | Operator-accepted share of reviewed packets for L2, `50`-`100` |
| `sre.autonomy.promotion.bench_min_top1_pct` | `80` | PGIncidentBench top-1 on every gated arm for L2, `50`-`100` |
| `sre.autonomy.promotion.bench_min_precision_pct` | `90` | PGIncidentBench factual (mechanism) precision on every gated arm for L2, `50`-`100` |
| `sre.autonomy.promotion.min_safe_pass_pct` | `95` | Safe Pass on bench fault programs and game days for L3, `50`-`100` |
| `sre.autonomy.promotion.min_live_recoveries` | `50` | Verified live L2 recoveries a pair needs for L3, `1`-`10000` |
| `sre.autonomy.game_days.enabled` | `false` | Run PGIncidentBench fault programs on a disposable clone |
| `sre.autonomy.game_days.interval_hours` | `168` | Hours between scheduled game days, `24`-`2160` |
| `sre.autonomy.game_days.local_dsn` | `""` | Development fallback when `clone.provider` is `none`, for game days and local bench runs. A monitored or the metadata database is refused |
| `sre.autonomy.game_days.families` | `[]` | Families to exercise. Empty: every family the bench covers |
| `sre.autonomy.canary.canary_instances` | `1` | Databases changed and verified before a fleet rollout widens, `1`-`10` |
| `sre.autonomy.canary.regression_limit_pct` | `10` | Regression that halts and rolls back a rollout, `0`-`100` |
| `sre.autonomy.canary.settle_seconds` | `60` | Seconds to wait after each change before measuring it, `0`-`3600` |

Game days run only the deterministic causal-graph arm, so they spend no LLM tokens. A
forbidden action on a game day is recorded as a safety violation. The clone DSN is never
stored.

#### Bench evidence: signed release reports and local runs

L2 needs a PGIncidentBench report for the family. You do not have to copy CI artifacts:

- **Shipped with every release.** The release workflow runs PGIncidentBench on the tagged
  commit, signs each shard report keyless with Sigstore (cosign, GitHub OIDC identity of
  the pg_sage `ci.yml` workflow; no key to manage) and ships the reports in the image at
  `/usr/share/pg_sage/bench` and in the release archives in `bench/` next to the binary.
  They are also release assets (`pgincidentbench-<shard>.json` with
  `.json.sigstore.json`). The sidecar ingests them at startup and hourly, verifying the
  signature offline against the Sigstore trusted root embedded in the binary, so an
  air-gapped install has them too.
- **Bound to the build.** A report names the pg_sage version and commit it scored. A
  report for another build is refused, and stored reports of an older build stop
  counting after an upgrade (until the new release's report is ingested). A signature
  must name the same commit as the report.
- **Run bench locally.** On the Autonomy page an admin can run the fault programs of a
  family on a disposable clone (`clone.provider` `dle` or `snapshot`, else
  `sre.autonomy.game_days.local_dsn`; game days need not be enabled). A monitored
  database or the metadata database is refused. The report counts only for the
  families it covered, marked "local run", and repeats every scenario until each family
  has the promotion bar's sample size (`n >= 10`).
- **Operator reports.** A report uploaded through the API or found under
  `bench_results_path` without a bundle works as before, marked "unsigned
  (operator-provided)". With a `<report>.sigstore.json` bundle next to it, it is
  verified, and a bundle that does not verify refuses the report.

The Path to next level panel shows each family's report and where it came from. To check
a downloaded report by hand: `pg_sage bench verify --commit <sha> pgincidentbench.json`
(the bundle next to it as `pgincidentbench.json.sigstore.json`).

The fleet canary is started by an admin from a verified action on one database. On every
other database, it applies the same statement only where that database has its own open
finding with the same SQL, and only through the approval path. It verifies the canary
databases first, then widens. It halts and rolls back everything in reverse order on a
failed verification or a regression above the limit.

API, under `/api/v1/sre/autonomy`. Every route takes `?database=<name>`.

The ledger is per database: each database earns its own levels from its own shadow reviews,
live outcomes, game days and safety record, and its own failover cooldown and carry-over
apply. PGIncidentBench reports are about pg_sage itself and are shared by every database of
the deployment. A level stored before the ledger was per database applies to every database
at that level until the database changes it. Only a verified success counts as a live
recovery: an action that is never verified is recorded as `unverified` after 24 hours and
earns nothing.

- Viewers: `GET` the root, `/history`, `/proposals`, `/game-days`, `/rollouts` and
  `/rollouts/{id}`.
- Operators: `POST /evaluate` (returns the proposals created and, for every other pair,
  why not: each unmet check with how to meet it), `/proposals/{id}/reject`, `/downgrade`,
  `/reviews` (accept or reject a concluded or inconclusive investigation, with an optional
  `note` and `actual_root_cause`; it also records the investigation outcome) and
  `/outcomes` (harmful or safety_violation).
- Admins: `POST /proposals/{id}/approve`, `/bench-results`, `/game-days` and `/rollouts`.

The UI page is **Advanced > Earned autonomy**: **Evaluate now** and a **Path to next
level** checklist per family and class. Reviews are recorded with **Accept diagnosis** /
**Reject diagnosis** on a finished investigation in **Cases**. With MCP enabled, agents get
`sre_get_autonomy`, and operators also `sre_downgrade_autonomy`, `sre_review_investigation`
and `sre_evaluate_autonomy`. There is no approval tool. Only a review by a person (UI or
REST) counts as shadow evidence: a review an agent records through MCP is kept, says that
it does not count, and never replaces a person's review of the same investigation.

### Fast elevation (dogfood databases)

pg_sage earns trust before it acts, and by default that takes weeks: an 8-day ramp for SAFE
actions, 31 days for MODERATE ones, a 7-day IO baseline before autonomous index builds, and a
30-day shadow record and 50 verified recoveries before earned autonomy reaches L3. On a
database you are dogfooding you can shorten all of it to hours. Every elevation setting is
configurable, the spec value is the default, and each has a minimum: no setting accepts `0`
to skip its check.

Use this only where you accept the risk:

<!-- fast-elevation-profile:start -->
```yaml
trust:
  ramp_safe_hours: 1              # propose L2 after 1 hour observed (spec: 192)
  ramp_moderate_hours: 4          # propose MODERATE L3 after 4 hours (spec: 744)
verify:
  io_baseline_hours: 2            # learned IO baseline after 2 hours (spec: 7 days)
  drop_window_hours: 2            # verify an index drop over 2 hours (spec: 168)
sre:
  autonomy:
    evaluate_interval_minutes: 5  # propose promotions every 5 minutes (default: 60)
    promotion:
      shadow_window_hours: 4      # 4 hours of shadow reviews (spec: 720)
      shadow_min_reviewed: 3      # 3 reviewed packets in that window (spec: 20)
      min_live_recoveries: 3      # 3 verified L2 recoveries for L3 (spec: 50)
```
<!-- fast-elevation-profile:end -->

The profile lowers time and volume only. The accuracy bar stays at the spec: 80% top-1,
90% precision, 95% accepted packets and 95% Safe Pass. Your own `trust.level`,
`tier3_*`, execution mode and maintenance window still decide what may run at all.

Some limits stay fixed and cannot be configured:
- Actions that cannot be rolled back (`not_reversible`, `forward_fix_only`,
  `application_rollback`, `mitigation_only`, `not_applicable` or undeclared) always wait
  the full 8- or 31-day ramp, however short the configured ramp is.
- Irreversible classes never go above L1, and L4 is never reached.
- The emergency stop always wins, and the policy gate and your trust settings stay the
  outer bound.
- An admin still approves every promotion.
- The downgrade signals still apply. A harmful outcome still demotes the family for
  `safety_window_days`.

Fast elevation is never silent. At startup the sidecar logs a `FAST ELEVATION` WARN line for
each setting below the spec, including a shorter `io_baseline_days`, `safety_window_days`
or `evaluate_interval_minutes`. The autonomy API returns them as `fast_elevation`, and
**Advanced > Earned autonomy** shows a "Fast elevation" badge that lists them. These keys
are YAML-only and need a restart; the config API refuses them.

To approve a promotion quickly, review investigations in **Cases**, press **Evaluate now**
on the Earned autonomy page (or `POST /api/v1/sre/autonomy/evaluate` as an operator), then
approve the pending promotion as an admin (or `GET /api/v1/sre/autonomy/proposals` and `POST
/api/v1/sre/autonomy/proposals/{id}/approve`).

### Retention

| Parameter | Default | Description |
|---|---|---|
| `retention.snapshots_days` | `90` | Days to retain snapshot data |
| `retention.findings_days` | `180` | Days to retain resolved findings |
| `retention.actions_days` | `365` | Days to retain action log entries |
| `retention.explains_days` | `90` | Days to retain EXPLAIN plan captures |
| `retention.sage_size_warning_pct` | `10` | Raise a `sage_footprint` finding when pg_sage's own tables (the `sage` schema, with TOAST and indexes) exceed this percent of the database size. `0` disables the check. |

Catalog snapshots (tables, indexes, sequences, foreign keys, partitions,
queries, `pg_stat_io`, configuration) are stored compactly: a full row (keyframe) at
most every 6 hours, and in between only what changed. Reads return every snapshot
exactly as collected, through `sage.snapshot_data(data, base_id)`. Retention keeps a
keyframe as long as a retained row is built on it, so the oldest kept rows can be up
to 6 hours older than `snapshots_days`. Rows written by earlier versions stay readable
and age out with `snapshots_days`; nothing is rewritten.

> **Complete reference:** See `sidecar/config.example.yaml` for all
> configuration fields including `safety`, `alerting`, `auto_explain`,
> `forecaster`, `tuner`, `advisor`, and `api` sections.
