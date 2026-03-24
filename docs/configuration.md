# Configuration

All pg_sage settings are registered as PostgreSQL GUC (Grand Unified Configuration) parameters under the `sage.*` namespace. Set them in `postgresql.conf`, via `ALTER SYSTEM`, or as command-line arguments.

```sql
-- View current value
SHOW sage.analyzer_interval;

-- Change at runtime (SIGHUP-reloadable parameters)
ALTER SYSTEM SET sage.analyzer_interval = '120s';
SELECT pg_reload_conf();
```

!!! note
    Parameters marked with context `PGC_POSTMASTER` require a full PostgreSQL restart to take effect. All others can be changed with `pg_reload_conf()`.

---

## Core Settings

| Parameter | Type | Default | Context | Description |
|---|---|---|---|---|
| `sage.enabled` | bool | `on` | SIGHUP | Master enable/disable switch for all pg_sage activity |
| `sage.database` | string | `postgres` | POSTMASTER | Database that pg_sage workers connect to |

**Example:**

```ini
sage.enabled = on
sage.database = 'myapp'
```

---

## Collector Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.collector_interval` | int (seconds) | `60` | 10--3600 | SIGHUP | Seconds between snapshot collection cycles |
| `sage.collector_batch_size` | int | `1000` | 100--100000 | SIGHUP | Maximum rows to collect per cycle from `pg_stat_statements` |

**Example:**

```ini
sage.collector_interval = 30
sage.collector_batch_size = 5000
```

---

## Analyzer Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.analyzer_interval` | int (seconds) | `600` | 60--86400 | SIGHUP | Seconds between analysis runs |
| `sage.slow_query_threshold` | int (ms) | `1000` | 100--60000 | SIGHUP | Minimum `mean_exec_time` (ms) to flag a query as slow |
| `sage.seq_scan_min_rows` | int | `100000` | 1000--INT_MAX | SIGHUP | Minimum table rows for a sequential scan to trigger a finding |
| `sage.unused_index_window` | string | `30d` | -- | SIGHUP | Time window after which an unused index is flagged (e.g., `30d`) |
| `sage.index_bloat_threshold` | int (%) | `30` | 5--90 | SIGHUP | Bloat percentage above which an index is flagged |
| `sage.idle_session_timeout` | int (minutes) | `30` | 5--1440 | SIGHUP | Minutes of idle-in-transaction before flagging a session |
| `sage.disk_pressure_threshold` | int (%) | `5` | 1--50 | SIGHUP | Free disk space percentage below which a warning is emitted |
| `sage.max_connections` | int | `2` | 1--5 | POSTMASTER | Maximum database connections pg_sage may use |

**Example:**

```ini
sage.analyzer_interval = 300
sage.slow_query_threshold = 500
sage.seq_scan_min_rows = 50000
sage.index_bloat_threshold = 20
```

---

## Noise Reduction

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.toast_bloat_min_rows` | int | `1000` | 1--INT_MAX | SIGHUP | Minimum table rows before TOAST bloat findings are generated |
| `sage.schema_design_min_rows` | int | `100` | 1--INT_MAX | SIGHUP | Minimum table rows before schema design findings are generated |
| `sage.schema_design_min_columns` | int | `2` | 1--100 | SIGHUP | Minimum columns before schema design findings are generated |

**Example:**

```ini
sage.toast_bloat_min_rows = 5000
sage.schema_design_min_rows = 500
sage.schema_design_min_columns = 3
```

---

## LLM Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.llm_enabled` | bool | `on` | -- | SIGHUP | Enable LLM-powered Tier 2 features |
| `sage.llm_endpoint` | string | `""` | -- | SIGHUP | HTTP endpoint for LLM API calls |
| `sage.llm_api_key` | string | `""` | -- | SIGHUP | API key for LLM service (superuser only) |
| `sage.llm_api_key_file` | string | `""` | -- | SIGHUP | Path to a file containing the LLM API key (alternative to setting `sage.llm_api_key` directly; reloaded on SIGHUP) |
| `sage.llm_model` | string | `""` | -- | SIGHUP | Model name to use for LLM calls |
| `sage.llm_timeout` | int (seconds) | `30` | 5--120 | SIGHUP | Timeout for LLM API calls |
| `sage.llm_token_budget` | int | `50000` | 0--INT_MAX | SIGHUP | Maximum tokens per day across all LLM calls |
| `sage.llm_context_budget` | int | `4096` | 512--32768 | SIGHUP | Maximum tokens for context assembly per LLM call |
| `sage.llm_features` | string | `briefing,explain,diagnostic,shell` | -- | SIGHUP | Comma-separated list of LLM features to enable |
| `sage.llm_cooldown` | int (seconds) | `300` | 30--3600 | SIGHUP | Minimum seconds between LLM calls for the same finding |
| `sage.react_max_steps` | int | `10` | 1--50 | SIGHUP | Maximum steps in a ReAct reasoning chain |

!!! warning
    `sage.llm_api_key` is restricted to superusers. It is not visible in `pg_settings` for non-superuser roles.

**Example:**

```ini
sage.llm_enabled = on
sage.llm_endpoint = 'https://api.anthropic.com/v1/messages'
sage.llm_api_key = 'sk-...'
sage.llm_model = 'claude-sonnet-4-20250514'
sage.llm_token_budget = 100000
```

---

## Auto-Explain Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.autoexplain_enabled` | bool | `off` | -- | SIGHUP | Enable passive EXPLAIN plan capture via ExecutorEnd hook |
| `sage.autoexplain_min_duration_ms` | int (ms) | `1000` | 0--INT_MAX | SIGHUP | Minimum query duration before passive EXPLAIN capture |
| `sage.autoexplain_sample_rate` | float | `0.1` | 0.0--1.0 | SIGHUP | Fraction of slow queries to auto-EXPLAIN (0.1 = 10%) |
| `sage.autoexplain_capture_window` | int (seconds) | `300` | 30--1800 | SIGHUP | Seconds after a finding during which EXPLAIN plans are captured |

!!! tip
    Setting `sage.autoexplain_min_duration_ms = 0` captures all queries. This is not recommended for production workloads.

**Example:**

```ini
sage.autoexplain_enabled = on
sage.autoexplain_min_duration_ms = 500
sage.autoexplain_sample_rate = 0.05
```

---

## Trust and Action Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.trust_level` | string | `observation` | -- | SIGHUP | Trust level: `observation`, `advisory`, or `autonomous` |
| `sage.maintenance_window` | string | `""` | -- | SIGHUP | Cron-style window for autonomous actions. Use `*` or `always` to allow any time |
| `sage.rollback_threshold` | int (%) | `10` | 1--100 | SIGHUP | p95 latency regression percentage that triggers automatic rollback |
| `sage.rollback_window` | int (minutes) | `15` | 5--60 | SIGHUP | Minutes after an action during which rollback is possible |
| `sage.rollback_cooldown` | int (days) | `7` | 1--90 | SIGHUP | Days to wait before retrying a rolled-back action |
| `sage.trust_ramp_override_days` | int | `0` | 0--365 | SIGHUP | Override the trust ramp timeline (0 = use natural ramp). For testing: set to 31 to immediately allow autonomous actions |

The trust model controls what pg_sage is allowed to do:

| Trust Level | Actions Allowed |
|---|---|
| `observation` | No actions; findings only |
| `advisory` | SAFE actions (drop unused/duplicate indexes, vacuum tuning) |
| `autonomous` | SAFE + MODERATE actions (create indexes, reindex, config changes) |

!!! warning
    HIGH-risk actions always require manual confirmation, regardless of trust level.

**Example:**

```ini
sage.trust_level = 'advisory'
sage.maintenance_window = '0 2 * * * UTC'
sage.rollback_threshold = 15
```

---

## Briefing Settings

| Parameter | Type | Default | Context | Description |
|---|---|---|---|---|
| `sage.briefing_schedule` | string | `0 6 * * * UTC` | SIGHUP | Cron expression for daily briefing generation |
| `sage.briefing_channels` | string | `stdout` | SIGHUP | Comma-separated delivery channels: `stdout`, `slack`, `email` |
| `sage.slack_webhook_url` | string | `""` | SIGHUP | Slack incoming-webhook URL (superuser only) |
| `sage.email_smtp_url` | string | `""` | SIGHUP | SMTP URL for email delivery (superuser only) |

**Example:**

```ini
sage.briefing_schedule = '0 8 * * * UTC'
sage.briefing_channels = 'stdout,slack'
sage.slack_webhook_url = 'https://hooks.slack.com/services/...'
```

---

## Privacy Settings

| Parameter | Type | Default | Context | Description |
|---|---|---|---|---|
| `sage.redact_queries` | bool | `off` | SIGHUP | Redact literal values from query texts sent to LLM |
| `sage.anonymize_schema` | bool | `off` | SIGHUP | Anonymize table and column names before sending to LLM |

**Example:**

```ini
sage.redact_queries = on
sage.anonymize_schema = on
```

---

## Cloud Settings

| Parameter | Type | Default | Context | Description |
|---|---|---|---|---|
| `sage.cloud_provider` | string | `""` | SIGHUP | Cloud provider for cost/sizing recommendations (`aws`, `gcp`, `azure`) |
| `sage.instance_type` | string | `""` | SIGHUP | Current cloud instance type for sizing recommendations |

**Example:**

```ini
sage.cloud_provider = 'aws'
sage.instance_type = 'db.r6g.xlarge'
```

---

## Retention Settings

| Parameter | Type | Default | Range | Context | Description |
|---|---|---|---|---|---|
| `sage.retention_snapshots` | int (days) | `90` | 1--3650 | SIGHUP | Days to retain snapshot data |
| `sage.retention_findings` | int (days) | `180` | 1--3650 | SIGHUP | Days to retain resolved findings |
| `sage.retention_actions` | int (days) | `365` | 1--3650 | SIGHUP | Days to retain action log entries |
| `sage.retention_explains` | int (days) | `90` | 1--3650 | SIGHUP | Days to retain EXPLAIN plan captures |

**Example:**

```ini
sage.retention_snapshots = 30
sage.retention_findings = 90
sage.retention_actions = 365
sage.retention_explains = 60
```

---

## Self-Monitoring

| Parameter | Type | Default | Context | Description |
|---|---|---|---|---|
| `sage.max_schema_size` | string | `1GB` | SIGHUP | Maximum size of the `sage` schema before self-throttling |

When the sage schema exceeds this size, pg_sage throttles collection and triggers retention cleanup.

**Example:**

```ini
sage.max_schema_size = '500MB'
```

---

## Sidecar YAML Configuration

The pg_sage sidecar process uses a separate YAML configuration file (`config.example.yaml`) rather than PostgreSQL GUCs. This file controls sidecar-specific settings such as the database connection string, MCP server port, Prometheus exporter port, and LLM provider configuration.

The sidecar supports hot-reload: changes to the YAML config file are detected and applied without restarting the sidecar process. Copy `config.example.yaml` to `config.yaml` and edit it for your environment.
