# pg_sage

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![PostgreSQL 14+](https://img.shields.io/badge/PostgreSQL-14%2B-336791.svg)](https://www.postgresql.org/)
[![Works without LLM](https://img.shields.io/badge/LLM-optional-green.svg)](#tier-2----llm-enhanced-analysis)
[![CI: PG 14-17](https://img.shields.io/badge/CI-PG%2014--17-336791.svg)](#testing)

**Autonomous PostgreSQL DBA Agent** -- a native C extension and standalone Go sidecar that continuously monitor, analyze, and maintain your PostgreSQL database.

pg_sage ships in two complementary forms:

1. **C extension** (v0.5.0) -- runs inside PostgreSQL as background workers, requires `shared_preload_libraries` access.
2. **Standalone sidecar** (v0.7.0-rc1) -- a Go binary that connects to any PostgreSQL instance over a standard connection. Works with **Cloud SQL, AlloyDB, RDS, and Aurora** without any extension installation.

All Tier 1 analysis runs without any external dependencies. LLM integration is optional and only enhances Tier 2 features (briefings, diagnose, explain narrative).

---

## Quick Start

### With Docker (extension + sidecar)

```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage
docker compose up
```

Once the container is running:

```bash
docker exec -it pg_sage-pg_sage-1 psql -U postgres
```

```sql
-- Extension is auto-loaded via shared_preload_libraries
SELECT * FROM sage.status();
SELECT category, severity, title FROM sage.findings WHERE status = 'open' ORDER BY severity;
SELECT sage.briefing();
```

### Standalone sidecar (no extension required)

```bash
cp sidecar/config.example.yaml config.yaml
# Edit config.yaml with your connection details
cd sidecar && go build -o sage-sidecar ./cmd/pg_sage_sidecar
./sage-sidecar --config ../config.yaml
```

The standalone sidecar runs the full collector, analyzer, and executor pipeline against any PostgreSQL database -- managed or self-hosted.

---

## Architecture

pg_sage implements a three-tier architecture. Both the C extension and the standalone sidecar implement all three tiers.

### Tier 1 -- Rules Engine

Deterministic checks that run every analyzer interval, no LLM required:

| Category | What it detects |
|---|---|
| **Index health** | Duplicate indexes, unused indexes, missing indexes, index bloat |
| **Query performance** | Slow queries, query regressions, sequential scans on large tables |
| **Sequences** | Approaching exhaustion (bigint/int overflow) |
| **Maintenance** | Vacuum needs, table bloat, dead tuple accumulation, XID wraparound |
| **Configuration** | Audit of `postgresql.conf` against best practices |
| **Security** | Overprivileged roles, missing RLS on sensitive tables |
| **Replication** | Lag monitoring, inactive slots, WAL archiving staleness |
| **Self-monitoring** | Extension health, circuit breaker status, schema footprint |

### Tier 2 -- LLM-Enhanced Analysis

Optional features that use an external LLM for natural-language intelligence:

- **Daily briefings** -- summarized health reports delivered on schedule
- **Interactive diagnose** -- ReAct loop that reasons through problems step by step
- **Explain narrative** -- human-readable query plan analysis via `sage.explain(queryid)`
- **Cost attribution** -- map storage and IOPS costs to unused indexes and missing indexes
- **Migration review** -- detect long-running DDL blocking production
- **Schema design review** -- timezone-naive timestamps, missing PKs, naming issues

### Tier 3 -- Action Executor

Automated remediation with a graduated trust model:

| Trust Level | Timeline | Allowed Actions |
|---|---|---|
| **OBSERVATION** | Day 0--7 | No actions; findings only |
| **ADVISORY** | Day 8--30 | SAFE actions (drop unused/duplicate indexes, vacuum tuning) |
| **AUTONOMOUS** | Day 31+ | MODERATE actions (create indexes, reindex, configuration changes) |

HIGH-risk actions always require manual confirmation regardless of trust level.

Every autonomous action is logged to `sage.action_log` with before/after state and rollback SQL. The rollback checker monitors for p95 latency regressions and automatically reverts actions that degrade performance.

**DDL Worker** (extension only): The C extension includes a dedicated background worker (`ddl_worker.c`) for executing DDL statements (CREATE INDEX, REINDEX, etc.) outside the main analyzer loop. This prevents long-running DDL from blocking analysis cycles.

---

## Cloud Database Support

The v0.7.0 standalone sidecar removes the requirement for `shared_preload_libraries` access, enabling pg_sage on managed PostgreSQL services:

| Provider | Service | Status |
|---|---|---|
| **Google Cloud** | Cloud SQL for PostgreSQL | Validated |
| **Google Cloud** | AlloyDB | Validated |
| **AWS** | RDS for PostgreSQL | Supported |
| **AWS** | Aurora PostgreSQL | Supported |
| **Any** | Self-hosted PostgreSQL 14+ | Supported |

The sidecar connects as a regular database user and queries `pg_stat_statements`, `pg_stat_user_tables`, and other catalog views directly. It manages its own `sage` schema for findings, snapshots, and action logs.

**Requirements for managed databases:**
- `pg_stat_statements` extension enabled
- A database user with read access to `pg_catalog` and the ability to create objects in a `sage` schema
- Network connectivity from the sidecar host to the database

---

## SQL Functions

```sql
-- System status as JSONB
SELECT * FROM sage.status();

-- Daily health briefing (works with or without LLM)
SELECT * FROM sage.briefing();

-- Interactive diagnostic with ReAct reasoning (Tier 2)
SELECT * FROM sage.diagnose('Why are my queries slow today?');

-- Human-readable query plan narrative (Tier 2)
SELECT * FROM sage.explain(query_id);

-- Suppress a specific finding
SELECT sage.suppress(finding_id, 'Known issue, vendor fix pending', 30);

-- Emergency controls
SELECT sage.emergency_stop();   -- halt all autonomous activity immediately
SELECT sage.resume();           -- resume normal operation
```

---

## MCP Sidecar (v0.7.0-rc1)

The Go sidecar exposes pg_sage capabilities via the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) over HTTP+SSE. This lets AI coding assistants (Claude, Cursor, Copilot) interact with your database through pg_sage.

**In standalone mode**, the sidecar runs the full analysis pipeline itself -- no C extension needed. In extension mode, it delegates to the extension's SQL functions.

### Architecture

```
┌──────────────────────┐     MCP (JSON-RPC over SSE)     ┌─────────────────┐
│  AI Assistant / IDE  │ <-------------------------------> │  sage-sidecar   │
└──────────────────────┘          port 5433               │  (Go binary)    │
                                                          └────────┬────────┘
                                                                   │ SQL
                                                          ┌────────v────────┐
                                                          │   PostgreSQL    │
                                                          │ (any provider)  │
                                                          └─────────────────┘
```

### MCP Resources

| URI | Description |
|---|---|
| `sage://health` | System health overview (connections, cache hit ratio, disk, workers) |
| `sage://findings` | Open findings with severity, recommendations, and remediation SQL |
| `sage://schema/{table}` | DDL, indexes, constraints, columns, and foreign keys |
| `sage://stats/{table}` | Table size, row counts, dead tuples, vacuum status |
| `sage://slow-queries` | Top slow queries from pg_stat_statements |
| `sage://explain/{queryid}` | Cached EXPLAIN plan |

### MCP Tools

| Tool | Description |
|---|---|
| `diagnose` | Interactive diagnostic analysis via ReAct reasoning |
| `briefing` | Generate an on-demand health briefing |
| `suggest_index` | Get index recommendations for a table |
| `review_migration` | Review DDL for production safety |

### Prometheus Metrics

The sidecar exposes Prometheus metrics at `:9187/metrics`:

- `pg_sage_info{version}` -- Extension version
- `pg_sage_findings_total{severity}` -- Open findings by severity
- `pg_sage_circuit_breaker_state{breaker}` -- Circuit breaker status

### MCP SQL Functions

These SQL functions return JSONB and are used by the sidecar, but can also be called directly (extension mode only):

```sql
SELECT sage.health_json();                     -- system health overview
SELECT sage.findings_json();                   -- open findings
SELECT sage.findings_json('resolved');         -- resolved findings
SELECT sage.schema_json('public.orders');      -- table schema
SELECT sage.stats_json('public.orders');       -- table statistics
SELECT sage.slow_queries_json();               -- slow queries
SELECT sage.explain_json(queryid);             -- cached explain plan
```

---

## Configuration

### Extension GUCs

Set these in `postgresql.conf` or via `ALTER SYSTEM`:

| Parameter | Default | Description |
|---|---|---|
| `sage.enabled` | `on` | Master enable/disable switch |
| `sage.collector_interval` | `30s` | Interval between snapshot collections |
| `sage.analyzer_interval` | `60s` | Interval between analysis runs |
| `sage.trust_level` | `observation` | Current trust tier (`observation`, `advisory`, `autonomous`) |
| `sage.slow_query_threshold` | `1s` | Slow query threshold |
| `sage.seq_scan_min_rows` | `100000` | Minimum table rows for sequential scan alerts |
| `sage.rollback_threshold` | `10` | p95 latency regression % that triggers automatic rollback |
| `sage.llm_enabled` | `off` | Enable Tier 2 LLM features |
| `sage.llm_api_key_file` | `''` | Path to file containing the LLM API key (preferred over inline) |

GUC check hooks validate all parameters at `SET` time. Invalid values are rejected immediately.

### Sidecar YAML Configuration

The standalone sidecar uses YAML-based configuration with hot-reload support. Copy `sidecar/config.example.yaml` to get started:

```yaml
mode: standalone     # "extension" or "standalone"

postgres:
  host: localhost
  port: 5432
  user: sage_agent
  password: ${SAGE_PG_PASSWORD}   # Environment variable expansion
  database: postgres

collector:
  interval_seconds: 60
  batch_size: 1000

analyzer:
  interval_seconds: 600
  slow_query_threshold_ms: 1000

trust:
  level: observation
  maintenance_window: "0 2 * * 6 America/Chicago"

llm:
  enabled: false
  endpoint: ""
  api_key: ${SAGE_LLM_API_KEY}
  model: ""
```

**Hot-reloadable fields** (change without restart): all `collector`, `analyzer`, `safety`, `trust`, `llm`, `briefing`, and `retention` settings.

**Non-hot-reloadable** (require restart): `mode`, `postgres.*`, `mcp.listen_addr`, `prometheus.listen_addr`.

Precedence: CLI flags > environment variables > config file > built-in defaults.

See `sidecar/config.example.yaml` for the full reference with all defaults.

---

## Grafana Dashboard

A pre-built Grafana dashboard is included at `grafana/pg_sage_dashboard.json` with 18 panels covering findings by severity, connections, cache hit ratio, TPS, deadlocks, circuit breaker status, and database size. See `grafana/README.md` for import instructions.

---

## Schema

All objects live in the `sage` schema:

| Table | Purpose |
|---|---|
| `sage.snapshots` | Point-in-time system state captures (indexes, tables, sequences, system) |
| `sage.findings` | Detected issues with severity, recommendation, and remediation SQL |
| `sage.action_log` | Audit trail for every autonomous action with rollback metadata |
| `sage.explain_cache` | Cached EXPLAIN plans keyed by queryid |
| `sage.briefings` | Generated briefing reports with delivery status |
| `sage.config` | Extension configuration overrides |
| `sage.mcp_log` | Audit log of MCP sidecar requests |

---

## Circuit Breaker

pg_sage includes a circuit breaker that protects your database from runaway analysis or action loops:

- **Separate breakers** for database operations and LLM calls
- Trips automatically when error thresholds are exceeded
- `sage.emergency_stop()` trips both breakers immediately
- `sage.resume()` resets breakers and resumes normal operation

---

## Installation

### Prerequisites

- PostgreSQL 14, 15, 16, or 17
- `pg_stat_statements` extension
- `libcurl` development headers (extension only, for optional LLM integration)

### Docker (recommended for extension mode)

```bash
docker compose up
```

The included `docker-compose.yml` configures `shared_preload_libraries`, `pg_stat_statements`, and default GUCs automatically.

### Extension from Source

```bash
make
sudo make install
```

Add to `postgresql.conf`:

```
shared_preload_libraries = 'pg_stat_statements,pg_sage'
sage.database = 'postgres'
```

Restart PostgreSQL, then:

```sql
CREATE EXTENSION pg_stat_statements;
CREATE EXTENSION pg_sage;
```

### Standalone Sidecar

No extension installation needed. Build the Go binary and point it at your database:

```bash
cd sidecar
go build -o sage-sidecar ./cmd/pg_sage_sidecar
./sage-sidecar --config config.yaml
```

The sidecar creates the `sage` schema and required tables on first startup.

---

## Testing

### Extension Tests

| Suite | Tests | Purpose |
|---|---|---|
| `test/regression.sql` | 27 | Core functionality and schema validation |
| `test/run_tests.sql` | 14 | Integration tests across tiers |
| `test/test_all_features.sql` | -- | Comprehensive feature coverage (all tiers) |

```bash
docker exec -i pg_sage-pg_sage-1 psql -U postgres < test/test_all_features.sql
docker exec -i pg_sage-pg_sage-1 psql -U postgres < test/regression.sql
```

### Sidecar Tests

```bash
cd sidecar && go test -v ./...
```

### CI Matrix

The CI pipeline tests the extension against PostgreSQL 14, 15, 16, and 17.

---

## File Structure

```
pg_sage/
├── Dockerfile
├── Makefile
├── docker-compose.yml
├── pg_sage.control
├── include/
│   └── pg_sage.h                     # Shared header
├── sql/
│   ├── pg_sage--0.5.0.sql            # Full install SQL for v0.5.0
│   ├── pg_sage--0.1.0.sql            # Legacy install SQL
│   └── pg_sage--0.1.0--0.5.0.sql     # Migration path
├── src/
│   ├── pg_sage.c                     # Entry point, shared memory, worker registration
│   ├── guc.c                         # GUC definitions with check hooks
│   ├── collector.c                   # Snapshot collection background worker
│   ├── analyzer.c                    # Rules engine, analysis loop, adaptive scheduling
│   ├── analyzer_extra.c             # Vacuum/bloat, security, replication analysis
│   ├── action_executor.c            # Tier 3 action execution with trust gating
│   ├── ddl_worker.c                 # Background DDL execution worker
│   ├── briefing.c                   # Briefing generation, diagnose, explain narrative
│   ├── tier2_extra.c               # Cost attribution, migration review, schema design
│   ├── context.c                    # Context assembly for LLM prompts
│   ├── llm.c                       # LLM HTTP integration (libcurl)
│   ├── mcp_helpers.c               # JSONB SQL functions for MCP sidecar
│   ├── circuit_breaker.c           # Circuit breaker implementation
│   ├── explain_capture.c           # EXPLAIN plan capture and caching
│   ├── autoexplain_hook.c          # auto_explain hook integration
│   ├── findings.c                  # Finding creation and management
│   ├── ha.c                        # High availability awareness
│   ├── self_monitor.c              # Self-monitoring checks
│   └── utils.c                     # SPI utilities, JSON helpers
├── sidecar/
│   ├── Dockerfile                   # Multi-stage Go build
│   ├── config.example.yaml          # Full configuration reference
│   ├── go.mod
│   ├── cmd/
│   │   └── pg_sage_sidecar/         # CLI entry point
│   ├── internal/
│   │   ├── analyzer/                # Standalone rules engine
│   │   ├── briefing/                # Briefing generation
│   │   ├── collector/               # Catalog snapshot collector
│   │   ├── config/                  # YAML config + hot-reload
│   │   ├── executor/                # Action executor with trust gating
│   │   ├── ha/                      # HA awareness (standby detection)
│   │   ├── llm/                     # LLM client (OpenAI-compatible)
│   │   ├── retention/               # Data retention policies
│   │   ├── schema/                  # Schema introspection queries
│   │   └── startup/                 # First-run schema bootstrap
│   ├── main.go                      # HTTP server, SSE transport, session mgmt
│   ├── mcp.go                       # MCP protocol types and JSON-RPC dispatcher
│   ├── resources.go                 # MCP resource handlers
│   ├── tools.go                     # MCP tool handlers
│   ├── prompts.go                   # MCP prompt templates
│   ├── prometheus.go                # Prometheus /metrics endpoint
│   ├── auth.go                      # API key authentication
│   ├── ratelimit.go                 # Per-IP rate limiting
│   └── sidecar_test.go              # Sidecar integration tests
├── test/
│   ├── regression.sql
│   ├── run_tests.sql
│   └── test_all_features.sql
├── grafana/
│   └── pg_sage_dashboard.json
├── docs/
│   └── pg_sage_spec_v2.2.md
└── docker-entrypoint-initdb.d/
```

---

## Roadmap

- ~~**v0.5** -- C extension with MCP sidecar, JSONB SQL functions, Grafana dashboard~~ Done
- **v0.7** (current) -- Standalone sidecar with full pipeline, Cloud SQL/AlloyDB/RDS/Aurora support, YAML config with hot-reload, DDL worker, GUC hardening, PG 14-17 CI matrix
- **v1.0** -- Production hardening, pg_upgrade compatibility, PGXN publishing

---

## License

pg_sage is licensed under the [GNU Affero General Public License v3.0](https://www.gnu.org/licenses/agpl-3.0.html).
