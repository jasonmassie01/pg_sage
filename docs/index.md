# pg_sage Documentation

Agentic Postgres DBA — a Go sidecar that monitors, analyzes, and optimizes PostgreSQL 14+ databases with trust-ramped safety controls.

---

## Getting Started

- [Installation](installation.md) — Download, prerequisites, database user setup
- [Try It Out](try-it-out.md) — 20-minute hands-on tutorial
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
