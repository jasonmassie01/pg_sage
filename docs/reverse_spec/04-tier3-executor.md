# Tier 3 — Action Executor & Safety Model

Reverse-engineered from the actual code in `sidecar/internal/executor`,
`sidecar/internal/cases`, `sidecar/internal/schema`, and the orchestration in
`sidecar/cmd/pg_sage_sidecar`. File:line citations are to those paths.

---

## 1. Two parallel "Tier 3" subsystems

There are **two distinct action systems** that both call themselves Tier 3, and
they are largely **not wired to each other**:

1. **The live Executor** (`executor.Executor`, `executor.go`) — consumes
   `analyzer.Finding` values directly, gates them through `ShouldExecute`
   (`trust.go`), and runs SQL. This is the only path that actually mutates the
   database.
2. **The cases/policy projection layer** (`internal/cases` + `action_policy.go`
   + `action_contract.go`) — projects findings/incidents/query-hints into
   `Case` objects with `ActionCandidate`s and an `ActionPolicyDecision`. This is
   a **read/advisory projection** consumed only by the REST API
   (`internal/api/cases_handlers.go`) and fleet capability reporting
   (`internal/fleet/capabilities.go:133`). `cases.ProjectFinding` is **never
   called from the executor or RunCycle** (grep confirms the only non-test
   callers are API handlers).

The richer risk model (`read_only`/`safe`/`moderate`/`high`, per-action
`ActionContract`s, `EvaluateActionPolicy`) therefore governs **what the
dashboard shows and what approval metadata is attached**, not what the
autonomous loop actually executes. The autonomous loop uses the simpler
`ShouldExecute` gate. This split is the single most important thing to know
about Tier 3.

---

## 2. The trust ramp

### 2.1 ramp_start computation

`rampStart` is resolved once at startup, not from wall-clock "install date":

- YAML `trust.ramp_start` is parsed by `parseConfigRampStart`
  (`main.go:2317`, accepts date or RFC3339 forms).
- `schema.PersistTrustRampStart(ctx, pool, configRampStart)`
  (`internal/schema/bootstrap.go:89`) reads `sage.config` key
  `trust_ramp_start`. **First-write-wins semantics:**
  - If the row exists, its stored timestamp is parsed (several PG layouts
    tried) and returned — the config value is *ignored* once persisted
    (`bootstrap.go:100-116`).
  - If the row is absent, `configRampStart` is inserted if non-zero, else
    `now()` (`bootstrap.go:118+`).
- On parse failure `rampStart` falls back to `time.Now()` (`main.go:362`),
  which silently resets the ramp to day 0.
- In fleet mode each database persists its own ramp_start
  (`main.go:1160`, `metadb.go:513`), seeded from the same YAML
  `configRampStart`.

`rampAge := time.Since(rampStart)` (`trust.go:30`).

### 2.2 Trust levels and the day gates

`ShouldExecute` (`trust.go:19-60`) is the real gate. It is keyed off
`cfg.Trust.Level` (a string: `observation` / `advisory` / `autonomous`), **not**
off rampAge directly — rampAge is an *additional* requirement layered on top:

| `Trust.Level` | Behavior in `ShouldExecute` |
|---|---|
| `observation` | Always returns `false`. Findings only, no execution. |
| `advisory` | Only `ActionRisk=="safe"` **and** `Trust.Tier3Safe` **and** `rampAge ≥ 8 days` (`trust.go:36-40`). |
| `autonomous` | `safe`: `Tier3Safe && rampAge ≥ 8d`. `moderate`: `Tier3Moderate && rampAge ≥ 31d && inMaintenanceWindow(...)`. `high_risk`: always `false`. (`trust.go:42-55`) |
| anything else | `false` (`trust.go:57`). |

So the documented "OBSERVATION day0-7 / ADVISORY day8-30 / AUTONOMOUS day31+"
ramp is enforced as **day-8 and day-31 thresholds**, and the *level* is a
manually-set config string — the executor does **not** auto-promote
`observation → advisory → autonomous` as days pass. An operator must change
`trust.level`. The day gates only further restrict an already-chosen level.

Hard short-circuits at the top of `ShouldExecute` (`trust.go:26-28`): if
`emergencyStop || isReplica`, return `false` regardless of level.

### 2.3 Per-instance override

`Executor.SetTrustLevel` (`executor.go:180`) sets `trustLevelOverride`
(validated against `observation|advisory|autonomous|""`). `shouldExecute`
(`executor.go:208`) copies the config with the override applied. `TrustLevel()`
returns override-or-config.

---

## 3. Risk tiers

Two **different** risk vocabularies exist:

- **Executor/finding vocabulary** (used by `ShouldExecute`): `analyzer.Finding.ActionRisk`
  is `"safe"`, `"moderate"`, or `"high_risk"`. `high_risk` is never
  auto-executed (`trust.go:51-52`).
- **Contract/policy vocabulary** (`ActionContract.BaseRiskTier`, used by
  `EvaluateActionPolicy`): `"read_only"`, `"safe"`, `"moderate"`, `"high"`
  (`action_policy.go:50-60`). Note the names differ (`high` vs `high_risk`),
  and there is no automatic mapping between the two systems.

### 3.1 EvaluateActionPolicy (the projection-layer gate)

`action_policy.go:39`. Returns one of `execute` / `queue_for_approval` /
`blocked` / `observe_only`:

- **Hard blocks** (`hardBlockReason`, `action_policy.go:76`): emergency stop
  active; replica + non-read_only; provider unsupported.
- `read_only` → `execute`.
- `safe` (`evaluateSafePolicy`, :93): `observation`→`observe_only`;
  `approval` mode or `advisory` level→`queue_for_approval`; else requires
  `Tier3Safe && rampAge ≥ 8d` and a safe-action concurrency slot
  (`SafeActionLimit`), else `blocked`; finally `execute`.
- `moderate`/`high` (`evaluateApprovalPolicy`, :118): always
  `queue_for_approval`, `RequiresMaintenanceWindow=true`, with a
  `BlockedReason` set if outside the maintenance window (but note the decision
  is still `queue_for_approval`, not `blocked` — the window only annotates).

This logic is exercised when building approval-proposal metadata
(`executor.go:390-394`, only via the `ActionMetadataProposer` path) and in the
API. It does **not** run inside the autonomous `executeFinding` path.

---

## 4. Which actions exist

### 4.1 SQL whitelist (the real enforcement boundary)

`ValidateExecutorSQL` (`validate.go:87`) is called by every execution helper
(`ExecConcurrently`, `ExecInTransaction`, `runAnalyzeOnConn`, `ExecuteManual`,
`RollbackAction`). It is the genuine guardrail on *what SQL can run*:

- Allowed statement prefixes (`validate.go:14-30`): `CREATE INDEX` /
  `CREATE UNIQUE INDEX`, `DROP INDEX`, `REINDEX`, `VACUUM`, `ANALYZE`,
  `ALTER TABLE`, `ALTER SYSTEM SET/RESET`, `ALTER DATABASE`, `SET `, `RESET `,
  `SELECT ` (restricted), `INSERT INTO hint_plan.hints`,
  `DELETE FROM hint_plan.hints`.
- `ALTER SYSTEM` restricted to a GUC whitelist (`safeAlterSystemParams`,
  :35-67) — memory/WAL/autovacuum/planner knobs only.
- `SELECT` restricted to `pg_terminate_backend(` / `pg_cancel_backend(`
  (`allowedSelectPatterns`, :71-74).
- `ALTER TABLE` restricted to `SET (`, `RESET (`, `SET TABLESPACE`
  (`safeAlterTableSubcmds`, :78-82) — i.e. storage params, not column DDL.
- Multi-statement SQL rejected (`rejectMultiStatement`, :260).
- Protected-schema guard (`checkProtectedSchemaUsage`, :275):
  `pg_catalog`, `information_schema`, `google_ml`, `_timescaledb_*` may not be
  targeted.

### 4.2 Action types and their risk

`categorizeAction` (`executor.go:861`) labels for `action_log`:
`create_index`, `drop_index`, `reindex`, `vacuum`, `analyze`,
`terminate_backend`, `alter`, `ddl`.

`ActionContract`s exist (`action_contract.go`) for ~25 action types including
`analyze_table`, `vacuum_table`, `create_index_concurrently`,
`drop_unused_index`, `reindex_concurrently`, `set_table_autovacuum`,
`promote_role_work_mem`, `create_statistics`, `cancel_backend`,
`terminate_backend`, `alter_table` (high), `ddl_preflight` (high), and the
incident `diagnose_*` family (safe/read_only). Each contract carries
`Guardrails`, `Prechecks`, `PostChecks`, `RollbackClass`, `ProviderSupport`.
These contracts are **descriptive metadata** consumed by the policy evaluator
and API; they are not consulted by `executeFinding`.

### 4.3 Runaway-query actions

`runaway.go` implements a `warn → cancel → terminate` state machine
(`advanceState`, :200) producing findings whose `RecommendedSQL` is
`SELECT pg_cancel_backend(pid)` (`ActionRisk=safe`) then
`SELECT pg_terminate_backend(pid)` (`ActionRisk=moderate`). Self-protection:
`isSafeRunawayProcess` (:81) never targets the sidecar's own PID or any backend
whose `application_name` contains `pg_sage`, plus configured safe patterns.
Requires ≥2 observation cycles before warning (:207).

---

## 5. Action execution mechanics

### 5.1 Dispatch (`executeFinding`, `executor.go:442`)

Selection of execution mode by SQL shape:

- `analyze` → `executeAnalyze` (dedicated conn, `analyze.go`).
- `NeedsConcurrently(sql)` (contains `CONCURRENTLY`) **or** `NeedsTopLevel(sql)`
  (`VACUUM` / `ALTER SYSTEM`) → `ExecConcurrently` (raw pooled conn, **no
  transaction**). (`ddl.go:170-181`)
- else → `ExecInTransaction` (atomic). (`ddl.go:95`)

This correctly keeps `VACUUM` / `CREATE INDEX CONCURRENTLY` / `ALTER SYSTEM`
out of transactions (a known Postgres footgun).

### 5.2 Timeouts (`ddl.go`)

- `statement_timeout` = `cfg.Safety.DDLTimeout()` (`DDLTimeoutSeconds`,
  default 300s; `config.go:474`). Set per-connection in `ExecConcurrently`
  (`SET statement_timeout`, :64) or `SET LOCAL` in `ExecInTransaction` (:115).
- `lock_timeout` = `cfg.Safety.LockTimeout()` (`LockTimeoutMs`, default 30000ms;
  `config.go:478`, never returns 0 — clamps to default). Applied via
  `WithLockTimeout` option (`ddl.go:162`).
- ANALYZE uses `Tuner.AnalyzeTimeoutMs` with a 10-min safety default
  (`analyze.go:89-92`).
- Connection-level timeouts are reset to 0 before the raw connection is
  returned to the pool (`ddl.go:83-84`, `analyze.go:146-147`). In the
  transaction path `SET LOCAL` scopes automatically.

### 5.3 CONCURRENTLY / lock failure handling

`IsLockNotAvailable` matches SQLSTATE `55P03` (`ddl.go:22`) and
`wrapDDLError` wraps it as the sentinel `ErrLockNotAvailable` (:14). On lock
timeout, `executeFinding` circuit-breaks the table by stamping
`recentActions[ObjectIdentifier]` (`executor.go:468-474`), so the cascade
cooldown blocks immediate retry.

After `CREATE INDEX`, a post-check (`optimizer.CheckIndexValid`,
`executor.go:495-544`) drops an **invalid** index via
`DROP INDEX CONCURRENTLY IF EXISTS`; if the cleanup drop itself fails it logs a
CRITICAL, dispatches an `ActionFailedEvent`, and writes a durable
`drop_index_failed` row to `action_log` (`recordInvalidIndexCleanupFailure`,
:716). An INCLUDE-upgrade path drops the old index only after verifying the new
one (:547-571).

### 5.4 Rollback metadata and auto-rollback goroutine

`logAction` (`executor.go:812`) inserts into `sage.action_log` with
`before_state` (a JSON snapshot from `snapshotBeforeState`, :774 —
cache-hit ratio, active backends, write mean-exec-time), `rollback_sql`,
and `outcome`. `outcome` starts as `"monitoring"` on success, `"failed"` on
error (`actionOutcome`, :888). `acted_on_at`/`resolved` on the finding is only
set when outcome ≠ failed (`logAction:853`, `markFindingActioned:745`), so
failed actions stay retryable.

If `RollbackSQL != "" && actionID > 0`, a goroutine runs `MonitorAndRollback`
(`rollback.go:57`), tracked under `e.monitors` WaitGroup and detached via
`context.WithoutCancel(ctx)` so the window survives RunCycle/HTTP return
(`executor.go:578-588`). The monitor:

1. Waits `Trust.RollbackWindowMinutes` (default 15; `rollback.go:67`).
2. On `shutdownCh` close → marks `"interrupted"` and returns without checking
   (`rollback.go:74-80`).
3. After the window → `checkRegression` (`rollback.go:120`): regression if
   cache-hit ratio dropped by more than `Trust.RollbackThresholdPct` (default
   10) **or** INSERT/UPDATE mean-exec-time rose >20% (hardcoded, :180).
4. Regression → execute `rollback_sql` (concurrently or transactional per
   shape, 60s timeout) and mark `rolled_back`; else mark `success` with an
   `after_state` snapshot (`updateActionSuccess`, :207).

Actions with **no** rollback SQL (VACUUM, ANALYZE, pg_terminate_backend) are
marked `success` immediately (`executor.go:589-593`). `RollbackAction`
(`manual.go:108`) is the operator-triggered manual rollback (re-checks
emergency stop and validates SQL first).

---

## 6. Safety controls

### 6.1 Emergency stop (kill switch)

- `CheckEmergencyStop` (`trust.go:146`) reads `sage.config` key
  `emergency_stop`. **Fails closed**: only `ErrNoRows` means "not stopped";
  any other DB error returns `true` (`trust.go:151-158`) so a DB hiccup can't
  bypass the stop.
- `SetEmergencyStop` (`trust.go:163`) upserts the flag.
- **Checked at:** start of `RunCycle` (`executor.go:229`), inside
  `ShouldExecute` (`trust.go:26`), `ExecuteManual` (`manual.go:33`),
  `RollbackAction` (`manual.go:135`), and as a `hardBlockReason` in the policy
  evaluator (`action_policy.go:81`).
- **Gap:** the autonomous loop checks emergency stop **once** at the top of
  `RunCycle`, then iterates and executes findings. A stop set *mid-cycle* is
  not re-checked before each `executeFinding`, and the detached rollback
  goroutine does **not** re-check emergency stop before firing its rollback DDL
  after the window elapses (`rollback.go:87-108`). Manual paths do re-check.

### 6.2 Replica / HA gating

- `isReplica` comes from `haMon.Check(ctx)` in the standalone loop
  (`main.go:706-708`) and short-circuits `ShouldExecute` (`trust.go:26`).
- **Gap (fleet mode):** the fleet orchestrator calls `RunCycle(ctx, false)`
  unconditionally (`main.go:1444`) — fleet-mode executors get **no replica
  gating at all**.
- **Gap (HA safe-mode):** `ha.Monitor.InSafeMode()` (`ha.go:110`, set when
  excessive role flips/flapping is detected) is **never consulted** on any
  execution path (grep finds no caller outside the `ha` package). The flapping
  detector exists but is unwired to the executor.

### 6.3 Circuit breakers / cooldowns / hysteresis

- **DDL concurrency cap:** `ddlSem` (size `maxConcurrentDDL = 3`,
  `executor.go:65`) — non-blocking acquire; if full the finding is skipped this
  cycle (`executor.go:340-348`).
- **Cascade cooldown:** `recentActions[objID]` timestamps; `isCascadeCooldown`
  (`executor.go:625`) blocks re-action on the same object within
  `CascadeCooldownCycles × Collector.IntervalSeconds` (fallback 5 min;
  `cascadeCooldown`, :612). Pruned each cycle (`pruneRecentActions`, :642).
- **Lock-timeout circuit break:** see §5.3 — a 55P03 stamps `recentActions`.
- **Hysteresis (rollback cooldown):** `CheckHysteresis` (`rollback.go:29`)
  blocks re-executing a finding rolled back within
  `Trust.RollbackCooldownDays` (default 7). Checked in auto mode only
  (`executor.go:330`).
- **Max retries:** `exceedsMaxRetries` (`executor.go:677`,
  `maxActionRetries = 3`) — after 3 failed `action_log` rows for a finding, the
  finding is stamped `acted_on_at` to stop the retry loop.
- **Analyze cooldown + semaphore:** per-table cooldown
  (`Tuner.AnalyzeCooldownMinutes`) and a **process-wide** `analyzeSem`
  (`analyze.go`, `WithAnalyzeSemaphore`, `executor.go:96`) serialize ANALYZE
  fleet-wide; plus a size cap `AnalyzeMaxTableMB` (`analyze.go:57-68`).
- **Approval-mode dedup:** in `approval` mode, `HasPendingForFinding`,
  `HasPendingForSQL`, and recently-rejected checks
  (`HasRecentlyRejectedForFinding/SQL`) avoid re-queueing
  (`executor.go:261-312`).

### 6.4 Maintenance window

`inMaintenanceWindow` / `inMaintenanceWindowAt` (`trust.go:69-136`) parse a
minimal cron (`minute hour * * *`), plus `"always"`. A match yields a **1-hour
window** from the scheduled time. Only `moderate` autonomous actions require it
(`trust.go:50`). Note the config doc strings suggest range syntax like
`"02:00-06:00"` / `"Sun 02:00-06:00"`, but the parser only understands cron
fields and `"always"` — a range string parses to `< 2 fields` → `false`
(`trust.go:84-86`), i.e. it would **never** be in-window. This is a
config-format mismatch worth flagging.

---

## 7. Cases → actions flow (the projection layer)

`internal/cases` turns sources into `Case` objects (`case.go`):

- `ProjectFinding` (`projector.go:9`) builds a `Case` with
  `Why`/`WhyNow`/`Evidence` and `ActionCandidates`. Source type derived from
  category prefix (`forecast_`, `schema_lint` → schema_health, else finding).
- `actionCandidatesForFinding` (`projector.go:54`) dispatches:
  `migration_safety` → `migrationSafetyCandidates`; vacuum/bloat/reindex/freeze
  categories → `vacuumAutopilotCandidate` (`vacuum_autopilot.go:8`); query
  categories → `queryTuningCandidate`; else a generic candidate keyed off
  `actionTypeForSQL` (`projector.go:153`).
- **vacuum_autopilot** (`vacuum_autopilot.go`) maps categories to candidates:
  `table_bloat`→`vacuum_table` (safe), `bloat_remediation`→
  `plan_bloat_remediation` (high, script-only, blocked by default),
  `reindex_candidate`→`reindex_concurrently` (moderate),
  `xid_wraparound`→`diagnose_freeze_blockers` (safe),
  `vacuum_tuning`→`set_table_autovacuum` (moderate, PR/script),
  `blocked_vacuum`→`diagnose_vacuum_pressure` (safe). Each candidate carries
  `RiskTier`, `Confidence`, `ExpiresAt`, `OutputModes`
  (`execute`/`queue_for_approval`/`generate_pr_or_script`/`script`),
  `RollbackClass`, and a `VerificationPlan`.
- The API handler (`cases_handlers.go:495-508`) attaches an
  `ActionPolicyDecision` per candidate by calling `EvaluateActionPolicy` with a
  contract from `ContractForActionType`.

**Crucially**, executing a case action goes back through the *Executor* via the
approval queue / manual path (`ExecuteManual`), which re-validates SQL against
the `ShouldExecute` / whitelist gates — not through any case-specific executor.
A case's `ActionCandidate` with `OutputModes:["execute"]` does not by itself
cause execution; something must enqueue/approve it.

---

## 8. End-to-end flow: finding → execution → logged action

**Autonomous (auto mode):**
1. Analyzer produces `analyzer.Finding`s (with `RecommendedSQL`, `RollbackSQL`,
   `ActionRisk`).
2. `RunCycle` (`executor.go:223`): skip if `manual` mode; `CheckEmergencyStop`
   once; for each finding with non-empty `RecommendedSQL`:
   `shouldExecute` (trust+ramp+replica+emergency) → cascade cooldown →
   `lookupFindingID` → `exceedsMaxRetries` → (approval-mode branch) →
   `CheckHysteresis` → acquire `ddlSem` → `executeFinding`.
3. `executeFinding`: snapshot before-state → `ValidateExecutorSQL` (inside exec
   helper) → dispatch (analyze / concurrently / transactional) → `logAction`
   (insert `action_log`, mark finding resolved on success) → post-checks →
   spawn `MonitorAndRollback` (or immediate success).
4. After window: regression check → `rolled_back` or `success` (+`after_state`).

**Manual / approval:** finding surfaced → operator/queue calls
`ExecuteManual(findingID, sql, rollbackSQL, approvedBy)` (`manual.go:24`) →
re-validates SQL, re-checks emergency stop, verifies the SQL matches the
finding's `recommended_sql` (`verifyManualFinding`, :161), handles
CREATE INDEX coverage/invalid-blocker cleanup with up to 3 retries
(`execManualSQLWithRetry`, :231) → `logManualAction` (records `approved_by`/
`approved_at`) → same rollback-monitor path.

---

## 9. Wired vs documented-but-unwired (summary of gaps)

- **cases/contract/policy model is advisory only.** `cases.ProjectFinding`,
  `ActionContract`, and `EvaluateActionPolicy` do not gate autonomous
  execution; only `ShouldExecute` + `ValidateExecutorSQL` do. The two risk
  vocabularies (`high_risk` vs `high`) never reconcile.
- **No auto-promotion of trust level.** `trust.level` is a manual config
  string; rampAge only restricts, it does not advance the level.
- **Fleet executors have no replica gating** — `RunCycle(ctx, false)`
  (`main.go:1444`).
- **HA `InSafeMode()` (flap detection) is wired to nothing** on the execution
  path (`ha.go:110`).
- **Emergency stop is not re-checked mid-cycle** in the autonomous loop, nor by
  the detached rollback goroutine before it fires rollback DDL.
- **Maintenance-window config format mismatch:** docs imply `"HH:MM-HH:MM"`
  ranges; the parser only handles cron `minute hour ...` and `"always"`, so a
  range string is silently never-in-window.
- **Regression detection is a coarse proxy** (global cache-hit ratio + global
  INSERT/UPDATE mean time), not per-object — a healthy global metric can mask a
  regression localized to the changed table.
