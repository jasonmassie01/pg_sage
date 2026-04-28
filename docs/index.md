# pg_sage Documentation

Agentic Postgres DBA — a Go sidecar that monitors, analyzes, and optimizes
PostgreSQL 14+ databases with trust-ramped safety controls.

v1 centers the product around an autonomous DBA workflow instead of a
traditional observability dashboard:

- **Overview** shows fleet health, provider readiness, and agent state.
- **Cases** is the primary work queue for findings, incidents, and proposed
  actions. Schema-health, forecast, query-hint, and incident routes open Cases
  with the matching source filter selected; the legacy `#/findings` route
  aliases here.
- **Actions** shows pending approval and executed action history.
- **Fleet** manages database connections and per-database runtime state.
- **Settings** includes system configuration, emergency controls, and the
  Shadow Mode report proving what auto-safe policy would have handled.

The web UI and JSON API are authenticated by default. On first start, pg_sage
prints a one-time password for `admin@pg-sage.local`; API clients authenticate
through `/api/v1/auth/login` and reuse the `sage_session` cookie.

Migration-safety cases are non-executing by default for high-risk DDL. pg_sage
generates deterministic lock/rewrite and live-risk preflight evidence,
migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI
metadata so teams can review schema work before anything touches production.

Incident and maintenance cases now carry richer automation candidates. Lock
blockers, runaway queries, connection exhaustion, standby conflicts,
WAL/replication pressure, autovacuum pressure, and sequence exhaustion route to
typed playbooks with safe diagnostics or reviewed scripts. Vacuum, bloat, and
freeze findings project guarded `VACUUM`, concurrent reindex, bloat-remediation
plans, freeze diagnostics, and per-table autovacuum tuning actions with
explicit verification plans and IO-saturation gates.

Query tuning cases are no longer limited to planner hints. pg_sage can turn
suggested rewrites into PR-ready artifacts, retire broken hints through a safe
metadata action, recommend `CREATE STATISTICS`, draft parameterization changes,
and promote repeated per-query `work_mem` hints into reviewed role-level
settings when the blast radius is explicit.

Provider readiness is adapter-driven for self-managed Postgres, Cloud SQL,
AlloyDB, RDS, and Aurora. The matrix shows extension enablement paths, log
access, provider limitations, and action-family readiness so automation policy
can differ by managed-service constraints.

---

## Getting Started

- [Installation](installation.md) — Download, prerequisites, database user setup
- [Try It Out](try-it-out.md) — local v1 smoke path and UI checklist
- [Walkthroughs](walkthrough.md) — Platform-specific getting started guides
  - [Linux / macOS](walkthrough-linux.md)
  - [Windows](walkthrough-windows.md)
  - [Fleet Mode](walkthrough-fleet.md)

## Reference

- [Architecture](architecture.md) — Tier 1 rules engine, Tier 2 LLM analysis, Tier 3 executor
- [Configuration](configuration.md) — YAML config, environment variables, runtime overrides
- [Finding Types](findings.md) — All diagnostic findings and severity levels
- [SQL Schema](sql-reference.md) — `sage.*` tables, views, and functions
- [LLM Costs & Budgets](llm-costing.md) — Token usage, model routing, per-database budgets

## Operations

- [Deployment](deployment.md) — Docker, systemd, Kubernetes
- [Security](security.md) — Least-privilege roles, network policies, audit logging
