# pg_sage

**Autonomous PostgreSQL DBA Agent** -- a Go sidecar that monitors, analyzes, and optimizes any PostgreSQL 14-17 database over the network.

pg_sage connects to your database via standard libpq/pgx, collects performance data from `pg_stat_statements` and catalog views, runs 18+ diagnostic rules, sends enriched context to an LLM for index optimization, and executes fixes autonomously with trust-ramped safety controls.

No C extension required. Works on managed services (Cloud SQL, AlloyDB, Aurora, RDS) and self-managed instances with zero code changes.

---

## Key Features

| Feature | Description |
|---|---|
| **Connects over the network** | Go sidecar, no extension installation, no PostgreSQL restart |
| **Works everywhere** | Managed services (Cloud SQL, AlloyDB, Aurora, RDS) and self-managed |
| **Three-tier architecture** | Rules engine (Tier 1), LLM index optimizer (Tier 2), trust-gated executor (Tier 3) |
| **Trust-ramped autonomy** | Graduated from observation to advisory to autonomous over time |
| **LLM optional** | Tier 1 works without any LLM endpoint configured |
| **MCP server** | Exposes capabilities to AI assistants (Claude Desktop, Cursor) via Model Context Protocol |
| **Prometheus metrics** | Scrape `:9187/metrics` for monitoring and alerting |
| **Self-protecting** | Circuit breakers prevent pg_sage from becoming the incident |
| **Full audit trail** | Every action logged with before/after state and rollback SQL |

---

## What It Detects

**Tier 1 -- Rules Engine** (18+ deterministic checks, no LLM required):

- **Index health**: duplicate indexes, unused indexes, invalid indexes, missing FK indexes
- **Query performance**: slow queries, high plan time, query regressions, sequential scans on large tables
- **Sequences**: approaching exhaustion (bigint/int overflow)
- **Maintenance**: table bloat, XID wraparound risk
- **System health**: connection leaks, low cache hit ratio, checkpoint pressure
- **Replication**: lag monitoring, inactive replication slots

**Tier 2 -- LLM Index Optimizer** (optional):

- Consolidated index recommendations across your workload
- 8 validators (CONCURRENTLY keyword, column existence, duplicate detection, write impact, max indexes, extension requirements, BRIN correlation, expression volatility)
- HypoPG validation with measured cost reduction
- Confidence scoring (0.0-1.0) with 6 weighted signals
- Dual-model routing (fast model for general tasks, reasoning model for optimization)

---

## Quick Start

```bash
# Download
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64 -o pg_sage
chmod +x pg_sage

# Run (observation mode, no LLM)
./pg_sage --database-url "postgres://sage_agent:YOUR_PASSWORD@host:5432/postgres"
```

Findings appear within 60 seconds. See the [Installation](installation.md) guide for detailed setup, or the [Configuration](configuration.md) reference for all options.

---

## Comparison

| | pg_sage | pganalyze | OtterTune / DBtune |
|---|---|---|---|
| **Works on managed DBs** | Yes, zero changes | SaaS agent required | Cloud-only SaaS |
| **Takes action** | Trust-ramped autonomous remediation | Recommendations only | Knob tuning only |
| **Self-hosted** | Fully, AGPL-3.0 | Proprietary | Proprietary |
| **LLM dependency** | Optional (Tier 1 works without it) | N/A | Required |
| **Installation** | Download binary + connect | Install collector agent | Cloud signup |

---

## C Extension (Frozen)

The C extension at `extension/` is frozen at v0.6.0-rc3. It works on self-managed PostgreSQL but is not the product. No new features -- security fixes only. The Go sidecar is the product.

---

## License

pg_sage is licensed under the [GNU Affero General Public License v3.0](https://www.gnu.org/licenses/agpl-3.0.html).
