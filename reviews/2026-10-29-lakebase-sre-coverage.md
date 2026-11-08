# Lakebase SRE functionality and test coverage — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

> Historical acceptance/audit snapshot. For current dev.4 behavior and validation, see the [current acceptance matrix](2026-10-30-v2.5-dev4-acceptance.md) and [per-source native SRE matrix](2026-10-30-native-sre-source-matrix.md). Original results and failures below are retained.

## Assessment

**The later gap-closure work verifies a database-scoped native SRE investigator
with real Unity Gateway tool turns, durable evidence and terminal readback.**
It also verifies owned lock/idle and sequence diagnoses plus a signed runbook.
The source matrix classifies all 32 regular probes and six auxiliary sources;
cluster/filesystem/external sources are explicitly unexecuted by policy.
See [native SRE acceptance](2026-10-29-lakebase-sre-acceptance.md) for current
case IDs, source outcomes, limits and preserved failures/retries.

This still **does not establish full Sage SRE compatibility**. The deployed
App investigator requires its own acceptance, and database scope deliberately
disables several cluster diagnostics, external SLO/pooler sources and backend
actions. The telemetry connector and its source-specific native acceptance are
tracked separately; direct SQL and a working model loop do not certify captured
plans, complete logs, historical events, live resource admission or remediation.

## Earlier audit snapshot

The remainder records the initial audit before the trusted read policy and
native SRE harness were implemented. Statements that a loop or connector was
not yet implemented/verified describe that earlier snapshot; current evidence
and outstanding gates are in the acceptance report above and the gap-closure
plan. The original findings are retained so the scope of the later repairs is
reviewable.

## What the investigator can read

The model receives five data/context tools and a final-answer tool. It cannot
submit arbitrary SQL or directly execute remediation:

| Tool | Actual source and behavior | Important limit |
| --- | --- | --- |
| `run_probe` | One typed probe from the fixed 32-probe catalog | Catalog contains database-local and cluster-wide probes; selecting a database in the API does not filter every SQL source to it |
| `read_stat_view` | `database`, `tables`, `statements` map to `stat_database`, `stat_tables`, `stat_statements` | Current-database counters; statement view excludes text and marks the monitoring role, but is not historical execution-plan ingestion |
| `explain_statement` | Looks up one current-database `pg_stat_statements.queryid`, prepares a read statement, forces a generic plan and issues `EXPLAIN (FORMAT JSON)` | Never `ANALYZE`; does not execute the workload statement; returns bounded shapes/cost/estimated rows, not actual past execution |
| `graph_state` | Re-evaluates the causal graph from stored investigation evidence | Derived context, not a new database observation |
| `confirmed_facts` | Operator-confirmed database context | Context, not citable measured evidence |
| `submit_conclusion` | Structured final output with evidence references | Final-answer protocol, not a sixth data source or mutation tool |

The default narrow model plan allows 5 steps, 3 additional probes, 45 seconds
and 32,000 tokens. Operator/SLO broad plans allow 10 steps, 6 additional probes,
90 seconds and 64,000 tokens. The investigation ceiling is 12 total probes and
120 seconds; tool steps cannot raise it. Narrow plans offer EXPLAIN only for a
plan-regression trigger. All offered read tools are fixed implementation code;
the model supplies only typed arguments.

Probe reads use read-only transactions, a 500 ms statement cap, 100 ms lock
timeout, at most 500 rows/256 KiB and shared concurrency limits. Selected
background runway reads have a separate maximum 2-second timeout. Model-facing
results are further bounded to 20 rows/4,000 runes and scrubbed. Missing
privilege (`42501`), unsupported objects/features, timeouts and successful empty
results have different statuses; an unavailable source must not become a
healthy empty result.

Evidence: `sidecar/internal/sre/investigator_plan.go`,
`investigator_tools.go`, `investigator_explain.go`,
`sidecar/internal/sre/probes/types.go`, `runner.go`, `classify.go`.

## Complete regular probe catalog

The regular registry contains **32 probes**. Signal probes for change feeds,
SLOs and poolers are wired separately; remediation verification also has a
separate action registry (`signal_target`, `recovery_sample`).

| Family | Probe IDs | Sources / scope caveats |
| --- | --- | --- |
| Locks and transactions (5) | `lock_chains`, `lock_graph`, `long_transactions`, `prepared_xacts`, `backend_identity` | `pg_stat_activity`, `pg_blocking_pids`, `pg_locks`, `pg_prepared_xacts`; current-database predicates on application sessions; blocker traversal can include the blocking backend |
| Connections (1) | `connection_saturation` | Cluster-wide `pg_stat_activity` and connection settings; returns application name/client address/state and marks whether each group belongs to the current database |
| Replication and WAL (3) | `replication_lag`, `replication_slots`, `wal_checkpoint` | Replication streams/slots, WAL and bgwriter/checkpointer statistics; cluster-wide resources, not only the selected database |
| Maintenance and plans (3) | `autovacuum_wraparound`, `vacuum_progress`, `plan_regressions` | Table/catalog ages, progress views, and **`sage.query_store`** latency/plan-hash history; not Lakebase `plan_history` |
| Change and archiver (2) | `archiver`, `sage_actions` | `pg_stat_archiver` and pg_sage's own action ledger |
| Checkpoint/temp/standby/waits (6) | `checkpoint_activity`, `temp_file_activity`, `temp_file_holders`, `temp_spill_statements`, `standby_replay_state`, `lwlock_waits` | SQL checkpoint/WAL/IO and database temp counters, **`pg_ls_tmpdir`**, statement temp blocks, standby conflicts/receiver and live wait snapshots |
| Runway (9) | `xid_runway`, `wraparound_tables`, `xmin_horizon`, `autovacuum_cancellations`, `wal_runway`, `cluster_database_size`, `wal_directory`, `sequence_runway`, `runway_trends` | XID/MXID/catalog ages/settings, old snapshots/slots/prepared transactions, database sizes, sequences, `sage.runway_samples`; WAL directory uses **`pg_ls_waldir`/`pg_ls_archive_statusdir`**; cancellation count depends on log-derived **`sage.incidents`** |
| Stat views (3) | `stat_database`, `stat_tables`, `stat_statements` | Current-database cumulative counters, user-table activity and text-free normalized statement counters |

Several multi-sample probes also read server incarnation through
`pg_postmaster_start_time`, **`pg_control_system`**, timeline/WAL identity and,
on a standby, **`pg_control_checkpoint`**. Required predefined monitoring roles,
managed function privileges, versioned view columns, filesystem functions and
topology assumptions need per-probe native verification. This audit did not
run those probes against Lakebase and does not assert that they all fail.

Evidence: `sidecar/internal/sre/probes/registry.go`, `catalog_locks.go`,
`catalog_capacity.go`, `catalog_maintenance.go`, `catalog_change.go`,
`catalog_m6.go`, `catalog_runway.go`, `catalog_sequences.go`,
`catalog_stats.go`, `identity.go`.

## Sources around the tool loop

| Capability | Current implementation | Native Lakebase evidence | Remaining gap |
| --- | --- | --- | --- |
| Durable investigator lifecycle, deterministic probe plan, causal graph, model turns, evidence hashes/transcript | Per-database coordinator and Postgres store, installed by `database_runtime_sre.go` | App starts; no completed native SRE case verified | Run one controlled real-Gateway case and read back lifecycle, evidence, transcript and terminal state |
| Live lock, connection, vacuum, replication, WAL/temp/wait diagnosis | Fixed direct-SQL catalog above | Native collector success does not invoke this catalog as an SRE investigation | Run explicit bounded probe matrix; then controlled incident cases with expected diagnoses |
| SRE model EXPLAIN | `NewStatementExplainer` on monitored pool | Separate optimizer generic EXPLAIN path passed | Invoke the SRE tool itself, including missing queryid/privilege/version behavior |
| Cached estimated plans | `autoexplain.Collector` selects slow statements and issues plan-only EXPLAIN; parameterized PG16+ source `generic_plan` | Native optimizer behavior is available; collector SRE history not independently accepted | Establish provenance and consumer readback; source name `auto_explain` alone does not mean an observed execution |
| Genuine observed plans | `ParseObservedPlan`/`StoreObservedPlan` can persist observed logged plans as `auto_explain_log`; per-node actual execution statistics require analyze-enabled capture | No Lakebase provider reader verified | Bridge Lakebase `plan_history`, validate actual/estimated flags, masking, event time, identity and deduplication |
| Query history / regressions | `querystore.Recorder` combines statement counter samples with cached plan hashes into `sage.query_store` | Slow-query findings/persistence passed | No observed Lakebase execution-plan history or native SRE regression case |
| Server logs and log-triggered RCA | Local readable csvlog/jsonlog watcher/fanout, plus Supabase provider adapter | No direct Lakebase server filesystem; no Lakebase log reader | Read `system.lakebase.postgres_logs` through an explicitly authorized connector |
| Error SLI proxy | Server-class log errors / transaction delta | No working Lakebase log fanout established | Missing counter is `source_unavailable`, not zero errors |
| Change feed | pg_sage actions/config audit, migration findings, statement resets, restart/recovery/timeline/extension polling | No full native SRE signal loop verified | External DDL/provider events incomplete; timeline privilege/topology behavior untested |
| SLO engine | External Prometheus or signed/pushed application SLI; optional DB latency/errors/connections/replication proxies | No native SLO burn investigation verified | Configure and test sources; stale/no-data is unknown; database proxies do not prove an application's SLO |
| Host CPU/memory/storage and maintenance admission | Provider/cloud telemetry hooks; SQL IO evidence and declared capacity/window | Lakebase SQL counters work; no Lakebase host adapter | Database cache/deadlock/block/checkpoint/size counters are not measured host CPU/RAM/free storage; unknown CPU stays nil and can withhold maintenance |
| Pooler diagnosis | Separate pooler sources/signals and topology configuration | No native built-in pooler incident corpus | Verify supported topology and metrics; direct endpoint success is insufficient |
| Runbooks and remediation | Durable typed runbook graph and proposal/action approval path | Native operator suite verifies a bounded Lakebase contract subset, not SRE runbooks | No native SRE runbook/action lifecycle verified; action execution was excluded from this follow-up |

Current provider observability is implemented for Supabase; cloud telemetry
contains AWS RDS/Aurora and GCP paths, with no Lakebase reader found. Searching
Go runtime code for `system.lakebase`, `plan_history` and `postgres_logs` found
no integration. The Lakebase adapter's `LogAccess: unavailable` / auto_explain
unavailable labels describe current pg_sage ingestion capability. They must
not be read as a statement that the Lakebase platform has no logs or plans.

Remediation is a separate safety surface. Lakebase currently enables eight
contracts: `analyze_table`, `vacuum_table`, `create_index_concurrently`,
`revert_created_index`, `reindex_concurrently`, `create_statistics`,
`revert_created_statistics`, `alter_database_guc`. SRE-derived backend
cancel/terminate and other remediation are not enabled by this work. Reading a
diagnosis never directly executes its recommendation; proposals use the
existing policy/approval path.

Evidence: `sidecar/cmd/pg_sage_sidecar/database_runtime_sre*.go`,
`sre_investigator_wiring.go`, `sre_signals.go`, `database_runtime_signals.go`,
`database_runtime_logs.go`, `provider_observability.go`,
`sidecar/internal/autoexplain/{collector,provider_log}.go`,
`internal/querystore/recorder.go`, `internal/logwatch/detect.go`,
`internal/sre/changefeed/sources.go`, `internal/sre/slo/`,
`internal/collector/collect_system.go`, `internal/executor/host_load.go`,
`internal/verify/io_admission.go`, `internal/fleet/provider_adapter.go`.

## Test evidence: tested versus untested

The local PG17 race/coverage run `.local-test-results/go-v25-final.json`
contains the following passing test events, including subtests. These are
local evidence, not a Lakebase test run:

| Package | Pass | Skip |
| --- | ---: | ---: |
| `internal/sre` | 414 | 0 |
| `internal/sre/action` | 59 | 0 |
| `internal/sre/causal` | 215 | 2 |
| `internal/sre/changefeed` | 27 | 0 |
| `internal/sre/pooler` | 16 | 3 |
| `internal/sre/probes` | 215 | 0 |
| `internal/sre/runbook` | 124 | 0 |
| `internal/sre/signed` | 8 | 0 |
| `internal/sre/slo` | 76 | 0 |
| **SRE package subtotal** | **1,154** | **5** |
| `internal/autoexplain` | 89 | 0 |
| `internal/logwatch` | 175 | 1 |
| `internal/rca` | 527 | 2 |
| `sre-bench` | 206 | 2 |
| `sre-bench/replay` | 59 | 0 |

No failures occurred in these listed package streams. Some tests execute real
local PostgreSQL queries/store operations. Investigator DB fixtures explicitly
combine a real local store with scripted probes and a fake OpenAI-compatible
model; replay/fixture success is not evidence of real Gateway protocol/model
behavior or managed Lakebase probe access.

The three native suites in `sidecar/tests/lakebaselive/` test collector/analyzer,
schema/native auth, operator/executor/policy and optimizer/planner features.
They do **not** directly instantiate or invoke the SRE coordinator,
investigator, SLO engine or runbook loop. Five native Lakebase + Gateway advisor
pipelines passed separately in 47.404 seconds; they are advisor validation,
not investigator tool-loop validation. App acceptance covers browser health,
readiness, dashboard/config, EXPLAIN/Gateway and restart/reconnect behavior,
not a completed SRE case.

Keep known native platform refusals separate from untested SRE behavior:
HypoPG was absent; pg_hint_plan was installed but its hook inactive (`LOAD`
returned `0A000`, preload SET privilege unavailable); pgAudit installation was
refused with `42501`. Those are recorded observations. They do not establish
whether every SRE SQL probe is supported or refused.

One evidence qualification matters: the original native final log records
77 durable snapshots, 17 slow-query findings and a replication read canceled
at shutdown. It does not establish an empty final unavailable map; the native
report now records that qualification. The cancellation is not a demonstrated
platform fault. A subsequent test-source improvement captures a pre-shutdown
snapshot, but was added after that binary run. Queries/tables/indexes/sequences
had explicit success assertions; an empty collector map alone would still not
certify every sensor, because some extras failures are only logged.

## Public platform support

The separate [telemetry assessment](2026-10-29-lakebase-advanced-telemetry.md) compares the public system-table contract with read-only aggregate live observations. In this earlier audit, the selected test projects’ telemetry coverage remained unresolved despite fresh account-wide rows.

The [October 19 telemetry reference](https://docs.databricks.com/aws/en/oltp/projects/observability-telemetry-reference)
and [Lakebase system-table reference](https://docs.databricks.com/aws/en/admin/system-tables/lakebase)
document the **Lakebase Observability in System Tables** Beta:

- Admin preview enablement supplies ten `system.lakebase` tables covering
  active sessions, waits, plans, statement counters, DDL, database/compute
  counters/gauges and Postgres logs; retention is seven days.
- `plan_history.analyze_enabled` distinguishes actual execution statistics
  from estimates. Captured plan availability does not imply every plan has
  actual rows, or every workload execution is captured.
- Shared resource identity includes project/branch/endpoint/compute and event
  time; compute identity changes on resume. Connectors need resource/database
  filters, epoch-aware cursors and asynchronous freshness handling.
- UC access is schema-level and can expose every project in the account.
  Plan JSON/query text/parameters are masked unless the reader is an account
  admin or in `databricks_pii_access`. Do not grant broad read/PII access
  automatically or assume the App principal matches the current user's view.
- Table schemas can change before GA. Native PostgreSQL auth, browser App auth
  and Gateway model auth do not grant the UC/warehouse query permissions.

No telemetry settings, grants or group memberships were changed by this audit.

## Controlled acceptance and the current API boundary

The available invocation is:

```text
POST /api/v1/databases/{db}/investigations
{"case_id":"...","kind":"operator","subject":"...","idempotency_key":"..."}
```

It requires an admin/operator and strictly decodes an 8 KiB body. `{db}` must
resolve to one registered database, not `all`; investigation storage/readback
is bound to that database identity. Detail, events, evidence, transcript and
export routes permit inspection of durable results. Stop/resume require the
current version (JSON `version` or `If-Match`). The start request itself
contains no arbitrary SQL or action-execution request.

However, **the request has no probe allowlist or tool configuration**.
`investigator_tools.go` advertises every ID from `probes.Catalog()` to
`run_probe`, including narrow trigger plans. `connection_saturation` reads
other databases' session groups/application names/client addresses;
replication/slots and cluster-size/runway sources also have server scope.
The deterministic operator plan in `signals.go` already includes
`connection_saturation` before any model-selected tool calls, so avoiding that
tool in a prompt would not constrain the initial collection.
Changing subject text or selecting a narrow trigger is not an enforced data
boundary. A manual case cannot therefore guarantee only the fresh `pg_sage`
database in `<project>/<development-branch>` is observed. The additional App case was left
unexecuted as a scope limitation, not an SRE system failure.

No standalone HTTP/MCP endpoint exposing exactly one SRE catalog probe was
found in the inspected runtime routes. `GET .../evidence/...` reads previously
stored evidence; it does not safely invoke a selected probe. Direct native SQL
or a dedicated test harness can check selected current-database sources such
as `stat_database`, `stat_tables`, text-free `stat_statements` or
`long_transactions`, with explicit read-only/timeout controls. Lock-chain
probes start from the current database but follow blockers, so a strict
isolation claim still needs scrutiny. These checks would be individual sensor
acceptance, not the full investigator/Gateway lifecycle.

Recommended next acceptance:

1. Use a dedicated test branch/compute with independently verified isolated
   activity, or an explicit fixture/test mode enforcing a tool/probe allowlist
   in code. Do not rely on model instructions to enforce scope.
2. Keep `automatic_start`/remediation execution disabled, use real Gateway,
   bound steps/probes/time/tokens, and record actual unavailable statuses.
3. Run one synthetic case with known safe test data; export lifecycle,
   evidence hashes, tool transcript, model identity/protocol, terminal outcome
   and unchanged action ledger. An inconclusive diagnosis may be correct for
   a healthy fixture and is distinct from failure to complete the loop.
4. Separately validate each managed probe family and missing-privilege/error
   classification. Then add representative lock/temp/vacuum/plan/SLO cases,
   restarts/lease recovery and pooler topology, without treating one case as
   full coverage.
5. Integrate and accept Lakebase telemetry independently: controlled tagged
   execution/log/DDL readback, masking, permissions, identity/freshness,
   duplicates, retention and compute resume before claiming observed-plan,
   server-log or host-metric support.

Evidence: `sidecar/internal/api/sre_handlers.go`, `sre_operator_handlers.go`,
`sre_transcript_handlers.go`, `sidecar/internal/sre/investigator_tools.go`,
`sidecar/internal/sre/probes/catalog_capacity.go`.
