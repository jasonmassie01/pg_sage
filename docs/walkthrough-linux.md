# pg_sage Walkthrough -- Linux / macOS

Your database has been silently accumulating problems: duplicate indexes burning
write I/O, sequences about to overflow, queries doing full table scans on 100K
rows. In the next 10 minutes, pg_sage will find all of them and tell you exactly
how to fix each one.

**Time**: ~10 minutes

**Prerequisites**: A PostgreSQL 14-17 database you can connect to. If you do not
have one handy, use a local Docker instance:

```bash
docker run -d --name pg-test -e POSTGRES_PASSWORD=testpass \
  -p 5432:5432 postgres:17 \
  -c shared_preload_libraries=pg_stat_statements
```

---

## 1. Download and Start

```bash
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64 -o pg_sage
chmod +x pg_sage
```

Create the database user (connect to your database with a superuser):

```bash
psql -h localhost -U postgres -c "
  CREATE USER sage_agent WITH PASSWORD 'sagepw';
  GRANT pg_monitor TO sage_agent;
  GRANT pg_read_all_stats TO sage_agent;
  GRANT CREATE ON SCHEMA public TO sage_agent;
  GRANT pg_signal_backend TO sage_agent;
"
```

Start pg_sage:

```bash
./pg_sage --database-url "postgres://sage_agent:sagepw@localhost:5432/postgres"
```

pg_sage connects, bootstraps the `sage` schema, and starts collecting. Leave it
running in this terminal.

---

## 2. Check Findings

Open a second terminal and connect to the database:

```bash
psql -h localhost -U sage_agent -d postgres
```

Wait about 2 minutes for the first analyzer cycle, then:

```sql
SELECT category, severity, title
FROM sage.findings
WHERE status = 'open'
ORDER BY
  CASE severity WHEN 'critical' THEN 1 WHEN 'warning' THEN 2 ELSE 3 END;
```

You will see findings for any detected issues -- duplicate indexes, unused
indexes, slow queries, sequence exhaustion, cache hit ratio, etc.

Every finding comes with a fix and a rollback:

```sql
SELECT title, recommended_sql, rollback_sql
FROM sage.findings
WHERE severity = 'critical' AND status = 'open';
```

---

## 3. Enable LLM (Optional)

To get LLM-powered index optimization and briefings, set an API key:

Create a `config.yaml`:

```yaml
mode: standalone

postgres:
  host: localhost
  port: 5432
  user: sage_agent
  password: sagepw
  database: postgres

llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
  model: "gemini-2.5-flash"
  api_key: YOUR_API_KEY_HERE
  optimizer:
    enabled: true

trust:
  level: observation
```

Restart pg_sage with the config:

```bash
./pg_sage --config config.yaml
```

---

## 4. Check the Action Log

pg_sage logs every autonomous action it takes (or considers taking):

```sql
SELECT id, action_type, finding_id, outcome, executed_at
FROM sage.action_log
ORDER BY executed_at DESC
LIMIT 10;
```

In observation mode (the default), no actions are taken. Promote to `advisory`
in the config to allow safe actions like dropping duplicate indexes.

---

## 5. Prometheus Metrics

Open a browser or curl the metrics endpoint:

```bash
curl -s http://localhost:9187/metrics
```

Output includes:

```
pg_sage_findings_total{severity="critical"} 2
pg_sage_findings_total{severity="warning"} 4
pg_sage_findings_total{severity="info"} 1
pg_sage_connection_up 1
pg_sage_database_size_bytes 2.68435456e+08
```

Wire this into Grafana with the included dashboard: `grafana/pg_sage_dashboard.json`.

---

## 6. MCP Server (For AI Assistants)

pg_sage exposes the Model Context Protocol on port 8080 (configurable). Claude
Desktop, Cursor, and other MCP-compatible tools can connect.

Configure Claude Desktop (`~/.config/claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "pg_sage": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

Available MCP resources:

| Resource | Description |
|----------|-------------|
| `sage://health` | System health snapshot |
| `sage://findings` | Open findings |
| `sage://slow-queries` | Top slow queries |
| `sage://schema/{table}` | Table DDL and indexes |
| `sage://stats/{table}` | Table statistics |
| `sage://explain/{queryid}` | Cached EXPLAIN plan |

Available MCP tools: `diagnose`, `briefing`, `suggest_index`, `review_migration`,
`sage_status`, `sage_emergency_stop`, `sage_resume`.

Ask questions like:
- "What are my slowest queries?"
- "Show me duplicate indexes"
- "Why is my application slow?"

---

## 7. Clean Up

Stop pg_sage with Ctrl+C. To remove the sage schema:

```sql
DROP SCHEMA sage CASCADE;
```

If using the test Docker container:

```bash
docker rm -f pg-test
```

---

## What You Just Saw

| Feature | Tier | LLM Required? |
|---------|------|---------------|
| 18+ deterministic rules (indexes, queries, sequences, bloat, replication) | 1 | No |
| LLM index optimizer with HypoPG validation | 2 | Yes |
| Trust-gated action executor with rollback | 3 | No |
| Prometheus metrics | Core | No |
| MCP server for AI assistants | Core | No |
| Health briefings | Core | No (enhanced with LLM) |

pg_sage continuously monitors your database, catches problems before they
become outages, and -- when you are ready -- fixes them autonomously during
maintenance windows, with automatic rollback if anything regresses.
