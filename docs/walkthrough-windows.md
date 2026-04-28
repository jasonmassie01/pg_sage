# pg_sage Walkthrough -- Windows

Use PowerShell or Git Bash. PowerShell itself works fine for pg_sage; if a
terminal bridge or editor integration misbehaves, restart that tool rather than
changing pg_sage setup.

## Prerequisites

- Windows 11
- Docker Desktop, or a local PostgreSQL 14-17 install
- Go 1.24+
- Node.js 20+ and npm
- `psql` client

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

Create the monitoring user:

```powershell
psql -h localhost -U postgres -d postgres -c "CREATE USER sage_agent WITH PASSWORD 'sagepw'; GRANT pg_monitor TO sage_agent; GRANT pg_read_all_stats TO sage_agent; GRANT CREATE ON SCHEMA public TO sage_agent; GRANT pg_signal_backend TO sage_agent; CREATE EXTENSION IF NOT EXISTS pg_stat_statements;"
```

## 2. Build and Run pg_sage

```powershell
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage\sidecar
cd web
npm ci
npm run build
cd ..
go build -o pg_sage.exe .\cmd\pg_sage_sidecar

.\pg_sage.exe --pg-url "postgres://sage_agent:sagepw@localhost:5432/postgres?sslmode=disable"
```

Leave pg_sage running. Copy the one-time `INITIAL ADMIN PASSWORD` printed to
stderr.

## 3. Open the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the startup password

Verify:

1. **Overview** -- database tiles and Provider Readiness.
2. **Cases** -- ranked work queue. `#/findings` aliases to Cases.
3. **Actions** -- pending approval and executed action tabs.
4. **Fleet** -- database connection management.
5. **Settings** -- emergency controls and Shadow Mode report.

## 4. API and Metrics Checks

```powershell
curl.exe -c cookies.txt -H "Content-Type: application/json" `
  -X POST http://localhost:8080/api/v1/auth/login `
  --data "{\"email\":\"admin@pg-sage.local\",\"password\":\"INITIAL_PASSWORD\"}"

curl.exe -b cookies.txt http://localhost:8080/api/v1/cases
curl.exe -b cookies.txt http://localhost:8080/api/v1/shadow-report
curl.exe http://localhost:9187/metrics
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
  `9187`.
- Use `curl.exe` in PowerShell when you need the real curl binary instead of
  PowerShell aliases.
