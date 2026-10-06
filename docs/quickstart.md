# Five-minute quickstart

From an empty install to the first useful finding in under five minutes, on any
PostgreSQL 14+ database, managed or self-hosted, without a superuser. pg_sage reads the
catalog the moment it connects and shows a **first look** within the first minute. It
needs no query history and no LLM for that. It starts **read-only**: it only observes
until you grant more.

A test keeps this page honest. Every setting, flag, environment variable, metric, API
path and UI label named here must exist in the product (`TestQuickstart*` in
`sidecar/internal/config`). The e2e test runs the role SQL below exactly as written
(`TestFirstLookWithinAMinute`).

## 1. Create a role for pg_sage

Run this as a superuser, or as your provider's admin role (`rds_superuser`,
`cloudsqlsuperuser`, `azure_pg_admin`), in the database you want pg_sage to watch:

```sql
-- Log in with a password of your choice.
CREATE ROLE sage_agent LOGIN PASSWORD 'YOUR_PASSWORD';
-- Read statistics, settings and query text (pg_monitor includes pg_read_all_stats).
GRANT pg_monitor TO sage_agent;
-- pg_sage keeps its own state in the sage schema and writes nowhere else.
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
-- Sequence runway: lets pg_sage read each sequence's current value.
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO sage_agent;
-- Query statistics. Optional: without it the first look tells you how to enable it.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
```

That is all a read-only install needs. The privileges that changes need (table ownership,
`pg_signal_backend`, parameter changes) come later, from the **"Grant more"** step.

## 2. Start pg_sage

With Docker, in one line:

```bash
docker run -d --name pg_sage -p 8080:8080 -p 9187:9187 -e SAGE_PROMETHEUS_PORT=9187 -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@db.example.com:5432/mydb" ghcr.io/jasonmassie01/pg_sage:latest
```

Or with the binary:

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@db.example.com:5432/mydb"
```

Sign in at `http://localhost:8080` as `admin@pg-sage.local`. The one-time password is
printed to stderr: `docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'`.

Have no database at hand? [Try pg_sage locally](try-it-out.md) starts one with Docker.

## 3. What you will see

### Minute 1: the first look

The landing page opens with **"Getting started"**, a checklist with live status:

| Step | Done when |
|---|---|
| **"Connected"** | pg_sage reaches the database |
| **"Extensions"** | pg_stat_statements is ready; HypoPG and auto_explain are listed as optional |
| **"First look ready"** | the catalog-only first look has finished |
| **"MCP token"** | an active token exists for coding agents (optional) |
| **"Notifications"** | a notification channel is configured (optional) |
| **"Grant more"** | you have granted a trust level above observation |

Below it, **"First look"** lists what a DBA would flag on day one. Each finding cites the
catalog evidence it rests on, for example `pg_index.indisvalid = false`:

- invalid indexes left by a failed concurrent build;
- duplicate indexes, and redundant ones whose columns lead another index;
- indexes never scanned. The caveat states the statistics window: since the last reset or
  the server start. Scans on replicas are not counted;
- foreign keys whose columns lead no index;
- transaction ID and multixact runway, and sequence runway, including a bigint sequence
  that feeds an integer column;
- tables of at least 100 MB with a high share of dead tuples. This is an estimate from
  the counters;
- test-named schemas. Idle ones become fact proposals that you confirm or reject;
  pg_sage never drops a schema;
- pg_stat_statements, HypoPG and auto_explain, if missing, with the exact steps for your
  provider (RDS, Aurora, Cloud SQL, AlloyDB, Azure or self-managed).

The first look reads only the system catalog and the statistics views. It runs once, in
one read-only transaction, never touches table data, and keeps every statement inside
`safety.query_timeout_ms`. A check it cannot run (a missing privilege, a timeout) is shown
as degraded, with the reason and the grant that fixes it. With a model configured
(`llm.enabled`, `llm.endpoint`, `llm.api_key`), a short summary heads the list. The
findings never depend on it.

The same data is available from the API at `/api/v1/first-look` and `/api/v1/onboarding`,
and from Prometheus as `pg_sage_time_to_first_finding_seconds`,
`pg_sage_first_look_items` and `pg_sage_first_look_duration_seconds` (one series per
database).

### Minute 5: the analyzer

The collector takes its first snapshot at startup, and then every
`collector.interval_seconds` (60 by default). The analyzer runs as soon as that first
snapshot exists, and then every `analyzer.interval_seconds` (600 by default). Its findings
appear in Cases: table bloat, wraparound per table, configuration findings and, once
pg_stat_statements has a few snapshots, slow and costly queries.

### Hour 1: history and the model

Query findings sharpen as statistics accumulate. With a model configured, index
recommendations and the investigator start proposing typed, evidence-backed actions.
Below your trust level they only record what they would do. Some rules wait longer on
purpose. Unused-index drops wait for `analyzer.unused_index_window_days`, and test-schema
facts are proposed after six quiet hours unless the first look already saw a long enough
statistics window.

## 4. Read-only until you grant more

A new install starts at `trust.level` observation. pg_sage reads the catalog and the
statistics, and records findings, the first look and what it would do in its own `sage`
schema. Nothing else in your database changes.

When you are ready, click **"Grant more"** on the landing page. For each trust level it
shows what pg_sage may do, what it never does without a person's approval, how much of the
trust ramp is left, and which grants the level needs, checked live against your role. It
also gives the SQL for any grant that is missing. Choosing a level sets `trust.level`
through the same settings API the Settings page uses (`/api/v1/config/global`). In a YAML
fleet it gives the line to put in your file instead. Even at a higher level, every action
still passes the policy gate, waits for its ramp, `trust.tier3_safe` and
`trust.tier3_moderate`, and is verified afterwards. High-risk actions always need
approval.

Already running pg_sage? An upgraded install keeps its configured trust level. A level
set in your configuration file or with `SAGE_TRUST_LEVEL` is your grant: pg_sage honors
it from the first start.

## When something is missing

| Situation | What happens |
|---|---|
| pg_stat_statements not installed or not loaded | pg_sage still starts; the first look gives the steps; query analysis waits |
| Sequences not readable | the sequence check is degraded and names the `GRANT SELECT ON ALL SEQUENCES` to run |
| Query text hidden | a finding asks for `pg_read_all_stats` |
| Slow catalog | each statement stops at `safety.query_timeout_ms`; the check is degraded, the others still run |
| MCP for coding agents | MCP is on by default over HTTP; create a token under **"MCP tokens"** ([MCP guide](mcp.md)) |
