# Lakebase Advanced Telemetry and pg_sage SRE gaps — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

## Assessment

**Latest complete dev4 native run:** a fresh frozen-source invocation passed
79 test/subtest records, failed none and explicitly skipped two in 453.537
seconds after the query-filter fix. The preceding 535.758-second candidate is
preserved separately. Strict telemetry delivery passed all 24 records, with usable controlled
plans/log/DDL and nonempty reads in all ten metadata streams. The two skips are
the separate strict incompatible-extension gate and prior-run attribution
diagnostic, not delivery skips. The [provider follow-up](2026-10-30-provider-compatibility-gaps.md)
preserves exact artifacts, cleanup and scope. Earlier counts below remain
historical results.
An independent exact-window re-read of this final run found nine valid plans
and one self-monitoring exclusion, with no masked/malformed/other rejection;
the classification does not reuse the earlier run's result. The rebuilt
final-source payload/cache/history/hash chain also passed, with its first
filtered negative-query-ID history read completing in 1,430 ms.

**October 30 follow-up:** actual native controlled delivery is now verified.
The corrected strict telemetry gate passed **24/24 records, zero skips** in
51.36 seconds, with eight valid owned plans, one valid synthetic log, one valid
DDL event, all ten metadata streams nonempty and fixture cleanup verified.
The earlier exact application selector exceeded the provider's observed
31-character plan/DDL application-name field. A 28-character unique selector
resolved those false negatives without broadening payload reads. A later
read-only normalization report classified the sole rejected plan as intentional
self-monitoring exclusion. See the
[provider follow-up](2026-10-30-provider-compatibility-gaps.md) for preserved
earlier failures, preview-state caveats, precise evidence and remaining limits.
The full 46-pass/3-skip invocation below remains a historical result. The
following paragraphs preserve the earlier dev.3 assessment.

The subsequent scoped actual-provider chain also passed durable cache/ACK
replay, provenance/context flags, plan hashes and historical SRE evidence
hash/event-chain verification for two captured workload plans, followed by
exact cleanup. It exposed and fixed a pg_sage whole-read deadline bug: the
final historical read completed in 1,230 ms under a bounded five-second
operation with individual 500 ms backend statement limits. See the linked
follow-up for failed attempts and precise native scheduler coverage limits.

A final read-only follow-up normalized all 490 actual metadata events from
the remaining seven streams, with zero rejection and 1,864 numeric metric
values validated. Combined with controlled plan/log/DDL acceptance, all ten
streams now have direct native reader/normalizer coverage under the tested
user identity. A separate deployed-App check also passed one exact synthetic
plan event through its actual scheduler, durable metadata inbox and
authenticated history API. App payload remained absent by consent; raw-plan
cache evidence comes from the native test. This does not establish every
durable consumer or model-selected historical citation; the linked follow-up
preserves these boundaries.

Lakebase provides a supported Beta surface for query plans, Postgres logs,
active-session history, DDL history and compute metrics. The **dev.3 working
tree now implements an opt-in system-table reader and durable history**. All
ten resource-scoped SDK/schema reads passed natively under the selected operator’s `<test-workspace-profile>`
identity. At that earlier checkpoint actual telemetry delivery was
**unverified**: every selected-resource count was zero, including after a
controlled workload, safe timeout and temporary DDL. At that point a usable
observed plan had not traveled through the native Lakebase ingestion path.

The restored full native `-race -count=1 ./tests/lakebaselive` run on October
30 passed in **353.262 seconds: 46 passing test/subtest records, 3 skipped,
0 failed**. Native database, operator, planner and SRE/Gateway checks passed;
the three skips are provider plan/log/DDL delivery, not functional acceptance
of those delivery paths. Standard error was empty.

The earlier Lakebase `auto_explain: unavailable` result described pg_sage's
integration gate, not provider impossibility. A connector, preview flag or
successful empty query alone does not establish usable workload coverage.

SRE remains useful through direct SQL probes, local history and bounded
EXPLAIN; separate dev.3 SRE acceptance is recorded in the SRE findings. At that
dev.3 checkpoint full provider observability was **not established**. Plans had a structured cache
sink and provenance-aware consumers, while logs, ASH, DDL and metrics
fed normalized administrator history only. They were not automatically wired
into historical SRE evidence, RCA/migration fanout or an event-window error SLI.
Asynchronous host metrics remain historical evidence, outside live admission.

This report cross-checks current public documentation against the selected test workspace. Initial discovery was
aggregate-only/read-only. Dev.3 acceptance subsequently used a task-owned
Postgres session and temporary fixture; no preview/endpoint capture settings,
grants or group memberships changed. No customer SQL, plan or log payload was
retrieved or retained. SQL discovery woke the existing serverless warehouse
`633f86eee364e1d1`.

## Current provider contract

The [public telemetry reference](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference),
updated October 19, documents the **Lakebase Observability in System Tables**
Beta. A workspace admin enables that preview in **Previews**. Enabling
**Lakebase Insights** also enables capture, but running the Insights agent is
not required merely to collect or query telemetry. Capture runs while project
computes are active; no destination catalog/schema or exporter credentials are
chosen for the system-table pipeline.

| Surface | Data available | Relevant pg_sage use |
| --- | --- | --- |
| `system.lakebase.plan_history` | Captured execution plans, `queryid`, `duration_ms`, `plan_json`, `plan_fingerprint`, actual/estimated rows, analyze/buffer flags, query/DB/role/application identity | Genuine workload-plan history and plan-regression evidence |
| `system.lakebase.postgres_logs` | Timestamp, severity, SQLSTATE, database/backend/application, message, detail/hint/context and statement | Error SLI, log-triggered RCA and server-event context |
| `system.lakebase.active_session_history`, `wait_event_counters` | Sampled active sessions, blockers, waits and counters | Historical lock/wait analysis beyond a live SQL snapshot |
| `system.lakebase.ddl_history` | Captured top-level DDL, actor, object and timing | Changes outside pg_sage's own action ledger |
| `system.lakebase.pg_stat_statements_counters` | Per-window normalized-query execution statistics | History across collection intervals; do not difference already-windowed values as lifetime totals |
| `system.lakebase.compute_counters`, `compute_gauges` | Compute/host CPU, memory, cache and other resource telemetry | Potential source for host guards, subject to metric semantics and freshness validation |
| `system.lakebase.database_counters`, `database_gauges` | Database counters and point-in-time readings | Historical database-health context |

Rows have project, branch, endpoint and compute identifiers plus capture time.
`compute_id` changes after scale-to-zero/resume. `_counters` rows describe the
interval beginning at `previous_ts`; `_gauges` are point-in-time readings.
Filtering and cursor identity must therefore include the monitored resource,
database and compute/epoch, not just `queryid` or PID.

The [system-table reference](https://docs.databricks.com/aws/en/admin/system-tables/lakebase)
says: **"The system.lakebase tables retain 7 days of data."** The telemetry
reference says: **"Access applies at the schema level, so a user who can read
system.lakebase sees telemetry for every project in the account."** It describes
users with both account-admin and metastore-admin roles as able to read/manage
access, with access delegated through Unity Catalog. A connector needs a
reviewed access arrangement; a Lakebase database grant does not supply these
Unity Catalog permissions or SQL-warehouse access.

`query_text`, `plan_json` and parameters are masked as `<REDACTED>` unless the
reader is an account admin or belongs to the account-level
`databricks_pii_access` group. That is a separate requirement from table read
access. Do not treat masked, NULL, truncated or unparsable plans as usable
evidence. Account-wide PII membership is a material authorization choice, not a
setting pg_sage should grant itself. The final App service principal's six
default metadata-only reads passed independently; payload unmasking/masking
behavior remains unverified.

There is no documented system-table ingestion-lag SLA in the sources reviewed.
Do not transfer the separate OTLP export page's "Within a few minutes" guidance
to this pipeline, or treat asynchronously delivered rows as live snapshots.

### The old Advanced Telemetry configuration changed

The [October 19 release note](https://docs.databricks.com/aws/en/release-notes/lakebase)
says the preview was previously named **Lakebase Advanced Postgres Telemetry**.
Insights is now configured per project, while capture lands in system tables.
It explicitly states: **"Those configurations no longer write telemetry to
their Unity Catalog destinations, so their dashboards retain historical data
but receive no new data."** Old schedules do not carry over.

Use the current public setup instructions for the supported system-table destination.

### Documented limits and separate OTLP export

The telemetry reference states that COPY is not captured in query-plan history;
PL/pgSQL nested statements have a top-level tracking limitation; and DDL around
compute resume can be missed. Capture/sampling is not a guarantee that every
statement execution appears. Public schemas may change before GA.

The documented diagnostic checks are `SHOW neon_monitor.log_capture_enabled`
and `SHOW databricks.o11y_exporter_enabled`; both should be `on`. The docs
recommend restarting an existing compute if either remains `off` after preview
enablement. No restart was performed here, and both were already `on` on the
test endpoint.

[OpenTelemetry export](https://docs.databricks.com/aws/en/oltp/projects/opentelemetry)
is a separate Beta integration: project **Settings → Integrations**, OTLP
endpoint/authentication, and selected metrics/Postgres logs. It forwards through
a managed collector to an external backend. It does not establish a pg_sage
receiver or guarantee that the structured `plan_history` dataset is exported in
the same shape. Suspended computes emit no telemetry until active.

## Live observations in the selected workspace

At approximately **19:39 UTC on October 29**, the `<test-workspace-profile>` user could enumerate
all ten `system.lakebase` tables and execute bounded aggregate queries. The
deployed schema has the documented plan columns plus `workspace_id`,
`project_name`, `branch_name`, `endpoint_name` and `rescue`. Existence of those
additional columns is not evidence they are populated reliably.

| Table | Account-visible rows in the last 7 days | Latest captured timestamp | Rows matching selected `<test-project>` project or the two pg_sage branch IDs |
| --- | ---: | --- | ---: |
| `plan_history` | 439,971 | 19:39:24.228 UTC | 0 |
| `postgres_logs` | 1,486,254 | 19:39:33.211 UTC | 0 |
| `compute_gauges` | 3,198,561 | 19:39:40.968 UTC | 0 |

The selected project API UID is `<project-id>`.
The API-returned internal branch UIDs are
`<branch-id>` (`<native-test-branch>`) and
`<branch-id>` (`<development-resource>`). Project-ID/name checks and branch-ID
checks found no matching rows. A separate plan query also checked
`neon_branch_id` for these branches and found none. This does not prove the
feature is disabled: project inclusion, ID mapping, exporter routing and
capture/delivery still need investigation. The fresh account-wide rows prove
the surface exists and is receiving data somewhere, not that either selected
pg_sage branch is covered.

The final plan aggregate counted 426,947 non-NULL/non-`<REDACTED>` plans and zero
literal `<REDACTED>` values for the current user. It did not retrieve plan/query
contents or establish valid JSON, absence of truncation, completeness, selected
project coverage, or App-principal access. The remaining plan rows were NULL.
Latest-row freshness reflects another resource's most recent event, not a
measured end-to-end delivery latency for a controlled event.

Read-only `pg_settings` on `<test-project>/<native-test-branch>` confirmed:

- `auto_explain` and `neon_monitor` in `shared_preload_libraries`.
- Standard `auto_explain.log_min_duration=-1`, `log_format=text`,
  `log_analyze=off`, `log_buffers=off`; the selected role cannot SET
  `auto_explain.log_min_duration`.
- `databricks.o11y_exporter_enabled=on` and
  `neon_monitor.log_capture_enabled=on`.
- `neon_monitor.plan_enabled=on`, buffer/timing/WAL/nested-plan flags `on`,
  `plan_log_all_min_duration=100`, `plan_log_once_per=1000`, and
  `session_enabled=on`, `session_sample_interval_ms=1000`.
- `neon_monitor.log_capture_min_level=warning`. A logs connector alone would
  not establish delivery of LOG-level checkpoint/temp-file/standard-auto_explain
  events; the capture policy and actual event delivery need separate validation.

These are observations of this endpoint, not a public configuration API or
fleet-wide defaults. The `neon_monitor` settings demonstrate why standard
auto_explain's disabled logging threshold alone cannot establish that Advanced
Telemetry plan capture is disabled. No private capture function was invoked.

Ignored local evidence, containing no credentials or query/plan payloads:

- `.local-test-results/lakebase-advanced-telemetry-gucs.json`
- `.local-test-results/lakebase-advanced-telemetry-monitor-settings.json`
- `.local-test-results/lakebase-advanced-telemetry-discovery.json`

The discovery file records exact SQL, timestamps and warehouse statement IDs.
That initial 19:39 UTC phase used read-only SQL and aggregates, with no workload
or error generated. The later controlled dev.3 acceptance is distinguished below.

## Current pg_sage functionality and gaps

| SRE function | Current source | Dev.3 status and remaining gap |
| --- | --- | --- |
| Lock chains, connection pressure, active waits, vacuum/wraparound, WAL/slot and supported stat-view diagnostics | Bounded direct PostgreSQL catalog/statistics probes | Can function with the required grants/views; each native incident behavior still needs its own validation |
| Bounded generic EXPLAIN | Read-only, plan-only direct SQL by `pg_stat_statements.queryid` | Available independently; it does not reproduce a past parameter-specific execution |
| Plan-regression diagnosis | `sage.query_store` plan hashes and interval latency, plus cached plans | Structured `lakebase_plan_history` ingestion/cache provenance is implemented; native provider-plan delivery remains unverified. Generic estimates remain distinguishable |
| pg_sage-owned changes and config events | Action/config audit, migration findings and direct polling | Available for those sources; normalized provider DDL history does not yet bridge into the automatic SRE change feed |
| Server error SLI | Server-class SQLSTATE log counter divided by transaction delta | No Lakebase event-window numerator/denominator bridge. Missing source remains unavailable; empty delayed reads are not healthy zero-error evidence |
| Log-triggered checkpoint/temp-file/replication-conflict RCA and migration log detection | Local-file log fanout or a supported provider source | Lakebase structured logs have a reader/normalizer/admin history, but no RCA/migration fanout. `warning` capture still does not prove LOG-level event coverage |
| CPU/memory/disk admission/runway evidence | Host/cloud telemetry adapter | Lakebase metrics are historical-only; no system-table sample supplies current CPU, available memory or durable free-space admission |
| Historical waits/active sessions and full DDL context | Typed ASH/DDL reader, durable inbox and normalized administrator history | No automatic historical SRE probe/change-feed bridge yet; sampled ASH and documented DDL gaps remain explicit |
| Application SLOs | Configured external Prometheus/app sources | Independent configuration; not supplied automatically by either Advanced Telemetry or the Lakebase connection |
| Lakebase built-in pooler diagnostics / HA behavior | Topology/admin-specific probes | Not established by a single direct endpoint or by telemetry enablement alone |

Code evidence:

- `sidecar/internal/sre/probes/registry.go`: fixed SQL probe catalog and log-signal mapping.
- `sidecar/internal/sre/investigator_explain.go`: bounded plan-only generic EXPLAIN.
- `sidecar/internal/sre/probes/catalog_maintenance.go`: plan-regression history dependency.
- `sidecar/internal/sre/slo/proxy_errors.go`: missing log counter yields `source_unavailable`.
- `sidecar/internal/sre/changefeed/sources.go`: currently polled change sources.
- `sidecar/cmd/pg_sage_sidecar/database_runtime_logs.go`: local-file watcher/fanout wiring.
- `sidecar/internal/lakebaseobs/`: typed, resource/database-scoped Statement
  Execution reader, structural normalizers, inbox/cursors/ACKs, reconciliation,
  policy-aware ownership/retention and bounded history reads.
- `sidecar/internal/databricksauth/telemetry_scope.go`: endpoint-host verification
  and API resource UID mapping; workspace/Statement Execution accessors retain
  the pinned SDK authentication path.
- `sidecar/cmd/pg_sage_sidecar/lakebase_telemetry_wiring.go`: opt-in runtime,
  structured plan sink, normalized history and independent cleanup wiring.
- `sidecar/internal/autoexplain/` and
  `sidecar/internal/optimizer/plancapture.go`: provider event identity,
  source/resource/compute provenance and plan-consumer selection.
- `sidecar/cmd/pg_sage_sidecar/provider_observability.go`: the existing Supabase
  Prometheus/log adapter remains separate from the structured Lakebase reader.
- `sidecar/internal/fleet/provider_adapter.go`, `runtime_capabilities.go` and
  `sidecar/internal/firstlook/capabilities.go`: current Lakebase unavailable badge/gate.

## Dev.3 implementation and acceptance

The new `internal/lakebaseobs` reader uses the official Go SDK's SQL Statement
Execution interface, verified fixed projections and parameterized project,
branch, endpoint, database and optional application predicates. The actual
`<test-workspace-profile>` resource mapping verified by the API was:

- Project UID `<project-id>`.
- Branch UID `<branch-id>`.
- Endpoint UID `<branch-id>`.
- Postgres database `pg_sage_test`, OID `180227`.

The SDK workspace origin is checked separately; display names and a populated
system-table `workspace_id` are not required for the binding. Compute-wide
streams need explicit authorization. Metadata mode suppresses query, plan,
parameter and free-text log payloads. Optional governed source views must use
validated three-part identifiers and retain the same verified schema contract.
Results are INLINE/JSON only: no external result links receive credentials.
Queries/results have time, row, byte and per-poll limits, with cancellation,
schema checks and offset-bearing event timestamps to avoid a warehouse-session
timezone becoming a false capture time.

The control pool stores inbox events and compare-and-swap cursors atomically,
then acknowledges each idempotent consumer separately. Bounded adaptive windows
stop rather than skip a saturated timestamp range. Rotating reconciliation pins
its upper target, so a moving present cannot strand every sweep at the tail.
Resource/compute provenance preserves late events from an earlier compute.
No query-through cursor is presented as provider completeness; content identity
does not prove exact multiplicity of indistinguishable occurrences.

Payload retention is based on event age, at most seven days and configurable
downward. Payload-free identities/ACKs remain for the source horizon plus a
24-hour margin. Persisted owner/configuration-policy bindings support independent
cleanup when collection is disabled, credentials fail or scope/payload policy
changes. Prior verified scopes can survive a transient SDK failure for the same
trusted configuration; changed or removed bindings revoke old payloads. Scope
ownership prevents one registered database's cleanup from erasing another's
unexpired data. Source/sink lifecycle integration and cache cleanup are tested
separately by the runtime owner.

An owner policy fence additionally serializes inbox commits and sink delivery
against revocation. Verified binding explicitly activates a scope; polling does
not. Cleanup takes the policy update lock before erasure, while accepting
transactions and subscriber delivery/ACK hold shared locks. A previously loaded
event therefore cannot repopulate the plan sink after cleanup. A process-start
UTC epoch rejects older deployment instances trying to reactivate or overwrite a
newer policy; this ordering assumes synchronized workspace clocks and does not
change the stable resource scope hash. Local database barrier tests cover both
commit/revoke orderings, held pending events, payload downgrade and an older
disabled runtime attempting to poll/reactivate.

`NormalizePlan` accepts a verified EXPLAIN object or single-element array and
retains source, capture/receipt time, query identifier, fingerprint, duration
provenance and nullable analyze/buffer/timing flags. It rejects masks, missing
IDs, malformed/truncated/oversized data, inconsistent embedded query IDs,
nonfinite values, self-monitoring and analyze flags without actual rows.
The native `plan_json` shape remains unobserved for this selected resource.
Other normalizers expose typed historical logs, ASH samples, DDL summaries
without SQL, and interval counters/gauges; they provide no live host interface.

Local PG17 unit/database acceptance passed, including the race detector and
`go vet`. Cases cover hostile scope/host/source inputs, result chunks, schema
drift, external-link refusal, cancellation, timezone offsets, masked and
partial data, concurrent cursor CAS, duplicate/late/old-compute events, subscriber
failure after sink commit, payload expiry without replay resurrection, scope
and policy changes, disabled-source cleanup, transient SDK failure, shared
control-pool isolation and history bounds. These are local/offline contracts,
not evidence that Lakebase delivered an observed plan.

The earlier controlled native run used its own external application identity,
two 1.05-second synthetic executions, temporary-table DDL and an expected safe
statement timeout (`57014`). Only that application's plan/log/DDL payloads were
eligible for retrieval. A plan-only EXPLAIN retained only its own query
identifier, `104241301566513084`; its synthetic plan stayed in memory. At
**00:46:44–00:46:59 UTC October 30** (October 29 local time), all ten scoped
schema/SDK reads passed:

| Stream | Verified scoped read | Selected-resource rows |
| --- | --- | ---: |
| `plan_history` | yes | 0 |
| `postgres_logs` | yes | 0 |
| `active_session_history` | yes | 0 |
| `ddl_history` | yes | 0 |
| `compute_counters` | yes | 0 |
| `compute_gauges` | yes | 0 |
| `database_counters` | yes | 0 |
| `database_gauges` | yes | 0 |
| `wait_event_counters` | yes | 0 |
| `pg_stat_statements_counters` | yes | 0 |

The restored full native run repeated the controlled check at
**14:42:35.174531–14:43:35.971206 UTC October 30**, after read-only API
verification of the `<test-workspace-profile>` workspace, ready test branch and expected endpoint.
Its synthetic application was
`lakebaseobs_acceptance_dc666facf80a402a9322e5d074ffeec0`, and its plan-only
EXPLAIN again verified owned query ID `104241301566513084`. The two 1.05-second
executions, temporary DDL and safe timeout (`57014`) succeeded. The synthetic
plan stayed in memory. Exporter, log capture and provider-plan capture were
`on`; minimum log level was `warning` and standard auto_explain duration was
`-1`. All ten scoped reads in the table above again returned **zero rows**;
the three application-filtered delivery reads also returned zero after the
configured additional 30-second wait. The harness deferred the task-owned
session's application-name reset and temporary-table drop, then closed its
pool; those best-effort cleanup calls have no separate delivery assertion.
No capture setting, grant, group membership or private provider API was changed.

The earlier controlled run used 30 seconds of additional polling, also with
zero selected-resource delivery. The earlier timezone-offset run confirmed the
explicit projections and query-ID evidence. In the restored run,
`ControlledDelivery_plan_history`, `ControlledDelivery_postgres_logs` and
`ControlledDelivery_ddl_history` are the three controlled-delivery subtests
explicitly **skipped/unverified**, not native functional successes. Zero rows
prove neither that masking is absent nor that capture is disabled. No ingestion
latency, payload-validity or capture-completeness claim follows.

Final deployed App acceptance separately verified the actual service principal
on `01f1d472793f135aa1802c4e4ef7c979`: all six default metadata-only streams had
fresh successful cursors without query errors or reported retention gaps, and
each bounded history request returned HTTP 200 with zero rows. This establishes
warehouse/query authorization for those projections, not unmasked payload
permission, delivery, exact multiplicity or completeness. All four App resources
were preserved; no broad UC/PII grant was introduced. See the
[final acceptance report](2026-10-29-v2.5-dev3-acceptance.md).

Credential-free, payload-free evidence is saved locally in
`.local-test-results/gap-dev3-telemetry-native.json`,
`gap-dev3-telemetry-native-offset.json` and
`gap-dev3-telemetry-native-final.json`. The restored result is
`.local-test-results/gap-dev3-native-restored-telemetry.json`, with the complete
test event log in `gap-dev3-native-restored.json` and separate empty
`gap-dev3-native-restored.stderr`. Telemetry reports contain timestamps,
resource/query/statement identifiers, setting values and classified counts, not SQL/plan/log
payloads. Do not distribute the surrounding ignored directory wholesale.

Earlier full-native attempts remain preserved: `gap-dev3-native-all.json`
recorded 35 passes and one telemetry setup failure because the warehouse
environment variable was omitted; `gap-dev3-native-final.json` recorded 15
passes and 12 failures after workspace IP-ACL access was lost mid-run. Those
are distinct setup/access failures. The restored run supplies the complete
configuration and passes the full package with the delivery skips stated above.

The native harness is `sidecar/tests/lakebaselive/telemetry_live_test.go`.
Reproduce using the explicitly isolated test endpoint, verified-TLS passwordless
DSN and selected SDK profile already established for the native suite:

```sh
PG_SAGE_LIVE_LAKEBASE_MUTATE=1 \
PG_SAGE_LIVE_LAKEBASE_TELEMETRY=1 \
DATABRICKS_CONFIG_PROFILE=<test-workspace-profile> \
SAGE_LAKEBASE_ENDPOINT=projects/<project>/branches/<branch>/endpoints/<endpoint> \
SAGE_LAKEBASE_LIVE_DATABASE_URL='<verified-TLS passwordless test DSN>' \
SAGE_LAKEBASE_TELEMETRY_WAREHOUSE_ID=633f86eee364e1d1 \
SAGE_LAKEBASE_TELEMETRY_WAIT_SECONDS=120 \
SAGE_LAKEBASE_TELEMETRY_REQUIRE_DELIVERY=1 \
go test -v -count=1 ./tests/lakebaselive -run '^TestLakebaseTelemetry$'
```

Run from `sidecar/`. `SAGE_LAKEBASE_TELEMETRY_REQUIRE_DELIVERY=1` converts absent
usable controlled events into failures, so promotion cannot treat an empty read
as functional telemetry acceptance. The bounded wait is not a promised provider
latency. Without that flag the missing delivery cases remain explicit skips.

## Provider escalation and work remaining

1. Resolve selected-project telemetry visibility first. Check preview status,
   mapping and exporter/project inclusion with the existing resource IDs and
   read-only evidence. Both documented GUC checks are already `on`, so a blind
   restart or unsupported setting change is not an established remedy.
2. App-principal warehouse/metadata query access is now verified. Once scoped
   delivery exists, verify payload permissions and masking independently under
   a reviewed authorization arrangement. No account-wide
   `databricks_pii_access` membership is granted automatically.
3. Supply the sanitized native diagnostic packet for provider investigation:
   workspace and API resource IDs, database OID, controlled application/query
   identity, UTC windows, statement IDs, all ten zero counts and the observed
   exporter/capture/plan flags. Verify project preview inclusion and exporter
   routing rather than guessing a display-name mapping or changing private GUCs.
4. Once delivery exists, prove a real provider plan's shape/flags and end-to-end
   persistence/consumer readback. Test native missing-permission, masking,
   truncation, duplicate delivery and authorized restart/resume boundaries;
   local simulations do not replace those native cases.
5. Implement and independently accept historical SRE evidence/change-feed,
   RCA/migration subscriber and aligned event-window error-SLI bridges where
   provider sampling, severity and completeness permit them. Retain unknown or
   partial outcomes where they do not. Keep delayed metrics out of live guards.

The [current public telemetry contract](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
and [October release notes](https://docs.databricks.com/aws/en/release-notes/lakebase)
remain authoritative for shipped system-table behavior and retired custom-UC
destinations. No provider message or ticket was sent automatically.

This is additional integration and acceptance work. Existing native successes
remain valid within their recorded scope; they do not close these gaps.
