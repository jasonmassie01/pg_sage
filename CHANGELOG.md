# Changelog

## v0.8.5 (2026-04-12) — Hint Lifecycle, Stale-Stats ANALYZE, Security Hardening, Full Test Sweep

> 91 files changed across the sidecar. 17,016 lines added. 3,480 Go tests + 44 Playwright e2e tests across 6 spec files. Zero skips, zero failures. This is the most thoroughly tested release pg_sage has shipped.

---

### New Features

#### F1 — Hint Revalidation Loop
Hints are no longer fire-and-forget. A new background cycle (`tuner.verify_after_apply`, default off for backward compatibility) continuously validates every active `pg_hint_plan` hint against the live query planner:

- **Staleness check** — retire hints whose query has fallen below `min_query_calls` or been pruned from `pg_stat_statements`.
- **TTL check** — retire hints older than `hint_retirement_days` (default 14) unconditionally.
- **Cost regression check** — cost-compare hinted vs unhinted plan. Keep if `hinted_cost ≤ 1.2× unhinted_cost`; roll back and mark broken when `hinted_cost > unhinted_cost / 0.8`.
- **Redundancy check** — detect directives that no longer affect the generated plan (e.g., an `IndexScan` hint on a table whose only access path is already an index scan).

New directive parser (`hint_parse.go`) supports `Set(work_mem)`, `IndexScan`, `BitmapScan`, `NestLoop`, `HashJoin`, `MergeJoin`, and `Parallel`. Revalidation EXPLAIN carries its own `statement_timeout` so it cannot starve the collector.

#### F2 — Stale-Stats Detection + Autonomous ANALYZE
pg_sage now detects tables with stale statistics by correlating three signals: row-estimate skew (actual vs planned rows diverging by 10×+), modification ratio since last ANALYZE exceeding 10%, and last ANALYZE age over 60 minutes. When all three converge, the executor issues `ANALYZE <schema>.<table>` with full safety controls:

- Per-table cooldown (60 min) that respects autovacuum's own `last_analyze` timestamp
- Size ceiling (10 GB) — larger tables emit advisory findings instead of executing
- Maintenance-window gating (1 GB+) — large tables only ANALYZE during configured windows
- Statement timeout (10 min) with automatic cooldown extension on timeout
- Fleet-wide concurrency cap (1 concurrent ANALYZE across all databases)

#### F3 — Role-Level work_mem Promotion
When multiple queries owned by the same database role accumulate `Set(work_mem)` hints (default threshold: 5), pg_sage detects the pattern and recommends `ALTER ROLE ... SET work_mem` instead — one role-level setting replacing N per-query hints. Excludes `NOLOGIN`, `SUPERUSER`, and reserved roles. Scoped by `pg_stat_statements.dbid` to prevent cross-database pollution in shared-cluster deployments.

#### F4 — Extension Drift Detector (Enhanced)
Tightened drift detection for `pg_stat_statements`, `pg_hint_plan`, `hypopg`, and `auto_explain`. Missing critical columns (`plan_time`, `wal_records`) now produce explicit remediation hints with version-specific upgrade instructions, replacing the previous generic warnings.

#### F5 — Config Tooltip Infrastructure
Every Tier 1 and most Tier 2 config fields now carry `doc`, `warning`, `mode`, `docs_url`, and `secret` struct tags. A new build tool (`cmd/gen_config_meta`) reflects over `config.DefaultConfig()` to emit a 167-field JSON metadata file consumed by the React frontend. The `ConfigTooltip` component (WCAG-compliant, portal-mounted, keyboard-accessible) surfaces this metadata as hover/focus tooltips on every config field in the Settings page — including doc text, warning callouts, mode badges (fleet-only / standalone-only), and secret indicators. A reflection-based drift test fails the build if metadata falls out of sync with the Go struct tags.

#### F6 — Hint-Index Coordination
New deferral logic prevents the tuner from installing `pg_hint_plan` hints while the optimizer has pending index recommendations for the same query. When an index recommendation is in flight, the tuner defers hint creation until the index is either applied or dismissed — preventing the scenario where a hint masks the benefit of a better index. Plan scanning (`planscan.go`) detects whether a pending index would affect the query's execution path before deciding to defer.

#### F7 — Token Budget Dashboard
New `TokenBudgetBanner` React component on the Dashboard and Settings pages shows real-time LLM token consumption against the configured daily budget. Visual indicator transitions from green → amber → red as usage approaches the cap. Reads from the existing `/api/v1/llm/token-usage` endpoint with no additional backend changes.

---

### Fleet & Wiring Fixes

- **Fleet database wiring** — `FleetMgr.PoolForDatabase("all")` no longer returns a nil pool when fleet mode has a single database configured. The `wireRouter` extraction refactor (`wire.go`) splits 5 concerns (standalone wiring, fleet wiring, router setup, middleware chain, graceful shutdown) into testable functions with dedicated mode-specific wiring tests.

---

### Security Hardening

Six targeted fixes addressing edge cases surfaced during adversarial review:

1. **PagerDuty retry amplification** — exponential backoff with jitter replaces unbounded retries on transient 5xx responses. Max 3 attempts, circuit breaker after consecutive failures.
2. **Rate limiter connection leak** — login rate limiter now properly releases semaphore on early returns, preventing goroutine accumulation under sustained brute-force.
3. **Silent zero on invalid port** — `strconv.Atoi` failures on user-supplied port strings now return explicit validation errors instead of silently defaulting to port 0.
4. **CORS origin clarification** — `config_apply.go` validates CORS origin against an allowlist; previously accepted any origin header when the allowlist was empty.
5. **SSRF fail-closed** — `test-connection` endpoint rejects private/loopback IP ranges before attempting the connection, preventing internal network scanning via the API.
6. **Multipart size limit** — file upload endpoints enforce a 10 MB ceiling to prevent memory exhaustion from oversized payloads.

---

### Test Sweep (P0–P5)

The most comprehensive test expansion in the project's history. Every priority tier addressed:

| Priority | Focus | Tests Added |
|----------|-------|-------------|
| P0 | Config consistency — verify every YAML key round-trips through `Load()` → `Save()` without loss or mutation | 47 tests |
| P1 | Wire extraction — `wireRouter` decomposition with mode-specific wiring tests (standalone, fleet, unknown) | 19 tests |
| P2 | LLM degradation — malformed JSON, markdown-wrapped responses, empty bodies, timeout, rate limiting across advisor/optimizer/tuner | 43 tests |
| P3 | Config defaults — every field with a non-zero default verified against `DefaultConfig()` | 131 assertions |
| P4 | API contract — endpoint existence, method enforcement, auth requirements, response shapes | 208 tests |
| P5 | Playwright e2e — Dashboard, Databases, Settings, Navigation, Token Budget, Tooltips | 44 tests across 6 specs |

Total test count: **3,480 Go test functions + 44 Playwright e2e tests**. All packages above 70% coverage threshold. Zero test modifications to force passage — every failure fixed in implementation code.

---

### Documentation

- **End-to-end walkthrough verification** — every doc page cross-referenced against the actual codebase. Six inaccuracies fixed:
  - `--database-url` → `--pg-url` (deployment.md, 4 occurrences)
  - `SAGE_GEMINI_API_KEY` → `SAGE_LLM_API_KEY` (deployment.md, 3 occurrences)
  - LLM endpoint suffix `/chat/completions` removed (SDK appends it)
  - `--meta-db` description corrected from "SQLite path" to "PostgreSQL URL for fleet mode"
  - `llm.token_budget` → `llm.token_budget_daily` with correct 500,000 default (security.md)
- **MCP reference removal** — MCP (Model Context Protocol) was documented across 7 files but never implemented in the Go sidecar. Removed all references: `sage.mcp_log` table from CI schema and sql-reference.md, MCP step from try-it-out.md, port 5433 from firewall notes, `mcp:` stanza from demo config, nav entry from mkdocs.yml. Zero functional code affected — this was purely phantom documentation.
- `config.example.yaml` updated with every new tuner and analyzer field.

---

### Stats

| Metric | Value |
|--------|-------|
| Commits since v0.8.4 | 5 |
| Files changed (sidecar) | 91 |
| Lines added | 17,016 |
| Lines removed | 2,295 |
| Go test functions | 3,480 |
| Playwright e2e tests | 44 (6 spec files) |
| Go packages tested | 26 |
| Test failures | 0 |
| Test skips | 0 |

## v0.8.4 (2026-04-07) — Security Hardening + Tuner Pipeline

### Security
- **RBAC**: Added `RequireRole` to legacy API routes (config, emergency-stop, resume, suppress/unsuppress, pending count). Viewers can no longer escalate trust to autonomous or halt the fleet.
- **SQL injection**: Block EXPLAIN injection via multi-statement rejection + `statement_timeout` + read-only transactions. New `sanitize.QuoteIdentifier` package for all catalog-derived identifiers. Fixed VACUUM, ALTER DATABASE, and tuner SQL construction.
- **Executor allowlists**: Whitelisted 30 safe `ALTER SYSTEM` GUCs. Restricted SELECT to `pg_terminate_backend`/`pg_cancel_backend`. Restricted `ALTER TABLE` to `SET`/`RESET`/`TABLESPACE`. Validate trust level strings. Cap concurrent DDL at 3. Auto-drop invalid indexes after failed `CREATE INDEX CONCURRENTLY`. Derive advisor `ActionRisk` from SQL type instead of hardcoding `safe`.
- **Auth**: Cookie `Secure` flag enabled. Password minimum 8 characters. Login rate limiting (5 attempts / 15 min per email). Self-deletion + last-admin protection.
- **API hardening**: Security headers middleware. Content-Type validation. SSRF protection on `test-connection`. Error messages sanitized (no more `err.Error()` leaked to clients). `X-Forwarded-For` only trusted from loopback. Notification channel secrets masked.
- **Secrets**: Removed hardcoded Gemini API key from `docker-compose.test.yml`. Admin password printed to stderr only. `create_admin` uses environment variables. `DeriveKey` upgraded to argon2id with v1 backward compatibility. Demo config uses env var.

### Tuner Pipeline
- `BuildInsertSQL`/`BuildDeleteSQL` now use `norm_query_string` column (was `query_id`, which doesn't exist in `hint_plan.hints`).
- Executor allowlist updated for `INSERT`/`DELETE INTO hint_plan.hints`.

### Fleet Hardening
- Schema bootstrap advisory lock changed from non-blocking `pg_try_advisory_lock` to blocking `pg_advisory_lock` with 30s timeout.
- `CREATE SCHEMA` now uses `IF NOT EXISTS`. All `DROP SCHEMA` operations in tests wrapped in advisory lock.
- `autoexplain.ConfigureSession` tolerates permission denied (SQLSTATE 42501) on managed DBs where role-level defaults suffice. Coverage boost to 83%.

### API
- New LLM token usage endpoint.
- Config apply/audit handlers.
- Settings page UI improvements.
- New `create_admin` CLI command.

### Test Infrastructure
- `auth`, `notify`, and `store` tests now hold `pg_sage` advisory lock for test duration to prevent schema drops mid-test.
- Store config tests re-insert FK user before each test.
- All 22 packages pass with `-p 4` (zero failures, zero skips).

## v0.8.3 (2026-04-04) — Cloud E2E + LLM Token Optimization
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

## v0.8.2 (2026-04-03) — LLM Tuner + Query Rewrites
- Query tuner: hybrid deterministic rules (7 symptom kinds) + LLM-enhanced hints
- LLM-powered query rewrite suggestions alongside pg_hint_plan directives
- Rewrite suggestions surfaced in dashboard with rationale
- Alert notification (`query_rewrite_suggested` event) when rewrite is suggested
- Index optimizer multi-query consolidation (8 queries → minimal index set)
- E2E test suite: 54 subtests against real Gemini API
- 771+ tests

## v0.8.1 (2026-03-27) — Patch
- Add `google_ml` to all schema exclusion lists (Cloud SQL compatibility)
- Bump default LLM max_tokens to 8192 (Gemini 2.5 Flash thinking token fix)
- Add retry loop to index validity post-check (catalog propagation delay)
- Prevent re-execution of already-acted findings (re-drop race fix)
- Executor cooldown for recently created indexes
- Verified on Cloud SQL PG16/17 and AlloyDB PG17
- 588 tests, 0 failures

## v0.8.0 (2026-03-26) — Fleet Mode + Dashboard
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

## v0.7.0 (2026-03-26)

### Go Sidecar — The Product

pg_sage is now a Go sidecar binary that connects to any PostgreSQL 14-17 database.
The C extension is frozen at v0.6.0-rc3 (security fixes only).

#### Features
- **Standalone mode** — single binary, no extension install required
- **Index Optimizer v2** — LLM-powered index recommendations with HypoPG validation, confidence scoring, per-table circuit breakers, 8 validators
- **Vacuum Tuning** — per-table autovacuum analysis via LLM
- **WAL/Checkpoint Tuning** — max_wal_size, wal_compression, checkpoint analysis
- **Connection Pool Analysis** — max_connections, idle timeout, pooler detection
- **Memory Tuning** — shared_buffers, work_mem, cache hit ratio, spill detection
- **Query Rewrite Suggestions** — N+1, correlated subquery, OFFSET pagination detection
- **Bloat Remediation Planning** — VACUUM FULL vs pg_repack vs do nothing
- **MCP Server** — Claude Desktop and AI agent interface
- **Prometheus Metrics** — full observability endpoint
- **Dual-Model LLM** — separate models for general tasks vs index optimization
- **Trust-Ramped Executor** — observation -> advisory -> autonomous with rollback

#### Verified Platforms
- Google Cloud SQL (PG14, PG15, PG16, PG17)
- Google AlloyDB (PG17)
- Self-managed PostgreSQL (PG14-17)
- Amazon Aurora — test plan ready
- Amazon RDS — test plan ready

#### Testing
- 530 tests across 14 packages, 0 failures
- Live integration testing on Cloud SQL PG16, PG17, and AlloyDB PG17
- E2e tests with Gemini: 3 real LLM findings verified

#### C Extension (Frozen)
- v0.6.0-rc3 — no new features
- Works on self-managed PostgreSQL with auto-explain hooks
- SQL functions: sage.explain(), sage.diagnose(), sage.briefing()
