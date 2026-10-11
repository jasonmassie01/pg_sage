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

Read the one-time admin password from startup logs:

```bash
docker logs pg_sage 2>&1 | grep 'INITIAL ADMIN PASSWORD'
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
  maintenance_window: "0 2 * * *"   # 02:00-03:00; see docs/configuration.md

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
# Build and push the sidecar image
gcloud builds submit sidecar \
  --tag us-central1-docker.pkg.dev/PROJECT/repo/pg_sage

# Deploy
gcloud run deploy pg_sage \
  --image us-central1-docker.pkg.dev/PROJECT/repo/pg_sage \
  --set-env-vars SAGE_DATABASE_URL="postgres://sage_agent:pw@/db?host=/cloudsql/PROJECT:REGION:INSTANCE" \
  --set-env-vars SAGE_LLM_API_KEY="YOUR_KEY" \
  --add-cloudsql-instances PROJECT:REGION:INSTANCE \
  --port 8080 \
  --region us-central1
```

After first deploy, read the revision logs for the generated admin password and
rotate it after login.

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
      securityContext:
        fsGroup: 1000          # the image runs as uid/gid 1000 (sage)
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
            # Secrets as files (see "Secrets from files" in configuration.md):
            # they stay out of the process environment and rotate in place.
            - name: SAGE_DATABASE_URL_FILE
              value: /var/run/secrets/pg-sage/database-url
            - name: SAGE_LLM_API_KEY_FILE
              value: /var/run/secrets/pg-sage/gemini-api-key
            # Optional: serve HTTPS from a cert-manager or other TLS secret.
            - name: SAGE_TLS_CERT
              value: /var/run/tls/tls.crt
            - name: SAGE_TLS_KEY
              value: /var/run/tls/tls.key
          livenessProbe:          # static: the process answers HTTP
            httpGet: {path: /health, port: api, scheme: HTTPS}
            periodSeconds: 10
            failureThreshold: 3
          readinessProbe:         # config loaded, control DB up, schema migrated
            httpGet: {path: /ready, port: api, scheme: HTTPS}
            periodSeconds: 10
            timeoutSeconds: 2
            failureThreshold: 3
          startupProbe:           # first start bootstraps the sage schema
            httpGet: {path: /health, port: api, scheme: HTTPS}
            periodSeconds: 5
            failureThreshold: 60
          volumeMounts:
            - name: config
              mountPath: /etc/pg_sage
            - name: secrets
              mountPath: /var/run/secrets/pg-sage
              readOnly: true
            - name: tls
              mountPath: /var/run/tls
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: pg-sage-config
        - name: secrets
          secret:
            secretName: pg-sage-secrets
            defaultMode: 0440   # group-readable by fsGroup below
        - name: tls
          secret:
            secretName: pg-sage-tls
            defaultMode: 0440   # group-readable by fsGroup below
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

Without `SAGE_TLS_CERT`/`SAGE_TLS_KEY`, drop them and use `scheme: HTTP` in the
probes.

### Health and readiness

Both endpoints are on the API port and need no session.

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /health` | Always `200 {"status":"ok"}` while the process serves HTTP | Liveness and startup probes, Docker `HEALTHCHECK` |
| `GET /ready` | `200` when ready, `503` when not | Readiness probes, load balancer health checks |

`/ready` is ready when the configuration is loaded, the control database
answers (the metadata database, or the monitored database in standalone and
fleet mode), and its `sage` schema has every table the bootstrap migrations
create. Each probe is bounded to 0.9 s. The body names the state of each check
and never an error message, because the endpoint is unauthenticated:

```json
{"status":"not_ready","checks":{"config":"ok","control_db":"unreachable","schema":"skipped"}}
```

| Check | Values |
|---|---|
| `config` | `ok`, `not_loaded` |
| `control_db` | `ok`, `absent` (no reachable database owns sessions), `unreachable` |
| `schema` | `ok`, `not_migrated`, `unknown` (the check failed), `skipped` |

The reason behind a `503` is logged once each time readiness changes. Don't use
`/ready` as a liveness probe: a control database outage would restart every
replica without fixing anything.

---

## First Admin and API Authentication

The dashboard and `/api/v1/*` endpoints are session-authenticated in v1. The
first time pg_sage starts against a metadata database with no users, it creates
`admin@pg-sage.local` and prints the initial password to stderr. Capture that
startup log in your service manager or secret handoff process, then rotate the
password after login.

For API scripts, log in and reuse the cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST https://pg-sage.example.com/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt https://pg-sage.example.com/api/v1/cases
```

Expose the API/dashboard only on trusted networks or behind your normal
identity-aware proxy. Prometheus metrics remain on the separate
`prometheus.listen_addr`.

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

With `history.store: meta` (meta-db mode), `sage.snapshots` and `sage.query_store` of
every monitored database live in the metadata database: back it up to keep them.

### Moving history to the metadata database

To take pg_sage's telemetry history (snapshots and the query store, most of its storage)
out of a monitored database in meta-db mode: stop pg_sage, run
`pg_sage history migrate --to meta --database NAME` with
`SAGE_HISTORY_MONITORED_DSN` and `SAGE_META_DB` set, set `history.store: meta`, start
pg_sage, then run the same command with `--cleanup` to remove the copied rows. Repeat
the migrate step for each database. A database whose history was not migrated is
refused at startup (the error names the command); the others start. See
[Configuration](configuration.md#keeping-telemetry-history-in-the-meta-database-historystore).

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
curl -fsSL https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz | tar xz
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
