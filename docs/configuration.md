# Configuration

pg_sage uses three configuration sources with the following precedence (highest wins):

1. **CLI flags** (`--pg-url`, `--config`, `--prom-addr`, `--meta-db`, `--encryption-key`)
2. **Environment variables** (`SAGE_DATABASE_URL`, `SAGE_LLM_API_KEY`, etc.)
3. **YAML config file** (`config.yaml`)
4. **Built-in defaults**

The sidecar validates a complete candidate before publishing a YAML reload.
Every field has a typed lifecycle: `live_policy` swaps an immutable policy
snapshot, `reconfigure` tears down and rebuilds its named runtime owner, and
`restart` remains pending until process restart. Fleet database records use
their dedicated lifecycle API. In-flight work keeps its original snapshot.

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
  enabled: false
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 30
  token_budget_daily: 500000
  optimizer:
    enabled: false
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
| `rca.lock_chain_interval_seconds` | `60` | Seconds between lock-chain fast-path checks. Each check opens or updates the `lock_contention` incident (with the root blocker's pid, `backend_start` and query identity) and sends `incident_detected` without waiting for the analyzer cycle. `0` disables the fast path; otherwise `10`-`3600`. Escalation and auto-resolution still count analyzer cycles. Restart to change |

### Trust & Actions

| Parameter | Default | Description |
|---|---|---|
| `trust.level` | `observation` | Trust tier: `observation`, `advisory`, `autonomous` |
| `trust.maintenance_window` | (none) | When autonomous MODERATE actions may run; see [Maintenance windows](#maintenance-windows). Unset or `never` closes the window |
| `trust.ramp_start` | (auto) | Auto-persisted on first start; set to override |

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

| Parameter | Default | Description |
|---|---|---|
| `llm.enabled` | `false` | Enable LLM-powered features |
| `llm.endpoint` | (none) | OpenAI-compatible chat completions endpoint |
| `llm.model` | (none) | Model name |
| `llm.api_key` | (none) | API key (supports `${ENV_VAR}` expansion) |
| `llm.timeout_seconds` | `30` | Timeout for LLM API calls |
| `llm.token_budget_daily` | `500000` | Maximum tokens per day |
| `llm.optimizer.enabled` | `false` | Enable index optimizer |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new indexes per table per cycle |
| `rca.narration_enabled` | `false` | Let the LLM rewrite the summary on `incident_detected` and `incident_escalated` notifications. The model can only read the incident's own evidence: its causal chain (`E#`), the results of the fixed read-only catalog probes run for it (`P#`, also readable with `get_probe_result`) and the deterministic causal-graph hypotheses (`H#`). It writes no SQL and has no database access. It answers with claims; each must cite evidence ids, and every number in a claim must appear in the evidence that claim cites. Budget per narration: 2 model turns, 1,024 output tokens per turn, about 16k input tokens, 20 s per turn; at most 3 narrations per persistence cycle within 45 s. Any failure (LLM off, budget, rate limit, timeout, malformed or uncited output) uses the deterministic summary, which is always labeled. `llm.enabled=false` is the hot kill switch and cancels narrations in flight |

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

With `sre.automatic_start: true`, pg_sage investigates each open RCA incident of the lock,
connection or WAL family and each open `plan_regression` finding, once. An investigation runs
the fixed read-only catalog probes of its family (connection and WAL investigations sample
twice, `sre.sample_interval_seconds` apart), matches them against the deterministic causal
graph and stores the result: the likely explanation, contributing factors, alternatives and
ruled-out explanations, each citing its evidence, the evidence that could not be collected,
and an operator step. Nothing is executed. "Inconclusive" is a normal outcome.

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
configured for other features, set `sre.llm.enabled: false`. Models that reserve extra
reasoning tokens (Gemini 2.5+/3, OpenAI o-series, DeepSeek R1) need more output tokens than the
4k ceiling allows, so their turns are refused before any request is sent and recorded as
`budget_exhausted`. Use a non-reasoning model for investigations.

| Parameter | Default | Description |
|---|---|---|
| `sre.automatic_start` | `false` | Start investigations from incidents and plan regressions. Off: investigations already stored are still resumed and retained. Restart to change |
| `sre.trigger_interval_seconds` | `15` | Seconds between checks for new triggers and pending investigations, `5`-`600` |
| `sre.sample_interval_seconds` | `5` | Seconds between the two samples connection and WAL investigations compare, `1`-`30` |
| `sre.evidence_retention_days` | `30` | Days a finished, unpinned investigation keeps its probe evidence. The delete leaves a tombstone, and the investigation is shown as "evidence deleted by retention" |
| `sre.timeline_retention_days` | `90` | Days a finished, unpinned investigation is kept at all (hypotheses, steps, event chain), leaving a tombstone. At least `sre.evidence_retention_days`, at most `3650` |
| `sre.llm.enabled` | `true` | Model turn in investigations, used whenever an LLM is configured. `false` keeps investigations deterministic. Restart to change |

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

### Retention

| Parameter | Default | Description |
|---|---|---|
| `retention.snapshots_days` | `90` | Days to retain snapshot data |
| `retention.findings_days` | `180` | Days to retain resolved findings |
| `retention.actions_days` | `365` | Days to retain action log entries |
| `retention.explains_days` | `90` | Days to retain EXPLAIN plan captures |

> **Complete reference:** See `sidecar/config.example.yaml` for all
> configuration fields including `safety`, `alerting`, `auto_explain`,
> `forecaster`, `tuner`, `advisor`, and `api` sections.
