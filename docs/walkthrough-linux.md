# pg_sage Walkthrough -- Linux / macOS

This walkthrough mirrors the v1 product shape: pg_sage starts as a single
sidecar binary, creates an authenticated admin account, and presents an
autonomous DBA workflow in the web UI.

## Prerequisites

- PostgreSQL 14-18, or Docker
- Go 1.26+
- Node.js 22+ and npm
- `curl`

## 1. Start a Local Target

```bash
docker run -d --name pg-test -e POSTGRES_PASSWORD=testpass \
  -p 5432:5432 postgres:17 \
  -c shared_preload_libraries=pg_stat_statements \
  -c pg_stat_statements.track=all

until docker exec pg-test pg_isready -U postgres; do sleep 1; done
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
docker exec -i pg-test psql -U postgres -v ON_ERROR_STOP=1 < sage_role.sql
```

With a local PostgreSQL install instead of Docker, run
`psql -U postgres -d postgres -v ON_ERROR_STOP=1 -f sage_role.sql`.

pg_sage starts read-only. The privileges that changes need (table ownership,
CREATE on the schemas that hold your tables, `pg_signal_backend`, parameter changes) come
later, from the **"Grant more"** step on the landing page.

## 2. Build and Run pg_sage

```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage/sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar

./pg_sage --pg-url \
  "postgres://sage_agent:YOUR_PASSWORD@localhost:5432/postgres?sslmode=disable"
```

If you chose another password in `sage_role.sql`, put the same one in place of
`YOUR_PASSWORD` here.

Leave pg_sage running. Copy the one-time `INITIAL ADMIN PASSWORD` printed to
stderr.

## 3. Open the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the startup password

Walk through:

1. **Minute 1: Getting started and First look** -- the landing page opens with the
   **"Getting started"** checklist and the **"First look"** panel: what a DBA would
   flag on day one, each finding with its catalog evidence.
2. **Overview** -- fleet health, Provider Readiness, recent recommendations.
3. **Cases** -- the primary DBA work queue. The old `#/findings` route aliases
   here for compatibility.
4. **Workload** -- the top statements of the last hour by load (execution seconds per
   second), calls, mean time or rows, and the active sessions by wait class, sampled
   each collector cycle. Open a statement for its history, plan changes, latest plan
   and the cases about it; a case's evidence links to the same page.
5. **Actions** -- pending approval and executed action history.
6. **Fleet** -- managed database connections and runtime status.
7. **Settings** -- system info, emergency stop/resume, and Shadow Mode (shadow decisions per class).

## 4. API and Metrics Checks

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/shadow-decisions
curl -b cookies.txt 'http://localhost:8080/api/v1/queries/top?database=all&limit=5'
curl -s http://localhost:8187/metrics | head
```

## 5. Clean Up

Stop pg_sage with `Ctrl+C`.

```bash
docker rm -f pg-test
```
