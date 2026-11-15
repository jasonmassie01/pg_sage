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
- Ports `5433`, `5434`, `8080`, and `8187` available

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
  listen_addr: "0.0.0.0:8187"
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
chmod 600 fleet-local.key
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

### Limit an operator to one database (optional)

By default every operator can approve and execute on every database. To give a
team only its own database, set `api.database_grants: true` in `config.yaml`
and restart, then on **Users** open **Databases** for that operator and grant
`operator` on `staging`. That operator can now approve on `staging` and gets
`403 database_forbidden` on `production`; with no grants defined nothing
changes. See [Per-database Authorization](security.md#per-database-authorization).

### Tenant boundary for fleet learning

Fleet learning (look-alike databases and shared action priors) only compares
databases inside one boundary. A database tagged `tenant=<name>` belongs to
that tenant's boundary; untagged databases share one. Leave both databases
untagged here so they share a boundary. Fleet learning idles until two
connected databases share one, and its first cycle runs 2 minutes after
startup. See [Tenant boundaries](fleet.md#tenant-boundaries-tenant-tags)
and [Fleet learning](fleet.md#fleet-learning).

### Several databases on one PostgreSQL instance

WAL and connection settings, and every `ALTER SYSTEM` or managed parameter
change (an RDS/Aurora parameter group, a Cloud SQL database flag), apply to
the whole instance, not to one database. When the fleet monitors several
databases of one instance, one of them owns those settings: the database
with the lowest fleet name among those whose configuration advisor is on.
Only the owner asks the model the WAL and connection questions (one model
call per interval for the instance, not one per database) and only it
proposes instance-wide changes. On the other databases:

- the WAL and connection sub-advisors do not run, and their open WAL and
  connection findings stay open as advisory guidance with
  `detail.instance_owner` naming the owner and no `recommended_sql`;
- an instance-wide change from the advisor or the tuning agent (for example
  `work_mem` or `autovacuum_*` through `ALTER SYSTEM`) is advisory with
  "instance settings are managed from database <owner>" and is never
  executed, nor turned into a managed-change proposal there.

Database-scope advice (rewrites, bloat, per-table storage parameters,
`ALTER DATABASE` settings) stays per database. When the owner is removed or
stopped the next database by name takes over on its next cycle. Its
advisory WAL and connection rows stay open until its own review of that
category runs, which then replaces them or, when it advises nothing for
them, resolves them.

The instance is identified by `pg_control_system().system_identifier`,
which needs `EXECUTE` on that function. Grant it to the monitoring role for
the exact identity:

```sql
GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO sage_agent;
```

A physical standby shares its primary's system identifier but keeps its
own settings, so it is its own instance (keyed by identifier and address)
and never defers to its primary. Without the grant (some managed services
revoke it) the connection's `host:port` identifies the instance. A proxy
such as PgBouncer fronting several servers on one port then groups them
under one owner, which only withholds advice from the others. If the identity cannot be read at all (connection error) the
database is advised on its own and pg_sage logs why. The fleet status
(`GET /api/v1/databases`, `status.capabilities.instance_owner`) shows each
database's owner, whether it is the owner, and how the instance was
identified (`system_identifier` or `address`). The dashboard shows the same on each
database tile of the fleet overview and in the "Instance settings" column
of the Provider Readiness tab: "Owns the instance settings", "Instance
settings handled by <owner>", or "Instance owner unknown" with the reason
in the tooltip. Separate
pg_sage processes that monitor the same instance are not coordinated: each
elects its own owner. Single-database mode is unchanged.

#### Cluster runways and forecasts

Some runways and forecasts describe the whole cluster, not one database:
the XID and multixact age (`forecast_wraparound_runway`, `xid` and `mxid`),
multixact member space (`forecast_multixact_member_runway`), disk use and
every replication slot (`forecast_wal_runway`), connection saturation
(`forecast_connection_saturation`, which counts every backend) and
checkpoint pressure (`forecast_checkpoint_pressure`). With several
databases of one cluster in the fleet, one database reports them: the
lowest fleet name among the databases whose runway monitor runs (for the
forecasts, among those whose forecaster runs). Its findings carry
`detail.scope = "cluster"`, `detail.owner_database` and
`detail.cluster_databases`, and only it starts the pre-incident
investigation, so one filling slot opens one finding, one case and one
investigation instead of one per database. Overdue tables, sequences, disk
growth, cache pressure and query volume stay per database.

Every database still samples the cluster series, so a new owner has the
history it needs. The owner re-claims each runway interval; when it stops,
is removed or misses two intervals, the next database by name takes over
on its next tick and reports the open runway in its own database. The
other databases close their own copies (upgrading closes the duplicates
left from earlier versions) with `detail.cluster_owner` naming the owner,
but only after the owner has reported a complete evaluation within two
intervals: an owner that fails to read its trends never makes the
cluster's runway disappear. `GET /api/v1/findings?database=<name>` for a
database that is not the owner adds the owner's open cluster findings under
`cluster_refs`, each marked `cluster_ref` and `read_only` (open the case
or act on it from the owner's database); they are not part of the page or
its `total`.

The runway monitor keys the cluster by `pg_control_system()`'s system
identifier and the postmaster start time, which the WAL runway probe
already reads (no new SQL; PostgreSQL 14-18 and managed providers alike).
A restored clone shares the identifier but not the start, so it is its own
cluster. Without the identifier (the grant above is missing) each database
reports its cluster runways itself, logged once; the forecasts then also
stay per database, since an address may front several clusters. The
forecasts key the cluster by the system identifier and the address the
database is reached at, so a restored clone or a promoted standby at
another address raises its own (one cluster reached at two addresses
raises them twice, never hides them). As for instance settings, separate
pg_sage processes on one cluster are not coordinated.

### Discover databases automatically

A meta-db fleet can find members instead of waiting for them to be added
by hand (manual, CSV and config-file enrolment are unchanged). Discovery
is off by default:

```yaml
fleet:
  discovery:
    enabled: true
    max_enrolled: 10   # live discovered members, adopted ones included; 0 = candidates only
    ttl_hours: 72      # an unadopted member is disabled at its expiry
    sources:
      - name: prod-cluster
        type: cluster          # other databases on this member's server
        member: managed-a
        owner: team-db         # required: who answers for what it finds
        reuse_credentials: true
      - name: lakebase-dev
        type: lakebase         # endpoints of every branch of the project
        project: my-project
        owner: data-platform
      - name: aws
        type: rds              # PostgreSQL instances carrying the tag
        region: us-east-1
        tag_key: pg_sage
        tag_value: discover
        owner: platform
```

Every 10 minutes each source lists at most 200 candidates. A cluster
source runs one read-only, timeout-bounded
`SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate AND
datallowconn` on the member's pool; templates and the member's own
database are left out. What a source reports is kept in
`sage.discovered_databases` (one row per source and external id, so two
sidecars on one meta database make no duplicates).

Only a cluster source with `reuse_credentials: true` enrols: the candidate
is added with the member's role, one connection, trust `observation`,
execution `approval`, the source's `owner` and an `expires_at` of now plus
`ttl_hours`. Lakebase and RDS candidates stay candidates; add them by hand
when you want them. A database that is already a member is never
enrolled twice. At most 5 candidates per source enrol each cycle; one
whose enrolment fails (a role without CONNECT, say) goes back to
`candidate` with its error and is retried after the untried ones.

A discovered member is not trusted further by being found. Until an owner
adopts it, a trigger on `sage.databases` rejects any trust-level or
execution-mode change (`PUT /api/v1/config/databases/{id}` answers 409
Conflict, the store `ErrDiscoveredTrust`); discovery
itself has no path that changes either. In the fleet page's **Discovered**
tab an admin can **Adopt** an enrolled member (its expiry is cleared and
its trust can then be raised the usual way), **Extend 24h**, or **Ignore**
a candidate. An unadopted member is disabled at `expires_at` exactly; one
its source stops reporting is disabled and marked `gone` (never on the
source's first listing in a process, a failed listing or one cut at the
cap). A failing source (an RDS 403, say) keeps its previous candidates and
shows its error on the tab. Every enrolment, adoption, extension, ignore,
expiry and gone is recorded in `sage.config_audit` under
`fleet.discovery.*`. Standalone and YAML fleets ignore the setting and log
that they do.

## 5. Verify the v1 UI

| Screen | What to verify |
|---|---|
| Overview | Two database tiles, Provider Readiness matrix, fleet summary |
| Cases | Cases from both databases, sorted by urgency/actionability |
| Actions | Pending Approval and Executed tabs |
| Fleet | Both managed databases with their runtime state (columns below) |
| Header | **Stop all** and **Resume all**, the emergency controls |
| Settings | Shadow Mode (shadow decisions per class) |

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

### Fleet page columns

Each managed database row shows its registration (**Name**, **Host**,
**Trust Level**, **Execution Mode**, and **Registration**, which says
`disabled` or the last **Test** result) and then what the running
sidecar reports for it, from `GET /api/v1/databases`:

| Column | Shows |
|---|---|
| Connection | `connected`, `disconnected` or `unknown`, plus the HA role when known; `not running` when the sidecar has no runtime for the registration |
| Findings | Open and critical findings |
| Health | The health score and its reasons |
| Effective trust | The trust level in force, marked `override` when it differs from the registered one |
| Emergency stop | `stopped by <actor> at <time>`, or `not stopped` |
| LLM budget | Tokens used today / this database's share of `llm.fleet_token_budget_daily`, marked `exhausted` when spent; `—` without a fleet budget |
| Explore | **Inspect database** opens the Database page |

A registration joins its runtime row on the database id, or on the name
when either side has no id. Databases from the config file get the same
columns. The header shows `leader` or `follower of <holder>` while fleet
learning runs leader election.

The **Database** page leads with a summary card: connection, platform and
version, health, last collector and analyzer runs, effective trust,
emergency stop, LLM budget and the database's fleet look-alikes with their
similarity ("Fleet learning is not running" when it is off). The raw
snapshot is in the collapsed **Raw snapshot** section below it. On the
**Fleet** page, each database under a recurring problem links to that
database's case on the **Cases** page.

## 6. API Checks

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/fleet/readiness
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/actions/pending
curl -b cookies.txt http://localhost:8080/api/v1/shadow-decisions
```

### The cases response in a fleet

`GET /api/v1/cases` reads every database concurrently (at most 8 at a
time), each under its own 5-second deadline. One database that is down or
slow does not hide the others:

- `unavailable` lists each database whose cases could not be read, as
  `{"database": "pg2", "error_class": "connection"}`. `error_class` is
  `timeout`, `connection` or `query`; the raw error stays in the sidecar
  log (`cases read failed for database`). A fleet request (`database=all`
  or no parameter) still answers `200` with the other databases' cases. A
  request for one database that cannot be read answers `503` with
  `error_class`. A database that could not connect when the sidecar
  started is listed as `connection` too (log line `cases read skipped
  for database without a pool`).
- `truncated` names each database whose list was cut at its cap, per
  source: `{"pg1": {"findings": true, "incidents": false, "query_hints":
  false}}`. The caps are 500 open findings (most severe first), 100 open
  incidents and 200 query hints per database.
- `?case=<case id>` also loads that one case from its database when the
  capped list does not carry it; `focus` reports `{"case", "found",
  "loaded"}`. The Cases page uses this for case links.
- A case whose action history could not be read carries
  `"actions_unavailable": true`, so an empty `actions` list does not read
  as "nothing was done".
- `?limit=N` (1 to 100) returns the top N cases of the global rank
  (severity, then time to impact, then most recent); `total` still counts
  every case and `limit` echoes N. `?summary=1` returns each case without
  `evidence`, `action_candidates`, `actions`, `recommendation`,
  `recurrence`, `why` and `source_ids`, adding `has_action_candidates`,
  and sets `"summary": true`. Neither combines with `?case=` (`400`). The
  Overview's **Top Cases** tab uses `?summary=1&limit=5`, names each
  `unavailable` database, and links each row to its case.
- `?finding_source=<subsystem>` keeps only that subsystem's open findings
  (for example `optimizer`, `rules` for the analyzer, or `forecaster`). The
  filter runs in the database read, before the 500-finding cap, so a
  low-severity optimizer finding is not lost behind other findings.
  Incidents and query hints are left out and the response says
  `"filtered": {"finding_source": "optimizer"}`. Every response lists the
  accepted values in `finding_sources`. An empty value means no filter; any
  other value answers `400` with `"code": "unknown_finding_source"`. The
  Cases page's **Finding source** select uses it and keeps the choice in
  the URL (`#/cases?finding_source=optimizer`).

All of these fields are additive; existing fields are unchanged.
`GET /api/v1/shadow-decisions` serves each database's shadow ledger (see
[Shadow mode](configuration.md#shadow-mode)).

Fleet routes (sample responses in [Fleet Mode](fleet.md#api-routes)). With
two databases, pass `min_databases=2`: a fleet finding needs 3 databases by
default.

```bash
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/findings?min_databases=2'
curl -b cookies.txt 'http://localhost:8080/api/v1/fleet/lookalikes?database=production'
curl -b cookies.txt http://localhost:8080/api/v1/fleet/leader
```

The leader route names the control database the lease lives in
(`control_datname`, and `control_system_identifier` where the provider allows
`pg_control_system()`), and `leadership_refused` with its reason when this
sidecar refused to lead. A second sidecar for this fleet must use the same
`--meta-db`; a YAML fleet without a meta database needs
`fleet_learning.control_database` pinned on every sidecar instead (see
[Leader election](fleet.md#leader-election)).

Metrics:

```bash
curl -s http://localhost:8187/metrics | head
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
