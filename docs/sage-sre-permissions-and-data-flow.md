# Sage SRE: permissions and data flow

Sage SRE investigates PostgreSQL incidents (lock blocking, connection pressure, WAL and
replication retention, plan regressions, runway and the M6 families). This page says what
it reads, which privileges it needs on each provider, what leaves the database and the
pg_sage process, what is sent to an LLM, how long things are kept, and how to turn each part
off. It has two halves: [investigation](#investigation-read-only), which is read-only, and
[approved actions](#approved-actions-m5-cancel-m6-runbooks-and-maintenance), which change
the database only after a person approves one queued item.

**In short.** Investigations are read-only. They run a fixed catalog of read-only probes,
store the results as hashed evidence, and diagnose with a deterministic causal graph. If an
LLM is configured, the investigator (the default model mode) may ask for more catalog reads,
a plan-only `EXPLAIN` and, on Lakebase, retained history, and writes cited claims; it never
runs SQL of its own and never decides the root cause. Since M4 (R1 general availability),
investigations start automatically for each open incident (`sre.automatic_start: true`).
Since M5, a concluded lock or connection-pressure investigation can propose one
`pg_cancel_backend`; since M6, an operator can propose `CREATE STATISTICS` from an
investigation's evidence, and signed runbooks can end in a proposal. A proposal never runs by
itself: it runs once, after a human approval, through the executor's policy gate.

---

## What runs where

```text
monitored database                         pg_sage process                      optional
 ┌──────────────────────┐   read-only   ┌───────────────────────────────┐   HTTPS   ┌──────────┐
 │ pg_stat_activity     │  ───────────► │ probe runner (catalog only)    │ ────────► │ LLM      │
 │ pg_locks, slots, WAL │   500 ms,     │ causal graph (deterministic)   │  redacted │ endpoint │
 │ archiver, settings   │   500 rows,   │ investigator (validated)       │  fenced   └──────────┘
 │ sage.action_log      │   256 KiB     │ redaction                      │
 │ sage.query_store     │               └──────┬───────────────┬────────┘
 └──────────▲───────────┘                      │ writes        │ approval request
            │ one approved action:             ▼               ▼
            │ pg_cancel_backend, or     sage.sre_* tables   ChatOps channels
            │ CREATE STATISTICS +       (meta database if configured,
            │ ANALYZE (rollback: DROP)  else the monitored database)
            └──────── executor ◄── human approval ◄── redacted reads:
                                                      Cases panel, REST API, MCP read tools,
                                                      export
```

- **Probes** run in a read-only transaction with a fixed `search_path`, a 500 ms statement
  timeout, a 100 ms lock timeout, at most 500 rows and 256 KiB per probe, and one probe at a
  time per database and 4 per sidecar. The investigator's plan-only `EXPLAIN` waits for the
  same slots. A probe that waited 5 s for a slot without one is `concurrency_limit`; an
  investigation's probe refused that way runs once more when the investigation has the time,
  and the second result is stored whatever it is. Each investigation runs at most 12 probes
  in 120 s of active time. Outside investigations, the runway monitor samples the M6 runway
  probes on its own cadence (sequences every 10 minutes by default); there a probe given a
  background budget, such as `sequence_runway`, has a statement timeout of up to 2 s. The
  catalog is fixed and versioned (`internal/sre/probes`); the model can only name a catalog
  probe with typed arguments.
- **Probes return identities, states, counts, ages and settings, never query text.** The
  lock and transaction probes return process ids, backend start times, lock modes, relation
  names and transaction ages. The connection probe returns `application_name` (first 64
  characters), client address and per-state counts. `stat_statements` and
  `temp_spill_statements` read `pg_stat_statements` without its text. No probe reads table
  data.
- **pg_sage's own action sessions carry their decision.** While the executor runs a decided
  action, its session's `application_name` is `pg_sage action:<decision id>`; it is reset to
  `pg_sage` before the connection returns to the pool (a connection whose reset fails is
  closed). In `pg_stat_activity` this tells which decision a running backend executes. The
  name still contains `pg_sage`, so collectors and probes exclude these sessions as before.
  pgaudit records from such a session are kept and tied to the decision only when pgaudit
  logged pg_sage's own login role; the same name set by any other role is ignored.
- **Evidence** is stored as the typed probe result (`ok`, `empty`, `error`, `no_privilege`,
  `unsupported`) with its SHA-256 hash. A probe that fails is stored as a failure and shown as
  missing evidence. It is never read as "healthy".
- **Evidence must be current.** A run that resumes (a restarted sidecar picking up an
  orphaned investigation, or an operator resuming a paused one) keeps the steps it already
  committed. If the newest stored evidence is more than 5 minutes old when the run resumes, it
  runs no probe and no model turn and ends `expired` with failure code `stale_evidence`. No
  diagnosis is shown for that evidence; the Cases panel says "Evidence too old to conclude".
  While the incident or finding is still open, the next trigger poll starts a new attempt that
  collects fresh evidence (see "Attempts" below). Every conclusion records `evidence_newest_at` and
  `evidence_age_s` in its summary, and the panel shows the age when it is more than a sample
  interval. This works the same in single and fleet mode.
- **Attempts.** A run of an automatic trigger (an incident, a plan-regression finding, a
  detector episode, an SLO burn, a runway alert) that ends `expired` with failure code
  `queue_expired` (it waited more than 10 minutes unclaimed, for example during a backlog or a
  sidecar outage) or `stale_evidence` is followed, on the trigger's next poll while it is still
  open, by attempt n+1. The new run records `attempt` and `supersedes` (the expired run's id);
  the expired run shows `superseded_by` and the Cases panel links the two. At most 5 attempts
  run per trigger; after the fifth expires, an `attempts_exhausted` event is recorded once and
  nothing more starts by itself. Every expiry is a `system` transition on the event chain with
  `reason` `queue_expired` or `stale_evidence`. A concluded, inconclusive, failed or stopped run
  is never re-run automatically, and an operator's `idempotency_key` always returns the same
  run. The investigation exports (JSON and Markdown) and a contested run's replay export
  carry `attempt`; the replay case document itself is unchanged.

## Investigation (read-only)

### Investigation privileges

Every read an investigation can make, and what the role pg_sage connects as needs for it:

| Read | Reads | Needs |
|---|---|---|
| `lock_graph`, `lock_chains` | `pg_stat_activity`, `pg_locks`, `pg_blocking_pids()`, `pg_class` | `pg_read_all_stats` (through `pg_monitor`) to see other roles' sessions |
| `long_transactions`, `backend_identity` | `pg_stat_activity` | `pg_read_all_stats` |
| `prepared_xacts` | `pg_prepared_xacts` | none beyond `CONNECT` |
| `connection_saturation` | `pg_stat_activity`, `max_connections`, `superuser_reserved_connections` (plus `reserved_connections` on PG16+), `pg_postmaster_start_time()` | `pg_read_all_stats` for other roles' states |
| `replication_slots` | `pg_replication_slots` | none beyond `CONNECT` |
| `replication_lag` | `pg_stat_replication` | `pg_read_all_stats` for lag columns |
| `wal_checkpoint` | `pg_stat_wal`, `pg_stat_bgwriter` (PG14-16) or `pg_stat_checkpointer` (PG17+), `pg_settings` | `pg_read_all_settings` (through `pg_monitor`) |
| `checkpoint_activity` | `pg_stat_wal`, `pg_stat_bgwriter` or `pg_stat_checkpointer`, `pg_stat_io` (PG17+), `pg_settings` | `pg_read_all_settings` |
| `archiver` | `pg_stat_archiver`, `archive_mode` | none beyond `CONNECT` |
| `autovacuum_wraparound`, `vacuum_progress` | `pg_class`, `pg_stat_all_tables`, `pg_database`, `pg_stat_progress_vacuum` | `pg_read_all_stats` |
| `plan_regressions` | `sage.query_store` | `SELECT` on pg_sage's own schema |
| `sage_actions` | `sage.action_log` | `SELECT` on pg_sage's own schema |
| `lwlock_waits`, `standby_replay_state` (M6) | `pg_stat_activity` (wait events; the standby's longest query), `pg_stat_wal_receiver`, `pg_stat_database_conflicts` | `pg_read_all_stats` |
| `temp_file_activity` (M6) | `pg_stat_database` temp counters, `temp_file_limit`, `work_mem` | none beyond `CONNECT` |
| `temp_spill_statements` (M6) | `pg_stat_statements(false)` (no query text), in the schema the extension is installed in | `pg_read_all_stats` (other roles' query ids); `unsupported` without the extension |
| `temp_file_holders` (M6) | `pg_ls_tmpdir()`, `pg_stat_activity` | `pg_monitor` (`no_privilege` without it) |
| `xid_runway`, `xmin_horizon` (M6) | `pg_database`, `pg_stat_activity` (busy autovacuum workers, xmin holders), `pg_prepared_xacts`, `pg_replication_slots` | `pg_read_all_stats` |
| `wraparound_tables` (M6) | `pg_class`, table `reloptions`, `pg_stat_get_*` vacuum counters, `pg_stat_progress_vacuum` | none beyond `CONNECT` |
| `autovacuum_cancellations` (M6) | `sage.incidents` (log-derived autovacuum cancellations) | `SELECT` on pg_sage's own schema |
| `incident_snapshot` (lock investigations) | `sage.incidents` (the triggering incident's recorded root blockers: pid, `backend_start`, state, sessions blocked, chain depth and the query's hash, at most 5), read once on the first step in a read-only transaction with a 2 s statement and 500 ms lock timeout, in the investigation's own database scope | `SELECT` on pg_sage's own schema. Committed as evidence observed when RCA last recorded the chain: when the live lock graph is empty it names the blocker as it was at detection, as a contributing hypothesis only, never a conclusive root; more than 5 minutes older than the probes it is `stale_evidence` and not used |
| `wal_runway` (M6) | WAL positions, `pg_control_system()`, size settings | none beyond `CONNECT` |
| `wal_directory` (M6) | `pg_ls_waldir()`, `pg_ls_archive_statusdir()` | `pg_monitor` (`no_privilege` without it) |
| `cluster_database_size` (M6) | `pg_database_size()` of every database the role may connect to | `CONNECT` on each database; the others are counted, not sized |
| `sequence_runway` (M6) | `pg_sequence`, `pg_sequence_last_value()`, owning columns | `SELECT` or `USAGE` on each sequence; the others are counted as unreadable |
| `runway_trends` (M6) | `sage.runway_samples` | `SELECT` on pg_sage's own schema |
| `stat_database`, `stat_tables` (investigator) | `pg_stat_database`, `pg_stat_user_tables` | none beyond `CONNECT` |
| `stat_statements` (investigator) | `pg_stat_statements(showtext => false)` counters of this database | `pg_read_all_stats` (other roles' statements); `unsupported` without the extension |
| `explain_statement` (investigator) | `pg_stat_statements(showtext => false)` to confirm the queryid; the statement text from the collector's in-memory snapshot, never the query-text file; a plan-only `EXPLAIN` of the statement | `pg_read_all_stats` for the queryid, and `SELECT` on the tables and views the statement reads: without it the result is `no_privilege` / `missing_select` (`pg_monitor` alone is not enough). A statement outside the collector's top queries is `unsupported` / `text_not_cached`; a write, row-locking or `SELECT INTO` statement, data-modifying CTEs included, is `unsupported` / `not_a_read_statement` before it is prepared |
| `read_history` (investigator, Lakebase) | pg_sage's own retained Lakebase history (`sage.lakebaseobs_*`): `lakebase_plan_history`, `lakebase_log_history`, `lakebase_session_history`, `lakebase_ddl_history`, `lakebase_database_history`; at most 100 structured observations per read, 500 ms per statement and 5 s per read, after the database's physical identity is checked again | `SELECT` on pg_sage's own schema; offered only on Lakebase with `lakebase_observability` enabled and the database bound. It grants no warehouse or Unity Catalog access ([Lakebase](lakebase.md)) |

The stat views, `explain_statement` and `read_history` are offered only to the investigator
(below); the deterministic plan uses the probes above them.

**Grant `pg_monitor`.** It includes `pg_read_all_stats` and `pg_read_all_settings`. This is
the same grant [installation](installation.md#database-user-setup) and
[security](security.md#required-database-grants) already ask for. Investigations need
nothing else: no superuser and no write privilege on application schemas. `EXPLAIN` of a
statement needs `SELECT` on what it reads. `pg_signal_backend` and table ownership are
needed only by [approved actions](#action-privileges).

```sql
GRANT pg_monitor TO sage_agent;
```

**Plan fingerprints and partitions.** `plan_regressions` reports a plan flip when a
statement's `query_store.plan_hash` changes. Since planhash v2 a captured plan names each
partition by its partition root, so a query pinned to one day that scans
`events_p20261105` one day and `events_p20261106` the next keeps one fingerprint. Under
`Append` and `Merge Append` only the set of child shapes, a child-count bucket (`1`,
`2-10`, `>10`, or `all` partitions) and whether runtime pruning removed subplans count;
losing a partition prune still changes the fingerprint. To find the roots, pg_sage runs one
read-only catalog query per capture (`pg_class`, `pg_namespace`, `pg_inherits` and
`pg_partition_root()`, which `CONNECT` alone allows) under a 2 s statement timeout and a
100 ms lock timeout; it takes no relation locks. A fingerprint whose relations were all
resolved starts with `v2:`. One that could not be resolved (no database, a failed read, a
name without `EXPLAIN VERBOSE` that exists in several schemas and is a partition or
partitioned table in one of them, more than 256 relations) starts with `v2u:`. A name shared
by plain tables or indexes in several schemas is its own root, so it resolves.
`plan_regressions` v2 compares a fingerprint only with the previous one of the same
version, so the upgrade from v1 and unresolved captures never report a flip; the cost is
that a flip which also changes the version (a new plan that names an unresolvable relation)
is not reported as a flip. No migration is needed: old `v1:` values age out with the query
store.

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
`vacuum_progress`, `stat_statements`, and in M6 `lwlock_waits`, `standby_replay_state`,
`temp_spill_statements`, `xid_runway` and `xmin_horizon`) first check
`pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')` in their read-only transaction.
Without the role they return `no_privilege` with reason `missing_role` and the message "role
pg_read_all_stats (granted by pg_monitor) is required", and run nothing. The investigation
lists them under **Missing evidence** and concludes "inconclusive" rather than guessing.
`prepared_xacts`, `replication_slots`, `wal_checkpoint` and `archiver` work for any role.
`TestRunner_ActivityProbesWithoutReadAllStatsAreNoPrivilege` and
`TestRunner_PgMonitorGrantsTheActivityProbes` check this on every supported version.

### Per provider

| Provider | How to grant | Verified |
|---|---|---|
| Self-managed PostgreSQL 14-18 | as a superuser: `GRANT pg_monitor TO sage_agent;` | yes: the probe catalog and the privilege tests run on PG14-18 |
| Google Cloud SQL, AlloyDB | as the `postgres` admin user (a `cloudsqlsuperuser` / `alloydbsuperuser` member): `GRANT pg_monitor TO sage_agent;` | not exercised by a live test in this repository (the v0.8 setup scripts were retired) |
| Azure Database for PostgreSQL (flexible server) | as the server admin (`azure_pg_admin`): `GRANT pg_monitor TO sage_agent;` | see [Azure](azure.md) (CHECK-AZ-02) |
| Amazon RDS / Aurora PostgreSQL | as the master user (`rds_superuser`): `GRANT pg_monitor TO sage_agent;` | not exercised by a live test in this repository |
| Databricks Lakebase | the tested identity had `databricks_superuser` and `pg_monitor`; a new principal needs its own grants | see [Lakebase](lakebase.md) |
| Neon, Supabase | as the project owner role: `GRANT pg_monitor TO sage_agent;` | not exercised for investigations; see [Neon and Supabase](neon-supabase.md) for what the tested roles could do |

Disk capacity is never inferred from database sizes. Without a provider disk metric the WAL
family says so (`disk_capacity: provider_metric_unavailable`) and makes no runway claim.

## Approved actions (M5 cancel, M6 runbooks and maintenance)

An investigation never executes anything. An action exists only as a proposal, and a
proposal runs only through this path:

1. It is derived from the investigation's stored evidence (a cancel) or asked for by an
   operator from that evidence (maintenance). The model never chooses an action, and a
   runbook can only end in a proposal.
2. It is queued as one item in the database's existing approval flow, by an operator or, for
   a cancel with `sre.actions.request_approval: true`, at once.
3. A human approves that item (the Actions page, the Cases panel or a signed ChatOps button).
   The approval is single use and records who approved.
4. The executor's policy gate decides as for any approved action: the executor must be
   enabled, `trust.level` must be `advisory` or `autonomous`, the emergency stop holds it
   back, and the policy must allow its change class. The execution mode does not: an approved
   item is already the human step that `approval` and `manual` wait for. The target is
   checked again just before the change.

The keys are in [configuration](configuration.md#sage-sre-approved-actions).

### Backend cancel (M5)

When an investigation concludes that one active backend is the root of a lock or
connection-pressure incident, pg_sage proposes `pg_cancel_backend` of that backend and nothing
else; it never proposes termination. The proposal names the exact backend (pid, backend
start, query start, database, user, query hash and query id). After approval, pg_sage samples
the backend again (`signal_target`) and signals it only in a statement that rechecks the whole
identity, at most `sre.actions.max_evidence_age_seconds` (5 s at most) after the evidence was
sampled. Replication connections, pg_sage's own sessions, `pg_dump`, `pg_basebackup` and
`pg_restore` sessions and the configured protected roles and applications are never
signalled. It then samples the blocking (`recovery_sample`) until a recovery verdict is
reached.

### Runbooks (M6)

Runbooks are operator-written step graphs over catalog probes, predicates and the causal
graph. A runbook runs only after an admin signs its version. An edit stages an unsigned
draft, and the signed version keeps running until an admin signs that draft. Its probe
steps are catalog probes with the same bounds and privileges as any probe, and it ends in
a proposal: an operator step, an action type for the policy gate to judge when someone
requests it, or an escalation. A runbook never executes an action and never runs SQL of
its own.

### Maintenance: extended statistics (M6)

An operator can propose `CREATE STATISTICS` on one table from a concluded or inconclusive
investigation's fresh, hash-verified `stat_tables` evidence of that table; the columns are
the operator's hypothesis. A refresh (`sre_refresh_table_evidence`) re-reads `stat_tables`
once under the investigation's read policy and is recorded as `operator_refresh` evidence
with an `evidence_refreshed` event; it never changes the investigation's state. It runs only where the coordination tables and the target are the same database
(the colocated topology; with a separate meta database pg_sage logs that maintenance is
unavailable). After approval the executor takes `SHARE UPDATE EXCLUSIVE` on the table,
checks that the table, owner and column identities are unchanged and that no object of that
name exists, then runs `CREATE STATISTICS sage_stx_...` and `ANALYZE` of the table, with the
executor's statement timeout and the policy's lock timeout. An admin can roll it back, which
drops only the object it created; prior `ANALYZE` statistics are not restored.

### Action privileges

| Action or read | Runs | Needs |
|---|---|---|
| `cancel_backend` | `pg_cancel_backend(pid)` on one backend, in a statement that rechecks its identity in `pg_stat_activity` | `pg_signal_backend`, or the privileges of the target's role; a superuser's backend only if pg_sage's role is a superuser. Without it the approved cancel fails with PostgreSQL's permission error and nothing is signalled |
| `signal_target`, `recovery_sample` | `pg_stat_activity`, `pg_blocking_pids()`; the query and `application_name` are hashed, never returned | `pg_read_all_stats` (through `pg_monitor`): without it other roles' identities read NULL, never match the evidence, and no cancel is sent |
| `create_statistics` | `LOCK TABLE ... IN SHARE UPDATE EXCLUSIVE MODE`, `CREATE STATISTICS`, `ANALYZE` of the table; rollback `DROP STATISTICS` of the created object | ownership of the table (PostgreSQL requires it for `CREATE STATISTICS`; it also allows the `ANALYZE`) and `CREATE` on the schema the statistics object is created in |

```sql
GRANT pg_signal_backend TO sage_agent;   -- only for approved backend cancels
```

`pg_signal_backend` also lets the role signal other roles' non-superuser backends outside
pg_sage's checks, so grant it only where you want approved cancels.

| Provider | `pg_signal_backend` | Verified |
|---|---|---|
| Self-managed PostgreSQL 14-18 | as a superuser: `GRANT pg_signal_backend TO sage_agent;` | yes: the agent kill tests grant and revoke it on a real server |
| Google Cloud SQL, AlloyDB | as the `postgres` admin user | not exercised by a live test in this repository (the v0.8 setup scripts were retired) |
| Azure Database for PostgreSQL (flexible server) | as the server admin (`azure_pg_admin`) | yes: the documented setup, this grant included, ran without error on every flavor ([Azure](azure.md)) |
| Amazon RDS / Aurora PostgreSQL | as the master user (`rds_superuser`) | not exercised by a live test in this repository |
| Databricks Lakebase | not established | no: backend signalling is restricted or unproven there and is not an enabled Lakebase action ([Lakebase](lakebase.md)) |
| Neon, Supabase | as the project owner role | not exercised |

With a separate action identity (`action_dsn`,
[security](security.md#separate-observation-and-action-roles-action_dsn)), grant
`pg_signal_backend` and `pg_read_all_stats` to the action role, not to the observation
role: approved cancels run as the action role, and AP-14 warns when the observation role
holds `pg_signal_backend`.

## What leaves the database, and where it goes

| Destination | What | When |
|---|---|---|
| pg_sage process | probe results (typed, capped as above) | each investigation |
| `sage.sre_*` tables | evidence (probe results with hashes), hypotheses, the summary (graph result and model output side by side), the append-only event chain, budget reservations, action and maintenance proposals | each investigation |
| Cases panel, REST API, MCP read tools (`sre_list_incidents`, `sre_get_investigation`, `sre_get_evidence`), export | the stored records, **redacted on every read** | when a signed-in user or MCP principal asks; export needs the operator role |
| LLM endpoint (`llm.endpoint`) | the investigator's messages and the results of the reads it asks for, or the review turn's prompt (below); a runbook draft to compile | only with an LLM configured and `sre.llm.enabled: true` (default); compiling only when an operator asks |
| Slack and Telegram channels with an `approval_needed` rule | a cancel proposal's approval card: the database, the target (pid, backend start, user), how many sessions it blocks, the cancel SQL and its expiry; never query text. The buttons carry only a single-use card token | when a cancel proposal is queued for approval |
| The Slack, Telegram and email channels an incident's page was sent to | the diagnosis follow-up (`investigation_concluded`): incident id, database, family, the root cause's label (or "inconclusive"), one line of the diagnosis reason, redacted like every read (credentials, connection URIs, bearer tokens, SQL literals) and clipped to 200 characters, and the case id. Never SQL, query text, application or user names, or evidence | once per incident, when its newest conclusion has settled for 60 s; never to PagerDuty or to a channel the page did not reach |

Nothing else leaves: investigations send no other notifications of their own, and the probes
make no network calls.

### Deciding a cancel from chat

A cancel proposal is decided in chat through its approval card, the same protocol as every
other queued action. The card token works only in the channel it was sent to, only while the
queued cancel and its target (pid plus backend start) are unchanged, and only until the
earlier of 15 minutes and the approval request's expiry. The card has Approve and Reject
and no Snooze. Each callback must carry the provider's signature, come from the channel's
workspace or chat, be new (replays are refused) and come from a chat user mapped to an
operator or admin. A Reject records the denial on the proposal at once. The Approve / Deny
buttons of earlier releases carried only the proposal id; for one release a press of one is
answered "This button is outdated; open pg_sage to decide" and changes nothing.

### What is sent to the LLM

In investigator mode (`sre.llm.mode: investigator`, the default) the model works through a
bounded tool-calling loop. Its plan sets how far it may go:

| Plan | Used for | Model steps | Extra probes | Wall clock | Token cap | Step answer cap | Tools |
|---|---|---|---|---|---|---|---|
| `narrow` | every other trigger (incidents, detector episodes, plan regressions, runway alerts) | 5 | 3 | 45 s | 32,000 | 1,024 | `run_probe`, `read_stat_view`, `graph_state`, `confirmed_facts`, `read_history`; `explain_statement` for `plan_regression` |
| `broad` | operator-started (`operator`) and SLO-burn (`slo_burn`) investigations | 10 | 6 | 90 s | 64,000 | 1,024 | `run_probe`, `read_stat_view`, `explain_statement`, `graph_state`, `confirmed_facts`, `read_history` |

The token cap counts prompt, answer and reasoning tokens over every step. Configuration
cannot raise the hard ceilings: 12 steps, 120 s, the investigation's 12 probes, and no plan
can exceed 64,000 tokens. A tool is offered only when its reads are allowed for the database
(`read_history` only on Lakebase with history bound). The first message carries:

- fixed instructions (the model's rules, the untrusted-data rule and the conclusion schema);
- the incident family, trigger kind and causal-graph version;
- the trigger subject and the diagnosis subject, **redacted and fenced** as untrusted data;
- the graph result, and the open and ruled-out hypotheses: graph node ids, labels,
  mechanisms and graph scores (pg_sage's own text);
- missing evidence (probe, status, redacted reason);
- similar past incidents of this database and confirmed facts, fenced, as context;
- the offered tools and the catalog menu (probe ids and argument types).

Each later message is the result of one read the model asked for: the probe's or view's rows,
history observations, or `EXPLAIN` plan node shapes (node type, relation, index, costs, row
estimates, never query text), **redacted, fenced and capped at 20 rows and 4,000
characters**. Every read is stored as evidence with its digest, and the transcript is
redacted on every read like the rest of the investigation.

In review mode (`sre.llm.mode: review`), the opt-down, a single review turn reads the
graph's result instead: one line per evidence item (an alias `E1`, `E2`, ..., the probe id,
its status and reason, and the facts the causal graph derived from it, **redacted, fenced and
capped at 1200 characters per item**; raw probe rows are not sent), the hypotheses, the
missing evidence and, when a probe may be requested, the catalog menu. It may ask for one
more catalog probe. The facts are sentences computed by code ("root blocker pid 4242 is idle
in transaction") and can contain process ids, relation names, slot names, `application_name`
and numbers.

Compiling a runbook draft (`POST .../runbooks/compile`) sends the operator's playbook text,
fenced as untrusted data, with the trigger kinds, catalog probes, graph nodes and action types
it may use. No database data is sent.

Not sent: database credentials or connection strings, query text (no probe collects it, and
an `EXPLAIN` result carries only plan shapes), raw rows except as above, other
investigations, other databases' evidence, pg_sage user data.

**Redaction** (`internal/sre/redact.go`) removes connection URIs (anything shaped like
`scheme://user:pass@host`, and any `postgres://` URI), `password=`/`secret=`/`token=`/
`api_key=`-style values, bearer tokens, SQL string literals and raw vectors. It runs before a
prompt is built, before model output is stored, and again on every read (UI, API, MCP,
export).

**Fencing.** Database-derived text sits inside `<data label="...">` blocks whose look-alike
delimiters are neutralized. The system prompt tells the model that the content of those
blocks is data, never instructions. Fencing reduces prompt injection; it does not rely on the
model obeying it. **Authority stays outside the model.** Each reply is validated: known graph
nodes only, catalog reads with checked arguments, evidence ids of this investigation, every
number found in the cited evidence, at most 16 KiB. A conclusive graph root always wins over
the model. The model has no tool that changes anything and cannot propose an action: a
cancel proposal is derived by code from the stored evidence, and every action still needs a
human approval.

The replay corpus of PGIncidentBench checks this end to end: cases plant secrets in
`application_name` and probe errors, and instructions in relation names, slot names,
`application_name` and incident subjects. The grader fails a run if a planted secret appears
in the export or in any prompt sent to the model, or if any call outside the probe catalog is
made (`sidecar/sre-bench/README.md`, "Replay corpus").

### Model provider and cost

Any OpenAI-compatible endpoint works, including local and VPC-hosted models, so the prompt
need not leave your network. The daily allocation is `llm.token_budget_daily`. The release
report of each milestone records measured token use for a live model.

The investigator reserves every call against its plan's token cap (32,000 narrow, 64,000
broad): the request estimate, the step's answer cap (1,024 tokens) and, for thinking models,
2,048 reasoning tokens per call. Its loop counts the same reasoning allowance when it decides
whether two more calls fit, so near the cap it asks only for the conclusion while that call
still fits. If the cap still refuses a call that offered more than the conclusion (a resumed
run finds earlier runs' tokens held), the call is retried once as conclusion-only, its answer
capped at what remains (at least 256 tokens). A refusal by a daily allocation is never
retried.

A review-mode investigation makes at most 2 model turns, 16,000 input and 4,000 output
tokens, plus 16,384 reasoning tokens for thinking models.

## Who can do what

| Role | Can |
|---|---|
| viewer | list and read investigations, evidence, the event timeline, action and maintenance proposals, runbooks and their runs (Cases panel, API, MCP read tools) |
| operator | the above, plus export, pin and unpin, start an investigation of a case (`POST .../investigations` with `case_id`, `kind`, `subject`, `idempotency_key`), stop it (`POST .../{id}/stop`, a resumable pause that never expires) and resume it (`POST .../{id}/resume`); stop and resume need the version you saw (`If-Match` or JSON `version`). Propose a cancel and request its execution (`POST .../{id}/proposals`, `.../proposals/{pid}/request`), propose and request maintenance (`POST .../{id}/maintenance`, `.../maintenance/{pid}/request`), approve or reject a queued action (`POST /api/v1/actions/{id}/approve`, `.../reject`, or a ChatOps button), and draft, compile and revise runbooks (a revision stages a draft; the signed version keeps running) |
| admin | the above, plus sign a runbook version (`POST .../runbooks/{id}/sign`), retire a runbook (`POST .../runbooks/{id}/retire`, permanent) and roll back maintenance (`POST .../maintenance/{pid}/rollback`) |

Each change is recorded, with the user, in the investigation's append-only event chain. No
role can make an investigation execute anything; an action runs only after a person approves
its one queued item, and the policy gate can still refuse it.

## Operator maintenance (extended statistics)

An operator can propose `CREATE STATISTICS` from a concluded or inconclusive
investigation's retained `stat_tables` evidence, refreshed if it is older than 15 minutes. It runs only after human approval, only where the coordination
and target databases are the same database (colocated topology), and a rollback drops only
the object it created; prior `ANALYZE` statistics cannot be restored.

A run that dies between its claim and its record (a crash, or a failed write to the
control database) is settled by the action pass once it is older than twice the executor's
apply timeout plus one minute (13 minutes with the default `safety.ddl_timeout_seconds`;
the wait for a DDL slot is bounded to one apply timeout, so a live run never reaches it).
Its age is measured on the database clock that stamps the proposal, so a skewed sidecar
host clock cannot settle a live run early.
The pass takes the table lock the executor takes, reads the catalog for the proposal's
`sage_stx_<id>` object on the bound table and the exact `action_log` record, and never
re-runs the forward SQL or the DROP:

| Stranded in | Found | Settled as |
|---|---|---|
| `executing` | the action record and the object on the bound table | `uncertain` with both; typed rollback works |
| `executing` | neither (or only a failed action record) | `failed` |
| `executing` | anything else (object without a record, a record without the object, the name on another table) | `uncertain` without an object; rollback is refused and an operator must review |
| `rolling_back` | the created object is gone | `rolled_back` |
| `rolling_back` | the created object still exists | `rollback_failed`; the rollback can be retried |

Each settlement adds an `action_failed` or `action_executed` event marked `reconciled`.

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
| The model's own reads | `sre.llm.mode: review` | the single review turn: no probe rows sent |
| Every LLM feature | `llm.enabled: false` (or no `llm.endpoint`) | no LLM traffic at all from pg_sage |
| Automatic cancel proposals | `sre.actions.proposals: false` | concluded investigations no longer propose a cancel by themselves; an operator can still propose one |
| Approval requests sent at once | `sre.actions.request_approval: false` | a cancel proposal is queued and sent to ChatOps only when an operator requests it |
| Every approved action | `trust.level: observation` | the policy gate withholds every proposal, and the proposal shows why |
| Keeping data | lower `sre.evidence_retention_days` / `sre.timeline_retention_days` | retention deletes sooner (minimum 1 day) |

The settings are restart settings ([configuration](configuration.md#sage-sre-investigations)).
