# pg_sage Sidecar — Reverse Spec: Architecture & Process Model

> Reverse-engineered from the actual code in `sidecar/` at the time of writing.
> All citations are `file:line`. This documents reality, not aspiration.

## 1. Entry Point & Orchestration

The binary is `cmd/pg_sage_sidecar`. Its three source files split as:

- `main.go` — process lifecycle, mode dispatch, standalone + YAML-fleet
  init, Prometheus server, metrics, rate limiter, detection helpers,
  shutdown.
- `metadb.go` — meta-DB connect/bootstrap, the store-backed (meta-DB)
  fleet path, and the reconnect loop.
- `wire.go` — pure, testable HTTP-router assembly (`wireRouter`).
- `rca_adapter.go` — thin adapter wrapping the RCA engine for the
  analyzer interface.

The package uses **global mutable state** (`main.go:54-91`) — `pool`,
`cfg`, `coll`, `anal`, `exec`, `fleetMgr`, `shutdownCtx`, etc. This
contradicts the project's own "no global mutable state" rule in
`CLAUDE.md`; the entry package is the one place it is pervasive.

### Startup sequence (`main` — `main.go:93-261`)

1. `--version` short-circuit (`main.go:94-97`).
2. `config.Load(os.Args[1:])` (`main.go:100`). Fatal on error.
3. `setTrustedProxies(cfg.API.TrustedProxies)` before any listener
   (`main.go:113`) so rate-limiter IP extraction is correct from request 1.
4. **Connection setup, branching by mode:**
   - Meta-DB present (`cfg.HasMetaDB()`): `connectMetaDB` then
     `initMetaDB`; the meta pool becomes the global `pool`
     (`main.go:117-135`).
   - Else standalone (not meta, not fleet): `connectMonitoredDB` builds
     the single global `pool` (`main.go:139-153`). DSN falls back to
     `SAGE_DATABASE_URL` then a hardcoded `localhost:5432/postgres`.
   - Fleet creates its own per-DB pools later; no global pool.
5. `shutdownCtx, shutdownCancel = context.WithCancel(...)` (`main.go:156`)
   — the cancellation root for every background goroutine.
6. `go poolHealthCheck()` (`main.go:159`, body `main.go:2264`): 30s ping +
   pool-exhaustion warning. No-op in fleet (no global pool).
7. `detectCloudEnvironment()` (`main.go:162`, `main.go:2231-2262`): probes
   `aurora_version()`, `rds.extensions`, `alloydb.*`, `cloudsql.*`,
   `azure.extensions` settings → `aurora|rds|alloydb|cloud-sql|azure|self-managed`.
8. `detectExtension()` (`main.go:167`, `main.go:2210`): true only if a
   `sage` schema **and** a `sage.health_json` proc exist (the legacy C
   extension). In sidecar deployments this is always false.
9. `parseConfigRampStart` (`main.go:174`) parses `trust.ramp_start`.
10. **Mode-specific init** (`main.go:181-187`): exactly one of
    `initMetaDBFleet`, `initStandalone`, `initFleetMultiDB`.
11. `initFleetAndAPI()` (`main.go:190`) — always runs; wraps whatever was
    built into a `fleet.DatabaseManager` and starts the API server.
12. Config hot-reload watcher if `cfg.ConfigPath != ""` (`main.go:193-203`).
13. Rate limiter (`main.go:206`), Prometheus server (`main.go:210`).
14. Block on `SIGINT/SIGTERM` (`main.go:213-216`).

### The three deployment modes

Mode is selected by `cfg.Mode` (`standalone` | `fleet` | `extension`)
plus the orthogonal `--meta-db` flag. Note `extension` is a *valid* mode
in config validation (`config.go:582`) but **the entry point never
branches on it** — `main.go:181-187` only handles meta-DB, standalone,
and fleet. Extension mode is effectively dead at the process level; the
only residue is `detectExtension()` and the `pg_sage_info{mode="extension"}`
metric path (`main.go:1780`).

| Mode | Selector | Pools | Init function |
|---|---|---|---|
| **Standalone** | `mode: standalone`, no `--meta-db` | one global `pool` | `initStandalone` (`main.go:295`) |
| **YAML Fleet** | `mode: fleet`, `databases:` in YAML | one pool per DB | `initFleetMultiDB` (`main.go:828`) |
| **Meta-DB Fleet** | `--meta-db` set (any mode) | meta pool + one pool per stored DB | `initMetaDBFleet` (`metadb.go:279`) |

`HasMetaDB()` takes precedence over `IsFleet()`/`IsStandalone()` in the
connection branch (`main.go:117,139`) — a meta-DB DSN routes you to the
store-backed fleet path regardless of `mode`.

**Standalone** (`initStandalone`, `main.go:295-690`): runs `startup.RunChecks`,
`schema.Bootstrap`, admin bootstrap, builds the full single-DB pipeline
(collector, analyzer + optimizer/advisor/forecaster/tuner, executor,
briefing, alerting, autoexplain, schema-lint, migration advisor, RCA,
retention), then `go standaloneOrchestrator()`.

**YAML Fleet** (`initFleetMultiDB`, `main.go:828-1280`): iterates
`cfg.Databases`, building an independent pipeline per DB. Connection
failures register a *failed* instance (nil pool, error string) so the
dashboard shows them but they never run goroutines (`main.go:852-907`).
A shared LLM client/manager is created once (`main.go:832-833`). `application_name=pg_sage`
is stamped on every pool conn (`main.go:875`) so the analyzer/executor can
recognise their own backends.

**Meta-DB Fleet** (`initMetaDBFleet`, `metadb.go:279-299`): loads enabled
`store.DatabaseRecord`s from `sage.databases`, decrypts connection strings,
and calls `registerStoreDatabase` → `bootstrapAndRegister` per DB. Adds a
`fleetReconnectLoop` (30s ticker) that retries failed/errored instances
(`metadb.go:406-470`). This is the only mode with dynamic
add/remove/reconnect of databases at runtime.

## 2. Per-Database Goroutine Model

There is no single "agent" goroutine. Each subsystem is its own
long-lived goroutine plus a per-DB **orchestrator** that drives the
executor/briefing/retention on the analyzer cadence.

### Goroutines launched per database

| Component | Started by | Cadence | Cancellation |
|---|---|---|---|
| Collector | `coll.Run` (`main.go:394`, fleet `:968`/`:1154`) | `collector.interval_seconds` (internal ticker) | instance ctx |
| Analyzer | `anal.Run` (`main.go:580`, fleet `:1154`) | `analyzer.interval_seconds` | instance ctx |
| Orchestrator | `standaloneOrchestrator` (`main.go:686`) / `fleetDBOrchestrator` (`main.go:1251`) | `analyzer.Interval()+5s` ticker | `shutdownFlag` / instance ctx |
| Tuner revalidation | `qt.StartRevalidationLoop` (`main.go:521`, fleet `:1082`) | `revalidation_interval_hours` | ctx |
| AutoExplain collector | `aec.Run` (`main.go:641`, fleet `:1112`) | `collect_interval_seconds` | ctx |
| Schema lint | `lintRunner.Run` (`main.go:656`, fleet `:1202`) | `scan_interval_minutes` | ctx |
| Migration detector | `migDetector.Run` (`main.go:677`, fleet `:1222`) | `poll_interval_seconds` | ctx |
| Alerting (standalone only) | `alertMgr.Run` (`main.go:620`) | `check_interval_seconds` | shutdownCtx |
| Action expiry | `store.StartActionExpiry` (`main.go:604`, fleet `:1189`) | internal | ctx |
| Log-watch drain (fleet) | `runFanoutDrain` (`main.go:1364`) | `logwatch.poll_interval_ms` | shutdownCtx |

The **briefing**, **optimizer**, and **advisor** are *not* independent
goroutines. The optimizer and advisor are dependencies injected into the
analyzer (`analyzer.New(... opt, advIface ...)`, `main.go:533`) and run
inside the analyzer cycle. The briefing worker is invoked from the
orchestrator when `briefWorker.ShouldRun(now)` is true
(`main.go:717-725`, `main.go:1447-1458`).

### Orchestrator (the real per-DB control loop)

`standaloneOrchestrator` (`main.go:692-742`) ticks every
`analyzer.Interval()+5s` and, per tick: runs an HA replica check
(`haMon.Check`), `exec.RunCycle`, scheduled briefing, retention cleaner,
and fleet-status refresh + `RecordHealthSnapshots`. `fleetDBOrchestrator`
(`main.go:1425-1467`) is the per-instance equivalent minus HA and
retention, plus per-instance finding refresh.

### Lifecycle / cancellation

- **Standalone** goroutines all take `shutdownCtx` and stop on
  `shutdownCancel()`. The orchestrator also checks the global
  `shutdownFlag` bool (`main.go:700`).
- **Fleet** derives a *per-instance* context: `instCtx, instCancel :=
  context.WithCancel(shutdownCtx)` (`main.go:962`, `metadb.go:363`). The
  `instCancel` is stored on `DatabaseInstance.Cancel` and called by
  `RemoveInstance` (`manager.go:325`), so a single DB can be torn down
  (delete / edit) without affecting the rest of the fleet. Comments are
  explicit that **EmergencyStop deliberately does NOT call Cancel** —
  monitoring continues, only action execution is gated (`types.go:30-34`,
  `manager.go:196-198`).

## 3. internal/fleet

`DatabaseManager` (`manager.go:21-26`) is a mutex-guarded
`map[string]*DatabaseInstance` plus a `primaryName`.

- **Registration** (`RegisterInstance`, `manager.go:43-50`): first
  instance **with a non-nil pool** becomes primary. Failed (nil-pool)
  instances are registered for visibility but can never be primary — a
  nil-pool primary would break auth and all "all"-scoped queries.
- **Primary selection / "all" scoping** (`PoolForDatabase`,
  `manager.go:262-300`): a named DB returns that instance's pool; `""`
  or `"all"` returns the primary's pool, falling back to
  `firstConnectedPoolLocked()` (sorted-name order) if the primary is gone.
  The primary pool is what auth/session storage and `sage.databases`
  catalog writes use (`wire.go:44-52`, `main.go:815`).
- **Health score** (`computeHealthScore`, `manager.go:131-146`):
  `0` if disconnected or errored; else `100 − 25·critical − 5·warning`,
  floored at 0. Warning-only (`s.FindingsWarning`) — no `info` penalty.
  Computed lazily inside `FleetStatus` (`manager.go:93`) and persisted
  by `RecordHealthSnapshots` into `sage.health_history` (`manager.go:153-193`).
- **Instance registration in catalog**: YAML fleet upserts each DB into
  `sage.databases` via `upsertFleetDatabase` (`main.go:1391-1419`) with a
  `[]byte{0}` placeholder password (creds stay in YAML). Meta-DB fleet
  reads them back from the store.
- **EmergencyStop / Resume** (`manager.go:199-257`): toggles `inst.Stopped`
  and persists via `executor.SetEmergencyStop` into each DB's
  `sage.config` (`manager.go:230`). Empty name = whole fleet; named =
  one DB. `*Strict` variants return `ErrDatabaseNotFound`.
- **Per-DB budgets** (`budget.go`): `FleetBudget` divides
  `TotalDaily / len(databases)` equally (`budget.go:18-31`) with
  `CanSpend/Spend/Used/ResetDaily`. **This type is defined but I found no
  construction or call site in the entry point or fleet manager** — token
  budgeting in practice is enforced per-`llm.Client` (daily token budget),
  not via `FleetBudget`. Treat `FleetBudget` as currently-unwired /
  aspirational.

## 4. Startup Validation, Schema Bootstrap, Locks, Migrations

### internal/startup (`checks.go`)

`RunChecks` (`checks.go:23-65`) is **standalone-only** (`main.go:300`) and
fails the process if any check errors. It verifies: connectivity
(`SELECT 1`), PG ≥ 14000 via `server_version_num`, `pg_stat_statements`
installed + readable, and **detects (not requires)** query-text
visibility, WAL columns (`wal_records`), and plan-time columns
(`total_plan_time`) via `pg_attribute` on the view OID. Results populate
`cfg.PGVersionNum / HasWALColumns / HasPlanTimeColumns`. Fleet/meta-DB
paths skip `RunChecks` entirely and instead do a best-effort
`SHOW server_version_num` per DB (`main.go:950`, `metadb.go:477`),
defaulting to PG 14 (140000) on failure.

### internal/schema (`bootstrap.go`)

`Bootstrap` (`bootstrap.go:49-76`): acquires the advisory lock, creates
the full schema if absent or ensures each `expectedTables` entry exists,
then folds in `MigrateConfigSchema` + `migrateIncidentConstraints`. It
**never drops** objects. There are 21 `sage.*` tables (`bootstrap.go:12-37`):
`action_log`, `snapshots`, `findings`, `explain_cache`, `briefings`,
`config`, `alert_log`, `query_hints`, `users`, `sessions`, `databases`,
`notification_*` (3), `action_queue`, `incidents`, `size_history`,
`explain_results`, `schema_findings`, `crypto_meta`, `health_history`.

- **Advisory lock** (`acquireAdvisoryLock`, `bootstrap.go:157-177`): a
  *blocking* `pg_advisory_lock(hashtext('pg_sage'))` with a 30s context
  timeout — chosen over `try_advisory_lock` so concurrent sidecars/test
  packages serialize rather than spuriously fail. Standalone holds it for
  the process lifetime and releases on shutdown (`main.go:237-240`); fleet
  and meta-DB release it immediately after bootstrap per DB
  (`main.go:917`, `metadb.go:351`) — i.e. fleet does **not** hold a
  single-writer lock.
- **Migrations** are append-only idempotent DDL: `ensureTablesExist` runs
  `migrationStatements()` (`bootstrap.go:259-272`) — a fixed list of
  `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`
  blocks (OAuth cols, action-queue lifecycle cols, schema-lint absorption,
  fleet-scale indexes, etc.). `MigrateConfigSchema` (separate file) adds
  `database_id`/audit. There is no version table; idempotency is by
  `IF NOT EXISTS`.
- **Trust ramp** (`PersistTrustRampStart`, `bootstrap.go:89-155`):
  reads/inserts `trust_ramp_start` in `sage.config`, honouring a
  YAML-supplied `configRampStart` on first write and tolerating a
  concurrent-insert race by re-selecting.

### internal/smoke

End-to-end tests only (`smoke_test.go`, `chaos_test.go`) — threads
snapshot → analyzer → retention against a live PG, skipping when
`SAGE_DATABASE_URL` is unset (`smoke_test.go:36-42`). Not part of the
runtime.

### internal/selfmonitor

Despite the name, this is **not a monitoring loop** — it is a pure
self-reference *filter* (`selfmonitor.go`). `IsFinding` / `IsQueryText` /
`FindingsSQLExclusionClause` detect whether a finding/query refers to the
`sage` schema or `pg_sage` app_name, so the analyzer excludes its own
activity from findings (used at `analyzer/finding.go:129`). No goroutine,
no state.

## 5. internal/config

`config.Load` (`config.go:498-617`) precedence is **CLI > env > YAML >
defaults**: `newDefaults()` → `loadYAML` (with `${ENV}` expansion only,
bare `$` preserved — `expandBracedEnv`, `config.go:25-30`) → `overlayEnv`
(`SAGE_*` vars, `config.go:924-1000`) → CLI flag overlay → `normalize()`
→ `validate()`.

- **Modes & validation** (`config.go:582-589`, `619-672`): mode must be
  `extension|standalone|fleet`; trust level must be
  `observation|advisory|autonomous` (note: NOT `monitor`/`auto` — those
  are *execution_mode* values, a separate axis); fleet requires ≥1 DB or
  a meta-DB; fleet DB names must be non-empty and unique.
- **Normalization** (`fleet.go:89-144`): standalone with legacy
  `postgres:` config synthesizes `Databases[0]`; fleet applies
  `DefaultsConfig` (max_conns, trust_level, execution_mode default `auto`,
  intervals) to any zero-valued per-DB field.
- **Defaults** live in `internal/config/defaults.go` (consts) wired in
  `newDefaults()` (`config.go:674-883`). Notable real defaults: trust
  level, `unused_index_window_days`, lock-chain enabled with safe-patterns
  `pg_sage/replication/patroni`, forecaster `AlertHorizons [30,7,3]`,
  tuner `MaxConcurrentAnalyze`.
- **Hot reload** (`watcher.go`): `fsnotify` watch on the config path; on
  Write/Create it re-parses, validates, and applies **only** hot-reloadable
  fields via `applyHotReload` (`watcher.go:145-405`), logging
  restart-required warnings for connection/mode/listener changes
  (`warnNonReloadable`). The set is enumerated in `HotReloadable()`
  (`config.go:1003-1019`): collector/analyzer/safety/trust(level,tiers,
  window)/llm/briefing/alerting/auto_explain/forecaster/tuner/retention.
  Postgres connection, listen addrs, and mode are intentionally *not*
  reloadable. Reload mutates the live `*Config` in place under
  `w.mu`; package-level `hotReloadMu` (`config.go:38-52`) guards
  multi-field readers elsewhere.
- **Env-expansion caveat**: `overlayEnv` has a dead branch for
  `SAGE_RATE_LIMIT` that does nothing (`config.go:955-957`); the rate
  limit is actually resolved later in `RateLimit()` (`config.go:1042-1047`).

## 6. Shutdown, Circuit Breakers, Self-Monitoring

### Shutdown (`main.go:216-260`)

On signal: set `shutdownFlag=true`, call `shutdownCancel()` (cancels every
ctx-bound goroutine). A **hard 10s watchdog** `os.Exit(1)`s if graceful
shutdown stalls (`main.go:222-226`). Then, under an 8s timeout: stop the
rate limiter, release the standalone advisory lock (`main.go:237-240`),
`Shutdown` the Prometheus + API servers, stop the login limiter, and
`shutdownExecutors` (`main.go:266-293`) which drains every registered
executor's rollback monitors in parallel via a `WaitGroup`.

### Circuit breakers

- **LLM**: per-`llm.Client` breaker (`llm/client.go:18-28`,
  `IsCircuitOpen` `:86-95`, opens after N failures `:248-250`, auto-closes
  after a cooldown). Surfaced as `pg_sage_llm_circuit_open` metric
  (`main.go:1910-1915`).
- **Executor table-level**: a lock-timeout during DDL "circuit-breaks the
  table" to avoid immediate retry (`executor/executor.go:470`,
  `ddl.go:16`). This is per-table backoff, not a process breaker.
- **DB circuit breaker**: only surfaced for the *extension* path
  (`pg_sage_circuit_breaker_state{breaker="db"}` read from `sage.status()`,
  `main.go:1862-1877`) — there is no sidecar-side DB circuit breaker
  object; pool health is handled by `poolHealthCheck` pings.

### Self-monitoring

As above, `internal/selfmonitor` is a static filter, not telemetry. The
closest thing to runtime self-monitoring is `poolHealthCheck`
(`main.go:2264-2287`) and the Prometheus `/metrics` endpoint
(`main.go:1752-2024`), which exposes connection/findings/fleet/LLM/optimizer
gauges.

---

## Notable Dead / Aspirational Code

1. **`extension` mode is a phantom** — accepted by validation
   (`config.go:582`) but never dispatched in `main` (`main.go:181-187`).
   Only `detectExtension()` and an extension metrics branch remain.
2. **`fleet.FleetBudget`** (`budget.go`) is fully implemented and tested
   but **not constructed anywhere** in the runtime path; per-DB token
   budgeting is not actually wired. LLM budgeting is per-client daily
   tokens instead.
3. **`SAGE_RATE_LIMIT` overlay branch is a no-op** (`config.go:955-957`).
4. **Global mutable state** in `package main` contradicts the repo's own
   `CLAUDE.md` rule; it is the architectural outlier.
5. **`envFloat` / `strings.Contains`** kept alive only by
   `var _ = ...` suppressors (`config.go:1074-1076`).
