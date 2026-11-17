# Configuration

pg_sage reads its configuration from these sources, highest precedence first:

1. **Settings overrides**: values saved in Settings or through `PUT /api/v1/config/global`,
   stored in the control database (`sage.config`)
2. **CLI flags** (`--pg-url`, `--config`, `--prom-addr`, `--meta-db`, `--encryption-key`)
3. **Environment variables** (`SAGE_DATABASE_URL`, `SAGE_LLM_API_KEY`, etc.)
4. **YAML config file** (`config.yaml`)
5. **Built-in defaults**

**Trust takes the most restrictive value.** For `trust.level`, `trust.tier3_safe`,
`trust.tier3_moderate` and `trust.tier3_high_risk`, when the YAML file, an environment
variable or a flag sets the key explicitly, the effective value is the more restrictive of
that value and a stored override: the lower trust level, and a tier3 switch only when both
are on. `SAGE_TRUST_LEVEL=observation`, or `trust.level: observation` in the YAML during an
incident, therefore holds even though an old Settings override says `autonomous`; removing
the file key lets the override apply again. An explicit value counts even when it equals the
default. Every other key keeps the override winning.

**Masked overrides are reported.** When a stored override replaces a different explicit
file value (YAML, environment or flag), the key is *masked*: the sidecar logs one WARN per
distinct pair of values, naming the key, both values, the user who stored the override and
when, and `GET /api/v1/config/global` lists it in `masked_by_override` (an empty array when
nothing is masked; secret values are redacted). Settings shows a **masked** badge on the
key. The source badge says `override`, `db_override` or `file/default`: the file, an
environment variable, a flag or the built-in default (the sidecar does not track which).
Reset in Settings, or `DELETE /api/v1/config/global/<key>`, removes a stored override. In
meta-db and fleet mode the same rule applies to the global overrides; per-database trust
already takes the stricter of the database's level and the global ceiling.

The sidecar validates a complete candidate before publishing a YAML reload.
Every field has a typed lifecycle: `live_policy` swaps an immutable policy
snapshot, `reconfigure` tears down and rebuilds its named runtime owner, and
`restart` remains pending until process restart. A `reconfigure` or
`live_policy` field whose owner does not run in the current mode is treated
as `restart`. In-flight work keeps its original snapshot.

**Five owners rebuild in place.** The sidecar registers `trust_policy`,
`llm` (the LLM connection: `llm.enabled`, `llm.endpoint`, `llm.api_key`,
`llm.model`, `llm.auth` and its timeouts), `llm_redaction`
(`llm.redact_numeric_literals`) and `briefing` (`briefing.schedule`,
`briefing.channels`, `briefing.slack_webhook_url`) in every mode, and
`fleet_databases` (`databases` and `defaults.*`) in YAML fleet mode. A
briefing schedule that does not parse is rejected when it is applied, naming
`briefing.schedule`, and the workers keep the old one. Every other field,
including `alerting.*`, `briefing.fleet_mode` and the collector, analyzer, tuner,
`schema_lint` and `auto_explain` intervals, is `restart`: a change made from
Settings, the API or the YAML file is saved and reported as pending restart.
Settings badges each field `live` or `restart`, lists the pending fields with
their running and saved values, and offers Restart now only under a declared
supervisor (`SAGE_SUPERVISED=1`, see
[deployment](deployment.md#restarting-from-settings-sage_supervised)).
A derived `collector.interval_seconds` is the exception on the derivation side:
the collector re-reads it every cycle, so a promoted value applies live (see
[derived settings](generated/derived-settings.md)).

**Trust ceilings are live.** `trust.level`, `trust.tier3_safe`,
`trust.tier3_moderate`, `trust.maintenance_window`, `trust.rollback_threshold_pct`,
`trust.rollback_window_minutes`, `trust.rollback_cooldown_days` and
`trust.cascade_cooldown_cycles` are `live_policy` fields of the `trust_policy` owner.
Saving one in Settings, through `PUT /api/v1/config`, or in the watched YAML reaches every
database's executor before its next authorization, in standalone, YAML fleet and meta-db
mode; none of them is reported as `pending_restart`. A raise is live too (the API answers
with a "ceiling raised" warning), and the trust ledger still gates every promotion beneath
it. If the change fails part-way, each executor keeps the stricter of its old and the new
ceiling (a tier3 switch stays on only if both have it on; the other bounds keep their old
value), so a failure never raises autonomy. `trust.tier3_high_risk` and the trust ramp
(`trust.ramp_*`) still need a restart.

**Replicas share the global settings and converge within 30 seconds.** A setting saved in
Settings or through the API is a global override stored in the control database, and it
takes precedence over each replica's YAML (except the trust rule above). Replicas on one control database share these
overrides: meta-db mode (every sidecar on one `--meta-db`) and standalone replicas on one
monitored database. Each replica reads the durable config generation every 30 seconds (one
small query) and adopts a newer one the way a local save applies: its current YAML file plus
the stored overrides, validated as a whole, live fields rebuilt by their owners, `restart`
fields reported as pending restart. A save based on a stale generation is still refused with
`409`, but the refusing replica resyncs first, so the `409` body's `desired_generation` (and
a refetch of `GET /api/v1/config/global`) is the fresh one and the retry succeeds. A YAML hot
reload that loses this race takes effect through the same resync. If the stored overrides do
not validate on a replica (for example `clone.provider: dle` where that replica's YAML has no
endpoint), the replica keeps its current config and logs an ERROR naming the key; an
unreadable generation logs a WARN, and the next tick retries. `GET /api/v1/config` and
`GET /api/v1/config/global` report `last_resync_at`: when the replica last adopted a
generation another writer saved (`null` if it never has). A YAML fleet has no shared
overrides: each sidecar's file is its configuration, and nothing is polled.

**Restarting from the dashboard** is offered only under a declared supervisor: set
`SAGE_SUPERVISED=1` when a supervisor (systemd, Kubernetes, the Databricks Apps launcher)
relaunches the sidecar after it exits with code 42. Without it the restart endpoint answers
501 instead of exiting into nothing.

**Adding, removing and changing databases without a restart.** In YAML fleet
mode, editing `databases` (or `defaults`) in the watched config file is
applied at once: a new entry starts a runtime, a removed entry is retired, a
renamed entry is a removal plus an addition. Per-database `trust_level`,
`execution_mode`, `executor_enabled` and `tags` apply in place to the running
database; any other per-database change (connection, credentials, pool size,
intervals, `llm_enabled`, `verify.io_capacity`) rebuilds only that database's
runtime. A retired runtime first lets in-flight actions finish (up to 60
seconds; new actions on it park and are retried by the next runtime), then
stops its workers, Sage SRE investigator, executor and pool, and its metrics
and LLM budget share disappear. A reload is all-or-nothing: an invalid file,
an invalid per-database trust level or execution mode, or a changed database
whose new connection fails is rejected and the running runtimes keep going
(fix the file and save again). A newly added database that cannot connect is
shown as failed, as at startup. Removing or reconnecting the control database
(the first database that started, which holds logins and standing policy)
still needs a restart. In meta-db mode `sage.databases` is the source of
truth: the managed-database API applies changes immediately, and rows added,
removed, disabled or changed by another replica or by SQL are picked up within
30 seconds with the same rules.

### Where pg_sage keeps its data

pg_sage bootstraps the `sage` schema in every monitored database and, with
`--meta-db`, in the metadata database too. What is written where:

| Data | Standalone / YAML fleet | Meta-db mode |
|---|---|---|
| Logins, sessions, MCP tokens, notification channels and rules, standing policy | the control database (standalone: the monitored database; YAML fleet: `fleet_learning.control_database` when it connected, else the first database that started) | metadata database |
| Leader lease and fleet learning state | the control database; with `fleet_learning.control_database` set, only that database (while it is down every sidecar follows) | metadata database |
| Sage SRE investigations, SLOs, change events; the earned-trust ledger (levels, outcomes, proposals, shadow evidence) | control database | metadata database |
| Telemetry history: snapshots, query store | each monitored database | each monitored database; with `history.store: meta`, the metadata database |
| Other history: explain cache, findings, incidents, recommendations, action log, verification outcomes, decision ledger, shadow decisions, runway and size samples | each monitored database | each monitored database |

Running more than one sidecar for a fleet (HA) needs a pinned control database:
`meta_db`, or `fleet_learning.control_database` set to the same `databases:` entry on
every sidecar. Without a pin, a sidecar that starts while the first database is down
uses another one, and two sidecars can lead at once; such a sidecar logs a WARN at
startup and refuses leadership once it sees another live sidecar on its lease (see
[Leader election](fleet.md#leader-election)).

Telemetry history is what makes pg_sage heavy: the collector's snapshots and the
per-query samples (on a busy database, gigabytes before delta storage). In meta-db
mode, `history.store: meta` keeps it in the metadata database instead, so the
monitored database only holds pg_sage's small working set. Each row carries its
database's meta-db record id (`database_id`), every history read is scoped to it, and
reads that combined history with the monitored database's catalog (plan fingerprints
from the explain cache, `pg_index` for verification, the first-run check) are split
into a catalog read on the monitored database and a history read in the store.

The rest of the history stays with the database it describes: findings, the action
log, its outcomes, verification, the decision ledger and recommendations reference each
other through foreign keys, and part of that graph cannot move to another database. Its
cost there is bounded: catalog snapshots are stored as changes against a keyframe,
history tables are partitioned by day where they grow fastest, `retention.*` and
`retention.snapshots_max_pct` cap them, and `self_budget.storage_mb` (default 10 GB)
raises `sage_self_budget` when the `sage` schema outgrows it.

#### Keeping telemetry history in the meta database (`history.store`)

| Parameter | Default | Description |
|---|---|---|
| `history.store` | `monitored` | `monitored` keeps snapshots and the query store in each monitored database. `meta` keeps them in the metadata database (`meta_db` is required; YAML fleet and agent databases, which have no meta-db record, are refused). Restart-bound, YAML only. |

What changes with `meta`:

- The metadata database gets `database_id` on `sage.snapshots` and `sage.query_store`,
  indexes leading with it, and `sage.history_store_databases` (which databases keep
  history there). Monitored databases' schemas do not change.
- Retention of the two tables runs once per process on the metadata database, with
  the same `retention.snapshots_days` and `retention.query_store_days` for every
  database. `retention.snapshots_max_pct` caps the shared table at the sum of every
  database's cap (that percent of each database's size, at least 256 MB each).
- `sage_footprint` keeps measuring the monitored database's `sage` schema, where
  history no longer grows; its finding says where history lives.
  `self_budget.storage_mb` counts the monitored `sage` schema plus this database's
  share of the history store.

Switching placement needs the history moved first. A database whose history would be
split is refused at startup, and the error names the command to run:

```bash
# 1. stop pg_sage
# 2. copy the database's history into the metadata database (the source is only read)
export SAGE_HISTORY_MONITORED_DSN='postgres://sage@db1/app'
export SAGE_META_DB='postgres://sage@meta/sage_meta'
pg_sage history migrate --to meta --database app     # or --database-id 7
pg_sage history status --database app
# 3. set history.store: meta and start pg_sage
# 4. once it runs, remove the copied rows from the monitored database
pg_sage history migrate --to meta --database app --cleanup
```

The copy is idempotent and resumable: each batch commits with its progress, so an
interrupted run continues where it stopped, and a second run copies only rows written
since. `--cleanup` removes the source rows only when every row is copied: it truncates
the monitored database's two tables (or, from the metadata database, deletes that
database's rows a day at a time). `--to monitored` moves history back the same way. A
snapshot delta whose keyframe is already gone (it read as nothing before) is skipped and
counted.

The generated [per-field lifecycle reference](generated/config-lifecycles.md)
is the authoritative list. Regenerate it from the typed registry with:

```bash
cd sidecar
go run ./cmd/gen_config_meta -lifecycle-only \
  -lifecycle-out ../docs/generated/config-lifecycles.md
```

---

## CLI Flags

```bash
./pg_sage --pg-url "postgres://user:pass@host:5432/db" --config config.yaml
```

| Flag | Description |
|---|---|
| `--pg-url` | PostgreSQL connection string (overrides YAML and env) |
| `--config` | Path to YAML config file |
| `--prom-addr` | Prometheus listen address (e.g., `0.0.0.0:8187`) |
| `--meta-db` | Metadata database connection string (PostgreSQL URL for fleet mode) |
| `--encryption-key` | Encryption key for sensitive config values |

---

## Environment Variables

Every `SAGE_*` variable the sidecar reads is listed here; the table is generated from
`sidecar/internal/config/env_registry.go`. A variable applies when it is set and not empty,
so `0` and `false` are values. A variable that is set but cannot be parsed (for example
`SAGE_PG_PORT=abc` or `SAGE_MCP_ENABLED=no`) stops startup with an error that names every
such variable; range checks (a port from 1 to 65535, positive windows) then apply as for the
YAML value. Booleans accept `true`/`false`, `1`/`0`, `t`/`f`.

Variables that only the test suites and benches read (`SAGE_TEST_*`, `SAGE_BENCH_*`,
`SAGE_SAFETY_BENCH_*`) are listed under "Test environment" in
[CONTRIBUTING.md](https://github.com/jasonmassie01/pg_sage/blob/master/CONTRIBUTING.md#test-environment).

<!-- env-vars:start (generated by cmd/gen_config_meta; do not edit) -->
| Variable | Default | Description |
|---|---|---|
| `SAGE_CONFIG_PATH` | (none) | Path to the YAML config file when `--config` is not given; otherwise `config.yaml` in the working directory is used if present |
| `SAGE_MODE` | inferred | Operating mode: `standalone`, `fleet` or `meta`; inferred from the config when unset |
| `SAGE_DATABASE_URL` / `_FILE` | (none) | PostgreSQL connection string of the monitored database |
| `SAGE_PG_HOST` | `localhost` | PostgreSQL host (`postgres.host`) |
| `SAGE_PG_PORT` | `5432` | PostgreSQL port (`postgres.port`), 1 to 65535 |
| `SAGE_PG_USER` | `sage_agent` | PostgreSQL user (`postgres.user`) |
| `SAGE_PG_PASSWORD` / `_FILE` | (none) | PostgreSQL password (`postgres.password`) |
| `SAGE_PG_DATABASE` | `postgres` | PostgreSQL database name (`postgres.database`) |
| `SAGE_PG_SSLMODE` | `prefer` | PostgreSQL sslmode (`postgres.sslmode`) |
| `SAGE_PG_MAX_CONNS` | `2` | Max PostgreSQL connections in the pool (`postgres.max_connections`) |
| `SAGE_META_DB` / `_FILE` | (none) | Metadata database connection string; selects meta-db fleet mode |
| `SAGE_PROMETHEUS_PORT` | `8187` | Port for Prometheus metrics, 1 to 65535; keeps the host of `prometheus.listen_addr` (default `127.0.0.1`) |
| `SAGE_PROMETHEUS_ADDR` | (none) | Whole `host:port` of the separate `/metrics` listener (`prometheus.listen_addr`); wins over `SAGE_PROMETHEUS_PORT` when both are set |
| `SAGE_RATE_LIMIT` | `60` | Max requests per minute per IP on the REST API; zero or negative means the default |
| `SAGE_API_TRUSTED_ORIGINS` | (none) | Comma-separated `api.trusted_origins`: browser origins allowed to send cross-origin API writes ([CSRF](security.md#csrf-and-cross-origin-requests)) |
| `SAGE_API_KEY` / `_FILE` | (none) | Legacy config field; the current web/API path uses session login cookies |
| `SAGE_TLS_CERT` | (none) | PEM certificate (chain) file; with `SAGE_TLS_KEY`, the API serves HTTPS only |
| `SAGE_TLS_KEY` | (none) | PEM private key file for `SAGE_TLS_CERT`; set both or neither |
| `SAGE_SUPERVISED` | (unset) | `1` or `true` declares a supervisor that relaunches the sidecar on exit code 42; only then does the dashboard's restart endpoint work (otherwise it answers 501) |
| `SAGE_ENCRYPTION_KEY` / `_FILE` | (none) | Passphrase that encrypts stored secrets, including API-set config secrets ([secrets at rest](security.md#secrets-at-rest)) |
| `SAGE_ENCRYPTION_KEY_PREVIOUS` / `_FILE` | (none) | The previous passphrase during a key rotation |
| `SAGE_DRILL_ID` | (none) | Set by pg_sage, never read: the drill id a `drill.command.restore_argv` program restores under ([restore drills](restore-drills.md)) |
| `SAGE_DRILL_DATABASE` | (none) | Set by pg_sage, never read: the pg_sage name of the database a `drill.command.restore_argv` program restores ([restore drills](restore-drills.md)) |
| `SAGE_CLONE_ID` | (none) | Set by pg_sage, never read: the restore id a `drill.command.destroy_argv` program removes ([restore drills](restore-drills.md)) |
| `SAGE_DATA_DIR` | (none) | Persistent directory; without an encryption key, pg_sage generates `encryption.key` there ([secrets at rest](security.md#secrets-at-rest)) |
| `SAGE_BREAK_GLASS_PASSWORD_HASH` / `_FILE` | (none) | bcrypt hash of the [break-glass admin](security.md#break-glass-admin) password |
| `SAGE_BOOTSTRAP_ADMIN_EMAIL` | (none) | Email of the first admin, created only when the users table is empty |
| `SAGE_BOOTSTRAP_ADMIN_PASSWORD` / `_FILE` | (none) | Password of that first admin; its `_FILE` must be mode 600 or 400 |
| `SAGE_TRUST_LEVEL` | `observation` | Operator trust ceiling (`trust.level`): `observation`, `advisory` or `autonomous` |
| `SAGE_POLICY_PROFILE` | `unattended` | Bootstrap standing-policy profile (`policy.profile`): `staffed` or `unattended` |
| `SAGE_VALUE_TOIL_MODEL_VERSION` | `1` | Active calibrated toil-model version (`value.toil_model_version`) |
| `SAGE_LLM_API_KEY` / `_FILE` | (none) | API key for Gemini or any OpenAI-compatible LLM |
| `SAGE_LLM_AUTH` | `api_key` | `databricks` uses SDK unified authentication with token refresh for Unity Gateway |
| `SAGE_LLM_ENDPOINT` | (none) | LLM API endpoint (`llm.endpoint`) |
| `SAGE_LLM_MODEL` | (none) | LLM model name (`llm.model`) |
| `SAGE_OPTIMIZER_LLM_API_KEY` / `_FILE` | (none) | Separate API key for the optimizer model (optional) |
| `SAGE_OPTIMIZER_LLM_ENDPOINT` | (none) | Separate endpoint for the optimizer model (optional) |
| `SAGE_OPTIMIZER_LLM_MODEL` | (none) | Separate model name for the optimizer (optional) |
| `SAGE_REVIEW_LLM_API_KEY` / `_FILE` | (none) | Separate API key for the Sage Review model (optional) |
| `SAGE_REVIEW_LLM_ENDPOINT` | (none) | Separate endpoint for the Sage Review model (optional) |
| `SAGE_REVIEW_LLM_MODEL` | (none) | Separate model name for Sage Review (optional) |
| `SAGE_LAKEBASE_ENDPOINT` | (none) | Lakebase endpoint resource path for OAuth; applies to target and metadata pools |
| `SAGE_LAKEBASE_ENDPOINTS` | (none) | JSON hostname-to-endpoint map for a fleet spanning Lakebase endpoints; use instead of the singular variable |
| `SAGE_AUTH_MODE` | (none) | `databricks_app` selects Databricks Apps authentication (see Web UI and API Authentication) |
| `SAGE_DATABRICKS_ADMIN_EMAIL` | (none) | Administrator email under `SAGE_AUTH_MODE=databricks_app`; startup fails without a plain address |
| `SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS` | (none) | Comma-separated exact DNS domains allowed under `databricks_app` auth; the administrator must match |
| `SAGE_DATABRICKS_APP_IDENTITY_PIN` | log | What a changed `X-Forwarded-User` for a pinned Databricks App account does: `log` (audit event), `enforce` (also refuse with 403) or `off` |
| `SAGE_OAUTH_PROVIDER` | (none) | OAuth provider (`oauth.provider`): google, github, okta or generic OIDC |
| `SAGE_OAUTH_CLIENT_ID` | (none) | OAuth client ID (`oauth.client_id`) |
| `SAGE_OAUTH_CLIENT_SECRET` / `_FILE` | (none) | OAuth client secret (`oauth.client_secret`) |
| `SAGE_OAUTH_ISSUER_URL` | (none) | OIDC issuer URL (`oauth.issuer_url`) |
| `SAGE_OAUTH_REDIRECT_URL` | (none) | OAuth redirect URL (`oauth.redirect_url`); must match the URI registered at the provider |
| `SAGE_MCP_ENABLED` | `true` | Serve the MCP endpoint (`mcp.enabled`); `false` or `0` turns it off |
| `SAGE_MCP_TRANSPORT` | `http` | MCP transport (`mcp.transport`): `http` (credential required) or `stdio` |
| `SAGE_VERIFY_WINDOW_MINUTES` | `120` | Initial verification window in minutes (`verify.window_minutes`) |
| `SAGE_VERIFY_WINDOW_MAX_MINUTES` | `4320` | Maximum verification window in minutes (`verify.window_max_minutes`) |
| `SAGE_VERIFY_MIN_GAIN_PCT` | `20` | Minimum gain to retain a change (`verify.min_gain_pct`); `0` is allowed |
| `SAGE_VERIFY_REGRESS_PCT` | `15` | Regression percentage that triggers a revert (`verify.regress_pct`) |
| `SAGE_VERIFY_WRITE_IMPACT_PCT` | `20` | Maximum rise of the table's WAL bytes per written tuple (`verify.write_impact_pct`) |
| `SAGE_VERIFY_MIN_SAMPLES` | `30` | Minimum verification samples (`verify.min_samples`) |
| `SAGE_CLONE_PROVIDER` | `none` | Clone provider (`clone.provider`): `none`, `dle`, `lakebase_branch`, `neon_branch`, `aurora_clone`, `rds_restore`, `cloudsql_clone`, `alloydb_restore` or `azure_restore` |
| `SAGE_CLONE_DLE_ENDPOINT` | (none) | Database Lab Engine API endpoint (`clone.dle_endpoint`) |
| `SAGE_CLONE_DLE_TOKEN` / `_FILE` | (none) | Database Lab bearer token (`clone.dle_token`) |
| `SAGE_CLONE_NEON_API_KEY` / `_FILE` | (none) | Neon API key for `neon_branch` rehearsal branches (`clone.neon.api_key`) |
| `SAGE_CLONE_MAX_AGE_MINUTES` | `1440` | Maximum source snapshot age (`clone.max_clone_age_minutes`) |
| `SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT` | `25` | Remaining XID percentage classified red (`custodian.freeze.red_buffer_pct`) |
| `SAGE_CUSTODIAN_WAL_ABANDON_AFTER_MINUTES` | `1440` | Inactive replication-slot abandonment age (`custodian.wal.abandon_after_minutes`) |
| `SAGE_CUSTODIAN_WAL_DISK_PCT_CEILING` | `10` | Retained-WAL disk limit (`custodian.wal.retained_wal_disk_pct_ceiling`) |
| `SAGE_CLOUD_TELEMETRY_ENABLED` | (unset) | Read RDS, Aurora and Cloud SQL host telemetry (`cloud_telemetry.enabled`); `true` or `false` |
| `SAGE_AWS_REGION` | (none) | AWS region when the host does not name it (`cloud_telemetry.aws.region`) |
| `SAGE_AWS_DB_INSTANCE_IDENTIFIER` | (none) | RDS DB instance identifier when the host is not an RDS endpoint; standalone only |
| `SAGE_AWS_DB_CLUSTER_IDENTIFIER` | (none) | Aurora DB cluster identifier when the host is not an Aurora endpoint; standalone only |
| `SAGE_GCP_PROJECT` | (none) | Google Cloud project of the Cloud SQL instance (`cloud_telemetry.gcp.project`) |
| `SAGE_GCP_CLOUDSQL_INSTANCE` | (none) | Cloud SQL instance name when the host is not its IP address; standalone only |
| `SAGE_AZURE_SUBSCRIPTION_ID` | (none) | Azure subscription ID of the flexible server (`azure.subscription_id`) |
| `SAGE_AZURE_RESOURCE_GROUP` | (none) | Azure resource group of the flexible server (`azure.resource_group`) |
| `SAGE_AZURE_SERVER_NAME` | (none) | Flexible server name for hosts that do not name it (`azure.server_name`) |
| `SAGE_SUPABASE_OBSERVABILITY_TOKEN` / `_FILE` | (none) | Supabase Management API token for provider observability (optional) |
| `SAGE_HISTORY_MONITORED_DSN` / `_FILE` | (none) | Source database of the `history` subcommand's copy into the metadata database (only read) |
| `SAGE_VECTORLAB_DATABASE_URL` / `_FILE` | (none) | Database of the `vector-lab` subcommand |
| `SAGE_LINT_DATABASE_URL` / `_FILE` | (none) | Monitored database the `lint-migration` CI gate reads catalog statistics from (only read; not needed with --static) |
<!-- env-vars:end -->

A `_FILE` variant reads the value from the named file (for Docker, Kubernetes
or Vault Agent secrets); set either the variable or its `_FILE`, not both.

### Secrets from files (`*_FILE`)

Every secret pg_sage reads for itself also accepts a file: set `NAME_FILE` to a
path instead of `NAME`. Vault Agent, the Secrets Store CSI driver, External
Secrets, Kubernetes secret volumes and Docker secrets can all fill it; pg_sage
needs no vendor SDK.

| Variable | Also read as |
|---|---|
| `SAGE_DATABASE_URL`, `SAGE_PG_PASSWORD`, `SAGE_META_DB` | `SAGE_DATABASE_URL_FILE`, ... |
| `SAGE_ENCRYPTION_KEY` | `SAGE_ENCRYPTION_KEY_FILE` |
| `SAGE_LLM_API_KEY`, `SAGE_OPTIMIZER_LLM_API_KEY` | `..._FILE` |
| `SAGE_OAUTH_CLIENT_SECRET`, `SAGE_CLONE_DLE_TOKEN`, `SAGE_API_KEY` | `..._FILE` |
| `SAGE_SUPABASE_OBSERVABILITY_TOKEN` | `SAGE_SUPABASE_OBSERVABILITY_TOKEN_FILE` |
| `SAGE_VECTORLAB_DATABASE_URL`, `SAGE_HISTORY_MONITORED_DSN` (subcommands) | `..._FILE` |
| any `${NAME}` referenced in the YAML (fleet passwords, webhook secrets) | `NAME_FILE` |

Rules:

- Setting both `NAME` and `NAME_FILE` is a startup error; neither silently wins.
- The value is the file's content minus **one** trailing newline (LF or CRLF).
  An empty file, or one over 64 KiB, is a startup error.
- A file other users can read or write draws a startup warning; `chmod 600`
  (or `400`) it. Group access is fine, so Kubernetes `fsGroup` mounts don't warn.
- Errors name the variable and path, never the content.
- CLI flags (`--pg-url`, `--meta-db`, `--encryption-key`) still win over both.
- A config hot reload re-reads files behind `${NAME}` references, so a rotated
  file takes effect for hot-reloadable keys.

---

## YAML Config File

Full example (see also `sidecar/config.example.yaml`):

```yaml
mode: standalone

postgres:
  host: your-instance-ip
  port: 5432
  user: sage_agent
  password: ${PGPASSWORD}       # env var expansion supported
  database: postgres
  sslmode: require
  max_connections: 2

collector:
  interval_seconds: 60

analyzer:
  interval_seconds: 600

trust:
  level: observation             # observation | advisory | autonomous
  maintenance_window: "weeknights"  # see "Maintenance windows" below
  ramp_start: ""                 # Auto-persisted on first start; set to override

llm:
  enabled: true                  # default; false turns every LLM feature off
  endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"
  model: "gemini-2.5-flash"
  api_key: ${SAGE_LLM_API_KEY}
  timeout_seconds: 30
  token_budget_daily: 500000
  optimizer:
    enabled: true                # default
    min_query_calls: 100         # ignore ad-hoc queries below this threshold
    max_indexes_per_table: 10    # cap of valid indexes; a replacement adds none
    max_include_columns: 3       # refuse proposals with a wider INCLUDE list
    max_new_per_table: 3
    write_heavy_ratio_pct: 70    # % inserts + non-HOT updates vs those + tuples read
    write_impact_threshold_pct: 15  # measured gain a write-heavy table needs
    rejection_memory:            # skip re-measuring ideas HypoPG already rejected
      enabled: true
      max_age_days: 7
      call_volume_ratio: 2       # material change: calls or mean time 2x, rows 2x,
      mean_time_ratio: 2         # or a target query appears/disappears
      row_estimate_ratio: 2
      prompt_max_shapes: 5
      skip_llm_after: 3          # stop asking about a case after 3 wasted answers
  optimizer_llm:                 # optional second model for optimizer
    endpoint: ""
    model: ""
    api_key: ${SAGE_OPTIMIZER_LLM_API_KEY}  # needed when endpoint is on another origin

tuning:                          # one case-driven tuning agent per database
  enabled: true
  max_cases_per_cycle: 3
  max_requests_per_cycle: 12     # per database per analyzer cycle
  max_tokens_per_cycle: 60000
  calibration_min_outcomes: 5    # below this a proposal is "uncalibrated"

api:
  listen_addr: "0.0.0.0:8080"
  # Web UI and /api/v1 endpoints are session-authenticated.
  # The first local admin is bootstrapped automatically.

alerting:
  enabled: false
  check_interval_seconds: 60
  slack_webhook_url: ${SAGE_SLACK_WEBHOOK}
  pagerduty_routing_key: ${SAGE_PAGERDUTY_KEY}
  routes:
    - severity: critical
      channels: [slack, pagerduty]

prometheus:
  listen_addr: "0.0.0.0:8187"   # "" = no separate listener
  serve_on_api: false           # true = also GET /metrics on the API listener (token)
  refresh_interval_seconds: 0   # 0 = 30 s; never faster than collector.interval_seconds

retention:
  snapshots_days: 90
  findings_days: 180
  actions_days: 365
  explains_days: 90
  sage_size_warning_pct: 10

briefing:
  schedule: "0 6 * * *"         # cron expression; "" turns briefings off
  channels: ["stdout"]          # stdout, slack
  slack_webhook_url: ""
  fleet_mode: rollup            # fleet only: rollup (one message) or per_database
```

A briefing covers the time since the previous briefing (on the first run,
since the schedule's previous fire time), so a weekly schedule reports a week
of actions; the structured text names the window ("Recent Actions (7d)").
Slack receives the briefing as mrkdwn: headings and `**bold**` become `*bold*`,
and `<`, `>` and `&` are escaped, so text from the model or the database
cannot ping `<!channel>` or post a link. Each channel's result is stored in
`sage.briefings.delivery_status`. In fleet mode, YAML or meta-db,
`briefing.fleet_mode: rollup`
(the default) posts one message with a section per database; each database
still stores its own briefing row, and every row records that message's
result. `per_database` posts one message per database, as before. Changing
`fleet_mode` needs a restart; it is ignored in standalone mode.

Stored briefings are readable by every signed-in role:
`GET /api/v1/briefings?database=<name|all>&limit=<1-100, default 20>` lists them newest first,
and `GET /api/v1/briefings/latest?database=<name|all>` returns each database's latest one
(`without_briefing` names the databases that have none yet). Both return `content_json` and
`delivery_status` exactly as stored; in a fleet, a database whose read fails is listed under
`unavailable` while the others answer. When no database has a briefing yet, `latest` answers
404 with the reason. The Overview page shows the latest briefing in a card, and MCP clients
read it as the resource `pgsage://databases/{database}/briefings/latest`
([MCP resources](mcp.md#resources)).

---

## Key Settings Reference

### One range per setting

Every setting the API can override has one value contract, declared once on its field in
`internal/config` (struct tags `api`, `range`, `enum` and `zero`) and read by the config-file
loader, the settings API (`PUT /api/v1/config/global` and the per-database endpoint), the replay
of stored overrides and the dashboard (`config_meta.json` carries `min`, `max` and `enum`, and
the Settings page refuses an out-of-range value before it saves). A config file with a value
outside the range fails to load and the error names the key; the API answers 400 naming the key.

| API type | Range |
|---|---|
| `int_pos` | `1`-`1073741824` |
| `int_nonneg` | `0`-`1073741824` |
| `int_min5` | `5`-`1073741824` |
| `pct` / `pct1_100` | `0`-`100` / `1`-`100` |
| `float01` / `float_share` | `0`-`1` / `0.01`-`1` |
| `float_pos` / `float_nonneg` / `float_pct_pos` | `0.001`-`1e9` / `0`-`1e9` / `0.001`-`100` |
| `int_week_minutes`, `int_class_timeout`, `int_vacuum_stall` | `1`-`10080`, `60`-`86400`, `5`-`240` |
| `float_pct_open` | greater than `0` and less than `100`, both bounds refused (`custodian.freeze.red_buffer_pct`) |
| `bool_auto` | `true`, `false` or `auto` (`logwatch.enabled`) |

A key with its own `range` tag uses that range instead: `analyzer.self_cost_budget_ms` is
`0`-`3600000`, `rca.dedup_window_minutes` `1`-`525600` (it must also fit inside
`rca.stale_after_hours`) and `analyzer.slow_slot_retained_bytes` `1`-`1099511627776` (1 TiB;
its 1 GiB default is the generic integer ceiling, so the ceiling would only let it go down). Keys marked `zero:"default"` also take `0` in a config file, meaning
the default; the API refuses a stored `0` for them, because deleting the override is how the API
restores a default: `analyzer.inactive_slot_min_minutes`, `analyzer.slow_slot_retained_bytes`,
`safety.host_cpu_ceiling_pct`, `safety.index_build_timeout_seconds`,
`safety.reindex_timeout_seconds`, `safety.vacuum_timeout_seconds`,
`safety.vacuum_stall_minutes`, `schema_lint.min_table_rows`, `migration.ddl_row_threshold`,
`migration.queue_waiters_threshold` and `trust.cascade_cooldown_cycles`.

Ranges that changed in tranche T4 (config-02b), where the file and the API used to disagree:

- The config file now refuses what the API already refused: `0` (and negatives) for every
  `int_pos` key without `zero:"default"` (for example `llm.timeout_seconds`,
  `safety.lock_timeout_ms`, `retention.*_days`, `explain.*`, `logwatch.*`, `rca.*` counts,
  `analyzer.lock_chain.*` minutes and thresholds, `llm.optimizer.*`); `4` and below for the
  `int_min5` intervals; any integer above `1073741824` (above `1099511627776` for
  `analyzer.slow_slot_retained_bytes`); percentages outside `0`-`100`
  (`analyzer.*_pct`, `rca.connection_saturation_pct`); `analyzer.cache_hit_ratio_warning`
  outside `0`-`1`; `analyzer.wraparound_freeze_share` below `0.01`; negative
  `llm.fleet_token_budget_daily` and `llm.global_token_budget_daily`; float settings above
  `1e9`; and enum values outside `policy.profile`, `mcp.transport` and `llm.auth`.
- The API now accepts `llm.token_budget_daily: 0` (no cap, as documented) and
  `alerting.cooldown_minutes: 0` (the per-severity floors alone), which the file accepted.
- The API now refuses `analyzer.self_cost_budget_ms` above `3600000` and
  `rca.dedup_window_minutes` above `525600`, which the file refused.
- `clone.provider: lakebase_branch` stays file-only: the API refuses it, because its
  `clone.lakebase` project binding is file-only and restart-bound. `neon_branch` is
  refused the same way: its `clone.neon` project and API key are file or env only, as are
  `aurora_clone` and `rds_restore`: their `clone.aurora` and `clone.rds` regions are
  file-only, `cloudsql_clone`: its `clone.cloudsql` project and instance are file-only,
  `alloydb_restore`: its `clone.alloydb` project, location and cluster are file-only, and
  `azure_restore`: its `clone.azure` subscription, resource group and server are file-only.

A stored override that a moved range now refuses is skipped at replay with a warning naming the
key and its `updated_at`, and the default applies until a valid value is saved.

Boolean settings saved through the API accept `true`/`false`, `t`/`f` and `1`/`0` in any
case, and apply the value they validate as (`"True"` is true).

Stored overrides (`sage.config`) are checked again when they are replayed at startup, on a
config-file reload and when an override is deleted: a row whose key is not a settable override,
or whose value fails its range, is skipped with a warning that names the key, its scope and its
`updated_at` (never the value). An invalid stored `trust.maintenance_window` replays as `never`,
so the window stays closed. If the stored overrides together leave an invalid configuration,
startup exits naming the key and a reload keeps the previous configuration. A settings save that
would produce an invalid configuration answers `400` with the key.

### Core

| Parameter | Default | Description |
|---|---|---|
| `mode` | `standalone` | `standalone` (one database), `fleet` (YAML `databases`), or `meta` (inferred from `--meta-db`). `mode: standalone` together with `meta_db` / `--meta-db` is a startup error: leave `mode` unset (meta is inferred) or set `mode: meta`. The former `extension` mode was removed with the C extension and is rejected at startup |
| `postgres.max_connections` | `2` | Connection pool size |
| `postgres.sslmode` | `prefer` | SSL mode (`disable`, `prefer`, `require`, `verify-ca`, `verify-full`) |

### Collection & Analysis

| Parameter | Default | Description |
|---|---|---|
| `collector.interval_seconds` | `60` | Seconds between snapshot collections |
| `analyzer.interval_seconds` | `600` | Seconds between analysis cycles |
| `analyzer.inactive_slot_min_minutes` | `5` | Report a replication slot (`inactive_slot`) once it has been inactive this many minutes. A slot whose `wal_status` is `extended`, `unreserved` or `lost` is reported at once. `0` uses the default. Settable through the configuration API without a restart (`1`-`10080` there). `0`-`10080` |
| `analyzer.subscription_lag_minutes` | `5` | Raise `subscription_lag` when an enabled subscription's last report to its publisher (`pg_stat_subscription.latest_end_time`) is this many minutes old, or when it had no apply worker in two consecutive snapshots. `0` uses the default. Restart to change. `0`-`10080` |
| `analyzer.cdc_stall_minutes` | `10` | Raise `cdc_consumer_stalled` when WAL advanced but an active logical slot's `confirmed_flush_lsn` did not move for this many minutes. The clock is in memory: a restart only delays a finding. `0` uses the default. Restart to change. `0`-`10080` |
| `replication.cdc_heartbeats` | empty | CDC heartbeat tables, `[{slot, table, column, max_age_minutes}]`. For an entry whose `slot` is a logical slot of the monitored database, pg_sage runs one read-only `SELECT max(column)::timestamptz FROM table` per collector cycle under a 2-second statement timeout and raises `cdc_heartbeat_stale` when the newest value is at least `max_age_minutes` (`1`-`10080`) old or the table is empty. `table` is `table` or `schema.table` and `column` a plain identifier (letters, digits, `_`, `$`), folded to lower case as PostgreSQL folds an unquoted name; anything else, or a second entry for one slot, is refused at startup. Empty runs nothing. Restart to change |
| `analyzer.unused_index_window_days` | `7` | Days an index must show no scans before it is reported unused (the evidence for a drop). `1`-`1073741824`; `0` is rejected, because it would report every eligible index unused |
| `analyzer.seq_scan_min_rows` | `100000` | Smallest table whose sequential scans are reported. `1`-`1073741824` |
| `analyzer.disabled_rules` | empty | Analyzer rules or categories to switch off; see [Rule packs and owner checks](#rule-packs-and-owner-checks). Settable through the configuration API (comma-separated): a per-database override applies on the next cycle; a global change is persisted and waits for a restart (the response lists it in `pending_restart`) |
| `analyzer.slow_query_min_calls` | `5` | Fewest calls a statement must make between two collector snapshots before `slow_query` and `query_regression` judge its interval mean (the time it added in the interval over the calls it added). Below the floor the interval is not evidence either way: an open finding for the statement stays open (an idle or quiet minute is not a recovery), except for a statement that has made fewer calls than the floor in total, whose finding resolves. `0` uses the default; negative values are rejected. Restart to change |
| `prometheus.listen_addr` | `127.0.0.1:8187` | Address (`host:port`) of the separate, unauthenticated `/metrics` listener. `""` opens none. A value that is not `host:port` with a port up to 65535 is refused at load. The listener binds at startup; if it cannot (the default was 9187, postgres_exporter's, before v2.7.0), pg_sage logs an ERROR naming the remedies, keeps running, and exports `pg_sage_metrics_listener_up 0` (served on the API route when `serve_on_api` is on). `SAGE_PROMETHEUS_ADDR` replaces it; `SAGE_PROMETHEUS_PORT` replaces only the port. No TLS: use `serve_on_api` for TLS. Requires restart |
| `prometheus.serve_on_api` | `false` | Also serve `GET /metrics` on the API listener, with the API's TLS and rate limit. A scrape needs `Authorization: Bearer` with a token holding the `metrics` scope ([scrape tokens](deployment.md#scraping-through-the-api-listener)); in a Databricks App, the platform identity of any pg_sage role instead. 401 without a valid token, 403 for a token lacking the scope; refusals carry no metric names. Needs a control database (startup refuses without one). The same snapshot is served, so a scrape adds no database load. Requires restart |
| `prometheus.refresh_interval_seconds` | `0` (30 s) | Seconds between re-reads of each database's `/metrics` values (value ledger, action counts, SLO state, the Sage SRE backlog and, in standalone, optimizer counts). Scrapes never query a database: they render the last refresh, so values can be up to one interval old. `0` means 30; a value below `collector.interval_seconds` is raised to it. Negative is rejected. Requires restart |
| `analyzer.self_cost_budget_ms` | `3000` | Raise a `sage_self_cost` finding when pg_sage's own statements (the `pg_stat_statements` entries carrying the `/* pg_sage */` tag after their first keyword) use more than this many milliseconds of database time per collector cycle, measured over each analyzer cycle. `0` disables the finding; the `pg_sage_self_*` Prometheus metrics are exported either way. `0`-`3600000` |
| `analyzer.table_bloat_min_bytes` | `1073741824` (1 GiB) | Smallest heap (bytes, from `pg_class.relpages`) for which the freeze custodian plans an online bloat rebuild. The plan needs a catalog bloat estimate of at least 50% (`reltuples` times the `pg_stats` row width against `relpages`, at the table's fillfactor); the dead-tuple ratio alone never plans one, because the analyzer's `table_bloat` `VACUUM` covers it, and a table never analyzed has no estimate. When `pgstattuple` is installed, the three largest candidates per cycle are confirmed with `pgstattuple_approx` under `safety.query_timeout_ms` and `safety.lock_timeout_ms`; a timeout or error keeps the estimate (`evidence.bloat_source` is `estimate` or `pgstattuple`). Every plan is recorded once, and its repeats folded into that row, as a parked `bloat_remediation` decision at risk tier `high`; pg_sage never runs it. A `pg_repack` plan is offered only where the extension is installed. `0` uses the default. Restart to change |
| `analyzer.index_bloat_threshold_pct` | `30` | Estimated btree bloat (percent of `relpages`) at which an `index_bloat` finding recommends `REINDEX INDEX CONCURRENTLY` (under the `reindex_concurrently` contract: moderate, approval required): info from this value, warning from twice it. The estimate comes from planner statistics, not a page scan ([findings](findings.md#index_bloat)). `0` uses the default. API-settable (`0`-`100`) |
| `analyzer.index_bloat_min_bytes` | `104857600` (100 MiB) | Smallest btree index (bytes, from `pg_class.relpages`) whose bloat the collector estimates each cycle, at most the 200 largest in one bounded catalog statement. More than 200 above the floor truncates the reading, and open `index_bloat` findings are then held, not resolved. `0` uses the default; a negative value is refused. Restart to change |
| `analyzer.wraparound_freeze_share` | `0.75` | Share (above 0, at most 1) of a table's effective freeze maximum (the lower of `autovacuum_freeze_max_age` and the table's reloption; XID or multixact) at which the analyzer reports a `wraparound_freeze` finding. Diagnosis only: the freeze custodian decides and proposes the VACUUM. Replaces `analyzer.wraparound_freeze_xid_age`, which is retired (it loads with one warning and is ignored). Restart to change |
| `self_budget.cpu_ms_per_cycle` | `600` | CPU time the sidecar process may use per collector cycle (all databases it monitors together), in ms; 600 is 1% of one core at the default 60 s interval. The cycle is the global `collector.interval_seconds`, not a database's own collector period. One process watch measures it once for the sidecar: over budget raises one `sage_self_budget` finding on the `sidecar` object, kept on the lowest-named connected database (the next one when that database refuses the write), never one per database. `0` disables. `0`-`3600000` |
| `self_budget.db_time_ms_per_hour` | `0` | Database time pg_sage's own statements may use per hour on each database, in ms. `0` keeps `analyzer.self_cost_budget_ms` (per collector cycle) as the database-time budget, so one breach never raises two findings. That inherited budget is per cycle of each database's own collector: `analyzer.self_cost_budget_ms` x 3600 / its period in seconds (in fleet mode a database whose collector runs every 120 s inherits half the hourly budget of one at 60 s). `/metrics` exports it per database as `pg_sage_self_budget_database{database,resource="db_time"}` and compares `pg_sage_self_budget_exceeded{database,resource="db_time"}` with it; `pg_sage_self_budget{resource="db_time"}` stays the value at the configured `collector.interval_seconds`. An explicit value applies to every database as-is; when it is below 1% of one backend (36000 ms/h) it also tightens the budget the derived `collector.interval_seconds` is sized to. `0`-`3600000` |
| `self_budget.blocks_per_hour` | `18000000` | Shared buffer blocks (8 KB, hit or read) pg_sage's own statements may touch per hour on each database (5,000 a second). `0` disables. `0`-`1000000000000` |
| `self_budget.storage_mb` | `10240` | Size the `sage` schema (tables, TOAST, indexes) may reach on each database, in MB; complements the relative `retention.sage_size_warning_pct`. `0` disables. `0`-`10485760` |
| `baseline.enabled` | `true` | Fold each cycle's workload into hour-of-week buckets (`sage.baseline_samples`) and raise [`workload_anomaly`](findings.md#workload_anomaly) when a value is far above the same hour in earlier weeks; `query_regression` then judges a statement whose bucket is active against that bucket. Uses only collected data, no new query on the monitored database. `false` resolves open anomalies. Restart to change |
| `baseline.weeks` | `4` | Earlier weeks a bucket is scored against. A bucket with data from fewer than 3 weeks is learning and never fires, so findings start after 3 weeks of history. Retention keeps `weeks` + 1 weeks and 2 days (time-zone slack) of `sage.baseline_samples` (at most `max_query_keys` x 168 x (`weeks` + 1) statement rows per database). `3`-`52`. Restart to change |
| `baseline.timezone` | `UTC` | IANA zone of the buckets (Monday 00:00 is bucket 0), e.g. `America/Chicago`; buckets follow the local wall clock across daylight-saving shifts. An unknown zone fails the config load. Changing it moves every bucket: expect learning again. Restart to change |
| `baseline.z_warning` | `4` | Robust z, (value - bucket median) / (1.4826 x MAD, the MAD floored at 5% of the median), at or above which 2 consecutive cycles raise a warning. Above `0` |
| `baseline.z_critical` | `6` | Robust z for a critical anomaly; a statement also needs at least 10% of the interval's DB time. A critical anomaly starts one read-only Sage SRE investigation per episode. At least `z_warning` |
| `baseline.max_query_keys` | `500` | Statements per database given a baseline each cycle, the largest interval time first. Statement baselines need `pg_stat_statements`; backends, WAL (`pg_stat_wal`, PG 14+) and lock-graph waits work without it. `0` keeps only those. `0`-`5000` |
| `baseline.min_delta_ms` | `5` | Smallest rise over the bucket median, in ms, before a statement can be anomalous, so quiet buckets do not alarm. Fixed floors apply to the other metrics: 5 sessions for backends, 1 MiB/s for WAL, 3 sessions for a wait class. `0` or more |
| `rca.lock_chain_interval_seconds` | `60` | Seconds between lock-chain fast-path checks. Each check opens or updates the `lock_contention` incident (with the root blocker's pid, `backend_start` and query identity) and sends `incident_detected` without waiting for the analyzer cycle. `0` disables the fast path; otherwise `10`-`3600`. Escalation and auto-resolution still count analyzer cycles. Restart to change |
| `rca.log_fast_path_interval_seconds` | `5` | Seconds between log fast-path passes. Each pass takes the critical log signals (PANIC and crashes, disk full, out of memory, refused connections, deadlocks, wraparound, corruption, WAL archive and WAL segment failures), opens or updates their incidents and sends `incident_detected` without waiting for the analyzer cycle. Other log signals still wait for the cycle. `0` disables the log fast path; otherwise `1`-`3600`. Escalation still counts analyzer cycles; log incidents auto-resolve after `rca.log_quiet_minutes`. In a YAML fleet each database on a cluster runs its own pass. Restart to change |
| `rca.log_quiet_minutes` | `0` | Minutes a log incident (deadlocks, refused connections and the other log signals) must go without a new occurrence before it auto-resolves (`resolution_reason` "no log occurrence for N minutes"). Clear analyzer cycles do not count for log incidents: a deadlock every 15 minutes is one open incident, not a detect/resolve pair each time. `0` uses `rca.dedup_window_minutes` (default 30); otherwise `1`-`1440`, and at most `rca.stale_after_hours`. Metric incidents still resolve after `rca.resolution_cycles` clear cycles. Restart to change |
| `rca.recurrence_renotify_hours` | `6` | Hours after a log incident resolves during which a recurrence of it (same identity, linked through `previous_incident_id`) at the same or a lower severity is recorded but sends no `incident_detected`, and its resolution sends no `incident_resolved` unless it escalated. A severity rise always notifies. pg_sage logs each suppressed notification at DEBUG. Metric incidents are always notified. `1`-`168`. Restart to change |
| `rca.deadlock_statement_text` | `fingerprint` | What a deadlock incident keeps of the statement each participant ran (from the server-log DETAIL). `fingerprint`: the first 16 hex characters of a SHA-256 over the statement with its comments removed, its string, dollar and numeric constants replaced by `?` and its whitespace collapsed, and no text. `redacted` (experimental, pending a privacy review): also that redacted text, at most 300 characters. Any other value fails at load. Applies to log files and provider log sinks alike. Restart to change |
| `rca.stale_after_hours` | `24` | Hours an open incident may go without being re-detected before pg_sage resolves it (`resolved_by` `pg_sage:stale`). Incidents about an idle-in-transaction session also resolve once that session is gone (`pg_sage:subject_gone`). Resolutions of incidents last seen longer ago than this send no notification. `1`-`8760`, and at least `rca.dedup_window_minutes` |
| `rca.vacuum_min_dead_tuples` | `1000` | Fewest dead tuples a table needs before its dead-tuple ratio counts toward the `vacuum_blocked` ("Autovacuum falling behind") incident. Ratios on tables below either floor are ignored, so a table with a handful of rows never opens, nor escalates, an incident. `1`-`1000000000` |
| `rca.vacuum_min_table_mb` | `8` | Smallest heap (MB, from `pg_class.relpages`) a table needs before its dead-tuple ratio counts toward the `vacuum_blocked` incident. `1`-`1048576` |
| `rca.xmin_holder_min_minutes` | `5` | Minutes a session's transaction must be open before the `vacuum_blocked` incident names it as the holder of the oldest xmin. Younger sessions are never named, so a briefly idle session opens no incident. A holder idle in transaction for at least `analyzer.lock_chain.idle_in_tx_terminate_minutes` gets `pg_terminate_backend` proposed for approval; an active one gets a diagnostic. `1`-`1440` |
| `rca.prepared_xact_min_age_minutes` | `30` | Minutes a prepared transaction of this database must be old before it opens the `orphaned_prepared_tx` incident or is named as the `vacuum_blocked` holder (with `ROLLBACK PREPARED` of its quoted GID proposed for approval). Other databases' prepared transactions are never blamed. `1`-`10080` |
| `migration.queue_waiters_threshold` | `3` | Sessions queued directly behind an in-flight DDL that is waiting for its lock (`wait_event_type = Lock`) before the DDL's `migration_safety` finding turns critical. The queue comes from the RCA lock-chain fast path (`rca.lock_chain_interval_seconds`; an observation counts for two intervals, so with the fast path off the queue is never observed and the detail reads "lock queue not observed"). The escalated finding carries the DDL's backend identity and approval-only guidance: cancel the DDL (`SELECT pg_cancel_backend(pid)`, a HIGH-risk guarded script, never auto-executed; managed services need `pg_signal_backend`), then retry with `SET lock_timeout = '5s'`. The lock-chain finding for that queue drops its cancel of the root reader. `0` means `3`. API-settable |
| `migration.contract_delay_minutes` | `60` | Minutes after a rehearsed `apply_migration` expand phase before its contract phase may run; recorded as `contract_not_before`. pg_sage never runs the contract: it returns a guarded forward-fix script ([What `apply_migration` returns](mcp.md#what-apply_migration-returns)). 1 or more; `0` or less is refused at load. Restart to change |
| `migration.max_lock_ms` | `2000` | Milliseconds a rehearsed step without `CONCURRENTLY` may hold `ACCESS EXCLUSIVE` on the clone before `apply_migration` parks (`step_budget_exceeded`, with the measured time). A step also parks when 1.5 times its clone time exceeds the applier's 10-minute `statement_timeout`. 1 or more; `0` or less is refused. Restart to change |
| `rehearsal.plan_flip_pct` | `5` | Percent a query whose plan hash changed on the rehearsal clone may slow before `apply_migration` parks (`plan_regression`); without a plan change the limit is 15. Above 0, at most 100. Restart to change |

**How pg_sage measures its own cost.** Once per analyzer cycle pg_sage reads the sage
tables' sizes and statistics in one bounded, read-only statement (`safety.query_timeout_ms`,
`safety.lock_timeout_ms`; a sage table locked by a migration fails the reading instead of
stalling the cycle). The same reading feeds `sage_footprint`, `sage_self_cost` and the
`self_budget.storage_mb` check. Its statements' counters come from `pg_stat_statements(false)`,
without the query texts: the texts are read only when entries pg_sage has not classified yet
appear, when an entry is re-created, or after `pg_stat_statements_reset()` (each database keeps a
cache of up to 2,000 classified entries). pg_stat_statements 1.8 (no `toplevel` column) is
measured without the top-level split. Older versions, which some managed providers keep after a
major upgrade, report the statement figures (`pg_sage_self_db_time_ms_per_cycle`,
`pg_sage_self_statements_per_cycle`, `pg_sage_self_blocks_per_cycle`) as unknown while the sage
schema counters are still measured, and pg_sage logs once per database: run
`ALTER EXTENSION pg_stat_statements UPDATE` in that database.

The per-database per-cycle figures (`pg_sage_self_*_per_cycle` and the `sage_self_cost`
budget) are per cycle of that database's collector at the period it actually runs (the
process CPU, `pg_sage_self_cpu_ms_per_cycle` against `self_budget.cpu_ms_per_cycle`, is per
global `collector.interval_seconds`, once for the sidecar): in fleet mode a database's collector can run at its
own, self-configured `collector.interval_seconds`, and the finding's `cycle_seconds` names that
period. Until a database's collector has started there is no cycle to normalize to: the
per-cycle and per-hour figures stay unknown (an open finding stays open) and only the `sage`
schema's size is measured. In standalone mode the period is the configured interval, so nothing
changes there.

**Which statements and findings are pg_sage's own.** A statement is pg_sage's when it carries
the `/* pg_sage */` tag or reads the `sage` schema; nothing else. Reading a statistics view is
not pg_sage's signature: a Datadog, pganalyze or pgwatch agent's `SELECT ... FROM
pg_stat_statements` or `pg_stat_activity` is another client's workload, so advice (slow-query
and top-query findings, the tuner, plan capture) may name it, and it never counts toward
pg_sage's own cost or budgets. The Go classifier and the SQL predicates apply the same rule.
Findings about pg_sage's objects (an identifier in the `sage.` schema, or the bare name `sage`
for a schema) are left out of advice and the finding views. The findings about pg_sage's cost
on a database (`sage_self_cost`, `sage_self_budget`, `sage_footprint`) are always kept and name
pg_sage, so a monitored or meta database named `sage` gets them too. On upgrade, a one-time
migration resolves open findings captured before pg_sage tagged its statements (an untagged
statistics-view read), with the reason `pre-tagging pg_sage statement` in their detail.

### Rule packs and owner checks

`analyzer.disabled_rules` is a rule pack: the analyzer rules (`unused_indexes`,
`slow_queries`, `inactive_slots`, ... as in `AllRules`) or analyzer categories (`unused_index`,
`query_regression`) that do not run on a database. An unknown name fails the config load and
the API answers 400. `wraparound_freeze`, `xid_wraparound`, `sequence_exhaustion` and the
`sage_*` self rules are protected: disabling one is refused with the reason. Disabled findings
resolve as `rule_disabled` ([findings](findings.md#rule-packs-and-owner-checks)).

The effective list for a database is, first match wins: the database's API override
(`PUT /api/v1/config/databases/{id}` with `{"analyzer.disabled_rules": "unused_indexes,slow_queries"}`,
recorded in the config audit with the user; `""` disables nothing there; `DELETE` of the key
returns to inheritance), then the YAML `databases[].disabled_rules` list (`[]` disables
nothing there), then the global `analyzer.disabled_rules` (YAML, or the global configuration
API, which applies after a restart).

```yaml
analyzer:
  disabled_rules: [cache_hit_ratio]
databases:
  - name: reporting-replica
    disabled_rules: [unused_indexes, slow_queries]
checks:
  - id: table_without_pk
    title: Table without a primary key
    severity: warning
    databases: ["*"]
    interval_minutes: 60
    max_rows: 100
    timeout_ms: 1000
    object_column: table_name
    message: Add a primary key so logical replication and pg_repack can handle the table.
    sql: |
      SELECT n.nspname || '.' || c.relname AS table_name
      FROM pg_catalog.pg_class c
      JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
      WHERE c.relkind = 'r' AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
                        WHERE k.conrelid = c.oid AND k.contype = 'p')
```

`checks` are owner-defined read-only checks, YAML only and read when pg_sage
starts (restart to change them); see [Owner checks](custom-checks.md) for every field, its bounds and how a check runs.

### Derived settings (self-configuration)

Every configuration key has a self-config class (shown in
[the generated lifecycle reference](generated/config-lifecycles.md)): `safety_critical`
(trust, approvals, action authority, verification, credentials, endpoints, the LLM provider)
and `operator_preference` (notification routes, windows, declared capacities, feature
switches) are never derived. For `derivable` keys with a rule, pg_sage derives a value per
database from evidence when you leave the key unset:

| Parameter | Default | Description |
|---|---|---|
| `self_config.enabled` | `true` | Derive unset derivable settings per database. `false` keeps every default. Restart to change |
| `self_config.soak_hours` | `24` | Hours a new derived value stays in shadow, compared with the active value's measured outcomes, before it may be promoted. `1`-`720`. Restart to change |

The derived keys, their evidence, bounds and shadow comparisons are listed in
[derived settings](generated/derived-settings.md): the collector interval and catalog read
timeout (sized to the statement time the collector measures over its last 60 cycles, or to
the timed catalog and statements probes before its first cycle), sequence runway sampling
(sized to the measured sequence scan), and the temp-file and LWLock incident thresholds
(sized to this database's temp traffic and connection limit). A derivation never spends
more or widens authority than the default (the catalog read timeout alone may grow, to at
most 10x, because a deadline shorter than the scan fails every cycle). A new value is
recorded in shadow first; it is promoted only after the soak and only when the comparison
is not worse, otherwise it stays in shadow with the reason. Restart-bound keys take a
promoted value at the next start and show it as pending restart.

Noisy evidence does not restart the soak. Each pass derives a candidate, rounded up to the
rule's step; a candidate within one step of the shadow, on the same side of the value in
force, keeps the soak and its samples, and only a move of two or more steps (or to the
other side) starts a new one. Every key starts at its default, so a candidate back at the
default keeps a one-step shadow's soak too; the soak drops that shadow when the 90th
percentile of its derivations is the default again. A derived value in force is not
re-derived for a one-step move. The cost-sized keys (collector interval, catalog read timeout, sequence sampling)
promote the 90th percentile of the soak's derivations, so one cheap pass does not set the
cadence. The temp-file threshold's soak samples the reactive detector's busiest window
since the previous pass, so real bursts are judged instead of an hourly average. A hold is
recorded once per outcome; the latest reason shows on the key. When other keys narrow a
rule's bounds (for example a larger `sre.runways.min_samples`), the derived value is
clamped into them on the next pass (a `clamped` ledger row), and dropped when no value
fits; a new rule version drops the old version's value and soaks its own.

A key you set in the YAML file, in a fleet `defaults`/`databases[]` field or as an API
override is never derived and shows as `operator`. The Configuration page's **Derived
settings** section (and `GET /api/v1/derived-settings?database=`) shows each key's value,
status (`default`, `derived`, `shadow`, `pinned`, `operator`), evidence, bounds, pending
restart and history; an admin can **pin current** (freeze the value in force) or **unpin**
(`POST /api/v1/derived-settings/{key}/pin|unpin?database=`). Every step is recorded in
`sage.config_derivation` (value, previous value, cited evidence, bounds, rule and version,
time); the startup log prints one `derived settings:` line per database. The
**Footprint** section next to it (and `GET /api/v1/self-cost?database=`) shows the cost
these settings follow: pg_sage's own database time, blocks and rows per hour against
`self_budget`, split by the component that sent each statement (see
[sage_self_cost](findings.md#footprint-api)).

### Trust & Actions

| Parameter | Default | Description |
|---|---|---|
| `trust.level` | `observation` | Autonomy ceiling: `observation`, `advisory`, `autonomous`. The trust ledger decides each class's level; this is the most it may use, never a grant |
| `trust.maintenance_window` | (none) | When autonomous MODERATE actions may run; see [Maintenance windows](#maintenance-windows). Unset or `never` closes the window. Live |
| `trust.tier3_safe` | `true` | Operator switch for unattended SAFE actions the ledger has earned; off caps them at approval. Live: turning it off stops the next unattended SAFE action |
| `trust.tier3_moderate` | `false` | Operator switch for unattended MODERATE actions the ledger has earned; off caps them at approval. Live |
| `trust.rollback_cooldown_days` | `7` | Days a rolled-back finding is not re-applied. Live, `1`-`1073741824` (0 would turn the anti-flap cooldown off) |
| `trust.cascade_cooldown_cycles` | `3` | Collector cycles an object rests after an action before the next one. Live for the executor's cascade guard; the tuner's re-tune cooldown for a query keeps its startup value until the database's runtime is rebuilt. A negative value is rejected |
| `trust.ramp_start` | (auto) | When pg_sage began observing the database. Auto-persisted on first start; set to override |
| `trust.ramp_safe_hours` | `192` | Minimum hours observed before pg_sage may propose a promotion to L2 (and to L3 for SAFE classes), 8 days, `1`-`8760`. A floor, never a grant. Actions that cannot be rolled back always wait at least `192` |
| `trust.ramp_moderate_hours` | `744` | Minimum hours observed before pg_sage may propose a MODERATE class's promotion to L3, 31 days, `1`-`8760`, at least `ramp_safe_hours`. A floor, never a grant. Actions that cannot be rolled back always wait at least `744` |

The trust model controls what pg_sage is allowed to do:

| Trust Level | Actions Allowed |
|---|---|
| `observation` | Cases and recommendations only |
| `advisory` | With `auto`, execute eligible typed SAFE actions; queue higher risk |
| `autonomous` | With `auto`, execute eligible typed SAFE/MODERATE actions; queue HIGH |

Execution mode is independent from trust: `manual` disables background
queueing and execution, `approval` queues supported actions, and `auto`
applies the table above. Trust never promotes `manual` to `auto`.

HIGH-risk actions always require manual confirmation regardless of trust level.
Plain CREATE/DROP/REINDEX and `VACUUM FULL` do not satisfy the typed background
contracts; concurrent or non-FULL forms are required.

### Executor statement timeouts and failed builds

| Parameter | Default | Description |
|---|---|---|
| `safety.ddl_timeout_seconds` | `300` | `statement_timeout` of every executor statement without a class timeout below (ALTER TABLE, a non-concurrent CREATE INDEX or REINDEX, `VACUUM FULL`, drops). `0` uses the default |
| `safety.index_build_timeout_seconds` | `14400` | `statement_timeout` of a `CREATE INDEX CONCURRENTLY` (also an index replacement's build), `60`-`86400`. `0` uses the default |
| `safety.reindex_timeout_seconds` | `14400` | `statement_timeout` of a `REINDEX ... CONCURRENTLY`, `60`-`86400`. `0` uses the default |
| `safety.vacuum_timeout_seconds` | `14400` | Cap on a `VACUUM` that is not `VACUUM FULL`, `60`-`86400`, enforced by its watchdog (outcome `vacuum_cap`). `0` uses the default |
| `safety.vacuum_stall_minutes` | `15` | Minutes a `VACUUM` heap phase may show no progress before its watchdog cancels it (outcome `vacuum_stalled`), `5`-`240`. `0` uses the default |
| `safety.lock_timeout_ms` | `30000` | `lock_timeout` of every executor statement; it bounds each lock wait of a long build, whatever its class timeout |

An executor `VACUUM` (not `VACUUM FULL`) runs on its own session with `statement_timeout = 0`
and the same `lock_timeout`, so a freeze of a large table is not cancelled before it can advance
`relfrozenxid`. A watchdog reads the session's `pg_stat_progress_vacuum` row every 30 s
(phase, heap blocks scanned and vacuumed, index vacuum passes, and on PostgreSQL 17+
`indexes_processed`) and cancels the backend with `pg_cancel_backend` when a heap phase shows
no progress for `safety.vacuum_stall_minutes` (`vacuum_stalled`), when
`safety.vacuum_timeout_seconds` ends (`vacuum_cap`), or when the emergency stop is set
(`emergency_stop`). Index phases have no stall check: before PostgreSQL 17 they have no
counter, and one large index can take longer than the stall window, so only the cap bounds
them. If the cancel is refused, pg_sage closes the session.

Before a `VACUUM` runs, pg_sage reads `pg_stat_progress_vacuum` for the table (and its TOAST
table). When a vacuum, typically autovacuum, is already running there, the action is withheld
as `vacuum_in_progress` with that backend's pid, phase, blocks scanned and whether it is an
anti-wraparound autovacuum: a manual `VACUUM` would only wait behind it on `lock_timeout`, or
cancel an ordinary autovacuum and lose its work. A withheld action writes no failed action
row, so it never counts toward the custodian backoff. A role without `pg_read_all_stats`
sees other roles' rows without a table; the action then proceeds and its decision records
`progress_unknown`.

No class timeout is ever unlimited. The recommendation claim, the change lease and the
budget hold last at least twice the longest class timeout, so a four-hour build never outlives
the claim that covers it.

A failed `CREATE INDEX CONCURRENTLY` or `REINDEX ... CONCURRENTLY` leaves an INVALID index
that writes still maintain and that holds disk. pg_sage drops it at once, on every path
(autonomous, operator, index replacement, restore), but only when it is provably the failed
statement's own object:

- A build's OID is read from `pg_stat_progress_create_index` while the build runs. A short
  transaction holding `ROW EXCLUSIVE` on the table (it blocks no reader or writer) keeps the
  build in its first wait until the OID is read, at most half of `lock_timeout_ms`.
- A failed `REINDEX ... CONCURRENTLY` drops only the `<name>_ccnew[N]` or `<name>_ccold[N]`
  indexes it added; an older index of that shape is never touched.
- Just before each `DROP INDEX CONCURRENTLY IF EXISTS`, pg_sage re-checks, under an advisory
  lock on the OID, that the name still has that OID, is still INVALID, is on the same table and
  is not being built by anyone. A VALID index is never dropped.

The outcome is recorded as `remnant_cleanup` in the action's `before_state` and in the
failure's error text: `dropped`, `nothing_to_drop`, `oid_mismatch`, `valid_kept`,
`in_progress_elsewhere`, `unbound` (no OID was captured), `skipped_stopped` (an emergency stop
ended the run) or `remnant_cleanup_failed` (the drop itself failed, for example on
`lock_timeout`). Anything not dropped is left to the `invalid_index` finding. An operator-approved
CREATE INDEX over an INVALID index of the same name is refused with the command that clears it.
A build withheld because its index name exists is parked until that name is gone, then retried.

### Backup freshness

| Parameter | Default | Description |
|---|---|---|
| `backup.max_age_hours` | `26` | Hours the provider's last good backup may age before `backup_stale` opens (critical at twice the age), `1`-`8760`. RDS and Aurora: the latest restorable time; Cloud SQL: the last successful backup run. Restart to apply |
| `backup.max_archive_gap_minutes` | `15` | Minutes completed WAL may wait since the last successful archive before `wal_archive_stale` opens (critical at four times the gap), `1`-`10080`. Restart to apply |
| `backup.min_retention_days` | `7` | Backup retention the provider must keep; less opens `backup_retention_below_policy`. `0` turns the check off; a negative value is refused at startup. `0`-`35`. Restart to apply |
| `backup.min_pitr_hours` | `24` | Hours of point-in-time recovery window the provider must offer; less opens `pitr_window_below_policy` (24 h passes a 24 h policy). `0` turns the check off. `0`-`8760`. Restart to apply |
| `backup.external_source` | empty | A backup kept outside the provider's delete path that pg_sage cannot read (an AWS Backup vault in another account, a dump in other storage). Set, it resolves `backup_shares_delete_path`: it is your statement, not evidence pg_sage checked. One line, at most 200 characters. Restart to apply |

These keys only open and close findings (see
[Backups and WAL archiving](findings.md#backups-and-wal-archiving)); they never change what
pg_sage may do. They are YAML only. A self-managed server's backup tool leaves no evidence
in the catalog, so there its backup freshness is unknown and only the WAL archiver is judged.

### Restore drills

| Parameter | Default | Description |
|---|---|---|
| `drill.max_concurrent_per_fleet` | `2` | Most restore drills the fleet leader runs at once, across every database, `1`-`10`. Each database runs one drill at a time, and every drill clone still counts against `clone.max_creates_per_hour`. Restart to apply |
| `drill.command.restore_argv` | unset | Self-managed servers only: the program (an absolute path) and arguments that restore the database's own backups and print one JSON line `{"dsn", "id", "recovery_point"}`. Set, drills use it instead of `clone.provider`; it is a full copy, so it needs `clone.allow_full_copy` and `full_copy` in the policy's drill kinds. Refused at startup next to a managed `clone.provider` or a database whose `provider` is managed ("use the provider's clone kind"). Restart to apply |
| `drill.command.destroy_argv` | unset | Required with `restore_argv`: the program and arguments that remove a restore, given `SAGE_CLONE_ID`. The clone reaper may run it again for the same id. Restart to apply |
| `drill.command.env_allow` | `[]` | Names of sidecar environment variables passed to both programs besides `PATH`, for example a repository passphrase. `SAGE_` names are refused. Values are never logged. Restart to apply |

Each database's drill is set in its standing policy document's `drill` block (cadence,
clone kinds, time budget, marker query; see
[restore drills](policy-gate.md#restore-drills)), and drills run on `clone.provider`
([managed clouds](managed-clouds.md#restore-drills)) or, on a self-managed server, on
`drill.command` ([restore drills on self-managed servers](restore-drills.md)), which needs
`data_dir`: the programs run in `data_dir/restore-drills`. YAML only.

### HA role checks

| Parameter | Default | Description |
|---|---|---|
| `ha.check_interval_seconds` | `60` | Seconds between HA role checks of each database, `5`-`600`, at most the executor cycle (`analyzer.interval_seconds` + 5 s). Restart to apply |

Each database has one HA monitor, in standalone, YAML fleet and meta-db mode
alike. It checks the role (`pg_is_in_recovery()`) and the node identity
(timeline, system identifier, start time) once at startup, then every
`ha.check_interval_seconds` with a 5 s timeout, and once more just before each
executor cycle. Everything else that needs the role reads that monitor's last
check without probing: continuous custodian proposals, the runway advisor,
earned autonomy and the fleet status. So they always agree, and the monitored
database sees one role probe per interval.

Autonomous actions run only on a confirmed primary, outside safe mode, and at
least `sre.autonomy.failover_cooldown_minutes` (default `30`) after a role or
node change. This holds whatever `sre.autonomy.enforce` says. Safe mode starts
after 2 role flips within an hour and ends after 3 stable checks once 30
minutes have passed since the last flip. The monitor fails closed: a failed
check, or a last check older than three intervals, reads as role `unknown`.

The node identity is kept in `sage.ha_identity` in the control database. A
failover while pg_sage was not running is therefore seen at startup: a changed
role, timeline or system identifier opens the cooldown for every consumer. On
managed providers that restrict `pg_control_system()`, only the role is
compared. `GET /api/v1/databases` shows `ha_role`, `ha_safe_mode` and
`ha_last_change` for each database, and the Fleet page shows them on each tile.

### One trust system

Every action class pg_sage runs on its own initiative has one level per database in the
trust ledger: the incident remediations (see [Sage SRE earned autonomy](#sage-sre-earned-autonomy))
and the self-initiated classes, grouped by their goal:

| Family | Classes | A success is |
|---|---|---|
| `tuning` | `index_create`, `config_guc` (GUC), `autovacuum_tuning` (reloption), `query_hint`, `statistics` | an `improved` verdict |
| `hygiene` | `index_drop`, `vacuum`, `analyze`, `retention`, `reindex` | an `improved` verdict, or a `neutral` one that held (the action was not rolled back) |

The levels are the earned-autonomy levels: L1 writes the script (the default), L2 hands the
action to one-click approval, L3 runs it unattended (SAFE classes at any time, MODERATE ones
inside the maintenance window, one object at a time). Irreversible classes (`retention`)
never exceed L1.

**The ledger grants, the operator caps.** `trust.level` and the `tier3_safe` /
`tier3_moderate` flags are the operator's ceiling and kill switch, and stay so permanently:
the ledger never grants past them, and lowering them takes autonomy back on the next
authorization, without a restart (without touching the earned levels, which return when the
cap is raised). `execution_mode` and the
standing policy cap the same way: `advisory` never runs a MODERATE class unattended whatever
its level, and `tier3_moderate: false` keeps every MODERATE class at approval.

**Evidence.** The verification verdicts of sage.action_outcome are the evidence:
`insufficient_evidence` and `unverifiable` count neither way. A `regressed` verdict, an
operator's rollback of the action or an operator's rejection of its approval item demotes the
class one level at once, records the cause in the history and notifies an operator through
the notification rules for failed actions (`action_failed`). The automatic revert of a
neutral index create (no gain) is not a demerit. A demerit already known when the level was
set does not demote it again.

**Promotion.** pg_sage proposes one level up when the class has (defaults, configurable under
`sre.autonomy.class_promotion`):
- L2: `min_successes_l2` (3) verified successes since its last demerit, and the database
  observed for at least `trust.ramp_safe_hours`;
- L3: `min_successes_l3` (10) successes since the last demerit, `min_success_rate_pct` (80%)
  of decided outcomes since then successful, and the database observed for at least
  `trust.ramp_moderate_hours` (`trust.ramp_safe_hours` for SAFE classes).

An admin approves every promotion (the Trust page, or the earned-autonomy API). The ramp is
only a floor: it never grants anything by itself.

**Existing configurations.** On the first start of a database under the unified ledger, the
level the time ramp had already given each self-initiated class is kept as a
**grandfathered** level (L3 when it ran unattended, L2 when it queued for approval), once.
Grandfathered levels demote like any other; the ramp elapsing later grants nothing. The
startup log explains the new meaning of `trust.level` and the ramp, and lists what was
grandfathered. `sre.autonomy.enforce: false` keeps the legacy behaviour (the ramp grants)
and is logged as a warning. Rollbacks of pg_sage's own changes and owner-declared retention
deletes are never withheld by the ledger.

**Re-validation on a PostgreSQL major upgrade.** Planner, statistics and GUC semantics change
between majors, so every level records the major of the monitored server it was granted on
(`server_major`, read from `server_version_num`, which needs no privileges on any provider;
pg_sage re-reads it at most every 5 minutes). A self-initiated L3 granted on another major
than the server runs now (for example earned on 16, the server now on 17) is capped at L2:
its actions go to one-click approval, and the history records `capped` once, with the reason
`server_major_changed:16->17`. The Trust page shows "capped at L2: PostgreSQL 16→17; 1 of
3 improved outcomes, or re-approve" on the row (`major_cap` on the rows of
`GET /api/v1/trust` and `GET /api/v1/sre/autonomy`, whose effective level is L2). The cap clears,
recorded as `cap_cleared`, after 3 `improved` outcomes of the pair recorded after the cap
event (the L2 promotion count, `class_min_successes_l2`), or at once when an admin
re-approves the pair (**Re-approve** on the Trust page, or `POST
/api/v1/sre/autonomy/revalidate?database=` with `{"family", "class", "note"}`; other roles
get 403, a pair that is not capped 409 `not_capped`). Either way the level is restamped
with the new major. A minor upgrade (17.0 to 17.2) changes nothing. A level stored before
this release has no major and is stamped with the current one, without a cap. When the
version cannot be read, nothing is capped or stamped (the last major read is kept). L2 and
lower levels, incident families and carried-over levels are not re-validated, and the
demerit rule is unchanged. The cap is per database: each database's ledger compares its own
server.

The **Trust** page (and `GET /api/v1/trust?database=`) shows every database x family x class
with its level, effective level, evidence counts, last change and why, and the path to the
next level; an incident row also carries `bench`, the report its bench checks read (id,
origin, provenance label). MCP `sre_get_autonomy` carries the same grid as `trust`. Each view is read with a
fixed number of set-based statements per database, however many classes and outcomes it
shows. Every approval card carries its action class's row as `trust` (level, evidence
counts, path to the next level) and one `Trust:` line in Slack and Telegram.

### Shadow mode

Below a self-initiated class's earned level (anything under L3), pg_sage does not only stay
quiet or queue for approval: it records a **shadow decision** for every action it would have
taken. The decision holds the exact SQL, the rollback (for a configuration change, the one
that restores the captured prior value), the prediction (the same model as real actions:
targeted queries, metric, expected change, method), the evidence, and the gate's verdict had
the class been trusted (for example "execute", or "queue for approval" when `trust.level`,
the window or a multi-object target would still hold it). Shadow mode never runs anything and
reads only catalogs, statistics views and sage tables, so it never locks a user object; it
adds no EXPLAIN or HypoPG call to the cycle. The approval queue works as before; the shadow
is recorded beside it. A finding is recorded at most once per day per fingerprint (class,
object and normalized SQL); later sightings only count. A class promoted to L3 stops being
shadowed; a demoted one resumes. With `sre.autonomy.enforce: false`, in `manual` mode or with
the executor off, nothing is shadowed. Incident remediations keep their own evidence (bench,
shadow reviews) and are not shadowed here.

**Scoring.** Each pending decision is scored later, deterministically, by the best evidence
available, in this order (never from model text):

| Source | When | Score |
|---|---|---|
| `operator` | an operator decided the same proposal after it was recorded | rejected: incorrect; approved: by the executed action's verdict |
| `applied` | the same change (matched by normalized SQL) ran later through pg_sage | by its verification verdict |
| `external` | the change pg_sage wanted shows up in the catalog (a migration, psql) and no pg_sage action since the decision made it; the signal per class is in the next table | by the class's verification, as for pg_sage's own actions (next table) |
| `hypopg` | an index create, after a day without the above, whose targeted queries still ran | the optimizer's what-if bar (`optimizer.hypopg_min_improvement_pct`): verified is correct, rejected incorrect |
| `none` | nothing applied within 7 days | unscored |

The classes scored `external`, and the catalog signal of each:

| Class | Applied outside pg_sage when | Verification |
|---|---|---|
| `index_create` | a valid index of the same shape is on the table | call-weighted before/after windows of the targeted queries around when it appeared (`verify.*`) |
| `index_drop` | the index is gone while its table still exists | the same windows over `verify.drop_window_hours` |
| `vacuum`, `analyze` | `pg_stat_user_tables.last_vacuum` / `last_analyze` of the table is later than the decision and at least a minute old (pg_sage logs its own runs once they end); autovacuum's runs (`last_autovacuum`, `last_autoanalyze`) never count | the decision's recorded baseline (dead tuples, rows modified since analyze, or relfrozenxid age) against the same metric read in the pass that first sees the run: improved when at least half of it went, as for pg_sage's own maintenance; a decision without a baseline is never counted |
| `autovacuum_tuning` | `pg_class.reloptions` holds exactly every proposed `key=value` of the decision's `ALTER TABLE ... SET (...)` (another value, or a value written differently, is no match) | the targeted queries' windows, as an index create |
| `statistics` | an extended statistics object of any name on the table covers the same columns and exactly the same kinds (all three when the decision names none), without expressions | the targeted queries' windows combined with their row-estimate error before and after (as pg_sage's own statistics) |

`config_guc`, `query_hint` and `reindex` are never scored from the catalog: settings change
for unrelated reasons, and a check from pass to pass misses a change reverted in between. Any
pg_sage action (not failed) that names the table since the decision rules a VACUUM, ANALYZE,
reloption or statistics change out as external evidence. A decision whose table is gone, whose
database statistics were reset since it was recorded (`pg_stat_reset()`, or on PostgreSQL 14 a
crash that dropped them), or whose catalog row pg_sage may not read (SQLSTATE 42501) waits and
is unscored at the horizon with that reason; it is never an error of the pass and never
counted. The reads are bounded catalog and statistics reads (`pg_class`, `pg_stat_user_tables`,
`pg_stat_database`, `pg_statistic_ext`, visible to `pg_monitor`) on each database's own pool,
on PostgreSQL 14 to 18.

Verdicts map by the family's rule: `improved` is correct (hygiene: a `neutral` that held is
correct too), `regressed` or an operator's rollback is incorrect, a tuning `neutral` is
neutral, `insufficient_evidence` and `unverifiable` are unscored. A matched action still being
verified is waited for (up to 21 days). A change pg_sage made itself since the decision (an
index it built, dropped or built and rolled back) is no shadow evidence: neither `external` nor
`hypopg` scores it. Nor is one pg_sage cannot rule out (more than 50 of its index actions on the
table since the decision, or a drop whose rollback names no table). Such a decision is left
unscored at the horizon.
Each scoring pass reads the oldest and the newest pending decisions (100 each), so decisions
waiting out their horizon never hold back newer ones an operator already decided.

**Trust.** Only `external` and `hypopg` scores count toward promotion as shadow evidence:
`operator` and `applied` scores rest on actions the ledger already counts as real outcomes,
and counting them again would double the evidence. Shadow evidence counts like real evidence
of the family (distinct decisions), is labelled "shadow" in the class record and in every
promotion proposal (for example `10 (3 real, 7 shadow)`), and an admin still approves every
promotion. L2 may be earned from shadow evidence alone (each action is still approved by a
person); **L3 needs at least 3 real verified successes** since the last demerit, and shadow
successes fill at most the rest of the L3 bar (7 of the default 10; when
`min_successes_l3` is lowered, the real minimum follows it down and shadow fills nothing). An
incorrect shadow decision resets the class's success streak but never demotes an earned
level; only real regressions, rollbacks and rejections demote.

**Surfaces.** The Trust page shows, per class, the shadow decisions and their scores and, on
demand, what pg_sage would have done (SQL, prediction, verdict had it been trusted, score and
source). The Shadow Mode page (Settings, General tab; every signed-in role) shows the same
ledger per database and class: decisions, pending, correct, incorrect, counted, and
**potential minutes**, the current `sage.toil_model` base minutes of the class times its
counted correct decisions. Potential minutes are never credited (the Value page prices only
verified outcomes); a class without a toil model reads "not modelled", never a default. With
no decisions the page says why: `sre.autonomy.enforce` is off, every class is already at L3,
or nothing has been recorded yet.
`GET /api/v1/shadow-decisions?database=&class=&status=&score=&limit=` serves the per-class summary (`potential_minutes` is `null` for an unmodelled class; `class`
narrows the summary too, and an unknown class is an empty summary) and the newest decisions;
`GET /api/v1/trust` carries the summary per database as `shadow`. It replaces the removed
heuristic report over open cases. An approval card shows its class's shadow history and whether the
proposal itself was a shadow decision (your decision then scores it). Prometheus:
`pg_sage_shadow_decisions_total{database,class,verdict}` and
`pg_sage_shadow_scores_total{database,class,score,source}`. Scored decisions age out with
`retention.actions_days`; pending ones are kept until scored.

### Verifying actions

Every action pg_sage takes records, before it runs, what it expects: the queries it
targets, the metric that should move and by how much, and how that was predicted (`hypopg`,
`model`, `rule`, or `none`). Afterwards it records what it observed and a verdict:

| Verdict | Meaning | Trust and credit |
|---|---|---|
| `improved` | the predicted effect was observed (a drop: reads held for the whole cycle) | counts toward earned autonomy and value |
| `neutral` | measured, no change beyond the bars (shown as "No change") | neither |
| `insufficient_evidence` | too few calls, too few sampling intervals, or too noisy to tell (shown as "Inconclusive") | neither |
| `unverifiable` | nothing measurable, or no prediction | neither |
| `regressed` | a targeted query (or the targeted metric) got significantly worse | rolled back; counts against trust |

The targeted queries are compared on their call-weighted mean execution time, before and
after, with Welch's test over the sampling intervals in `sage.query_store`: a change counts
only when it is both significant and beyond its bar. With several targets each is tested
with a Bonferroni-corrected significance and the call-weighted pool decides. A first verdict
comes after `trust.rollback_window_minutes` once there are `verify.min_samples` calls; without
enough evidence the watch extends up to `verify.window_max_minutes`.

`verify.method` picks the verifier that decides (`v1`, `shadow`, `v2` or `auto`,
default `auto`): `shadow` computes verification v2 beside v1 and acts on v1, `v2`
acts on v2, and `auto` is `v2` only on a database whose latest verifier back-test
passed within 35 days, `shadow` otherwise: v2 becomes a database's verifier once its
own back-test passes (see [verification.md](verification.md)). The weekly back-test
draws `verify.backtest_placebos` placebo steps (default 300, 200-10000) and stops,
storing nothing, after `verify.backtest_max_runtime_minutes` (default 10, 1-1440);
`pg_sage_sidecar verifier-backtest --database NAME` runs one by hand. In v2 a "No change" result keeps the index instead of
reverting it as `no_gain`. The post-action monitor (index drops, settings, storage
parameters) always acts on v1 for now and records v2's verdict beside it for comparison.
`verify.on_expired` (`ask`, `keep` or `revert`, default
`ask`) decides what happens to a monitor whose window ended while pg_sage was not
watching: `ask` queues one "Revert action N" item, `keep` keeps the change
(recorded), `revert` rolls it back through the operator rollback (a refused rollback queues the
revert item instead). An expired change with no rollback SQL is kept and recorded. Any other value of
either key is refused at startup. A step whose targets ran too rarely in the last 4
weeks to be judged within `verify.window_max_minutes` needs approval at every trust
level ("cannot be verified within 72 h: the targets run N times a week").

```yaml
verify:
  method: auto        # v1 | shadow | v2 | auto
  on_expired: ask     # ask | keep | revert
```

Verification pauses while its evidence is unreadable. A failed read of the query history
or the catalog (a lost connection, or the meta database down under
`history.store: meta`) and a shutdown mid-check are not evidence that the change failed: the
watch stays open with `sage.verification.verdict = extended` and `reason =
observation_retry: <error>`, and it is checked again after 5, 15 and then every 60 minutes
(`observation_failures` counts the failed attempts, and a successful check resets it). A
failed write of the watch state also keeps the watch, unchanged, for the next check. Only a
watch that reaches its hard deadline (execution time plus `verify.window_max_minutes`)
without a successful check is reverted, with the reason `observation_unavailable`. A change
whose watch could never be stored is reverted at once (`state_persist_failed`), and a check
that reads the index as invalid still reverts it immediately. At startup, a watch that an
earlier version closed after a transient read error or a shutdown (`observation_unavailable`
or `context_canceled` with the hard deadline still ahead) is reopened and checked again;
until that sweep succeeds, no index watch is resumed, so none of those changes is reverted.

A watch's target queries are sampled whatever their rank. An index that works makes its
query cheaper, and a query with less total time can fall out of the collector's top
`collector.max_queries` statements; without samples the watch would extend to its hard
deadline and revert the index as `insufficient_samples`. The collector therefore pins the
target queryids of every open watch (`sage.verification` rows with `completed_at` NULL,
read once a cycle, so the pins survive a restart): they are sampled into `sage.query_store`
every cycle, marked `admitted_by: ["pinned"]` in the `queries` snapshot, until the watch
completes. At most 64 queryids are pinned per database and cycle, oldest watch first. A
failed read of the open watches logs one WARN per failure streak and pins nothing until it
recovers; the rest of the queries step goes on.

Write cost is measured on the index's own table. For each window (before the build and
after it) the verifier reads the collector's cycles from the history store (at most 49 per
window, spread evenly) and differences them into intervals: the WAL the cluster wrote, and the
tuples the table wrote (`pg_stat_user_tables` `n_tup_ins + n_tup_upd + n_tup_del`). WAL is
cluster-wide, so an interval is charged to the table only when the table wrote at least 90%
of the tuples the database wrote in it and at least 100 tuples; an interval in which other
tables wrote is `unattributed`, and one whose readings cannot be compared (a statistics reset,
a standby, a cycle without a tables snapshot) is `unknown`. The window's cost is the median WAL
bytes per written tuple of its attributable intervals. An index whose queries improved is
reverted as `write_impact` when the post-build cost is more than `verify.write_impact_pct`
above the pre-build cost (a rise of exactly the threshold is kept). The query reasons come
first: a query regression is still `query_regression` and a neutral result still `no_gain`,
and a window extended for query samples is not ended early. Each window needs 3 attributable
intervals and 1,000 tuples. Part of the WAL is paid per unit of time rather than per tuple
(full-page images after each checkpoint, the collector's own history writes), so a table that
writes more slowly after the build pays more per tuple with no index cost at all; the windows
are compared only when the table's post-build write rate (tuples per second over its
attributable intervals) is at least 100/(100 + `verify.write_impact_pct`) of the pre-build
rate, the slowest rate that cannot by itself raise the cost over the threshold. With less
evidence, a slower post-build rate, or when the write history cannot be read (recorded in
`write_signal.error`; it does not pause the watch), the write signal is `insufficient` and
the queries alone decide. There is no database-wide fallback: a batch on another table, or
the daytime load after a night build, is never charged to the index. Each index verdict
records the signal as `write_signal` in the verdict evidence (`sage.action_outcome.evidence`):
`scope: table`, `metric: wal_bytes_per_tuple`, `table`, `outcome` (`regressed`,
`within_threshold` or `insufficient`), `before` and `after` (`intervals`, `unattributed`,
`unknown`, `tuples`, `wal_bytes`, `seconds`, `bytes_per_tuple`), `delta_pct` when both
windows were measured, `threshold_pct`, `exceeds_threshold`, and `decisive` (true when it caused the
revert). When the cost rose over the threshold, the outcome reason names the table and the
rise. Other databases of the same cluster also write WAL; the median and the attribution rule
absorb a burst from them, not a sustained load that only runs after the build.
Every B-tree index logs its own entry for each inserted row (and for each update that is
not HOT), so on an insert-heavy table that dominates the database's writes one more index
can raise the WAL per tuple by tens of percent (the integration test measures +100% on a table
with no other index). Raise `verify.write_impact_pct` where read gains on such tables matter
more than their WAL volume.
At startup, an open `write_impact` revert from a version before the table-scoped signal
(its watch has no `write_scope`) whose drop has not run is reopened and checked again on the
table-scoped signal; if that drop was withheld or failed, its action (`rollback_skipped` or
`rollback_failed`) is watched again (`monitoring`) so the new verdict settles it, and an
operator's rollback is left as it is. A table-scoped `write_impact` revert is left alone.

An index drop is watched for its business cycle, `verify.drop_window_hours`, and checked
from the start. Its definition is kept, and the index is re-created as soon as a query on
the table regresses or an active pg_sage hint names it (soft drop). VACUUM and ANALYZE are
checked right after they run (dead tuples, rows modified since analyze, relfrozenxid age
for VACUUM FREEZE).

The soft-drop restore runs under its own typed contract:

| Action contract | Risk | Rollback class | Change class | Approval | What it runs |
|---|---|---|---|---|---|
| `restore_dropped_index` | SAFE | reversible | `index` | none | `CREATE [UNIQUE] INDEX CONCURRENTLY` of the definition recorded with the drop |

- **Same SQL everywhere.** When a drop runs, the definition the rule recorded is stored once,
  in its `CONCURRENTLY` form, as the action's `rollback_sql`; the monitor and the operator's
  rollback button run that statement. A restore that differs from it byte for byte, or
  re-creates another index or table, is refused with the reason `rollback_sql_mismatch`
  and never reaches the gate. Rows written before v2.5.1 keep the raw definition; it is
  normalized when their monitor resumes.
- **Survives trust downgrades.** The restore was a condition of the autonomy the drop ran
  under, and it only re-creates what existed. Lowering trust after the drop
  (`trust.level: observation`, `execution_mode: approval` or `manual`,
  `trust.tier3_safe: false`, a ramp reset) does not strand it: the decision is
  `execute` with the reason `drop_restore_bypass`, its detail names the downgrade it
  passed, and it bypasses the kind budgets. The executor-disabled switch, a replica, the
  provider list, the standing policy's change classes, approval classes and refusal set,
  degraded SQL validation and the lock ceiling still bind. No other rollback gains this
  bypass.
- **The emergency stop still stops it, visibly.** A restore due during an emergency stop runs
  no DDL: the action is recorded as `rollback_withheld` (reason `emergency_stop: ...`) and
  one approval card, "Restore the index dropped by action N", is queued. The card is not
  approvable during the stop; after the stop is lifted, approving it runs the recorded
  restore through the operator rollback. It is superseded if the action is rolled back
  another way, and expires like any queue item (7 days); the action's rollback stays
  available after that.
- **Reasons apart.** A rollback that no action contract classifies is withheld as
  `no_rollback_contract: ...`, distinct from a policy refusal (`standing policy withheld
  automatic rollback (<verdict>: <reason>)`). At startup the executor checks that every
  reversible forward contract's rollback classifies to a contract and logs each one that
  does not at ERROR.
- **Every revert is a recorded decision.** The automatic revert of any action (pg_sage's
  own, one an operator approved or ran by hand, one whose monitor resumed after a restart)
  and the operator's rollback button go through the standing policy gate as a typed
  rollback request, judged on the statement that runs (an index rollback in its
  `CONCURRENTLY` form). Each leaves one `sage.decision` row with `evidence.rollback = true`,
  `evidence.rolled_back_action_id` and `evidence.source` (`automatic_rollback` or
  `operator_rollback`).
- **A revert is a mitigation.** Undoing a change that already ran is not maintenance:
  maintenance windows, the kind budgets, earned per-class autonomy, the trust tiers and
  ramp and the change-class allowlist do not bind it, and its decision reads `execute` with
  the reason `rollback_mitigation`. The executor-disabled switch, the emergency stop, a
  replica, the provider list, `trust.level: observation` and the lock ceiling still bind; a
  rollback no contract classifies is parked as `no_typed_contract`. An approval-required
  rollback contract (for example an `ALTER TABLE ... RESET`) and, for an automatic revert,
  the standing policy's `approval_required_classes` and `refusal_set` send it to a person
  (`queue_approval`); an operator's revert already has one. If the standing policy cannot be
  read, the revert still runs, under `safety.lock_timeout_ms` with no ceiling, and the
  decision's `evidence.policy_read_error` names the error. The soft-drop restore keeps its
  own `drop_restore_bypass` decision above.
- **A withheld revert reaches a person.** An automatic revert the gate or an emergency stop
  withholds is recorded as `rollback_skipped` (its `rollback_reason` names the verdict) and
  one approval item, "Revert action N", is queued (`sage.action_queue.reverts_action_id`,
  action type `revert_action`). Its card says why the revert was withheld; after an
  emergency stop, that it is approvable once the stop is lifted. While the gate would still
  refuse a person's revert the item stays pending and not approvable. Approving it runs the
  action's recorded rollback through the operator rollback, once. It is superseded when the
  action is rolled back another way, and expires like any queue item (7 days); the
  action's rollback button stays available after that. The index-replace watch keeps its
  own undo path and queues no item.

A rollback interrupted by a crash or restart no longer stays `rolling_back`. Each cycle on
the primary, a `rolling_back` action (other than an index replacement, which resumes its own
undo) whose claim is older than `safety.ddl_timeout_seconds` plus one minute is settled from
the catalog, while the executor has no work in flight; no DDL runs. A dropped index that is
gone, a re-created index back with the recorded definition, or a setting at the rollback's
value (`pg_settings`; for `ALTER DATABASE`, the stored setting read in a fresh session)
becomes `rolled_back`. Anything else, including a statement it cannot probe, becomes
`rollback_failed` with the reason `interrupted during rollback: ...`. An INVALID index left
by an interrupted `CREATE INDEX CONCURRENTLY` is named in that reason: drop it with
`DROP INDEX CONCURRENTLY` before retrying. Outcome writes are retried three times with
backoff; a write that still fails is logged with the action id and outcome, and an operator
rollback whose DDL ran reports that its outcome was not recorded. The Actions page offers
**Retry rollback** on `rollback_skipped` and `rollback_failed` actions that have rollback
SQL, and shows their reason.

Each executed action carries `verification_outcome` in `GET /api/v1/actions` and
`/api/v1/actions/{id}`; `GET /api/v1/action-outcomes?database=&class=&verdict=&since=&limit=` lists the
outcome ledger (`sage.action_outcome`).

| Parameter | Default | Description |
|---|---|---|
| `trust.rollback_window_minutes` | `15` | Minimum minutes before a first verdict (not for drops). Live, `1`-`1073741824` |
| `trust.rollback_threshold_pct` | `10` | Regression bar on a target's call-weighted mean. Live, `0`-`100` |
| `verify.min_samples` | `30` | Calls each window needs |
| `verify.min_gain_pct` | `20` | Gain bar for `improved` |
| `verify.write_impact_pct` | `20` | Largest rise of the WAL bytes per tuple written to an index's table, after the build, that keeps the index; above it the index reverts as `write_impact` |
| `verify.window_max_minutes` | `4320` | Longest a non-drop action is watched for evidence |
| `verify.drop_window_hours` | `168` | Business cycle an index drop is verified over, `1`-`8760`. See [Fast elevation](#fast-elevation-dogfood-databases) |

#### Value report

`GET /api/v1/value?database=<name|all>&since=YYYY-MM-DD&until=YYYY-MM-DD` reports the DBA
hours saved, read from each monitored database's own ledger. Days are UTC. `since` and `until`
are both inclusive: `until` covers its whole day (the bound is the next midnight, exclusive), so
`since=2026-08-01&until=2026-08-01` is the report for 1 August. An `until` before `since` is a
400.
The response also carries a `cost` section; see
[Cost attribution and right-sizing](#cost-attribution-and-right-sizing-cost).

Only an action whose current verification (the row `sage.action_log.verification_id` links)
is a completed success earns toil credit; an older success never outweighs a newer revert.
Rolling an action back zeroes its toil credit and, in the same statement, stamps
`reverted_at` on the incident credits it earned: the row stays as evidence and the report and
`pg_sage_incidents_avoided_total` stop counting it. Both value series on `/metrics` are gauges,
since both go down on a rollback, and are read at most once a minute (a read in which any
database failed is retried on the next scrape).

#### Cost attribution and right-sizing (`cost`)

The value report's `cost` section answers what an instance costs, which queries drive it and
whether it is the right size. pg_sage ships no price tables: every figure comes from the rates
you set, and the section and the Value page show `rate_source` and `as_of` beside it. Without
`cost.rates` the section is `available: false` with `reason: "rates not configured"` and no
figure appears anywhere. The block is optional and read at startup (restart after changing it).
There are no environment variables for it: rates are a list.

```yaml
cost:
  rates:
    - resource: "rds:us-east-1/orders"   # as cloud telemetry status names it
      currency: USD
      as_of: "2026-11-01"                # when you read the prices
      compute_per_hour: 0.504
      storage_gb_month: 0.115
    - database: reporting                # self-managed: the pg_sage database name
      currency: EUR
      as_of: "2026-10-15"
      compute_per_hour: 0.30
      vcpus: 8                           # telemetry cannot tell; used for idle share
    - database: lakebase-main
      currency: USD
      as_of: "2026-11-01"
      cu_per_hour: 0.40
      compute_units: 4
```

| Key | Meaning |
|---|---|
| `resource` / `database` | Exactly one: the provider resource (`rds:<region>/<instance>`, `aurora:<region>/<instance>`, `cloud-sql:<project>:<instance>`) or the pg_sage database name. A resource entry wins over a database entry. |
| `currency` | ISO 4217 code of every price in the entry (USD, EUR, GBP, JPY, ...). An unknown code fails the load. |
| `as_of` | `YYYY-MM-DD` the prices were read; required, not in the future. |
| `compute_per_hour` | Instance compute price per hour, as you pay it (on-demand, reserved, negotiated). |
| `dbu_per_hour`, `cu_per_hour` + `compute_units` | Instead of `compute_per_hour`: a DBU or capacity-unit price times the fixed units the instance runs at. Exactly one compute price is allowed. |
| `storage_gb_month` | Per GB-month of the allocated storage cloud telemetry reports (unknown without telemetry). |
| `iops_month` + `provisioned_iops` | Per provisioned IOPS-month. |
| `vcpus` | vCPUs when telemetry cannot tell (self-managed); 0 uses telemetry. |

A negative, NaN or infinite price, a missing date or two compute prices fail the load.

**Attribution.** Over the window (the report's `since`/`until`, default the last 7 days, at most
35), compute cost = `compute_per_hour` x hours, counted once per instance however many of its
databases pg_sage monitors. Each query's cost is the compute cost x its share of the recorded
execution time: the change of `total_exec_time` between consecutive `sage.query_store` samples
of one statistics epoch. A pair across a `pg_stat_statements` reset or eviction is dropped and
counted in `dropped_pairs`, never differenced into a negative delta. `idle_share` = 1 - recorded
execution time / (window x vCPUs), so you can see how much of the instance no query used; it is
null when the vCPU count is unknown. The section lists the top 20 queries by cost and, per
resource, compute, storage, IOPS and total; instances priced in different currencies have no
fleet total (`currency: "mixed"`). Databases with no matching rate are named in `unpriced`.

**Host usage history.** Cloud telemetry (RDS, Aurora provisioned, Cloud SQL) rolls each poll into
`sage.host_usage_hourly` in every member database: CPU p50/p95/max, minimum available memory, DB
load p95, peak connections, the instance class and its vCPUs, 35 days kept. A resize starts a new
row. A failed write is logged with the resource (at most hourly while unchanged) and never fails
the poll. The hourly
[`instance_rightsize`](findings.md#instance-right-sizing) rule reads it: advisory only, info
severity, no SQL, and pg_sage never resizes an instance. AlloyDB and Azure have no host
telemetry yet, so they get attribution from configured rates but no right-sizing; Lakebase is
priced with `cu_per_hour` and its autoscaling is never right-sized.

### Maintenance windows

`trust.maintenance_window` and the standing policy's `maintenance_windows`
use one grammar. An invalid value is rejected when the config is loaded,
reloaded or saved.

| Form | Example | Meaning |
|---|---|---|
| Always | `always`, `24x7` | Every minute |
| Preset | `nights`, `weeknights`, `weekends`, `weekdays`, `business-hours`, `off-hours` | `daily 22:00-06:00`, `weekdays 22:00-06:00`, all day Sat-Sun, all day Mon-Fri, `weekdays 09:00-17:00`, `daily 20:00-08:00` |
| Days | `sat,sun`, `Mon-Fri`, `daily` | All day on those days (names are case-insensitive; `fri-mon` wraps) |
| Time range | `22:00-02:00`, `weekdays 01:00-05:00` | A range that ends before it starts wraps midnight and belongs to the day it starts on: `weeknights` includes Friday night into Saturday morning, not Sunday night |
| Cron | `0 2 * * *`, `0 2 * * 1-5 @30m` | Five fields with lists, ranges and steps. Each matching minute opens a one-hour window unless `@<duration>` (whole minutes, `@1m` to `@24h`) says otherwise |
| Time zone | `weekdays 01:00-05:00 America/Chicago` | A trailing IANA zone evaluates the window on that zone's clock, including DST. Without one, the process clock (UTC in the container) is used. `Local` is refused as ambiguous |

`never`, `off`, `none` and `disabled` are accepted only for
`trust.maintenance_window`; a policy always names at least one window.

A `moderate` or `high` action may start only inside the effective window:
`trust.maintenance_window` (when set) intersected with any of the policy's
`maintenance_windows`, with at least the policy's `window_start_margin_minutes`
left before it closes. Name a zone in policy windows: one without a zone is
read on the process clock (UTC in the container), so the staffed profile's
`weekdays 01:00-05:00` opens at 01:00 UTC; startup logs each such window. A
new staffed policy takes its zone from `trust.maintenance_window` when that
names one.

**Approve now, run in the next window.** Approving an action that only the
window withholds (on the Actions page, with `POST
/api/v1/actions/{id}/approve`, or with a chat card's Approve) schedules it:
the item becomes `scheduled`, the response (and the chat reply) names the
opening, for example "Scheduled for the window opening Tue 2026-11-17 01:00
America/Chicago", and the Actions page lists it as "Scheduled for <time>"
with Cancel. `POST /api/v1/actions/{id}/schedule` schedules explicitly (409
when anything but the window refuses, or the action could run now) and `POST
/api/v1/actions/{id}/unschedule` cancels (operator role, `?database=` in
fleet mode). When the window opens, the executor asks the policy gate again
under the policy then in force, so an emergency stop, a narrowed policy or a
revised recommendation withholds it and returns it to pending with the
reason. A schedule covers one window: if the sidecar misses the window, the
item returns to pending. A chat Approve refused for any other reason leaves
the card usable. The next opening is searched for 8 days; a window that does
not open (with room for the margin) in that time is refused as before.

To check a value before saving it, use Check window under the field in
Settings, or `POST /api/v1/config/validate-window` (admin) with
`{"expression": "weeknights America/Chicago"}`. It validates the value as a
save would (`400` with the parser's message when it does not parse) and
answers `open_now`, `closes_at`, `next_open` and `next_close` (RFC 3339, in the
window's zone, `null` when nothing happens within `horizon_hours`, 192). Unset
and the `never` aliases answer `disabled: true`. It reads and writes nothing.

### LLM

LLM features are on by default, but they call a provider only once `llm.endpoint` and
`llm.api_key` (or `SAGE_LLM_ENDPOINT` and `SAGE_LLM_API_KEY`) are set. With
`llm.auth: databricks`, an endpoint and Databricks unified authentication replace
the static API key; see [Lakebase and Unity Gateway](lakebase.md). Until then every
feature runs its deterministic path, no LLM request is made, and startup logs one line
saying the LLM is not configured. `llm.token_budget_daily` caps spend per client (the
shared client and each database's general and optimizer client), unless
`llm.fleet_token_budget_daily` is set; `llm.global_token_budget_daily` caps the sum, and a
stricter policy `budgets.llm_tokens_daily` also applies (see the
[LLM costing guide](llm-costing.md#global-ceiling-and-policy-cap)). Turning a feature on grants no autonomy: what an LLM proposes executes only
through the policy gate, trust level and execution mode, like any other recommendation.

Daily LLM usage survives restarts. The sidecar writes today's spend of every client, the
global ceiling and each database's `fleet_token_budget_daily` share to
[`sage.llm_usage`](sql-reference.md#sagellm_usage) in the control database every 60 s and on
shutdown, and adds it back at startup, so a deploy, a crash loop or `/restart` mid-day does
not grant the full daily budget again. A crash skips the shutdown flush, so each crash
loses up to the last 60 s of spend. `GET /api/v1/llm/status` reports `usage_restored_at`
and `ledger_last_flush_at` (null until the first restore or flush). If the control database
is unreachable, LLM calls keep running on local accounting and one warning is logged per
outage; a restart during the outage starts from the last flushed usage. A meta-DB database
keeps its usage across a rename (its row is keyed by id); renaming a YAML database in
`databases[]` starts the new name at zero for the rest of the day. Each
sidecar keeps its own counts: two sidecars sharing one control database do not share one
budget.

| Parameter | Default | Description |
|---|---|---|
| `llm.enabled` | `true` | Master switch for every LLM feature; `false` is a hot kill switch |
| `llm.endpoint` | (none) | OpenAI-compatible chat completions endpoint |
| `llm.model` | (none) | Model name |
| `llm.api_key` | (none) | API key (supports `${ENV_VAR}` expansion) |
| `llm.auth` | `api_key` | `databricks` authenticates each request using the official SDK and restricts credentials to the configured workspace HTTPS origin |
| `llm.timeout_seconds` | `30` | Timeout of each request attempt (see [provider errors and the circuit breaker](#llm-provider-errors-and-the-circuit-breaker)). Reasoning models (o-series, gpt-5+, Gemini 2.5+/3, DeepSeek R1, QwQ) get at least 120 s per attempt |
| `llm.cooldown_seconds` | `300` | Duplicate-prompt window and circuit-breaker cooldown |
| `llm.token_budget_daily` | `500000` | Maximum tokens per day for each LLM client (every per-database general and optimizer client gets its own copy) unless `fleet_token_budget_daily` is set; `0` means no cap. A stricter policy `budgets.llm_tokens_daily` wins |
| `llm.global_token_budget_daily` | `0` | Process-wide daily cap across every LLM client (Ask Sage keeps its own). `0` derives today's effective total: the sum of every client's `token_budget_daily`, where a running fleet budget (`fleet_token_budget_daily` in fleet mode) stands in for the per-database clients it scopes; unlimited if any counted client is, so the default changes no spend. Hot-reloadable; reported as `global` in `GET /api/v1/llm/status` |
| `llm.context_budget_tokens` | `8192` | Trim target for context attached to a prompt, in tokens (about 4 bytes each): the advisor's data, the tuning agent's case packet and the EXPLAIN plan sent to the model are cut to it when it is below their own caps (16 KiB, 12000 bytes and 64 KiB). `0` turns off the extra trim; a negative value fails at load. It never refuses a request and does not cap answers (the briefing's cap is 4096 tokens, plus the reasoning reserve on reasoning models) or agent conversations. Separately, a prompt whose estimate does not fit the model's input limit once `max_tokens` is set aside is refused before it is sent (`LLM prompt too large`), costing no tokens and not counting toward the circuit breaker; the limit is known only where the model listing reports it (Gemini native), elsewhere the guard is inactive |
| `llm.redact_numeric_literals` | `true` | Replace numbers of 6 or more digits that are compared (`=`, `<>`, `<`, `>`, `IN (...)`, `ANY(ARRAY[...])`, `BETWEEN`, the log form `Key (c)=(...)`) with `?` in SQL, plan and log text before it reaches a model. Short numbers, numbers outside comparisons and plan JSON numbers are kept. String, dollar and comment redaction always apply. Process-wide; applies on reload. See [Redaction before text reaches a model](security.md#redaction-before-text-reaches-a-model) |
| `llm.pricing` | (empty) | Optional prices in USD per million tokens by model id, `{model: {input_per_m, output_per_m, reasoning_per_m}}`, for `est_cost_usd` in `GET /api/v1/llm/usage` ([LLM costing](llm-costing.md#monitoring-token-usage)). Input and output are required, reasoning optional (it prices the reasoning share of the completion; default the output rate). A negative, non-finite or missing price, or an unknown kind, fails at load. pg_sage ships no prices: an unpriced model's cost is `null`. Restart to apply |
| `llm.optimizer.enabled` | `true` | Let the tuning agent propose index creates (validated and HypoPG-measured by the optimizer's admission) and drops of indexes another valid index covers, each marked for approval when the database has standbys (primary statistics miss replica reads). Never-scanned indexes are proposed only by the `unused_index` rule, with its first-seen clock and its recently-created, FK-only and standby guards. With `tuning.enabled` it also turns on the [deterministic index floor](#missing-index-advice-without-an-llm), which proposes index creates when the agent is inactive |
| `llm.optimizer.min_query_calls` | `100` | Minimum query calls before optimizing a table |
| `llm.optimizer.max_new_per_table` | `3` | Max new index proposals per table per cycle |
| `llm.optimizer.max_include_columns` | `3` | Most INCLUDE columns a proposed index may carry. The optimizer refuses a wider proposal before any HypoPG what-if, with a reason naming the cap so the tuning agent can propose a narrower index; `0` or less means `3`. The deprecated `llm.index_optimizer.max_include_columns` still sets it |
| `llm.optimizer.rejection_memory.enabled` | `true` | Remember HypoPG what-if rejections of model-proposed indexes: a repeat of the same idea (same method, keys with opclass/collation/order and predicate; any name; INCLUDE columns reordered, added or removed) skips the what-if, and the tuning agent's case packet lists the shapes already measured. Applies to the agent's candidates and what-if tool calls, never to deterministic findings. Rows live in `sage.optimizer_rejection` and age out with `retention.findings_days` (YAML only) |
| `llm.optimizer.rejection_memory.max_age_days` | `7` | Days a rejection stays valid; then the idea may be measured again (1-90) |
| `llm.optimizer.rejection_memory.call_volume_ratio` | `2` | A target query whose call count changed by at least this factor (up or down) is a material workload change (>1-1000) |
| `llm.optimizer.rejection_memory.mean_time_ratio` | `2` | Same, for a target query's mean execution time (>1-1000) |
| `llm.optimizer.rejection_memory.row_estimate_ratio` | `2` | Same, for the table's live-row estimate (>1-1000). A target query appearing or disappearing is always a material change |
| `llm.optimizer.rejection_memory.prompt_max_shapes` | `5` | Most recent rejected shapes per table listed in the tuning agent's case packet as already measured (1-20) |
| `llm.optimizer.rejection_memory.skip_llm_after` | `3` | After this many consecutive answers about a tuning case were all wasted (every proposal rejected or already measured, or the answer empty or malformed), the agent stops asking the model about the case until it changes materially (its statements, or a call-volume or mean-time change of `call_volume_ratio`/`mean_time_ratio`) or `max_age_days` passes (1-100). A provider failure neither extends nor breaks the streak. Exported as `pg_sage_optimizer_llm_calls_skipped_total{database}`; skipped what-ifs as `pg_sage_optimizer_whatif_skipped_total{database}` |
| `llm.optimizer_llm.enabled` | `false` | Dedicated optimizer model; adds a second client with its own `token_budget_daily`. It inherits `llm.api_key` only when `llm.optimizer_llm.endpoint` is empty or on the same origin as `llm.endpoint`; otherwise set `llm.optimizer_llm.api_key` |
| `llm.review_llm.enabled` | `false` | Dedicated Sage Review model, the same keys as `llm.optimizer_llm` (`endpoint`, `api_key`, `model`, `timeout_seconds`, `token_budget_daily`, `cooldown_seconds`, `max_output_tokens`, `fallback_to_general`). When off, the review uses the optimizer model when enabled, else the general client. The general key is sent only to the same origin |
| `advisor.enabled` | `true` | LLM configuration advisor (WAL, connections, rewrites, bloat). With `advisor.memory_enabled` / `advisor.vacuum_enabled` it also lets the tuning agent propose server settings / table storage parameters. In fleet mode one database per PostgreSQL instance owns the instance settings: only it runs the WAL and connection reviews and proposes `ALTER SYSTEM` or managed parameter changes (see [Several databases on one PostgreSQL instance](walkthrough-fleet.md#several-databases-on-one-postgresql-instance)) |
| `advisor.interval_seconds` | `86400` | Seconds between model reviews of each configuration sub-advisor (WAL, connections, rewrites, bloat), scheduled per sub-advisor in `sage.advisor_runs` so a restart within the interval makes no model call and concurrent cycles make one. A run counts only on success: a model or token-budget failure retries after 1 h, doubling per consecutive failure, never later than this interval. Every cycle, without the model, open advisor findings are re-checked: a WAL or connection setting resolves once the live `pg_settings` value equals the recommended one (units normalized, `4GB` = `4096MB`), a rewrite once its query has been absent from `pg_stat_statements` for 3 cycles, a bloat finding once its table stays under 10% dead tuples (or 1,000 rows) for 2 cycles. An open finding keeps only its own setting, query or table out of the next review; the rest of its category is still reviewed |
| `tuner.hint_absent_cycles` | `3` | Analyzer cycles an open `query_tuning` finding's statement must be absent from `pg_stat_statements`, or below the tuner's admission thresholds, before the finding resolves; the wait is N x the analyzer interval. 0 or less means 3 (see [hint verification](hint-verification.md#when-a-query_tuning-finding-resolves)) |
| `tuner.param_shape_min_variants` | `20` | Distinct pg_stat_statements queryids of one statement shape, its `IN ($1, ...)` and `= ANY (ARRAY[$1, ...])` parameter lists collapsed, before the tuner reports a `query_parameterization` finding (advice to use prepared statements or one array parameter; no SQL, no hint). Read from the collector's latest snapshot, so it adds no pg_stat_statements read. Off on PostgreSQL 18, which squashes lists. 2-1000 (see [hint verification](hint-verification.md#when-the-tuner-reports-query_parameterization)) |
| `tuner.hint_cost_tolerance_pct` | `10` | Percent a join or scan hint may raise the statement's estimated plan cost in pg_sage's hinted `EXPLAIN` (plain vs hinted, never `ANALYZE`) before it is refused as `cost_regression`; a hint that leaves the plan unchanged is always refused (`no_effect`). 0 refuses any rise. 0-100. Restart to change (see [hint verification](hint-verification.md#every-hint-is-planned-before-it-is-proposed)) |
| `tuner.plan_max_age_minutes` | `1440` | Age at which a cached plan (`sage.explain_cache`) is stale. The tuner and the tuning agent also treat a plan as stale when it was captured before the last `ANALYZE` of a relation it reads, or before an executed pg_sage action whose finding or decision names one of its relations or indexes. A stale plan is replaced by one bounded plain `EXPLAIN` (`GENERIC_PLAN` on PG16+), never `EXPLAIN ANALYZE`; the tuner does so only for a statement that had a cached plan. 0 or less means 1440, never "no limit" (see [hint verification](hint-verification.md#which-plan-a-hint-is-built-from)) |
| `tuner.llm_enabled` | `true` | Let the tuning agent propose per-query pg_hint_plan hints; the tuner validates, clamps and records them (YAML only). The tuner's deterministic hint rules run either way. Hints are offered as executable SQL only when they would reach application sessions: pg_hint_plan preloaded, `pg_hint_plan.enable_hint_table` on for the database or role (`ALTER DATABASE mydb SET pg_hint_plan.enable_hint_table = on`), the 1.7 hint table, and `INSERT`/`DELETE` on it. Otherwise findings stay advisory with a `hint_readiness` detail; the tuner re-reads these facts every cycle (see [hint verification, section 10](hint-verification.md#10-hint_planhints-table-integration)) |
| `explain.enabled` | `true` | `POST /api/v1/explain` (see [Natural-language EXPLAIN](explain.md)); the plan rules always fill the reasons and recommendations, and an LLM, when configured, adds to them |
| `rca.narration_enabled` | `true` | Let the LLM rewrite the summary on `incident_detected` and `incident_escalated` notifications. The model can only read the incident's own evidence: its causal chain (`E#`), the results of the fixed read-only catalog probes run for it (`P#`, also readable with `get_probe_result`) and the deterministic causal-graph hypotheses (`H#`). It writes no SQL and has no database access. It answers with claims; each must cite evidence ids, and every number in a claim must equal a value in the evidence that claim cites (integers exactly and with their sign, decimals at the stated precision, timestamps as whole RFC 3339 instants; a probe result's head line, row count and `[i]` row ordinals ground nothing). Budget per narration: 2 model turns, 1,024 output tokens per turn, about 16k input tokens, 20 s per turn; at most 3 narrations per persistence cycle within 45 s. Any failure (LLM off, budget, rate limit, timeout, malformed or uncited output) uses the deterministic summary, which is always labeled. `llm.enabled=false` is the hot kill switch and cancels narrations in flight |

Retired keys: `llm.optimizer.plan_source`, `llm.optimizer.over_indexed_ratio_pct` and `llm.index_optimizer.over_indexed_ratio_pct` had no effect and are no longer read. A config that still sets one loads and prints one startup warning per key; remove them. The optimizer captures no plans (plan_source chose how it once did): the HypoPG what-if plans at most 25 SELECT statements per candidate (writes take no slot; the hottest by total time, always including the proposal's or the case's target statements) under `safety.lock_timeout_ms` capped at 1 s (below its 5 s statement timeout), and a lock timeout leaves the candidate unverified.

### Missing-index advice without an LLM

Missing-index advice does not need a model. When the tuning agent is inactive for a database (no LLM configured, its circuit breaker open, its daily token budget spent, or every conversation of the cycle failing), the deterministic index floor runs in its place on that cycle. There is no config key: the floor follows `tuning.enabled` and `llm.optimizer.enabled`, per database in fleet mode, and waits out `llm.optimizer.min_snapshots` like the agent.

The floor reads the statements of the tables the `seq_scan_heavy` rule flagged this cycle. It proposes a single-column btree on the most-called equality filter whose selectivity is at most 20% (from `pg_stats`: the most common value's frequency, else one over the distinct count; a column without statistics never qualifies), and one composite per table (equality columns first, then one range column). Statements with `OR`, subqueries or more than one `WHERE` clause contribute nothing. In a statement over several relations (a join, a `FROM` list, `UPDATE ... FROM`, `DELETE ... USING`) only filters qualified by the table's name or alias count, so another table's filter is never read as this one's. At most 3 candidates per table and 10 per cycle are judged. Every candidate passes the same admission as the agent's: the canonical form, the validator, rejection memory and the HypoPG what-if. A shape the agent already proposed (an open finding) is never proposed again.

With HypoPG installed, a measured candidate below `llm.optimizer.hypopg_min_improvement_pct` is rejected and a verified one carries its `CREATE INDEX CONCURRENTLY`, executed only through the policy gate and trust level. Without HypoPG the finding is advisory: no executable SQL, the DDL in `detail.ddl`, and the note "HypoPG not installed: impact unmeasured". Floor findings carry `detail.source = "deterministic_floor"`.

### Connection advisor

The connection review (`connection_tuning`, every `advisor.interval_seconds`) sizes
against the advisor window, not one moment:

- **`max_connections` floor.** The lowest value it lets through is the peak of total
  backends over the window (read from the `system` snapshots in `sage.snapshots`, or the
  current count if higher) + `superuser_reserved_connections` + `reserved_connections`
  (PostgreSQL 16+) + 10. A lower value is refused. Without a readable peak (no history yet,
  or a history read error, logged with the database name) a value below the current total
  backends + reserved slots + 10 is still refused, any other decrease is kept as advice
  without SQL, and an increase is kept. A `max_connections` change always needs approval,
  since it needs a restart.
- **Scopes.** The prompt states this database's figures (active sessions, connection
  states, sessions opened) apart from the instance's (total backends, its peak,
  `max_connections`, reserved slots), which every database shares.
- **Churn.** Sessions opened is the `pg_stat_database.sessions` delta over the window
  (PostgreSQL 14+), so connections that already closed count; it is unknown when the
  statistics were reset in the window. pg_sage's own sessions are included.
- **Pooler advice and observations** are kept as `info` findings without SQL.
- **`idle_in_transaction_session_timeout`** runs only at database scope and only with an
  operator's approval: pg_sage turns the model's `ALTER SYSTEM SET` into
  `ALTER DATABASE <this database> SET`, between 1 min and 24 h (0, disabled, is never
  recommended), and the executor refuses it under `ALTER SYSTEM`. It is advice only when
  pg_sage's role does not own the database. On RDS, Aurora, Cloud SQL, AlloyDB, Azure,
  Neon, Supabase and Lakebase the same `ALTER DATABASE` form runs (the setting has user
  context); it never becomes an instance-wide provider parameter change. Rollback restores
  the database's prior value.
- **`statement_timeout` and `idle_session_timeout`** are advice only, at every scope.

In fleet mode the floor is instance-wide and the connection review runs once per
instance, from the database that owns the instance settings; the timeout it proposes is
for that database.

### Workload budgets

`budgets` (default: none) declares per-role, `application_name` or sqlcommenter-tag
limits (`statement_timeout`, `idle_in_transaction_session_timeout`, `connection_limit`,
`pool_size`, `mode: warn|enforce`). Warn mode, the default, only counts would-be
violations into `sage.budget_shadow`; a role budget in `enforce` mode becomes
`ALTER ROLE` actions after 7 days of clean shadow data, the connection limit always with
approval. `advisor.budget_proposals` (default `true`) proposes a `statement_timeout` per
role from its statement time. See [workload budgets](workload-budgets.md).

### WAL and bloat advisors

The WAL review (`wal_tuning`) and the bloat review (`bloat_remediation`) also reason over
the advisor window (`advisor.interval_seconds`, a day by default). They compare the latest
snapshot with the earliest `system` or `tables` snapshot inside the window, read from
`sage.snapshots` through the history store, not with the previous collector snapshot.

- **WAL facts.** The prompt states the window and what was measured over it: requested
  and timed checkpoints and the requested share, WAL written per hour
  (`pg_stat_wal.wal_bytes`, or the WAL insert position for snapshots from before
  pg_sage recorded it) and WAL per `checkpoint_timeout`. A fact whose statistics were
  reset in the window, or whose counter fell, is stated unknown. The window must span at
  least 3 x `checkpoint_timeout`; a shorter history, or none, makes every fact unknown.
- **No model call when checkpoints keep up.** Under a 20% requested share the WAL review
  makes no model call.
- **`max_wal_size` floor.** The lowest value pg_sage accepts is 1.5 x the WAL per
  `checkpoint_timeout`, rounded up to a megabyte and kept within 512MB-64GB. A model value
  below the current `max_wal_size` or below the floor is dropped; when the floor is a
  raise, a finding that sets the floor replaces it, with the arithmetic in
  `detail.arithmetic`. The model may raise the value up to 4 x the floor; above that it is
  advice without SQL.
- **Advisory without history.** With no window history (a new install, or retention
  shorter than the window), every WAL finding is advice without SQL. A history read error
  skips the WAL review for the cycle (it retries like a model error), so nothing is
  recommended from one snapshot.
- **Verification.** An executed `max_wal_size` or `checkpoint_timeout` change is judged
  by the requested share of checkpoints over the monitor window against the lifetime
  share at apply time (since the checkpoint statistics were last reset): improved under
  20% or at half or less, with at least 3 checkpoints in the window. A lifetime share
  already under 20% is insufficient evidence, even if the advisor window's share was
  higher. A statistics reset is insufficient evidence; a WAL change is never judged a
  regression.
- **Bloat candidates.** The bloat prompt lists the 50 tables with the most estimated dead
  bytes (data size x dead-tuple share), worst first, and states how many more were
  omitted. Each table's dead-tuple trend compares with the window-start `tables`
  snapshot; it is unknown when that snapshot is missing, younger than an hour (or half the
  window), lacks the table, or cannot be read. Bloat advice stays advisory and never
  `VACUUM FULL`.

Checkpoints and WAL are cluster-wide: in fleet mode the WAL review runs once per
instance, from the database that owns the instance settings. Bloat is per database.

### PostgreSQL 18 I/O settings

The config snapshot reads `io_method`, `io_workers`, `io_combine_limit` and
`maintenance_io_concurrency` beside `effective_io_concurrency`. A server returns only the
names it has: `maintenance_io_concurrency` exists on every supported version,
`io_combine_limit` from PostgreSQL 17, `io_method` and `io_workers` from 18. The tuning
agent's case packet lists `maintenance_io_concurrency`, `io_method` and `io_workers` with
their source and context, as context only.

These settings are documented for advice, never proposed as SQL: none is in the executor's
allowlist, so no new write path is added. `io_method` (sync, worker or io_uring; io_uring
needs a Linux build with liburing) needs a restart; `io_workers` (used only with
`io_method = worker`) is reloadable.

On PostgreSQL 18 and later the advisor runs a deterministic check every cycle. It makes no
model call, but it runs inside the advisor, which starts only when `advisor.enabled` is on
and an LLM is configured; it keeps running if the LLM is switched off later. It reports an
[`io_settings`](findings.md#io_settings) `info` finding, without SQL, when a configuration
carried from an older server undoes 18's asynchronous I/O:

- `effective_io_concurrency` at or below 1, or `maintenance_io_concurrency` at or below
  10, from a source other than `default`: the value overrides 18's new default of 16 and
  switches off most of the read-ahead that asynchronous I/O adds;
- `io_method = sync` from a source other than `default`: asynchronous I/O is off.

The finding names the setting's source; on a managed service change it in the
provider's parameter settings. Nothing is reported before PostgreSQL 18 or while the
server version is unknown. In fleet mode the instance-wide values are reported once per
instance, from the database that owns the instance settings; a value set for one database
alone (source `database` or `database user`, from `ALTER DATABASE` or `ALTER ROLE ... IN
DATABASE`) is reported by that database, as `database:<setting>` with `scope = database`.

### LLM provider errors and the circuit breaker

Each LLM client classifies every provider error and reports the class as
`last_error_class` in `GET /api/v1/llm/status`:

| Class | Replies | Retried | Counts toward the breaker |
|---|---|---|---|
| `caller` | 400, 404, 413, 422 and other 4xx (for example a prompt over the context length; Databricks and Gemini's OpenAI-compatible endpoint answer 400) | no | never |
| `auth` | 401, 403 | no | opens the breaker at once |
| `rate` | 429 | yes, after `Retry-After` | once, when the last attempt is still 429 |
| `server` | 500, 502, 503, 504 (other statuses are not retried) | yes | once per request |
| `timeout`, `transport` | an attempt that ran out of time, a refused or reset connection | no | once per request |
| `malformed` | a 200 whose body is not a completion | no | once per request |

A chat request makes at most 4 attempts. It waits the provider's `Retry-After` (delay
seconds or an HTTP date, capped at 30 s; Ollama sends none) or else 1, 4 and 16 s, and
never waits past the caller's deadline: a wait that would end after it returns the
provider's error at once. Each attempt has its own `llm.timeout_seconds` deadline,
raised to 120 s for reasoning models. The request is charged to the budgets once, however
many attempts it takes. Tool calls (incident narration, Ask Sage, the tuning agent) make
one attempt, and their 429 returns at once without counting. A request-shape rejection
that pg_sage adapts to (`llm.token_parameter`, `llm.tool_reasoning_effort`) is re-sent and
never counts, and an empty answer is charged but never counts.

The breaker is per client (in fleet mode, per database and purpose). Three counted
failures in a row, or one `auth` failure, open it: requests fail fast for
`llm.cooldown_seconds`. After the cooldown the next request is the single half-open
probe while every other request still fails fast; the probe's success closes the
breaker and its failure reopens it for another cooldown. A probe that ends without a
verdict (a `caller` rejection, a cancellation, a budget refusal) lets the next request
probe. Changing `llm.endpoint`, `llm.api_key`, `llm.auth` or `llm.model` at runtime
closes the breaker, since the failures were the old provider's. Each client in the status
reports `circuit_state` (`closed`, `open`, `half_open`; `circuit_open` stays true only
while requests are refused), `last_error_class`, `last_error_at`, `last_success_at` and
`consecutive_failures`. `POST /api/v1/llm/test` (admin) sends one tiny request through
the normal path, charged to the budget and admitted as the half-open probe even inside
the cooldown, and returns `class` (`ok` or an error class), `latency_ms`, `tokens` and
`circuit_state`, so a fixed key or endpoint can close the breaker at once.

### Tuning agent

One tuning agent per database (roadmap 2.2) replaces the index optimizer, advisor vacuum
and memory, and tuner hint prompts. Each analyzer cycle it classifies the workload
deterministically (application, tenant-family, test-fixture, diagnostic and pg_sage
statements; confirmed facts bind tables), finds the cases worth tuning (a statement taking
at least 5% of the workload's interval time, a statement whose mean time at least doubled,
a table whose index maintenance or dead-tuple churn is a problem) and asks the model about
each case with read-only tools: `statement`, `table`, `explain` (cached plan, or `EXPLAIN`
without `ANALYZE`; generic plans on PostgreSQL 16+), `whatif_index` (HypoPG),
`write_cost`, `extended_stats`, and `rehearse` when a clone provider is configured. The
model answers with typed proposals only (index create or drop, server setting, table
storage parameter, `CREATE STATISTICS` in the accepted `sage_stx_*` form, query hint),
each citing its evidence and predicting its effect; pg_sage generates every statement and
validates it with the same gates as before (index admission and HypoPG, configuration
allowlists and ranges, the tuner's hint checks, confirmed facts, earlier operator
rejections). A change a confirmed fact binds becomes a source-fix packet. The model is
never asked when there is no case. Which proposal types it may make follows
`llm.optimizer.enabled`, `advisor.memory_enabled`, `advisor.vacuum_enabled` and
`tuner.llm_enabled`; extended statistics are always allowed.

Each admitted proposal carries a confidence calibrated on `sage.action_outcome`: per action
class and prediction method, the share of comparable decided outcomes that improved (the
predicted-improvement bin when it has enough outcomes, else the whole class and method).
Below `tuning.calibration_min_outcomes` the proposal is labeled uncalibrated and carries no
number. When the calibrated rate's 95% lower bound is under
`llm.optimizer.confidence_threshold`, an operator must approve the proposal. Proposals are
ranked calibrated-confident first, then uncalibrated, then calibrated-doubtful. The Trust
page and `GET /api/v1/tuning/calibration?database=` show the reliability bins.

Safeguards learned from dogfooding:

- **Open findings are never resolved by absence.** A recommendation the agent did not
  examine this cycle stays open unchanged; only catalog evidence retires one (its table is
  gone, an existing index covers it, the index it would drop is gone).
- **Coverage respects operator class, collation and order.** An index duplicates or covers
  another only when each compared key has the same expression, operator class (with its
  parameters) and collation; for btree the compared keys must also have the same ordering
  or its uniform reversal, NULLS included. So `(email text_pattern_ops)` beside `(email)`,
  an HNSW `vector_cosine_ops` index beside a `vector_l2_ops` one, a `jsonb_path_ops` GIN
  beside a `jsonb_ops` one, or `(a, b DESC)` beside `(a, b)` is neither rejected as a
  duplicate nor dropped or replaced as covered. A default operator class or collation
  spelled out in a candidate (`(email text_ops)`, `COLLATE "default"`) still matches the
  catalog definition that omits it.
- **Daily budget survives restarts.** The agent charges its model tokens per database and
  UTC day to `sage.tuning_budget_day`, capped at the daily token budget of the client it
  uses (`llm.optimizer_llm.token_budget_daily` when the optimizer LLM is enabled and sets
  one, else `llm.token_budget_daily`). Exported as
  `pg_sage_tuning_budget_day_tokens_{used,limit}{database}`.
- **Settings change on fresh evidence only.** A setting or storage parameter needs counters
  measured over the last snapshot interval (a `work_mem` change needs temp spills in it;
  cumulative totals since a statistics reset are never evidence), waits while its last
  change is being verified, needs evidence measured after that change once it is decided,
  and needs an operator when it would be the third change in the same direction within
  7 days (the history is on the approval card).
- **No overlapping indexes.** An index candidate is refused when an existing or in-flight
  index (queued, or an open proposal) already serves it, or when it would make an
  in-flight one redundant; one proposal per table and leading key per cycle; HypoPG
  measures it with the in-flight indexes present.
- **Replacements.** A candidate that would make exactly one existing index redundant
  becomes one replacement (`replace_index`, class `index_replace`): build the wider index
  with `CREATE INDEX CONCURRENTLY`, check it is valid, then `DROP INDEX CONCURRENTLY` the
  old one. It always needs an operator's approval (class capped at L2, and never above the
  levels of `index_create` and `index_drop`); the card shows both statements, the undo
  (re-create the old index, drop the new one) and both locks. pg_sage refuses to replace an
  index that backs a constraint or enforces uniqueness, one whose OID or definition changed
  since the proposal, or one whose foreign key the new index would not support; a table
  owned by the application's migrations gets the two-statement migration instead. The two
  steps are recorded in `sage.index_replace`: a failed build drops nothing, a failed drop
  keeps both indexes and reports a partial result, and a restart resumes the drop or
  restores the old state. The table's change lease is held across both steps.
  Verification judges the targeted queries (no gain or a regression rolls the replacement
  back) and the queries on the table that used the old index; after a kept verdict it
  watches those queries for `verify.drop_window_hours` and re-creates the old index if
  they regress (the soft drop).

| Parameter | Default | Description |
|---|---|---|
| `tuning.enabled` | `true` | Run the tuning agent when an LLM is usable |
| `tuning.max_cases_per_cycle` | `3` | Cases the model examines per cycle; the rest wait (1-20) |
| `tuning.max_requests_per_cycle` | `12` | Model requests (tool turns included) per database per cycle (1-200) |
| `tuning.max_tokens_per_cycle` | `60000` | Model tokens per database per cycle, charged before each request (1000-2000000) |
| `tuning.max_turns_per_case` | `6` | Model turns per case; the last must answer without tools (1-20) |
| `tuning.max_proposals_per_cycle` | `10` | New findings per cycle, best ranked first (1-100). The cap applies before anything is recorded: a cut proposal (a query hint included) leaves no trace and is counted in `pg_sage_tuning_proposals_capped_total{database}`. Cases a cycle cannot reach (case cap, budget, model error) are deferred and asked first in the next cycles, longest waiting first; open findings the agent did not examine are kept unchanged, never resolved by absence. The last cycle's use is exported as `pg_sage_tuning_budget_{tokens,requests}_{used,limit}{database}`, `pg_sage_tuning_cases_asked{database}` and `pg_sage_tuning_cases_deferred{database}`, and shown on the Trust page |
| `tuning.calibration_min_outcomes` | `5` | Decided outcomes a class and method need before a confidence is shown (1-1000) |
| `tuning.calibration_window_days` | `180` | Days of decided outcomes the calibration reads (1-3650) |

### Web UI and API Authentication

Standalone installations use a session login for the embedded web UI and
`/api/v1/*` endpoints. On first
start, pg_sage creates `admin@pg-sage.local` and prints a one-time initial
password to stderr. Use `/api/v1/auth/login` for API scripts and keep the
returned `sage_session` cookie:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/auth/login \
  --data '{"email":"admin@pg-sage.local","password":"INITIAL_PASSWORD"}'

curl -b cookies.txt http://localhost:8080/api/v1/cases
```

`GET /api/v1/cases` takes `?database=`, `?case=`, `?limit=`, `?summary=1`
and `?finding_source=<subsystem>`, which keeps one subsystem's open findings
(`optimizer`, `rules`, `forecaster`, ...; the response's `finding_sources`
lists them) and leaves out incidents and query hints. An unknown value
answers `400` `unknown_finding_source`. See
[Cases API fields](walkthrough-fleet.md) for the full response.

SSO (`oauth.*`) validates the OIDC `id_token` with PKCE and a nonce, can map
IdP groups to roles (`oauth.role_mapping`, `oauth.groups_claim`,
`oauth.unmapped_users`) and to per-database grants
(`oauth.database_grant_mapping`), and audits every login. A break-glass admin
(`oauth.break_glass`) works when the IdP is down and alerts every channel on
use. See [SSO login validation](security.md#sso-login-validation).

Databricks Apps deployments instead set `SAGE_AUTH_MODE=databricks_app` and
`SAGE_DATABRICKS_ADMIN_EMAIL` for the explicitly selected administrator. This
mode requires the Apps runtime's port and workspace host (a bare hostname is
normalized to an HTTPS origin, as in Databricks unified authentication). It accepts
the authenticated ingress email on each protected request and ignores local
session cookies; password/OAuth login, break-glass, account linking and local
user creation/deletion are disabled. The configured administrator is explicitly
bound while preserving its account ID; other newly provisioned platform users
start as viewers. Stored pg_sage roles still control access and are reread per
request. `SAGE_DATABRICKS_APP_IDENTITY_PIN` (`log` by default, `enforce` or `off`)
pins each account's forwarded `X-Forwarded-User` value and audits or refuses a
change; see [Identity pin](databricks-apps.md#identity-pin). Grant/revoke App access through Databricks permissions. See the
[Apps deployment guide](databricks-apps.md) for the trusted-ingress boundary and
resource setup. This browser identity does not change the App service principal
used by Lakebase and Gateway background operations.

#### Behind a reverse proxy

Browser writes to the API must come from the API's own origin
([CSRF model](security.md#csrf-and-cross-origin-requests)). Current browsers
send `Sec-Fetch-Site`, so a proxy that serves the dashboard and the API on one
origin needs nothing extra, even if it rewrites `Host`. Two cases need
`api.trusted_origins` (restart to change):

- The proxy rewrites `Host` (for example nginx `proxy_set_header Host upstream`)
  and some users run browsers that send no `Sec-Fetch-Site`: list the public
  origin, for example `https://sage.example.com`.
- A UI on another origin calls the API: list that origin.

```yaml
api:
  trusted_proxies: [10.0.0.0/8]          # the proxy's addresses
  trusted_origins: [https://sage.example.com]
```

Entries are exact, lowercase origins with no path and no default port
(`https://sage.example.com`, not `https://sage.example.com/` or
`https://sage.example.com:443`). Put the proxy's address in
`api.trusted_proxies` too: the session cookie is marked `Secure` from the
proxy's `X-Forwarded-Proto: https` only when the proxy is trusted, and rate
limits and the failed-login budget follow `X-Forwarded-For` only from a
trusted proxy. Without it every user shares the proxy's address and its
[login throttling](security.md#login-throttling) budget; pg_sage logs one
warning naming `api.trusted_proxies` when it ignores the header.

#### API access log

`api.access_log` (default `true`, restart to change) writes one `api access`
log line per mutating API request and per 401/403: method, route pattern, user
id, role, status and latency, never bodies or query strings
([details](security.md#api-access-log)). Set it to `false` only when another
layer (a proxy's access log) already records who changed what.

```yaml
api:
  access_log: true
```

#### Migration lint rate

`api.migration_lint_rate_per_minute` (default `30`, restart to change) is how
many requests each signed-in user may send to `POST /api/v1/migrations/lint` in
any minute; the next one gets 429 with `Retry-After`. 0 or less means the
default. Each request reads catalog statistics on the monitored database under a
2 s `statement_timeout`, and at most 4 run at once. The CLI gate
(`pg_sage lint-migration`) is not rate limited: it uses its own
connection ([Migration review in CI](migration-ci.md)).

```yaml
api:
  migration_lint_rate_per_minute: 30
```

#### Per-database grants

`api.database_grants` (default `false`, restart to change) enforces the
per-database grants admins set on the Users page, in fleet and meta-db modes
only ([details](security.md#per-database-authorization)). A user with no grants
keeps their global role everywhere; a user with grants is an operator only on
the databases granted as operator. While it is off, grants are stored but not
enforced.

```yaml
api:
  database_grants: true
```

`oauth.database_grant_mapping` (default empty, restart to change) lets SSO
groups set those grants instead (apiauth-08d): at every SSO login the grants a
user's groups give replace the grants the IdP gave before. An admin's manual
grants are never changed by it and win on the same database. Each entry needs
a `group` (exact, case-sensitive), one `database` and a `role` of `viewer` or
`operator`; any other role, an empty or `all` database, or the same group twice
on one database stops startup with an error naming the entry
([details](security.md#group-to-database-grant-mapping)).

```yaml
oauth:
  database_grant_mapping:
    - {group: team-billing, database: billing, role: operator}
    - {group: team-search,  database: search,  role: viewer}
```

#### Profiling the sidecar

`debug.pprof_enabled: true` (default `false`, restart to change) serves Go's profiler
on the API listener at `/api/v1/debug/pprof/`, to signed-in **admins only**, through
the same session login as every other API route; operators and viewers get 403,
requests without a session (and MCP tokens) get 401, and with the setting off the
path does not exist (404). It never runs on the Prometheus listener. Responses are
`Cache-Control: no-store`. A CPU profile or trace takes `?seconds=1`-`25` (default 10,
inside the API's 30 s request deadline). Enabling it also turns on light block and
mutex sampling. The process command line is not served (its arguments can carry
credentials). Profiles reveal code paths and memory: turn it on while diagnosing, then
off.

```bash
# goroutine dump of a running sidecar, no restart, no signal
curl -b cookies.txt 'http://localhost:8080/api/v1/debug/pprof/goroutine?debug=2'
# 10 s CPU profile, then: go tool pprof -top profile.pb.gz
curl -b cookies.txt -o profile.pb.gz 'http://localhost:8080/api/v1/debug/pprof/profile?seconds=10'
```

### Agent-native autonomy

The `policy`, `verify`, `clone`, `custodian`, `value`, and `mcp` sections
configure the closed-loop autonomy stack. MCP is an intent-level JSON-RPC
surface: it exposes policy, change-request, and evidence-ledger tools, never
raw SQL execution. Caller claims are recorded as untrusted input and do not
grant authority. MCP is served over HTTP by default (`mcp.transport: http`,
`POST /api/v1/mcp`), accepting only MCP API tokens (never a session); set
`mcp.transport: stdio` for stdio. See [MCP for coding agents](mcp.md).

The default `unattended` policy profile permits explicitly bounded deadline
overrides for XID and disk emergencies. Use `staffed` for narrow maintenance
windows without deadline overrides. Clone-backed migration rehearsal defaults
to disabled (`clone.provider: none`) and stale clones are recommendation-only.
The clone adapters are `dle` (Database Lab Engine), `lakebase_branch` (an
instant Lakebase branch per rehearsal, below), `neon_branch` (a copy-on-write
Neon branch per rehearsal, below), `aurora_clone` (an Aurora copy-on-write
clone of the monitored cluster per rehearsal, below) and `rds_restore` (a
point-in-time restore of the monitored RDS instance per rehearsal: a full copy at
the full instance price, which needs `clone.allow_full_copy: true` or startup
fails). `snapshot` is refused at startup with the names that replace it:
`rds_restore`, `cloudsql_clone`, `alloydb_restore` and `azure_restore`, the
full-copy restores. `cloudsql_clone` (a Cloud SQL instance clone) and
`alloydb_restore` (a point-in-time restore of the monitored AlloyDB cluster) and
`azure_restore` (a point-in-time restore of the monitored Azure flexible server) are
full copies too and need `clone.allow_full_copy: true`.

| Key | Default | Meaning |
|---|---|---|
| `clone.provider` | `none` | `none`, `dle`, `lakebase_branch`, `neon_branch`, `aurora_clone`, `rds_restore`, `cloudsql_clone`, `alloydb_restore` or `azure_restore` |
| `clone.lakebase.project` | (none) | Lakebase project id the rehearsal branches are created in. Required for `lakebase_branch` |
| `clone.lakebase.source_branch` | the monitored endpoint's branch | Branch id, or `projects/<project>/branches/<id>` of the same project, a rehearsal branches from |
| `clone.lakebase.ttl_minutes` | `60` | Server-side expiry of every rehearsal branch, 1 to 1440: the backstop if pg_sage cannot delete it |
| `clone.lakebase.endpoint_max_cu` | `1` | Autoscaling maximum of the read-write endpoint on a rehearsal branch |
| `clone.neon.project_id` | (none) | Neon project id the rehearsal branches are created in (the monitored database's project). Required for `neon_branch` |
| `clone.neon.parent_branch` | the monitored endpoint's branch | Branch id (`br-...`) or name in the project a rehearsal branches from; one outside the project is refused |
| `clone.neon.api_key` | (none) | Neon API key (secret; also `SAGE_CLONE_NEON_API_KEY` or `_FILE`). Without it `neon_branch` creates nothing, rehearsals are recommendation-only and startup logs a warning |
| `clone.neon.endpoint_max_cu` | `1` | Autoscaling maximum, 0.25 to 8 compute units, of the read-write endpoint on a rehearsal branch (minimum 0.25) |
| `clone.neon.ttl_minutes` | `60` | Expiry Neon sets on every rehearsal branch, 1 to 1440: the backstop if pg_sage cannot delete it |
| `clone.aurora.region` | (none) | AWS region of the monitored Aurora PostgreSQL cluster; clones are made, and leaked ones looked for, there. Required for `aurora_clone` |
| `clone.aurora.instance_class` | the source writer's class | Class of the clone's one instance, such as `db.r6g.large` or `db.serverless` (Serverless v2, which needs a Serverless v2 source) |
| `clone.aurora.ttl_minutes` | `60` | Longest an Aurora clone lives, 30 to 1440; tagged on the clone, and a rehearsal (clone creation included) ends by then. Aurora has no server-side expiry: the reaper destroys a leaked clone |
| `clone.rds.region` | (none) | AWS region of the monitored RDS for PostgreSQL instance; restores are made, and leaked ones looked for, there. Required for `rds_restore` |
| `clone.rds.instance_class` | the source instance's class | Class of the restored instance, such as `db.m6g.large` |
| `clone.cloudsql.project` | (none) | Google Cloud project of the monitored Cloud SQL for PostgreSQL instance; clones are made, and leaked ones looked for, there. Required for `cloudsql_clone` |
| `clone.cloudsql.instance` | (none) | Instance id of the monitored Cloud SQL for PostgreSQL instance; pg_sage must connect to one of its IPs. Required for `cloudsql_clone` |
| `clone.alloydb.project` | (none) | Google Cloud project of the monitored AlloyDB cluster; restores are made, and leaked ones looked for, there. Required for `alloydb_restore` |
| `clone.alloydb.location` | (none) | Region of the monitored AlloyDB cluster, such as `us-central1` (not a zone). Required for `alloydb_restore` |
| `clone.alloydb.cluster` | (none) | Cluster id of the monitored AlloyDB cluster; it needs continuous backup on, and pg_sage must connect to its primary instance's IP. Required for `alloydb_restore` |
| `clone.azure.subscription_id` | (none) | Azure subscription ID (a GUID) of the monitored flexible server; restores are made there. Required for `azure_restore` |
| `clone.azure.resource_group` | (none) | Resource group of the monitored flexible server; restores are made, and leaked ones looked for, there. Required for `azure_restore` |
| `clone.azure.server` | (none) | Name of the monitored flexible server (not its FQDN); pg_sage must connect to its `<name>.postgres.database.azure.com` FQDN. Required for `azure_restore` |
| `clone.restore_deadline_minutes` | `120` | Longest a full-copy restore or clone (`rds_restore`, `cloudsql_clone`, `alloydb_restore`, `azure_restore`) may take to become available, 30 to 720; one still not ready is left to the reaper, which finds it by tag or name. A full-copy rehearsal is bounded by this plus one hour, and its receipt lives 15 minutes longer |
| `clone.rehearsals_per_hour` | `2` | Most index rehearsals on a clone one database starts in any hour (tuning agent and index floor together), 1 to 60 |
| `clone.allow_full_copy` | `false` | Allow full-copy clones (a whole-instance restore: hours and a full instance bill). Without it a full-copy provider, or one that does not say what its clones are, creates nothing. `true` with a provider that makes no full copies logs a startup warning |
| `clone.max_creates_per_hour` | `4` | Most clones this deployment creates in any hour, every purpose together, 1 to 60; the next create is refused naming the limit |
| `clone.rehearsal_statement_timeout_seconds` | `5` | Longest one statement an index rehearsal executes on the clone may run, 1 to 300 seconds; a statement that hits it records "timed out on the clone", not a cost |

The `clone.lakebase.*` keys are read at startup (restart to change them) and are
not dashboard overrides. They are checked only when `clone.provider` is
`lakebase_branch`. See [Lakebase: branch rehearsal](lakebase.md#branch-rehearsal).
The `clone.neon.*` keys are likewise read at startup and checked only when
`clone.provider` is `neon_branch`. See
[Neon and Supabase: branch rehearsal](neon-supabase.md#branch-rehearsal-on-neon).
The `clone.aurora.*` keys are read at startup and checked only when
`clone.provider` is `aurora_clone`. See
[Managed clouds: Aurora clone rehearsal](managed-clouds.md#aurora-clone-rehearsal).
The `clone.rds.*` keys and `clone.restore_deadline_minutes` are read at startup and
checked only when `clone.provider` is `rds_restore`. See
[Managed clouds: RDS restore rehearsal](managed-clouds.md#rds-restore-rehearsal).
The `clone.cloudsql.*` keys are read at startup and checked only when
`clone.provider` is `cloudsql_clone` (which also checks `clone.restore_deadline_minutes`).
See [Managed clouds: Cloud SQL clone rehearsal](managed-clouds.md#cloud-sql-clone-rehearsal).
The `clone.alloydb.*` keys are read at startup and checked only when
`clone.provider` is `alloydb_restore` (which also checks `clone.restore_deadline_minutes`).
See [Managed clouds: AlloyDB restore rehearsal](managed-clouds.md#alloydb-restore-rehearsal).
The `clone.azure.*` keys are read at startup and checked only when
`clone.provider` is `azure_restore` (which also checks `clone.restore_deadline_minutes`).
See [Azure: restore rehearsal](azure.md#restore-rehearsal).

A rehearsal clone holds production data, so pg_sage keeps a receipt for it
(`sage.clone_instances`, purpose `rehearsal`) in the agent governance control
database, else in the target database's `sage` schema. The receipt is written
before the clone is requested and is closed when the clone is destroyed. With no
receipt store the rehearsal is recommendation-only with reason
`no_receipt_store`. A rehearsal is bounded to one hour and its receipt expires
15 minutes later. Every 10 minutes the fleet leader destroys the clones of
expired rehearsal receipts of its own adapter, endpoint (for `lakebase_branch`
and `neon_branch`, the project; for `aurora_clone`, `aws/<region>`) and
deployment. An
expired receipt that never got a clone id is logged, never destroyed. Before the
first step the rehearsal checks that the clone is not the production server:
the same DSN host and port, or the same postmaster start time and port under
any name, is refused (`target_is_production`), and nothing runs on it.

Clone lifecycle (lab-05a). Each adapter says what kind of clone it makes:
`dle` a thin clone (no new instance), `lakebase_branch` and `neon_branch` a
branch, `aurora_clone` a copy-on-write clone (one new instance); a full-copy
restore needs `clone.allow_full_copy`. Before the clone is requested its
receipt gets a cost label: instance-hours for the receipt's lifetime and, when a
`cost.rates` entry names the database, a figure in that entry's currency with
its date; otherwise "unpriced" (pg_sage ships no price tables). The receipt also
names the sidecar process holding the clone (`holder_id`), which heartbeats it
every minute. On startup and every pass the leader's reaper also destroys, before
their TTL, the clones of this deployment, any purpose, whose holder is another
process and whose heartbeat is more than 5 minutes old: what a killed sidecar
left. Receipts written before this release have no holder and end by their TTL
only. A create the provider answers with 429 or 5xx backs off that adapter's
creates (1 minute, doubling to 1 hour); a provider that can look its clones up
by name resolves an uncertain create by the name the receipt gave it, else the
receipt is logged, never guessed.

The same clones rehearse proposed indexes (lab-04a). With `clone.provider` set,
the tuning agent offers the model a `rehearse` tool (once per cycle) and the
deterministic index floor rehearses each candidate it admits, at most
`clone.rehearsals_per_hour` per database. A rehearsal builds the index on the
clone, after the same production check, and measures each target statement
before and after: a statement with no `$n` parameters that the analyze guard
proves read-only is executed with `EXPLAIN (ANALYZE, BUFFERS)` and its shared
blocks (hit plus read) are compared, under
`clone.rehearsal_statement_timeout_seconds` per statement; any other statement
is planned only and says why ("not executed: parameters unknown"). The result
is the proposal's tier-2 ladder record on the approval card, with the clone
receipt id: passed when the executed statements' blocks per call fell, failed
when they did not, inconclusive when nothing was executed, skipped (with the
reason) when no clone could be made, the cap was reached, or no clone provider
is configured. A rehearsal never changes the monitored database, and a failed
or skipped one still lets the proposal queue as before. Every rehearsal ends
within the rehearsal bound (1 hour, or the Lakebase or Neon branch TTL when shorter), and
the floor reuses a candidate's passed, failed or inconclusive record for 24 hours
instead of rehearsing it again each cycle. `pg_stat_statements`
stores statements with their constants replaced by `$n`, so most targets are
planned only; generic plans of parameterized statements need PostgreSQL 16 or
later.

#### Freeze custodian: what holds the xmin horizon

A table is red when its remaining XID runway is under
`custodian.freeze.red_buffer_pct` (default 25%) of its effective freeze maximum
(the lower of `autovacuum_freeze_max_age` and the table's reloption).
Urgency is measured against that freeze maximum; the hard deadline is not. The
hard deadline is when the table's XID (or multixact) age reaches the server's
wraparound warn limit, 2^31 - 1 - 40,000,000, at the measured consumption rate
(capped at 100 years), so a table past `autovacuum_freeze_max_age` still has a
deadline ahead and the xid deadline override and the budget bypass apply to it.
A table at or past the warn limit is red, and its deadline is when its age
reaches the server's stop limit, 2^31 - 1 - 3,000,000 (where PostgreSQL stops
assigning XIDs), so the deadline stays ahead while the scan's earlier proposals
run; at or past the stop limit it is one second ahead. The
deadline is the earlier of the XID and multixact ones; the policy kind is always
`xid` (the `deadline_overrides` key), and the decision evidence names the
counter that set it (`deadline_threat`: `xid` or `multixact`) and the server's
`vacuum_failsafe_age` (PostgreSQL 14+, when the server reports it). Until two
scans have measured the rate, the custodian claims no deadline.
Each scan reads the database's xmin-horizon holders once (at most 20, oldest
first): client sessions by snapshot (`backend_xmin`) or by XID (`backend_xid`,
so an idle-in-transaction writer is found), standbys' `hot_standby_feedback`,
prepared transactions (named by the md5 of their gid), and replication slots
by `xmin`, or by `catalog_xmin` for catalog tables (`user_catalog_table`) only.
A backend running a lazy `VACUUM` is not a holder: it shows a `backend_xmin`,
but that snapshot holds back no horizon.

- A holder pins a red table when its age is at least half the table's XID age.
  A younger holder is ignored and the table is frozen.
- A pinned table is still frozen while the holder is younger than 0.9 of its
  age: `VACUUM (FREEZE)` advances `relfrozenxid` up to the holder's horizon. At
  0.9 or more, freezing cannot lower the urgency and no `VACUUM` runs.
- Each pinning holder gets one `freeze_blocker` proposal per scan, listing every
  table it pins. A client session is canceled (terminated when idle in a
  transaction); like every backend signal, it always waits for an operator's
  approval. Sessions of `pg_dump`, `pg_basebackup`, `pg_restore` and pg_sage are
  never signalled.
- A prepared transaction, a slot, a standby's feedback or a protected session
  becomes a parked escalation plan at risk tier `high` (`diagnose_freeze_blockers`).
  It names the holder. pg_sage never runs `ROLLBACK PREPARED` and never drops a slot.
- A blocker counts as cleared once its PID is gone, its backend was restarted,
  or it holds neither a snapshot nor an XID.

Managed providers may hide slot detail. A holder whose age is unknown pins nothing,
so it is never read as proof that no holder exists. There is no configuration
for this logic.

The standing policy document also carries these safety fields, all enforced:

- `refusal_set`: actions pg_sage never runs on its own. A refused action is
  queued for approval with reason `refused_by_policy` (the token is in the
  detail); an operator approval runs it. Tokens: `rls_change` (row-level
  security or its policies), `grant_expansion` (GRANT, ALTER ROLE, OWNER TO,
  ALTER DEFAULT PRIVILEGES), `major_upgrade` (major or extension upgrades),
  `non_dup_object_drop` (dropping a table, column, constraint, sequence,
  schema or replication slot; index drops are rebuildable and are not
  refused) and `unrollbackable` (rollback class `not_reversible` or
  `forward_fix_only`, unless the action is a retention delete on the column
  the owner declared as the contract's `retention.column`; see [Retention
  contracts](#retention-contracts)). Unknown tokens are rejected.
- `lock_duration_ceiling_ms`: caps `lock_timeout` for DDL that runs inside a
  transaction at the smaller of the ceiling and `safety.lock_timeout_ms`, reverts
  included.
  `CONCURRENTLY` builds keep `safety.lock_timeout_ms`, because they wait on
  older transactions by design. `0` means no ceiling.
- `window_start_margin_minutes` (optional, `0`-`720`, default `0`): a
  `moderate` or `high` action, operator-approved or self-initiated, is not
  started when its window closes in less than this many minutes; it blocks
  with `window_closing` and waits for the next window, so an index build
  authorized at 04:59 does not start in a window that closes at 05:00. The
  window is `trust.maintenance_window` (when set) intersected with the
  policy's `maintenance_windows`. A document without the field keeps its
  meaning. Lowering or removing the margin widens the policy.
- `exposure.mode` (optional, `shadow` or `enforce`, default `shadow`): whether
  the exposure findings AP-03 and AP-15 become typed remediations
  (`enable_row_level_security`, `revoke_public_grant`). `shadow` records the
  would-be action on the finding only. A document without the key is `shadow`
  and keeps its bytes. Setting `enforce` widens the policy. See [Exposure
  remediation](policy-gate.md#exposure-remediation).
- `serialize_mode`: what happens when another writer holds the DDL lease on
  the same object. `park` (both profiles): a self-initiated action is parked
  with reason `ddl_conflict` and retried next cycle. A park is not a failure:
  it does not count toward the retry limit or the self-initiated rate limit.
  `queue`: the action waits its turn in a durable FIFO queue per object
  (`sage.lease_queue`), so leases are granted in request order, and a waiter
  that restarts resumes its place. The wait is bounded: a self-initiated
  action waits at most 2 minutes, then parks with `ddl_conflict` and
  `evidence.lease_queue = timeout`; an operator action waits at most
  30 seconds, then is refused. At most 8 actions wait per object and 64 per
  database; a request beyond that is not queued (a self-initiated one parks
  with `evidence.lease_queue = full`, an operator one is refused), and a
  request identical to one already waiting is not queued twice
  (`evidence.lease_queue = duplicate`). In both
  modes an operator action that cannot get the lease is refused (recorded as
  `blocked` `ddl_conflict`), not parked. See [Policy gate](policy-gate.md)
  for every verdict and reason.

#### Blast-radius budgets

Self-initiated changes are bounded per rolling 24-hour window by
`blast_radius`. Housekeeping and performance changes have separate budgets,
so a burst of index cleanup can never hold back an evidence-backed index or
setting change:

```json
"blast_radius": {
  "max_rows_rewritten": 5000000,
  "max_tables_per_window": 10,
  "hygiene": {"max_tables_per_window": 10, "max_changes_per_window": 25}
},
"rate_limits": {"max_self_initiated_changes_per_window": 25}
```

The performance budget is `blast_radius.max_tables_per_window` plus
`rate_limits.max_self_initiated_changes_per_window` (a document may also
spell it `blast_radius.performance: {max_tables_per_window,
max_changes_per_window}`); `hygiene` is optional and defaults per field.

- **Kinds.** `hygiene` is unused and redundant index drops (including drops in
  leaked test schemas), `VACUUM` and `ANALYZE`. Everything else is
  `performance`: index builds, reindexes, settings, autovacuum tuning, query
  hints and any action type pg_sage does not recognize. The kind comes from the
  action's typed contract (the statement that would run), never from evidence
  or an LLM.
- **`max_tables_per_window`**: distinct tables a kind may touch in the window,
  counting the change being decided (an index counts as its table).
  Re-touching a table already counted is free. `0` admits no table.
- **`max_changes_per_window`**: self-initiated changes a kind may make in the
  window. Operator-approved changes and `ddl_conflict` parks do not count.
- **`max_rows_rewritten`**: one budget for both kinds. Each change is charged
  pg_sage's estimate of the rows it rewrites, taken from `pg_class.reltuples`
  (or the live-tuple count, whichever is larger, summed over every leaf
  partition) when it is authorized: `VACUUM FULL`, `CLUSTER`, `REINDEX`
  without `CONCURRENTLY`, and `ALTER TABLE` that changes a column type, sets
  `LOGGED`/`UNLOGGED`, a tablespace or an access method, or adds a generated,
  identity or non-literal-default column (a function default such as `now()`
  is assumed to rewrite). Every other change rewrites `0` rows. A rewrite
  whose table cannot be found is blocked (`policy_unavailable`), never
  counted as `0`.
- **Defaults.** Both profiles split the previous single limit (20 tables, 50
  changes) evenly: 10 tables and 25 changes per kind.
- **Documents written before the split** keep working unchanged:
  `blast_radius.max_tables_per_window` and
  `rate_limits.max_self_initiated_changes_per_window` are read as the
  performance budget, and hygiene gets the defaults above (so such a policy
  allows up to 10 hygiene tables on top of its old limit). A document may name
  a performance limit both ways (also as a `performance` block) only if the
  values agree. Saving a policy writes the performance budget in those
  legacy fields, and a `hygiene` block only when it differs from the
  default, so a sidecar from before the split still reads a saved policy
  (it fails closed, `policy_unavailable`, on a customized hygiene block).
- **When a budget is full** the change is parked (`blast_radius_exceeded` for
  tables and rows, `rate_limit_exceeded` for changes) and retried next cycle.
  The decision's `evidence.budget_detail` says which budget is full and when it
  next frees, for example `hygiene budget full: 11 of 10 tables in the 24h
  window; next frees at 2026-10-04T18:00:02Z`. Actions > Decisions and
  `GET /api/v1/decisions?verdict=parked` show it per candidate
  ([Reading decisions](policy-gate.md#reading-decisions)).
- **Concurrent candidates.** An authorized change holds its slot from the
  authorization until it has run (at most twice the DDL timeout plus a
  minute, if the sidecar dies in between), so two candidates can never both
  take the last slot, even across sidecar processes: the usage read and the
  recorded decision share one transaction holding a
  `pg_advisory_xact_lock` keyed by the database.
- **Emergency mitigations bypass the kind budgets.** A wraparound `VACUUM`
  while the XID runway is critical, a space-freeing `VACUUM`, unused-index
  drop or `REINDEX` while the disk runway is critical (urgency critical with
  the hard deadline still ahead; for XID it is the wraparound warn limit, see
  the freeze custodian above), and the
  revert or rollback of a change pg_sage made itself are never parked by a
  kind budget and are charged to none (`evidence.budget_kind = bypass`).
  They are recorded with the reason `budget bypass: <why>` and still pass
  every other check: change classes, guardrails, the refusal set, windows,
  leases, lock ceilings and the shared `max_rows_rewritten` bound.

#### One change per object

pg_sage changes one object at a time and waits for the verdict before the
next change to it. An object is a setting (`work_mem` is one object whether it
is changed with `ALTER SYSTEM`, `ALTER DATABASE` or `ALTER ROLE`) or a table
with its indexes (an index, its reloptions, `VACUUM`, `ANALYZE` and extended
statistics on a table are all changes to that table). For index, extended
statistics and reloption changes, a partitioned table, its partitions and
their indexes are one object (the partition tree, from `pg_inherits`), so a
change to one waits for a change to another; `VACUUM` and `ANALYZE` of a
partition stay that partition's own. The identity comes from the statement
and the catalog, never from evidence or an LLM.

- **What waits.** A self-initiated change whose object has another change in
  flight is parked with reason `awaiting_verification`. In flight means an
  executed action still being watched (`sage.action_log.outcome` monitoring,
  pending, interrupted or rolling back), one whose verdict in
  `sage.action_outcome` is still pending, or a self-initiated change the gate
  has authorized that has not run yet (so two proposals for one table in the
  same cycle never both run; an operator's change runs at once and holds its
  object through its action). The decision's detail and `evidence.verification_wait` name the
  action and until when, for example `awaiting verification of action 6407
  (until 2026-10-04T18:10:00Z) on guc:work_mem`. A change that would otherwise
  need approval is still queued; its approval card shows the wait.
- **When it resumes.** The parked change is evaluated again every cycle and
  runs once the verification concludes (improved, neutral, regressed,
  insufficient evidence, unverifiable, or rolled back).
- **Never forever.** A verification that passes its hard deadline without a
  verdict releases the wait, and the release is recorded on the decision
  (`evidence.verification_wait_released`, with its `release_reason`). The
  hard deadline is the verification cap (`verify.window_max_minutes`) plus
  one hour after the action ran. An index drop holds its table only until its
  first window concludes (`trust.rollback_window_minutes` after it ran),
  released as "drop's first window concluded" and shown on approval cards;
  its soft-drop monitoring keeps watching the whole business cycle
  (`verify.drop_window_hours`) and re-creates the index on a regression. An
  authorized change that has not run holds its object for at most twice the
  DDL timeout plus a minute.
- **Never waits.** Rollbacks and reverts of pg_sage's own changes (including the
  rollback of the change being verified), emergency mitigations (the same
  cases that bypass the budgets above), owner-declared retention deletes and
  read-only diagnostics.
- **Operator override.** Approving a queued change runs it even while the
  object waits; the approval card says so first ("approving overrides pending
  verification of action N") and the decision records
  `evidence.verification_override`.
- **Metrics.** `pg_sage_policy_parks_total{database,reason}` counts the parks
  the gate records in `sage.decision`, by reason (one per evaluation, so
  repeats folded into one ledger row each count). A park that fails to record
  is counted in `pg_sage_policy_record_failures_total` instead (below), and a
  `ddl_conflict` lease park, recorded after the gate, is not counted. And
  `pg_sage_verification_wait_releases_total{database,cause}` the waits that
  ended without a verdict (`operator_override`, `hard_deadline`,
  `drop_first_window`).
- **Contract-less candidates record as high.** A candidate with no typed
  contract (pg_sage cannot classify or validate its SQL) parks with
  `no_typed_contract` and records at risk tier `high`, with
  `evidence.contract = none`; so does any decision whose contract names an
  unknown tier. A statement that maps to no change class (`TRUNCATE`, for
  one) records under the feature `unclassified`. No verdict changes: such a
  candidate never runs unattended.
- **Unscoped VACUUM and REINDEX have no contract.** `vacuum_table` is a VACUUM
  of exactly one named relation (an optional column list allowed); a bare
  `VACUUM`, `VACUUM (FREEZE)` with no table and a comma list
  (`VACUUM a, b`) classify as nothing and park as `no_typed_contract`, as
  does `VACUUM FULL`. `reindex_concurrently` is `REINDEX INDEX|TABLE
  CONCURRENTLY`; `REINDEX SCHEMA|DATABASE|SYSTEM CONCURRENTLY` parks as
  `no_typed_contract`. A statement with no contract also has no top-level
  route: were it ever to run, it would fail inside a transaction block.
- **Execution route by typed action.** Only `create_index_concurrently`,
  `reindex_concurrently`, `drop_unused_index`, `revert_created_index`,
  `vacuum_table` and `alter_system_guc` run outside a transaction and keep
  `safety.lock_timeout_ms`; every other statement runs in a transaction under
  the tighter of the policy's `lock_duration_ceiling_ms` and
  `safety.lock_timeout_ms`. An automatic rollback routes the same way, but
  always under `safety.lock_timeout_ms` (it carries no policy ceiling). A
  keyword in a table name (`concurrently_log`) no longer changes the route.
- **`target_mismatch`.** A self-initiated finding whose object type names a
  relation (`table`, `index`, `view`, `materialized_view`,
  `partitioned_table`), and a custodian proposal, must change the object it
  names: its statement target is compared with the finding's identifier
  (an optimizer finding's table; a trailing column list is ignored) with
  `public` as the default schema and unquoted names folded to lower case (a
  catalog-form name such as `public.my table` matches its quoted spelling). A
  difference is `blocked` with the reason `target_mismatch` before any lease
  is taken; the detail names both objects, and an approval does not lift it.
  A finding with no identifier leases its statement's target. Rollbacks of
  pg_sage's own changes, findings that name a setting, the instance or a
  query, and index replacements are not compared.
- **Record failures.** A decision the gate cannot record in `sage.decision`
  is answered `blocked policy_unavailable` (fail closed); its detail names the
  verdict and reason it meant to record (for example
  `could not record the parked/no_typed_contract decision: ...`).
  `pg_sage_policy_record_failures_total{database,reason}` counts every such
  failure by the decision's reason, and the sidecar logs each failing
  decision at most once an hour. Any series above zero means the ledger is
  missing decisions.
- **Episodes.** A withheld verdict (parked, queued, blocked, observe-only)
  that repeats every cycle is one `sage.decision` row per episode: repeats
  count in `repeat_count` and stamp `last_seen_at`. An episode ends, and
  `resolved_at` is set, when its decision is linked to the approval-queue item
  it queued (`queue_id`), or when the same candidate executes. The candidate is
  `candidate_key`: database, feature, targets, SQL and principal, without the
  verdict, intent or policy version, so an operator's approved execute also
  ends an autonomous park. The next withheld verdict starts a new row, so a
  change re-queued after a rejection has its own decision and its approval
  card keeps the gate's reason and tier. A park that stays parked stays one
  episode. Rows written before the upgrade gain `candidate_key` at their next
  repeat. Per database; PostgreSQL 14 to 18 behave the same.

#### Retention contracts

The only way pg_sage deletes user rows is a retention contract declared with the
MCP tool `declare_table_contract`. The owner names both the window and the column
whose age defines it; pg_sage never infers the column:

```json
{"table": "public.events", "append_only": true,
 "retention": {"interval": "90 days", "column": "ingested_at"}}
```

- `retention.column` is required whenever `retention.interval` is set. It must
  exist, not be dropped, and be `timestamptz`, `timestamp` or `date`. On a
  partitioned table a column that is not the partition key is accepted with a
  warning: each retention batch then scans every partition.
- A contract without a usable column parks (deletes nothing). The ledger reason
  shows pg_sage's suggested column, for example `retention column not declared;
  suggested: created_at — re-declare the contract with retention.column to
  enable deletes`.
- The first cycle is always a dry run. Deletion starts only after a dry run at
  least 24 hours old (and at most 7 days) for the same table (by OID), column
  (by attnum and type), contract version and window. Renaming or swapping the
  column, rebuilding the table, re-declaring the contract, or the eligible row
  count growing past 2x + 100 of the reviewed dry run starts a new dry run.
- Each batch deletes at most 1,000 rows under a `ROW EXCLUSIVE` table lock, after
  re-checking the column identity in the same transaction, and requires standing
  policy consent to the `retention` change class.

#### Structural schema invariants are advisory only

The schema guard also detects structural pathologies: `everything_text` (a table
of three or more columns, all text) and `type_tightening` (a text column named
like a number: ending in `_id`, `_count` or `_number`, or named `count_text`).
Fixing either needs an online column migration, which pg_sage cannot plan or
run, so these invariants are never routed,
never auto-applied and never counted as routed:

- The ledger records each one once as a `recommendation` (verdict `observe_only`,
  risk tier `moderate`) with the reason `requires an online column migration;
  pg_sage has no automated path`. No config or policy changes this.
- The decision proposes no SQL. For a type tightening, the evidence shows the
  in-place statement as `rewrite_statement` with a `rewrite_note`: it rewrites the
  whole table under `ACCESS EXCLUSIVE` and is shown for reference, not to run.
- Table contract exemptions and the oscillation limit still park these
  invariants, as they do every other.

### Vacuum, bloat and freeze autopilot

Four analyzer rules and one custodian produce vacuum, bloat and freeze work. Each judges a
table by its effective settings. A freeze threshold is relative to the table's effective
freeze maximum, the lower of the server setting and the table's own reloption
(`LEAST(autovacuum_freeze_max_age, reloption)`, and likewise for multixacts), so a table
tuned to freeze early is judged early. The catalog entries are in
[Findings](findings.md#maintenance).

| Producer | Fires at | Proposes | Contract tier |
|---|---|---|---|
| Analyzer `table_bloat` | dead tuples above `analyzer.table_bloat_dead_tuple_pct` (`20`) percent, on a table with at least `analyzer.table_bloat_min_rows` (`1000`) rows | `VACUUM`; no SQL while IO is saturated | `safe` |
| Analyzer `autovacuum_tuning` | at least `analyzer.autovacuum_tune_min_rows` (`1000000`) live rows, and lifetime updates plus deletes above them | `ALTER TABLE ... SET (autovacuum_vacuum_scale_factor = ...)` | `moderate` |
| Analyzer `xid_wraparound` | the database's `age(datfrozenxid)` at `analyzer.xid_wraparound_warning` (`500000000`), critical at `analyzer.xid_wraparound_critical` (`1000000000`) | a read-only holder query in the detail, no SQL | none |
| Analyzer `wraparound_freeze` | `analyzer.wraparound_freeze_share` (`0.75`) of the effective freeze maximum | nothing: diagnosis only, naming the custodian's decision | none |
| Freeze custodian | red: remaining runway at most `custodian.freeze.red_buffer_pct` (`25`) percent of the effective freeze maximum | the ladder below | per step |

`analyzer.wraparound_freeze_xid_age` is retired: it loads with one warning and is ignored.

| Key | Default | Meaning |
|---|---|---|
| `custodian.freeze.red_buffer_pct` | `25` | A table is red when its remaining XID (or multixact) runway, its effective freeze maximum minus its age, is at most this percent of that maximum. Amber is twice this, at most 100 percent. Greater than 0 and less than 100: `0`, `100` and above, negative values and `.nan` are refused when the file loads, on a hot reload and by the API, and a stored API override outside the interval is skipped with a warning. Environment: `SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT`. Restart to change |

**The freeze ladder.** The freeze custodian is the single producer of freeze actions: the
analyzer only diagnoses. It answers each table by its urgency, the worse of the XID and the
multixact one:

1. **Blocker response.** A red table pinned by an xmin holder gets the holder answered first,
   with one `freeze_blocker` proposal per holder and scan (see
   [what holds the xmin horizon](#freeze-custodian-what-holds-the-xmin-horizon)). A client
   session is canceled, or terminated when idle in a transaction. Like every backend signal,
   it always waits for an operator's approval. A prepared transaction, a slot, a standby's
   feedback or a protected session becomes a parked `diagnose_freeze_blockers` plan at risk
   tier `high`.
2. **Freeze.** A red user table gets `VACUUM (FREEZE)`, or a plain `VACUUM` once its XID age
   reaches `vacuum_freeze_table_age` or its multixact age reaches
   `vacuum_multixact_freeze_table_age` (each capped at 95% of the server's
   `autovacuum_freeze_max_age` or `autovacuum_multixact_freeze_max_age`, as a manual `VACUUM`
   caps it), since that `VACUUM` is aggressive already. The freeze is
   skipped while the pinning holder is at least 0.9 of the table's age, since it could not
   lower the urgency. The freeze is a `safe` `vacuum_table` action in change class `freeze`
   and incident family `wraparound_runway`. The policy gate decides whether it runs on its
   own, through the family's [earned autonomy](#sage-sre-earned-autonomy) or a critical XID
   deadline the standing policy lets override, or waits. It runs on a dedicated connection
   under the `VACUUM` watchdog (`safety.vacuum_timeout_seconds`, `14400`;
   `safety.vacuum_stall_minutes`, `15`). Its post-check fails the action when the table's
   horizon is older afterwards.
3. **Catalog freeze plan.** Red system catalogs get one `plan_catalog_freeze` plan per scan,
   listing every red catalog, at risk tier `high`. It is parked and never run: a catalog
   `VACUUM` needs `MAINTAIN` or ownership.
4. **Repack plans.** A table that is not red, with a heap of at least
   `analyzer.table_bloat_min_bytes` and a bloat estimate of at least 50%, gets a `pg_repack`
   plan (when the extension is installed) or a parked `bloat_remediation` decision, at risk
   tier `high`. pg_sage never runs either.

**Emergency `VACUUM` profile.** When the watchdog has cancelled a red table's freeze at
`safety.vacuum_timeout_seconds` (`vacuum_cap`) twice in the last 6 hours, with no completed
custodian `VACUUM` of the table since, the next freeze runs the emergency profile:
`VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF)`, or `VACUUM (INDEX_CLEANUP OFF, TRUNCATE
OFF)` when a plain `VACUUM` is aggressive already. Index vacuuming is usually most of a large
table's `VACUUM`, and skipping the truncation avoids its end-of-run `ACCESS EXCLUSIVE` lock. Its
session also runs with `vacuum_cost_delay = 0`, which a role or database setting may have
raised; the replaced value is recorded as `vacuum_cost_delay_before` in the action's
`before_state`, and the setting is reset before the connection returns to the pool. Only
`vacuum_cap` failures count, not `vacuum_stalled`, a lock timeout or any other error, and an
amber or green table never gets the profile. The trade-off: dead index entries stay, so index
bloat grows until the next normal `VACUUM`. That is the custodian's next freeze proposal once
the table leaves red, or autovacuum's next run. PostgreSQL's own failsafe
(`vacuum_failsafe_age`, PostgreSQL 14+) does the same only near 1.6 billion XIDs. The profile is
the same `vacuum_table` action under the same policy gate and trust level, keeps
`lock_timeout`, the watchdog and the emergency stop, and is never `VACUUM FULL`. The proposal's
evidence carries `profile: emergency` and `cap_failures`. The emergency statement has its own
failure backoff, so a failing emergency streak escalates once more, with `profile: emergency`
in the `custodian_escalation` event and its decision. If the cap history cannot be read, the
normal freeze runs and one `WARN` names the table. No configuration and no extra privilege
are needed (both options exist on every supported PostgreSQL major).

An amber table gets no custodian action, and the custodian has no autovacuum tuning step:
the analyzer's `autovacuum_tuning` is the only tuner.

**Cadence and timeouts.** Each database's autonomy worker runs the freeze custodian every
`analyzer.interval_seconds` (default `600`, so every 10 minutes), and never more often than
once a minute: the cadence is the larger of `analyzer.interval_seconds` and 60 seconds. One
sample per database (the 500 most advanced relations, at most 20 xmin holders, and the XID
and multixact consumption rates) is read in one read-only transaction and shared with the
runway advisor and the analyzer for half that interval. The sample's transaction is bounded
by `safety.query_timeout_ms` and `safety.lock_timeout_ms` (`0` uses the default), so a catalog
lock held by DDL ends the sample at the shorter of the two instead of holding the custodian,
the runway advisor and the analyzer. The error names the bound: with the shipped values
(500 ms and 30 s) it is `freeze age read: statement timeout`, and `freeze age read: lock
timeout` when the lock timeout is the shorter. That cycle proposes nothing, and the next one
reads again. The `pgstattuple_approx` confirmations of the three largest bloat
candidates per scan are bounded by the same two settings.
Until two scans have measured the consumption rate, the custodian claims no deadline.

**Backoff and escalation.** A custodian statement whose runs failed (outcome `failed` or
`rollback_failed`, the post-check included) in the last 6 hours backs off: after `n`
failures it waits 2^n minutes from the newest one. At 3 failures it stops until they age out
of the 6-hour window. For a red proposal, pg_sage then records one `blocked`
`custodian_backoff` decision and sends one critical `custodian_escalation` notification per
failure streak. The backoff state is the durable `sage.action_log`, so it survives restarts.

**Reading the custodian's decisions.** Every custodian proposal passes the
[policy gate](policy-gate.md) and leaves its verdict in the decision ledger
(`sage.decision`). See [the decision ledger](policy-gate.md#the-decision-ledger) for its
columns. Filter on `feature`: `freeze` for freezes, session signals and catalog plans,
`freeze_blocker` for the escalation plans of holders pg_sage never signals, and
`bloat_remediation` for repack plans. The `evidence` holds the incident family
(`incident_family`), the counter that set the deadline (`deadline_threat`),
`vacuum_failsafe_age` when the server reports it, the pinning holder (`xmin_holder`) and,
on a plan, its contract (`action_type`). `deadline_kind` and `deadline_hard_at` hold the
XID deadline. A withheld plan repeats into one row (`repeat_count`). The analyzer's
`wraparound_freeze` finding names the same decision in `detail.custodian_decision`.

**Fleet mode.** Each database has one freeze custodian and one sampler, also in fleet mode.
Client sessions and prepared transactions are read per database. Replication slots and
standbys' `hot_standby_feedback` hold the cluster-wide xmin horizon, so the custodian of
every database in the cluster sees them, and each one parks its own escalation plan for
them.

### Load admission for autonomous index builds

Autonomous `CREATE INDEX CONCURRENTLY` and custodian index proposals (for
example FK supporting indexes) start only when load evidence says the host is
quiet. pg_sage measures IO itself from Postgres, every minute, with no cloud
credentials:

- Data IO: `pg_stat_io` reads, writes and extends of relation data (PG16+;
  PG18 byte columns). On PG14/15, `pg_stat_database.blks_read` plus the
  buffers written by the checkpointer, bgwriter and backends
  (`pg_stat_bgwriter`), times `block_size`. `blks_read` also counts reads served
  by the OS page cache, so on PG14/15 the rate over-states device reads; the
  learned baseline compares the database with itself, which keeps the bias
  consistent.
- WAL: `pg_stat_wal.wal_bytes` (PG14+). PostgreSQL 13 and older have no IO
  evidence, so admission stays withheld.

A statistics reset, or any counter that goes backwards, discards that interval.
It is never read as a quiet period.

Admission uses one of two evidence modes:

| Mode | When | Admits when |
|---|---|---|
| `declared_capacity` | `verify.io_capacity` (standalone) or `databases[].verify.io_capacity` (fleet) is set | data and WAL throughput are at or below `safety.data_io_ceiling_pct` / `safety.wal_io_ceiling_pct` of the declared MiB/s |
| `learned_baseline` | no declared capacity and `verify.io_baseline_days` > 0 | after that many days of observation, current data and WAL rates are at or below the learned median (p50) |

Host CPU is used when a provider reader supplies it (Supabase with
`SAGE_SUPABASE_OBSERVABILITY_TOKEN`) and must be at or below
`safety.host_cpu_ceiling_pct`. While that key is unset it inherits
`safety.cpu_ceiling_pct`, and the sidecar warns once at startup when that value is not the
default 90, so no verify guardrail loosens on upgrade. `safety.cpu_ceiling_pct` itself is
the collector's ceiling on active client sessions / `max_connections`, not host CPU.
When CPU is unavailable, admission is granted only
while the maintenance window is open (`trust.maintenance_window` and the
standing policy's windows, evaluated as the policy gate does).

| Parameter | Default | Description |
|---|---|---|
| `verify.io_baseline_days` | `7` | Days of observation before the learned baseline can admit. `0` disables the learned baseline |
| `verify.io_baseline_hours` | `0` | Hours of observation instead, `1`-`8760`. When set it takes precedence over `io_baseline_days` (even `0`). `0` uses `io_baseline_days`. See [Fast elevation](#fast-elevation-dogfood-databases) |
| `verify.io_sample_retention_days` | `14` | Days of rate samples kept in `sage.io_rate_sample`; the rolling baseline covers this window. Must be at least `io_baseline_days` |
| `verify.io_capacity.read_write_mbps` | (none) | Standalone only. Declared data read+write throughput in MiB/s |
| `verify.io_capacity.wal_mbps` | (none) | Standalone only. Declared WAL throughput in MiB/s |
| `databases[].verify.io_capacity` | (none) | Fleet: the same attestation per database. A fleet-wide `verify.io_capacity` is rejected |
| `safety.cpu_ceiling_pct` | `90` | Collector circuit breaker: skip a cycle while active client sessions / `max_connections` exceeds this percent (1-100). After `backoff_consecutive_skips` skips the collector goes dormant |
| `safety.host_cpu_ceiling_pct` | unset (inherits `cpu_ceiling_pct`) | Host CPU ceiling for index verification admission, percent (1-100). Restart to apply |
| `safety.data_io_ceiling_pct` | `70` | Data IO ceiling, percent of declared capacity. Independent of `cpu_ceiling_pct` |
| `safety.wal_io_ceiling_pct` | `70` | WAL IO ceiling, percent of declared capacity. Independent of `cpu_ceiling_pct` |

Declared capacity is an operator attestation and overrides the learned
baseline. Declare only what the volume really provides (for example the
provisioned throughput of the EBS/PD/Azure disk). Meta-mode databases use the
learned baseline only.

Each decision records the evidence mode, rates, capacity or baseline in
`sage.decision.evidence.load_admission`. A withheld build is recorded once per
finding and reason in `sage.admission_withheld` instead of a failed action on
every cycle. `GET /api/v1/admission` (optionally `?database=`) and
`GET /api/v1/admission/{name}` report each database's mode, reason and
baseline progress; the dashboard shows them on the Actions page and in the
Overview provider-readiness tab.

### Sage SRE investigations

By default (`sre.automatic_start: true`), pg_sage investigates each open RCA incident of the
lock, connection or WAL family and each open `plan_regression` finding, once. Investigations
are read-only and bounded (at most 12 catalog probes and 120 s of active time each), so they
start by themselves; set `sre.automatic_start: false` to start them only from a case or the
API. With the LLM off, investigations still run and conclude deterministically. An investigation runs
the fixed read-only catalog probes of its family (connection and WAL investigations sample
twice, `sre.sample_interval_seconds` apart), matches them against the deterministic causal
graph and stores the result: the likely explanation, contributing factors, alternatives and
ruled-out explanations, each citing its evidence, the evidence that could not be collected,
and an operator step. Nothing is executed. "Inconclusive" is a normal outcome.

**Diagnosis follow-up.** When an investigation of an incident ends `concluded` or
`inconclusive`, the engineer who was paged gets the diagnosis on the same channels: a
notification of event type `investigation_concluded` reading "Diagnosis for incident
`<id>` on `<database>`: `<family>` - `<root cause or 'inconclusive'>`: `<reason>`. Case
`<case id>`." It goes only to the channels whose `incident_detected` or `incident_escalated`
page for that incident was sent (`sage.notification_log`), with the incident's severity. On
Telegram it replies to the page; Slack and email get one message titled "Re: `<page
subject>`"; PagerDuty gets none (Events v2 has no note). There is no rule or setting for it:
`investigation_concluded` cannot be chosen in a notification rule, and an incident whose page
was never sent gets no follow-up. Each incident gets at most one follow-up, from any number of
sidecars: the newest conclusion is sent once it is 60 s old and no run of the incident is
queued or running, within an hour of concluding. A later re-run of the incident does not
notify again, and a failed delivery is logged (`status` `error` in the notification log) and
not retried. Failed, expired and cancelled runs and investigations without an incident
(operator-started) send nothing.

Four more families are investigated the same way: **checkpoint storms**, **temp-file
explosions**, **replication lag** and **LWLock contention**. Their triggers are the RCA
incidents for "checkpoints are occurring too frequently", temp files, replication conflicts and
replication lag (`replication_lag_increasing`), plus a deterministic detector that samples the
database once per trigger poll (only while `sre.automatic_start` is on). The detector opens one
episode at a time per family, with conservative default thresholds (`sre.detectors.*`, below):
3 or more requested checkpoints within 5 minutes that outnumber timed ones; 1 GiB of temp
files in this database within 5 minutes; or 8 or more backends waiting on one modeled LWLock
in 3 consecutive polls. A new episode of a family waits 30 minutes after the previous one
started.

Each episode is an incident, like an RCA incident: a `warning` row in `sage.incidents`
(signal `sre_checkpoint_storm`, `sre_temp_file_explosion` or `sre_lwlock_contention`) with the
measurement and its threshold as evidence. It appears in the Cases panel, sends the usual
incident notifications (detected, escalated, resolved) and links its investigation, which an
operator can review for earned autonomy. One investigation per incident: a new episode while
its incident is still open counts as another occurrence (the RCA engine escalates an incident
after `rca.escalation_cycles` occurrences), and the incident resolves itself
`rca.resolution_cycles` analyzer cycles after the episodes stop. An open RCA incident of the
same family that already has at least warning severity (for example "checkpoints are occurring
too frequently" from the logs) is used instead of a second incident. An incident an operator
resolves is not reopened by the same episode. With `rca.enabled: false` there is no incident:
the investigation keeps its own case (`sre:detector:<family>:<database>`). A checkpoint investigation
compares samples 6 sample intervals apart (30 s by default). The live-temp-file probe needs
`pg_monitor` (or superuser), and per-statement spills need `pg_stat_statements`. Without them,
the investigation reports the evidence as unavailable instead of guessing. Standby-side
evidence (paused replay, standby queries holding replay back) is only visible when pg_sage
monitors the standby itself.

**Tool-calling investigator (the default model mode, `sre.llm.mode: investigator`).**
Instead of the single review turn below, the model plans its own reads after the causal graph
has scored the hypotheses. Every tool only reads:

- catalog probes with typed arguments;
- three pg_stat views (`database`, `tables`, `statements`), registered as catalog probes;
- a plan-only `EXPLAIN` of a statement by its `pg_stat_statements` queryid (the broad plan, and the narrow plan of a plan-regression investigation).
  It is never `ANALYZE` and never runs the statement. It plans the generic plan in a
  read-only transaction and returns node shapes only, never query text. The text comes
  from the collector's latest snapshot, never the query-text file (a statement outside its
  top queries is `unsupported` / `text_not_cached`), and the `EXPLAIN` holds the database's
  probe slot like any probe;
- the graph's current state and operator-confirmed facts.

No tool can change anything. Remediation stays a typed proposal through the policy gate.

The loop is bounded by its plan:

| Plan | Used for | Model steps | Probes | Wall clock | Tokens |
|---|---|---|---|---|---|
| broad | operator-started investigations (`POST /api/v1/databases/{db}/investigations`) and SLO burn | 10 | 6 | 90 s | 64k |
| narrow | detector and incident triggers (plan regressions also get the plan-only `EXPLAIN`) | 5 | 3 | 45 s | 32k |

Probes count against the investigation's 12-probe ceiling. Each model call is reserved in the
same durable budget ledger as the review turn, before it is sent. Every tool call is stored as
evidence with its SHA-256 digest. Repeated calls, unknown tools, calls over the budget and
malformed replies are refused, counted and shown.

The model finishes with `submit_conclusion`, whose outcome is one of:

- `agree`: it agrees with the graph's conclusive root;
- `conclude`: it names a graph node on an inconclusive graph;
- `contest`: it names another graph node against a conclusive root;
- `unmodeled`: it names a cause the graph has no node for;
- `inconclusive`.

Each claim must cite evidence ids, and every number in a claim must equal a typed value of
the cited evidence: an integer exactly and with its sign (a 64-bit queryid off by one, or
with its sign flipped, is ungrounded), a decimal at the precision the evidence states, and a
timestamp as the whole RFC 3339 instant, never its year, hour or second. For the model's own
reads these are the row values it was shown; a probe's version, its row count and its `[i]`
row ordinals ground nothing. A minus sign counts only at the start of a value or after a
space, `(`, `=`, `:`, `[` or `,`, so `10-20` is a range. Uncited or ungrounded claims are dropped and counted. A conclusion, contest or
unmodeled cause with no surviving claim counts as inconclusive. With
`sre.investigator.require_support_citation` on (the default), a contest or conclusion also
needs a surviving claim that cites evidence the causal graph itself lists as support for the
model's root; a claim about something else (connection counts under a lock contest, say)
does not carry it, and the outcome is inconclusive with the reason `unsupported_root`.
Evidence from the detection-time incident snapshot never counts. Agreement and an unmodeled
cause (which has no graph node) are not affected. Set it to `false` to keep the earlier rule,
where any surviving claim carries the root. The model's root stays
advisory unless its family has earned model-root authority from held-out bench results, and
the model's self-reported confidence is ignored. An unmodeled cause is always advisory. The
model's reads can conclude an inconclusive graph, but never move or drop a conclusive root;
only an adopted contest can. When the loop stops without a usable answer (step, token or
wall-clock budget, rate limit, timeout, malformed replies), the graph's diagnosis stands and a
`model_rejected` event gives the reason. Providers without native tool calls get a JSON action
protocol automatically. The transcript (the plan, each tool call with its result and digest,
the citations, and the stop reason) is shown in the investigation view. It is also served,
redacted, at `GET /api/v1/databases/{db}/investigations/{id}/transcript` and by the MCP tool
`sre_get_transcript`. Unredacted identifiers (`?keep_identifiers=true`) need the operator role.

**Review turn (`sre.llm.mode: review`).** After the causal graph has
scored the hypotheses, the configured LLM (`llm.*`) reviews the result. It may only:

- reorder the graph's own open hypotheses (shown separately as "model ranking", never merged
  into the graph's scores);
- while the graph is inconclusive, ask for one more catalog probe with typed arguments and a
  one-line reason. The probe counts against the 12-probe ceiling, and the graph then
  diagnoses again;
- write up to 5 claims, each citing the evidence it rests on (shown as "model-generated
  narrative").

Every reply is checked: known node ids only, catalog probes with valid arguments, evidence ids
of this investigation, every number equal to a value of the cited evidence, and a 16 KiB size limit. A
reply that fails gets one repair attempt. If the repair fails too, or the model times out, is
rate limited or is over budget, the investigation ends with the deterministic result and a
`model_rejected` event that gives the reason. Before concluding, a verifier rechecks the ranking
and every claim against the stored evidence. When the graph has a conclusive root cause, it
always wins: if the model ranks another hypothesis first, nothing the model said is kept and a
`model_disagreed` event records the disagreement. Each investigation is limited to 2 model
turns, 16k input and 4k output tokens, and its 120 s active time. The daily allocation is
`llm.token_budget_daily`.

With no LLM configured, investigations run deterministically, and the sidecar logs once that
the model turn is unavailable and why. To turn the model turn off while an LLM stays
configured for other features, set `sre.llm.enabled: false`. Reasoning models (Gemini
2.5+/3, OpenAI o-series, DeepSeek R1) work. Their thinking gets its own allowance of 16k tokens
per investigation (8k per turn), separate from the 4k answer limit. A turn requests 2k answer
plus 8k reasoning tokens, and the daily allocation counts both. If a provider reports more
reasoning than allowed, the usage is recorded as reported and no further turn is started.

| Parameter | Default | Description |
|---|---|---|
| `sre.automatic_start` | `true` | Start a read-only investigation for each incident, plan regression and reactive detector episode (checkpoint storm, temp-file explosion, LWLock contention). `false`: start them only on request; investigations already stored are still resumed and retained. Restart to change |
| `sre.trigger_interval_seconds` | `15` | Seconds between checks for new triggers and pending investigations, `5`-`600` |
| `sre.sample_interval_seconds` | `5` | Seconds between the two samples connection and WAL investigations compare, `1`-`30` |
| `sre.evidence_retention_days` | `30` | Days a finished, unpinned investigation keeps its probe evidence. The delete leaves a tombstone, and the investigation is shown as "evidence deleted by retention" |
| `sre.timeline_retention_days` | `90` | Days a finished, unpinned investigation is kept at all (hypotheses, steps, event chain), leaving a tombstone. At least `sre.evidence_retention_days`, at most `3650` |
| `sre.llm.enabled` | `true` | Model turn in investigations, used whenever an LLM is configured. `false` keeps investigations deterministic. Restart to change |
| `sre.llm.mode` | `investigator` | `investigator`: the model plans read-only probes within the plan's budget and concludes with cited evidence. `review`: one cheaper review turn. Restart to change |
| `sre.investigator.require_support_citation` | `true` | A model contest or conclusion keeps its root only when a surviving claim cites evidence the causal graph lists as support for that root; otherwise it is inconclusive (`unsupported_root`). `false`: any surviving cited claim carries it. Restart to change |
| `sre.detectors.window_seconds` | `300` | Seconds over which checkpoint and temp-file growth is measured, `60`-`3600`. Keep it at least twice `sre.trigger_interval_seconds`; a shorter window cannot see growth between two polls, and the sidecar warns at startup |
| `sre.detectors.checkpoint_requested` | `3` | Requested checkpoints within the window that are a checkpoint storm (they must also outnumber timed ones), `1`-`1000` |
| `sre.detectors.temp_file_mb` | `1024` | MiB of temp files this database writes within the window that are a temp-file explosion, `1`-`1048576` |
| `sre.detectors.lwlock_waiters` | `8` | Backends waiting on one modeled LWLock class that count as contention, `1`-`10000` |
| `sre.detectors.lwlock_polls` | `3` | Consecutive trigger polls the waiters must hold before an episode opens, `1`-`100` |
| `sre.detectors.cooldown_minutes` | `30` | Minutes after an episode starts before a new episode of the same family can open, `1`-`1440` |

Stop (in the Cases panel, or `POST .../{id}/stop`) pauses an investigation: it keeps its
evidence, steps and used budget, never expires while paused, and continues from where it
stopped on Resume. A queued investigation that is not claimed within 10 minutes (a backlog or
a sidecar outage) ends `expired` with `queue_expired`. While its incident or finding is still
open, the next trigger poll starts a new attempt, up to 5 per trigger; the same holds for a
resumed run that ends `expired` on `stale_evidence`. Investigations started by an operator are
not re-run automatically.

Pinned investigations (Pin in the Cases panel, or `POST .../pin`) and running ones are never
deleted. Where the data lives: the `sage.sre_*` tables are in the meta database when one is
configured and in the monitored database otherwise; triggers are read from the monitored
database's `sage.incidents` and `sage.findings`. Probes run with a 500 ms statement timeout, at
most one probe per database and four per sidecar, and return identities, counts and ages, never
query text. Evidence, hypotheses, events and exports are redacted before they leave the store
(connection URIs, credentials, bearer tokens, SQL literals and raw vectors are removed).

Reading investigations (any signed-in role):
`GET /api/v1/investigations?database=<name|all>`, and per database
`GET /api/v1/databases/{db}/investigations`, `.../{id}`, `.../{id}/events`,
`.../{id}/evidence/{evidence_id}`. Operators and admins can also
`GET .../{id}/export` (JSON, or `?format=markdown`) and `POST .../{id}/pin` or `/unpin`.
Both lists page newest first (`limit`, `cursor`); one `case_id` pages that case's
investigations. Two or more `case_id` values (`?case_id=a&case_id=b`, at most 100, each at
most 256 characters) return the newest investigation of each named case in one page, however
old, with no cursor; more than 100 ids, or ids together with `cursor`, is a 400
`invalid_request`. The Cases page reads its visible cases this way, 100 ids per request, and
refreshes an open investigation every 5 s while it is queued, collecting, evaluating or
waiting for evidence.
With MCP enabled, agents get the read-only tools `sre_list_incidents`,
`sre_get_investigation` and `sre_get_evidence`.

### Sage SRE runbooks

A runbook belongs to one database and is a list of immutable versions. The version that
runs inside matching investigations is the newest one an admin signed whose stored
content still hashes to the signed hash and still validates against today's probe
catalog. A version whose content was changed behind the store never runs.

An edit (`POST /api/v1/databases/{db}/runbooks/{id}/versions` with `base_version`, the
Revise button on the Runbooks page, or the MCP tool `sre_draft_runbook`) stages an
unsigned draft. The signed version keeps running until an admin signs the draft, so an
edit made during an incident never takes a signed runbook offline. A draft never
inherits a signature, and an unsigned version never runs. Signing the draft makes it
the running version in the same transaction. `base_version` must name the latest
version, or the edit is refused with `409 version_conflict`. The response and
`GET .../runbooks/{id}` carry `running_version` (0 when nothing runs) and `running`
beside `latest`.

Before signing, the Runbooks page shows a structural diff of the draft against the
newest signed version (nodes added, removed or changed, by node id), then the full
content hash. Retiring (`POST .../runbooks/{id}/retire`) is permanent and stops every
version, so it needs the admin role, and the page asks for the runbook's name to be
typed first.

### Sage SRE approved actions

When an investigation concludes that one active backend is the root of a lock or
connection-pressure incident (a statement holding a lock that a DDL or other sessions queue
behind), pg_sage proposes one action: `pg_cancel_backend` of that backend. Nothing else is
ever proposed. Termination is never proposed, an idle-in-transaction holder is not proposed
(a cancel does not end an idle transaction; the investigation says so), and a prepared
transaction or contention spread across many sessions is "not proposed" with the reason.

The proposal is derived only from the investigation's cited evidence and a fresh sample of
the target. It names the exact backend: pid, backend start, query start, database, user,
query hash and query id. It also carries its repair contract (mitigation only, one backend,
the conditions recovery is checked against, what is never done) and the policy verdict. A
proposal never executes by itself:

1. With `sre.actions.request_approval: true`, a proposal the policy would allow gets exactly
   one item in the database's existing approval queue, and is sent to ChatOps channels.
   Operators can also request approval in the Cases panel or with the MCP tool
   `sre_request_execution`.
2. A human approves it, on the Actions page, in the Cases panel or with a chat button. The
   approval is single use and records who approved.
3. pg_sage samples the backend again and compares the whole identity. The evidence may be at
   most `sre.actions.max_evidence_age_seconds` old, and the signalling statement checks the
   identity once more. If the backend finished, changed, stopped blocking or is protected,
   nothing is signalled, and the investigation timeline records why.
4. After the cancel, pg_sage samples the blocking every `sre.actions.recovery_sample_seconds`
   until `sre.actions.recovery_samples` fresh samples decide: recovered (the target's wait
   edges cleared and lock waits fell), not recovered, or inconclusive. The result is recorded
   on the timeline and closes the action's verification. Value is credited only on recovery.

The policy gate decides as for any action. `trust.level` must be `advisory` or `autonomous`
(under `observation` the proposal shows why it is withheld), and the executor switch and the
emergency stop apply. The execution mode does not hold back an approved item. An approved
cancel is incident mitigation, so maintenance windows do not delay it. Replication connections, pg_sage's own sessions and `pg_dump`,
`pg_basebackup` and `pg_restore` sessions are never targeted. Superuser sessions are not
protected by default, so list roles or applications that must never be cancelled.

| Key | Default | Meaning |
|---|---|---|
| `sre.actions.proposals` | `true` | Propose automatically for concluded lock and connection investigations. Operators can always propose explicitly. Restart to change |
| `sre.actions.request_approval` | `true` | Queue a proposal the policy allows for approval and notify ChatOps channels at once |
| `sre.actions.approval_ttl_minutes` | `15` | Minutes an approval request stays open, `1`-`60` |
| `sre.actions.max_evidence_age_seconds` | `5` | Oldest identity evidence a cancel may act on, `1`-`5` |
| `sre.actions.recovery_sample_seconds` | `40` | Seconds between recovery samples, `1`-`300` |
| `sre.actions.recovery_samples` | `3` | Fresh samples a recovery verdict needs, `3`-`20` |
| `sre.actions.recovery_deadline_minutes` | `30` | Minutes before an unproven recovery is reported inconclusive, `1`-`240`, at least the sampling span |
| `sre.actions.chatops_tolerance_seconds` | `300` | Maximum age and clock skew of a signed Slack callback, `30`-`900` |
| `sre.actions.protected_roles` | `[]` | Roles whose sessions are never targeted |
| `sre.actions.protected_applications` | `[]` | `application_name` values whose sessions are never targeted |

Routes: `GET .../investigations/{id}/proposals` (any role), and for operators and admins
`POST .../investigations/{id}/proposals` (propose) and
`POST .../investigations/{id}/proposals/{proposal_id}/request`. Approval and denial use the
existing `POST /api/v1/actions/{queue_id}/approve` and `/reject`. With MCP enabled, operators
and admins get `sre_propose_action` and `sre_request_execution`. Neither tool executes.

**Statistics maintenance the morning after.** Extended statistics
(`CREATE STATISTICS`) can be proposed from a concluded or inconclusive investigation that
observed the table in `stat_tables` (the columns are your hypothesis; nothing claims they
cause the incident). The cited evidence must be at most 15 minutes old at propose and at
request, so a review the next day starts with a refresh. Over MCP:

1. `sre_refresh_table_evidence` with `investigation_id`, `schema` and `table` re-reads
   `stat_tables` once, under the investigation's read policy, and records it as
   `operator_refresh` evidence with an `evidence_refreshed` event on the investigation's
   chain. It is refused when the table's database or table OID changed (start a new
   investigation), for cancelled, failed or purged investigations, and more than once a
   minute per investigation (a call refused as too soon reads nothing on the database).
2. `sre_propose_statistics` with the table, `columns` (2 to 8), optional `kinds` and a
   `rationale`. pg_sage cites the newest matching evidence itself; no call carries an
   evidence id or SHA-256.
3. `sre_request_statistics` with the `proposal_id` queues one approval item. A person
   approves it; the executor rechecks the table, owner and column identities under the
   table lock.
4. `sre_rollback_statistics` (approve scope, admin people only, like the REST route) drops
   the object pg_sage created; prior `ANALYZE` statistics are not restored.

Refresh, propose and request need the propose scope; agents never hold approve. One
proposal per investigation still applies, so refresh, propose and request within the
15 minutes. The REST routes under `.../investigations/{id}/maintenance` take the evidence
id and SHA-256 explicitly.

**Approving in Slack or Telegram.** Approval requests are `approval_needed` notifications,
so add a notification rule for that event to the channel. An admin sets this up in the
dashboard under **Notifications > Channels**:

- *Slack*: add a Slack channel with its incoming webhook URL, tick **Interactive
  approvals**, and enter the Slack app's signing secret, plus the team id to accept only
  your workspace. After saving, the tab shows the callback URL
  (`https://<sidecar>/api/v1/chatops/slack/<channel id>`) with a copy button: set it as
  the app's Interactivity Request URL. Requests are verified with Slack's v0 signature
  within `sre.actions.chatops_tolerance_seconds`.
- *Telegram*: add a Telegram channel with the bot token, a numeric chat id and a webhook
  secret (**Generate** makes a random 32-character one). After saving, the tab shows the
  callback URL and the `setWebhook` URL to open once, with the bot token and secret left
  as placeholders for you to fill in. Without a webhook secret the channel notifies but
  cannot approve, and an `@channel` name notifies only. Only button presses from that
  chat are accepted.

The callback URL must be reachable from Slack or Telegram over HTTPS. Each channel's
**Edit** dialog changes its settings: secrets show masked and are kept unless you type a
new one, and a refused change shows the server's message. The channel list shows each
channel's last delivery (status, time and error), or *never delivered*.

A chat user can decide only after an admin links them to a pg_sage user in the **Chat
identities** panel on the same tab: provider, the Slack team id (Telegram has none), the
chat user id (Slack `U...`, Telegram's numeric user id) and the pg_sage user.

The API remains the alternative. Create a channel with `POST /api/v1/notifications/channels`
and `{"name": "ops", "type": "slack", "config": {"webhook_url": "https://hooks.slack.com/...",
"interactive": "true", "signing_secret": "...", "team_id": "T..."}}` (Telegram:
`{"bot_token": "...", "chat_id": "-100...", "webhook_secret": "..."}`), register the webhook
with `setWebhook?url=https://<sidecar>/api/v1/chatops/telegram/<channel id>&secret_token=<webhook_secret>`,
and map a chat user with `POST /api/v1/chatops/identities` and `{"provider": "slack",
"team_id": "T...", "external_user_id": "U...", "user_id": 7}` (Telegram: the numeric user
id, no `team_id`). `GET /api/v1/notifications/channels` returns each channel with masked
secrets and `last_status`, `last_error` and `last_sent_at` (null when never delivered).
In fleet mode channels are global, kept in the sidecar's metadata store.
The pg_sage user's current role must be operator or admin. Each callback is processed once,
and the decision goes through the same approval as the browser, attributed to the mapped
user. Bot tokens, signing secrets and webhook secrets are sealed at rest like other channel
secrets.

*One protocol: the approval card.* A Sage SRE cancel is sent as an approval card, like every
other queued action. Its Approve and Reject buttons carry a single-use card token that works
only in the channel the card was sent to, only while the card is unchanged, and only until
the earlier of 15 minutes and the approval request's expiry. The card is bound to the exact
backend (pid and backend start), so a reused pid is a different card and an old card is
refused as changed. A backend cancel has no Snooze button, and a snooze press for one is
refused. A chat Reject records the denial on the proposal at once. The Approve / Deny
buttons of releases before this one no longer act: for one release, pressing one replies
"This button is outdated; open pg_sage to decide" and changes nothing (the callback still
passes the signature, workspace or chat, replay and role checks first). Decide such requests
in pg_sage or on a newer card.

*Snooze and follow-ups.* Snoozing a card keeps the action pending; autonomy does not take it
over while the snooze runs. When the snooze ends, pg_sage sends the card again, once per
snooze. If that re-send fails, the other ended snoozes are still sent, and the failed one is
retried 5 minutes later; the warning log names each queue item that failed. A re-sent card
replaces the older cards of the same action in the same channel (closed as `superseded`), so
the verification verdict is posted once per channel, as a reply to the message the decision
was made from, even when the decision came from a card sent before the re-send. A card whose
action has no verdict yet is checked again after an eighth of its age (at least 1 minute, at
most 1 hour), so cards still waiting do not hold up newer ones; a card with no verdict after
14 days is closed.

#### Runways and pre-incident investigations

A runway is the time left before a hard limit. Every `sre.runways.interval_seconds`, pg_sage
samples the series a limit is approached along into `sage.runway_samples`: the XID and
multixact counters, the WAL position, the WAL each replication slot retains, the size of the
databases, disk usage (databases plus `pg_wal`, which needs `pg_monitor`) and the sequences
nearest their limit. It fits a straight line through the last `sre.runways.lookback_hours` of
each series and projects when the series reaches its limit:

| Runway | Limit | Finding (category) | Investigation |
|---|---|---|---|
| XID or multixact age, only at or past the cluster's `autovacuum_freeze_max_age` (`autovacuum_multixact_freeze_max_age`) | the wraparound warning limit (2^31 - 1 - 40,000,000) | `forecast_wraparound_runway` (`xid`, `mxid`) | wraparound runway |
| A table 1.25 times past its freeze maximum | its effective `autovacuum_freeze_max_age` (the table's own setting when lower) | `forecast_wraparound_runway` (the table) | wraparound runway |
| Disk usage | `forecaster.disk_capacity_bytes` (only when you declare it; `0`, the default, means undeclared: nothing auto-detects capacity on a self-hosted server, so there is no disk runway, no disk-full credit and no `days_until_full`) | `forecast_wal_runway` (`disk`) | disk/WAL runway |
| WAL a slot retains | `max_slot_wal_keep_size` when it is set, else the WAL custodian's 10 GiB ceiling | `forecast_wal_runway` (`slot:<name>`) | disk/WAL runway |
| A sequence's last value | ascending: the lower of its maximum and its owning integer column's maximum; descending: the higher of its minimum and the column's minimum (a `CYCLE` sequence wraps and is never projected) | `forecast_sequence_runway` (the sequence) | sequence runway |

The sequence runway is pg_sage's only sequence forecast: the forecaster's
`forecast_sequence_exhaustion` rule, which measured each sequence against its own range
and so missed a bigint sequence feeding an int4 column, is retired, and upgrading resolves
its open findings (`detail.resolved_reason`: superseded by `forecast_sequence_runway`).
Each reading lists the 50 used sequences nearest their limit plus up to 50 more that
already have samples in the lookback, so a sampled sequence keeps its series when others
overtake it.

A projection needs `sre.runways.min_samples` samples over at least
`sre.runways.min_span_minutes`. Disk and slot projections also need a steady trend (a line
that fits, r² at least 0.5), so a sawtooth never alarms. A runway inside its horizon opens a
forecast finding (critical inside the critical horizon) and, with `sre.runways.investigate`,
one read-only pre-incident investigation per finding and severity, whatever
`sre.automatic_start` says. A finding resolves when its runway clears and its series is
measured; an unmeasured series keeps its finding open, marked stale. A series is unmeasured
when the lookback holds too few samples, too short a span or an unknown rate, or none at all
(for example after a gap longer than `sre.runways.lookback_hours`). The finding keeps its id
and gains `detail.stale = true`, `detail.stale_reason` (`unmeasured` or `no_samples`) and
`detail.stale_since`; the next measured projection replaces the detail and clears them. A
replication slot or sequence is held only while the current reading still lists it, so the
finding of a dropped slot or sequence (or one past the 50 kept sequences) resolves.
While the slots cannot be read, an open slot finding without samples is held too.
When the trends read reaches its row cap, the categories it may have cut are not resolved
until it fits again (logged once). A standby samples nothing (it is read-only).
In fleet mode the cluster runways (`xid`, `mxid`, `disk` and every `slot:<name>`) are
reported by one owner database per cluster, not once per database; see
[Cluster runways and forecasts](walkthrough-fleet.md#cluster-runways-and-forecasts).

Anti-wraparound autovacuum freezes a table once its age passes `autovacuum_freeze_max_age`,
which resets the age, so below that setting the XID runway is bounded by freezing however
fast transactions consume XIDs: a busy, healthy cluster opens no wraparound finding. The
XID (multixact) trend is projected only once the cluster's oldest age is at or past
`autovacuum_freeze_max_age` (`autovacuum_multixact_freeze_max_age`), read with
`current_setting` (postmaster-level on PostgreSQL 14-18; managed providers expose it the same
way). Past it, the projection uses the net age slope (the age's own trend, freezing included)
when it is known and rising, else the consumption rate; the finding's detail records
`freeze_max_age`, `age`, `gate`, `projection_basis` (`net_age_slope` or `consumption_rate`),
`age_rate_per_s` and `consumption_rate_per_s`. Below the setting, a stuck xmin holder is the
xmin-horizon probe's and the overdue-table rule's (a table 1.25 times past its maximum). When
the cluster's age or freeze maximum cannot be read, the XID finding is held, marked stale with
`stale_reason` `freeze_gate_unknown`. On upgrade, an open XID or multixact finding on a
cluster below the setting resolves on its first measured tick.

A pre-incident investigation explains what drives the runway. Wraparound: a session,
prepared transaction or replication slot holding the xmin horizon, busy, disabled or
cancelled autovacuum (cancellations are read from logged incidents, so with no log source
their absence proves nothing), or a surge in XID use. Disk/WAL: an inactive slot, a slow
consumer, a failing archiver, a write surge or database growth. Sequences: which limit binds
(the sequence's type, a narrower owning column, or an explicit `MAXVALUE`). A concluded
investigation lists the existing custodian action that addresses it (the freeze custodian's
`VACUUM (FREEZE)` or blocker response, the WAL custodian's bound or its escalation plan) with
the standing policy gate's verdict. The investigation never executes it: the custodian
workers submit their proposals on their own schedule under your autonomy settings.
Sequences have no custodian action, because widening a column rewrites the table; the
investigation says so and gives the manual step.

| Parameter | Default | Description |
|---|---|---|
| `sre.runways.enabled` | `true` | Sample runways and open forecast findings |
| `sre.runways.investigate` | `true` | Open a read-only pre-incident investigation per runway finding and severity |
| `sre.runways.interval_seconds` | `60` | Seconds between samples, `15`-`3600` |
| `sre.runways.sequence_interval_seconds` | `600` | Seconds between sequence samples, `15`-`86400`. Sequences are consumed slowly and are the costliest series to read (one lock per sequence read), so they are read less often, through a separate 2 s background budget. The effective period is never shorter than `interval_seconds` and never so long that `min_samples` no longer fit in `lookback_hours` |
| `sre.runways.size_interval_seconds` | `600` | Seconds between measurements of the databases' total size, `15`-`86400`. Summing every database's size stats every file of every database (hundreds of milliseconds on a large catalog), and the disk runway forecasts over hours, so the size is measured less often and the last measurement is reused in between; only a new measurement is sampled into the disk and database trends. Fleet runtimes on one cluster share one measurement. The effective period is never shorter than `interval_seconds` and never so long that `min_samples` no longer fit in `lookback_hours` |
| `sre.runways.lookback_hours` | `6` | Hours a trend is fitted over, `1` to `sample_retention_hours` (at most `168`). Pre-incident investigations read the same window |
| `sre.runways.min_samples` | `10` | Fewest samples a projection needs, `3`-`1000` |
| `sre.runways.min_span_minutes` | `30` | Shortest span of samples a projection needs |
| `sre.runways.wraparound_horizon_hours` | `336` | Wraparound runway horizon (14 days) |
| `sre.runways.wraparound_critical_hours` | `72` | Critical wraparound runway |
| `sre.runways.disk_horizon_hours` | `72` | Disk and slot runway horizon |
| `sre.runways.disk_critical_hours` | `24` | Critical disk and slot runway |
| `sre.runways.sequence_horizon_days` | `30` | Sequence runway horizon |
| `sre.runways.sequence_critical_days` | `7` | Critical sequence runway |
| `sre.runways.sample_retention_hours` | `48` | Hours samples are kept, `lookback_hours` to `720` |

Changing these needs a restart.

#### SLO burn-rate evaluation and Prometheus coverage

Each SLO (`sre.slo`) is evaluated against multi-window burn-rate rules (by default the
Google SRE workbook pairs: page at 14.4x over 1h and 5m or 6x over 6h and 30m, ticket at 1x
over 3d and 6h). A rule fires only when both of its windows burn at or above its factor.
A window is unknown when it has too few eligible events (`min_eligible_events`), none,
samples covering under 90% of it, no data, stale data, a failed query or an invalid value.

A rule is decided as a Kleene AND of its windows: when one window is known and below the
factor, the rule is not firing even though the other window has too few events, zero
events or partial coverage (`low_traffic`, `zero_eligible`, `partial_window`). The rule
then reports that reason as `partial` instead of `unknown`, so a quiet low-traffic service
is `ok` at night rather than `unknown`. A rule stays unknown while a known window is at
or above the factor (it could still fire), while neither window is known, and whenever
the other window is absent, stale, failed or invalid (`no_data`, `stale`, `source_error`,
`invalid_value`, `ambiguous_series`): a stale source never turns `ok`. A firing rule
still beats an unknown one.

A Prometheus SLI's windows are only as honest as their coverage. Set
`sre.slo.objectives[].coverage_query` to PromQL that returns the earliest sample time
(Unix seconds) over `$window` as one value, for example
`min(min_over_time(timestamp(http_requests_total{job="checkout"})[$window:1m]))`. Apply
`timestamp()` to a raw series selector (on an aggregate it returns the evaluation time,
which would claim full coverage); the outer `min` makes several series one value (several
values are `ambiguous_series`). pg_sage then evaluates
it over each window (coverage) and over `stale_after_seconds` (freshness): a series that
began two hours ago gives a 30-day budget of `partial_window`, and no sample within the
stale limit makes the window `stale`. Prometheus resolves recency to its lookback delta
(5 minutes by default), so a scrape outage is seen up to that much later. Keep
`stale_after_seconds` at least the subquery step (`1m` above): a shorter freshness range
can hold no step, and the window then reads `stale`. A failing
coverage query makes the window unknown with the query's reason, never fully covered.
Without `coverage_query`, Prometheus windows are taken as fully covered and fresh (as
before) and the SLO reports `coverage_unverified: true`; the SLOs page shows "coverage
unverified". Pushed and proxy SLIs measure coverage from their stored samples.

| Parameter | Default | Description |
|---|---|---|
| `sre.slo.objectives[].coverage_query` | empty | Optional PromQL for the earliest sample time over `$window` (must contain `$window`; Prometheus sources only). Adding it does not reset the SLO's history |
| `sre.slo.objectives[].stale_after_seconds` | `300` | Pushed samples, and Prometheus samples with `coverage_query`, older than this are stale |

Each SLO status (`GET /api/v1/sre/slos`, and `status` in `GET /api/v1/sre/slos/{name}`) also
carries `budget_coverage`, the share of the SLO window its data covers, and, for a
`partial_window` budget only, `budget_spent_so_far`: the covered part's bad over eligible
events divided by `1 - target` (1 means the whole error budget). `budget_remaining` stays
null for a partial window: the spent-so-far describes only the covered part and is never
read as budget left. `budget_coverage` is absent when the budget window was not read
(no data, stale, a failed query or invalid counts) and for a Prometheus SLI without
`coverage_query`, whose coverage is only assumed. `budget_spent_so_far` is absent with fewer
covered eligible events than `min_eligible_events`, as a burn rate from a handful of events
is noise. A fleet SLO with an `owner_database` also carries `owner_database`, the
database whose investigator opens its `slo_burn` investigations. These fields are
additive; no migration or configuration is needed.

The SLOs page shows, per row, how long the SLO has been in its state (`state_since`) and,
while it burns, when the burn started (`burn_started_at`). An unknown budget names its
reason, with "partial window: N% covered" and the spent-so-far for a partial window. A row
expands into its state transitions (kept 90 days, newest 50), the recovery verdict since
the burn started (only while burning) and the newest `slo:<name>` investigation, read from
the `owner_database` when the SLO has one. When none was opened for a paging SLO it says so
(`sre.slo.open_investigations` off, or `sre.automatic_start` off for a database proxy);
when that database's investigator cannot be read it says so instead, never "none opened".
The expansion reads the detail once per open and always names the row's database, so it
works in fleet mode and for viewers.

#### Fleet SLOs and `owner_database`

An app objective with no `database` belongs to every database. In standalone mode that is
the one monitored database. In a fleet (`mode: fleet`, or a meta-db fleet) every database's
engine evaluates the same queries, so one service burn used to open one `slo_burn`
investigation per database: N model sessions, N probe budgets and N customer-impact rows.

Fleet and meta-db configs must now either bind the objective with `database`, or name the
database whose investigator handles its burns with `owner_database`. Every database still
evaluates an owned objective, so its error budget still caps autonomy on every database,
but only the owner opens the investigation. The others log at INFO that the burn is owned
by the owner database. A static fleet's `owner_database` must be one of `databases[].name`.
A meta-db fleet discovers its databases, so pg_sage cannot check the name when it loads the
config. At startup it logs a WARN for each owner that matches no registered database: until
one does, that SLO's burns open no investigation anywhere. The owner must also evaluate the
objective: a read policy that keeps only bound push objectives (database scope, such as
Lakebase) or that leaves out `slo_status` drops it on the owner, so pg_sage logs a WARN
naming the objective when that owner database starts.

| Parameter | Default | Description |
|---|---|---|
| `sre.slo.objectives[].owner_database` | empty | Fleet and meta-db modes, objectives with no `database` only: the one database that opens the SLO's `slo_burn` investigations. Refused with `database` set and in standalone mode. Setting or changing it starts a new SLO definition (fresh history) |

**Upgrade note (breaking for fleets):** a fleet or meta-db config with an app objective that
has no `database` and no `owner_database` now fails to load. The error names the objective
(`sre.slo.objectives[N] (name)`) and both fixes. Add `database: <db>` when the service uses
one database, or `owner_database: <db>` when it spans the fleet. Standalone configs and
bound objectives are unchanged, and so are idempotency keys and the SLO history of every
objective without an owner. No migration is needed.

### Sage SRE earned autonomy

pg_sage keeps an autonomy level per incident family and action class (for example
`wal_retention` / `wal_bound`, `wraparound_runway` / `freeze`). The level applies to actions
pg_sage starts on its own for an incident family, custodians included. Operator-approved
actions and read-only diagnostics are not restricted.

| Level | Meaning |
|---|---|
| L0 | Observe only |
| L1 | Diagnose and write the script; never executes (default for every known pair that was not autonomous before) |
| L2 | Hand off for one-click approval (a finding plus an approval-queue item) |
| L3 | Execute a reversible, single-object action inside the standing-policy window, then notify |

L4 is never reached. Irreversible classes (slot drop, backend terminate, schema change,
sequence migration, anything unclassified) never go above L1, and mitigation-only classes
(`backend_cancel`, `wal_bound`) and `config_guc` never go above L2. Operator trust settings
and the standing policy stay the outer bound: the ledger can only restrict them.

**Carried-over autonomy.** The ledger gates new autonomy and keeps what pg_sage already
had. When a database's ledger is first set up, four pairs are seeded at the level that
database's trust and execution settings already let run unattended:
- the custodian freeze and autovacuum tuning (`wraparound_runway`). The freeze custodian no
  longer tunes autovacuum, so no action uses the autovacuum tuning pair today: the analyzer's
  `autovacuum_tuning` finding is self-initiated and earns trust in the `tuning` family;
- the WAL bound (`wal_retention`);
- load-admitted index creation (`plan_regression`).

Settings with execution mode `auto` and the matching tier enabled give L3. Anything else
gives the default L1. The view shows these as "carried over", with the decision that
granted them. Irreversible classes are never carried above L1. A carried level does not
depend on promotion evidence. The downgrade signals below still cap it, and it resumes by
itself when they clear. An operator downgrade ends the carry-over, and the pair then has to
earn its level back with evidence.

**Approved actions and outcomes.** M5's `cancel_backend` is approval-only in the ledger: it
is mitigation-only, never above L2 and never carried over. Each approved cancel whose
recovery pg_sage verified is recorded as an L2 outcome of its family (`lock_blocking` or
`connection_pressure`), and a cancel that failed or did not recover is recorded as not
recovered. Executed custodian actions are recorded as well: an L3 execution as an L3 outcome
and a mandatory deadline override at L1, which is never promotion evidence. A harmful outcome
of any of them is a family safety regression. Runbook action proposals name their family and
autonomy class.

**Mandatory deadlines.** A critical XID or disk deadline that the standing policy lets
override (a red wraparound freeze, for example) is not held back by the ledger level or by
a downgrade, because waiting risks an outage. The emergency stop and the rest of the gate
still apply. The ledger records each such action once per object and deadline while the
sidecar runs.

**Promotion.** pg_sage proposes one level at a time, and only an admin approves; pg_sage, the
system and MCP clients cannot. The evidence is checked again at approval time, and a proposal
expires after `proposal_ttl_hours`. A family's bench checks read its newest report that counts
for the running build, whatever its origin (an air-gapped install runs on operator reports), so
a proposal names the deciding report's provenance and id (for example `evidence supports L2;
bench: unsigned (operator-provided) <id>`), and the Trust page shows it on the row and on the
approval card. Promotion to L2 requires all of the following:

- PGIncidentBench top-1 accuracy of at least 80% over at least 10 runs, mechanism precision
  of at least 90%, and no forbidden actions, on every gated arm for the family. The report
  must be at most 30 days old.
- At least 30 days of shadow reviews, with at least 20 reviewed packets and at least 95% of
  them accepted.
- No safety violations in the family in the last 30 days.

Promotion to L3 also needs a Safe Pass rate of at least 95%, and at least 95% on game days if
any have run. It also needs at least 50 verified recoveries at L2, and the pair must never
have had a harmful outcome. An action an operator rolled back is never a verified recovery,
whatever its verdict and whether the rollback came before or after pg_sage recorded it.
These are the spec's thresholds and the defaults; the timing, volume and accuracy values
can be lowered under `sre.autonomy.promotion` (see
[Fast elevation](#fast-elevation-dogfood-databases)). The report age (30 days) and the
minimum bench run counts (10) are fixed.

**Evidence decay.** An earned L2 or L3 holds only while its evidence does. The bench report
stops counting 30 days after its `generated_at` (a release's shipped report too), and shadow
reviews leave the 30-day window one by one. No evidence is permanent: a signed report for the
running build expires like any other, because it proves the build, not today's workload. The
workload, the data volume, the model behind the LLM features and the PostgreSQL minor version
all drift while the build stays the same. Only a fresh report renews the bench check (new
shadow reviews renew only the shadow window), and the level returns once both hold again. The
Autonomy and Trust pages show, on every earned row, the date the level holds until and the
checks that expire then (`holds_until`,
`expiring_checks` in `GET /api/v1/sre/autonomy` and `GET /api/v1/trust`), for example "L3
holds until 2026-12-03 09:30 UTC unless a new bench report arrives". The date is the last
moment the evidence still counts: for the shadow window, the review whose leaving breaks the
20-review or 95%-accepted bar, not the oldest review. Every evaluate interval
(`evaluate_interval_minutes`), pg_sage re-reads the evidence of each earned pair without
waiting for an action:

- When the evidence no longer supports the granted level, the gate allows only the supported
  level (as before), and the history now records it once as `capped` with the reason
  `evidence_decayed:<checks>` and the evidence `{granted, supported, failing_checks}`. It
  records `cap_cleared` once new evidence supports the level again. The granted level is
  not changed: ingest a new report (or record the reviews) and the level is back. Neither is
  recorded again after a restart.
- Once the date is 7 days away or less, pg_sage warns once per pair and date: a history
  entry `evidence_expiring` and a notification through the rules for failed actions
  (`action_failed`, severity warning), like a demotion. A date that moves later by 7 days
  or less (a shadow window renewed review by review, a report only a little newer) is the
  same warning and is not sent again. A date that moves earlier, or a report that renews
  the level by more than 7 days, warns again once the new date comes near.

Carried-over and self-initiated levels do not decay with evidence and are not swept. The
local bench is not scheduled by this: run it, or ingest a report, before the date.

**Downgrade.** At authorization time, any of the following caps an L2 or L3 pair at L1. The
change is logged once per transition.

- The database's SLO engine (`sre.slo`) reports an SLO burning its error budget at the page
  rate (database proxy SLOs included), or a
  registered app SLO's state is unknown. An unknown proxy SLO does not count, since it is
  often unknown for structural reasons (no standbys, no log access). A rule decided "not
  firing" by one quiet window (see
  [SLO burn-rate evaluation](#slo-burn-rate-evaluation-and-prometheus-coverage)) does not
  make the SLO unknown, so a low-traffic window alone no longer caps autonomy. A
  Prometheus SLO with `coverage_unverified` is not unknown and does not cap autonomy by
  itself; add `coverage_query` so a scrape outage or a new series is seen as stale or
  partial.
- The HA role is not primary, safe mode is on, or the role changed in the last
  `failover_cooldown_minutes`.
- The action's evidence is older than `max_evidence_age_seconds`.
- Another pg_sage action holds or recently touched the same object.
- The family had a harmful or unsafe outcome in the last `safety_window_days`.

A harmful or unsafe outcome also demotes every earned class of its family to L1 durably,
and re-promotion needs the evidence again. A carried-over pair is instead capped for
`safety_window_days`. Operators can downgrade a pair or a whole family at
any time.

An operator's rollback of an L2 handoff or an L3 action demotes that pair one level, as a
rejection of its handoff does, records the cause (`rolled back by an operator`) in the history
and notifies an operator. It is not a family-wide demotion; the rollback of an action's own
regression is not a second demerit (the regression already demoted the family), and the
automatic revert of a neutral index create is no demerit. A carried-over pair keeps its
level but loses the rolled-back action's credit. A rollback is judged once, on the next
reconcile pass. After an upgrade, a rollback older than the newest rollback the reconciler had
already read is not judged, and one that happened before the pair's level was last set never
demotes it.

**How live outcomes are read.** Each reconcile pass reads the approved L2 handoffs and the L3
executions of the last 30 days forward from a cursor kept per database in
`sage.trust_ledger_state` (`handoff_cursor`, `auto_cursor`), at most 1000 of each kind per
pass, so a busy database's newest actions are reached within a few passes. The cursor moves
past an action only once it is 5 minutes old, so an action whose transaction commits late is
still read. An action still waiting for its verification verdict, or whose L3 notification
failed, stays in a pending set (`handoff_pending`, `auto_pending`, at most 1000 each) and is
read again by id each pass until it is recorded and notified. The reconciler stops waiting for
a pending action once it is older than 30 days, or when the set overflows (the oldest go
first). It logs a warning naming the action ids, whose outcomes are then not recorded. A pass
that finds nothing new writes nothing and keeps the evidence cache. After an upgrade the
cursors are empty, so the first pass reads the 30 days as before.

**Model-sourced roots (measured).** When an investigation's causal graph is conclusive and
the model ranks another open hypothesis first, the model's root is advisory (L1) by default:
the graph's root stands and the investigation shows the contest (`model_contest`). The model
may override the graph's root for a family only once PGIncidentBench measured it on the
held-out replay cases with a live model: override precision with a Wilson 95% lower bound of
at least 0.80 over at least 10 overrides, no forbidden action, no drop in Safe Pass, and the
run inside its budget. The bench report comes through the same path as promotion evidence
(signed release or nightly report, local run or upload, for the running build, at most 30
days old), and the newest live measurement of each family decides. A model's own confidence
never counts. Actions that follow an adopted root still pass the family's earned levels.
The authority belongs to the model that earned it: a measurement counts only while the
database's `llm.model` is the model the report's `llm.model` names (compared trimmed,
lowercased and without a provider prefix, so `OpenAI/GPT-6` is `gpt-6`; `gpt-6-mini` is
another model), and the newest measurement of the running model decides even when a newer
report measured another one. A report that names no model grants nothing. After a hot
reload to another model, the next investigation is advisory and the change is recorded as
`root_authority_revoked`. The endpoint is not identified: what `llm.endpoint` serves under
that model name is part of what you trust.
The Trust page's "Model lift over deterministic" card and `GET /api/v1/model-lift[?database=]`
show each family's lift (Safe Pass against the graph, override precision, inconclusive-case
lift), the model it was measured with and the model the investigator runs (`measured_model`,
`running_model`), whether the model may override, and how many more correct held-out
overrides the
family needs (`overrides_needed`; families are never pooled). A family earning or losing
the authority is recorded in the trust history (`root_authority_granted` /
`root_authority_revoked`, with the deciding report's id) and told through your notification
rules (`action_executed` for a grant, `action_failed` for a loss). See
`sidecar/sre-bench/README.md` ("Model lift", "Nightly live-model arm").

**Contested investigations become replay cases.** After you refute an investigation (or
confirm it with another actual root), `GET /api/v1/databases/{db}/investigations/{id}/replay-case`
(operator) or `pg_sage bench export-replay --investigation <id>` (reading the control
database from `PG_SAGE_EXPORT_DSN`) exports it as a redacted PGIncidentBench replay case:
identifiers hashed unless `keep_identifiers=true` / `--keep-identifiers`, secrets and PII-like
literals always removed.

| Parameter | Default | Description |
|---|---|---|
| `sre.autonomy.enforce` | `true` | The ledger restricts self-initiated incident-family actions. `false` returns them to the trust ramp; the sidecar warns at startup |
| `sre.autonomy.bench_results_path` | `""` | PGIncidentBench JSON report, or a directory searched three levels deep, ingested at startup and hourly. Point it at the CI `pgincidentbench/` artifact to read the shard reports (`core-a`, `core-b`, `reactive-a`, `reactive-b`, `runway-a`, `runway-b`; its replay corpus report, `pgincidentbench-replay.json`, is not a bench report and is skipped; naming it as the path is an error); each family reads the newest report that scored it. A `<report>.sigstore.json` bundle is verified; a report for another pg_sage build is refused, and a refused report is logged once per process per file version (at INFO, naming the build it scores; rewriting the file logs it again), while other ingest errors warn on every ingest. Not needed for the reports a release ships. `pg_sage bench install` copies a release's signed live-model report and its bundle here after verifying them; only a signed, stamped report earns model-root authority. Empty: upload through the API |
| `sre.autonomy.evaluate_interval_minutes` | `60` | Minutes between promotion evaluations, `5`-`1440` |
| `sre.autonomy.reconcile_interval_seconds` | `60` | Seconds between recording live outcomes, `10`-`3600` |
| `sre.autonomy.max_evidence_age_seconds` | `300` | Older evidence caps an action at L1, `5`-`3600` |
| `sre.autonomy.concurrency_window_minutes` | `15` | A same-object action this recent caps at L1, `1`-`1440` |
| `sre.autonomy.safety_window_days` | `30` | Days a harmful outcome caps its family, `1`-`365` |
| `sre.autonomy.failover_cooldown_minutes` | `30` | Minutes after a role change autonomy stays at L1, `1`-`1440` |
| `sre.autonomy.proposal_ttl_hours` | `168` | Hours a promotion proposal waits for an admin, `1`-`720` |
| `sre.autonomy.report_retention_days` | `90` | Days a stored bench or game-day report is kept after it was ingested, `30`-`3650`. Never deleted: a report that is the evidence of a current autonomy level or a pending promotion, and the newest report of each family (per source, and per database for game days). Pruned with the other retention rules, in the database that holds the ledger (the meta database when one is configured) |
| `sre.autonomy.promotion.shadow_window_hours` | `720` | Hours of shadow reviews (from the first) a family needs for L2; reviewed and accepted packets are counted in this window. `1`-`8760` |
| `sre.autonomy.promotion.shadow_min_reviewed` | `20` | Reviewed packets needed inside the shadow window for L2, `3`-`1000` |
| `sre.autonomy.promotion.shadow_min_accepted_pct` | `95` | Operator-accepted share of reviewed packets for L2, `50`-`100` |
| `sre.autonomy.promotion.bench_min_top1_pct` | `80` | PGIncidentBench top-1 on every gated arm for L2, `50`-`100` |
| `sre.autonomy.promotion.bench_min_precision_pct` | `90` | PGIncidentBench factual (mechanism) precision on every gated arm for L2, `50`-`100` |
| `sre.autonomy.promotion.min_safe_pass_pct` | `95` | Safe Pass on bench fault programs and game days for L3, `50`-`100` |
| `sre.autonomy.promotion.min_live_recoveries` | `50` | Verified live L2 recoveries a pair needs for L3, `1`-`10000` |
| `sre.autonomy.game_days.enabled` | `false` | Run PGIncidentBench fault programs on a disposable clone |
| `sre.autonomy.game_days.interval_hours` | `168` | Hours between scheduled game days, `24`-`2160` |
| `sre.autonomy.game_days.local_dsn` | `""` | Development fallback when `clone.provider` is `none`, for game days and local bench runs. It must be its own cluster, marked (see below). A database on the cluster of a monitored or the metadata database is refused |
| `sre.autonomy.game_days.families` | `[]` | Families to exercise. Empty: every family the bench covers |
| `sre.autonomy.game_days.customer_programs` | unset | Run the customer programs, which exercise the monitored database's own hottest table on the clone (see below). Unset: on when the clone provider copies the database (`dle`, `lakebase_branch`, `neon_branch`, `aurora_clone`); `local_dsn` never runs them |
| `sre.autonomy.canary.canary_instances` | `1` | Databases changed and verified before a fleet rollout widens, `1`-`10` |
| `sre.autonomy.canary.regression_limit_pct` | `10` | Measured regression (a database's effect net of the untreated members' change) that halts and rolls back a rollout, `0`-`100` |
| `sre.autonomy.canary.stage_wait_hours` | `24` | Hours a fleet rollout waits after a database's verdict before it changes the next one, `0`-`720` (`0`: no pause) |
| `sre.autonomy.canary.settle_seconds` | removed | Removed in executor-04: a canary now waits for each database's own verification verdict instead of a fixed settle time. A config that still sets it loads and prints one startup warning that the key is ignored; remove the key, which a later release rejects |

Game days run only the deterministic causal-graph arm, so they spend no LLM tokens. A
forbidden action on a game day is recorded as a safety violation. The clone DSN is never
stored.

The fault programs act on the whole cluster: they run `ALTER SYSTEM SET max_wal_size`
with `pg_reload_conf()`, CHECKPOINT storms, a physical replication slot, XID burn and a
pinned xmin horizon. So `local_dsn` must name a database on a cluster of its own. Before
every game day and local bench run pg_sage opens one short, read-only connection to it
(5 s timeouts) and refuses the run when:

- it is on the same cluster as any monitored database (fleet members added after start
  included) or the metadata database. The cluster is identified by
  `pg_control_system().system_identifier`; where that function is revoked, by the shared
  `pg_database` catalog and `pg_postmaster_start_time()`. A sibling database, another host
  name for the same server, a PgBouncer port or a standby all count as the same cluster;
- it is a standby (`pg_is_in_recovery()`) or streams to standbys (`pg_stat_replication`);
- it names more than one server (a multi-host DSN), since only the server it reaches is
  checked;
- it is not marked disposable. Mark it with
  `COMMENT ON DATABASE scratch IS 'pg_sage:gameday_target';`;
- any of these checks cannot be read (fail closed).

A refused run is recorded as `failed` with the reason and the fix, and no fault program
runs.

One run at a time per `local_dsn` cluster. Every database of a fleet is given the same
`local_dsn`, so game days and local bench runs on it take turns: a run holds the target
from the start of its fault programs to the end, inside the sidecar and, through a
session-level advisory lock in the target server's `postgres` database (the role needs
`CONNECT` there), across sidecar processes. A run started while another holds the target
is refused before anything is recorded: the API answers `409` naming the holder
(`target busy: game day <id> for <database>`, or "another session" for another
process), and a scheduled game day tries again at its next hourly check. A holder that
crashes releases the target with its connection. Each database's first game-day check
waits five minutes plus a delay derived from its name (under an hour), so a fleet does
not start its game days together. Clone providers (`dle`, `snapshot`) give each run its
own clone and are not serialized.

A run that its sidecar never finished (a crash, `kill -9`, a redeploy mid-run) is
cleaned up afterwards. Game days and local bench runs are both recorded in
`sage.sre_game_days` (`kind` is `game_day` or `local`; the game-day list and schedule
read only game days), and each run's clone id is recorded as soon as the clone exists. A
run still `running` 6 hours after it started is marked `abandoned` and its recorded
clone destroyed (no live run gets that old: a run's clone and fault programs stop after
5 hours, and the run fails): game days at each hourly check, local bench runs at
startup and before each new run. A younger `running` row may be another
sidecar's live run and is left alone. A clone that cannot be destroyed is named in the
row's `error`, for the operator to remove; with `local_dsn` nothing is destroyed (the
target stays). Shutdown no longer stops a fault program's recovery: it finishes on a
2-minute budget of its own, no further fault program starts, and the run fails
without a report (a partial run is never counted as evidence). Before any fault
program runs, a run also removes what a crashed run left on the target's cluster,
under the same cluster lock the WAL and checkpoint programs hold: `ALTER SYSTEM`
settings a fault program recorded in `sage.bench_fixture_state` on the target are
reset, inactive `bench_slot_*` replication slots are dropped, and bench prepared
transactions (`bench_gid_*`, `bench_prepared_*`) prepared more than 6 hours ago are
rolled back. Nothing a bench did not name or record
is touched: an operator's own `ALTER SYSTEM` setting, another slot or an application's
prepared transaction stays.

Each game day and local bench run ends in one status, shown on the Autonomy page and in
`GET /api/v1/sre/autonomy/game-days` (`status`):

| Status | Meaning |
|---|---|
| `running` | In progress |
| `completed` | The fault programs ran, the report was ingested and at least one run scored |
| `completed_unscored` | The fault programs ran and the report was ingested, but no run scored: every fault program skipped on the clone (for example a snapshot clone without superuser skips the checkpoint, WAL-slot, temp-file and archiver programs, and `max_prepared_transactions = 0` skips the prepared-transaction programs), or the report had no runs. It is no safety signal: fix the clone and run again |
| `failed` | The clone, the fault programs or the ingest failed (`error` says which) |
| `destroy_failed` | The run finished but its clone could not be destroyed (`error` names it) |
| `abandoned` | Its sidecar never finished it (see above) |

Every finished run also records `summary` (`sage.sre_game_days.summary`): the count of
`scored`, `skipped` and `errored` runs and `top_skips`, the five most frequent skip
reasons with their counts. A `completed_unscored` run logs one WARN naming them. A run
that failed before it had a report, and a row written before this release, has no
`summary`. Game-day Safe Pass evidence is unchanged: a `completed_unscored` report adds
nothing to a family's `game_day_safe_pass` check, so that check still reads "none"
until a game day scores a run of the family.

**Customer programs.** The fault programs above create their own `bench_*` tables, so on
their own a game day says nothing about the customer's database. A game day therefore
also runs the customer program `lock_customer_hot_table` on the clone. Before the clone
is created it picks the monitored database's hottest table from the collector's two most
recent snapshots: the permanent ordinary table (not in `sage`, `pg_*` or
`information_schema`) with the largest increase of `n_tup_ins + n_tup_upd + n_tup_del +
idx_scan` between them (lifetime counters are not used; ties go to the larger table, then
the name). On the clone it checks the table exists (`to_regclass`), then one session holds
`ROW EXCLUSIVE` on it while it sleeps for 60 seconds, a `SHARE ROW EXCLUSIVE` request queues
behind it and two `ROW EXCLUSIVE` requests queue behind that one (each with
`lock_timeout = '90s'`, each rolled back). No row is read or written, so the work does not
depend on the table's size, and the table is never altered or dropped. The investigator
is graded as for the other lock programs (the DDL lock queue, with the holder as its root
blocker); recovery ends only the program's `bench_*` sessions and checks no lock wait is
left. Bindings are per database, so in fleet mode each database's game day uses its own
hottest table. On a managed database it runs only on a branch or snapshot clone.

The program is skipped, with the reason recorded, when the provider is `local_dsn` (its
target holds no customer objects), `customer_programs` is `false`, the collector has fewer
than two snapshots or no table had activity between them, or the clone lacks the table
(`relation not in clone`). A game day whose customer program ran and scored nothing
finishes `completed_unscored` (it says nothing about this database), and a customer
program that failed (for example `SQLSTATE 42501`, the clone's role may not lock the
table) fails the game day. Local bench runs do not run customer programs.

Each game day records what it exercised in `sage.sre_game_days.exercised` (and
`exercised` in the game-days API): `provider`, `customer_objects` (whether the clone is a
copy of the database), and `scenarios`, each with its `id`, `runs`, `scored`, the first
`skip` or `error`, and for a customer program its `binding` (the quoted table name) and
`binding_hash`. The table name stays in this row; the bench report ingested as evidence
names only the program and the hash.

```sql
SELECT exercised->'scenarios' FROM sage.sre_game_days ORDER BY started_at DESC LIMIT 1;
```

#### Bench evidence: signed release reports and local runs

L2 needs a PGIncidentBench report for the family. You do not have to copy CI artifacts:

- **Shipped with every release.** The release workflow runs PGIncidentBench on the tagged
  commit, signs each shard report keyless with Sigstore (cosign, GitHub OIDC identity of
  the pg_sage `ci.yml` workflow; no key to manage) and ships the reports in the image at
  `/usr/share/pg_sage/bench` and in the release archives in `bench/` next to the binary.
  They are also release assets (`pgincidentbench-<shard>.json` with
  `.json.sigstore.json`). The sidecar ingests them at startup and hourly, verifying the
  signature offline against the Sigstore trusted root embedded in the binary, so an
  air-gapped install has them too. The signer must be the `ci.yml` workflow of the
  jasonmassie01/pg_sage repository, pinned by its immutable GitHub repository and owner
  IDs, so a repository registered later under the same name cannot sign accepted reports.
- **Bound to the build.** A report names the pg_sage version and commit it scored. A
  report for another build is refused, and stored reports of an older build stop
  counting after an upgrade (until the new release's report is ingested). A signature
  must name the same commit as the report.
- **Run bench locally.** On the Autonomy page an admin can run the fault programs of a
  family on a disposable clone (`clone.provider` `dle`, else
  `sre.autonomy.game_days.local_dsn`; game days need not be enabled). `local_dsn` must
  be its own cluster, marked, as for game days: a database on the cluster of a monitored
  or the metadata database is refused. The report counts only for the
  families it covered, marked "local run", and repeats every scenario until each family
  has the promotion bar's sample size (`n >= 10`).
- **Operator reports.** A report uploaded through the API or found under
  `bench_results_path` without a bundle works as before for promotions, marked
  "unsigned (operator-provided)". With a `<report>.sigstore.json` bundle next to it, it
  is verified, and a bundle that does not verify refuses the report.
- **Model-root authority needs a signed report.** A family's model lift counts only
  from a signed release or local-run report stamped with the running build. An
  unsigned upload, or a report with no `pg_sage_version`/`pg_sage_commit`, never grants
  it. Install the release's signed live-model report with
  `pg_sage bench install [--config FILE] [--commit SHA] pgincidentbench-live.json`: it
  verifies the bundle for this binary's commit and copies the report and bundle,
  atomically, into `bench_results_path` (it exits 2 when that is not set), where the
  hourly ingest stores it as a signed release report. An admin can instead upload both
  to `POST /api/v1/sre/autonomy/bench-results` as `multipart/form-data` parts `report`
  and `bundle` (at most 1 MiB together); a JSON body stays unsigned. Each database's
  ledger in a fleet ingests from the shared path.

The Path to next level panel shows each family's report and where it came from. To check
a downloaded report by hand: `pg_sage bench verify --commit <sha> pgincidentbench.json`
(the bundle next to it as `pgincidentbench.json.sigstore.json`).

The fleet canary is started by an admin from a verified action on one database. On every
other database, it applies the same statement only where that database has its own open
finding with the same SQL, and only through the approval path. It verifies the canary
databases first, then widens. It halts and rolls back everything in reverse order on a
failed verification or a regression above the limit.

After each change the canary waits for that database's own verdict. A reversible action is
`monitoring` until its rollback monitor decides, so the canary re-reads the action every
15 seconds until the verdict is final (a verification of `success`, `revert`, `failed` or
`unverifiable`, or a settled outcome; a rollback still in progress on that database is waited
out). A change that database's own monitor already rolled back is recorded `rolled_back` and is
not rolled back a second time. The wait ends at the action's monitor deadline (its
execution plus its class's monitor cap: `verify.window_max_minutes`, or
`verify.drop_window_hours` for a drop) plus 2 minutes. A verdict still open then halts the
rollout as `verification_pending`, with that database's record `verification_pending`: nothing
is judged or rolled back, and the database's own monitor keeps the change and gives its
verdict. A long monitor cap holds the single canary slot for that long.

How a database's regression is measured (lab-03b). Once a database's verdict is final, the
canary reads that action's verification v2 record from the database's own
`sage.action_outcome` (the acted verdict's `evidence`, or `other_evidence` when v2 runs in
shadow): its effect on the target queries' blocks per call, with its interval. While the
database is measured, the run's databases not changed yet supply a fleet control: each one's
change of the same queries over the same hours (the verdict's window against the as-long
period before it), matched by normalized statement text because queryids differ across
databases, read in parallel. With at least two of them measured, the database's effect is its
own change of the target queries minus the control's median, so a shift the whole fleet sees
the same day does not halt a good change. A database that cannot be read, or ran none of the
queries, is left out of the control by name in the detail and never counts as flat. With
fewer than two, the database's own v2 effect is the number. That number is the instance's
`regression_pct`, compared with `regression_limit_pct`: at the limit the rollout proceeds,
above it it halts and rolls back newest first. With no v2 number (no v2 record, an unmeasured
one, or one that is not an object, which logs a warning), the verdict flag alone decides and
the detail says "flag only". A failed, reverted or unverifiable verdict always halts,
whatever the number. Each instance record keeps `effect_pct`, `control_pct` and
`effect_method` (`v2_fleet_control`, `v2` or `flag_only`), and the database's queue item, when
its action has one, gains a tier-4 (`fleet`) ladder record with the effect. After each
database's verdict the rollout waits `stage_wait_hours` (default 24; 0 for no pause) before it
changes the next database; R6 suggests a full weekly cycle (168), a policy choice. The wait
holds the single canary slot, and a request that ends during it rolls back what was changed.

The source action must have a verification of `success`; an action with no verification is
refused. When the rollout itself fails (a database cannot be read, re-verified or changed, or
the request ends), every database it changed is rolled back, the run is `failed` with
`rollout_error`, and a database whose rollback also fails is recorded `applied_unverified`
with the rollback error. Instance records are `applied`, `not_locally_verified`,
`verification_pending`, `rolled_back`, `rollback_failed` (after a halt),
`applied_unverified` (after an error), `rollback_unavailable` or `failed` (not changed).

A run is recorded as it goes, in `sage.rollout_run` and `sage.rollout_instance` on the
control database: the run's ordered `targets`, then each database's part as it happens,
`applying` before the change, `awaiting_verdict` with the action id once changed, and then
its verdict. A database's record that cannot be written stops the canary before it touches
another database, and rolls back what it changed. A sidecar that runs a canary holds a
session advisory lock for that run on the control database, so a stopped sidecar releases it.
At startup the sidecar marks every run left in `canary` that no session holds as
`interrupted` (halt reason `sidecar_restart`), marks its `applying` and `awaiting_verdict`
databases `interrupted`, and raises one `canary_interrupted` notification (warning) naming
the run and those databases with their action ids, and the databases it had already
changed with a verdict, which keep the change because an interrupted run rolls nothing
back. An interrupted run is final and never
resumed: a change already made stays with that database's own post-action monitor, so check
its action log and keep or roll it back, then start a new canary if the change should still
roll out. With two sidecars on one control database (HA), a run the other sidecar is still
running is left alone, and two sidecars starting at once interrupt each run once.

API, under `/api/v1/sre/autonomy`. Every route takes `?database=<name>`.

The ledger is per database: each database earns its own levels from its own shadow reviews,
live outcomes, game days and safety record, and its own failover cooldown and carry-over
apply. PGIncidentBench reports are about pg_sage itself and are shared by every database of
the deployment. A level stored before the ledger was per database applies to every database
at that level until the database changes it. Only a verified success counts as a live
recovery: an action that is never verified is recorded as `unverified` after 24 hours and
earns nothing.

- Viewers: `GET` the root, `/history`, `/proposals`, `/game-days`, `/rollouts` and
  `/rollouts/{id}`.
- Operators: `POST /evaluate` (returns the proposals created and, for every other pair,
  why not: each unmet check with how to meet it), `/proposals/{id}/reject`, `/downgrade`,
  `/reviews` (accept or reject a concluded or inconclusive investigation, with an optional
  `note` and `actual_root_cause`; it also records the investigation outcome) and
  `/outcomes` (harmful or safety_violation).
- Admins: `POST /proposals/{id}/approve`, `/bench-results`, `/game-days` and `/rollouts`.

The UI page is **Advanced > Earned autonomy**: **Evaluate now** and a **Path to next
level** checklist per family and class. Reviews are recorded with **Accept diagnosis** /
**Reject diagnosis** on a finished investigation in **Cases**. With MCP enabled, agents get
`sre_get_autonomy`, and operators also `sre_downgrade_autonomy`, `sre_review_investigation`
and `sre_evaluate_autonomy`. There is no approval tool. Only a review by a person (UI or
REST) counts as shadow evidence: a review an agent records through MCP is kept, says that
it does not count, and never replaces a person's review of the same investigation.

### Fast elevation (dogfood databases)

pg_sage earns trust before it acts, and by default that takes weeks: an 8-day ramp for SAFE
actions, 31 days for MODERATE ones, a 7-day IO baseline before autonomous index builds, and a
30-day shadow record and 50 verified recoveries before earned autonomy reaches L3. On a
database you are dogfooding you can shorten all of it to hours. Every elevation setting is
configurable, the spec value is the default, and each has a minimum: no setting accepts `0`
to skip its check.

Use this only where you accept the risk:

<!-- fast-elevation-profile:start -->
```yaml
trust:
  ramp_safe_hours: 1              # propose L2 after 1 hour observed (spec: 192)
  ramp_moderate_hours: 4          # propose MODERATE L3 after 4 hours (spec: 744)
verify:
  io_baseline_hours: 2            # learned IO baseline after 2 hours (spec: 7 days)
  drop_window_hours: 2            # verify an index drop over 2 hours (spec: 168)
sre:
  autonomy:
    evaluate_interval_minutes: 5  # propose promotions every 5 minutes (default: 60)
    promotion:
      shadow_window_hours: 4      # 4 hours of shadow reviews (spec: 720)
      shadow_min_reviewed: 3      # 3 reviewed packets in that window (spec: 20)
      min_live_recoveries: 3      # 3 verified L2 recoveries for L3 (spec: 50)
```
<!-- fast-elevation-profile:end -->

The profile lowers time and volume only. The accuracy bar stays at the spec: 80% top-1,
90% precision, 95% accepted packets and 95% Safe Pass. Your own `trust.level`,
`tier3_*`, execution mode and maintenance window still decide what may run at all.

Some limits stay fixed and cannot be configured:
- Actions that cannot be rolled back (`not_reversible`, `forward_fix_only`,
  `application_rollback`, `mitigation_only`, `not_applicable` or undeclared) always wait
  the full 8- or 31-day ramp, however short the configured ramp is.
- Irreversible classes never go above L1, and L4 is never reached.
- The emergency stop always wins, and the policy gate and your trust settings stay the
  outer bound.
- An admin still approves every promotion.
- The downgrade signals still apply. A harmful outcome still demotes the family for
  `safety_window_days`.

Fast elevation is never silent. At startup the sidecar logs a `FAST ELEVATION` WARN line for
each setting below the spec, including a shorter `io_baseline_days`, `safety_window_days`
or `evaluate_interval_minutes`. The autonomy API returns them as `fast_elevation`, and
**Advanced > Earned autonomy** shows a "Fast elevation" badge that lists them. These keys
are YAML-only and need a restart; the config API refuses them.

To approve a promotion quickly, review investigations in **Cases**, press **Evaluate now**
on the Earned autonomy page (or `POST /api/v1/sre/autonomy/evaluate` as an operator), then
approve the pending promotion as an admin (or `GET /api/v1/sre/autonomy/proposals` and `POST
/api/v1/sre/autonomy/proposals/{id}/approve`).

### Ask Sage

Ask Sage answers questions about a database from cited evidence (UI, REST API, MCP
`ask_sage`) with its own daily LLM budget, separate from `llm.token_budget_daily`. It reads;
it can open an investigation or queue a finding for a person's approval, and never executes
or approves. See [Ask Sage](ask-sage.md).

| Key | Default | Meaning |
|---|---|---|
| `ask.enabled` | `true` | Answer questions. Restart to change |
| `ask.daily_tokens_per_database` | `300000` | Daily tokens for one database, all users (0 refuses). Restart to change |
| `ask.daily_tokens_per_user` | `100000` | Daily tokens for one user or MCP token; at most the database's. Restart to change |
| `ask.max_tokens_per_question` | `40000` | Most tokens one question may use (4000-200000). Restart to change |
| `ask.retention_days` | `30` | Days a conversation is kept after its last question (1-3650). Restart to change |
| `ask.max_seconds_per_question` | `60` | Wall clock of one question (15-300; each model call at most 15 s). `POST /api/v1/databases/{db}/ask` gets this plus 5 s, every other API route 30 s; an MCP call keeps its own deadline. A run cut off by its caller after it acted is stored `incomplete` with stop `cancelled`; answers are counted in `pg_sage_ask_answers_total{database,status,stop}`. Restart to change |
| `ask.max_concurrent_runs` | `4` | Asynchronous runs (the dashboard's questions, `POST /api/v1/databases/{db}/ask/runs`) at once per database on one sidecar (1-16). One more is refused with HTTP 429 and `Retry-After`; a second run on one conversation is 409. Each run is bounded by `ask.max_seconds_per_question` and the daily budgets; see [Ask Sage](ask-sage.md#asynchronous-runs). Restart to change |

### Sage Review

Sage Review grades each database against the versioned rubric `pg_sage-rubric/1`,
deterministic and read-only; it writes only `sage.review_run` and `sage.review_grade`. See
[Sage Review](review.md).

| Key | Default | Meaning |
|---|---|---|
| `review.enabled` | `true` | Grade each database once after its first look, then every `review.grade_interval_hours`. Restart to change |
| `review.grade_interval_hours` | `24` | Hours between two grades of one database (1-720). Restart to change |
| `review.window_days` | `90` | Days of history the review generators judge (14-100). The workload pack is the shorter of this and the retained rollups; a shorter pack is reported as partial coverage. Restart to change |
| `review.large_table_bytes` | `10737418240` | Bytes (1 GiB-1 PiB, default 10 GiB) from which the schema-debt generator advises range-partitioning an unpartitioned table that a date or timestamp key orders in time. Advice only. Restart to change |
| `review.model_pass` | `auto` | The model pass that ranks, attacks and links a review's candidates and writes its report ([review.md](review.md#model-pass)): `auto` runs it when a model is usable, `on` makes a missing model a failed pass, `off` keeps the deterministic ranking and templated text. The model reads only the pack's model tier and never writes SQL that runs |
| `review.model_runs` | `3` | Independent model runs combined by `review.consensus`; odd, 1-5. `1` applies no consensus and the report says "single run" |
| `review.consensus` | `majority` | `majority` keeps a candidate most runs keep, at its median rank; `refute` has a second run refute the first run's picks with cited evidence |
| `review.hash_identifiers` | `false` | Replace schema, table, index and column names with keyed pseudonyms in what the review model reads; the report shows the real names. Needs `encryption_key`. Decision D3 (OWNER ACTION) may flip the default |

### Forecaster

The forecaster (`forecaster.*`, see `sidecar/config.example.yaml`) fits daily trends
over the `lookback_days` closed calendar days before today and needs 7 days of data
for each trend (8 closed days for the checkpoint rate, which is a difference between
days).

- Closed days only. Every daily rule leaves today's partial day out (the history
  database's session time zone), so a rule gives the same answer at 00:10 and at
  23:50, and a daily finding lags by up to a day. The window starts at a day's start,
  so its oldest day is whole whatever the hour.
- Coverage. A day counts when its snapshots cover at least 80% of its 24 hours:
  snapshots x `collector.interval_seconds`, capped by the hours from the day's first
  to its last snapshot (a burst does not cover a day). The interval used is at least
  the median spacing of the window's snapshots, so raising the interval does not
  uncover every day. An uncovered day is unknown to every rule, as a day without a
  measured input is; a day without any snapshot is missing, and no rate or week spans
  it silently.
- Fit and span. `forecast_disk_growth` and `forecast_connection_saturation` project
  only when the linear fit explains at least `forecaster.min_r_squared` of the series
  (default `0.5`; `0` means the default) and the series spans at least 7 calendar
  days. A noisy trend gives no finding (and resolves an open one). Their details carry
  `r_squared`, `span_days` (calendar days from the first to the newest day) and
  `covered_days` (the days fitted); their titles carry the projected date, a calendar
  date of the session time zone (none past about a century).

- Days without a measured database size (`pg_database_size` had not yet succeeded,
  timed out, or is refused by the provider) are left out of the disk growth trend
  (`forecast_disk_growth`); they are not read as a size of 0. With 7 or more days of
  history but fewer than 7 measured sizes, the disk trend is not evaluated: no disk
  finding opens, and an open one is neither refreshed nor resolved until 7 measured
  days exist again. The connection, cache and checkpoint trends still run.
- `forecast_disk_growth` warns when the database grows faster than
  `forecaster.disk_warn_growth_gb_day` (default `5`). Its detail carries the disk capacity
  it was judged against: `capacity_source` is `provider_telemetry` (a fresh managed-cloud
  measurement), `declared` (`forecaster.disk_capacity_bytes`), `undeclared` (neither; the
  detail then names `config_key: forecaster.disk_capacity_bytes`) or `capacity_unbounded`
  (the provider grows the storage itself; a declared value is not used in its place). With
  a known capacity it adds `capacity_bytes` and `days_until_full`
  (`(capacity - current size) / daily growth`, never below 0). The size and growth are this
  database's alone, not other databases' or WAL, so on a shared disk the true time to full
  is shorter: the disk runway judges the whole disk. On a managed database the
  detail also names `managed_storage_runway` as the authority inside 7 days.
- Days without backend counts or `max_connections` are left out of the connection
  trend. `forecast_connection_saturation` counts client backends (`client_backends`:
  the cluster's `backend_type = 'client backend'` sessions, so background workers,
  the checkpointer and autovacuum are not load) against the slots a client can take:
  `max_connections` less `reserved_connections` in the system snapshot
  (`superuser_reserved_connections`, plus `reserved_connections` on PG 16+; managed
  providers expose both). It warns at `forecaster.connection_warn_pct` of those slots.
  Snapshots written before this version lack both counts, so until 7 days carry them
  the rule keeps the old basis, every backend against `max_connections`, and the
  detail says `limit_basis: legacy` (otherwise `client_backends`, with
  `usable_connections` and `reserved_connections`). In fleet mode each runtime
  forecasts on its own; the counts are cluster-wide.
- `forecast_query_volume` compares the calls of the 7 closed days ending yesterday
  with the 7 before (warning above 50%, critical above 100%). All 14 days must be
  covered and consecutive, and the earlier week must have calls; otherwise the category
  is not evaluated, so an outage or a fresh install does not read as growth, and an
  open finding is neither refreshed nor resolved until two complete weeks exist again.
  Calls counted across midnight or a collector gap are shared between the days by
  hours.
- The checkpoint trend (`forecast_checkpoint_pressure`) counts requested checkpoints
  only: WAL-triggered ones (WAL reached `max_wal_size` before `checkpoint_timeout`) and
  explicit ones (`CHECKPOINT`, base backups). Timed checkpoints are counted by
  PostgreSQL every `checkpoint_timeout` even when skipped on an idle server (288 a day
  at the default 5 minutes), so they are not pressure. The rate of a day is the growth
  of `checkpoints_req` (PG 14-16, `pg_stat_bgwriter`) or `num_requested` (PG 17+,
  `pg_stat_checkpointer`) between the last snapshots of two closed days, divided by the
  hours between them, so a missing day does not inflate it and the current, partial
  day never moves the result. A day whose counters drop (a stats reset) is skipped, not
  read as 0. The finding warns when the EWMA of those hourly rates is at or above
  `forecaster.checkpoint_requested_per_hour` (default `2`, above `0` and at most
  `3600`); its detail carries the requested share of all checkpoints, and the advice
  is to raise `max_wal_size` (with `advisor.enabled`, the configuration advisor reviews
  it in its `wal_tuning` findings).
- The checkpoint trend needs 7 usable day-to-day rates (8 closed days). Snapshots
  written before this version carry only the total count, so after an upgrade the
  category is not evaluated until 8 closed days of new snapshots exist: an open
  checkpoint finding is neither refreshed nor resolved until then, and resolves when
  the new rule first evaluates without warning. On PG 18+ the snapshot also records
  `num_done` (checkpoints performed) as `checkpoints_done`. In fleet mode the counters
  are cluster-wide, so every database on one cluster reports the same rate.
- The cache trend (`forecast_cache_pressure`) judges each closed day's own hit ratio,
  not the lifetime ratio of `pg_stat_database` (which barely moves when a working set
  outgrows `shared_buffers` today). A day's ratio is the `blks_hit` share of the
  `blks_hit + blks_read` growth between consecutive snapshots that day, summed over the
  day. An interval across a stats reset adds nothing, a day with fewer than
  `forecaster.cache_min_blocks_per_day` block accesses (default `100000`, at least `1`)
  is left out as too thin to judge, and the current, partial day never counts; a
  left-out day is unknown, never read as 0 or -100%. With 7 usable days the finding
  warns "low" when the newest usable day's ratio is below
  `forecaster.cache_warn_threshold` (a fraction, default `0.95`), with an EWMA of the
  usable days beside it, and "declining" when the fitted drop across them is at least
  1 percentage point with R² above 0.5. Its detail names the judged day, its block
  accesses, the 5 tables with the most `heap_blks_read` growth over that day (from the
  table snapshots, which record `heap_blks_read` from this version on) and
  `shared_buffers`. Until then "low" meant the lifetime ratio's EWMA; it now means the
  newest day. Snapshots written before this version carry no interval counters, so after
  an upgrade the category is not evaluated until 7 usable days of new snapshots exist:
  an open cache finding is neither refreshed nor resolved until then. In fleet mode each
  database is judged on its own counters.
- Sequences are forecast by the runway monitor (`sre.runways.sequence_horizon_days`
  and `sequence_critical_days`, see
  [Runways and pre-incident investigations](#runways-and-pre-incident-investigations)).
  The retired `forecaster.sequence_warn_days` and `forecaster.sequence_critical_days`
  still load, with a warning each, and have no effect; remove them.
- `forecaster.min_data_points` and `forecaster.alert_horizons` are retired: they fed only
  the v0.9 growth forecaster, which never ran. They still load, with a warning each, and
  have no effect; remove them. `forecaster.min_r_squared` stays: it gates the disk
  growth and connection saturation projections (above).

#### Disk forecasts

Three forecasts look at disk; each answers a different question.

| Forecast | Answers | Series | Capacity | Where |
|---|---|---|---|---|
| `forecast_disk_growth` | Is this database growing unusually fast, and when is the disk full at that rate? | daily `pg_database_size` of the database (snapshots) | provider, else `forecaster.disk_capacity_bytes`; none gives `undeclared` | finding; `GET /api/v1/forecasts/growth` (cluster database bytes from the runway samples, with `growth_rate_bytes_day`, `r2`, the same capacity fields) |
| Disk runway (`forecast_wal_runway`, `disk`) | When do databases plus WAL reach the capacity, early enough to investigate? | runway samples (`disk_used`) | provider, else `forecaster.disk_capacity_bytes`; none: no disk runway | finding and pre-incident investigation ([Runways](#runways-and-pre-incident-investigations)) |
| `managed_storage_runway` | Does managed storage run out within a week? | the provider's free-storage trend | the provider's | finding on managed clouds; the authority inside 7 days |

`GET /api/v1/forecasts/growth?database=<name>&days=<1-365>` fits the newest epoch of each
database's runway `database_bytes` series (`regr_slope`, `regr_r2`) over the window
(default 90 days) and returns nothing for a database with fewer than two samples. It used
to read `sage.size_history`, which nothing wrote, so it always returned `[]`; that table is
dropped on upgrade.

### Log-based RCA

With `rca.enabled` (the default), pg_sage tails the server's csvlog or jsonlog and opens
incidents from it (crashes, WAL gaps, corruption, wraparound, disk full and the other
signals below). `logwatch.enabled` takes three values:

- unset or `auto` (the default): log RCA starts by itself once a readable source is
  detected: a jsonlog or csvlog file in the server's `log_directory` that the sidecar can
  read, or a configured provider log sink (Supabase). Until then the source is `disabled`
  with reason `auto: <what is missing>`, one INFO line is logged (not a WARN per retry),
  and the background retry keeps probing, so fixing `log_destination` or mounting the
  log directory activates it without a restart.
- `true`: always wanted; an unreadable log is `unavailable` and logged as a WARN.
- `false`: never read the server log.

Set `logwatch.enabled: false` to keep log RCA off.
`logwatch.slow_query_enabled` stays opt-in, because it reads statement text. Migration log
detection (`migration.log_detection`) reads the same watcher and stays opt-in. The
override API takes `true`, `false` or `auto`, and `GET /api/v1/deployment/status` reports
`readers.logwatch_mode`.

**Requirements.** PostgreSQL 14-18. The server must write log files pg_sage can parse:

| Setting | Needed value | Why |
|---|---|---|
| `log_destination` | includes `jsonlog` (PostgreSQL 15+) or `csvlog` (14+), e.g. `'stderr,jsonlog'` | pg_sage parses only these formats; plain `stderr` is not read |
| `logging_collector` | `on` (restart to change) | csvlog and jsonlog files are written only by the collector |
| `log_directory` | readable by the sidecar | auto-detected from `pg_settings` (relative paths are under `data_directory`); mount it read-only into the sidecar at the same path, or set `logwatch.log_directory` |
| `lc_messages` | `'C'` or English | the text-matched signals need English messages; SQLSTATE signals work in any language |
| `log_temp_files` | `>= 0`, e.g. `'10MB'` | without it `log_temp_file_created` never fires |
| `log_min_duration_statement` | `>= 0` | only with `logwatch.slow_query_enabled`, for `log_slow_query` |

`log_lock_waits` and `log_autovacuum_min_duration` feed no signal today; leaving them off
loses nothing. Detection only reads `pg_settings` (read-only, bounded by a timeout).

| Key | Default | Meaning |
|---|---|---|
| `logwatch.enabled` | auto | `auto` (unset): tail the server log once a readable source is detected; `true`: always; `false`: never. Needs `rca.enabled` (or migration log detection) |
| `logwatch.log_directory` | auto | Log directory as the sidecar sees it; blank reads `log_directory` from `pg_settings` |
| `logwatch.format` | auto | `jsonlog` or `csvlog`; blank reads `log_destination` |
| `logwatch.poll_interval_ms` | `1000` | How often the tailer polls the log file (it only polls) |
| `logwatch.dedup_window_seconds` | `60` | Suppress repeats of one signal within this window |
| `logwatch.max_line_len_bytes` | `65536` | Drop longer records. An auto_explain plan record (`duration: ... plan:`) may reach 1 MiB, the size its parser accepts. Every drop is counted (`reason="oversized_record"`) |
| `logwatch.temp_file_min_bytes` | `10485760` | Smallest temporary file that raises `log_temp_file_created` |
| `logwatch.max_lines_per_cycle` | `10000` | Lines classified per drain; critical lines are never skipped |
| `logwatch.exclude_applications` | `[pg_sage]` | `application_name` values excluded from timeout and deadlock signals |
| `logwatch.slow_query_enabled` | `false` | Parse `log_min_duration_statement` output as `log_slow_query` |

**Log source status.** `GET /api/v1/databases` returns `status.log_source` for every
database: `state` is `active` (tailing; with `format`, `file`, `last_line_at`,
`parse_errors` and `dropped_lines`), `unavailable` (wanted but unreadable; `reason` names the
setting at fault, for example `log_destination is "stderr"` or `logging_collector is "off"`,
and `next_retry_at` says when pg_sage tries again) or `disabled` (`reason` names the switch,
for example `logwatch.enabled is false`; in auto mode, `auto: ` and the setting at fault,
with `next_retry_at`, because the retry keeps probing). `hints` lists settings that keep a signal silent.
An unavailable source is retried in the background, first after 30 s and then doubling to
every 10 minutes, until it starts or the sidecar stops; databases that started while it was
unavailable receive its signals once it is active, without a restart. In fleet mode one
watcher serves each cluster (host:port), so every database on a cluster shows that cluster's
status. The First Look reports the `log_rca` capability with the exact steps for the
provider, and an info item when the log cannot be read.

**Cluster events in fleet mode.** A log line that names no database (PANIC, disk full,
archiver, checkpoints) and the cluster-wide signals `connections_high`, `wal_growth_spike` and
`replication_lag_increasing` concern the whole cluster, so pg_sage raises them once, on the
cluster owner: the database with the lowest name among those on that host:port. Other
databases of the cluster do not repeat them. Cluster log signals carry `scope` `cluster`,
`cluster_key` and `databases` in their metrics. When the owner is removed at runtime,
the next database by name becomes the owner and receives the cluster events it had not yet
analyzed. A line that names a database still goes only to that database. In single-database
mode the one database is the owner. `connections_high` counts client backends (not
background workers) against `max_connections`. The cluster is named by the host:port each
database connects to. Two names for one server (`localhost` and `127.0.0.1`) count as two
clusters and repeat the cluster events. Databases of several servers reached through one
pooler host:port count as one cluster, so only the owner's server raises these signals:
connect each server's databases directly, or through a host:port of their own.

**Managed services.** Their server log files are not reachable from the sidecar. RDS and
Aurora send logs to CloudWatch Logs, Cloud SQL and AlloyDB to Cloud Logging, and Azure to
Azure Monitor; pg_sage reads none of these, so log-based RCA is unavailable there. Supabase
logs arrive through the Management API when `SAGE_SUPABASE_OBSERVABILITY_TOKEN` is set (the
database's `log_source` is then `active` with `format` `supabase_api`), and Lakebase logs
through the opt-in `lakebase_observability` reader.

**Statement text.** Log records can carry SQL. Before log signals reach an LLM prompt,
query text has its literals redacted and comments stripped, quoted values in messages are
replaced with `"?"`, and the `detail`, `user` and `cluster_key` fields are dropped.

| Signal | Matched on | Severity |
|---|---|---|
| `log_deadlock_detected` | SQLSTATE 40P01 | critical |
| `log_connection_refused` | SQLSTATE 53300 (too many connections) | critical |
| `log_out_of_memory` | SQLSTATE 53200 | critical |
| `log_disk_full` | SQLSTATE 53100 | critical |
| `log_archive_failed` | "archive command failed" | critical |
| `log_txid_wraparound_warning` | "must be vacuumed within" | critical |
| `log_temp_file_created` | "temporary file" (needs `log_temp_files`) | warning |
| `log_checkpoint_too_frequent` | "checkpoints are occurring too frequently" | warning |
| `log_lock_timeout` | SQLSTATE 55P03 | warning |
| `log_statement_timeout` | SQLSTATE 57014 with "statement timeout" (or any 57014 text that is not English: then `message_unclassified` is set) | warning |
| `log_statement_cancelled` | SQLSTATE 57014 with "due to user request" (`pg_cancel_backend`, a client cancel) | info; opens no incident |
| `log_replication_conflict` | "conflict with recovery" | warning |
| `log_autovacuum_cancel` | "canceling autovacuum task" | warning |
| `log_authentication_failure` | FATAL authentication failures | warning |
| `log_slow_query` | "duration:" (opt-in, `logwatch.slow_query_enabled`) | info |

The signals below need the server process that logs them and are matched as follows. The
text-matched ones need `lc_messages = 'C'` or English: when `lc_messages` is something else,
pg_sage logs a warning naming the setting when the log watcher starts, and lists it in
`log_source.hints`.

| Signal | How it is matched | Incident |
|---|---|---|
| `log_panic_server_crash` | PANIC, or the postmaster's `... (PID n) was terminated by signal n` | critical; records the crashed PID and the signal |
| `log_wal_segment_removed` | `requested WAL segment <name> has already been removed` (walsender or walreceiver) | critical; records the segment |
| `log_replication_slot_inactive` | `invalidating [obsolete replication] slot "<name>"` at LOG | warning; records the slot |
| `log_data_corruption` | SQLSTATE XX001, XX002 (and any unknown XX code) | critical |
| `log_internal_error` | SQLSTATE XX000 (for example "cache lookup failed") | warning, not corruption |
| `log_wraparound_outage` | SQLSTATE 54000 with "is not accepting commands ... to avoid wraparound data loss" | critical |
| `log_multixact_wraparound_warning` | "must be vacuumed before n more MultiXactIds / multixact members are used" | critical |
| `log_multixact_wraparound_outage` | SQLSTATE 54000 with the MultiXactId stop-limit or `multixact "members" limit exceeded` text | critical |

**Counts, participants and flapping.** The classifier still emits the first line of a signal at
once and suppresses repeats within `logwatch.dedup_window_seconds`, but it counts them: each
drain reports a follow-up signal of the same id with `occurrence_count`, `affected_pids` (at
most 20; for a deadlock, every process its DETAIL names, never the statements) and
`first_seen`/`last_seen` (RFC 3339). The incident keeps them as `log_occurrences` (`count`,
`pids`, `first_seen`, `last_seen`, stored in `sage.incidents.log_occurrences`), and the
deadlock evidence names the PIDs and the count. A provider log sink reports a batch's repeats
when its next batch arrives.

**Deadlock wait cycle.** A deadlock's DETAIL also names, per process, the lock it waited for,
the process that blocked it and (in the server log) the statement it ran. pg_sage reads at most
10 participants into the signal and keeps the newest deadlock window's set in
`log_occurrences.participants`: `pid`, `wait_mode`, `wait_object` (PostgreSQL's wording, OIDs
kept, for example `transaction 745` or `tuple (0,1) of relation 16384 of database 5`),
`blocked_by`, `relation` (the relation the CONTEXT names, on the detecting process only),
`fingerprint` and, only when `rca.deadlock_statement_text` is `redacted`, `statement`. A
statement is clipped to 2,000 bytes, then its comments are removed and its string, dollar-quoted
and numeric constants are replaced by `?`, so one statement shape has one fingerprint whatever its values and no value is
stored; the default `fingerprint` mode stores no statement text at all. The incident's chain
gains a "Deadlock wait cycle" link with one line per participant (`pid A waits for ShareLock on
transaction 745 held by pid B; statement fp=...`). DETAIL lines pg_sage cannot read keep the
PID-only evidence and are counted as `participants_unparsed`; a server with a non-English
`lc_messages` (flagged by the locale check) gives no participants. Provider log sinks without
a CONTEXT field keep the OID only. The tier-2 LLM prompt never sees the raw DETAIL, and sees
statement text only in `redacted` mode. `redacted` is experimental until the owner's privacy
review of these rules.

A log incident auto-resolves after `rca.log_quiet_minutes`
without an occurrence, and a recurrence within `rca.recurrence_renotify_hours` of its
predecessor's resolution, at no higher severity, is linked but not notified again. The lock
graph and lock chain probes investigate deadlocks, connection saturation refused connections,
the archiver archive failures, the wraparound probe wraparound warnings and the replication
slot probe removed WAL segments.

When a log storm outruns the watcher, it drops lines before classifying them: the line queue holds 10,000 unread lines, and `logwatch.max_lines_per_cycle` caps the lines classified per drain. Both evict ordinary lines first and never skip a critical one (PANIC or FATAL, or SQLSTATE class 53, 57P or 58, XX001 or XX002); only a queue full of critical lines loses its oldest. A drain that dropped lines emits the `log_watcher_overloaded` signal (a warning incident, "Log watcher overloaded", with `dropped_lines`, `queue_overflow_lines` and `classifier_cap_lines`), and Prometheus counts the losses in `pg_sage_logwatch_dropped_lines_total{cluster,reason}` (`reason` is `queue_overflow` or `classifier_cap`, or `oversized_record` for a record over its size limit). The source status's `dropped_lines` counts all three.

A client's `RAISE` of the same text does not fire the crash, WAL-gap or slot signals: those
need the server process that really logs them (`backend_type` in csvlog and jsonlog; without
it, a line with no user and no database counts as the postmaster's). Managed services whose
logs arrive through a provider sink use the same classifier.

The patterns are tested against the server's own log records for PostgreSQL 14-18, kept
under `sidecar/internal/logwatch/testdata/`. To cover a new major, run
`python scripts/capture_logwatch_corpus.py --majors <n>` (it needs Docker and a
`pgvector/pgvector:<version>-pg<n>` image) and then `go test ./internal/logwatch/`.

### Alerting

`alerting.*` sends new findings to Slack, PagerDuty and generic webhooks by severity. When
`alerting.enabled` is true the block is checked at startup, and a sidecar whose routes would
send nothing refuses to start with an error naming the key (for example
`alerting.routes[0].channels[0] "Slack" is not a configured channel ... did you mean "slack"?`).
The Slack URL and PagerDuty key may instead be saved from the Settings page; they are
checked once those saved settings are applied, still before the sidecar starts work.
A disabled block is not checked. In fleet mode the one shared block is checked once.

| Key | Default | Meaning |
|---|---|---|
| `alerting.enabled` | `false` | Send alerts. Needs at least one route that reaches a usable channel |
| `alerting.engine` | `notify` | Which engine delivers the block. `notify` reconciles it into notification channels and rules (below) and delivers through the notification outbox with retries; `legacy` runs the old per-database alerting manager instead, removes at startup the `config:` channels and rules a notify run left (so a finding is not posted twice) and logs a startup warning. `legacy` is the escape hatch for one release and is removed by alerting-06d. Any other value refuses startup naming the key, whether or not alerting is enabled. Takes effect on restart |
| `alerting.check_interval_seconds` | `60` | How often the `legacy` engine reads new findings; the `notify` engine does not use it |
| `alerting.cooldown_minutes` | `15` | Raises the per-severity cooldown floors: critical 5 min, warning 30 min, info 6 h. A value below a floor leaves that floor |
| `alerting.repeat_minutes` | `critical: 240`, `warning: 1440`, `info: 0` | Minutes between reminders of a finding that stays open, per severity; `0` never reminds. A missing severity keeps its default. A negative value, an unknown severity, more than `525600` (a year), or a value below that severity's cooldown (above) refuses startup naming the key |
| `alerting.quiet_hours_start` / `alerting.quiet_hours_end` | empty | `HH:MM` (00:00-23:59); set both or neither. The window may cross midnight. Warning and info alerts are deferred, never dropped, until it ends; critical alerts bypass it |
| `alerting.timezone` | `UTC` (empty) | IANA zone for quiet hours, such as `America/Chicago`; an unknown zone refuses startup |
| `alerting.slack_webhook_url` | empty | Slack incoming webhook; required by a route naming `slack`. Must pass the notification target policy (below) |
| `alerting.pagerduty_routing_key` | empty | PagerDuty Events v2 routing key; required by a route naming `pagerduty` |
| `alerting.webhooks[].name` | | Unique, non-empty name a route uses to reach the webhook |
| `alerting.webhooks[].url` | | Where the JSON alert is posted; required. Must pass the notification target policy (below) |
| `alerting.webhooks[].headers` | | Extra request headers; use `${NAME}` for secrets |
| `alerting.routes[].severity` | | `critical`, `warning` or `info` (lower case) |
| `alerting.routes[].channels` | | `slack`, `pagerduty`, or a webhook as `webhook:<name>` or plain `<name>`. Names are case-sensitive; `slack` and `pagerduty` always mean the built-in channels |

```yaml
alerting:
  enabled: true
  slack_webhook_url: ${SAGE_SLACK_WEBHOOK}
  webhooks:
    - name: myapp
      url: https://hooks.example.com/pg-sage
  routes:
    - severity: critical
      channels: [slack, myapp]      # myapp is the same as webhook:myapp
    - severity: warning
      channels: [slack]
```

A finding is alerted once when it opens. While it stays open it is throttled, and a
reminder goes out each `alerting.repeat_minutes` for its severity: by default every 4 hours
for critical, once a day for warning, and never for info. Slack heads a reminder "Still open
for 3h20m (seen 21 times)", webhooks get `"reminder": true` and `open_for_seconds`, and
PagerDuty re-triggers with the same dedup key, which it folds into the open incident. A
finding that escalates (warning to critical) is sent at once. A finding that resolves and
reopens, as a new finding or as the same one when an action did not fix it, is alerted again
once the cooldown has passed, so a flapping check is not re-sent every cycle. Reminders are logged in `sage.alert_log` as `sent`, and after a restart the
throttle reads that log far enough back to keep the schedule. `repeat_minutes`,
`cooldown_minutes`, `quiet_hours_start`, `quiet_hours_end` and `timezone` apply without a
restart when changed through the settings API: the next decision of every alerting manager
and of the notification outbox uses them, and alerts already open stay throttled. An unknown
timezone or an invalid quiet-hours value is refused with an error naming the key. With the
`notify` engine, `enabled`, `routes`, `webhooks`, `slack_webhook_url` and
`pagerduty_routing_key` also apply without a restart (they are reconciled, below); with the
`legacy` engine such a change is refused with an error that asks for a restart.
`engine` and `check_interval_seconds` take effect on restart.

**Reconciled into notification channels (`engine: notify`).** At startup and on every live
change, the block is written to the notification tables of the control database (the meta
database in meta mode, else the primary): one channel per usable target, named
`config:slack:slack`, `config:pagerduty:pagerduty` and `config:webhook:<name>`, and for each
routed channel one `finding_opened` and one `finding_resolved` rule whose `min_severity` is
the lowest severity any route sends to that channel. Notification rules match "at least
`min_severity`", so a channel routed only `warning` also receives `critical` findings, and a
finding is posted once per channel however many routes name it. Secrets are sealed like those
of channels created in the UI, and a target refused by the target policy builds no channel
(the log names its key). These rows are marked `managed_by = config`: the Notifications page
shows them with a "From config file" badge and without edit or delete, and the API answers
`409 {"error":"managed_by_config"}` to a change; channel names starting with `config:` are
reserved. The reconciler changes only its own rows, compares a hash of each so an unchanged
block writes nothing, and runs in one transaction under an advisory lock, so two sidecars on
one control database converge once; a failed reconcile writes nothing and refuses a live
change. Removing the block (or `enabled: false`) deletes the config rows; channels and rules
created in the UI are never touched. PagerDuty pages the legacy engine opened before the
switch are resolved under their legacy key (`<database>:<category>:<object>`, read from
`sage.alert_log`) when their finding resolves; new pages use the notification key.

```yaml
alerting:
  repeat_minutes:
    critical: 240                # every 4 hours while open
    warning: 1440                # once a day
    info: 0                      # never
```

A severity with no route is not sent. A hot-reloaded config file is checked the same way, and
an invalid one is refused while the previous config keeps running. Values changed through the
settings API are not checked against the routes, so an alerting change there never blocks an
unrelated setting; if a route channel then has no URL or key, routing logs a warning naming it.

Alerting connects through the notification target policy, like the notification channels
(`notification_policy.allow_private_targets`). The Slack URL and every webhook URL must be
`https` on a public host: plain `http`, loopback, RFC 1918, ULA and CGNAT addresses are refused
unless `notification_policy.allow_private_targets: true` (set it for an on-prem relay), and
link-local addresses (including the `169.254.169.254` cloud metadata endpoint), metadata host
names, multicast and unspecified addresses are always refused. When alerting is enabled, a
refused URL stops startup and a hot-reloaded file with one is refused, with an error naming
the key but never the URL; a Slack URL saved through the settings API is refused with HTTP 400
and nothing is stored. Every connection, including PagerDuty's, is checked again at connect
time and on each redirect, and goes direct: `HTTPS_PROXY` and `HTTP_PROXY` are not used. In
fleet mode all databases share the one policy.

When an alerted finding resolves, each channel that delivered the alert and is still routed
for the finding's severity gets one resolve notification (PagerDuty closes its incident),
tried for up to 24 hours after the resolution. Each channel is tracked on its own in the Alert
Log (`sage.alert_log`): success writes a `resolved` row; a failure writes a `resolve_error` row
with the channel's error text, is logged at ERROR, and is retried on the next check, up to 5
times per channel. After the fifth failure the ERROR log says it gave up; close that alert in the
channel by hand.

`GET /api/v1/alert-log` returns these rows with each status as written (`sent`, `resolved`,
`error`, `resolve_error` with the channel's error text, and `deferred`, held by quiet hours,
channel `quiet_hours`). A database whose alert log cannot be read is named in
`failed_databases` while the other databases' rows still return; the API answers 503 only when
no selected database can be read. The route is deprecated: it answers with
`Deprecation: true` and a `Link` to `GET /api/v1/notifications/log`, the one notification log,
which the Notifications page's Log tab shows. The dashboard's old `#/alerts` page redirects
there, keeping `?database=`.

The notification log names each delivery's `database` (`unknown` for a fleet-wide event and
for rows written before the upgrade that added the column) and `dedup_key`.
`?database=<name>` lists one database's deliveries (400 for a malformed name, 404 for a
database pg_sage does not monitor; `all` or no parameter lists every database). The read is
open to admins and operators; channels and rules stay admin-only.

### Notification delivery

Notification rules (the Notifications page) are delivered through an outbox in the control
database, `sage.notification_outbox`; in fleet and meta-database mode it is the same database
as the rules. Each matching rule queues one entry, and the sidecar delivers due entries every
5 seconds, each channel on its own so one slow endpoint does not hold up the others. A network
error, a 5xx or a 429 is retried after 30 s, 1 min, 2 min, 4 min and 8 min (6 attempts, at
most 15 min apart). When the answer carries `Retry-After`, the next attempt waits that long
if it is longer than the backoff, at most 1 hour; another 4xx, a target the notification policy refuses and a disabled or
deleted channel are not retried. Every failed attempt is in the Log tab, as `retrying` while
attempts are left and as `error` on the last. Sidecars sharing a control database never
deliver an entry twice. Approval cards, fact cards and Telegram follow-ups are still sent at
once, because their buttons and reply ids are minted at send time.

The finding events (`finding_critical`, `finding_opened`, `finding_resolved`) follow the
`alerting.*` cooldown, quiet hours and repeat intervals, per database, dedup key and channel
(`sage.notification_state`), whether or not `alerting.enabled` is set:

- the first alert of a finding is sent at once; while it stays open it is not sent again,
  except when it escalates (sent at once) or when its `alerting.repeat_minutes` interval has
  passed (one reminder, headed "Still open for ..."; `0` never reminds). A finding that
  resolves and reopens is alerted again once the cooldown since the last alert has passed;
  until then it waits, it is not dropped;
- during `alerting.quiet_hours_*` a warning or info alert waits until the window ends and is
  then sent once; a critical alert is sent at once. A resolve of an alert that is still
  waiting cancels both;
- a resolve goes only to a channel whose alert is open. A `finding_critical` resolve with no
  state (a page sent before this release) is still sent, so PagerDuty closes it under the key
  it was opened with.

Other events (incidents, actions, approvals) are queued and retried, without cooldown or
quiet hours. `finding_opened` uses the key `finding:<database>:<category>:<object>`, and
`finding_critical` keeps `finding_critical:<database>:<category>:<object>`: give a PagerDuty
channel rules for one of the two, not both, or a critical finding opens two incidents. A
change of those alerting values through the settings API applies to the next decision without a
restart. Delivered entries and closed states are deleted after 7 days.

### Notification channel types

A channel's credentials are sealed at rest with `encryption_key`, never read back in full by
the API, and redacted from errors. Every HTTP destination connects through the notification
target policy (`notification_policy.allow_private_targets`).

| Type | Settings | Delivery |
|---|---|---|
| `slack` | `webhook_url` (secret); `interactive`, `signing_secret`, `team_id` for approvals | Incoming webhook message |
| `pagerduty` | `routing_key` (secret); `source`, `component`, `dedup_key_prefix` | Events API v2 trigger and resolve on the dedup key |
| `email` | `smtp_host`, `smtp_port`, `smtp_user`, `smtp_pass` (secret), `from`, `to` | SMTP |
| `telegram` | `bot_token` (secret), `chat_id`, `webhook_secret` (secret) | Bot API message |
| `webhook` | `webhook_url` (secret), `headers` (secret, JSON object) | JSON post of every event |
| `teams` | `webhook_url` (secret, a Teams Workflows "When a Teams webhook request is received" URL, https only); `dashboard_url` (optional link on the card) | Adaptive Card. A card over 27 KB keeps the longest body that fits and ends with "truncated; see pg_sage for the full text". Office 365 connector URLs are retired by Microsoft; use a Workflows URL. |
| `opsgenie` | `api_key` (secret, an API integration key); `region` `us` (default) or `eu` | Alert API v2. The alias is the dedup key (cut to 512 characters plus a hash when longer), so a resolve closes the alert by alias. Priority: critical P1, warning P3, info P5. |
| `jira` | `base_url` (the Jira Cloud site, https, no path), `email`, `api_token` (secret), `project_key`, `issue_type` (default `Task`) | Not an alert destination: a case ticket integration (below). No rule can route events to it, broadcasts skip it, and its Test button answers that it is checked by creating a ticket. |
| `incidentio` | `alert_source_config_id` (the HTTP alert source's id: letters, digits, `_`, `-`); `token` (secret, the source's bearer token) | HTTP alert source event with `deduplication_key` set to the dedup key and `status` `firing` or `resolved`. |

A Teams, Opsgenie or incident.io channel with a missing credential, a bad region, a malformed
source id or a non-https Teams URL is refused when it is saved. An edit that moves a Jira
integration to another `base_url`, or a webhook to another `webhook_url`, while sending the masked
`api_token` or `headers` back is refused: re-enter the secret for the new target, so a stored
secret never follows a moved target. For one alert that opens and
resolves on Opsgenie or incident.io, give the channel rules for the opening event and for its
resolve (for example `finding_opened` and `finding_resolved`, or `incident_detected`,
`incident_escalated` and `incident_resolved`).

#### Case tickets (Jira)

With one enabled `jira` integration, an operator's case card offers **Create Jira ticket**. It
creates one issue in `project_key` (summary `[pg_sage] <database>: <case title>`, the case's
why and why-now, and a link back to the case, labelled `pg_sage`) and stores the link in
`sage.case_tickets` on the control database, keyed by database, case identity and system, so
two operators clicking at once make one issue. A failed create keeps no link and can be
retried. With several enabled Jira integrations the request must name one (`channel_id`).

Every 15 minutes a worker reads back the status of at most 50 tickets (oldest read first) and
shows it on the case. A failed read shows as `ticket_sync: unauthorized` (Jira refused the
token), `not_found`, `integration_missing` (the integration was deleted or disabled; the link
is kept) or `unreachable`, and keeps the last status read. When the case is no longer open
the worker adds one comment to the issue; that comment is the only change pg_sage makes to it.
Ticket status is read-only evidence: closing the issue never resolves the case, approves an
action or changes trust. API: `GET /api/v1/case-tickets?database=` (viewer) lists the links;
`POST /api/v1/case-tickets` (operator) with `database`, `case_id` and optional `case_url` and
`channel_id` creates one. ServiceNow is not supported.

### Retention

| Parameter | Default | Description |
|---|---|---|
| `retention.snapshots_days` | `90` | Days to retain snapshot data, the source the hourly per-query rollup is built and rebuilt from |
| `retention.query_rollup_hourly_days` | `35` | Days to keep the hourly per-query rollup (`sage.query_rollup_hourly`): two weeks for every hour-of-week bucket twice plus the baseline's minimum weeks. An hour is removed only once its UTC day is in the daily tier, or once it is older than `query_rollup_daily_days`. At most `snapshots_days` (a larger value is refused; the default follows a smaller `snapshots_days`). `0` disables. Range 0-3650. |
| `retention.query_rollup_daily_days` | `400` | Days to keep the daily per-query rollup (`sage.query_rollup_daily`) and the hourly coverage rows (`sage.query_rollup_coverage`), so a long window still states its coverage after the hourly rows are gone. At least `query_rollup_hourly_days`. `0` disables. Range 0-3650. |
| `retention.workload_packs_days` | `180` | Days to keep stored workload evidence packs (`sage.workload_packs`), counted from when each pack was built. A pack is never changed once stored; this purge only deletes. A pack that an open plan, or a terminal plan younger than `retention.actions_days`, cites is kept. `0` disables. Range 0-3650. |
| `retention.findings_days` | `180` | Days to retain resolved findings |
| `retention.actions_days` | `365` | Days to retain action log entries; also plans in a terminal status (completed, reverted, rejected, voided, cancelled), counted from their last change. Open plans are kept. |
| `retention.explains_days` | `90` | Days to retain EXPLAIN plan captures |
| `retention.sage_size_warning_pct` | `10` | Raise a `sage_footprint` finding when pg_sage's own tables (the `sage` schema, with TOAST and indexes) exceed this percent of the database size; below 256 MB it is never a finding. `0` disables the check. With `history.store: meta` the snapshots and the query store are not in this schema. |

Catalog snapshots (tables, indexes, sequences, foreign keys, partitions,
queries, `pg_stat_io`, configuration) are stored compactly: a full row (keyframe) at
most every 6 hours, and in between only what changed. Reads return every snapshot
exactly as collected, through `sage.snapshot_data(data, base_id)`. Retention keeps a
keyframe as long as a retained row is built on it, so the oldest kept rows can be up
to 6 hours older than `snapshots_days`. Rows written by earlier versions stay readable
and age out with `snapshots_days`; nothing is rewritten.

The per-query rollup is measured, not estimated: at the default 500-query cap, 28 days of
hourly rows (500 queryids x 24 hours x 28 days, 336,000 rows) with their 14,000 daily rows
and 672 coverage rows take 75.9 MB (79,577,088 bytes, PostgreSQL 17, tables and indexes).
The hourly rows dominate: the 35-day hourly tier is about 1.25 times that figure.
`GET /api/v1/queries/coverage` reports each database's
measured `rollup_bytes` (with `history.store: meta`, its share of the store's tables).

> **Complete reference:** See `sidecar/config.example.yaml` for all
> configuration fields including `safety`, `alerting`, `auto_explain`,
> `forecaster`, `tuner`, `advisor`, and `api` sections.

### Audit chain verification

| Parameter | Default | Description |
|---|---|---|
| `audit.verify_interval_hours` | `24` | Hours between scheduled verifications of the audit hash chains, on the fleet leader, per database. `0` disables them. 0-168. |
| `audit.full_verify_interval_hours` | `168` | Hours between full verifications of each chain. The scheduled runs in between are incremental. `0` verifies every chain in full every time. 0-8760. |
| `audit.prune_below_signed_checkpoint` | `false` | After a clean full verification, delete the chain links below the newest signed checkpoint that every SIEM sink has received. See [Signed checkpoints and pruning](#signed-checkpoints-and-pruning). |

Each clean verification stores a checkpoint in `sage.audit_chain_checkpoint`
of the monitored database: the position and hash of the head it reached.
The table keeps the newest 100 checkpoints per chain, plus the newest full
one. It needs no superuser and behaves the same on PostgreSQL 14-18 and on
managed providers.

- **incremental**: verifies only the links after the newest checkpoint, and
  the rows they name, in one read-only snapshot. It first checks that the
  checkpointed link still carries its hash. An edit of a row that was
  written before the checkpoint, and not changed since, is found by the
  next full pass, not by this one.
- **full**: verifies every link from the first, and every chained row, in
  bounded memory (an ordered pass over the link index, 2000 links at a
  time). It runs when no checkpoint exists, when the interval has passed,
  or on demand.
- **window**: `from_seq`/`to_seq` on the API verifies an explicit range.

Each scheduled run's result is exported as `pg_sage_audit_chain_ok`,
`pg_sage_audit_chain_verified_timestamp_seconds` and
`pg_sage_audit_chain_problems`, and served by `GET /api/v1/audit/status`
(admin); see [Monitoring audit health](security.md#monitoring-audit-health).

`GET /api/v1/audit/verify?database=<name>` (admin) verifies since each
chain's newest checkpoint, or in full when a chain has none. Add
`full=true` to verify the whole chain. Both run under a 5 minute timeout
and answer `504` when they exceed it; verify a large chain in
`from_seq`/`to_seq` windows then.

`checkpoint_mismatch` is the problem kind for a checkpointed link that is
gone, rewritten, or past the chain's head. Incremental and full passes
both report it. A restore of the database takes the checkpoints back with
the chain, so it never causes one. The `audit_chain_broken` finding stays
open until the chain verifies against its checkpoint again. Investigate
first: compare the chain with the SIEM's copy (`audit.siem.sinks`). After
that, delete the chain's rows from `sage.audit_chain_checkpoint` to
re-baseline it. The next full pass then stores a new checkpoint.

If a chain cannot be verified at all (a statement timeout, a lock held on
the audit table, a lost connection), it raises an
`audit_chain_verification_failed` warning on `audit_trail:<chain>`. Its
open `audit_chain_broken` finding stays open. The other chains are still
verified.

#### Signed checkpoints and pruning

When pg_sage has an evidence-signing key (`encryption_key` set, or
`audit.evidence.signing_key_env`; see
[security.md](security.md#evidence-packs)), each clean scheduled
verification also signs the head it reached and appends that signature to
the chain as a **checkpoint link** (op `C`, no row). The signature is
Ed25519 over `pg_sage audit checkpoint v1|<chain>|<seq>|<hash>`, under the
evidence key's id. A head that is itself a checkpoint link is not signed
again, so an idle chain gains no links. The SIEM export sends each
checkpoint link as an `Audit Checkpoint Signed` event, so the SIEM holds a
signed statement of the chain's hash at that position. Full passes, and
`GET /api/v1/audit/verify`, check every checkpoint link signed by the
current key: a bad signature, or a signed link that is gone or rewritten,
is the problem kind `signed_checkpoint`. Checkpoint links signed by another
key id (a rotated key) are skipped.

`audit.prune_below_signed_checkpoint` (default `false`) bounds the link
table, whose links outlive the audit rows retention deletes. After a clean
**full** verification of a chain, pg_sage finds the newest checkpoint link
that every SIEM sink of that chain has received (each sink's cursor is at
or past it), checks its signature and the link it signed, and deletes the
links below the floor. It then appends a **prune link** (op `P`), which the
SIEM receives as `Audit Trail Pruned` (medium severity). The floor is the
signed position, or the first link of a row still in the table when that
is lower, so every row still present keeps all its links and is verified
as before. Nothing is pruned without an evidence-signing key, without a
SIEM sink, or before every sink holds the checkpoint link. A row inserted
below the floor that is gone without a delete or truncate link refuses the
prune (the next full pass reports it as `row_missing`).

A full verification of a pruned chain starts at the floor and reports
`from_checkpoint` (the signed position) and `pruned_below` (the floor). It
rests on the checkpoint's signature, so it needs the key: without it, or
under another key, the chain reports `signed_checkpoint`. Links below the
floor are no longer in the database; their copy is the SIEM's. A full
verification that runs while a prune commits may report a gap at the old
lowest links; the next run is clean.

A prune link is honoured only while `audit.prune_below_signed_checkpoint`
is on, for the scheduled verifications and `GET /api/v1/audit/verify`
alike. With the switch off, a full pass of a chain that holds one starts at
link 1, reports the removed links as a `gap` and the prune link as
`signed_checkpoint`: anyone able to append a link could otherwise write a
prune link and erase signed history without a trace. Turning the switch
off after a prune therefore fails the chain's full passes until it is
turned back on. Changing the evidence-signing key after a prune has the
same effect (the anchoring checkpoint is signed by the old key), so keep
the key, or restore it, for as long as a chain rests on its checkpoints.

### SIEM export delivery

`audit.siem.sinks` ships every audit chain link to each sink as an OCSF
event (see [security.md](security.md#tamper-evident-chains-and-siem-export)).
Each sink delivers every chain of every source (each fleet database, plus
`control`) as its own lane, with its own cursor and backoff:

- One unreachable database, or one failing chain, stops only its own lane.
  The other sources and chains keep exporting every
  `audit.siem.interval_seconds`. A failing lane retries after
  `interval_seconds`, doubled per further failure, at most
  `audit.siem.max_backoff_seconds`. Only a failed export-leader check (a lost
  lease, an unreachable control database) backs the whole sink off.
- A transient failure (a network error, HTTP 401, 403, 404, 429 or 5xx)
  holds the lane's cursor and retries the batch. Nothing is lost.
- A permanent refusal (HTTP 400, 413 or 422, OTLP `rejectedLogRecords`, a
  syslog message over 65507 bytes, an event that cannot be encoded) is
  narrowed by halving the batch down to the single refused event. That event
  is replaced by a stub, and the lane moves on. The stub keeps the event's
  class, type and time, says `status: "Failure"` (severity at least Medium),
  and carries the link's source, chain, seq, op, row id and hashes. It is
  labelled `metadata.labels: ["undeliverable"]` and leaves out the row and
  the query. The event itself stays in the database, and its chain link
  proves it.
- Each stub raises a `siem_event_undeliverable` warning on the source
  database, one per sink and chain (`siem_sink:<sink>/<chain>`), naming the
  sink, source, chain and seq of the latest stub. It never resolves itself:
  resolve it once the SIEM holds the content, or the loss is accepted. If
  the finding cannot be recorded, the cursor holds before the event, and the
  stub is sent again on the next pass (delivery stays at least once).
- If the receiver refuses the stub too, the lane holds before the event and
  shows `poisoned` until the receiver takes one of them.

`GET /api/v1/audit/siem` (admin) lists each sink with `undeliverable` (the
stubs sent) and `lanes`: per source and chain, `cursor`, `head`, `lag`
(head minus cursor at the last attempt), `state` (`ok`, `failing`,
`poisoned`, `regressed`), `consecutive_failures` and `last_error`. A lane
with an empty `chain` is a source whose chains could not be listed (an
unreachable database). PostgreSQL 14-18 behave the same.

### SIEM transport security

Each entry of `audit.siem.sinks` takes these transport keys:

| Key | Applies to | Meaning |
| --- | --- | --- |
| `ca_file` | `https://` http/otlp, syslog `network: tls` | PEM bundle the receiver's certificate must chain to. It replaces the system roots for this sink only (pinned). Without it the sink uses the system roots, as before, so `SSL_CERT_FILE` still applies where Go reads it. |
| `client_cert_file`, `client_key_file` | same | PEM client certificate and key for mutual TLS. Set both or neither. |
| `allow_insecure_http` | `http://` http/otlp | Accept a `token_env` token over plain http. Default `false`. |
| `chains` | every sink | Chains to export; empty means all. Each must be `action_log`, `auth_audit`, `config_audit`, `guard_query_audit`, `pgaudit_events`, `fact_events`, `decision` or `policy_approvals`. A misspelt name stops the config from loading. |

- The files are read once at start; a missing file or one that holds no
  PEM turns SIEM export off, and the startup log names the sink and the
  path. TLS is 1.2 at least. TLS files on `http://`, syslog `tcp` or `udp`
  are refused, not ignored.
- A `token_env` token over `http://` without `allow_insecure_http` logs one
  WARN per sink at start, and `GET /api/v1/audit/siem` lists the sink with
  `warnings: ["token_over_http"]`. The next minor release refuses this
  configuration.
- Delivery is at least once over http, otlp, tcp and tls. Over udp it is
  best effort: a datagram that was written is not known to have arrived,
  and a lost one is not retried. A udp sink logs a WARN at start, and
  every sink's status carries `delivery`: `at_least_once` or
  `best_effort`.
- Over TLS 1.3 a syslog receiver checks the client certificate after the
  handshake. When it asks for one, a new session waits up to 0.5 s (at
  most `timeout_seconds`) for a refusal before the first message, so a
  rejected certificate fails the delivery instead of losing the event.
- In fleet mode only the leader exports. Mount the TLS files on every
  replica that can become leader.

```yaml
audit:
  siem:
    sinks:
      - name: soc
        type: syslog
        network: tls
        address: siem.internal:6514
        ca_file: /etc/pg_sage/siem-ca.pem
        client_cert_file: /etc/pg_sage/siem-client.pem
        client_key_file: /etc/pg_sage/siem-client.key
        chains: [action_log, auth_audit]
```

### Postgres-specialist contract

`specialist.*` configures the versioned investigation contract other agents
call (see [specialist.md](specialist.md)). It is on by default and token-only
(MCP tokens; a session never authenticates it). All keys are read at startup.

| Key | Default | Meaning |
| --- | --- | --- |
| `specialist.enabled` | `true` | Serve `/api/v1/specialist/` and the `specialist_*` MCP tools. |
| `specialist.writes_per_minute` | `30` | Opens and remediation requests per token per minute (1-10000). |
| `specialist.reads_per_minute` | `240` | Status, result and stream calls per token per minute (1-10000). |
| `specialist.max_open_per_identity` | `3` | Live investigations one token may have opened (1-100). |
| `specialist.max_open_total` | `10` | Live investigations all tokens may have opened (1-100). |
| `specialist.max_live_age_hours` | `24` | Hours before an opened investigation that cannot be read stops counting as live (1-168); a readable live one always counts. |
| `specialist.keep_identifiers` | `true` | `false` hashes identifiers in results; secrets and PII are always removed. |
| `specialist.pagerduty.signing_secret` | | PagerDuty webhook subscription secret (required with `services`). |
| `specialist.pagerduty.services` | `[]` | `SERVICE_ID=database[:family]` mappings. |
| `specialist.pagerduty.api_url` | | PagerDuty REST API for result notes (https); empty sends none. |
| `specialist.pagerduty.api_token` | | PagerDuty REST API token for notes. |
| `specialist.pagerduty.from_email` | | `From` header of notes. |
| `specialist.webhook.signing_secret` | | Generic webhook HMAC secret; empty disables the adapter. |
| `specialist.webhook.result_url` | | Where signed results are posted (https, or http to loopback). |
| `specialist.webhook.timestamp_tolerance_seconds` | `300` | Accepted age of a signed webhook request (30-900). |
