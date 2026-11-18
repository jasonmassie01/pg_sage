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
# systemd relaunches the sidecar, so Settings may offer "Restart now".
Environment=SAGE_SUPERVISED=1

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now pg_sage
```

### Restarting from Settings (`SAGE_SUPERVISED`)

Fields whose lifecycle is `restart` (see
[configuration](configuration.md)) are saved at once but take effect at the
next start. Settings lists them in a "settings wait for restart" panel with
the running and the saved value, and `GET /api/v1/config/global` returns them
as `pending` (`key`, `active`, `desired`, `lifecycle`).

`POST /api/v1/restart` (the "Restart now" button) exits the process with code
42 and relies on a supervisor to start it again. Set `SAGE_SUPERVISED=1` (or
`true`) only where one does: systemd with `Restart=always`, Docker with
`--restart unless-stopped`, or Kubernetes, whose default `restartPolicy:
Always` restarts the container. Without it the route answers `501`, the API
reports `restart_supported: false`, and Settings tells you to restart through
your supervisor (`systemctl restart pg_sage`, `kubectl rollout restart
deployment/pg-sage`) instead of offering a button that would stop the sidecar
for good. Databricks Apps and other platforms that do not relaunch on exit
must leave it unset.

---

## Docker

```bash
docker run -d --name pg_sage \
  -e SAGE_DATABASE_URL="postgres://sage_agent:YOUR_PASSWORD@host:5432/db" \
  -e SAGE_LLM_API_KEY="YOUR_KEY_HERE" \
  -p 8080:8080 -p 8187:8187 \
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
  -p 8080:8080 -p 8187:8187 \
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

Validated live on Cloud SQL majors 14 to 17; pg_sage itself is tested in CI on
PostgreSQL 14-18 (CI matrix), and Cloud SQL 18 is not yet validated live. Zero code changes.

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
  listen_addr: 0.0.0.0:8187
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

Install with the Helm chart in [`deploy/helm/pg-sage`](https://github.com/jasonmassie01/pg_sage/tree/master/deploy/helm/pg-sage)
(each release also attaches it as `pg-sage-<version>.tgz`). Create the Secret first; the
chart never creates one, and mounts its keys as files (`SAGE_*_FILE`) so they stay out of
the process environment and rotate in place:

```sh
kubectl create secret generic pg-sage-secrets \
  --from-file=database-url=./database-url \
  --from-file=llm-api-key=./llm-api-key \
  --from-file=encryption-key=./encryption-key
helm install sage deploy/helm/pg-sage \
  --set secrets.existingSecret=pg-sage-secrets \
  --set secrets.llmAPIKeyKey=llm-api-key \
  --set secrets.encryptionKeyKey=encryption-key
```

The chart renders:

- a Deployment with `replicas: 1` and `strategy: Recreate`, fixed so that two sidecars
  never run executors together, and the image tag pinned to the chart `appVersion` (never
  `latest`; `image.tag: latest` fails the render);
- a non-root pod (uid/gid 1000) with a read-only root filesystem, no privilege escalation,
  all capabilities dropped and the `RuntimeDefault` seccomp profile; `/tmp` and `data_dir`
  (`/var/lib/pg_sage`, an emptyDir unless `persistence.existingClaim` names a claim) are the
  only writable paths;
- the liveness, readiness and startup probes below (HTTPS when `tls.existingSecret` names a
  TLS Secret), and `SAGE_SUPERVISED=1`, so "Restart now" in Settings may apply
  restart-bound fields;
- the `config` value as `config.yaml` in a ConfigMap mounted as a directory, a Service
  (8080 `api`, 8187 `metrics`), and optionally a ServiceMonitor and a NetworkPolicy;
- with `cnpg.cluster` (or `cnpg.rbac.clusters`), a Role that grants only `get` on the named
  `clusters.postgresql.cnpg.io` and binds it to the chart's service account; the token is
  mounted only then. See [CloudNativePG](managed-clouds.md#cloudnativepg).

Without `secrets.encryptionKeyKey` pg_sage generates `data_dir/encryption.key`; on the
default emptyDir a pod restart loses it and the secrets sealed under it, so set an
encryption key or a persistent claim. Every value is listed in the chart's
[README](https://github.com/jasonmassie01/pg_sage/blob/master/deploy/helm/pg-sage/README.md).
To deploy without Helm, `helm template sage deploy/helm/pg-sage -f my-values.yaml` prints
plain manifests.

### ConfigMap edits and hot reload

An edit to the chart's ConfigMap (`<release>-pg-sage-config`) hot-reloads without a restart. The
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
      - targets: ["pg-sage:8187"]
```

The separate listener (`prometheus.listen_addr`, default `127.0.0.1:8187`) has
no authentication and no TLS. Until v2.7.0 the default port was 9187,
postgres_exporter's default; v2.7.0 moved it to 8187 so both run on one host.
Update scrape configs and published ports that relied on the old default, or
keep it with `SAGE_PROMETHEUS_PORT=9187`. If pg_sage cannot bind the address it
logs an ERROR, keeps running, and
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

### Alert rules and the metrics reference

[Metrics reference](metrics.md) lists every family with its type, labels, the
modes that render it and its meaning; it is generated from the registry the
sidecar renders `/metrics` from, so it cannot name a family that does not
exist. Alert rules ship in
[`deploy/prometheus/pg_sage.rules.yml`](https://github.com/jasonmassie01/pg_sage/blob/master/deploy/prometheus/pg_sage.rules.yml)
(database down, collector stale, LLM circuit open, failed and rolled-back
actions, self-budget exceeded, SLO paging, old Sage SRE investigations, an API
TLS certificate within 14 days of expiry, a stale leader lease, failing SSO
discovery, and approvals left waiting for a day), with
`promtool test rules` tests beside them. Load the file with `rule_files:`; each
alert's `runbook_url` opens its runbook in the reference.

### Metric renames in v2.7.0

v2.7.0 is the one release that removes and renames metric families. Update
dashboards, recording rules and alerts from the old name to its replacement:

<!-- metrics-renames:start -->

| Old name | Use instead | Note | Release |
|---|---|---|---|
| `pg_sage_findings_total` | `pg_sage_findings_open` | per database and severity | v2.7.0 |
| `pg_sage_collector_last_run_timestamp` | `pg_sage_collector_last_success_timestamp_seconds` | per database | v2.7.0 |
| `pg_sage_fleet_instance_findings` | `pg_sage_findings_open` | sum by database | v2.7.0 |
| `pg_sage_fleet_instance_health` | `pg_sage_database_health_score` | - | v2.7.0 |
| `pg_sage_connections_total` | - | use postgres_exporter's pg_stat_activity_count | v2.7.0 |
| `pg_sage_cache_hit_ratio` | - | use postgres_exporter's pg_stat_database_blks_hit and pg_stat_database_blks_read | v2.7.0 |
| `pg_sage_optimizer_recommendations_total` | `pg_sage_optimizer_recommendations` | renamed: a gauge, not a counter | v2.7.0 |
| `pg_sage_fleet_findings_total` | `pg_sage_fleet_findings_open` | renamed: a gauge, not a counter | v2.7.0 |

<!-- metrics-renames:end -->

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

### Sage SRE investigation backlog

Each database whose Sage SRE investigator runs refreshes its investigations from the
coordination store (the meta database when there is one) on the metrics refresh cadence,
one bounded read-only query per database (2 s `statement_timeout`).

| Metric | Type | Labels |
|---|---|---|
| `pg_sage_sre_investigations` | gauge | `database`, `state` |
| `pg_sage_sre_investigation_oldest_open_age_seconds` | gauge | `database` |

- `state` is one of the ten investigation states. Every live state (`queued`,
  `collecting`, `evaluating`, `needs_evidence`, `paused`) is sampled, zero included;
  a terminal state (`concluded`, `inconclusive`, `cancelled`, `expired`, `failed`) only
  when an investigation reached it in the past 24 hours (by `updated_at`).
- The oldest-open age is 0 when no investigation is live. It is computed at scrape time
  from the last refresh, so it keeps growing between refreshes.
- A failed backlog read keeps the previous sample, so a slow refresh does not blank the
  series; `pg_sage_metrics_refresh_success{source="sre"} 0` shows it. Both families are
  absent, HELP included, when no database runs Sage SRE.

### Loop health

`pg_sage_self_loop_busy_seconds_total{loop}` and `pg_sage_self_loop_runs_total{loop}`
are joined by:

- `pg_sage_self_loop_errors_total{loop}` (counter): runs that failed or produced nothing.
  The collector counts a cycle the circuit breaker skipped or whose collection or
  snapshot persist failed; the analyzer a cycle with no fresh snapshot or whose findings
  could not be persisted or filtered; `sre` a coordinator tick that could not reach its
  store, bind the database, read triggers, list pending work or apply retention;
  `briefing` a failed generation. The executor and retention loops log each step's
  failure and carry on, so they report runs but no cycle-level errors.
- `pg_sage_self_loop_last_success_timestamp_seconds{loop}` (gauge): Unix time the loop's
  last successful run ended, omitted until it has one.

`loop` is one of `analyzer`, `collector`, `executor`, `retention`, `briefing`, `sre` and
`metrics_refresh`. These are process-wide (in fleet mode every database's collector counts
as the `collector` loop); per-database freshness is
`pg_sage_collector_last_success_timestamp_seconds{database}`.

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
  (`value`, `actions`, `slo`, `optimizer`, `sre`) succeeded. A failed source's series are
  omitted rather than shown stale or zero, except `sre`, which keeps its last sample.
- `pg_sage_metrics_refresh_timestamp_seconds{database,source}`: Unix time of the last
  successful refresh of `source`; it keeps its value while refreshes fail.
- `pg_sage_value_metrics_up{database}`: 1 if the last refresh read the database's value
  ledger.
- `pg_sage_connection_up`: the last pool health check (every 30 s) in standalone and meta
  mode; in YAML fleet mode, 1 if any database's last refresh reached it. Before the first
  result there is no sample.
- A database that failed to connect or prepare has no refresher; it keeps reporting
  `pg_sage_value_metrics_up 0` and `pg_sage_metrics_refresh_success{source="value"} 0`.

### Executor outcomes

Since v2.7.0 `/metrics` reports what the executor did, per database (observability-04):

| Metric | Type | Meaning |
|---|---|---|
| `pg_sage_actions_total{database,class,outcome}` | counter | Actions that ended. `outcome` is `success`, `failed` (also a failed rollback or a partial change), `rolled_back` (also an index already restored outside pg_sage), `cancelled`, `unverifiable` (also an expired watch) or `other` (a rollback skipped or withheld for a person) |
| `pg_sage_verification_verdicts_total{database,class,verdict}` | counter | Verifications finalized: `success`, `revert`, `unverifiable`, `failed` or `other` |
| `pg_sage_actions_pending_approval{database,risk}` | gauge | Approvals a person can act on now (pending, past any cooldown, not expired), by `risk` (`safe`, `moderate`, `high`, `other`) |
| `pg_sage_action_last_failure_timestamp_seconds{database}` | gauge | When the latest `failed`, `partial` or `rollback_failed` outcome was recorded; no sample if none was |

- `class` is the policy change class of the action (`index`, `vacuum`, `config_guc`,
  `agent_access` for agent guard contracts, and the other classes a policy document names),
  or `other`. The label sets are fixed, so these families never grow with your workload.
- Only outcomes count, once their write has committed: an action still `pending`,
  `monitoring` or `rolling_back` has not ended, and a write that failed counts nothing. One
  action can count twice, for example `success` and later `rolled_back` when an operator
  reverts it.
- The two counters live in the sidecar's memory: they start at zero when it restarts, as
  Prometheus counters do. Alert with `increase()` or `rate()`, never on the raw value. The
  two gauges are read from each database's action ledger by its refresher (source
  `actions`), so they survive a restart.
- In HA only the sidecar that executes (the leader) counts; `sum` across replicas.
- Earlier versions of this guide recommended a failed-action alert on a family that was
  never exported, so it could not fire; use the `pg_sage_actions_total` rows above.

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
| `pg_sage_time_to_first_finding_seconds{database}` | Seconds from start to the first finding: a warning or critical catalog item of the first look, else the analyzer's first open warning or critical finding. Setup and posture notes and info findings do not count; absent until a finding exists |
| `pg_sage_setup_within_hour_ratio{part}` | Of the new installs this process monitors, the share whose model (`part="model"`) or notification channel (`part="channel"`) the setup wizard recorded within one hour of install (exactly 3,600 s counts); absent until a new install is known |
| `pg_sage_first_look_items{database}` | Every item of the catalog-only first look (findings, setup and posture notes) |
| `pg_sage_first_look_findings{database}` | The first look's findings only: warning or critical catalog items, without setup or posture notes |
| `pg_sage_first_look_duration_seconds{database}` | How long the first look took |

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
| `pg_sage_llm_tokens_total{client,database,purpose,kind}` | Tokens charged since start; `purpose` is the feature that made the calls (`briefing`, `explain`, `query_tuning`, ...; `unlabeled` when none); `kind` is `prompt`, `completion` or `unattributed` (estimated, or not split by the provider). The spend signal; costs are in `GET /api/v1/llm/usage` |
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

Audit health (scheduled chain verification and SIEM export, which run on the fleet leader
only) is listed in [security.md](security.md#monitoring-audit-health):
`pg_sage_audit_chain_ok`, `pg_sage_audit_chain_verified_timestamp_seconds`,
`pg_sage_audit_chain_problems`, `pg_sage_audit_chain_verify_errors_total`,
`pg_sage_siem_lag_links`, `pg_sage_siem_consecutive_failures` and
`pg_sage_siem_delivered_total`.

The families removed in v2.7.0 are listed in [Metric renames in v2.7.0](#metric-renames-in-v270).

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
  `pg_sage_policy_record_failures_total`, `pg_sage_actions_total`,
  `pg_sage_verification_verdicts_total`, `pg_sage_shadow_*_total`) included. A database
  added again under the same name counts from zero, which `rate()` treats as a counter reset.
- Series are written in the same order on every scrape, and every family has one `HELP` and
  one `TYPE` line.

One gauge still carries the counter suffix `_total`:
`pg_sage_incidents_avoided_total`, which falls when a rollback voids credit. Do not apply
`rate()` or `increase()` to it.

### Grafana Dashboard

Import the included dashboard from `grafana/pg_sage_dashboard.json`. Its `job`, `instance`
and `database` variables keep two sidecars, or the databases of a fleet, apart. Its rows
cover the sidecar, the monitored databases, the executor and trust, LLM use, Sage SRE and
SLOs, and value and footprint; [grafana/README.md](https://github.com/jasonmassie01/pg_sage/blob/master/grafana/README.md) lists every panel.
It charts no PostgreSQL internals: use postgres_exporter and its community dashboard for
those.

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

- **A second start runs no DDL.** pg_sage records every migration step it ran
  in `sage.schema_migrations` (step name, SHA-256 of its SQL, when, and the
  pg_sage version), together with a fingerprint of the `sage` catalog the last
  complete start left. While the catalog still matches that fingerprint, a
  start skips every recorded step; only their data statements (small
  backfills of rows an older pg_sage may have written) run again. When the
  catalog differs (the first start of a version with the ledger, a start that
  was cut off, an older pg_sage or a person having changed the schema, an
  `INVALID` index), the start trusts nothing: before each step it reads the
  catalog, a column, index or table that already exists is not touched, and
  missing objects are created again. An `INVALID` index (left by a killed
  `CREATE INDEX CONCURRENTLY`) is dropped and rebuilt by the step that owns it;
  for an `INVALID` index no step builds, the start logs `no step rebuilds this
  index; drop it` with the `DROP INDEX CONCURRENTLY` and `REINDEX INDEX
  CONCURRENTLY` commands, and keeps distrusting the ledger until you do.
- **Large indexes are built in the background.** The decision-ledger indexes
  (`idx_decision_fingerprint`, `idx_decision_created`,
  `idx_decision_action_log`, `idx_decision_queue`,
  `idx_verification_decision`, `idx_change_lease_decision`,
  `idx_incident_avoided_decision`, `idx_schema_baseline_decision`) are not
  built by a start when their table has a million rows or more
  (`pg_class.reltuples`): a build that size would not fit the startup
  deadline and would leave an `INVALID` index each start. The start leaves
  them out, and a background builder, one per database, builds them with
  `CREATE INDEX CONCURRENTLY` once the database is running: on a dedicated
  session, under its own advisory lock (a second sidecar on the same
  database logs "another sidecar is building" and waits), with
  `lock_timeout = 2s`, no `statement_timeout`, and the server's
  `maintenance_work_mem`. A build that fails (a lock it cannot get within 2
  seconds, a long transaction it would wait for) is retried after 1 minute,
  doubling to at most 1 hour, with one `WARN` per run of failures; an
  interrupted build leaves an `INVALID` index that the next run drops and
  builds again. Each built index logs one `INFO` line. Stopping the sidecar,
  removing the database from the fleet, or an emergency stop cancels a running
  build on the server. `pg_sage_schema_deferred_indexes{database,state}`
  counts the indexes still `missing`, `building` or `invalid`; it reads 0
  once they are built. On a large install, some of these indexes therefore
  appear a few minutes after an upgrade. These indexes do not count against
  the fingerprint above, so their builds never make a start distrust the
  ledger. Managed PostgreSQL services allow `CREATE INDEX CONCURRENTLY` on
  tables pg_sage owns.
- **A changed step stops the start.** A released migration step never
  changes. If the step recorded in `sage.schema_migrations` has another
  checksum than this build's, startup fails naming the step, for example
  `migration step changed since it was applied: ddlUsersOAuth was recorded with checksum ...`.
  This means the build is not a release (or the ledger was edited). To run
  the step again through its catalog checks, delete its row and restart:
  `DELETE FROM sage.schema_migrations WHERE id = 'ddlUsersOAuth';`.
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
  their self-checking or data statements, `skipped` steps found everything in
  place, and `N recorded` (when present) counts the steps the ledger skipped
  without reading the catalog. Each applied step is also logged at DEBUG with
  its duration.

If a start fails on a lock, find the holder with
`SELECT pid, mode, query FROM pg_locks JOIN pg_stat_activity USING (pid) WHERE relation::regclass::text LIKE 'sage.%';`
and wait for it to finish, or restart pg_sage after it.

### Docker

```bash
docker pull ghcr.io/jasonmassie01/pg_sage:latest
docker rm -f pg_sage
# Re-run with same config
```
