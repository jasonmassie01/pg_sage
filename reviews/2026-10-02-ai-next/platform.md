# Platform and observation: AI-next review (2026-10-02)

Master @ v1.8.1 (`9c52624`, lifeos-1 fixes included). Paths are relative to `sidecar/internal/`
unless they start with `cmd/`, `docs/` or `README.md`. Everything was checked in code unless
marked *inferred*.

## 1. Inventory

| Feature | Today | Where | LLM | Maturity | Evidence |
|---|---|---|---|---|---|
| Collector | Every 60 s it reads the top 500 statements, all tables, indexes and FKs, locks, sequences, system, replication, io, partitions and 2PC. It writes one jsonb row per category to `sage.snapshots`, and categories degrade one at a time. | `collector/` | — | Solid (pages bounded, total not) | 87.6% coverage; lifeos page 127→22 ms |
| Load breaker | Skips a cycle when active backends / max_connections is over the ceiling. | `collector/circuit_breaker.go` | — | Prototype | Unit tests |
| Forecaster | Regression/EWMA on daily aggregates for disk, connections, cache, sequences, query volume and checkpoints. | `forecaster/rules.go` | — | Solid after the fix | lifeos 70 s→20 ms |
| Growth forecast | Size-history forecast. | `forecaster/growth.go` | — | Dead | Tests only |
| LogWatch | Tails local csvlog/jsonlog and turns 18 fixed patterns into RCA signals. | `logwatch/` | — | Solid, narrow, off by default | Parser regressions |
| Self-monitor | Hides pg_sage's own queries and findings. It measures nothing. | `selfmonitor/` | — | Solid | Tests |
| Provider obs | Supabase CPU/memory and logs, used for index-build admission, RCA and plans. | `providerobs/` | Indirect (RCA) | Prototype | Env-gated live test |
| Fleet | Per-database runtimes, health score, emergency stop, equal LLM split, hot add/remove. | `fleet/`, `config/watcher.go` | — | Solid | Wave tests |
| Meta-db | Registry in `sage.databases`, reconciled every 30 s, capped at 50 databases. | `cmd/.../metadb.go` | — | Solid | Integration |
| HA | Role, timeline and system-id identity; safe mode on flapping; fails closed. | `ha/` | — | Solid | Container failover test |
| Startup | Pooler rejection, version ≥14, pg_stat_statements readable, query text visible. | `startup/checks.go` | — | Production | Tests |
| Schema/store | 101 `sage.*` tables, bootstrapped into **every monitored database**. 37 idempotent DDL groups and no version ledger. | `schema/`, `store/` | — | Production | 81.6% / 74.5% coverage |
| Retention | 1,000-row batched deletes by age; snapshots kept 90 days. | `retention/cleanup.go` | — | Solid | Tests |
| Alerting + Notify | Two separate stacks: findings → Slack/PD/webhook, and events/incidents/ChatOps → Slack/PD/email/Telegram. | `alerting/`, `notify/` | — | Solid | Tests |
| Briefing | Daily 06:00: top findings, a system blob and 24 h of actions, polished by the LLM. Goes to stdout by default. | `briefing/briefing.go` | Text polish | Prototype | Tests |
| Config | 350 YAML fields (299 restart / 46 reconfigure / 5 live), 138 API keys, 32 env vars. | `config/` | — | Production, sprawling | 89.2% coverage |
| Agent DB | LLM blueprint (JSON) → policy → Terraform text → dry run. Live create on RDS, Cloud SQL, Lakebase, Neon and Supabase behind 3+ flags. | `agentdb/` (13.7k lines) | Blueprint spec | Prototype | Live tests env-gated; no run evidence |
| Azure | ARM write of `max_slot_wal_keep_size` only. | `azure/`, `executor/managed_config_adapter.go:93` | — | Prototype | Env-gated |
| Install | Binary or Docker; about 5 steps; raw k8s YAML in the docs; no Helm. | `README.md`, `docs/installation.md` | — | Solid self-hosted | `TestDocumentedQuickStart` |

## 2. What is weak

**Collection cost (lifeos was only partly fixed)**

1. **pg_sage stores full copies of everything every minute, inside the production database.**
   - `persist` writes each category in full each cycle (`collector/collector_helpers.go:79-110`).
   - The `sage` schema is bootstrapped into each monitored database (`cmd/pg_sage_sidecar/database_runtime.go:155-165`), so the 9.3 GB goes into the customer's WAL, replicas and backups.
   - `sage.snapshots` is an unpartitioned heap (`schema/bootstrap.go:437-446`). Retention removes it with batched `DELETE` (`retention/cleanup.go:13,306-313`): bloat that pg_sage creates and then has to vacuum.
   - The lifeos report lists change-only storage as open work.
2. **The per-minute catalog read pays for sizes and DDL strings.**
   - Per table it calls `pg_total_relation_size`, `pg_table_size` and `pg_indexes_size`. Per index it calls `pg_get_indexdef` and `pg_relation_size` (`collector/queries.go:75-111`).
   - On lifeos that is about 100k size lookups and 35k definition strings every 60 s, for data that changes a few times a day.
3. **A forecaster-style bomb remains.** Every 10-minute analyzer cycle, `buildHistoricalAverages` loads every `queries` snapshot from the last 7 days (`analyzer/analyzer_checks.go:97-127`), roughly 10k rows × 500 queries. It then downsamples to 100 in Go (`:133`). *Inferred:* GBs are detoasted per cycle.
4. **The cold-start check counts the whole table.** It runs `SELECT COUNT(*) FROM sage.snapshots` on every optimizer run (`optimizer/coldstart.go:18`, `optimizer/optimizer.go:101`) just to learn whether there are at least 2 rows.
5. **The breaker is mislabeled and covers only the collector.**
   - The doc says "database-host CPU" (`config/config.go:192`). The code compares backends to max_connections (`circuit_breaker.go:27-31`).
   - Only `collector.go:84` checks it. The forecaster, history reads, runways and retention deletes ignore it, and the forecaster is what overloaded lifeos.
6. **pg_sage cannot see its own cost.**
   - `selfmonitor` filters pg_sage's own queries out of results (`selfmonitor.go:35-83`). Its callers are the analyzer, tuner, advisor, auto-explain and API.
   - No metric covers collector duration, `sage` schema size, or pg_sage's share of statement time. The closest is `pg_sage_collector_last_run_timestamp`.
   - Both lifeos overloads were found by a human.

**Signal quality**

7. **Cumulative values are used as trends.**
   - `cache_hit_ratio` is computed since the last stats reset (`collector/queries.go:131-148`). The forecaster then averages it per day and runs EWMA on that (`forecaster/rules.go:116-141`), so a cache collapse barely moves it.
   - Regression detection averages the cumulative `mean_exec_time` (`analyzer_checks.go:115-153`).
8. **FKs are collected without a schema** (`collector/queries.go:114-127`, unpaged). The analyzer has to skip any FK whose table name exists in more than one schema (`analyzer/rules_fk_index.go:57-63`). Missing-FK detection therefore goes quiet on multi-tenant and clone-heavy databases.
9. **Signals that are not collected:**
   - TPS and rollbacks, `pg_stat_wal`, temp bytes (`collector/queries.go:136-169`).
   - Per-statement `userid` / `toplevel` (`snapshot.go:37-60`).
   - Wait-event history (wait events are sampled only inside SRE probes, `sre/probes/catalog_m6.go:132`).
   - Host metrics outside Supabase.
   - Disk capacity: `forecaster.disk_capacity_bytes` is 0 and "nothing auto-detects it" (config_meta).
   - App, ORM and sqlcommenter context.
   - Deploy events: available only through the HMAC push API, which stays off until a secret is set. No CI or migration-tool history is ingested; the Flyway/Alembic/Prisma tables would have explained the index that was recreated 8 times.
10. **Logs are narrow, local and off by default.**
    - LogWatch needs local csvlog/jsonlog (`logwatch/detect.go:73-84`), which rules out RDS and Cloud SQL.
    - It is disabled by default (`config/config.go:885-891`).
    - Lines that match none of the 18 patterns are dropped (`classifier.go:210-213`), as is everything past 10,000 lines per cycle (`:200-202`).

**Dead or unwired code**

11. **`ForecastGrowth` has no caller.** It is the only writer of `sage.size_history` (`forecaster/growth.go:131`). `GET /api/v1/forecasts/growth` (`api/router.go:360`) reads that empty table.
12. **Per-database `collector_interval_seconds` / `analyzer_interval_seconds` and their `defaults.*` versions are normalized but never read** (`config/fleet.go:27-28,95-96,166-171`). The collector reads the global interval (`collector.go:66`). Four config knobs do nothing.
13. **Live-created RDS databases never join the fleet:** "needs secret resolution (not yet wired)" (`cmd/pg_sage_sidecar/agentdb_fleet.go:33-35`).
14. **Other unused code:**
    - `ha.Monitor.MutationsAllowed` (`ha.go:164`): its comment says "Executors should gate on this", but nothing calls it.
    - `RecordHealthSnapshots` (`fleet/manager.go:185`).
    - The briefing's own Slack sender (`briefing.go:378`) is a third Slack stack beside `alerting/` and `notify/`.

**Fleet, onboarding, config**

15. **Fleet health is noise.** The score is `100 − 25×critical − 5×warning` (`fleet/manager.go:164-177`). lifeos's 478 duplicate-index warnings score 0, the same as a database that is down.
16. **The LLM budget is split equally** (`fleet/budget.go:59`). A database mid-incident gets the same share as an idle one.
17. **Fleet learning is accidental.**
    - Earned SRE autonomy is pooled per deployment with no database column (`earned/store_evidence.go:115-120`). Verified recoveries on database A promote B, without asking whether A and B are alike.
    - Everything else learns per database: recommendations (`recommendation/candidates.go:35`), clone collapse (`analyzer/cycle.go:46`), app-managed index memory.
18. **No leader election.** Advisory locks guard only bootstrap and per-object leases (`policy/postgres_lease.go:148`). Two sidecars on one database duplicate collection, LLM spend and proposals (*inferred*).
19. **First insight takes about 10 minutes, avoidably.**
    - The collector waits a full interval before its first read (`collector.go:66-80`).
    - The analyzer's immediate run finds no snapshot (`analyzer/analyzer.go:170`, `cycle.go:20-21`), so first findings arrive at 600 s (`docs/installation.md:181`).
    - The catalog alone already has invalid and duplicate indexes, wraparound age and sequence runway.
20. **Install friction.**
    - The schema-ownership step is missing from try-it-out.md and walkthrough-linux.md (*inferred* failure for non-superusers).
    - The admin password has to be read from stderr (`README.md:56`).
    - There is no Helm chart.
    - Automatic index builds are withheld without host CPU/IO evidence (`README.md:41-43`, `executor/host_load.go:44-72`), which in practice means Supabase only.
21. **Config sprawl.**
    - 85% of the 350 fields need a restart. Only two reconfigure owners are registered (`fleet_reload.go:38`, `llm_config_owner.go:12`); any other field falls back to restart (`config/controller.go:148-153`), even `collector.interval_seconds`.
    - Nothing is derived from the database except the lock-table-based sequence cap.
22. **Managed clouds are mostly read-only.**
    - RDS, Aurora, Cloud SQL, AlloyDB, Azure, Neon and Supabase are detected (`cmd/pg_sage_sidecar/main.go:1435-1451`). ALTER SYSTEM is blocked on all of them (`executor/config_apply.go:31-37`).
    - The only cloud write is Azure `max_slot_wal_keep_size`.
    - There is no CloudWatch, Performance Insights, Cloud Monitoring or Azure Monitor ingestion; `go.mod` has only `service/rds`.
    - The README claims Lakebase monitoring, but the sidecar does not detect it.

## 3. Iterate

| # | Change | Value | Effort | Risk | Guardrail |
|---|---|---|---|---|---|
| I1 | Split collection by volatility: counters every 60 s; catalog (sizes, indexdef, FKs) every 15 min or on DDL (change feed). Store catalog rows only when their hash changes. | ~10× less storage and catalog load | M | Rules reading stale catalog | Snapshot records `catalog_as_of`; rules declare max staleness |
| I2 | Monthly-partition `sage.snapshots` and drop partitions instead of deleting rows | No bloat, instant retention | M | Migration on existing 9 GB tables | Online migration through the executor lease path |
| I3 | Compute deltas at collection (per-interval hit ratio, exec time, calls) and store rates | Real trends; cheap forecasters | M | Reset handling | Existing `StatsEpoch` and reset detection |
| I4 | Replace `buildHistoricalAverages` with day-sampled SQL; replace `COUNT(*)` with `EXISTS` | Removes the next lifeos-style overload | S | Low | Bench test with EXPLAIN loops, like the forecaster fix |
| I5 | Self-cost budget: tag every pg_sage query, read its own `pg_stat_statements` rows, export `pg_sage_self_db_time_ratio` and `sage` schema bytes, and throttle all background work (not only the collector) above a budget, e.g. 1% of DB time | pg_sage notices its own overload | M | Over-throttling during incidents | Investigations keep a reserved budget |
| I6 | Add schema and oid to FKs, page them | Missing-FK rule works on multi-tenant databases | S | Low | Existing paging tests |
| I7 | Collect immediately at start, run a catalog-only "first look" pass, and show it in the UI within 60 s | Time to value 10 min → 1 min | S | Low | Read-only |
| I8 | Wire or delete: `ForecastGrowth`/`size_history`, per-database interval knobs, `MutationsAllowed` | Honesty; fewer knobs | S | Low | — |
| I9 | Health score = worst incident severity + runway + coverage, not a finding count | Fleet triage that works | S | Low | — |
| I10 | Briefing leads with pending approvals, proposable promotions and their missing evidence, verified outcomes and pg_sage's own cost. Send it through `notify`, not stdout | Fixes "promote but nothing proposable" | S | Low | Read-only |
| I11 | Merge `alerting` into `notify` (one routing, throttle and log) | One fewer subsystem | M | Migrating channel config | Config migration and retired-keys path |
| I12 | Resolve `secret_ref` (AWS Secrets Manager / GCP Secret Manager) so agent databases join the fleet | Closes the provision→monitor loop | M | Credential handling | Never logged; read at connect |

## 4. Expand

- **Cloud telemetry adapters** (CloudWatch + Performance Insights, Cloud Monitoring, Azure Monitor). They supply host CPU/IOPS, disk capacity and logs. They unlock index-build admission and disk runway beyond Supabase, and the logs reach RDS/Cloud SQL, which LogWatch cannot.
- **Cloud parameter adapters.** RDS parameter groups and Cloud SQL flags, generalizing the Azure adapter into an allowlist. Reboot-required parameters stay at L1/approval.
- **Change context ingestion.** Built-in pollers for migration-tool tables (`flyway_schema_history`, `alembic_version`, `_prisma_migrations`, `schema_migrations`) plus GitHub deployments. They feed the change feed and the app-managed object memory, so "the app owns this index" is known before the first drop instead of after the eighth.
- **Workload identity.** Sample `userid`, `application_name` and sqlcommenter tags so findings name the service.
- **ASH-lite.** Wait events sampled every 1-5 s into an in-memory ring, persisted as per-minute rollups by (wait event, query id).
- **Fleet priors.** Schema fingerprints (table/column/index set hashes) across databases, so a verified outcome on one database becomes a prior for look-alike databases. A leaked-schema finding becomes a fleet finding.
- **Leader election.** One advisory lock per monitored database, plus a lease row, so active/passive sidecars are safe.

## 5. AI-first redesign

**The model owns the observation plan.** Today a fixed 60 s loop collects everything. Instead, a planner turn (cheap model) runs at startup, daily, and on change-feed events. It gets a database profile (catalog size, workload shape, provider, extensions, pg_sage's self-cost, incident history) and returns a typed `ObservationPlan`:

- category cadences;
- which tables need per-table stats;
- sampling for ASH;
- retention per category;
- which logs to subscribe to.

The plan is data, not SQL. Go checks it against hard bounds before applying it:

- a self-cost ceiling (default 1% of DB time and 1% of database size);
- minimum cadences that safety rules require (wraparound, locks, replication);
- a total `sage` storage budget.

**Self-configuration.** Most of the 350 knobs become *plan outputs* with operator-pinnable overrides:

- detector thresholds, learned from the database's own baseline distributions;
- `disk_capacity_bytes` from the cloud API;
- `unused_index_window_days` from observed workload periodicity (monthly jobs);
- collector batch size from catalog size.

Every derived value is a row in a `sage.config_derivation` ledger (value, evidence, model, confidence), shown in the config UI as "derived, because…". This goes through `policy.Gate` as an L0/L1 action class (`observe.config`). Narrowing observation that safety rules depend on needs approval, and loosening it never does.

**Tools the model needs** (MCP, extending `internal/mcp`):

- `get_self_cost`, `get_catalog_profile`;
- `sample_activity` (bounded ASH);
- `read_provider_metrics` and `read_provider_logs` (adapters);
- `list_changes` (exists);
- `classify_log_lines` (batches of unmatched lines);
- `compare_with_fleet_peers` (fingerprint search);
- `propose_observation_plan`.

All are read-only and use the probe runner's budgets: 500 ms for investigations, 2 s for background work.

**Logs.** The 18 regexes stay as the fast path. Unmatched lines are clustered by template (Drain-style, in Go). Each *new* template, not each line, goes to the model once, and the model's classification is cached with its rationale and an expiry. That keeps log LLM cost proportional to novelty.

**Evaluation.**

- *Replay:* the lifeos snapshot history (and future dogfoods) as a fixture. A plan must keep every finding the full collector found (recall ≥ 0.98 per rule family) at lower self-cost.
- *Shadow mode:* the derived plan runs beside the static one for 7 days, and the findings diff goes into the outcome ledger.
- *PGIncidentBench-style cases* for observation, e.g. "cache collapse on a long-uptime server" (fails today because of cumulative ratios), "50k-index catalog", "app recreates index".
- *Outcome ledger:* every missed or late incident is attributed to the observation plan that was in force.

**Learning.** Plan revisions are recorded with the outcomes they produced, as missed detections and self-cost. Fleet priors seed new databases: a new database with the fingerprint of a known one starts from that one's plan and thresholds, which also shortens time to value.

**Cost control.**

- The planner runs about once a day per database (≤5k tokens).
- Log classification is per template.
- The fleet budget is allocated by need: open incidents and pending investigations first, then a floor per database, instead of an equal split.
- Without an LLM, the static defaults remain the deterministic path.

## 6. Top 5

1. **Make pg_sage cheap and self-aware about its own footprint (I1+I2+I4+I5).** lifeos showed the AI DBA can become the incident: a forecaster overload, a 9.3 GB store inside production, and per-minute DDL-string copies. The analyzer's 7-day history read is the same bug, still live. Volatility-split collection, change-only catalog storage, partitioned snapshots and a self-cost budget enforced across all background work are prerequisites for running on any large customer database. A self-cost metric is also a cheap trust signal.
2. **Cloud telemetry and parameter adapters for RDS/Aurora and Cloud SQL.** Most paying Postgres is managed. Today pg_sage there cannot read host CPU, IO, disk capacity or logs, so index-build autonomy is withheld, the disk runway is off and LogWatch is blind. It also cannot apply a single parameter. One adapter pair (CloudWatch/PI + parameter groups) unlocks autonomy that the code already supports on Supabase.
3. **Five-minute time to value.** Collect immediately and run a catalog-only first look (invalid, duplicate and never-scanned indexes, wraparound, sequence runway, leaked schemas). Fix the install docs, and add Helm plus a "connect read-only, see findings, then grant more" path. Lead the briefing with what is waiting for approval and why promotions are not yet proposable. That is the UX gap the user hit.
4. **Model-planned observation and self-configuration with a derivation ledger.** Replace about 300 static knobs with derived values that carry evidence, are bounded in Go, shadow-tested against replay, and can be pinned by the operator. This is the AI-first version of config. It also fixes signal quality: delta-based metrics, schema-qualified FKs, and workload identity.
5. **Real fleet learning.** Add schema fingerprints, peer-based priors for thresholds and plans, fleet-level findings (leaked clone schemas, the same missing index on 30 tenants), need-based LLM budgets and leader election. Earned-autonomy pooling should require look-alike databases instead of pooling blindly. Today each database learns alone.
