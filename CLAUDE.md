# CLAUDE.md — pg_sage Project

> PostgreSQL extension + Go sidecar for autonomous database health monitoring.

---

## Identity & Environment

- **Project**: pg_sage — PostgreSQL DBA agent extension
- **Languages**: C (extension), Go (sidecar), SQL (migrations/functions)
- **Database**: PostgreSQL 17 (Docker: `pg_sage-pg_sage-1`)
- **Container Runtime**: Docker Compose
- **Test Runner**: `pg_regress` (extension), `go test` (sidecar)

### Key Commands
```bash
# Start everything (from pg_sage/)
docker compose up -d

# Rebuild after C changes
docker compose up -d --build pg_sage

# Rebuild sidecar
docker compose up -d --build sidecar

# Connect to psql
docker exec -it pg_sage-pg_sage-1 psql -U postgres

# Check sidecar logs
docker logs pg_sage-sidecar-1

# Test metrics endpoint
curl -s http://localhost:9187/metrics

# MCP endpoint
curl -s http://localhost:5433/sse

# Build sidecar locally (requires Go)
cd sidecar && go build -o sage-sidecar.exe .
```

---

## Architecture

```
pg_sage/                          # Root project directory
├── src/                          # C extension source
│   ├── pg_sage.c                 # Entry point, GUC, shared_preload_libraries
│   ├── collector.c               # Stats collection (pg_stat_statements, etc.)
│   ├── analyzer.c                # Finding generation
│   ├── explain_capture.c         # EXPLAIN plan capture + narration
│   ├── action_executor.c         # Autonomous action execution
│   ├── context.c                 # LLM context assembly
│   └── mcp_helpers.c             # JSON helpers for MCP
├── sql/                          # Extension SQL (CREATE FUNCTION, etc.)
├── sidecar/                      # Go MCP server + Prometheus exporter
├── grafana/                      # Dashboard JSON
├── docs/                         # Spec, walkthrough, references
├── demo/                         # Demo scripts
├── docker-compose.yml            # Docker Compose config
├── Dockerfile                    # Extension build
└── FIXLIST.md                    # Known bugs to fix
```

---

## Docker Setup

- **Compose file**: `pg_sage/docker-compose.yml` (run from project root)
- **postgres user password**: `postgres` (set in compose env, matches SAGE_DATABASE_URL)
- **Sidecar connects via**: `postgres://postgres:postgres@pg_sage:5432/postgres`
- **Host port 5432**: conflicts with local PostgreSQL install — sidecar must use Docker networking
- **Ports exposed**: 5432 (postgres), 5433 (MCP), 9187 (Prometheus)
- **Volume**: `pgdata` persists DB data — `POSTGRES_PASSWORD` env var only applies on first init

---

## Extension Details

- Shared preload: `pg_stat_statements,pg_sage`
- Schema: `sage.*` (functions, tables, views)
- Key functions: `sage.health()`, `sage.explain(bigint)`, `sage.findings_json()`, `sage.status()`
- GUCs: `sage.database`, `sage.collector_interval`, `sage.analyzer_interval`, `sage.trust_level`

---

## Known Issues (see FIXLIST.md)

1. `sage.explain()` fails on parameterized queries ($N placeholders from pg_stat_statements)
2. Walkthrough references `curl localhost:9187/metrics` without mentioning sidecar prerequisite

---

## Project-Specific Rules

- All C code follows PostgreSQL extension conventions (PG_MODULE_MAGIC, SPI, palloc/pfree)
- SQL functions live in `sql/pg_sage--X.Y.Z.sql`
- Sidecar Go code uses `pgx/v5` for database access
- MCP transport: HTTP + SSE (not stdio)
- Never modify `pg_hba.conf` in Docker — set passwords via `ALTER USER` instead
- Test extension changes by rebuilding the container, not reloading
