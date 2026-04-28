# Try pg_sage v1 Locally

This smoke path gets pg_sage running against a local PostgreSQL target, logs in
to the authenticated UI, and verifies the v1 autonomous DBA workflow:
Overview, Cases, Actions, Fleet, Settings, and Shadow Mode.

## Prerequisites

| Tool | Why |
|---|---|
| Docker or a local PostgreSQL 14+ | Target database |
| Go 1.24+ | Build the sidecar from source |
| Node.js 20+ and npm | Build the embedded React UI |
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

Create the monitoring user and extension:

```bash
PGPASSWORD=demopw psql -h localhost -U postgres -d postgres <<'SQL'
CREATE USER sage_agent WITH PASSWORD 'sagepw';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
```

## 2. Build pg_sage

From the repository root:

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
```

## 3. Start pg_sage

```bash
./pg_sage --pg-url "postgres://sage_agent:sagepw@localhost:5432/postgres?sslmode=disable"
```

Keep this terminal open. On first startup, pg_sage creates the first admin and
prints a one-time password to stderr:

```text
first admin created — email: admin@pg-sage.local  password: [redacted, see stderr]
*** INITIAL ADMIN PASSWORD: <copy this value> ***
```

## 4. Log In and Walk the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the `INITIAL ADMIN PASSWORD` from startup stderr

Verify these screens:

| Check | Expected result |
|---|---|
| Overview | Fleet summary, database tiles, Provider Readiness, recent recommendations |
| Cases | Ranked DBA cases with source filters, state, why-now text, policy, lifecycle, and next action |
| Legacy routes | `#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and `#/incidents` open Cases with the relevant context |
| Migration safety | DDL cases show lock/rewrite/live-risk preflight, migration SQL, rollback or forward-fix guidance, verification SQL, and PR/CI output |
| Incident playbooks | Incident cases show read-only diagnostics or reviewed scripts for locks, runaway queries, connection pressure, WAL/replication risk, standby conflicts, vacuum pressure, and sequence exhaustion |
| Vacuum autopilot | Bloat/freeze cases show guarded `VACUUM`, concurrent reindex, bloat-remediation plans, freeze-blocker diagnostics, or autovacuum tuning scripts with verification plans |
| Query tuning | Query-hint cases can show rewrite artifacts, broken-hint retirement, `CREATE STATISTICS`, parameterization plans, and reviewed role-level `work_mem` promotion |
| Provider readiness | Overview shows provider-specific extension paths, log access, limitations, and action-family readiness |
| Actions | Executed and Pending Approval tabs; expanded rows show SQL, expiration, rollback, verification state, and generated script output for DDL |
| Fleet | Managed databases table and connection-test/edit controls |
| Settings | System info, emergency controls, Shadow Mode report |

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
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
```

Prometheus remains unauthenticated on the metrics listener:

```bash
curl -s http://localhost:9187/metrics | head
```

## 6. Clean Up

Stop pg_sage with `Ctrl+C`, then remove the demo database if you used Docker:

```bash
docker rm -f pg-sage-demo
```
