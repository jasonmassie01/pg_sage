# Configuration

pg_sage uses three configuration sources with the following precedence (highest wins):

1. **CLI flags** (`--database-url`, `--config`, `--mcp-addr`, `--prometheus-addr`)
2. **Environment variables** (`SAGE_DATABASE_URL`, `SAGE_GEMINI_API_KEY`, etc.)
3. **YAML config file** (`config.yaml`)
4. **Built-in defaults**

The sidecar supports hot-reload: changes to the YAML config file are detected and applied without restarting. Connection settings (`postgres.*`, `mcp.listen_addr`, `prometheus.listen_addr`) require a restart.

---

## CLI Flags

```bash
./pg_sage --database-url "postgres://user:pass@host:5432/db" --config config.yaml
```

| Flag | Description |
|---|---|
| `--database-url` | PostgreSQL connection string (overrides YAML and env) |
| `--config` | Path to YAML config file |
| `--mcp-addr` | MCP server listen address (e.g., `0.0.0.0:8080`) |
| `--prometheus-addr` | Prometheus listen address (e.g., `0.0.0.0:9187`) |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SAGE_DATABASE_URL` | (none) | PostgreSQL connection string |
| `SAGE_GEMINI_API_KEY` | (none) | API key for Gemini or any OpenAI-compatible LLM |
| `SAGE_OPTIMIZER_LLM_API_KEY` | (none) | Separate API key for the optimizer model (optional) |
| `SAGE_API_KEY` | (none) | API key for MCP server authentication (empty = no auth) |
| `SAGE_TLS_CERT` | (none) | Path to TLS certificate file for MCP server |
| `SAGE_TLS_KEY` | (none) | Path to TLS private key file for MCP server |
| `SAGE_MCP_PORT` | `8080` | Port for MCP server |
| `SAGE_PROMETHEUS_PORT` | `9187` | Port for Prometheus metrics |
| `SAGE_RATE_LIMIT` | `60` | Max requests per minute per IP on MCP server |
| `SAGE_PG_MAX_CONNS` | `5` | Max PostgreSQL connections in pool |
| `SAGE_PG_MIN_CONNS` | `1` | Min PostgreSQL connections in pool |
| `SAGE_TOKEN_BUDGET` | `10000` | Token budget for LLM calls |

---

## YAML Config File

Full example (see also `sidecar/config.example.yaml`):

```yaml
mode: standalone

postgres:
  host: your-instance-ip
  port: 5432
  user: sage_agent
  password: ${PGPASSWORD}       # env var expansion supported
  database: postgres
  sslmode: require
  max_connections: 5

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 120

trust:
  level: observation             # observation | advisory | autonomous
  maintenance_window: "0 2 * * *"  # cron expression for autonomous actions
  ramp_override_days: 0          # 0 = natural ramp; 31 = skip to autonomous

llm:
  enabled: false
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_GEMINI_API_KEY}
  timeout_seconds: 30
  token_budget: 50000
  optimizer:
    enabled: false
    min_query_calls: 100         # ignore ad-hoc queries below this threshold
    max_indexes_per_table: 10    # skip tables already at this index count
    max_include_columns: 3
    max_new_per_table: 3
    over_indexed_ratio_pct: 80
    write_heavy_ratio_pct: 70
  reasoning_model:               # optional second model for optimizer
    endpoint: ""
    model: ""
    api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}

mcp:
  enabled: true
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"

retention:
  snapshots_days: 90
  findings_days: 180
  actions_days: 365
  explains_days: 90

briefing:
  schedule: "0 6 * * *"         # cron expression
```

---

## Key Settings Reference

### Core

| Parameter | Default | Description |
|---|---|---|
| `mode` | `standalone` | Operating mode |
| `postgres.max_connections` | `5` | Connection pool size |
| `postgres.sslmode` | `disable` | SSL mode (`disable`, `require`, `verify-ca`, `verify-full`) |

### Collection & Analysis

| Parameter | Default | Description |
|---|---|---|
| `collector.interval_seconds` | `60` | Seconds between snapshot collections |
| `analyzer.interval_seconds` | `120` | Seconds between analysis cycles |

### Trust & Actions

| Parameter | Default | Description |
|---|---|---|
| `trust.level` | `observation` | Trust tier: `observation`, `advisory`, `autonomous` |
| `trust.maintenance_window` | (none) | Cron expression restricting when autonomous actions run |
| `trust.ramp_override_days` | `0` | Override the trust ramp timeline (0 = natural ramp) |

The trust model controls what pg_sage is allowed to do:

| Trust Level | Actions Allowed |
|---|---|
| `observation` | No actions; findings only |
| `advisory` | SAFE actions (drop unused/duplicate indexes, VACUUM) |
| `autonomous` | SAFE + MODERATE actions (create indexes, reindex) |

HIGH-risk actions always require manual confirmation regardless of trust level.

### LLM

| Parameter | Default | Description |
|---|---|---|
| `llm.enabled` | `false` | Enable LLM-powered features |
| `llm.endpoint` | (none) | OpenAI-compatible chat completions endpoint |
| `llm.model` | (none) | Model name |
| `llm.api_key` | (none) | API key (supports `${ENV_VAR}` expansion) |
| `llm.timeout_seconds` | `30` | Timeout for LLM API calls |
| `llm.token_budget` | `50000` | Maximum tokens per day |
| `llm.optimizer.enabled` | `false` | Enable index optimizer |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new indexes per table per cycle |

### Retention

| Parameter | Default | Description |
|---|---|---|
| `retention.snapshots_days` | `90` | Days to retain snapshot data |
| `retention.findings_days` | `180` | Days to retain resolved findings |
| `retention.actions_days` | `365` | Days to retain action log entries |
| `retention.explains_days` | `90` | Days to retain EXPLAIN plan captures |
