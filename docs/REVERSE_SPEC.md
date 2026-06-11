# pg_sage — Reverse-Engineered Specification (as-built, 2026-06-10)

This document describes **what pg_sage actually is and does today**, derived by reading the
source — not the README or marketing. It is the ground-truth baseline for the roadmap.
Detailed sections live in [`docs/reverse_spec/`](./reverse_spec/):

1. [Architecture & Process Model](./reverse_spec/01-architecture.md)
2. [Tier 1 — Collector & Deterministic Rules](./reverse_spec/02-tier1-rules.md)
3. [Tier 2 — LLM-Enhanced Features](./reverse_spec/03-tier2-llm.md)
4. [Tier 3 — Action Executor & Safety](./reverse_spec/04-tier3-executor.md)
5. [AgentDB — Provisioning Subsystem](./reverse_spec/05-agentdb.md)
6. [API, Auth & Web Dashboard](./reverse_spec/06-api-auth-web.md)
7. [Data Model & Supporting Subsystems](./reverse_spec/07-data-model-support.md)

---

## 1. What it is

pg_sage is an **external Go sidecar** that connects to PostgreSQL over the wire protocol
(no extension, no `shared_preload_libraries`) and continuously monitors, analyzes, and
— at sufficient trust — mutates the database. It is built around a **three-tier model**:

- **Tier 1 (deterministic):** a collector snapshots ~13 stat categories into `sage.*` tables;
  a rules engine (~20 rules) turns them into `findings`.
- **Tier 2 (LLM, optional):** 15 distinct LLM call-sites enrich analysis (briefings, index
  recs, config tuning, query rewrites, RCA, migration risk, JSONB lint, AgentDB blueprints).
  **Every LLM output degrades to a `finding`/`incident`; none feeds the executor directly.**
- **Tier 3 (executor):** a trust-gated, SQL-whitelisted action runner applies SAFE/MODERATE
  changes (CREATE INDEX CONCURRENTLY, VACUUM, REINDEX, config) with rollback metadata.

It runs in three deployment shapes selected by `cfg.Mode` + the `--meta-db` flag:
**standalone** (one DB), **fleet** (N YAML-defined DBs, one pipeline each), and **meta-db
fleet** (store-backed, the only mode with dynamic add/remove/reconnect). A separate
**AgentDB** subsystem provisions agent-requested databases on AWS RDS / GCP Cloud SQL /
Databricks Lakebase.

**Scale of the codebase:** ~65k LOC Go across 32 internal packages, 38 `sage.*` tables,
~139 REST endpoints, 10 dashboard pages, React 19 + Vite frontend embedded via `go:embed`.

## 2. Control flow (one database)

```
collector(60s) ──> sage.snapshots ──> analyzer cycle ──┬─> Tier1 rules ──> findings
                                                        ├─> optimizer (LLM, inline)
                                                        └─> advisor   (LLM, inline)
orchestrator(analyzerInterval+5s) ──> executor.RunCycle ──> ShouldExecute(trust,risk,estop)
                                                          └─> ValidateExecutorSQL(whitelist)
                                                          └─> ddl.go (dedicated conn, timeouts)
                                                          └─> action_log (+ rollback metadata)
                                                          └─> MonitorAndRollback goroutine
```
Findings dedup on `(Category, ObjectIdentifier)`; cleared findings auto-resolve; a 2-min
grace prevents action-induced flapping. Emergency-stop is persisted per-DB in `sage.config`
and **fails closed** (now hardened). Monitoring never stops on emergency-stop — only actions gate.

## 3. Reality vs. the docs — deltas a reader must know

The system is broadly sound, but several advertised capabilities are **coded-but-unwired or
absent**. These materially shape the roadmap (don't re-build what exists; do finish/cut these):

| Claim / expectation | Reality |
|---|---|
| "17 REST endpoints" (CLAUDE.md) | **~139** method+path pairs; AgentDB alone is 62. |
| Trust auto-ramps observation→advisory→autonomous | **No auto-promotion.** Day-8/day-31 thresholds gate a *manually-set* `trust.level` string. |
| HA safe-mode halts actions during failover | `ha.InSafeMode()` is built + tested but **wired to nothing**; fleet hardcodes `isReplica=false`. |
| Per-database LLM token budgets (fleet isolation) | `fleet.FleetBudget` exists + tested but **never constructed**; budgeting is per-`llm.Client` daily only. |
| Index bloat / REINDEX advising | Threshold config plumbed to the API, but **no Tier-1 rule implements it**. |
| Interactive LLM "diagnose / ReAct loop" | **Does not exist.** `diagnose_*` are deterministic read-only SQL probes. |
| Optimizer advisory threshold (0.5) gates index recs | `ConfidenceThreshold` has **zero consumers**; every accepted rec becomes a finding. |
| Cases model drives execution | **Two disconnected Tier-3 systems.** The live executor gates `findings`; the richer `cases`/`ActionContract` model is API-advisory only (`ProjectFinding` never called from executor). |
| Event-path notifications (executor/analyzer → Slack/email) | Senders **never registered**; all event notifications silently no-op ("no sender for type"). Only the API test path delivers. |
| AgentDB databases are monitored | **Not in the fleet.** No collector/analyzer; "monitoring" = agent self-ping + self-reported cost. Reconciler exists but **nothing schedules it**. |
| Terraform provisioning | Rendered + policy-scanned but **never executed**; only the SDK/REST runners run. |
| Maintenance windows as `HH:MM-HH:MM` | Parser understands only cron + `"always"`; range strings are silently never-in-window. |

## 4. Safety model (what genuinely protects the database)

- **Emergency stop** checked at RunCycle start, in `ShouldExecute`, and all manual paths; fails
  closed on DB error (hardened 2026-06-10). *Gap:* not re-checked mid-cycle nor by the
  auto-rollback goroutine before firing rollback DDL.
- **SQL whitelist** `ValidateExecutorSQL` + `RejectMultiStatement` (anti-stacked-query).
- **CONCURRENTLY/VACUUM/ALTER SYSTEM** run outside transactions on dedicated conns; statement/
  lock/ddl timeouts set and reset; lock failure (55P03) circuit-breaks the table.
- **Auto-rollback** goroutine reverses regressions after `RollbackWindowMinutes`; 7-day hysteresis.
- **Concurrency caps:** `ddlSem`=3, process-wide ANALYZE semaphore + table-size cap.
- **Secrets** encrypted AES-256-GCM (argon2id KDF) at rest in `sage.databases.password_enc`.
- *Gaps:* fleet executors call `RunCycle(ctx, false)` with **no replica gating**; regression
  detection uses coarse *global* metrics (masks per-table regressions).

## 5. Data model (38 `sage.*` tables)

Core (22): `findings`, `action_log`, `action_queue`, `snapshots`, `config`, `config_audit`,
`databases`, `users`, `sessions`, `incidents`, `cases`(projection), `explain_cache`,
`explain_results`, `query_hints`, `briefings`, `alert_log`, `notification_channels/rules/log`,
`size_history`, `health_history`, `schema_findings`(legacy), `crypto_meta`. AgentDB (16):
`agent_db_*` (instances, leases, costs, budgets, tokens, blueprints, templates, deploy_requests,
audit, …). See section 7 for the full reference and the ~25 config blocks.

## 6. Tech stack & conventions

Go 1.24, `pgx/v5` + `pgxpool` (no `database/sql`, no ORM), `yaml.v3` + env + `sage.config`
runtime overrides, `log/slog`, `fsnotify` hot-reload, stdlib `testing` (no testify), React 19 +
Vite + Tailwind v4 + Recharts, `go:embed` for the dashboard, goreleaser. Prometheus text metrics
on `:9187` (hand-rendered, no client_golang); REST + SPA on `:8080` (`:8085` in the local config).

---

*Generated by reverse-engineering the source on 2026-06-10. Section files contain file:line
citations for every claim above.*
