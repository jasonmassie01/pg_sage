# pg_sage Documentation

Agentic Postgres DBA — a Go sidecar that monitors, analyzes, and optimizes
PostgreSQL 14+ databases with trust-ramped safety controls.

v0.9 centers the product around an autonomous DBA workflow instead of a
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
generates deterministic preflight evidence, migration SQL, rollback or
forward-fix guidance, verification SQL, and PR/CI metadata so teams can review
schema work before anything touches production.

---

## Getting Started

- [Installation](installation.md) — Download, prerequisites, database user setup
- [Try It Out](try-it-out.md) — local v0.9 smoke path and UI checklist
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
