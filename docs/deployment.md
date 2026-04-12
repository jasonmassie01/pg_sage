# Deployment

pg_sage is a single Go binary. Deployment is straightforward: download (or build), configure, and run.

---

## Binary

Download the pre-built binary for your platform and run it:

```bash
./pg_sage --pg-url "postgres://sage_agent:YOUR_PASSWORD@host:5432/db"
```

Or with a config file:

```bash
./pg_sage --config config.yaml
```

For production, run as a systemd service:

```ini
# /etc/systemd/system/pg_sage.service
[Unit]
Description=pg_sage PostgreSQL DBA Agent
After=network.target

[Service]
Type=simple
User=pg_sage
ExecStart=/usr/local/bin/pg_sage --config /etc/pg_sage/config.yaml
Restart=always
RestartSec=5
Environment=SAGE_LLM_API_KEY=YOUR_KEY_HERE

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now pg_sage
```

---

## Docker

```bash
docker run -d --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@host:5432/db" \
  -e SAGE_LLM_API_KEY="YOUR_KEY_HERE" \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

With a config file:

```bash
docker run -d --name pg_sage \
  -v /path/to/config.yaml:/etc/pg_sage/config.yaml \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest \
  --config /etc/pg_sage/config.yaml
```

---

## Cloud SQL (Google Cloud)

Validated on PostgreSQL 14, 15, 16, 17. Zero code changes.

```yaml
# config.yaml
mode: standalone

postgres:
  host: YOUR_CLOUD_SQL_IP
  port: 5432
  user: sage_agent
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
  api_key: ${SAGE_LLM_API_KEY}

prometheus:
  listen_addr: 0.0.0.0:9187
```

```bash
./pg_sage --config config.yaml
```

### Cloud Run

Deploy pg_sage as a Cloud Run service for fully managed operation:

```bash
# Build and push
gcloud builds submit --tag us-central1-docker.pkg.dev/PROJECT/repo/pg_sage

# Deploy
gcloud run deploy pg_sage \
  --image us-central1-docker.pkg.dev/PROJECT/repo/pg_sage \
  --set-env-vars SAGE_DATABASE_URL="postgres://sage_agent:pw@/db?host=/cloudsql/PROJECT:REGION:INSTANCE" \
  --set-env-vars SAGE_LLM_API_KEY="YOUR_KEY" \
  --add-cloudsql-instances PROJECT:REGION:INSTANCE \
  --port 8080 \
  --region us-central1
```

---

## AlloyDB (Google Cloud)

AlloyDB is fully supported with zero code changes. Point the config at your AlloyDB IP. The sidecar auto-detects AlloyDB.

---

## RDS / Aurora (AWS)

Connect via standard PostgreSQL connections. Set `sslmode: require` and use IAM auth or password auth.

```yaml
postgres:
  host: your-rds-endpoint.us-east-1.rds.amazonaws.com
  port: 5432
  user: sage_agent
  password: ${PGPASSWORD}
  database: postgres
  sslmode: require
```

---

## Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pg-sage
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pg-sage
  template:
    metadata:
      labels:
        app: pg-sage
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9187"
    spec:
      containers:
        - name: pg-sage
          image: ghcr.io/jasonmassie01/pg_sage:latest
          args: ["--config", "/etc/pg_sage/config.yaml"]
          ports:
            - containerPort: 8080
              name: api
            - containerPort: 9187
              name: metrics
          env:
            - name: SAGE_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: database-url
            - name: SAGE_LLM_API_KEY
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: gemini-api-key
            - name: SAGE_API_KEY
              valueFrom:
                secretKeyRef:
                  name: pg-sage-secrets
                  key: api-key
          volumeMounts:
            - name: config
              mountPath: /etc/pg_sage
      volumes:
        - name: config
          configMap:
            name: pg-sage-config
---
apiVersion: v1
kind: Service
metadata:
  name: pg-sage
spec:
  selector:
    app: pg-sage
  ports:
    - port: 8080
      targetPort: 8080
      name: api
    - port: 9187
      targetPort: 9187
      name: metrics
```

---

## Monitoring with Prometheus + Grafana

### Prometheus Configuration

```yaml
scrape_configs:
  - job_name: pg_sage
    scrape_interval: 30s
    static_configs:
      - targets: ["pg-sage:9187"]
```

### Key Metrics to Alert On

| Metric | Alert Condition | Description |
|---|---|---|
| `pg_sage_findings_total{severity="critical"}` | > 0 | Critical findings need attention |
| `pg_sage_connection_up` | == 0 | Database unreachable |
| `pg_sage_cache_hit_ratio` | < 0.95 | Cache hit ratio below 95% |
| `pg_sage_llm_circuit_open` | == 1 | LLM circuit breaker tripped |
| `pg_sage_executor_actions_total{outcome="failed"}` | rate > 0 | Failed autonomous actions |

### Grafana Dashboard

Import the included dashboard from `grafana/pg_sage_dashboard.json`.

---

## Backup Considerations

pg_sage stores data in the `sage` schema within the target database. Standard PostgreSQL backup tools capture it automatically.

**Critical to back up:** `sage.action_log` (audit trail for autonomous actions).

**Can be regenerated:** `sage.snapshots`, `sage.explain_cache`.

```bash
# Full database backup (includes sage schema)
pg_dump -U postgres -Fc postgres > backup.dump

# Backup only the sage schema
pg_dump -U postgres -Fc -n sage postgres > sage_backup.dump
```

---

## Upgrading

The sidecar is stateless. Replace the binary and restart:

```bash
# Download new version
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64 -o pg_sage
chmod +x pg_sage

# Restart
sudo systemctl restart pg_sage
```

The sidecar handles schema migrations automatically on startup (adding new columns or tables as needed).

### Docker

```bash
docker pull ghcr.io/jasonmassie01/pg_sage:latest
docker rm -f pg_sage
# Re-run with same config
```
