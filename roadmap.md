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

### v0.8.3 (2026-04-04) — Cloud E2E + LLM Token Optimization
- Cloud E2E validation across 8 managed PostgreSQL databases:
  RDS PG14/18, Aurora PG14/17, Cloud SQL PG14/18, AlloyDB PG14/17
- Auto-detect cloud environment (rds, aurora, cloud-sql, alloydb)
- ALTER SYSTEM → ALTER DATABASE rewriting for managed platforms
- Executor max-retry limit (3 failures → mark as acted_on)
- LLM deduplication: skip redundant calls when open findings/hints exist
  (optimizer, tuner, advisor all check sage.findings/sage.query_hints first)
- 11 token waste fixes: bloat category mismatch, vacuum validation bug,
  thinking-model budget, CapturePlans loop hoist, column stats filtering,
  per-cycle table cap, per-query rewrite dedup, briefing LIMIT,
  retry scope (429/503 only), tuner stats cap, single-symptom deterministic skip
- Thinking model support: +16384 token overhead for Gemini 2.5 reasoning
- Cross-platform findings: 1615 total, 373 open, 802 acted on across 8 DBs
- All packages above 70% test coverage

### v0.8.4 (2026-04-07) — Security Hardening + Tuner Pipeline

**Security**
- RBAC: Added `RequireRole` to legacy API routes (config, emergency-stop, resume, suppress/unsuppress, pending count). Viewers can no longer escalate trust to autonomous or halt the fleet.
- SQL injection: Block EXPLAIN injection via multi-statement rejection + `statement_timeout` + read-only transactions. New `sanitize.QuoteIdentifier` package for all catalog-derived identifiers. Fixed VACUUM, ALTER DATABASE, and tuner SQL construction.
- Executor allowlists: Whitelisted 30 safe `ALTER SYSTEM` GUCs. Restricted SELECT to `pg_terminate_backend`/`pg_cancel_backend`. Restricted `ALTER TABLE` to `SET`/`RESET`/`TABLESPACE`. Validate trust level strings. Cap concurrent DDL at 3. Auto-drop invalid indexes after failed `CREATE INDEX CONCURRENTLY`. Derive advisor `ActionRisk` from SQL type instead of hardcoding `safe`.
- Auth: Cookie `Secure` flag enabled. Password minimum 8 characters. Login rate limiting (5 attempts / 15 min per email). Self-deletion + last-admin protection.
- API hardening: Security headers middleware. Content-Type validation. SSRF protection on `test-connection`. Error messages sanitized (no more `err.Error()` leaked to clients). `X-Forwarded-For` only trusted from loopback. Notification channel secrets masked.
- Secrets: Removed hardcoded Gemini API key from `docker-compose.test.yml`. Admin password printed to stderr only. `create_admin` uses environment variables. `DeriveKey` upgraded to argon2id with v1 backward compatibility. Demo config uses env var.

**Tuner Pipeline**
- `BuildInsertSQL`/`BuildDeleteSQL` now use `norm_query_string` column (was `query_id`, which doesn't exist in `hint_plan.hints`).
- Executor allowlist updated for `INSERT`/`DELETE INTO hint_plan.hints`.

**Fleet Hardening**
- Schema bootstrap advisory lock changed from non-blocking `pg_try_advisory_lock` to blocking `pg_advisory_lock` with 30s timeout.
- `CREATE SCHEMA` now uses `IF NOT EXISTS`. All `DROP SCHEMA` operations in tests wrapped in advisory lock.
- `autoexplain.ConfigureSession` tolerates permission denied (SQLSTATE 42501) on managed DBs where role-level defaults suffice. Coverage boost to 83%.

**API**
- New LLM token usage endpoint.
- Config apply/audit handlers.
- Settings page UI improvements.
- New `create_admin` CLI command.

**Test Infrastructure**
- `auth`, `notify`, and `store` tests now hold `pg_sage` advisory lock for test duration to prevent schema drops mid-test.
- Store config tests re-insert FK user before each test.
- All 22 packages pass with `-p 4` (zero failures, zero skips).

---

## Considering for Future Releases

**Multi-database aggregation** — was originally scoped as v0.9 (agent push protocol) and v1.0 (control plane). Currently re-evaluating based on user feedback. Fleet mode (single sidecar → N databases) covers most use cases today.

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
