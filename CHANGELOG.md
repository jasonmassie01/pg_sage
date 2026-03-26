# Changelog

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
