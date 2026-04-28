# pg_sage Fleet Walkthrough

Run one pg_sage sidecar against multiple PostgreSQL databases and verify the
v1 autonomous DBA workflow.

## Prerequisites

- Docker or Docker Desktop
- Go 1.24+
- Node.js 20+ and npm
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

## 5. Verify the v1 UI

| Screen | What to verify |
|---|---|
| Overview | Two database tiles, Provider Readiness matrix, fleet summary |
| Cases | Cases from both databases, sorted by urgency/actionability |
| Actions | Pending Approval and Executed tabs |
| Fleet | Both managed databases and their status |
| Settings | Emergency controls and Shadow Mode report |

The old `#/findings` route remains a compatibility alias for **Cases**.

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
