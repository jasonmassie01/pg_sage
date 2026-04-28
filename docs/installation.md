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

Windows users should currently build from source with Go 1.24+ and Node.js
20+. The release archives are published for Linux and macOS.

---

## Docker

```bash
docker run --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@host:5432/db" \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

Then read the generated admin password from container logs:

```bash
docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'
```

---

## Database User Setup

pg_sage needs a database user with monitoring and limited DDL privileges. Run this on your target database:

```sql
CREATE USER sage_agent WITH PASSWORD 'YOUR_PASSWORD';
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;    -- for index creation
GRANT pg_signal_backend TO sage_agent;           -- for query termination

-- pg_sage bootstraps these automatically, but you can pre-create if preferred:
CREATE SCHEMA sage;
GRANT ALL ON SCHEMA sage TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent;
```

Ensure `pg_stat_statements` is loaded on your database (`shared_preload_libraries = 'pg_stat_statements'`). Most managed services have this enabled by default.

---

## Running with Connection URL

The simplest way to start -- observation mode, no LLM:

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@your-instance:5432/postgres"
```

This starts the collector, analyzer, authenticated API+dashboard on `:8080`,
and Prometheus metrics on `:9187`.

On first start, pg_sage creates the first admin user and prints the initial
password to stderr:

```text
first admin created — email: admin@pg-sage.local  password: [redacted, see stderr]
*** INITIAL ADMIN PASSWORD: <copy this value> ***
```

Open `http://localhost:8080`, sign in as `admin@pg-sage.local`, and change or
store that password according to your local policy. API calls under `/api/v1/`
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
  listen_addr: "0.0.0.0:9187"
```

```bash
./pg_sage --config config.yaml
```

---

## Build from Source

Requires Go 1.24+ and Node.js 20+:

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
curl -s http://localhost:9187/metrics | head -10

# Check web UI
curl -I http://localhost:8080

# Log in before calling authenticated APIs
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/databases
curl -b cookies.txt http://localhost:8080/api/v1/cases
curl -b cookies.txt http://localhost:8080/api/v1/shadow-report
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
| Self-managed | 14, 15, 16, 17 | Verified |
| Amazon Aurora | 14-17 | Supported |
| Amazon RDS | 14-17 | Supported |
