# pg_sage Walkthrough -- Windows

Use PowerShell or Git Bash. PowerShell itself works fine for pg_sage; if a
terminal bridge or editor integration misbehaves, restart that tool rather than
changing pg_sage setup.

## Prerequisites

- Windows 11
- Docker Desktop, or a local PostgreSQL 14-18 install
- Go 1.26+
- Node.js 22+ and npm
- `psql` client, only for a local PostgreSQL install (the Docker path runs `psql`
  inside the container)

## 1. Start a Local Target

```powershell
docker run -d --name pg-test -e POSTGRES_PASSWORD=testpass `
  -p 5432:5432 postgres:17 `
  -c shared_preload_libraries=pg_stat_statements `
  -c pg_stat_statements.track=all
```

Wait for readiness:

```powershell
docker exec pg-test pg_isready -U postgres
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

```powershell
Get-Content sage_role.sql | docker exec -i pg-test psql -U postgres -v ON_ERROR_STOP=1
```

With a local PostgreSQL install instead of Docker, run
`psql -U postgres -d postgres -v ON_ERROR_STOP=1 -f sage_role.sql`.

pg_sage starts read-only. The privileges that changes need (table ownership,
CREATE on the schemas that hold your tables, `pg_signal_backend`, parameter changes) come
later, from the **"Grant more"** step on the landing page.

## 2. Build and Run pg_sage

Without a C compiler (for example MSYS2 MinGW gcc) this build leaves out
parse-tree SQL validation, and unattended changes then wait for approval; see [Installation](installation.md#build-from-source).

```powershell
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage\sidecar
cd web
npm ci
npm run build
cd ..
go build -o pg_sage.exe .\cmd\pg_sage_sidecar

.\pg_sage.exe --pg-url "postgres://sage_agent:YOUR_PASSWORD@localhost:5432/postgres?sslmode=disable"
```

If you chose another password in `sage_role.sql`, put the same one in place of
`YOUR_PASSWORD` here.

Leave pg_sage running. Copy the one-time `INITIAL ADMIN PASSWORD` printed to
stderr.

## 3. Open the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the startup password

Verify:

1. **Minute 1: Getting started and First look** -- the landing page opens with the
   **"Getting started"** checklist and the **"First look"** panel: what a DBA would
   flag on day one, each finding with its catalog evidence.
2. **Overview** -- database tiles and Provider Readiness.
3. **Cases** -- ranked work queue. `#/findings` aliases to Cases.
4. **Workload** -- top statements by load and the wait-class load chart; each statement
   opens its history, plan changes and related cases.
5. **Actions** -- pending approval and executed action tabs.
6. **Fleet** -- database connection management.
7. **Settings** -- emergency controls and Shadow Mode (shadow decisions per class).

## 4. API and Metrics Checks

```powershell
curl.exe -c cookies.txt -H "Content-Type: application/json" `
  -X POST http://localhost:8080/api/v1/auth/login `
  --data "{\"email\":\"admin@pg-sage.local\",\"password\":\"INITIAL_PASSWORD\"}"

curl.exe -b cookies.txt http://localhost:8080/api/v1/cases
curl.exe -b cookies.txt http://localhost:8080/api/v1/shadow-decisions
curl.exe -b cookies.txt "http://localhost:8080/api/v1/queries/top?database=all&limit=5"
curl.exe http://localhost:8187/metrics
```

## 5. Clean Up

Stop pg_sage with `Ctrl+C`.

```powershell
docker rm -f pg-test
```

## Windows Notes

- A local PostgreSQL install may already bind port `5432`; use it directly or
  map Docker to another port.
- Windows Firewall may prompt when pg_sage starts listening on `8080` and
  `8187`.
- Use `curl.exe` in PowerShell when you need the real curl binary instead of
  PowerShell aliases.
