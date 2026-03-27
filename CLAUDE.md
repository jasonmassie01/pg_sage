# pg_sage — Autonomous PostgreSQL DBA Agent

## What This Is

pg_sage is an autonomous PostgreSQL DBA agent that continuously monitors, analyzes, and maintains PostgreSQL databases. It runs as an **external Go sidecar process** that connects to Postgres over the standard wire protocol — no C extension, no shared_preload_libraries. It combines a deterministic rules engine (Tier 1), optional LLM-enhanced analysis (Tier 2), and a trust-ramped action executor (Tier 3).

**License:** AGPL-3.0
**Module:** `github.com/pg-sage/sidecar`
**Go:** 1.24
**Target:** PostgreSQL 14+
**Tests:** 584+ (571 unit + 13 integration), 0 failures
**Modes:** Standalone (single DB) and Fleet (multi-DB from one sidecar)

## Architecture

### Core Goroutines (per database instance)

1. **Collector** — snapshots `pg_stat_statements`, `pg_stat_user_tables`, `pg_stat_user_indexes`, sequences, replication state
2. **Analyzer** — Tier 1 rules engine, generates findings, invokes Tier 3 action executor at sufficient trust
3. **Briefing** — periodic Tier 2 health briefings when LLM is enabled
4. **Optimizer** — LLM-powered index recommendations (optional)
5. **Advisor** — LLM-powered config tuning: vacuum, WAL, connections, memory (optional)

### Tier 1 — Rules Engine (deterministic)
Index health (duplicates, unused, missing, bloat), query performance (slow queries, regressions, seq scans on large tables), sequence exhaustion, vacuum/bloat/dead tuples, config audit, security, replication monitoring, self-monitoring.

### Tier 2 — LLM-Enhanced (optional)
Daily briefings, interactive diagnose (ReAct loop), explain narrative, index optimization, config advising. Pluggable LLM provider via OpenAI-compatible endpoint (covers Gemini, OpenAI, Groq, Ollama, etc).

### Tier 3 — Action Executor
Trust-ramped: OBSERVATION (day 0-7, findings only) → ADVISORY (day 8-30, SAFE actions) → AUTONOMOUS (day 31+, MODERATE actions). HIGH-risk always requires manual confirmation. All actions logged with rollback metadata.

### Fleet Mode (v0.8.0)
One sidecar binary monitors N databases. `fleet.DatabaseManager` wraps instances. Standalone mode auto-normalizes into `Databases[0]`. REST API + React dashboard on `:8080`.

### Safety
Circuit breakers for DB and LLM. Emergency stop halts all autonomous activity. Per-database token budgets in fleet mode.

## Tech Stack

- **Postgres driver:** `github.com/jackc/pgx/v5` + `pgxpool`
- **Testing:** `go test` (no testify — uses stdlib `testing` package)
- **Config:** `gopkg.in/yaml.v3` + env vars + runtime overrides via `sage.config` table
- **Logging:** `log/slog` (structured, stdlib)
- **Frontend:** React 19 + Vite + Tailwind CSS v4 + Recharts + lucide-react
- **Embedding:** `go:embed` for React build assets in Go binary
- **Release:** goreleaser (builds linux/darwin amd64/arm64)

## Project Structure

```
pg_sage/
├── sidecar/                          # Go sidecar (main codebase)
│   ├── cmd/pg_sage_sidecar/
│   │   └── main.go                   # Entry point, orchestration
│   ├── internal/
│   │   ├── advisor/                  # LLM config tuning (vacuum, WAL, memory)
│   │   ├── analyzer/                 # Tier 1 rules engine + findings
│   │   ├── api/                      # REST API (14 endpoints) + embedded dashboard
│   │   │   ├── dist/                 # React build output (go:embed)
│   │   │   └── *.go                  # Router, handlers, middleware, helpers
│   │   ├── briefing/                 # Tier 2 health briefings
│   │   ├── collector/                # Stats snapshot collection
│   │   ├── config/                   # Config structs, loading, fleet normalization
│   │   ├── executor/                 # Tier 3 trust-gated action execution
│   │   ├── fleet/                    # Multi-DB manager, health scores, budgets
│   │   ├── ha/                       # High availability monitoring
│   │   ├── llm/                      # LLM provider (OpenAI-compatible)
│   │   ├── optimizer/                # LLM index recommendations
│   │   ├── retention/                # Data retention + cleanup
│   │   ├── schema/                   # DB schema bootstrap (sage.*)
│   │   └── startup/                  # Startup validation checks
│   ├── web/                          # React dashboard
│   │   ├── src/
│   │   │   ├── components/           # 11 reusable components
│   │   │   ├── pages/                # 5 pages (Dashboard, Findings, Actions, DB, Settings)
│   │   │   └── hooks/useAPI.js       # Polling fetch hook
│   │   ├── vite.config.js            # Output → ../internal/api/dist/
│   │   └── package.json
│   ├── main.go                       # MCP server, Prometheus, auth, tools, resources
│   ├── go.mod
│   ├── Dockerfile
│   └── .golangci.yml
├── sql/                              # SQL schema files
├── src/                              # C extension (legacy reference on master)
├── docs/                             # Specifications
├── .claude/                          # Claude Code config
│   ├── settings.json                 # Hooks + permissions
│   └── skills/                       # TDD, pgx patterns, LLM provider
├── .goreleaser.yml                   # Release config
├── docker-compose.yml
├── config.example.yaml
├── roadmap.md
└── LICENSE
```

## Build & Run

```bash
# From sidecar/ directory
cd sidecar

# Build (includes embedded React dashboard)
cd web && npm ci && npm run build && cd ..
go build ./cmd/pg_sage_sidecar/

# Run tests (584+ tests)
go test ./... -count=1 -timeout 300s

# Run with race detector
go test ./... -race -count=1

# Integration tests only
go test ./... -tags=integration -count=1

# Lint
go vet ./...
golangci-lint run ./...

# Release build
cd .. && goreleaser build --snapshot --clean
```

## Servers & Ports

| Server | Port | Purpose |
|--------|------|---------|
| MCP (SSE) | `:5433` | Model Context Protocol for Claude Desktop / AI agents |
| Prometheus | `:9187` | Metrics endpoint (text format) |
| API + Dashboard | `:8080` | REST API (`/api/v1/*`) + React SPA |

## REST API Endpoints (14)

All under `/api/v1/`, all accept `?database=` filter in fleet mode:

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/databases` | Fleet overview + health scores |
| GET | `/findings` | List findings (filter: severity, status, database) |
| GET | `/findings/:id` | Finding detail |
| POST | `/findings/:id/suppress` | Suppress finding |
| POST | `/findings/:id/unsuppress` | Unsuppress finding |
| GET | `/actions` | List executed actions |
| GET | `/actions/:id` | Action detail |
| GET | `/snapshots/latest` | Latest collector snapshot |
| GET | `/snapshots/history` | Snapshot time series |
| GET | `/config` | Current config |
| PUT | `/config` | Update config (hot reload) |
| GET | `/metrics` | JSON metrics |
| POST | `/emergency-stop` | Halt all autonomous actions |
| POST | `/resume` | Resume after emergency stop |

## Testing Strategy

584+ tests using stdlib `testing` package. Three levels:

1. **Unit tests** (`*_test.go`) — rules, config parsing, fleet manager, API handlers, optimizer, advisor
2. **Integration tests** (`//go:build integration`) — real HTTP via httptest, fleet scenarios
3. **Schema tests** — require local PostgreSQL on port 5432 (bootstrap, idempotency)

## Code Style & Conventions

- **Go version:** 1.24 (use range-over-func, slog, etc.)
- **Naming:** PascalCase exported, camelCase internal
- **Packages:** `internal/` for everything. No public library API.
- **Errors:** Wrap with `fmt.Errorf("context: %w", err)`. Sentinel errors for known conditions.
- **Logging:** `log/slog` with structured fields
- **Context:** First argument on all I/O functions. Respect cancellation.
- **Database:** `pgxpool` only. Parameterized queries (`$1`). Never string-concat SQL.
- **Config:** YAML + env vars. Validate at startup, fail fast.
- **Interfaces:** Define at consumer. Keep small (1-3 methods).

## Dependencies (keep minimal)

- `github.com/jackc/pgx/v5` — Postgres driver + pool
- `gopkg.in/yaml.v3` — config parsing
- `github.com/fsnotify/fsnotify` — config hot reload
- `github.com/google/uuid` — finding/action IDs
- No ORM. No web framework. No DI container. No testify.

## What NOT To Do

- Do not use `database/sql` — use `pgx` native interface
- Do not use global mutable state — pass dependencies explicitly
- Do not ignore `context.Context` cancellation
- Do not hardcode connection strings — always config/env
- Do not store secrets in source — env vars or secret manager
- Do not skip error wrapping — bare `return err` loses context
- Do not use `panic` for recoverable errors
- Do not add dependencies without justification — stdlib first
- Do not modify existing migration files — append new numbered ones
- Do not edit `internal/api/dist/` manually — it's generated by `npm run build`
