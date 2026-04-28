# Configuration

pg_sage uses three configuration sources with the following precedence (highest wins):

1. **CLI flags** (`--pg-url`, `--config`, `--prom-addr`, `--meta-db`, `--encryption-key`)
2. **Environment variables** (`SAGE_DATABASE_URL`, `SAGE_LLM_API_KEY`, etc.)
3. **YAML config file** (`config.yaml`)
4. **Built-in defaults**

The sidecar supports hot-reload: changes to the YAML config file are detected and applied without restarting. Connection settings (`postgres.*`, `prometheus.listen_addr`, `api.listen_addr`) require a restart.

---

## CLI Flags

```bash
./pg_sage --pg-url "postgres://user:pass@host:5432/db" --config config.yaml
```

| Flag | Description |
|---|---|
| `--pg-url` | PostgreSQL connection string (overrides YAML and env) |
| `--config` | Path to YAML config file |
| `--prom-addr` | Prometheus listen address (e.g., `0.0.0.0:9187`) |
| `--meta-db` | Metadata database connection string (PostgreSQL URL for fleet mode) |
| `--encryption-key` | Encryption key for sensitive config values |

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `SAGE_DATABASE_URL` | (none) | PostgreSQL connection string |
| `SAGE_LLM_API_KEY` | (none) | API key for Gemini or any OpenAI-compatible LLM |
| `SAGE_OPTIMIZER_LLM_API_KEY` | (none) | Separate API key for the optimizer model (optional) |
| `SAGE_API_KEY` | (none) | Legacy config field; the current web/API path uses session login cookies |
| `SAGE_TLS_CERT` | (none) | Legacy/reserved; terminate TLS at a reverse proxy |
| `SAGE_TLS_KEY` | (none) | Legacy/reserved; terminate TLS at a reverse proxy |
| `SAGE_PROMETHEUS_PORT` | `9187` | Port for Prometheus metrics |
| `SAGE_RATE_LIMIT` | `60` | Max requests per minute per IP on REST API |
| `SAGE_PG_MAX_CONNS` | `2` | Max PostgreSQL connections in pool |

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
  max_connections: 2

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 600

trust:
  level: observation             # observation | advisory | autonomous
  maintenance_window: "0 2 * * *"  # cron expression for autonomous actions
  ramp_start: ""                 # Auto-persisted on first start; set to override

llm:
  enabled: false
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 30
  token_budget_daily: 500000
  optimizer:
    enabled: false
    min_query_calls: 100         # ignore ad-hoc queries below this threshold
    max_indexes_per_table: 10    # skip tables already at this index count
    max_include_columns: 3
    max_new_per_table: 3
    over_indexed_ratio_pct: 80
    write_heavy_ratio_pct: 70
  optimizer_llm:                 # optional second model for optimizer
    endpoint: ""
    model: ""
    api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}

api:
  listen_addr: "0.0.0.0:8080"
  # Web UI and /api/v1 endpoints are session-authenticated.
  # The first local admin is bootstrapped automatically.

notifications:
  slack:
    enabled: false
    # webhook_url: ${SAGE_SLACK_WEBHOOK}
  pagerduty:
    enabled: false
    # routing_key: ${SAGE_PAGERDUTY_KEY}

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
| `mode` | `extension` | Operating mode; set `standalone` explicitly for sidecar-only deployments |
| `postgres.max_connections` | `2` | Connection pool size |
| `postgres.sslmode` | `prefer` | SSL mode (`disable`, `prefer`, `require`, `verify-ca`, `verify-full`) |

### Collection & Analysis

| Parameter | Default | Description |
|---|---|---|
| `collector.interval_seconds` | `60` | Seconds between snapshot collections |
| `analyzer.interval_seconds` | `600` | Seconds between analysis cycles |

### Trust & Actions

| Parameter | Default | Description |
|---|---|---|
| `trust.level` | `observation` | Trust tier: `observation`, `advisory`, `autonomous` |
| `trust.maintenance_window` | (none) | Cron expression restricting when autonomous actions run |
| `trust.ramp_start` | (auto) | Auto-persisted on first start; set to override |

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
| `llm.token_budget_daily` | `500000` | Maximum tokens per day |
| `llm.optimizer.enabled` | `false` | Enable index optimizer |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new indexes per table per cycle |

### Web UI and API Authentication

The embedded web UI and `/api/v1/*` endpoints require a session login. On first
start, pg_sage creates `admin@pg-sage.local` and prints a one-time initial
password to stderr. Use `/api/v1/auth/login` for API scripts and keep the
returned `sage_session` cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
```

### Retention

| Parameter | Default | Description |
|---|---|---|
| `retention.snapshots_days` | `90` | Days to retain snapshot data |
| `retention.findings_days` | `180` | Days to retain resolved findings |
| `retention.actions_days` | `365` | Days to retain action log entries |
| `retention.explains_days` | `90` | Days to retain EXPLAIN plan captures |

> **Complete reference:** See `sidecar/config.example.yaml` for all
> configuration fields including `safety`, `alerting`, `auto_explain`,
> `forecaster`, `tuner`, `advisor`, and `api` sections.
