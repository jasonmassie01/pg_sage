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

### ConfigMap edits and hot reload

An edit to the `pg-sage-config` ConfigMap hot-reloads without a restart. The
kubelet publishes it after its sync delay, about 60 s plus its ConfigMap cache
TTL, by swapping the volume's `..data` symlink. The sidecar watches the mount
directory, waits 300 ms for the swap to settle (at most 2 s while other files
in that directory keep changing), and reloads only when the file
behind `config.yaml` has changed (its resolved path, size, mtime or content).
The log then shows `[config-watcher] reloaded: [...]`. A candidate that fails
validation is logged and the previous config stays active; fix the file, or
`touch` it, to try again. Fields that need a restart (`postgres.*`, `mode`,
`prometheus.listen_addr`) are logged as `restart required`.

A ConfigMap mounted with `subPath` never receives updates, so a change there
needs a pod restart. Secret files keep the rule in
[Secrets from files](configuration.md#secrets-from-files-_file): a config
reload re-reads the files behind `${NAME}` references, but rotating a secret
alone does not trigger a reload, because only the config directory is watched.

### Health and readiness

Both endpoints are on the API port and need no session.

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /health` | Always `200 {"status":"ok"}` while the process serves HTTP | Liveness and startup probes, the image `HEALTHCHECK` (`sage-sidecar healthcheck`) |
| `GET /ready` | `200` when ready, `503` when not | Readiness probes, load balancer health checks |

`/ready` is ready when the configuration is loaded, the control database
answers (the metadata database, or the monitored database in standalone and
fleet mode), and its `sage` schema has every table the bootstrap migrations
create plus a valid `sage.idx_config_key_db` (the unique index every config
write needs). Each probe is bounded to 0.9 s. The body names the state of each check
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
| `pg_sage_llm_circuit_open{client,database}` | == 1 | LLM circuit breaker tripped |
| `sum by (client,database) (rate(pg_sage_llm_requests_total{result=~"error\|timeout\|rate_limited"}[15m])) / sum by (client,database) (rate(pg_sage_llm_requests_total[15m]))` | > 0.2 | The model is failing before the breaker opens |
| `pg_sage_executor_actions_total{outcome="failed"}` | rate > 0 | Failed autonomous actions |
| `pg_sage_sre_probe_refusals_total{caller="investigation"}` | rate > 0 | Investigations losing probe slots |
| `pg_sage_sso_discovery_ready` | == 0 for 10m | SSO is enabled but OIDC discovery has not succeeded; SSO sign-in is unavailable |
| `pg_sage_sso_discovery_failures_total` | rate > 0 | OIDC discovery attempts failing (IdP outage, DNS, or misconfiguration) |
| `pg_sage_api_tls_cert_not_after_seconds` | `- time() < 14*86400` | API TLS certificate expires within 14 days; rotate `SAGE_TLS_CERT`/`SAGE_TLS_KEY` (present only when the API serves TLS) |
| `pg_sage_api_tls_reload_failures_total` | increase > 0 | A rotated TLS pair failed to load; the old certificate is still serving |
| `pg_sage_leader{scope}` | `sum by (scope)` == 0 across sidecars | No sidecar holds the lease: leader-only jobs (fleet learning, SIEM export, governance reconcilers) are not running. 1 = this sidecar leads, 0 = follower |
| `pg_sage_leader_transitions_total{scope}` | rate > 0 for long | Leadership is flapping; a lapse (renewal overdue past the local deadline) counts and logs one WARN |
| `pg_sage_leader_last_renew_age_seconds{scope}` | > lease TTL | Seconds since the last successful lease-store round trip (since start when none yet). Each renewal is bounded to a third of the lease, so a stall shows here instead of wedging the elector |
| `pg_sage_metrics_refresh_success{database,source}` | == 0 for 5m | A database's metrics could not be read (its series are omitted) |
| `time() - pg_sage_metrics_refresh_timestamp_seconds` | > 3 x refresh interval | A database's exported values are stale |
| `pg_sage_metrics_listener_up` | == 0 | The separate `/metrics` listener failed to bind (scraped through the API route) |

### Sage SRE probe slots

Sage SRE probes share two limits: one probe at a time on each monitored
database, and four probes at a time across the sidecar. A probe that
finds no free slot within 5 seconds is refused before it runs and is
recorded as `concurrency_limit` evidence. These metrics show who waits:

| Metric | Type | Labels |
|---|---|---|
| `pg_sage_sre_probe_queue_wait_seconds_sum` / `_count` | summary | `database`, `caller`, `limiter` |
| `pg_sage_sre_probe_refusals_total` | counter | `database`, `caller`, `limiter` |

- `caller` is the work the probe runs for: `investigation`, `detector`
  (the reactive detector), `runway` (the runway monitor), `log_rca`,
  `narration` (incident narration evidence), `action_target` (the target
  checks of an SRE action), or `other`.
- `limiter` is `database` (the per-database slot) or `sidecar` (the
  shared four).
- Every wait is counted, granted or refused, so `_sum / _count` is the
  mean wait per probe. A probe whose caller gave up while it waited (a
  shutdown, a lost lease) counts as a wait, not a refusal.
- The counters live in the sidecar process and start at zero on a
  restart. A database removed from the fleet (a reload, a delete or a
  rename) loses its series; added back, it starts at zero.
- An investigation probe that is refused (a plan step, an investigator
  read, the model's follow-up probe or a runbook probe step) runs once
  more, but only when its lease leaves at least 10 seconds (the queue
  wait plus the step margin). The second result is kept whatever it is.
  A sustained `caller="investigation"` refusal rate means background
  sampling is crowding out investigations. In fleet mode, compare
  `limiter="sidecar"` refusals across databases.

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
LLM families render in every mode whenever their data exists.

### LLM health

Since v2.7.0 every LLM client the sidecar runs has its own series, labelled `client`
(`general` or `optimizer`) and `database` (the monitored database; empty for the
process-wide shared client and for standalone mode). A scrape only reads the clients: it
never closes or probes a circuit breaker.

| Metric | Meaning |
|---|---|
| `pg_sage_llm_requests_total{client,database,result}` | Calls since start; `result` is `ok`, `error`, `timeout`, `rate_limited`, `budget_refused`, `circuit_open`, `empty` or `cancelled` |
| `pg_sage_llm_request_duration_seconds{client,database}` | Histogram (buckets 0.25 s to 120 s) of calls with result `ok`, `error`, `timeout`, `rate_limited` or `empty`; budget and breaker refusals and cancellations are not timed, and a prompt refused by the pre-flight size guard is an `error` of about 0 s |
| `pg_sage_llm_tokens_total{client,database,kind}` | Tokens charged since start; `kind` is `prompt`, `completion` or `unattributed` (estimated, or not split by the provider). The spend signal: no prices are applied |
| `pg_sage_llm_enabled{client,database}` | 1 if the client is enabled |
| `pg_sage_llm_circuit_open{client,database}` | 1 while the circuit breaker refuses requests |
| `pg_sage_llm_tokens_used_today{client,database}` | Tokens charged today |
| `pg_sage_llm_tokens_budget_daily{client,database}` | The client's live `token_budget_daily` (0 is unlimited); follows a config change without a restart |

The four gauges kept their names and gained the two labels, so an unlabelled selector such as
`pg_sage_llm_circuit_open == 1` still matches (now one series per client). A disabled client
and the per-work-item request cooldown are not calls and are not counted. The counters keep
counting when a database's runtime reconnects, and leave `/metrics` with the database.
`pg_sage_llm_fleet_budget_*` (per-database shares) and `pg_sage_tuning_budget_*` are
unchanged.

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

User emails become case-insensitive on upgrade. If two accounts have emails
that differ only in case, startup logs an ERROR and opens the critical finding
`auth_email_case_duplicates` instead; see
[Email Identity](security.md#email-identity) for how to resolve it.

### How startup migrations behave

Every start runs the `sage` schema migrations under a PostgreSQL advisory lock,
in every database pg_sage bootstraps (the monitored database in standalone
mode, each managed database in fleet mode, and the metadata database).

- **A second start changes nothing.** Before each migration step runs, pg_sage
  reads the catalog: a column, index or table that already exists is not
  touched, so a restart takes no DDL lock on its tables. Steps whose effect the
  catalog cannot show (`DO` blocks, function definitions, small backfills) run
  every time and are written to check the catalog themselves.
- **Lock waits are bounded.** The bootstrap session waits at most 2 seconds for
  any table lock (`lock_timeout = 2s`, reset before the session goes back to
  the pool). A step that cannot get its lock is retried after about 0.5, 1, 2
  and 4 seconds while its 10-second budget lasts, then startup fails with an
  error naming the step, for example
  `migration step ddlActionLogApprovalCols: table lock not granted in 3 attempts (lock_timeout 2s each, ...)`.
  Nothing is left half-applied (each step is one transaction), and the next
  start retries. This keeps a start during a long `pg_dump` or a long-running
  transaction from queueing your application's sessions behind pg_sage's DDL.
  The whole migration list has a budget of 10 seconds per step, capped at 5
  minutes, inside the caller's own startup deadline.
- **The config key index is verified.** Startup fails, naming
  `idx_config_key_db`, when that unique index on `sage.config` cannot be built
  (usually duplicate `(key, database_id)` rows) or exists but is invalid or not
  unique, and `/ready` answers `503` (`schema: not_migrated`) until it is
  fixed. Remove the duplicate rows, or drop the broken index, and restart.
- **One summary line per boot** is logged at INFO, for example
  `schema: 89 steps, 0 applied, 42 rerun, 47 skipped, 95 ms; slowest ddlSREAutonomy 4 ms`.
  `applied` steps created something that was missing, `rerun` steps ran only
  their self-checking statements, and `skipped` steps found everything in
  place. Each applied step is also logged at DEBUG with its duration.

If a start fails on a lock, find the holder with
`SELECT pid, mode, query FROM pg_locks JOIN pg_stat_activity USING (pid) WHERE relation::regclass::text LIKE 'sage.%';`
and wait for it to finish, or restart pg_sage after it.

### Docker

```bash
docker pull ghcr.io/jasonmassie01/pg_sage:latest
docker rm -f pg_sage
# Re-run with same config
```
