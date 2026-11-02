# Lakebase compatibility and operations

This guide describes the native Lakebase integration delivered through
**pg_sage `2.5.0-dev.10`**. Database and App acceptance evidence was collected
October 29–30, 2026; public platform documentation was cross-checked November 1,
2026. These are scoped development results, **not full Lakebase compatibility
certification** or a production reliability guarantee. A passing local
PostgreSQL suite is separate from a passing native Lakebase test.

The [dev.11–12 follow-up](../reviews/2026-11-02-v2.5-dev12-acceptance.md) adds an
optional App email-domain audience gate and classic meme images. It does not
extend the version-specific database compatibility evidence in this guide.
It also corrects telemetry inbox contention, App request-limit isolation and
event-stream lifetime behavior; those availability checks are reported
separately from feature compatibility.

Start with the [native Databricks implementation guide](databricks-native.md)
for the architecture and linked setup, or the [Databricks Apps guide](databricks-apps.md)
for packaging, resources and platform sign-in. This page owns the database,
telemetry and SRE compatibility boundaries. Earlier acceptance reports are
dated evidence; their original failures and limitations are retained even when a
later revision closes a gap.

The Lakebase integration uses the standard PostgreSQL wire protocol and the
official Databricks Go SDK for unified authentication. It supports local CLI
profiles and Databricks Apps service principal credentials. Short-lived
database tokens are obtained at physical connection time, cached until two
minutes before expiry, and never written into pg_sage configuration or its
metadata database.

The current [dev.10 acceptance](../reviews/2026-10-30-v2.5-dev10-acceptance.md)
verifies Databricks-only App sign-in, native Gateway OAuth M2M, connected Fleet,
database-preserving Cases/Actions/Ask workflows and desktop/mobile rendering.
Cases retain exact technical evidence while distinguishing unknown urgency,
missing measurements and unproven impact. Unnamed indexes no longer receive a
guessed inverse or catalog target; proposals without captured recovery remain
unknown. Inapplicable Lakebase `ALTER SYSTEM` procedures are withheld, including
older saved SQL when current applicability guidance rejects it. Saved and current
procedures retain separate risk/recovery claims; matching a source does not
validate an older proposal.

The [dev.8 acceptance](../reviews/2026-10-30-v2.5-dev8-acceptance.md)
also preserves invalid-index findings during restart warmup and exact query
identities in finding responses and Cases artifacts. The
[dev.6 acceptance](../reviews/2026-10-30-v2.5-dev6-acceptance.md)
fixed singleton Ask Sage selection and Fleet inventory and added hint-free spill
advice. Nine controlled finding families have real Lakebase proof. The
[scenario matrix](../reviews/2026-10-30-nerdlab-findings.md) distinguishes positive
finding/recovery tests from readable sources, provider restrictions and incident
conditions that were not induced.

The [dev.5 acceptance](../reviews/2026-10-30-v2.5-dev5-acceptance.md) passed
one complete 142-package Go invocation, the native PG18 suite and final App
SNAPSHOT `01f1d48e0748107d95b5bdd1ffad0e22`. All five historical SRE adapters
returned positive observations and accepted citations, including exact controlled
plan and DDL events. The same binary separately passed a real-time synthetic
application SLI lifecycle. The App remains in observation mode with metadata-only
consent; App-principal workload-table maintenance execution is unproven.

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

See the [dev.5 native and App source matrix](../reviews/2026-10-30-native-sre-source-matrix.md),
[native action tests](../reviews/2026-10-29-lakebase-native-tests.md),
[provider follow-up](../reviews/2026-10-30-provider-compatibility-gaps.md) and
[dev.10 acceptance](../reviews/2026-10-30-v2.5-dev10-acceptance.md) for the
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

Every mapped hostname is checked against endpoint metadata before its pool
receives a credential. Unmapped hosts retain ordinary PostgreSQL authentication.
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
[original telemetry assessment](../reviews/2026-10-29-lakebase-advanced-telemetry.md)
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
  queries_per_poll: 12
  retention_hours: 168
  bindings:
    - host: '<direct-lakebase-host>'
      database: '<monitored-database>'
      endpoint: projects/<project>/branches/<branch>/endpoints/<endpoint>
      allow_compute: false
```

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
times. See the [provider follow-up](../reviews/2026-10-30-provider-compatibility-gaps.md)
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
The [dev.5 acceptance](../reviews/2026-10-30-v2.5-dev5-acceptance.md#final-app-history-and-slo-evidence)
records exact events, hashes, delays and the preserved failed transport attempt.
Earlier [telemetry](../reviews/2026-10-29-lakebase-advanced-telemetry.md) and
[SRE acceptance](../reviews/2026-10-29-lakebase-sre-acceptance.md) reports retain
their original zero-row and version-specific evidence.

### SRE with and without Advanced Telemetry

Advanced Telemetry is not required for catalog-based monitoring, deterministic
findings or the live database-scoped SRE probes. Without it, pg_sage can read
the permitted `pg_stat_*`/catalog sources and investigate observed database
conditions, but it lacks the provider's retained plan/log/session/DDL history.
With it, historical evidence augments investigation; it does not prove a current
root cause, change live incident state, or authorize maintenance by itself.

The [per-source native matrix](../reviews/2026-10-30-native-sre-source-matrix.md)
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
The [scenario inventory](../reviews/2026-10-30-nerdlab-findings.md#complete-analyzer-inventory-what-the-nine-fixtures-do-not-prove)
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
[October 30 dev.5 App SLO acceptance](../reviews/2026-10-30-app-slo-acceptance.md#dev5-post-upgrade-pass)
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
run passed in 65.86 seconds. The [scenario report](../reviews/2026-10-30-nerdlab-findings.md)
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

## Managed-service limits

| Feature | Verified behavior / limitation |
| --- | --- |
| PostgreSQL 16, 17, 18 | Native TLS/OAuth reconnect tests and collector SQL probes passed against existing Lakebase endpoints |
| `pg_stat_statements` | Supported but must be enabled per database; only 1 of 18 accessible databases had it installed in the October 29 survey. That inventory is not a current platform default or account-wide claim |
| HypoPG | Absent from the surveyed endpoints' available extensions; the PG18 native install attempt returned `42501`. Hypothetical-index validation is unavailable; a real index experiment on a disposable fixture is a separate workflow |
| `pg_hint_plan` | Publicly listed for PG18 and installed, but the planner hook is inactive; `LOAD` returned `0A000` and session/database preload settings returned `42501`. Hint enforcement remains unsupported in this configuration |
| `pgstattuple` | Owned-table inspection passed on the isolated PG18 branch |
| `pgaudit` | October 29 endpoint catalog advertised it, but installation returned `42501`; audit-log integration is unverified. The public extension table rechecked November 1 does not list it; catalog advertisement is not install/activation proof |
| `vector` | PG18 vector lab passed HNSW and exact-baseline comparison for three synthetic queries (recall 1.0); broader workload performance is unverified |
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
unmasking or zero errors. See the [Advanced Telemetry and SRE gap assessment](../reviews/2026-10-29-lakebase-advanced-telemetry.md).

Lakebase's default SRE policy is database-only. A `policy_scope_denied` result
for a cluster/filesystem source describes that trusted policy boundary; it does
not prove the platform lacks the source. A separate, explicitly authorized
read-only native matrix completed all 16 selected cluster/filesystem probes
using the existing role and no added grants. Eleven returned usable rows and
five returned valid empty results. Production policy remains database-only.
Query support, configured authorization and positive incident detection are
three separate claims: empty replication/lock/WAL reads do not establish those
incident or platform semantics. The [provider matrix](../reviews/2026-10-30-provider-compatibility-gaps.md#explicit-provider-probe-compatibility-matrix)
records the exact sources and execution evidence.

The same [provider follow-up](../reviews/2026-10-30-provider-compatibility-gaps.md#planner-extension-incompatibilities-observed)
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

Version `2.5.0-dev.1` explicitly enables eight Lakebase contracts after testing
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
The [dev.10 workflow acceptance](../reviews/2026-10-30-v2.5-dev10-acceptance.md)
covers that distinction, recovery guards and source-preserving proposal
deduplication.

## Operator-requested statistics maintenance

The typed SRE maintenance bridge adds one explicit `create_statistics` request
to a concluded investigation with retained, hash-verified `stat_tables` v3
evidence. An operator/admin supplies the schema, table, columns, supported
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
ownership of workload tables. See the [maintenance evidence and refusal coverage](../reviews/2026-10-30-sre-maintenance.md).

Other mutation families retain their prior provider gates. User-index DROP,
instance settings, OS/log access, broker role authentication, and failover are
not established by these results. See the [native test report](../reviews/2026-10-29-lakebase-native-tests.md)
for actual grants, extension versions, action evidence, and unsupported fixtures.
This remains a development release with explicit managed-platform boundaries.

## Implementation and evidence map

| Implementation | Source / recorded validation |
| --- | --- |
| Verified endpoint mapping and physical-connection credential refresh | [Native OAuth pool hook](../sidecar/internal/databricksauth/lakebase.go); [native compatibility findings](../reviews/2026-10-29-lakebase-compatibility.md) |
| Resource-bound warehouse reader, normalization, durable delivery and retention | [Configuration](../sidecar/internal/config/lakebase_observability.go), [reader and store](../sidecar/internal/lakebaseobs/); [provider follow-up](../reviews/2026-10-30-provider-compatibility-gaps.md) |
| Provider plan provenance, cache and historical evidence | [Provider-plan consumer](../sidecar/internal/autoexplain/provider_plan.go), [historical evidence adapter](../sidecar/internal/lakebaseobs/evidence.go); [source matrix](../reviews/2026-10-30-native-sre-source-matrix.md) |
| Hint-free interval spill finding and recovery guards | [Spill rule](../sidecar/internal/analyzer/rules_temp_spill.go); [nine-scenario inventory](../reviews/2026-10-30-nerdlab-findings.md) |
| Enabled Lakebase action families and connected-database GUC execution | [Provider adapter](../sidecar/internal/fleet/provider_adapter.go), [database setting executor](../sidecar/internal/executor/database_setting.go); [native action tests](../reviews/2026-10-29-lakebase-native-tests.md) |
| SRE statistics proposal, approval and exact-object recovery | [Maintenance bridge](../sidecar/internal/sre/action/maintenance.go); [maintenance acceptance](../reviews/2026-10-30-sre-maintenance.md) |
| Current applicability and recovery presentation | [Case applicability](../sidecar/internal/cases/provider_applicability.go), [recovery normalization](../sidecar/internal/cases/recovery.go); [dev.10 acceptance](../reviews/2026-10-30-v2.5-dev10-acceptance.md) |

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
synthetic prompts/evidence only. All five full advisor workflows passed with
real native Unity Gateway calls and billed-token assertions on both isolated
local PG17 and native Lakebase PG18. The native advisor fixture used a fresh
prefixed database and an in-memory short-lived credential for its raw test
connections; the production OAuth refresh hook is tested separately. Post-run
database inventory confirmed cleanup. No real application query text was sent
to the model.

The real credential-expiry retry passed in 3,634.55 seconds: the held connection
remained healthy through 30 seconds beyond expiry, then a fresh backend used a
second minted credential with a later expiry. This verifies constant-query
connection liveness and production credential refresh for the native user role;
it does not test table grants or UC-only claims across expiry. The first attempt
lost network continuity before expiry and remains recorded separately. Native
idle/resume also passed: the API reported ACTIVE → IDLE → ACTIVE, followed by
production OAuth reconnect and a read-only check of the existing marker,
77 snapshots and 63 findings. The two race-enabled tests passed in 8.077 seconds.
The App's separate service-principal hour-boundary check is tracked in its
acceptance report; failover and multi-day reliability remain unverified.

See the [source-linked live findings](../reviews/2026-10-29-lakebase-compatibility.md)
for exact versions, databases, failures and evidence, and the
[Databricks Apps pilot](databricks-apps.md) for packaging and deployed checks.
