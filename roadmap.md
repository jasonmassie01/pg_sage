# pg_sage — Roadmap

## Released

### v0.7.0 (2026-03-26)
- Go sidecar with standalone mode
- Tier 1 rules engine (18+ deterministic checks)
- Index Optimizer v2 (LLM-powered, HypoPG validation, confidence scoring, 8 validators)
- 6 LLM advisor features (vacuum, WAL, connection, memory, query rewrite, bloat)
- MCP server (Claude Desktop / AI agent interface)
- Prometheus metrics
- Trust-ramped executor (observation → advisory → autonomous)
- Verified on self-managed PG14–17
- 530 tests, 0 failures

### v0.8.0 (2026-03-26) — Fleet Mode + Dashboard
- Fleet manager: single sidecar → N databases via `mode: fleet` config
- `DatabaseManager` with per-database collector/analyzer/executor goroutines
- Per-database advisory locks, trust levels, executor toggles
- Per-database LLM token budget (equal, proportional, or priority-weighted)
- Database-aware data model (every finding, action, metric carries `database_name`)
- Prometheus labels: `{database="prod-orders"}`
- Graceful per-database failure (one DB down doesn't crash others)
- REST API: 14 endpoints on `:8080` alongside MCP
- Fleet overview: `GET /api/v1/databases` with health scores
- Findings, actions, snapshots, config — all filterable by `?database=`
- Config hot-reload via `PUT /api/v1/config`
- Emergency stop/resume per-database and fleet-wide
- Web dashboard (React SPA embedded in binary via `//go:embed`)
- Demo environment: Docker Compose with 7 pre-planted problems, 46 verification checks
- 584+ tests, 0 failures, CI green (6 workflows)

**Integration bug fixes shipped in v0.8.0:**
- VACUUM routed through non-transaction connection (pgxpool wraps in tx by default)
- Trust ramp `ramp_start` config honored on first boot (was always `now()`)
- Unused index window default changed to 7 days (was 0, caused index churn)
- Advisor strips markdown fences from Gemini JSON responses
- `database_name` resolved to actual instance name (was showing "all")

### v0.8.1 (2026-03-27) — Patch
- Add `google_ml` to all schema exclusion lists (Cloud SQL compatibility)
- Bump default LLM max_tokens to 8192 (Gemini 2.5 Flash thinking token fix)
- Add retry loop to index validity post-check (catalog propagation delay)
- Prevent re-execution of already-acted findings (re-drop race fix)
- Executor cooldown for recently created indexes
- Verified on Cloud SQL PG16/17 and AlloyDB PG17
- 588 tests, 0 failures

### v0.8.2 (2026-04-03) — LLM Tuner + Query Rewrites
- Query tuner: hybrid deterministic rules (7 symptom kinds) + LLM-enhanced hints
- LLM-powered query rewrite suggestions alongside pg_hint_plan directives
- Rewrite suggestions surfaced in dashboard with rationale
- Alert notification (`query_rewrite_suggested` event) when rewrite is suggested
- Index optimizer multi-query consolidation (8 queries → minimal index set)
- E2E test suite: 54 subtests against real Gemini API
- 771+ tests

---

## Planned

### v0.9.0 — Agent Mode (Tier 2)
**Status:** Spec complete, not started

- `mode: agent` — single sidecar → one database, pushes to control plane
- Agent push protocol: heartbeat, findings, metrics over HTTPS
- Finding fingerprint for cross-push deduplication
- Control plane API contract defined (5 endpoints)
- Agent runs locally even if control plane unreachable
- Designed for 50–1000 database fleets

### v1.0.0 — Control Plane
**Status:** Architecture defined, not started

- Central API + PostgreSQL store for aggregated findings
- Fleet dashboard served from control plane (replaces per-sidecar dashboard)
- Agent registration, configuration push, status monitoring
- Multi-tenant support (API key per customer)
- Cross-database correlation (same query pattern across DBs)

---

## Future

### Post-v1.0

**Cloud platform verification**
- Aurora + RDS — test plans written, execution pending, zero code changes expected
- Azure Flexible Server — new platform detection + managed service restriction table

**HypoPG dependency note**
- HypoPG is optional but recommended for index optimization
- Without it, optimizer falls back to EXPLAIN-only validation (no hypothetical index testing)

**GitHub Actions integration**
- DDL review as PR check (migration review finding → GitHub comment)
- Schema change impact analysis

**Alerting integrations**
- PagerDuty, Slack, OpsGenie webhook on critical findings
- Configurable alert routing per severity

**Historical trend analysis**
- Long-term finding trends (weekly, monthly)
- Regression detection across releases
- Workload shift detection (query pattern changes)

**Query plan diffing**
- Detect plan changes for the same query across time
- Alert on plan regression (new seq scan where index scan was used)

**Cost attribution**
- Map resource consumption (CPU, I/O, memory) to queries and schemas
- Show cost per query in the dashboard
