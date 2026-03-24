# Deployment

## Docker Compose

The included `docker-compose.yml` is production-ready with minor adjustments. For cloud database deployments where the extension cannot be installed, use the sidecar in standalone mode with a YAML config file (see [Cloud SQL](#cloud-sql-google-cloud) below).

### Basic Setup

```yaml
services:
  pg_sage:
    build: .
    environment:
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: postgres
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    command: >
      postgres
        -c shared_preload_libraries='pg_stat_statements,pg_sage'
        -c sage.database='postgres'
        -c sage.collector_interval=30
        -c sage.analyzer_interval=60
        -c sage.trust_level='advisory'
        -c pg_stat_statements.track=all
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 5s
      timeout: 5s
      retries: 5

  sidecar:
    build: ./sidecar
    depends_on:
      pg_sage:
        condition: service_healthy
    environment:
      SAGE_DATABASE_URL: postgres://postgres:${POSTGRES_PASSWORD}@pg_sage:5432/postgres?sslmode=disable
      SAGE_MCP_PORT: "5433"
      SAGE_PROMETHEUS_PORT: "9187"
      SAGE_RATE_LIMIT: "60"
      SAGE_API_KEY: ${SAGE_API_KEY}
    ports:
      - "5433:5433"
      - "9187:9187"

volumes:
  pgdata:
```

!!! warning
    Always set `POSTGRES_PASSWORD` and `SAGE_API_KEY` via environment variables or a `.env` file. Never hardcode credentials.

### Production Recommendations

- Set `sage.trust_level = 'observation'` initially and promote after validation
- Configure `sage.maintenance_window` to restrict autonomous actions to off-peak hours
- Enable `sage.redact_queries = on` if query literals contain sensitive data
- Set `SAGE_API_KEY` to require authentication for the MCP sidecar
- Use `SAGE_TLS_CERT` and `SAGE_TLS_KEY` for TLS on the sidecar

---

## Cloud SQL (Google Cloud)

The pg_sage sidecar runs standalone against Cloud SQL PostgreSQL instances. Validated on PG 17.

```bash
# Using YAML config
cat > config.yaml << 'EOF'
mode: standalone
postgres:
  host: 34.72.70.25
  port: 5432
  user: postgres
  password: ${PGPASSWORD}
  database: postgres
  sslmode: require
trust:
  level: advisory
  maintenance_window: "0 2 * * *"
llm:
  enabled: true
  endpoint: https://generativelanguage.googleapis.com/v1beta/openai
  model: gemini-2.5-flash
  api_key: ${GEMINI_API_KEY}
mcp:
  enabled: true
  listen: 0.0.0.0:5433
prometheus:
  listen: 0.0.0.0:9187
EOF

cd sidecar && go build -o sage-sidecar ./cmd/pg_sage_sidecar
./sage-sidecar --config config.yaml
```

The sidecar auto-detects Cloud SQL via `cloudsql_proxy` connection metadata or IP ranges.

---

## AlloyDB (Google Cloud)

AlloyDB is fully supported with zero code changes. The sidecar detects AlloyDB via `current_setting('alloydb.iam_authentication')`.

```bash
# Same config as Cloud SQL, just point to AlloyDB IP
```

---

## RDS / Aurora (AWS)

The sidecar connects to RDS and Aurora via standard PostgreSQL connections. Set `sslmode: require` and use IAM auth or password auth.

---

## Kubernetes

### Basic Deployment

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: pg-sage
spec:
  serviceName: pg-sage
  replicas: 1
  selector:
    matchLabels:
      app: pg-sage
  template:
    metadata:
      labels:
        app: pg-sage
    spec:
      containers:
        - name: postgres
          image: pg-sage:latest
          ports:
            - containerPort: 5432
          env:
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: postgres-password
            - name: POSTGRES_DB
              value: postgres
          args:
            - postgres
            - -c
            - shared_preload_libraries=pg_stat_statements,pg_sage
            - -c
            - sage.database=postgres
            - -c
            - sage.trust_level=observation
          volumeMounts:
            - name: pgdata
              mountPath: /var/lib/postgresql/data
          livenessProbe:
            exec:
              command: ["pg_isready", "-U", "postgres"]
            initialDelaySeconds: 30
            periodSeconds: 10
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "postgres"]
            initialDelaySeconds: 5
            periodSeconds: 5
  volumeClaimTemplates:
    - metadata:
        name: pgdata
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 50Gi
```

### Sidecar Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sage-sidecar
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sage-sidecar
  template:
    metadata:
      labels:
        app: sage-sidecar
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9187"
    spec:
      containers:
        - name: sidecar
          image: sage-sidecar:latest
          ports:
            - containerPort: 5433
              name: mcp
            - containerPort: 9187
              name: metrics
          env:
            - name: SAGE_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: database-url
            - name: SAGE_API_KEY
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: api-key
            - name: SAGE_MCP_PORT
              value: "5433"
            - name: SAGE_PROMETHEUS_PORT
              value: "9187"
```

### Service and Secret

```yaml
apiVersion: v1
kind: Service
metadata:
  name: pg-sage
spec:
  selector:
    app: pg-sage
  ports:
    - port: 5432
      targetPort: 5432
---
apiVersion: v1
kind: Service
metadata:
  name: sage-sidecar
spec:
  selector:
    app: sage-sidecar
  ports:
    - port: 5433
      targetPort: 5433
      name: mcp
    - port: 9187
      targetPort: 9187
      name: metrics
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-sage-secrets
type: Opaque
stringData:
  postgres-password: "CHANGE_ME"
  database-url: "postgres://postgres:CHANGE_ME@pg-sage:5432/postgres?sslmode=disable"
  api-key: "CHANGE_ME"
```

---

## Monitoring with Prometheus + Grafana

### Prometheus Configuration

Add the sidecar as a scrape target in `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: pg_sage
    scrape_interval: 30s
    static_configs:
      - targets: ["sage-sidecar:9187"]
```

### Key Metrics to Monitor

| Metric | Alert Condition | Description |
|---|---|---|
| `pg_sage_findings_total{severity="critical"}` | > 0 | Critical findings need attention |
| `pg_sage_circuit_breaker_state{breaker="db"}` | == 1 | Database circuit breaker tripped |
| `pg_sage_circuit_breaker_state{breaker="llm"}` | == 1 | LLM circuit breaker tripped |
| `pg_sage_cache_hit_ratio` | < 0.95 | Cache hit ratio below 95% |
| `pg_sage_deadlocks_total` | rate > 0 | Deadlocks occurring |

### Example Alertmanager Rules

```yaml
groups:
  - name: pg_sage
    rules:
      - alert: PgSageCriticalFindings
        expr: pg_sage_findings_total{severity="critical"} > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "pg_sage has critical findings"

      - alert: PgSageCircuitBreakerOpen
        expr: pg_sage_circuit_breaker_state == 1
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "pg_sage circuit breaker {{ $labels.breaker }} is open"
```

### Grafana Dashboard

Import the sidecar metrics as a Prometheus data source in Grafana and create panels for:

- Findings count by severity (stacked bar)
- Circuit breaker state (status panel)
- Cache hit ratio (gauge)
- Connection utilization (time series)
- Database size growth (time series)
- Transaction rate (time series)

---

## Backup Considerations

pg_sage stores its data in the `sage` schema within the target database. Standard PostgreSQL backup tools capture it automatically.

**What to back up:**

- The `sage` schema is included in `pg_dump` by default
- `sage.action_log` is the most critical table (audit trail for autonomous actions)
- `sage.findings` history is useful for trend analysis

**What can be regenerated:**

- `sage.snapshots` -- can be recollected after restore
- `sage.explain_cache` -- plans are recaptured on demand

```bash
# Full database backup (includes sage schema)
pg_dump -U postgres -Fc postgres > backup.dump

# Backup only the sage schema
pg_dump -U postgres -Fc -n sage postgres > sage_backup.dump
```

!!! tip
    Consider shorter retention for `sage.snapshots` (via `sage.retention_snapshots`) to reduce backup size.

---

## Upgrading pg_sage

### Minor Version Upgrades (e.g., 0.5.0 to 0.5.1)

1. Build and install the new version:

    ```bash
    cd pg_sage
    git pull
    make
    sudo make install
    ```

2. Restart PostgreSQL to load the new shared library.

3. Run the upgrade migration:

    ```sql
    ALTER EXTENSION pg_sage UPDATE;
    ```

### Major Version Upgrades (e.g., 0.1.0 to 0.5.0)

Migration scripts are provided in the `sql/` directory:

```sql
-- Check current version
SELECT * FROM pg_extension WHERE extname = 'pg_sage';

-- Upgrade
ALTER EXTENSION pg_sage UPDATE TO '0.5.0';
```

!!! warning
    Always test upgrades in a staging environment first. Back up the `sage` schema before upgrading.

### Docker Upgrades

```bash
cd pg_sage
git pull
docker compose build
docker compose up -d
```

The container will restart PostgreSQL with the new extension. Migration scripts run automatically if the extension version changes.

### Sidecar Upgrades

The sidecar is stateless. Simply rebuild and restart:

```bash
cd sidecar
docker compose build sidecar
docker compose up -d sidecar
```
