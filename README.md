# pg_sage

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8.svg)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14%2B-336791.svg)](https://www.postgresql.org)

**Agentic Postgres DBA.** No extension required.

## What It Does

pg_sage runs as a single Go binary alongside your PostgreSQL instance. It connects
over the standard wire protocol, collects performance data from catalog views and
`pg_stat_statements`, projects issues into DBA **Cases**, and optionally uses an
LLM for deeper analysis. A trust-ramped executor proposes or applies typed
actions with guardrails, approval gates, rollback metadata, and a shadow-mode
report showing what autonomous policy would have handled. Works on Cloud SQL,
AlloyDB, Aurora, RDS, and self-managed Postgres.

## Quick Start

```bash
# Binary (Linux amd64)
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz | tar xz
./pg_sage --pg-url "postgres://sage_agent:pw@localhost:5432/mydb"

# Docker
docker run --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:pw@host:5432/mydb" \
  -p 8080:8080 -p 9187:9187 ghcr.io/jasonmassie01/pg_sage:latest
```

Dashboard at `http://localhost:8080` -- API and Prometheus metrics at `:8080/api/v1/` and `:9187/metrics`.

On first start, pg_sage creates `admin@pg-sage.local` and prints a one-time
initial admin password to stderr. The dashboard and JSON API use the
`sage_session` login cookie; unauthenticated API calls return `401`.

For Docker, retrieve the password with:

```bash
docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'
```

## Features

| Area | What You Get |
|------|-------------|
| **Cases Work Queue** | Findings, incidents, migration risks, and action history are projected into ranked DBA cases with why-now context and next actions |
| **Rules Engine** | 20+ deterministic checks: duplicate/unused/missing indexes, slow queries, regressions, seq scans, vacuum & bloat, dead tuples, sequence exhaustion, replication lag, security audit, config drift |
| **Index Optimizer** | LLM-powered recommendations validated through 8 checks + HypoPG cost estimation, confidence scored 0.0--1.0 |
| **Config Advisors** | 6 LLM advisors: vacuum tuning, WAL/checkpoint, connections, memory, query rewrite, bloat remediation |
| **Health Briefings** | Periodic LLM-generated summaries of database state; interactive diagnose via ReAct loop |
| **Trust-Ramped Executor** | Observation -> Advisory -> Autonomous. Typed actions carry risk tier, guardrails, expiration, rollback/mitigation, and verification state. HIGH-risk actions always require approval. |
| **Shadow Mode** | Shows avoided toil and proof rows for actions pg_sage would have handled under auto-safe policy before teams turn on autonomous execution |
| **Fleet Mode** | Monitor N databases from one binary with per-database trust levels, token budgets, and health scores |
| **Per-Query Tuner** | EXPLAIN plan analysis with `pg_hint_plan` directives for disk sorts, hash spills, bad joins, missed index scans |
| **Workload Forecaster** | Predicts disk growth, connection saturation, cache pressure, sequence exhaustion, query volume spikes, checkpoint pressure |
| **Alerting** | Slack, PagerDuty, and webhook channels with per-severity routing, cooldown, and quiet hours |
| **Dashboard & API** | React SPA + REST API embedded in the binary -- Overview, Cases, Actions, Fleet, Settings, authenticated by default |
| **Prometheus** | Standard `/metrics` endpoint with findings, collector, LLM, executor, and database size gauges |

## Documentation

See the [docs/](docs/) directory for guides and reference:

- [Installation](docs/installation.md) -- database user setup, binary and Docker deployment
- [Configuration](docs/configuration.md) -- YAML, environment variables, hot reload
- [Architecture](docs/architecture.md) -- component design, goroutine model, data flow
- [Deployment](docs/deployment.md) -- production hardening, resource sizing
- [Security](docs/security.md) -- permissions model, network, secrets management
- [SQL Reference](docs/sql-reference.md) -- schema tables and diagnostic queries
- [Findings Reference](docs/findings.md) -- every rule, severity, and remediation
- [Try It Out](docs/try-it-out.md) -- local v0.9 smoke path and UI checklist
- **Walkthroughs:** [Linux](docs/walkthrough-linux.md) | [Windows](docs/walkthrough-windows.md) | [Fleet](docs/walkthrough-fleet.md)

## Building from Source

Requires Go 1.24+ and Node.js 20+. See [docs/installation.md](docs/installation.md) for details.

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar/
```

## License

[AGPL-3.0](LICENSE)
