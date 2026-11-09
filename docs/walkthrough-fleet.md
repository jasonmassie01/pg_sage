# pg_sage Fleet Walkthrough

Run one pg_sage sidecar against multiple PostgreSQL databases and verify the
v1 autonomous DBA workflow. [Fleet Mode](fleet.md) explains the pieces used
here: the control database, tenant boundaries, fleet learning, leader
election and the fleet routes.

## Prerequisites

- Docker or Docker Desktop
- Go 1.26+
- Node.js 22+ and npm
- `openssl`, to generate the encryption key
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

pg_sage keeps the fleet's control data (logins, the database registry, the
trust ledger, the leader lease) in a meta database. Create a dedicated one,
`sage_meta`, next to the monitored databases:

```bash
docker exec pg_sage-pg1-1 psql -U postgres -c "CREATE DATABASE sage_meta;" 2>/dev/null || true
```

Then create and seed the two monitored databases:

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

meta_db: "postgres://postgres:postgres@localhost:5433/sage_meta?sslmode=disable"

api:
  listen_addr: "0.0.0.0:8080"

prometheus:
  listen_addr: "0.0.0.0:9187"
```

`mode: fleet` with a `meta_db` runs a meta-db fleet: the databases come from
the meta database, where you add them in the next step. You could point
`meta_db` at a monitored database such as `app_production`; that is
supported, but its `sage` schema then also holds the control data and the
lease writes, and its backups carry them (see
[Sharing the control database](fleet.md#sharing-the-control-database-with-a-monitored-database)).

Database passwords are stored encrypted in the meta database, so pg_sage
needs an encryption key before you add a database. Create one and keep it:
the same key must be used on every restart.

```bash
openssl rand -hex 32 > fleet-local.key
```

Build and run:

```bash
cd sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
SAGE_ENCRYPTION_KEY_FILE=../fleet-local.key ./pg_sage --config ../fleet-local.yaml
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
that tenant's boundary; untagged databases share one. Leave both databases
untagged here so they share a boundary. Fleet learning idles until two
connected databases share one, and its first cycle runs 2 minutes after
startup. See [Tenant boundaries](fleet.md#tenant-boundaries-tenant-tags)
and [Fleet learning](fleet.md#fleet-learning).

## 5. Verify the v1 UI

| Screen | What to verify |
|---|---|
| Overview | Two database tiles, Provider Readiness matrix, fleet summary |
| Cases | Cases from both databases, sorted by urgency/actionability |
| Actions | Pending Approval and Executed tabs |
| Fleet | Both managed databases and their status |
| Header | **Stop all** and **Resume all**, the emergency controls |
| Settings | Shadow Mode report |

The old `#/findings` route remains a compatibility alias for **Cases**.

**Stop all** in the header is a fleet-wide stop. A database you add while it
holds starts stopped, and only **Resume all** lifts it. Resuming a single
database returns `409 fleet-wide stop is active: resume all first`. See
[Fleet-wide stop](security.md#fleet-wide-stop).

Each database tile shows a health score and the reasons behind it; see
[Health score](fleet.md#health-score). The **Fleet** page lists problems
open on several databases at once, 3 by default, so it stays empty with
this walkthrough's two databases; see
[Recurring across the fleet](fleet.md#recurring-across-the-fleet).

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

Fleet routes (sample responses in [Fleet Mode](fleet.md#api-routes)). With
two databases, pass `min_databases=2`: a fleet finding needs 3 databases by
default.

```bash
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/findings?min_databases=2'
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/lookalikes?database=production'
curl -b cookies.txt http://localhost:8080/api/v1/fleet/leader
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

Delete `fleet-local.key` only together with the `sage_meta` database: the
passwords stored there cannot be read without it.

Use `docker compose down -v` only when you intentionally want to delete the
local demo database volumes.
