# Changelog

## [Unreleased]

These changes are planned as five releases: v2.5.1 (tranche T0a, the safety fixes),
v2.5.2 (tranche T0b), v2.6.0 (tranche T1), v2.7.0 (tranche T2) and v2.8.0+ (tranche T3).
The tranche T4 changes finish them and ship with the T4 release; the T4 follow-ups (tranche T4c)
follow them, the T5 foundations (tranche T5) come next, and the T6 changes
(tranche T6: Sage Review, restore drills, fleet policy, change guards) are listed last.

### v2.5.1 (tranche T0a, safety)

#### Upgrade notes

- Game days and local bench runs refuse a `local_dsn` without the marker `COMMENT ON DATABASE
  <db> IS 'pg_sage:gameday_target'` or on a monitored cluster: add the marker and give the
  target a cluster of its own (benches-02).
- Databricks OAuth database connections need `sslmode=verify-full`; `require` and `verify-ca`
  are refused before pg_sage contacts Databricks (databricks-02).
- MCP `explain_query` needs the operator or admin scope for people; agents get plan-only
  EXPLAIN under their own database role (security-02).
- Unrecognized agent DDL is classed as locking, not additive, so agents that relied on it earn
  trust in the locking class (migration-04a).
- A failed provider probe reads as `unknown`: `ALTER SYSTEM` and other provider-gated actions
  wait for a successful re-probe (clouds-05a).
- Write regressions after an index build are logged as `write_signal`, not reverted, until
  executor-03b in v2.7.0 (executor-03).
- pg_sage's own DDL runs with `search_path = pg_catalog, pg_temp`: an unqualified target now
  fails and is recorded as `failed` (security-01a, search-path-fix).
- `llm.optimizer.plan_source` is ignored, with one startup warning; remove it from your config
  (indexes-05).
- PagerDuty: add a rule for `incident_resolved` to each channel, and close by hand, once, the
  pages opened before the upgrade (alerting-01).
- To stop pg_sage from SQL when the dashboard is down, use `SELECT sage.emergency_stop(true,
  '<actor>')`; the old documented `UPDATE` did nothing on databases never stopped through the
  API (executor-06).
- "Stop all" is a fleet-wide latch: resuming one database answers 409 until "Resume all", and
  databases added while it holds start stopped (fleet-09).

#### Security

- **DDL search_path.** pg_sage's own DDL now runs with `search_path = pg_catalog, pg_temp`,
  both transactional and `CONCURRENTLY`, so a function another role plants in `public` can no
  longer bind into indexes pg_sage builds (the CVE-2018-1058 class). Unqualified targets now
  fail and the action is recorded as `failed`, and optimizer index recommendations always name
  the table's schema (security-01a). A bare operator class in a `CREATE INDEX`, such as
  `gin_trgm_ops` or `vector_l2_ops`, still builds. It runs with pg_catalog's class when one
  exists, otherwise with the schema of the one installed extension that defines it. A class
  outside every extension, or defined by more than one extension, is refused before the
  statement runs. Functions, types and tables are never resolved this way.
- **Expression indexes.** The optimizer no longer recommends an expression index that calls a
  function outside pg_catalog or an allowlisted extension (refused with
  `function_schema_not_allowed`). It also refuses indexes on functions with any non-IMMUTABLE
  overload, and refuses when the volatility catalog read fails. A function planted in `public`
  can no longer make an index admissible (security-01b).
- **MCP `explain_query`** now needs the propose scope (operator/admin) for people, as REST
  does, and honours `explain.enabled`. Agents get plan-only EXPLAIN under their own database
  role through agent governance. agent_query and agent EXPLAIN now refuse a denied column (prod
  PII, or a secret anywhere) used outside the plain output (security-02).
- **Lock snapshots.** The `locks` snapshot is now a bounded lock wait graph of at most 200
  rows: waiters with `blocked_by`, plus blockers, idle-in-transaction sessions and the 5
  oldest-xmin sessions. It gains `xact_start`, `backend_xmin_age` and `query_id`, and its
  statement text has string and dollar-quoted literals redacted and is cut to 256 bytes before
  it is stored. Older `locks` rows, which held raw statement text, are deleted once at startup
  in batches under `lock_timeout` (collection-01).
- **Managed-parameter commands.** Proposals now quote values read back from Cloud SQL and RDS
  before putting them in a command: gcloud `--database-flags` is single-quoted (with `^DELIM^`
  syntax when a value has a comma) and RDS `--parameters` is quoted JSON. An unsafe value gives
  a blocker and a comment line in place of the command. When pg_sage does not know the current
  flags, it prints a fill-in template instead of a command that would clear every other flag.
  The dashboard's Approve button stays disabled while a proposal has blockers (clouds-01).
- **Databricks/Lakebase.** OAuth database credentials are now sent only over
  `sslmode=verify-full`, on the main host and every fallback; `require` and `verify-ca` are
  refused before pg_sage contacts Databricks. The Apps launcher raises a platform-injected
  `PGSSLMODE=require` to `verify-full` and refuses any other mode below it (databricks-02).

#### Fixed

- **Partitioned tables.** Partitioned parent tables no longer show a transaction age of
  2,147,483,647 or get an automatic VACUUM (FREEZE) of their whole partition tree. Their age is
  now recorded as unknown (-1) and each partition is checked on its own. The age used is now
  the older of the table's and its TOAST table's, and multixact age is shown in the finding.
  Any VACUUM aimed at a partitioned table, from any producer, now waits for operator approval;
  one whose table was dropped is skipped as stale (vacuum-01).
- **Contractless candidates.** Candidates with no typed contract (for example TRUNCATE) now
  park in sage.decision at risk tier high with `evidence.contract=none`, instead of failing the
  `decision_risk_tier_check` CHECK every cycle and logging an ERROR to PostgreSQL. A decision
  that cannot be recorded still fails closed (`policy_unavailable`), but its detail now names
  the intended verdict and reason. The new
  `pg_sage_policy_record_failures_total{database,reason}` counts these failures, and the
  sidecar logs each one at most once an hour (policy-01).
- **Freeze custodian holders.** A red table is now frozen unless an xmin holder actually pins
  it (at least half the table's XID age), and the freeze still runs while that holder is
  younger than 0.9 of the table age. Every holder kind is found: idle writers by backend_xid,
  prepared transactions, slots (catalog_xmin only for catalog tables) and standby feedback.
  Each pinning holder gets one freeze_blocker proposal per scan, listing all the tables it
  pins. A session holder is a cancel or terminate that still needs approval. Every other holder
  kind, and pg_dump/basebackup/restore/pg_sage sessions, becomes a parked high-risk plan that
  names the holder. A blocker counts as cleared only when its PID is gone, its backend
  restarted, or it holds neither a snapshot nor an XID (vacuum-02).
- **Bloat rebuilds.** The freeze custodian now plans a bloat rebuild only from a catalog bloat
  estimate of at least 50% on a heap of at least `analyzer.table_bloat_min_bytes` (new, default
  1 GiB), and no longer plans one from dead tuples alone. When `pgstattuple` is installed it
  confirms at most 3 candidates per cycle. Each plan is recorded once as a parked, high-risk
  `bloat_remediation` decision; before, the ledger rejected it with a `risk_tier` CHECK error
  every cycle (vacuum-04).
- **Index EXPLAINs.** The index optimizer no longer runs an EXPLAIN for every statement of a
  table it analyzes. These plans were never used, and on PG16+ they could wait forever behind
  an ACCESS EXCLUSIVE lock. The HypoPG what-if now plans at most 25 statements per candidate
  (the proposal's target statements first, then the hottest), under `safety.lock_timeout_ms`. A
  lock timeout leaves the candidate unverified. `llm.optimizer.plan_source` is deprecated and
  ignored, with one startup warning (indexes-05).
- **Index equivalence.** Index advisor and tuning now compare operator class, collation and
  sort order when they decide whether one index duplicates or covers another. A
  text_pattern_ops, COLLATE-specific, mixed-order, jsonb_path_ops or HNSW cosine index is no
  longer rejected as a duplicate of a plain index, and is no longer dropped or replaced as
  "covered" by one. Explicitly spelled defaults (text_ops, COLLATE "default") are still
  recognised as duplicates (indexes-01).
- **work_mem hints.** Stat-based work_mem hints are now sized from the per-call spill between
  tuner cycles, against the work_mem the statement's own role runs with. There is no 64 MB
  floor. Every work_mem hint, from rules or the tuning agent, is capped at
  min(`tuner.work_mem_max_mb`, 5% of host memory). Without host memory telemetry, work_mem
  hints need operator approval (tuning-03).
- **Hint revalidation** no longer retires a hint because its query got fast or old, nor marks
  hints broken after `pg_stat_statements_reset()` or an idle period: idle hints become
  `dormant` (still installed), resets re-baseline via `pg_stat_statements_info.stats_reset`,
  calls are summed across roles, and hints past `tuner.hint_retirement_days` are stamped
  `reverify_due` instead of removed (tuning-01).
- **Hint rollbacks.** Rolling back a query-hint action, or removing a retired hint, now deletes
  only the `hint_plan.hints` row whose text matches pg_sage's own hint, so an operator's hint
  for the same query survives. Rollbacks of actions recorded by earlier releases are rewritten
  to the same scoped form at rollback time, or refused with `hint_text_unknown` when pg_sage
  has no single recorded hint text for the query (tuning-02a).
- **Dropped-index restore.** A regressed autonomous index drop is now restored automatically.
  It is re-created with `CREATE INDEX CONCURRENTLY` from the recorded definition, under the new
  safe `restore_dropped_index` contract. This holds even after trust is lowered (observation,
  approval or manual mode, `tier3_safe` off, ramp reset); the decision reason is
  `drop_restore_bypass`. During an emergency stop the restore is recorded as
  `rollback_withheld` and one approval card is queued, which can be approved once the stop is
  lifted. A failed restore build is now reported as `rollback_failed`, no longer as
  `already_restored` (executor-01).
- **Index verification reads.** A transient read error while verifying an autonomously created
  index (lost connection, a metadata-database outage under `history.store: meta`, or a shutdown
  mid-check) no longer drops the index. The watch stays open as `observation_retry` and is
  checked again after 5, 15, then 60 minutes; only reaching the hard deadline without a
  successful check reverts it (`observation_unavailable`). Watches closed by this bug in
  earlier versions are reopened at startup (executor-02).
- **Index verification writes.** Index verification no longer reverts an index on a
  database-wide write-time rise: write regressions after an index build are now logged as
  advisory evidence (`write_signal`), not reverted, until executor-03b adds a table-scoped
  write signal. A failed write-history read no longer pauses the watch, and at startup a
  pending `write_impact` revert from an earlier version is reopened and decided on its queries
  (executor-03).
- **Cases playbooks.** The lock and idle-in-transaction playbooks no longer suggest
  pg_cancel_backend/pg_terminate_backend for a PID scraped from incident text (for example
  "LIMIT 20"). A signal is offered only for a lock_contention incident with exactly one
  structured root blocker, as an operator-run script. The script re-checks pid, backend_start,
  query_start, the query hash, the state, the current database and the protected-session
  filter. Terminate is offered only for critical incidents or idle-in-transaction blockers, and
  the Cases page shows the identity the script is pinned to (playbooks-01).
- **RCA xmin holders.** RCA's vacuum_blocked incident now names the real oldest holder of the
  database's xmin horizon: a session open past `rca.xmin_holder_min_minutes` (default 5), a
  prepared transaction past `rca.prepared_xact_min_age_minutes` (default 30), a standby, or a
  slot. It proposes `pg_terminate_backend` only for a holder idle in transaction past
  `lock_chain.idle_in_tx_terminate_minutes`; every other holder gets a read-only diagnostic.
  Other databases' prepared transactions are never blamed, a prepared transaction's GID is
  quoted in `ROLLBACK PREPARED`, and an open incident's recommended SQL follows its refreshed
  root cause (rca-01).
- **CDC slot facts.** Confirmed CDC slot facts now block every finite `max_slot_wal_keep_size`
  bound and RESET, whichever slot it names, and bind sibling databases of the same cluster
  monitored by one process (facts-02).
- **PagerDuty incidents.** An incident's detected, escalated and resolved events now share one
  dedup key (`incident:<db>:<id>`), and a resolved incident now closes its page. Critical
  findings are keyed by database, category and object, and are resolved when the finding
  clears. Channels need a rule for incident_resolved, and pages opened before the upgrade must
  be closed by hand once (alerting-01).
- **Audit chain.** The chain verifier now reports `orphan_row` when tail links or a whole chain
  were deleted; SIEM export stops a chain that went back behind its cursor (`regressed`)
  instead of silently skipping reused positions and raises a critical `audit_chain_regressed`
  finding; admins resume with `POST /api/v1/audit/siem/{sink}/reanchor`, and re-exported events
  are labelled `fork` (audit-01).
- **Dashboard errors.** A page that fails to render now shows an inline error with Retry, and
  the header, navigation and emergency stop stay usable. Blocked site storage no longer
  white-screens the login screen (dashboard-03).
- **Fleet tags.** Meta-DB fleets now carry database tags, so `tenant=<x>` tags split
  fleet-learning look-alikes and priors per tenant, and tag edits apply without a restart
  (fleet-01).
- **Provider probes.** A failed provider probe (timeout, dropped connection, permission error)
  now reads as `unknown`, never `self-managed`. Provider-gated actions such as `ALTER SYSTEM`
  wait until a re-probe succeeds. Each probe now has its own 2 s timeout and is retried after
  1, 2 and 4 s at startup. After that, pg_sage re-probes every 1 to 30 minutes and rebuilds
  that database's runtime once the provider is known. In standalone mode, and for the control
  database of a YAML fleet, readiness instead asks for a restart (clouds-05a).
- **Walkthroughs.** The try-it and Linux/Windows walkthrough pages' role SQL now matches the
  quickstart's read-only block, which creates the `sage` schema, so pg_sage bootstraps on the
  stock postgres image they start instead of refusing the database; the pages' connection
  strings now use the same password, and they cover Getting started and First look
  (firstlook-02).

#### Changed

- **Emergency stop** now cancels the statements pg_sage is already running (CIC, VACUUM, ALTER
  waiting on a lock, rollbacks) with a PostgreSQL cancel request; cancelled actions end
  `failed` with reason "emergency stop", and a cancelled CIC names the INVALID index and its
  `DROP INDEX CONCURRENTLY IF EXISTS` cleanup. The new `SELECT sage.emergency_stop(true|false,
  '<actor>')` SQL function sets the stop with an audit row when the dashboard is down (the old
  documented `UPDATE` was a no-op on databases never stopped through the API) (executor-06).
- **Fleet-wide stop.** "Stop all" is now a fleet-wide latch. Databases added while it holds (UI
  add, CSV import, meta-DB reconcile, config reload) start stopped. Only "Resume all" lifts it:
  resuming one database returns 409 "fleet-wide stop is active: resume all first". The latch
  survives restarts: in meta mode as `fleet_emergency_stop`, in YAML fleets as
  `emergency_stop_scope`. Every stop now cancels running actions and lists them in the
  response. The sidecar re-reads every database's emergency_stop every 10 s, so a stop or
  resume made in SQL shows within one tick. Bootstrap seeds `emergency_stop='false'`, so the
  documented UPDATE works. The overview reports `fleet_stopped`, `fleet_stopped_by` and
  `fleet_stopped_at` (fleet-09).
- **Live trust ceilings.** Changing `trust.tier3_safe`, `tier3_moderate`, `maintenance_window`,
  `rollback_threshold_pct`, `rollback_window_minutes`, `rollback_cooldown_days` or
  `cascade_cooldown_cycles` now reaches every executor on the next cycle without a restart. A
  failed change keeps the stricter ceiling, and invalid rollback and cascade bounds are
  rejected (config-01).
- **Agent DDL classes.** Unrecognized agent DDL is now classed as locking, not additive, so
  agents that relied on it earn trust in the locking class. lint_migration returns review
  instead of safe for unrecognized statements, and a DROP or TRUNCATE hidden behind a literal
  is classed as destructive (migration-04a).
- **Game-day targets.** Game days and local bench runs now refuse a
  `sre.autonomy.game_days.local_dsn` on the cluster of any monitored database (fleet members
  added after start included) or of the metadata database, including sibling databases, host
  aliases, pooler ports and standbys. They also refuse a standby, a primary that streams to
  standbys, and a database without the marker `COMMENT ON DATABASE <db> IS
  'pg_sage:gameday_target'`, and they refuse whenever these checks cannot be read. Existing
  local_dsn setups must add the marker and use a cluster of their own (benches-02).
- **AgentSafetyBench scoring.** other_error is no longer counted as held. It is reported as
  "not measured" (n/m), and the harness self-checks now require a credited refusal from every
  design, so a design that cannot log in fails the bench instead of passing it (benches-04).
- **AgentSafetyBench gates.** AgentSafetyBench now fails CI when any (design, case) result
  changes from the committed baseline (`safety-bench/testdata/baseline.json`, regenerate with
  `go run ./safety-bench/cmd/baseline`), the EXPLAIN guard has its own hard gate, and release
  tags and versioned images now wait for the PG14/15/16/18 integration matrix (`:edge` is
  unchanged) (benches-05).

### v2.5.2 (tranche T0b)

#### Upgrade notes

- Every dashboard user signs in once after the upgrade (apiauth-04).
- A separately hosted UI or a Host-rewriting proxy must list its public origin in
  `api.trusted_origins` / `SAGE_API_TRUSTED_ORIGINS`; the localhost dev CORS allowlist is gone
  (apiauth-10).
- Databricks Apps: pg_sage exits at startup when `DATABRICKS_APP_PORT` is set and
  `SAGE_AUTH_MODE` is not `databricks_app`; there is no override (databricks-08).
- Pending OAuth-agent proposals made before the upgrade have no recorded sponsor: re-propose
  them or check their approvers by hand (agentgov-02).
- Release builds move to Go 1.27.3, Alpine 3.24 and Node.js 22; building from source needs
  Go 1.26+ and Node.js 22+ (ci-01).
- The PagerDuty dedup key becomes `<database>:<category>:<object>`: alerts open at the upgrade
  page once more under the new key, so close the old incidents by hand (alerting-02).
- Check your `SAGE_*` values: one that cannot be parsed stops startup, `0` and `false` now
  apply, and a container that publishes metrics must pass `--prom-addr 0.0.0.0:9187`
  (config-08).
- The container HEALTHCHECK reads `SAGE_CONFIG_PATH`: name your config file there instead of
  `--config` in the container args, or override the check with `--addr` (ci-06).
- VACUUM and autovacuum ALTER TABLE actions are withheld with `capability_missing` when the
  connected role lacks the privilege; the finding names the grant that fixes it (vacuum-06).

#### Security

- **Session cookies.** Dashboard session cookies are now random 256-bit tokens, and
  sage.sessions stores only their SHA-256, so read access to the sage schema, a dump or a
  bind-parameter log no longer exposes live sessions. Logout is audited. The new `DELETE
  /api/v1/auth/sessions` signs out all other sessions. A session-store outage now answers 503
  instead of 401. Every dashboard user signs in once after the upgrade (apiauth-04).
- **Cross-origin writes.** The API now refuses cross-origin and same-site browser writes on
  every /api/v1 route (Go cross-origin protection), including the multipart database import;
  list a separately hosted UI or a Host-rewriting proxy's public origin in the new
  `api.trusted_origins` / `SAGE_API_TRUSTED_ORIGINS`. The localhost dev CORS allowlist is
  removed, and behind a proxy session cookies are marked Secure from `X-Forwarded-Proto` only
  when the proxy is in `api.trusted_proxies` (apiauth-10).
- **Login hardening.** Unknown emails, SSO-only accounts and wrong passwords are now refused in
  the same time; a full login limiter never evicts a locked account and instead refuses new
  emails while every entry is locked (counted by the new
  `pg_sage_login_limiter_saturated_total`); each client address gets 20 failed logins per 15
  minutes across all emails; IPv6 clients are rate-limited per /64; and an `X-Forwarded-For`
  from a peer outside `api.trusted_proxies` now logs a one-time warning (apiauth-06).
- **pgaudit attribution.** pgaudit records are now attributed to an agent or to pg_sage only
  when the database role matches. An agent's application_name set by any other role is stored
  as `application_name_unverified`, with no principal and no action. In the SIEM export, a
  delete of an audit row is Informational only when pg_sage's own role made it; any other
  role's delete is Medium, whatever application_name it set (audit-04).
- **Advisor output.** Advisor model output is now treated as data. Reserved detail keys
  (managed_change, what_if_verdict, drop_ddl, index_replace, approval_required, sage_*) are
  dropped. Config advisors may only propose setting changes. The managed-change worker proposes
  only from config producer categories and allowlisted settings (advisors-04).
- **Model SQL.** RCA Tier 2 now keeps model-suggested SQL only when it is proven read-only;
  otherwise the SQL is withheld with a note. Migration-advisor model scripts are now checked to
  touch only the target table with allowed DDL; a script that fails the check is shown as
  "Model text, not validated by pg_sage" and never replaces the deterministic safe alternative.
  Every LLM prompt site now fences database content (security-05).
- **Answer-only questions.** Explain a case is now enforced as answer-only: Ask Sage and the
  MCP ask_sage tool accept `read_only`, and a read-only question is never offered
  propose_action, open_investigation or propose_fact. The answer is stored with read_only and
  shows an "Answer only" badge. Before this, the restriction was only prompt text (mcp-01).
- **Databricks Apps.** pg_sage now exits at startup when `DATABRICKS_APP_PORT` is set but
  `SAGE_AUTH_MODE` is not `databricks_app`. It never falls back to local password auth there
  and never prints a generated admin password to App logs, even when started without launch.py.
  There is no override (databricks-08).

#### Fixed

- **First look.** First look no longer reads "No problems found" when checks could not run: the
  checklist says "First look partial: N of M checks could not run" (and is not done when none
  ran), the panel lists degraded and truncated catalog checks with the GRANT that fixes each,
  /api/v1/onboarding first_look gains checks/degraded/truncated, and the model summary covers
  degraded-only first looks (firstlook-01).
- **Wraparound deadline.** A table past autovacuum_freeze_max_age, the most urgent freeze case,
  now has a wraparound hard deadline ahead, so the xid deadline override and the budget bypass
  apply to it. The deadline is now the server's wraparound warn limit (2^31-1-40M), capped at
  100 years, and no longer overflows to a far-past date at low multixact rates. Freeze decision
  evidence now names the deadline_threat (xid or multixact) and the server's
  vacuum_failsafe_age (PG14+) (vacuum-03).
- **Autovacuum tuning.** The freeze custodian no longer sets `autovacuum_vacuum_scale_factor =
  0.02` on freeze-amber tables. That setting was re-applied every cycle with no rollback and
  undid the analyzer's 0.01 on large tables. The analyzer's `autovacuum_tuning` finding is now
  the only autovacuum tuner, and its rollback restores a table's prior scale factor exactly as
  stored (0.10 stays 0.10) (vacuum-07).
- **Per-table autovacuum rule.** autovacuum_tuning now reads each table's own reloptions from
  the tables collection, so it works without an LLM. It never loosens a DBA's explicit
  autovacuum_vacuum_scale_factor, skips tables with autovacuum_enabled=false or reloptions it
  cannot parse, stops re-firing after its own change, and its rollback restores the table's
  previous explicit value (rules-02).
- **Schema-wide facts.** A confirmed test-fixture fact (or any schema-wide fact) no longer
  blocks configuration, database or query recommendations such as ALTER SYSTEM. Targets that
  name no relation never bind, and unqualified target tables are matched through the schemas
  that actually have them (facts-01).
- **App-managed index facts.** The app-managed index detector now names indexes as the catalog
  does (EF Core's `public."IX_Orders_CustomerId"` instead of `public.ix_orders_customerid`), so
  confirmed facts bind pg_sage's drops; the analyzer's app_managed_index suppression now also
  matches pg_sage's own quoted drops, and an index name with a space no longer breaks the
  drop-history read on PG14/15. Stale lower-cased facts stop being re-proposed and expire
  through the normal existence check; list them with `SELECT id, subject, status FROM
  sage.facts WHERE fact_type = 'owned_by_app_migrations' AND proposed_by =
  'detector:app_managed_index' AND status IN ('proposed','confirmed') AND
  pg_catalog.to_regclass(subject) IS NULL;` (facts-03).
- **Fixture cleanup.** Test-fixture cleanup batches now drop each schema in its own guarded
  transaction: the script runs in psql autocommit with lock_timeout 5s, and before each drop it
  re-checks the schema's activity against a baseline taken when the batch was built and checks
  for locks held by other sessions, skipping the schema with a NOTICE when either check fails.
  A few hundred fixture schemas no longer fail with "out of shared memory", and a schema
  matched by a family pattern that later became busy is no longer dropped (facts-06).
- **Online-migration planner.** `apply_migration` now resolves identifiers the way PostgreSQL
  does. Quoted names are kept verbatim, bare names are folded to lower case, and each name is
  quoted exactly once. `[NOT] DEFERRABLE` / `INITIALLY DEFERRED|IMMEDIATE` are carried into the
  attach step. `INCLUDE`, `WITH`, `USING INDEX TABLESPACE`, `NULLS [NOT] DISTINCT`, trailing
  text, and names over 63 bytes are refused with "unsupported option ...; plan it by hand"
  instead of being silently dropped or truncated (migration-02).
- **Fleet alerts.** Fleet alerts from the alerting manager now name their database in Slack,
  PagerDuty and webhook payloads, and the PagerDuty dedup_key becomes
  `<database>:<category>:<object>`. Identical schemas sharing a routing key no longer merge
  into one incident, and one database's resolve no longer closes another's page. Alerts open at
  upgrade re-page once under the new key, so close the old PagerDuty incidents by hand
  (alerting-02).
- **Fact cards** (proposed binding facts) now go out at warning severity, so they reach
  approval_needed notification rules created with the default min_severity instead of being
  silently dropped; rules that filter an event by severity now leave a DEBUG log line
  (alerting-03).
- **Audit isolation.** pg_sage now pins its sessions to READ COMMITTED and writes action_log in
  explicit READ COMMITTED transactions. Under a repeatable read or serializable role or
  database default, concurrent actions no longer fail with 23505 and drop out of the audit. A
  chained write at another isolation level is refused with SQLSTATE 55000 and a fix hint. At
  startup pg_sage logs one WARN per database whose default is not read committed (audit-02).
- **SIEM export after reloads.** SIEM audit export keeps running after a YAML fleet reload
  adds, removes or renames a database. Before this fix every export pass failed with "leader
  lease lost" until the sidecar restarted (fleet-02).
- **Managed-cloud outages.** A failed RDS/Aurora/Cloud SQL target lookup (throttling, expired
  token) no longer supersedes approved proposals or brings rejected ones back before the 7-day
  cooldown ends. Proposals are now identified by provider, parameter and value, and keep the
  last resolved target during an outage. On upgrade the stored identities are recomputed once:
  open proposals keep their ids, status and decisions, and duplicates left by earlier outages
  collapse to one (clouds-02).
- **Collector interval.** A collector interval promoted live by self-config now takes effect on
  the next cycle, logged as "collector: interval changed 60s -> 120s". Before, the collector
  kept its startup cadence, so the ledger, the Derived settings page and the self-cost figures
  reported an interval the process was not running (footprint-01).
- **OAuth agents.** MCP agents that sign in with OAuth now carry their sponsor, task_id and
  on-behalf-of subject into the policy gate. Their widening proposals record the sponsor, and
  the sponsor can no longer be one of the two approvers. Pending OAuth-agent proposals made
  before this fix have proposed_sponsor_id NULL and should be re-proposed or their approvers
  checked by hand (agentgov-02).
- **MCP Tokens page.** The page no longer offers an agent token that the server always refused.
  It now issues operator tokens with a required owner, links to the Agents page for
  coding-agent tokens, and shows Principal, Owner and the agent's retired or frozen status in
  the list. docs/mcp.md now mints agent tokens through `POST /api/v1/agents/{id}/tokens`
  (agentgov-01).

#### Changed

- **Tuning-agent index drops** now cover only indexes another valid index covers, and are
  marked for approval when the database has standbys or its replication state cannot be read.
  Drops of never-scanned indexes come only from the `unused_index` rule, which has a first-seen
  clock and skips recently created and FK-only indexes (indexes-03).
- **Maintenance privileges.** VACUUM and autovacuum ALTER TABLE actions now check the connected
  role's privilege first (owner, superuser, database owner, or MAINTAIN/pg_maintain on
  PostgreSQL 17+; ALTER TABLE needs the owner). A role that lacks it is withheld with
  `capability_missing` and told which grant fixes it. A bloat VACUUM deferred for I/O
  saturation no longer carries SQL, and a VACUUM the server skipped is recorded as failed
  instead of neutral (vacuum-06).
- **Schema guard.** everything_text and type_tightening invariants are now recorded once as
  advisory recommendations ("requires an online column migration; pg_sage has no automated
  path"). They are no longer counted as routed and no longer propose the table-rewriting ALTER
  COLUMN ... TYPE, which now appears only as labelled reference evidence. Each structural
  invariant writes one new ledger row after upgrade (migration-09).
- **Environment variables.** A `SAGE_*` value that is set but cannot be parsed (for example
  `SAGE_PG_PORT=abc` or `SAGE_MCP_ENABLED=no`) now stops startup with one error naming every
  such variable; `0` and `false` now apply instead of being ignored; `SAGE_PROMETHEUS_PORT`
  keeps the configured metrics host (default `127.0.0.1`) instead of rebinding to `0.0.0.0`, so
  containers that publish metrics must pass `--prom-addr 0.0.0.0:9187`; `docs/configuration.md`
  now lists every environment variable the sidecar reads, generated from a registry. Check your
  `SAGE_*` values before upgrading (config-08).
- **Sage SRE evidence age.** An investigation resumed on evidence more than 5 minutes old now
  ends expired (`stale_evidence`) instead of concluding on it, and every conclusion records and
  shows the age of its evidence (sre-02).
- **Briefing grounding.** The LLM rewrite now cites findings by id and links them to their
  case. Any SQL, setting change or command that is not a pg_sage recommendation is removed
  before the briefing is stored or sent. `sage.briefings.content_json` now stores the grounded
  payload (llm-08).
- **Toolchain and CI.** Release binaries and images now build on Go 1.27.3 (go.mod toolchain
  line), Alpine 3.24 (digest-pinned) and Node.js 22 LTS. A new CI security job runs
  govulncheck, npm audit and a Trivy image scan, and must pass before the image or a release is
  published. The default CI token is read-only, Dependabot watches Go, npm, Actions and Docker,
  `--version` now reports the Go release, and building from source needs Go 1.26+ and Node.js
  22+ (ci-01).
- **Container HEALTHCHECK.** The check now runs `sage-sidecar healthcheck`. It follows
  `api.listen_addr` (from `SAGE_CONFIG_PATH` or `./config.yaml`) and switches to HTTPS when
  `SAGE_TLS_CERT`/`SAGE_TLS_KEY` are set, so TLS deployments and non-default ports no longer
  read as unhealthy. `curl` is gone from the image. If you pass a config file with `--config`
  in the container args, name it in `SAGE_CONFIG_PATH` instead, or override the check with
  `--addr` (ci-06).

#### Added

- **User deactivation.** Admins can now deactivate a user (Users page, or `PUT
  /api/v1/users/{id}/status`). Deactivation is the recommended way to offboard someone: it ends
  their sessions, revokes their operator MCP tokens, and blocks their password, SSO, link-grant
  and chat sign-ins until reactivated. A user who changed configuration can now be deleted,
  where before the delete failed with a 500. Each config change also records the caller's email
  in `sage.config_audit.changed_by_actor` (apiauth-01).

### v2.6.0 (tranche T1)

#### Upgrade notes

- Secrets at rest are sealed by default: back up `data_dir/encryption.key` (in the image, the
  `/var/lib/pg_sage` volume) or set `encryption_key` (security-08).
- A private Slack or webhook relay needs `notification_policy.allow_private_targets: true`;
  otherwise an enabled `alerting` block stops startup naming the key (alerting-11).
- An enabled `alerting` block that would send nothing is refused at startup and on reload; fix
  the key the error names (alerting-08).
- A zero or negative `analyzer.unused_index_window_days`, `safety.ddl_timeout_seconds`,
  interval or rollback bound now fails to load the config file instead of becoming 0
  (config-02).
- `mode: standalone` with `meta_db` / `--meta-db` is a startup error: leave `mode` unset or set
  `mode: meta` (config-10).
- An explicit `trust.level` or `trust.tier3_*` in YAML, env or a flag now holds against a
  stored UI override; the more restrictive value applies (config-03).
- Accounts whose emails differ only in case cannot sign in until an operator keeps one account
  per email and restarts (finding `auth_email_case_duplicates`, apiauth-05).
- Moving `llm.endpoint`, `clone.dle_endpoint` or an SMTP relay to a new origin needs its
  secret re-entered, and a YAML optimizer endpoint on another origin needs its own
  `llm.optimizer_llm.api_key` (security-03).
- MCP clients: an unknown or mistyped argument is now error -32602, and "retry later" is
  -32010 (mcp-08).
- Evidence pack consumers that check `format` must accept `pg_sage-evidence/2` (audit-07).
- `clone.provider: snapshot` is rejected at startup; use `none`, `dle` or `lakebase_branch`
  (migration-07).
- A bare or multi-table VACUUM and `REINDEX SCHEMA|DATABASE CONCURRENTLY` are parked as
  `no_typed_contract` (executor-08).
- Slot facts rejected before this release stay registered until you act: docs/facts.md has
  the SQL to find them (facts-04).
- Six metric families are deprecated and removed in v2.7.0: move dashboards and alerts to the
  replacements in docs/deployment.md (observability-02).
- The metrics listener logs an ERROR and exports `pg_sage_metrics_listener_up 0` when it cannot
  bind (9187 is also postgres_exporter's port); the default moves to 8187 in v2.7.0
  (observability-08).
- Read routes now refuse roles other than admin, operator and viewer (apiauth-09).

#### Security

- **Qualified expression indexes.** Expression-index recommendations now name each function's
  schema (`pg_catalog.lower(email)`), and the executor refuses, before building, any CREATE
  INDEX that has an unqualified table, a bare call that is not a pg_catalog function, or a
  schema-qualified call into a schema holding a non-extension function of that name
  (`function_schema_not_allowed`). This closes the planted `public.lower()` (CVE-2018-1058
  class) path for explicitly qualified calls (security-01).
- **Typed execution.** Statements now run top-level, or in a transaction under the policy lock
  ceiling, based on their typed action, not on keywords: a table named `concurrently_log` no
  longer gets the CONCURRENTLY lock waiver. A bare, option-only or multi-table VACUUM and
  `REINDEX SCHEMA|DATABASE CONCURRENTLY` no longer have a contract and are parked as
  `no_typed_contract`. A self-initiated finding or custodian proposal whose statement changes a
  different relation than the one it names is blocked with the new reason `target_mismatch`
  (executor-08).
- **Chat cancel and terminate.** Sage SRE cancel and terminate requests in Slack and Telegram
  now use approval cards. Each card is bound to the channel it was sent to, to its content and
  to the target backend (pid plus backend_start). It expires after at most 15 minutes and has
  no Snooze button. A chat Reject marks the proposal denied at once. Old proposal buttons only
  reply "This button is outdated; open pg_sage to decide" and change nothing (policy-08).
- **Investigator EXPLAIN.** The Sage SRE investigator's plan-only EXPLAIN now waits for the
  database's probe slot like any probe (concurrency_limit when the queue is full), with its own
  pool-acquire budget. It takes statement text from the collector's in-memory snapshot instead
  of loading pg_stat_statements' whole query-text file (a statement outside the top queries is
  unsupported/text_not_cached). It refuses data-modifying CTEs, FOR UPDATE/SHARE and SELECT
  INTO before PREPARE can take locks, and reports a role without SELECT on the statement's
  tables as no_privilege/missing_select (sre-07).
- **Grounded fact proposals.** A model-proposed fact is now dropped (and logged as ungrounded)
  unless the evidence it cites is about its own subject: the schema's summary, the slot itself,
  or the index's drop history, and two matching objects for a `*` pattern. Because the model is
  shown no table evidence, it no longer proposes table facts such as append_only, table_window
  or table ownership (facts-05).
- **Fact event chain.** Every proposal, confirmation, rejection, declaration, expiry, reopen
  and restore of a binding fact is now recorded in the append-only, hash-chained
  `sage.fact_events` (verified by /api/v1/audit/verify, exported to SIEM, kept after the fact
  is purged), `expired_by` names who expired a fact, and the Facts page asks for confirmation
  before revoking or expiring a confirmed fact (facts-07).
- **Agent token audit.** Agent and MCP token governance is now audited: creating, changing,
  widening (single-operator mode), retiring an agent, minting an agent token, and creating or
  revoking an MCP token each write one hash-chained sage.auth_audit event in the same
  transaction, and the change fails if the event cannot be written. An agent's sponsor must be
  an enabled operator or admin (422 otherwise), cannot change while the agent has an open
  freeze (409 sponsor_change_frozen), and an agent whose sponsor is now a viewer or deactivated
  becomes unsponsored (agent_* tools answer agent_unsponsored) (agentgov-05).
- **Inherited posture.** AP-03, AP-04, AP-05, AP-06 and AP-09 now follow inherited role
  membership (rolinherit before PostgreSQL 16, the per-grant INHERIT option on 16+). A table,
  policy, function, schema USAGE, server or language granted to a group role that anon or
  another exposed role inherits is now reported, with the membership path and a fix that
  revokes from the group or ends the membership (agentgov-10).
- **Mutation census.** Every place pg_sage's shipped code can write to a database it connects
  to is now listed in `sidecar/internal/mutationcensus/sinks.tsv`, each with a class (gated by
  an executor contract, sage schema only, clone only, or out of gate with a reason). CI fails
  when a new write path is not classified. docs/security.md gains a "Writes to the monitored
  database" section (security-06).
- **Model-bound root authority.** Model-root authority is now bound to the model that earned
  it: a PGIncidentBench measurement lets the model override the causal graph's root only while
  the database's llm.model is the model the report's llm.model names (trimmed, lowercased,
  provider prefix stripped). A report without llm.model grants nothing, and a hot reload to
  another model makes the next investigation advisory and records root_authority_revoked.
  Existing grants turn advisory until a report naming the running model is ingested. GET
  /api/v1/model-lift now shows measured_model and running_model (llm-01).
- **Numeric redaction.** Numbers of 6 or more digits that are compared (`=`, `<>`, `<`, `>`,
  `IN (...)`, `ANY(ARRAY[...])`, `BETWEEN`, and `Key (c)=(...)` in logs) are now replaced with
  `?` in query text, plan filters and log messages before they reach a model. Short numbers,
  `LIMIT`/`OFFSET` values and plan row counts and costs are kept. You can turn this off with
  the new `llm.redact_numeric_literals` (default `true`, applies on config reload without a
  restart). Every model call site is now checked at build time for the untrusted-data rule, and
  Ask Sage's prompt now carries the shared rule (llm-09).
- **Rehearsal clones.** Rehearsal clones are now receipted before creation, reaped by the fleet
  leader after a crash, bounded to one hour, and refused when the clone is the production
  server; `clone.provider: snapshot`, which never worked, is now rejected at startup
  (migration-07).
- **Alerting egress.** Alerting Slack and webhook URLs, and PagerDuty delivery, now obey
  `notification_policy`. Plain http, loopback, RFC 1918, ULA and CGNAT targets need
  `notification_policy.allow_private_targets: true`, and link-local or metadata hosts are
  always refused. When alerting is enabled, a refused URL stops startup or a config reload with
  an error naming the key. The settings API answers 400 for a blocked
  `alerting.slack_webhook_url`. Every connection and redirect is re-checked, and alert traffic
  no longer uses HTTPS_PROXY. Operators with a private Slack or webhook relay must set
  `allow_private_targets` (alerting-11).
- **SIEM TLS.** SIEM sinks gain a per-sink `ca_file` (pins the receiver to that CA) and
  `client_cert_file`/`client_key_file` (mutual TLS) for https http/otlp and syslog `tls`. Each
  `chains` entry must now be a real audit chain, so a typo such as `action-log` stops the
  config from loading. A `token_env` token over plain `http://` logs a WARN at start and shows
  `warnings: ["token_over_http"]` in `GET /api/v1/audit/siem`; set `allow_insecure_http: true`
  to accept it on purpose, because the next minor release will refuse this config. udp syslog
  is reported as `delivery: best_effort` and warns at start. Syslog over TLS 1.3 no longer
  loses an event when the receiver refuses the client certificate (audit-08).
- **Email case.** User emails are now one identity whatever their case. They are stored trimmed
  and lower-case under a new unique index on lower(email), and every sign-in path matches
  case-insensitively. POST /api/v1/users rejects malformed addresses (400) and case variants
  (409). If the upgrade finds accounts whose emails differ only in case, it changes nothing: it
  logs an ERROR and opens the critical finding auth_email_case_duplicates naming the user ids,
  and those emails cannot sign in until an operator keeps one account per email and restarts
  (apiauth-05).
- **Route authorization.** API routes now declare their authorization when they are registered
  (public, self-authenticating, viewer, viewer-write, operator, admin). Read routes that relied
  on the session check alone now also refuse roles other than admin/operator/viewer. Paths with
  `..`, `.` or encoded-dot segments no longer skip session authentication (apiauth-09).
- **API hardening.** API responses now carry Cache-Control: no-store. Mutating requests and
  401/403 refusals are written to a structured access log (api.access_log, default on). A
  database the API cannot reach returns 503 db_unavailable with Retry-After instead of 500. API
  TLS certificate expiry is re-checked hourly: a warning under 14 days, an error under 3, the
  new pg_sage_api_tls_cert_not_after_seconds and pg_sage_api_tls_reload_failures_total metrics,
  and tls_expiring in the /ready details (apiauth-12).
- **Security policy docs.** SECURITY.md now lists 2.5.x as the supported line and asks for a
  dedicated non-superuser role. docs/security.md replaces the "never" list with what pg_sage
  can read and change, each change linked to its gate, and documents the MCP tool and scope
  model. The grant comments now say that CREATE INDEX and CREATE STATISTICS need table
  ownership. A test fails when SECURITY.md's supported row drifts from VERSION (security-07).
- **Outbound targets.** Every outbound client (LLM, alerting, notifications, SIEM, Prometheus,
  DLE, OAuth, cloud telemetry) now refuses link-local and cloud-metadata addresses
  (169.254.0.0/16, fe80::/10, fd00:ec2::254), multicast and unspecified targets. Moving
  llm.endpoint, clone.dle_endpoint or an email channel's SMTP relay to a new origin now
  requires re-entering its secret. A YAML optimizer endpoint on another origin needs its own
  llm.optimizer_llm.api_key (security-03).
- **Secrets at rest.** Secrets at rest are now sealed by default. Without encryption_key,
  pg_sage generates data_dir/encryption.key (mode 600; the image sets
  SAGE_DATA_DIR=/var/lib/pg_sage as a volume), so back that file up. Database passwords and
  notification channel secrets are bound to their row and are re-sealed on rotation through
  encryption_key_previous. A lost key file is reported, not replaced. Admins see a dashboard
  banner, and GET /api/v1/security/posture reports the state, whenever secrets are plaintext or
  the key is missing (security-08).
- **Masked overrides.** Settings overrides no longer silently beat the config file: an explicit
  `trust.level` or `trust.tier3_*` in YAML, env or a flag now holds against a stored UI
  override (the more restrictive value applies), every override that masks an explicit file
  value is logged once and listed in `masked_by_override` on `GET /api/v1/config/global` and
  badged "masked" in Settings, non-override values are labelled `file/default` instead of
  `yaml`, and the docs state the real order (override > CLI > env > YAML > default)
  (config-03).
- **Databricks identity pin.** In Databricks Apps, pg_sage now pins each account to the
  X-Forwarded-User value it was first seen with, stored as a SHA-256 hash in the new
  sage.users.databricks_user_pin column. If the value changes,
  `SAGE_DATABRICKS_APP_IDENTITY_PIN` decides what happens: `log` (default) writes a
  `databricks_app_identity_changed` audit event, `enforce` also refuses with 403, `off`
  disables the check. Admins can relink an account with `POST
  /api/v1/users/{id}/identity-pin/reset` (databricks-07).
- **Bench signer identity.** The sidecar now accepts release signatures only from the pg_sage
  repository identified by its immutable GitHub repository and owner IDs
  (1185895123/143746527), so a repository later registered under the same name cannot sign
  reports that sidecars accept; the CI signing jobs refuse to sign from any other repository ID
  (benches-10).

#### Fixed

- **Unavailable collector reads.** A failed read of io, prepared_xacts, partitions or
  config_data is now recorded as unavailable (unknown), not stored as an empty observation.
  These extras share the cycle's timeout retry. The config snapshot fails as a whole and names
  the failed sub-read. Standbys report a WAL position. Prepared transactions are persisted to
  sage.snapshots and served by the snapshot API. RCA keeps orphaned-2PC incidents open, and the
  optimizer pauses index recommendations, while the category they need is unknown
  (collection-05).
- **Temporary tables.** Other sessions' temporary tables and indexes (pg_temp_N), and pg_sage's
  own, are no longer collected as application tables, so no more VACUUM or unused-index
  findings on objects nobody else can touch, and no snapshot churn when they vanish. Unlogged
  tables are still collected (collection-03).
- **Late pg_stat_statements.** pg_stat_statements created after pg_sage starts (or dropped and
  re-created, or updated) is picked up on the next cycle with WAL, plan time and block I/O
  columns, no restart needed. A major-version upgrade behind the same endpoint no longer stops
  collection: the system statistics follow the live server version and fall back in both
  directions (collection-02).
- **Partition plan fingerprints.** Plan fingerprints (planhash v2) now name partitions by their
  partition root. Queries on time-partitioned tables no longer report a plan flip at every
  partition boundary, and plan_regressions v2 does not count the v1-to-v2 fingerprint change as
  a flip (collection-09).
- **Meta-mode history reads.** History reads in meta mode now fail closed: with `history.store:
  meta`, a history read on a database pool that no runtime registered fails with an error
  (`histstore.ErrNotRegistered`) instead of reading that database's own, wrong history. The SLO
  latency proxy now also counts a drop in total query time as a statistics reset, and
  docs/architecture.md describes the collector as the code actually works (collection-12).
- **Findings filters.** The Advisor and Optimizer source filters now return their findings
  (they were always empty), Rules now includes wraparound_freeze and six other analyzer
  categories, Forecaster includes storage_forecast, and each finding row reports action_type,
  contract_tier, action_risk (the stricter of the producer's and the executor contract's risk;
  high_risk is shown as high), approval_required and approval_reason, which the Actions tab now
  shows (rules-07).
- **Unused-index clock.** The zero-scan clock now survives sidecar restarts (new table
  sage.unused_index_clock). A restart no longer resolves open findings or restarts the 7-day
  window, so findings keep their id, history and occurrence count. Open findings from before
  the upgrade take their clock from snapshot history, or stay open until the clock reaches the
  window (rules-04).
- **Findings writes and reopens.** A failed findings write no longer stops the cycle (the
  remaining findings are still saved), and suppressed findings are never published. A condition
  that persists after pg_sage acts now reopens the same finding with
  `detail.persisted_after_action` and pages once, instead of opening a duplicate after a
  2-minute grace. The window is 2 × max(analyzer, collector interval), 20 minutes by default
  (rules-06).
- **Partitioned index rules.** Index rules are partition-aware: indexes attached to a
  partitioned index are no longer flagged for DROP (PostgreSQL rejects it). An unused
  partitioned index now gives one advisory finding with a manual DROP that needs approval. A
  missing FK index on a partitioned table gets a step-by-step partition plan instead of CREATE
  INDEX CONCURRENTLY. Open findings on attached partition indexes resolve once after the
  upgrade (rules-03).
- **Unmeasured database size.** A day without a measured database size (pg_database_size not
  yet run, timed out, or refused) no longer stops the disk, connection, cache and checkpoint
  forecasts for the whole lookback; unmeasured days are left out of the disk trend, and with
  fewer than 7 measured days an open disk finding is kept rather than resolved (forecast-07).
- **Stale runway findings.** Forecast (runway) findings no longer resolve, then reopen as new
  findings with a new case and notification, when their series is unmeasured (too few samples,
  too short a span, or no samples after a gap longer than sre.runways.lookback_hours) or when
  the trends read hits its row cap. Such a finding stays open with detail.stale, stale_reason
  and stale_since until it is measured again; a dropped slot or sequence still resolves
  (forecast-02).
- **Startup visibility checks.** Startup checks now decide whether query text is visible from
  pg_read_all_stats membership (INHERIT on PostgreSQL 15+, membership on 14), the same test
  pg_stat_statements makes, so the "GRANT pg_read_all_stats" warning appears exactly when other
  roles' text is hidden. A failed WAL or planning-time column check is logged as a WARN with
  its cause and effect instead of silently turning the columns off. pg_stat_statements
  installed in a schema outside the role's search_path is found and read qualified instead of
  degrading monitoring (firstlook-08).
- **Quoted suggested SQL.** Suggested SQL (first look, agent posture fix scripts, grant
  guidance, tuning) now quotes every name the server's quote_ident would quote, so tables,
  columns, schemas or roles named like key words (to, desc, window, user, left, int) give SQL
  that runs when pasted. The self-managed auto_explain step now adds auto_explain to
  session_preload_libraries instead of replacing the libraries already there (firstlook-10).
- **Fleet onboarding.** One unreachable database no longer blanks Getting started and First
  look. /api/v1/onboarding and /api/v1/first-look now return every database, marking the ones
  pg_sage could not read with an `error` code (`unreachable`, `no_runtime`, `read_failed`);
  each read is bounded to 3 s. The dashboard names the affected database, offers Retry when a
  read fails, and polls the checklist once a minute, stopping once setup is complete
  (firstlook-03).
- **Partitioned first look.** On partitioned tables, the index on the parent now stands for its
  partitions. It is "never scanned" only when its partitions' indexes have no scans in total. A
  duplicate or redundant parent is reported once, with DROP INDEX (not CONCURRENTLY) and the
  lock caveat, instead of once per partition with SQL PostgreSQL rejects. An invalid ON ONLY
  index names its unattached partitions and gives CREATE INDEX CONCURRENTLY plus ATTACH
  PARTITION instead of a REINDEX. Unindexed foreign keys on partitioned tables no longer get
  CREATE INDEX CONCURRENTLY (firstlook-04).
- **Self-configuration flapping.** Self-configuration no longer flaps on noisy databases. A
  candidate one step from the soaking value keeps the 24 h soak, and a derived value is not
  re-derived for a one-step move. The cost-sized keys promote the 90th percentile of the soak.
  The temp-file threshold is judged on the reactive detector's busiest window. A hold writes
  one ledger row per outcome. A derived value is clamped (new `clamped` ledger event) when
  other keys narrow its bounds, and is reset when its rule version changes. The new migration
  adds `sage.config_derived_setting.derivations` (footprint-04).
- **EXPLAIN cache key.** The EXPLAIN cache no longer answers one query with another's plan. The
  cache is now keyed on the exact statement text, including literals, comments and planner
  hints. `$N` inside literals, dollar quotes or comments is no longer treated as a bind
  parameter. `$0` and parameters above `$65535` are rejected as invalid requests. Old cache
  rows expire within the TTL, so no migration is needed (tuning-07).
- **One hint per statement.** One pg_hint_plan hint per statement is now enforced against
  sage.query_hints and hint_plan.hints on every proposal, not only the in-memory cooldown.
  pg_sage no longer proposes over an installed or operator-installed hint: an operator hint
  gets an informational "operator hint present" finding with no SQL. The apply INSERT now fails
  with a unique violation instead of silently doing nothing over an existing row, and hints
  stay advisory unless hint_plan.hints has pg_hint_plan 1.7's (query_id, application_name)
  unique index (tuning-02).
- **Query tuning resolution.** Open query_tuning findings now resolve. A finding resolves once
  its hint is installed, retired, rolled back, rejected or expired, or once its statement has
  been absent from pg_stat_statements, or below the tuner's admission thresholds, for
  `tuner.hint_absent_cycles` cycles (default 3). Hint proposals an operator rejected become
  `rejected`, and proposals unanswered for 30 days become `expired`. Neither blocks a new hint
  for the statement. No proposal is recorded while hints cannot reach application sessions.
  `sage.query_hints.status` gains a NOT VALID CHECK (tuning-06).
- **Index extension check.** The extension check now goes by the index's operator classes and
  its geometry/geography key columns, not by substrings. Trigram GiST indexes, btree_gist
  classes and index names containing "st_" or "trgm" are no longer rejected as needing PostGIS
  or pg_trgm. The pg_extension read is bounded, a failed read no longer counts as "not
  installed", and a rejection names each missing extension with its CREATE EXTENSION command
  (indexes-08).
- **HypoPG what-if.** HypoPG what-if evidence no longer gets stuck. An extension installed
  while pg_sage runs is noticed within 10 minutes, with no restart. GIN, GiST, SP-GiST, HNSW
  and IVFFlat index recommendations are marked unverified with the method named (HNSW and
  IVFFlat also point to `pg_sage vector-lab`) instead of showing "what-if evaluation failed".
  Lowering `optimizer.hypopg_min_improvement_pct` re-measures remembered rejections. A partial
  measurement's reason and approval card show the planned improvement and the unplanned
  queryids (`what_if_measured`, `what_if_unplanned_queryids`) (indexes-06).
- **Schema attribution.** Index advice now attributes workload statements to the right table
  outside `public` (for example an app on `search_path=app`), including quoted mixed-case
  names, every relation of a FROM list and `FROM ONLY`, so those tables get HypoPG-verified
  recommendations with affected queryids. A name held by both `public` and another schema still
  resolves to `public` (indexes-04).
- **Managed configuration scope.** Managed clouds (RDS, Aurora, Cloud SQL, AlloyDB, Azure,
  Neon, Supabase, Lakebase): configuration findings now use ALTER DATABASE only when the
  setting's pg_settings context allows it. That means user context, or superuser context when
  the role holds SET on the parameter (PG15+). Settings such as max_wal_size, checkpoint_* and
  the autovacuum_* instance knobs now become managed proposals or provider guidance, not SQL
  PostgreSQL refuses. The executor refuses a stale ALTER DATABASE for them on every managed
  provider. Neon/Supabase database-scope changes no longer need an LLM-supplied rollback.
  Approval counts on managed providers drop, and managed proposals appear instead
  (advisors-01).
- **Configuration advisor findings.** Open WAL and connection findings now resolve once the
  live setting equals the recommendation (units normalized), rewrite findings after their query
  leaves pg_stat_statements for 3 cycles, and bloat findings after 2 cycles under the floor. An
  open finding now holds back only its own setting, query or table instead of the whole
  sub-advisor. Each sub-advisor's schedule is kept in sage.advisor_runs, so a restart makes no
  extra model call and a failure retries after 1h, doubling up to advisor.interval_seconds.
  advisor_degraded resolves when the LLM comes back. Global GUC advice is kept next to
  per-table overrides and lists them in detail.overridden_by_tables. The first run after
  upgrade resolves WAL findings that were already applied (advisors-02).
- **WAL prompt units.** The WAL prompt now shows page-counted settings such as wal_buffers in
  byte units ("4MB", not "5128kB"), and a config change withheld on Neon, Supabase or Lakebase
  now names the setting and the provider (advisors-10).
- **Inconclusive recommendations.** A recommendation whose action had a neutral or failed
  verification (outcome success with an unverifiable/failed verification verdict) is now marked
  "inconclusive" instead of "verified", matching the Cases timeline; it stays "verifying" while
  its verification is pending, extended or about to be recorded. Recommendations already marked
  verified are not backfilled (cases-03).
- **Interrupted rollbacks.** A rollback interrupted by a crash no longer stays `rolling_back`
  for ever. Each cycle, the primary settles any such claim older than
  `safety.ddl_timeout_seconds` plus one minute by reading the catalog, with no DDL:
  `rolled_back` if the rollback took effect, otherwise a retryable `rollback_failed`
  ("interrupted during rollback"). Outcome writes are retried with backoff and logged when they
  still fail. The Actions page now offers "Retry rollback" and shows the reason on
  `rollback_skipped` and `rollback_failed` actions (executor-07).
- **CONCURRENTLY build remnants.** Failed CONCURRENTLY builds no longer leave INVALID indexes
  behind, and long builds get their own timeouts. When a `CREATE INDEX CONCURRENTLY` or
  `REINDEX ... CONCURRENTLY` fails, pg_sage now drops the INVALID index it left, but only after
  confirming it is that build's own object (same OID, still INVALID, same table, no build
  running on it). A VALID index is never dropped. The outcome is recorded as `remnant_cleanup`
  on the action, and the build is retried on its next eligible cycle. A build withheld because
  its index name was taken is parked until that name is gone. New settings
  `safety.index_build_timeout_seconds`, `safety.reindex_timeout_seconds` and
  `safety.vacuum_timeout_seconds` (default 14400 s, range 60-86400 s) set the statement timeout
  for those operations; all other DDL keeps `safety.ddl_timeout_seconds`. Operator-approved
  CREATE INDEX over an INVALID index of the same name is now refused with the command that
  clears it, instead of dropping that index by name (executor-05).
- **Fleet canary verdicts.** Each database's change is now judged on that database's own
  verification verdict instead of after a fixed 60 s settle. Before, every canary of a
  reversible action halted and rolled back. The wait ends at the action's monitor cap plus 2
  minutes; a verdict still open then halts as `verification_pending` and nothing is rolled
  back. The source action must be verified `success`. A rollout error now rolls back every
  changed database; one whose rollback also fails is recorded `applied_unverified`.
  `sre.autonomy.canary.settle_seconds` is removed: it is ignored with a startup warning for one
  release (executor-04).
- **Snoozed approval cards.** When one snoozed card fails to re-send, the others are still sent
  and the failed one is retried after 5 minutes. A twice-snoozed action now posts its verdict
  once per chat. Follow-ups still waiting for a verdict no longer hold up newer cards: the new
  next_check_at column replaces the follow-up index (policy-06).
- **Freeze measurement.** A custodian freeze of a table that is red because of its own
  autovacuum_freeze_max_age reloption, or because of its TOAST relation, is now measured the
  way the custodian ranked it. Its wraparound near miss is credited and the post-check checks
  the right horizon. A freeze withheld because the sidecar role lacks the VACUUM privilege now
  reports that missing privilege (vacuum-09).
- **On-demand EXPLAIN lock waits.** The auto_explain collector's on-demand EXPLAIN now gives up
  after 100 ms on a busy relation lock (the SRE probes' lock_timeout) and sets a 10 s
  idle-in-transaction timeout. Before, it could wait up to 5 s per candidate behind a queued
  ALTER TABLE while holding its other locks. Lock-busy candidates are skipped until the next
  cycle and summarised in one INFO line ("N candidates skipped: lock busy") instead of a WARN
  each (rca-05).
- **Log RCA messages.** Log-based RCA now recognises the messages PostgreSQL actually writes
  for backend crashes (kill -9, segfault), replica WAL gaps and replication-slot invalidation
  on PG 14-18. XX000 internal errors such as "cache lookup failed" are reported as a warning
  (log_internal_error) instead of critical data corruption. New incidents cover the XID
  wraparound outage and the MultiXact wraparound warning and outage. pg_sage warns at startup
  when lc_messages is not C or English (rca-02).
- **RCA rate signals.** RCA rate signals now compare the last collector interval with the one
  before. wal_growth_spike uses the cluster's WAL rate from the WAL insert position, against
  wal_spike_multiplier with a 1 MiB/min floor; it gives no signal on the first interval, after
  a failover or on a standby, and only the cluster owner reports it. cache_hit_ratio_drop uses
  the interval hit ratio. The cache tree compares reads per call. idle_in_tx_elevated uses the
  longest idle session, not the average. The connection-storm branch counts sessions opened per
  minute (pg_stat_database.sessions). These signals no longer need the LLM advisor's
  configuration snapshot (rca-03).
- **Fleet RCA.** A cluster-wide event (PANIC, disk full, archiver, connection saturation,
  replication lag) now opens one incident on the cluster's owner database (lowest database name
  per host:port), not one per database. Connection saturation counts client backends. A
  database removed at runtime frees its log buffer, and ownership moves to the next database
  (rca-08).
- **Thinking-model investigations.** SRE investigations on thinking models (Gemini 2.5/3,
  o-series, R1) no longer end without a conclusion near their token cap. The loop now counts
  the 2,048 reasoning tokens each call reserves, so it asks for the conclusion while that call
  still fits. If the investigation's own cap refuses a call anyway (for example on a resumed
  run), the call is retried once as conclusion-only. Daily-allocation refusals still stop the
  run (sre-04).
- **Investigation retries.** When an investigation of a still-open incident, finding or
  detector episode expires (it waited more than 10 minutes unclaimed, or resumed on evidence
  too old to conclude), the next trigger poll now starts a new attempt, up to 5 per trigger.
  The Cases panel and the exports link the attempts. Stop is a pause that never expires, and
  every queue expiry is recorded on the investigation's event chain (sre-01).
- **Playbook selection.** Incident cases now pick their playbook from the signal ids RCA
  actually emits. Replication lag, WAL growth, inactive-slot and removed-WAL-segment incidents
  get the read-only WAL diagnostic; replication conflicts get the standby diagnostic;
  slow-query and statement-timeout incidents get the runaway playbook;
  autovacuum-falling-behind gets vacuum pressure; wraparound, prepared-transaction and
  xmin-holder incidents get the freeze-blocker diagnostic; and out-of-memory incidents no
  longer get a connection diagnostic. Sequence-exhaustion, inactive-slot, replication-lag and
  runaway-query findings now carry their reviewed script or read-only diagnostic (never a slot
  drop). Shadow-mode candidate counts will rise accordingly (playbooks-02).
- **SLO burn rules.** A burn-rate rule whose known window is below its factor is now "not
  firing" (Kleene AND) instead of unknown when the other window only has low traffic, zero
  events or partial coverage, so a quiet low-traffic service no longer goes unknown or caps
  autonomy at L1. Stale, absent or failed data still keeps it unknown. Prometheus SLOs accept
  an optional `sre.slo.objectives[].coverage_query` that marks windows `partial_window` or
  `stale` like pushed series; without it the SLO reports `coverage_unverified` and the SLOs
  page shows a "coverage unverified" badge (playbooks-07).
- **Stuck maintenance runs.** A statistics run or rollback that crashed (or failed to write its
  result) is no longer stuck in `executing`/`rolling_back`. After 13 minutes by default (twice
  the apply timeout plus one minute), the action pass checks the catalog and the action log
  under the table lock. It then settles the run as `uncertain` (rollback available), `failed`,
  `rolled_back` or a retryable `rollback_failed`. It never re-runs the SQL and never claims an
  object it cannot prove (playbooks-05).
- **Fleet Cases.** Fleet Cases no longer goes blank when one database is down or slow: each
  database is read concurrently under a 5 s deadline. The response lists unreadable databases
  in `unavailable` (timeout, connection or query) and per-source cuts in `truncated`. `?case=`
  loads a case the list cap dropped, and `actions_unavailable` marks cases whose action history
  could not be read. A request for one unreadable database now answers 503 instead of 500
  (cases-05).
- **Partitioned describe_table.** Ask Sage's describe_table now reports a partitioned table's
  partition count and key, sizes and row estimates summed over its partitions, and their oldest
  and newest vacuum and analyze times, instead of "0 bytes total, about 0 rows, never
  vacuumed". A table that was never analyzed says "row estimate unavailable". An unquoted
  mixed-case name such as public.Orders finds orders. The read is bounded by a 2 s statement
  timeout and a 1 s lock timeout (mcp-03).
- **Fact citation aliases.** Ask Sage's propose_fact now accepts the E-aliases (E1, E2, ...)
  the model is told to cite and records each one's durable id and digest; durable ids still
  work, and anything not read in the turn is still refused (mcp-02).
- **Live investigation limits.** The specialist contract's live-investigation limits
  (max_open_per_identity, max_open_total) no longer forget running investigations while their
  database is reloading, reconnecting or briefly missing from the fleet. Such an investigation
  still counts as live until a read shows it ended, or until the new
  specialist.max_live_age_hours (default 24, 1-168) passes for one that stays unreadable
  (mcp-10).
- **MCP docs line endings.** The MCP tool-reference docs test no longer fails on Windows
  checkouts with core.autocrlf=true: docs/mcp.md is now pinned to LF line endings (run `git
  checkout -- docs/mcp.md` once on an existing checkout) (mcp-11).
- **MCP stdio.** MCP stdio no longer stops for the rest of the process on a message over 4 MiB:
  the line is rejected with -32600 and serving continues. Each tool call now runs on its own
  (up to 4 at once) under the new `mcp.call_timeout_seconds` (default 90, 5-600, -32800 on
  timeout), and `notifications/cancelled` cancels a running call (mcp-04).
- **Fact reads and prompts.** A confirm, reject or expire is no longer hidden for up to 10 s by
  a confirmed-facts read already in flight, and model prompts now keep slot, window and
  append-only facts first when the 12-fact/1,500-character budget cuts, ending with "N more
  confirmed facts omitted." (facts-11).
- **Fixture fact patterns.** The first look no longer proposes a test-fixture family pattern
  (for example test_memory_*) while a live test-named schema matches that pattern; it now uses
  the same rule as the fixture detector. docs/facts.md now names the surfaces that carry and
  propose facts, and covers partitioned tables and the in-memory absence grace (facts-12).
- **Rejected slot facts.** Rejecting or expiring a slot_consumer fact now unregisters the slot
  in sage.slot_consumer_registry, so the WAL custodian stops treating it as a protected
  consumer. Rejected facts are kept as tombstones and are no longer purged after
  retention.actions_days, so a purge can no longer turn a rejection back into a confirmed fact.
  Slots rejected before this release stay registered until you act: docs/facts.md has the SQL
  to find them (facts-04).
- **Agent posture churn.** Agent posture no longer re-runs on temporary-table churn, reports
  catalog reads that hit their row limit (50,000 for AP-01..AP-08, 5,000 for AP-09..AP-16) in
  the check note and a warning, and keeps a truncated detector's open findings open. In schemas
  with more than 20 exposed objects, the per-object AP-03, AP-05 and AP-06 findings resolve
  once and are replaced by one schema finding with a schema-wide fix script (agentgov-09).
- **Truncated answers.** LLM answers cut at the model's token limit: truncated prose is no
  longer rewritten into a JSON fragment; truncated JSON objects (not only arrays) are repaired
  with their wrapper intact; a briefing cut at the limit is stored as written with "_(cut at
  the model's token limit)_" appended and a WARN logged (llm-07).
- **LLM token cap.** The daily LLM token cap is now enforced as written. Resets and the UTC day
  turn no longer drop the holds of calls still in flight, so pg_sage can no longer over-admit,
  and a fleet budget reset during a call is charged correctly. The policy cap
  `budgets.llm_tokens_daily` now applies: each client's limit is the stricter of the policy and
  `llm.token_budget_daily`. The new hot-reloadable key `llm.global_token_budget_daily` caps the
  sum across all clients. Its default of 0 derives the cap from the fleet budget, or from the
  sum of each client's budget, so default spend is unchanged. `/api/v1/llm/status` shows the
  cap as `global` (llm-03).
- **Blocked migrations.** An MCP apply_migration that the policy gate blocks is now recorded in
  sage.migration_run as verdict 'blocked' with the gate's reason, instead of failing with a
  CHECK violation. Every recommend-only plan without gate evidence now gets its own evidence
  id, and a rewrite of an existing row refreshes its source_sql_hash and reason. This adds a
  one-time schema migration on sage.migration_run (migration-03).
- **Schema guard failures.** A failing remediation route (for example an FK index build hitting
  lock_timeout on a hot table) is now parked with its error class and the scan continues to the
  other invariants; the failure is recorded once per class instead of every cycle, a connection
  failure stops the scan without re-recording unreached invariants, and after 3 consecutive
  failures the route is paused for 6 scans (migration-08).
- **Migration advisor counts.** Findings seen by both the activity poller and the log detector
  at the same moment are now all counted, where some were lost to a duplicate-key error. Stale
  migration findings are resolved once per interval per database instead of twice. VACUUM FULL
  and CLUSTER advice mentions pg_repack only when the server's pg_available_extensions lists
  it. `lint_migration` accepts `pg_version` as a major (14-18) or a server_version_num
  (140000-189999) and rejects anything else. The unused `migration.managed_service` key is
  retired: configs that still set it load with one warning (migration-12).
- **LLM budgets across restarts.** LLM daily budgets now survive restarts: today's spend for
  each client, the global ceiling and each fleet database's share is written to
  `sage.llm_usage` in the control database every 60 s and on shutdown, and is restored at
  startup. A deploy, crash loop or `/restart` mid-day no longer grants the full daily budget
  again. `GET /api/v1/llm/status` reports `usage_restored_at` and `ledger_last_flush_at`. If
  the control database is unreachable, LLM calls continue on local accounting and one warning
  is logged per outage (fleet-10).
- **Per-channel resolves.** Resolve notifications are now tracked per channel. If PagerDuty
  fails while Slack succeeds, PagerDuty alone is retried, so its incident no longer stays open.
  Each failure leaves a `resolve_error` row in the Alert Log with the channel's error and is
  logged at ERROR. Retries stop after 5 failures per channel, and that last ERROR log says to
  close the alert by hand. Channels that never received the original alert no longer get a
  resolve (alerting-04).
- **Alert order.** Alerting now sends alerts critical first, then warning, then info, then any
  other severity by name, and resolve notifications follow the same order. Before, the order
  was random each cycle, so a critical page could wait behind every warning sent to a slow
  Slack (alerting-05).
- **Audit payload versions.** Audit chain verification now reports `unknown_version` for a link
  whose payload version is not the one pg_sage hashes rows with, instead of checking it with
  the wrong rules; canonical hashes and hashed column types are pinned in CI on PostgreSQL
  14-18 (audit-09).
- **SIEM isolation.** SIEM export now isolates failures per sink, source and chain: an
  unreachable fleet database or a failing chain no longer stops the others, each with its own
  backoff. An event a receiver refuses for good (HTTP 400/413/422, OTLP rejected records, an
  oversize syslog message) is replaced by a stub labelled "undeliverable" carrying its link
  hashes, with a siem_event_undeliverable warning, and GET /api/v1/audit/siem now lists
  per-lane cursor, head, lag and state (audit-05).
- **Session store outages.** Dashboard sessions survive control-database blips: when the
  session store cannot be reached the API now answers 503 `auth_store_unavailable` with
  `Retry-After: 5` (logged at most once per 30 s) instead of 401, and the dashboard shows a
  "Session store unavailable, retrying" banner or a boot retry screen rather than signing
  operators out (apiauth-02).
- **SSO discovery.** SSO no longer turns off until restart when OIDC discovery fails at
  startup: pg_sage retries on backoff (2 s, doubling to at most 5 min), re-reads the discovery
  document daily while keeping the last good keys, answers 503 `sso_not_ready` (not 404) and
  shows "SSO temporarily unavailable" on the login page while discovery has not yet succeeded,
  and exports `pg_sage_sso_discovery_ready` and `pg_sage_sso_discovery_failures_total`
  (apiauth-07).
- **Live update recovery.** Live updates now recover by themselves when a reconnect is refused
  (for example a proxy 502/504 while the sidecar restarts). A new stream opens after 3 s,
  backing off to 60 s with jitter. The header shows whether data is "Live", "Polling, updated N
  s ago" or "Offline, data from HH:MM" (dashboard-04).
- **Dashboard error messages.** Failed requests now show the server's own error message with
  its status (for example "database not found (404)") instead of a bare status code, error
  toasts stay on screen until dismissed, and every error banner is announced as an alert
  (dashboard-05).
- **Refresh failures.** A failed refresh now shows a "Refresh failed at HH:MM (reason). Showing
  data from HH:MM · Retry" strip and keeps the page instead of replacing it with a full-page
  error. "Load more" pages on Actions (and Findings) now survive the 30 s refresh, polls and
  live events, with no duplicate rows (dashboard-01).
- **ConfigMap reloads.** Config hot reload now picks up Kubernetes ConfigMap edits (the
  kubelet's ..data symlink swap) and debounces bursts of file events into one reload; a
  rejected file is retried after it changes or is touched (config-06).
- **Startup migrations.** Startup schema migrations now skip changes the catalog already shows,
  so a restart takes no DDL locks on sage tables. The bootstrap session waits at most 2 s for a
  lock (lock_timeout), retries a blocked step within a 10 s budget, then fails naming the step.
  A missing or invalid sage.config key index (idx_config_key_db) now fails startup and keeps
  /ready at 503 instead of silently breaking config writes. One INFO summary line is logged per
  boot (config-04).
- **Replica convergence.** Replicas that share a control database (meta mode, standalone
  replicas) now converge on global settings within 30 s; a stale replica's 409 carries the
  fresh generation so the retry succeeds, and GET /api/v1/config reports last_resync_at
  (config-05).
- **Leader lease renewal.** Each lease renewal now times out after a third of the lease, so a
  stalled control connection or an exhausted pool no longer stops renewals and leader-only jobs
  for good. A lapse is counted and logged once at WARN. The leader status API shows
  `last_renew`, and `/metrics` exports `pg_sage_leader{scope}`,
  `pg_sage_leader_transitions_total{scope}` and `pg_sage_leader_last_renew_age_seconds{scope}`
  (fleet-05).
- **Fleet control database.** `fleet_learning.control_database` pins the leader lease to one
  YAML database, so sidecars never lead from different databases; unpinned multi-sidecar fleets
  warn and refuse leadership, and GET /api/v1/fleet/leader reports control_datname and
  control_system_identifier. `fleet_learning.orphan_scope_days` (14) prunes the state of
  renamed or retired fleets. Approval card follow-ups are claimed under the leader lease, so a
  handover cannot post them twice (fleet-03).
- **Metric label escaping.** /metrics label values are now escaped the way the Prometheus text
  format requires (a database or SLO name with a tab, control character or invalid UTF-8 no
  longer breaks the whole scrape); new `pg_sage_build_info{version,commit}` and
  `process_start_time_seconds`; a database removed by a fleet reload, or deleted or renamed in
  meta mode, no longer leaves policy-park, record-failure or shadow counter series behind;
  series order is stable across scrapes. No metric is renamed (observability-07).
- **Aurora instance endpoints.** Aurora databases reached through an instance endpoint (for
  example the writer's own endpoint) are now reported as Aurora instead of RDS, so
  restart-required findings get managed proposals again. The telemetry status shows the cluster
  and the instance's role (writer, reader or unknown). A reader's proposal warns that its
  parameter group may differ from the writer's, and a disagreement between the detected
  platform and the RDS engine is reported as one target error naming the instance (clouds-03).
- **Telemetry backoff.** Cloud telemetry backs off after failed polls: up to 30 minutes when
  credentials are missing, the identity is unknown or access is denied, and up to 10 minutes
  (with jitter) for throttling and other transient errors. It logs once when a failure starts
  or changes and once on recovery, instead of every minute. GET /api/v1/cloud-telemetry now
  shows next_poll_at and consecutive_failures (clouds-06).
- **Supabase observability.** The log query now filters by the monitored database, so a busy
  neighbour can no longer fill the 1000-row cap. Malformed log records are skipped and counted
  with one warning per poll, without stalling the batch. Fresh Supabase host memory now feeds
  the advisor and tuning memory gates, such as the work_mem cap of 5% of RAM. Supabase CPU and
  memory status appears in GET /api/v1/cloud-telemetry (clouds-10).
- **Lakebase credentials.** Database credentials now refresh 15 minutes before expiry. Each
  refresh is limited to 20 seconds and runs one at a time, so new connections no longer queue
  behind it. If a refresh fails, new connections keep using the cached credential while it has
  more than 30 seconds left, with one warning per minute. At startup, a throttled or
  unavailable endpoint lookup is retried with backoff instead of stopping pg_sage, and a
  403/404 stops it at once with the endpoint named (databricks-01).
- **Lakebase telemetry windows.** Busy streams now keep up. Forward reads stay
  settle_lag_seconds (default 120) behind now and use a per-stream width that adapts and
  survives restarts. Each poll adds one short reconcile read per stream over the
  short_lookback_seconds (default 2 h) behind the forward edge, plus a 7-day deep sweep at most
  once a day. queries_per_poll now defaults to 2x streams+4 (16), and status reports
  forward_lag_seconds and reconcile_progress per stream (databricks-03).
- **Lakebase delivery.** One failing event no longer stalls delivery for the whole binding. A
  failed plan is retried with backoff (30 s up to 15 min) while later events keep flowing, and
  after 5 attempts it is counted as `delivery_failed`. Plans that can never be stored count as
  `rejected_row`. Sink outages (timeout, connection, SQLSTATE 08/57) spend no retry attempt.
  Only `plan_history` events queue for delivery; the other streams are acknowledged when
  stored. Telemetry status now reports a per-consumer `delivery` backlog with `pending_rows`
  (capped at 10,001), `pending_rows_capped`, `oldest_pending_at` and
  `oldest_pending_age_seconds`. The schema upgrades to v6 automatically (databricks-05).
- **Lakebase endpoint IDs.** OAuth, history telemetry, telemetry scope and the Apps
  configure.py now share one endpoint ID rule (1-63 lowercase letters, digits or hyphens, no
  leading hyphen), so endpoints such as projects/1abc/... that already authenticated now also
  pass lakebase_observability validation. lakebase_observability.bindings[].endpoint may now be
  omitted and is taken from SAGE_LAKEBASE_ENDPOINT(S) for the binding's host. IDs with a
  leading hyphen or more than 63 characters are now refused at startup (databricks-11).
- **Shadow evidence.** An index create, drop or replace that pg_sage made itself, or an index
  that disappeared with its table, no longer counts as external shadow evidence. Scoring passes
  now also read the newest pending decisions, so expect shadow external counts to fall on busy
  databases (new partial index sage.idx_action_log_index_ddl) (earned-03).
- **Operator rollbacks.** An operator's rollback of an incident-family L2 handoff or L3 action
  now demotes that pair one level and no longer counts toward the verified L2 recoveries L3
  needs, whether it came before or after pg_sage recorded the action. Rollbacks from before the
  upgrade are not re-judged (earned-01).
- **Live-outcome cursor.** The live-outcome reconciler now reads each database's L2 handoffs
  and L3 executions forward from a stored cursor, with a pending set for actions still waiting
  on a verdict or notice. On busy databases the newest actions, including harmful outcomes, are
  recorded and notified within a few passes instead of never. Idle passes no longer re-insert
  outcomes, clear the evidence cache or rewrite sage.trust_ledger_state. Additive migration
  adds handoff/auto cursor columns (earned-02).
- **Value report.** `GET /api/v1/value?until=` now includes its own day (`since == until` is a
  one-day report). Credit follows only the verification the action links to. A rollback voids
  its incident credit (`incident_avoided.reverted_at`). The unproduced `lock_storm` kind and
  `prevented` severity are retired. `pg_sage_incidents_avoided_total` is now a gauge, and value
  metrics come from each database's metrics refresher rather than a read on every scrape
  (earned-09).
- **Serialized bench runs.** Game days and "Run bench locally" on a shared local_dsn target now
  run one at a time, both within the sidecar and across sidecar processes (advisory lock in the
  target server's postgres database). A refused run gets 409 naming the holder and records
  nothing. Benches sharing a server no longer grade each other's prepared transactions or decoy
  actions as forbidden actions, so families are no longer falsely demoted. Each database's
  first game-day check is staggered (5-60 min) so a fleet does not start its game days together
  (benches-01).
- **Abandoned bench runs.** Game days and local bench runs now record their clone as soon as it
  exists. A run still 'running' 6 hours after it started (its sidecar crashed) is marked
  'abandoned' and its clone destroyed. Each bench run first resets leftover ALTER SYSTEM
  settings, inactive bench slots and old bench prepared transactions that a crashed run left on
  the target (benches-03).

#### Changed

- **Foreign key snapshot.** Foreign keys are now collected once per constraint, with the schema
  of both sides, the columns in declared order and no per-partition clones, so the foreign_keys
  snapshot is stored as changes instead of in full every cycle. missing_fk_index is now
  schema-aware: it reports the unindexed tenant in schema-per-tenant layouts, lists composite
  index columns in key order, and checks keys on partitioned tables against each leaf
  partition. The foreign_keys document from /api/v1/snapshots/latest now has one element per
  constraint, with `schemaname`, `ref_schemaname`, `columns` and `oid` replacing `fk_column`
  (collection-04).
- **Interval statistics.** Each snapshot's system stats now record the database's block,
  transaction and temp-byte counters, plus an `interval` of what changed since the previous
  snapshot (hit ratio, blocks, deadlocks, transactions, temp bytes, I/O time, checkpoints).
  Unknown values are null across a statistics reset. The `cache_hit_ratio` finding now judges
  the last interval, needs at least 10,000 blocks accessed, and reports the lifetime ratio in
  its detail (collection-06).
- **Query text reads.** The queries read now ranks pg_stat_statements on its counters alone.
  Texts are read only for statements new to a per-collector cache (at most 4 x
  collector.max_queries entries and 32 MiB) or due their hourly refresh, so a steady workload
  reads the query-text file about once an hour instead of every cycle on every database.
  Classification near capacity now runs at most hourly, and only when the entry count or
  dealloc count moved (collection-11).
- **Replication slot findings.** `inactive_slot` now waits until a slot has been inactive for
  `analyzer.inactive_slot_min_minutes` (default 5, settable through the API), measured from
  `inactive_since` on PG17+. A `wal_status` of extended/unreserved/lost reports at once.
  Temporary slots and synchronized failover slots on a standby are no longer reported. The
  finding is now `high_risk` and carries no executable SQL: the drop is in `detail.manual_sql`
  and needs approval. `replication_lag` findings are now named
  `replica:<application_name>@<client_addr>`, so open rows under the old row-index name resolve
  once after the upgrade (rules-09).
- **XID and multixact runways.** XID and multixact runway findings now open only once the
  cluster age reaches autovacuum_freeze_max_age (autovacuum_multixact_freeze_max_age). They are
  projected with the net age slope, and the detail records the gate and the projection basis,
  so busy, healthy clusters stop raising permanent wraparound warnings. On upgrade, open XID
  findings on clusters below the setting resolve on the first measured tick (forecast-01).
- **Sequence runway.** Sequence exhaustion has a single forecast, the runway
  (forecast_sequence_runway). It now covers descending sequences (toward the higher of MINVALUE
  and the owning column's minimum) and keeps sampled sequences listed when 50 others overtake
  them. The forecaster's forecast_sequence_exhaustion rule is retired: its open findings are
  resolved on upgrade ("superseded by forecast_sequence_runway"), and
  forecaster.sequence_warn_days and forecaster.sequence_critical_days now load with a warning
  and have no effect (forecast-05).
- **Checkpoint forecast.** Checkpoint forecast (forecast_checkpoint_pressure) now counts only
  requested (WAL-triggered or explicit) checkpoints per hour between closed days, so idle
  servers and the time of day no longer trigger it. It skips stats resets, warns at
  forecaster.checkpoint_requested_per_hour (default 2) and advises raising max_wal_size. After
  an upgrade the rule waits 8 closed days of new snapshots before it evaluates (forecast-03).
- **Cache pressure forecast.** Cache pressure forecast now judges each closed day's own cache
  hit ratio (blks_hit share of the day's block accesses between snapshots) instead of the
  lifetime ratio: "low" means the newest usable day is below forecaster.cache_warn_threshold,
  "declining" needs a drop of at least 1 percentage point (R² > 0.5), days with fewer than
  forecaster.cache_min_blocks_per_day (new, default 100000) block accesses or no data are left
  out rather than read as -100%, and findings name the top 5 tables by heap blocks read that
  day and shared_buffers; after upgrade the category stays unevaluated (open findings kept)
  until 7 days of new snapshots exist (forecast-04).
- **Invalid indexes in first look.** An invalid index that is still being built now shows as
  information with no SQL. A `_ccnew`/`_ccold` copy left by a failed REINDEX CONCURRENTLY gets
  `DROP INDEX CONCURRENTLY`. A failed unique index gets a read-only duplicate-key query to run
  before any rebuild. If builds by other roles cannot be seen, the items say so (firstlook-06).
- **Grant more.** Grant more now lists each role that owns tables pg_sage cannot act on, with
  its table count, and gives a real GRANT for each (or, for a superuser owner, advice to
  reassign its tables). It warns that owner membership confers DROP and TRUNCATE, leads with
  pg_maintain on PostgreSQL 17+, and adds a sequence SELECT grant naming the exact schemas. The
  startup check now shares pg_signal_backend and sequence checks with it, so a member granted
  WITH INHERIT FALSE is reported missing, with SQL that fixes it (firstlook-09).
- **Self-cost measurement.** pg_sage's measurement of its own cost now runs as a bounded
  read-only query (safety.query_timeout_ms / lock_timeout_ms), reads the sage schema size once
  per analyzer cycle for sage_footprint, sage_self_cost and the storage self-budget, loads
  pg_stat_statements query texts only when new entries appear, works on pg_stat_statements 1.8,
  and on older versions reports statement cost as unknown (schema counters still measured) and
  logs once to run ALTER EXTENSION pg_stat_statements UPDATE (footprint-03).
- **Per-database self-cost.** pg_sage's per-cycle self-cost (sage_self_cost,
  pg_sage_self_*_per_cycle, sidecar CPU per cycle) is now normalized to the period each
  database's collector actually runs, not the global collector.interval_seconds. The
  database-time budget inherited from analyzer.self_cost_budget_ms is now per database,
  exported as the new pg_sage_self_budget_database{database,resource="db_time"} series, and
  each database's pg_sage_self_budget_exceeded{resource="db_time"} flag is compared against it.
  Before a database's collector starts, its cost is reported as unknown instead of assuming 60
  s (footprint-02).
- **Third-party monitors.** Third-party monitors' statistics-view reads (Datadog, pganalyze,
  pgwatch) now count as workload in advice and never as pg_sage's own cost. A monitored or meta
  database named `sage` keeps its `sage_self_cost`, `sage_self_budget` and `sage_footprint`
  findings, whose titles now name pg_sage. A one-time migration resolves open pre-tagging
  pg_sage findings with reason "pre-tagging pg_sage statement" (footprint-06).
- **Nested-loop rule.** The bad-nested-loop rule now honours tuner.nested_loop_row_threshold.
  It prescribes HashJoin (or ANALYZE on a stale table) only when the row estimate is off by
  more than 10x on the join or its outer side and the inner side read at least the threshold
  rows over all loops (default 10000), so small misestimated joins get fewer HashJoin hints and
  ANALYZEs. tuner.revalidation_keep_ratio, revalidation_rollback_ratio and
  revalidation_explain_timeout_ms never had an effect and are now retired: they load with a
  warning and are ignored (tuning-09).
- **Hint readiness.** Query hints are offered as executable SQL only when they would actually
  change application plans: pg_hint_plan preloaded for application sessions,
  `pg_hint_plan.enable_hint_table` on for the database or role, the pg_hint_plan 1.7 hint
  table, and INSERT/DELETE on it. Otherwise `query_tuning` findings stay advisory with a
  `hint_readiness` detail naming what is missing. The facts are re-read every tuner cycle (no
  restart needed) and appear in the fleet capability report. Behaviour change: installs with
  `enable_hint_table` off (the default) stop getting executable hints; enable with `ALTER
  DATABASE mydb SET pg_hint_plan.enable_hint_table = on;` (tuning-04).
- **Fresh plans.** A cached plan now drives hints only while it is fresh, meaning newer than
  tuner.plan_max_age_minutes (new, default 1440), newer than the last ANALYZE of its tables and
  newer than pg_sage's last action on them. A stale plan is replaced by one bounded plain
  EXPLAIN (GENERIC_PLAN on PG16+) or not used. Plans without actuals never yield spill or
  nested-loop hints. Each plan is read once per candidate. Agent explain results and
  query_tuning findings show the plan's source, captured_at and age_seconds (tuning-08).
- **INCLUDE column cap.** The optimizer now enforces `llm.optimizer.max_include_columns`
  (default 3; 0 or less means 3). It refuses a proposed index with a wider INCLUDE list before
  the HypoPG what-if, and the reason names the cap. `llm.optimizer.over_indexed_ratio_pct`,
  `llm.index_optimizer.over_indexed_ratio_pct` and `llm.optimizer.plan_source` never had an
  effect and are retired: a config that still sets one loads and prints one startup warning per
  key, so remove them (indexes-11).
- **Write-heavy admission.** Index admission no longer refuses candidates on write-heavy tables
  before measuring them. HypoPG measures the candidate first, and only a complete measurement
  below write_impact_threshold_pct rejects it (the rejection is remembered while the table
  stays write-heavy). The write rate is now inserts plus non-HOT updates against the tuples
  read, so some append-heavy or bulk-loaded tables change workload class once. A failed column
  read no longer produces "column does not exist". max_indexes_per_table counts only valid
  indexes, and a replacement does not count the index it drops (indexes-02).
- **Hidden-index measurement.** Index replacements (replace_index) and the tuning agent's
  covered index drops are now measured with the old index hidden (HypoPG 1.4+, detected per
  instance): a statement that used the old index and plans more than 10% costlier without it
  rejects the change, naming its queryid; a measured drop's change becomes its
  verify-and-revert prediction; with an older HypoPG a replacement is measured as before and
  says "HypoPG without hypopg_hide_index (1.4+)" (indexes-07).
- **Hosting platform in prompts.** Config advisor prompts now name the real hosting platform.
  Before, RDS, Aurora, Cloud SQL, AlloyDB, Azure, Neon, Supabase and Lakebase were all
  described as self-managed. On a managed provider the prompt says ALTER SYSTEM is unavailable,
  where parameters change, and which settings are not adjustable. Prompts also carry the
  documented safe ranges of the tuned parameters, with version notes for PG17 and PG18
  (advisors-06).
- **Instance owner.** In fleet mode, databases on one PostgreSQL instance now elect one owner
  (the lowest fleet name, identified by system identifier, then provider resource, then
  host:port). Only the owner asks the WAL and connection questions and proposes ALTER SYSTEM or
  managed parameter changes. The other databases show them as advisory, with
  detail.instance_owner set, and the owner per database appears in
  status.capabilities.instance_owner. Grant EXECUTE on pg_control_system() for exact identity
  (advisors-08).
- **Unused rollout runtime.** Removed the unused fleet rollout runtime (automatic rollouts were
  never wired; fleet canaries are unchanged) and the superseded synchronous schema-guard
  trigger; no configuration or behaviour change (executor-09).
- **Rollbacks as decisions.** Every rollback, whether automatic or the operator's button and
  including approved, manual and resumed actions, is now a recorded standing-policy decision
  (`rollback_mitigation`, `evidence.rollback=true`). Maintenance windows, budgets and tiers no
  longer hold a revert. Hard stops, replicas, the provider list, observation trust and the lock
  ceiling still do. An automatic revert that policy or an emergency stop withholds now queues a
  "Revert action N" approval item instead of silently stopping at rollback_skipped (policy-05).
- **Decision episodes.** A withheld verdict is now one `sage.decision` row per episode. Linking
  a decision to its approval-queue item ends that episode, and so does an execute of the same
  candidate (new `candidate_key` column). A change re-queued after a rejection now gets its own
  decision row, and its approval card keeps the gate's reason and risk tier. New index:
  `idx_decision_candidate_open` (policy-02).
- **Decision policy provenance.** Decisions now record the standing-policy version they were
  actually evaluated under, plus the SHA-256 of that policy document, their guardrails and
  off_window_ok. Evidence packs now ship a non-empty policy.json (read from the control
  database in fleet mode) and a policy_hash_match flag in the manifest. A migration drops the
  unused sage.decision.policy_id foreign key and adds policy_doc_sha256 (policy-03).
- **VACUUM watchdog.** Executor VACUUM (not VACUUM FULL) no longer runs under a
  statement_timeout. It runs with statement_timeout 0 under a watchdog that reads
  pg_stat_progress_vacuum and cancels it with pg_cancel_backend in three cases: a heap phase
  stalled for the new safety.vacuum_stall_minutes (default 15, outcome vacuum_stalled),
  safety.vacuum_timeout_seconds reached (vacuum_cap), or the emergency stop (emergency_stop). A
  VACUUM whose table is already being vacuumed, autovacuum included, is withheld as
  vacuum_in_progress and no longer counts toward the custodian backoff. The freeze custodian
  uses a plain (already aggressive) VACUUM at or above the effective freeze table age. A red
  custodian proposal that exhausts its backoff now records a custodian_backoff decision and
  sends one critical custodian_escalation notification (vacuum-05).
- **Freeze horizon sampler.** Freeze horizons now have one reader. The freeze custodian, the
  runway advisor and the analyzer share one per-database sampler, which judges each table
  against its effective freeze maximum (autovacuum_freeze_max_age, or the table's own reloption
  when lower), answers TOAST horizons on the owning table, and gives red system catalogs a
  plan-only escalation, never an automatic VACUUM. The wraparound_freeze finding is now
  diagnosis only, reported at the new analyzer.wraparound_freeze_share (default 0.75); the
  custodian is the only producer of freeze VACUUMs. analyzer.wraparound_freeze_xid_age is
  retired: it loads with one warning and is ignored (vacuum-08).
- **Unused freeze code.** Removed the unused freeze Scanner and its never-read proposal fields
  (SQL, RequiresAuthorization, ExecuteDirectly, Park), and the reindex_candidate and
  blocked_vacuum case branches, which nothing produced; a guard test now requires every vacuum
  autopilot category to have a producer (vacuum-10).
- **Exact number checks.** Sage SRE, rca narration and Ask Sage now check each number in a
  claim against the exact values in the evidence it cites. Integers must match exactly, sign
  included, at any size, so a queryid that is off by one or has its sign flipped is rejected.
  Decimals must match at the precision the evidence states, and timestamps as whole RFC 3339
  instants. A probe's version, its row count, its [i] row ordinals and the year, hour or second
  of a timestamp no longer make a number count as cited, so more claims are dropped: in the
  replay corpus with the fake model, plan_regression keeps 54.5% fewer claims on the LLM-on arm
  and 33% fewer on the investigator arm, and the other families are unchanged (sre-03).
- **Catalog probe diagnostics.** Incident and finding case diagnostics now propose the Sage SRE
  catalog probe SQL: bounded by each probe's row cap, scoped to current_database(), and tagged
  /* pg_sage sre:<probe> <version> */. The lock diagnostic now reports only the real blocker of
  a CREATE INDEX CONCURRENTLY. The freeze diagnostic now lists XID-only sessions, prepared
  transactions and slots. The WAL diagnostic now shows retained bytes, wal_status and
  safe_wal_size. connection_saturation (v4) counts PG16 reserved_connections as reserved slots
  (playbooks-04).
- **Fleet app SLOs.** An app SLO with no `database` is evaluated by every database but now
  opens one `slo_burn` investigation, on the database named by the new
  `sre.slo.objectives[].owner_database`. Breaking: a fleet config with an unbound app SLO and
  no `owner_database` now fails to load, with an error naming the objective and both fixes.
  Meta-db fleets log a startup WARN when no registered database matches the owner
  (playbooks-08).
- **Cases policy reads.** The Cases page and shadow report now explain the standing policy once
  per database instead of once per case. Policy reads per refresh no longer grow with open
  findings (9 statements for 50 or 500 findings, down from 157 and 1,507), and a closed
  dashboard request stops its policy reads (cases-06).
- **Case action contracts.** Cases now show the executor's own action contract, risk tier and
  policy verdict for each recommended statement: ALTER SYSTEM and ALTER DATABASE show as GUC
  changes, VACUUM FULL and a plain DROP INDEX as high-risk script-only preflights, and
  confirmed facts, target binding, approval rules and per-table budgets now appear on the case
  (cases-04).
- **MCP argument and error codes.** Behaviour change: every MCP intent tool now rejects an
  unknown or mistyped argument with -32602 (a misspelled `databse_id` is now an error instead
  of being ignored). An unavailable target database or production dependency, and Ask Sage
  being temporarily unavailable, now return -32010 "retry later" instead of -32603. Planner
  refusals such as an unsupported `request_change` kind return -32602. Other failures stay
  -32603 "internal error", with the detail logged server-side (mcp-08).
- **Ask Sage time limit.** One question may now take up to `ask.max_seconds_per_question`
  (default 60 s, 15-300). The ask API route gets that plus 5 s and keeps its connection open
  past the server's read and write timeouts. A question whose caller goes away after Ask Sage
  queued a proposal, opened an investigation or proposed a fact is now stored as `incomplete`
  with stop `cancelled` and its actions. Answers are counted in
  `pg_sage_ask_answers_total{database,status,stop}` (mcp-07).
- **Partitioned table facts.** A table fact on a partitioned table now covers all its
  partitions, sub-partitions and their indexes (declarative partitioning, read from the catalog
  via pg_partition_root; legacy inheritance children are not covered), both at the policy gate
  (fails closed if the catalog cannot be read) and when the analyzer turns findings into
  source-fix packets (facts-10).
- **Fact absence.** A confirmed operator or model fact now keeps binding while its subject is
  absent for `facts.confirmed_absence_days` (default 7; previously 1 hour). The absence start
  is stored in the database, so a restart no longer resets it. If the subject comes back with
  the same value within that window after an expiry, the original confirmation is restored. A
  new `fact_expired` notification is sent when a confirmed fact expires (facts-08).
- **Principals file.** Removed the unused declarative principals-file package and its
  `sage.guard_principal_files` table (dropped on bootstrap; it never held principals);
  declarative principals are deferred to G2 on the kill/retire-governed paths (agentgov-04).
- **LLM error classes.** LLM provider errors are now classified. A 400/404/422 (for example a
  prompt over the context length) no longer opens the circuit breaker. A 401/403 opens it at
  once. 429 and 500/502/503/504 are retried up to 4 attempts, honouring Retry-After (seconds or
  HTTP-date, capped at 30 s) and never waiting past the caller's deadline. llm.timeout_seconds
  now applies per attempt, with a 120 s floor for reasoning models. The breaker gains a
  half-open probe and resets when the endpoint, key, auth or model changes. /api/v1/llm/status
  reports circuit_state, last_error_class, last_error_at, last_success_at and
  consecutive_failures. New admin-only POST /api/v1/llm/test sends one charged test request and
  returns its class and latency (llm-02).
- **Prompt size limits.** Prompts larger than the model's known input limit (from Gemini model
  listings) are now refused before they are sent, and llm.context_budget_tokens now trims the
  context attached to advisor, tuning and explain prompts instead of capping the length of the
  briefing answer (briefings use their own 4096-token cap) (llm-05).
- **In-flight DDL.** In-flight DDL is now re-checked on every poll. A DDL waiting for its lock
  with migration.queue_waiters_threshold (default 3) sessions queued behind it turns its
  finding critical, with approval-only guidance to cancel the DDL (a HIGH-risk guarded script,
  never auto-executed) and retry under SET lock_timeout = '5s'. The lock-chain finding stops
  recommending a cancel of the root reader. Long-running DDL findings no longer auto-resolve
  after 24 h. Unqualified table names resolve through the DDL role's search_path (ambiguous
  names report unknown stats) (migration-05).
- **DDL preflight.** The case DDL preflight now reads the migration advisor's detection-time
  measurements (size, rows, activity, pending locks, lag, lock level, rewrite) and lock levels
  from the single DDL rule catalog. ADD CHECK is now ACCESS EXCLUSIVE and FK SHARE ROW
  EXCLUSIVE. A running DDL is no longer counted as its own competing query, lock-hold estimates
  are labelled estimated, and the dashboard shows when the values were measured (migration-01).
- **Migration enrichment.** Model work now runs on a bounded worker per database, with a shared
  24 h cache. When the queue is full the oldest job is dropped and counted in
  pg_sage_migration_enrich_dropped_total. Model text appears only in the llm_ detail keys and
  never becomes recommended_sql (migration-10).
- **DDL classifier.** lint_migration, the agent gate and the migration advisor now share one
  DDL classifier. On release (cgo) builds it reads the PostgreSQL parse tree and recognizes SET
  LOGGED/UNLOGGED, SET ACCESS METHOD, EXCLUDE, non-concurrent DETACH PARTITION, PG 18 NOT NULL
  constraints, RENAME, CREATE TRIGGER, non-concurrent DROP INDEX, and UNIQUE/REFERENCES/CHECK
  written inline on ADD COLUMN. Trust-ramp effect: agent DDL that matches a rule, is
  unrecognized or does not parse is always classed ddl_locking, never ddl_additive, so some
  agent DDL that used to count toward the additive class's autonomy now builds trust in the
  locking class. lint_migration returns safe only when every statement is known, and it now
  agrees with the advisor that binary-coercible type changes (varchar to text) do not rewrite
  the table. Builds without cgo log once that classification is degraded (migration-04).
- **Alerting validation.** An enabled `alerting` block is now checked at startup and on
  config-file reload, and refused with an error naming the key when a route would send nothing.
  Refused: an unknown or wrongly-cased channel (`Slack`), a severity other than
  critical/warning/info, slack or pagerduty without its URL or key, missing or duplicate
  webhook names, enabled with no routes, bad or half-set quiet hours, an unknown timezone. A
  bare webhook name in a route now works as an alias of `webhook:<name>`. Onboarding reports
  notifications as configured only when a route reaches a usable channel. See the new
  `alerting.*` table in docs/configuration.md (alerting-08).
- **Checkpointed audit verification.** Audit chain verification now runs from checkpoints:
  scheduled runs check only the links written since the last clean verification, a full pass
  runs every `audit.full_verify_interval_hours` (default 168, 0 = always full) in bounded
  memory (1M links in 4 MB, was 684 MB), a rewritten or removed checkpointed link is reported
  as `checkpoint_mismatch`, a chain that cannot be verified raises an
  `audit_chain_verification_failed` warning without hiding the other chains or resolving its
  findings, and `GET /api/v1/audit/verify` verifies since the checkpoint by default
  (`full=true` for the whole chain, 5 min timeout, 504 when exceeded) (audit-03).
- **Evidence pack v2.** Evidence packs are now format `pg_sage-evidence/2`. In fleet mode a
  window pack reads sign-ins (`auth_audit.json`) and the database's own and global config
  changes (`config_audit.json`) from the control database, proved by its chains. Files over
  10,000 rows hold the lowest ids in id order, each manifest entry reports `source`, `rows`,
  `truncated`, `last_id` and `note`, and `&file=<name>&after_id=<last_id>` fetches the next
  page. Window packs add `guard_query_audit.json`, action packs add `policy_approvals.json`.
  Consumers that check `format` must accept `/2` (audit-07).
- **Encryption key changes.** Changing encryption_key no longer makes every fleet listing or
  reconnect run two argon2id derivations per database: legacy keys are derived once at startup.
  A stored database password that no longer opens now fails fast with "database <id>:
  encryption key does not open this value" and logs one ERROR per database with the recovery
  steps. A sealed channel secret names its key and the same steps (apiauth-11).
- **Dashboard polling.** Dashboard tabs that are not shown no longer poll the sidecar: they
  refresh once when shown. Failing endpoints are retried at 2x, 4x and so on of the interval,
  up to 5 minutes. Live events refresh only the selected database's pages, once per 2 s burst.
  The Dashboard reuses the app's fleet poll, and the "Getting started" checklist stops polling
  once setup is complete (dashboard-08).
- **Dashboard caching.** The embedded dashboard is served gzipped (when Accept-Encoding allows
  it) with strong ETags and 304s. Hashed /assets/ files are cached as immutable for a year and
  index.html is revalidated on every load. The fleet health chart now loads on demand, cutting
  the entry script from 1.14 MB to 0.80 MB (220 KB gzipped). Meme images are hashed and cached
  too (dashboard-09).
- **Removed dashboard code.** The hidden Findings page and its execute button are removed
  (unreachable since the redesign), as are the Dashboard "new since visit" badge that never
  counted anything and the unused /sse dev proxy (dashboard-12).
- **Trust badges.** Dashboard header shows a Mixed trust badge for a fleet on more than one
  level, with the families each database may auto-execute now. The context strip says what may
  run without approval, how many databases are emergency-stopped, and how many have not
  reported their action policy. The pending-actions badge follows the selected database and
  shows N+ with the unconnected databases named when the count is incomplete (dashboard-02).
- **Lifecycle labels.** `alerting.*`, `briefing.*` and the collector, analyzer, tuner,
  schema_lint and auto_explain intervals are now labelled `restart` in the lifecycle reference,
  the derived-settings API and the Settings banner. They were always restart-bound and only the
  label changed. Only trust_policy, llm, llm.redact_numeric_literals and (YAML fleet)
  fleet_databases rebuild in place. A derived collector interval still applies live
  (config-07).
- **Invalid config values.** Config files with a zero or negative
  analyzer.unused_index_window_days, analyzer.seq_scan_min_rows, safety.ddl_timeout_seconds,
  trust rollback window or cooldown, or advisor, alerting, schema-lint or migration interval
  now fail to load, and the error names the key. Stored settings overrides that no longer
  validate are skipped with a warning naming the key, instead of becoming 0. A settings save
  that would leave an invalid config returns 400 instead of 500. Boolean settings accept True,
  TRUE and 1 (config-02).
- **Mode and controller.** `mode: standalone` combined with `meta_db` / `--meta-db` is now a
  startup error (leave `mode` unset or set `mode: meta`). The API refuses to start without a
  config controller. Settings for retired keys (briefing, forecaster, auto_explain, tuner,
  llm.optimizer_llm, alerting.timezone) are no longer applied from stored rows (config-10).
- **Health score reasons.** Fleet health scores now explain themselves (`health_reasons` on
  `/api/v1/databases` and `/api/v1/metrics?database=`, shown on the Overview tiles). A database
  whose collector has not sampled for more than 3 intervals is capped at 25, so
  `sage.health_history` steps down for stale databases. A database counts as healthy only when
  connected, error-free and scoring at least 50 (`pg_sage_fleet_healthy`). Databases with equal
  scores are listed by name. The per-database metrics endpoint now reports the real score
  instead of 0. The unused `advisory_lock_held` field is removed (fleet-07).
- **Fleet findings cache.** Fleet findings (Fleet page and MCP fleet_findings) are now read at
  most once a minute per database through a shared cache. Up to 8 databases are read at a time
  and a request answers within 15 s, listing slow databases as "timed out". Databases with more
  than 500 open findings are named in the new `truncated` list, and the page says their counts
  may be low (fleet-08).
- **Idle fleet learning.** Fleet learning now idles, with no catalog or pg_stat_statements
  reads and no fingerprint writes, until two connected databases share a tenant boundary. It
  logs "fleet learning: skipped, no peer in boundary" once each time it starts idling, and runs
  a cycle within a minute of a second database joining (fleet-11).
- **HA monitor.** Each database now has one HA monitor, persisted in the control database. It
  checks the role at startup, every `ha.check_interval_seconds` (new, default 60 s, 5-600, at
  most the executor cycle) and just before each executor cycle. Custodian proposals, the runway
  advisor, earned autonomy and the fleet status read that one result, so they agree on role,
  safe mode and the failover cooldown. That cooldown now holds every autonomous path, even with
  `sre.autonomy.enforce: false`. The monitored database sees one role probe per interval
  instead of up to four. `GET /api/v1/databases` and the Fleet page show `ha_role`,
  `ha_safe_mode` and `ha_last_change` (fleet-04).
- **Off-scrape metrics.** /metrics no longer queries any database during a scrape. Each
  database refreshes its exported values on its own every `prometheus.refresh_interval_seconds`
  (default 30 s, never faster than the collector), with a 2 s timeout per source. One
  unreachable database no longer stalls the scrape or blanks the other databases. New series
  `pg_sage_metrics_refresh_success` and
  `pg_sage_metrics_refresh_timestamp_seconds{database,source}` show refresh health and
  staleness (observability-03).
- **Per-database metrics.** Every mode (standalone, fleet, meta) now exports per-database
  health from in-memory status: pg_sage_database_up,
  pg_sage_collector/analyzer_last_success_timestamp_seconds, pg_sage_findings_open{severity},
  pg_sage_database_health_score, pg_sage_emergency_stopped, pg_sage_trust_level (one-hot) and
  pg_sage_database_size_bytes, which gains a database label. LLM gauges and the fleet summary
  also render in every mode. pg_sage_findings_total, pg_sage_collector_last_run_timestamp,
  pg_sage_fleet_instance_findings/_health, pg_sage_connections_total and
  pg_sage_cache_hit_ratio are deprecated (removed in v2.7.0; see docs/deployment.md for
  replacements). pg_sage_connection_up now documents the control database (observability-02).
- **Cosmos DB for PostgreSQL.** Azure Cosmos DB for PostgreSQL (`azure-cosmos`) is now a
  managed provider everywhere: its configuration findings no longer carry ALTER SYSTEM (they
  are guidance-only, naming `az cosmosdb postgres configuration`), fleet readiness shows its
  own adapter and limitations, and agent-role contracts cover it. One provider registry now
  drives managed detection, aliases, preload steps and readiness families, so self-managed
  readiness no longer hides ALTER SYSTEM and readiness agrees with the executor contracts
  (clouds-11).
- **Provider storage capacity.** On managed providers (RDS, Cloud SQL, AlloyDB, Azure), the WAL
  custodian's `custodian.wal.retained_wal_disk_pct_ceiling` and the runway disk forecast now
  use the provider's measured storage capacity. WAL proposals record `capacity_source`
  (`provider_telemetry` or `capacity_unbounded`) with `disk_pct`, and runway disk findings
  record `provider_telemetry` or `declared`. Without fresh telemetry the behaviour is unchanged
  (clouds-07).
- **Per-instance telemetry.** Fleets that monitor several databases on one RDS, Aurora or Cloud
  SQL instance now poll provider telemetry once per instance, not once per database, so
  CloudWatch billing is per instance. `/api/v1/cloud-telemetry` lists the instance once with
  its `databases`. A parameter-group change one database already holds (open, or rejected in
  the last 7 days) is no longer proposed again by the other databases on that instance, and
  `/api/v1/managed-changes` shows each database's `member_databases` (clouds-08).
- **CI runs and bench shards.** Pull-request pushes now cancel the superseded run (master, tag,
  nightly and manual runs are never cancelled or queued); the Go and npm caches now restore;
  PG17 runs the unit and integration suites once together under the race detector; the PG17
  PGIncidentBench runs in six shards (core-a/b, reactive-a/b, runway-a/b) so a release tag's
  runs, sized by SAGE_BENCH_MIN_RUNS_PER_FAMILY, fit each shard's 40-minute step, and releases
  ship six signed pgincidentbench-<shard> reports (ci-03).
- **Docs site.** Internal notes are no longer published; the Guides and Governance and safety
  sections now list the walkthroughs, Neon/Supabase, managed clouds, LLM costing, agent
  governance and the safety bench; pull requests build the site with `mkdocs build --strict`
  from pinned requirements; and the site deploys only from release tags, so it describes the
  latest release (old URLs of internal notes and FINDING_CATALOG now 404) (ci-09).
- **Promotion bench reports.** Promotion proposals now name the bench report that decided them,
  with where it came from (for example "evidence supports L2; bench: unsigned
  (operator-provided) <id>"). The Trust page row, its pending-approval card and the action
  approval card show the same. A report under bench_results_path stamped for another pg_sage
  build is now logged once per process per file version at INFO, naming the build it scores,
  instead of a WARN on every hourly ingest for every database (earned-10).
- **Autonomy page reads.** The Autonomy page now reads its levels and evidence in one batch (no
  more than 3 database round trips per 30-second poll, down from about 165). The gate, the
  Autonomy page and the Trust page now share one effective-level rule, so a page can no longer
  show a level the gate does not enforce. Example: a pair granted L1 now shows L1, not L0
  (earned-08).
- **Bench expected scenarios.** PGIncidentBench release runs now fail when a scenario the new
  expected-scenario manifest (sre-bench/testdata/ci-expected.json, set with
  SAGE_BENCH_EXPECTED) requires is skipped, or when skips leave a gated arm's family gate
  without data. SAGE_BENCH_MIN_RUNS_PER_FAMILY reruns families that skips left short and fails
  any family still below the bar. The report now lists each family's "n=<k> against bar <bar>";
  tag builds use bar 10 instead of a fixed 4 repeats (benches-06).
- **Exercised incidents.** In AgentSafetyBench, an incident row counts as "exercised" only when
  the run saw every one of its posture detectors fire. Rows that were not measured are reported
  as the new "declared" status, with a `declared` count in `incident_score`. The published
  incident number drops from 2 exercised to 1, because INC-ORM's AP-13 and AP-16 have no bench
  scenario (benches-07).
- **Bench verifier and replay.** The sidecar builds the bench release verifier once per process
  instead of once per database per hour; CI replays the PGIncidentBench corpus once per job
  (core part A shard, SAGE_BENCH_REPLAY=1) instead of in every shard; the production binary no
  longer links `testing` through sre-bench (benches-11).

#### Added

- **Collector health.** Collector health is now visible: a skipped cycle logs its real cause
  ("load ratio 0.93 > ceiling 0.90 (active sessions / max_connections)" or "load query failed:
  <error>", no longer "db load too high" for both); GET /api/v1/databases carries a
  per-database `collector` status (breaker state, unavailable categories, persist/query_store
  failures, capabilities); /metrics adds pg_sage_collector_breaker_state,
  _breaker_skips_total{reason}, _last_success_timestamp, _category_unavailable{category},
  _persist_failures_total and _query_store_failures_total, labelled by database in every mode;
  new safety.host_cpu_ceiling_pct sets index verification's host CPU ceiling (unset inherits
  cpu_ceiling_pct with a startup warning, so nothing loosens on upgrade), and cpu_ceiling_pct
  is now documented as the active sessions / max_connections ceiling (collection-08).
- **Performance gate H.** New gate H checks pg_sage's own self-cost meter against the DB time
  the gate measures (within 10%, 5 ms floor) and its cycle length against the observed
  collector period. The gate also forces a live self-configuration pass into the steady window,
  so a meter, tag or normalization regression no longer ships green (footprint-09).
- **Policy gate reference.** Added the policy gate reference to the docs (verdicts, reasons,
  ledger, approval flow), and corrected the serialize_mode queue and park-metric docs
  (policy-09).
- **Log fast path.** Critical PostgreSQL log lines (PANIC, crashes, disk full, OOM, refused
  connections, deadlocks, wraparound, corruption, WAL archive and segment failures) now open
  incidents and send incident_detected within rca.log_fast_path_interval_seconds (default 5, 0
  disables) instead of waiting for the analyzer cycle. They also survive log storms: the
  watcher drops ordinary lines first, reports any loss as a "Log watcher overloaded" incident,
  and counts it in pg_sage_logwatch_dropped_lines_total{cluster,reason} (rca-06).
- **Log source status.** Log-based RCA is now visible and self-healing: /api/v1/databases
  reports each database's log_source (active, unavailable with the setting at fault, or
  disabled), an unreadable server log is retried every 30 s, backing off to 10 min, without a
  restart, First Look shows a log_rca capability with the steps for each provider, and
  docs/configuration.md documents the requirements (logwatch stays off by default) (rca-09).
- **Probe slot metrics.** `/metrics` now shows how long probes wait for the probe slots and how
  often they are refused one: `pg_sage_sre_probe_queue_wait_seconds_sum/_count` and
  `pg_sage_sre_probe_refusals_total`, by database, caller class and limiter. An investigation
  probe refused its slot is now retried once when its lease has time, instead of being recorded
  as missing evidence (sre-06).
- **Sage SRE permissions doc.** `docs/sage-sre-permissions-and-data-flow.md` now covers
  approved actions and the investigator: an investigation half, with a privilege row for every
  read including `explain_statement` and `read_history`, and an approved-actions half for the
  M5 cancel and the M6 runbooks and statistics maintenance. The actions half lists
  `pg_signal_backend` / table-ownership needs and records per provider whether
  `pg_signal_backend` can be granted. The LLM data-flow section is rewritten around the
  investigator plans, and a contract test keeps the doc's numbers matched to the code (sre-10).
- **Sequence forward fix.** Sequence exhaustion cases now carry a real forward-fix script,
  which pg_sage never runs: a read-only runway query over `pg_sequences` that names the column
  fed by the sequence, whether the sequence or the column is the binding limit, and how many
  values are left (descending sequences included); an `ALTER SEQUENCE ... AS bigint` under
  `lock_timeout`, guarded so it refuses while a smallint or integer column takes the values;
  and the online column-widening steps for review. A finding that names no sequence now shows a
  blocked candidate instead of nothing (playbooks-03).
- **Legacy AgentDB drop.** Added an admin-only, irreversible drop of the 27 legacy AgentDB
  tables: POST /api/v1/agentdb/decommission/drop-legacy-tables with {"inventory_sha256": "<from
  GET /api/v1/agentdb/decommission>", "confirm": "drop 27 legacy tables"}. It refuses (409)
  while any inventory item is unacknowledged, when the inventory changed since the export, or
  when another object depends on a table; it never cascades and writes one sage.config_audit
  row per dropped table. The inventory now reports inventory_sha256 (agentgov-11).
- **Agent governance doc.** Agent governance is documented in full: docs/agent-governance.md
  (now "Agent governance", in the site nav) covers agent principals, sponsors and profiles,
  taint, agent versus operator MCP tokens and legacy-token binding, `mcp.oauth` (per-database
  audience, delegation, task claim, identity bindings), grants and activity, environment
  labels, and one table of the two-person rules; a test keeps every agent-governance route and
  the shipped profiles in step with the doc (agentgov-08).
- **LLM costing guide.** The LLM costing guide now lists every model consumer with its per-call
  cap, has a defaults table checked against the code (advisor runs daily, not hourly), points
  token monitoring to GET /api/v1/llm/status rather than /api/v1/metrics, and explains the
  optimizer cooldown as a dedup window plus breaker pause; the sage.briefings reference now
  documents delivery_status and the bigserial id (llm-10).
- **Action pgaudit records.** Executor sessions now carry application_name 'pg_sage
  action:<decision id>' so pgaudit records of long-running actions (CREATE INDEX CONCURRENTLY,
  REINDEX, VACUUM) are kept and appear in the action's evidence pack; failed batches are
  retried, and pg_sage_pgaudit_records_total{database,outcome} counts stored, dropped, evicted,
  requeued and unmatched records (audit-06).
- **Dashboard CI.** CI now enforces a 70% coverage floor on the shared data layer (src/hooks,
  src/lib) and runs the 10 mocked Playwright specs against the built bundle served by vite
  preview (dashboard-11).
- **Fleet Mode docs.** A new Fleet Mode page (docs/fleet.md) covers the control database,
  tenant boundaries, fleet learning and look-alike priors (evidence only, never a grant),
  leader election, the LLM budget split, the health score and the
  /api/v1/fleet/{findings,lookalikes,leader} routes with sample JSON. The fleet walkthrough now
  uses a dedicated sage_meta database, sets the encryption key it needs to add databases, and
  points to the header Stop all / Resume all (fleet-12).
- **Metrics on the API.** New `prometheus.serve_on_api` (default off) also serves `GET
  /metrics` on the API listener, with its TLS and rate limit, to an API token whose only scope
  is the new `metrics` scope. The token grants no MCP tool. In a Databricks App, any signed-in
  pg_sage user can scrape, and the App launcher now turns this on by itself, so App metrics can
  be scraped through the App URL. `prometheus.listen_addr: ""` opens no separate listener.
  `SAGE_PROMETHEUS_ADDR` sets the whole listener address and wins over `SAGE_PROMETHEUS_PORT`.
  A listener that cannot bind (port 9187 is also postgres_exporter's default) now logs an ERROR
  and exports `pg_sage_metrics_listener_up 0` instead of failing silently. Upgrading adds a
  small migration to `sage.mcp_tokens` (observability-08).
- **Provider overrides.** Added optional `provider:` and `cloud_resource:` (aws/gcp/azure) keys
  on `postgres:` and each `databases[]` entry. The provider override only narrows: it applies
  while detection says unknown, and a disagreement keeps the detected provider and logs a
  warning, so `self-managed` can never unlock ALTER SYSTEM on a managed service. Cloud SQL Auth
  Proxy sockets (`/cloudsql/<project>:<region>:<instance>`) now name the instance directly. A
  loopback Auth Proxy with no named instance reports telemetry unavailable, with a clear reason
  and no API calls. Detection now recognizes Neon and Supabase behind custom domains
  (clouds-05).
- **Auto-safe readiness.** Fleet readiness can now report databases as auto-safe ready: ANALYZE
  permission is probed (superuser or
  rds_superuser/cloudsqlsuperuser/alloydbsuperuser/azure_pg_admin/pg_maintain membership gives
  ok, anything else is "limited" and checked at execution), provider enable paths appear as a
  separate extension_hints field ("hint:" in the dashboard), capabilities refresh every 15
  minutes and after a config reload, and Cosmos DB for PostgreSQL ALTER DATABASE changes are
  now blocked by the policy gate as readiness already reported (clouds-04).
- **Tagged test suites.** CI now compiles every tagged test suite (integration, e2e, perfgate,
  providerlive, cloudlive, convertscale) in a compile-only vet step, and a contract test fails
  if a new build tag is left out. The Neon/Supabase providerlive suite builds again. The
  retired v0.8 cloudsqltests scripts are replaced by an owner-run, read-only Cloud SQL/AlloyDB
  identity and readiness check (cloudlive tag) (ci-04).
- **Skip budget.** The skip budget now checks the PostgreSQL major and the skip reason
  (allow-list qualifiers pg<N/pg>=N and reason=/regex/), each -run step must show its named
  tests passed (-require), rules that matched nothing are reported as STALE, every skip-budget
  step runs with pipefail, and the alerting/notify database tests must pass (ci-02).
- **Assertlint.** A new assertlint gate fails CI on any test that can never fail, and on any
  test file that reads SAGE_TEST_DATABASE_URL outside internal/testdb. Existing offenders are
  on a shrink-only allowlist. Hollow executor, alerting, retention and notify tests are
  replaced with asserting ones, and `go test ./internal/analyzer` now passes without a database
  (ci-11).
- **Release pipeline.** Releases now wait for the whole CI matrix (PG 14-18, arm64, lint,
  security, container health) and are published only after the archived binary passes a smoke
  test. Checksums are signed keyless with cosign (checksums.txt.sigstore.json), with an SBOM
  per archive. The container image is multi-arch (amd64 and arm64), signed, and carries
  provenance and an SBOM. Release notes come from CHANGELOG.md, and pull requests that change
  release files get a goreleaser dry run (ci-05).
- **LLM end-to-end test.** Added an end-to-end test that runs the real sidecar binary with LLM
  features on (the default config) against a local scripted OpenAI-compatible stub. It covers
  advisor findings, briefings, the breaker, the daily budget and fleet prompt isolation
  (ci-07).
- **Browser suites.** CI now runs the mocked dashboard browser suite on every push and the
  full-surface browser suite (real binary, scripted disposable PostgreSQL 17 fleet) on nightly,
  tag and manual runs; releases wait for it, and any skipped browser test fails the build
  (ci-08).
- **Release metadata and env docs.** SECURITY.md now lists 2.5.x as the supported line, and a
  test fails whenever VERSION, the newest CHANGELOG release and SECURITY.md disagree. Every
  SAGE_* variable read outside tests is registered: the two SAGE_DATABRICKS_* variables are now
  in docs/configuration.md, and the test and bench variables are in a generated "Test
  environment" table in CONTRIBUTING.md. CONTRIBUTING now explains how to run the Go suites
  against a disposable test server with the skip budget. Test packages print one line when
  SAGE_TEST_DATABASE_URL is unset and their database tests will skip (ci-10).

### v2.7.0 (tranche T2)

#### Upgrade notes

- **Deprecation: YAML-only alerting.** The config file's `alerting:` block is replaced by
  Notifications (channels and rules) when alerting-06b, -06c and -06d land. Until then,
  configure one stack per destination: both stacks on one Slack webhook or PagerDuty key post
  twice and open two PagerDuty incidents (docs/architecture.md, "Alerting"; alerting-06).
- Metrics: six families are removed and two renamed, and the default port moves from 9187 to
  8187 (`SAGE_PROMETHEUS_PORT=9187` keeps it); update scrape configs and alerts from the table
  in docs/metrics.md (observability-09).
- Re-import the Grafana dashboard (uid `pg-sage`, version 2); its PostgreSQL-internals panels
  are gone, use postgres_exporter for them (observability-01).
- An open finding is reminded per `alerting.repeat_minutes` (critical 240), not every cooldown;
  set it lower if you relied on 20-minute repeats (alerting-07).
- `GET /api/v1/shadow-report` is removed: switch scripts to `GET /api/v1/shadow-decisions`
  (earned-04).
- Model-root authority counts only a signed, build-stamped bench report: install the release's
  report with `pg_sage bench install` (benches-09).
- A reset account, and the first sign-in with a generated bootstrap password, must choose a new
  password of 12 to 72 bytes before anything else works (apiauth-03).
- Widening an agent's ceiling or profile answers 202 and waits for a second admin to confirm
  (agentgov-06).
- An L3 level granted on another PostgreSQL major runs at L2 until 3 improved outcomes or an
  admin's re-approval (earned-07).
- Verified indexes are reverted again for write regressions, measured on the index's own table
  (`verify.write_impact_pct`, default 20%) (executor-03b).
- A migration step whose SQL changed since it was recorded in `sage.schema_migrations` stops
  startup, naming the step (config-04b).

#### Security

- **Evidence packs are signed by default.** Whenever an encryption key is set, the sidecar
  generates an Ed25519 key on first use and keeps it sealed in `sage.evidence_signing_key`;
  `audit.evidence.signing_key_env` still takes precedence. `GET /api/v1/audit/evidence/key`
  (admin) serves the public key, and `pg_sage_sidecar evidence verify <pack> [--public-key
  <file>]` checks a pack offline. Unsigned manifests now say `"signed": false` and
  `"integrity": "accidental-corruption-only"` (audit-10).
- **Signed audit checkpoints.** Each clean scheduled verification signs the chain head with
  the evidence-signing key into the chain as a checkpoint link, which reaches SIEM sinks as an
  "Audit Checkpoint Signed" event. The opt-in `audit.prune_below_signed_checkpoint` (default
  false) deletes chain links below a signed checkpoint that every sink has received, except
  links of rows still present, and records an "Audit Trail Pruned" event. Verification of a
  pruned chain then starts from that checkpoint (`from_checkpoint`, `pruned_below`)
  (audit-03b).
- **The approval trail is chained.** Two new tamper-evident chains: `decision` (the gate's
  decisions; risk tier, policy version and hash, and targets are sealed; verdict, reason,
  evidence and links are state; `repeat_count`/`last_seen_at` are not chained) and
  `policy_approvals` (which gains an `id` column). Evidence packs now prove the decisions and
  approvals behind an action (`chains.json`), and window packs prove `decisions.json` and add
  `policy_approvals.json`. SIEM sinks see the new `log_name` values `decision` and
  `policy_approvals`. A repeat of an open decision at a different risk tier now starts a new
  decision instead of re-tiering the recorded one (audit-11).
- **Passwords.** Users can change their own password (Account page, or
  `PUT /api/v1/auth/password`), which signs out their other sessions. Admins can reset a
  user's password (Users page, or `POST /api/v1/users/{id}/password-reset`), which signs the
  user out everywhere and shows a temporary password once. A reset account, and the first
  sign-in with a generated bootstrap password, must choose a new password before anything
  else works. New passwords must be 12 to 72 bytes and must not be the account email.
  Creating a user now answers 400 for a password that breaks these rules and 409 only for an
  email already in use (apiauth-03).
- **Widening agent changes need a second admin.** A higher ceiling or a broader profile now
  answers 202 and waits up to 15 minutes for a second admin, who confirms the same fields at
  `POST /api/v1/agents/{id}/changes/{cid}/confirm`; before, it was refused unless
  single-operator mode was on. A move to a narrower profile applies at once. Unknown profiles
  are refused (422) at create and patch; existing agents with an unknown profile keep loading
  and show "profile not recognised". New migration: `sage.guard_principal_changes`
  (agentgov-06).
- **Model-root authority needs a signed report.** It now counts only a signed release or
  local-run bench report stamped with the running build; unsigned or unstamped lift reports
  no longer grant it. Install the release's signed live-model report with
  `pg_sage bench install [--config FILE] [--commit SHA] pgincidentbench-live.json`, which
  verifies it and copies it with its bundle into `sre.autonomy.bench_results_path`, or upload
  both as `multipart/form-data` (`report`, `bundle`) to
  `POST /api/v1/sre/autonomy/bench-results`. Tag releases list the two asset URLs and the
  install command in their notes (benches-09).

#### Fixed

- **Query sampling.** The collector now also admits the slowest statements by mean time, the
  heaviest by I/O, and pinned queryids (the targets of open index verifications), within the
  same 2 x `max_queries` read. Each query records the arm that admitted it in `admitted_by`.
  An index whose query got cheap enough to leave the top-N is now measured and kept, not
  reverted as `insufficient_samples` (collection-07).
- **Utility statements.** Query advice (`slow_query`, `high_total_time`, the tuner, the
  briefing) now ignores utility statements (BEGIN/COMMIT/SAVEPOINT, SET/RESET/SHOW, DISCARD,
  DEALLOCATE, LISTEN/NOTIFY, LOCK) and DDL or privilege changes, and open findings about them
  resolve; a slow CALL or DO is still reported. Plan capture, the HypoPG what-if and the perf
  gate share one EXPLAIN check, so a query that starts with a comment is no longer skipped
  (collection-10).
- **Slow and regressed queries.** `slow_query` and `query_regression` now judge each query's
  mean over the last collector interval, not the cumulative `pg_stat_statements` mean, and
  need at least `analyzer.slow_query_min_calls` calls (new key, default 5).
  `query_regression` compares against the median of the query's hourly query_store means over
  the lookback, so plan flips on long-lived queries are caught. A single slow ad hoc call is
  no longer critical, and open findings for queries below the call floor resolve (rules-05).
- **Stale statistics and sequential scans.** `stale_statistics` flags a table only when the
  rows changed since its last ANALYZE pass autovacuum's own analyze threshold (per-table
  storage parameters respected), so static lookup tables are no longer re-flagged and
  re-analyzed every week. Partitioned parents, which autoanalyze never processes, get an
  approval-gated info finding. `seq_scan_heavy` judges scans since the previous snapshot,
  not lifetime counters. Open findings for static tables and old seq-scan storms resolve on
  upgrade (rules-10).
- **Sequence exhaustion.** `sequence_exhaustion` measures how much of a sequence's own range
  (MINVALUE to MAXVALUE) is used, in the direction it moves, so descending sequences with a
  custom MINVALUE are no longer under-reported. CYCLE sequences are no longer flagged and
  their open findings resolve. The finding points to `forecast_sequence_runway` for the time
  to the limit (rules-11).
- **Forecasts on closed days.** Daily findings now use closed calendar days only: today is
  left out (findings lag by up to a day) and days the collector watched for less than 80% of
  their hours do not count. Query volume compares the two complete weeks ending yesterday, or
  is not evaluated. Disk growth and connection saturation project only when R² >=
  `forecaster.min_r_squared` over 7+ calendar days. Connection saturation counts client
  backends against `max_connections` minus reserved slots (`reserved_connections` counts on
  PG 16+), falling back to `limit_basis: legacy` until 7 days of new snapshots exist
  (forecast-08).
- **Cluster runways in a fleet.** Cluster-wide runways (xid/mxid age, disk, replication
  slots) and the connection-saturation and checkpoint-pressure forecasts are reported by one
  owner database per cluster, so one filling slot opens one finding, one case and one
  investigation. Duplicate copies from earlier versions close on upgrade with
  `detail.cluster_owner` once the owner reports, and `GET /api/v1/findings?database=X` lists
  the owner's open cluster findings under `cluster_refs` (read-only) (forecast-09).
- **Fleet CPU budget.** `self_budget.cpu_ms_per_cycle` now raises one `sage_self_budget`
  finding on the `sidecar` object instead of one per database. Loops are tracked per database
  (`pg_sage_self_loop_busy_seconds_total{loop,database}`), LLM waits are reported as
  `llm_wait` instead of busy time, and open per-database CPU findings resolve on the next
  analyzer cycle (footprint-07). A database removed from the fleet now takes its loop and Ask
  Sage answer series off `/metrics` with it.
- **Vector hints.** The tuning agent no longer treats PostGIS KNN, pg_trgm or btree_gist
  distance queries (`<->`, `<#>`) as pgvector ANN queries: a vector hint needs a vector,
  halfvec or sparsevec column or cast. Multi-line queries no longer get a false missing-LIMIT
  warning, filtered ANN advice points at measuring recall with `pg_sage vector-lab`, and
  column types reach the model by name (`vector`, `geometry`), not `USER-DEFINED`
  (indexes-09).
- **Planning-time hints.** High planning time no longer installs a
  `Set(plan_cache_mode "force_generic_plan")` hint. The tuner reports `query_parameterization`
  findings (advice only: prepared statements or one `= ANY($1)` array parameter) when a
  statement's planning dominates, or when one statement shape shows up as at least
  `tuner.param_shape_min_variants` queryids (new key, default 20, 2-1000) because its IN
  lists vary in length; the IN-list trigger is off on PostgreSQL 18. Hints installed earlier
  stay active until revalidation or rollback (tuning-11).
- **Connection advisor.** `max_connections` is never recommended below the advisor window's
  peak of total backends plus `superuser_reserved_connections`, `reserved_connections` (PG16+)
  and 10, and a decrease stays advice when the peak is unknown. Pooler advice and
  observations appear as info findings, and churn counts closed sessions (the
  `pg_stat_database.sessions` delta). `idle_in_transaction_session_timeout` can be applied,
  only with `ALTER DATABASE` on the reviewed database and only with an operator's approval
  (1 min to 24 h); `statement_timeout` and `idle_session_timeout` stay advice only
  (advisors-07).
- **WAL and bloat advisors.** WAL and bloat advice reason over the advisor window instead of a
  60-second-old snapshot. The WAL review states the requested share of checkpoints, WAL per
  hour and WAL per `checkpoint_timeout`, makes no model call while under 20% of checkpoints
  are requested, and never emits a `max_wal_size` below the current value or the measured
  floor (1.5 x WAL per `checkpoint_timeout`, its arithmetic shown). Without history, WAL
  advice is advisory only. Executed `max_wal_size` and `checkpoint_timeout` changes are
  verified by the requested share of checkpoints. The bloat review lists the 50 tables with
  the most estimated dead bytes, says how many were omitted, and computes the dead-tuple
  trend over the window (advisors-05).
- **Index write cost.** Verified indexes are reverted again for write regressions, measured on
  the index's own table: if the WAL bytes per tuple the table writes rise more than
  `verify.write_impact_pct` (default 20%) after the build, an index whose queries improved is
  reverted as `write_impact`. Too little table-attributable evidence is recorded as
  `insufficient` and never reverts, and there is no database-wide fallback, so a batch on
  another table is never charged to the index (executor-03b).
- **Interrupted canaries.** Fleet canary runs are saved as they run (targets, each database's
  part, its action id and verdict). At startup pg_sage marks a canary left running by a
  stopped sidecar as `interrupted` and sends a `canary_interrupted` warning naming the
  databases that were mid-change. It never resumes the run, and a canary another live HA
  sidecar is still running is left alone (executor-04b).
- **Repeated log lines.** Log-based RCA now counts repeated log lines. Deadlock incidents name
  the participating PIDs and the occurrence count (`sage.incidents.log_occurrences`). A log
  incident resolves only after `rca.log_quiet_minutes` (default: the dedup window) with no new
  occurrence, and a recurrence within `rca.recurrence_renotify_hours` (default 6) at no higher
  severity is linked but not notified again, so periodic deadlocks no longer page all day. A
  `pg_cancel_backend` cancel is now `log_statement_cancelled` (info, no incident). Deadlock,
  refused-connection, archive, wraparound and WAL-gap incidents come with investigation
  probes (rca-07).
- **Executed plans from the log.** On-demand EXPLAIN plans are stored as `explain_estimate`
  instead of `auto_explain`, and existing rows are renamed on upgrade. Self-managed servers
  ingest auto_explain's executed JSON plans from the csvlog/jsonlog server log as
  `auto_explain_log` (plan records up to 1 MiB, at most `auto_explain.max_plans_per_cycle`
  per poll; rejected records are counted with the setting to fix). Oversized log records are
  counted (reason `oversized_record`) instead of dropped silently. The tuner and tuning agent
  prefer a fresh executed plan over a newer estimate, and the unused
  `auto_explain.prefer_session_load` key is ignored with a warning (rca-04).
- **Fact approval cards.** Every new fact proposal sends one approval card, whichever path
  made it (detectors, model, first look, Ask Sage, MCP `propose_fact` and `mark_object`), and
  repeats send none. The Facts page and declare form gain "Check what it matches" (new
  `GET /api/v1/facts/coverage`, at most 50 objects, read-only, 2 s timeout).
  `owned_by_app_migrations` no longer accepts the unread repo/path values; stored facts keep
  loading (facts-09).
- **Executor metrics.** `/metrics` reports what the executor did:
  `pg_sage_actions_total{database,class,outcome}`,
  `pg_sage_verification_verdicts_total{database,class,verdict}`,
  `pg_sage_actions_pending_approval{database,risk}` and
  `pg_sage_action_last_failure_timestamp_seconds{database}`. The failed-action alert in the
  deployment guide pointed at a metric that was never exported and could never fire; it now
  uses `increase(pg_sage_actions_total{outcome="failed"}[15m]) > 0` (observability-04).
- **First look counts findings.** The time to first finding
  (`pg_sage_time_to_first_finding_seconds`) counts only warning or critical catalog items.
  Setup notes and agent posture notes no longer count, and a first look with no findings
  waits for the analyzer's first finding, so TTFF rises on installs whose only first-look
  items were setup notes. A new gauge, `pg_sage_first_look_findings`, sits next to
  `pg_sage_first_look_items`, the checklist reads, for example, "3 findings (1 critical),
  2 setup notes, 1 posture note", and `/api/v1/onboarding` `first_look` gains `findings`,
  `critical`, `setup`, `posture` and `notes` (firstlook-07).
- **Overview health banner.** The top banner no longer reports health when there are open
  warning findings, open incidents, stale collection or collection it cannot judge. It says
  "No open critical or warning cases" only when all of these counts are zero, and otherwise
  lists the open work (critical first) with a link to Cases. `GET /api/v1/databases` adds
  `summary.total_warning`, `incidents_critical`, `incidents_warning`, `incidents_unknown`,
  `stale_collection` and `freshness_unknown`, plus `status.incidents_critical`/
  `incidents_warning` (-1 while unknown) and `status.collection_freshness` per database
  (cases-11).
- **Lock investigations after the chain clears.** When a lock chain clears before a lock
  investigation probes, the investigation names the root blocker RCA recorded at detection
  (pid, state, `backend_start`, sessions blocked), from one bounded read of `sage.incidents`
  kept as `incident_snapshot` evidence. It is a contributing hypothesis only, never a
  conclusive root, and a live lock graph always takes precedence (sre-05).
- **Time picker.** The header time picker shows only on the pages it filters (Actions,
  "Ledger window", and Snapshot & metrics, "Health chart window"). Actions marks proposals and
  approval requests as "Current state, not time-scoped", the fleet health chart no longer
  refetches on every 30 s tick, and an unknown saved range shows Last 24h (dashboard-07).

#### Changed

- **Metrics rename release.** v2.7.0 removes `pg_sage_findings_total`,
  `pg_sage_collector_last_run_timestamp`, `pg_sage_fleet_instance_findings`,
  `pg_sage_fleet_instance_health`, `pg_sage_connections_total` and `pg_sage_cache_hit_ratio`,
  and renames `pg_sage_optimizer_recommendations_total` to `pg_sage_optimizer_recommendations`
  and `pg_sage_fleet_findings_total` to `pg_sage_fleet_findings_open` (old-to-new table in
  docs/deployment.md and docs/metrics.md). The default metrics port moves from 9187
  (postgres_exporter's) to 8187 (`SAGE_PROMETHEUS_PORT=9187` keeps the old one). The new
  docs/metrics.md, generated from the registry the sidecar renders `/metrics` from, lists
  every family with type, labels, modes and meaning, and deploy/prometheus/pg_sage.rules.yml
  ships nine alerts with promtool tests and runbooks (observability-09).
- **Grafana dashboard rebuilt** (uid `pg-sage`, version 2, same `DS_PROMETHEUS` input;
  re-import it). It uses only exported families, has `$job`/`$instance`/`$database`
  variables, and its rows cover the sidecar, databases, executor and trust, LLM, SRE and SLO,
  and value and footprint; a panel whose feature is off says so. The PostgreSQL-internals
  panels (connections, cache hits, transactions, dead tuples, deadlocks) are gone: use
  postgres_exporter and its community dashboard (observability-01).
- **LLM metrics per client.** Every LLM client (shared, per-database general, optimizer) has
  its own `pg_sage_llm_requests_total{client,database,result}`,
  `pg_sage_llm_request_duration_seconds` histogram and `pg_sage_llm_tokens_total{kind}` in
  standalone, fleet and meta mode. `pg_sage_llm_enabled`, `_circuit_open`,
  `_tokens_used_today` and `_tokens_budget_daily` gain `client` and `database` labels and
  cover every client, so unlabelled selectors keep matching but return one series per client.
  The budget gauge follows a live `token_budget_daily` change (observability-05).
- **Briefings.** Briefings cover the time since the previous briefing (a weekly schedule
  reports a week), store each channel's delivery result in `sage.briefings.delivery_status`,
  post Slack-safe mrkdwn (model text cannot ping `<!channel>`), and apply
  `briefing.schedule`/`channels`/`slack_webhook_url` without a restart. Behaviour change: in
  fleet mode the new `briefing.fleet_mode` (default `rollup`) posts one message covering every
  database; set `per_database` to keep one message per database (llm-06).
- **Alert reminders.** A finding that stays open is no longer re-sent every cooldown (about
  every 20 minutes for a critical). It is alerted once and then reminded per the new
  `alerting.repeat_minutes` (defaults: critical 240, warning 1440, info 0 = never). Slack shows
  "Still open for 3h20m (seen 21 times)", webhooks get `"reminder": true` and
  `open_for_seconds`, and PagerDuty re-triggers the same incident. The schedule survives
  restarts. Operators who relied on 20-minute repeats should set `repeat_minutes` lower; the
  minimum is the severity's cooldown (alerting-07).
- **Disk growth forecast.** `forecast_disk_growth` reports the capacity it was judged against
  (`capacity_source`, `capacity_bytes`) and `days_until_full`, preferring a fresh provider
  capacity, then `forecaster.disk_capacity_bytes`, and naming that key when nothing is
  declared. `GET /api/v1/forecasts/growth` returns data fitted from the runway samples. The
  unused `sage.size_history` table is dropped on upgrade, and `forecaster.min_data_points`/
  `forecaster.alert_horizons` are retired (they load with a warning and have no effect;
  remove them) (forecast-06).
- **Self-configuration from measured cost.** `collector.interval_seconds` and
  `safety.query_timeout_ms` are sized from the statements the collector actually ran: the
  interval from the p90 cycle time, the deadline from 3x the p90 slowest statement, raised to
  at least 2x the current deadline after timeouts and flagged "timeouts seen".
  `self_budget.db_time_ms_per_hour` tightens the collector budget below 1%, and each cost
  citation in the derived-settings API and page names its source (probe before the first
  cycle, collector after) (footprint-05).
- **Hints are planned before they are proposed.** Query hints are planned with and without the
  hint (bounded read-only EXPLAIN, never ANALYZE). Hints that do not change the plan
  (`no_effect`) or raise the estimated cost by more than the new
  `tuner.hint_cost_tolerance_pct` (default 10) are refused, and hints that cannot be checked
  stay advisory. Findings carry `detail.hint_evidence` with both plans and costs, the tuning
  agent gets a `hinted_explain` tool, and revalidation retires installed hints that no longer
  change the plan (tuning-05).
- **Case ranking by time to impact.** Cases rank within each severity by the time to impact
  the producer measured (disk, WAL slot, sequence and wraparound runways, storage growth,
  connection saturation), nearest first. Each case shows how that number was calculated, for
  example "About 3 days until the disk is full (seconds_to_limit 259200 from 48 samples over
  6 h, R² 0.97)", or "Urgency not assessed". Recurring incidents link to their resolved
  predecessor ("4th occurrence; previous resolved 2026-11-02"). New API fields: `urgency`,
  `rank_basis`, `recurrence` (cases-07).
- **Recommendation lifecycle on cases.** Cases show what pg_sage already did with a proposal:
  "applied on <date> and reverted it: <reason>", "Stopped after 3 failed attempts" or
  "Retrying after 14:05 UTC". A change reverted or abandoned with the same content is blocked,
  a proposal whose evidence is past `expires_at` is blocked as `evidence_expired`, and each
  candidate shows how long its evidence is valid. If the recommendation history cannot be
  read, the case is marked `recommendation_unknown` instead of showing "none" (cases-10).
- **Runbook drafts.** Editing a signed runbook stages a draft and the signed version keeps
  running until an admin signs the draft (responses carry `running_version` and `running`).
  The Runbooks page adds Revise, a version list, a structural diff from the newest signed
  version before signing, and a typed-name confirmation for Retire, which now needs the
  admin role (playbooks-10).
- **MCP tool availability.** `tools/list` leaves out tools that cannot be served (`ask_sage`
  when Ask Sage is off or has no enabled LLM, the `specialist_*` tools when the contract is
  off, `top_queries` and `whatif_index` on databases without pg_stat_statements), narrows each
  tool's database enum to the databases that can serve it, and sends
  `notifications/tools/list_changed` when that changes; a hidden tool called directly still
  returns -32010 unavailable (mcp-09).
- **Migration ledger.** Startup records every sage schema migration step in the new
  `sage.schema_migrations` ledger (step, SHA-256, time, pg_sage version) with a fingerprint of
  the catalog. A restart over an unchanged schema skips the recorded steps and runs no DDL. A
  changed schema is re-checked step by step, an INVALID index is rebuilt, and a step whose
  SQL changed stops startup, naming the step and how to re-run it (config-04b).
- **Managed providers.** Azure flexible server and AlloyDB: a setting SQL cannot change now
  becomes an operator-approved proposal, as on RDS and Cloud SQL. On Azure it carries the
  exact `az postgres flexible-server parameter set` command, a restart line when needed, and a
  rollback to the prior value or the default; on AlloyDB it is a
  `gcloud alloydb instances update` fill-in template. pg_sage reads Azure server parameters
  over ARM (it needs only `configurations/read`) and still writes only `max_slot_wal_keep_size`.
  A new migration widens `sage.managed_change_proposals.provider` to `alloydb` and `azure`
  (advisors-03).
- **Lakebase scale to zero.** pg_sage reads each endpoint's scale-to-zero suspend timeout and
  warns once per endpoint at startup when its collector, pool health check or MinConns may
  keep the compute from suspending. The admin deployment status reports
  `suspend_timeout_seconds`, `compute_state`, `keeps_compute_active` and
  `scale_to_zero_blockers` per endpoint, and docs/lakebase.md gains a Cost model section. No
  defaults change (databricks-04).
- **Shadow report removed.** `GET /api/v1/shadow-report` and its heuristic "avoided toil"
  figures are gone. The Settings Shadow Mode page reads the shadow ledger
  (`GET /api/v1/shadow-decisions`, open to every signed-in role) and shows decisions, scores,
  counted evidence and potential toil minutes per class ("not modelled" when no toil model
  exists). Query-hint cases show the standing policy's verdict instead of "policy pending".
  Scripts that called shadow-report should switch to shadow-decisions (earned-04).
- **Earned autonomy decays visibly.** An earned L2/L3 incident level whose bench report or
  shadow-review window ages out is recorded as a `capped` history entry
  (`evidence_decayed:<checks>`) on the evaluate interval, and as `cap_cleared` when new
  evidence returns. Operators get one warning (`evidence_expiring` plus an `action_failed`
  warning notification) 7 days before the evidence expires, and the Autonomy and Trust pages
  show "L3 holds until <date> unless a new bench report arrives" (`holds_until`,
  `expiring_checks`). Thresholds and levels are unchanged (earned-06).
- **L3 re-validates after a major upgrade.** Each self-initiated L3 trust level records the
  server major it was granted on. A level granted on another major runs at L2 (capped, reason
  `server_major_changed:16->17`) until 3 improved outcomes on the new major, or an admin's
  re-approval (Trust page Re-approve, or `POST /api/v1/sre/autonomy/revalidate`), clear the
  cap. Minor upgrades, incident families and carried-over levels are unaffected. Adds the
  nullable column `sage.sre_family_autonomy.server_major` (earned-07).
- **Unscored game days.** Game days and local bench runs record what they scored (scored /
  skipped / errored runs and the top skip reasons in `sage.sre_game_days.summary`). A run
  where every fault program skipped, for example on a snapshot clone without superuser, ends
  `completed_unscored` instead of `completed` and logs a WARN naming the reasons; the
  Autonomy page shows it as a warning, with each game day's eval run and links to its
  game-day check (benches-08).
- **CI matrix.** PGIncidentBench runs as a PostgreSQL 14-18 x shard matrix job, and
  AgentSafetyBench and the performance gate each run in their own job, in parallel with the
  suites. CI's PostgreSQL images are built once per revision, on a base pinned by digest, with
  HypoPG, pg_hint_plan and pgaudit baked in, so CI no longer runs apt-get or restarts servers.
  A nightly-only shuffled race run (`-shuffle=on -count=2`) is reported and never gates
  (ci-03b).
- **Docs.** configuration.md has a "Vacuum, bloat and freeze autopilot" section (the freeze
  custodian's ladder and approvals, its cadence, `custodian.freeze.red_buffer_pct`, backoff
  and escalation, and reading its decisions in `sage.decision`), and the architecture overview
  and findings catalog match the code (vacuum-11). The architecture guide states PostgreSQL
  14-18 and adds "Own Footprint and Self-Configuration": the statement tag and exclusion
  rules, the self-cost and budget findings and units, every `pg_sage_self_*` family, derived
  settings and the performance gate (footprint-10).

#### Added

- **Index bloat findings.** The new `index_bloat` rule reads `analyzer.index_bloat_threshold_pct`
  (default 30). Each cycle it estimates btree bloat from planner statistics in one bounded
  catalog read (no pgstattuple, no relation opened) for the 200 largest indexes of at least
  `analyzer.index_bloat_min_bytes` (new, default 100 MiB). At the threshold it raises an info
  finding and at twice the threshold a warning, both recommending
  `REINDEX INDEX CONCURRENTLY` under the approval-required `reindex_concurrently` contract.
  Invalid indexes and partitioned parent indexes are skipped; leaf partition indexes are
  judged on their own (rules-13).
- **Finding category registry.** Finding categories come from one registry:
  `GET /api/v1/findings/categories` lists every source-filter subsystem with its categories,
  new `source` values `tuning`, `cloud`, `governance`, `migration` and `facts` reach
  categories no filter matched before, and docs/generated/finding-catalog.md (severities,
  config keys, executor contract tier) is generated from code (rules-12).
- **First look re-check.** Operators and admins can press Re-check on the Extensions and
  First look steps, or call `POST /api/v1/first-look/recheck?database=`, after CREATE
  EXTENSION or a GRANT; the checklist and degraded checks update with no restart, at most one
  re-check per database per minute. LLM first-look summaries are no longer regenerated on
  restart when nothing changed (firstlook-05).
- **Footprint panel.** `GET /api/v1/self-cost` and a Settings > General "Footprint" panel show
  pg_sage's own database time, blocks and rows per hour against its self-budget, the sage
  schema size and the costliest statements, split by the component named in each
  statement's tag (`/* pg_sage collector */`), with the new metric
  `pg_sage_self_component_db_time_ms_per_hour{database,component}`. Statements that
  pg_stat_statements first saw under the old bare tag show as "unattributed" until the entry
  is evicted or reset (footprint-08).
- **Missing-index advice without an LLM.** When the tuning agent is inactive (no endpoint,
  breaker open, daily budget spent), a deterministic index floor proposes `missing_index`
  findings for `seq_scan_heavy` tables (`detail.source = "deterministic_floor"`). They pass
  the same validator, rejection memory and HypoPG what-if as the agent's; without HypoPG they
  are advisory with no executable SQL ("HypoPG not installed: impact unmeasured"). It follows
  `tuning.enabled` and `llm.optimizer.enabled`, per database (indexes-12).
- **Plan anti-patterns.** Executed plans yield advisory `query_tuning` findings for an index
  scan whose filter discards at least 90% of the rows it reads (naming the composite index to
  build) and for a nested-loop OR join filter spanning both inputs (UNION rewrite advice).
  Neither proposes a hint or SQL; both detail the plan facts under `plan_antipatterns`. Plans
  that print fractional "Rows Removed" counters (PG18) now parse (market-10).
- **Natural-language EXPLAIN without an LLM.** The plan rules always fill `slow_because`,
  recommendations and the summary; an LLM can only add to them, and each entry carries its
  source (`insights`). `query_id` is resolved per database (a fresh captured auto_explain plan
  with actuals is used, with `plan_source`/`plan_age_s`), node `time_ms` counts every loop
  (`loops` added), and cases get an "Explain this query" panel (tuning-10).
- **Decisions tab and "Why hasn't this run?".** `GET /api/v1/decisions` and an Actions >
  Decisions tab (operators only) list every change the policy gate is holding back that has
  not run, with its verdict, the reason in plain words, how many times it repeated, when the
  budget frees and the policy version. `GET /api/v1/policy/explain` and a "Why hasn't this
  run?" link on finding cases show how the gate would decide that finding's changes now and
  record nothing. Settings > Trust & Safety shows the standing policy and its history,
  read-only. A new migration adds the partial index `idx_decision_open_withheld`
  (policy-07).
- **Approve now, run in the next window.** Approving a moderate or high action outside its
  maintenance window schedules it for the next opening (UI, API, approval card or chat;
  `POST /api/v1/actions/{id}/schedule` and `/unschedule`). When the window opens, pg_sage
  checks the current policy again and then runs it. A new policy field,
  `window_start_margin_minutes` (0-720, default 0), holds back a start when the window closes
  within the margin (reason `window_closing`), and startup warns about policy windows that
  name no time zone (policy-04).
- **Diagnosis follow-ups.** Sage SRE sends each incident's diagnosis to the engineer who was
  paged: a new `investigation_concluded` follow-up goes only to the Slack, Telegram or email
  channels the incident's page reached, at most once per incident and once the newest
  conclusion has settled for 60 s. On Telegram it is a threaded reply; PagerDuty gets nothing.
  The text is redacted to one line, and there is no new setting (sre-08).
- **Each case's own investigation.** The Cases page shows each case's own Sage SRE
  investigation even on a busy database (the investigations API accepts repeated `case_id`,
  up to 100, and returns the newest investigation of each), and an open investigation
  refreshes every 5 s while it is still running (sre-09).
- **SLO history.** Each SLO shows how long it has been in its state and when its burn started.
  An unknown budget names its reason; a partial window also shows the share covered and the
  budget spent so far (new `budget_coverage` and `budget_spent_so_far` status fields). Each
  row expands into its state transitions, the recovery verdict and the newest `slo:<name>`
  investigation (playbooks-09).
- **Statistics maintenance after an incident.** New MCP tools `sre_refresh_table_evidence`,
  `sre_propose_statistics` and `sre_request_statistics` (propose scope) re-read stat_tables
  for a table the investigation observed, propose CREATE STATISTICS against evidence pg_sage
  resolves itself (no evidence id or SHA in the call), and queue the human approval.
  `sre_rollback_statistics` needs the approve scope and an admin. Inconclusive
  investigations now qualify; the 15-minute evidence cap, the table-lock identity rechecks
  and human approval are unchanged. A schema migration admits the new `evidence_refreshed`
  event (playbooks-06).
- **OAuth agent identities.** Admins bind and unbind an identity (issuer, subject) on the
  agent's OAuth identities section or via `/api/v1/agents/{id}/identities`, without SQL. Each
  change is audited as `agent_identity_bound` or `agent_identity_unbound`, and docs/mcp.md
  has a new OAuth section (agentgov-03).
- **Agents page controls.** Admins can edit an agent's sponsor, profile and ceiling (a
  widening change waits for a second admin), retire it by typing its name (the dialog shows
  `agents.roles.retire_grace_days`), revoke its tokens, kill one agent or one database as well
  as all, and release fleet or database freezes from a new banner (two admins after a kill).
  The new read route `GET /api/v1/agents/freezes` lists the open freezes, at most 200
  (agentgov-07).
- **Exact Ask Sage citations.** Citations name the exact read they rest on (a new `alias` per
  citation and `aliases` per statement; a re-read of the same list no longer replaces the
  cited digest), and in the UI a finding, investigation or action citation opens that
  finding's case, the investigated case or the change record instead of the page root
  (mcp-05).
- **Source-fix verdicts in the background.** Every database runtime checks every 5 minutes
  for deploys whose verify window has closed and records the verdict even if the coding agent
  never comes back. Operators read a finding's source-fix report at
  `GET /api/v1/findings/{id}/source-fix` and in a finding case's technical details: PR,
  commit, deploy time, verdict with predicted vs observed change, and a "backdated" flag for
  deploys reported after their window had closed (mcp-06).
- **Stored briefings.** `GET /api/v1/briefings?database=&limit=` (newest first) and
  `/api/v1/briefings/latest?database=` (latest per database; 404 with the reason when none
  exists yet) for every signed-in role, an MCP resource
  `pgsage://databases/{database}/briefings/latest` (read scope), and a Latest briefing card on
  the Overview showing open findings by severity, delivery per channel and the full text;
  `content_json` and `delivery_status` are returned exactly as stored (llm-06b).
- **Alert Log delivery failures.** `error`/`resolve_error` rows are red with the channel's
  error text, `deferred` (quiet hours) rows amber, the fleet view adds a Database column and a
  banner naming databases whose log could not be read (`failed_databases`; 503 only when no
  database can be read), and the summary counts delivered, failed and deferred. The
  Notifications Log clears its error after a successful refresh (alerting-09).
- **Approve-from-chat setup.** Notifications > Channels sets up Telegram channels (bot token,
  chat id, generated webhook secret) and Slack "Interactive approvals" (signing secret, team
  id), with a copyable callback URL after saving, an Edit dialog that keeps masked secrets, a
  last delivery column and an admin-only Chat identities panel.
  `GET /api/v1/notifications/channels` adds `last_status`, `last_error` and `last_sent_at`
  (alerting-10).
- **Audit health.** `/metrics` exports `pg_sage_audit_chain_ok`,
  `pg_sage_audit_chain_verified_timestamp_seconds`, `pg_sage_audit_chain_problems{kind}`,
  `pg_sage_audit_chain_verify_errors_total`, `pg_sage_siem_lag_links`,
  `pg_sage_siem_consecutive_failures` and `pg_sage_siem_delivered_total`, all from the fleet
  leader. `GET /api/v1/audit/status` (admin) and a new Audit trail card in Settings show the
  chain state, SIEM lag and evidence signing, and the card builds evidence downloads. A
  database name with a tab or similar character no longer breaks the `pgaudit_records_total`
  scrape, and security.md gives each audit table's retention key and a threat model
  (audit-12).
- **Loop health and SRE backlog metrics.** `pg_sage_self_loop_errors_total{loop,database}` and
  `pg_sage_self_loop_last_success_timestamp_seconds{loop,database}` show a periodic loop that
  fails every cycle (collector, analyzer, sre, briefing), with the executor and retention
  loops tracked too. `pg_sage_sre_investigations{database,state}` and
  `pg_sage_sre_investigation_oldest_open_age_seconds{database}` show the Sage SRE
  investigation backlog; both SRE families are absent when SRE does not run
  (observability-06).
- **Top Cases.** The Overview's Recent Recos tab is now Top Cases: the five highest-ranked
  open cases (severity, then time to impact), each linking to its case and naming any
  database whose cases could not be read. `GET /api/v1/cases` gains `?limit=N` (1-100) and
  `?summary=1` for rows without evidence payloads (cases-11b).
- **Admin navigation.** A new Admin nav group (Fleet, Users, Notifications, Alert log,
  Settings, MCP tokens, Agents) makes the Users, Notifications and Alert log pages reachable.
  The duplicate Advanced entries "Findings explorer" and "Action history" are removed (their
  old URLs still open Cases and Actions), the command palette lists every page you can see,
  and `?` opens a keyboard shortcuts sheet (dashboard-06).
- **Confirmation dialogs.** Rollback, token and grant revokes, channel delete, restart and
  user delete/deactivate/SSO unlink ask in an accessible dialog that names the target and, for
  rollback, shows the action id, database, exact rollback SQL and risk tier. HIGH-tier
  rollbacks require typing the database name, a failed rollback keeps its error in the
  dialog, actions without stored rollback SQL show a disabled Rollback with the reason, and
  each browser tab is titled "<page> · pg_sage" (dashboard-10).
- **Pending restarts in Settings.** Settings lists the saved settings that wait for a restart,
  with their running and saved values, and badges every field live or restart. Restart
  appears only when `SAGE_SUPERVISED=1` declares a supervisor; without it Settings says how
  to restart. The maintenance window field lists the full grammar and previews the next
  opening through the new admin route `POST /api/v1/config/validate-window`.
  `GET /api/v1/config/global` adds `pending` and `restart_supported` (config-09).
- **Fleet runtime view.** Fleet page rows show what the running sidecar reports for each
  database: connection and HA role, open/critical findings, health and its reasons, effective
  trust (marked "override" when it differs from the registration), emergency stop with actor
  and time, and LLM budget used/allocation. A registered database that is not running reads
  "not running". The Database page leads with a runtime summary and fleet look-alikes, with
  the raw snapshot collapsed, and fleet-finding occurrences link to their case.
  `GET /api/v1/databases` adds `llm_budget` when `llm.fleet_token_budget_daily` is set
  (fleet-06).
- **Databricks runtime health.** Deployment status (admin) reports `runtime_liveness`
  "partial" and a `databricks` block: Lakebase credential expiry and mint-failure classes
  (never tokens), the last AI Gateway success and failure, and per-stream telemetry warehouse
  statements, rows, bytes and read time. A Databricks runtime card on the Settings General tab
  shows it, and `authentication_observed` turns false after a Gateway authentication failure
  (databricks-06).

### v2.8.0+ (tranche T3)

#### Upgrade notes

- What-if plans use the application's search_path (`ALTER DATABASE`, then `ALTER ROLE ALL`):
  an application that relies on a per-role search_path should set it on the database
  (security-01c).
- Finding alerts follow `alerting.cooldown_minutes`, quiet hours and `repeat_minutes` per
  database, dedup key and channel, and rule deliveries queue in an outbox on the control
  database (alerting-06).
- `contract_not_before` is now the expand time plus `migration.contract_delay_minutes` (60),
  and rehearsals park slow plan flips and long locks (migration-06).
- Game days on `local_dsn` finish `completed_unscored`, and a customer program that errors
  fails the game day (benches-12).
- Azure host telemetry needs the Reader and Monitoring Reader roles, AlloyDB needs
  `roles/monitoring.viewer` (clouds-09).

#### Known limitations

- Two notification stacks can double-post: configure one stack per destination (until
  alerting-06b, -06c and -06d).
- Observation pools (collector, analyzer, Ask Sage, MCP reads) do not pin `search_path` yet:
  keep `public` free of objects other roles create, for example with `REVOKE CREATE ON SCHEMA
  public FROM PUBLIC` (until security-01c-r).
- Databases added through the API (meta-DB members) run reads and actions as one role:
  `action_dsn` works for standalone and YAML fleet databases only, so give those members a
  least-privilege role (until security-09b).
- Large sage indexes on the ledger tables build under the bootstrap lock: plan the first start
  after an upgrade of a large control database for a quiet window (until config-04c).
- `clone.provider: snapshot` is refused at startup: use `none`, `dle` or `lakebase_branch`
  (no snapshot adapter until SPEC T5 lab-05).
- Databricks Apps answers 501 for MCP, specialist and agent-token routes: follow "Service
  principals and MCP" in docs/databricks-apps.md (databricks-09).
- An enforced `set_role_timeout` is not reverted automatically when real cancels exceed the
  shadow prediction: revert it through the action's rollback (until market-08b).

#### Security

- **Separate action identity.** Optional `action_dsn` (`postgres.action_dsn`,
  `databases[].action_dsn`) runs pg_sage's changes (workload budget role changes included) and
  backend cancel/terminate as a second role on the same database, so probes, Ask Sage and MCP
  reads no longer run with owner rights. Neither of pg_sage's two roles can be given a workload
  budget. AP-14 checks both roles and "Grant more" prints one script per role (security-09).
- **Per-database grants.** Fleet and meta-db modes can now limit operators to their own
  databases: set `api.database_grants: true` (off by default) and grant viewer or operator per
  database on the Users page. A user with no grants keeps their global role everywhere. A user
  with grants can approve, execute, roll back, stop and run other operator writes only on
  databases granted as operator. Admins are never limited. Every grant change and every ChatOps
  refusal is recorded in the auth audit chain (apiauth-08).
- **Application search_path for what-if plans.** Workload EXPLAIN and HypoPG what-if checks now
  plan statements on the application's search_path (`ALTER DATABASE`, then `ALTER ROLE ALL`,
  then the server value) and no longer use pg_sage's own session or role path. If your
  application relies on a per-role search_path, set it on the database instead (security-01c).

#### Changed

- **Migration outcomes and budgets.** `apply_migration` now says why it stopped (`reason`,
  including clone-cleanup failures), lists the contract steps it did not run
  (`pending_contract_sql`), and returns a guarded forward-fix `contract_script`. A rehearsal now
  parks a plan flip slowed by more than `rehearsal.plan_flip_pct` (5%), and parks a step that
  would exceed the 10-minute statement timeout or that holds ACCESS EXCLUSIVE longer than
  `migration.max_lock_ms` (2000). `contract_not_before` is now the expand time plus
  `migration.contract_delay_minutes` (60); before, cycles were counted as minutes
  (migration-06).
- **Notification delivery.** Rule deliveries now queue in an outbox on the control database and
  are retried with backoff (network errors, 5xx and 429; up to 6 attempts), and sidecars that
  share a control database never send one twice. Finding alerts (`finding_critical` and the new
  `finding_opened` and `finding_resolved`) follow `alerting.cooldown_minutes`, quiet hours and
  `repeat_minutes` per database, dedup key and channel. Critical alerts bypass quiet hours;
  others wait and are sent once after the window. A new webhook channel type posts JSON with
  sealed headers. Changes to `alerting.cooldown_minutes`, `quiet_hours_*`, `timezone` and
  `repeat_minutes` now apply without a restart. The empty Alert Log now says it shows only the
  config file's `alerting:` block (alerting-06).
- **Vector lab.** vector-lab now measures halfvec HNSW indexes (up to 4000 dimensions),
  `relaxed_order` iterative scans and per-variant `max_scan_tuples` / `scan_mem_multiplier`.
  Ties at the k-th distance are scored by accepting any tied row instead of disqualifying every
  variant. Reports are `format_version` 2 and add `vector_type` and `ambiguous_boundary_ties`
  ([Vector lab](docs/vector-lab.md), indexes-10).
- **Host telemetry for Azure and AlloyDB.** Azure Database for PostgreSQL flexible servers and
  AlloyDB instances now feed host telemetry (CPU, memory, storage, replica lag) to the memory
  gates, CPU index-build admission and the replica-lag and storage guards. Azure is named by
  host, `azure.*` or `cloud_resource.azure`, and needs the Reader and Monitoring Reader roles.
  AlloyDB is named by `cloud_resource.gcp.cluster`/`instance` or `cloud_telemetry.alloydb`, and
  needs `roles/monitoring.viewer`. Throttled provider reads are retried inside the poll
  (clouds-09).
- **Shadow mode scores maintenance.** Shadow mode now scores vacuum, analyze, autovacuum
  reloption and extended statistics decisions as external evidence when the same change is
  found in the catalog (a manual VACUUM/ANALYZE, never autovacuum; exactly the proposed
  reloption; the same statistics object) and no pg_sage action on the table explains it;
  `config_guc`, `query_hint` and `reindex` stay unscored (earned-05).
- **Game days on the customer's hottest table.** Game days now also exercise the monitored
  database's own hottest table on the clone. The customer program `lock_customer_hot_table`
  queues lock requests on the table and reads or writes no row. Each game day records what it
  exercised in `sage.sre_game_days.exercised` and in the game-days API: provider, whether the
  clone holds customer objects, scenarios, and the binding or skip reason. A game day whose
  customer program ran but scored nothing finishes `completed_unscored`, which includes every
  game day on `local_dsn`. A customer program that errors fails the game day. New switch
  `sre.autonomy.game_days.customer_programs`: on by default for `dle` and `lakebase_branch`,
  never on `local_dsn` (benches-12).
- **Databricks Apps service principals.** MCP, specialist and agent-token routes stay 501 until a
  service-principal identity is verified; a header-capture spike App and go/no-go procedure
  ([Databricks Apps](docs/databricks-apps.md), "Service principals and MCP") let operators
  answer the open service-principal question (databricks-09).

#### Added

- **Seasonal workload baselines.** pg_sage folds statement latency, backends, WAL rate and lock
  waits into hour-of-week buckets (`sage.baseline_samples`) and raises `workload_anomaly` when a
  value is far above the same hour in earlier weeks. `query_regression` judges against that
  bucket once it is active, and a critical anomaly opens one read-only Sage SRE investigation
  per episode. Configure under `baseline.*` (on by default, 4 weeks, UTC); findings start after
  3 weeks of history (market-02).
- **Subscriber and CDC health.** New advisory findings: `subscription_errors` (apply or sync
  errors rising, critical when `disable_on_error` has switched the subscription off),
  `subscription_lag` (`analyzer.subscription_lag_minutes`, default 5, or no apply worker),
  `cdc_consumer_stalled` (an active logical slot whose `confirmed_flush_lsn` stood still while
  WAL advanced, `analyzer.cdc_stall_minutes`, default 10) and `cdc_heartbeat_stale` (optional
  `replication.cdc_heartbeats` tables, each read with one bounded `SELECT max`). Subscription
  DDL appears only as manual SQL that needs approval (market-13).
- **Rule packs and owner checks.** `analyzer.disabled_rules` (global, per database through the
  config API, or YAML `databases[].disabled_rules`) switches analyzer rules off and resolves
  their open findings as `rule_disabled`; wraparound, sequence-exhaustion and `sage_*` rules
  cannot be disabled. A new `checks:` list runs your own single SELECT checks read-only and
  bounded, and each row becomes a `custom:<id>` finding that never carries SQL
  ([Owner checks](docs/custom-checks.md), market-11).
- **Workload budgets.** A new `budgets:` list ([Workload budgets](docs/workload-budgets.md)) sets
  statement, idle in transaction and connection limits per role, `application_name` or
  sqlcommenter tag. Warn mode (the default) counts would-be cancels and rejects in
  `sage.budget_shadow` and raises `workload_budget_violation`; `advisor.budget_proposals`
  (default on) proposes a role `statement_timeout` of 3x its p99. A role budget in enforce mode
  becomes `set_role_timeout` (`ALTER ROLE ... IN DATABASE ... SET`) or
  `set_role_connection_limit` actions only after 7 days of shadow data with no protected
  application cancelled. The connection limit is HIGH risk, always needs approval and runs on
  the fleet leader only (market-08).
- **Backup results.** Backup results are now watched, not just backup settings. New findings:
  `wal_archive_failing` (`pg_stat_archiver` failures, critical after a 30-minute streak) and
  `wal_archive_stale` (completed WAL waiting longer than `backup.max_archive_gap_minutes`) on
  any server, and `backup_stale` (the provider's last good backup older than
  `backup.max_age_hours`, default 26) and `backup_retention_below_policy` (below
  `backup.min_retention_days`, default 7) on RDS, Aurora and Cloud SQL. Cloud SQL backup runs
  are read at most every 15 minutes with `roles/cloudsql.viewer`. Missing evidence is reported
  as unknown, never as healthy (market-07).
- **LLM usage ledger.** LLM usage is now persisted per day, database, client role, purpose and
  model in `sage.llm_usage` (control database, kept 400 days). Each row carries the
  prompt/completion/reasoning split, calls, failures and an optional cost from the new
  `llm.pricing` (operator-supplied USD per million tokens; empty by default, NULL cost without a
  price). Read it with `GET /api/v1/llm/usage?days=N`. `pg_sage_llm_tokens_total` gains a
  `purpose` label (llm-04).
- **Pre-merge migration review.** `pg_sage lint-migration --file x.sql` runs in CI (exit 0
  safe, 1 review, 2 risky, 3 tool error; JSON, GitHub annotations or SARIF; `--static` needs no
  database, otherwise set `SAGE_LINT_DATABASE_URL`), and `POST /api/v1/migrations/lint` serves
  any signed-in role (`api.migration_lint_rate_per_minute`, default 30). MCP `lint_migration`
  now shares the advisor's engine, so it applies `migration.ddl_row_threshold`, lists a missing
  `lock_timeout` as advice instead of a score, adds guarded rewrites, and errors instead of
  guessing when a catalog read fails (migration-11).
- **Online column swap.** A schema-guard type-tightening decision now carries a recommend-only
  script (`online_swap_script`) that changes a column's type without a table rewrite: a sync
  trigger, a keyset backfill under `lock_timeout`, one short swap transaction that also moves
  the primary key, sequence, default and referencing foreign keys (re-added NOT VALID, then
  validated), and a final drop of the old column. Each step names the lock it takes. Columns it
  cannot swap online get `online_swap_refused` with the reason. pg_sage never runs the script
  ([Migration safety](docs/migration-safety.md), migration-09b).
- **Upgrade readiness.** `GET /api/v1/upgrade-readiness` and the read-only MCP tool
  `upgrade_readiness` check a database for a target major (`pg_upgrade`, `blue_green` or
  `logical`): tables without a key or replica identity, extensions on a target member, logical
  slots and subscriptions, PG18 data checksums, collation versions and FTS/pg_trgm indexes to
  reindex, removed settings, and dated RDS/Aurora/community support windows. Members at or near
  end of life get advisory `upgrade_readiness` cases daily (market-05).
- **Rehearsal on a Lakebase branch.** Migration rehearsal can now run on an instant Lakebase
  branch. Set `clone.provider: lakebase_branch` and `clone.lakebase.project`. Optional keys are
  `source_branch` (default: the monitored endpoint's branch), `ttl_minutes` (default 60, range
  1-1440) and `endpoint_max_cu` (default 1). Each rehearsal gets its own `sage-rh-*` branch,
  which Lakebase expires on its own after the TTL. Branches are receipted and reaped by the
  fleet leader, and are deleted after the run. A branch or endpoint that is production's is
  refused. Without Databricks auth the rehearsal is recommend-only. The principal needs
  CAN_MANAGE on the project (market-03).
- **Teams, Opsgenie, incident.io and Jira.** Notifications can now go to Microsoft Teams
  (Workflows webhook, Adaptive Card), Opsgenie (alias = dedup key, closed on resolve, us/eu)
  and incident.io (HTTP alert source, firing/resolved), all through the outbox, which now
  honours `Retry-After` (up to 1 hour). An operator can turn a case into one Jira issue from the
  case card; pg_sage reads its status back every 15 minutes (read-only evidence that never
  resolves the case) and comments once when the case is no longer open (market-14).
- **Workload explorer.** A new Workload page ranks the window's statements by load (execution
  seconds per second), calls, mean time or rows from the query store, stacks active sessions
  by wait class (sampled each collector cycle, stored as the new `wait_events` snapshot
  category), and opens a page per query with its history, plan changes, newest captured plan
  and related cases, hints and actions (`GET /api/v1/queries/top`,
  `/api/v1/queries/{queryid}`, `/api/v1/wait-load`) (market-01).
- **Fleet auto-discovery.** Meta-db fleets can discover databases (off by default).
  `fleet.discovery` takes cluster, Lakebase and RDS sources, each with a required owner. It
  finds new databases, branch endpoints and tagged instances every 10 minutes. With
  `reuse_credentials`, it enrols cluster databases at trust observation and approval, up to
  `max_enrolled`. An enrolled member is disabled after `ttl_hours` unless an owner adopts it
  from the new Discovered tab. A member that disappears is disabled and marked gone. Every step
  is audited (market-06).
- **Provider advice.** pg_sage now reads the provider's own advisors hourly (Cloud SQL and
  AlloyDB index advisor view, Azure flexible server index tuning, Supabase performance
  advisors; Neon has none) and gives each piece of advice a verdict against pg_sage's own index
  findings: `agree`, `pgsage_only`, `provider_only` (with whether pg_sage would act, from its
  HypoPG what-if), `conflict`, `unparsed` (never offered as SQL) or `unknown`. The results are at
  `GET /api/v1/provider-advice?database=` and on finding cases (`provider_advice`). Advice is
  evidence only and never raises confidence, risk or autonomy. Set
  `provider_advice.enabled: false` to turn it off; one new migration
  (`sage.provider_recommendations`, `sage.provider_advice_sources`) (market-09).
- **Cost on the Value report.** The Value report now shows dollar cost when you set rates in the
  new optional `cost.rates` block: compute, storage and IOPS cost per instance, idle capacity,
  and the top 20 queries by cost, each figure shown with its rate source and `as_of` date.
  pg_sage ships no price tables. Cloud telemetry now keeps hourly host usage for 35 days
  (`sage.host_usage_hourly`). A new advisory `instance_rightsize` finding (info severity, no
  SQL) suggests one size up or down in the same family after 14 days of usage history
  (market-04).
- **Helm chart and CloudNativePG.** A Helm chart (`deploy/helm/pg-sage`: one replica with
  Recreate, image pinned to the release, non-root read-only pod, optional ServiceMonitor and
  NetworkPolicy) and a CloudNativePG adapter: `postgres.cnpg` / `databases[].cnpg` names the
  Cluster, so pg_sage never runs ALTER SYSTEM there. Instance settings become
  `kubectl patch cluster` proposals with a rollback and a GitOps fragment, read through a
  get-only Role and never applied by pg_sage. Unconfigured CNPG servers open finding
  `cnpg_target_unconfigured` (market-12).

### T4 finish and release (tranche T4)

#### Upgrade notes

- Check your YAML against the ranges in docs/configuration.md: the config file now refuses 0 for
  positive-only keys, intervals below 5, integers above 1073741824 and out-of-range percentages or
  fractions (config-02b).
- Log RCA now starts by default once a readable jsonlog/csvlog source or a provider log sink is
  detected; set `logwatch.enabled: false` to keep it off (rca-09b).
- `custodian.freeze.red_buffer_pct: 100` no longer loads; use 99. A stored API override of 100 is
  skipped with a warning (vacuum-11b).
- pg_sage's own sessions pin `search_path = pg_catalog, pg_temp`: behind PgBouncer, use session
  pooling for pg_sage's connections (security-01c-r).
- The config file's `alerting:` block is now delivered by the notification outbox and shows as
  read-only "From config file" channels and rules; `alerting.engine: legacy` restores the old
  engine for one release (alerting-06b).
- `GET /api/v1/alert-log` is deprecated in favour of `GET /api/v1/notifications/log`
  (alerting-06c).
- On large installs some decision-ledger indexes appear a few minutes after the upgrade, built in
  the background; watch `pg_sage_schema_deferred_indexes` (config-04c).
- Brokered agent sessions set `application_name = 'pg_sage agent:<agp_ id>'`; pgaudit rows written
  before keep the name slug (audit-04b).

#### Security

- **Per-database reads.** With `api.database_grants: true` (fleet and meta modes), a user limited
  by grants now sees only their granted databases on reads. Lists and `?database=all` totals cover
  those databases only. Another team's database answers 404, the same as a database that does not
  exist. Pages that cannot be filtered yet answer `403 fleet_scope_required`. The live-update
  stream drops other databases' events, and a revoked grant stops live updates within 45 seconds.
  Users with no grants, admins, and installs with the switch off see no change (apiauth-08b).
- **MCP tokens and Ask Sage under grants.** With `api.database_grants` on, a person's MCP token
  now reads only the databases on its own list that its owner is granted, checked on every call (a
  revoke applies at the next call, a later grant widens only up to the token's list, a grant-store
  failure answers `-32010 unavailable`). Minting a token that names `*` or any other database for
  a limited owner is refused with `400 database_forbidden`. Ask Sage, its asynchronous runs
  included, answers a database outside the caller's grants like an unknown database (404) and
  stores nothing. Agent tokens are unchanged (apiauth-08c).
- **SSO group grants.** SSO groups can now grant per-database access: set
  `oauth.database_grant_mapping: [{group, database, role}]` (viewer or operator; restart to
  change) and every SSO login replaces the grants the IdP gave before, in one audited transaction.
  Admins' manual grants are never changed by it and win on the same database
  (`idp_grant_shadowed`). If the grants cannot be written, the login is refused with 503. The
  Users page shows each grant's source and keeps IdP grants read-only. Empty (the default) keeps
  today's behaviour (apiauth-08d).
- **Session lifetimes on the database clock.** The expiry is stored as `now() + duration`, so a
  sidecar host clock that runs ahead of or behind PostgreSQL no longer lengthens or shortens login
  and break-glass sessions (apiauth-13).
- **Hidden volatile functions.** EXPLAIN ANALYZE, agent broker reads and RCA tier-2 SQL proofs now
  refuse statements that would run a volatile function the query text does not show: a volatile
  implicit or assignment cast anywhere in the database, or a volatile default argument of a called
  function. Previously both executed inside the read-only transaction (env-cgo-c).
- **ALTER ROLE ... CONNECTION LIMIT.** The parse-tree validator now needs an explicit allowlist
  for it and refuses postgres, pg_ and provider administration roles (set_role_connection_limit
  already refused them at run time, so no working path changes) (env-cgo).
- **Extensions by catalog schema.** pg_sage now finds pg_stat_statements, pgstattuple and HypoPG
  by the schema each extension was created in (from pg_extension) and names their objects
  qualified in every reader (collector, analyzer, tuner, executor checks, Ask Sage and MCP tools,
  self-cost, change feed, provider what-if, cases). An extension outside the role's search_path
  now works everywhere, and the collector follows an extension created or moved after startup from
  its next capability refresh. An absent extension degrades exactly as before (security-01c-q).
- **Application search_path for plans.** On-demand auto_explain capture, the SRE investigator's
  EXPLAIN, the tuner's hinted EXPLAIN, the tuning store's plan fallback and clone rehearsals now
  plan application statements on the application's own search_path (apppath), so they keep working
  once pg_sage's sessions are pinned to pg_catalog, pg_temp (security-01c-n).
- **Pinned session search_path.** pg_sage's own sessions (collector, analyzer, Ask Sage, MCP,
  fleet, meta and action pools) now run with `search_path = pg_catalog, pg_temp`. An ALTER
  ROLE/DATABASE ... SET search_path no longer affects what pg_sage reads, and functions or
  operators planted in public never run inside pg_sage. Extensions are found through their catalog
  schema. Behind PgBouncer, use session pooling for pg_sage's connections (security-01c-r).
- **Action identity for API-registered databases.** Meta-DB databases can now take an action
  identity: set write-only `action_username` / `action_password` on the managed-database API or in
  the Databases form. Changes and backend signals then run as that second role on the same
  database, stored sealed (sage-enc:v1) in two new sage.databases columns and re-sealed on key
  rotation. A credential that no key can open fails closed, and nothing falls back to the
  observation role (security-09b).
- **EXPLAIN errors withhold values.** EXPLAIN errors that agents and models see (the MCP
  explain_query tool, agent plan-only EXPLAIN, and the SRE investigator's EXPLAIN probe) no longer
  quote column values. A SQLSTATE class 22 or 23 server message is replaced with a withheld note,
  its detail, hint and context are dropped, and the SQLSTATE is kept. The explain tools write the
  full text to the pg_sage log at INFO (security-10).
- **pgaudit attribution bound to the principal's roles.** Brokered agent sessions now set
  `application_name = 'pg_sage agent:<agp_ id>'` rather than the agent's name. A pgaudit record
  counts as a principal's only when the session's role is one of that principal's roles: an id
  claim is checked against the role names the id derives, and a name claim against
  `sage.guard_cluster_roles` on the control database (read at most once a minute). Anything else
  is stored as `application_name_unverified`, so one agent can no longer sign its statements as
  another. Direct agent sessions (`correlated_by = role`) now carry the principal id. Rows written
  before this change keep their name slug, because they are hash-chained; docs/security.md shows
  how to map them to ids (audit-04b).

#### Changed

- **One value contract per setting.** The config file, the settings API, stored-override replay
  and the dashboard now read each key's range and allowed values from one tag registry, and the
  Settings page refuses an out-of-range value before saving. Accepted ranges changed where the
  file and the API disagreed. The config file now rejects 0 for positive-only keys such as
  `llm.timeout_seconds`, `safety.lock_timeout_ms` and `retention.*_days`, intervals below 5,
  integers above 1073741824, and percentages or fractions out of range. The API now accepts
  `llm.token_budget_daily: 0` (no cap) and `alerting.cooldown_minutes: 0`, and caps
  `analyzer.self_cost_budget_ms` at 3600000 and `rca.dedup_window_minutes` at 525600. An empty
  config file now loads with the defaults. Check your YAML against the list in
  docs/configuration.md before upgrading (config-02b).
- **Freeze red buffer interval.** `custodian.freeze.red_buffer_pct` now loads only when it is
  greater than 0 and less than 100, and NaN is refused, in the config file, on hot reload, through
  the API and in the Settings page. Before, a value of 100 was accepted and silently run as 25.
  Use 99 instead of 100. A stored API override of 100 is now skipped with a warning (vacuum-11b).
- **Log RCA on by default.** Log-based RCA now starts once pg_sage detects a readable
  jsonlog/csvlog source or a configured provider log sink. `logwatch.enabled` takes `auto`
  (default), `true` or `false`; set `logwatch.enabled: false` to keep it off. With no readable
  source, auto mode reports the source as disabled (`auto: <reason>`), logs one INFO line and
  keeps probing, so fixing `log_destination` turns it on without a restart (rca-09b).
- **Config-file alerting through the outbox.** The config file's `alerting:` block is now
  delivered by the notification outbox, with retries and no double posts. It is reconciled into
  "From config file" notification channels and rules, which the API and dashboard cannot change
  (409 `managed_by_config`). Route and target changes apply without a restart. Open legacy
  PagerDuty pages resolve under their old key. The new `alerting.engine: legacy` restores the old
  engine for one release (alerting-06b).
- **Alert log folded into Notifications.** Every delivery in sage.notification_log now records its
  database and dedup key ("unknown" for fleet-wide events and older rows). The Notifications Log
  tab (now open to operators) and `GET /api/v1/notifications/log` filter by `?database=`, the
  dashboard's #/alerts page redirects there, and `GET /api/v1/alert-log` is deprecated
  (Deprecation and Link headers) (alerting-06c).
- **Deferred ledger indexes.** Startup no longer builds the decision-ledger indexes inline when
  their table has a million rows or more: a background builder, one per database, creates them
  with CREATE INDEX CONCURRENTLY after startup (lock_timeout 2s, retried 1 min to 1 h, cancelled
  by an emergency stop or database removal). On large installs some sage indexes therefore appear
  a few minutes after an upgrade; watch `pg_sage_schema_deferred_indexes{database,state}`. A WARN
  for an INVALID sage index that no step rebuilds now says so and names DROP/REINDEX INDEX
  CONCURRENTLY (config-04c).
- **Investigator support citations.** A model contest or conclusion now keeps its root only when
  one of its kept claims cites evidence that the causal graph lists as support for that root;
  otherwise it becomes inconclusive with the reason `unsupported_root`. Controlled by
  `sre.investigator.require_support_citation` (default `true`, YAML only, restart to change);
  `false` restores the earlier any-cited-claim rule. PGIncidentBench adds a `--compare
  require_support_citation` replay mode that runs both settings on one corpus and prints a
  per-family table (sre-03b).
- **Investigator step budgets on the database clock.** SRE investigations now size each step and
  model turn by the time the database says is left on the lease, not by database timestamps read
  on the host clock. Clock skew between the sidecar host and PostgreSQL no longer starts work the
  database then refuses, and no longer skips work too early (sre-12).
- **Fewer evidence re-reads.** The SRE investigator no longer re-reads every evidence payload
  after each tool call. It reads back only the step it just committed and answers graph_state from
  a session cache, while the final conclusion still re-reads and re-verifies every cited hash. A
  broad run of 6 reads now makes 2 full evidence reads instead of 13, which takes load off the
  monitored database when no meta database is configured (sre-11).
- **Online-migration planning on PostgreSQL's parser.** MCP apply_migration now reads the ALTER
  TABLE with PostgreSQL's own parser instead of a hand-written token reader. Comments no longer
  count as clauses. Names longer than 63 bytes are refused even when the parser would truncate
  them. Refusals come back as invalid_arguments (-32602) with the reason instead of an internal
  error. Builds without cgo (local CGO_ENABLED=0 only; release builds use cgo) refuse
  online-migration planning with "online migration planning needs the parse-tree build"
  (migration-02b).
- **Workload budgets revert themselves.** An enforced role statement_timeout now reverts
  automatically when its statement-timeout cancels, counted from the server log, exceed twice the
  shadow prediction within 24 hours (never fewer than 11), and the budget stays in warn mode for a
  fresh 7 days. Without a readable server log the action needs approval ("auto-revert unavailable:
  no statement log"). New sage.budget_shadow columns: actual_cancels, cancel_source,
  cancels_lower_bound (market-08b).
- **Skip scan on PostgreSQL 18.** The index optimizer no longer proposes a single-column index
  that an existing low-cardinality composite already serves through btree skip scan; the plans of
  the top statements must show this first. Such a candidate is rejected as
  `served_by_skip_scan:<index>` and remembered until that index is dropped. PostgreSQL 14-17
  behave as before (market-15b).
- **Lakebase wait counters.** On Lakebase, the workload explorer's wait-load chart now names
  "Lakebase wait counters" as the preferred source (average active sessions by wait class from
  wait_event_counters, which needs `allow_compute: true`). Performance Insights still comes first,
  and the pg_stat_activity samples are still returned. The wait_event_time unit is assumed to be
  ms and flagged as unconfirmed (market-01b).
- **Emergency freeze profile.** A red table whose VACUUM the watchdog cancelled at
  `safety.vacuum_timeout_seconds` (vacuum_cap) twice in 6 h is now frozen with an emergency
  profile, VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) with vacuum_cost_delay 0. The replaced
  value is recorded as vacuum_cost_delay_before. Index cleanup is skipped until the next normal
  VACUUM. A failing emergency streak escalates once more with profile emergency (vacuum-05b).
- **Time to first finding.** TTFF now counts only warning and critical findings, and only on
  installs measured after the upgrade; an existing TTFF is kept as recorded. The first-look log
  now reads "1 finding, 1 item" (firstlook-07b).
- **AgentSafetyBench leaks in errors.** A masked value leaked through an error message is now
  scored NOT HELD (`leaked_in_error`) instead of "not measured", and a withheld data error is
  credited (`withheld_data_error`); AQ-04 is now scored as a leak, the report moves to
  schema_revision 2, and older baselines still compare cleanly (benches-04r).
- **Accessible confirmations.** Password reset, OAuth identity unbind, agent token revoke and chat
  identity unlink now ask in the dashboard's accessible confirm dialog, which names the target,
  instead of the browser's confirm(). A failed request now shows its error inside the open dialog,
  and these controls keep working when the browser blocks pop-up dialogs (dashboard-10b).
- **Settings dialogs.** The review-changes and trust-level dialogs now use the dashboard's shared
  accessible dialog: focus stays inside, starts on Cancel and goes back to the button that opened
  it. A failed save shows inside the open dialog and keeps the diff. Raising trust to autonomous
  now asks you to type a phrase: the database name, or `autonomous` at fleet scope
  (dashboard-10c).
- **Earned autonomy evidence ages out.** Documented and pinned that no evidence is permanent: a
  signed bench report for the running build stops counting 30 days after `generated_at` like any
  other, so an earned L2/L3 lowers to the supported level until a fresh report or shadow reviews
  arrive (earned-06b).
- **Provider advice stays index-only.** No provider (Cloud SQL Recommender, AlloyDB, Azure,
  Supabase, Neon) publishes PostgreSQL parameter advice; documented in docs/managed-clouds.md
  (market-09b).
- **No slow-COMMIT finding.** pg_stat_statements does not time the commit itself (deferred
  triggers, WAL flush, synchronous standby wait); a waiting standby shows as replication_lag.
  Documented (collection-10b).
- **Version claims.** The walkthroughs, the installation table's Self-managed row and the Cloud
  SQL deployment note now say PostgreSQL 14-18, tested in CI from this release (Cloud SQL stays
  validated live on 14 to 17), and a docs contract test keeps every version claim in line with the
  CI matrix (footprint-10b).
- **Cloud SQL signal grant.** The Cloud SQL / AlloyDB `pg_signal_backend` row in the SRE
  permissions guide no longer cites the retired `cloudsqltests/` setup scripts; it states the
  grant is not exercised by a live test, and a docs test now rejects any guide that names a
  deleted repository path (cloudsqltests).
- **Internal splits, no behaviour change.** The API handler files are split by concern (findings,
  actions, snapshots, config, incidents, managed databases, runtime control) with routes, statuses
  and bodies pinned by a new golden suite (apiauth-30); internal/config's config.go into types,
  load, validate and defaults files (config-30); the schema bootstrap with every DDL string pinned
  by a SHA-256 golden (config-31); the fleet package's types.go and capabilities.go (fleet-30);
  the meta-DB reconnect loop and first-admin bootstrap out of metadb.go (fleet-31); action
  contracts behind one ordered registry with a golden of every contract field (executor-30); the
  Lakebase telemetry store (databricks-30); the slot and archiver fault programs of sre-bench with
  the scenario catalog pinned in a golden (benches-30); the cases handler (cases-30); the Settings
  page (dashboard-30) and the Actions page (dashboard-31).

#### Added

- **Asynchronous Ask Sage.** Ask Sage questions from the dashboard now run asynchronously. `POST
  /api/v1/databases/{db}/ask/runs` answers 202, the page polls `GET .../ask/runs/{id}` and shows
  the steps as they happen, and `POST .../ask/runs/{id}/cancel` cancels. Closing the tab no longer
  stops a question. A restart marks the sidecar's own abandoned runs interrupted and keeps their
  recorded actions. The new `ask.max_concurrent_runs` (default 4, 1-16) caps runs per database:
  over the cap is 429, and a second run on one conversation is 409. New metrics
  `pg_sage_ask_runs_active` and `pg_sage_ask_runs_total`. The synchronous `POST .../ask` (MCP,
  scripts) is unchanged (mcp-07b).
- **What Sage read.** Ask Sage citations now keep an excerpt of what Sage read: the redacted text
  the model was shown, up to 1,500 characters each and 24 KiB per answer. It is stored with the
  answer, purged after `ask.retention_days`, returned by the ask_sage MCP tool, and shown under
  each citation in the dashboard as "What Sage read" (mcp-05b).
- **Migration lint with a read token.** CI can call `POST /api/v1/migrations/lint` with a
  read-scope MCP API token (`Authorization: Bearer`) instead of a session. The token is the only
  credential used, a token's database list is enforced (other databases are 404), with
  `api.database_grants` on a person's token is also limited to its owner's grants, and the rate
  limit counts per token. Not in Databricks Apps, where the endpoint stays session-only
  (migration-11b).
- **PostgreSQL 18 I/O settings.** On PostgreSQL 18 a new `io_settings` info finding (no SQL, works
  without the LLM) flags `effective_io_concurrency` <= 1 or `maintenance_io_concurrency` <= 10
  carried over from an older server (they override 18's default of 16), and `io_method = sync` set
  outside the defaults. The config snapshot now collects `io_method`, `io_workers`,
  `io_combine_limit` and `maintenance_io_concurrency`, and the tuning agent's packet shows them.
  None of them can be executed (market-15).
- **pg_cron and pg_partman checks.** pg_cron_job_failed, pg_cron_history_unbounded,
  partman_premake_gap and partman_default_rows run only where the extension is installed, read its
  schema from the catalog, carry no SQL, and leave open findings open when the monitoring role
  cannot read the extension's tables (see docs/security.md for the grants) (market-16).
- **TimescaleDB checks.** `timescale_job_failed` flags a scheduled compression, retention, refresh
  or user job whose last run failed, and `timescale_cagg_stale` flags a continuous aggregate whose
  watermark lags more than 3 x its refresh policy's schedule_interval plus end_offset. Both read
  the TimescaleDB 2.x `timescaledb_information` views only where the extension is installed, are
  advisory only, and can be switched off by rule pack (market-16b).
- **Deadlock wait cycle.** Deadlock incidents now name the wait cycle: for each participant (up to
  10), the lock mode and object it waited for, the process that blocked it, the relation from
  CONTEXT, and a fingerprint of the statement with its literals and numbers redacted. These are
  stored in log_occurrences.participants, and the incident gets a "Deadlock wait cycle" evidence
  link. New key `rca.deadlock_statement_text` (default fingerprint, which stores no statement
  text; redacted is experimental) (rca-07b).
- **Finding-source filter on Cases.** `GET /api/v1/cases?finding_source=<subsystem>` (e.g.
  `optimizer`, `rules`, `forecaster`) filters open findings on the server before the 500-finding
  cap and leaves out incidents and query hints; unknown values answer 400
  `unknown_finding_source`, every response lists the accepted values in `finding_sources`, and the
  Cases page has a "Finding source" select that keeps the choice in the URL (rules-12b).
- **Hint plan comparison on approval cards.** Approval cards for pg_hint_plan actions
  (apply_query_hint, retire_query_hint) now show the plan comparison that gated the hint: the
  verdict, the cost before and after with the signed change, the reason when the verdict is not
  "changed", and the plans without and with the hint in collapsed blocks. The card hash and the
  chat and Slack text are unchanged (tuning-05b).
- **Instance settings owner.** Each fleet database tile and a new "Instance settings" column in
  the Provider Readiness tab show which database owns the instance-wide settings ("Owns the
  instance settings", "Instance settings handled by <owner>", or "Instance owner unknown" with the
  reason in the tooltip) (advisors-08b).
- **Measured runway in sequence scripts.** Sequence forward-fix scripts now state the measured
  runway rate in step 1 and the PR body: values per second and about how many days until the
  limit, with the time the forecast was read, taken from the open forecast_sequence_runway
  finding. With no forecast, the script says the rate is not measured yet and does not guess
  (playbooks-03b).
- **Four alert rules.** deploy/prometheus/pg_sage.rules.yml adds PgSageTLSCertExpiring (API
  certificate within 14 days of expiry), PgSageLeaderLeaseStale (lease store unreachable for 3
  TTLs, pages), PgSageSSODiscoveryDown (OIDC discovery failing 15 minutes) and
  PgSageApprovalsWaiting (approvals pending for a day), each with a runbook in docs/metrics.md
  (observability-09b).
- **Explain Retry and dashboard browser checks.** The Explain panel now offers Retry after a
  failed explain request. Earned autonomy lists each database's state in a running fleet canary
  ("unknown" when none is stored), and a dialog whose request fails (rollback, revoke, config
  review) keeps keyboard focus inside it. The remaining manual page checks (Cases, Ask Sage, query
  pages, Actions, Settings, Trust, Footprint, the app shell, sign-in, polling, Users, First look,
  Alert log, MCP tokens, Shadow Mode, facts, fleet health, Runbooks, SLOs, the Agents kill switch
  and two-admin freeze release) now run as mocked Playwright specs (dashboard-13, dashboard-14,
  dashboard-15, dashboard-16, dashboard-18, agentgov-07b).
- **Exclusive test server.** Tests that measure cluster-wide state can now run on their own
  server: testdb.Exclusive gives them a database on SAGE_TEST_EXCLUSIVE_URL and holds that server
  for one test at a time. It fails when that URL names the same cluster as SAGE_TEST_DATABASE_URL
  (checked by system_identifier), and when the URL is unset it skips with an exclusive-cluster:
  reason that the CI skip budget never accepts. Every CI job with a test server now starts a
  postgres-exclusive service on port 5433 (ci-12).
- **Test fixtures and clocks.** Statement-statistics resets now clear only the test's own database
  (testdb.ResetStatements); tests that measure WAL, checkpoints, XID ages, I/O or whole-cluster
  counts, or force a CHECKPOINT, run on the exclusive server, and TestResetLint fails on any new
  unscoped reset or CHECKPOINT (ci-13). Database-clock tests read times through testdb.DBNow and
  widen a bound only by the step testdb.ClockWatch measured (ci-14). Packages can opt into
  testdb.RunBootstrapped, one bootstrapped template per run copied per fixture, and a timed-out
  migration step names the deadline that expired (ci-15). `scripts/ci/port-usage.sh` records
  dynamic TCP port use per group and `testdb.Pool` shares one pool per package (ci-16). Seven
  recurring flakes have a named root cause and a fix (ci-17).
- **Test baselines and gates.** The accepted-failure baseline was rebuilt
  (reports/baseline-failures-t4.txt, each entry with suite, package, test, exact error and a [cgo]
  or [posix] tag) and tools/baseline-check.py fails a round on any unlisted failure or changed
  error (ci-18). CI fails a change that adds a file over 500 lines, grows an allowlisted file, or
  adds a Go function over 50 lines beyond the allowlist's count, and any edit, reordering or
  deletion of a migration-ledger line shipped in the last release tag (ci-30). CI fails a package
  whose statement coverage drops under its floor in sidecar/.coverage-floors via cmd/covfloor
  (ci-20). The skip allow-list covers eight version-gated tests, each pinned to its skip text, and
  drops a stale rule (env-pg-a). The assertlint gate's allowlist is empty and it scans cgo-only
  test files (ci-11b).
- **Local Linux suite and helper tests.** `scripts/ci/local-linux-suite.sh` runs CI's Go suite
  with cgo and `-race` on Linux, once per PostgreSQL major (default 14, 15 and 17), using local
  images only, and writes a summary.md that labels each failure real or image-limited (ci-19). The
  Sigstore trusted-root refresher has offline tests against a signed local TUF repository (ci-21),
  and the fakeidp, snapfixture, lockqueue and servertlstest helpers have tests of their own at 90%
  coverage or more (ci-22).
- **Plan and release tooling.** `scripts/plan/tick_todo.py` ticks the improvement plan's todo.md
  from the item reports (dry run by default, `--write` idempotent, `--changelog` lists done ids
  missing from [Unreleased]) (todo-stale). Each release in [Unreleased] now opens with a short
  Upgrade notes list, v2.7.0 carries the YAML-only alerting deprecation, and v2.8.0+ lists 7 known
  limitations with what to do meanwhile; `scripts/ci/release-notes.sh` refuses a directory as the
  CHANGELOG and has its own test (release-notes). The 2026-09-26 full-review checklist is closed
  with every item pointing at its evidence (repo-tasks).

#### Fixed

- **Admin bootstrap.** create_admin and reset_admin_for_test now report an unreachable database as
  "connect to admin database", and create_admin no longer fails with a duplicate key when another
  writer is inserting a user at the same time (the sage.users id sequence only moves forward, and
  concurrent runs are serialized with an advisory lock that waits at most 30s) (apiauth-14).
- **Half-written fleet settings.** A configuration reload or dashboard edit of a fleet database
  could be read half-written by the trust-level owner, the agent kill switch, the meta reconcile
  and the per-database /config API; these now read a locked copy of the database's settings
  (env-race).
- **Tool evidence copies.** An Ask Sage or investigator tool can no longer rewrite the evidence
  values the agent loop grounds claim numbers against (or race the loop from a goroutine it left
  behind); each tool now gets its own copy (env-race-b).
- **Case timelines.** A finding with pending, failed or rejected proposals no longer marks its
  case timeline "actions unavailable": the queue ledger read now returns every queued-action
  column (config-32).
- **Cited investigations.** An Ask Sage citation of an investigation now opens its case even when
  the investigation is older than the database's newest 100: the page reads that one investigation
  by id. A malformed id or a fleet view with no database shows a notice and makes no request
  (cases-12).
- **Double approvals.** Approving or rejecting a pending action in the table view of Needs your
  review now sends one request even when the button is double-clicked (dashboard-31).
- **WAL runway samples.** The WAL runway monitor could skip a new database-size measurement on
  Windows when two measurements read the same clock tick; measurements are now identified by a
  sequence number, not their timestamp (forecast-11).
- **Freeze sample bounds.** The freeze custodian's shared freeze sample (counters, xmin holders,
  table horizons) now runs under `safety.query_timeout_ms` and `safety.lock_timeout_ms`; a catalog
  lock held by DDL ends that cycle with a `freeze age read: lock timeout` error instead of
  stalling the custodian, the runway advisor and the analyzer (vacuum-11c).
- **Join hints for OR joins.** A misestimated nested loop whose join is only an OR (or has no join
  condition) no longer gets a HashJoin hint PostgreSQL could not apply; the finding keeps the
  OR/UNION advice and gains a join_hint_note saying why no join hint is offered (market-10b).
- **Lakebase rehearsal branches.** Lakebase fleet discovery no longer lists pg_sage's own
  short-lived `sage-rh-*` rehearsal branches as Discovered candidates; skipped branches still
  count toward the per-cycle walk limit (market-06b).
- **Broker login errors.** A failed agent broker login now names the broker role and database in
  its error (never the password), so operators can tell which login to check after a credential
  rotation or kill (agentgov-12).
- **ADD COLUMN volatile defaults.** Migration lint now reports ADD COLUMN ... DEFAULT with a
  quoted or schema-qualified volatile function call (for example "random"() or app.now()) as a
  table rewrite on builds without the SQL parser and in the regex classifier (env-cgo-b).
- **End-to-end pipeline test.** The full-binary pipeline test now really executes: on a build host
  without a C compiler it builds with the test-only tag sage_e2e_assume_sqlast, every run dumps
  each seeded object's gate decisions and trust-ledger levels, and a queue verdict without a known
  reason fails the run. Shipped builds are unchanged: a test refuses any build recipe that uses
  the tag (executor-10).
- **Order-independent tests.** The manual CREATE INDEX CONCURRENTLY executor test runs in its own
  database and checks the action id, the action_log row and index validity (executor-11; its
  race-detector run waits for a cgo Go image, executor-11r). The statement-delta attribution test
  runs alone on the exclusive server, fails with "the premise does not hold" if pg_stat_statements
  evicts entries, and takes its window from the server clock (market-04b). The Databricks Apps
  self-test passes on Windows, with the restored binary's owner-only mode checked in a POSIX-only
  test (databricks-12). The meta lifecycle fixture no longer fails under a shuffled order, and
  cmd/pg_sage_sidecar measures 79.9% (Windows) and 80.0% (Linux, PG17) as one package (config-14).

### T4 follow-ups (tranche T4c)

#### Security

- **ALTER ROLE ... CONNECTION LIMIT.** The parse-tree validator now accepts `ALTER ROLE ...
  CONNECTION LIMIT` only with an explicit allowlist, and refuses postgres, pg_ and provider
  administration roles. The parse-tree layer is now verified under cgo and `-race` on PostgreSQL
  14, 15 and 17 (env-cgo).

#### Changed

- **Parser refusals in migration lint.** Migration lint now ends the note of a statement the SQL
  parser refused with the parser's own message (for example "The SQL parser refused it: syntax
  error at end of input."). Literals stay redacted, and no statement's lock class changed
  (env-cgo-b).
- **EXPLAIN guard bench score.** The safety bench's EXPLAIN guard now scores AQ-04 (a masked value
  in a cast error) as held (`withheld_data_error`) instead of leaked, matching security-10's
  withheld agent-facing errors. The committed baseline and the published results table
  (explain_guard 23/23) are updated from a cgo PostgreSQL 17 run (env-cgo-d).

#### Added

- **Local Linux suite toolchain and server options.** The local Linux CI suite runner can now take
  its Go toolchain from a Docker volume (`GO_ROOT_VOLUME`). Its PostgreSQL server cap and data
  directory can be set (`LOCAL_SUITE_PG_MEMORY`, `LOCAL_SUITE_PG_TMPFS=off`). It unpacks the
  source inside the container, and it makes loopback logins pass scram-sha-256 like CI's, so a
  full `-race` run on 14, 15 and 17 completes and reproduces CI's password checks (ci-19).
- **Local Linux suite: PSS server and package scope.** `scripts/ci/local-linux-suite.sh` takes
  `LOCAL_SUITE_PSS_SMALL=1`, which starts a server with `pg_stat_statements.max=100` so the
  attribution dealloc test runs instead of skipping, and `LOCAL_SUITE_PACKAGES=<./pkg ...>` for a
  scoped cgo/`-race` run; with both, the agent broker measures 90.6% coverage with zero skips on
  PostgreSQL 14 and 17 (agentgov-12).

#### Fixed

- **Post-action monitor clock.** The post-action monitor now runs its verification windows, its
  schedule, the expiry of a resumed monitor and the pre-action baselines on the database clock
  (`clock_timestamp()`) instead of the sidecar host's clock. A database clock that leads the host
  no longer drops the newest samples or delays the first check. If the clock read fails, the
  monitor uses the host clock and records why in the verdict's evidence (`clock.source`,
  `clock.reason`) (executor-31).
- **Runtime wiring races.** A drained fleet database no longer leaves its LLM client release
  running, standalone mode creates its fleet manager before any runtime worker reads it, and
  `logwatch.poll_interval_ms` is read under the hot-reload lock (env-race).
- **Cases API race.** A per-database cases read that outlives its request at the deadline no
  longer reads shared package state (env-race-b).
- **Notifications Log in the embedded dashboard.** The embedded dashboard is rebuilt from current
  sources, so the Notifications Log tab now shows why a log read failed (server message and HTTP
  status) instead of calling the log empty (dist-regen).
- **Race-clean and isolated tests.** The EXPLAIN ANALYZE guard's real-parser tests now run cleanly
  with cgo and `-race` on PostgreSQL 14 and 17; no guard behaviour changed (env-cgo-c). The
  manual CONCURRENTLY index-build tests (TestCoverage_ExecuteManual_*) now pass 50 times in a row
  under the Go race detector on PostgreSQL 14 and 17, which closes out the April race-flake
  report (executor-11r). The emergency-freeze lifecycle test and the three pg_stat_statements
  ranking and text-read collector tests now run on the exclusive test server
  (SAGE_TEST_EXCLUSIVE_URL), which starts with an empty pg_stat_statements, so other packages'
  work on the shared server no longer makes them flake (ci-23).

### T5 foundations: Evidence, Lab, Plan, Inbox (tranche T5)

#### Security

- **Workload pack model tier.** Workload evidence packs now show what would leave the network
  before any model reads it. The database page's new Evidence packs card previews the model tier:
  statement texts redacted (numbers included), numeric column statistics only, no plan filter
  values, and sparkline charts. Identifiers can be hashed. A leak guard refuses the tier if a
  seeded value or credential-shaped token survives. Admins can download a signed export (`GET
  .../workload-packs/{sha}/export`, recorded as `evidence_exported`), and the new MCP tool
  `get_workload_pack` returns the model tier only (evid-02c).
- **Do not touch.** A new "Do not touch" fact (`do_not_touch`) for tables, indexes and schemas
  blocks every change pg_sage would make to them, operator-approved or not, until a person expires
  the fact. Only pg_sage's own rollbacks still run, and findings about the object stay listed as
  advice. The Facts page gains an Owner brief form that shows a peak-window suggestion measured
  from 28 days of the hourly query rollup (`GET /api/v1/databases/{db}/facts/peak-suggestion`);
  saving the form declares the brief (evid-03b).
- **Plan audit chains.** Plans now leave a tamper-evident audit record in two new append-only
  audit chains: `plan_events` (every plan create, edit, approval and void, written in the same
  transaction as the change, with who made it) and `plan_model_records` (every model pass a plan
  was built from, raw output kept byte for byte up to 1 MiB with its SHA-256). A plan's replay
  input is returned only when it still matches its hashes and chain. The evidence pack of an
  action run by a plan step now includes the plan's events and model records with their chain
  proofs (plan-01b).

#### Changed

- **Verification v2 core.** Verification v2's core is in place, not yet deciding verdicts: a step
  is judged by shared blocks per call, with time per call as a guardrail. Targets are pooled with
  call shares frozen before the step, so a traffic shift no longer reads as a gain. The step is
  compared with the same hour-of-week of earlier weeks when 3 or more exist, and the median change
  of at least 3 untouched control statements is subtracted. Watches now store their frozen weights
  (lab-01a).
- **Verification v2 guards.** Verification v2 gains three guards, not yet used in verdicts. An
  index step is credited only for targets whose plan changed at the step and uses the index;
  otherwise the result is `no_mechanism`, or `mechanism_unavailable` when plans cannot be read. A
  CUSUM scan marks a window `contaminated_at=<time>` when an unrelated break happens after the
  step. Repeated looks share alpha with O'Brien-Fleming spending, and the spend is stored with the
  watch so a restart continues it (lab-01b).
- **Verification v2 in shadow; expired monitors ask.** Verification v2 runs in shadow until the
  back-test passes. New `verify.method` (`v1`, `shadow`, `v2` or `auto`; default `auto`, which is
  shadow until a passing back-test) and `verify.on_expired` (`ask`, `keep` or `revert`; default
  `ask`). A step whose targets run too rarely to be judged within the verification window now
  needs approval at every trust level. The outcome ledger records which verifier decided and the
  other verifier's verdict, and the dashboard shows "No change", "Inconclusive" and "v2 would say:
  ..." (lab-01c).
- **Verification ladder.** Each queued change records what it was tested on before production
  (plans only, a clone, production), shown on the approval card and as the highest tier on the
  outcome ledger row. A drop, an index replace, an `ALTER TABLE` or a restart-only setting now
  needs approval at every trust level unless it was rehearsed on a clone. Automatic rollbacks are
  not affected. The migration adds `sage.action_queue.ladder` (lab-03a).
- **Fleet canary on a measured number.** Fleet canary rollouts now halt on a measured number: each
  database's effect on its target queries, net of the median change of the same queries on the
  databases not yet changed (or its own verification v2 effect when there are fewer than two such
  controls), compared with `sre.autonomy.canary.regression_limit_pct`. Databases with no v2 number
  are judged by their verdict flag, marked "flag only". The new
  `sre.autonomy.canary.stage_wait_hours` (default 24) pauses between databases.
  `sage.rollout_instance` records `effect_pct`, `control_pct` and `effect_method` (lab-03b).
- **Clone receipts and limits.** Clones now say what they are and what they cost: every clone
  receipt records its kind and a cost label (instance-hours, priced only from your `cost.rates`)
  before the clone is requested. A full-copy restore creates nothing unless
  `clone.allow_full_copy` is true (default false). `clone.max_creates_per_hour` (default 4) caps
  clones per deployment, and creates back off after provider 429/5xx replies. Clones left by a
  killed sidecar are reaped once its heartbeat is 5 minutes old instead of waiting for their TTL.
  `clone.provider: snapshot` is still refused, now naming its replacements `rds_restore`,
  `cloudsql_clone`, `alloydb_restore` and `azure_restore` (lab-05a).
- **Home is the Inbox.** Home is now the Inbox: decisions waiting on you, decided in place, and
  the changes made since your last visit (kept per user across devices), with Undo. The one nav
  badge moves from Actions to Inbox (N+, 99+). Rollback asks for a reason. The Value page moved to
  `#/value` (ux-01b).
- **Actions accessibility.** Actions now opens on Needs your review when approvals wait. Approve,
  Reject and danger buttons use dark fills that reach WCAG AA under white text. The approval cards
  answer j/k/a/r/s, and `a` always asks you to confirm. On screens narrower than 768 px the card's
  decision row stays at the bottom of the screen. The scheduled-approval message now names where
  to cancel the approval (ux-05a).
- **Held back.** The Actions "Decisions" tab is now "Held back". Its rows and a case's "Why hasn't
  this run?" show verdicts and reasons in words, with a next step for budget, verification-wait,
  emergency-stop, replica and provider-managed holds, and each row links to its exact approval
  request or finding (ux-05b).
- **Page guides.** The meme and "About this page" guide now open only on your first visit to each
  page, then fold to a one-line toggle; phones (below 768 px) no longer show them. Error and Not
  found pages link "Go to Inbox". An empty Value page says the first verified change earns the
  first credit and links to the first look (ux-05c).
- **Time zones and keyboard tabs.** Every time in the dashboard now shows its zone (2026-11-15
  14:05 CST), and a header Time zone switch (Local or UTC) changes every time on the page and is
  remembered per browser. The five tab bars (Actions, Snapshot, Fleet, Notifications, Settings)
  are proper keyboard tabs with Arrow, Home and End. Grant more is a real modal (Escape closes it,
  focus returns), and table headers are announced per column (ux-05d).
- **Agent requests and long database lists.** External agent requests now link to their
  investigation on Cases and, for a queued cancel remediation, to its approval (new `queue_id` on
  `GET /api/v1/specialist-requests`). The Databases table has a name/host/tag filter and shows at
  most 100 rows at a time. From six databases, the header scope picker adds a "Find database" box
  (ux-05e).
- **MCP errors say what to do next.** MCP tool errors now tell agents what to do next: `next` (a
  tool to call, the same tool with its required arguments, or "wait"), `retry_after` in seconds
  where pg_sage knows it (the rate limiter's reset, 30 s for an unavailable dependency) and
  `approval_url` for approvals reserved for a person (paired with `next: get_inbox`). `code`,
  `reason` and `message` are unchanged. The `get_policy`, `request_change`, `get_ledger`,
  `get_value`, `optimize_query` and `set_maintenance_policy` descriptions now say when to use each
  tool, what it returns and what it never does (ux-05f).

#### Added

- **Hourly query history.** pg_sage now keeps hourly per-query history beyond 14 days: a rollup of
  the snapshot queries category (calls, time, rows, blocks, temp, WAL per queryid and UTC hour,
  epoch breaks flagged, never negative) in `sage.query_rollup_hourly`, with per-hour coverage in
  `sage.query_rollup_coverage`. It backfills 90 days in about 7.5 hours per database and is
  reported by `pg_sage_query_rollup_hours_total` and `pg_sage_query_rollup_lag_hours` (evid-01a).
- **Long query history and coverage.** Per-query history keeps a daily tier for 400 days and
  hourly rows for 35 days (new settings `retention.query_rollup_hourly_days` and
  `retention.query_rollup_daily_days`). History migrations now carry the rollup with them. A new
  `GET /api/v1/queries/coverage` endpoint, plus a one-line notice in the workload explorer,
  reports how much of the execution time the long history covers (evid-01b).
- **Workload evidence packs.** `POST /api/v1/databases/{db}/workload-packs` (operator) builds a
  hashed, versioned bundle (`pg_sage-workload/1`) of a database's workload over 14-100 days
  (default 28). It holds statement families with an hour-of-week shape, coverage, and weekly and
  capacity trends with slope, R2 and standard error. Every fact has a stable evidence id. The same
  window and data always give the same `content_sha256`, and a pack is stored once and never
  changed. `GET .../workload-packs`, `.../{sha}` and `.../{sha}/evidence/{id}` read packs (viewer,
  scoped to the database). Also adds `retention.workload_packs_days` (default 180) and the metrics
  `pg_sage_workload_pack_builds_total` and `pg_sage_workload_pack_bytes` (evid-02a).
- **Workload pack sections.** Workload evidence packs now also hold indexes, settings, autovacuum,
  schema, plans (shape only), connections by hour and age, provider, column statistics and
  owner-context sections, each fact with an evidence id and "unknown" rather than zero where
  pg_sage could not measure it. Builds read the catalog under the safety timeouts, and plan
  conditions or `pg_stats` value samples enter a pack only when that build opts in
  (`include_conditions`, `include_value_samples`) (evid-02b).
- **Owner brief.** Record an owner brief per database (owner, contact, peak windows, growth,
  growth % per month, retention, note) with `POST /api/v1/facts` or MCP `propose_fact` (type
  `owner_brief`, subject kind `database`, subject `*`). It never binds, is never expired for
  absence, goes first in model prompts, and is measured by `pg_sage_owner_brief_present` and
  `pg_sage_owner_brief_steps` (accepted vs rejected queued actions before and after the brief)
  (evid-03a).
- **Verifier back-test.** Verification v2 becomes the verifier per database once its own back-test
  passes. pg_sage replays 300 placebo steps over the stored history every week on the leader, and
  the new `pg_sage_sidecar verifier-backtest --database NAME` command runs one by hand. The run
  stores the placebo false-positive rate (with its Wilson 95% bound), the inconclusive share and
  the detection rate on injected 30% drops in `sage.verifier_backtest`. `verify.method: auto` acts
  on v2 only when the latest row has at least 200 usable placebos, a rate at or below 5% and
  detection at or above 80%, and ran within 35 days. New settings:
  `verify.backtest_max_runtime_minutes` (default 10) and `verify.backtest_placebos` (default 300).
  The bench fixture rates are in docs/generated/verifier-backtest.md (lab-02).
- **Index rehearsals on a clone.** Index proposals are now rehearsed on a clone when
  `clone.provider` is set (`dle` or `lakebase_branch`). The tuning agent offers a rehearse tool
  and the index floor rehearses each candidate it admits. pg_sage builds the index on a receipted
  clone after checking that the clone is not production. It then compares shared blocks per call
  before and after for read-only statements that have no parameters, and records the result as the
  tier-2 ladder entry on the approval card. New settings: `clone.rehearsals_per_hour` (default 2)
  and `clone.rehearsal_statement_timeout_seconds` (default 5) (lab-04a).
- **Migration rehearsals measure the workload.** Migration rehearsals now measure the 20 busiest
  production statements that reference the migrated table (texts from pg_stat_statements) on the
  clone before and after the expand steps. A plan or latency regression parks the migration with
  the statement named. A measured workload with no regression promotes the expand steps. The
  verdict stays inconclusive, with the reason, when production has no pg_stat_statements or no
  statement references the table. The result is a tier-2 ladder record with the clone receipt,
  returned in the `apply_migration` outcome (`affected_queries`, `ladder`) and kept on
  `sage.migration_run` (`measurement.ladder`, `clone_id`) (lab-04b).
- **Neon branch clones.** `clone.provider: neon_branch` rehearses migrations and index changes on
  a copy-on-write Neon branch of production (`clone.neon.project_id`, `parent_branch`, `api_key`
  or `SAGE_CLONE_NEON_API_KEY[_FILE]`, `endpoint_max_cu`, `ttl_minutes`). Each branch is named
  `sage-*`, asks Neon to expire it, and is deleted on failure or found by name by the reaper.
  Without an API key, rehearsals are recommendation-only (lab-05b).
- **Aurora clones.** `clone.provider: aurora_clone` gives each rehearsal an Aurora PostgreSQL
  copy-on-write clone of the monitored cluster (one instance, ready in 10 to 20 minutes), with the
  monitored DSN pointed at the clone's endpoint. Configure it with `clone.aurora.{region,
  instance_class, ttl_minutes}` and the AWS default credentials. It refuses a 16th copy-on-write
  clone, the cluster is tagged so the reaper can find a leaked clone, and docs/managed-clouds.md
  has the IAM policy it needs (lab-05c).
- **RDS restores.** `clone.provider: rds_restore` rehearses on a point-in-time restore of the
  monitored RDS for PostgreSQL instance. Each restore is a full copy billed at the full instance
  price. It needs `clone.allow_full_copy: true` (startup refuses it otherwise), logs a cost
  warning at startup, and takes `clone.rds.region` / `clone.rds.instance_class` plus
  `clone.restore_deadline_minutes` (default 120). Restores are tagged so the reaper deletes any
  that leak (lab-05d).
- **Cloud SQL clones.** `clone.provider: cloudsql_clone` rehearses on a Cloud SQL for PostgreSQL
  instance clone. It is a full copy: it needs `clone.allow_full_copy` and
  `clone.cloudsql.{project,instance}`, is bounded by `clone.restore_deadline_minutes`, and the
  clone is labelled `pg-sage-receipt` so the reaper can find and delete leaks (lab-05e).
- **AlloyDB restores.** `clone.provider: alloydb_restore` rehearses on a point-in-time restore of
  the monitored AlloyDB cluster's continuous backup to a new cluster plus a zonal primary
  instance. It is a full copy: it needs `clone.allow_full_copy` and
  `clone.alloydb.{project,location,cluster}` and is bounded by `clone.restore_deadline_minutes`
  (lab-05f).
- **Azure restores.** `clone.provider: azure_restore` rehearses on a point-in-time restore of the
  monitored Azure Database for PostgreSQL flexible server to a new `sage-*` server in the same
  region and resource group. It is a full copy: it needs `clone.allow_full_copy` and
  `clone.azure.{subscription_id,resource_group,server}` and is bounded by
  `clone.restore_deadline_minutes`. Clones are tagged with their receipt and keep the source's
  subnet and customer key, and untagged servers are never deleted (lab-05g).
- **Plans.** New `sage.plan`, `sage.plan_step`, `sage.plan_step_edge` and `sage.plan_approval`
  tables (migration ddlPlans) with validation (DAG edges, executor contracts and SQL allowlist,
  required rollbacks, evidence-cited steps, up-front unverifiable rule) and a plan-v1 hash that
  binds an approval to what runs. Editing a step or edges voids the approval, and a direct SQL
  edit reports `hash_mismatch`. Terminal plans age out with `retention.actions_days`, and workload
  packs a plan cites are kept (plan-01a).
- **Plan engine.** Each database's plan engine runs an approved plan one step at a time through
  the executor's normal approval path (gate, trust, Apply, verification). It waits for each step's
  verdict before the next step starts, waits for other changes on the same queries or objects,
  keeps, reverts or halts on each verdict as the step says, and sends one notification when a plan
  halts. It runs only plans a person approved, needs no configuration, runs only on the fleet
  leader, and pauses under the emergency stop. Approving a plan step's queue item from the Actions
  page or the scheduled runner runs nothing, and no approval card is sent for it (plan-02a).
- **Plan caps and never-automatic steps.** The standing policy gains a `plans` block
  (`max_new_indexes_per_table` 2, `max_new_index_bytes_pct` 10, `max_restart_bound_steps` 1,
  `peak_windows`). A plan over a cap is refused when it is proposed or approved. A step that would
  cross a cap, or start inside a peak window, waits, and expires past its deadline. Steps that are
  never automatic stop with `awaiting_approval` whatever the plan's approval says: restart-bound
  settings, rewrites, dropped data, type changes, revokes, topology or pooler changes, and changes
  that cannot be undone (plan-02b).
- **Revert to a checkpoint.** `revert_to_checkpoint` reverts the failed step and every kept step
  back to the latest checkpoint, newest first. It never reverts a step that a kept step outside
  the walk depends on, and then halts the plan with what it reverted and kept, and why. Every plan
  revert is checked in the catalog before the step counts as reverted; a revert that does not land
  fails the step and halts the plan. Validation now refuses a checkpoint that cannot run before
  the step that reverts to it (`checkpoint_not_before`) (plan-02c).
- **Operator steps.** Plans can include operator steps ("you run this"). pg_sage tells the
  operator the command to run, waits until the change shows up in pg_settings or the managed
  change proposal is marked applied, then judges it like any other step. It never reverts one: if
  the change makes things worse, the plan halts and the notification gives the operator's revert
  command (plan-02d).
- **Plan resume.** Plans resume after a sidecar restart or a fleet leader change. Each step left
  in flight is read back from its queue item and the action ledger: it is started again, run once,
  judged, or has its revert read back, and is never run twice. A step whose state cannot be
  determined halts the plan as `interrupted_unknown`, with one notification. Plan step claims use
  the leader's holder id and the elector's lease, so an old leader that wakes up can no longer
  move a step (plan-02e).
- **Plan approval API and CLI.** Plans can be approved all at once, up to step N, or step by step,
  from the new `/api/v1/plans` routes or the new `pg_sage_sidecar plan
  list|show|approve|reject|resume|cancel` command. The command uses an operator API token: read
  scope to read, approve scope to decide; agent tokens are refused. Never-automatic steps still
  need their own approval, and editing a plan voids every approval (plan-03a).
- **Plans in chat and MCP.** Proposed plans go to the chat channels that already get approval
  cards, with Approve all / Approve up to step K / Approve step 1 only / Reject buttons; only
  steps of risk tier safe or moderate can be approved from chat, and the rest needs the app.
  Agents can propose plans over MCP with `propose_plan` (at most 5 open per principal) and read
  them with `list_plans` and `get_plan`, but no MCP tool approves a plan (plan-03b).
- **Inbox.** `GET /api/v1/inbox` and `/api/v1/inbox/count`, and the read-only MCP tool
  `get_inbox`, list every decision waiting on a person (change approvals, plans, autonomy
  promotions, proposed facts, unsigned runbooks, agent capability requests and policy proposals)
  across the databases you may read, newest risk first. A source that could not be read is named
  and the total marked as a lower bound. New metrics `pg_sage_inbox_items` and
  `pg_sage_inbox_decision_seconds`; decision requests may send `X-Sage-Surface` (ux-01a).
- **Outcome ledger.** One row per change pg_sage made in a window, giving what, why, origin, the
  expected and measured effect, the verdict (pending until judged) and whether the change can
  still be rolled back. It is available as `GET /api/v1/outcomes`, the dashboard's new Ledger page
  (`#/ledger`), `pg_sage ledger --since 24h [--json]` and the MCP tool `get_outcomes`, with the
  new metric `pg_sage_outcomes_measured_ratio`. Value's incident evidence ids now link to the
  ledger (ux-02a).
- **Plans tab.** Actions has a Plans tab (first in the tab bar) to review a multi-step plan.
  Stages show status, evidence, exact SQL, expected and measured effect, ladder tier, gate and
  rollback with its dependants; operator stages read "You run this". You approve it all at once,
  up to stage N, or step by step, bound to the plan hash ("This plan changed after approval;
  approval voided" when it is stale). Cases show "Part of plan: <goal>", and /metrics exports
  `pg_sage_plan_approvals_total{mode}` (ux-03).
- **Digest.** The scheduled briefing sends a digest (Did, Verified or reverted, Saved, Needs you,
  Could not do and why) as the `digest_ready` notification event through your Notifications
  channels. By default it is sent only when something changed (`briefing.only_when_changed`).
  `briefing.slack_webhook_url` becomes the managed channel `config:slack:briefing`. An optional
  `briefing.dba_hour_rate_usd` shows saved hours in dollars. Home shows the latest digest
  (ux-02b).
- **pg_sage doctor.** `pg_sage doctor --pg-url ... [--config sage.yaml] [--json] [--send-test]` is
  a read-only preflight that checks the connection, PostgreSQL version, role grants (with the
  exact GRANT), pg_stat_statements/HypoPG, provider, query text visibility, model key, a test
  notification, the meta database schema and clock skew, each with a copyable fix (exit 0/1/2, DSN
  never printed). The same report is at admin-only `GET /api/v1/doctor`, and each database logs
  its doctor counts once at startup (ux-04a).
- **Setup wizard.** A new install opens on "Set up pg_sage", four steps you can skip: Connect
  (paste a URL, get the provider's role SQL and a doctor report), Model (endpoint and key, tested
  in place), Where to reach you (one channel and a test send) and How much may I do (Watch only,
  Ask me first, Do safe things and tell me). The checklist gains a Model step, `POST
  /api/v1/onboarding/setup` records each finished step, and
  `pg_sage_setup_within_hour_ratio{part=model|channel}` is exported (ux-04b).

### T6: Sage Review, restore drills, fleet policy, change guards (tranche T6)

#### Security

- **Exposure sweep.** Agent posture now sweeps for exposure between analyzer cycles: every
  `agents.exposure.sweep_seconds` (default 60, range 15-600) pg_sage reads the catalog hash. When
  the catalog changed, or a database was just enrolled, it runs AP-01 to AP-05, AP-15 and the new
  AP-17 at once, so a table exposed to anon is flagged within about a minute. AP-17 flags agent-like
  sessions connected as a table owner, a BYPASSRLS or superuser role, or Supabase's service_role.
  Exposure findings carry mode shadow and the action that would close them (fleetpol-02a).
- **Exposure remediation (opt-in).** Set `exposure.mode: enforce` in the standing policy document to
  turn AP-15 PUBLIC grants into `revoke_public_grant` actions and AP-03 tables into
  `enable_row_level_security` proposals. The revoke is moderate, reversible by its stored exact
  re-GRANT, and reverted if a reader that relied on PUBLIC stops. Enabling RLS always needs approval
  and shows the affected statements and the roles that would lose access. Nothing changes until an
  operator sets `enforce`. Adds the `sage.exposure_remediations` table (fleetpol-02b).
- **Supabase Data API exposure.** On Supabase, AP-03 and AP-04 findings now show whether the
  project's Data API serves the finding's schema (`detail.data_api_exposed`: true, false or
  unknown). pg_sage reads this hourly from the Management API using
  `SAGE_SUPABASE_OBSERVABILITY_TOKEN`. A schema the Data API is known not to serve lowers AP-03 from
  critical to warning; unknown keeps it critical (fleetpol-02c).
- **Backup isolation.** pg_sage now reports when every backup of a managed database would be deleted
  along with the instance (`backup_shares_delete_path`; critical on production with the agent gate
  on), when a production instance has deletion protection off (`deletion_protection_off`), and when
  the point-in-time window is shorter than `backup.min_pitr_hours` (default 24;
  `pitr_window_below_policy`). It reads RDS and Aurora manual snapshots and restore windows
  (bounded, cached 6 h) and Cloud SQL's retainBackupsOnDelete. Set `backup.external_source` to
  declare a backup kept elsewhere. RDS needs rds:DescribeDBSnapshots, rds:DescribeDBClusterSnapshots
  and rds:DescribeDBInstanceAutomatedBackups (drill-01b).

#### Changed

- **Change feed.** Sage SRE's change feed now puts server version changes (minor and major), host
  changes (server address, system identifier) and restarts or failovers on the timeline even with
  `sre.change_events.feed_enabled` off. Each is labelled operator, provider or unknown. With cloud
  telemetry it also reads RDS/Aurora DescribeEvents (needs rds:DescribeEvents) and Cloud SQL
  maintenance and failover operations, as source `provider_api` (guard-02b).
- **Enrollment by policy set.** Fleet discovery now enrolls by policy set. Each candidate carries
  tags, a creator and its creation time: RDS tags, cluster database comments (`pg_sage:
  creator=agent:x`), Lakebase branch times and a new read-only `neon` source. Each set enrolls on
  its own `enrollment.interval_minutes` (default 10, min 2) and `enrollment.per_cycle` (default 5,
  max 20), with its owner rule and `ttl_hours`. Members get a first Sage Review on day one, and
  per-set time to enroll and 24 h baseline share are exported as `pg_sage_fleet_enroll_seconds` and
  `pg_sage_fleet_baseline_24h_ratio`. Enrollment still starts at observation with approval
  (fleetpol-01b).

#### Added

- **Sage Review grade.** pg_sage now grades each database against a versioned "well-run Postgres"
  rubric (`pg_sage-rubric/1`, eight dimensions). The grade runs once after the first look and then
  every `review.grade_interval_hours` (default 24). It is deterministic and read-only, and a check
  that could not run can only lower the grade. The grade is served at `GET
  /api/v1/databases/{db}/review/grade` and through MCP `review_grade`, and the time to the first
  grade is reported as `pg_sage_review_first_grade_seconds`. It is on by default (`review.enabled`)
  (review-01a).
- **Review page.** The Review page at `#/review` shows each database's grade, its eight dimensions
  and their checks, what changed since the last grade, and the ranked report. Unknown always reads
  "Unknown: <reason>", never a grey letter. Grade chips on the first look and the database page link
  to it. Opening a finished review records a read receipt in `sage.review_view` through `POST
  /api/v1/databases/{db}/review/runs/{id}/viewed`, once per user (review-01b).
- **Index portfolio.** Sage Review now proposes index portfolio candidates over a 90-day window
  (`review.window_days`, 14-100): exact duplicate drops, drops of indexes unused for the whole
  window, and consolidation sets validated by hiding the set in HypoPG (advice when HypoPG is
  unavailable). Each candidate cites its pack evidence, states the bytes reclaimed and write blocks
  saved per day, and is stored in `sage.review_candidate` as a proposal only (review-02a).
- **Configuration and autovacuum.** Sage Review now judges the server's settings as a set and
  proposes per-table autovacuum settings. A fixed rule table checks shared_buffers, work_mem,
  max_wal_size, random_page_cost, the PostgreSQL 18 I/O settings and
  idle_in_transaction_session_timeout against memory, temp files, checkpoints, storage and idle
  sessions. Values are clamped to pgconf's safe range. Restart-bound and managed-service changes
  become operator steps that pg_sage never runs, at most one restart-bound step per review.
  Per-table autovacuum steps are sized so that dead tuples peak under 5% of live rows (review-02b).
- **Plan stability.** Sage Review flags plan-stability cliffs on top queries: nearly-all-NULL
  filtered columns, skew beyond the sample, time-ordered inserts past the histogram, and correlated
  predicates. It proposes the new moderate, reversible `set_column_statistics` action (10x the
  target, capped at 10000, rolled back to the prior target) or pg_sage's extended statistics, and
  reports a table whose statistics it cannot read as unknown with the exact GRANT (review-02c).
- **Schema debt.** Sage Review now has a schema-debt generator. It turns int4 keys and narrow
  foreign keys into a measured runway from the runway samples ("exhausts in 247 days", with an error
  range, or "rate not measured yet" when there are fewer than 3 samples). Under 365 days it proposes
  a capacity migration. It also drafts online column swaps for type changes and gives advice on
  timestamp-without-time-zone columns, unpartitioned tables of `review.large_table_bytes` or more
  (default 10 GiB), and queue or soft-delete tables. Every step needs approval (review-02d).
- **Connections.** Sage Review's new connections generator finds recycle herds (more than 30% of an
  application's sessions connecting in the same minute across 3 samples) and each application's
  idle/active mix, with pooler sizing advice. It also proposes approval-only
  `idle_in_transaction_session_timeout` and role `CONNECTION LIMIT` steps (never below the role's
  observed peak) when connection headroom at peak is under 10%. The collector now records client
  sessions by application and connection minute (review-02e).
- **Capacity.** Sage Review gains a capacity section: 30/90/365-day size projections for the
  database and its largest tables, each with a fit error and band; storage-headroom and right-size
  advice with dollar figures only from `cost.rates` ("unknown: no operator rates" otherwise); an
  idle-database rule (14 or more observed days with no workload calls); and a stored 30-day forecast
  error. None of it ever changes an instance (review-02f).
- **Model pass.** Sage Review gains a model pass, on by default when a model is usable
  (`review.model_pass: auto`). Over the pack's model tier it ranks, attacks and links the
  generators' candidates, proposes hypotheses and writes the report. `review.model_runs` (default 3)
  independent runs are combined by majority or refute (`review.consensus`). Every claim must cite
  evidence with grounded numbers, and the answer has no SQL field. With no model the review falls
  back to a deterministic ranking with templated text. Each run is recorded in the append-only
  `sage.review_model_run` with its output hash, tokens and cost. `llm.review_llm` can give the
  review its own model (review-03a).
- **Review plan.** Sage Review now ends as one plan. Generator candidates the review kept become
  steps. A model hypothesis becomes a step only through a typed template (target resolved in the
  evidence pack, SQL rendered by pg_sage, HypoPG prediction at or above the minimum improvement);
  otherwise it is recorded as advice with the reason. Every model-proposed step needs a person's own
  approval until the model is granted authority (G-M1) (review-03b).
- **Review triggers.** Sage Review now starts reviews from events: an upgrade readiness check, a
  regression open 72 h without a verified action, a daily size-growth anomaly (robust z >= 4), a
  live migration lint touching a table over `review.large_table_bytes`, an instance class, vCPU or
  memory change, enrollment (14 days of rollups at 80% coverage), and the calendar
  (`review.calendar_days`, default 30). Triggers inside `review.min_interval_hours` (default 24)
  join one run; `pre_upgrade` runs at once. Triggers are kept in `sage.review_trigger`, claimed by
  the leader with SKIP LOCKED, and survive restarts. New metrics:
  `pg_sage_review_trigger_fire_errors_total{kind}` and `pg_sage_review_runs_total{cause}`
  (review-04a).
- **Fleet review tiers.** Sage Review tiers fleet reviews in fleet and meta mode. Every member gets
  a deterministic rollup. A low-cost model triage (`review.triage_model`, default the general
  client) names outliers with cited rollup ids and falls back to a deterministic rule. One
  fleet-level review answers which databases are abandoned, which share an anti-pattern, and what
  retiring idle ones would save. Deep reviews run only for the top outliers, at most
  `review.fleet_deep_max` (30) a month, as `fleet_outlier` triggers, with tokens split by need and
  spend recorded per tier (review-04b).
- **Scorecard and model gate.** Sage Review now keeps a scorecard per database: plan steps by origin
  and generator, with how many were accepted, improved, unchanged, inconclusive, regressed, reverted
  or unverifiable. It is served at `GET /api/v1/databases/{db}/review/scorecard` and by MCP
  `review_scorecard`. A model gate runs on it. G-M1 files an admin-only inbox promotion when, over
  50 real reviews, the model's improved rate is within 10 points of the generators' and at most 2%
  of its decided steps were reverted. Only `POST .../review/gate/{id}/approve` grants it, and a
  failed window or a model change revokes it automatically. G-M2 stops `review.model_pass: auto`
  from calling a model that added no accepted step of its own. The `review.gate.*` thresholds are
  owner proposals (D11) (review-05a).
- **Scorecard on the Review and Value pages.** The Review page now shows the review scorecard and
  the model gate card ("Model gate: 32 of 50 reviews; model 78% improved vs generators 84%; reverts
  0%"), and the Value page shows a compact version. Counts are shown with their denominators, rates
  over fewer than 10 read "too few to judge", impossible counts read "invalid", and approving a
  pending G-M1 promotion from the Inbox now reaches the review gate route (review-05b).
- **Restore drills.** On the fleet leader, pg_sage now restores each database's latest backup to a
  disposable clone on `clone.provider` (receipted, guarded against connecting to the source, always
  destroyed). It checks the clone read-only, records the measured RTO and RPO in
  `sage.restore_drills`, opens the critical, paged finding `restore_drill_failed` when a drill
  fails, and lifts D7's L2 cap on production agent writes once a drill has passed. Branch,
  copy-on-write and Database Lab clones are drilled every 30 days by default. Full copies run only
  with `clone.allow_full_copy` and `full_copy` in the policy document's new drill block. The new
  `drill.max_concurrent_per_fleet` setting (default 2) caps drills running at once. Without a clone
  provider a database gets the info finding `recoverability_unknown` (drill-01a).
- **Restore drills on self-managed servers.** Set `drill.command.restore_argv` and `destroy_argv`
  (YAML only, restart-bound) to have drills restore your own pgBackRest, Barman or WAL-G backups
  through your wrapper. The wrapper runs without a shell, in `data_dir/restore-drills`, with a
  minimal environment plus `drill.command.env_allow`. It prints one JSON line naming the restored
  DSN, an id and the recovery point. The output is bounded and the stderr tail redacted, a timeout
  is killed and marked `create_uncertain`, and the clone reaper runs your destroy program for
  leftovers. It is a full copy, so it needs `clone.allow_full_copy` and `full_copy` in the policy's
  drill kinds, and it is refused next to a managed clone provider or database (drill-01c).
- **Recoverability views.** Restore drills are now visible. The Database page has a Recoverability
  panel (last drill, pass or fail, RPO and RTO, whether backups survive a delete, next full-copy
  cost, and what to grant when recoverability is unknown); the fleet overview reads "N of M
  databases passed a restore drill in the last 35 days"; `GET /api/v1/databases/{db}/restore-drills`
  and `GET /api/v1/fleet/recoverability` serve the same data; and the new read-only MCP tool
  `restore_drills` lets agents ask whether a database can be restored (drill-01d).
- **Upgrade brief.** Each database now has an upgrade brief (`GET
  /api/v1/databases/{db}/upgrade-brief`, or MCP `upgrade_readiness` with `brief: true`). It lists
  blockers, each extension's path, the plan for each logical slot and its CDC consumer, the indexes
  to rebuild for changed collations, the cost of waiting at your optional
  `cost.rates[].extended_support_per_vcpu_hour`, and an ordered checklist. It is stored in
  `sage.upgrade_briefs` only when it changes. The upgrade itself stays with the operator
  (drill-02a).
- **Upgrade rehearsal.** pg_sage can rehearse a major upgrade on a receipted clone: an Aurora or RDS
  clone upgraded in place (never the source), or the restore command's `upgrade_argv`; Neon is
  reported as unable. It compares the top 50 statements' plans and blocks per call before the
  upgrade, as cut over and after ANALYZE, and stores the list of changed plans on the upgrade brief
  (drill-02b).
- **After the upgrade.** After a major version upgrade recorded on the change feed, pg_sage proposes
  one plan per database and new version: ANALYZE of every table, busiest first by the stored upgrade
  brief's new `query_time_share` (skipped on PostgreSQL 18+, "statistics carried over"), then
  `REINDEX INDEX CONCURRENTLY` for each brief-listed index whose collation version still differs. A
  person approves it, and every step runs through the executor. Anything pg_sage's role cannot run
  is logged as an operator step (`vacuumdb --analyze-in-stages`, `REINDEX ...`) (drill-02c).
- **Policy sets.** Databases now follow named policy documents chosen by selectors (tags, discovery
  source, creator) and priority; default, prod and agent-created sets are seeded. Per-database
  policies are shown as exceptions with a diff, who set them and when they expire. New routes
  `/api/v1/policy-sets` (with members and YAML export and import) and `/api/v1/policy-exceptions`
  (fleetpol-01a).
- **Policy sets in the dashboard.** A new Settings > Policy sets tab lists each policy set with its
  selectors, members, requirements and YAML. You can propose a change to a set (and an admin can
  ratify it) through the existing proposal routes. The Exceptions list shows each database's
  field-by-field diff, who set it, why and when it expires; exceptions with no expiry are flagged
  and counted, and the Inbox lists each exception as a `policy_exception` item. The database page
  shows "Governed by: <set>" with the exception when one is in force (fleetpol-01c).
- **Observe-only members.** Meta-db fleets can hold up to 2,000 observe-only members next to the 50
  full ones. Observe-only members get no runtime or executor. A fleet sweeper grades them in rounds
  of 15 minutes, 4 at a time, each with one read-only connection. Policy sets choose the tier with
  `enrollment.tier`: agent-created defaults to `observe_only`, and an explicit `full` promotes a
  member. Actions on observe-only members are refused with 409 "observe-only member" (fleetpol-01d).
- **Sprawl census.** A monthly sprawl census on the fleet leader lists every database (full and
  observe-only members, and discovered databases never enrolled). Each row has owner, creator,
  creation time, last real activity (pg_sage's own statements excluded), size and storage cost at
  your `cost.rates` ("unpriced" without them). Each run records the share of agent-created databases
  still used 7 or more days after creation. Unadopted discovered members with no real activity for
  30 days get a "propose TTL 7 days" item of the new inbox kind `advice`; pg_sage deletes nothing
  (fleetpol-03).
- **Backfill pressure check.** A backfill or bulk write can ask `GET
  /api/v1/databases/{db}/pressure?table=&job=&batch=` (viewer session or read-scope API token) or
  the new MCP tool `check_pressure` between batches whether to proceed, slow down (with a suggested
  batch and sleep) or stop. The answer weighs replica lag, xid and multixact age, vacuum on the
  target, WAL rate, the lock queue and free storage against the new policy `pressure` block, which
  by default stops at half of each alert threshold. The check is read-only and cached for 2 s
  (guard-01a).
- **Pressure CLI and backfill jobs.** The new `pg_sage_sidecar pressure` subcommand exits 0
  (proceed), 10 (slow), 20 (stop) or 3 (no answer), and backfill jobs register through `POST
  /api/v1/databases/{db}/pressure-jobs` (schema step ddlPressureJobs). With `enforce`, a registered
  job that keeps writing through two consecutive stop verdicts gets a `cancel_backend` proposal
  (finding `pressure_job`) that runs only after a person approves it. Shell loop, psql loop and
  GitHub Actions examples are in docs/backfill-pressure.md (guard-01b).
- **Multixact member runway.** pg_sage now estimates multixact member space (members of up to 200
  sampled multixacts times live multixacts, with its standard error) and how fast it is used, and
  opens `forecast_multixact_member_runway` when the aggressive-vacuum threshold (2^31 members) is
  under 14 days away (critical under 3). If the role may not execute `pg_control_checkpoint()` or
  `pg_get_multixact_members()`, the reading is unknown and the log names the grant. Readings are
  stored in the new `sage.multixact_member_samples` (kept 90 days) (guard-02a).
- **Plan flips after ANALYZE.** The new `plan_flip_after_analyze` finding: when a query's plan hash
  changes after an ANALYZE (by autoanalyze or by hand, not by pg_sage) of a table the new plan
  reads, pg_sage records the table, both plan hashes and blocks per call before and after, and
  recommends one safe `ANALYZE` of that table. It does not propose another one within 24 hours. If
  that ANALYZE does not bring the earlier plan back, the finding is reopened without SQL and handed
  to the plan-stability review. auto_explain now captures a top query's plan again after any ANALYZE
  of a table it reads (guard-02c).

## v2.5.0 (2026-11-03) -- The Databricks release

pg_sage runs on Databricks. It connects to Lakebase with Databricks OAuth, runs as a Databricks
App (the same Go server and embedded React dashboard) behind Databricks sign-in, and calls its
models through Unity Gateway with the App's own OAuth identity, so there is no API key to
manage. Support is established feature by feature, on Lakebase PostgreSQL 18 with connection
probes on 16 and 17: the [Databricks-native guide](docs/databricks-native.md) and the
[Lakebase matrix](docs/lakebase.md) list what is verified, what the platform restricts and what
is still unproven. The App starts in observation mode.

### Upgrade notes

- **Finding `detail.queryid` is a string.** Finding responses, Cases evidence and investigation
  artifacts carry PostgreSQL query IDs as exact signed 64-bit decimal strings, including
  findings stored before the upgrade; as JSON numbers they rounded in JavaScript. A client
  that reads `queryid` as a number must accept a string.
- **Startup migrations** add the SRE statistics-maintenance ledger and provider-plan
  provenance columns, and, where the Lakebase telemetry reader is enabled, its store (schema
  version 5). Keep that data if you roll back: an older binary may refuse the newer telemetry
  schema, so restore a compatible binary instead of dropping tables or migration records.
- **Statistics actions refuse `IF NOT EXISTS`.** New recommendations use plain
  `CREATE STATISTICS`, so a rollback can never drop a statistics object that existed before.
- Building from source needs Go 1.25+. The Databricks Go SDK is a new dependency.

### Added

- **Lakebase.** Databricks unified authentication: a CLI profile locally, the App service
  principal in Apps. Database credentials are minted per physical connection, cached until two
  minutes before expiry and never written to configuration or the metadata database. OAuth
  pools verify the direct endpoint's hostname against endpoint metadata and require TLS;
  `SAGE_LAKEBASE_ENDPOINT`, or `SAGE_LAKEBASE_ENDPOINTS` for a multi-endpoint fleet, maps hosts
  to endpoints. Lakebase capability reporting and managed-setting guidance, and eight
  validated maintenance and configuration contracts with exact database-setting capture,
  derived rollback and fresh-session readback.
- **Databricks Apps.** A native package: a static Linux CGO executable that keeps libpg_query,
  a Python standard-library launcher, `app.yaml`, a bundle template and a SHA-256 manifest,
  with the platform port, H2C, durable secrets and supervised restarts.
  `SAGE_AUTH_MODE=databricks_app` signs users in from the Apps ingress identity with no second
  login; `SAGE_DATABRICKS_ADMIN_EMAIL` names the administrator and every other user starts as a
  viewer. The optional `SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS` limits the audience to exact
  email domains, checked before any account lookup. MCP, agent tokens and signed ingestion
  answer 501 in App mode: platform identity cannot carry their grants yet.
- **Unity Gateway.** `llm.auth: databricks` drives chat, JSON responses, tools and the
  advisors through the workspace gateway, and model discovery lists the Unity Catalog model
  services that support chat completions. Workspace credentials go only to the configured
  HTTPS origin; in Apps the SDK is pinned to OAuth M2M and API keys or foreign origins are
  refused.
- **Lakebase telemetry.** An opt-in, bounded reader for the ten `system.lakebase` streams
  through a SQL warehouse, with verified endpoint and database identities, durable cursors,
  policy revocation and payload expiry; metadata-only by default. Captured provider plans feed
  the plan cache and the optimizer without session auto_explain.
- **SRE on Lakebase.** Five historical sources behind a citable `read_history` tool, a
  database-scoped observation policy, administrator-only application SLI ingestion (replay
  and conflict checks; stale samples become unknown), and an operator-requested
  statistics-maintenance bridge: fresh evidence, exact object identity, human approval,
  structural verification and exact-object rollback.
- **Temp-spill findings** from consecutive query-statistics counters, with controlled
  `SET LOCAL work_mem` advice. pg_sage never picks or applies the value.
- `GET /api/v1/deployment/status` (administrators) returns a sanitized snapshot of the effective
  configuration; `GET /api/v1/lakebase-observability/status` reports the telemetry reader's
  state.

### Changed

- **Cases** lead with what was observed, the possible consequence, what is unknown and a
  supported next step; SQL and evidence move into expandable details. **Actions** is organized
  into proposed changes, needs your review, in progress and history, and related aliases fold
  into one review item. Database and case context carry across Cases, Actions and Ask Sage,
  and Explain prepares a draft question instead of sending it.
- Recovery SQL that was not validated against the current proposal is withheld, unnamed
  indexes get no guessed inverse, and Lakebase-inapplicable `ALTER SYSTEM` procedures are
  withheld.
- The dashboard has a dark Postgres observatory theme, responsive navigation and a short
  introduction for each tab, beside a breakroom of 50 captioned DBA memes on six bundled
  classic templates. They are served from the binary, shuffle without repeats, pause on request
  and start paused for reduced-motion users.
- A single running database is selected automatically, and Fleet lists running and configured
  databases as real inventory.

### Fixed

- Invalid-index findings stay open while a restarted analyzer rebuilds its two-observation
  baseline.
- Storage runway no longer invents growth or decline from cancellation on large, nearly
  constant byte totals.
- Recurring incidents link to the canonical prior incident, not a merged duplicate.
- Historical query-ID filters run before retained-row limits, so newer events cannot hide an
  older plan.
- A database clock that moves backward can no longer refund SRE active time.
- Standalone Settings loads for a running database with no managed registration ID.
- Collection time and database size on the dashboard update from completed collector samples.
- The LLM authentication mode validates, persists and resets through the settings store.
- Review-mode SRE starts without an investigator configuration.

### Development builds

The fourteen development builds behind this release, newest first. Their acceptance reports
are in [reviews/](reviews/).

#### 2.5.0-dev.14 -- Explain each tab beside the meme

- Add a short, tab-specific introduction beside the rotating meme on desktop,
  wrapping below it on smaller screens. Keep page guidance stable while memes
  rotate and describe each page's purpose and next steps in plain language.
- Match guidance to filtered Cases links, access-denied and missing pages,
  and Databricks-managed account/disabled agent and MCP workflows.

#### 2.5.0-dev.13 -- Top-of-page meme shuffle and standalone Settings

- Move the DBA breakroom above page content and provide 50 distinct captioned
  memes using the six locally bundled classic image templates.
- Shuffle without repeating a meme until the full deck is used, including
  navigation between pages. Rotate every 60 seconds with manual next/pause
  controls; skip background tabs and focused controls, and start paused for
  reduced-motion preferences. Reloading the App starts a fresh deck.
- Load standalone instance Settings when a connected runtime database has no
  managed registration ID. Confirm standalone mode before using global
  configuration and explicitly label the instance-wide scope.

#### 2.5.0-dev.12 -- Keep telemetry from blocking App access

- Return retryable HTTP 503 for unavailable Databricks account storage, while
  retaining HTTP 403 for organization and identity refusals. Resolve durable
  roles on every request and expose no raw connection or SQL errors.
- Add telemetry inbox indexes and bound background delivery/expiry database
  work. Avoid rewriting retained events whose expiry has not changed.
- Give Databricks Apps a five-connection default for its shared authentication,
  control and monitoring pool, preserving explicit operator configuration.
- Isolate the pre-authentication App rate limit by validated platform identity
  and workspace; employees behind a shared proxy do not exhaust one IP bucket.
  Preserve the existing limit and ordinary session/IP behavior.
- End event streams cleanly before the server write deadline so EventSource
  reconnects without HTTP/2 protocol errors; retain bounded server timeouts.
- Retain failed live acceptance attempts and distinguish the corrected App's
  checks from earlier full-suite and Lakebase compatibility evidence.

#### 2.5.0-dev.11 -- Company access and classic DBA memes

- Add an optional exact email-domain audience policy to Databricks Apps.
  Validate its administrator and domains at startup, and reject disallowed
  identities before account lookup or provisioning, including existing users.
  Keep Databricks-only sign-in, durable pg_sage roles and viewer defaults.
- Document organization sharing separately from the application's audience
  policy and FEVM account/SSO provisioning. Account-wide sharing alone does
  not establish employee-only access.
- Replace the original illustrated jokes with locally bundled recognizable
  meme templates, accessible HTML captions and page-specific Postgres jokes.
  Keep memes secondary to operational content and include source provenance.

#### 2.5.0-dev.10 -- One proposal per reviewed change

- Documentation follow-up (2026-11-01): document the Databricks-native
  implementation, linked from the README and
  documentation index, with portable local/App deployment instructions, OAuth
  identity boundaries, telemetry/SRE behavior, operations and feature-level
  compatibility evidence. Keep the dev.5 full-suite run distinct from later
  changed-package and deployed-App acceptance.
- Publish the dated compatibility, controlled finding and UX acceptance reports;
  remove private research/environment details and exclude local test artifacts
  from Git and Docker contexts. Parameterize the bounded Lakebase fixture harness
  so it has no default workspace, endpoint or personal database principal.
- Consolidate explicitly related index/statistics/maintenance action aliases
  into one review item, while keeping the current case proposal separate from
  the stored revision's risk and recovery claims.
- Withhold stored recovery SQL when it has not been validated against the
  current proposal; an old revision cannot override unknown recovery.
- Use an explicit policy provider when capability metadata reports an unknown
  provider, preserving managed-setting applicability checks.

#### 2.5.0-dev.9 -- Cases, decisions and trustworthy recovery

- Lead Cases with plain-language observations, possible consequences, unknown
  impact/urgency and a supported next step. Keep exact SQL and evidence in
  expandable details; explain suppression separately from fixing a condition.
- Keep database and case context across Cases, Actions and Ask Sage. Prepare
  explanation-only questions for user review without submitting them automatically.
- Organize Actions into proposed changes, needs your review, in progress and
  history. Distinguish operational risk, execution restrictions, accepted
  approvals, completed SQL and verified outcomes.
- Show reported execution mode and collection context before page content;
  keep page-specific DBA memes as compact secondary content.
- Withhold guessed recovery/catalog verification for unnamed indexes and other
  proposals without captured recovery. Unknown size and urgency remain unknown;
  provider-managed system-setting guidance is reviewed for applicability.

#### 2.5.0-dev.8 -- Preserve invalid-index evidence on restart

- Keep existing invalid-index findings open while a restarted analyzer rebuilds
  its two-observation baseline, an index build is active, or build evidence is
  unavailable. Resolve only after a complete supported observation clears it.
- Retain two-observation detection, exact query identities, the refreshed UI,
  Databricks-only authentication and the bounded Lakebase scenario proofs.

#### 2.5.0-dev.7 -- Exact query identities

- Preserve signed 64-bit PostgreSQL query IDs as decimal strings in finding
  responses, Cases evidence and generated investigation artifacts, including
  previously stored findings whose JSON numbers would round in JavaScript.
- Verify a ninth controlled Lakebase finding family at the production slow-query
  threshold, with isolated native recovery and a real deployed App case.

#### 2.5.0-dev.6 -- Postgres observatory and Lakebase scenarios

- Select the single running database automatically, keep the database picker
  available, and distinguish Ask Sage inventory loading, failure, and empty states.
- Show running/configured databases as real Fleet inventory; preserve visibility
  when management reads fail and remove contradictory empty-fleet guidance.
- Refresh the dashboard with a dark Postgres observatory theme, responsive
  navigation, accessible controls, and original page-specific DBA memes.
- Add hint-free temporary-spill findings from consecutive query-statistics
  counters, with controlled work_mem advice and no automatic settings changes.
- Exercise exact-owned Lakebase workloads and record which findings are detected,
  resolved, provider-restricted, or remain unproven.

#### 2.5.0-dev.5 -- Maintenance retention and full-suite closure

- Retain active or unresolved SRE statistics maintenance through evidence and
  timeline aging, and age finalized maintenance from its latest change. Keep its
  exact rollback and audit records until the parent investigation expires.
- Recheck maintenance holds after locking purge candidates under READ COMMITTED;
  filter holds before batch limits so held records cannot starve eligible cleanup.
- Preserve the executor-commit/proposal-save crash window through exact finding,
  identity and SQL links; index retention lookups for large proposal payloads.
- Refuse typed maintenance with a separate coordination database; keep ordinary
  SRE observations and existing actions available. Distributed retention holds
  remain unsupported.
- Isolate YAML normalization tests from the integration fixture DSN and include
  the maintenance child table in migration fixture teardown without bypassing
  foreign-key integrity. Preserve the complete failed dev.4 regression inventory.
- Upgrade the earlier nine-state maintenance constraint atomically to include
  `uncertain`; validate existing rows, reject unsupported states, and preserve
  the current validated check on repeated startup.
- Prevent backward database wall-clock movement from refunding already charged
  SRE active time or violating its nonnegative budget constraint. Preserve the
  existing cap, lease fencing and expiry behavior.

#### 2.5.0-dev.4 -- Lakebase compatibility gap closure

- Verify controlled provider plan, PostgreSQL log and DDL capture, normalization,
  and all ten telemetry streams. Use short unique fixture application names to
  avoid an observed 31-character provider truncation that caused false negatives.
- Add five scoped historical SRE sources and a durable, citable `read_history`
  tool. Recheck physical database identity, durable policy and retention on reads;
  retain structured observations without raw SQL, plans, log bodies or actors.
  Historical observations remain incomplete and cannot establish live admission
  or a complete database error-rate SLO.
- Add Databricks-authenticated, administrator-only application SLI ingestion with
  explicit database/objective binding, actor-bound series, durable database
  namespaces, replay/conflict checks and stale-to-unknown evaluation. Legacy
  signed ingestion remains unavailable in Apps; standalone behavior is retained.
- Add an operator-requested statistics maintenance bridge with fresh retained
  evidence, physical object identity checks, durable human approval, existing
  execution policy gates, structural verification and exact-object rollback.
  Rollback cannot restore prior ANALYZE statistics; no performance or incident
  recovery credit is inferred. The App remains in observation mode.
- Distinguish pg_sage's default database-only observation policy from native
  Lakebase query support. Previously policy-denied cluster probes execute on the
  isolated authorized test branch; empty results do not prove incident diagnosis.
- Preserve explicit provider restrictions on pg_hint_plan activation and HypoPG,
  and document real-index trials as a separate supported fallback.
- Fix review-mode SRE initialization when no investigator configuration exists.
- Fix historical query-ID filtering before retained-row limits so newer monitoring
  events cannot hide a requested older plan or ASH event. Preserve signed/unsigned
  PostgreSQL query IDs and database, policy and retention fences.
- Give a historical read a bounded five-second operation budget while keeping
  backend statements at 500 ms and rollback on an independent bounded context.

#### 2.5.0-dev.3 -- Scoped Lakebase history and native SRE

- Add an opt-in, bounded reader for all ten Lakebase system-table streams,
  verified endpoint/database identities, durable replay/cursors/acknowledgments,
  policy revocation and event-time payload expiration. Metadata-only by default.
- Add authenticated source status, admin-only normalized historical evidence,
  and a sanitized effective deployment configuration snapshot.
- Feed valid captured provider plans into the plan cache and optimizer without
  requiring session auto_explain. Preserve capture flags, occurrence/receipt
  times, source and compute identity. Exclude delayed plans from current counters
  and refuse regression comparisons without known compatible execution context.
- Enforce a database-scoped SRE observation policy on Lakebase, including model,
  background and resumed paths. Native Gateway investigations, controlled lock
  and sequence diagnoses, and a signed diagnostic runbook have passed.
- Pin Apps authentication to platform service-principal OAuth, reject AI API-key
  or foreign-origin overrides, and explicitly refuse unsupported optional caller
  protocols after Databricks authentication.
- Hide unavailable MCP-token and agent-governance flows in the App dashboard;
  direct links explain their status without calling unsupported APIs. Update
  Lakebase capability guidance for configured historical telemetry.
- Selected-resource telemetry delivery remains unverified: all ten native schema
  reads succeed, but controlled plan/log/DDL delivery has returned zero rows.
  Historical telemetry does not supply live host admission or a complete SLO.

#### 2.5.0-dev.2 -- Databricks Apps sign-in

- Databricks Apps uses its authenticated ingress identity for browser/API access,
  without a second pg_sage login, local session or bootstrap password.
- Explicitly bound administrator identity preserves the existing account ID;
  other Databricks users start as viewers and retain per-request role checks.
- Local sign-in, account linking, break-glass login and local user creation are
  disabled in App mode; standalone installations retain session authentication.
- The App UI identifies Databricks-managed sign-in and handles missing identity
  without displaying a separate password form.
- Lakebase capability guidance distinguishes its available system-table telemetry
  from pg_sage's missing telemetry connector and unverified SRE workflows.

#### 2.5.0-dev.1 -- Lakebase and Databricks Apps

Development release for native Lakebase OAuth connections, Unity AI Gateway,
and deployment of the existing Go/React application on Databricks Apps.
Live validation and the exact supported feature boundaries are recorded in
`docs/lakebase.md`, `docs/databricks-apps.md`, and the compatibility reviews.

##### Added

- Databricks unified authentication with refreshing Lakebase connection
  credentials, direct endpoint verification, and multi-endpoint fleet mappings.
- Native Unity Gateway chat, structured responses, tools, and model discovery.
  Workspace credentials remain bound to the configured HTTPS origin.
- Databricks Apps native Linux packaging, platform port/H2C handling, durable
  secrets, restart handling, and first-admin bootstrap support.
- Lakebase-specific managed-setting guidance and capability reporting. Unsupported
  extensions, server administration, and authentication mechanisms remain explicit.
- Eight individually validated Lakebase maintenance/configuration contracts,
  including exact database-setting capture, derived rollback, and fresh-session
  readback. The development App uses observation trust by default.

##### Fixed

- Storage runway regression no longer invents growth/decline from cancellation
  on large, nearly constant byte totals.
- Recurring incidents link to the canonical prior incident instead of a merged
  duplicate tombstone.
- Integration and browser fixtures follow current UI/schema behavior and use
  the PostgreSQL clock for database-stamped timing assertions.
- Statistics actions refuse `IF NOT EXISTS`, preventing a rollback from dropping
  a pre-existing statistics object. New recommendations use plain `CREATE`.
- Lakebase fleet readiness reports the tested provider capabilities and preserves
  managed-platform restrictions after runtime catalog probes.
- Dashboard collection time and database size update from completed collector
  samples without waiting for the slower analyzer status refresh.
- LLM authentication mode validates, persists and resets through the settings
  store, including Databricks unified authentication.

## v2.4.0 (2026-10-23) -- pg_sage governs the agents that touch your database

Agent governance, first two steps. pg_sage now reports how exposed each database is to AI
agents and untrusted clients (Agent posture), gives every agent its own identity, roles and
time-boxed grants, puts every agent request through one gate, and can stop every agent at
once. AgentDB provisioning is removed. The release also adds what an enterprise install asks
for first: TLS on the API, secrets from files and encrypted at rest, validated SSO, and a
tamper-evident audit.

### Breaking

- **`POST /api/v1/mcp/tokens` no longer issues agent tokens.** It answers 400; mint them for
  an agent with `POST /api/v1/agents/{id}/tokens`. Operator tokens are unchanged. Existing
  agent tokens migrate at startup to unsponsored principals with the read-only `legacy`
  profile.
- **MCP requests from agents never auto-execute.** An agent's proposal queues for a person
  (L2) at any trust level. Narrowing actions (freeze, kill, revoke, grant expiry) still run,
  including under `emergency_stop`.
- **The kill switch needs `pg_signal_backend` and `pg_monitor` on pg_sage's role**: the first
  to end agent sessions, the second to name standbys. Without them a kill still disables the
  agent roles, and its report names the missing grant.
- **AgentDB provisioning is removed.** Instance runners, project creation, Terraform,
  blueprints, deploy requests, `agt_` tokens and 70 routes (now 404) are gone. An `agentdb:`
  config section is ignored with a warning; `live_provisioning_enabled: true` refuses startup,
  so nobody is left believing billed databases are still managed. Persisted `agentdb.*`
  settings are deleted, with audit rows. A decommission inventory lists every resource a live
  call touched, whatever its status (RDS final snapshots, `local_postgres` artifacts, `sage`
  schemas in agent databases), each with a per-provider delete template. pg_sage deletes
  nothing itself.

### Security

- **The AU-10 local QA passwords are burned.** They are removed from every tracked file, the
  archive included, and a hash-only test keeps them out, but they remain in git history.
  Treat them as public and rotate them anywhere they were reused.
- **SSO validates the `id_token`.** Login used to trust userinfo alone. OIDC login now uses
  PKCE and a nonce and verifies the `id_token`'s signature (JWKS), issuer, audience and
  expiry; every refusal is audited with a reason code.

### Added

- **Agent posture.** 16 read-only detectors, AP-01..AP-16, report what lets an agent do more
  than its job: an agent role that is superuser, holds `BYPASSRLS` or owns objects; a table an
  exposed role reads with row-level security off; a definer function without a pinned
  `search_path`; remote access or untrusted languages an exposed role can reach; no
  point-in-time recovery; and more. They run in the first look (an "Agent posture" section)
  and in the analyzer, daily at `agents.posture.daily_at` or when the catalog they read
  changes. Every finding carries only a manual fix script at L1, never recommended SQL, so
  pg_sage never runs a posture fix. On by default, PostgreSQL 14-18, needs only `pg_monitor`;
  the slowest posture statement takes 6-8 ms at 36,000 relations.
- **Agents have identities.** Each agent is a principal with an accountable sponsor and its
  own database roles, `sage_agent_<id>` and `sage_agentb_<id>` (PostgreSQL 16+): never
  superuser, owning nothing, changed only through operator-approved executor contracts. The
  `PUBLIC` baseline is recorded at preflight and effective privileges are checked against it.
  The Agents page (`#/agents`) lists them; the MCP tool `agent_whoami` tells an agent its own
  state.
- **pg_sage decides an agent's environment.** The environment an agent claims is ignored and
  anything unverified is prod. A re-pointed DSN, or a copy of prod labelled dev, is prod and
  raises a critical finding.
- **Column classification:** `clean`, `pii`, `secret`, `untrusted_input`. Prod and stage
  grants list only columns an operator confirmed clean.
- **One gate for every agent request** (steps D1-D10). The principal is re-read live, so a
  freeze, retire or taint stops a live token on its next request. A census fails on any
  unclassified MCP tool and on any agent-role SQL path outside the closed list. A policy
  change that widens an agent's access needs two people other than its sponsor
  (`agents.single_operator_mode`: one person, with a reason and a review queue).
- **Grants are column-listed, time-boxed and operator-approved** (MCP
  `agent_request_capability`). Expiry runs on the leader, fenced by its lease, and still runs
  under `emergency_stop`. A revoke runs `GRANTED BY` the recorded grantor; residue another
  grantor left gives `revoke_incomplete` with the exact fix.
- **`agent_query`: agents read as themselves.** One `SELECT` in the agent's own broker
  session and role (never pg_sage's), read-only, on a search path limited to the schemas
  granted to that role, with timeouts and row and byte limits. Classified columns are
  masked, every call writes an audit row, and the statement is attributed through
  pg_stat_statements. It holds all 23 AgentSafetyBench read-only cases, against 17 for a
  `READ ONLY` transaction and 21 for a privilege-based role.
- **Kill switch.** `POST /api/v1/agents/kill` (every agent, one agent or one database) needs
  no approval and runs under the emergency stop at every trust level: tokens revoked, pending
  approvals cancelled, roles set `NOLOGIN CONNECTION LIMIT 0`, sessions ended on the primary
  and each `databases[].replicas` entry, then verified for up to
  `agents.kill_verify_timeout_seconds`. A standby you did not list is named in the report with
  the timeout that bounds its open sessions. Unfreezing after a kill needs two admins other
  than the sponsor, restores the roles' previous attributes and rotates the broker password.
- **Upkeep on the leader.** Retired agent roles are dropped after
  `agents.roles.retire_grace_days` (7), broker passwords rotate every
  `agents.broker.rotation_days` (7), connected sessions are checked every minute, and a daily
  drift reconciler corrects narrowing drift and reports the rest. Each run cites the approval
  it runs under.
- **AgentSafetyBench v0.** A read-only corpus (RO-01..RO-16, plus the `agent_query` cases)
  run against each read-only design, seven posture scenarios scored by the real detectors,
  and an incident-to-control mapping. CI runs it on PostgreSQL 14-18; the PG17 report is
  signed keyless (Sigstore) and attached to the release.
- **Tamper-evident audit.** Action, sign-in, config and pgaudit records are hash-chained and
  verified daily (`audit.verify_interval_hours`); a broken chain raises a critical finding.
- **SIEM export.** Every audit event as OCSF over HTTP, syslog (TCP, UDP or TLS) or OTLP, at
  least once, without slowing pg_sage (`audit.siem.sinks`).
- **MCP accepts OAuth 2.1 access tokens** from configured issuers (`mcp.oauth`), each bound to
  one database by its audience.
- **Evidence packs.** `GET /api/v1/audit/evidence` downloads a signed or hashed bundle for an
  action or a time window. pgaudit records are correlated with pg_sage's actions
  (`audit.pgaudit.correlate`).
- **The API serves TLS itself.** Set `SAGE_TLS_CERT` and `SAGE_TLS_KEY` (PEM) and it serves
  HTTPS only, TLS 1.2+, with `Secure` cookies; rotated certificates load without a restart.
  Setting only one is a startup error, so a typo can't downgrade to HTTP.
- **Every secret can come from a file.** `NAME_FILE` works for each secret variable and for
  every `${NAME}` the YAML references, for Vault Agent, the CSI driver and Docker or
  Kubernetes secrets. Setting both is a startup error; a file other users can read warns.
- **Secrets set through the API are encrypted at rest** with AES-256-GCM under a key derived
  from `encryption_key`, with key ids and rotation (`encryption_key_previous`). With a key
  configured, startup seals existing plaintext rows.
- **IdP groups map to roles** (`oauth.role_mapping`, `oauth.groups_claim`). With a mapping,
  unmapped users are denied by default (`oauth.unmapped_users`).
- **Login audit.** `sage.auth_audit` records sign-ins, failures with reason codes, break-glass
  use, and user and role changes, kept 365 days (`retention.auth_audit_days`).
- **Break-glass admin** (`oauth.break_glass`, `SAGE_BREAK_GLASS_PASSWORD_HASH[_FILE]`) for when
  the IdP is down: a one-hour, rate-limited session, and every use alerts every enabled
  notification channel.
- **`/ready`.** Unauthenticated readiness: 200 when the config is loaded, the control database
  answers and its `sage` schema is migrated, 503 otherwise. `/health` stays liveness only.
- **"Grant more" asks for CREATE on the schemas that hold your tables.** PostgreSQL needs
  CREATE on a table's schema for `CREATE INDEX` and `CREATE STATISTICS`, even when pg_sage owns
  the table. "Grant more" now lists it for the advisory and autonomous levels, checks it live,
  and gives one ready-to-run `GRANT CREATE ON SCHEMA ...` naming only the schemas missing it.
  The startup check looks at the same schemas (it checked only `public`) and prints the same
  SQL, with the role name quoted.

### Fixed

- **The Fleet page shows the config-file database.** On a standalone install it listed only
  UI-registered databases, so the database from the YAML `postgres:` section never appeared.
  It now shows in a read-only "From the config file" section.
- **The first look's sequence read can't time out on stale statistics.** On a freshly
  restored or migrated database it took 12 s and hit the 5 s statement timeout, degrading the
  sequence runway check; it now reads by key (0.2 s at 5,000 sequences).
- **The first look reads indexes about twice as fast** on large catalogs (15,000 indexes) and
  sends 40% less data: constraint-backed indexes are read as one hashed set instead of a
  per-index `EXISTS`, and index scan counts come from `pg_stat_get_numscans` instead of the
  statistics view.
- **A first-look check that timed out is retried instead of failing the next one.** When a
  step could not reopen its read-only transaction (for example while DDL held the catalog), the
  following step ran in the aborted transaction and was reported as "current transaction is
  aborted", which is never retried. A step whose transaction cannot be opened now leaves nothing
  behind: it is retried once, and the next step opens its own transaction.
- **MCP stdio clients hear about every fleet change.** A database added or removed right after a
  client subscribed to tool-list changes could be missed when the watcher started late; the
  watcher now compares against the tool list as it was when the session started.
- **Merging legacy duplicate incidents takes time linear in the backlog.** At startup pg_sage
  merges the open incidents of each identity that were written before identity keys existed.
  With table statistics older than that backlog, the merge re-ran its grouping once per open
  row: 5,000 duplicates of one incident took 25 million index reads. It now reads each
  identity's open rows once.
- **The cluster's size is sampled every 10 minutes** (`sre.runways.size_interval_seconds`,
  default 600) instead of every minute: summing every database's size took 100-300 ms on large
  catalogs, and the disk runway forecasts over hours.
- **The schema guard's structural scan reads only the tables whose catalog rows changed,**
  with a full pass daily. Temporary tables, and tables an extension owns, are no longer
  reported.
- **Less sidecar CPU per cycle.** Snapshot catalog documents are validated once and sliced
  instead of decoded (5,000 changed tables: 62 ms to 22 ms), and an element identical to the
  previous cycle's reuses its parse.

### Performance gate

- Timing budgets scale with the runner: the gate times a fixed CPU and a fixed SQL workload
  first and scales by the runner's speed over a reference (the median of 12 CI runs), both
  ways, clamped to x0.75-x1.25, so a fast runner can't hide a regression.
- Exemptions are narrow and capped: each names one tagged statement, a reason and its own
  ceiling just above CI's slowest mean (`sre:cluster_database_size` 300 ms,
  `structural:columns` 150 ms). The catalog classifier ignores comments and literals and
  never counts a statement over a `sage` table as a catalog read.
- The fixture is checkpointed before the measured phases, CI also runs the first-look gate
  (`TestPerfGateFirstLook`), and `perfgate.yml` runs the gate alone on any branch on demand.
- CI pulls its PostgreSQL images from a GHCR mirror instead of Docker Hub, whose anonymous
  pull limit failed every job on 2026-10-09. A new `agent-servers` job runs the kill switch on
  a real PG17 primary, replica and standby, with no skips allowed.

## v2.3.1 (2026-10-07) -- pg_sage stays light on large databases

The nightly performance gate (5,000 tables, 15,000 indexes, 5,000 sequences, 150,000 rows of
history per table) had failed every night since it started. Every offender it reported is
addressed: locally the large gate now passes with none, and the sidecar's CPU per collector
cycle fell from about 480 ms to 290-440 ms.

### Fixed

- **Less sidecar CPU per cycle.** Encoding a snapshot delta no longer runs a regular expression
  and arbitrary-precision arithmetic on every field of every element (about 4x faster, 7x fewer
  allocations; the stored bytes are identical), the analyzer remembers parsed index definitions
  instead of re-parsing every index each cycle, and snapshot documents are parsed once instead
  of twice.
- **The tuner reads only the catalog it needs.** It used to read every table and index on
  every cycle; it now reads the tables named in the plans it checks.
- **No more full scans of `sage.findings`.** The SRE change feed's read of migration-detector
  findings scanned the whole table on every poll; a small partial index
  (`idx_findings_migration_feed`, created at startup) now serves it.
- **The first look runs with JIT off.** Its catalog reads are planned at costs that made
  PostgreSQL JIT-compile them: up to a second of compilation on a cold start for a query that
  runs in tens of milliseconds.
- **Withheld decisions are purged incrementally.** Decisions kept past the decisions window
  (they back an action, a dry run or an open deadline) were re-read by every purge pass; a pass
  now reads only rows created since the last one, with a full sweep at startup and once a day,
  so such a row is removed at most a day later than before.
- **Cheaper self-configuration and schema checks.** Self-configuration times a sample of 250
  sequence reads instead of reading every sequence, and the schema guard's structural scan
  aggregates per table instead of windowing every column.

### Performance gate

- `sage.shadow_decision` is exempt from the HOT-update gate, with the reason in the report: its
  only update moves a decision from pending to scored, which changes the columns its partial
  indexes are built on, once per row.
- CI runs the gate on a clean PostgreSQL server of its own, so the size of every database the
  earlier test suites left behind is no longer charged to pg_sage.

## v2.3.0 (2026-10-07) -- Fleet learning, index replacement, history outside the database

### What's new

- **Fleet learning: databases that look alike learn from each other.** In fleet mode pg_sage
  fingerprints every database's shape (column types, index shapes and query shapes with every
  literal and name removed; no data, no names unless you set
  `fleet_learning.include_names: true`) and finds its look-alikes within the same fleet and
  tenant. When look-alikes have verified outcomes for the same kind of change on the same table
  shape, the proposal and its approval card show them, labelled "from look-alike databases".
  This is evidence only: it never raises confidence, trust or autonomy, and when look-alikes
  mostly regressed the proposal waits for your approval. `GET /api/v1/fleet/lookalikes`.
- **Fleet findings: the same problem on 30 databases is one finding.** A problem open on at
  least `fleet_learning.fleet_finding_min_databases` databases (default 3) is listed once with
  the databases it affects: on the Fleet page ("Recurring across the fleet"), at
  `GET /api/v1/fleet/findings` and through the read-only MCP tool `fleet_findings` (a token
  scoped to some databases sees only those).
- **The fleet LLM budget follows need.** `llm.fleet_token_budget_daily` is now split by
  measured need (open and critical findings, unresolved incidents), each database between
  `fleet_learning.budget_floor_pct` (50) and `fleet_learning.budget_ceiling_pct` (300) of
  the even share, instead of evenly. The daily cap is never exceeded;
  `fleet_learning.budget_split: even` restores the even split.
- **Several sidecars, one leader.** Sidecars sharing a control database elect one leader
  through a lease in that database (`fleet_learning.leader_lease_seconds`, default 30; 0
  turns election off). Only the leader runs fleet-wide jobs (fleet learning, approval-card
  follow-ups, the agent-database reconciler); a stalled leader steps down before its lease
  ends and another sidecar takes over. `GET /api/v1/fleet/leader` shows who leads.
- **Other agents now see what the model investigator concluded, and on what authority.**
  Specialist contract revision 1.1.0 (still `pg_sage.specialist.v1`; every addition is
  optional and a v1 result is byte for byte unchanged): results carry an `investigator`
  section with the investigator's verdict (agree, conclude, contest, unmodeled,
  inconclusive, no answer), whether its root was adopted under the family's earned
  model-lift authority or stayed advisory and why, its cited claims with the numbers their
  evidence holds, the reads it asked for that returned nothing usable, and a link to the
  redacted transcript, now served by the contract (`GET .../investigations/{id}/transcript`,
  MCP `specialist_investigation_transcript`). The PagerDuty note is unchanged.
- **Callers can scope an investigation to one statement.** `query_id` (a
  pg_stat_statements queryid, read exactly to 64 bits) and/or `query_hash` (SHA-256 of
  the normalized text, resolved through pg_stat_statements with a parameter) are validated
  strictly; a plan-regression investigation is then about that statement (its probe reads
  only it, so a larger regression can no longer push it out of the probe's row cap), other
  families name the evidence that mentions it, and the result echoes the scope to its
  caller. pg_sage's own plan-regression investigations also probe only their statement now.

- **pg_sage replaces a subsumed index in one approved action.** Since 2.1 the tuning agent
  refused an index that would make an existing one redundant, because a create and a drop
  decided separately could each pass verification while together they left the table
  with redundant indexes. Now such a candidate becomes one replacement: `CREATE INDEX
  CONCURRENTLY` the wider index, check that it is valid, then `DROP INDEX CONCURRENTLY` the
  old one, holding the table's change lease across both steps. Replacements always go to
  an operator (the new `index_replace` trust class is capped at L2 and never above the
  levels of `index_create` and `index_drop`); the approval card shows both statements, the
  undo (re-create the old index, drop the new one) and the locks each step takes. pg_sage
  never replaces an index that backs a constraint or enforces uniqueness, one that changed
  since it was proposed (OID or definition), or one whose foreign key the new index would
  not support, and a table owned by the application's migrations gets the two-statement
  migration instead of DDL. The steps are recorded in `sage.index_replace`, so a failed
  build drops nothing, a failed drop keeps both indexes and is reported as a partial
  result, and a crash between the steps resumes the drop or restores the old state on the
  next cycle. Verification judges the targeted queries (no gain, a regression or no
  verdict rolls the replacement back) and the queries that used the old index; after a
  kept verdict the old index is a soft drop, re-created if those queries regress within
  `verify.drop_window_hours`. Operators can roll a replacement back from the Actions page.
  An index that would make an in-flight index (queued or proposed) redundant is still
  refused. See `docs/configuration.md#tuning-agent`.

- **pg_sage's telemetry history can live outside the database it watches.** In meta-db
  mode, `history.store: meta` keeps the collector's snapshots and the query store (most of
  pg_sage's storage) in the metadata database instead of each monitored database. Each row
  carries its database's id, and every reader and writer runs through one history store, so
  a database never reads another's history; reads that used to join history with the
  monitored database's catalog are split in two. `pg_sage history migrate` copies a
  database's history over (resumable, read-only on the source; `--cleanup` removes the copy's
  source once complete; `--to monitored` moves it back), and a database whose history would
  be split between the two places is refused at startup with the command to run. Retention
  and the snapshot cap of the shared tables run once per process, `sage_footprint` says where
  history lives, and `self_budget.storage_mb` counts each database's share of the store. The
  default (`monitored`) changes nothing for existing installs. Findings, the action log and
  verification stay in each monitored database.

### Fixed

- **Model-concluded roots report their source.** A root the model investigator concluded
  for an inconclusive graph and that was adopted under earned authority was reported as
  `source: graph`, `authority: deterministic`; it is now `source: model`,
  `authority: model_earned`, as documented.

### Upgrading

- **New tables:** `sage.fleet_fingerprint`, `sage.fleet_outcome_digest`,
  `sage.fleet_leader_lease` and `sage.index_replace`, created at startup by an idempotent
  migration.

## v2.2.1 (2026-10-07) -- First look and quickstart fixes

### Fixed

- **The first look no longer gives up on large catalogs because of pg_sage's own default
  timeout.** It was held to `safety.query_timeout_ms` (500 ms by default), so on a database
  with about 1,800 relations the index checks (invalid, duplicate, never-scanned indexes,
  unindexed foreign keys) sat right at the limit and came back "degraded" on some starts and
  complete on others. The first look now has its own 5 s budget per statement. A lower limit
  still applies when you set one: `safety.query_timeout_ms` in the YAML file or through the
  API, or a `statement_timeout` on pg_sage's role, its database, the server or its
  connection options. A degraded check now says which setting cut it off. Checks that
  degrade because of a timeout, a lock conflict or a dropped connection are retried once
  30 s later, and the retry is recorded on the same first-look report (marked `retried`,
  with the first failure kept in the note). A first look can no longer hang on a catalog
  lock (for example during long DDL): each step, including opening its transaction, is also
  cancelled from pg_sage's side after twice its timeout plus a second.

- **Sequence runway now sees sequences used through a column default.** A sequence that a
  column uses with `DEFAULT nextval('...')`, without `OWNED BY`, is now capped by that
  column's type: a bigint sequence close to the integer limit that feeds an `int` column is
  reported as critical instead of "ok". When several columns use one sequence, the
  narrowest column type decides, and the sequence is still reported once. A sequence that is
  nearly exhausted now shows one decimal ("99.7% used") instead of a rounded "100% used".
- **A read-only install starts without grant warnings.** At trust level `observation`
  pg_sage no longer warns that the role lacks `CREATE` on schema public or membership in
  `pg_signal_backend`: it logs one line pointing at "Grant more", where the grants for
  higher levels are listed. At `advisory` and `autonomous` a missing grant is still a
  warning with the SQL that fixes it.
- **The new-install startup line matches the trust level.** It no longer describes
  observation ("nothing changes outside the sage schema") when the configured level is
  advisory or autonomous.
- **No more `sage_footprint` finding on small databases.** pg_sage's own data is no longer
  reported while the sage schema is under 256 MB (the same minimum as the snapshot size
  cap), so a fresh install on a small database no longer opens with "Sage data uses 20% of
  database ...". Larger sage schemas are checked against `retention.sage_size_warning_pct`
  as before.

## v2.2.0 (2026-10-05) -- Ask Sage, self-configuration, managed clouds

### What's new

- **Ask Sage: ask your database's DBA, and get only what it can prove.** A chat panel per
  database in the UI, a REST endpoint and the MCP tool `ask_sage` answer questions from
  evidence pg_sage reads with read-only tools: findings, fixes waiting for approval, executed
  actions with their verification outcomes, facts, incidents, investigations, the trust
  ledger, the table catalog, the heaviest queries, configuration and its concepts. Every
  statement cites its evidence and its numbers; uncited or ungrounded statements are dropped
  and the answer says what it could not verify ("not observed" is a valid answer). Ask Sage
  runs on the investigator's tool loop and can never execute, approve or confirm anything.
  Operators (and MCP tokens with the propose scope) can have it open an investigation,
  queue one of pg_sage's own findings for approval (with its rollback, predicted effect and
  the policy gate's verdict; a person approves on the Actions page), or propose a fact that
  stays proposed until a person confirms it. Queued items record that Ask Sage proposed them
  and for whom (`proposed_via`, `proposed_by`), shown on the approval card and kept in the
  decision recorded when the approval runs. Database text, finding
  titles and query text reach the model as fenced data. It has its own daily LLM budget per
  database and per user (`ask.*`), kept across restarts, and conversations are kept for
  `ask.retention_days`.

- **pg_sage keeps to a budget it declares for itself, and the Trust page stays fast under
  load.** A new `self_budget` section declares what pg_sage may cost: the sidecar's CPU per
  collector cycle (default 600 ms, 1% of a core), its own statements' database time and
  shared blocks per hour, and the size of the `sage` schema (default 10 GB). Each is
  measured continuously and exported (`pg_sage_self_cpu_ms_per_cycle`,
  `pg_sage_self_db_time_ms_per_hour`, `pg_sage_self_blocks_per_hour`,
  `pg_sage_self_budget`, `pg_sage_self_budget_exceeded`, loop busy time); going over
  raises one `sage_self_budget` finding that names the resources and the top consumers
  (pg_sage's costliest statements and busiest loops). Admins can now profile a running
  sidecar without restarting or signalling it: `debug.pprof_enabled` (off by default)
  serves Go's profiler at `/api/v1/debug/pprof/` behind the normal login, admins only.
  `GET /api/v1/trust` sent 17 statements one after another and took 1.2-2.1 s on a busy
  host; it now sends its ledger reads in one pipelined round trip (7 round trips in all,
  median 11-22 ms in the performance gate), reads the safety record once, and two new
  indexes keep the safety read and the shadow summary from reading whole history tables.
  The shadow scorer and the trust reconciler read each action's verification verdict by
  key instead of scanning every verdict. The performance gate now charges each API
  endpoint the median of five calls after a warm-up call, budgets the sidecar's CPU per
  cycle, and seeds verification outcomes so their scans are caught. Where pg_sage keeps
  its data in each mode is now documented; keeping history outside the monitored database
  is not supported yet.

- **Managed clouds: host telemetry and parameter-group proposals for RDS, Aurora and Cloud
  SQL.** pg_sage now reads CloudWatch and Performance Insights (RDS/Aurora) and Cloud
  Monitoring (Cloud SQL) with the providers' standard credential chains: CPU, memory,
  storage, IOPS, replica lag and DB load. Host memory grounds `shared_buffers` proposals
  (and caps `work_mem`), provider CPU lets index builds be admitted outside a maintenance
  window, and builds wait while a replica lags, storage runs out or memory is short. Settings
  SQL cannot change become proposals on the Actions page with the exact parameter group or
  database flag, the reboot they need, the rollback and the CLI command; pg_sage never applies
  them, it marks them applied once PostgreSQL runs the new value and reports parameter drift.
  Without credentials telemetry is unavailable and nothing else changes. See
  `docs/managed-clouds.md` (`cloud_telemetry:` section, `GET /api/v1/cloud-telemetry`,
  `/api/v1/managed-changes`).

- **pg_sage now sizes some of its own settings to your database.** Every configuration key
  is classified: safety-critical (trust, approvals, credentials, endpoints, LLM provider,
  anything that widens what pg_sage may do) and your preferences (notification routes,
  windows, declared capacities) are never derived. For five keys that misbehave on large or
  busy databases (the collector interval, the catalog read timeout, sequence sampling, the
  temp-file and LWLock incident thresholds), pg_sage derives a value per database from
  measured evidence when you leave the key unset, always inside bounds that never spend
  more or widen authority. A new value first runs in shadow for 24 hours
  (`self_config.soak_hours`), is compared with the current value's measured outcomes, and
  is promoted only if it is not worse; restart-bound keys change at the next start. Every
  step is recorded with its evidence, bounds and rule version. A value you set always wins;
  the Configuration page's new "Derived settings" section shows each value with its evidence
  and history and lets an admin pin the current value or unpin it
  (`/api/v1/derived-settings`). Turn it off with `self_config.enabled: false`. See
  `docs/generated/derived-settings.md`.

### Fixed

- **pg_sage now makes one change at a time to a setting or a table, and waits for the
  verdict before the next.** On 2026-10-04 lifeos got `work_mem = '10MB'` 39 minutes
  after `work_mem = '9MB'`, while the first change was still being measured, and a second
  index on `public.memories` ten minutes after a first one it subsumed: overlapping
  changes make every verdict meaningless. Now a change pg_sage starts on its own waits
  while an earlier change to the same object (the same GUC, or the same table and its
  indexes; for index, statistics and reloption changes, a whole partition tree) is
  still being verified, or authorized and about to run. The decision log and
  the approval card say which action it waits for and until when; it runs by itself once
  the verdict lands (improved, neutral, regressed, insufficient evidence, unverifiable or
  rolled back) and never waits past that verification's hard deadline (the verification
  cap plus an hour), which is recorded. An index drop holds its table only until its
  first window concludes; its soft-drop monitoring keeps watching the business cycle. Rollbacks and
  reverts of pg_sage's own changes and emergencies (a critical wraparound freeze, a
  critical disk runway) never wait. Approving a queued change overrides the wait, and the
  approval card says so before you click ("approving overrides pending verification of
  action N"); the override is recorded on the decision. `/metrics` gains
  `pg_sage_policy_parks_total{database,reason}` and
  `pg_sage_verification_wait_releases_total{database,cause}`.

### Upgrading

- **Self-configuration is on by default.** On a database where you left them unset,
  pg_sage may derive the collector interval, the catalog read timeout, sequence sampling
  and the temp-file and LWLock incident thresholds, each after a 24-hour shadow run. A key
  you set is never derived. Set `self_config.enabled: false` to keep the static defaults.
- **Cloud telemetry is on by default** and needs no configuration on RDS, Aurora or Cloud
  SQL beyond read-only credentials in the provider's standard chain. Without them it
  reports `unavailable` and nothing else changes.
- **New tables:** Ask Sage conversations and budgets, managed-change proposals and the
  derived-settings ledger, plus two indexes for the Trust page's reads, are created at startup by
  idempotent migrations.

## v2.1.0 (2026-10-04) -- The model drives: tool-calling investigator, tuning agent, specialist API

### What's new

- **The model now investigates instead of only reviewing.** With an LLM configured,
  pg_sage's investigations plan their own reads after the causal graph has scored the
  hypotheses. The reads are catalog probes, pg_stat views, a plan-only `EXPLAIN` by
  queryid, the graph's state and confirmed facts, and none of them can change anything.
  Operator-started and SLO-burn investigations get a broad budget; detector triggers get a
  narrow one. Steps, probes, wall clock and tokens are all capped, and every read is stored
  as evidence with its digest. The model may agree with the graph, conclude an inconclusive
  graph, contest a conclusive root, or name a cause the graph does not model. Every claim
  must cite evidence, and uncited claims are dropped and counted. The model's root stays
  advisory until its family earns root authority on the bench, and its own confidence is
  never used. Contests are tagged in the replay-case export. The investigation view, the
  API (`.../investigations/{id}/transcript`) and MCP (`sre_get_transcript`) show the
  redacted transcript: the plan, each read with its result, and the citations. Set
  `sre.llm.mode: review` to keep the single review turn. PGIncidentBench gains a
  `causal-graph+investigator` arm, gated like the LLM-on arm and scored with scripted
  adversarial transcripts.
- **One tuning agent per database, with confidence it has earned.** The index optimizer,
  the advisor's vacuum and memory prompts and the tuner's LLM hints are replaced by one
  case-driven tuning agent. Each cycle it classifies the workload (application, tenant,
  test-fixture, diagnostic and pg_sage statements; confirmed facts bind tables), finds the
  cases worth tuning (a statement taking a large share of the workload's time, a
  regression, write amplification) and asks the model about each case only, with
  read-only tools (plans, HypoPG what-ifs, write cost, extended statistics, clone
  rehearsal when configured) within a per-database budget per cycle (`tuning.*`). The model
  answers with typed proposals only (index create or drop, setting, storage parameter,
  extended statistics, query hint), each citing its evidence and predicting its effect;
  pg_sage writes the SQL and keeps every gate (HypoPG what-if and rejection memory,
  configuration allowlists, the tuner's hint checks, confirmed facts, earlier operator
  rejections). Every proposal carries a confidence calibrated on the outcomes of comparable
  past actions, or says "uncalibrated" when there are too few; approval cards and the Trust
  page show it, and a low calibrated confidence needs an operator. The fixed-weight
  optimizer confidence score is gone. Lessons from dogfooding are built in: open
  recommendations the agent did not examine stay open (only the catalog can retire one),
  cases the budget cannot reach go first next cycle, the daily token budget is kept in the
  database so a restart does not reset it, a proposal cut by the cycle cap leaves no trace,
  setting changes need evidence measured in the last interval and after the previous
  change's verification (a third change in the same direction within 7 days needs an
  operator), and an index is never proposed beside an existing or in-flight index that
  already serves it or that it would make redundant. See
  `docs/configuration.md#tuning-agent`.
- **The Postgres specialist other agents call.** AWS DevOps Agent, PagerDuty,
  Datadog or any HTTP/MCP client can now ask pg_sage what is wrong with a
  database and why through a stable, versioned contract
  (`pg_sage.specialist.v1`, OpenAPI at `/api/v1/specialist/openapi.json`):
  open or attach an investigation, poll or stream it, and read a cited causal
  chain with the root cause's source and authority, honest confidence
  (uncalibrated unless the bench calibrated the family), missing evidence and
  typed candidate remediations. Agents authenticate with MCP tokens (read to
  investigate, propose to request a remediation); a requested remediation is
  only a proposal that pg_sage's gate decides, and the caller gets the verdict.
  PagerDuty incident webhooks and a generic signed webhook map onto it, with
  results posted back only to endpoints you configure; the MCP tokens page
  shows which agent asked what. See `docs/specialist.md`.
- **A useful first look within a minute, read-only until you grant more.** pg_sage now
  reads the catalog the moment it connects and shows a first look on the landing page in
  about a second: invalid, duplicate, redundant and never-scanned indexes, unindexed
  foreign keys, transaction ID and sequence runway, dead-tuple bloat estimates, leftover
  test schemas (proposed as facts, never touched) and the exact steps to enable
  pg_stat_statements, HypoPG and auto_explain on your provider. Each finding cites its
  catalog evidence; it needs no query history, no superuser and no LLM. The collector
  takes its first snapshot at startup and the analyzer runs on it right away, instead of
  after 1 and 10 minutes. A first-run checklist (connected, extensions, first look, MCP
  token, notifications, grant more) shows live status, and a "Grant more" guide explains
  what each trust level allows and which grants it needs before it sets `trust.level`.
  New installs only observe; upgraded installs keep their configured trust. A database
  without pg_stat_statements now starts degraded instead of refusing to start. Time to
  first finding is exported as `pg_sage_time_to_first_finding_seconds`. See
  docs/quickstart.md.
- **pg_sage now says when pg_stat_statements is full of utility statements.** The capacity
  finding used to count only the 500 statements pg_sage reads, so it never fired on a
  database at 98% of `pg_stat_statements.max`. It now counts every entry and, near the
  limit, how many are utility statements such as pg_dump's `COPY ... TO stdout`, with the
  deallocations so far. When those are at least half, it recommends
  `pg_stat_statements.track_utility = off` (a reload, no restart) or a higher
  `pg_stat_statements.max` (which needs a server restart), citing the counts. It is advice:
  pg_sage does not change the setting itself.

### Fixed

- **pg_sage recognizes all of its own statements on PostgreSQL 14 to 18.** Each statement
  of a multi-statement query (pg_sage's schema setup) and a statement that starts with a
  parenthesis now carry the `/* pg_sage */` tag where `pg_stat_statements` keeps it
  (PostgreSQL 18 drops comments in front of a statement); before, the setup statements
  (about a third of pg_sage's entries right after a start) were untagged and left out of
  its self-cost. The API's connection test now runs as `pg_sage` and tagged, like every
  other pg_sage session.
- **Cheap diagnostic probes no longer time out because pg_sage itself was busy.** A probe's
  time limit used to start before it had a database connection, so on a busy connection
  pool a 0.1 ms catalog read (xid_runway, wraparound_tables, checkpoint_activity) could fail
  with `deadline_exceeded`. Waiting for a slot, waiting for a connection and running the
  query now each have their own limit. A probe that fails says which of them ran out
  (queue_wait, pool_acquire, server_execution or lock_wait) with the time each took, and a
  probe that hit a server-side timeout is tried once more when the caller has time left.
- **The collector retries a statistics read that timed out once.** A category whose read hit
  a statement or lock timeout is read again in the same cycle (at most two retries a cycle),
  and a timed-out read names its phase in the log.
- **pg_sage reads query texts from pg_stat_statements only where it needs them.** The
  temp-spill probe no longer reads any text, and the collector ranks statements on their
  counters before reading the texts of the ones it keeps: on a server with 45,000 entries
  these reads take about half the time.
- **Runway sampling on PostgreSQL 14 tolerates a slow statistics collector.** On PostgreSQL
  14 a backend waits for a fresh statistics file before it reads table statistics, which can
  take seconds on a loaded host; the runway monitor now reads wraparound_tables with its
  2 s background limit. Investigations keep 500 ms.
- **Two flaky PGIncidentBench scenarios are reliable again.** disk-slow-consumer-fill read a
  slot's retained WAL before the WAL was written, because an earlier scenario left its
  connection committing asynchronously; seq-cycling-near-limit failed on PostgreSQL 14 when
  the statistics collector was slow.

## v1.10.0 (2026-10-04) -- The model earns authority: binding facts, model measurement, MCP v2

### What's new

- **Facts you confirm once now bind pg_sage.** pg_sage keeps typed facts about each
  database: an index, table or schema owned by the application's migrations, schemas that
  are test fixtures, a replication slot that feeds a named consumer (CDC), an append-only
  table, a table's maintenance or batch window. Detectors propose them from evidence (an
  index pg_sage dropped that came back, idle test schemas, logical slots, insert-only
  tables) and the model proposes them from the catalog, always citing the evidence; you
  confirm or reject each once on the new Facts page, inline on findings and approval cards,
  in Slack or Telegram, through the API or MCP. A confirmed fact can only narrow or redirect
  what pg_sage does, never widen it: pg_sage no longer creates, drops or alters an object
  your migrations own and attaches the migration it recommends instead (a source-fix
  packet for your PR); test fixtures leave findings and budgets and come back as one
  cleanup script you run; a CDC slot is never dropped or advanced; an archive keeps its
  data and indexes. Every blocked action names the fact, who confirmed it and when. Facts
  expire when the objects they describe are gone. See `docs/facts.md`.
- **The model now has to earn the right to overrule pg_sage's diagnosis, family by family,
  and the benchmark measures it.** pg_sage's causal graph used to win every disagreement
  with the model. Now the model may replace the graph's root cause for an incident family
  only after PGIncidentBench has measured it on held-out cases it was never tuned on, with
  a live model: its overrides must be right with a 95% Wilson lower bound of at least 80%
  over at least 10 of them, with no unsafe action and no drop in Safe Pass. Until then its
  root cause is shown beside pg_sage's as advisory and changes nothing. The benchmark now
  reports, per family, how often the model's overrides were right, how many cases the graph
  left open that the model resolved (right minus wrong), and its Safe Pass against the
  graph's; the Trust page ("Model lift over deterministic") and `GET /api/v1/model-lift`
  show it. The replay corpus is split into tuning and held-out cases by a stable hash of
  each case id, and the quality gates read only the held-out half. New replay cases cover
  real plan flips and incidents with two independent causes. A nightly, paced job measures
  a live OpenAI model under hard caps on requests, tokens, time and spend, stops at the
  first cap, and signs its report like the release benchmark; it runs only once the
  `PG_SAGE_BENCH_OPENAI_API_KEY` repository secret is set. It also runs on every release
  tag and attaches its signed report to the GitHub release (`pgincidentbench-live.*`)
  without ever holding the release back. The Trust page says, per family, how many more
  correct held-out overrides the model needs, and a family earning or losing the right is
  recorded in the trust history with the report that decided it and sent through your
  notification rules. When you refute an
  investigation, you can export it as a redacted replay case
  (`GET /api/v1/databases/{db}/investigations/{id}/replay-case` or
  `pg_sage bench export-replay`): identifiers are hashed unless you opt in, and secrets
  and personal data are removed either way.
- **Coding agents can fix database problems at their source.** pg_sage's MCP server now
  works with Claude Code and Cursor: one `claude mcp add` line with a scoped API token an
  admin creates on the new MCP tokens page (read, propose, and approve only for a person's
  own token; limited to chosen databases; expiring; stored as a hash). MCP is now served over
  HTTP by default and accepts only these tokens, never a dashboard session; set
  `mcp.transport: stdio` to keep stdio. Agents can see the top
  queries with their plans, run a safe EXPLAIN, try an index with HypoPG, lint a migration
  for lock level and rewrites, trace a query to the code that sends it (sqlcommenter tags,
  `application_name`), and propose that an object belongs to the app's migrations. For a
  finding, pg_sage hands the agent a cited source-fix packet (the migration, the evidence,
  the code it touches, how it will be checked), the agent opens the PR, reports the deploy,
  and pg_sage measures the targeted queries before and after and returns its verdict. Agents
  can never approve anything or bypass the policy gate. Every tool now takes a `database`
  argument for fleets, `apply_migration` accepts the arguments it documents and is checked by
  the gate before any rehearsal, and the server follows the current MCP protocol (both the
  handshake versions and 2026-07-28). See `docs/mcp.md`.
- **pg_sage can create extended statistics.** It runs `CREATE STATISTICS` on correlated
  columns and the `ANALYZE` that builds it as one action (one transaction, one entry in the
  action log), and verifies it by the row estimates of the targeted queries. Only its own
  form runs: a `sage_stx_` object in the table's schema, of kinds `ndistinct`, `dependencies`
  and `mcv`, on 2 to 8 plain columns of one table, taking the same `SHARE UPDATE EXCLUSIVE`
  lock as `ANALYZE` (reads and writes continue). It is undone by dropping exactly that object,
  by hand or automatically when the targeted queries regress. It is policy change class
  `analyze` and trust class statistics (tuning family), so it goes through the same approval,
  budgets and earned trust as every other action.
- **`REINDEX` can now earn trust, and `CREATE STATISTICS` has its verifier.** A rebuild is
  verified after it runs by the space it reclaimed, the rebuilt index being valid and the
  table's queries not regressing, and its verdict now counts on the Trust page instead of only
  carrying a level over. Extended statistics are verified by whether the targeted queries'
  row estimates got better (from sampled plans with actual rows) without their latency getting
  worse.

### Fixed

- **Advice is about your workload, never about diagnostic statements.** pg_sage no longer
  raises slow-query, plan-regression or top-query findings, LLM advice, hints, plan captures
  or briefing items for `EXPLAIN` (with or without `ANALYZE`), maintenance (`VACUUM`,
  `ANALYZE`, `CREATE INDEX`, `REINDEX`, `CLUSTER`, `CHECKPOINT`), statistics resets or backup
  `COPY ... TO`, nor for its own tagged statements. One rule applies on every advice path;
  open findings about such statements resolve on the next analyzer cycle. Raw query views,
  self-cost and incident analysis still see every statement.
- **"Autovacuum falling behind" no longer fires on tiny tables.** A table's dead-tuple ratio
  counts only when it has at least `rca.vacuum_min_dead_tuples` dead tuples (default 1000)
  and a heap of at least `rca.vacuum_min_table_mb` (default 8 MB), so a table with a handful
  of rows can no longer open, or escalate to critical, an incident.
- **pg_sage's own small tables stay clean.** The trust ledger no longer rewrites its state row
  on every pass when nothing changed, and pg_sage's small state tables (trust ledger state,
  budget reservations, change leases, approval queue, verifications, query hints, trust
  levels) are vacuumed after a few dead rows instead of 50 rows plus 20%.
- **No stale or redundant index approvals.** An index an existing index already covers
  (including through `INCLUDE`, the same partial predicate or expressions) is never proposed,
  queued or run, whichever path proposed it, and its finding resolves naming the covering
  index. Pending approvals whose finding is resolved, that an existing index covers, or that a
  newer equivalent proposal replaces are withdrawn automatically with the reason (also posted
  to the approval card's chat). Withdrawn and expired approvals never count as a rejection
  against pg_sage's trust.

## v1.9.0 (2026-10-04) -- Earned trust: verified actions, shadow mode, approval cards

### What's new

- **One trust system: earned evidence decides, time is only a floor.** Every action pg_sage
  takes on its own (index create and drop, configuration changes, per-table autovacuum
  settings, vacuum, analyze, query hints, retention, and the SRE remediations) now has one
  trust level per database, earned from verified outcomes and approved by an admin. The trust
  ramp no longer lets anything run by itself; it is the minimum time pg_sage must have watched
  a database before it may propose a promotion. A regressed result, a rollback you make or an
  approval you reject lowers that action's level by one at once, says why, and notifies you.
  Autonomy your current settings already granted is kept as "grandfathered" on the first start,
  and the startup log explains the new meaning of `trust.level` and the ramp settings. A new
  **Trust** page (and `GET /api/v1/trust`) shows every database, action class, level, its
  evidence (improved, neutral, regressed, rolled back, rejected), the last change and why, and
  what is needed for the next level.
- **Shadow mode: pg_sage earns trust from what it would have done.** Below an action's earned
  trust level, pg_sage now records every action it would have taken (the exact SQL, how to
  undo it, its predicted effect and what it would have done if trusted) and never runs it.
  Each one is scored later from what really happened: your decision on the same proposal, the
  same change made later through pg_sage or by a migration (verified like pg_sage's own
  actions), or a HypoPG what-if for index creates. Changes made outside pg_sage and what-ifs
  count toward promotion as "shadow" evidence: shadow evidence alone can earn one-click
  approval (L2), but running unattended (L3) still needs at least 3 real verified successes,
  and every promotion still needs an admin. A wrong shadow decision delays promotion but never
  lowers a level you already granted. The Trust page shows each action's shadow decisions,
  their scores and what pg_sage would have done; approval cards show the action's shadow
  history; `GET /api/v1/shadow-decisions` and two Prometheus counters expose the same.
- **Every action that waits for you now comes as an approval card with the why, and you can
  decide it in one click in the UI, Slack or Telegram.** A card says what pg_sage wants to
  do and to which objects, why it needs you (for example: HypoPG has not verified the index,
  trust level is advisory, the setting needs a restart, or you rejected this exact change
  before), the evidence with its numbers, the model's rationale, the predicted effect, the
  exact SQL and how to undo it, the lock it takes and when the request expires. Approve,
  Reject (with a reason) and Snooze buttons now come with every action type in Slack and
  Telegram, not only with SRE query cancels. A chat button works once, only for the chat it
  was sent to, only for a chat user linked to a pg_sage operator or admin, and only while
  the action is unchanged: if its SQL changed after the card was sent, the approval is
  refused. Approvals from chat run through the same checks as the UI and are recorded under
  the approver's name. Once the action is verified (or rolled back), pg_sage posts the
  result back to the same chat. A snoozed action stays behind approval until the snooze
  ends, then pg_sage asks again; a rejected one stays behind approval as before.
- **Index cleanup can no longer hold back the changes that speed your database up.**
  Housekeeping (unused and redundant index drops, VACUUM, ANALYZE) now has its own daily
  budget, separate from evidence-backed performance changes (on one database, about 20
  index drops in leaked test schemas used the whole day's budget and parked two verified
  indexes for most of a day). A parked change now says which budget is full and when it
  frees. `max_rows_rewritten` is enforced: changes that rewrite a table are charged its
  estimated rows. Two changes can no longer both take the last slot of a budget, even
  from two pg_sage processes on one database, and a waiting change is evaluated once per
  cycle instead of twice. Emergencies are never held back by these budgets: a wraparound
  freeze or disk-space cleanup when the runway is critical, and the undo of pg_sage's own
  change, still pass every other safety check and are recorded as a budget bypass.
  Existing policies keep their limit for performance changes, and a policy saved by this
  version stays readable by older ones; see [Blast-radius
  budgets](docs/configuration.md#blast-radius-budgets).
- **The index optimizer remembers what HypoPG already measured, so the model stops proposing the
  same index every cycle.** On a real database the model suggested one index 18 times in three
  hours under different names and INCLUDE lists, and every time the what-if measured 0% gain.
  pg_sage now remembers each what-if rejection with the workload it was measured on. A repeat
  of the same idea (same columns, operator classes, ordering and filter; any name; INCLUDE
  columns reordered, added or removed) skips the what-if, and the model is told which shapes
  were already measured. The idea is measured again once the workload or table changes
  materially (a target query's calls or mean time, or the table's rows, change 2x; a target
  query appears or disappears) or after 7 days. After three proposals in a row that were all
  already measured, pg_sage stops asking the model about that table until something changes, so
  the repeat costs neither a what-if nor an LLM call; both savings are exported as Prometheus
  counters. Only model suggestions are remembered against:
  deterministic findings such as missing foreign-key indexes and re-checks of open
  recommendations are never skipped. Tune or turn it off under
  `llm.optimizer.rejection_memory`.
- **Every release ships its own signed benchmark, so earning L2 no longer means copying CI
  files.** The release build signs its PGIncidentBench reports with Sigstore (no key to
  manage) and puts them in the image, in the release archive next to the binary and on the
  release page. The sidecar checks the signature offline at startup and counts a report only
  for the exact pg_sage build it measured. On the Autonomy page, "Path to next level" shows
  where each family's benchmark came from (signed release, local run, or unsigned upload),
  and an admin can press "Run bench locally" to run it on a disposable clone; that result
  counts only for the families it covered. Reports you upload yourself still work, marked
  "unsigned (operator-provided)".

### Added

- **Every action pg_sage takes now says what it expects, and is checked against it.** Before
  an index create or drop, a config or table-setting change, a VACUUM/ANALYZE, a query hint or
  a retention batch runs, pg_sage records its predicted effect: which queries it targets, which
  metric should move and by how much, and whether the estimate came from HypoPG, a model or a
  rule (or that there is none). Afterwards it records what it observed and a verdict:
  improved, neutral, regressed, insufficient evidence or unverifiable. The action detail in
  the Actions page shows predicted vs observed with the evidence, and the outcomes are served
  by `GET /api/v1/actions`, `/api/v1/actions/{id}` and the new
  `GET /api/v1/action-outcomes` ledger.
- **Index drops are verified over a business cycle and come back on the first miss.** A drop
  is watched for `verify.drop_window_hours` (default 168, one week) instead of 15 minutes.
  Its definition is kept, and the index is re-created as soon as a query on that table gets
  slower or an active pg_sage hint names it. A shorter window is a fast-elevation setting
  and is reported at startup like the others.

### Changed

- **"It did not get worse" is no longer "success".** Verification compares the call-weighted
  mean execution time of the queries an action targets, before and after, with a
  significance test over the sampling intervals, over windows that grow until there are
  enough calls. Noise no longer flips a verdict, and too little traffic is "insufficient
  evidence", not success. The old check of database-wide cache hit and write latency is
  gone.
- **Only verified improvements earn trust or credit.** Earned autonomy and the value ledger
  count an action only when its verdict is improved; neutral, insufficient evidence and
  unverifiable count for nothing either way, and a regression is rolled back and counts
  against trust. VACUUM and ANALYZE are verified by the dead tuples and modified rows they
  were meant to clear instead of being marked successful at once.


## v1.8.5 (2026-10-03) -- Safety: only verified index advice runs unattended

### What's new

- **Only HypoPG-verified index advice runs unattended, whatever its age.** Index advice saved by
  older pg_sage versions under per-type categories (covering, partial, composite) skipped the
  what-if gate; it is now held for approval until re-verified, and an existing index that
  already covers a candidate rules it out. Stale rollback SQL from those versions is repaired,
  and an action withheld for its own content is recorded once instead of retried every cycle.
- **The blast-radius limit holds at its configured number of tables**, and a change to an index
  counts against its table once.
- **Session counts are the application's sessions only** (autovacuum, replication and parallel
  workers no longer inflate them), and **a stop signal during startup shuts pg_sage down
  cleanly**.
- **Steadier CI:** 29 tests that depended on timing or server-wide state now isolate it.


### Fixed

- **Index advice saved by older pg_sage versions no longer runs without a verified what-if.**
  Before v1.8.0 the optimizer filed its advice under the LLM's own label (`covering_index`,
  `partial_index`, `composite_index`, ...), and those findings skipped the HypoPG what-if
  check, so one built an index unattended on lifeos. Every automatic `CREATE INDEX` now needs
  a verified what-if or an operator approval; only the deterministic missing-foreign-key-index
  rule is exempt. The optimizer re-checks such old advice like current advice (HypoPG,
  duplicates) and retires the old copy, and it no longer proposes an index that an existing
  index already covers. Old findings whose rollback dropped a different index than the one
  they create are repaired at startup, and an approval of such a pair is refused rather than
  run. A withheld index build is recorded once with a closed verification (the ledger
  self-audit no longer flags it) and is not retried until its content changes.

- **Active and idle-in-transaction session counts are the application's sessions only.** The
  system snapshot also counted autovacuum workers, logical replication senders and parallel
  query workers of the database as active sessions, so a busy autovacuum or one parallel
  query inflated the active backends the connection advisor, forecaster and lock analysis
  read, and those recorded in each action's before-state evidence. They now count client
  sessions, as the connection states already did.

- **A stop signal during startup now shuts pg_sage down cleanly.** The SIGTERM/SIGINT handler
  was installed only after the API and metrics servers were listening, so an orchestrator that
  stopped the container right after it reported ready (a rolling deploy) killed the process
  without its graceful shutdown. The handler is now installed before startup begins.

- **The blast-radius limit now holds at its configured number of tables.** pg_sage counted
  the tables its own changes had touched in the last 24 hours without the table the next
  change would touch, so `max_tables_per_window: 20` let a 21st table through. An index
  advice also counted as a table of its own next to its table. The count now includes the
  next change's table and counts index advice as its table, so a second change on an
  already counted table no longer uses up the limit. On lifeos, a verified index on
  `public.events` was held back by this limit: 21 tables were counted, mostly index drops
  in leftover `test_*` schemas. Such an index runs once the window has room for its table,
  and then closes its outdated approval request. Until then the decision log records why
  it waits (`blast_radius_exceeded`).

## v1.8.4 (2026-10-03) -- Dogfood fixes: idle sidecar CPU, verified indexes build themselves, snapshot cap works

### What's new

- **The sidecar no longer burns CPU on databases with many indexes.** The duplicate-index
  rule compared every index with every other one in the database: about 90 s of CPU per
  10-minute analyzer cycle on lifeos (35,000 indexes). It now compares within each table:
  0.19 s, same findings.
- **Verified index advice is built without waiting for an approval it no longer needs.** An
  index queued for approval before HypoPG could verify it now runs once it is verified (the
  pending request is closed as superseded), and the first autonomous optimizer index no longer
  fails at its change lease. Indexes you rejected stay rejected.
- **The schema guard is fast however much history it has.** It re-read every decision it had
  recorded each scan (13 s per scan on lifeos); it now reads the newest one per issue (under
  40 ms).
- **Snapshot history shrinks after the upgrade.** The size cap now acts on the history
  partition the v1.8.3 conversion created, drops it whole when it closes, and the background
  conversion no longer gives up on databases with long-running queries. One slow cleanup no
  longer starves the others.


### Fixed

- **Large `sage.snapshots` tables now convert to daily partitions on busy databases.** The
  background conversion builds the new key with `CREATE INDEX CONCURRENTLY`, which waits for
  every query already running in the database, also queries that never touch pg_sage's
  tables. It ran under pg_sage's 2 s lock timeout, so on a database with queries of a few
  seconds (lifeos: 4-5 s application queries) every attempt was cancelled and the table stayed
  plain. That wait blocks none of the table's readers or writers, so it is now bounded by the
  conversion's 10 minute statement timeout instead.
- **The snapshot size cap now works right after the upgrade, and gives the disk space
  back.** Converting `sage.snapshots` to daily partitions put all existing rows (lifeos:
  9.3 GB) into one history partition that still takes rows for up to two days. The cap
  could not touch it until then and warned on every run. Now the cap waits for that
  partition to close and then drops it whole, which returns its disk space at once
  (deleting rows would only free space for reuse and write as much WAL). It says so once:
  when the partition closes and that it will be dropped then. A history partition that
  stays open longer than 48 hours (rows dated ahead at the conversion) has its oldest rows
  deleted in small paced batches instead (at most 1 GiB a run, never today's rows, never
  a snapshot that a kept one is built on). An empty or fully expired history partition of
  `sage.snapshots` or `sage.query_store` is dropped instead of truncated, and dropping a
  partition can no longer break a kept snapshot that reaches it through a checkpoint.
  Over-cap warnings are logged once per change or day and say what pg_sage is doing.
- **One slow cleanup no longer starves the others.** When one table's cleanup used the
  whole 30 s run budget, the next run started with it again, so on lifeos the query
  history backlog ran alone run after run and the other tables waited. A table that uses
  the budget now goes last in the next run, so every table is cleaned at least once every
  few runs.

- **The analyzer no longer pins a CPU core on databases with many indexes.** The duplicate
  and subset index rule compared every btree index with every other one in the database, and
  the unused-index rule re-read every index definition for each unused index that backs a
  foreign key. On lifeos (35,000 indexes) that cost about 90 seconds of sidecar CPU every
  10-minute analyzer cycle. Both rules now compare only indexes of the same table: 0.19 s on
  42,000 indexes, with the same findings.

- **An index waiting for approval now runs on its own once it no longer needs approval.** An
  index proposal queued because HypoPG had not yet verified it stayed pending even after
  pg_sage verified it later, until the request expired a day afterwards. pg_sage now
  checks the current verdict every cycle. Once the change may run unattended (it was
  verified, or you raised trust), the pending request is closed as `superseded` with the
  reason "approval no longer required" and the change runs exactly once. Requests you
  approved or rejected are never overridden, and a change you rejected stays behind
  approval. A related fix: autonomous index builds from the optimizer no longer fail at the
  change lease with "invalid identifier".

- **The schema guard no longer slows down as its decision history grows.** Each scan
  re-read every decision it had ever recorded about the tables it checks; on the lifeos
  dogfood database, which still holds 253,000 rows written by v1.8.1's ledger flood, that
  took 13 seconds a scan. It now reads only the newest decision of each schema issue and
  the few rows it counts, through two small indexes: under 40 ms on a 250,000-row history,
  whatever its size. The large index the old read used is removed at upgrade.

## v1.8.3 (2026-10-03) -- Ships high performing: pg_sage keeps its own footprint small

### What's new

- **pg_sage never needs a DBA to make it behave.** A new performance gate (now blocking in CI)
  runs pg_sage against a large, messy catalog and fails on any seq scan of a large sage table,
  slow statement, write amplification, catalog timeout, slow API list or non-HOT update. It
  went from 16 offenders to 0. On the lifeos dogfood database pg_sage's ledger writes fell from
  ~38,000 rows/hour to ~550, and the database's CPU from 85% to under 1%.
- **Its own data stays small.** Query history and snapshots are partitioned by day and dropped a
  day at a time; query history records only queries that moved; snapshots are capped at 5% of
  the database; every pg_sage table has paced, budgeted retention. Existing tables are converted
  in place without copying rows, larger ones in the background.
- **It shows its own cost instead of hiding it.** pg_sage no longer turns off
  `pg_stat_statements` for its sessions: its statements are tagged `/* pg_sage */`, its
  sessions are named `pg_sage`, it reports `pg_sage_self_*` metrics and a `sage_self_cost`
  finding, and it leaves its own activity out of everything it analyzes.
- **Reads that stay flat as your database grows.** Catalog collection, the SRE probes, runways,
  earned autonomy, the analyzer and the API (cursor paging, capped totals, live updates from
  change counters) read only what they need.
- **Index advice made before HypoPG was installed is re-checked**, so it can now be verified
  and built autonomously instead of waiting for approval forever.

### Changed (read before upgrading)

- **pg_sage keeps its own data small and cleans it up without a DBA.** Its query
  history and snapshot tables are now split into one partition per day, so old data is
  removed by dropping a whole day (instant, no vacuum, disk space returned at once) instead
  of deleting rows; existing tables are converted in place on first start (the old rows
  stay readable and go when they age out). The per-query history now records a query only
  when its numbers changed (plus once an hour), in one write per minute instead of one per
  query, and keeps 14 days (`retention.query_store_days`) instead of following the 90-day
  snapshot window. Snapshots are capped at 5% of the database (`retention.snapshots_max_pct`,
  never below 256 MB), oldest days first. Cleanup runs in small, paced batches with a 30 s
  limit per run, also covers the approval queue and the agent database tables, and expires
  cached explanations when they expire. pg_sage also stops rewriting rows that did not
  change (change feed cursors, incidents and their causal chains) and stops indexing a
  column it updates every cycle, so updates of findings no longer leave dead index entries.
  Upgrading converts `sage.query_store` and `sage.snapshots` without blocking pg_sage or
  your sessions: the old rows are checked while writes continue, and the tables are locked
  only for a catalog change (about 0.1 s for a 1 GB table). Small tables are converted at
  startup; larger ones in the background after it. If a conversion cannot finish (a long
  transaction holds the table, a timeout, a full disk), pg_sage keeps working on the
  unconverted table, still deletes its expired rows in small batches, logs one warning
  saying what to do, and tries again later (after 1 hour, then up to once a day).

- **pg_sage shows its own cost instead of hiding it, and its API reads stay small on big
  histories.** pg_sage no longer turns `pg_stat_statements` tracking off for its sessions:
  every statement it sends carries `/* pg_sage */` after its first keyword (where even
  PostgreSQL 18 keeps it) and its sessions are named
  `pg_sage`, so a DBA can see exactly what it costs. pg_sage leaves its own statements and
  sessions out of everything it analyzes (index and hint advice, schema guard, leftover
  schema detection, connection leaks), and reports its bill as `pg_sage_self_*`
  Prometheus metrics (database time, statements, blocks, sage-table rows read and written
  per collector cycle, sage schema size) plus a `sage_self_cost` finding above
  `analyzer.self_cost_budget_ms` (default 3000, `0` turns the finding off). With a
  dashboard open, live updates no longer re-count the findings, actions and health tables
  every 2 seconds; they read the tables' change counters. The findings and actions lists
  page with a `cursor` (`next_cursor` in each response) over new indexes, their `total`
  stops counting at 1,000 (`total_capped` says so), and `offset` is limited to 1,000.
  Snapshot history refuses per-object categories (`tables`, `indexes`, `queries`,
  `sequences`, `foreign_keys`, `locks`, `partitions`, `config_data`: read them with
  `/snapshots/latest`) and stops at 4 MB (`truncated`).

### Fixed

- **An index recommendation made before HypoPG was installed is now re-checked.** The
  optimizer re-emits a table's open index recommendation instead of asking the LLM again,
  but it kept the stored "unverified" verdict forever, so installing HypoPG later never
  let pg_sage verify (and, with autonomy, build) the index: it waited for approval
  indefinitely. An open unverified recommendation is now re-measured with HypoPG when it
  is re-emitted: a measured gain makes it verified, no gain resolves it, and without
  HypoPG nothing changes.

- **pg_sage's own catalog reads no longer grow with the size of your database.** On a
  database with 15,000 tables, 35,000 indexes and 12,000 sequences, each collector cycle used
  to rebuild the table statistics view once per 1,000-row page, re-render every index
  definition, stat() every table and index file, and hold one lock per sequence in a single
  transaction (12,000 locks every minute: on a server with default lock settings that could
  make other sessions fail with "out of shared memory"). Now table and index pages read
  counters directly (about 5x faster per page in our tests), sizes come from the catalog's page
  counts with exact sizes for the 100 largest tables and indexes, index definitions are read
  again only when an index or its table changes, and sequences are read 1,000 at a time in
  separate transactions (never more than a quarter of the lock table). The database size is
  measured every 15 minutes instead of every minute and on every `/metrics` scrape, and a slow
  size measurement no longer loses the whole snapshot. The tuner's stale-statistics check and
  the TOAST lint rule stopped opening every table, so pg_sage's connections stay small. Every
  read-only check pg_sage runs on your database (analyzer checks, the optimizer's table
  context, the DDL risk assessment and all schema lint rules) now runs read-only under
  `safety.query_timeout_ms`: a slow check is cut off and reported as not evaluated instead of
  hanging the cycle.

- **pg_sage no longer reads whole history tables to clean up or to find recent rows, and
  a new performance gate keeps it that way.** A test now builds a large synthetic database
  (5,000 tables, 15,000 indexes, 5,000 sequences, 150,000 rows in each of pg_sage's history
  tables), runs pg_sage against it and fails if pg_sage scans a large `sage.*` table end to
  end, runs a slow statement, writes rows per object instead of per change, or runs a catalog
  query over 500 ms. Its first run found the work fixed here: the retention purges of
  explain, alert, verification and resolved-finding history, the change-feed age-out, the
  clean-up that runs when old actions and decisions are purged, and the check for due
  verifications each read their whole table; they now use indexes (added automatically at
  startup). Runway sampling and several Sage SRE windows bounded time in a way PostgreSQL
  cannot use with an index; they now can. The dashboard's live-update check no longer runs
  when no dashboard is open. The remaining findings (the live-update check while a dashboard
  is open, the actions list, forecast history reads, the earned-autonomy reconcile and a
  sequence catalog query) are listed in `reviews/2026-10-03-perf-gate-report.md` for the
  next fix pass.

- **Sage SRE, runway, earned autonomy and the analyzer read only what they need.** The
  earned-autonomy reconcile, the autovacuum-cancellation probe, the verification watch
  lookup and the investigations list each read their whole table on every pass; they now use
  small indexes (added automatically at startup, one migration that checks the catalog
  first). The startup migrations no longer scan `sage.incidents` or the autonomy events
  when there is nothing to change. The analyzer's query-history check decoded every
  snapshot of the lookback window on every cycle (224 ms on the performance gate); it now
  decodes the first snapshot of each of at most 100 time buckets once and remembers it.
  The forecaster decodes two snapshots per day once instead of every snapshot each cycle.
  The plan-regression rule reads the newest two plans per query instead of every plan of
  the week, and no longer stops on plans captured without an execution time. The
  sequence-runway probe reads at most 2,000 sequences per statement and covers larger
  catalogs in slices; the wraparound probe ranks tables before reading their statistics;
  runway trends and the runway restart read one series at a time through the index. The
  schema-health scan of `pg_attribute` runs only after a DDL change (checked every 5
  minutes) or once an hour. SLO windows are computed from running totals stored with each
  sample (a few index probes per series) instead of re-reading every sample every minute;
  samples stored before the upgrade are still read the old way until they age out.

- **pg_sage no longer counts its own sessions and statements as your workload.** Now that
  pg_sage is visible in `pg_stat_statements`, every analysis that reads sessions or
  statements leaves pg_sage's own out: the snapshot's active and idle-in-transaction
  counts, locks, connection states and churn (now per database, also in fleet mode), the
  load circuit breaker, lock chains and the Sage SRE lock graph, long-transaction, wait and
  temp-spill evidence, the runaway detector's blocker counts, the DDL risk score, the
  tuner's and the briefing's active sessions, auto_explain plans from the logs, and the
  write-latency check that decides whether an action caused a regression (pg_sage's own
  writes could trigger a rollback). Connection slots still count pg_sage, and pg_sage
  still shows up when it holds a lock or the xmin horizon. Every withheld index build is
  now counted: a failed record used to be dropped silently. `/value` reads only credited
  actions through a new index, the actions list counts through the time index, the
  app-managed-index check and RCA's rollback history read pg_sage's drops and rollbacks
  through small indexes, retention checks verifications and credited actions by index
  instead of reading those tables,
  and SRE investigation updates are heap-only again (no index on `updated_at`). The
  Findings and Actions pages show a capped total as "1000+" and load further pages.

## v1.8.2 (2026-10-03) -- Safety first: reversible config, safe EXPLAIN, per-database trust, promote from the UI

### What's new

- **Config changes pg_sage makes are reversible and checked.** The prior value and real
  rollback are captured, the effective value is read back, and success requires the targeted
  metric to move. Settings and table options outside an allowlist become advice only.
- **Safer SQL and LLM handling.** `/explain` only runs `ANALYZE` when the query provably has no
  side effects; the prompt-injection guard and redaction were hardened; destructive DDL must be
  schema-qualified; MCP stdio no longer gets corrupted by the briefing.
- **Index advice you can trust.** HypoPG checks no longer fail open; gains are weighted by query
  time; expression and partial indexes work (every partial index was rejected before); an
  unverified optimizer index needs approval; live tenant schema families are never called
  leftovers.
- **Earned autonomy per database, and you can earn it from the UI.** Accept or reject
  investigations, "Evaluate now", and a "Path to next level" panel. Only verified outcomes and a
  person's reviews count.
- **pg_sage stops flooding its own ledger.** The schema guard and policy gate write a decision
  only when something changes (lifeos: ~38,000 rows/hour before), with the indexes and retention
  that ledger needs.

### Added

- **Safety fixes for EXPLAIN, LLM prompts, index drops and MCP (Phase 0).** `/explain` only
  runs EXPLAIN ANALYZE when the query provably calls nothing with side effects (no volatile
  functions such as `pg_terminate_backend` or `dblink`, also not inside views, no row locks or
  data-modifying CTEs); otherwise it returns the plan without ANALYZE and says why. A zero or
  negative `explain.timeout_ms` now means the default instead of no limit, plan-only and
  ANALYZE results are cached separately, and a fallback after an LLM failure is cached for one
  minute only. Text sent to the LLM is delimited and redacted more reliably (Unicode tag
  tricks, `E''` strings, dollar quotes, plan JSON), the plan-regression narrator and action
  justifier now use that protection, and the narrator no longer replaces a finding's
  recommendation. `DROP INDEX` and `ALTER TABLE` run by pg_sage must name their schema, so a
  search_path cannot steer them into `sage` or `pg_catalog`. With the MCP stdio transport the
  daily briefing's stdout channel is written to stderr, and MCP now answers `ping`, ignores
  notifications and returns tool results as `content` blocks.

### Changed (read before upgrading)

- **pg_sage's snapshot history takes about a tenth of the space, and pg_sage warns when it
  grows too big.** The collector used to store the full list of every table, index,
  sequence and query each minute, so `sage.snapshots` reached 9.3 GB on a personal
  database. It now stores a full copy at most every 6 hours and, in between, only what
  changed. On an hour of collection with 5,000 indexes this writes 11x fewer bytes overall
  (indexes alone 21x to 26x fewer). Every screen, forecast and API reads exactly the same
  data as before. Existing history is not rewritten: old rows stay readable and age out
  with `retention.snapshots_days`. To read snapshots in SQL yourself, use
  `sage.snapshot_data(data, base_id)` instead of the `data` column. A new
  `sage_footprint` finding warns when pg_sage's own tables pass
  `retention.sage_size_warning_pct` percent of the database (default 10, `0` turns it off).

- **Config changes are reversible and checked, not assumed.** When pg_sage applies an
  LLM-proposed setting or table storage parameter, it first records the old value and the
  exact SQL to put it back, then reads the setting back after the reload. A change that did
  not take effect is undone and marked failed; one that needs a restart is marked as such.
  It is only counted as a success when the thing it was meant to fix measurably improved
  (fewer temp-file spills, fewer dead rows, more HOT updates); otherwise it is
  "unverifiable". Only allowlisted settings run; anything else, including turning
  autovacuum off, stays advice. Unused-index drops now use `last_idx_scan` (PG16+) and are
  never automatic on a database with replicas, whose index use pg_sage cannot see.

- **Earned autonomy is per database, and you can earn it from the UI.** In a fleet, one
  database's reviews and outcomes no longer promote or demote another (bench reports stay
  shared); levels set before this release apply to each database until it decides
  otherwise. A success that was never verified now earns nothing. Cases gains Accept /
  Reject (with a note and the actual root cause) on every finished investigation, and the
  Earned autonomy page gains "Evaluate now" and a "Path to next level" checklist that says
  what is still missing, with counts and ETAs. MCP adds `sre_review_investigation` and
  `sre_evaluate_autonomy`; a review an agent records through MCP is kept but never counts
  toward promotion, and approving a promotion stays a human step.

### Fixed

- **Internal cleanup of the sidecar's largest files, with no change in behavior.** The
  sidecar's entry point, the core of the action executor and the API router were split
  into smaller files, one per job, so each file and function stays within the project's
  size limits. New tests pin what each mode starts, every API route and who may call it,
  and they pass unchanged before and after the split. One small visible difference: if
  writing a Prometheus `/metrics` response fails (for example, the scraper hung up), the
  sidecar now logs a warning instead of ignoring the error.

- **A statistics reset can no longer make a used index look unused.** Unused-index
  findings can lead to an automatic `DROP INDEX`. If the statistics were reset (by
  `pg_stat_reset()`, a single-table reset or a restart) between two snapshots, scans that
  happened before the reset were invisible. pg_sage now records when the statistics last
  reset with every snapshot, and restarts an index's unused clock at that reset. It also
  restarts the clock when a counter goes down or when the index is dropped and recreated
  under the same name. An index is reported only after a full clean window. Before any
  drop, automatic or approved by an operator, pg_sage checks the evidence again live. If
  it no longer holds, pg_sage refuses and says which check failed: the index was scanned,
  the statistics were reset inside the window, or the index is gone.

- **Index, schema-family and hint advice you can trust more (Phase 0 tuning correctness).**
  New indexes run on their own only when HypoPG measured the whole workload and showed a
  gain, weighted by how much time each query really takes; anything it could not measure
  waits for your approval. Expression and partial indexes are understood, partitioned tables
  get a step-by-step plan instead of a statement PostgreSQL would reject, and the database
  collation is read correctly on PostgreSQL 16+. Live schema-per-tenant designs are no longer
  mistaken for leftover copies: each problem is shown once with the list of affected schemas,
  and pg_sage only suggests dropping schemas that are truly idle. Query hints are suggested
  only when the plan and catalog support them (no more hints for every scan or empty join
  hints), and the hints page shows whether a hint was proposed, applied or rolled back. The
  analyzer and optimizer read a bounded slice of their own history instead of all of it.

- **The decision ledger no longer grows by tens of thousands of rows an hour.** On a
  database with 15,000 tables and 160 leftover test schemas, `sage.decision` grew by about
  41,000 rows an hour (about a million a day, kept for a year). The policy gate wrote a new
  row every time it re-checked a candidate it was holding back. The schema guard wrote
  one row for every schema issue on every cycle and ran one unindexed lookup per issue.
  Now:
  - A parked, queued, blocked or observe-only verdict that repeats updates its existing row
    (`repeat_count`, `last_seen_at`) instead of adding a new one. Each executed decision
    still keeps its own row.
  - Cheap checks that skip a candidate now run before the policy gate.
  - The schema guard records a decision only when it changes (its route, outcome, reason or
    SQL, or the issue coming back after it was gone). It reads its history and table
    contracts in one query each per cycle and reads `pg_stat_statements` once per cycle.
    Unchanged issues are still checked and handled every cycle exactly as before.
  - Schemas that are copies of one another (same tables, generated names, five or more
    copies) are handled as one family. A family in use, such as one schema per tenant, is
    still fixed in every schema but recorded once per issue with the list of schemas. A
    family nothing has used within `analyzer.unused_index_window_days` (no scans, writes,
    statements or sessions) is noted once as an idle leftover and not fixed.
  - After pg_sage's own DDL, the guard re-checks at most once per
    `analyzer.schema_guard_ddl_debounce_seconds` (default 60).
  - Non-executed decisions are removed after the new `retention.decisions_days` (default
    30, counted from when they were last seen), unless an action or a verification refers
    to them.
  - At startup, pg_sage adds the ledger's missing indexes once, keeping any that already
    exist: `idx_decision_schema_guard_targets`, `created_at`, and every foreign key into
    or out of `sage.decision`. On a very large existing ledger, create them
    `CONCURRENTLY` by hand first (see the review report for the statements).

## v1.8.1 (2026-10-02) -- Fast trust, big-catalog fixes from dogfooding, current OpenAI models

### What's new

- **Works with current OpenAI models.** gpt-5/gpt-6 models (including the low-cost
  gpt-6-luna) now work for every LLM feature; pg_sage adapts the request shape automatically.
- **Trust in hours, not weeks, when you ask for it.** Every trust timer and promotion
  threshold is a setting with the spec value as default, for dogfood and test databases.
  Irreversible actions, L4 and admin approval stay hard limits.
- **Safe on big, messy databases.** Found by running pg_sage on a real 18 GB database:
  catalog collection, forecasting and sequence runways stay bounded on tens of thousands of
  objects; stale and duplicate incidents clean themselves up; indexes the application keeps
  recreating are left alone; long-stale rollback monitors expire instead of acting.
- **Sage SRE follow-ups.** Detector episodes become incidents, PgBouncer pool exhaustion is
  diagnosed, failovers between or before samples are caught (`causal-v4`), and fleet
  databases can be added, removed or changed without a restart.
- **One locked path for every change.** Retention deletes run through the executor with
  leases and verification; operator actions lock the exact object; `serialize_mode: queue`
  really queues.

### Added

- **Fast trust elevation for dogfood databases.** Every timer and threshold that gates
  trust is now configurable, with the spec value as the default. These are the trust ramp
  (`trust.ramp_safe_hours` / `ramp_moderate_hours`), an hour-scale IO baseline
  (`verify.io_baseline_hours`) and the earned-autonomy promotion bar
  (`sre.autonomy.promotion.*`). A documented profile lets a database earn autonomy in hours
  instead of weeks. Irreversible actions keep the full ramp and never go above L1, L4 is
  never reached, and an admin still approves every promotion. The sidecar logs a WARN for
  each lowered value, and the Earned autonomy page shows a "Fast elevation" badge.

- **Checkpoint storms, temp-file explosions and LWLock contention are now incidents.** When
  the Sage SRE detector sees one of them, it opens a warning incident (or uses the open
  incident it belongs to) with the measurement and its threshold as evidence. The incident
  shows in the Cases panel with its investigation, sends the usual incident notifications,
  resolves itself once the episodes stop, and its investigation can be reviewed for earned
  autonomy. The detector's thresholds are now settings (`sre.detectors.*`, today's values
  are the defaults). Stored bench and game-day reports now age out after
  `sre.autonomy.report_retention_days` (90 days), except the reports that current autonomy
  levels or pending promotions rest on and the newest report of each family.

- **Databases can be added, removed and changed without restarting pg_sage.** In fleet
  mode, saving the config file with a new, removed or changed database applies it at once:
  trust level, execution mode, the executor switch and tags change on the running database;
  connection, credential and other changes restart only that database's monitoring. A
  removed database first lets running actions finish (up to a minute), then stops cleanly
  and drops out of the metrics. A bad edit is rejected and everything keeps running as
  before. In meta-db mode, changes made to the database list by another pg_sage or by SQL
  are picked up within 30 seconds. Removing or moving the first (control) database still
  needs a restart.

- **Sage SRE sees pool exhaustion at PgBouncer, and failovers between or before its
  samples.** List your PgBouncer admin consoles under `sre.poolers` (the DSN from an
  environment variable or a mounted file, never logged) and connection investigations read
  `SHOW POOLS` and `SHOW STATS` at both samples, read-only and time-bounded. Clients queueing
  at the pooler while PostgreSQL has headroom are now diagnosed as pool exhaustion at the
  pooler instead of "no connection pressure"; without a pooler the diagnosis says pooler
  telemetry is unavailable. Connection and WAL investigations now refuse to compare two
  samples taken across a restart, a failover, a different server or a major-version upgrade
  and say so as missing evidence. After pg_sage restarts it remembers the database's role,
  timeline and cluster identity, so a failover that happened while it was down still pauses
  earned autonomy for the failover cooldown. The causal graph is now `causal-v4`. In a
  fleet, the runway monitor measures each cluster's total database size once per pass
  instead of once per database.

- **pg_sage keeps working on databases with huge catalogs and cleans up after itself (dogfood
  on a real database with 12,000 sequences and 35,000 indexes).** Sequence runways are measured
  again (the probe now takes about 200 ms instead of timing out), the collector and forecaster
  no longer overload the database they watch, and one slow catalog query no longer drops the
  whole snapshot. Incidents that have not been seen for 24 hours, or whose idle session is
  gone, now resolve on their own, and old duplicate incidents are merged. Indexes that the
  application keeps recreating are left alone and reported instead of dropped again, copies of
  one schema are reported once, a monitor resumed months later no longer rolls anything back,
  and expected policy refusals are no longer logged as errors.

### Changed (read before upgrading)

- **Retention deletes, operator actions and queued changes now share one locked path.**
  A retention delete (an owner-declared `retention_column` contract, D5) now runs as a
  recorded action through the same pipeline as every other change: it is authorized,
  takes a lease on its table, is re-authorized after waiting (so an emergency stop pressed
  meanwhile stops it), and is verified. A batch never deletes more rows than its reviewed
  dry run described (twice its count plus 100, in total across batches); after that a new
  dry run must pass review. Every deleted row is checked against the declared column and
  the table before the batch commits. Operator actions ("Take action", approved queue
  items) now lease the exact table or index they change, found by its database identity,
  so a rename or a different spelling cannot slip past. An index lease also covers its
  table, and custodian `VACUUM` (freeze) runs take a lease too. While another action holds
  the object, an operator gets HTTP 409 naming the holder. The policy's
  `serialize_mode: queue` now really queues: a waiting change keeps its place in line per
  object (first come, first served), the line survives a sidecar restart, at most 8 wait
  per object and 64 per database, and a wait ends after 2 minutes for pg_sage's own
  actions (it parks and retries next cycle) or 30 seconds for an operator (refused).
  `park`, the default, is unchanged.

### Fixed

- **pg_sage works with current OpenAI models (gpt-5, gpt-6 and later).** These models
  refuse `max_tokens`, and refuse tool calls unless `reasoning_effort` is `none`, so every
  LLM feature used to fall back to its deterministic path. pg_sage now notices the refusal,
  re-sends the request once in the shape the model wants, and remembers it for that model.
  Other providers, Gemini included, see no change. Two new settings pin the shape if you
  need to: `llm.token_parameter` and `llm.tool_reasoning_effort` (both default `auto`).

## v1.8.0 (2026-10-02) -- Sage SRE: eleven incident families, approved actions, earned autonomy

### What's new

- **pg_sage is an AI DBA: LLM features are on by default.** As soon as an LLM endpoint and
  key are configured, the optimizer, advisor, tuner hints, incident narration and the Sage
  SRE model turn use it. Without one, everything runs deterministically and pg_sage logs
  one line saying how to configure it. Spend is capped by `llm.token_budget_daily`.
- **Sage SRE investigates eleven incident families and starts by itself.** Lock blocking,
  connection pressure, WAL retention, plan regression, checkpoint storms, temp-file
  explosions, replication lag, LWLock contention, and three runways: wraparound, disk/WAL
  and sequence exhaustion. Runway investigations open *before* the incident. Every
  investigation answers "what changed?" with cited evidence, and your LLM reviews the
  causal graph's result: it may rank, ask for one more probe and write cited claims, but
  never overrides a conclusive root cause.
- **Approved actions.** When one backend is blocking everyone, pg_sage proposes cancelling
  exactly that backend. A human approves it in the UI, Slack or Telegram; pg_sage re-checks
  the identity, cancels, then verifies the incident cleared.
- **SLOs and burn-rate alerts.** App SLOs from Prometheus or signed pushes, plus four
  labeled database proxies; page-level burns open an investigation.
- **Earned autonomy.** Each incident family and action has a level from L0 to L4.
  Promotion needs benchmark evidence, a track record and your approval. Autonomy drops
  automatically while an error budget burns or during failover, and existing custodian
  autonomy is carried over unchanged.
- **Signed runbooks and incident memory.** The LLM drafts a runbook from an English
  playbook; it runs only after an admin signs it. Similar past incidents inform the model.
- **PGIncidentBench.** An open benchmark that scores the investigator against baselines on
  real fault programs and a 60-case replay corpus, with decoys, background load and
  pre-registered gates, in CI on PostgreSQL 14–18.

### Added

- **Operators can start, stop and resume Sage SRE investigations.** `POST
  /api/v1/databases/{db}/investigations` starts one for a case (a repeat joins the running
  one), and `.../{id}/stop` and `.../{id}/resume` pause and resume it without losing its
  steps or evidence. Stop and resume need the version you saw, so two people cannot undo each
  other by accident. The Cases panel has Stop and Resume buttons for operators. Viewers can
  only read. Every change is recorded with the user.

- **The Cases panel shows what the model said, and the timeline.** An investigation now
  shows the model ranking (an order, labeled, kept apart from the graph's scores), the
  model's claims with links to the exact evidence they cite, the probe the model asked for,
  and a timeline of every step, including when a model reply was not used and why, and when
  the model disagreed with the graph (the graph's root cause stands).

- **PGIncidentBench replays 60 recorded incidents.** Besides the live fault programs, the
  bench now replays a corpus of redacted incident recordings through the real investigator:
  30 lock, connection and WAL incidents, 15 harmless lookalikes, and 15 cases with missing
  or hostile data (no privilege, timeouts, a restart, stale or contradictory samples,
  instructions planted in table, slot and application names, passwords in error messages).
  It checks that no call outside the probe catalog is made, nothing in the database changes,
  no planted password reaches an export or a model prompt, and every model claim cites real
  evidence. The replay runs with every test run (about 25 seconds). Its case format is
  documented in `sidecar/sre-bench/README.md` so new incident families can add cases. With
  the LLM off, the investigator names the right cause in all 30 positive cases (29 before
  the connection-leak fix below, which that one missed case motivated) and abstains on all
  23 cases whose evidence is insufficient.

- **New page: Sage SRE permissions and data flow** (`docs/sage-sre-permissions-and-data-flow.md`):
  what investigations read, which role each provider needs (`pg_monitor`), what leaves the
  database, exactly what is sent to the LLM (redacted, fenced summaries, never raw rows or
  query text), retention, and how to turn each part off.
- **SLOs, burn-rate alerts and a change feed for Sage SRE.** pg_sage now tracks error
  budgets. Out of the box it measures four database proxies for every database: query
  latency (compared with the database's own last week), server-side errors in the log per
  transaction (needs log access), connection slots running out, and replication lag over 60
  s. They are always labeled "proxy" and never claim customer impact. To track what your
  customers see, register an app SLO under `sre.slo.objectives`: either two PromQL queries
  (bad and eligible events, read through a Prometheus-compatible API with a bearer token) or
  counters your service pushes to `POST /api/v1/sre/sli/{name}`, signed with
  `sre.slo.push.hmac_secret`. Burn rates use the Google SRE workbook rules: page at 14.4x
  over 1 h and 5 min or 6x over 6 h and 30 min, ticket at 1x over 3 days and 6 h; you can
  change them. Too little traffic, no data, stale or partial data is shown as "unknown"
  with the reason, never as "ok". When an app SLO burns at page level, pg_sage opens a
  read-only investigation that checks locks, connections and plan regressions and says when
  the cause is probably outside PostgreSQL (a proxy burn does this only with
  `sre.automatic_start`). Every investigation now also answers "what changed?": deploys,
  migrations and feature flags you send to `POST /api/v1/sre/change-events` (signed with
  `sre.change_events.hmac_secret`, timestamp-checked, replays ignored), plus pg_sage's own
  actions, config changes, DDL seen by the migration detector, `pg_stat_statements` resets,
  restarts, failovers and extension upgrades. After a fix, the SLO recovery check only
  confirms recovery from enough fresh traffic: missing data, counter resets or traffic that
  simply stopped never count. See it on the new SLOs page, `GET /api/v1/sre/slos`,
  `GET /api/v1/sre/changes`, the `sre_list_slos`, `sre_get_slo` and `sre_list_changes` MCP
  tools, and `pg_sage_slo_*` metrics. Turn parts off with `sre.slo.enabled`,
  `sre.slo.proxies.enabled` or `sre.change_events.feed_enabled`.
- **Sage SRE proposes one approved action: cancelling the backend that blocks everyone.**
  When an investigation concludes that one active statement is the root of a lock or
  connection-pressure incident, it proposes `pg_cancel_backend` for that exact backend (pid,
  backend start, query start, database, user and query hash), derived only from the
  investigation's evidence. Termination and idle-in-transaction holders are never proposed;
  the investigation says why. Proposals are on by default (`sre.actions.proposals`) and never
  execute by themselves. Each gets one item in the existing approval queue, and only a human
  approval runs it through the policy gate. pg_sage then rechecks the backend's identity
  (evidence at most 5 s old) and refuses, with the reason on the timeline, if anything
  changed. After the cancel it verifies recovery over fresh samples and records the result on
  the investigation. Approve or deny on the Actions page, in the Cases panel, or with Slack
  and Telegram buttons. Callbacks are signed, processed once, and attributed to the pg_sage
  user an admin mapped the chat user to. MCP agents get `sre_propose_action` and
  `sre_request_execution`, which create at most one approval item and never execute.
  Telegram is a new notification channel type. Requires `trust.level` `advisory` or
  `autonomous`.

- **Sage SRE investigates four more incident types: checkpoint storms, temp-file
  explosions, replication lag and LWLock contention.** Each one runs its own read-only
  probes and tells apart the usual causes. For checkpoint storms, that is `max_wal_size`
  too small for the write rate, something issuing `CHECKPOINT`, or a short
  `checkpoint_timeout`. For temp files, one runaway query, one statement that spills on
  every call, or `work_mem` small for many statements. For replication lag, where the lag
  sits: not sent, not flushed, or not replayed. On a standby, it also checks paused replay
  and standby queries holding replay back. For LWLock contention, the lock manager, WAL
  writes, buffers, or the subtransaction or multixact caches, attributed to a query. The
  packet also lists the explanations it ruled out and why. Investigations start from the
  matching RCA incidents. Replication lag incidents now start a replication lag
  investigation instead of a WAL retention one. A built-in detector also starts them for
  checkpoint storms, temp-file growth and LWLock contention, with conservative thresholds
  (documented in `docs/configuration.md`). Nothing is executed. PGIncidentBench gains
  fault programs, decoys and background-noise runs for all four types.

- **Runways and pre-incident investigations (Sage SRE).** pg_sage now watches how fast your
  database approaches four hard limits: transaction-ID wraparound, a full disk (when you
  declare `forecaster.disk_capacity_bytes`), the WAL a replication slot may retain, and a
  sequence running out (including a bigint sequence feeding an integer column). Every minute
  it samples these series and projects when each limit is reached. When one comes within its
  horizon (14 days for wraparound, 72 hours for disk and slots, 30 days for sequences), it
  opens a forecast finding and a read-only investigation that explains why: for example a
  forgotten transaction holding back vacuum, a slot nobody consumes, or a column narrower than
  its sequence. The investigation lists the existing freeze or WAL-bound action that fixes it
  with the policy gate's verdict, but never runs it; your custodians still act under your
  autonomy settings. A verified WAL bound now earns an "incident avoided" (disk full, near
  miss) when the measured fill trend shows the disk would have filled within the horizon and
  no longer does; managed providers are never credited. Settings are under `sre.runways`.

- **Sage SRE runbooks and incident memory.** You can now write typed runbooks for a
  database: a small flowchart of read-only catalog probes, yes/no decisions on their
  results or on the causal graph's hypotheses, and a final proposal (a manual step, a typed
  action to request through the normal approval flow, or "escalate"). Write one as JSON, or
  paste an English playbook (for example an imported Xata playbook) and your configured LLM
  turns it into a draft, which is checked against the probe catalog and graph. A runbook
  only ever runs after an admin signs the exact version they reviewed; any edit needs a new
  signature, and a retired runbook never runs. When a signed runbook matches an
  investigation, it adds its probes (within the usual 12-probe and 120 s limits), records
  which version ran and what it proposes, and never executes anything. Investigations also
  look up similar past incidents of the same database, with any outcome an operator
  confirmed or refuted, and show them to the model and in the Cases panel as context only
  (never as evidence, and never anything newer than the investigation itself). Manage
  runbooks under Advanced > Runbooks, the `/api/v1/databases/{db}/runbooks` routes, or the
  new MCP tools (which can draft but never sign).

- **Sage SRE investigations get a model turn (on by default whenever an LLM is
  configured).** After the causal graph diagnoses an incident, your configured LLM reviews
  the result. It can reorder the graph's own hypotheses (shown separately as "model
  ranking", never mixed into the graph's scores). While the graph is inconclusive, it can
  ask for one more catalog probe, after which the graph diagnoses again. It can also write up
  to 5 claims, each citing the evidence it rests on (shown as "model-generated narrative").
  Every reply is checked: known hypotheses only, catalog probes with valid arguments,
  evidence of this investigation, and every number found in that evidence. A bad reply gets
  one retry; after that, or on a timeout, a rate limit or an exhausted budget, the
  investigation keeps its deterministic result and records why (`model_rejected`). When the
  graph has a conclusive root cause it always wins, and a disagreement is recorded
  (`model_disagreed`). Each investigation is limited to 2 model turns, 16k input and 4k
  output tokens, and 120 s. The daily allocation is `llm.token_budget_daily`. Without an LLM,
  investigations stay deterministic and the sidecar logs once why. To turn it off, set
  `sre.llm.enabled: false`. Reasoning models (Gemini 2.5+/3, OpenAI o-series, DeepSeek R1)
  work. Their thinking gets its own 16k-token allowance per investigation, separate from
  the 4k answer limit and counted in the daily budget.

- **Wraparound near misses are credited.** When a table is inside the red wraparound
  buffer and pg_sage's verified `VACUUM (FREEZE)` returns it to green, the value ledger
  records one "incident avoided" (`xid_wraparound`, near miss, 120 minutes). Both sides are
  measured on the table, immediately before and after the action. The credit is separate
  from DBA-hours saved and links to the decision and its verification. Until now
  `sage.incident_avoided` was never written, so the Value page always showed zero
  incidents.
- **PGIncidentBench v1** (`sidecar/sre-bench`). It scores the Sage SRE investigator
  side by side with an "always escalate" baseline and a rules-only baseline. The
  scenarios cover lock blocking, connection pressure, WAL/replication retention and plan
  regression. Each fault program has a clean variant, a variant under unrelated background
  load, and benign decoys. A decoy looks like a fault, and the right answer is
  "inconclusive". Results are reported per family, never only pooled: Safe Pass (correct
  or abstain, with no forbidden action), top-1, top-3, abstention, selective accuracy,
  false diagnoses on decoys, run-to-run consistency, probe count and time to first
  evidence. Each result shows its denominator and a 95% Wilson interval. A safety grader
  checks the database itself for forbidden actions. The release gates are pre-registered
  as code constants: top-1 >= 80%, abstention >= 95% where evidence is insufficient, zero
  forbidden actions, and decoy or noise variants no more than 10 points below clean
  (CHECK-42). The bench fails when the investigator misses a gate. Gates that need the
  LLM-on arm or replay data are reported as "not evaluated", never as passed. The LLM-on
  arm runs the investigator with its model turn. By default it uses a built-in
  adversarial fake model. The fake ranks the graph's last hypothesis first, always asks
  for a probe, and sometimes sends bad replies (fenced JSON, unknown nodes, invented
  numbers, rate limits). The bench fails if, against the fake, the LLM-on arm scores lower
  than the deterministic arm on Safe Pass or top-1 in any family, or changes a root the
  deterministic arm concluded. It can be pointed at a real OpenAI-compatible endpoint
  (`PG_SAGE_BENCH_LLM_URL`, `_MODEL`, `_KEY`) to evaluate the §12 quality gates. The bench writes a JSON result and a Markdown summary to
  `SAGE_BENCH_REPORT_DIR`, and CI uploads them. On PostgreSQL 16 the deterministic
  investigator passes every gate it can be evaluated on, with no false root on any decoy.
  The rules-only baseline names a false root on every decoy that imitates a connection,
  WAL or plan fault. This is an in-distribution seed set, not a held-out measurement. The
  slow-consumer scenario now uses a logical slot, so it runs where `pg_hba.conf` refuses
  physical replication connections (it used to be skipped).
- **Sage SRE earned autonomy (on by default; existing autonomy is kept).** pg_sage keeps a
  level per incident
  family and action class: L0 observe, L1 script only, L2 one-click approval handoff, L3
  auto-execute a reversible single-object action inside the standing-policy window and
  notify. L4 is never reached, and irreversible classes never go above L1. pg_sage proposes
  a promotion one level at a time from the spec's evidence: PGIncidentBench results, 30
  days of accepted shadow reviews, verified recoveries and zero safety violations. Only an
  admin can approve it, and the approver is recorded. Error-budget fast burn, an unknown
  app SLO, a non-primary or recently failed-over node, stale evidence, a concurrent action
  on the same object, or a family safety regression caps actions at L1, and each cap is
  logged. A harmful outcome demotes the whole family until the evidence is earned again.
  A critical XID or disk deadline that the standing policy lets override (a red
  wraparound freeze, for example) is never held back by the ledger. It is still stopped by
  the emergency stop and the rest of the gate, and the ledger records it. Also added: game days on disposable clones (off by default), a fleet canary that halts
  and rolls back on regression, the **Advanced > Earned autonomy** page, and the
  `/api/v1/sre/autonomy` routes. The MCP tools `sre_get_autonomy` and
  `sre_downgrade_autonomy` are also new. See `sre.autonomy.*` in the configuration
  reference.
  On the integrated M5 and M6 code, the ledger reads each database's own SLO engine for
  the error-budget signal. An approved M5 backend cancel is approval-only (never above L2,
  never carried over), and each recovery pg_sage verified is recorded as evidence for its
  family. Executed custodian actions are recorded as outcomes, including mandatory deadline
  overrides, which never count toward promotion. The M6 reactive and runway families are in
  the ledger. `sre.autonomy.bench_results_path` now also reads the per-shard reports CI
  writes under `pgincidentbench/`.

### Changed (read before upgrading)

- **Sage SRE investigations now start by themselves.** `sre.automatic_start` defaults to
  `true`: after the upgrade, each open lock, connection or WAL incident and each plan
  regression gets one read-only investigation (catalog probes only, at most 12 probes and
  120 s, never an action). It works with or without an LLM. To keep starting them only by
  hand, set `sre.automatic_start: false`.
- **LLM features are on by default.** pg_sage is an AI DBA, so `llm.enabled`,
  `llm.optimizer.enabled`, `advisor.enabled`, `tuner.llm_enabled` and
  `rca.narration_enabled` now default to `true` (`explain.enabled` already did). They call
  a provider only once an LLM is configured: `llm.endpoint` and `llm.api_key` (or
  `SAGE_LLM_ENDPOINT` and `SAGE_LLM_API_KEY`). Without one, every feature keeps its
  deterministic path, no LLM request is made, and startup logs one line saying the LLM is
  not configured and how to set it.
  - **Upgrading:** if your config or saved Settings already has an endpoint and key but
    never set these switches, LLM calls start after the upgrade. An explicit `false` in
    YAML or a saved Settings/API value still wins.
  - **Cost:** spend is capped by `llm.token_budget_daily`, 500,000 tokens a day by default
    across all general-LLM features (`0` means no cap).
    [docs/llm-costing.md](docs/llm-costing.md) estimates about 150,000 tokens a day for a
    small-to-medium database and lists per-feature estimates and prices.
    `llm.optimizer_llm.enabled` stays `false`: turning it on adds a second client with its
    own daily budget.
  - **No new autonomy:** trust level, tier flags and execution mode keep their defaults.
    LLM output becomes a finding, a hint or a narrative; any change it proposes runs only
    through the policy gate, trust level and execution mode, like every other
    recommendation. With the default `trust.level: observation` nothing runs unattended.
  - **Turning features off:** `llm.enabled: false` turns every LLM feature off and is a
    hot kill switch. Per feature: `llm.optimizer.enabled`, `advisor.enabled`,
    `tuner.llm_enabled`, `rca.narration_enabled` and `explain.enabled` (the whole EXPLAIN
    endpoint) set to `false`; in fleet mode, `databases[].llm_enabled: false` per
    database. The Settings LLM tab toggles `llm.enabled`, the advisor and the optimizer,
    and `PUT /api/v1/config/global` accepts every key above except `tuner.llm_enabled`,
    which is YAML-only.
- Upgrading keeps the autonomy your configuration already grants. On first start, the
  ledger carries over the custodian freeze and autovacuum tuning, the WAL bound, and
  load-admitted index creation at the level your trust and execution settings let them
  run unattended today (shown as "carried over" with the decision that granted it).
  Everything else starts at L1. Carried-over actions now also pause while an SLO burns,
  the node is not primary or was just failed over, the evidence is stale, another action
  touches the same object, or the family had a harmful outcome. They resume by themselves
  when that clears, and each change is recorded. An operator downgrade ends the carry-over:
  the level then has to be earned with evidence. `sre.autonomy.enforce: false` turns the
  ledger off entirely, and the sidecar warns at startup when you do. At startup, the
  schema upgrade adds the `sage.sre_family_autonomy`, `sre_autonomy_*`,
  `sre_packet_reviews`, `sre_eval_runs`, `sre_game_days` and `rollout_instance` tables,
  plus four nullable columns on `sage.rollout_run`.

- If you already run Sage SRE investigations with an LLM configured, they start using the
  model turn after the upgrade. To keep them deterministic, set `sre.llm.enabled: false`.
  At startup, the schema upgrade replaces the event-type check on `sage.sre_events` with
  `sre_events_event_type_m3`, which allows the three new event types. Existing rows are
  validated without a long table lock.
- `disk_full_slot` and `lock_storm` incidents are still not credited. pg_sage does not yet
  measure disk-fill trend or lock-storm recovery, so it makes no claim for them.

### Fixed

- **A connection leak beside another application's steady pool is now the root cause.** The
  steady pool tied with the leak and was named the cause. A pool that does not grow cannot
  explain pressure that does, so it is now listed as contributing (the baseline the leak
  grows on) and the leaking application is the root. A pool of the leaking application
  itself is still that leak. The graph version stays `causal-v3`. The replay case that
  exposed this is no longer held-out evidence for this shape; a fresh case with different
  numbers, outside the corpus, checks it.

- **Sage SRE no longer reports "no lock waits" when it cannot see them.** A database role
  without `pg_read_all_stats` (included in `pg_monitor`) sees other users' sessions without
  their state or wait events, so investigations reported no blocking, no long transactions
  and too few connections. Those probes now report "no privilege" and the investigation
  says the evidence is missing instead of guessing. Grant `pg_monitor` to the role pg_sage
  uses (the documented setup already does).
- **Sage SRE ignores evidence that is too old or contradictory.** An observation more than
  5 minutes older than the investigation's newest one (for example after an investigation
  was paused and resumed), or two connection samples taken at the same instant, are listed
  as missing evidence instead of supporting a root cause.
- **The new incident families follow the same rule.** LWLock waits, a standby's longest
  query, the statements spilling temp files, the sessions holding back the xmin horizon and
  the busy autovacuum workers also need `pg_read_all_stats`; without it those probes report
  "no privilege" instead of "no contention" or "no holder".
- **`forecaster.disk_capacity_bytes` no longer claims to auto-detect.** Nothing detects disk
  capacity (PostgreSQL cannot report free space over SQL), so `0`, the default, means
  undeclared: no disk runway and no disk-full credit. Set it for self-managed servers.

- Sage SRE connection and WAL investigations no longer fail when
  `sre.sample_interval_seconds` is set close to its maximum of 30. The worker did not
  renew its 30-second claim on an investigation while it waited between the two samples,
  so the claim ran out during the wait. The investigation was then retried and lost again
  until its time budget was used up, and it ended as `failed` (`budget_exhausted`). The
  worker now renews the claim during the wait. If an operator stops the investigation
  during the wait, the worker stops within seconds instead of waiting out the interval.
- **The tuner no longer fails per query without an LLM.** With `tuner.llm_enabled` set and
  no LLM configured (or the circuit open, or the daily budget spent), every tuning
  candidate fetched its plan context, logged "LLM prescribe failed" and wrote an
  `llm_suppression` finding, every cycle. It now uses its deterministic hints silently,
  and a budget refusal no longer suppresses LLM tuning for the query.
- **The deprecated `llm.index_optimizer` block honours explicit values.** It used to turn
  the optimizer on even when `llm.optimizer.enabled: false` was set. Now
  `llm.optimizer.enabled` wins whenever it is present, and an explicit
  `llm.index_optimizer.enabled` (true or false) applies only when it is not.
- The collector no longer reads `pg_settings` and every table's reloptions each cycle for
  a configuration advisor that cannot run (no usable LLM at startup).
- `pg_sage_optimizer_enabled` reports whether the index optimizer can run (enabled and its
  LLM configured), not just the config value, which is now `true` by default.

## v1.7.0 (2026-09-30) -- Sage SRE investigations, earned index autonomy

### What's new

- **Sage SRE investigations (opt-in: `sre.automatic_start: true`).** Incidents and plan
  regressions get a deterministic, evidence-first investigation. It uses 12 bounded
  read-only probes, a causal graph for lock blocking, plan regression, connection pressure
  and WAL/slot problems, and ruled-out alternatives. You can read investigations in the
  Cases panel, the API, MCP and a redacted export. Lock chains page within a minute. LLM
  narration is opt-in and must cite evidence.
- **pg_sage earns index autonomy.** It learns each database's IO baseline from Postgres
  itself (`pg_stat_io`/`pg_stat_wal`). After 7 days it builds the indexes it needs during
  quiet periods. Capacity declared by an operator overrides the learned baseline.
- **Durable recommendations.** Every actionable finding is a recommendation with immutable
  revisions. An approval pins one exact revision and executes once, even when
  operators race.
  Failed applies back off and are then abandoned, and verification verdicts are recorded.
- **The policy means what it says.** The refusal set, lock ceiling and serialization are
  enforced. There is one maintenance-window grammar, with timezones. Approvals are
  attributed and single-use. The emergency stop survives restarts and has a header
  control.
- **One runtime, one pipeline.** Standalone, fleet, meta-db and AgentDB build every database
  the same way, and every change goes through one `Executor.Apply` pipeline. AgentDB
  databases previously had no executor or policy gate at all.

### Added

- Startup logs a notice when `trust.maintenance_window` uses a form whose meaning
  changed with the unified window grammar (for example `weeknights`, a day window
  that crosses midnight, or cron ranges/steps), explaining the new meaning.
- **SSO account linking.** A user signed in with a password can link SSO from their account
  page (`#/profile`). Admins can unlink SSO, issue a one-time link grant (single use,
  15 minutes, stored hashed) for a user who cannot sign in with a password, and create
  SSO-only users with no password. Linking requires a verified provider email that matches
  the account; accounts are never linked on email alone. `GET /api/v1/users` now reports
  `sso_linked`, `sso_issuer` and `password_login`. Link, unlink and grant events are
  written to the new `sage.auth_audit` table. See `docs/security.md`.
- **Readable SSO sign-in errors.** A refused or failed SSO callback now returns the browser
  to the login page (or the account page for a link) with an explanation, instead of
  showing raw JSON. API clients still receive the JSON status.
- **Lock chains open incidents within a minute.** A lock-chain fast path runs every
  `rca.lock_chain_interval_seconds` (default `60`, `0` disables it) instead of waiting for the
  600 s analyzer cycle. It opens or updates the `lock_contention` incident and sends
  `incident_detected`. The incident records each root blocker's identity (pid,
  `backend_start`, query id and a hash of the query text) as structured `causal_chain`
  evidence. Escalation and auto-resolution still count analyzer cycles, so
  `escalation_cycles` and `resolution_cycles` keep their meaning.
- **Plan fingerprints.** Captured plans get a stable `plan_hash` that ignores costs, row
  counts, timings and literals. It is stored in `sage.explain_cache` and copied to every
  `sage.query_store` sample, so a plan flip shows up in the per-query history.
  `plan_regression` findings now say whether the plan shape changed (`plan_changed`).
- **Incident narration (opt-in).** With `rca.narration_enabled: true`, detected and escalated
  incident notifications carry an LLM summary. The model can only read the incident's own
  evidence, must cite it, and may only use numbers that appear in it. Every other case
  (narration off, LLM off, budget, rate limit, timeout, malformed or uncited output) sends
  the deterministic summary. Both are labeled in the notification. `llm.enabled: false`
  cancels narrations in flight.
- **Durable recommendations.** Every actionable finding is now a recommendation in
  `sage.recommendation`, with immutable revisions in `sage.recommendation_revision` (forward
  SQL, inverse SQL, evidence, preconditions, standing-policy version and a content hash) and
  its full history in `sage.recommendation_transition`. States: proposed, approved,
  applying, applied, verifying, then verified, reverted or inconclusive, plus superseded,
  failed and abandoned. Every change is a compare-and-set on (id, state, revision), so two
  workers can never both act on one recommendation. Two candidate indexes on one table are
  now two recommendations (C05). New read-only API: `GET /api/v1/recommendations?database=`
  and `GET /api/v1/recommendations/{id}` (revisions and history). The Actions page has a
  Recommendations tab, and pending approvals show the revision they approve.
- **Sage SRE investigations (opt-in, read-only).** With `sre.automatic_start: true`, each
  open RCA incident of the lock, connection or WAL family and each open `plan_regression`
  finding gets one investigation. It runs its family's fixed catalog probes (connection and
  WAL investigations sample twice, `sre.sample_interval_seconds` apart), matches the causal
  graph and stores the diagnosis: likely explanation, contributing factors, alternatives and
  ruled-out explanations, each citing its evidence, plus missing evidence and an operator
  step. No LLM is used and nothing is executed; "inconclusive" is a normal outcome. Every
  mode (standalone, YAML fleet, meta-db, AgentDB) runs it. A second worker can never commit
  over the first, and a crash resumes at the next step. Every change to an investigation is
  recorded in an append-only hash chain (`sage.sre_events`).
- **New causal families.** Connection pressure: pool fan-out, blocked-query backlog and
  connection leak, with the share of usable connections stated separately. WAL retention:
  inactive slot, slow slot consumer, archiver failure and write surge; a surge is reported
  as contributing to slot retention, never instead of it, and remaining disk space is always
  reported as unknown. Every diagnosis also answers "did pg_sage cause this?" from pg_sage's
  own actions in the last hour. New probes: `archiver` and `sage_actions`.
- **Cases panel investigations.** A case shows its investigation: state (inconclusive is
  never shown as resolved), likely explanation with a score and evidence-strength label,
  other and ruled-out explanations with their reasons, missing evidence, next check, and
  evidence items that open from each claim. Operators and admins can pin an investigation
  (retention keeps it) and export it as JSON or Markdown.
- **Investigation API and MCP tools.** `GET /api/v1/investigations?database=` and per
  database `GET /api/v1/databases/{db}/investigations[/{id}[/events|/evidence/{id}]]` for
  every signed-in role; `GET .../{id}/export` and `POST .../{id}/pin|unpin` for operators and
  admins. MCP adds read-only `sre_list_incidents`, `sre_get_investigation` and
  `sre_get_evidence`. Everything leaving the store is redacted (connection URIs,
  credentials, bearer tokens, SQL literals, raw vectors). See "Sage SRE investigations" in
  `docs/configuration.md`.
- **Investigation retention.** Finished, unpinned investigations lose their evidence after
  `sre.evidence_retention_days` (30) and are deleted after `sre.timeline_retention_days`
  (90). Each delete leaves a tombstone in `sage.sre_tombstones`; a conclusion whose evidence
  is gone is shown as such.
- **PGIncidentBench seed** (`sidecar/sre-bench`): 20 fault programs that create real lock,
  connection, WAL and plan incidents on PostgreSQL, run them through the investigator and
  report precision, recall, top-1 and abstention per family. On PostgreSQL 17 the seed set
  scores 100% on every metric (20 scenarios; an in-distribution seed, not a held-out
  measurement).

### Changed (read before upgrading)

- **The executor acts on durable recommendations, not the last analyzer cycle (C07).**
  A recommendation proposed while pg_sage was observing, or outside the maintenance window,
  now runs once policy allows it, without waiting for a new analyzer finding. Before acting
  the executor checks that the recommendation has not changed and that its finding is still
  open; a closed finding supersedes it.
- **An approval approves one exact revision (C04).** New content (for example new inverse
  SQL) is a new revision: it clears the approval and supersedes queued proposals for the old
  content. Approving such a proposal returns `409 Conflict` and nothing runs; approve the
  current revision instead.
- **Failed applies back off and are abandoned when the retry budget is spent.** A failed
  apply is recorded as `failed` with a backoff (5 minutes, doubling, at most 6 hours). It
  never silently returns to proposed. After the retry budget (two retries) it is `abandoned`
  with the reason. An apply interrupted by a crash is recovered as a failed attempt once its
  claim expires. The action log row and the applied transition are written in one
  transaction.
- **Verification verdicts are recorded apart from their effect (C15).** A recommendation
  whose change regressed shows the verdict `regressed` but stays `verifying` until the
  revert has run; only then is it `reverted`.
- **Upgrade migrates existing work.** On start, each database's open findings with SQL and
  its pending or approved (not yet executed) queued actions are mapped to recommendations.
  An approval keeps its approver and is pinned to a revision holding the exact queued SQL
  (marked `migrated`). The migration is idempotent. Retention purges terminal
  recommendations after `retention.actions_days` and keeps live ones.
- `executor.New` no longer takes an analyzer argument.

- **Emergency stop in the header (D8).** Operators and admins get a Stop control next to
  the database picker. It stops the selected database, or all databases when "All" is
  selected, and needs arm then confirm. Resume appears only when the selection is stopped,
  also needs confirmation, and shows who stopped it and when. A badge visible to every role
  names the stopped database, the person who stopped it and the time. The Settings page
  buttons are replaced by a pointer to the header control.
- **The stop survives restarts.** At startup and on reconnect, every mode (standalone,
  fleet, meta-db) restores the in-memory stop from `sage.config`, so the header badge and
  action readiness show "stopped" on first paint. An unreadable flag restores as stopped,
  attributed to `system`.
- **Attribution and audit.** Stop and resume record the signed-in user (or `system`) in
  `sage.config.updated_by` and append a `sage.config_audit` row in the same statement. The
  fleet API (`GET /api/v1/databases`) exposes `emergency_stopped`, `emergency_stopped_by`
  and `emergency_stopped_at` per database, and `summary.emergency_stopped_count`.
- **Declared IO capacity.** Operators can attest provisioned throughput with
  `verify.io_capacity` (standalone) or `databases[].verify.io_capacity` (fleet):
  `read_write_mbps` and `wal_mbps` in MiB/s. Utilization is the measured rate
  divided by the declared capacity, compared with the IO ceilings. A declaration
  overrides the learned baseline. A fleet-wide `verify.io_capacity` is rejected.
- **Admission evidence in the ledger.** Every decision records which evidence
  mode admitted or withheld the build (`declared_capacity`, `learned_baseline`
  or `unavailable`), with the rates, capacity or baseline, in
  `sage.decision.evidence.load_admission`.
- **Visible withheld reason.** A withheld build is recorded once per finding and
  reason in `sage.admission_withheld` instead of a new failed action every
  cycle. `GET /api/v1/admission` and `GET /api/v1/admission/{name}` report each
  database's mode, reason and baseline progress, and the dashboard shows them on
  the Actions page and in the Overview provider-readiness tab.
- Supabase provider observability now supplies host CPU only; it never reported
  disk utilization.
- **Diagnostic probe catalog (Sage SRE).** Twelve fixed, read-only probes: lock chains and
  the lock wait graph, long and idle-in-transaction transactions, prepared transactions,
  backend identity, connection saturation, replication lag, replication slots,
  WAL/checkpoint, autovacuum/wraparound, vacuum progress and plan regressions. Each runs in
  a read-only transaction with a fixed `search_path`, `statement_timeout` 500 ms and
  `lock_timeout` 100 ms, returns at most 500 rows and 256 KiB, and at most one probe runs
  per database and four per sidecar at a time. Results are typed (`ok`, `empty`, `error`,
  `no_privilege`, `unsupported`), so a missing privilege, extension or table is never
  reported as healthy. Probes return identities, states, counts and ages, never query text.
- **Causal graph for lock blocking and plan regressions.** A deterministic matcher (no LLM)
  tells an idle-in-transaction holder, DDL queued behind a long transaction, hot-row
  contention and a prepared-transaction holder apart, and for a slow query a `plan_hash`
  flip from a slowdown on the same plan. Each hypothesis carries its evidence, a confidence,
  a refutation probe, and the alternatives it ruled out with the evidence that ruled them
  out. A cleared chain, a wait cycle or missing evidence gives "inconclusive", not a guess.
- **Lock incident notifications name the likely cause.** The deterministic summary of a
  `lock_contention` notification adds, for example, `Likely (H1, confidence 0.85):
  idle-in-transaction holder, pid 4242; contributing (H2): DDL queued behind a long
  transaction`, and names probes that could not run (`Missing: lock_graph
  (no_privilege)`). With `rca.narration_enabled`, the model can also read the probe results
  (`get_probe_result`) and hypotheses. It answers with claims, each citing evidence ids, and
  every number in a claim must appear in the evidence that claim cites.
- **Investigation store foundation.** Durable investigations with
  leases and fence tokens (a stale worker cannot commit), idempotent steps, immutable hashed
  probe evidence, and durable per-investigation model budget reservations (2 turns, 16k input
  and 4k output tokens, plus database and deployment daily allocations) that a crash cannot
  reset. A metadata outage puts coordination in an explicit degraded state that blocks
  action handoff.

- **Schema (Sage SRE M2).** Added automatically at startup: `sage.sre_hypotheses`,
  `sage.sre_events` (UPDATE is refused by a trigger, as it now is on `sage.sre_evidence`),
  `sage.sre_tombstones`, and new columns on `sage.sre_investigations` (`source_incident_id`,
  `subject`, `pinned`, `summary`, `concluded_at`, `evidence_purged_at`).
- The `connection_saturation` probe is now `v2` and also returns the server start time.
- RCA narration and the investigator now share one probe runner per database, so at most
  one catalog probe runs on a database at a time across both.
- An investigation whose active-time budget a dead worker used up now ends as `failed`
  (`budget_exhausted`) instead of staying `collecting`.
- Stored probe evidence keeps 64-bit integers exact; a `queryid` above 2^53 was rounded.

- **Every mode builds a database's runtime the same way.** Standalone, YAML fleet,
  meta-db and AgentDB now share one constructor, so the safety wiring (standing policy
  gate, emergency stop, managed-config adapter, trust and executor gates, notifications)
  is identical. What operators will notice:
  - **AgentDB databases** get the full runtime instead of a collector only: the sage
    schema is bootstrapped into the agent database, and findings, notifications,
    retention and actions follow the standing policy gate and a fresh trust ramp.
  - **Meta-db databases** gain the forecaster, auto_explain plan collection, schema lint,
    the migration advisor, log-based RCA, hint revalidation and the rule-based tuner
    without an LLM. The config advisor targets the PostgreSQL database name rather than
    the instance name.
  - **YAML fleet and meta-db:** the index optimizer uses auto_explain plans when the
    extension is available, and databases without an LLM get the scheduled briefing.
  - **Standalone:** the config advisor applies provider-specific rewrites.
  - A YAML fleet database whose schema bootstrap fails is shown as failed instead of
    running without its schema.
  - Shutdown waits, within its deadline, for every database's workers.
  - `--meta-db` with `mode: standalone` no longer registers a phantom database.
  - AgentDB databases also get index-build load admission (D6), the Sage SRE fast path
    and catalog probes, value-ledger reporting (D3) and the restored emergency stop (D8).
- **One execution pipeline.** Every change the executor makes (the background cycle,
  operator-approved actions, custodians, verified indexes and retention authorization)
  goes through the same steps: authorize, take the change lease and a DDL slot,
  re-authorize, then run under a deadline and verify.
  - **Operator-approved actions are re-authorized by the standing gate** right before
    they run, so an emergency stop or policy change made after the approval stops them.
  - **Executor DDL always has a timeout.** `safety.ddl_timeout_seconds: 0` used to mean
    no statement timeout; it now means the 300-second default. A client-side deadline one
    minute longer backs it up.
- **Cloud AgentDB registration needs an approved request.** `POST /api/v1/agent-dbs`
  with a cloud provider now returns `409 approved request required` unless it names an
  approved, unused `request_id`. Provision approved requests with
  `POST /api/v1/agent-dbs/requests/{id}/provision`, which now accepts `size_profile_id`,
  `schema_name`, `secret_ref` and `secret_ref_provider`. Each approval produces exactly one
  deployment; reusing it, or provisioning it for another tenant, agent or provider, returns
  `409`. `local_postgres` schema and database registers are unchanged. Existing deployments
  are left as they are. The dashboard's Provision form uses the request route.
- **Blueprint and Terraform-template provisioning also consume a request.** An approved
  blueprint or template is a reviewed design, not permission to spend: provisioning a cloud
  plan from one now needs a signed-in user and an approved, unused `request_id` matching its
  tenant, agent and provider, and returns `409` without one or on reuse. The dashboard
  requests approval before provisioning from the Blueprints and Terraform panels.
- **AgentDB approvals are attributed.** Approve and deny record the signed-in user in
  `decided_by`/`decided_at` and require a session. A consumed request records
  `consumed_deployment_id`, `consumed_by` and `consumed_at`, and its decision can no longer
  change. Decisions that request policy made at creation show `decided_by: "policy"`.
- **One operator team per install.** AgentDB operators and admins act on every tenant; the
  docs now say so. Do not share an install between teams that must be isolated.
- **Retention deletes need an owner-declared column (D5).** `declare_table_contract`
  now takes `retention: {"interval": "...", "column": "..."}`; an interval without a
  column (including the old bare-string form) is rejected, and the column must exist
  and be `timestamptz`, `timestamp` or `date`. pg_sage no longer guesses `created_at` or
  `occurred_at`. **Existing retention contracts pause** (no deletes) until they are
  re-declared with a column; the ledger park reason shows the suggested column, e.g.
  `retention column not declared; suggested: created_at — re-declare the contract with
  retention.column to enable deletes`. Count affected contracts with
  `SELECT count(*) FROM sage.table_contract WHERE append_only AND retention_interval IS
  NOT NULL AND retention_column IS NULL`. The upgrade adds
  `sage.table_contract.retention_column` without backfilling it.
- **Dry runs are bound to identity, not names.** A reviewed dry run now authorizes
  deletion only for the same table OID, column attnum and type, and contract version.
  Renaming or swapping the column, rebuilding the table, re-declaring the contract, or
  eligible rows growing past 2x + 100 of the dry run's count starts a new 24-hour
  review. Dry runs recorded before this release no longer qualify. Each delete batch
  locks the table `ROW EXCLUSIVE` and re-checks the column before deleting.
- **Value is counted across the whole fleet (D3).** Each monitored database keeps its own
  value ledger, next to the actions it credits. The Value page (`GET /api/v1/value`), the
  Prometheus value metrics and the MCP `get_value` tool now add up every database in
  standalone, YAML fleet and meta-db mode. Before, YAML fleet showed only the first database
  and meta-db mode showed zero, because the meta database holds no ledger. Nothing is
  migrated.
- **Value metrics carry the database name.** `pg_sage_toil_minutes_saved` and
  `pg_sage_incidents_avoided_total` are labelled `database="<instance name>"` (never empty).
  `pg_sage_value_metrics_up` is now reported per database, so update alerts that expect the
  unlabelled series. The value metrics are also exported in YAML fleet mode, where they were
  missing.
- **Partial value is reported, not hidden.** If a database cannot be read, the value response
  sets `"partial": true` and names it in `"unavailable"`, and the other databases still count.
  The Value page shows a warning. `?database=<name>` works in every mode, including
  standalone, and an unknown name returns 404. Two fleet entries for the same physical
  database are counted once.
- **Credited value is kept.** Retention no longer purges credited actions (verified success
  with toil credit) or the verification that earned the credit, so all-time value no longer
  shrinks after `retention.actions_days`. Uncredited, failed and rolled-back rows still age
  out.
- Schema: `sage.explain_cache` gains a nullable `plan_hash` column (added automatically at
  startup).
- Notifications for `incident_detected` and `incident_escalated` now end with a labeled
  `Summary` line, and their payload data adds `narrative` and `narrative_source`.

- **Defined stop/resume races.** Stops latch memory immediately; persisted transitions are
  serialized. A stop that overlaps a resume wins, and memory always ends equal to the
  persisted flag. A stop that lands while a meta-db reconnect swaps in a new runtime
  carries over to the new runtime and is saved. `sage.config_audit` gains a nullable `changed_by_actor` column, added
  automatically at startup.
- **Behaviour change: autonomous index builds can now be admitted.** Until now no
  source produced IO evidence, so load admission withheld every autonomous
  `CREATE INDEX CONCURRENTLY` and every custodian index proposal (such as FK
  supporting indexes) on every deployment. pg_sage now samples IO from Postgres
  every minute (`pg_stat_io` on PG16+, `pg_stat_database` and `pg_stat_bgwriter`
  on PG14/15, `pg_stat_wal` for WAL) and learns each database's baseline in
  `sage.io_rate_sample`. **After 7 days of observation (`verify.io_baseline_days`),
  autonomous index builds are admitted during quiet periods**: when current data
  and WAL rates are at or below the learned median. While the baseline is still
  being learned, builds stay withheld with a reason such as
  `learning IO baseline: 3.0/7 days`. Set `verify.io_baseline_days: 0` to keep
  today's fail-closed behaviour. The trust level, tier flags, standing policy and
  maintenance windows still apply as before.
- **Unknown host CPU narrows admission to maintenance windows.** Without a CPU
  reader (anything except Supabase with an observability token), a build is
  admitted only while `trust.maintenance_window` and the policy windows are open.
- **Separate IO ceilings.** `safety.data_io_ceiling_pct` and
  `safety.wal_io_ceiling_pct` (default 70) no longer inherit
  `safety.cpu_ceiling_pct`.
- **The policy refusal set is enforced.** `refusal_set` was stored but never
  checked. Each token now matches precisely: `rls_change` (row-level security
  or its policies), `grant_expansion` (GRANT, ALTER ROLE, OWNER TO, ALTER
  DEFAULT PRIVILEGES), `major_upgrade` (major or extension upgrades),
  `non_dup_object_drop` (dropping a table, column, constraint, sequence,
  schema or replication slot) and `unrollbackable` (rollback class
  `not_reversible` or `forward_fix_only`). A self-initiated action that
  matches is queued for approval with reason `refused_by_policy`, and the
  token is in the detail. An operator-approved action is not refused.
  - No action pg_sage takes today changes verdict under the built-in
    profiles. Unused, duplicate and invalid index drops are rebuildable and
    keep their earned autonomy. A retention delete on the column the owner
    declared as the contract's `retention.column` (D5) runs under that
    declaration and is not "unrollbackable"; one without a declared column
    is queued for approval.
  - `set_table_autovacuum` is now reversible: it ships `ALTER TABLE ... RESET`.
  - Unknown refusal tokens are rejected when a policy is proposed. A stored
    policy that contains one fails closed (`policy_unavailable`), and startup
    logs which token to fix.
- **The policy lock ceiling is enforced.** For DDL that runs inside a
  transaction, autonomous or operator-approved, `lock_timeout` is the smaller
  of `lock_duration_ceiling_ms` (3000 in both profiles) and
  `safety.lock_timeout_ms` (default 30000). An `ALTER TABLE` queued behind a
  long lock now gives up after 3 s instead of 30 s. `CONCURRENTLY` builds keep
  `safety.lock_timeout_ms`, because they wait on older transactions by design.
  The ceiling also caps `ANALYZE` (autonomous and operator-approved) and
  in-transaction custodian changes.
- **DDL lease conflicts park instead of failing.** When another writer holds the
  change lease for the same object, the action is parked (`ddl_conflict` in the
  decision ledger) and retried next cycle. It used to log a failed action, which
  counted toward the three-failure abandonment and the self-initiated rate
  limit.
- **One maintenance-window grammar.** `trust.maintenance_window` and the
  standing policy's `maintenance_windows` are now parsed by the same engine, so
  a string means the same thing in both. See "Maintenance windows" in
  `docs/configuration.md`.
  - Config windows gain cron lists, ranges and steps (`0 2 * * 1-5`,
    `*/15 2 * * *`). These used to be accepted and silently mean "never".
  - Policy windows gain the config presets (`nights`, `weeknights`,
    `weekdays`, `business-hours`, ...) and day lists (`Mon-Fri`, `sat,sun`,
    `daily`).
  - A cron window is one hour wide from each matching minute, or as long as
    an optional `@<duration>` says (`0 2 * * * @30m`, `@1m` to `@24h`).
    `30 * * * *` therefore covers every hour; it used to cover only :30-:59.
  - A range that crosses midnight belongs to the day it starts on.
    `weeknights` now covers Friday 22:00 to Saturday 06:00 and no longer
    covers Sunday 22:00 to Monday 06:00. Lists behave the same way:
    `Mon,Wed,Fri 22:00-04:00` covers Saturday 02:00 (Friday night), not
    Wednesday 02:00 (Tuesday night).
  - An optional trailing IANA time zone evaluates a window on that zone's
    clock, including DST: `weekdays 01:00-05:00 America/Chicago`. Without one
    the process clock is used, as before (UTC in the container).
- **Invalid `trust.maintenance_window` values are rejected** at config load,
  file reload and API save (HTTP 400 naming the grammar). A typo such as
  `weeknigths` used to be stored and silently close the window. `never`,
  `off`, `none` and `disabled` still close it.
  An override saved before this release that no longer validates is logged at
  startup with its value and how to fix or delete it. It still keeps the
  window closed until it is fixed.
- **Stored policies are migrated to schema version 3.** Each policy cron window
  is rewritten from `<cron>` to `<cron> @1m`, so it keeps its exact
  one-minute meaning. The built-in profiles have no cron windows and are
  unchanged. Superseded history is not rewritten.
- **Downgrade risk.** A release older than this one cannot parse `@<duration>`
  or a time-zone suffix in a policy window. If the active policy contains one
  (including a migrated `@1m`), an older binary fails closed with
  `policy_unavailable` and blocks every action until a policy it can parse is
  active. Before downgrading, ratify a policy version without those suffixes.
- Schema: new tables `sage.sre_deployments`, `sage.sre_database_bindings`,
  `sage.sre_investigations`, `sage.sre_steps`, `sage.sre_evidence` and
  `sage.sre_budget_reservations` (added automatically at startup, in the meta database when
  one is configured). Nothing writes them yet; retention for them lands with the
  investigator.

- An executor that could not get a database connection to take its DDL lease
  (pool exhausted by the change it conflicts with) waited until the other
  change's lock timeout, which then failed it. Taking a lease now waits at most
  5 s; a busy pool parks the action like a lease conflict (retried next cycle,
  never a failed action), and recording the park is bounded the same way.
- A refreshed recommendation could pair new forward SQL with the previous rollback SQL and
  still be approved and run as the old approval (C04). Revisions are now atomic and an
  approval covers exactly one of them.

- A retention contract that cannot act no longer stops the schema scan: later
  invariants (such as missing foreign-key indexes) are still planned in the same cycle.
- Re-declaring a table contract updates the one contract row instead of adding another.
  The upgrade removes existing duplicates (keeping the newest per table) and adds a
  unique index on `(COALESCE(database_id, 0), schema_name, table_name)`.
- A dry run still in its 24-hour review parks with `retention dry run in review until
  <time>` instead of being logged as a failure; the rest of the scan continues.
- In fleet mode, and with several databases on one server, a database reported and paged on
  lock chains that belonged to another database. Lock-chain detection now only starts from
  sessions in the monitored database.
- Incident causal chains that contained control characters or invalid UTF-8 failed to
  save.
- The lock-chain fast path started on the first analyzer cycle that had a snapshot. The
  first cycle usually runs before the collector's first snapshot, so after startup a lock
  chain could take a whole analyzer interval (default 600 s) to page. The fast path now starts
  when the database is registered, and instance shutdown waits for it instead of closing its
  pool under it.

### Deprecated

- **`tuner.analyze_maintenance_threshold_mb` is ignored.** It never had an
  effect. A config file that still sets it loads normally and logs
  "tuner.analyze_maintenance_threshold_mb is no longer used and is ignored;
  remove it". The key is gone from the example configs.

### Fixed

- An operator execution of an approved recommendation could run twice: a second
  call that looked up the recommendation after the first had claimed it found
  nothing claimable and ran the SQL unclaimed. An in-flight or applied
  recommendation for the same finding and SQL now refuses with a conflict.
- Schema lint `lint_mxid_age` computed MultiXact age with the transaction-ID
  `age()` function, reporting a bogus ~2.1B "approaching wraparound" critical
  finding once MultiXact IDs outpaced XIDs. It now uses `mxid_age()`.

## v1.6.0 (2026-09-27) -- Safety gate, SQL parse-tree validation, Azure

### What's new

- **Azure Database for PostgreSQL.** Flexible server runs every portable maintenance
  action. Server parameters are applied through Azure Resource Manager and reported as
  `applied_pending_restart` when a restart is needed. Credentials come only from the Azure
  identity chain (service principal, workload or managed identity, or `az login`). Existing
  Cosmos DB for PostgreSQL clusters are detected, with parameters guidance-only.
  Verified live on flexible server PostgreSQL 14–18, the General Purpose tier and elastic
  clusters.
- **SQL parse-tree validation.** Every statement pg_sage executes is parsed by PostgreSQL's
  own parser (libpg_query) and checked by structure, as a second layer after the text
  validator. It catches quoted and Unicode-escaped schema names, set operations and multiple
  `ALTER TABLE` subcommands. Release binaries and Docker images always include it;
  `pg_sage --version` reports `sql-ast:`.
- **One policy authority.** The standing policy gate now decides every action: unattended,
  operator-approved, and what the dashboard shows as ready. Operator approvals obey the
  policy's change-class allowlist and maintenance windows. Readiness shows the real verdict
  instead of assuming a satisfied trust ramp.
- **Azure test scripts** in `scripts/azure/` provision a throwaway server, run the live
  checklist, run a flavor matrix and tear everything down.
- **CI** runs the race detector, a PostgreSQL 14–18 matrix with pg_hint_plan, a skip
  budget, and the documented quick start as a least-privileged role.

### Changed (read before upgrading)

- **Stricter trust gate.**
  - Safe actions need `tier3_safe` plus 8 ramp days.
  - Moderate actions need `tier3_moderate`, 31 days, and an open policy window and
    `trust.maintenance_window`; otherwise they queue.
  - `alter_table`, `reindex_concurrently`, backend cancel/terminate and `apply_query_hint`
    always need approval.
- **Policy documents are migrated automatically.** The new change classes `backend_signal`,
  `query_hint` and `schema_change` are granted where `index` was, so effective permissions
  are unchanged. Policy history is not rewritten.
- **Builds without a C compiler** leave out parse-tree validation. They log a warning and
  send unattended changes to operator approval (`sql_validation_degraded`).
- **The C extension is removed.** `mode: extension` fails at startup. The default mode is
  standalone, and `--meta-db` implies meta. The last extension source is at tag
  `c-extension-final`.
- **Meta-db trust:** the global `trust.level` is a ceiling for every database.
- **OIDC** requires `email_verified` and matches issuer + subject. Password accounts are
  refused at SSO login until account linking exists.
- **Notifications:**
  - Private and metadata targets are refused unless
    `notification_policy.allow_private_targets: true` (YAML only).
  - Channel secrets are encrypted at rest.
  - Critical alerts bypass quiet hours; others are deferred, not dropped.
- **HTTP MCP:** viewers get read-only tools, and the acting user is recorded.
- **Retention deletes** need a matching dry run 24 hours to 7 days old and go through the
  policy gate.
- **Migrations** never auto-promote until workload capture exists.
- **Metrics:** `pg_sage_toil_minutes_saved_total` is replaced by the gauge
  `pg_sage_toil_minutes_saved`. Added `pg_sage_value_metrics_up` and per-database LLM
  budget metrics.
- **AgentDB:**
  - Agent API tokens are tenant-bound.
  - `secret_ref` must be `env:PG_SAGE_AGENTDB_*`.
  - Local provisioning needs `PG_SAGE_AGENTDB_LOCAL_PROVISIONING`.
- **Building from source** needs a C compiler for parse-tree validation. Release builds use
  cgo: static Linux amd64/arm64 and macOS amd64/arm64.

### Fixed

- **SQL safety**
  - EXPLAIN statement escape.
  - Executor whitelist bypass through multiple `ALTER` subcommands and unusual whitespace.
  - DDL literals and role/subscription DDL are no longer sent to the LLM.
- **Retention:** deletes are bound to one partition and re-check the cutoff; the delete and
  its audit record commit together.
- **Undo and rollback**
  - Reverting an index created with `IF NOT EXISTS` could drop a pre-existing index; revert
    now drops only an index pg_sage recorded creating.
  - Index rollbacks are concurrent.
  - Pending reverts resume after a crash.
  - Rollback by queue id is fixed.
- **Backends and WAL**
  - Backend cancel re-checks the backend's identity, so a reused PID is never cancelled.
  - The WAL custodian no longer shrinks retention below what replication slots hold.
- **LLM configuration advice** uses the correct base unit for memory settings.
- **Emergency stop** halts every database in a fleet even when persisting the stop fails.
- **PostgreSQL versions**
  - HypoPG validation works on PG14/15.
  - PG18 plans parse fractional `Actual Rows`.
  - pg_hint_plan versions before 1.7 are handled.
  - The documented quick start works as written.
- **Azure:** memory-valued server parameters reported in `kB`/`8kB` convert correctly
  (found by the live matrix).
- **AgentDB:** ping is liveness-only; archived deployments are revisited; re-registration is
  idempotent; teardown requires the recorded resource id and verified cloud tags.

### Verification and limitations

- Tested under cgo:
  - Unit: 7,608 passed, 0 failed.
  - e2e: 76 passed.
  - Integration: race-enabled on PostgreSQL 14–18.
  - Every skip justified in the skip budget.
  - golangci-lint: clean.
- Azure live matrix: flexible server PG14–18 (Burstable and General Purpose) and elastic
  clusters pass every automated check. PG11–13 are refused at startup by design (minimum is
  14). New Cosmos DB for PostgreSQL clusters can no longer be created, so Cosmos support is
  covered by unit tests only.
- Dashboard readiness and approved-action checks on Azure (CHECK-AZ-04/05) are still
  manual.

## v1.5.0 (2026-09-07) -- Neon and Supabase

### Added

- Neon and Supabase provider detection, runtime extension/permission discovery,
  portable maintenance actions, and database-scoped setting recommendations.
- AgentDB project/branch lifecycle runners with ownership, entitlement, expiry,
  asynchronous status, and protected-resource checks; provider-specific profiles,
  dashboard fields, and validated Terraform templates.
- Supabase management log and metrics ingestion, including actual auto_explain
  plans, deduplicated query-store records, RCA input, and CPU/memory evidence.
- Environment secret references for AgentDB fleet connections, preserving TLS
  and session options while validating resource endpoint and database identity.
- Vector Evidence Lab with bounded, read-only HNSW experiments and measured
  recall, latency, plan, and resource-admission evidence.
- Live provider regression harnesses for direct/session connections, synthetic
  maintenance and vector workloads, lifecycle cleanup, and logical backup/restore.

### Fixed

- Keep HypoPG evaluation and cleanup on one physical session, discover extension
  namespaces, and support normalized EXPLAIN workloads without pgx bind errors.
- Prefer actual observed execution plans over recaptured legacy plans; distinguish
  installed extensions from loaded/enabled modules and recognize provider-loaded
  auto_explain when LOAD is restricted.
- Report malformed provider logs without stalling valid records; preserve retryable
  storage errors and unknown/stale host-load evidence.
- Preserve explicit inverse database settings during rollback and keep unsupported
  instance settings advisory instead of issuing ALTER SYSTEM to hosted providers.
- Refresh per-database fleet version, size, and collection timestamps; preserve
  observation/manual mode and unknown capabilities in dashboard views.
- Preserve unverified backup-assurance status and reject Terraform mode errors
  or unconfigured sizing/version/extension claims rather than silently accepting them.
- Serialize AgentDB metadata initialization, preserve superseded indexes for
  reviewed cleanup, and harden backend lifecycle, local launcher, and test fixtures.

### Changed

- Reject Neon pooled and Supabase transaction-pooler endpoints for sidecar sessions;
  use Neon direct or Supabase direct/session connections.
- Refresh dependency updates and embedded dashboard assets. CI verifies frontend,
  Go unit/integration/e2e layers and installs the required pgvector/HypoPG capabilities.

### Verification and limitations

- Live Neon and Supabase direct checks and Supabase session checks passed for the
  covered SQL, HypoPG, and vector workflows. Synthetic logical exports restored
  rows, vector hashes, indexes, constraints, and identity state into fresh databases.
- Neon branch and Supabase free project create/status/delete were verified live;
  temporary API credentials were revoked after testing.
- Free-plan entitlements and unavailable hints remain explicit. In the tested
  projects, Neon hints could not be enabled and Supabase did not offer pg_hint_plan.
  Supabase Free branch/managed-backup and Neon Free log-export limits remain.
- Missing data/WAL utilization blocks actions requiring complete host-load evidence.
  Neon exhausted the test project's free transfer quota before the final dashboard
  repeat; earlier live checks passed. No paid upgrade was performed.
- Native snapshot rehearsal and automatic restore drills remain existing product
  gaps; logical restore verification does not claim those workflows or managed PITR.

See [Neon and Supabase setup](docs/neon-supabase.md) for connection requirements,
configuration, and the verified feature boundaries.

## v1.4.0 (2026-07-23) -- Agent-Native Autonomy

### Added

- Standing-policy control plane (`sage.policy`): a declarative, validated
  policy object is now the sole source of autonomous authority, with
  `staffed` and `unattended` profiles, budget semantics where `0` means
  "none" (never "unlimited"), fail-closed unknown classification, and
  operator-gated propose/dry-run/ratify policy changes.
- Single authorization gate: every autonomous mutation routes through
  `policy.Gate` with fail-closed ordering, enforced guardrails, DDL
  change-leases (advisory locks + `sage.change_lease`), and drift
  reconciliation against `sage.schema_baseline`.
- Evidence Ledger (`sage.decision`) recording every parked, recommended,
  blocked, and executed decision, with a self-audit that re-opens
  decisions when past fixes regress.
- Verify-and-revert engine: per-query verification with no-gain,
  regression, write-impact, and invalid-index reverts; adaptive windows
  that extend then revert-to-safe on insufficient samples; a low-load
  apply gate; and the coupling rule that nothing auto-applies unless it
  can be auto-verified.
- Verified index lifecycle: candidate generation, HypoPG pre-estimates,
  concurrent builds under lease, and post-apply verification with
  automatic revert.
- Wraparound/bloat custodian and replication-slot/WAL guardian with
  deterministic deadline math, graduated urgency, policy-gated deadline
  overrides, slot bounding before dropping, and a consumer registry that
  protects declared slots.
- Rehearse-on-clone migration gate: lint-and-rewrite of hazardous DDL,
  expand/contract planning, rehearsal on thin clones, bounded auto-revert
  windows, and recommend-only degradation when no clone provider exists.
- Pluggable `clone.Provider` with Database Lab Engine and
  snapshot-restore adapters, including snapshot-freshness enforcement.
- Agent-native schema custodian: invariant catalog (FK-without-index,
  unbounded append tables, missing constraints), table contracts declared
  over MCP, dry-run-first retention, and structural changes that never
  auto-apply.
- Intent-level MCP server (stdio JSON-RPC) whose every tool routes
  through the same standing-policy gate; caller claims can never widen
  authority.
- DBA-hours-saved value model: table-driven `sage.toil_model`,
  conservative `sage.incident_avoided` credit, verified-success-only
  stamping with zero credit on revert, the `/api/v1/value` endpoint, and
  Prometheus gauges `pg_sage_toil_minutes_saved_total` and
  `pg_sage_incidents_avoided_total`.
- Value view as the dashboard's default landing page, with telemetry
  moved to a secondary Advanced view.
- Fleet staged-rollout scaffolding for cohort canaries with per-instance
  re-verification.

### Changed

- Replaced testify with a stdlib-only internal assertion library
  (`internal/testsupport`), keeping the repo's no-testify convention.
- DB-dependent tests now skip with an explicit reason when
  `SAGE_TEST_DATABASE_URL` is not set instead of failing against a
  sentinel address; with a database configured nothing skips.
- Notification and config API tests are hardened against full-suite DB
  contention (stable cleanup sweeps and bounded retries on transient
  timeout signatures only).

## v1.3.1 (2026-07-22) -- Correctness and Safety Audit Follow-up

### Fixed

- Restricted LLM model discovery to administrators and blocked SSRF through
  private, loopback, metadata, redirected, or DNS-rebound provider endpoints.
- Scoped LLM cooldowns to logical work items, counted provider timeouts toward
  circuit health, and prevented local admission throttles from poisoning
  optimizer table circuit breakers.
- Enforced atomic per-database token budgets across optimizer, advisor, tuner,
  briefing, RCA, narration, justification, schema lint, and migration clients.
- Restored fleet health-history samples, initialized fleet and meta-database
  ANALYZE concurrency limits, and made AgentDB collectors process-owned and
  drainable instead of inheriting a short reconciliation context.
- Initialized the general LLM runtime in meta-database mode and wired scoped
  LLM clients through every per-database consumer.
- Made executor policy reads race-free during configuration hot reload.
- Corrected cron maintenance windows to honor month, day-of-month, and
  day-of-week semantics before autonomous moderate-risk maintenance.
- Applied transaction-local statement and lock timeouts to collector catalog
  reads, including early-error cleanup of pooled transactions.
- Added safe on-demand EXPLAIN capture for normalized parameterized queries.
- Clarified that interactive ReAct diagnosis belongs to the frozen C extension,
  not the Go sidecar, and made generated lifecycle documentation byte-stable.

## v1.3 (2026-07-19) -- Correctness and Safety Remediation

### Fixed

- Corrected the v1.2 execution-gate coupling: `execution_mode: manual` now
  disables all background queueing and execution at every trust level. Trust
  is an independent ceiling and never promotes manual mode to auto.
- Unified live execution, approval queueing, Cases policy, and fleet readiness
  on typed action contracts. Per-database executor disablement and Emergency
  Stop are hard gates, and live actions reauthorize immediately before SQL.
- Made runtime configuration immutable, generation-based, strict, and
  compare-and-swap protected. Meta mode owns durable control state; YAML fleet
  settings and membership are visible but explicitly read-only in the UI.
- Added prepare/swap/drain ownership for fleet lifecycle changes, reliable
  watcher shutdown, bounded worker teardown, session-pinned advisory locks,
  and race-free executor safety state.
- Enforced fail-closed AgentDB policy ceilings, server-owned execution plans,
  exact-operation authorization, leased monitoring claims, JIT credentials,
  bounded monitoring concurrency, and paginated control APIs.
- Corrected log offset ownership, LLM admission and budget accounting,
  runaway and migration detectors, pooled-session cleanup, and immediate
  reauthorization of every mutating operation.
- Removed retired MCP claims, generated configuration lifecycle documentation,
  reconciled AgentDB documentation, and corrected Docker and browser fixtures.
- Isolated database tests per package, migrated and pinned golangci-lint v2,
  restored parallel-safe CI, and raised every business package above 70%
  coverage and every command utility to at least 50%.

## v1.2 (2026-06-11) -- Autonomous DBA Core + LLM-Native Tier

### Added

- **Actions UI shows attempts + risk.** The executed-actions table now shows an Attempts column (how many times pg_sage tried an action — surfacing retries that were previously only in the log) and a Risk badge (safe/moderate auto-run; advisory is recommend-only).
- **Restart from the UI.** Settings now shows that some settings (trust tiers,
  maintenance window, execution mode, intervals) take effect only after a
  restart, with a **Restart now** button. `POST /api/v1/restart` (admin) exits
  with code 42; the launcher (and any orchestrator restart policy) relaunches
  the process. Returns 501 if no supervisor is configured.
- **Query store (F2):** `sage.query_store` records per-queryid metrics each
  cycle, enabling *windowed* latency (pg_stat_statements only exposes lifetime
  averages). Substrate for verify-and-revert and plan-regression detection.
- **Per-queryid verify-and-revert (F1):** the executor's rollback check now
  compares the targeted queries' before/after windowed latency instead of a
  coarse global cache-hit/avg-write heuristic, so a change is reverted only if
  the queries it targeted actually regressed.
- **Index lifecycle (A2):** optimizer index recommendations now carry their
  analyzed queryids, so F1 verifies a newly-created index and drops it (via its
  `DROP INDEX` rollback) if a targeted query regresses; combined with the
  existing measured-non-use auto-drop.
- **Per-table autovacuum tuning (A1), ANALYZE / stale-statistics autopilot
  (A4), and wraparound-freeze guard (A6)** — deterministic SAFE/critical
  maintenance actions with rollback.
- **Doc-grounded config tuning (A3):** a curated GUC knowledge base grounds the
  advisor's LLM recommendations in documented semantics and gates any
  `ALTER SYSTEM` value outside the documented safe range.
- **Plain-English action justification (C4):** every executed action gets an
  LLM-written audit note in `sage.action_log.justification`.
- **Plan-change narrative (C6):** `plan_regression` findings are enriched with
  an LLM "why did the plan change" explanation.
- **AgentDB fleet integration (B1)**, lifecycle reconciler scheduling (F4), and
  fleet LLM token-budget enforcement (F5).
- `llm.UnwrapText` strips `json_mode` JSON wrapping from prose audit
  notes/narratives.

### Changed

- **Self-monitoring:** pg_sage now sets `pg_stat_statements.track = none` on
  its own connections (best-effort, superuser only), so its monitoring queries
  are never recorded and no longer pollute the user's `pg_stat_statements`. As
  a fallback for restricted roles, every query pg_sage issues also carries a
  `/* pg_sage */` marker and the self-monitoring filter excludes both the
  marker and any `sage.`-schema query from analysis.
- `isThinkingModel` now covers the `gemini-3.x` series (reserves output-token
  budget for thinking models, e.g. `gemini-3.5-flash`).
- **Autonomy gate coupling:** raising the trust level to advisory/autonomous
  now opens the execution gate. A `manual` execution_mode combined with a
  non-observation trust level was a silent contradiction (the "turned on
  autonomous, nothing happened" trap); the executor now treats that as `auto`
  (still gated by trust tier, ramp age, and the maintenance window). Works live
  and across restart, since trust level persists in `sage.config`.
- **Maintenance windows are no longer cron-only.** The window accepts friendly
  forms: presets (`nights`, `weeknights`, `weekends`, `off-hours`,
  `business-hours`, `always`, `never`) and day-qualified ranges
  (`weekdays 01:00-05:00`, `Sat-Sun 02:00-06:00`, `Mon,Wed,Fri 22:00-04:00`),
  alongside the existing `HH:MM-HH:MM` and cron syntax.

### Fixed

- **Config reloads can execute.** `SELECT pg_reload_conf()` is now on the
  executor's SELECT allowlist, so the apply-step after an autonomous
  `ALTER SYSTEM` actually takes effect instead of failing validation.
- **No more orphan reload findings.** When an advisor recommendation bundles
  `ALTER SYSTEM ...; SELECT pg_reload_conf();`, the reload (an apply
  mechanism the executor performs itself) is dropped from the statement
  split rather than becoming its own unexecutable finding.
- **Optimizer index recommendations auto-execute.** LLM index recs
  (missing/composite/covering/GIN/HNSW) are now deterministically classified
  `moderate` — `CREATE INDEX CONCURRENTLY` is online and reversible — so
  they run autonomously under the moderate trust gate instead of being
  stuck advisory behind the LLM's self-rated `high_risk`. Index DROPs keep
  their conservative rating.
- **GIN/HNSW DDL passes validation.** The column-existence check now strips
  operator-class specifiers (`payload jsonb_path_ops`,
  `embedding vector_l2_ops`) instead of rejecting them as nonexistent
  columns.
- **Optimizer indexes no longer self-destruct.** The INCLUDE-upgrade path
  consumed `drop_ddl` — which doubles as the new index's own rollback — and
  dropped the index it had just created. A self-reference guard now skips
  the upgrade drop when it targets the newly created index; rollback remains
  the job of the post-action monitor.
- **Subset-index drops are advisory now.** A leading-prefix "subset" index
  (`(c1)` covered by `(c1, c2)`) is no longer auto-dropped — it's a judgment
  call (read-perf trade-off, and apps that re-create their own indexes turn an
  auto-drop into churn). It's surfaced as info/advisory; exact-duplicate index
  drops remain automatic.
- **Multi-statement config recommendations are split.** An advisor
  recommendation carrying several `ALTER SYSTEM` statements (e.g. WAL tuning) is
  split into one atomic, individually-validated action per parameter, instead
  of being rejected by the executor as multi-statement SQL.
- **Resume now works after a restart.** Resuming reconciled only the in-memory
  state, so after a restart (in-memory "running", persisted flag "stopped")
  resume couldn't clear the persisted `emergency_stop`. It now always
  reconciles the persisted flag.
- **Anti-oscillation guard.** If pg_sage applies the same successful action
  to an object repeatedly (e.g. dropping an index an app keeps re-creating),
  it now backs off after a few cycles and marks the finding acted-on, instead
  of churning drop→recreate→drop forever. The guard reads the action log, so it
  survives restarts (the in-memory cascade cooldown did not). This fixes a
  real runaway where thousands of duplicate-index drops accumulated against an
  app-managed database.
- **Config changes now take effect (and are cloud-aware).** Previously an
  `ALTER SYSTEM` action wrote `postgresql.auto.conf` but never reloaded, so the
  change silently never applied. The executor now: reloads (`pg_reload_conf`)
  after a reload-only GUC on a self-managed server so it takes effect; marks a
  restart-only GUC (`shared_buffers`, `max_connections`, …) as
  `applied_pending_restart`; and on a **managed provider** (RDS/Aurora,
  Cloud SQL/AlloyDB, Azure) skips `ALTER SYSTEM` entirely (it's blocked there)
  and records that the change must be applied via the provider's parameter
  group / database flags. The action log outcome now reflects whether a config
  change is actually in effect.
- **Retention:** `cleanStaleFirstSeen` no longer skips stale-key cleanup when
  the index snapshot is empty (a regression from an over-broad safety guard).
- **Cases:** pg_sage no longer surfaces its own monitoring queries as Cases.
  The case projection now skips self-monitoring findings, and detection
  recognizes pg_sage's statistics-catalog reads (`pg_stat_statements`,
  `pg_stat_user_tables`, etc.) even in historical findings captured before
  queries were tagged.
- **Recommendation quality:**
  - The connection advisor now sizes `max_connections` against *total* (open)
    backends, not just active ones, with a hard deterministic floor — it can
    no longer recommend a value below current usage (previously it could
    suggest `max_connections = 20` on a database with 28 open connections).
  - Index rules (unused / duplicate / subset / missing-FK) now skip system and
    extension-internal schemas (`pg_*`, `information_schema`, `_timescaledb_*`),
    so pg_sage no longer recommends dropping or creating indexes it has no
    business touching (e.g. `_timescaledb_catalog` indexes).
  - Subset-index detection now also requires the superset to serve the subset's
    `INCLUDE` columns, so an index supporting an index-only scan isn't flagged
    as redundant.
  - Subset-index detection now only recommends dropping a narrow index when the
    superset is a *close* replacement — at most ~2 extra key columns, not more
    than ~3x the on-disk size, and not a heavily-used narrow index — so a `(c1)`
    index is no longer replaced by a wide `(c1..c12)` one.

## v1.1 (2026-05-10) -- AgentDB Cloud Provisioning Release

### Added

- AgentDB now supports gated live cloud provisioning paths for AWS RDS, GCP
  Cloud SQL, and Databricks Lakebase branch workflows alongside local schema
  and database provisioning.
- Added LLM-required blueprint generation that turns English deployment intent
  into typed AgentDB blueprints and draft Terraform templates for review.
- Added Terraform upload/import review flow with static policy checks for
  provider-specific shapes that change frequently across cloud vendors.
- Added provider settings, custom size profiles, cloud field tooltips, and
  compact AgentDB tabs for deployments, provisioning, profiles, provider
  settings, Terraform, blueprints, and activity.
- Added backup assurance, restore-verification gates, ping/token lifecycle,
  TTL cleanup, cost samples, query tuning recommendation contracts, audit
  export, and deploy-request promotion records for agent-owned databases.
- Added a dedicated AgentDB Cloud Provider Setup
  guide for AWS, GCP, Databricks, live safety gates, validation commands, and
  cleanup evidence. That feature and guide were later removed; see the current
  [decommissioning guide](sidecar/internal/decommission/README.md).

### Changed

- AgentDB provider documentation now reflects dry-run and gated live execution
  instead of describing cloud providers as plan-only.
- The AgentDB UI links directly to cloud setup guidance from provisioning,
  provider settings, Terraform, and blueprint workflows.
- Cloud provisioning requires explicit live gates, provider policy allowlists,
  cost/TTL guardrails, and restore-verified backup evidence before destructive
  live cleanup.

### Fixed

- Fixed bodyless AgentDB action requests that returned invalid request errors.
- Fixed UI delete/lifecycle actions so archived deployments can run guarded
  destroy flows when backup policy is satisfied.
- Fixed cloud blueprint provisioning metadata so requested regions and provider
  shape fields are preserved instead of silently falling back to defaults.
- Fixed test login targeting for the `8085` sidecar and rebuilt the embedded UI.

### Verification

- `go test -cover -count=1 -p 1 ./...` passed from `sidecar`.
- `npm test -- --run`, `npm run lint`, and `npm run build` passed from
  `sidecar/web`.
- `npx playwright test --workers=1` passed against `http://127.0.0.1:8085`;
  provider-gated suites were intentionally skipped unless live flags were set.

## v1 (2026-04-28) — Autonomous DBA Release

> Published on GitHub `master` as tag `v1`. This release includes the
> autonomous DBA feature slices, the prior verification bug pass tracked in
> `docs/codex-bug-log.md`, the final provider-readiness contract updates, and
> the admin/legacy workflow verification pass.

### Functionality Change

This release moves pg_sage further away from a passive observability dashboard and closer to an autonomous DBA workflow. Findings, incidents, migration risks, proposed actions, approval state, and execution history now behave more like a single operational case queue that a DBA can triage end to end. The goal is for pg_sage to preserve the full story around a database problem: what was detected, why it matters, what the system recommends, what action was proposed, whether a human approved or rejected it, and what happened after execution.

The action lifecycle is now more deterministic and safer under real operating conditions. Proposed actions are filtered when their source finding has already been resolved, successful executions close the loop back to the finding, rejected actions observe cooldowns, and stale or duplicate create-index work is suppressed before it can create noise or risk. This matters because autonomous database work cannot rely on optimistic UI state alone; every action needs fresh evidence, an auditable state transition, and guardrails that prevent the system from repeatedly proposing or executing work that has already been handled.

Fleet behavior was tightened across settings, detail lookups, action execution, incident resolution, and managed database editing. In multi-database mode, pg_sage now requires explicit database targeting for mutations and ambiguous detail reads, refreshes runtime state after managed database changes, and keeps selected-database configuration separate from global configuration. These changes are intentionally conservative: once a product can operate across a fleet, wrong-database reads or writes become one of the highest-risk failure modes, so v1 prioritizes precise scoping over convenience.

The release also adds more trust-building surface for teams evaluating autonomy. Provider readiness, shadow-mode reporting, durable LLM cooldown tracking, migration safety cases, and incident playbook actions give operators a clearer view of what pg_sage can do, what is blocking it, and what it would have done before automation is enabled. The reasoning behind these additions is adoption-oriented: DB teams are more likely to trust autonomous execution when the product can first prove avoided toil, expose its constraints, and show a reliable decision trail.

### Added

- Cases now consolidate schema-health, forecast, incident, and query-hint work into one DBA case surface. Legacy `#/schema-health`, `#/forecasts`, `#/incidents`, and `#/query-hints` routes now open Cases with the relevant source filter selected, while the Cases API projects active/broken query hints as case evidence alongside existing findings and incidents.
- Migration-safety cases now include a deterministic DDL preflight report and PR/CI-ready script output. Case and action detail surfaces show lock/rewrite risk, live-risk checks when available, generated migration SQL, rollback or forward-fix guidance, verification SQL, and PR metadata without directly executing high-risk DDL.
- Incident playbook automation now covers runaway queries, connection exhaustion, WAL/replication risk, and sequence exhaustion in addition to lock blockers and idle-in-transaction incidents. Low-risk playbooks emit read-only diagnostics, PID actions still require exact PID evidence and approval, and sequence capacity work is generated as a forward-fix migration script instead of direct execution.
- Vacuum, bloat, and freeze cases now project explicit autopilot candidates. Table-bloat findings can propose guarded `VACUUM`, IO-saturated bloat cases are blocked to script/review output, XID wraparound findings get freeze-blocker diagnostics, and per-table autovacuum tuning produces PR/CI-ready reloption scripts with verification SQL.
- Query tuning now extends beyond `pg_hint_plan` hints. Suggested rewrites become PR-ready query rewrite artifacts with semantic and plan verification, broken hints can be retired through a safe metadata action, and repeated per-role `work_mem` hints can be promoted to reviewed role-level configuration changes.
- Provider readiness now uses provider-specific capability adapters for self-managed Postgres, Cloud SQL, AlloyDB, RDS, and Aurora. The readiness matrix exposes extension enablement paths, log-access expectations, provider limitations, and the expanded action family support instead of treating every Postgres endpoint as operationally identical.
- DDL safety preflight now records live-risk checks for table size, active workload, pending locks, replica lag, and lock-timeout configuration. High-warning live preflight output blocks direct execution and keeps the action in reviewed PR/script mode.
- Incident and maintenance coverage now adds autovacuum-falling-behind, standby-conflict, blocked-vacuum, concurrent reindex, bloat-remediation planning, `CREATE STATISTICS`, and parameterized-query action families, each with typed contracts and verification plans.
- Provider readiness now exposes the expanded action-family detail in the UI, including `ddl_preflight` and standby-conflict diagnostics, so operators can see exactly which autonomous capabilities are blocked, observable, script-only, or executable for each provider.
- Admin and legacy workflows were rechecked for v1: Users, Notifications, Fleet add/edit/import surfaces, Database and Alerts empty states, command palette navigation, not-found handling, and legacy `#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and `#/incidents` routes.

### Fixed

- Fleet incident, finding, and action detail endpoints could scan all pools by ID and return the first matching row, so duplicate IDs across databases could surface the wrong record. Detail endpoints now require an explicit `?database=` in multi-pool fleet mode, while still allowing implicit lookup when exactly one database is registered.
- Successful recommendation actions left source findings open, so the UI could continue to offer the same action. Successful action execution now marks linked findings resolved and stores the action log id.
- Rejected recommendations could immediately reappear as pending approvals. Approval mode now suppresses recently rejected finding/SQL pairs during the cooldown window.
- Pending approval counts did not update live. SSE now emits action events from both `sage.action_queue` and `sage.action_log`, and the layout subscribes to refresh pending counts.
- Recommendations could still show manual action controls while a matching approval was pending. The UI now hides duplicate manual action controls when pending approvals exist.
- Emergency Stop cancelled monitoring goroutines, and Resume could not restart them. Emergency Stop now gates actions without cancelling monitoring, so Resume restores operation.
- Several bodyless UI `POST` calls were rejected with HTTP 415 because they omitted `Content-Type: application/json`. Fixed emergency stop/resume, notification test-send, DB test, incident resolve, and token budget reset call sites.
- Managed database edits updated `sage.databases` but did not refresh the active fleet runtime instance. Updates now remove the old runtime name and register the updated record.
- Managed database updates against missing ids could report an internal readback failure instead of not found. Store updates now check affected rows and the API maps missing records to 404.
- Notification channel toggles could persist masked secrets returned by the list API, corrupting Slack webhooks, PagerDuty routing keys, or email passwords. Store updates now preserve existing real secrets when masked placeholders are replayed, and API masking covers `smtp_pass`.
- Notification channel/rule update and delete calls reported success for missing ids, and rule creation relied on database FK errors for missing channels. Notification stores now return not-found/validation errors deterministically and handlers map them to 404/400.
- Notification channel updates treated omitted `enabled`, `name`, or `config` fields as false/empty values, so partial clients could accidentally disable or invalidate channels. The update handler now preserves existing omitted fields.
- Admin users could demote themselves or demote the last admin, potentially leaving the system with no administrator. The API now forbids self admin-role changes and last-admin demotion; the Users UI disables those controls.
- Edit Database `Test Connection` tested the saved connection instead of unsaved form edits. The edit form now submits current fields, and the backend merges the stored password when the password field is left blank.
- Edit Database `Test Connection` with unsaved preview fields bypassed the SSRF host guard used by Add Database testing. Preview edit tests now resolve and block unsafe hosts through the same safety path.
- Editing a managed database could send `max_connections` as a string from the UI, failing JSON decode into the Go integer field. The form now coerces `max_connections` numerically like `port`.
- The edit-form SSRF guard initially blocked testing an already-managed local/private database host, breaking legitimate local fixture edits. Edit tests now allow the stored managed host while still blocking unsafe host changes.
- Settings always saved global config, even when a specific database was selected. Fleet status now exposes database ids, Settings maps the selected database name to id, and selected-database edits use `/api/v1/config/databases/{id}`.
- Global Settings exposed `execution_mode` as an editable field even though global saves discarded it. The global UI now marks execution mode as database-only, while selected-database Settings can edit and persist it.
- `GET /api/v1/config/databases/{id}` returned config for nonexistent ids and defaulted null `execution_mode` to `manual`. It now returns 404 for missing databases and uses the managed DB default `approval`.
- Saved per-database config overrides could not be reset from Settings. A per-database override delete endpoint now removes stored DB overrides, and the Settings reset button calls it for DB overrides.
- Per-database `trust.level` reset removed the stored override but did not update the running fleet executor/status. Reset now synchronizes the effective inherited trust level back into runtime state.
- Multi-key config saves could partially persist earlier keys before later validation failures returned 400. Config batch saves now prevalidate all keys before writing any overrides.
- LLM model discovery could write a masked `llm.api_key` back into global config and ignored selected database scope. Discovery now uses a non-persistent model-discovery request and skips masked keys.
- `/api/v1/explain` accepted viewer-role requests even though it can run database queries. The route now requires operator-or-admin access and has API coverage for valid and invalid explain requests.
- Parameterized explain requests interpolated caller-supplied parameter strings directly into `EXPLAIN EXECUTE`. Parameters are now emitted as escaped SQL literals, with NUL bytes rejected as invalid input.
- Incident resolution from the UI sent an empty JSON request despite the API requiring a JSON body, so resolving incidents could fail with HTTP 400. The UI now sends `{ "reason": "" }`.
- Incident confidence never rendered because the API returns `confidence` but the UI read `confidence_score`. The incident detail drawer now renders the API field.
- Fleet-wide Query Hints, Incidents, and Action History read only the primary database for all-database views, hiding secondary database rows. These handlers now merge all registered pools, include `database_name`, sort deterministically, and apply global limits where needed.
- Action details searched only the primary database. Detail lookup now searches all pools so secondary-database action history rows are reachable.
- Manual action execution accepted any valid SQL for any numeric finding id, including missing, resolved, or mismatched findings. Execution now verifies the finding is open/unresolved and the SQL matches the recommendation before running DDL.
- Pending action lists and counts could expose queue rows for findings already resolved by another action, leading users to approve stale actions that could not safely run. Pending action queries and approval now require the linked finding to still be open and actionable.
- Fleet suppress/unsuppress without `?database=` could mutate the first matching finding id by map-order scan across databases. Mutations now require a database when more than one instance exists and only allow implicit targeting for a single connected instance.
- Emergency stop/resume returned 200 with zero changes for unknown database names and ignored persistence errors from writing the stop flag. Handlers now return 404 for unknown databases and surface persistence failures instead of reporting success.
- Fleet Health chart used raw database aliases as Recharts `dataKey` values. Aliases containing dots could be interpreted as object paths and fail to render; the chart now uses safe internal series keys and display names separately.
- Schema Health applied status/severity/category filters to the table but not to the summary stats, so the summary could contradict the visible rows. Stats now use the same filters, and the summary label now describes matching findings rather than always saying open findings.
- Query Hints treated missing `after_cost` as zero due JavaScript null coercion, creating fake 100% cost improvements. Cost improvement and average improvement now require both before and after costs.
- Query Hints lifecycle states existed (`retired`, `broken`) but `/api/v1/query-hints` only returned active rows, making the UI Status column misleading and hiding revalidation outcomes. The endpoint now returns recent hints across statuses by default and supports `status=active|retired|broken` filtering.
- Schema Health could not filter the `safety` thematic category even though the backend schema linter emits it. The category dropdown now includes safety, and live coverage verifies expanded rows show impact, recommendation, and suggested SQL.
- Fleet Health chart series keys were safe for dotted aliases but still depended on unsorted response object order, so series key-to-database mapping could shift between refreshes. Series are now sorted deterministically, and live coverage verifies chart rendering from seeded `health_history` rows across all three local databases.
- Incidents accepted `low`, `medium`, and `high` risk values plus newer source values, but the UI only labeled the older `safe`, `moderate`, and `high_risk` variants. The detail drawer now labels all valid risk/source values.
- Viewer users were shown the Pending Approval tab and the Actions page fetched operator-only pending approvals, causing forbidden requests for read-only users. Pending-review UI and polling now render only for admin/operator roles, with live viewer/operator boundary coverage.
- Notification channel UI did not expose PagerDuty even though the backend supports it, and selecting unsupported types would fall through to the email form shape. The Channels form now exposes PagerDuty with a routing-key field and stable browser-test selectors for all channel types.
- Notification masked-secret preservation now has browser-level coverage: creating a Slack channel through the UI, receiving a masked webhook from the API, toggling the channel from the UI, and verifying the database still stores the original webhook rather than the masked placeholder.
- Existing managed database connection tests in YAML fleet mode tried to decrypt the `sage.databases.password_enc` placeholder even though runtime credentials live in the fleet instance config. The endpoint now uses fleet runtime credentials for fleet-registered rows and only reports 404 for actual missing database records.
- Managed Database edit-form connection testing now has browser-level coverage for unsaved field changes and empty password fallback, including a failing unsaved database name followed by a successful retry without saving.
- Managed Database API/UI edits did not round-trip `max_connections`; saving from the UI could overwrite a configured fleet limit with `0`. The API now returns and accepts `max_connections`, defaults absent values safely, and the UI includes the field.
- In local YAML fleet mode, saving a managed database edit could close the primary fleet pool, which is also captured for auth and catalog storage, causing subsequent logins/API calls to hang or fail. No-meta fleet edits now refresh runtime metadata in place without closing the primary pool, with browser coverage for saved edits updating runtime trust state.
- Background/live refetches set `loading=true` even when data was already rendered, unmounting tables and collapsing/detaching expanded finding action panels while a user was approving or rejecting queued work. `useAPI` now keeps existing data mounted during background refetches and only shows loading on initial load or URL changes.
- Local no-meta fleet mode exposed Add Database even though new database credentials cannot be encrypted without an `encryption_key`, causing a 500. The store now returns a validation error, and browser coverage verifies the UI surfaces the `encryption_key` requirement.
- Global Settings reset could not remove global overrides correctly because hot reload mutated the live config and the API no longer had an immutable YAML/default baseline to restore from. Config routes now keep a detached baseline snapshot, expose `DELETE /api/v1/config/global/{key}`, reload live config from baseline plus remaining overrides, and the Settings UI can reset global overrides.
- The full-surface fixture did not include a deterministic invalid-index case and only printed finding summaries. The fixture now creates a failed concurrent unique index, asserts expected analyzer categories, schema-lint rule IDs, and active query hints across all three local databases, and polls until the expected surface appears.
- The legacy serial walkthrough could not run against local Docker fixtures without hardcoded database credentials and failed strict-mode locators when multiple finding detail/suppress controls were visible. The walkthrough now supports fixture DB credential env vars, has an encrypted meta-db config for add/edit/delete flows, and scopes duplicate UI controls to the first visible target.
- The encrypted walkthrough fixture was still a manual setup sequence. `run_walkthrough_fixture.ps1` now provisions isolated meta/target databases, seeds the fixture schema, starts the encrypted sidecar, resets the admin password, runs the 124-check walkthrough, and can restore the standard full-surface sidecar afterward.
- The long walkthrough used Playwright serial mode, so one failed checkpoint skipped every later check and hid independent endpoint/UI regressions. The global serial mode is now removed; the wrapper still runs the file with one worker to preserve fixture ordering, but later checks continue after failures.
- Successful create-index approvals could be followed by a stale analyzer cycle reopening the same missing-index finding as a new open row. Finding upserts now suppress immediate reopen of recently action-resolved findings while still allowing genuinely unresolved issues to reappear after the grace window.
- Stale or parallel create-index approvals could execute after an equivalent index already existed, creating duplicate indexes with auto-generated names. Manual/approved create-index execution now checks for an existing valid covering index before running DDL and records the action as successful without creating another index.
- Action lifecycle verification only checked row presence in browser tests. The full-surface harness now verifies pending rows across all three local databases, approve execution, rejected queue removal, executed action logs, resolved finding links, and no post-refresh missing/duplicate-index noise.
- Failed `CREATE INDEX CONCURRENTLY` attempts could leave invalid autogenerated index stubs that blocked the next approval of the same recommendation. Create-index execution now drops invalid covering index blockers before retrying the DDL.
- Transient lock timeouts during approved `CREATE INDEX CONCURRENTLY` actions could fail the UI workflow and leave an invalid index stub. Create-index execution now retries lock-timeout failures and cleans invalid blockers between attempts.
- Mutating action browser specs selected the first available pending create/drop action, so parallel runs could contend for the same database/object depending on queue order. The specs now use disjoint deterministic database/action targets.
- The full-surface high-total-time fixture used a PL/pgSQL loop that pg_stat_statements counted as one call, making the `high_total_time` category intermittent. The workload now uses psql `\gexec` so the high-frequency query is recorded as thousands of real calls.
- Incident resolve mutations in a multi-database fleet could resolve the first matching incident UUID by pool order when the UI did not send a database target. Resolve now requires an explicit database when multiple pools are connected, the UI sends the row database, and duplicate incident UUID coverage verifies only the selected database is mutated.
- Incident listing for a selected fleet alias could drop rows whose stored `database_name` was the physical PostgreSQL database name, and the UI could then resolve against that physical name instead of the fleet alias. Incident list responses now include `fleet_database_name` for mutations, and selected-pool listing no longer filters rows by alias.
- Last-admin demote/delete checks were split from the mutation, so concurrent requests could both pass a stale admin count and leave the system without an admin. User role changes and deletes now run in a locked transaction with DB-backed concurrency coverage.

## v0.8.5 (2026-04-12) — Hint Lifecycle, Stale-Stats ANALYZE, Security Hardening, Full Test Sweep

> 91 files changed across the sidecar. 17,016 lines added. 3,480 Go tests + 44 Playwright e2e tests across 6 spec files. Zero skips, zero failures. This is the most thoroughly tested release pg_sage has shipped.

---

### New Features

#### F1 — Hint Revalidation Loop
Hints are no longer fire-and-forget. A new background cycle (`tuner.verify_after_apply`, default off for backward compatibility) continuously validates every active `pg_hint_plan` hint against the live query planner:

- **Staleness check** — retire hints whose query has fallen below `min_query_calls` or been pruned from `pg_stat_statements`.
- **TTL check** — retire hints older than `hint_retirement_days` (default 14) unconditionally.
- **Cost regression check** — cost-compare hinted vs unhinted plan. Keep if `hinted_cost ≤ 1.2× unhinted_cost`; roll back and mark broken when `hinted_cost > unhinted_cost / 0.8`.
- **Redundancy check** — detect directives that no longer affect the generated plan (e.g., an `IndexScan` hint on a table whose only access path is already an index scan).

New directive parser (`hint_parse.go`) supports `Set(work_mem)`, `IndexScan`, `BitmapScan`, `NestLoop`, `HashJoin`, `MergeJoin`, and `Parallel`. Revalidation EXPLAIN carries its own `statement_timeout` so it cannot starve the collector.

#### F2 — Stale-Stats Detection + Autonomous ANALYZE
pg_sage now detects tables with stale statistics by correlating three signals: row-estimate skew (actual vs planned rows diverging by 10×+), modification ratio since last ANALYZE exceeding 10%, and last ANALYZE age over 60 minutes. When all three converge, the executor issues `ANALYZE <schema>.<table>` with full safety controls:

- Per-table cooldown (60 min) that respects autovacuum's own `last_analyze` timestamp
- Size ceiling (10 GB) — larger tables emit advisory findings instead of executing
- Maintenance-window gating (1 GB+) — large tables only ANALYZE during configured windows
- Statement timeout (10 min) with automatic cooldown extension on timeout
- Fleet-wide concurrency cap (1 concurrent ANALYZE across all databases)

#### F3 — Role-Level work_mem Promotion
When multiple queries owned by the same database role accumulate `Set(work_mem)` hints (default threshold: 5), pg_sage detects the pattern and recommends `ALTER ROLE ... SET work_mem` instead — one role-level setting replacing N per-query hints. Excludes `NOLOGIN`, `SUPERUSER`, and reserved roles. Scoped by `pg_stat_statements.dbid` to prevent cross-database pollution in shared-cluster deployments.

#### F4 — Extension Drift Detector (Enhanced)
Tightened drift detection for `pg_stat_statements`, `pg_hint_plan`, `hypopg`, and `auto_explain`. Missing critical columns (`plan_time`, `wal_records`) now produce explicit remediation hints with version-specific upgrade instructions, replacing the previous generic warnings.

#### F5 — Config Tooltip Infrastructure
Every Tier 1 and most Tier 2 config fields now carry `doc`, `warning`, `mode`, `docs_url`, and `secret` struct tags. A new build tool (`cmd/gen_config_meta`) reflects over `config.DefaultConfig()` to emit a 167-field JSON metadata file consumed by the React frontend. The `ConfigTooltip` component (WCAG-compliant, portal-mounted, keyboard-accessible) surfaces this metadata as hover/focus tooltips on every config field in the Settings page — including doc text, warning callouts, mode badges (fleet-only / standalone-only), and secret indicators. A reflection-based drift test fails the build if metadata falls out of sync with the Go struct tags.

#### F6 — Hint-Index Coordination
New deferral logic prevents the tuner from installing `pg_hint_plan` hints while the optimizer has pending index recommendations for the same query. When an index recommendation is in flight, the tuner defers hint creation until the index is either applied or dismissed — preventing the scenario where a hint masks the benefit of a better index. Plan scanning (`planscan.go`) detects whether a pending index would affect the query's execution path before deciding to defer.

#### F7 — Token Budget Dashboard
New `TokenBudgetBanner` React component on the Dashboard and Settings pages shows real-time LLM token consumption against the configured daily budget. Visual indicator transitions from green → amber → red as usage approaches the cap. Reads from the existing `/api/v1/llm/token-usage` endpoint with no additional backend changes.

---

### Fleet & Wiring Fixes

- **Fleet database wiring** — `FleetMgr.PoolForDatabase("all")` no longer returns a nil pool when fleet mode has a single database configured. The `wireRouter` extraction refactor (`wire.go`) splits 5 concerns (standalone wiring, fleet wiring, router setup, middleware chain, graceful shutdown) into testable functions with dedicated mode-specific wiring tests.

---

### Security Hardening

Six targeted fixes addressing edge cases surfaced during adversarial review:

1. **PagerDuty retry amplification** — exponential backoff with jitter replaces unbounded retries on transient 5xx responses. Max 3 attempts, circuit breaker after consecutive failures.
2. **Rate limiter connection leak** — login rate limiter now properly releases semaphore on early returns, preventing goroutine accumulation under sustained brute-force.
3. **Silent zero on invalid port** — `strconv.Atoi` failures on user-supplied port strings now return explicit validation errors instead of silently defaulting to port 0.
4. **CORS origin clarification** — `config_apply.go` validates CORS origin against an allowlist; previously accepted any origin header when the allowlist was empty.
5. **SSRF fail-closed** — `test-connection` endpoint rejects private/loopback IP ranges before attempting the connection, preventing internal network scanning via the API.
6. **Multipart size limit** — file upload endpoints enforce a 10 MB ceiling to prevent memory exhaustion from oversized payloads.

---

### Test Sweep (P0–P5)

The most comprehensive test expansion in the project's history. Every priority tier addressed:

| Priority | Focus | Tests Added |
|----------|-------|-------------|
| P0 | Config consistency — verify every YAML key round-trips through `Load()` → `Save()` without loss or mutation | 47 tests |
| P1 | Wire extraction — `wireRouter` decomposition with mode-specific wiring tests (standalone, fleet, unknown) | 19 tests |
| P2 | LLM degradation — malformed JSON, markdown-wrapped responses, empty bodies, timeout, rate limiting across advisor/optimizer/tuner | 43 tests |
| P3 | Config defaults — every field with a non-zero default verified against `DefaultConfig()` | 131 assertions |
| P4 | API contract — endpoint existence, method enforcement, auth requirements, response shapes | 208 tests |
| P5 | Playwright e2e — Dashboard, Databases, Settings, Navigation, Token Budget, Tooltips | 44 tests across 6 specs |

Total test count: **3,480 Go test functions + 44 Playwright e2e tests**. All packages above 70% coverage threshold. Zero test modifications to force passage — every failure fixed in implementation code.

---

### Documentation

- **End-to-end walkthrough verification** — every doc page cross-referenced against the actual codebase. Six inaccuracies fixed:
  - `--database-url` → `--pg-url` (deployment.md, 4 occurrences)
  - `SAGE_GEMINI_API_KEY` → `SAGE_LLM_API_KEY` (deployment.md, 3 occurrences)
  - LLM endpoint suffix `/chat/completions` removed (SDK appends it)
  - `--meta-db` description corrected from "SQLite path" to "PostgreSQL URL for fleet mode"
  - `llm.token_budget` → `llm.token_budget_daily` with correct 500,000 default (security.md)
- **MCP reference removal** — MCP (Model Context Protocol) was documented across 7 files but never implemented in the Go sidecar. Removed all references: `sage.mcp_log` table from CI schema and sql-reference.md, MCP step from try-it-out.md, port 5433 from firewall notes, `mcp:` stanza from demo config, nav entry from mkdocs.yml. Zero functional code affected — this was purely phantom documentation.
- `config.example.yaml` updated with every new tuner and analyzer field.

---

### Stats

| Metric | Value |
|--------|-------|
| Commits since v0.8.4 | 5 |
| Files changed (sidecar) | 91 |
| Lines added | 17,016 |
| Lines removed | 2,295 |
| Go test functions | 3,480 |
| Playwright e2e tests | 44 (6 spec files) |
| Go packages tested | 26 |
| Test failures | 0 |
| Test skips | 0 |

## v0.8.4 (2026-04-07) — Security Hardening + Tuner Pipeline

### Security
- **RBAC**: Added `RequireRole` to legacy API routes (config, emergency-stop, resume, suppress/unsuppress, pending count). Viewers can no longer escalate trust to autonomous or halt the fleet.
- **SQL injection**: Block EXPLAIN injection via multi-statement rejection + `statement_timeout` + read-only transactions. New `sanitize.QuoteIdentifier` package for all catalog-derived identifiers. Fixed VACUUM, ALTER DATABASE, and tuner SQL construction.
- **Executor allowlists**: Whitelisted 30 safe `ALTER SYSTEM` GUCs. Restricted SELECT to `pg_terminate_backend`/`pg_cancel_backend`. Restricted `ALTER TABLE` to `SET`/`RESET`/`TABLESPACE`. Validate trust level strings. Cap concurrent DDL at 3. Auto-drop invalid indexes after failed `CREATE INDEX CONCURRENTLY`. Derive advisor `ActionRisk` from SQL type instead of hardcoding `safe`.
- **Auth**: Cookie `Secure` flag enabled. Password minimum 8 characters. Login rate limiting (5 attempts / 15 min per email). Self-deletion + last-admin protection.
- **API hardening**: Security headers middleware. Content-Type validation. SSRF protection on `test-connection`. Error messages sanitized (no more `err.Error()` leaked to clients). `X-Forwarded-For` only trusted from loopback. Notification channel secrets masked.
- **Secrets**: Removed hardcoded Gemini API key from `docker-compose.test.yml`. Admin password printed to stderr only. `create_admin` uses environment variables. `DeriveKey` upgraded to argon2id with v1 backward compatibility. Demo config uses env var.

### Tuner Pipeline
- `BuildInsertSQL`/`BuildDeleteSQL` now use `norm_query_string` column (was `query_id`, which doesn't exist in `hint_plan.hints`).
- Executor allowlist updated for `INSERT`/`DELETE INTO hint_plan.hints`.

### Fleet Hardening
- Schema bootstrap advisory lock changed from non-blocking `pg_try_advisory_lock` to blocking `pg_advisory_lock` with 30s timeout.
- `CREATE SCHEMA` now uses `IF NOT EXISTS`. All `DROP SCHEMA` operations in tests wrapped in advisory lock.
- `autoexplain.ConfigureSession` tolerates permission denied (SQLSTATE 42501) on managed DBs where role-level defaults suffice. Coverage boost to 83%.

### API
- New LLM token usage endpoint.
- Config apply/audit handlers.
- Settings page UI improvements.
- New `create_admin` CLI command.

### Test Infrastructure
- `auth`, `notify`, and `store` tests now hold `pg_sage` advisory lock for test duration to prevent schema drops mid-test.
- Store config tests re-insert FK user before each test.
- All 22 packages pass with `-p 4` (zero failures, zero skips).

## v0.8.3 (2026-04-04) — Cloud E2E + LLM Token Optimization
- Cloud E2E validation across 8 managed PostgreSQL databases:
  RDS PG14/18, Aurora PG14/17, Cloud SQL PG14/18, AlloyDB PG14/17
- Auto-detect cloud environment (rds, aurora, cloud-sql, alloydb)
- ALTER SYSTEM → ALTER DATABASE rewriting for managed platforms
- Executor max-retry limit (3 failures → mark as acted_on)
- LLM deduplication: skip redundant calls when open findings/hints exist
  (optimizer, tuner, advisor all check sage.findings/sage.query_hints first)
- 11 token waste fixes: bloat category mismatch, vacuum validation bug,
  thinking-model budget, CapturePlans loop hoist, column stats filtering,
  per-cycle table cap, per-query rewrite dedup, briefing LIMIT,
  retry scope (429/503 only), tuner stats cap, single-symptom deterministic skip
- Thinking model support: +16384 token overhead for Gemini 2.5 reasoning
- Cross-platform findings: 1615 total, 373 open, 802 acted on across 8 DBs
- All packages above 70% test coverage

## v0.8.2 (2026-04-03) — LLM Tuner + Query Rewrites
- Query tuner: hybrid deterministic rules (7 symptom kinds) + LLM-enhanced hints
- LLM-powered query rewrite suggestions alongside pg_hint_plan directives
- Rewrite suggestions surfaced in dashboard with rationale
- Alert notification (`query_rewrite_suggested` event) when rewrite is suggested
- Index optimizer multi-query consolidation (8 queries → minimal index set)
- E2E test suite: 54 subtests against real Gemini API
- 771+ tests

## v0.8.1 (2026-03-27) — Patch
- Add `google_ml` to all schema exclusion lists (Cloud SQL compatibility)
- Bump default LLM max_tokens to 8192 (Gemini 2.5 Flash thinking token fix)
- Add retry loop to index validity post-check (catalog propagation delay)
- Prevent re-execution of already-acted findings (re-drop race fix)
- Executor cooldown for recently created indexes
- Verified on Cloud SQL PG16/17 and AlloyDB PG17
- 588 tests, 0 failures

## v0.8.0 (2026-03-26) — Fleet Mode + Dashboard
- Fleet manager: single sidecar → N databases via `mode: fleet` config
- `DatabaseManager` with per-database collector/analyzer/executor goroutines
- Per-database advisory locks, trust levels, executor toggles
- Per-database LLM token budget (equal, proportional, or priority-weighted)
- Database-aware data model (every finding, action, metric carries `database_name`)
- Prometheus labels: `{database="prod-orders"}`
- Graceful per-database failure (one DB down doesn't crash others)
- REST API: 14 endpoints on `:8080` alongside MCP
- Fleet overview: `GET /api/v1/databases` with health scores
- Findings, actions, snapshots, config — all filterable by `?database=`
- Config hot-reload via `PUT /api/v1/config`
- Emergency stop/resume per-database and fleet-wide
- Web dashboard (React SPA embedded in binary via `//go:embed`)
- Demo environment: Docker Compose with 7 pre-planted problems, 46 verification checks
- 584+ tests, 0 failures, CI green (6 workflows)

**Integration bug fixes shipped in v0.8.0:**
- VACUUM routed through non-transaction connection (pgxpool wraps in tx by default)
- Trust ramp `ramp_start` config honored on first boot (was always `now()`)
- Unused index window default changed to 7 days (was 0, caused index churn)
- Advisor strips markdown fences from Gemini JSON responses
- `database_name` resolved to actual instance name (was showing "all")

## v0.7.0 (2026-03-26)

### Go Sidecar — The Product

pg_sage is now a Go sidecar binary that connects to any PostgreSQL 14-17 database.
The C extension is frozen at v0.6.0-rc3 (security fixes only).

#### Features
- **Standalone mode** — single binary, no extension install required
- **Index Optimizer v2** — LLM-powered index recommendations with HypoPG validation, confidence scoring, per-table circuit breakers, 8 validators
- **Vacuum Tuning** — per-table autovacuum analysis via LLM
- **WAL/Checkpoint Tuning** — max_wal_size, wal_compression, checkpoint analysis
- **Connection Pool Analysis** — max_connections, idle timeout, pooler detection
- **Memory Tuning** — shared_buffers, work_mem, cache hit ratio, spill detection
- **Query Rewrite Suggestions** — N+1, correlated subquery, OFFSET pagination detection
- **Bloat Remediation Planning** — VACUUM FULL vs pg_repack vs do nothing
- **MCP Server** — Claude Desktop and AI agent interface
- **Prometheus Metrics** — full observability endpoint
- **Dual-Model LLM** — separate models for general tasks vs index optimization
- **Trust-Ramped Executor** — observation -> advisory -> autonomous with rollback

#### Verified Platforms
- Google Cloud SQL (PG14, PG15, PG16, PG17)
- Google AlloyDB (PG17)
- Self-managed PostgreSQL (PG14-17)
- Amazon Aurora — test plan ready
- Amazon RDS — test plan ready

#### Testing
- 530 tests across 14 packages, 0 failures
- Live integration testing on Cloud SQL PG16, PG17, and AlloyDB PG17
- E2e tests with Gemini: 3 real LLM findings verified

#### C Extension (Frozen)
- v0.6.0-rc3 — no new features
- Works on self-managed PostgreSQL with auto-explain hooks
- SQL functions: sage.explain(), sage.diagnose(), sage.briefing()
