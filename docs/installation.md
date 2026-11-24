# Installation

pg_sage is a standalone Go binary. Download it, point it at your database, and it starts monitoring.

---

## Download Binary

### Linux (amd64)

```bash
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz | tar xz
chmod +x pg_sage
```

### macOS (arm64)

```bash
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_darwin_arm64.tar.gz | tar xz
chmod +x pg_sage
```

### Windows

Windows users should currently build from source with Go 1.26+ and Node.js
22+. The release archives are published for Linux and macOS.

---

## Docker

```bash
docker run --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@host:5432/db" \
  -p 8080:8080 -p 8187:8187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

The image is published for linux/amd64 and linux/arm64 (one manifest list;
Docker pulls the image for your platform, including Graviton and Apple
silicon).

Then read the generated admin password from container logs:

```bash
docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'
```

---

## Verify a Release

Releases are signed keyless with [Sigstore](https://www.sigstore.dev/) by
this repository's CI workflow, with the same identity and issuer as the
signed bench reports. `checksums.txt` lists the SHA-256 of every archive and
`checksums.txt.sigstore.json` is its signature bundle. Each archive also has
an SPDX SBOM, `<archive>.sbom.json`. A release is published only after its
linux/amd64 binary has answered `--version` with the tag's version and the
libpg_query SQL parser.

With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
installed (replace `v2.6.0` with your release in both places):

```bash
base=https://github.com/jasonmassie01/pg_sage/releases/download/v2.6.0
curl -fsSL -O "$base/checksums.txt" -O "$base/checksums.txt.sigstore.json" \
  -O "$base/pg_sage_linux_amd64.tar.gz"
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/tags/v2.6.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

`cosign verify-blob` prints `Verified OK` and `sha256sum` prints
`pg_sage_linux_amd64.tar.gz: OK`. On macOS use
`shasum -a 256 --ignore-missing -c checksums.txt`.

### Verify the image

Each image tag is a manifest list (linux/amd64 and linux/arm64) signed
keyless the same way, and each platform image carries BuildKit provenance
(`mode=max`) and an SBOM. Release tags drop the `v` (`2.6.0`, `2.6`,
`latest`):

```bash
cosign verify ghcr.io/jasonmassie01/pg_sage:2.6.0 \
  --certificate-identity "https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/tags/v2.6.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker buildx imagetools inspect ghcr.io/jasonmassie01/pg_sage:2.6.0 \
  --format '{{ json .Provenance }}'
```

`:edge` and `:sha-*` images from master are signed with the identity
`https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/heads/master`.

---

## Database User Setup

pg_sage needs a database user with monitoring and limited DDL privileges. Run this on your target database:

```sql
CREATE USER sage_agent WITH PASSWORD 'YOUR_PASSWORD';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;    -- new objects in this schema
-- CREATE INDEX and CREATE STATISTICS need table ownership; for autonomous index work:
-- GRANT <table owner role> TO sage_agent;
GRANT pg_signal_backend TO sage_agent;           -- for query termination
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- pg_sage keeps its state in the sage schema. sage_agent cannot create
-- schemas without CREATE on the database, so create it for the agent:
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
```

`pg_stat_statements` must also be preloaded (`shared_preload_libraries = 'pg_stat_statements'`). Most managed services have this enabled by default. The end-to-end test `TestDocumentedQuickStart` runs the SQL block above verbatim, so keep it runnable.

At startup pg_sage checks each prerequisite and logs what it could not confirm:

- **Query text.** Without `pg_read_all_stats`, `pg_stat_statements` shows `<insufficient privilege>` for other roles' statements. pg_sage asks the server the same question `pg_stat_statements` does: `SELECT pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')` on PostgreSQL 15+ (the role must inherit it, so a `NOINHERIT` member does not count), `'MEMBER'` on PostgreSQL 14. A superuser passes. When it is false the sidecar logs a `GRANT pg_read_all_stats` warning and the first look shows a "Query text is hidden" finding. Managed roles that are not superusers (RDS, Cloud SQL) need the grant.
- **Extension schema.** pg_sage finds the schema each extension it reads (`pg_stat_statements`, `pgstattuple`, `hypopg`) was created in from `pg_extension` and names every object of it qualified by that schema, in every reader (collector, analyzer, tuner, executor checks, Ask Sage and MCP tools), so no extension needs to be on the role's `search_path`. The collector re-reads the schema on each capability refresh, so an extension created or moved after startup is followed from the next cycle.
- **pg_sage's own search_path.** Every pg_sage session runs with `search_path = pg_catalog, pg_temp`, set after connect, so an `ALTER ROLE <pg_sage role> SET search_path` (or a database-level one) no longer changes anything pg_sage reads, and nothing planted in `public` runs in its sessions. It needs no privilege and applies on PostgreSQL 14-18 and every managed provider. Through PgBouncer, pool pg_sage's connections in session mode. See [Security](security.md).
- **WAL and planning-time columns** (`pg_stat_statements` 1.9+, shipped with PostgreSQL 14). If the check itself fails, for example on a 5-second timeout, the sidecar logs a WARN naming the cause, and per-query WAL usage or planning time is not collected until it restarts. Without `pg_stat_statements`, monitoring runs degraded: the first look lists the exact steps to enable it on your provider.

### Optional: a separate action role

To keep probes from running with owner rights, give pg_sage a second role for its
actions and point `action_dsn` (`postgres.action_dsn`, or `databases[].action_dsn` in a
fleet) at it. The main role then needs only `pg_monitor` and the `sage` schema; the action
role gets the table owner memberships, `pg_maintain`, `pg_signal_backend` and
`pg_read_all_stats`. See
[separate observation and action roles](security.md#separate-observation-and-action-roles-action_dsn).

---

## Running with Connection URL

The simplest way to start -- observation mode, no LLM:

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@your-instance:5432/postgres"
```

This starts the collector, analyzer, authenticated API+dashboard on `:8080`,
and Prometheus metrics on `:8187`.

On first start, pg_sage creates the first admin user and prints the initial
password to stderr:

```text
first admin created — email: admin@pg-sage.local  password: [redacted, see stderr]
*** INITIAL ADMIN PASSWORD: <copy this value> ***
```

Open `http://localhost:8080` and sign in as `admin@pg-sage.local` with it.
pg_sage then asks you to choose your own password (12 to 72 bytes) before
anything else; see [Passwords](security.md#passwords). API calls under `/api/v1/`
require the `sage_session` cookie returned by `/api/v1/auth/login`.

---

## Running with Config File

For full control, create a `config.yaml`:

```yaml
mode: standalone

postgres:
  host: your-instance-ip
  port: 5432
  user: sage_agent
  password: YOUR_PASSWORD
  database: postgres
  sslmode: require
  max_connections: 2

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 600

trust:
  level: observation

llm:
  enabled: true
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  optimizer:
    enabled: true

prometheus:
  listen_addr: "0.0.0.0:8187"
```

```bash
./pg_sage --config config.yaml
```

---

## Build from Source

Requires Go 1.26+ and Node.js 22+. Releases are built with the Go release named
by the `toolchain` line in `sidecar/go.mod` (Go 1.27.4); with the default
`GOTOOLCHAIN=auto` the `go` command fetches that release automatically.
A C compiler (gcc or clang) is also needed: with cgo the binary links
libpg_query, which checks every executor statement against its PostgreSQL
parse tree. Without a C compiler the build still succeeds, but that layer is
left out; `pg_sage --version` then reports `sql-ast: unavailable`, startup
logs a warning, and changes pg_sage would make on its own wait for operator
approval instead. Release binaries and Docker images always include it.


```bash
git clone https://github.com/jasonmassie01/pg_sage.git
cd pg_sage/sidecar
cd web && npm ci && npm run build && cd ..
go build -o pg_sage ./cmd/pg_sage_sidecar
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@host:5432/postgres"
```

---

## Verifying the Installation

After starting pg_sage, verify it is running:

```bash
# Check Prometheus metrics
curl -s http://localhost:8187/metrics | head -10

# Check web UI
curl -I http://localhost:8080

# Log in before calling authenticated APIs
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/shadow-decisions
```

Connect to your database and check the sage schema:

```sql
-- Findings appear after the first analyzer cycle (~600 seconds)
SELECT category, severity, title
FROM sage.findings
WHERE status = 'open'
ORDER BY severity;
```

You should see findings for any detected issues (duplicate indexes, unused indexes, slow queries, sequence exhaustion, etc.).

---

## Supported Platforms

| Platform | PG Versions | Status |
|----------|-------------|--------|
| Google Cloud SQL | 14, 15, 16, 17 | Verified |
| Google AlloyDB | 17 | Verified |
| Self-managed | 14-18 | Verified (CI matrix) |
| Amazon Aurora | 14-17 | Supported |
| Amazon RDS | 14-17 | Supported |
