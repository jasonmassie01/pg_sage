# pg_sage — Claude Code Project Prompt

You are working on **pg_sage**, an open-source autonomous PostgreSQL DBA agent. This is a native C extension (not Rust/pgrx) that runs inside PostgreSQL as three background workers (collector, analyzer, briefing). It also includes a Go sidecar for MCP server, Prometheus metrics, and managed-cloud support.

## Project Context

pg_sage was designed through extensive research and spec iteration. The full product spec is at `docs/pg_sage_spec_v2.2.md`. A comprehensive marketing and launch plan exists. The codebase is at approximately 18,000 lines across C extension code, Go sidecar, SQL schema, tests, docs, and a Grafana dashboard.

### Brand Architecture
- **pg_sage** — the autonomous DBA agent (this project). Open source, AGPL-3.0.
- **pg_mage** (future) — learned query optimization, GPU-accelerated execution, NL query interface. The Clawgres vision.
- **Internal codename**: pg_dead_dba
- The sage earns trust. The mage wields power. pg_sage is the distribution engine for the full platform.

### Core Architecture Decisions (Do Not Change)
- **Native C extension** using PostgreSQL's Background Worker API, shared memory, SPI, and hook system. Not Rust/pgrx.
- **Three background workers**: collector (30-60s cycle), analyzer (5-15min cycle, workload-adaptive), briefing (daily).
- **Three-tier feature model**: Tier 1 (rules engine, no LLM), Tier 2 (LLM-enhanced), Tier 3 (autonomous actions with trust ramp).
- **Trust ramp**: Day 0-7 observation → Day 8-30 advisory → Day 31+ autonomous (opt-in per action type).
- **Circuit breakers**: Separate DB and LLM circuit breakers. sage must NEVER become the incident.
- **HA awareness**: Checks `pg_is_in_recovery()` every cycle. Suppresses all writes on replicas.
- **pg_stat_statements is a hard prerequisite**. sage refuses to start without it.
- **auto_explain**: Used for passive EXPLAIN capture via an ExecutorEnd hook with a lock-free ring buffer. NOT constant full capture — sampling at configurable rate (default 1%), with burst capture for targeted diagnostics.
- **LLM is pluggable and optional**: OpenAI-compatible API. Works with Ollama, Claude, OpenAI, vLLM, OpenRouter. Tier 1 features work without any LLM.
- **Privacy model**: LLM only receives metadata (schema DDL, EXPLAIN plans, parameterized query text, aggregate metrics). NEVER row data.
- **Findings deduplication**: Partial unique index on `(category, object_identifier) WHERE status = 'open'`. Escalation after 7/14 days. Auto-unsuppression of expired suppressions.
- **Go sidecar**: MCP server (JSON-RPC 2.0 over SSE), Prometheus exporter, rate limiting, API key auth, TLS support. Auto-detects whether the C extension is installed and falls back to direct catalog queries.
- **PostgreSQL 14+ support**: Code uses `#if PG_VERSION_NUM >= 150000` guards. The SQL install script says PG14+. Dockerfile currently builds against PG17 only.

### Key Files
```
src/pg_sage.c          — Entry point, shmem, worker registration, SQL-callable functions
src/guc.c              — All 45+ sage.* GUC parameter definitions
src/collector.c        — Collector background worker main loop + all collection functions
src/analyzer.c         — Analyzer background worker + all Tier 1 rules engine analysis (2600+ lines)
src/analyzer_extra.c   — Vacuum/bloat, security, replication analysis
src/action_executor.c  — Tier 3 trust-gated action execution
src/briefing.c         — Briefing generation, sage.diagnose(), sage.explain() narrative
src/tier2_extra.c      — Cost attribution, migration review, schema design review
src/context.c          — Context Assembly Pipeline for LLM prompts (token budgeting, scoped injection)
src/llm.c              — libcurl-based OpenAI-compatible API client
src/circuit_breaker.c  — DB + LLM circuit breaker logic
src/ha.c               — HA/failover detection
src/self_monitor.c     — sage self-monitoring (CPU, memory, schema size, cycle health)
src/findings.c         — Upsert with dedup/escalation, resolve, suppress, auto-unsuppress
src/explain_capture.c  — EXPLAIN plan capture and caching
src/autoexplain_hook.c — ExecutorEnd hook with lock-free ring buffer for passive capture
src/mcp_helpers.c      — SQL functions that return JSONB for MCP sidecar consumption
src/utils.c            — SPI helpers, JSON escaping, interval parsing
include/pg_sage.h      — Shared state struct, all extern declarations, GUC externs
sql/pg_sage--0.5.0.sql — Full schema (snapshots, findings, action_log, explain_cache, briefings, config)
sidecar/main.go        — Sidecar entry point, HTTP routing, SSE session management
sidecar/mcp.go         — MCP protocol types and helpers
sidecar/resources.go   — MCP resource handlers (health, findings, schema, stats, slow-queries, explain)
sidecar/tools.go       — MCP tool handlers (diagnose, suggest_index, review_migration, briefing)
sidecar/prompts.go     — MCP prompt templates
sidecar/prometheus.go  — Prometheus metrics exporter
sidecar/ratelimit.go   — Per-IP rate limiting
sidecar/auth.go        — API key authentication middleware
test/regression.sql    — 27 schema and function validation tests
test/run_tests.sql     — 14 integration tests
test/test_all_features.sql — Comprehensive feature coverage tests
```

## Known Issues to Fix (Prioritized for Launch)

### P0 — Must fix before any public announcement

1. **README says "PostgreSQL 17" but code supports PG14+.** The SQL install script literally says `/* Requires: PostgreSQL 14+ */`. Update README to say PG14+ with a version compatibility table. Update Dockerfile to mention PG14+ even though the image builds against PG17. This is leaving 80% of the market on the table.

2. **README doesn't mention the sidecar, MCP server, Prometheus exporter, or Grafana dashboard.** These are major differentiators and they're invisible. Add sections to the README for each.

3. **`sage.llm_enabled` default mismatch.** guc.c line 52 sets `bool sage_llm_enabled = true;` but the README config table says default is `off`. LLM features should be opt-in. Change the C default to `false`. Users who configure an LLM endpoint should explicitly enable it.

4. **Advisory lock hash function.** The `hashtext_simple` function in pg_sage.h is a custom reimplementation, not Postgres's actual hashtext. Replace with a fixed magic number to avoid collision risk: `#define SAGE_ADVISORY_LOCK_KEY 483722657` (or similar). Use this constant in both the C code and the SQL advisory lock calls.

5. **Missing `go.sum` in sidecar.** Run `go mod tidy` in the sidecar directory and commit `go.sum` for reproducible builds.

### P1 — Should fix before HN launch

6. **Maintenance window cron parsing is stubbed.** `sage_check_maintenance_window()` in action_executor.c only handles `*`/`always`/empty. Either implement basic day-of-week + hour parsing (sufficient for v1) or document the limitation clearly in the README and docs. Don't silently return false for cron expressions.

7. **Add `docs/security.md`** covering: why superuser is required (background worker registration), what sage does with those privileges, what it explicitly never does (no row data access, no credential access), network behavior (zero outbound calls when LLM disabled, metadata-only when enabled), the privacy model, and the `sage.redact_queries` / `sage.anonymize_schema` controls.

8. **Add cloud environment auto-detection.** In the sidecar, detect Aurora (check for `aurora_version()` function), Cloud SQL (check for GCP-specific settings), AlloyDB, and Azure. When on Aurora/RDS, suppress ALTER SYSTEM recommendations and instead suggest parameter group changes. Log the detected environment at startup.

9. **Add GitHub Actions CI.** Build matrix across PG14, 15, 16, 17. Run regression tests. This is a green badge on the README that signals maturity. The test infrastructure already exists — just needs the workflow YAML.

10. **Add pganalyze comparison to README.** Stay in lane — agent vs dashboard, not feature-for-feature. Key points: pg_sage lives inside Postgres (not external collector), takes action (not just recommends), free/AGPL (not $149-399/mo), has MCP server (pganalyze doesn't), works air-gapped. A pganalyze G2 reviewer literally asked for "an official MCP server for agentic workflows" — we have one.

### P2 — Post-launch improvements

11. **`sage.llm_provider` shortcut.** Set to `ollama` and sage auto-configures endpoint to `http://localhost:11434/v1`. Set to `openai` and it just needs the API key. Reduces LLM setup from 3 parameters to 1-2.

12. **Add `CONTRIBUTING.md`** with build instructions, test instructions, code style guidelines, and how to add a new analysis function to the rules engine.

13. **Add GitHub topics** to the repo: `postgresql`, `postgres`, `database`, `dba`, `ai`, `llm`, `monitoring`, `extension`, `mcp`.

14. **Add `llms.txt`** to the mkdocs site for AI-friendly documentation (the "chattable docs" feature).

15. **Custom findings hook** — allow users to register a SQL function that returns findings in the sage.findings format. Sage calls it during each analyzer cycle. This is the extensibility mechanism — not a plugin system, just "give me a function that returns findings."

16. **Static analysis (cppcheck/Coverity) run** and publish results. Clean report is a trust signal for enterprise security teams.

17. **Build against PG14/15/16 Docker images** and publish pre-built binaries for Debian/Ubuntu/RHEL. The Dockerfile currently only builds PG17.

## Design Principles (Follow These Always)

- **Simplicity over features.** Don't replace database complexity with tool complexity. A developer who has never been a DBA should install pg_sage, read their first briefing the next morning, and understand what to do.
- **The extension IS the product.** The sidecar is for managed-cloud users and MCP. For self-managed Postgres, everything should work with just `CREATE EXTENSION pg_sage`. No sidecar required.
- **Tier 1 is the foundation.** It must work perfectly without any LLM. The LLM is a multiplier, not a dependency.
- **Never become the incident.** Circuit breakers, statement timeouts, connection budgets, backoff escalation, emergency stop. Every code path that touches the database must have a safety limit.
- **Findings are the currency.** Everything the analyzer detects flows through `sage_upsert_finding()`. This is the single source of truth. Briefings, actions, and MCP resources all read from `sage.findings`.
- **Stay in lane.** pg_sage is the autonomous DBA agent. It's not a dashboard product (that's pganalyze's lane), not a query rewriter (that's pg_mage territory), not a schema migration tool (that's Flyway/Alembic). It detects problems, explains them, and fixes them if you let it.
- **Parameterized queries everywhere.** All SPI calls use `SPI_execute_with_args` with typed parameters. Never string-concatenate user input into SQL. This is both a security requirement and a code quality standard.
- **Error isolation.** Every analysis function runs inside PG_TRY/PG_CATCH. One failure cannot take down the analyzer cycle.
- **Honest about limitations.** If something is stubbed or not yet implemented, document it clearly. Don't silently no-op.

## Competitive Positioning

pg_sage is NOT competing with:
- **pganalyze** — they're a monitoring dashboard, we're an autonomous agent. Some users will use both.
- **Datadog/New Relic** — they're APM platforms with DB monitoring bolted on. Different category.
- **pg_cron** — we're not a scheduler. We use background workers.

pg_sage IS competing with:
- **PostgresAI** — SaaS DBA service. We're self-hosted, open source, lives inside Postgres.
- **DBtune** — SaaS config tuner. We're broader (full DBA surface area) and self-hosted.
- **OtterTune (dead)** — died because it was one-shot SaaS. We're continuous, self-hosted, trust-ramped.
- **The human DBA** — the $150-200K/year role that most startups can't fill. We're the $0 alternative.

## What's Next After Launch

The roadmap from the spec:
- Phase 0.2: Vacuum/bloat management depth, security audit depth, backup health, sidecar mode polish
- Phase 0.3: Maintenance windows (full cron), trust ramp automation, Tier 3 actions, auto-rollback
- Phase 0.4: ReAct diagnostic loop depth, cost attribution engine, data retention policies
- Phase 0.5: Migration review, sage shell CLI, schema design review, Prometheus polish
- 1.0: Production-hardened release

Long term: pg_sage gets distribution → MCP server makes sage the gateway for AI-database interaction → pg_mage ships learned query optimization → full Clawgres platform.
