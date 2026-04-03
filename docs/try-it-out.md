# Try pg_sage in 20 Minutes

A hands-on walkthrough that takes you from zero to a fully
monitored PostgreSQL instance with findings, a dashboard,
Prometheus metrics, and optional LLM-powered analysis.

Everything runs locally. You will use Docker for PostgreSQL,
pgbench for realistic load, and pg_sage to watch it all unfold.

---

## Prerequisites

You need four things on your machine:

| Tool | Why | Install |
|------|-----|---------|
| **Docker** | Runs PostgreSQL 17 | [docker.com](https://docker.com) |
| **psql** | Runs SQL against Postgres | Ships with PostgreSQL |
| **pgbench** | Generates realistic load | Ships with PostgreSQL |
| **pg_sage** | The star of the show | See below |

> **Running on a cloud VM (GCE, EC2, etc.)?** Everything works
> the same. Install Docker + postgresql-client:
> ```bash
> # Debian/Ubuntu
> sudo apt-get update && sudo apt-get install -y \
>   docker.io postgresql-client golang-go
> sudo usermod -aG docker $USER && newgrp docker
> ```
> Access the dashboard at `http://<external-ip>:8080`. Make
> sure your firewall allows ports 8080, 9187, and 5433.

### Get the pg_sage binary

Download a release from the
[Releases page](https://github.com/pg-sage/sidecar/releases),
or build from source:

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar/
```

---

## Step 1: Start PostgreSQL with Extensions

pg_sage works best with four PostgreSQL extensions:

| Extension | What it does | Required? |
|-----------|-------------|-----------|
| **pg_stat_statements** | Tracks query stats (calls, time, rows) | Yes |
| **HypoPG** | Creates hypothetical indexes for cost estimation | No (enhances optimizer) |
| **pg_hint_plan** | Applies per-query optimizer hints | No (enhances tuner) |
| **auto_explain** | Captures EXPLAIN plans for slow queries | No (enhances analysis) |

The demo ships a Dockerfile that pre-installs everything.
Build the image, then start the container:

```bash
cd sidecar

# Build the custom image (one-time, takes ~30s)
docker build -t pg-sage-demo:17 tests/demo/

# Start PostgreSQL with all extensions loaded
docker run -d --name pg-sage-demo \
  -e POSTGRES_PASSWORD=demopw \
  -p 5432:5432 \
  pg-sage-demo:17 \
  -c shared_preload_libraries='pg_stat_statements,pg_hint_plan,auto_explain' \
  -c pg_stat_statements.track=all \
  -c auto_explain.log_min_duration=500 \
  -c auto_explain.log_analyze=on \
  -c auto_explain.log_format=json
```

Wait for it to accept connections:

```bash
docker exec pg-sage-demo pg_isready
```

You should see `accepting connections`. If not, wait a few
seconds and try again.

> **No Docker?** A local PostgreSQL 14+ works too. Install
> the extensions via your package manager (e.g.,
> `apt install postgresql-17-hypopg postgresql-17-pg-hint-plan`)
> and add them to `shared_preload_libraries` in `postgresql.conf`.

---

## Step 2: Create the sage_agent User

pg_sage connects as a dedicated monitoring user. It never
needs superuser:

```bash
PGPASSWORD=demopw psql -h localhost -U postgres -c "
  CREATE USER sage_agent WITH PASSWORD 'sagepw';
  GRANT pg_monitor TO sage_agent;
  GRANT pg_read_all_stats TO sage_agent;
  GRANT CREATE ON SCHEMA public TO sage_agent;
  GRANT pg_signal_backend TO sage_agent;
"
```

Now enable the extensions:

```bash
PGPASSWORD=demopw psql -h localhost -U postgres -c "
  CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
  CREATE EXTENSION IF NOT EXISTS hypopg;
  CREATE EXTENSION IF NOT EXISTS pg_hint_plan;
"
```

These grants let pg_sage read stats, create its `sage.*`
schema, and (at higher trust levels) kill idle sessions.
The extensions give pg_sage the ability to test hypothetical
indexes (HypoPG), apply per-query hints (pg_hint_plan), and
capture execution plans for slow queries (auto_explain).

---

## Step 3: Seed Realistic Data

The seed script creates an e-commerce data model across
multiple schemas with deliberate problems baked in:

- ~200K rows across partitioned tables
- Duplicate indexes on `customers.email`
- Missing foreign-key indexes on `region_id` and `product_id`
- Dead tuples in `audit_log` (bloat, no vacuum)
- A nearly-exhausted sequence

```bash
PGPASSWORD=demopw psql -h localhost -U postgres \
  -f tests/integration/seed_objects.sql
```

This takes about 10 seconds. When it finishes, you have
a database full of problems for pg_sage to find.

---

## Step 4: Initialize pgbench Tables

pgbench ships with a standard TPC-B schema. At scale factor
10 this adds ~1 million rows:

```bash
PGPASSWORD=demopw pgbench -h localhost -U postgres \
  -i -s 10 postgres
```

Now you have two workloads worth of tables: the seeded
e-commerce model and the TPC-B benchmark tables.

---

## Step 5: Start pg_sage

The demo config is pre-tuned with fast collection intervals
(15s) and low thresholds so findings appear quickly:

```bash
./pg_sage_sidecar --config tests/demo/config_demo.yaml
```

Leave this running in its own terminal. You will see:

1. Startup validation (connection check, version detect)
2. Schema bootstrap (`sage.findings`, `sage.action_log`, etc.)
3. First collection cycle within 15 seconds
4. First analysis cycle within 30 seconds

> **Tip:** The demo config starts in `observation` mode.
> pg_sage will detect problems but will not change anything
> in your database yet.

---

## Step 6: Generate Load with pgbench

Open a **second terminal** and run the standard TPC-B
benchmark. This creates sustained read/write traffic:

```bash
PGPASSWORD=demopw pgbench -h localhost -U postgres \
  -c 8 -j 4 -T 300 postgres
```

Open a **third terminal** and run the custom workload that
specifically targets pg_sage rules — sequential scans on
large tables, missing-index joins, bloat-inducing updates:

```bash
PGPASSWORD=demopw pgbench -h localhost -U postgres \
  -f tests/demo/custom_workload.sql \
  -c 4 -T 300 postgres
```

Both run for 5 minutes. That is enough time for pg_sage
to collect multiple snapshots and generate findings.

---

## Step 7: Watch pg_sage React

After 1-2 minutes of load, pg_sage will have findings.
Query them directly:

```bash
PGPASSWORD=sagepw psql -h localhost -U sage_agent \
  -d postgres -c "
  SELECT category, severity, title,
         detail->>'table' AS tbl
    FROM sage.findings
   WHERE status = 'open'
   ORDER BY
     CASE severity
       WHEN 'critical' THEN 1
       WHEN 'warning'  THEN 2
       ELSE 3
     END;
"
```

You should see findings like:

| category | severity | title | tbl |
|----------|----------|-------|-----|
| index | warning | Duplicate index detected | customers |
| index | warning | Missing foreign key index | customers |
| index | info | Unused index detected | customers |
| bloat | warning | Table bloat above threshold | audit_log |
| query | warning | Sequential scan on large table | audit_log |
| query | info | Sort without index | order_items |

Every finding includes a recommended fix and a rollback:

```bash
PGPASSWORD=sagepw psql -h localhost -U sage_agent \
  -d postgres -c "
  SELECT title,
         recommended_sql,
         rollback_sql
    FROM sage.findings
   WHERE severity IN ('critical', 'warning')
     AND status = 'open'
   LIMIT 5;
"
```

pg_sage tells you *what* is wrong, *why* it matters,
*how* to fix it, and *how* to undo the fix. All without
touching your database.

---

## Step 8: Explore the Dashboard

Open your browser to:

> **http://localhost:8080**

The React dashboard gives you a visual overview:

- **Database Health** — connection count, cache hit ratio,
  transaction throughput, and active backends over time
- **Findings** — sortable, filterable list with severity
  badges; click any finding for full detail and SQL
- **Actions** — empty for now (observation mode); this is
  where executed fixes appear at higher trust levels
- **Snapshots** — time-series charts of key metrics from
  each collection cycle

Spend a minute clicking through. The dashboard updates
automatically every few seconds.

---

## Step 9: Check Prometheus Metrics

pg_sage exposes all its internal counters in Prometheus
text format:

```bash
curl -s http://localhost:9187/metrics | grep pg_sage
```

You will see gauges and counters for findings by severity,
collection duration, analyzer cycles, safety circuit
breaker state, and more. Wire this into Grafana for
production use.

---

## Step 10: See Extensions in Action

The three optional extensions are now working behind the
scenes. Here is how to verify each one:

### auto_explain — Slow Query Plans

auto_explain automatically logs EXPLAIN output for any query
exceeding 500ms. Check the PostgreSQL logs:

```bash
docker logs pg-sage-demo 2>&1 | grep -A5 '"Query Text"' | head -30
```

pg_sage reads these plans via the `sage.explains` table and
uses them to make better index recommendations.

### HypoPG — Hypothetical Index Testing

When pg_sage's optimizer suggests a new index, it tests the
idea with HypoPG first. You can try this yourself:

```bash
PGPASSWORD=demopw psql -h localhost -U postgres -c "
  -- Create a hypothetical index (not real, zero I/O)
  SELECT hypopg_create_index(
    'CREATE INDEX ON app.audit_log (action)'
  );

  -- See if the planner would use it
  EXPLAIN SELECT * FROM app.audit_log
   WHERE action = 'event_1';

  -- Clean up
  SELECT hypopg_reset();
"
```

You should see an **Index Scan using hypothetical index**
in the plan. pg_sage does this automatically — it creates
hypothetical indexes, measures the estimated cost reduction,
and only recommends indexes that show a real benefit.

### pg_hint_plan — Per-Query Optimizer Hints

When pg_sage's tuner detects a query choosing a bad plan,
it can inject hints to force a better one. Verify it works:

```bash
PGPASSWORD=demopw psql -h localhost -U postgres -c "
  -- Force a seq scan even if an index exists
  /*+ SeqScan(pgbench_accounts) */
  EXPLAIN SELECT * FROM pgbench_accounts WHERE aid = 1;

  -- Force an index scan
  /*+ IndexScan(pgbench_accounts) */
  EXPLAIN SELECT * FROM pgbench_accounts WHERE aid = 1;
"
```

The first plan uses Seq Scan, the second uses Index Scan —
pg_hint_plan overrides the planner. pg_sage writes hints
to the `hint_plan.hints` table so they apply automatically
to matching queries.

---

## Step 11: Enable LLM Analysis (Optional)

This step requires an API key for an OpenAI-compatible
provider (OpenAI, Gemini, Groq, Ollama, etc.).

Edit `tests/demo/config_demo.yaml` — uncomment the llm
block and add your key:

```yaml
llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  api_key: YOUR_GEMINI_API_KEY
  model: "gemini-2.5-flash-preview"
  timeout_seconds: 30
  token_budget_daily: 500000
  context_budget_tokens: 4096
  index_optimizer:
    enabled: true
    min_query_calls: 10
```

Save the file. pg_sage hot-reloads the LLM config —
no restart needed. Now you get:

- **LLM-powered index recommendations** that consider
  your actual query patterns and table statistics
- **Daily health briefings** summarizing overnight
  changes and emerging trends
- **Config tuning advisors** for vacuum, WAL, memory,
  and connection settings

Check the logs for LLM activity, or wait for the next
briefing cycle.

---

## Step 12: Try MCP (Optional)

pg_sage speaks the
[Model Context Protocol](https://modelcontextprotocol.io/),
so any MCP-compatible client can query your database health
conversationally.

Add this to your Claude Desktop or Cursor config:

```json
{
  "mcpServers": {
    "pg_sage": {
      "url": "http://localhost:5433/sse"
    }
  }
}
```

Then ask natural-language questions:

- *"What are my database problems?"*
- *"Show me duplicate indexes"*
- *"Which queries are doing sequential scans?"*
- *"What is the current cache hit ratio?"*

pg_sage responds with structured data from its findings,
snapshots, and analysis history.

---

## Step 13: Promote Trust Level

So far pg_sage has been watching. Now let it act.

Edit `tests/demo/config_demo.yaml`:

```yaml
trust:
  level: advisory
```

Save the file. pg_sage hot-reloads and starts executing
**safe** actions: dropping duplicate indexes, killing
sessions idle in transaction too long, and similar
low-risk fixes.

Check what it did:

```bash
PGPASSWORD=sagepw psql -h localhost -U sage_agent \
  -d postgres -c "
  SELECT action_type, finding_id, outcome,
         executed_at
    FROM sage.action_log
   ORDER BY executed_at DESC
   LIMIT 10;
"
```

Every action is logged with the exact SQL that ran, the
finding that triggered it, whether it succeeded, and the
rollback SQL to undo it.

> **Production note:** In real deployments, pg_sage
> auto-ramps trust over time: observation for 7 days,
> then advisory, then autonomous. You can also set a
> maintenance window so moderate actions (like creating
> indexes) only run during off-peak hours.

---

## Clean Up

When you are done exploring:

```bash
# Stop pgbench — Ctrl+C in its terminals
# Stop pg_sage — Ctrl+C in its terminal

# Remove the Docker container and all data
docker rm -f pg-sage-demo
```

That is it. Nothing lingers.

---

## What You Just Saw

Here is every feature you exercised, organized by tier:

| Feature | Tier | Needs LLM? |
|---------|------|------------|
| 25+ deterministic rules engine | 1 | No |
| Duplicate index detection | 1 | No |
| Missing FK index detection | 1 | No |
| Table bloat / dead tuple analysis | 1 | No |
| Sequence exhaustion warnings | 1 | No |
| Sequential scan detection | 1 | No |
| Query regression tracking | 1 | No |
| auto_explain plan capture | 1 | No |
| HypoPG hypothetical index testing | 2 | Yes |
| pg_hint_plan query tuning | 1 | No |
| Trust-gated action executor | 3 | No |
| Auto-rollback on regression | 3 | No |
| React dashboard | Core | No |
| Prometheus metrics exporter | Core | No |
| MCP server for AI clients | Core | No |
| Config hot reload | Core | No |
| LLM index optimizer | 2 | Yes |
| Health briefings | 2 | Yes |
| Config tuning advisors | 2 | Yes |

The Tier 1 rules engine and Tier 3 executor work entirely
without an LLM. You get a production-grade PostgreSQL DBA
agent with zero API costs. LLM features are additive — they
enhance analysis but are never required.

---

## Next Steps

- Read `config.example.yaml` for the full configuration
  reference with every threshold and knob documented
- Point pg_sage at a staging database and let it run in
  observation mode for a week
- Set up Prometheus + Grafana to visualize pg_sage metrics
  alongside your existing PostgreSQL monitoring
- Try fleet mode to monitor multiple databases from a
  single pg_sage instance
- Join the conversation on
  [GitHub Issues](https://github.com/pg-sage/sidecar/issues)
