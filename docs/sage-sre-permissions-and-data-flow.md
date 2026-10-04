# Sage SRE: permissions and data flow

Sage SRE investigates PostgreSQL incidents (lock blocking, connection pressure, WAL and
replication retention, plan regressions). This page says what it reads, which privileges it
needs on each provider, what leaves the database and the pg_sage process, what is sent to an
LLM, how long things are kept, and how to turn each part off.

**In short.** Investigations are read-only. They run a fixed catalog of read-only probes,
store the results as hashed evidence, and diagnose with a deterministic causal graph. If an
LLM is configured, a model turn may reorder the graph's hypotheses, ask for one more catalog
probe and write cited claims; it never runs SQL and never decides the root cause. Nothing is
executed against the database. Since M4 (R1 general availability), investigations start
automatically for each open incident (`sre.automatic_start: true`).

---

## What runs where

```text
monitored database                         pg_sage process                      optional
 ┌──────────────────────┐   read-only   ┌───────────────────────────────┐   HTTPS   ┌──────────┐
 │ pg_stat_activity     │  ───────────► │ probe runner (catalog only)    │ ────────► │ LLM      │
 │ pg_locks, slots, WAL │   500 ms,     │ causal graph (deterministic)   │  redacted │ endpoint │
 │ archiver, settings   │   500 rows,   │ model turn (validated)         │  fenced   └──────────┘
 │ sage.action_log      │   256 KiB     │ redaction                      │
 │ sage.query_store     │               └──────────────┬────────────────┘
 └──────────────────────┘                              │ writes
                                                       ▼
                                  sage.sre_* tables (meta database if configured,
                                  else the monitored database)
                                                       │ redacted reads
                                                       ▼
                                  Cases panel, REST API, MCP read tools, export
```

- **Probes** run in a read-only transaction with a fixed `search_path`, a 500 ms statement
  timeout, a 100 ms lock timeout, at most 500 rows and 256 KiB per probe, one probe at a time
  per database and four per sidecar. Each investigation runs at most 12 probes in 120 s of
  active time. The catalog is fixed and versioned (`internal/sre/probes`); the model can only
  name a catalog probe with typed arguments.
- **Probes return identities, states, counts, ages and settings, never query text.** The
  lock and transaction probes return process ids, backend start times, lock modes, relation
  names and transaction ages. The connection probe returns `application_name` (first 64
  characters), client address and per-state counts. No probe reads table data.
- **Evidence** is stored as the typed probe result (`ok`, `empty`, `error`, `no_privilege`,
  `unsupported`) with its SHA-256 hash. A probe that fails is stored as a failure and shown as
  missing evidence. It is never read as "healthy".

## Privileges

### The role pg_sage connects as

The probes read these sources:

| Probe | Reads | Needs |
|---|---|---|
| `lock_graph`, `lock_chains` | `pg_stat_activity`, `pg_locks`, `pg_blocking_pids()`, `pg_class` | `pg_read_all_stats` (through `pg_monitor`) to see other roles' sessions |
| `long_transactions`, `backend_identity` | `pg_stat_activity` | `pg_read_all_stats` |
| `prepared_xacts` | `pg_prepared_xacts` | none beyond `CONNECT` |
| `connection_saturation` | `pg_stat_activity`, `max_connections`, `superuser_reserved_connections`, `pg_postmaster_start_time()` | `pg_read_all_stats` for other roles' states |
| `replication_slots` | `pg_replication_slots` | none beyond `CONNECT` |
| `replication_lag` | `pg_stat_replication` | `pg_read_all_stats` for lag columns |
| `wal_checkpoint` | `pg_stat_wal`, `pg_stat_bgwriter` (PG14-16) or `pg_stat_checkpointer` (PG17+), `pg_settings` | `pg_read_all_settings` (through `pg_monitor`) |
| `archiver` | `pg_stat_archiver`, `archive_mode` | none beyond `CONNECT` |
| `autovacuum_wraparound`, `vacuum_progress` | `pg_class`, `pg_stat_all_tables`, `pg_database`, `pg_stat_progress_vacuum` | `pg_read_all_stats` |
| `plan_regressions` | `sage.query_store` | `SELECT` on pg_sage's own schema |
| `sage_actions` | `sage.action_log` | `SELECT` on pg_sage's own schema |
| `lwlock_waits`, `standby_replay_state` (M6) | `pg_stat_activity` (wait events; the standby's longest query), `pg_stat_wal_receiver`, `pg_stat_database_conflicts` | `pg_read_all_stats` |
| `temp_spill_statements` (M6) | `pg_stat_statements(false)` (no query text), in the schema the extension is installed in | `pg_read_all_stats` (other roles' query ids); `unsupported` without the extension |
| `temp_file_holders` (M6) | `pg_ls_tmpdir()`, `pg_stat_activity` | `pg_monitor` (`no_privilege` without it) |
| `xid_runway`, `xmin_horizon` (M6) | `pg_database`, `pg_stat_activity` (busy autovacuum workers, xmin holders), `pg_prepared_xacts`, `pg_replication_slots` | `pg_read_all_stats` |
| `wal_directory` (M6) | `pg_ls_waldir()`, `pg_ls_archive_statusdir()` | `pg_monitor` (`no_privilege` without it) |

**Grant `pg_monitor`.** It includes `pg_read_all_stats` and `pg_read_all_settings`. This is
the same grant [installation](installation.md#database-user-setup) and
[security](security.md#required-database-grants) already ask for. Investigations need
nothing else: no superuser, no `pg_signal_backend` (R1 never signals a backend), no write
privilege on application schemas.

```sql
GRANT pg_monitor TO sage_agent;
```

The coordination tables (`sage.sre_investigations`, `sage.sre_evidence`, `sage.sre_events`,
`sage.sre_hypotheses`, the budget ledger and the rest) are created at startup by the schema
bootstrap, in the meta database when one is configured and in the monitored database
otherwise. pg_sage's role needs to own, or be able to write, its `sage` schema there. It
already needs that for the rest of pg_sage.

### Without the privileges

Verified on PostgreSQL 17 with a role that has only `LOGIN`: `pg_stat_activity` still lists
other roles' sessions, but with `state`, `wait_event_type`, `xact_start` and `backend_type`
set to NULL. A lock-graph query would then find "no lock waits" while sessions are blocked,
and connection counts would leave other roles out: a healthy-looking zero caused by a missing
privilege. So the probes that read those columns (`lock_graph`, `lock_chains`,
`long_transactions`, `backend_identity`, `connection_saturation`, `replication_lag`,
`vacuum_progress`, and in M6 `lwlock_waits`, `standby_replay_state`, `temp_spill_statements`,
`xid_runway` and `xmin_horizon`) first check `pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')` in
their read-only transaction. Without the role they return `no_privilege` with reason
`missing_role` and the message "role pg_read_all_stats (granted by pg_monitor) is required",
and run nothing. The investigation lists them under **Missing evidence** and concludes
"inconclusive" rather than guessing. `prepared_xacts`, `replication_slots`, `wal_checkpoint`
and `archiver` work for any role. `TestRunner_ActivityProbesWithoutReadAllStatsAreNoPrivilege`
and `TestRunner_PgMonitorGrantsTheActivityProbes` check this on every supported version.

### Per provider

| Provider | How to grant | Verified |
|---|---|---|
| Self-managed PostgreSQL 14-18 | as a superuser: `GRANT pg_monitor TO sage_agent;` | yes: the probe catalog and the privilege tests run on PG14-18 |
| Google Cloud SQL, AlloyDB | as the `postgres` admin user (a `cloudsqlsuperuser` / `alloydbsuperuser` member): `GRANT pg_monitor TO sage_agent;` | the repo's Cloud SQL and AlloyDB setup (`cloudsqltests/`) runs this grant |
| Azure Database for PostgreSQL (flexible server) | as the server admin (`azure_pg_admin`): `GRANT pg_monitor TO sage_agent;` | see [Azure](azure.md) (CHECK-AZ-02) |
| Amazon RDS / Aurora PostgreSQL | as the master user (`rds_superuser`): `GRANT pg_monitor TO sage_agent;` | not exercised by a live test in this repository |
| Neon, Supabase | as the project owner role: `GRANT pg_monitor TO sage_agent;` | not exercised for investigations; see [Neon and Supabase](neon-supabase.md) for what the tested roles could do |

Disk capacity is never inferred from database sizes. Without a provider disk metric the WAL
family says so (`disk_capacity: provider_metric_unavailable`) and makes no runway claim.

## What leaves the database, and where it goes

| Destination | What | When |
|---|---|---|
| pg_sage process | probe results (typed, capped as above) | each investigation |
| `sage.sre_*` tables | evidence (probe results with hashes), hypotheses, the summary (graph result and model output side by side), the append-only event chain, budget reservations | each investigation |
| Cases panel, REST API, MCP read tools (`sre_list_incidents`, `sre_get_investigation`, `sre_get_evidence`), export | the stored records, **redacted on every read** | when a signed-in user or MCP principal asks; export needs the operator role |
| LLM endpoint (`llm.endpoint`) | the model turn's prompt and, in investigator mode, the results of the reads the model asks for (below) | only with an LLM configured and `sre.llm.enabled: true` (default) |

Nothing else leaves: investigations send no notifications of their own, and the probes make no
network calls.

### What is sent to the LLM

One investigation makes at most 2 model calls. Each call carries:

- fixed instructions (the model's rules, the untrusted-data rule and the reply schema);
- the incident family, trigger kind and causal-graph version;
- the trigger subject and the diagnosis subject, **redacted and fenced** as untrusted data;
- the open and ruled-out hypotheses: graph node ids, labels, mechanisms and graph scores
  (pg_sage's own text);
- one line per evidence item: an alias (`E1`, `E2`, ...), the probe id, its status and reason,
  and the facts the causal graph derived from it. The facts are sentences computed by code
  ("root blocker pid 4242 is idle in transaction", "idle backends of "app" grew from 4 to 9
  in 5 s"). They can contain process ids, relation names, slot names, `application_name` and
  numbers. They are **redacted, fenced and capped at 1200 characters per item**. Raw probe
  rows are not sent;
- missing evidence (probe, status, redacted reason);
- when a probe may be requested, the catalog menu (probe ids and argument types).

Not sent: database credentials or connection strings, query text (no probe collects it), raw
rows (except as below), other investigations, other databases' evidence, pg_sage user data.

In investigator mode (`sre.llm.mode: investigator`, the default), the model makes up to 5
calls (narrow plan) or 10 (broad plan) instead of 2. It also receives the result of each read
it asks for, as the next message. A result is the probe's rows, **redacted, fenced and capped
at 20 rows and 4000 characters**. The reads are catalog probes, the three pg_stat views and,
in the broad plan, a plan-only `EXPLAIN`, whose result is plan node shapes (node type,
relation, index, costs, row estimates), never query text. Confirmed facts and the graph's
state are sent the same way. Every read is stored as evidence with its digest, and the
transcript is redacted on every read like the rest of the investigation.

**Redaction** (`internal/sre/redact.go`) removes connection URIs (anything shaped like
`scheme://user:pass@host`, and any `postgres://` URI), `password=`/`secret=`/`token=`/
`api_key=`-style values, bearer tokens, SQL string literals and raw vectors. It runs before a
prompt is built, before model output is stored, and again on every read (UI, API, MCP,
export).

**Fencing.** Database-derived text sits inside `<data label="...">` blocks whose look-alike
delimiters are neutralized. The system prompt tells the model that the content of those
blocks is data, never instructions. Fencing reduces prompt injection; it does not rely on the
model obeying it. **Authority stays outside the model.** Each reply is validated: known graph
nodes only, catalog probes with checked arguments, evidence ids of this investigation, every
number found in the cited evidence, at most 16 KiB. A conclusive graph root always wins over
the model. R1 has no action the model could trigger.

The replay corpus of PGIncidentBench checks this end to end: cases plant secrets in
`application_name` and probe errors, and instructions in relation names, slot names,
`application_name` and incident subjects. The grader fails a run if a planted secret appears
in the export or in any prompt sent to the model, or if any call outside the probe catalog is
made (`sidecar/sre-bench/README.md`, "Replay corpus").

### Model provider and cost

Any OpenAI-compatible endpoint works, including local and VPC-hosted models, so the prompt
need not leave your network. Each investigation is capped at 2 turns, 16k input and 4k output
tokens, plus 16k reasoning tokens for thinking models; the daily allocation is
`llm.token_budget_daily`. The release report of each milestone records measured token use
for a live model.

## Who can do what

| Role | Can |
|---|---|
| viewer | list and read investigations, evidence and the event timeline (Cases panel, API, MCP read tools) |
| operator, admin | the above, plus export, pin and unpin, start an investigation of a case (`POST .../investigations` with `case_id`, `kind`, `subject`, `idempotency_key`), stop it (`POST .../{id}/stop`, a resumable pause) and resume it (`POST .../{id}/resume`); stop and resume need the version you saw (`If-Match` or JSON `version`) |

Each change is recorded, with the user, in the investigation's append-only event chain. No
role can make an investigation execute anything: R1 has no actions.

## Retention

| Data | Kept | Setting |
|---|---|---|
| Evidence of a finished, unpinned investigation | 30 days, then deleted with a tombstone | `sre.evidence_retention_days` |
| The investigation itself (hypotheses, summary, event chain) | 90 days, then deleted with a tombstone | `sre.timeline_retention_days` |
| Pinned or running investigations | until unpinned and finished | Pin in the Cases panel, or `POST .../pin` |
| Model output | inside the investigation's summary, same lifetime | |
| Budget reservations | with their investigation | |

## Turning things off

| To stop | Set | Effect |
|---|---|---|
| Automatic investigations | `sre.automatic_start: false` | investigations start only when an operator asks (`POST /api/v1/databases/{db}/investigations`); stored ones still resume and are retained |
| The model turn only | `sre.llm.enabled: false` | investigations stay fully deterministic; nothing is sent to the LLM |
| The model's own reads | `sre.llm.mode: review` | the single review turn: at most 2 calls, no probe rows sent |
| Every LLM feature | `llm.enabled: false` (or no `llm.endpoint`) | no LLM traffic at all from pg_sage |
| Keeping data | lower `sre.evidence_retention_days` / `sre.timeline_retention_days` | retention deletes sooner (minimum 1 day) |

All five are restart settings ([configuration](configuration.md#sage-sre-investigations)).
