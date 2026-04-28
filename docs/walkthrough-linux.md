# pg_sage Walkthrough -- Linux / macOS

This walkthrough mirrors the v1 product shape: pg_sage starts as a single
sidecar binary, creates an authenticated admin account, and presents an
autonomous DBA workflow in the web UI.

## Prerequisites

- PostgreSQL 14-17, or Docker
- Go 1.24+
- Node.js 20+ and npm
- `curl`

## 1. Start a Local Target

```bash
docker run -d --name pg-test -e POSTGRES_PASSWORD=testpass \
  -p 5432:5432 postgres:17 \
  -c shared_preload_libraries=pg_stat_statements \
  -c pg_stat_statements.track=all

until docker exec pg-test pg_isready -U postgres; do sleep 1; done
```

Create the monitoring user:

```bash
PGPASSWORD=testpass psql -h localhost -U postgres -d postgres <<'SQL'
CREATE USER sage_agent WITH PASSWORD 'sagepw';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
```

## 2. Build and Run pg_sage

```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage/sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar

./pg_sage --pg-url \
  "postgres://sage_agent:sagepw@localhost:5432/postgres?sslmode=disable"
```

Leave pg_sage running. Copy the one-time `INITIAL ADMIN PASSWORD` printed to
stderr.

## 3. Open the UI

Open `http://localhost:8080` and sign in:

- Email: `admin@pg-sage.local`
- Password: the startup password

Walk through:

1. **Overview** -- fleet health, Provider Readiness, recent recommendations.
2. **Cases** -- the primary DBA work queue. The old `#/findings` route aliases
   here for compatibility.
3. **Actions** -- pending approval and executed action history.
4. **Fleet** -- managed database connections and runtime status.
5. **Settings** -- system info, emergency stop/resume, and Shadow Mode proof.

## 4. API and Metrics Checks

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
curl -s http://localhost:9187/metrics | head
```

## 5. Clean Up

Stop pg_sage with `Ctrl+C`.

```bash
docker rm -f pg-test
```
