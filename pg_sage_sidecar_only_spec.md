# pg_sage — Sidecar-Only Architecture Spec

## Decision

pg_sage is a Go sidecar. The C extension is frozen at v0.6.0-rc3 — no new features, no new development. All future work (optimizer v2, HypoPG validation, dual-model, non-B-tree indexes, materialized view detection, parameter tuning) is sidecar-only.

The extension code stays in the repo as `extension/` for self-managed power users who want in-process monitoring with auto-explain hooks. It receives security fixes only. The sidecar is the product.

---

## What Changes

### Repository Structure

```
pg_sage/
├── sidecar/                    ← THE PRODUCT (all new development here)
│   ├── cmd/pg_sage_sidecar/
│   ├── internal/
│   │   ├── analyzer/
│   │   ├── briefing/
│   │   ├── collector/
│   │   ├── config/
│   │   ├── executor/
│   │   ├── ha/
│   │   ├── llm/
│   │   ├── optimizer/          ← NEW: Index Optimizer v2
│   │   ├── retention/
│   │   ├── schema/
│   │   └── startup/
│   ├── resources.go
│   ├── tools.go
│   ├── sidecar_test.go
│   └── go.mod
├── extension/                   ← FROZEN (security fixes only)
│   ├── src/
│   ├── include/
│   ├── sql/
│   ├── Makefile
│   └── Dockerfile
├── docs/
│   ├── pg_sage_spec_v2.2.md
│   └── ARCHITECTURE.md          ← NEW: explains sidecar-only decision
└── README.md                    ← Updated: sidecar is primary install path
```

### What the Extension Keeps (Frozen)

- Background workers (collector, analyzer, briefing, DDL worker)
- Auto-explain hook + sage.explain_cache
- SQL functions: sage.explain(), sage.briefing(), sage.diagnose(), sage.health_json()
- 49 GUCs
- Tier 1 rules engine
- Tier 3 executor (DDL worker with libpq)
- ReAct function blocklist
- All rc3 bug fixes

### What the Extension Does NOT Get

- Index Optimizer v2 (LLM-powered)
- HypoPG validation
- Dual-model architecture
- Non-B-tree index recommendations (GIN, BRIN, GiST)
- Partial index detection
- Expression index detection
- Materialized view detection
- Parameter tuning recommendations
- Plan-aware optimization (sidecar uses GENERIC_PLAN or reads extension's explain_cache)
- Cross-table join optimization
- Cost estimation
- Confidence scoring
- MCP server (already sidecar-only)
- Prometheus metrics (already sidecar-only)

### What the Sidecar Gains

**Auto-explain data access (when extension is co-deployed):**

The sidecar can read from the extension's `sage.explain_cache` table when the extension is installed alongside it. This gives the optimizer actual EXPLAIN ANALYZE plans with real row counts, real buffer hits, and real heap fetches — richer than GENERIC_PLAN.

```go
func (o *Optimizer) getPlans(ctx context.Context, pool *pgxpool.Pool, queryID int64) (*PlanData, error) {
    // Try 1: Read from sage.explain_cache (extension co-deployed)
    var planJSON []byte
    err := pool.QueryRow(ctx,
        "SELECT plan_json FROM sage.explain_cache WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1",
        queryID).Scan(&planJSON)
    if err == nil {
        return parsePlanJSON(planJSON), nil
    }

    // Try 2: GENERIC_PLAN (PG16+, standalone sidecar)
    // ... EXPLAIN (GENERIC_PLAN, FORMAT JSON) ...

    // Try 3: No plan available (PG14-15 standalone)
    return nil, nil  // optimizer proceeds with query-text-only
}
```

**Standalone auto_explain integration (no extension needed):**

Users on managed services can enable the standard `auto_explain` extension (available on Cloud SQL, AlloyDB, Aurora, RDS) and the sidecar reads from the PG log:

```yaml
# Recommend users add to their managed PG config:
# shared_preload_libraries = 'pg_stat_statements,auto_explain'
# auto_explain.log_min_duration = '500ms'
# auto_explain.log_format = 'json'
# auto_explain.log_analyze = false  # don't execute, just plan

# Sidecar config:
plan_capture:
  source: "auto_explain_log"    # or "explain_cache" or "generic_plan"
  log_path: "/var/log/postgresql/"  # for auto_explain log parsing
```

This is a future enhancement — GENERIC_PLAN is the default for v2.

---

## Deployment Models (Updated)

### Model 1: Sidecar Standalone (Managed Services) — PRIMARY

```
┌─────────────────────────────────────────┐
│  Cloud SQL / AlloyDB / Aurora / RDS      │
│  ┌─────────────────────────────────┐    │
│  │  PostgreSQL                     │    │
│  │  + pg_stat_statements           │    │
│  │  + HypoPG (optional, for v2)    │    │
│  │  + sage schema (created by      │    │
│  │    sidecar at startup)          │    │
│  └────────────────┬────────────────┘    │
│                   │ pgx (TCP/SSL)        │
│                   │                      │
└───────────────────┼──────────────────────┘
                    │
    ┌───────────────┴───────────────┐
    │  pg_sage sidecar              │
    │  ┌──────────┐ ┌───────────┐  │
    │  │Collector │ │ Analyzer  │  │
    │  └──────────┘ └───────────┘  │
    │  ┌──────────┐ ┌───────────┐  │
    │  │Optimizer │ │ Executor  │  │
    │  │(LLM+     │ │(CONCURRENT│  │
    │  │ HypoPG)  │ │ LY DDL)  │  │
    │  └──────────┘ └───────────┘  │
    │  ┌──────────┐ ┌───────────┐  │
    │  │MCP Server│ │Prometheus │  │
    │  └──────────┘ └───────────┘  │
    │  ┌──────────────────────────┐ │
    │  │ Gemini Flash (general)   │ │
    │  │ Opus/Pro (optimizer)     │ │
    │  └──────────────────────────┘ │
    └───────────────────────────────┘
```

### Model 2: Sidecar + Extension (Self-Managed Power Users)

```
    ┌─────────────────────────────────────┐
    │  Self-Managed PostgreSQL             │
    │  ┌─────────────────────────────┐    │
    │  │  pg_sage extension (frozen) │    │
    │  │  - Collector (SPI)          │    │
    │  │  - Analyzer (Tier 1 rules)  │    │
    │  │  - Auto-explain hook        │    │
    │  │  - sage.explain_cache ←─────┼──── sidecar reads this
    │  │  - DDL worker (libpq)       │    │
    │  │  - SQL functions            │    │
    │  └─────────────────────────────┘    │
    └────────────────┬────────────────────┘
                     │ pgx (Unix socket)
    ┌────────────────┴────────────────────┐
    │  pg_sage sidecar                     │
    │  (same as Model 1, but reads         │
    │   explain_cache for richer plans)    │
    └──────────────────────────────────────┘
```

### Model 3: Extension Only (Legacy, Frozen)

```
    ┌─────────────────────────────────────┐
    │  Self-Managed PostgreSQL             │
    │  ┌─────────────────────────────┐    │
    │  │  pg_sage extension v0.6.0   │    │
    │  │  - All rc3 features         │    │
    │  │  - NO optimizer v2          │    │
    │  │  - NO MCP server            │    │
    │  │  - NO Prometheus            │    │
    │  │  - NO dual-model LLM        │    │
    │  │  - Tier 1 rules + Gemini    │    │
    │  │    (via libcurl, single     │    │
    │  │     model only)             │    │
    │  └─────────────────────────────┘    │
    └─────────────────────────────────────┘
```

---

## Packaging

### Primary: Single Static Binary

```bash
# Linux amd64
curl -fsSL https://github.com/pg-sage/pg-sage/releases/latest/download/pg_sage_linux_amd64 -o pg_sage
chmod +x pg_sage
./pg_sage --database-url "postgres://user:pass@host:5432/db" --gemini-api-key "AIza..."
```

No config file needed for basic usage. CLI flags for everything. Config file for advanced (dual-model, maintenance windows, retention, etc.).

### Docker

```dockerfile
FROM gcr.io/distroless/static-debian12
COPY pg_sage /usr/local/bin/pg_sage
ENTRYPOINT ["pg_sage"]
```

```bash
docker run -e SAGE_DATABASE_URL="postgres://..." -e SAGE_GEMINI_API_KEY="..." \
    -p 8080:8080 -p 9187:9187 ghcr.io/pg-sage/pg-sage:latest
```

### Kubernetes Sidecar

```yaml
containers:
  - name: app
    image: myapp:latest
  - name: pg-sage
    image: ghcr.io/pg-sage/pg-sage:latest
    env:
      - name: SAGE_DATABASE_URL
        valueFrom:
          secretKeyRef:
            name: pg-credentials
            key: url
      - name: SAGE_GEMINI_API_KEY
        valueFrom:
          secretKeyRef:
            name: llm-keys
            key: gemini
    ports:
      - containerPort: 8080  # MCP
      - containerPort: 9187  # Prometheus
```

---

## README Update

```markdown
# pg_sage — Autonomous PostgreSQL DBA Agent

pg_sage monitors your PostgreSQL database, detects performance problems,
and fixes them automatically. It's a Go binary that connects to any
PostgreSQL 14+ instance — managed (Cloud SQL, AlloyDB, Aurora, RDS,
Azure) or self-managed.

## Quick Start

curl -fsSL https://get.pg-sage.dev | sh
pg_sage --database-url "postgres://user:pass@host:5432/db"

## What It Does

- **Collects** snapshots from pg_stat_statements, pg_stat_user_tables,
  pg_stat_user_indexes, and 10+ catalog views
- **Analyzes** using 18+ rules to detect slow queries, missing indexes,
  duplicate indexes, table bloat, sequence exhaustion, and more
- **Optimizes** using LLM (Gemini, Claude, GPT) to consolidate index
  recommendations, detect partial index opportunities, and validate
  with HypoPG before acting
- **Executes** autonomously with trust-ramped safety: observation →
  advisory → autonomous. All DDL uses CONCURRENTLY. Rollback on
  regression.
- **Reports** via MCP (Claude Desktop), Prometheus, and structured
  briefings

## Verified Platforms

| Platform | Status | Report |
|----------|--------|--------|
| Cloud SQL PG17 | ✅ 129 tests + full pipeline | R10, R11 |
| AlloyDB PG17 | ✅ Full parity, 0 code changes | R13 |
| Self-Managed PG14-17 | ✅ 32 findings, 0 bugs | R8 |
| Aurora PostgreSQL | Testing | — |
| RDS PostgreSQL | Testing | — |
| Azure Flexible Server | Planned | — |
```

---

## Migration Path for Extension Users

Extension users who want optimizer v2 features deploy the sidecar alongside the extension:

```bash
# Keep the extension running (it still collects explain_cache)
# Add the sidecar:
./pg_sage --mode=standalone \
    --database-url "postgres://sage_agent:pw@localhost:5432/mydb" \
    --gemini-api-key "AIza..." \
    --plan-source explain_cache  # reads from extension's sage.explain_cache
```

The sidecar detects the extension's advisory lock and operates in complementary mode:
- Extension holds the advisory lock → sidecar skips schema bootstrap (already done)
- Sidecar reads from sage.explain_cache for richer plans
- Sidecar writes optimizer findings to sage.findings (shared table)
- Extension's DDL worker executes pending actions (or sidecar does, based on who holds the lock)

If the user wants to remove the extension later:
```sql
ALTER SYSTEM RESET shared_preload_libraries;
-- restart PG
-- Sidecar takes over fully (acquires advisory lock, bootstraps schema if needed)
```

---

## What Gets Cut from the Extension CLAUDE.md

The extension build spec (`pg_sage_ext_CLAUDE.md`) is archived. No new CLAUDE.md iterations for the extension. The last valid version is rc3.

Future CLAUDE.md files are sidecar-only:
- `CLAUDE.md` — sidecar build spec (existing, updated)
- `pg_sage_index_optimizer_v2_spec.md` — optimizer feature spec (sidecar-only)
- `pg_sage_index_optimizer_v2_test_plan.md` — test plan (Go tests only)
- `pg_sage_cloudsql_test_CLAUDE.md` — Cloud SQL integration (sidecar)
- `pg_sage_alloydb_test_CLAUDE.md` — AlloyDB integration (sidecar)
- `pg_sage_aurora_test_CLAUDE.md` — Aurora integration (sidecar)
- `pg_sage_rds_test_CLAUDE.md` — RDS integration (sidecar)
