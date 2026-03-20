# pg_sage Walkthrough — Linux / macOS

Your database has been silently accumulating problems: duplicate indexes burning
write I/O, sequences about to overflow, missing security policies, queries doing
full table scans on 100K rows. In the next 10 minutes, pg_sage will find all of
them — and explain exactly how to fix each one, in plain English.

**Time**: ~10 minutes

---

## 1. Start the Stack

```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage/pg_sage
docker compose up -d
```

Wait for healthy (~15 seconds):

```bash
docker compose ps
```

You should see two containers running: `pg_sage-pg_sage-1` (healthy) and
`pg_sage-sidecar-1` (running). The demo database ships with 100K orders,
intentionally bad indexes, and a nearly-exhausted sequence — a realistic mess
for pg_sage to find.

---

## 2. Connect

```bash
docker exec -it pg_sage-pg_sage-1 psql -U postgres
```

---

## 3. Ask Your Database What's Wrong

pg_sage has been analyzing your database since it started. Within 60 seconds of
boot, the collector and analyzer have already run. Let's skip the preamble and
go straight to the good part — ask it a question:

```sql
-- Give the AI a Gemini API key (or any OpenAI-compatible endpoint)
ALTER SYSTEM SET sage.llm_endpoint = 'https://generativelanguage.googleapis.com/v1beta/openai/chat/completions';
ALTER SYSTEM SET sage.llm_api_key = 'YOUR_API_KEY_HERE';
ALTER SYSTEM SET sage.llm_model = 'gemini-2.5-flash';
ALTER SYSTEM SET sage.llm_enabled = on;
SELECT pg_reload_conf();
```

Now ask it anything:

```sql
SELECT sage.diagnose('What are the biggest risks in my database right now?');
```

pg_sage examines its findings, queries the catalog for supporting evidence,
and returns a plain-English explanation of every critical issue — what's wrong,
why it matters, and exactly how to fix it. This uses a ReAct reasoning loop:
the AI thinks, queries, observes, and iterates up to 10 steps.

Try a targeted question:

```sql
SELECT sage.diagnose('Which indexes on the orders table are wasting resources?');
```

It identifies the duplicates, calculates the write overhead, and tells you
which one to drop — with the exact DDL.

---

## 4. See What It Found (No LLM Required)

Everything below works without an API key. The LLM enhances the output, but
the rules engine catches all the same issues.

```sql
SELECT id, severity, category, title
FROM sage.findings
WHERE status = 'open'
ORDER BY
  CASE severity WHEN 'critical' THEN 1 WHEN 'warning' THEN 2 ELSE 3 END,
  category;
```

You'll see findings like:

| severity | category | title |
|----------|----------|-------|
| critical | duplicate_index | Duplicate index on orders (customer_id) |
| critical | sequence_exhaustion | test_exhausted_seq at 93% capacity (integer) |
| critical | config | Cache hit ratio 0% |
| warning | security_missing_rls | customers table has sensitive columns but no RLS |
| warning | unused_index | Unused index on orders (zero scans in 30 days) |
| warning | slow_query | Slow query: SELECT pg_sleep(...) |

Every finding comes with a fix and a rollback:

```sql
SELECT title, recommended_sql, rollback_sql
FROM sage.findings
WHERE severity = 'critical' AND status = 'open';
```

---

## 5. Get a Health Briefing

```sql
SELECT sage.briefing();
```

A structured report: critical/warning/info counts, new findings since last
briefing, resolved issues, recent actions, and system metrics. With the LLM
enabled, this becomes a narrative summary.

---

## 6. Explore a Table

```sql
-- Full schema: columns, indexes, constraints, foreign keys
SELECT sage.schema_json('public.orders');

-- Runtime stats: size, row counts, dead tuples, vacuum status
SELECT sage.stats_json('public.orders');
```

---

## 7. Find Slow Queries

```sql
SELECT sage.slow_queries_json();
```

Returns the top 20 queries by execution time from `pg_stat_statements`, with
call counts, mean/max/min timing, and I/O stats.

---

## 8. Emergency Controls

If you ever need to halt all autonomous activity immediately:

```sql
SELECT sage.emergency_stop();
SELECT sage.status();  -- emergency_stopped = true
```

Resume when ready:

```sql
SELECT sage.resume();
SELECT sage.status();  -- emergency_stopped = false
```

---

## 9. Suppress a Known Issue

```sql
-- Suppress finding #4 for 30 days
SELECT sage.suppress(4, 'Expected on fresh demo database', 30);

-- Verify
SELECT id, title, status FROM sage.findings WHERE id = 4;
```

Suppressed findings auto-reopen when the duration expires.

---

## 10. Check the Action Log

pg_sage logs every autonomous action it takes (or considers taking):

```sql
SELECT id, action_type, category, status, executed_at
FROM sage.action_log
ORDER BY executed_at DESC
LIMIT 10;
```

In advisory mode (the default), safe actions like dropping duplicate indexes
may be executed. Each action includes `before_state`, `after_state`, and
`rollback_sql` for full auditability.

---

## 11. View Configuration

```sql
SELECT name, setting, short_desc
FROM pg_settings
WHERE name LIKE 'sage.%'
ORDER BY name;
```

Key settings to experiment with:
- `sage.trust_level` — `observation` → `advisory` → `autonomous`
- `sage.slow_query_threshold` — Lower to catch more queries (default: 1000ms)
- `sage.collector_interval` / `sage.analyzer_interval` — Collection frequency
- `sage.llm_features` — Enable specific AI features: `briefing,explain,diagnostic,shell`

---

## 12. Prometheus Metrics

Open a second terminal:

```bash
curl -s http://localhost:9187/metrics
```

Output:

```
# HELP pg_sage_findings_total Number of open findings by severity
# TYPE pg_sage_findings_total gauge
pg_sage_findings_total{severity="critical"} 4
pg_sage_findings_total{severity="warning"} 6
pg_sage_findings_total{severity="info"} 1

# HELP pg_sage_circuit_breaker_state Circuit breaker state (0=closed, 1=open)
# TYPE pg_sage_circuit_breaker_state gauge
pg_sage_circuit_breaker_state{breaker="db"} 0
pg_sage_circuit_breaker_state{breaker="llm"} 0
```

Wire this into Grafana with the included dashboard:
`grafana/pg_sage_dashboard.json` (18 panels).

---

## 13. MCP Server (For AI Assistants)

The sidecar exposes pg_sage via the Model Context Protocol on port 5433.
Claude, Cursor, Copilot, and other MCP-compatible tools can connect to it.

To test manually, you need **two terminals** (the SSE session must stay open):

**Terminal 1** — keep this running:

```bash
curl -N http://localhost:5433/sse
```

You'll see: `event: endpoint` with a session ID.

**Terminal 2** — send requests using that session ID:

```bash
curl -X POST "http://localhost:5433/messages?sessionId=YOUR_SESSION_ID" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'
```

The response appears in Terminal 1. Available MCP resources:

| Resource | Description |
|----------|-------------|
| `sage://health` | System health snapshot |
| `sage://findings` | Open findings |
| `sage://slow-queries` | Top slow queries |
| `sage://schema/{table}` | Table DDL and indexes |
| `sage://stats/{table}` | Table statistics |
| `sage://explain/{queryid}` | Cached EXPLAIN plan |

---

## 14. Clean Up

```bash
docker compose down -v   # -v removes the data volume
```

---

## What You Just Saw

| Feature | Tier | LLM Required? |
|---------|------|---------------|
| Automatic finding detection (indexes, queries, config, security, sequences, vacuum) | 1 | No |
| Health briefings with severity breakdown | 1 | No (enhanced with LLM) |
| EXPLAIN plan capture and caching | 1 | No (narrated with LLM) |
| AI diagnostic with ReAct reasoning | 2 | Yes |
| Emergency stop / resume | Core | No |
| Circuit breakers (DB + LLM) | Core | No |
| Finding suppression with auto-expiry | Core | No |
| Action executor with graduated trust | 3 | No |
| Prometheus metrics | Sidecar | No |
| MCP server for AI assistants | Sidecar | No |

pg_sage continuously monitors your database, catches problems before they
become outages, and — when you're ready — fixes them autonomously during
maintenance windows, with automatic rollback if anything regresses.
