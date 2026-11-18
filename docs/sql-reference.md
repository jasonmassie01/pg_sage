# SQL Reference

pg_sage bootstraps the `sage` schema automatically on first startup. All tables are created by the Go sidecar -- no `CREATE EXTENSION` required.

---

## Schema Tables

### `sage.snapshots`

Raw performance data collected every 60 seconds, one row per category and cycle.

| Column | Type | Description |
|---|---|---|
| `id` | bigserial | Primary key |
| `collected_at` | timestamptz | When the snapshot was taken |
| `category` | text | Snapshot category (`tables`, `indexes`, `queries`, `system`, ...) |
| `data` | jsonb | The document, or for a delta row only what changed |
| `base_id` | bigint | NULL for a full row; for a delta row, the row it is built on |

Catalog categories are stored as a full row (keyframe) plus delta rows. Always read
the document through the accessor, which returns it exactly as collected for full,
delta and legacy rows alike (NULL if the row's base was deleted by hand):

```sql
SELECT collected_at, sage.snapshot_data(data, base_id) AS data
  FROM sage.snapshots
 WHERE category = 'indexes'
 ORDER BY collected_at DESC
 LIMIT 1;
```

The `locks` category is the lock wait graph of the database, stored in full every cycle
and bounded at 200 elements (oldest xmin holders, then blockers, then waiters, then other
idle-in-transaction sessions). Each element is either a waiting lock, with `blocked_by`
(`pg_blocking_pids`, at most 200 waiters are asked), or a session row for a blocker, an
idle-in-transaction session or one of the 5 sessions holding back the xmin horizon most
(`locktype` `virtualxid`, or `session` for a blocker holding a session-level lock outside a
transaction). Elements carry `xact_start`, `backend_xmin_age` (age of the older of
`backend_xmin` and `backend_xid`) and `query_id` (NULL with `compute_query_id = off`). The
`query` text has its string and dollar-quoted literals replaced by `'?'` and `$?$` and is cut
to 256 bytes; numeric literals are kept. pg_sage's own sessions are left out. Locks rows
written by versions before 2.5.1 (every lock of every session, raw statement text) are
deleted once at startup in batches of up to 10,000 (a batch cut by its `statement_timeout`
is retried at a tenth of its size), under `lock_timeout`, within a 5 s budget that never runs
past half of the startup deadline; the rest resumes on the next start. `sage.data_cleanup`
records the cleanup.

The `wait_events` category is this database's active client sessions grouped by wait class
and event, one aggregate of `pg_stat_activity` per collector cycle, stored in full: a list
of `{wait_event_type, wait_event, sessions}`, with a session that has no wait event counted
as `wait_event_type` `CPU` and an empty `wait_event`. It holds no query text and no pid,
and leaves out pg_sage's own sessions. An idle database stores an empty list; a cycle that
could not read it stores no row (unknown, not zero). It is sampled each collector cycle,
not active session history, and it follows the snapshot retention. Wait event names
differ across PostgreSQL 14 to 18 and are kept as plain strings. On managed services the
monitoring role needs `pg_monitor` to see other sessions' wait events. The workload
explorer reads it through `GET /api/v1/wait-load?database=<name>&since=&until=`, which
also names a preferred provider breakdown when one exists, in this order: Performance
Insights, then Lakebase wait counters (see [Lakebase](lakebase.md#workload-explorer-wait-load)),
then these samples, which are always returned; the top
statements of a window come from `sage.query_store` through
`GET /api/v1/queries/top?database=<name|all>&since=&until=&order=load|calls|mean|rows&limit=`
(load is the window's execution seconds per wall-clock second; a statistics reset splits
the window), and one statement's history, plan changes, latest plan and links through
`GET /api/v1/queries/<queryid>?database=<name>`. That page is a viewer read, so the
captured plan's string literals are redacted to `'?'` (an `auto_explain` plan carries the
logged statement's real parameter values). With `agents.code_host` configured,
`GET /api/v1/queries/<queryid>/call-site?database=<name>` names the line of application
code that sends the statement ("Called from <path:line>" with its confidence); see
[MCP for coding agents](mcp.md#call-site-in-application-code).

---

### `sage.findings`

Issues detected by the rules engine and optimizer.

| Column | Type | Description |
|---|---|---|
| `id` | serial | Primary key |
| `created_at` | timestamptz | When first detected |
| `last_seen` | timestamptz | Most recent detection |
| `occurrence_count` | integer | How many times detected |
| `category` | text | Finding category (e.g., `duplicate_index`, `slow_query`) |
| `severity` | text | `critical`, `warning`, or `info` |
| `object_type` | text | Object type (e.g., `index`, `table`, `sequence`) |
| `object_identifier` | text | Fully qualified object name |
| `title` | text | Human-readable summary |
| `detail` | text | Detailed description |
| `recommendation` | text | What to do about it |
| `recommended_sql` | text | SQL to fix the issue |
| `rollback_sql` | text | SQL to undo the fix |
| `status` | text | `open`, `resolved`, `suppressed`, `acted_on` |
| `suppressed_until` | timestamptz | When suppression expires |
| `resolved_at` | timestamptz | When the finding was resolved |
| `acted_on_at` | timestamptz | When an action was taken |

**Common queries:**

```sql
-- All open findings ordered by severity
SELECT category, severity, title, recommended_sql
FROM sage.findings
WHERE status = 'open'
ORDER BY
  CASE severity WHEN 'critical' THEN 1 WHEN 'warning' THEN 2 ELSE 3 END;

-- Critical findings with fix and rollback
SELECT title, recommended_sql, rollback_sql
FROM sage.findings
WHERE severity = 'critical' AND status = 'open';
```

---

### `sage.action_log`

Audit trail for every autonomous action taken (or attempted) by the executor.

| Column | Type | Description |
|---|---|---|
| `id` | serial | Primary key |
| `finding_id` | integer | The finding that triggered this action |
| `action_type` | text | Type of action (e.g., `create_index`, `drop_index`, `reindex`) |
| `action_sql` | text | SQL that was executed |
| `rollback_sql` | text | SQL to reverse the action |
| `outcome` | text | `success`, `failed`, `rolled_back`, `skipped` |
| `before_state` | jsonb | State before the action |
| `after_state` | jsonb | State after the action |
| `executed_at` | timestamptz | When the action was executed |
| `error_message` | text | Error detail if failed |

**Common queries:**

```sql
-- Recent actions
SELECT id, action_type, finding_id, outcome, executed_at
FROM sage.action_log
ORDER BY executed_at DESC
LIMIT 10;
```

---

### `sage.decision`

The decision ledger: every standing-gate verdict, with what it was decided
under. A withheld verdict that repeats every cycle is one row per episode
(`repeat_count`, `last_seen_at`).

| Column | Type | Description |
|---|---|---|
| `id` | bigserial | Primary key; `sage.action_log.decision_id` points here |
| `database_id` | bigint | Fleet database the decision is about (NULL standalone) |
| `feature`, `intent` | text | Change class and intent |
| `target_objects` | jsonb | Objects the change touches |
| `verdict` | text | `execute`, `queue_approval`, `parked`, `blocked`, `observe_only` |
| `risk_tier` | text | `read_only`, `safe`, `moderate`, `high` |
| `reason` | text | Why the gate decided so (for example `deadline_override`) |
| `policy_version` | integer | Standing-policy version evaluated; 0 when the gate decided before reading one (a hard stop such as the emergency stop, or an unreadable policy) |
| `policy_doc_sha256` | text | SHA-256 (64 lower-case hex) of that policy document's stored jsonb text; NULL with version 0 and on rows written before the column existed |
| `guardrails` | jsonb | The action contract's guardrails |
| `off_window_ok` | boolean | A deadline override allowed it outside the maintenance windows |
| `evidence` | jsonb | Gate evidence (budgets, rollback, verification waits) |
| `evidence_id` | text | Unique evidence ID |
| `queue_id`, `action_log_id` | integer, bigint | The queue item or action the decision backs |
| `created_at`, `resolved_at` | timestamptz | When decided; when its episode ended |

The policy is named by version and hash rather than a foreign key: in
fleet mode `sage.policy` lives in the control database, not beside the
ledger. A lease-conflict park or refusal copies its execute decision's
version, hash, guardrails and `off_window_ok`.

```sql
-- Executions that name a policy version but not its document (expect 0)
SELECT count(*) FROM sage.decision
WHERE verdict = 'execute' AND policy_version > 0 AND policy_doc_sha256 IS NULL
  AND created_at > now() - interval '1 day';
```

---

### `sage.config`

Key-value configuration store used by the sidecar at runtime.

| Column | Type | Description |
|---|---|---|
| `key` | text | Configuration key (e.g., `emergency_stop`, `trust_level`) |
| `value` | text | Configuration value |
| `updated_at` | timestamptz | Last update timestamp |

The `emergency_stop` key is checked at every authorization. Set it with
`SELECT sage.emergency_stop(true, '<actor>')` (and `false` to resume), which
upserts the row and writes a `sage.config_audit` entry; see
[Emergency Stop](security.md#emergency-stop). Bootstrap seeds it as
`'false'` (`updated_by = 'bootstrap'`). `emergency_stop_scope` (`fleet` or
`database`) records whether a stop of all databases set the flag, and in
meta-database mode the meta database's `fleet_emergency_stop` holds the
fleet-wide stop; see [Fleet-wide stop](security.md#fleet-wide-stop).

---

### `sage.schema_migrations`

The migration ledger (config-04b): one row per migration step startup ran, in
every database pg_sage bootstraps (monitored, managed and metadata databases).

| Column | Type | Description |
|---|---|---|
| `id` | text | Step name, e.g. `ddlUsersOAuth`; the row `catalog fingerprint` holds the md5 of the `sage` catalog the last complete start left |
| `checksum` | text | SHA-256 (hex) of the step's SQL |
| `applied_at` | timestamptz | When the step was first recorded |
| `pg_sage_version` | text | The pg_sage version that recorded it (`dev` for local builds) |

A start skips the recorded steps while the catalog matches the fingerprint,
and runs every step through its catalog checks otherwise. A step recorded with
another checksum stops startup; deleting its row makes the next start re-run
it. See [How startup migrations behave](deployment.md#how-startup-migrations-behave).

Retention never purges it: it holds one row per step, and an emptied ledger
makes the next start run every step through its catalog checks.

---

### `sage.databases`

Databases registered through the API in meta-db mode, one row each. Passwords are
sealed (`sage-enc:v1`) and never returned by the API. The action identity columns
(security-09b) are both set or both NULL, and the action role is never `username`
(checks `databases_action_credential_pair`, `databases_action_role_distinct`).

| Column | Type | Description |
|--------|------|-------------|
| `username` | text | Observation role |
| `password_enc` | bytea | Observation password, bound to `sage.databases\|<id>\|password` |
| `action_username` | text | Optional action role (a second role on the same database); NULL for one identity |
| `action_password_enc` | bytea | Action password, bound to `sage.databases\|<id>\|action_password`; NULL with `action_username` |

Host, port, database name, TLS mode, trust level, execution mode, tags and the
discovery columns are the other columns; the action identity reuses the row's host,
port, database and TLS mode.

### `sage.briefings`

Generated health briefings.

| Column | Type | Description |
|---|---|---|
| `id` | bigserial | Primary key |
| `generated_at` | timestamptz | When the briefing was generated |
| `period_start`, `period_end` | timestamptz | Window the briefing covers: from the previous briefing's `generated_at` (on the first run, the schedule's previous fire time) to `generated_at` |
| `mode` | text | Briefing mode (`executive`) |
| `content_text` | text | Briefing text (structured or LLM-generated). In an LLM briefing, statements not among pg_sage's recommendations are replaced and each `#<id>` links to its case |
| `content_json` | jsonb | The briefing payload: `findings` (id, case_key, category, severity, title, object_identifier, occurrence_count, risk_tier, recommendation, redacted recommended_sql), `total_open`, `open_by_severity`, `system`, `actions` (executed in the window, newest 100), `window` (`start`, `end`) |
| `llm_used` | boolean | Whether the LLM wrote `content_text` |
| `token_count` | integer | LLM tokens used |
| `delivery_status` | jsonb | Per-channel delivery result, for example `{"slack": {"status": "error", "http_status": 500, "error": "slack rejected the briefing: HTTP 500", "at": "..."}}`. `status` is `sent`, `error` or `skipped`; the webhook URL is never stored. In a fleet roll-up every database's row holds the one message's result. NULL when the row could not be dispatched |

---

### `sage.llm_usage`

The LLM usage ledger in the control database (the meta database, the fleet's first
connected database, or the standalone database). Two kinds of row share it, flushed every
60 s and on shutdown:

- Budget counters (`purpose = '_total'`, empty `model`): today's token spend per counter,
  so a restart mid-day restores it instead of granting a fresh daily budget. A flush only
  raises `tokens`. Pruned after 35 days; the admin budget reset
  (`POST /api/v1/llm/budget/reset`) deletes today's counters.
- Attributed usage (llm-04; every other `purpose`, `role` `general` or `optimizer`): each
  flush adds the calls since the last one, so totals survive restarts. Pruned after 400
  days; read by `GET /api/v1/llm/usage`.

| Column | Type | Description |
|---|---|---|
| `day` | date | UTC day the spend belongs to |
| `database_key` | text | `id:<sage.databases id>` for a meta-DB database, `name:<name>` for a YAML or standalone one, `process` for the process-wide clients and the global ceiling |
| `role` | text | `general`, `optimizer`, `shared` (the `llm.global_token_budget_daily` ceiling) or `fleet` (the database's `fleet_token_budget_daily` share) |
| `tokens` | bigint | Budget counters: tokens spent that day. Attributed rows: prompt plus completion tokens |
| `updated_at` | timestamptz | Last flush that changed the row |
| `purpose` | text | `_total` for a budget counter, else the feature that made the calls (`briefing`, `explain`, `query_tuning`, ...; `unlabeled`, `overflow`) |
| `model` | text | Model id of the calls (empty for a budget counter) |
| `prompt_tokens` | bigint | Prompt tokens |
| `completion_tokens` | bigint | Completion tokens, reasoning included, plus any tokens the provider charged beyond its prompt/completion split |
| `reasoning_tokens` | bigint | Reasoning share of the completion tokens, as the provider reported it |
| `calls` | bigint | Calls counted (every result, as `pg_sage_llm_requests_total`) |
| `failures` | bigint | Calls that did not return `ok` |
| `est_cost_usd` | numeric(20,8) | Cost at the `llm.pricing` rates; NULL when any call in the row had no price |
| `source` | text | `provider`, or `estimate` when pg_sage estimated the split because the provider reported no usage |

Primary key `(day, database_key, role, purpose, model)`.

---

### `sage.explain_cache`

Cached EXPLAIN plans for query analysis.

| Column | Type | Description |
|---|---|---|
| `id` | serial | Primary key |
| `queryid` | bigint | Query ID from `pg_stat_statements` |
| `query_text` | text | Query text |
| `plan` | jsonb | EXPLAIN output in JSON format |
| `source` | text | How the plan was captured (e.g., `on-demand`, `generic_plan`) |
| `total_cost` | double precision | Estimated total cost |
| `execution_time` | double precision | Actual execution time if available |
| `captured_at` | timestamptz | When the plan was captured |
