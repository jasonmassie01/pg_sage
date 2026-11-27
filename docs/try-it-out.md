# Try pg_sage v1 Locally

This smoke path gets pg_sage running against a local PostgreSQL target, logs in
to the authenticated UI, and verifies the v1 autonomous DBA workflow:
Getting started and First look, Overview, Cases, Actions, Fleet, Settings, and
Shadow Mode.

## Prerequisites

| Tool | Why |
|---|---|
| Docker or a local PostgreSQL 14+ | Target database |
| Go 1.26+ | Build the sidecar from source |
| Node.js 22+ and npm | Build the embedded React UI |
| curl | API smoke checks |

## 1. Start PostgreSQL

```bash
docker run -d --name pg-sage-demo \
  -e POSTGRES_PASSWORD=demopw \
  -p 5432:5432 postgres:17 \
  -c shared_preload_libraries=pg_stat_statements \
  -c pg_stat_statements.track=all

until docker exec pg-sage-demo pg_isready -U postgres; do sleep 1; done
```

Create pg_sage's role. This is the read-only block from the
[quickstart](quickstart.md#1-create-a-role-for-pg_sage); on a managed database you run
it as your provider's admin role (`rds_superuser`, `cloudsqlsuperuser`,
`azure_pg_admin`) instead. Save it as `sage_role.sql`:

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

Run it as the `postgres` superuser inside the container:

```bash
docker exec -i pg-sage-demo psql -U postgres -v ON_ERROR_STOP=1 < sage_role.sql
```

With a local PostgreSQL install instead of Docker, run
`psql -U postgres -d postgres -v ON_ERROR_STOP=1 -f sage_role.sql`.

pg_sage starts read-only. The privileges that changes need (table ownership,
CREATE on the schemas that hold your tables, `pg_signal_backend`, parameter changes) come
later, from the **"Grant more"** step on the landing page.

## 2. Build pg_sage

From the repository root:

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
```

## 3. Start pg_sage

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@localhost:5432/postgres?sslmode=disable"
```

If you chose another password in `sage_role.sql`, put the same one in place of
`YOUR_PASSWORD` here.

Keep this terminal open. On first startup, pg_sage creates the first admin and
prints a one-time password to stderr:

```text
first admin created — email: admin@pg-sage.local  password: [redacted, see stderr]
*** INITIAL ADMIN PASSWORD: <copy this value> ***
```

The first sign-in with it asks you to choose your own password (12 to 72
bytes) before the dashboard opens.

## 4. Log In and Walk the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the `INITIAL ADMIN PASSWORD` from startup stderr

Verify these screens:

| Check | Expected result |
|---|---|
| Minute 1: Getting started and First look | The landing page opens with the **"Getting started"** checklist (Connected, Extensions, First look ready) and the **"First look"** panel: what a DBA would flag on day one, each finding with the catalog evidence it rests on |
| Overview | Fleet summary, database tiles, Provider Readiness, recent recommendations |
| Cases | Ranked DBA cases with source filters, state, why-now text, policy, lifecycle, and next action |
| Legacy routes | `#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and `#/incidents` open Cases with the relevant context |
| Migration safety | DDL cases show lock/rewrite/live-risk preflight, migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI output |
| Incident playbooks | Incident and finding cases show read-only diagnostics or reviewed scripts for locks, runaway queries, connection pressure, WAL/replication risk, standby conflicts, vacuum pressure, freeze blockers, and sequence exhaustion |
| Vacuum autopilot | Bloat/freeze cases show guarded `VACUUM`, concurrent reindex, bloat-remediation plans, freeze-blocker diagnostics, or autovacuum tuning scripts with verification plans |
| Query tuning | Query-hint cases can show rewrite artifacts, broken-hint retirement, `CREATE STATISTICS`, parameterization plans, and reviewed role-level `work_mem` promotion |
| Provider readiness | Overview shows provider-specific extension paths, log access, limitations, and action-family readiness |
| Actions | Executed and Pending Approval tabs; expanded rows show SQL, expiration, rollback, verification state, and generated script output for DDL |
| Fleet | Managed databases table and connection-test/edit controls |
| Settings | System info, emergency controls, Shadow Mode (shadow decisions per class) |

## 5. API Smoke Checks

All `/api/v1/*` endpoints require the login cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/actions/pending
curl -b cookies.txt http://localhost:8080/api/v1/fleet/readiness
curl -b cookies.txt http://localhost:8080/api/v1/shadow-decisions
```

Prometheus remains unauthenticated on the metrics listener:

```bash
curl -s http://localhost:8187/metrics | head
```

## 6. Clean Up

Stop pg_sage with `Ctrl+C`, then remove the demo database if you used Docker:

```bash
docker rm -f pg-sage-demo
```
