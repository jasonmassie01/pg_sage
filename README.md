# pg_sage

[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-14%2B-336791.svg)](https://www.postgresql.org)

**Agentic Postgres DBA.** No extension required.

**Verified and reversible.** Every change pg_sage makes is checked against what it predicted,
and every change that can be undone keeps a rollback. `pg_sage publish outcomes` turns that
record into a signed public bundle:
changes verified improved, no change, inconclusive and reverted, and the verifier's own
false-positive rate ([publishing outcomes](docs/publish-outcomes.md),
[sample bundle](docs/outcomes-sample/outcomes.json)). Databases are graded against an open,
versioned [rubric](docs/rubric.md).

## What It Does

pg_sage runs as a single Go binary alongside your PostgreSQL instance. It connects
over the standard wire protocol, collects performance data from catalog views and
`pg_stat_statements`, projects issues into DBA **Cases**, and uses an LLM for
deeper analysis: LLM features are on by default and start once you configure an
endpoint and its authentication (until then pg_sage runs its deterministic rules).
Databricks Apps uses platform OAuth for Unity Gateway, without an API key;
standalone providers can use API keys. A trust-ramped executor proposes or applies typed
actions with guardrails, approval gates, rollback metadata, and a shadow-mode
report showing what autonomous policy would have handled. Works on Lakebase, Cloud SQL,
AlloyDB, Aurora, RDS, Neon, Supabase, and self-managed Postgres.
See [Neon and Supabase setup](docs/neon-supabase.md) for hosted connection requirements.

For Lakebase and Databricks Apps, start with the
[Databricks-native implementation](docs/databricks-native.md): architecture,
Databricks-only sign-in, OAuth, deployment, verified compatibility and remaining
platform limits. The integration is a development pilot; support varies by feature.

## Quick Start

```bash
# Binary (Linux amd64)
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz | tar xz
./pg_sage --pg-url "postgres://sage_agent:pw@localhost:5432/mydb"

# Docker
docker run --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:pw@host:5432/mydb" \
  -p 8080:8080 -p 8187:8187 ghcr.io/jasonmassie01/pg_sage:latest
```

From an empty install to the first finding in under a minute: see the
[five-minute quickstart](docs/quickstart.md) (minimal role, read-only first, what you see
in minute 1, minute 5 and hour 1).

Dashboard at `http://localhost:8080` -- API and Prometheus metrics at `:8080/api/v1/` and `:8187/metrics`.

For bounded, read-only pgvector recall and latency experiments, run
`pg_sage vector-lab --manifest workload.json` with `SAGE_VECTORLAB_DATABASE_URL`.
It measures `vector` and `halfvec` HNSW indexes (up to 4000 dimensions with
halfvec), `ef_search`, all three iterative-scan modes, `max_scan_tuples` and
`scan_mem_multiplier`, and scores ties at the k-th distance fairly. See the
[Vector Evidence Lab guide](docs/vector-lab.md) for the manifest fields and ranges,
the tie rule and the report format (pgvector 0.8.0 or later).

Automatic index builds require verified host CPU and data/log I/O utilization.
The catalog-only PostgreSQL adapter cannot supply those metrics, so it withholds
automatic index admission; reviewed manual index actions remain available.

After verification retains a replacement index, superseded indexes are preserved
for reviewed cleanup. The verification reason records `reviewed_cleanup_required`
while the verified index remains retained.

On first standalone start, pg_sage creates `admin@pg-sage.local` and prints a one-time
initial admin password to stderr. The dashboard and JSON API use the
`sage_session` login cookie; unauthenticated API calls return `401`.

Databricks Apps uses Databricks sign-in and pg_sage roles, without this local
password login. For standalone Docker, retrieve the password with:

```bash
docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'
```

## Features

| Area | What You Get |
|------|-------------|
| **Cases Work Queue** | Findings, incidents, migration risks, and action history are projected into ranked DBA cases with why-now context and next actions |
| **Sage SRE Investigations** | Each lock, connection or WAL incident and plan regression gets a read-only, bounded investigation: catalog probes, a deterministic causal graph and, with an LLM, a validated model review with cited claims. Measured by PGIncidentBench (fault programs plus a 60-case replay corpus). See [permissions and data flow](docs/sage-sre-permissions-and-data-flow.md) |
| **Incident Playbooks** | Runaway queries, lock blockers, connection exhaustion, WAL/replication risk, and sequence exhaustion become typed diagnostics or reviewed action scripts |
| **Vacuum/Bloat/Freeze Autopilot** | Table bloat, dead tuples, XID runway, freeze blockers, and per-table autovacuum tuning produce guarded candidates with verification plans |
| **Query Tuning Beyond Hints** | Query rewrites, broken-hint retirement, `CREATE STATISTICS`, parameterization, and repeated role-level work_mem patterns become reviewable actions with verification steps |
| **Provider Capability Adapters** | Lakebase, Cloud SQL, AlloyDB, RDS, Aurora, and self-managed Postgres expose provider-specific extension paths, log access, limitations, and action readiness |
| **DDL Safety + PR/CI Output** | Review a migration before it merges: `pg_sage lint-migration` in CI (exit 0 safe, 1 review, 2 risky; GitHub annotations or SARIF), `POST /api/v1/migrations/lint` and MCP `lint_migration` share the advisor's lock/rewrite/live-size scoring and print a guarded rewrite ([Migration review in CI](docs/migration-ci.md)). DDL that already ran becomes a migration-risk case with forward-fix guidance |
| **Rules Engine** | 20+ deterministic checks: duplicate/unused/missing indexes, slow queries, regressions, seq scans, vacuum & bloat, dead tuples, sequence exhaustion, replication lag, security audit, config drift |
| **Index Optimizer** | LLM-powered recommendations validated through 8 checks + HypoPG cost estimation, confidence scored 0.0--1.0 |
| **Config Advisors** | 6 LLM advisors: vacuum tuning, WAL/checkpoint, connections, memory, query rewrite, bloat remediation |
| **Health Briefings** | Periodic LLM summaries of fleet health, findings, and actions. The LLM cites findings by id and cannot add SQL: any statement that is not one of pg_sage's own recommendations is removed. |
| **Trust-Ramped Executor** | Observation -> Advisory -> Autonomous. Typed actions carry risk tier, guardrails, expiration, rollback/mitigation, and verification state. HIGH-risk actions always require approval. |
| **Shadow Mode** | Below each class's earned trust level, records what pg_sage would have done (SQL, prediction, the verdict had it been trusted) without running it, scores each decision from what happened, and shows the counts and potential toil minutes per class (Settings and Trust pages, `GET /api/v1/shadow-decisions`) |
| **Fleet Mode** | Monitor N databases from one binary with per-database trust levels, token budgets, and health scores |
| **Per-Query Tuner** | EXPLAIN plan analysis with `pg_hint_plan` directives for disk sorts, hash spills, bad joins, missed index scans |
| **Workload Forecaster** | Predicts disk growth, connection saturation, cache pressure, sequence exhaustion, query volume spikes, checkpoint pressure |
| **Alerting** | Slack, PagerDuty, and webhook channels with per-severity routing, cooldown, and quiet hours |
| **Dashboard & API** | React SPA + REST API embedded in the binary -- Overview, Cases, Actions, Fleet, Settings, authenticated by default |
| **Prometheus** | Standard `/metrics` endpoint with findings, collector, LLM, executor, and database size gauges |

## Documentation

See the [docs/](docs/) directory for guides and reference:

- [Installation](docs/installation.md) -- database user setup, binary and Docker deployment
- [Databricks-native implementation](docs/databricks-native.md) -- architecture, auth, operations, test evidence and platform boundaries
- [Lakebase and Unity Gateway](docs/lakebase.md) -- OAuth refresh, live checks and managed-service limits
- [Databricks Apps pilot](docs/databricks-apps.md) -- native Go packaging and deployment acceptance checks
- [Configuration](docs/configuration.md) -- YAML, environment variables, typed reload lifecycles
- [Architecture](docs/architecture.md) -- component design, goroutine model, data flow
- [Deployment](docs/deployment.md) -- production hardening, resource sizing
- [Security](docs/security.md) -- permissions model, network, secrets management
- [Sage SRE: permissions and data flow](docs/sage-sre-permissions-and-data-flow.md) -- what investigations read, the roles each provider needs, what is sent to the LLM (redaction and fencing), retention, and how to turn each part off
- [MCP for coding agents](docs/mcp.md) -- connect Claude Code or Cursor with a scoped token; source-fix packets verified after deploy
- [Decommissioning the removed database provisioner](sidecar/internal/decommission/README.md) -- inventory of every cloud and local resource an earlier version may have created, delete templates, acknowledgement
- [SQL Reference](docs/sql-reference.md) -- schema tables and diagnostic queries
- [Findings Reference](docs/findings.md) -- every rule, severity, and remediation
- [Try It Out](docs/try-it-out.md) -- local v1 smoke path and UI checklist
- **Walkthroughs:** [Linux](docs/walkthrough-linux.md) | [Windows](docs/walkthrough-windows.md) | [Fleet](docs/walkthrough-fleet.md)

## Building from Source

Requires Go 1.26+ and Node.js 22+. See [docs/installation.md](docs/installation.md) for details.

A C compiler (gcc or clang) is also needed: with cgo the binary links
libpg_query, which checks every executor statement against its PostgreSQL
parse tree. Without a C compiler the build still succeeds, but that layer is
left out; `pg_sage --version` then reports `sql-ast: unavailable`, startup
logs a warning, and changes pg_sage would make on its own wait for operator
approval instead. Release binaries and Docker images always include it.

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar/
```

## License

[AGPL-3.0](LICENSE)
