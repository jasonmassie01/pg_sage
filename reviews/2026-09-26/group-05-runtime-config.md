# Group 05 — Runtime orchestration, config, fleet, storage

Reviewer: G5 agent, 2026-09-26, worktree `claude/full-review-ai-sre-2026-09-26` @ `b396595`.
Read-only review. Every CONFIRMED item was traced end-to-end in source (file:line given);
PLAUSIBLE items need a live repro before fixing.

## Scope

| Area | Files | Prod LOC |
|---|---|---|
| `cmd/pg_sage_sidecar` | main.go (2838), metadb.go (733), wire.go (344), mcp_runtime.go, autonomy_runtime.go, fleet_runtime_helpers.go, agentdb_fleet.go, config_trust_owner.go, llm_config_owner.go, provider_observability.go, rollout_runtime.go, hosted_provider.go, fleet_collection_status.go | ~5,000 |
| `internal/config` | config.go (1243), watcher.go (538), controller.go, lifecycle.go, clone.go, defaults.go, fleet.go, agent_native.go | 2,964 |
| `internal/fleet` | manager.go, types.go, lifecycle_mutation.go, budget.go, capabilities.go, runtime_capabilities.go, provider_adapter.go | 1,900 |
| `internal/schema` (not lint/) | bootstrap.go (969), config_migration.go, incident_migration.go, crypto_meta.go, databases.go, ddl_agent_* | 1,720 |
| `internal/store` | config_store.go, config_helpers.go, database_store.go, action_store.go (skimmed), action_expiry.go | 3,397 |
| `internal/startup`, `internal/crypto` | checks.go, session_endpoint.go, crypto.go | 422 |

I also read, where they touch runtime wiring: `api/config_handlers.go`, `api/config_apply.go`,
`api/router.go` (config/policy/notification routes), `llm/config_owner.go`,
`llm/client_controls.go` (budget), `retention/cleanup.go`, `notify/dispatcher.go`,
`executor/standing_policy.go`, `executor/trust.go` (emergency stop), `rollout/scheduler.go`,
`briefing/briefing.go` (Dispatch), `docker-compose.yml`.

### Answers to the specific leads

| Lead | Verdict |
|---|---|
| Is YAML hot-reload dead? | **No.** It is wired via `config.NewAcknowledgedWatcherWithLoader` (main.go:248). Only the wrappers `NewWatcher`/`NewWatcherWithLoader`, `Watcher.Current` and the 261-line `applyHotReload` are dead. But it **panics in extension mode** (G5-B02) and a later API DELETE can silently revert what it applied (G5-B03). |
| Does PUT /config reach running goroutines? | Only for `trust.level` (live policy) and `llm.{enabled,endpoint,api_key,model,timeout_seconds,json_mode}`. Those reach only the *shared* LLM client (G5-B04). The lifecycle registry names 8 reconfigure owners, but only 2 are registered (`trust_policy`, `llm`) (lifecycle.go:124-146 vs. the RegisterOwner call sites). So collector interval, analyzer thresholds, alerting, briefing and tuner all fail closed to `pending_restart` in every mode. That is honest but means "restart" is the real apply path (G5-B09). Executor trust: live in standalone; in YAML fleet only through YAML reload (API writes are disabled); in meta-db mode a global trust change reaches **no** instance (G5-B14). |
| Are legacy-encrypted passwords undecryptable? | **Yes, CONFIRMED** (G5-B05). v0.8.4 and v0.8.5 used a deterministic-salt argon2 key (`git show v0.8.5:sidecar/internal/crypto/crypto.go:88`). v0.9+ uses a random salt, and `DecryptWithMigration` has no caller. |
| `schema.EnsureDatabasesTable` | Dead duplicate. `ddlDatabases` is already in `fullSchemaDDL` and `expectedTables`. **DELETE.** |
| `fleet.BuildActionFamilyReadiness` | Dead exported wrapper that hardcodes `executorEnabled=true`. **DELETE.** |
| REVERSE_SPEC says "FleetBudget never constructed" | **Stale.** It is now constructed by `initializeFleetBudget` (fleet_runtime_helpers.go:130) in YAML-fleet and meta modes. However, it denies every database not present at startup (G5-B06). |
| Key-registry drift (the "7 places" lesson) | `allowedConfigKeys` (113), `configToMap` (113) and the `hotReload` switch agree exactly. `hotReload` has 24 extra arms for keys that can no longer be written: `tuner.*`, `forecaster.*`, `briefing.*`, `auto_explain.*`, `llm.optimizer_llm.*`, `alerting.timezone` (G5-D08). The drift that matters is registry vs. **consumer**: `databases[].llm_enabled`, `databases[].collector_interval_seconds`, `databases[].analyzer_interval_seconds` and `defaults.{collector,analyzer}_interval_seconds` have no runtime consumer (G5-B07, G5-D09). |
| Default-value masking | I checked every consumer whose struct default is 0: `schema_lint.*`, `migration.*`, `analyzer.slow_slot_retained_bytes`, `safety.lock_timeout_ms`, `logwatch.*`. Each guards `<= 0` with the documented default, so there is **no live masking bug**. Env overlays cannot set a value to 0 (`envInt`/`envFloat` treat 0 as "unset", config.go:1208-1224); see G5-I09. |
| main.go size | 2,838 lines. `initFleetMultiDB` is 527 lines, `initStandalone` 443, `main` 221, `wireRouter` 109, `buildStoreDatabaseRuntime` 89, and 20 more functions exceed 50 lines (Section E). The three mode builders have drifted, which is the root cause of B08, B10, B11, B12 and B13. |

---

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G5-B01 | P0 | CONFIRMED | fleet/manager.go:252-296 | Fleet-wide emergency stop aborts on the first per-DB persist failure. The remaining databases (random map order) stay live. Any agent DB (no `sage` schema) triggers it every time. |
| G5-B02 | P1 | CONFIRMED | main.go:223-228, 247-256 | Extension mode (the default mode) has a nil `configController`. Any edit to config.yaml then causes a nil dereference in the watcher goroutine and crashes the process. |
| G5-B03 | P1 | CONFIRMED | main.go:148; api/config_handlers.go:342-357 | `DELETE /config/global/{key}` rebuilds the candidate from the startup `configBase`. This silently reverts YAML hot-reloaded values, including a live trust change. |
| G5-B04 | P1 | CONFIRMED | llm_config_owner.go:7; main.go:557,1341,1903; fleet_runtime_helpers.go:31 | LLM reconfigure only reaches the shared client. Per-DB and optimizer clients keep the old key and endpoint, **and stay enabled after `llm.enabled=false`**. The API still reports "applied". |
| G5-B05 | P1 | CONFIRMED | store/database_store.go:283,314; crypto.go:144 | Passwords encrypted by ≤ v0.8.5 cannot be decrypted after upgrade. `DecryptWithMigration` is never called, so every meta-db database shows as failed. |
| G5-B06 | P1 | CONFIRMED | fleet_runtime_helpers.go:130-143; fleet/budget.go:34-43 | `FleetBudget` is sized from the startup DB list. Databases added, renamed or discovered later get `CanSpend=false` forever, so every LLM feature fails for them. |
| G5-B07 | P1 | CONFIRMED | config/fleet.go:25,72-78; api/handlers.go:803 | `databases[].llm_enabled: false` is displayed but never enforced. Per-DB LLM clients are built anyway, sending schema and query text to the provider. |
| G5-B08 | P1 | CONFIRMED | main.go:889,1032; retention/cleanup.go:30 | The retention cleaner runs only in standalone mode. In YAML-fleet and meta-db modes, `sage.snapshots`/`findings`/`action_log`/`query_store` grow forever inside every monitored production DB. |
| G5-B09 | P1 | CONFIRMED | docker-compose.yml:64-76; api/restart_handler.go:20; main.go:101,349 | The UI "Restart now" button exits with code 42. The shipped compose file has no `restart:` policy, so the sidecar stays down. The 501 "no supervisor" branch is unreachable. |
| G5-B10 | P1 | CONFIRMED | main.go:1554-1557; metadb.go:403-492; api/router.go:181 | Notifications are split-brained. YAML fleet dispatchers read rules from each monitored DB while the UI writes them to the primary DB. Meta-db instances have no dispatcher at all. |
| G5-B11 | P1 | CONFIRMED | executor/standing_policy.go:21; api/router.go:182; metadb.go:659 | Standing-policy edits made through the UI or API go to the control pool, but executors read policy from their own monitored DB. Fleet and meta-db executors therefore never see UI policy changes. |
| G5-B12 | P1 | CONFIRMED | main.go:1100,1634; metadb.go:694; agentdb_fleet.go:143; fleet/manager.go:152 | `Status.Connected` is never set to false and `Error` is never set after startup. A database that goes down keeps health score 100 in the UI, API and Prometheus. |
| G5-B13 | P1 | CONFIRMED | main.go:464-466; collector/collector.go:220 | `HasWALColumns`/`HasPlanTimeColumns`/`PGVersionNum` are set only by standalone `RunChecks`. Fleet and meta-db collectors never read WAL or plan-time columns, and lint/migration get PG version 0. |
| G5-B14 | P2 | CONFIRMED | config_trust_owner.go:34; metadb.go:722 | In meta-db mode every instance has `TrustLevelExplicit=true`. A global `trust.level` PUT changes nothing but returns `applied:[trust.level]` with `trust_policy: applied`. |
| G5-B15 | P1 | CONFIRMED | agentdb_fleet.go:57-60 | Agent-DB fleet connections default to `sslmode=disable` for cloud-provisioned databases, so credentials cross the network in plaintext. |
| G5-B16 | P2 | CONFIRMED | agentdb_fleet.go:90-155 | Agent DBs are registered without a schema bootstrap (persist and query_store errors every cycle) and are never removed after destroy or archive. |
| G5-B17 | P2 | CONFIRMED | metadb.go:316-344 | If loading the meta-db list fails once at startup, `initMetaDBFleet` returns early. The reconnect loop never starts and the fleet budget stays nil (budget fails open). |
| G5-B18 | P2 | CONFIRMED | fleet/manager.go:285-292; capabilities.go:275-290 | `inst.Stopped` is in-memory only. After a restart with a persisted stop, the UI shows "running / ReadyForAutoSafe". Readiness also uses static config mode/trust instead of the executor's live values. |
| G5-B19 | P2 | CONFIRMED | main.go:1186-1256 | YAML fleet mode has no reconnect. A database unreachable at startup stays failed until the process restarts. |
| G5-B20 | P2 | CONFIRMED | main.go:2246-2280 | In meta-db mode the Prometheus `connection_up`/`database_size`/`cache_hit_ratio` metrics describe the **meta DB**, unlabeled. `pg_sage_mode` reports 0 for fleet. |
| G5-B21 | P2 | CONFIRMED | briefing/briefing.go:373; config.go:803; defaults.go (`DefaultMCPEnabled=true`, stdio) | The default briefing channel `stdout` writes plain text into the default MCP stdio JSON-RPC stream. |
| G5-B22 | P2 | CONFIRMED | config/config.go:499-507 | `PostgresConfig.DSN()` interpolates an unquoted password into a key/value conninfo. Passwords containing spaces or quotes break, or inject parameters (e.g. `sslmode=disable`). |
| G5-B23 | P2 | CONFIRMED | schema/incident_migration.go:8-65 | Incident CHECK constraints are dropped and re-added on **every** boot. Each boot takes an ACCESS EXCLUSIVE lock and scans the whole table. |
| G5-B24 | P2 | PLAUSIBLE | schema/config_migration.go:84-110 | `DROP config_pkey` and `CREATE UNIQUE INDEX` both swallow `WHEN others`. If the index fails, every `ON CONFLICT (key, COALESCE(database_id,0))` fails, including emergency-stop persistence. |
| G5-B25 | P2 | PLAUSIBLE | main.go:1083-1112 | Meta-db combined with `mode: standalone` (allowed by validation) registers a phantom instance on the meta pool. If a real database shares that name, the phantom evicts and tears it down. |
| G5-B26 | P2 | PLAUSIBLE | main.go:339-345; executor.go:176 | Shutdown drains only rollback monitors. In-flight `RunCycle` DDL is cancelled through `shutdownCtx`, which can leave an INVALID index from CONCURRENTLY. Fleet instance WaitGroups are never awaited. |
| G5-B27 | P2 | CONFIRMED | fleet/manager.go:259 | `setEmergencyStopped` holds the manager write lock across N network writes (5 s timeout each), freezing every fleet read during a stop. |
| G5-B28 | P3 | CONFIRMED | main.go:85,304,996,1849; metadb.go:557; main.go:1707 | Data races: `shutdownFlag`, `inst.Stopped` (read in the reconnect loop without the lock), and `inst.DatabaseID` (written after publish). |
| G5-B29 | P3 | CONFIRMED | cmd/pg_sage_sidecar/*_integration_test.go:25 | DB tests check `SAGE_TEST_DATABASE_URL != ""`, but `testdb.Run` sets a disabled sentinel DSN. 14 tests therefore FAIL after 15 s of backoff each instead of skipping (about 210 s per run). |
| G5-B30 | P3 | CONFIRMED | main.go:848,1596; migration/classifier_match.go:77,150 | The migration classifier expects a *major* version (`< 11`, `< 12`) but receives `PGVersionNum` (e.g. 110000). PG ≤ 11 rules therefore never fire (EOL versions). |

### G5-B01 (P0): fleet-wide emergency stop partially applies

- **Scenario:** Take a meta-db or YAML fleet with ≥ 2 databases where one DB's `sage.config`
  write fails. The realistic trigger is any `agentdb:*` instance registered by
  `syncAgentDBsToFleet`: it has a pool but no `sage` schema (agentdb_fleet.go:115-150).
  Operator clicks **Emergency Stop (all)**.
  - `setEmergencyStopped("")` iterates `m.instances` in random map order. On the first
    `SetEmergencyStop` error it `return changed, err` (manager.go:276-283). Instances not yet
    visited never get the persisted flag, and the failing one never gets `Stopped=true`.
  - The API returns 500 (handlers.go:989-998). The executors of the unvisited DBs keep running,
    because the kill switch is the persisted `sage.config.emergency_stop` row
    (executor/trust.go:300).
  - Retrying hits the same failure and stops another random subset each time.
- **Root cause:** fail-fast loop inside a kill switch.
- **Fix:** Iterate every instance and set `inst.Stopped` first (the in-memory gate should also
  block `RunCycle`). Collect per-DB errors with `errors.Join` and return
  `{stopped:[...], failed:[{db,err}]}`. Skip instances with no executor (agent DBs).
  Also make the executor consult an in-memory fleet stop flag, so a DB whose `sage.config` is
  unwritable is still stopped.
- **Test:** A fake manager with 3 instances whose middle `SetEmergencyStop` fails. Assert all 3
  are stopped (in memory), 2 are persisted, and the error lists exactly the failing DB. Repeat
  with shuffled insertion order 100×.

### G5-B02 (P1): extension mode crashes on config.yaml edit

- **Scenario:** `pg_sage_sidecar` started with a `config.yaml` in the working directory (auto-detected,
  config.go:529-534) and no `mode:` (`DefaultMode = "extension"`).
  - `initializeConfigController` runs only for meta or fleet (main.go:223-228) or inside
    `initStandalone` (main.go:485). In extension mode `configController` stays nil.
  - The watcher still starts (main.go:247). The first write to config.yaml calls
    `configController.Desired()` (main.go:252), a nil dereference in the fsnotify goroutine
    that has no `recover`, and the process crashes.
- **Fix:** Always construct the controller (`initializeConfigController(pool)` right after
  `config.Load`, before the mode branch). This also removes the second, legacy non-controller
  write path in `api/config_apply.go` (G5-D10).
- **Test:** Unit test that sets `cfg.Mode="extension"` and runs the watcher callback built by a
  helper extracted from main.go:248-288, with a temp YAML. Assert no panic and generation
  advances.

### G5-B03 (P1): API DELETE reverts YAML hot-reloaded values

- **Scenario:** Standalone or meta mode with a YAML path. The YAML starts with
  `trust.level: autonomous`. During an incident the operator edits the YAML to `observation`;
  the watcher applies it live (trust_policy owner). Later an admin clicks "reset to default" on
  `llm.model` → `DELETE /api/v1/config/global/llm.model`.
  - `globalCandidateWithoutOverride` clones `baseCfg`. That is `configBase`, frozen at
    main.go:148 before any reload (router.go:493).
  - The candidate therefore carries `trust.level: autonomous`. `applyLocked` sees a
    `trust.level` diff versus active and the trust owner re-escalates every executor to
    autonomous. No user asked for that.
- **Root cause:** Two sources of truth for the config baseline: the controller's desired state
  and the startup clone.
- **Fix:** Build the delete candidate from the current YAML+env (`loadConfigCandidate()`
  without the omitted override), or keep `configBase` inside the controller and update it on
  every watcher reload.
- **Test:** Start with base trust=autonomous. Apply a watcher candidate with trust=observation,
  then run DELETE on an unrelated key. Assert trust remains observation.

### G5-B04 (P1): LLM reconfigure reaches only the shared client

- **Scenario A (privacy):** Meta-db mode. Admin sets `llm.enabled=false` (for example after a
  data-egress review).
  - `planApply` sees `llm.enabled` with owner `llm` registered and commits
    `llmClient.Reconfigure` (llm/config_owner.go:35).
  - Every per-DB `dbClient` created by `buildFleetLLMFeatures` (main.go:1903) or
    `initFleetMultiDB` (main.go:1341) snapshots `cfg.LLM` at construction (llm/client.go:105).
    Those clients stay enabled, and the optimizer, advisor, tuner, RCA, briefing and plan
    narrator keep sending prompts.
  - The API returns `applied:["llm.enabled"]`.
- **Scenario B:** Rotating `llm.api_key` leaves every per-DB client and the standalone
  optimizer client (`NewOptimizerClient`, main.go:557) on the revoked key.
- **Scenario C:** Enabling LLM at runtime reports "applied", but the advisor, optimizer and RCA
  LLM hooks were never constructed (main.go:584, 704).
- **Fix:** Make `llm.Client` generations shareable. Either (a) give per-DB clients a
  `Parent *Client` whose resource snapshot they read, keeping only their budget local, or
  (b) register one owner that fans out to every live client (keep a registry in the fleet
  instance). For toggles that need construction, report `pending_restart` instead of
  `applied`.
- **Test:** Build a fleet with 2 DBs using fake LLM transport. PUT `llm.enabled=false` and
  assert zero outbound calls from per-DB clients on the next analyzer tick.

### G5-B05 (P1): upgrade from ≤ v0.8.5 makes stored credentials unreadable

- **Scenario:** A meta-db deployment on v0.8.5 with `--encryption-key` upgrades to v1.5.
  - `initMetaDB` derives `DeriveKey(pass, randomSalt)` (metadb.go:127).
  - `GetConnectionString` calls plain `crypto.Decrypt` (database_store.go:283), which returns a
    GCM auth failure. Every database registers as failed with "decrypting password" and the
    reconnect loop retries every 30 s forever.
  - `DecryptWithMigration` (crypto.go:144), which exists precisely for this case, has no caller.
- **Fix:** In `DatabaseStore`, keep the passphrase and salt. Try `Decrypt`, and on failure call
  `DecryptWithMigration`. When `needsReEncrypt` is set, `UPDATE sage.databases SET password_enc`
  in the same call (row lock) and log once. Apply the same to `GetUpdateConnectionString`.
  Additionally persist a key-check value in `sage.crypto_meta` so a wrong passphrase fails at
  startup instead of per-DB.
- **Test:** Encrypt a fixture with `DeriveKeyV2Legacy` and with `DeriveKeyV1`. Assert that
  `GetConnectionString` succeeds and that the stored bytes now decrypt with the new key.

### G5-B06 (P1): per-DB LLM budget rejects databases that joined later

- **Scenario:** `llm.fleet_token_budget_daily: 100000`, meta mode, zero databases at first boot
  (the normal fresh install). The admin adds DBs via the UI.
  - `NewBudget(total, [])` has no `perDB` entries, so `CanSpend` returns false for unknown names
    (budget.go:37-40).
  - Every LLM call fails with "per-database token budget exhausted"
    (client_controls.go:187-191).
  - The same applies to renames (`applyMetaDatabaseUpdate`) and `agentdb:*` DBs. Deleted DBs
    keep their share of the budget.
- **Fix:** Add `FleetBudget.Register(name)`/`Unregister(name)` that rebalance
  `total/len(active)` (or allocate lazily on first `CanSpend`). Call them from
  `PublishRegistration`, `commitReplacement`, `DetachInstance`. Reset at UTC midnight, not
  24 h after boot.
- **Test:** `NewBudget(100, nil)` → `Register("a")` → `CanSpend("a", 50)` is true. Rename
  "a"→"b" and assert the allocation moves.

### G5-B07 (P1): `databases[].llm_enabled: false` is not enforced

- **Scenario:** A YAML fleet has a PII database marked `llm_enabled: false`.
  - `IsLLMEnabled()` is read only for display (api/handlers.go:803).
  - `initFleetMultiDB` builds `dbLLMClient`, optimizer, advisor, tuner-LLM, RCA-LLM, plan
    narrator and lint-LLM for it unconditionally (main.go:1341-1583).
- **Fix:** Gate the per-DB LLM construction on `dbCfg.IsLLMEnabled()` (and pass nil clients).
  Add the column to `sage.databases` for meta mode.
- **Test:** Fleet init with a fake transport. Assert zero requests carry that DB's name.

### G5-B08 (P1): no retention outside standalone

- **Scenario:** A YAML fleet or meta-db deployment. `cleaner` is created only in
  `initStandalone` (main.go:889). `fleetDBOrchestrator` has no retention step (main.go:1846-1881).
  - The collector writes 11 `sage.snapshots` rows per minute per DB into the customer's
    database, plus `query_store`. Retention settings shown in the UI are no-ops.
  - Even in standalone, `health_history`, `action_queue`, `notification_log`, `size_history`,
    `alert_log`, `config_audit` and `incidents` have no purge anywhere (no
    `DELETE FROM sage.<t>` in the codebase).
- **Fix:** Construct `retention.New(dbPool, cfg, …)` per instance and run it from
  `fleetDBOrchestrator`. Extend `Cleaner.Run` with the missing tables, using
  `retention.snapshots_days` or new keys.
- **Test:** A fleet orchestrator test with an injected cleaner asserting it runs per instance.
  A retention unit test enumerating all append-only tables from `expectedTables` and failing
  when a new one has no policy.

### G5-B09 (P1): UI restart kills the default docker-compose deployment

- **Scenario:** With the lifecycle registry, almost every setting is `pending_restart`, so the
  Settings page offers **Restart now** (SettingsPage.jsx:249).
  - `triggerRestart` exits with 42 (main.go:349-351). `docker-compose.yml`'s `sidecar` service
    has no `restart:` policy (Docker default `no`), so the dashboard never comes back.
  - `restartFunc` is always set (main.go:2011), so the "no supervisor configured" 501 branch
    can never trigger.
  - Separately, if graceful shutdown exceeds 10 s, the forced path exits 1 and loses the
    restart intent (main.go:309-313).
- **Fix:** Enable restart only when a supervisor is declared (`SAGE_SUPERVISED=1` or
  `--supervised`). Add `restart: unless-stopped` to compose files. In the forced-exit
  goroutine use `restartExitCode` when `restartRequested`.
- **Test:** A router test with the supervisor flag unset expects 501. A unit test that the
  forced-exit code honours `restartRequested`.

### G5-B10 (P1): notification routing split-brain

- **YAML fleet:** The UI's notification routes use `authPool` = primary DB (router.go:181).
  Each `dbDispatcher` reads `sage.notification_rules`/`channels` from its own `dbPool`
  (main.go:1554; notify/dispatcher.go:99,130). Only the primary DB's events find rules, so
  critical findings on other DBs are silently dropped.
- **Meta-db:** `buildStoreDatabaseRuntime` never creates a dispatcher or calls
  `WithDispatcher`/`WithDatabaseName` (metadb.go:403-492), so no events are sent at all.
  Events that carry `databaseName` would be empty.
- **Fix:** One dispatcher bound to the control pool (meta pool or primary), shared by every
  instance, with per-instance `WithDatabaseName`.
- **Test:** A fleet wiring test asserting every instance's analyzer and executor dispatcher
  pool equals the router's notification pool.

### G5-B11 (P1): policy edits never reach fleet executors

- **Scenario:** Meta mode. An operator tightens policy via `POST /api/v1/policy/proposals` and
  applies it. The store is `policy.NewStore(authPool)` = meta DB (router.go:182).
  - The executor's gate reads `policy.NewStore(e.pool)` = the monitored DB
    (standing_policy.go:21), scoped by `databaseID`, and bootstraps its own default document
    there (standing_policy.go:25).
  - The UI shows the new policy while the executor enforces the old default.
  - YAML fleet has the same split for every non-primary DB, with scope `nil`
    (main.go:1532-1533).
- **Fix:** Construct the policy store from the control pool for gates and pass `databaseID`
  (YAML fleet: after `registerFleetDatabases`). Alternatively, replicate on apply, but a single
  source is simpler.
- **Test:** A wiring test asserting the executor gate's store pool equals the API policy store
  pool in each mode.

### G5-B12 (P1): a down database still shows healthy

- **Scenario:** A monitored DB becomes unreachable after startup.
  - `updateInstanceFindings` fails its query and returns early (main.go:2160-2164) without
    updating any status.
  - `Connected` is only ever set true (4 constructors) and `Error` only at initial failure.
  - `computeHealthScore` keeps 100 − penalties from stale counts (manager.go:152-165). The
    fleet overview, `pg_sage_fleet_healthy` and `pg_sage_fleet_instance_health` report it
    healthy indefinitely.
- **Fix:** On query or ping failure set `Connected=false, Error=err`, and on success clear
  them. Add staleness to `computeHealthScore` (e.g. `LastSeen` older than 3× the collector
  interval gives 0).
- **Test:** An instance with a pool whose query fails. After `updateInstanceFindings`, assert
  `HealthScore==0` and `Summary.Degraded==1`.

### G5-B13 (P1): capability detection exists only in standalone

- `startup.RunChecks` runs only in `initStandalone` (main.go:459), so fleet and meta DBs get
  **no** PG14+/pg_stat_statements prerequisite validation.
- The global `cfg.HasWALColumns/HasPlanTimeColumns` stay false, so every fleet collector uses
  the base query without WAL or plan-time columns (collector.go:220-229). Plan-time-driven
  tuner rules and WAL-heavy query analysis are blind in fleet mode.
- `cfg.PGVersionNum=0` is passed to fleet lint and migration (main.go:1579,1596).
- **Fix:** Run `RunChecks` per DB. Carry `HasWAL/HasPlan/PGVersion` on the per-DB
  collector/cfg clone rather than the global cfg. Fail or mark the instance degraded on
  prerequisite failure.
- **Test:** A fleet init test with a fake check result asserting the collector variant selects
  the WAL template.

### G5-B14 (P2): global trust.level in meta mode is a no-op reported as applied

`storeRecordToDBConfig` sets `TrustLevelExplicit: true` (metadb.go:722), so
`trustPolicyOwner.Prepare` skips every instance (config_trust_owner.go:34-36). After restart, meta
executors read `rec.TrustLevel` (metadb.go:664). The response still shows
`component_status.trust_policy="applied"`.

**Fix:** In meta mode, either classify `trust.level` as not applicable, or return a warning
("0 databases inherit global trust").

**Test:** Meta fixture PUT trust.level → assert a warning and `applied` is empty.

### G5-B15 (P1): agent DB connections default to `sslmode=disable`

`agentDeploymentToFleetConfig` defaults `sslmode` to `"disable"` (agentdb_fleet.go:57-60) for
databases the product itself provisions on RDS, Cloud SQL or Lakebase, possibly with
`allow_public_ip`. The password and all monitoring traffic travel unencrypted.

**Fix:** Default to `verify-full` (or at least `require`) and honour an explicit
`connection_info.sslmode`.

**Test:** A table test with no sslmode must not produce `disable`.

### G5-B16 (P2): agent-DB fleet entries are half-built

`connectAgentDBToFleet` registers a collector without a `schema.Bootstrap`, so every cycle logs
`snapshot persist failed` and `query_store record failed` (collector.go:97-100).
`syncAgentDBsToFleet` only adds instances (agentdb_fleet.go:98-106). Destroyed or archived
deployments remain in the fleet with a pool pointed at a deleted host, and they break emergency
stop (B01).

**Fix:** Either bootstrap (with an explicit consent flag), or give the collector an in-memory
mode. Reconcile removals by diffing `agentdb:*` names against active deployments and calling
`RemoveInstanceContext`.

### G5-B17 (P2): meta fleet silently never recovers from a startup list failure

metadb.go:329-336 returns before `initializeFleetBudget` and before `go fleetReconnectLoop`. A
10 s meta-DB blip at boot yields an empty fleet until restart, and a nil budget, so the per-DB
cap does not apply.

**Fix:** Start the reconnect loop unconditionally and let it (re)load the list, including
databases not yet registered.

### G5-B18 (P2): emergency-stop and readiness display drift

`Stopped` is never initialized from the persisted flag. After a restart during a stop, the
Overview shows running and `ReadyForAutoSafe=true` while `CheckEmergencyStop` blocks execution.
Also, `EnsureCapabilities` uses `inst.Config.ExecutionMode/TrustLevel`
(capabilities.go:280-289). The API updates the executor (`SetExecutionMode`,
`applyDatabaseTrustLevel`) but not `inst.Config`, so readiness goes stale after every per-DB
policy edit. `RampStart` is also hardcoded to now−365 d (capabilities.go:318), so readiness
ignores the trust ramp.

**Fix:** Read `executor.CheckEmergencyStop` at registration. Derive readiness from
`inst.Executor.{ExecutionMode,TrustLevel}()` and the real ramp start.

### G5-B19 to G5-B30 (brief)

- **B19:** YAML fleet registers DSN, pool and ping failures as permanent failed instances
  (main.go:1192-1256). `fleetReconnectLoop` is meta-only.
  **Fix:** Share the reconnect loop, with a YAML record source.
- **B20:** `handleMetrics` uses the global `pool`, which in meta mode is the meta DB
  (main.go:177). Unlabeled `pg_sage_connection_up`, `pg_sage_database_size_bytes` and
  `pg_sage_cache_hit_ratio` therefore describe pg_sage's own DB. Fleet metrics are gated on
  `cfg.Mode=="fleet"`, so meta+standalone or meta+unset emits none.
  **Fix:** Per-instance labeled metrics, with the meta DB under its own `role="meta"` label.
- **B21:** `Briefing.Channels` defaults to `["stdout"]` (config.go:803) and `Dispatch` uses
  `fmt.Println` (briefing.go:373). `mcp.enabled` defaults to true with stdio (defaults.go),
  serving JSON-RPC on the same stdout (mcp_runtime.go:44). When an MCP client spawns the
  sidecar, the 06:00 briefing corrupts the stream.
  **Fix:** Briefing "stdout" goes to stderr (or is disabled) when MCP stdio is active.
- **B22:** `fmt.Sprintf("host=%s … password=%s …")` (config.go:503-506). A password like
  `p@ss word` fails to connect, and `x sslmode=disable` downgrades TLS.
  **Fix:** Build a URL with `url.UserPassword` (as `DatabaseConfig.ConnString` already does),
  or quote values per libpq rules.
  **Test:** Password with space and quote round-trips through `pgx.ParseConfig`.
- **B23:** `migrateIncidentConstraints` has no "already correct" check, so it runs
  DROP+ADD CONSTRAINT (validating scan, ACCESS EXCLUSIVE) on each boot for each monitored DB,
  inside the bootstrap advisory lock with no timeout (`ctx` only).
  **Fix:** Compare `pg_get_constraintdef` first, or add `NOT VALID` then `VALIDATE`. Better,
  introduce a `sage.schema_migrations` version table (I-02).
- **B24:** config_migration.go:84-110 turns any failure into success. If the unique index is
  missing, `SetEmergencyStop`, `upsertOverride` and `persistConfigGeneration` all fail with
  "no unique constraint matching ON CONFLICT".
  **Fix:** Only swallow `duplicate_object`, and verify the index exists after the migration.
- **B25:** `config.Load` accepts `mode: standalone` together with `--meta-db` (config.go:586).
  `initFleetAndAPI` then registers `resolveDBName()` (the default `postgres`) with
  `Pool=metaPool` and nil collector/executor. `RegisterInstance` replaces and tears down any
  meta-registered DB with that name (manager.go:53-69).
  **Fix:** Reject `standalone`+meta at validation, or skip the standalone registration when
  `HasMetaDB()`.
- **B26:** `executor.Shutdown` waits only on `monitors` (executor.go:186). Orchestrators run
  `RunCycle(shutdownCtx)`. Instance `Workers` groups are never waited on during process
  shutdown (only on remove or replace).
  **Fix:** Cancel intake, then `WaitGroup` the orchestrators with the 8 s budget before closing
  pools. Run DDL on a context detached from shutdown but bounded by `ddl_timeout`.
- **B27:** Move the DB writes outside `m.mu`. Snapshot the instances under the lock, write
  without it, then set `Stopped` under the lock.
- **B28:** `shutdownFlag` is a plain bool (use `atomic.Bool` or `ctx.Done()`). `inst.Stopped`
  is read at metadb.go:557 without `m.mu`. `inst.DatabaseID = dbID` is written at main.go:1707
  after publication, racing with `GetInstanceByDatabaseID`. `go test -race` with a fleet test
  would flag these.
- **B29:** Replace `os.Getenv(testdb.EnvName) == ""` with a `testdb.Enabled()` helper that
  treats the disabled sentinel as "skip". This is also the cause of the 3 `internal/agentdb`
  baseline FAILs.
- **B30:** Pass `PGVersionNum/10000` or change the classifier contract to version numbers.
  Low impact (PG ≤ 11 are EOL, and startup rejects < 14 in standalone), but fleet mode skips
  that check (B13).

---

## B. Dead / unwired / half-built

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G5-D01 | config/watcher.go:48,54 | `NewWatcher`, `NewWatcherWithLoader` | DELETE | Superseded by `NewAcknowledgedWatcherWithLoader`. Tests should use the acknowledged form. |
| G5-D02 | config/watcher.go:271-531 | `applyHotReload` (261 lines) | DELETE | Superseded by the lifecycle registry, `changedConfigPaths` and owners. It encodes a contradictory "hot" list and violates the 50-line limit. |
| G5-D03 | config/watcher.go:534, 137 | `Watcher.Current`, `Watcher.Done` | DELETE / TEST-ONLY OK | The controller is the source of truth. `Done` is a legitimate lifecycle probe for tests. |
| G5-D04 | config/controller.go:79 | `NewConfigController` | DELETE | Thin wrapper. Tests can call `NewConfigControllerAtGeneration(…,1,…)`. |
| G5-D05 | config/config.go:1155 | `Config.HotReloadable()` | DELETE | Stale list (claims `analyzer.*`, `safety.*`, `tuner.*`, `retention.*` are hot). Contradicts `lifecycle.go`. Used only by tests and a docs contract test. |
| G5-D06 | crypto/crypto.go:84,98,144 | `DeriveKeyV1`, `DeriveKeyV2Legacy`, `DecryptWithMigration` | **WIRE** | Required for ≤ v0.8.5 upgrades (B05). |
| G5-D07 | schema/databases.go:33 | `EnsureDatabasesTable` | DELETE | `ddlDatabases` is already created by `fullSchemaDDL` and `ensureTablesExist`. |
| G5-D08 | api/config_apply.go (24 arms) | `hotReload` cases for `tuner.*`, `forecaster.*`, `briefing.*`, `auto_explain.*`, `llm.optimizer_llm.*`, `alerting.timezone` | DELETE (or WIRE into `allowedConfigKeys`) | Unreachable: validation rejects these keys, so no API write or persisted row can hit them. |
| G5-D09 | config/fleet.go:27-28,86-87 | `databases[].collector_interval_seconds`, `analyzer_interval_seconds`, `defaults.{collector,analyzer}_interval_seconds` | WIRE or DELETE | Documented YAML keys with zero consumers. The collector and analyzer read global `cfg.*.IntervalSeconds`. |
| G5-D10 | api/config_apply.go:17-51; config_handlers.go:359-376, 648-671 | Legacy non-controller write path (`applyConfigOverrides` + `hotReload(cfg)` on the live global, `reloadGlobalConfigFromStore`, `syncTrustLevelToFleet`) | DELETE after B02 fix | Reachable only when the controller is nil, which today means extension mode. Two write paths with different semantics. |
| G5-D11 | fleet/capabilities.go:98 | `BuildActionFamilyReadiness` | DELETE | Unused. Hardcodes `executorEnabled=true`, which would lie if adopted. |
| G5-D12 | rollout_runtime.go:13-24 | `fleetRolloutFactoryState.factory` never assigned | WIRE (or stop scheduling) | `startFleetRolloutScheduler` runs every 6 h in fleet and meta modes and always logs "fleet rollout runtime is deferred". `internal/rollout.NewRuntime` exists but is not constructed. |
| G5-D13 | config/config.go:1052-1054, 1236-1237 | `SAGE_RATE_LIMIT` empty branch; `var _ = envFloat` / `strings.Contains` | DELETE | No-ops (`RateLimit()` reads env directly; `envFloat` is used). |
| G5-D14 | main.go:184-189 | `SAGE_DATABASE_URL` / localhost fallback when `DSN()==""` | DELETE | `DSN()` never returns "" (defaults fill host and port). The env var is already overlaid at config.go:1025. |
| G5-D15 | api/restart_handler.go:21-25 | 501 "no supervisor" branch | WIRE | `restartFunc` is always non-nil (B09). |
| G5-D16 | main.go:2687-2705, 211-216 | `detectExtension`/`detectCloudEnvironment` run against the meta pool in meta mode | WIRE correctly | They probe pg_sage's own DB and log a misleading "mode: SIDECAR/EXTENSION" line. |
| G5-D17 | agentdb_fleet.go:115-118 | Agent DBs get collector only ("analyzer/executor … follow-up") | WIRE or DELETE | Half-built (B16). Either a full runtime via `buildStoreDatabaseRuntime` or a visibility-only card without a collector. |
| G5-D18 | fleet/budget.go:67-85 | `Used`, `Allocation` | WIRE | No API or metric exposes per-DB spend. Operators cannot see why LLM features stopped (B06). |

REVERSE_SPEC corrections for this group:

- The claim that `FleetBudget` is never constructed is fixed (it is constructed now, but B06
  applies).
- 07 §4 says "retention … via the fleet orchestrator". **False** (B08).
- 07 §7.1 says credentials are "stored unencrypted with a warning" without a key. **False**:
  `Create` refuses with a validation error (database_store.go:69-73).
- 07 §7.1 describes `DecryptWithMigration` "lazy key rotation" as working. It is unwired (B05).
- 01 describes extension mode as "never branches". Still true, but the watcher now crashes it
  (B02).

---

## C. Feature improvements (ranked by impact × effort)

### Config and hot-reload

| ID | I×E | Proposal |
|---|---|---|
| G5-I01 | H×M | **Register the missing reconfigure owners.** Collector, analyzer and alerting already take `ctx` and tickers. A `Reconfigure(interval)` that does `ticker.Reset` plus an atomic pointer swap of an immutable per-component config snapshot turns the 8 declared owners into real live reloads and shrinks the restart surface. Start with `collector.interval_seconds`, `analyzer.*` thresholds and `alerting.*`. |
| G5-I02 | H×S | **One config source of truth.** Always build the controller (B02). Delete the legacy path (D10) and `configBase` (B03). Make components read `controller.Active()` snapshots instead of the mutable global `cfg`, which removes the `hotReloadMu` convention and the races it cannot cover. |
| G5-I03 | M×S | **Honest apply results.** Add `applied_components` versus `constructed_at_startup` distinctions (B04 scenario C, B14). Include "0 targets" warnings. |
| G5-I04 | M×S | **Generate the key registry.** Derive `allowedConfigKeys`, `configToMap` and the `hotReload` switch from struct tags (`api:"int_min5"`) via reflection, like `lifecycle.go` already does for paths. This removes the "7 places" class entirely, and a CI test can assert that every allowed key has a consumer. |
| G5-I05 | M×S | Detect k8s ConfigMap `..data` symlink swaps in the watcher (watch `..data` or re-stat on any dir event). Debounce bursts (editors write 2-3 events). |
| G5-I06 | L×S | Document per-field lifecycle in the Settings UI (the generated `docs/generated/config-lifecycles.md` already exists). |

### Fleet runtime

| ID | I×E | Proposal |
|---|---|---|
| G5-I07 | H×L | **Single `buildDatabaseRuntime(spec)`** used by standalone, YAML fleet, meta-db and agent-DB. Today's three copies differ in retention, alerting, dispatcher, forecaster, lint, migration, auto_explain, tuner-without-LLM, logwatch, RunChecks, budget and policy scope (B08, B10, B11, B13). Split it into ≤ 50-line feature builders. This also brings main.go under the 500-line cap. |
| G5-I08 | H×M | **Connectivity state machine** per instance (connected → degraded → down, with reason and time). Reconnect for YAML fleet (B19). Health score decays with staleness (B12). |
| G5-I09 | M×S | Budget: `Register`/`Unregister`/rebalance, UTC-midnight reset, `/api/v1/fleet/budget` and `pg_sage_llm_budget_used{database}` metrics (B06, D18). |
| G5-I10 | M×M | Emergency stop as a **fleet-level latch** persisted in the control DB and checked by every executor in addition to per-DB rows. New or reconnected DBs inherit it, and it survives restarts (B01, B18). |
| G5-I11 | M×S | Shutdown drain: stop intake, wait for orchestrators and instance workers within budget, then close pools (B26). |

### Schema, bootstrap and storage

| ID | I×E | Proposal |
|---|---|---|
| G5-I12 | H×M | **`sage.schema_migrations(version, applied_at, checksum)`**: run each migration once, in order, under the existing advisory lock. Stop swallowing errors (B23, B24). Keep the per-boot backfill (`ddlFindingsBackfillFromSchemaFindings`) and index DDL from re-running forever. Give each step its own timeout instead of one 30 s budget for the whole batch (bootstrap.go:351). |
| G5-I13 | M×S | Crypto: key-check row plus lazy re-encrypt (B05). Support key rotation (`--encryption-key-previous`). |
| G5-I14 | M×S | Retention for every append-only table, with a CI test that fails when `expectedTables` gains a table without a retention policy (B08). |
| G5-I15 | L×S | Separate control-plane tables (`users`, `sessions`, `databases`, `config_audit`, `crypto_meta`) from monitored-DB bootstrap in meta mode. Today every monitored DB gets auth tables. |

### Startup

| ID | I×E | Proposal |
|---|---|---|
| G5-I16 | M×S | Fail fast: meta mode without `--encryption-key`; `mode: standalone`+meta (B25); wrong passphrase (key-check). |
| G5-I17 | L×S | `envInt`/`envFloat` should distinguish unset from 0 and log parse errors. Today `SAGE_VERIFY_MIN_GAIN_PCT=0` or `=abc` is silently ignored. `SAGE_PROMETHEUS_PORT` silently rebinds metrics to `0.0.0.0` (config.go:1049); document it or split out `SAGE_PROMETHEUS_ADDR`. |

---

## D. Questions the user isn't asking

1. **Which mode is the product?** Standalone gets every feature. Meta-db is the only mode with
   UI add/remove and reconnect, yet it lacks retention, alerting, notifications, forecaster,
   lint, migration advisor, auto_explain, logwatch, tuner revalidation, tuner-without-LLM and
   prerequisite checks. Is meta-db the flagship, and if so, why does it have the thinnest
   runtime?
2. **What happens with two sidecars on one meta DB (HA)?** There is no leader election. Both
   would monitor and **execute** on every DB, and the config controller's in-memory generation
   diverges from the durable one after the peer writes, so every PUT becomes 409 until restart.
   Is single-writer an explicit, documented constraint?
3. **Should pg_sage write into customer production DBs at all in fleet mode?** Snapshots,
   findings, `users`/`sessions` tables and policy documents land in every monitored DB. Meta
   mode already has a control DB; moving time-series and control tables there would remove
   B08/B10/B11-class bugs and the CREATE-privilege requirement.
4. **Is "restart to apply" acceptable UX** given that ~95% of keys are restart-bound and the
   default compose file cannot restart? What is the supported supervisor contract (compose,
   systemd, k8s)?
5. **Is the kill switch trustworthy enough to market?** It is DB-persisted per database, has no
   fleet latch, has no in-memory gate in the executor, and applies partially on failure (B01).
   A table-top drill, "stop all while one DB is unreachable", would fail today.
6. **Is MCP stdio enabled by default intentional?** Any process that can write the sidecar's
   stdin gets unauthenticated tool access (the stdio trust model), and stdout is shared with the
   briefing (B21).
7. **Upgrade testing:** no test covers "v0.8.x meta DB → current". B05 shows the upgrade path is
   untested. Is there a supported upgrade matrix?
8. **Who owns `cmd/`?** 5k LOC of global mutable state (`cfg`, `pool`, `fleetMgr`, `exec`, …)
   is the reason wiring bugs cluster here. The repo's own CLAUDE.md limits (500-line files,
   50-line functions) are violated 25 times in this group alone.

---

## E. Verification notes

**Ran:**

- `go build ./...`: clean.
- `go vet` on cmd/pg_sage_sidecar, config, fleet, startup, schema, store, crypto: clean.
- `go test -count=1 -cover` on the same packages:

| Package | Result | Coverage |
|---|---|---|
| config | ok | 84.7% |
| fleet | ok | 71.0% |
| startup | ok | 16.7% |
| schema | ok | 6.9% |
| store | ok | 27.8% |
| crypto | ok | 86.4% |
| cmd/pg_sage_sidecar | **FAIL** | 24.5% |

The cmd failures are the 14 DB-dependent tests that fail instead of skipping (B29). They are
identical to `raw-baseline-unit.txt`, so no new failures. `startup`, `schema`, `store` and `cmd`
are below the 70% business-logic floor because their DB paths only run with a real test DB.

**Function and file limit scan** (go/ast script; FUNC = function > 50 lines, FILE > 500 lines):

- main.go: `main` 221, `initStandalone` 443, `standaloneOrchestrator` 55, `initFleetMultiDB`
  527, `fleetDBOrchestrator` 56, `startAPIServer` 72, `handleMetrics` 69.
- metadb.go: `initMetaDB` 51, `connectMonitoredDBContext` 73, `buildStoreDatabaseRuntime` 89,
  `retryFailedInstances` 55.
- wire.go: `wireRouter` 109.
- config: `Load` 120, `validate` 57, `newDefaults` 248, `overlayEnv` 78,
  `overlayAgentNativeEnv` 53, `applyHotReload` 261, `applyLocked` 65, `normalize` 60,
  `Clone` 66.
- fleet: `buildActionFamilyReadiness` 76, `FleetStatus` 53, `AdapterForProvider` 74.
- schema: `PersistTrustRampStart` 67, `ReadOrCreateKDFSalt` 61, `migrateIncidentConstraints` 58.
- store: `SetDatabaseOverridesCAS` 104, `Approve` 58, `ListLedgerByFindingIDs` 56, `Update` 56.
- Files over 500 lines: main.go 2838, config.go 1243, bootstrap.go 969, metadb.go 733,
  config_helpers.go 740, action_store.go 716, config_store.go 576, watcher.go 538.

**Config key registry diff:** a scripted extraction of `allowedConfigKeys` (113),
`configToMap`/`addField` keys (113) and `hotReload` case labels (137). Allowed = map exactly;
hotReload ⊃ allowed by the 24 keys in D08.

**Git archaeology for B05:** `git show v0.8.5:sidecar/internal/crypto/crypto.go` shows
`DeriveKey(passphrase)` with a deterministic salt. Commit `3e31b97` switched to a random salt
without wiring migration.

**Not verified (needs live PG, Docker or integration tags, which the brief disallows):**

- The runtime reproduction of B01 against a real agent DB.
- B23 lock duration on a large `sage.incidents`.
- B24 failure mode.
- B26 INVALID-index outcome on shutdown.
- The k8s ConfigMap watcher behaviour (I05).
- `-race` runs of fleet tests, so B28 is from reading only.

B25 (meta + standalone) and B26 are PLAUSIBLE. Everything else marked CONFIRMED was traced
through source end-to-end.
