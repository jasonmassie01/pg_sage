# pg_sage Fleet Walkthrough

Run one pg_sage sidecar against multiple PostgreSQL databases and verify the
v1 autonomous DBA workflow.

## Prerequisites

- Docker or Docker Desktop
- Go 1.26+
- Node.js 22+ and npm
- Ports `5433`, `5434`, `8080`, and `9187` available

## 1. Start Two PostgreSQL Targets

From the repository root:

```bash
docker compose up -d pg1 pg2
docker compose ps
```

This starts:

- `pg1` on `localhost:5433`
- `pg2` on `localhost:5434`

## 2. Seed Databases

```bash
docker exec pg_sage-pg1-1 psql -U postgres -c "CREATE DATABASE app_production;" 2>/dev/null || true
docker exec pg_sage-pg2-1 psql -U postgres -c "CREATE DATABASE app_staging;" 2>/dev/null || true

for target in "pg_sage-pg1-1 app_production" "pg_sage-pg2-1 app_staging"; do
  set -- $target
  docker exec -i "$1" psql -U postgres -d "$2" <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE TABLE IF NOT EXISTS orders (
  id serial PRIMARY KEY,
  customer_id int NOT NULL,
  status text NOT NULL,
  created_at timestamptz DEFAULT now()
);
INSERT INTO orders (customer_id, status)
SELECT (random()*1000)::int, CASE WHEN random() < 0.5 THEN 'open' ELSE 'closed' END
FROM generate_series(1, 20000);
CREATE INDEX IF NOT EXISTS idx_orders_status_a ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_status_b ON orders(status);
CREATE SEQUENCE IF NOT EXISTS ticket_seq AS integer MAXVALUE 100;
SELECT setval('ticket_seq', 95);
SQL
done
```

## 3. Start pg_sage in Fleet Mode

Create `fleet-local.yaml`:

```yaml
mode: fleet

meta_db: "postgres://postgres:postgres@localhost:5433/app_production?sslmode=disable"

api:
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

Build and run:

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
./pg_sage --config ../fleet-local.yaml
```

Copy the one-time `INITIAL ADMIN PASSWORD` from stderr.

## 4. Add Databases in the UI

Open `http://localhost:8080` and sign in as `admin@pg-sage.local`.

Go to **Fleet** and add:

| Name | Host | Port | Database | Username | Password | SSL Mode |
|---|---|---:|---|---|---|---|
| production | localhost | 5433 | app_production | postgres | postgres | disable |
| staging | localhost | 5434 | app_staging | postgres | postgres | disable |

Use **Test Connection** before saving each database.

### Tenant boundary for fleet learning

Fleet learning (look-alike databases and shared action priors) only compares
databases inside one boundary. A database tagged `tenant=<name>` belongs to
that tenant's boundary (the name is case-insensitive); untagged databases
share one boundary. This applies to databases added here (stored in the meta
database) exactly as to `databases[].tags` in YAML. A tag edit on a running
database needs no restart: an edit saved here applies at once, a change
written to `sage.databases` by other means applies at the next reconcile
pass, and the next fingerprint cycle regroups the database. At startup
pg_sage logs the count of databases per boundary, for example
`fleet-learning boundaries: shared=1 tenant:acme=2`.

## 5. Verify the v1 UI

| Screen | What to verify |
|---|---|
| Overview | Two database tiles, Provider Readiness matrix, fleet summary |
| Cases | Cases from both databases, sorted by urgency/actionability |
| Actions | Pending Approval and Executed tabs |
| Fleet | Both managed databases and their status |
| Settings | Emergency controls and Shadow Mode report |

The old `#/findings` route remains a compatibility alias for **Cases**.

**Stop all** in the header is a fleet-wide stop. A database you add while it
holds starts stopped, and only **Resume all** lifts it. Resuming a single
database returns `409 fleet-wide stop is active: resume all first`. See
[Fleet-wide stop](security.md#fleet-wide-stop).

### Health score

Each database tile shows a health score from 0 to 100 and the reasons
behind it (hover the score, or read the list under it). The same score and
reasons are in `status.health_score` and `status.health_reasons` of
`GET /api/v1/databases` and `GET /api/v1/metrics?database=<name>`.

| Condition | Effect on the score | Reason shown |
|---|---|---|
| Not connected | 0 | `disconnected: <error>` |
| Connected with an error | 0 | `error: <error>` |
| Each critical finding | minus 25 | `2 critical findings` |
| Each warning | minus 5 | `5 warnings` |
| No collector sample for more than 3 collector intervals | capped at 25 | `collector stale 42m` |
| Emergency stop active | none | `emergency stopped by alice` |

The interval is the database's `collector_interval_seconds`, else the
global `collector.interval_seconds`. A database that has never been sampled
counts as stale only once it has been registered for more than 3
intervals, so a new database is not penalised while its first sample runs.
The score never goes below 0.

A database counts as **healthy** (`summary.healthy` and the
`pg_sage_fleet_healthy` gauge) when it is connected, has no error and
scores at least 50; every other database counts as degraded. Databases are
listed worst first, and by name among equal scores, so the tiles and the
`pg_sage_fleet_instance_health` series keep a stable order. The rule is the
same in every mode and on every provider.

## 6. API Checks

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/fleet/readiness
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/actions/pending
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
```

Metrics:

```bash
curl -s http://localhost:9187/metrics | head
```

## 7. Clean Up

Stop pg_sage with `Ctrl+C`.

```bash
docker compose stop pg1 pg2
```

Use `docker compose down -v` only when you intentionally want to delete the
local demo database volumes.
