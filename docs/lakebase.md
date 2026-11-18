# Lakebase compatibility and operations

This guide covers operating pg_sage 2.5 against Lakebase: connections, Unity
Gateway, history telemetry, SRE and managed-service limits. Dated acceptance
results, with the build each one tested, are in the
[evidence appendix](lakebase-evidence.md#lakebase-guide). They are scoped
development results, **not full Lakebase compatibility certification** or a
production reliability guarantee. A passing local PostgreSQL suite is separate
from a passing native Lakebase test.

Start with the [native Databricks implementation guide](databricks-native.md)
for the architecture and linked setup, or the [Databricks Apps guide](databricks-apps.md)
for packaging, resources and platform sign-in. This page owns the database,
telemetry and SRE compatibility boundaries. Earlier acceptance reports are
dated evidence; their original failures and limitations are retained even when a
later revision closes a gap.

The Lakebase integration uses the standard PostgreSQL wire protocol and the
official Databricks Go SDK for unified authentication. It supports local CLI
profiles and Databricks Apps service principal credentials. Short-lived
database tokens are obtained at physical connection time and never written
into pg_sage configuration or its metadata database.

Each pool keeps one cached database token (in fleet mode, one per database
pool). Lakebase checks a token's expiry only at login, so open connections
are unaffected by expiry and only new physical connections need a valid token:

- A cached token with more than 15 minutes left is used without contacting
  Databricks.
- With 15 minutes or less left, the next new connection mints a fresh token.
  One mint runs at a time and is bounded to 20 seconds; other connections keep
  using the cached token meanwhile instead of queueing behind the mint.
- If the mint fails (for example while the workspace control plane throttles
  or is unavailable), connections keep using the cached token while it has
  more than 30 seconds left. pg_sage logs one warning per minute
  (`lakebase credential refresh failed; using the cached credential`) with the
  endpoint, the consecutive failure count and the time the cached token stays
  valid. With 30 seconds or less left, new connections fail with
  `refresh Lakebase database credential: ...` until a mint succeeds.

At startup the endpoint lookup is part of each connection attempt: the meta
database, the monitored database and every fleet database retry a throttled,
unavailable or unreachable endpoint lookup up to five times with backoff
(1, 2, 4 and 8 seconds), each lookup bounded to 30 seconds. A permission
denied (403) or not found (404) answer, an invalid endpoint mapping and an
unverified TLS mode stop startup at once with an error naming the endpoint.

`temp_spill` findings use new temporary-block counters from comparable query
snapshots and do not require pg_hint_plan or auto_explain. Their advice calls
for controlled `SET LOCAL work_mem` experiments and memory budgeting per
operation, worker and concurrent session. They do not choose or apply a setting.
A new observed non-spilling execution can clear the finding; missing samples
and known resets remain unknown. Aggregate counters cannot establish complete
per-entry lineage across an unseen selective reset/eviction and regrowth.

## Compatibility at a glance

"Verified" below identifies the tested identity and workflow, rather than every
role, endpoint, PostgreSQL version or deployment setting. The broad live action
and incident fixtures used an isolated PG18 branch. PG16/17 connection and
collector probes do not extend that entire PG18 result to those versions.

| Area | Native process using a selected Databricks identity | Databricks App using its service principal | Limit / next evidence needed |
| --- | --- | --- | --- |
| Direct TLS/OAuth connection, bootstrap and collection | Verified; PG16/17/18 connection/probe coverage and deeper PG18 tests | Verified on PG18, including ready database/schema and connected Fleet | Each identity needs its own role and object permissions; OAuth uses a direct endpoint |
| Credential renewal and idle/resume | Real database-credential expiry/reconnect and endpoint idle/resume passed | Earlier App artifact passed elapsed one-hour checks; dev.10 passed fresh M2M calls | Latest artifact was not subjected to a new hour-boundary or multi-day soak; HA/failover unproven |
| Unity Gateway model calls | Chat, structured responses, tools and five advisor workflows verified for the tested model | Uncached explanation and model-cited history verified with App M2M | No Gateway API key; model-specific structured capabilities require validation |
| Nine deterministic finding families | Actual collection → finding → controlled correction/resolution verified | All nine generated durable findings; scoped Cases/UI and invalid-index restart continuity verified | App findings are not App-executed remediation; some fixtures remain for review |
| `work_mem` and spill advice | Session-local spill/control and exact database-setting apply/restore verified | Hint-free spill finding and review guidance verified | No automatic memory value; role-wide promotion is not an enabled Lakebase execution contract |
| Eight enabled SQL action contracts | Native owned-object operator/policy paths verified | App stays in Observation mode; workload-table execution unproven | Trust, ownership, policy, admission and measured outcome checks remain independent |
| Ordinary session/log-file `auto_explain` | pg_sage ingestion gate unavailable; standard logging disabled and the tested role cannot change its threshold | Same integration gate and managed log-file restriction | Provider preloading does not establish usable session/file ingestion; Advanced Telemetry is the separate plan path below |
| Provider plan payload → cache → hashed history | Verified with scoped, authorized native payload reads | Actual scheduler/API plan metadata verified; raw plan payload/cache path unproven | Payload consent and unmasking required; no complete delivery or latency SLA |
| Plan/log/ASH/DDL/database historical SRE sources | Positive native delivery/normalization, including all ten underlying streams | All five history adapters returned positive, model-cited evidence in dev.5 | Asynchronous, partial history; not live admission or proof of current root cause |
| Live database SRE | Native read-only investigation and controlled lock/idle/sequence scenarios passed | Catalog/history and synthetic SLO investigation paths verified | Most incident/pressure scenarios were not induced; default policy is database-only |
| Application SLOs | Scoped push/burn/replay/restart/staleness lifecycle verified | Real-time synthetic sample → burn → cited case → stale-to-unknown passed | Requires application counters; provider logs cannot supply a complete customer-error denominator |
| Operator statistics-maintenance bridge | Native owned-table proposal → approval → execution → structural check → rollback verified | Not executed under the App principal | App workload-table ownership unproven; rollback cannot restore prior ANALYZE statistics |
| `pg_hint_plan` / HypoPG | Hook activation unavailable / extension unavailable on tested PG18 image and role | Not made available by Apps or telemetry | Plain EXPLAIN and a reviewed disposable real-index experiment are separate, limited alternatives |

See the [dev.5 native and App source matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-native-sre-source-matrix.md),
[native action tests](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-native-tests.md),
[provider follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md) and
[dev.10 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev10-acceptance.md) for the
specific successes, failed attempts and remaining boundaries behind this table.

## Local connection

Select the workspace profile explicitly and obtain the endpoint resource path
and hostname from `databricks postgres get-endpoint`. Configure a passwordless
PostgreSQL URL with TLS and the actual PostgreSQL role name:

```bash
export DATABRICKS_CONFIG_PROFILE='<workspace-profile>'
export SAGE_LAKEBASE_ENDPOINT=projects/<project>/branches/<branch>/endpoints/<endpoint>
export SAGE_DATABASE_URL='postgres://<url-encoded-role>@<endpoint-host>:5432/<database>?sslmode=verify-full'
pg_sage --config config.yaml
```

The singular endpoint variable applies to both monitored and metadata pools.
For a fleet with multiple endpoints, use `SAGE_LAKEBASE_ENDPOINTS` **instead**:

```bash
export SAGE_LAKEBASE_ENDPOINTS='{"<host-a>":"projects/<a>/branches/<branch>/endpoints/<endpoint>","<host-b>":"projects/<b>/branches/<branch>/endpoints/<endpoint>"}'
```

`sslmode=verify-full` is required, not just recommended, for every OAuth
connection: the main host and every fallback host. pg_sage refuses
`require` and `verify-ca` before it contacts Databricks, because neither binds
the server certificate to the endpoint hostname, and an interceptor could
capture the minted database credential. The refusal reads:
`lakebase OAuth requires sslmode=verify-full (got an unverified TLS mode); set
sslmode=verify-full in SAGE_DATABASE_URL, SAGE_PG_SSLMODE or the fleet's
databases[].sslmode`. In fleet mode each mapped database takes its TLS mode from
its own `sslmode` key, which defaults to `prefer`, so set `sslmode: verify-full`
on every database listed in `SAGE_LAKEBASE_ENDPOINTS`. Lakebase hostnames
verify against the system certificate roots, so no `sslrootcert` is needed.

Every mapped hostname is checked against endpoint metadata before its pool
receives a credential. Unmapped hosts retain ordinary PostgreSQL authentication
and their configured `sslmode`.
Plaintext fallback connections are refused for OAuth pools. Lakebase's built-in
PgBouncer pooler does not support OAuth; use the direct endpoint hostname. Native
OAuth configuration rejects the pooled hosts returned by endpoint metadata.
Environment changes,
including the profile or endpoint mapping, require a process restart.

pg_sage creates its `sage` schema and persistent state during normal startup,
including observation mode. Use a dedicated database for the first Apps
deployment. Run the full destructive self-managed integration fixtures on a
disposable PostgreSQL server: they change cluster roles, reset shared statistics
and attempt instance settings that managed Lakebase restricts. Run the dedicated
native Lakebase checks only on an isolated test branch/database; a separate
database on an existing application branch cannot isolate cluster-wide effects.
Schema ownership, monitoring privileges and access
to application objects must be granted to the actual connecting identity.

The dashboard's database connection-test and preview helpers use the same
configured native OAuth mapping as monitoring pools, including replacing a
bootstrap password when OAuth is selected. Preview keeps the endpoint hostname
for credential authorization and TLS SNI, and connects only to the IP approved
by its existing private-address/DNS check. Both native preview and saved-
connection tests passed against the isolated PG18 test branch. Private/internal
addresses remain blocked by the preview's existing safety policy.

Configured-replica administrative connections also use the native mapping;
their connection/bootstrap path passed against the PG18 direct endpoint.
Standby session termination and role-revocation topology remain separate
acceptance tests. Agent broker logins intentionally retain their own per-role
SCRAM credentials. New Lakebase projects disable native password connections
by default; workspace OAuth cannot substitute for a broker role's identity.
This integration does not enable project-wide password connections or claim
broker compatibility on an OAuth-only project. Action-provider gates remain
subject to the limits below.

## Unity Gateway

Use the unified gateway base URL and a fully qualified model service name:

```yaml
llm:
  enabled: true
  auth: databricks
  endpoint: https://<workspace-host>/ai-gateway/mlflow/v1
  model: system.ai.gpt-6-sol
  json_mode: true
  token_budget_daily: 50000
trust:
  level: observation
```

`SAGE_LLM_AUTH=databricks`, `SAGE_LLM_ENDPOINT` and `SAGE_LLM_MODEL` override the
corresponding fields. No `llm.api_key` is needed. The SDK refreshes workspace
authentication for chat, retries and tool calls; credentials are restricted to
the SDK's configured HTTPS workspace origin, and native requests reject HTTP
redirects. The optimizer inherits this auth
when it uses the same endpoint without an explicit API key. In Databricks Apps,
the SDK is pinned to oauth-m2m and every configured AI client must use
Databricks authentication on that same workspace origin. API-key overrides and
foreign origins are rejected; there is no key fallback.

The `/ai-gateway/openai/v1` provider-service API is a different interface and
requires the `Databricks-Model-Provider-Service` header. This integration's
native unified-auth configuration targets `/ai-gateway/mlflow/v1`; it does not
add provider-service selection. UI model discovery uses the SDK's Unity Catalog
model-service listing, because the unified gateway does not expose the generic
OpenAI `/models` route. It lists services in the selected model's catalog and
schema (default `system.ai`), follows pagination, and includes only services
advertising unified Chat Completions support. It returns fully qualified model
service names and uses the committed authentication settings without requiring
an API key. A discovery preview cannot redirect workspace credentials to another
origin. Live native discovery returned 53 accessible chat model services in the
selected workspace, including `system.ai.gpt-6-sol`.

Model capabilities vary. Live tests on 2026-10-29 verified chat, JSON-object
responses, function calls and tool-result follow-up with `system.ai.gpt-6-sol`.
The existing wire adaptation automatically adds `reasoning_effort: none` when
required. `system.ai.claude-sonnet-4-5` accepted chat/tool requests but rejected
`response_format: {type: json_object}`; use `json_mode: false` for that model and
validate each structured workflow separately.


## Advanced Telemetry, auto_explain and SRE history

### Platform capture and access

The public [telemetry reference](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
describes **Lakebase Observability in System Tables as Beta**. A workspace
administrator enables that preview; capture then starts automatically into
`system.lakebase` while a project's compute runs. This is a platform prerequisite,
separate from enabling the pg_sage reader below. The current public contract
does not require creating a legacy observability configuration or supplying a
customer catalog destination. Legacy configurations no longer receive new
telemetry and cannot be newly created. The
[observability overview](https://docs.databricks.com/aws/en/oltp/projects/observability-dashboards)
states seven-day system-table retention.

The SDK identity also needs access to the existing SQL warehouse and appropriate
Unity Catalog privileges. Public documentation describes schema-level telemetry
access as spanning projects in the account, so a provider grant is broader than
pg_sage's resource-filtered read. pg_sage performs no account-wide discovery and
does not grant permissions. Choose the reader identity and scope deliberately.
Statement-bearing columns are masked unless the identity has the platform's
separate unmasking authority. See the public reference for account-admin and
`databricks_pii_access` rules; this integration does not create that group or
request broad payload access.

### Ordinary auto_explain versus provider-captured plans

Enabling Advanced Telemetry supplies a useful alternative plan source; it does
not authorize session tuning, preload changes or server log-file access. The
tested endpoint already listed `auto_explain` in `shared_preload_libraries`,
but standard `auto_explain.log_min_duration` was `-1`, ANALYZE/buffer logging was
off and the selected native role could not SET the threshold. These are dated
endpoint observations, not fleet-wide defaults. The pg_sage ordinary
`auto_explain` ingestion gate therefore remains unavailable; this does not mean
that Lakebase cannot capture workload plans.

The tested integration uses `system.lakebase.plan_history` and `postgres_logs`
through the warehouse, rather than configuring the ordinary session path or
tailing files. A capability badge saying ordinary `auto_explain` is unavailable
must be read separately from provider-history availability. The
[original telemetry assessment](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-advanced-telemetry.md)
records those read-only settings checks and the initially empty delivery results;
the later positive provider tests are recorded separately below.

| Plan path | What was verified | What remains unknown or restricted |
| --- | --- | --- |
| Plain on-demand EXPLAIN | Native planner/cost reads and App plan explanation | Estimates do not establish actual runtime or hint enforcement |
| Native authorized provider payload | Actual captured plan JSON → scoped durable cache → normalized plan hash → historical SRE evidence; original capture/receipt times and capture flags retained | Partial asynchronous delivery, not a completeness or latency guarantee; unknown context equivalence blocks automatic regression comparison |
| App metadata-only provider path | Existing scheduler → durable inbox → authenticated history API, then positive model-cited structured history | App raw plan/log payload-cache ingestion has not been exercised; `allow_payload: false` intentionally excludes it |

An available plan does not imply execution statistics, buffers or timing were
captured: preserve each provider flag, including unknown values. Delayed plans
are not current query samples and cannot establish a live memory/CPU admission
decision. Payload-enabled native tests and metadata-only App tests are separate
proofs, even when both consume the same public system-table family.

Publicly documented capture gaps include COPY operations and statements that
run only inside PL/pgSQL rather than as top-level statements. DDL around
scale-to-zero/resume can be missed. Existing computes may not receive capture
GUCs immediately after preview enablement; follow the public troubleshooting
instructions rather than issuing unsupported ALTER SYSTEM/preload workarounds.
No restart or provider-setting change is performed by this integration.

### Configure the pg_sage reader

Use an existing SQL warehouse and explicitly bind each monitored database to its
verified direct endpoint. The reader uses the same Databricks SDK identity;
PostgreSQL/model permissions alone do not establish warehouse or UC permission.
The defaults read plans, logs, ASH, DDL and database counters/gauges:

```yaml
lakebase_observability:
  enabled: true
  warehouse_id: '<existing-warehouse-id>'
  allow_payload: false
  poll_interval_seconds: 60
  queries_per_poll: 16
  retention_hours: 168
  settle_lag_seconds: 120
  short_lookback_seconds: 7200
  bindings:
    - host: '<direct-lakebase-host>'
      database: '<monitored-database>'
      endpoint: projects/<project>/branches/<branch>/endpoints/<endpoint>
      allow_compute: false
```

`endpoint` is optional. When a binding omits it, pg_sage takes the endpoint that
`SAGE_LAKEBASE_ENDPOINT` or `SAGE_LAKEBASE_ENDPOINTS` maps to the binding's host,
the same mapping OAuth uses, so the host-to-endpoint mapping is stated once. If
neither variable maps the host, startup fails with `binding for <host> has no
endpoint and SAGE_LAKEBASE_ENDPOINT(S) maps none`. An `endpoint` you set is kept.
Every project, branch and endpoint ID is 1-63 lowercase letters, digits or
hyphens and must not start with a hyphen; OAuth, telemetry and the Apps
`configure.py` apply this same rule.

#### Read cadence and query budget

Each poll spends at most `queries_per_poll` SQL warehouse reads. When it is 0
or unset, the budget is two reads per stream plus four: 16 for the six default
streams. A poll spends the budget in this order:

1. **Forward.** Every stream reads new history first, ending
   `settle_lag_seconds` (default 120) before now, so rows the provider commits
   late are visible when their window is read. Each stream keeps one read
   reserved while earlier streams catch up, so no stream misses a poll.
2. **Short reconcile.** Each stream gets one read of a sweep over the
   `short_lookback_seconds` (default 7,200, two hours) behind its forward
   edge. This catches rows that arrive later than the settle lag.
3. **Deep reconcile.** Whatever budget is left goes to a sweep of the full
   seven-day source horizon. A stream starts a new deep sweep at most once
   every 24 hours.

The forward read width adapts to each stream. A read that exceeds
`max_rows` or `max_bytes` is split in half, and the narrower width is kept in
the cursor, so it survives a restart. After three reads projected under a
quarter of `max_rows`, the width doubles, up to `window_seconds`. A stream that
stays that sparse at the full window also reconciles four windows per read.
A reconcile read that is split keeps its narrower width for later polls, so a
dense stretch of old history is read in pieces rather than retried whole.
History that leaves the source horizon before it is read still counts as a gap.

`GET /api/v1/lakebase-observability/status` reports each stream's
`forward_lag_seconds` (how far the forward cursor trails the last poll) and
`reconcile_progress` (the share of the current deep sweep read, 0 to 1). A
forward lag that keeps growing means the stream needs more than its share of
the budget: raise `queries_per_poll` or `max_rows`.

Each stream also counts its SQL warehouse reads since `counters_since` (the
reader's start): `statements` (every statement submitted, including the
retries of a window halved on the row or byte budget, and failed reads),
`rows` and `bytes` (from the batches returned) and `read_millis` (wall time
spent in reads). Every statement spends the poll's query budget, so statements
per hour across all streams stay at or below
`queries_per_poll × 3600 / poll_interval_seconds`. The
administrator deployment status repeats this projection under
`databricks.telemetry`, next to credential expiry and Gateway outcomes (see
[Databricks Apps](databricks-apps.md)).

#### Cost and freshness

**Freshness.** A row the provider has committed to its system table is read by
the first forward read whose window covers it. Forward reads stop
`settle_lag_seconds` (default 120) short of now and run every
`poll_interval_seconds` (default 60), so a promptly committed row arrives about
two to three minutes after its event time. The provider's own delay before a
row appears in the system table comes on top of that; Databricks publishes no
delivery or latency guarantee for these tables.

**Settle behavior.** The settle lag trades latency for completeness: rows the
provider commits up to `settle_lag_seconds` late are read by the forward pass.
Rows later than that are picked up by the short reconcile sweep, which re-reads
the last `short_lookback_seconds` (default two hours) behind the forward edge,
or failing that by the deep sweep, which re-reads the seven-day source horizon
about once a day. Only a row committed so late that its event time has left
that horizon before a sweep reaches it is never read. Raising
`settle_lag_seconds` makes forward reads more complete but every finding later.

**Read width.** `window_seconds` (default 900) is the widest single forward
read and the base width of every reconcile read, not only the first one. Reads
narrow when a window hits `max_rows` or `max_bytes` and widen back up to this
cap; a stream that stays sparse at the full width reconciles up to four windows
per read. The first forward read of a new stream starts this far back.

#### Cost model

pg_sage keeps two Databricks resources busy, and each is billed while it runs:
the SQL warehouse the history reader queries, and the Lakebase compute pg_sage
monitors. Neither default changes on its own; this section says what the
defaults cost and which settings bring each resource back to zero.

**SQL warehouse.** Every read is a SQL statement on the bound warehouse. A
poll spends at most `queries_per_poll` statements (default 16 for six
streams), so the default schedule can issue up to 23,040 statements a day; most
polls spend fewer once reconcile sweeps finish. The warehouse stops only when
`poll_interval_seconds` exceeds its auto-stop idle time: with the default
60-second poll and any auto-stop setting of a minute or more, it never stops.

Estimate the warehouse's monthly use as:

```text
DBU per month = hours the warehouse runs per month × the size's DBU per hour
```

Worked example: a warehouse that never stops runs about 720 hours a month, so a
size rated 4 DBU per hour uses 2,880 DBU a month. The 4 DBU/hour rating is an
example only; check the size's rating and the DBU price against the current
Databricks rate card for your cloud, region and warehouse type before
budgeting. A poll interval longer than the auto-stop time lets the warehouse
stop between polls, but it then cold-starts for each poll.

To cut warehouse cost, raise `poll_interval_seconds` above the auto-stop time,
lower `queries_per_poll`, select fewer `streams`, or share a warehouse that
already runs for other work. Each of these delays findings: see
[Read cadence and query budget](#read-cadence-and-query-budget) and the
freshness notes above.

**The monitored Lakebase compute.** The history reader opens no connection to
the Lakebase endpoint, but the sidecar's own pools do:

| pg_sage cadence | Default | Pools |
| --- | --- | --- |
| Collector query | every `collector.interval_seconds` (60) | monitored and fleet databases |
| Pool health check | every 30 seconds (monitored), 1 minute (fleet, meta-db) | every pool that keeps a connection |
| `MinConns = 1` | one connection always open | monitored, fleet and meta-db pools |

An endpoint with scale-to-zero suspends its compute after its suspend timeout
of inactivity. Each cadence above that is shorter than the suspend timeout can
keep the compute active, so with the defaults an endpoint pg_sage monitors
stays up, and is billed, around the clock. pg_sage does not claim which of
these Lakebase counts as activity; it names every one that could.

At startup pg_sage reads the endpoint's suspend timeout and state from the
same endpoint lookup it already makes for OAuth (no extra API call). When
scale-to-zero is on and any cadence is shorter, it logs one warning per
endpoint naming them, for example:

```text
[WARN] [connect] Lakebase endpoint projects/p/branches/b/endpoints/primary suspends after
5m0s of inactivity, but pg_sage runs collector every 1m0s, health check every 30s,
min_conns=1 keeps a connection open; the compute may never scale to zero ...
```

The administrator deployment status reports the same per endpoint under
`databricks.endpoints[]`: `suspend_timeout_seconds`, `compute_state`,
`compute_observed_at`, `keeps_compute_active` and `scale_to_zero_blockers`
(see [Databricks Apps](databricks-apps.md)). A suspend timeout that is missing,
0 (scale-to-zero off) or negative gives no warning. pg_sage keeps collecting
while the endpoint is IDLE: monitoring is the point of running it, so it never
skips collection to let the compute sleep. Budget the compute as always on, or
monitor a branch or endpoint whose cost you accept running continuously.
Activity around a scale-to-zero or resume, such as DDL, can be missing from
provider capture (see [Platform capture and access](#platform-capture-and-access)).

#### Delivery, retries and backlog

Only `plan_history` events have a sink: they are copied into the monitored
database's plan cache. Events of the other streams stay in the durable inbox
as history and are acknowledged in the same transaction that stores them,
under the same policy fence, so they never queue for delivery. Each poll
delivers at most `max_rows` plan events, oldest first.

One event that cannot be delivered no longer blocks the events behind it:

- A plan that can never be stored (for example, one whose plan tree has no
  valid EXPLAIN shape) is acknowledged and counted as `rejected_row`.
- Any other sink failure is retried. The next attempt waits 30 seconds,
  doubling to at most 15 minutes, while later events keep being delivered.
  After 5 failed attempts the event is acknowledged and counted as
  `delivery_failed`; its inbox history remains until it expires.
- A timeout, cancellation, failed connection, or SQLSTATE class 08 or 57
  means the sink or control database is unavailable, not that the event is
  bad. Delivery stops for that poll without spending an attempt, and resumes
  on the next poll.
- Three consecutive failed events also stop the poll, after each spends one
  attempt. A sink-wide fault that is not one of the outage classes above (a
  full disk, a read-only sink, a revoked grant) then costs a few events per
  poll instead of exhausting the whole backlog as `delivery_failed`.

Attempts are kept in `sage.lakebaseobs_attempts` (schema version 6, created
by the telemetry schema setup). The table stores the attempt count, the next
attempt time and a fixed failure class such as `sqlstate_40P01` or `timeout`,
never error text.

`GET /api/v1/lakebase-observability/status` reports, per database, a
`delivery` list with one entry per consumer as of the last poll:

| Field | Meaning |
|---|---|
| `consumer` | Consumer name, for example `normalized_history_v1` |
| `pending_rows` | Deliverable, unacknowledged events, including those waiting for a retry |
| `pending_rows_capped` | `true` when `pending_rows` reached 10,001; the backlog is at least that large |
| `oldest_pending_at` | Occurrence time of the oldest pending event; absent when nothing is pending |
| `oldest_pending_age_seconds` | Age of that event at the last poll |

`delivery_outcomes_since_start` counts `normalized`, `payload_unavailable`,
`rejected_row`, `self_excluded` and `delivery_failed` per stream. A backlog
that keeps growing while plans are delivered means plan events arrive faster
than `max_rows` per poll: raise `max_rows`.

This configuration is restart-bound and never grants permissions or enables a
provider setting. Compute-wide streams require explicit `allow_compute: true`.
Payload mode requires separately approved access; metadata mode does not select
query text, parameters, plan JSON or log message/detail/hint/context/statement.
No broad PII grant is part of this integration.

`GET /api/v1/lakebase-observability/status` reports read cursors, fixed failure
states and unknown completeness. Administrators can request normalized history
at `/api/v1/lakebase-observability/history?database=<runtime-name>&stream=<stream>&from=<RFC3339>&to=<RFC3339>&limit=100`.
The bounded window is at most seven days and the row limit at most 1,000.

Captured provider plans carry their original capture time and flags into the
cache. The optimizer can use plans only after verifying the resource binding;
a provider source does not imply `ANALYZE`, buffers or timing were enabled.
Automatic regression comparison is withheld when execution-context equivalence
is unknown. Delayed plans are not stamped onto current cumulative query samples.

The SRE investigator's `read_history` tool can read five fixed sources from the
retained, verified database binding:

| Source | Historical observations |
| --- | --- |
| `lakebase_plan_history` | Query ID, cost/duration metadata and, when authorized and available, structured plan hash and capture flags |
| `lakebase_log_history` | Typed SQLSTATE/severity and classified RCA signal |
| `lakebase_session_history` | Sampled activity, waits and blocking metadata |
| `lakebase_ddl_history` | Validated DDL command and object type |
| `lakebase_database_history` | Database interval counters and gauges |

The tool accepts `source`, optional `window_seconds` (60–604800; default 24
hours), and optional `queryid` for plans or session history. Each read returns
at most 100 structured observations, retains original event/receipt times and
source identities, and enforces the current owner policy, pinned investigation
policy, physical database identity, retention and read budgets. Raw SQL, plan
JSON, log messages, actor identities and object names are excluded from durable
SRE history evidence. These sources require the telemetry reader and requested
streams to be configured; the tool grants no warehouse or UC access.

Historical log classifications and DDL observations become citable context in
the diagnosis. They do not change live root causes, confidence, incident state
or customer-impact claims. Empty or unavailable history remains unknown;
completeness and exact event multiplicity are never asserted. Sampled logs,
ASH and interval metrics cannot establish an application's error denominator
or populate live SLO counters. Automatic migration fan-out and provider-plan
regression comparison remain separate gaps. Historical CPU/memory/storage
observations never authorize live maintenance, and total memory does not mean
available memory.

The October 30 controlled native follow-up delivered exact-fixture plans,
logs and DDL, with nonempty metadata in all ten requested streams. The earlier
zero-row results remain valid for their original selectors and observation
times. See the [provider follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md)
for attribution, normalization and timing limits. This proves controlled native
delivery, not a completeness/latency SLA or App-principal payload access.
The final dev.5 App-principal investigator executed and cited all five history
sources through real Unity Gateway calls. Case
`e0f50f43-5e45-46d0-9701-ae1f8187a53d` cited retained plan/log evidence including
the exact controlled provider plan event. Case
`ea555ad2-3ca6-48a3-bbcc-69d48f18ed9c` cited ASH (100 observations), DDL (one)
and database metrics (100). The DDL event matched a fresh owned table's exact
resource/database/OID, short application tag, backend PID, command, object and
original time window; the actual scheduler/API and durable evidence shared its
event ID. Exact fixture cleanup, evidence hashes, lifecycle chains, idempotent
readback and eight unchanged action/approval ledgers passed.

Payload consent remained `allow_payload: false`. These are positive structured
history paths; raw App plan/log cache access, complete delivery and live root
cause remain separate claims. Both healthy cases concluded advisory inconclusive.
The [dev.5 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev5-acceptance.md#final-app-history-and-slo-evidence)
records exact events, hashes, delays and the preserved failed transport attempt.
Earlier [telemetry](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-advanced-telemetry.md) and
[SRE acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-sre-acceptance.md) reports retain
their original zero-row and version-specific evidence.

### Workload explorer wait load

The Queries page's wait-load chart (`GET /api/v1/wait-load`) names one
preferred source, in this order: Performance Insights (Amazon RDS and Aurora
only), then Lakebase wait counters, then pg_sage's own `pg_stat_activity`
samples. The activity samples are always returned as well. A Lakebase database
prefers its counters once the retained `wait_event_counters` stream has at
least one interval in the requested window; until then the answer is exactly
the activity-sample answer. The stream is endpoint-wide, so it needs
`allow_compute: true` on the binding and the stream in the reader's streams.

Each counter interval becomes average active sessions by wait class: the
interval's `wait_event_time` divided by its wall-clock length (`previous_ts` to
the row time); computes whose intervals share their edges are summed into one
point. The `provider` object carries the per-interval `series`, the heaviest
`events`, the current `load_by_wait` (each compute's newest interval that
overlaps the newest interval of all, summed, so a replica reporting a few
seconds later does not hide the primary), `skipped` (intervals with a bad time range, a
negative or non-numeric value, or no wait class) and `truncated` (the read is
capped at the newest 1,000 events). The unit of `wait_event_time` is not yet
confirmed against a live workspace: pg_sage assumes milliseconds and reports
`wait_event_time_unit: "ms"` with `unit_confirmed: false`, and the page says
so. A failed read is logged with the database and stream and the route still
answers from the activity samples.

### SRE with and without Advanced Telemetry

Advanced Telemetry is not required for catalog-based monitoring, deterministic
findings or the live database-scoped SRE probes. Without it, pg_sage can read
the permitted `pg_stat_*`/catalog sources and investigate observed database
conditions, but it lacks the provider's retained plan/log/session/DDL history.
With it, historical evidence augments investigation; it does not prove a current
root cause, change live incident state, or authorize maintenance by itself.

The [per-source native matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-native-sre-source-matrix.md)
inventories 32 catalog probes and the original auxiliary sources. A separate
explicitly authorized gate completed 16 selected cluster/filesystem queries
without new grants: eleven returned usable rows and five valid empty results.
That test policy was not deployed. Production's database-only policy continues
to withhold those broader sources. A policy refusal is not a Lakebase SQL
incompatibility; a valid empty result is not positive incident detection.

Controlled native lock/idle-blocker and sequence-pressure fixtures verified
real signals and a read-only investigation. Native Gateway investigation and
App history citations also passed, with healthy historical cases concluding
advisory/inconclusive. Exhaustion, failover, sustained resource pressure,
wraparound and many other incident variants were deliberately not induced.
The [scenario inventory](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-nerdlab-findings.md#complete-analyzer-inventory-what-the-nine-fixtures-do-not-prove)
distinguishes readable inputs, tested detectors, uninduced incidents and actual
provider restrictions. Automatic migration fan-out and context-equivalent
provider-plan regression comparison remain incomplete.

## Application SLOs

An application SLO requires cumulative bad-event and eligible-event counters
for the same real population and observation time. Series must partition that
population without double counting; counter decreases are treated as resets.
Configure an explicit database-bound application objective, target, traffic
floor, stale limit and burn windows. Stored samples must cover at least 90% of
each burn window, and both windows must reach the burn threshold. Missing,
low-traffic, partial-window, stale or unavailable input is unknown. A database
proxy SLO describes database symptoms and does not establish customer impact.

Databricks Apps adds a platform-authenticated administrator sample route:
`POST /api/v1/sre/slos/{name}/samples?database=<runtime-name>`. It accepts only
an explicitly configured database-bound `source: push` objective, binds series
to the persisted administrator, and namespaces storage by registered database
identity. See the [Apps SLO contract](databricks-apps.md#application-sli-ingestion)
for body fields, replay semantics and source limitations. Standalone signed
ingestion retains its existing source-credential contract.

With SLO evaluation and `sre.slo.open_investigations` enabled, a page-level
application burn can open a read-only `slo_burn` case even when
`sre.automatic_start` is false. The database-scoped `application_slo_status`
source provides bound push-SLO evidence; an explicit read allowlist must permit
it. This enables an investigation, not action execution or notification.
Synthetic acceptance counters establish synthetic behavior only. The
[October 30 dev.5 App SLO acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-app-slo-acceptance.md#dev5-post-upgrade-pass)
passed fifteen current-time samples over seven minutes, both configured burn
windows, a cited real-Gateway investigation and actual stale-to-unknown behavior
123.06 seconds after pushes stopped. Exact cleanup preserved the case and left
all eight action/approval ledgers unchanged. The final deployment has no synthetic
objective or samples. Historical investigator acceptance is separate from that
synthetic SLO scenario; neither establishes production customer impact.

## Nine controlled finding families

These scenarios used the actual collector, analyzer and durable findings path,
with production rule thresholds. No findings were inserted and no generated
recommendations were executed. Native fixtures were corrected or removed and
normal resolution was observed. The App separately generated all nine families;
its retained fixtures support UI inspection, not a remediation-execution claim.

| Family | Condition actually induced | Native correction / resolution evidence |
| --- | --- | --- |
| Duplicate index | Two ordinary btree indexes over the same column | One fixture index dropped; finding resolved |
| Missing foreign-key index | 12,000 child rows without a leading-column supporting FK index | Supporting index created; finding resolved |
| Stale statistics | Never-analyzed 12,000-row owned table | ANALYZE; finding resolved |
| Table bloat | 12,000 rows followed by 9,000 deletes in an owned fixture | Non-FULL VACUUM/ANALYZE; finding resolved; dead-tuple heuristic is not a physical-bloat measurement |
| Heavy sequential scans | 110,000 rows and 105 selective sequential scans without an index | Fixture removal resolved the finding; no index latency gain was claimed |
| Sequence exhaustion | Owned integer sequence at 950 of a maximum 1,000 | Only that sequence's maximum expanded; finding resolved |
| Invalid index | Concurrent unique-index build failed on real duplicate values | Invalid state persisted across cycles; fixture index removal resolved it; restart warmup continuity separately fixed and verified |
| Slow query | One constant-result sleep exceeded the unchanged 1,000 ms mean-latency threshold | A new zero-sleep call of the same normalized statement lowered its cumulative mean; finding resolved |
| Temporary spill | Real sort spilled under session-local 64 kB `work_mem`, with actual plan and temp counters | Same query under a 64 MB control did not spill; a newer observed non-spilling execution resolved the finding |

The eight-condition run passed in 99.92 seconds; the separate default slow-query
run passed in 65.86 seconds. The [scenario report](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-nerdlab-findings.md)
records exact evidence, bounded workload/cleanup rules and untested analyzer
branches. In particular, the ordinary unused-index rule's seven-day clean
observation window was not accelerated, million-row autovacuum tuning and
wraparound thresholds were not induced, and all duplicate/index variants were
not exhaustively reproduced. The last dev.10 App walkthrough saw 13 retained
Cases; that is a dated readback, not a permanent count or health assertion.

## work_mem: advice, experiments and setting scope

The new deterministic `temp_spill` rule closes the hint dependency: it requires
comparable query snapshots, fresh calls, matching known global statistics epochs
and at least 128 new temporary blocks. It needs neither `pg_hint_plan`, ordinary
`auto_explain`, Advanced Telemetry nor an LLM. Advice asks for a reviewed plan,
query/index alternatives and bounded `SET LOCAL work_mem` experiments. It does
not propose an executable value or mark an untested setting safe.

The 64 MB non-spilling control above is specific to a small synthetic fixture.
PostgreSQL allocates work memory per operation and can multiply that across
parallel workers and concurrent sessions. Fewer temp writes alone do not prove
that a broad memory increase is safe or improves production latency.

The [public compatibility contract](https://docs.databricks.com/aws/en/oltp/projects/compatibility)
permits user-context settings at session, database and role scope, subject to the
connecting identity's authority. Session-local experiments and connected-database
`work_mem` apply/fresh-session-readback/exact restore passed natively. The
`alter_database_guc` executor captures the actual prior database override and
refuses masked role/session overrides. Generic role-work_mem promotion remains
outside the enabled Lakebase action list. Its review artifact does not fabricate
`ALTER ROLE ... RESET work_mem` as restoration of an uncaptured prior setting.

Recovery requires a still-observable query, newer calls and unchanged temp
read/write counters. Inactivity, missing/top-N/evicted samples, unknown or changed
epochs, visible counter decreases and small continuing spills stay unknown/open.
Unseen selective reset/eviction followed by regrowth between samples can escape
those guards; complete per-entry lineage is not established.

## Branch rehearsal

pg_sage can rehearse a migration on an instant Lakebase branch of production
instead of a Database Lab clone. Set:

```yaml
clone:
  provider: lakebase_branch
  lakebase:
    project: <project>          # required
    # source_branch: <branch>   # default: the monitored endpoint's branch
    # ttl_minutes: 60           # 1-1440, server-side expiry of each branch
    # endpoint_max_cu: 1        # autoscaling maximum of the branch endpoint
```

Each rehearsal creates branch `sage-rh-<8 hex>` of the source branch, a
read-write endpoint on it and a database credential for the monitored role, and
connects with `sslmode=verify-full`. The rehearsal runs the expand steps only, as
with any clone, then deletes the endpoint and the branch. A branch is a copy of
production data, so:

- Every branch is created with an expiry `ttl_minutes` ahead. If pg_sage stops,
  or cannot delete the branch, Lakebase deletes it then. The default, 60 minutes,
  is the rehearsal bound. A shorter `ttl_minutes` shortens the rehearsal bound to
  match, so a rehearsal never outlives its branch.
- If a later step fails (endpoint, credential), pg_sage deletes what it made
  before it reports the failure. A create that timed out, or whose cleanup
  failed, leaves its receipt `create_uncertain` and is logged for an operator.
- Every branch is receipted in `sage.clone_instances` (adapter
  `lakebase_branch`, scope `projects/<project>`), and the fleet leader's reaper
  deletes the branch of an expired receipt. pg_sage deletes only
  `sage-rh-<8 hex>` branches of the configured project.
- A branch with production's branch uid, or an endpoint on production's host, is
  refused (`target_is_production`) before any credential is minted. Production's
  identity is read from live metadata, as for telemetry scope, using
  `SAGE_LAKEBASE_ENDPOINT` (or `SAGE_LAKEBASE_ENDPOINTS`).

Without Databricks unified auth, or when the monitored database is not a
Lakebase endpoint, the rehearsal is recommendation-only (`clone_unavailable`).
The principal needs **CAN_MANAGE** on the project to create and delete branches
and endpoints, and the monitored role must exist on the source branch (a branch
copies its roles). The minted credential expires after about an hour, which
covers the one-hour rehearsal bound. Game days and local bench runs use the same
adapter when `clone.provider` is `lakebase_branch`; their clones are tracked by
the game-day store and the branch expiry, not by rehearsal receipts. A game
day can run for hours, and Lakebase deletes its branch at the expiry, so set
`ttl_minutes` above the longest game day you run on this adapter.

This adapter is for Lakebase only. Other providers and self-managed PostgreSQL
use `dle`. In fleet mode one clone provider serves the deployment: each
database's rehearsal branches from its own endpoint's branch, and a database
whose endpoint is in another project is recommendation-only.

Index rehearsals use the same branches (lab-04a): with `lakebase_branch` set,
the tuning agent and the index floor build a proposed index on a branch and
compare the target statements' shared blocks per call before and after, at most
`clone.rehearsals_per_hour` (default 2) per database, each executed statement
bounded by `clone.rehearsal_statement_timeout_seconds` (default 5). The branch
gets the same `target_is_production` check, receipt and expiry as a migration
rehearsal. See [index rehearsal](configuration.md) under the `clone` keys.

To check that a live run left nothing behind, list the project's branches
(`databricks postgres list-branches projects/<project>`); no `sage-rh-*` branch
should remain after a rehearsal ends.

## Branch discovery

A meta-db fleet can list the endpoints of every branch of a Lakebase
project with a `lakebase` source under `fleet.discovery.sources` (see
[the fleet walkthrough](walkthrough-fleet.md#discover-databases-automatically)).
The listing uses the workspace's Postgres API (`ListBranches`, then
`ListEndpoints` per branch), stops at 200 endpoints a cycle, and records
each endpoint with a host as a candidate keyed by its uid, database
`databricks_postgres`. pg_sage's own rehearsal branches (`sage-rh-*`, see
[Branch rehearsal](#branch-rehearsal)) are skipped and never listed as
candidates; a skipped branch still counts toward the per-cycle walk limit.
Lakebase candidates are not enrolled automatically:
a store-managed member has no OAuth credential path yet, so add the ones
you want by hand. An endpoint the project no longer reports is marked
`gone`; a failed listing keeps the previous candidates.

## Managed-service limits

| Feature | Verified behavior / limitation |
| --- | --- |
| PostgreSQL 16, 17, 18 | Native TLS/OAuth reconnect tests and collector SQL probes passed against existing Lakebase endpoints |
| `pg_stat_statements` | Supported but must be enabled per database; only 1 of 18 accessible databases had it installed in the October 29 survey. That inventory is not a current platform default or account-wide claim |
| HypoPG | Absent from the surveyed endpoints' available extensions; the PG18 native install attempt returned `42501`. Hypothetical-index validation is unavailable; a real index experiment on a disposable fixture is a separate workflow |
| `pg_hint_plan` | Publicly listed for PG18 and installed, but the planner hook is inactive; `LOAD` returned `0A000` and session/database preload settings returned `42501`. Hint enforcement remains unsupported in this configuration |
| `pgstattuple` | Owned-table inspection passed on the isolated PG18 branch |
| `pgaudit` | October 29 endpoint catalog advertised it, but installation returned `42501`; audit-log integration is unverified. The public extension table rechecked November 1 does not list it; catalog advertisement is not install/activation proof |
| `vector` | PG18 vector lab passed HNSW and exact-baseline comparison for three synthetic queries (recall 1.0); broader workload performance is unverified. The lab needs pgvector 0.8.0 or later and reports the version it found otherwise; halfvec, `relaxed_order` and `max_scan_tuples` were verified on self-hosted pgvector 0.8.2, not yet on Lakebase |
| Server configuration | Instance settings and `ALTER SYSTEM` are unavailable; permitted user-context session/database/role settings remain separate |
| Server logs / OS metrics | No direct log-file or host access. Lakebase exposes Beta plans, Postgres logs and compute metrics through `system.lakebase`; scoped history and controlled native delivery are verified within the follow-up report's limits. Asynchronous history cannot supply live admission |
| Statistics continuity | Suspend/restart can reset `pg_stat_statements`; never interpret a reset as workload improvement |
| Roles | Tested identity had `databricks_superuser` and `pg_monitor`, but was not a PostgreSQL superuser; a new app principal needs its own grants |
| Existing project access | October 29 survey: 18 of 21 databases authenticated as the selected native test identity; three service-principal-owned databases denied access. Access is identity-specific |

Lakebase's [observability system tables](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
include real execution plans in `plan_history` and structured `postgres_logs`.
Workspace preview enablement, Unity Catalog access and statement-text unmasking
are separate prerequisites. The explicit telemetry configuration above enables
scoped reads. A successful empty query does not prove capture, completeness,
unmasking or zero errors. See the [Advanced Telemetry and SRE gap assessment](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-advanced-telemetry.md).

Lakebase's default SRE policy is database-only. A `policy_scope_denied` result
for a cluster/filesystem source describes that trusted policy boundary; it does
not prove the platform lacks the source. A separate, explicitly authorized
read-only native matrix completed all 16 selected cluster/filesystem probes
using the existing role and no added grants. Eleven returned usable rows and
five returned valid empty results. Production policy remains database-only.
Query support, configured authorization and positive incident detection are
three separate claims: empty replication/lock/WAL reads do not establish those
incident or platform semantics. The [provider matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md#explicit-provider-probe-compatibility-matrix)
records the exact sources and execution evidence.

The same [provider follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md#planner-extension-incompatibilities-observed)
cross-checks internal extension issues with the public extension list and live
activation/install results. Installed `pg_hint_plan` is reported as
`installed_not_loaded`; plain EXPLAIN remains usable but does not enforce hints.
No unsupported preload workaround or hypothetical benefit is inferred.

A separate disposable-fixture experiment created a real candidate index,
reduced plan-only estimated cost from 339 to 97.34, and rolled back the index
savepoint to restore the exact baseline estimate. This demonstrates a possible
reviewed real-index experiment on an isolated fixture/branch. It is not a
transparent HypoPG replacement, proof of latency improvement or permission to
create candidate indexes on production workload tables. Advanced Telemetry
does not activate either planner extension.

pg_sage 2.5 explicitly enables eight Lakebase contracts after testing
their production operator paths on an isolated PG18 test branch:
`analyze_table`, `vacuum_table`, `create_index_concurrently`,
`revert_created_index`, `reindex_concurrently`, `create_statistics`,
`revert_created_statistics`, and `alter_database_guc`. Standing policy, trust,
maintenance windows, object permissions, and replica checks still apply. The
deployed development App starts in observation mode.

Database-setting actions target only the connected database and user-context
settings. They capture the exact database-wide prior override, derive their
own inverse, refuse masked role/session overrides, and read back through a
fresh physical OAuth connection after both apply and rollback. Statistics
creation uses plain `CREATE STATISTICS` plus `ANALYZE`; `IF NOT EXISTS` is refused
because its inverse could otherwise drop a pre-existing object. Catalog success
does not establish a performance benefit. Unmeasured changes remain unverifiable.

| SQL family | Native Lakebase result | pg_sage execution boundary |
| --- | --- | --- |
| ANALYZE / ordinary VACUUM | Owned-fixture SQL and operator/policy path passed | `analyze_table`, `vacuum_table` enabled; VACUUM FULL refused |
| Concurrent index creation / its inverse | Owned-index creation, catalog validity and rollback passed | `create_index_concurrently`, `revert_created_index` enabled; inverse targets the exact created object |
| Concurrent REINDEX | Owned single-index path passed | `reindex_concurrently` enabled; database/schema/system-wide rebuilds refused; no restoration of prior physical layout is promised |
| Extended statistics / its inverse | CREATE STATISTICS plus ANALYZE and exact-object removal passed | `create_statistics`, `revert_created_statistics` enabled for the guarded supported shape; no IF NOT EXISTS, and prior ANALYZE statistics are not restored |
| Connected-database user-context GUC | `work_mem` forward/rollback on fresh OAuth sessions passed | `alter_database_guc` enabled with captured prior state, target/context checks and override refusal |
| Ordinary user-index DROP | Direct owned-fixture SQL passed | Generic `drop_unused_index` is not enabled for Lakebase by these results; this is a pg_sage action restriction, not a claim that Lakebase cannot DROP an owned index |
| Per-table autovacuum reloptions | Owned-fixture SET/restoration SQL passed | General `set_table_autovacuum` execution remains outside the Lakebase contract list; previous explicit options must be captured for recovery |
| Role work_mem / arbitrary table DDL | User-context scope may be permitted by platform/object authority | Not enabled as general Lakebase execution families here; manual review is separate from automatic execution |
| Instance settings, planner hints, broker roles, backend signaling and failover | Restricted or separately unproven as detailed above | Not enabled by these eight contracts, a successful model call or an attached App database resource |

The deployed App's Observation mode prevents database actions even where a
contract is enabled. Completing a baseline, accepting a proposal or obtaining
an approval does not prove SQL ran, recovery is complete or performance improved.
The [dev.10 workflow acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev10-acceptance.md)
covers that distinction, recovery guards and source-preserving proposal
deduplication.

## Operator-requested statistics maintenance

The typed SRE maintenance bridge adds one explicit `create_statistics` request
to a concluded or inconclusive investigation with retained, hash-verified
`stat_tables` v3 evidence (an operator can refresh it with
`sre_refresh_table_evidence`; see the Sage SRE section of the configuration
reference). An operator/admin supplies the schema, table, columns, supported
statistics kinds, evidence ID/SHA-256 and rationale. The evidence must identify
the same physical database/table OIDs; table counters establish observation
scope, not column correlation or a predicted benefit. The bridge excludes the
`sage` metadata schema and allows one maintenance request per investigation.

Under `/api/v1/databases/{db}/investigations/{id}/maintenance`, signed-in
viewers/operators/admins can list proposals; operators/admins can propose and
call `/{proposal}/request` to create a pending human approval. Execution uses
the existing durable approval queue and standing policy. Only an admin can
explicitly request `/{proposal}/rollback`, with a reason. There is no automatic
proposal, approval, maintenance execution or rollback path. Observation trust
continues to prevent mutation.

Evidence freshness and approval TTL are bounded to 15 minutes; approval also
expires when the cited evidence does. The current and pinned read policies,
target pool, approval identity, emergency stop, trust and action policy are
rechecked. The connecting database role needs CREATE on the statistics schema
and ownership of the target table; `MAINTAIN` alone is insufficient for this
bridge. Under a bounded `SHARE UPDATE EXCLUSIVE` table lock, one transaction
rechecks table/owner/column identities, runs CREATE STATISTICS plus ANALYZE, and
captures the newly built object's OID.

Success is structural only. Performance verification remains `unverifiable`,
without value or autonomy credit. Explicit rollback removes the exact created
statistics object only after fresh policy and OID checks; a same-name
replacement is preserved. **Rollback cannot restore the table's prior ANALYZE
statistics.** This native lifecycle passed on an owned PG18 fixture using the
native user role; App-principal maintenance execution remains unproven.
Database/schema ownership or an attached Apps resource does not establish
ownership of workload tables. See the [maintenance evidence and refusal coverage](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-sre-maintenance.md).

Other mutation families retain their prior provider gates. User-index DROP,
instance settings, OS/log access, broker role authentication, and failover are
not established by these results. See the [native test report](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-native-tests.md)
for actual grants, extension versions, action evidence, and unsupported fixtures.
This remains a development release with explicit managed-platform boundaries.

## Implementation and evidence map

| Implementation | Source / recorded validation |
| --- | --- |
| Verified endpoint mapping and physical-connection credential refresh | [Native OAuth pool hook](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/databricksauth/lakebase.go); [native compatibility findings](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-compatibility.md) |
| Resource-bound warehouse reader, normalization, durable delivery and retention | [Configuration](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/config/lakebase_observability.go), [reader and store](https://github.com/jasonmassie01/pg_sage/tree/master/sidecar/internal/lakebaseobs); [provider follow-up](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-provider-compatibility-gaps.md) |
| Provider plan provenance, cache and historical evidence | [Provider-plan consumer](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/autoexplain/provider_plan.go), [historical evidence adapter](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/lakebaseobs/evidence.go); [source matrix](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-native-sre-source-matrix.md) |
| Hint-free interval spill finding and recovery guards | [Spill rule](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/analyzer/rules_temp_spill.go); [nine-scenario inventory](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-nerdlab-findings.md) |
| Enabled Lakebase action families and connected-database GUC execution | [Provider adapter](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/fleet/provider_adapter.go), [database setting executor](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/executor/database_setting.go); [native action tests](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-native-tests.md) |
| SRE statistics proposal, approval and exact-object recovery | [Maintenance bridge](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/sre/action/maintenance.go); [maintenance acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-sre-maintenance.md) |
| Current applicability and recovery presentation | [Case applicability](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/cases/provider_applicability.go), [recovery normalization](https://github.com/jasonmassie01/pg_sage/blob/master/sidecar/internal/cases/recovery.go); [dev.10 acceptance](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-30-v2.5-dev10-acceptance.md) |

Public references for platform behavior: [PostgreSQL compatibility](https://docs.databricks.com/aws/en/oltp/projects/compatibility),
[extensions](https://docs.databricks.com/aws/en/oltp/projects/extensions),
[authentication](https://docs.databricks.com/aws/en/oltp/projects/authentication),
[external application connections](https://docs.databricks.com/aws/en/oltp/projects/external-apps-connect),
[connection pooling](https://docs.databricks.com/aws/en/oltp/projects/connection-pooling),
[scale-to-zero](https://docs.databricks.com/aws/en/oltp/projects/scale-to-zero),
and [telemetry tables and limitations](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference).
The public extension list and actual activation results must be read together:
listing `pg_hint_plan` does not resolve the measured install-versus-hook gap.

## Reproduce live read-only checks

The native reconnect test creates no database objects and performs its SQL in
read-only transactions:

```bash
cd sidecar
PG_SAGE_LIVE_LAKEBASE=1 \
  SAGE_LAKEBASE_LIVE_DATABASE_URL="$SAGE_DATABASE_URL" \
  go test ./internal/databricksauth -run '^TestLakebaseLiveOAuthReconnect$' -count=1 -v

PG_SAGE_LIVE_LLM=1 SAGE_LLM_AUTH=databricks \
  go test ./internal/llm -run '^Test(ChatWithToolsLive_RealProvider|ChatLive_RealProvider)$' -count=1 -v
```

Set the gateway endpoint and model before running the LLM checks. They send
synthetic prompts/evidence only. Results of these runs are in the
[evidence appendix](lakebase-evidence.md#lakebase-guide).

See the [source-linked live findings](https://github.com/jasonmassie01/pg_sage/blob/master/reviews/2026-10-29-lakebase-compatibility.md)
for exact versions, databases, failures and evidence, and the
[Databricks Apps pilot](databricks-apps.md) for packaging and deployed checks.
