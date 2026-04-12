# SQL Reference

pg_sage bootstraps the `sage` schema automatically on first startup. All tables are created by the Go sidecar -- no `CREATE EXTENSION` required.

---

## Schema Tables

### `sage.snapshots`

Raw performance data collected every 60 seconds.

| Column | Type | Description |
|---|---|---|
| `id` | serial | Primary key |
| `captured_at` | timestamptz | When the snapshot was taken |
| `category` | text | Snapshot category (e.g., `pg_stat_statements`, `tables`, `indexes`) |
| `data` | jsonb | Raw snapshot data |

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

### `sage.config`

Key-value configuration store used by the sidecar at runtime.

| Column | Type | Description |
|---|---|---|
| `key` | text | Configuration key (e.g., `emergency_stop`, `trust_level`) |
| `value` | text | Configuration value |
| `updated_at` | timestamptz | Last update timestamp |

The `emergency_stop` key is checked every cycle. Set to `true` to halt all autonomous activity.

---

### `sage.briefings`

Generated health briefings.

| Column | Type | Description |
|---|---|---|
| `id` | serial | Primary key |
| `generated_at` | timestamptz | When the briefing was generated |
| `content` | text | Briefing text (structured or LLM-generated) |
| `findings_snapshot` | jsonb | Findings state at generation time |

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

---

## C Extension SQL Functions (Frozen)

The following functions are part of the C extension (frozen at v0.6.0-rc3). They are available only when the extension is installed on self-managed PostgreSQL. The sidecar does not require them.

| Function | Description |
|---|---|
| `sage.status()` | Extension status and worker health |
| `sage.briefing()` | Generate a health briefing |
| `sage.diagnose(question TEXT)` | Interactive LLM diagnostic |
| `sage.explain(queryid BIGINT)` | EXPLAIN plan capture with narrative |
| `sage.suppress(finding_id INT, reason TEXT, days INT)` | Suppress a finding |
| `sage.emergency_stop()` | Halt all autonomous activity |
| `sage.resume()` | Resume after emergency stop |
| `sage.health_json()` | Health overview as JSONB |
| `sage.findings_json(status TEXT)` | Findings as JSONB array |
| `sage.schema_json(table TEXT)` | Table DDL as JSONB |
| `sage.stats_json(table TEXT)` | Table stats as JSONB |
| `sage.slow_queries_json()` | Top slow queries as JSONB |
| `sage.explain_json(queryid BIGINT)` | Cached plan as JSONB |

These functions are not needed when using the sidecar. The REST API, web UI, and Prometheus exporter provide equivalent functionality.
