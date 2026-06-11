# 07 — Data Model & Supporting Subsystems

Reverse-engineered from the actual Go source under `sidecar/internal`. Cited as
`file:line`. This section covers the `sage.*` schema, the full config surface,
alerting/notification, retention, HA, log-watching, and crypto/sanitization.

---

## 1. The `sage.*` Schema

The schema is bootstrapped in two layers:

1. **Core schema** — `internal/schema` package. `Bootstrap()`
   (`bootstrap.go:49`) acquires a Postgres advisory lock
   (`pg_advisory_lock(hashtext('pg_sage'))`, `bootstrap.go:168`), creates
   `CREATE SCHEMA IF NOT EXISTS sage`, then either runs the full DDL
   (`createFullSchema`, `bootstrap.go:195`) on a fresh install or, for an
   existing schema, ensures each table from `expectedTables` exists
   (`ensureTablesExist`, `bootstrap.go:217`) and runs idempotent migrations
   (`migrationStatements`, `bootstrap.go:259`). Finally it folds in
   `MigrateConfigSchema` (`config_migration.go:31`) and
   `migrateIncidentConstraints` (`incident_migration.go:12`).
2. **AgentDB schema** — `internal/agentdb` package. A separate, self-contained
   set of tables created by `Store.Ensure()` (`agentdb/schema.go:17`) from the
   `schemaStatements` slice (`agentdb/schema.go:180`). Only created when the
   AgentDB subsystem is initialized.

### 1.1 Core `sage.*` tables (21 bootstrapped + 2 migration-created)

`expectedTables` (`bootstrap.go:12`) lists 21 tables. Two more
(`config_audit`, and the `databases` table is in the list) are created by the
config migration. Full table reference:

| # | Table | Purpose | Key columns | DDL |
|---|-------|---------|-------------|-----|
| 1 | `sage.action_log` | Immutable record of every executed Tier‑3 action with rollback metadata & outcome | `id`, `action_type`, `finding_id`, `sql_executed`, `rollback_sql`, `before_state`/`after_state` (jsonb), `outcome` (default `pending`), `approved_by`/`approved_at` (migration `bootstrap.go:519`), `measured_at` | `bootstrap.go:292` |
| 2 | `sage.snapshots` | Collector time-series of raw stats by category | `id`, `collected_at`, `category`, `data` (jsonb) | `bootstrap.go:313` |
| 3 | `sage.findings` | Central findings store (Tier‑1 + absorbs schema_lint in v0.11) | `id`, `category`, `severity`, `object_identifier`, `title`, `detail` (jsonb), `recommendation`, `recommended_sql`, `rollback_sql`, `status` (default `open`), `occurrence_count`, `suppressed_until`, `action_log_id` FK, `rule_id`+`impact_score` (lint) | `bootstrap.go:326` |
| 4 | `sage.explain_cache` | auto_explain plan captures keyed by queryid | `id`, `queryid`, `query_text`, `plan_json` (jsonb), `source`, `total_cost`, `execution_time` | `bootstrap.go:364` |
| 5 | `sage.briefings` | Tier‑2 periodic health briefings | `id`, `period_start`/`period_end`, `mode`, `content_text`, `content_json` (jsonb), `llm_used`, `token_count`, `delivery_status` (jsonb) | `bootstrap.go:379` |
| 6 | `sage.config` | Runtime config key/value overrides (hot-reload) | `key` (PK→composite after migration), `value`, `updated_by`, + `database_id`, `updated_by_user_id` (migration) | `bootstrap.go:403` |
| 7 | `sage.alert_log` | Per-channel alert delivery history (internal/alerting) | `id`, `finding_id` FK, `severity`, `channel`, `dedup_key`, `status` (default `sent`), `error_message` | `bootstrap.go:412` |
| 8 | `sage.query_hints` | pg_hint_plan hint lifecycle + rewrite suggestions | `id`, `queryid`, `hint_plan_id`, `hint_text`, `symptom`, `before_cost`/`after_cost`, `status` (default `active`), `suggested_rewrite`, `rewrite_rationale`, `calls_at_last_check`, `last_revalidated_at` | `bootstrap.go:429` |
| 9 | `sage.users` | Local auth users / OAuth identities | `id`, `email` (unique), `password` (nullable after OAuth migration), `role` (default `viewer`), `oauth_provider`, `last_login` | `bootstrap.go:452` |
| 10 | `sage.sessions` | Login session tokens | `id` (uuid), `user_id` FK (ON DELETE CASCADE), `expires_at` | `bootstrap.go:463` |
| 11 | `sage.databases` | Fleet-mode registry of monitored targets; passwords encrypted at rest | `id`, `name` (unique), `host`, `port`, `database_name`, `username`, `password_enc` (bytea), `sslmode`, `trust_level` (default `observation`), `execution_mode` (default `approval`), `tags` (jsonb), `enabled` | `databases.go:11` |
| 12 | `sage.notification_channels` | Configured notification destinations (internal/notify) | `id`, `name` (unique), `type`, `config` (jsonb), `enabled` | `notifications.go:3` |
| 13 | `sage.notification_rules` | Event→channel routing with min severity | `id`, `channel_id` FK, `event`, `min_severity` (default `warning`), `enabled` | `notifications.go:15` |
| 14 | `sage.notification_log` | Notification delivery audit | `id`, `channel_id` FK (ON DELETE SET NULL), `event`, `subject`, `body`, `status` (default `pending`), `error`, `sent_at` | `notifications.go:30` |
| 15 | `sage.action_queue` | Pending/approval action queue with lifecycle, cooldown, guardrails | `id`, `database_id`, `finding_id`, `proposed_sql`, `rollback_sql`, `action_risk`, `status` (default `pending`), `expires_at` (default now()+7d), `identity_key`, `policy_decision`, `guardrails` (jsonb), `attempt_count`, `cooldown_until`, `failure_fingerprint`, `verification_status`, `shadow_toil_minutes`, `action_log_id` FK | `bootstrap.go:476` |
| 16 | `sage.incidents` | RCA incidents (root-cause narratives + causal chains) | `id` (uuid), `severity` (CHECK info/warning/critical), `root_cause`, `causal_chain` (jsonb), `affected_objects`/`signal_ids` (text[]), `recommended_sql`, `action_risk`, `source` (CHECK enum), `confidence`, `database_name`, `occurrence_count`, `last_detected_at`, `escalated_at`, `resolved_at` | `bootstrap.go:719` |
| 17 | `sage.size_history` | Storage growth forecasting time-series | `id`, `metric_type` (CHECK database/table/wal_slot), `object_name`, `size_bytes`, `dead_tuple_pct`, `database_name` | `bootstrap.go:752` |
| 18 | `sage.schema_findings` | Schema-lint findings (v0.10; now read-only/legacy, absorbed by `findings` in v0.11) | `id` (uuid), `rule_id`, `schema_name`/`table_name`/`column_name`/`index_name`, `severity` (CHECK), `category` (CHECK 8-value enum), `description`, `suggestion`, `suggested_sql`, `impact_score`, `suppressed`, `query_count` | `bootstrap.go:769` |
| 19 | `sage.crypto_meta` | Singleton: per-deployment argon2id KDF salt | `id` (PK, CHECK = 1), `kdf_salt` (bytea), `created_at` | `bootstrap.go:394` |
| 20 | `sage.explain_results` | LLM natural-language EXPLAIN cache with TTL | `query_hash`+`database_name` (composite PK), `expires_at`, `plan_json` (jsonb), `explanation` (jsonb) | `bootstrap.go:809` |
| 21 | `sage.health_history` | Per-database fleet health-score time-series | `id`, `database_name`, `health_score`, `findings_open`/`_critical`/`_warning`/`_info`, `actions_total`, `recorded_at` | `bootstrap.go:825` |
| 22 | `sage.config_audit` | Audit trail of `sage.config` mutations | `id`, `key`, `old_value`/`new_value`, `database_id`, `changed_by` FK→users, `changed_at` | `config_migration.go:11` |

> Note: `sage.explain_cache` (#4, auto_explain captures) and
> `sage.explain_results` (#20, LLM NL-EXPLAIN cache) are distinct tables with
> different purposes (`bootstrap.go:806` comment).

#### Notable migration behavior
- **`config` PK→composite** (`config_migration.go:79`): drops the single-column
  `key` PK and creates a composite unique `idx_config_key_db` on
  `(key, COALESCE(database_id, 0))` so per-database overrides coexist with
  global rows.
- **schema_findings → findings absorption** (v0.11): `findings` gains
  `rule_id`/`impact_score` (`bootstrap.go:591`) and a one-time backfill inserts
  `schema_findings` rows as `category = 'schema_lint:<rule_id>'`
  (`bootstrap.go:607`). `schema_findings` is retained as legacy/read-only.
- **incidents CHECK widening** (`incident_migration.go`): severity adds `info`;
  source adds `log_deterministic`, `self_action`, `manual_review_required`,
  `schema_advisor`, `schema_lint`, `n_plus_one`; action_risk adds
  `low/medium/high`.

### 1.2 AgentDB tables (`sage.agent_db_*`, 16 tables)

Created by `agentdb.Store.Ensure()` (`agentdb/schema.go:17`) — a self-contained
provisioning/lifecycle subsystem for agent-owned databases (local Postgres + AWS
RDS / GCP Cloud SQL / Lakebase providers).

| Table | Purpose | DDL |
|-------|---------|-----|
| `sage.agent_identities` | Registered agents (tenant/owner/status) | `schema.go:182` |
| `sage.agent_db_requests` | Provisioning requests (policy decision, idempotency, budget) | `schema.go:194` |
| `sage.agent_db_deployments` | Provisioned deployments (safety_mode, lease, secret_ref, connection_info) | `schema.go:223` |
| `sage.agent_db_provider_configs` | Per-provider enable/settings | `schema.go:279` |
| `sage.agent_db_creation_receipts` | Provider resource creation receipts | `schema.go:287` |
| `sage.agent_db_terraform_templates` | Terraform template store + policy findings | `schema.go:300` |
| `sage.agent_db_blueprints` | LLM-generated provisioning blueprints | `schema.go:316` |
| `sage.agent_db_size_profiles` | CPU/mem/storage size profiles (seeded defaults) | `schema.go:342` |
| `sage.agent_db_pings` | Liveness/metrics pings per deployment | `schema.go:357` |
| `sage.agent_db_ping_tokens` | Hashed ping auth tokens (rotation) | `schema.go:367` |
| `sage.agent_db_ping_token_failures` | Failed ping-token attempts | `schema.go:387` |
| `sage.agent_db_recommendations` | Tuning/index recommendations for agent DBs | `schema.go:396` |
| `sage.agent_db_cost_samples` | Cost telemetry samples | `schema.go:424` |
| `sage.agent_db_backups` | Backup records (verify/restore-verify) | `schema.go:437` |
| `sage.agent_db_tuning_hints` | Per-deployment tuning hints | `schema.go:451` |
| `sage.agent_db_provision_attempts` | Provision runner attempts (dry_run/live, stdout/stderr) | `schema.go:465` |
| `sage.agent_db_audit` | AgentDB event audit log | `schema.go:482` |
| `sage.agent_db_deploy_requests` | DDL/migration deploy requests with gate results | `schema.go:491` |

> The `internal/cases` package (incident/query-hint projectors, vacuum
> autopilot, shadow execution) defines **no tables of its own** — it projects
> onto `findings`, `incidents`, `query_hints`, and `action_queue`.

---

## 2. Config Reference (`internal/config`)

Top-level `Config` struct (`config.go:63`). Load precedence: **CLI > env > YAML >
defaults** (`Load`, `config.go:498`). Defaults live in `defaults.go`; the
`newDefaults()` constructor (`config.go:674`) wires them in. Standalone mode
auto-synthesizes `Databases[0]` from the legacy `postgres` block (`normalize`,
`fleet.go:89`).

### 2.1 Top-level blocks

| Block | YAML key | Struct | Source |
|-------|----------|--------|--------|
| Mode | `mode` | `string` (extension/standalone/fleet, default `extension`) | `config.go:64` |
| Postgres | `postgres` | `PostgresConfig` | `config.go:111` |
| Collector | `collector` | `CollectorConfig` | `config.go:139` |
| Analyzer | `analyzer` | `AnalyzerConfig` | `config.go:145` |
| Safety | `safety` | `SafetyConfig` | `config.go:173` |
| Trust | `trust` | `TrustConfig` | `config.go:183` |
| LLM | `llm` | `LLMConfig` (+`optimizer`, `optimizer_llm`) | `config.go:196` |
| Advisor | `advisor` | `AdvisorConfig` | `config.go:249` |
| Briefing | `briefing` | `BriefingConfig` | `config.go:260` |
| Alerting | `alerting` | `AlertingConfig` | `config.go:266` |
| AutoExplain | `auto_explain` | `AutoExplainConfig` | `config.go:290` |
| Forecaster | `forecaster` | `ForecasterConfig` | `config.go:298` |
| Tuner | `tuner` | `TunerConfig` | `config.go:395` |
| RCA | `rca` | `RCAConfig` | `config.go:315` |
| Runaway | `runaway` | `RunawayConfig` | `config.go:360` |
| Explain | `explain` | `ExplainConfig` | `config.go:367` |
| LogWatch | `logwatch` | `LogWatchConfig` | `config.go:337` |
| SchemaLint | `schema_lint` | `SchemaLintConfig` | `config.go:375` |
| Migration | `migration` | `MigrationConfig` (DDL safety advisor) | `config.go:385` |
| Retention | `retention` | `RetentionConfig` | `config.go:423` |
| Prometheus | `prometheus` | `PrometheusConfig` | `config.go:430` |
| OAuth | `oauth` | `OAuthConfig` | `config.go:435` |
| AgentDB | `agentdb` | `AgentDBConfig` | `config.go:122` |
| Databases | `databases` | `[]DatabaseConfig` (fleet) | `fleet.go:6` |
| Defaults | `defaults` | `DefaultsConfig` (fleet) | `fleet.go:64` |
| API | `api` | `APIConfig` | `fleet.go:73` |
| Meta DB / key | `meta_db`, `encryption_key` | strings (fleet) | `config.go:95` |

### 2.2 Key blocks + notable defaults

**postgres** — `host` (localhost), `port` (5432), `user` (`sage_agent`),
`database` (`postgres`), `sslmode` (`prefer`), `max_connections` (2),
`database_url` (overrides discrete fields). `DSN()`/`ConnString()` at
`config.go:486` / `fleet.go:25`.

**collector** — `interval_seconds` (60), `batch_size` (1000), `max_queries`
(500).

**analyzer** — `interval_seconds` (600), `slow_query_threshold_ms` (1000),
`seq_scan_min_rows` (100000), `unused_index_window_days` (**7**),
`index_bloat_threshold_pct` (30), `table_bloat_dead_tuple_pct` (20),
`table_bloat_min_rows` (1000), `idle_in_transaction_timeout_minutes` (30),
`cache_hit_ratio_warning` (0.95), `xid_wraparound_warning` (500M) /
`_critical` (1B), `regression_threshold_pct` (50), `regression_lookback_days`
(7), `checkpoint_frequency_warning_per_hour` (12),
`work_mem_promotion_threshold` (5), `slow_slot_retained_bytes` (1GB), nested
`lock_chain` (enabled, min_blocked 3, critical 10, idle_in_tx_terminate 5m,
active_query_cancel 15m, safe_patterns `[pg_sage, replication, patroni]`).

**safety** — `cpu_ceiling_pct` (90), `query_timeout_ms` (500),
`ddl_timeout_seconds` (300), `disk_pressure_threshold_pct` (5),
`backoff_consecutive_skips` (3), `dormant_interval_seconds` (600),
`lock_timeout_ms` (30000; `LockTimeout()` floors zero to default,
`config.go:478`).

**trust** — `level` (`observation`; validated to observation/advisory/autonomous
at `config.go:635`), `ramp_start` (auto-persisted, see
`PersistTrustRampStart` `bootstrap.go:89`), `maintenance_window`, `tier3_safe`
(**true**), `tier3_moderate` (false), `tier3_high_risk` (false; forced false
outside fleet), `rollback_threshold_pct` (10), `rollback_window_minutes` (15),
`rollback_cooldown_days` (7), `cascade_cooldown_cycles` (3).

**llm** — `enabled` (**false**), `endpoint`, `api_key` (secret), `model`,
`timeout_seconds` (30), `token_budget_daily` (500000), `context_budget_tokens`
(8192), `cooldown_seconds` (300), `json_mode`. Sub-blocks: `index_optimizer`
(deprecated, migrated to `optimizer` at `config.go:593`), `optimizer`
(enabled false, min_query_calls 100, confidence_threshold 0.5, plan_source
`auto`, hypopg_min_improvement_pct 10), `optimizer_llm` (dedicated reasoning
tier, timeout 120s, max_output_tokens 8192, fallback_to_general true).

**advisor** — `enabled` (false), `interval_seconds` (86400), and per-domain
toggles `vacuum_enabled`/`wal_enabled`/`connection_enabled`/`memory_enabled`/
`rewrite_enabled`/`bloat_enabled` (all true).

**briefing** — `schedule` (`0 6 * * *`), `channels` (`[stdout]`),
`slack_webhook_url`.

**alerting** — `enabled` (false), `check_interval_seconds` (60),
`cooldown_minutes` (15), `quiet_hours_start`/`_end`, `timezone` (`UTC`),
`slack_webhook_url`, `pagerduty_routing_key`, `routes []{severity, channels}`,
`webhooks []{name, url, headers}`.

**auto_explain** — `enabled` (true), `log_min_duration_ms` (1000),
`collect_interval_seconds` (300), `max_plans_per_cycle` (100),
`prefer_session_load` (true).

**forecaster** — `enabled` (true), `lookback_days` (30),
`disk_warn_growth_gb_day` (5.0), `connection_warn_pct` (80),
`cache_warn_threshold` (0.95), `sequence_warn_days` (90) / `_critical` (30),
`min_data_points` (24), `alert_horizons` (`[30,7,3]`), `disk_capacity_bytes`
(0=auto), `min_r_squared` (0.5).

**tuner** — `enabled` (true), `llm_enabled`, `work_mem_max_mb` (512),
`plan_time_ratio` (3.0), `nested_loop_row_threshold` (10000),
`parallel_min_table_rows` (1M), `min_query_calls` (100),
`verify_after_apply` (true) + revalidation loop (`hint_retirement_days` 14,
`revalidation_interval_hours` 24, keep_ratio 1.2, rollback_ratio 0.8) + stale-
stats/ANALYZE (`analyze_max_table_mb` 10240, `analyze_timeout_ms` 600000,
`max_concurrent_analyze` 1 — fleet-wide semaphore).

**rca** — `enabled` (true), `llm_correlation_threshold` (3),
`dedup_window_minutes` (30), `escalation_cycles` (5), `resolution_cycles` (2),
`connection_saturation_pct` (80), `replication_lag_threshold_seconds` (30),
`wal_spike_multiplier` (2.0).

**runaway** — `enabled` (false), `policies` (default: `long_running` 30m,
`blocker` 5 sessions), `safe_patterns` (`[pg_dump, pg_basebackup]`).

**explain** — `enabled` (true), `timeout_ms` (10000), `cache_ttl_minutes` (60),
`max_tokens` (4096).

**logwatch** — `enabled` (false; requires `rca.enabled`), `log_directory`/
`format` (auto-detected), `poll_interval_ms` (1000), `dedup_window_seconds`
(60), `max_line_len_bytes` (65536), `temp_file_min_bytes` (10MB),
`max_lines_per_cycle` (10000), `exclude_applications`, `slow_query_enabled`
(false).

**schema_lint** — `enabled`, `scan_interval_minutes` (60), `include_schemas`,
`exclude_schemas` (pg_catalog, information_schema), `disabled_rules`,
`min_table_rows` (1000).

**migration** (DDL safety advisor) — `enabled`, `mode` (`advisory`),
`managed_service` (none/rds/aurora/cloudsql/alloydb), `log_detection`,
`activity_polling`, `poll_interval_seconds` (5), `ddl_row_threshold` (10000).

**retention** — `snapshots_days` (90), `findings_days` (180), `actions_days`
(365), `explains_days` (90).

**prometheus** — `listen_addr` (`127.0.0.1:9187`).

**oauth** — `enabled`, `provider` (google/github/okta/oidc), `client_id`,
`client_secret` (secret), `redirect_url`, `issuer_url`, `default_role`.

**agentdb** — `live_provisioning_enabled` (false), `allow_public_ip` (false),
`require_backup_before_destroy` (true), `providers` map of
`{enabled, allowed_regions/accounts/projects/workspaces, max_ttl_seconds,
max_estimated_cost_usd}`.

**api** — `listen_addr` (`0.0.0.0:8080`), `trusted_proxies` (default loopback
`[127.0.0.1, ::1]`).

**fleet (`databases[]`)** — per-DB `name`/`host`/`port`/`user`/`password`
(encrypted at rest), `trust_level`, `execution_mode` (auto/approval/manual),
`executor_enabled`/`llm_enabled` (`*bool`, nil=on), per-DB collector/analyzer
intervals. `defaults` block supplies fallbacks (`normalize`, `fleet.go:113`).

---

## 3. Alerting & Notification

There are **two independent, non-overlapping** systems.

### 3.1 `internal/alerting` — findings-polling alert manager
- **`Manager.Run`** (`alerting.go:76`) ticks every `check_interval_seconds`
  (default 60), calling `evaluate` (`alerting.go:102`) which queries
  `sage.findings WHERE status='open' AND last_seen > lastCheck`, groups by
  severity, and dispatches per route. Wired in `main.go` only when
  `alerting.enabled` (default false).
- **Channels** (`Channel` interface, `channel.go:9`): **Slack** (`slack.go:14`,
  3-attempt exponential backoff), **PagerDuty** (`pagerduty.go:17`, Events API
  v2, self-managed `dedup_key`, 3-attempt retry), **Webhook** (`webhook.go:14`,
  no retry). **No email channel here.**
- **Severity routing** via `AlertRoute{Severity, Channels}` (`config.go:279`);
  severities absent from the route map are silently dropped.
- **Throttle** (`throttle.go:12`): per-severity cooldowns floored by config —
  critical max(5m), warning max(30m), info max(6h). **Dedup key** =
  `category:objectIdentifier` (`FormatDedupKey`, `throttle.go:152`).
  **Escalation override** fires immediately if severity rank rises.
  **Quiet hours** (`isQuietHours`, `throttle.go:110`) in configured tz, supports
  midnight wrap. `Record` is called only if at least one channel delivered
  (`alerting.go:185`), so total-failure keys re-fire next cycle.
- **Delivery logging**: one row per channel per finding into `sage.alert_log`
  (`logAlert`/`insertAlertLog`, `alerting.go:227`/`:31`), status `sent`/`error`.

### 3.2 `internal/notify` — event-driven, config-table-backed dispatcher
- **`Dispatcher.Dispatch`** (`dispatcher.go:42`) loads matching rules from
  `sage.notification_rules` (`loadMatchingRules`, `dispatcher.go:91`), resolves
  the channel from `sage.notification_channels` (`loadChannel`,
  `dispatcher.go:120`), checks `SeverityMeetsMin` (`types.go:47`), sends, and
  logs to `sage.notification_log` (`logDelivery`, `dispatcher.go:140`).
- **Senders** (registered in `api/router.go`): **Slack** (`slack.go`),
  **Email** (`email.go`, SMTP+TLS, default port 587), **PagerDuty**
  (`pagerduty.go`, Events API v2, 202). **No webhook sender here.**
- **Event sources** (`events.go`): `action_executed`, `action_failed`,
  `approval_needed`, `finding_critical`, `query_rewrite_suggested`.
- **No throttle / quiet-hours / cooldown / dedup map** — every qualifying event
  fires immediately (PagerDuty's server-side dedup_key is the only dedup).

### 3.3 Dead / unwired (alerting/notify)
- **Event-path notifications are effectively broken**: the executor/analyzer
  `Dispatcher` instances (`main.go:596`, `:1182`) are created but **never have
  senders registered** — only the API's separate dispatcher (`api/router.go`)
  registers senders. Events therefore log `sage.notification_log` rows with
  status `error` ("no sender for type") and **never deliver** Slack/email/PD
  through the event path. Only the API "test notification" (`SendDirect`,
  `dispatcher.go:172`) works.
- Channel coverage is asymmetric: `alerting` has webhook but no email;
  `notify` has email but no webhook. A `type='webhook'` notification channel
  can never deliver.
- `internal/alerting` is off by default (`alerting.enabled=false`).

---

## 4. `internal/retention` — Data Cleanup

`Cleaner.Run` (`cleanup.go:30`) purges four tables, batched 1000 rows/call via
`DELETE ... WHERE ctid IN (... LIMIT 1000)` (`purgeTable`, `cleanup.go:43`).
**Guard**: `retentionDays <= 0` skips the table entirely (`cleanup.go:50`) —
zero means *keep forever*, not delete everything.

| Table | Time column | Config field | Extra filter |
|-------|-------------|--------------|--------------|
| `sage.snapshots` | `collected_at` | `SnapshotsDays` (90) | — |
| `sage.findings` | `last_seen` | `FindingsDays` (180) | `AND status='resolved'` |
| `sage.action_log` | `executed_at` | `ActionsDays` (365) | — |
| `sage.explain_cache` | `captured_at` | `ExplainsDays` (90) | — |

A fifth sweep, `cleanStaleFirstSeen` (`cleanup.go:93`), deletes `first_seen:*`
keys from `sage.config` for indexes no longer present in the latest snapshot.

**Wiring**: WIRED. `retention.New` at `main.go:683`; run from the standalone
orchestrator on an `analyzer.Interval()+5s` ticker (`main.go:692`), and via the
fleet orchestrator. `Run` logs failures (never returns an error).

---

## 5. `internal/ha` — Replica / Role-Flip Monitor (safe-mode unwired)

`ha.go` tracks **primary/replica role only** via `SELECT pg_is_in_recovery()`
(`ha.go:41`) and detects role flips between successive `Check` calls. After
`flipThreshold = 5` consecutive flips it sets `safeMode=true` (`ha.go:76`);
after `stableThreshold = 5` stable checks it clears it. **No replication-lag or
replica-state monitoring exists** in this package — just the recovery boolean.

**Wiring**: PARTIAL. `haMon.Check(ctx)` runs in the standalone orchestrator
(`main.go:707`) and its `isReplica` bool gates the executor
(`exec.RunCycle(ctx, isReplica)`, `main.go:713`). **`InSafeMode()` is never
read in production** — the flip-counting safe-mode state machine runs but
nothing consumes it (DEAD logic). **Fleet mode ignores `ha` entirely** —
`RunCycle(ctx, false)` hardcodes `isReplica=false` (`main.go:1444`).

---

## 6. `internal/logwatch` — Log Tailing + RCA Fanout

- **Tailing is polling, not fsnotify** (`Tailer`, `tailer.go:18`). `poll` ticks
  at `poll_interval_ms` (default 1000) calling `ReadLines` (`tailer.go:142`),
  handling copytruncate (`detectTruncation`, `tailer.go:184`) and new-file
  rotation (`maybeRotate`, `tailer.go:276`); seeks 1MB back on first open. Log
  dir/format auto-detected from `pg_settings` (`detect.go:20`); only
  `jsonlog`/`csvlog` supported.
- **18 signal patterns** (`classifier.go:23`): deadlock (40P01), statement
  timeout (57014), lock timeout (55P03), temp file (size-gated by
  `TempFileMinBytes`), checkpoint-too-frequent, slow query (opt-in via
  `SlowQueryEnabled`), plus OOM, disk full, panic/crash, corruption, txid
  wraparound, archive failure, replication conflict, WAL-segment removed,
  autovacuum cancel, inactive replication slot, auth failure. Dedup by
  `(signalID, database)` within `dedup_window_seconds` (`classifier.go:357`);
  self-inflicted entries (`ExcludeApps`) dropped except deadlocks.
- **RCA handoff**: `FileWatcher` implements `rca.LogSource` (Start/Drain/Stop,
  `watcher.go:17`); `Drain` (`watcher.go:82`) returns `[]*rca.Signal`. Standalone
  binds it via `rcaEng.SetLogSource(fw)` (`main.go:570`). Fleet uses `LogFanout`
  (`fanout.go:23`) — one `FileWatcher` per host:port cluster fanned out to
  per-database `FanoutSubscriber`s (priority-capped 10000-entry buffers,
  critical never evicted), bound per-DB at `main.go:1147`.
- **Wiring**: FULLY WIRED (standalone gated on `logwatch.enabled`, `main.go:547`;
  fleet via `startClusterFanout`/`runFanoutDrain`, `main.go:1314`/`:1368`).

---

## 7. `internal/crypto` & `internal/sanitize`

### 7.1 `internal/crypto` — Encryption-at-rest
- **AES-256-GCM** (`crypto.go`), 12-byte nonce prepended to ciphertext.
  `Encrypt`/`Decrypt` (`crypto.go:23`/`:50`) require a 32-byte key.
- **KDF**: argon2id — `DeriveKey(passphrase, salt)` =
  `argon2.IDKey(pass, salt, time=1, mem=64MB, threads=4, keyLen=32)`
  (`crypto.go:113`; **panics** if salt < 8 bytes). Salt is the per-deployment
  random `NewKDFSalt()` (16 bytes, `crypto.go:127`) persisted in singleton
  `sage.crypto_meta` (`ReadOrCreateKDFSalt`, `crypto_meta.go:22`).
- **Lazy key rotation**: `DecryptWithMigration` (`crypto.go:144`) tries current
  argon2id → v2 deterministic-salt argon2id → v1 bare SHA-256, returning
  `needsReEncrypt=true` on a legacy hit. `DeriveKeyV1`/`DeriveKeyV2Legacy`
  retained for decrypt-only migration.
- **What's encrypted**: per-database connection passwords in
  `sage.databases.password_enc`. Write/read via
  `store/database_store.go:83`/`:287`. Key derived at startup in
  `cmd/.../metadb.go:121` from `--encryption-key` + persisted salt; if no
  passphrase, credentials are stored **unencrypted** with a warning.

### 7.2 `internal/sanitize` — SQL identifier/literal quoting (`quote.go`)
Scope note: this is **PostgreSQL escaping + a stacked-query guard**, *not* LLM
PII/literal redaction or prompt-injection filtering.
- `QuoteIdentifier(name)` — doubles `"`, wraps in `"…"` (`quote.go:11`).
- `QuoteQualifiedName(schema, name)` — quotes each part, joins with `.`
  (`quote.go:16`).
- `QuoteLiteral(s)` — doubles `'`, wraps in `'…'` (`quote.go:22`; safe only
  under `standard_conforming_strings=on`).
- `RejectMultiStatement(sql)` — rejects anything non-whitespace after the first
  `;` (`quote.go:28`); a single trailing `;` is allowed. Naive string scan
  (fails closed). Used to guard captured/LLM query text before EXPLAIN/HypoPG in
  `autoexplain/collector.go:158`, `optimizer/hypopg.go:141`,
  `optimizer/plancapture.go:134`.

---

## 8. Dead / Unwired Summary

| Component | Status |
|-----------|--------|
| `notify` event-path delivery | **Broken** — executor/analyzer dispatchers have no senders registered; events log `error` and never deliver. |
| `notify` webhook sender | Missing — `type='webhook'` channels can't deliver. |
| `alerting` email channel | Missing (only slack/pagerduty/webhook). |
| `ha.InSafeMode()` / safe-mode | Dead — state machine runs, no consumer. Fleet ignores `ha` (hardcodes `isReplica=false`). |
| `alerting` (whole system) | Off by default (`enabled=false`). |
| `sage.schema_findings` table | Legacy/read-only since v0.11 (absorbed into `findings`). |
| `migration` active mode | Advisory-only; active mode "deferred to future release". |
