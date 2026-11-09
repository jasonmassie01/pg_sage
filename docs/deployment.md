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

With a config file, name it in `SAGE_CONFIG_PATH` rather than `--config`, so
the image `HEALTHCHECK` reads the same `api.listen_addr`:

```bash
docker run -d --name pg_sage \
  -v /path/to/config.yaml:/etc/pg_sage/config.yaml \
  -e SAGE_CONFIG_PATH=/etc/pg_sage/config.yaml \
  -p 8080:8080 -p 9187:9187 \
  ghcr.io/jasonmassie01/pg_sage:latest
```

### Container health check

The image `HEALTHCHECK` runs `sage-sidecar healthcheck`, which sends
`GET /health` to the API listener and exits 0 on `200`. It finds the address
the way the sidecar does: `--addr`, then `api.listen_addr` from
`SAGE_CONFIG_PATH` or `./config.yaml`, then `0.0.0.0:8080`. A wildcard host
(`0.0.0.0`, `::` or empty) is probed on `127.0.0.1`. With `SAGE_TLS_CERT` and
`SAGE_TLS_KEY` set it uses HTTPS, and skips certificate verification only for
a loopback address. It sends no credential and checks liveness only; use
`GET /ready` for readiness.

Arguments after the image name (such as `--config`) are invisible to the
health check. When the API listens on an address the check cannot read from
those sources, override the check with it:

```yaml
healthcheck:
  test: ["CMD", "sage-sidecar", "healthcheck", "--addr", "127.0.0.1:9443"]
```

`--timeout` (default `3s`) bounds the probe. On failure stderr names the URL
and the status or error, and `docker inspect` shows it under
`.State.Health.Log`.

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
| `GET /health` | Always `200 {"status":"ok"}` while the process serves HTTP | Liveness and startup probes, the image `HEALTHCHECK` (`sage-sidecar healthcheck`) |
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
identity-aware proxy. Prometheus metrics stay on the separate
`prometheus.listen_addr` unless you also
[serve them on the API listener](#scraping-through-the-api-listener).

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

The separate listener (`prometheus.listen_addr`, default `127.0.0.1:9187`) has
no authentication and no TLS. Port 9187 is also postgres_exporter's default:
when both run on one host, move one of them (`SAGE_PROMETHEUS_ADDR=127.0.0.1:9188`,
say). If pg_sage cannot bind the address it logs an ERROR, keeps running, and
exports `pg_sage_metrics_listener_up 0` where it still serves `/metrics` (the API
route below). `prometheus.listen_addr: ""` opens no separate listener.

### Scraping through the API listener

Set `prometheus.serve_on_api: true` to serve `GET /metrics` on the API listener,
with its TLS and rate limit, behind a token holding the `metrics` scope. Use it
when only the API port is reachable or when scrapes must be encrypted;
optionally set `prometheus.listen_addr: ""` to close the separate listener. The
route needs the control database (pg_sage refuses to start without one) and
serves the same snapshot as the separate listener, so a scrape adds no
database load.

An admin creates the scrape token (the secret is shown once). `metrics` must
be the token's only scope, it covers every database (`"databases":["*"]`:
`/metrics` cannot be narrowed to one), and it is an operator token owned by an
operator or admin; it grants no MCP tool, and the MCP endpoint refuses it:

```bash
curl -b cookies.txt -X POST https://pg-sage.example.com/api/v1/mcp/tokens \
  -H 'Content-Type: application/json' \
  --data '{"name":"prometheus","kind":"operator","scopes":["metrics"],
           "databases":["*"],"expires_in_days":90,"owner_user_id":OWNER_ID}'
```

Store the `token` value in a file readable only by Prometheus, and rotate it
before it expires (90 days at most): create the next token, switch the file,
then revoke the old one with `DELETE /api/v1/mcp/tokens/{id}`.

```yaml
scrape_configs:
  - job_name: pg_sage
    scheme: https
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/pg_sage_metrics_token
    static_configs:
      - targets: ["pg-sage.example.com:8080"]
```

A request without a valid token gets 401, a valid token without the `metrics`
scope 403, and 503 when tokens cannot be checked; none of these carries metric
names. Revoking the token, or deactivating its owner, ends its scrapes at once.
Fleet and meta mode serve one `/metrics` per sidecar, on either listener.

### Key Metrics to Alert On

| Metric | Alert Condition | Description |
|---|---|---|
| `pg_sage_findings_open{database,severity="critical"}` | > 0 | Critical findings need attention |
| `pg_sage_database_up{database}` | == 0 for 5m | A monitored database is unreachable or its status refresh fails |
| `time() - pg_sage_collector_last_success_timestamp_seconds{database}` | > 3 x collector interval | A database's collector has stalled |
| `pg_sage_emergency_stopped{database}` | == 1 | The database's emergency stop holds |
| `pg_sage_connection_up` | == 0 | The sidecar cannot reach its control database |
| `pg_sage_cache_hit_ratio` | < 0.95 | Control database cache hit ratio below 95% (deprecated, removed in v2.7.0) |
| `pg_sage_llm_circuit_open` | == 1 | LLM circuit breaker tripped |
| `pg_sage_executor_actions_total{outcome="failed"}` | rate > 0 | Failed autonomous actions |
| `pg_sage_metrics_refresh_success{database,source}` | == 0 for 5m | A database's metrics could not be read (its series are omitted) |
| `time() - pg_sage_metrics_refresh_timestamp_seconds` | > 3 x refresh interval | A database's exported values are stale |
| `pg_sage_metrics_listener_up` | == 0 | The separate `/metrics` listener failed to bind (scraped through the API route) |

### Metrics freshness

A scrape never queries a monitored database. Each database has its own refresher that
reads the values `/metrics` exports (value ledger, SLO state and, in standalone mode,
optimizer recommendation counts and `pg_stat_activity`/`pg_stat_database`) every
`prometheus.refresh_interval_seconds` (default 30 s, never faster than the collector), each
source under a 2 s timeout, with `/* pg_sage */`-tagged statements. Scrapes render the last
refresh, so values can be up to one refresh interval old, and the load on your databases is
the same whether Prometheus scrapes every second or every minute. In fleet and meta mode
each database refreshes independently: one unreachable database reports
`pg_sage_metrics_refresh_success{source="value"} 0` and `pg_sage_value_metrics_up 0` while
every other database keeps its series, and the scrape itself stays fast.

- `pg_sage_metrics_refresh_success{database,source}`: 1 if the last refresh of `source`
  (`value`, `slo`, `optimizer`, `pgstat`) succeeded. A failed source's series are omitted
  rather than shown stale or zero.
- `pg_sage_metrics_refresh_timestamp_seconds{database,source}`: Unix time of the last
  successful refresh of `source`; it keeps its value while refreshes fail.
- `pg_sage_value_metrics_up{database}`: 1 if the last refresh read the database's value
  ledger.
- `pg_sage_connection_up`: the last pool health check (every 30 s) in standalone and meta
  mode; in YAML fleet mode, 1 if any database's last refresh reached it. Before the first
  result there is no sample.
- In meta mode, `pg_sage_connections_total` and `pg_sage_cache_hit_ratio` describe the
  metadata database, refreshed on the same cadence.
- A database that failed to connect or prepare has no refresher; it keeps reporting
  `pg_sage_value_metrics_up 0` and `pg_sage_metrics_refresh_success{source="value"} 0`.

### Per-database health

Since v2.6.0 the same per-database families render in standalone, fleet and meta mode,
from the sidecar's in-memory status of each monitored database (no query at scrape time).
In meta mode no sample describes the metadata database: every series names a monitored
database from `sage.databases`.

| Metric | Meaning |
|---|---|
| `pg_sage_database_up{database}` | 1 if the database is connected and its last status refresh succeeded |
| `pg_sage_collector_last_success_timestamp_seconds{database}` | Unix time of the last collector snapshot; absent until the first one |
| `pg_sage_analyzer_last_success_timestamp_seconds{database}` | Unix time of the last findings refresh after analysis; absent until the first one |
| `pg_sage_findings_open{database,severity}` | Open findings (`critical`, `warning`, `info`); the last known counts stay while the database is down |
| `pg_sage_database_health_score{database}` | 0-100, the dashboard's health score (0 while down or in error) |
| `pg_sage_emergency_stopped{database}` | 1 while the database's own or the fleet-wide emergency stop holds |
| `pg_sage_trust_level{database,level}` | One-hot effective trust level (`observation`, `advisory`, `autonomous`); no samples before the database starts |
| `pg_sage_database_size_bytes{database}` | Size from the collector's last snapshot; absent until measured (gained the `database` label in v2.6.0) |

`pg_sage_connection_up` now means the sidecar's control database: the monitored database in
standalone, the metadata database in meta mode, and any database in YAML fleet mode. Alert on
`pg_sage_database_up` for monitored databases. The fleet summary (`pg_sage_fleet_*`) and the
LLM gauges render in every mode whenever their data exists.

These families are deprecated and keep emitting in v2.6.0; they are removed in v2.7.0:

| Deprecated (removed in v2.7.0) | Use instead |
|---|---|
| `pg_sage_findings_total{severity}` | `pg_sage_findings_open{database,severity}` |
| `pg_sage_collector_last_run_timestamp` | `pg_sage_collector_last_success_timestamp_seconds{database}` |
| `pg_sage_fleet_instance_findings{database}` | `pg_sage_findings_open{database,severity}` (sum by database) |
| `pg_sage_fleet_instance_health{database}` | `pg_sage_database_health_score{database}` |
| `pg_sage_connections_total{state}` | postgres_exporter (`pg_stat_activity_count`) |
| `pg_sage_cache_hit_ratio` | postgres_exporter (`pg_stat_database_blks_hit` / `blks_read`) |

### Exposition contract

`/metrics` is Prometheus text format 0.0.4 (`text/plain; version=0.0.4; charset=utf-8`).
Since v2.6.0:

- Label values are escaped as the format requires: only `\\`, `\"` and `\n`. Other
  characters, a tab or a control character included, are written as they are, and invalid
  UTF-8 becomes U+FFFD. A database or SLO name with an odd character no longer makes the
  whole scrape unparseable.
- `pg_sage_build_info{version,commit} 1` names the running build, and
  `process_start_time_seconds` is the process start time (Unix seconds), so restarts show as
  `changes(process_start_time_seconds[1h])`.
- When a database leaves the sidecar (removed by a fleet reload, deleted or renamed in meta
  mode) its series disappear, the process-wide counters
  (`pg_sage_policy_parks_total`, `pg_sage_verification_wait_releases_total`,
  `pg_sage_policy_record_failures_total`, `pg_sage_shadow_*_total`) included. A database
  added again under the same name counts from zero, which `rate()` treats as a counter reset.
- Series are written in the same order on every scrape, and every family has one `HELP` and
  one `TYPE` line.

Four gauges still carry the counter suffix `_total`: `pg_sage_findings_total`,
`pg_sage_connections_total`, `pg_sage_fleet_findings_total` and
`pg_sage_optimizer_recommendations_total`. They keep their names in v2.6.0; v2.7.0 removes
the first two and renames the others, with `pg_sage_collector_last_run_timestamp`, in one
coordinated rename release. Do not apply `rate()` or `increase()` to them.

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
