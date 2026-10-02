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
