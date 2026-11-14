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
-- Sequence runway: lets pg_sage read each sequence's current value. List every
-- schema holding sequences; "Grant more" names any it still cannot read.
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO sage_agent;
-- Query statistics. Optional: without it the first look tells you how to enable it.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
```

You can also create `pg_stat_statements` after pg_sage is running: the collector checks for
it every cycle and starts reading query statistics (with WAL, plan time and block I/O
columns where the installed version has them) on the next cycle, with no restart of
pg_sage. The same check follows a major-version upgrade behind the same endpoint.

That is all a read-only install needs. The privileges that changes need (table ownership,
CREATE on the schemas that hold your tables, `pg_signal_backend`, parameter changes) come
later, from the **"Grant more"** step.

## 2. Start pg_sage

With Docker, in one line:

```bash
docker run -d --name pg_sage -p 8080:8080 -p 8187:8187 -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@db.example.com:5432/mydb" ghcr.io/jasonmassie01/pg_sage:latest --prom-addr 0.0.0.0:8187
```

Prometheus metrics listen on `127.0.0.1:8187` unless `--prom-addr` (or
`prometheus.listen_addr`) says otherwise; the endpoint has no authentication, so drop
`-p 8187:8187` and `--prom-addr` when nothing outside the container scrapes it.

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

- invalid indexes left by a failed concurrent build. An index still being built (seen in
  `pg_stat_progress_create_index`) is shown as information with no SQL. A `_ccnew` or
  `_ccold` copy left by a failed `REINDEX CONCURRENTLY` gets `DROP INDEX CONCURRENTLY`.
  A failed unique index gets a read-only query for its duplicate keys, to clean up
  before any rebuild;
- duplicate indexes, and redundant ones whose columns lead another index;
- indexes never scanned. The caveat states the statistics window: since the last reset or
  the server start. Scans on replicas are not counted;
- foreign keys whose columns lead no index;
- on partitioned tables, the index on the parent stands for its partitions. It is
  "never scanned" only when its partitions' indexes have no scans in total; a parent
  with no partitions is not reported. A duplicate or redundant parent is reported once,
  not once per partition, and the indexes attached to it are never reported on their
  own. PostgreSQL cannot drop a partitioned index concurrently, so the SQL is
  `DROP INDEX` and the caveat states the lock it takes on every partition. A parent
  index created `ON ONLY` stays invalid until every partition has an index attached; the
  finding names those partitions and gives `CREATE INDEX CONCURRENTLY` plus
  `ALTER INDEX ... ATTACH PARTITION` for each, not a `REINDEX`;
- transaction ID and multixact runway, and sequence runway, including a bigint sequence
  that feeds an integer column;
- tables of at least 100 MB with a high share of dead tuples. This is an estimate from
  the counters;
- test-named schemas. Idle ones become fact proposals that you confirm or reject;
  pg_sage never drops a schema;
- pg_stat_statements, HypoPG and auto_explain, if missing, with the exact steps for your
  provider (RDS, Aurora, Cloud SQL, AlloyDB, Azure or self-managed).

The first look reads only the system catalog and the statistics views. It runs when the
database's runtime starts and again when you re-check it, each time in one read-only
transaction. It never touches table data, and gives every statement 5 s, or
less if you set a lower `safety.query_timeout_ms` or a `statement_timeout` on pg_sage's
role. A check it cannot run (a missing privilege, a timeout) is shown as degraded, with
the reason and the grant that fixes it; one that timed out is retried once 30 s later
(2 s later on a re-check).
Those checks appear under **Catalog checks**, below the findings, with any grant SQL in a
code block; a first look with degraded checks says how many could not run instead of "No
problems found", and the checklist's "First look ready" step shows the same count.
With a model configured (`llm.enabled`, `llm.endpoint`, `llm.api_key`), a short summary
heads the list. The findings never depend on it. A first look that finds the same items
(rule, object and severity) and the same degraded checks as the one before it reuses that
summary, so a restart with nothing changed makes no model call.

**Re-check after you follow the steps.** After `CREATE EXTENSION pg_stat_statements`, a
`GRANT` from a degraded check or a `shared_preload_libraries` change, press **Re-check** on
the "Extensions" or "First look ready" step, or on the first look itself. pg_sage takes the
first look again (the same catalog reads, under a 45 s budget), and the checklist updates
with no pg_sage restart. Operators and admins see the button; viewers do not. Each database
can be re-checked once a minute ("Try again in N s" until then). A re-check never changes the
time to first finding, and nothing re-checks on its own.

The same data is available from the API at `/api/v1/first-look` and `/api/v1/onboarding`.
`POST /api/v1/first-look/recheck?database=<name>` (operator or admin) re-checks one
database and returns its new `first_look` summary. It answers 400 for `all` or no database,
404 for an unknown one, 503 while its runtime has no first look, 429 with `Retry-After`
within a minute of the last re-check, and 502 with an error `class` (`unreachable`,
`read_failed`) when the first look fails; the previous report then stays the latest.
Prometheus has `pg_sage_time_to_first_finding_seconds`, `pg_sage_first_look_items`,
`pg_sage_first_look_findings` and `pg_sage_first_look_duration_seconds` (one series per
database).

**Time to first finding** is the time from pg_sage's start (or a rebuilt runtime's start)
to its first *finding* for that database: a warning or critical item of the catalog
checks, such as a duplicate index or a sequence near its limit. Setup notes (an extension
pg_sage would like, hidden query text, a server log it cannot read) and the agent posture
notes are listed in the first look but are not findings, at any severity. When the first
look has no findings, the time is set by the analyzer's first open warning or critical
finding instead; an open info note does not set it. A time recorded before an upgrade is
kept as it was measured and is not recomputed.
`pg_sage_first_look_items` counts every item and `pg_sage_first_look_findings` only the
findings. The checklist says the same, for example "3 findings (1 critical), 2 setup notes,
1 posture note from the catalog.", and `/api/v1/onboarding` gives the counts in
`first_look` (`items`, `findings`, `critical`, `setup`, `posture`, `notes`).

In a fleet, a database pg_sage cannot read does not hide the others. Getting started and
First look name it ("pg_sage cannot reach the database ...") and show every other database
in full. Both endpoints mark it with an `error` field on its entry: `unreachable` (it
refused the connection or gave no answer within 3 s), `no_runtime` (its connection failed
at startup) or `read_failed` (it answered, but reading pg_sage's state failed). The cause
is in the pg_sage log, with the database name. The checklist polls once a minute until
setup is complete, then stops.

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
also gives the SQL for any grant that is missing. The startup check in the log uses the
same checks and prints the same SQL.

- Observation lists `pg_monitor`, the `sage` schema and SELECT on sequences. The
  sequence grant names exactly the schemas whose sequences pg_sage cannot read
  (`GRANT SELECT ON ALL SEQUENCES IN SCHEMA app, billing TO sage_agent;`), an
  extension's schema such as `hint_plan` included, since the first look reads those
  sequences too. The first look's degraded sequence check prints the same statement for
  the role it ran as, and the startup log notes it at every trust level.
- The advisory and autonomous levels lead with the least privilege. On PostgreSQL 17+
  that is `pg_maintain`, which covers VACUUM, ANALYZE, REINDEX and CLUSTER on every
  table without owning it.
- Table ownership comes next, for what only an owner may do (CREATE and DROP INDEX,
  ALTER TABLE storage settings). The guide lists each role that owns tables pg_sage
  cannot act on, with its table count, up to 20 roles, and gives `GRANT <role> TO
  sage_agent;` for each. Tables that belong to an extension are not counted.
  Membership lets pg_sage do anything that owner can, DROP and TRUNCATE included; only
  its typed actions and the policy gate stop that. A superuser owner is never granted,
  since that would make pg_sage a superuser: the guide advises reassigning those tables
  (`ALTER TABLE ... OWNER TO`, or `REASSIGN OWNED BY`) to an ordinary owner role and
  granting that. Do the same where your provider refuses membership in an owner role
  its admin role holds (RDS, Cloud SQL).
- CREATE on the schemas holding your tables, then `pg_signal_backend`. A role that is a
  member of `pg_signal_backend` but does not inherit its privileges cannot cancel
  queries; the guide gives `GRANT pg_signal_backend TO sage_agent WITH INHERIT TRUE;`
  (PostgreSQL 16+) or `ALTER ROLE sage_agent INHERIT;` for it.

In a fleet, each database is checked on its own. Choosing a level sets `trust.level`
through the same settings API the Settings page uses (`/api/v1/config/global`). In a YAML
fleet it gives the line to put in your file instead. Even at a higher level, every action
still passes the policy gate, waits for its ramp, `trust.tier3_safe` and
`trust.tier3_moderate`, and is verified afterwards. High-risk actions always need
approval.

On the Settings page, raising the trust level asks you to confirm first. Raising it to
autonomous asks you to type the database name, or the word `autonomous` when Settings
covers the whole fleet. Lowering the level applies at once, with no dialog.

Already running pg_sage? An upgraded install keeps its configured trust level. A level
set in your configuration file or with `SAGE_TRUST_LEVEL` is your grant: pg_sage honors
it from the first start.

## When something is missing

| Situation | What happens |
|---|---|
| pg_stat_statements not installed or not loaded | pg_sage still starts; the first look gives the steps; query analysis waits |
| Sequences not readable | the sequence check is degraded and names the `GRANT SELECT ON ALL SEQUENCES` to run |
| Query text hidden | a finding asks for `pg_read_all_stats` |
| Slow catalog | each statement stops after 5 s (or your lower limit); the check is degraded, retried once 30 s later, and the others still run |
| MCP for coding agents | MCP is on by default over HTTP; create a token under **"MCP tokens"** ([MCP guide](mcp.md)) |
