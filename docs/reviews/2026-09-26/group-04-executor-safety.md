# Group 04 — Execution & Safety

Reviewer: G4 (staff Go/PostgreSQL, automated-mutation safety). Base: `b396595` (v1.5.0).
Method: end-to-end code-path reading from every mutation entry point to the `Exec` call,
cross-checked against `docs/superpowers/specs/2026-07-18-wave-0-safety-contract.md` and
`docs/reverse_spec/04-tier3-executor.md`. All paths below are relative to `sidecar/`.

## Scope

| Package / file | Non-test LOC |
|---|---|
| `internal/executor` (24 files) | 6,776 |
| `internal/policy` | 1,821 |
| `internal/autonomy` | 1,268 |
| `internal/verify` | 742 |
| `internal/rollout` | 610 |
| `internal/custodian/{freeze,wal}` | 497 |
| `internal/clone` | 273 |
| `cmd/pg_sage_sidecar/{main,wire,autonomy_runtime,rollout_runtime,config_trust_owner,mcp_*}.go` | wiring only |

Also read (to trace SQL sources into the executor): `analyzer/rules_index.go`,
`analyzer/rules_lockchain.go`, `analyzer/finding.go`, `advisor/{prompt,vacuum,validate,docground}.go`,
`tuner/tuner.go` (hint SQL), `schemaguard/*`, `ledger/*`, `retention/cleanup.go`,
`fleet/manager.go` (emergency stop), `api/{action_handlers,cases_handlers}.go`.

**Headline.** In production every executor has the standing-policy gate installed
(`main.go:753`, `main.go:1532`, `metadb.go:659`), so the legacy `EvaluateActionPolicy` path is
never used for live execution. The gate silently dropped four safety properties the legacy path
and the docs still promise: approval-required guardrails, the tier3 flags + 8/31-day trust ramp,
the configured maintenance window, and "cancel/terminate always need approval". Together with a
retention enforcer that deletes user rows outside every gate, this group contains the highest-risk
findings in the review. Default config (`trust.level: observation`) is safe; the P0s bite as soon
as an operator selects `trust.level: autonomous` + `execution_mode: auto` — the product's stated goal.

---

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G4-B01 | P0 | CONFIRMED | `executor/executor.go:363`, `executor/action_contract.go:174,201,325,459,490,593,669,704,737,801,836` | "approval required" guardrail never honored (space vs `approval_required`) → drop index, cancel backend, autovacuum tuning, reindex, hints auto-execute |
| G4-B02 | P0 | CONFIRMED | `policy/gate.go:247-271`, `policy/document.go:318-342` | Standing gate ignores `tier3_safe/moderate/high_risk`, 8/31-day ramp and `trust.maintenance_window`; default profile window is `always` |
| G4-B03 | P0 | CONFIRMED | `analyzer/rules_lockchain.go:325`, `executor/runaway.go:292`, `custodian/freeze/response.go:83-94`, `executor/executor.go:826`, `executor/custodian.go:264` | Autonomous `pg_cancel_backend(<pid>)` by raw PID, no approval, no evidence re-match; freeze path targets cluster-wide oldest-xmin backend (pg_dump, walsender, other DBs) every cycle |
| G4-B04 | P0 | CONFIRMED | `autonomy/retention_postgres.go:20-51`, `autonomy/supervisor.go:163-172` | Retention enforcer DELETEs user rows outside the gate: ignores emergency stop, `executor_enabled`, observation trust, manual mode, windows, statement_timeout |
| G4-B05 | P0 | CONFIRMED | `autonomy/wal_postgres.go:210-217`, `custodian/wal/classifier.go:73-78,95-104` | WAL "bound" sets `max_slot_wal_keep_size` to the threshold the slot already exceeds → slot invalidated at next checkpoint (replica/CDC loss), registered consumers included |
| G4-B06 | P0 | CONFIRMED | `analyzer/rules_index.go:134,182`, `executor/rollback.go:119-123`, `executor/manual.go:104-112,155-164` | DROP INDEX rollback is non-concurrent `CREATE INDEX` (ShareLock, blocks writes) run with no lock_timeout; manual-path auto-rollback runs without policy authorization |
| G4-B07 | P0 | CONFIRMED (validator) / PLAUSIBLE (reach) | `executor/validate.go:97-101,287-296`, `executor/executor.go:735-754` | Whitelist bypass: `ALTER TABLE t SET (autovacuum_…), DROP COLUMN x` passes and classifies as moderate `set_table_autovacuum`; `VACUUM  FULL` (2 spaces) classifies as safe `vacuum_table`; source is LLM advisor output |
| G4-B08 | P0 | PLAUSIBLE | `executor/executor.go:795-831`, `executor/index_verification_runtime.go:178-190`, `analyzer/optimizer_mapping.go:40` | Verified-index revert drops by LLM-authored name: `CREATE INDEX … IF NOT EXISTS` on a colliding name is a no-op, verification says `no_gain`, revert drops the user's pre-existing index |
| G4-B09 | P1 | CONFIRMED | `verify/postgres.go:121-123`, `verify/evaluate.go:67-68`, `executor/index_verification_runtime.go:180` | `IndexValid` resolves an unqualified index name via `search_path` → every autonomously created index outside `search_path` (or mixed-case) is reverted at t=0 → churn loop |
| G4-B10 | P1 | CONFIRMED | `executor/rollback.go:35-51`, `executor/executor.go:481,568,1071-1097`, `analyzer/finding.go:12,88-95` | Hysteresis/max-retries keyed on `finding_id`; finding is re-inserted with a new id 2 min after resolve → rolled-back action re-applied (flap); oscillation guard counts only `success` |
| G4-B11 | P1 | CONFIRMED | `executor/manual.go:104-112`, `executor/rollback.go:106-113` | Manual/approved path auto-rollback has no `authorize` → runs rollback DDL after executor disabled or trust lowered to observation |
| G4-B12 | P1 | CONFIRMED | `executor/rollback.go:92-147,335-374`, `executor/manual.go:121-175` | Monitor never re-reads outcome: after operator rollback it re-runs rollback or overwrites `rolled_back` → `success` and credits value |
| G4-B13 | P1 | CONFIRMED | `executor/rollback.go:118-133`, `executor/manual.go:157-174` | GUC rollback (`ALTER SYSTEM RESET …`) never reloads config → action says `rolled_back`, bad value stays live |
| G4-B14 | P1 | CONFIRMED | `cmd/pg_sage_sidecar/autonomy_runtime.go:27-32,44-48` | Custodian proposals never set `IsReplica`; autonomy ignores HA replica and safe-mode (failover flap) gating |
| G4-B15 | P1 | CONFIRMED (logic) | `policy/gate.go:58-60,200-221,247-251` | Deadline override returns `execute` before trust/mode/tier checks → outside the window, approval mode, advisory trust and high-risk approval are all bypassed |
| G4-B16 | P1 | CONFIRMED | `executor/approval_readiness.go:106-122`, `executor/action_policy.go:160-181,231-246` | Approval readiness uses legacy policy: empty `trust.maintenance_window` (default) → every moderate/high approval is 409; auto-mode ramp blocks operator approvals |
| G4-B17 | P1 | CONFIRMED | `executor/standing_policy.go:34-69`, `policy/gate.go:154-177` | Gate `Usage` never wired → rate limit (50/window), blast radius, storage budget never enforced; `RefusalSet`, `LockDurationCeilingMS`, `SerializeMode` parsed but unused |
| G4-B18 | P1 | CONFIRMED | `executor/executor.go:378-391` | `featureForFinding` defaults to `index` → cancel/terminate backend, hints, reindex, alter_table all authorized as change class `index` |
| G4-B19 | P1 | CONFIRMED (race) | `executor/executor.go:845,864,1005-1029`, `executor/custodian.go:45` | `recentActions` map written from autonomy goroutine and RunCycle goroutine without a lock → `fatal error: concurrent map writes` |
| G4-B20 | P1 | CONFIRMED | `autonomy/supervisor.go:141-161`, `executor/custodian.go:82-144` | No cooldown/backoff for custodian proposals: failing VACUUM (FREEZE) / blocker cancel re-proposed every autonomy tick (≥1 min) |
| G4-B21 | P1 | PLAUSIBLE | `executor/validate.go:37-70`, `advisor/docground.go:112-126` | ALTER SYSTEM allowlist includes `statement_timeout`, `idle_in_transaction_session_timeout`, `lock_timeout`, `max_slot_wal_keep_size` with no value bounds anywhere → LLM can set a cluster-wide 100 ms statement_timeout autonomously |
| G4-B22 | P1 | PLAUSIBLE | `executor/validate.go:27-28`, `executor/ddl.go:111-118`, `executor/manual.go:85` | `SET `/`RESET ` prefixes allowed; manual execution runs `SET search_path…`/`SET ROLE…` in a committed tx → session state leaks into pgxpool |
| G4-B23 | P1 | PLAUSIBLE | `analyzer/rules_index.go:103-106,177-181` | Drop SQL built from unquoted names → mixed-case index `"Idx_A"` emits `DROP INDEX CONCURRENTLY public.Idx_A` → folds to `idx_a` → drops a different (used) index |
| G4-B24 | P2 | CONFIRMED | `policy/window.go:252-263` vs `executor/trust.go:65-101` | Two window parsers: cron `0 2 * * *` = 1 minute in gate, 1 hour in legacy/docs; presets (`nights`, `never`, `Mon-Fri`) rejected by gate parser; no timezone |
| G4-B25 | P2 | CONFIRMED | `executor/rollback.go:77-90`, `executor/executor.go:1298-1303` | `monitoring`/`interrupted` actions never resumed after restart → no regression check, no success, no value credit |
| G4-B26 | P2 | CONFIRMED | `executor/executor.go:466,580,600`, `policy/gate.go:314-340`, `retention/cleanup.go:30-43` | Each candidate writes up to 3 `sage.decision` rows per cycle (incl. blocked/observe) in the target DB; no retention |
| G4-B27 | P2 | CONFIRMED | `ledger/postgres.go:55-61`, `executor/manual.go:325-330` | Self-audit flags every manual/approved action (no `decision_id`) forever; error logged every autonomy tick with an ever-growing list |
| G4-B28 | P2 | CONFIRMED | `verify/evaluate.go:139-145`, `executor/rollback.go:316-331` | `writeRegressed` reverts any index when pre-window writes were 0; `perQueryRegression` treats missing data as "no regression" (fail-open) |
| G4-B29 | P2 | CONFIRMED | `executor/executor.go:65,586-605`, `executor/manual.go` | `maxConcurrentDDL=3` is effectively 1 for RunCycle and unbounded for manual, autonomy and rollback paths |
| G4-B30 | P2 | CONFIRMED | `executor/manual.go:45-54,279-292,347-411` | Approving a CREATE INDEX silently drops *other* invalid indexes (prefix match) — not in approved SQL, not in `action_log` |
| G4-B31 | P2 | CONFIRMED | `executor/custodian.go:264`, `executor/rollback.go:120-122`, `executor/index_verification_runtime.go:119` | Custodian `ALTER TABLE` and all automatic rollbacks run without `lock_timeout` |
| G4-B32 | P2 | CONFIRMED | `executor/custodian.go:25-47` | `SubmitVerifiedIndexProposal` authorizes once and never re-checks emergency stop/policy immediately before DDL (spec requires) |
| G4-B33 | P2 | PLAUSIBLE | `api/action_handlers.go:240,359,580,647` | Approve/take-action runs DDL on `r.Context()`; client disconnect cancels CIC and leaves an INVALID index |
| G4-B34 | P2 | CONFIRMED | `executor/managed_config_adapter.go:47-70`, `executor/executor.go:122-126` | No production `ManagedConfigAdapter`; on RDS/Cloud SQL etc. every WAL-bound proposal fails and writes a failed action row each tick |
| G4-B35 | P3 | PLAUSIBLE | `executor/coverage_boost_test.go:1902` | `TestCoverage_ExecuteManual_ConcurrentlyPath` flake: shared `sage.config.emergency_stop` row toggled by api/fleet/executor tests running in parallel packages |
| G4-B36 | P3 | CONFIRMED | `executor/executor.go:417-428` | `policySnapshot` copies `*e.cfg` without `config.RLockForHotReload` → data race with API config apply |
| G4-B37 | P3 | CONFIRMED | `cmd/pg_sage_sidecar/main.go:537-538`, `rollout_runtime.go:28-37` | Startup warning "moderate actions will NOT execute" is false under the gate; rollout scheduler logs a warning every 6 h forever |
| G4-B38 | P3 | CONFIRMED | `autonomy/freeze_postgres.go:184-200` | Freeze XID rate counts read-only xacts, uses 1.0/s on first tick, reuses XID rate for multixacts |
| G4-B39 | P3 | PLAUSIBLE | `policy/postgres_lease.go:160-177` | If `pg_advisory_unlock` fails (5 s cleanup timeout) the conn goes back to the pool still holding a session lock; the lock is re-entrant for that conn, so exclusion breaks |

### G4-B01 — "approval required" guardrail is dead (P0, CONFIRMED)
- **Scenario.** `trust.level: autonomous`, `execution_mode: auto`, default policy profile.
  `ruleUnusedIndexes` emits `DROP INDEX CONCURRENTLY public.idx_x;`. The `drop_unused_index`
  contract declares guardrail `"approval required"`. `policyContract` (executor.go:362-366) maps only
  `approval_required` (underscore), so `req.Contract.Guardrails` is empty. `gate.go:43` does not
  queue, and `tierDecision` returns `execute` for moderate at autonomous → the index is dropped with
  no human involved. The same applies to `cancel_backend`, `set_table_autovacuum`,
  `reindex_concurrently`, `apply_query_hint`, `create_statistics`, `prepare_*` and
  `promote_role_work_mem`. The legacy check at `action_policy.go:76` has the same mismatch.
- **Root cause.** Free-text guardrail strings compared to a typed constant.
- **Fix.** Make `ActionContract.Guardrails` a `[]policy.Guardrail`, use the constant
  `policy.GuardrailApprovalRequired` in every contract, and add `Contract.Validate()` to
  `ContractForActionType` that rejects unknown guardrail strings (`policy.ValidateContract` is
  already written and unused — see D09).
- **Test.** Table test over `ContractForActionType` for every type that asserts
  `standingPolicyDecision(gate.Authorize(...)).Decision == queue_for_approval` at
  autonomous/auto when the contract says approval is required.

### G4-B02 — Gate dropped tier flags, ramp, and configured window (P0, CONFIRMED)
- **Scenario.** A new install sets `trust.level: autonomous` and leaves `tier3_moderate: false`
  (the default) intending "safe actions only". `tierDecision` (gate.go:247-271) looks only at
  `TrustLevel`/`ExecutionMode`/`RiskTier`. `rampStart`, `Tier3*` and `cfg.Trust.MaintenanceWindow`
  never reach the gate. The default profile is `unattended` (`defaults.go:181`), whose windows are
  `["always","weekends"]` (document.go:321). Result: on day 0 pg_sage creates and drops indexes,
  changes GUCs, and reindexes at any hour. The docs (`config.go:199-201`,
  `reverse_spec/04 §2`) and the startup warning (`main.go:538`) say otherwise.
- **Fix.** Put the ramp, tier flags and configured window into `policy.RuntimeState` in
  `newStandingPolicyGate`, and enforce them in `tierDecision`/`windowDecision`. Alternatively,
  delete those config keys and make the policy document the only source. Pick one; today the
  config shows controls that do nothing.
- **Test.** Gate test: autonomous + `Tier3Moderate=false` + moderate → not `execute`. Ramp age
  30 days → not `execute`. Configured window excludes now → not `execute`.

### G4-B03 — Autonomous backend cancellation by raw PID (P0, CONFIRMED given B01)
- **Scenario A.** A lock-chain root blocker has been active for longer than
  `ActiveQueryCancelMinutes`, which yields `SELECT pg_cancel_backend(4711);`. The action type is
  `cancel_backend` (moderate). With B01 (no approval) and B18 (class `index`) the gate returns
  `execute`. `executeFinding` then runs the raw SQL through `ExecInTransaction` (executor.go:826).
  There is no re-match of `query_start`/`query_id`/`application_name`, no superuser/walsender
  exclusion, and the PID comes from the previous analyzer cycle. Compare the manual path
  (`backend_signal.go:225-249`), which re-matches all of these.
- **Scenario B (freeze).** A table is RED on XID age. `oldestXminBlockerSQL` picks the
  cluster-wide oldest `backend_xmin`, which can be a `pg_dump`, a logical walsender, a backend in
  another database, or an innocent query while the real horizon is held by a slot or a prepared
  xact. `blockerResponse` emits `pg_cancel_backend`, which is attached to every RED table's
  proposal. It runs every autonomy tick (B20), cancelling a new victim each minute while freeze age
  never improves. The legacy guard (`action_policy.go:80-83`, "cancel/terminate always queue") does
  not exist in the gate.
- **Fix.** (1) In the gate, force `queue_approval` for `cancel_backend`/`terminate_backend`
  unless an explicit policy class `backend_signal` is allowed. (2) Never execute backend-signal
  SQL through `ExecInTransaction`: route it through `signalMatchingBackend` using evidence
  captured by the finding or proposal. (3) The freeze blocker must prove that the backend holds
  the horizon of *this* database (`datname = current_database()`), and must exclude
  `backend_type <> 'client backend'`, replication, and `pg_dump`/backup application names.
- **Test.** Unit: a gate with a `cancel_backend` contract at autonomous returns
  `queue_for_approval`. Integration: `executeFinding` with a cancel finding whose PID was reused
  signals nothing.

### G4-B04 — Retention enforcer deletes rows outside every gate (P0, CONFIRMED)
- **Scenario.** An operator declares a table contract (`append_only`, `retention_window 30d`).
  Later the operator sets `trust.level: observation` and activates emergency stop because of an
  incident. The autonomy supervisor (a separate goroutine from the executor) runs
  `runSchemaGuard` → `schemaRemediationRouter.Route` → `postgresRetentionEnforcer.Apply`, and
  `DELETE`s 1,000 rows per tick through `pool.Exec`. The only checks are: the contract exists, one
  prior dry-run row exists (written by the first tick itself), and
  `AllowedChangeClasses ∋ retention` (true in both built-in profiles). There is no emergency stop,
  `executor_enabled`, trust, execution-mode, replica, maintenance-window or `statement_timeout`
  check, and no action_log row. This violates wave-0: "Emergency stop blocks every mutation path".
- **Fix.** Route retention through `Executor.SubmitCustodianProposal` (or add an `Executor.Authorize`
  used by the enforcer) with a `retention_delete` contract of risk `high`. Require at least N dry
  runs spread over at least 24 h. Run the delete with `SET LOCAL statement_timeout/lock_timeout`
  and record it in `action_log`.
- **Test.** Integration: emergency stop on + contract + prior dry run → `Apply` returns an error
  and the row count is unchanged. The same holds with trust `observation` and with
  `execution_mode: manual`.

### G4-B05 — WAL "bound" invalidates the slot it is protecting (P0, CONFIRMED)
- **Scenario.** A Debezium slot lags and retains 14 GB (active). `retainedPressure` is true
  because 14 GB > 10 GB, so the decision is `ActionBound`. `boundProposal` emits
  `ALTER SYSTEM SET max_slot_wal_keep_size = '10240MB'`, which is `alter_system_guc` (moderate)
  and runs autonomously. `applyConfigChange` reloads. At the next checkpoint PostgreSQL marks the
  slot `wal_status = lost` and the CDC pipeline must re-snapshot. A physical replica slot is
  invalidated the same way and the replica has to be rebuilt. `ConsumerRegistered` (registry) is
  consulted only for drop, never for bound. The `AllowDrop:false` safety intent
  (`autonomy_runtime.go:75`) is defeated. `RetainedBytesLimit` is also not configurable (not
  wired from cfg).
- **Fix.** Bound at `max(current retained × 1.5, threshold)`, capped at a disk-derived ceiling.
  Queue for approval when the slot is active or registered. Only bound unregistered, inactive
  slots automatically.
- **Test.** Unit on `boundProposal`: retained 14 GB → proposed value > 14 GB. A registered slot
  yields no autonomous proposal.

### G4-B06 — DROP INDEX rollback blocks writes (P0, CONFIRMED)
- **Scenario.** An operator approves `DROP INDEX CONCURRENTLY public.idx_big` (unused-index
  finding). The rollback SQL is `pg_get_indexdef()` + `;` = `CREATE INDEX idx_big ON public.big
  USING btree (c);`, which is non-concurrent. `ExecuteManual` starts `MonitorAndRollback` with no
  `authorize` (manual.go:105-111). After 15 min the coarse cache-hit fallback trips (for example
  after `pg_stat_reset`). `ExecInTransaction(..., 60s)` then runs with **no lock_timeout**
  (rollback.go:122), takes ShareLock and blocks every INSERT/UPDATE/DELETE on `big` for up to
  60 s, fails on statement_timeout for a big table, and leaves `rollback_failed`. An operator
  clicking "Rollback" (`RollbackAction`) blocks writes for up to `ddl_timeout_seconds` (300 s).
  In the autonomous path the gate happens to *withhold* this rollback, because non-concurrent
  `CREATE INDEX` has no contract, so a detected regression is silently never reverted.
- **Fix.** Rewrite `IndexDef` rollbacks to `CREATE [UNIQUE] INDEX CONCURRENTLY` (insert the
  keyword after `INDEX`). In `ValidateExecutorSQL`, reject non-concurrent `CREATE INDEX`,
  `DROP INDEX` and `REINDEX` for executor paths. Always pass `WithLockTimeout` in rollbacks.
- **Test.** Unit: every `RollbackSQL` produced by `rules_index.go` satisfies
  `NeedsConcurrently`. Unit: `MonitorAndRollback` passes a lock-timeout option (inject an exec
  func).

### G4-B07 — Whitelist bypasses via ALTER TABLE lists and whitespace (P0, CONFIRMED validator; PLAUSIBLE reach)
- **Scenario.** The vacuum advisor sends table names (DB-user controlled, 63 bytes) to the LLM
  (`advisor/vacuum.go:99-108`). The LLM returns
  `ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor=0.01), DROP COLUMN email`.
  `ValidateConfigSQL` returns ok for non-ALTER SYSTEM statements. `checkAlterTableSubcmd` only
  checks `HasPrefix(sub, "SET (")`, and PostgreSQL accepts comma-separated ALTER TABLE actions.
  `isSetTableAutovacuumSQL` classifies the statement as `set_table_autovacuum` (moderate), which
  auto-executes at autonomous (with B01/B02) and drops a column. Likewise `VACUUM  FULL t` (two
  spaces) fails `HasPrefix(upper,"VACUUM FULL ")` and is classified `vacuum_table` (**safe**, runs
  even at `advisory`), taking an ACCESS EXCLUSIVE rewrite for up to 300 s. The same kind of gap
  exists for `SET TABLESPACE` (full rewrite under AEL, allowed by `safeAlterTableSubcmds`).
- **Fix.** Replace prefix matching with a real parser (`pganalyze/pg_query_go`, which is
  CGo-free via wasm, or at minimum a tokenizer that normalizes whitespace and comments).
  Require exactly one ALTER TABLE subcommand and allowlist reloption *names* and value ranges.
  Remove `SET TABLESPACE`. Classify by AST, not prefix.
- **Test.** Fuzz `ValidateExecutorSQL` + `actionTypeForProposalSQL`: any input containing
  `DROP`, `TYPE`, `FULL`, `TABLESPACE` or a second subcommand must be rejected or map to no
  contract. Add explicit cases for double space, tab/newline, `/**/` between tokens, and the comma
  list.

### G4-B08 — Revert can drop a pre-existing user index (P0, PLAUSIBLE)
- **Scenario.** A user owns `idx_orders_customer_id ON orders(customer_id, created_at)`. The
  optimizer LLM proposes `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_customer_id ON
  orders(customer_id)`. `checkDuplicate` compares column sets only, so it passes. The CIC is a
  NOTICE no-op and succeeds. The verify watch sees no latency gain → `no_gain` → `Revert`, which
  runs `rec.DropDDL` (LLM text) and drops the user's index. A second vector: `RollbackSQL` is the
  LLM's `drop_ddl`, so it can name a different, pre-existing index.
- **Fix.** Before CREATE, require `to_regclass(qualified_name) IS NULL`. After CREATE, capture
  the new index OID in `before_state`. Derive rollback as `DROP INDEX CONCURRENTLY` of that OID's
  current qualified name, and re-check the OID before dropping (the pattern already exists in
  `retained_cleanup.go`). Reject `IF NOT EXISTS` in executor CREATE INDEX.
- **Test.** Integration: pre-create a same-name index, run the verified create, force a
  revert verdict → the pre-existing index survives.

### G4-B09 — IndexValid resolves via search_path (P1, CONFIRMED)
- **Scenario.** The optimizer emits `CREATE INDEX CONCURRENTLY idx_t_a ON sales.t(a)`.
  `extractIndexName` returns `idx_t_a`. `WatchApplied` → `evaluate` → `IndexValid` runs
  `to_regclass('idx_t_a')`, which does not resolve (the index lives in `sales`) → `invalid_index`
  → immediate revert. The finding reopens 2 min later with a new id (B10), and the same full-table
  CIC plus drop repeats indefinitely. Quoted mixed-case names hit the same failure.
- **Fix.** Store the schema-qualified, quoted name (or OID) in `verifiedIndexAction.IndexName`
  and look up by OID.
- **Test.** Engine test with a fake source asserting a qualified name; integration test on a
  non-public schema.

### G4-B10 — Hysteresis keyed on a volatile finding id (P1, CONFIRMED)
- **Scenario.** An action is rolled back. The finding was set to `resolved` at execution
  (`markFindingActioned`). 2 min later `UpsertFindings` inserts a fresh row (new id), because
  `recentlyResolvedByAction` only covers a 2-minute grace. `CheckHysteresis(newID)` and
  `exceedsMaxRetries(newID)` both see nothing. `exceedsOscillationLimit` counts only
  `outcome='success'`, so `rolled_back` rows never count and the same regressing action is
  re-applied every window, contradicting `rollback_cooldown_days`.
- **Fix.** Key hysteresis, retries and oscillation on `actionIdentityKey` (category:object:type)
  or on normalized SQL. Count `rolled_back`/`reverted`/`failed`, not only `success`.
- **Test.** Integration: roll back an action, re-insert the finding with a new id → RunCycle
  skips it for the cooldown.

### G4-B11 — Manual-path auto-rollback bypasses executor/trust gates (P1, CONFIRMED)
- **Scenario.** The operator approves an action, then disables the executor for the database.
  15 min later `MonitorAndRollback` (manual.go:105) checks only `CheckEmergencyStop` and runs
  rollback DDL. Wave-0 says `executor_enabled: false` blocks every mutation path.
- **Fix.** Always pass an authorize func that calls `manualMutationBlock` plus the gate.
  Better, make `authorize` a required, non-variadic parameter.
- **Test.** Unit with an injected pool stub: executor disabled → rollback SQL is never executed.

### G4-B12 — Monitor overwrites manual rollback (P1, CONFIRMED)
- **Scenario.** At t=5 min the operator clicks Rollback, so the outcome is `rolled_back`. At
  t=15 min the monitor's `checkRegression` finds no regression, and `updateActionSuccess` sets
  `outcome='success'`, creates a success verification and calls `CreditVerifiedAction`. If it
  does detect regression instead, it re-runs the rollback SQL (a second CREATE/DROP), which fails
  → `rollback_failed`.
- **Fix.** Use compare-and-set updates: `WHERE id=$1 AND outcome='monitoring'`. Check
  `RowsAffected` and exit the monitor when it is 0. Have `RollbackAction` claim the row in the
  same way.
- **Test.** Integration: manual rollback during the window → final outcome stays `rolled_back`
  and there is no value credit.

### G4-B13 — GUC rollback not reloaded (P1, CONFIRMED)
- **Scenario.** `ALTER SYSTEM SET work_mem='512MB'` is applied and reloaded. A regression causes
  auto-rollback `ALTER SYSTEM RESET work_mem`, but there is no `pg_reload_conf()`, so 512MB stays
  live and `action_log` says `rolled_back`. `RollbackAction` has the same gap.
- **Fix.** After any successful `isAlterSystem(rollbackSQL)`, call `applyConfigChange` and record
  the in-effect note.
- **Test.** Integration: after rollback, `current_setting('work_mem')` equals the original.

### G4-B14 — Autonomy ignores HA / safe mode (P1, CONFIRMED)
- `executorProposalRouter.Route` builds `CustodianProposal` without `IsReplica`. The supervisor
  has no HA monitor, so during a failover flap the RunCycle path is suppressed (`main.go:1004-1011`)
  but the custodians keep going. ALTER SYSTEM and pg_cancel_backend both work on a hot standby.
- **Fix.** Give `newDatabaseAutonomy` the same `ha.Monitor` and set
  `IsReplica = Check() || InSafeMode()` per proposal.
- **Test.** Router test with a fake HA that reports safe mode → the gate sees `IsReplica=true`
  and returns blocked.

### G4-B15 — Deadline override bypasses trust/mode/tier (P1, CONFIRMED logic)
- `windowDecision` runs before `tierDecision`. When outside every window with a valid XID/disk
  override, it returns `execute` directly. So in `execution_mode: approval`, at `advisory` trust,
  or for **high**-risk contracts (terminate_backend, alter_table), the action executes. The same
  action inside the window would have been queued. The built-in profiles avoid this only because
  `unattended` has window `always` and `staffed` disables overrides; any custom policy
  (`weekdays 01:00-05:00` + `xid: true`) is exposed.
- **Fix.** Compute `tierDecision` first. An override can only lift the *window* restriction on
  an otherwise-`execute` verdict.
- **Test.** Gate matrix test: {approval, advisory, high} × override outside window →
  never `execute`.

### G4-B16 — Approval readiness diverges from live policy (P1, CONFIRMED)
- **Scenario.** `execution_mode: approval`, `trust.maintenance_window` empty (default). The gate
  queues `CREATE INDEX CONCURRENTLY`. When the operator clicks Approve, `withPolicyReadiness`
  → `queueForApproval` sets `RequiresMaintenanceWindow`, and `inMaintenanceWindowForPolicy("")`
  is false → HTTP 409 "outside maintenance window", permanently. In `auto` mode at autonomous with
  ramp < 31 days, a queued moderate action is refused approval with "trust ramp not satisfied".
  The operator's explicit approval is gated by automatic-execution eligibility, which the wave-0
  contract forbids.
- **Fix.** Readiness must call the same gate (`StandingPolicyGate`) with an
  "operator-approved" request flag, and use the policy document's windows.
- **Test.** API test: approval mode + empty trust window → approve succeeds inside the policy
  window.

### G4-B17 — Policy limits never enforced (P1, CONFIRMED)
- `GateConfig.Usage` is never set (standing_policy.go:34-69), so `LimitUsage{}` is always zero.
  `MaxSelfInitiatedChangesPerWindow: 50`, `MaxTablesPerWindow: 20`, `MaxRowsRewritten: 5e6` and
  the storage budget are displayed and validated but never enforced. `RefusalSet`
  (`non_dup_object_drop`, `unrollbackable`), `LockDurationCeilingMS: 3000` and `SerializeMode`
  have no consumer.
- **Fix.** Implement a `Usage` function over `sage.action_log` and `sage.decision` for the current
  window. Enforce `RefusalSet` on the contract's RollbackClass/action type.
- **Test.** Gate integration: 51st self-initiated change in window → `park/rate_limit_exceeded`.

### G4-B18 — Change-class misclassification (P1, CONFIRMED)
- `featureForFinding` maps everything except analyze, vacuum, autovacuum and GUC to `index`.
  An operator who allows only `index` has also allowed backend cancels, plan hints, reindex and
  `alter_table`. An operator who removes `index` from the allowed classes also blocks the rollback
  authorizations of those actions.
- **Fix.** Derive the class from the contract (add a `ChangeClass` field to `ActionContract`).
  Unknown maps to no class, which fails closed.
- **Test.** For every contract, `featureForFinding` equals the contract's declared class.

### G4-B19 — Concurrent map writes on recentActions (P1, CONFIRMED race)
- `SubmitVerifiedIndexProposal` (autonomy goroutine, schema-guard FK index) → `executeFinding` →
  `e.recentActions[...] = time.Now()` (executor.go:864). At the same time, RunCycle in the
  orchestrator goroutine iterates or writes the same map (`pruneRecentActions`,
  `isCascadeCooldown`, :845). Go aborts the process with `fatal error: concurrent map writes`,
  which is not recoverable, possibly mid-DDL.
- **Fix.** Guard the map with a mutex, or move it into a `sync.Map` behind small accessors.
- **Test.** `go test -race` with two goroutines calling `executeFinding` (stub pool) and `RunCycle`.

### G4-B20 — No custodian backoff (P1, CONFIRMED)
- Each tick (`autonomyInterval` ≥ 1 min) re-scans and re-submits. There is no retry cap,
  cooldown, or identity dedup like RunCycle's `exceedsMaxRetries`/oscillation guards. When the
  horizon cannot advance, the result is a full `VACUUM (FREEZE)` of a large RED table back-to-back
  forever plus a cancel every minute (B03). Each attempt also writes decision, action and
  verification rows.
- **Fix.** Persist per-(feature,target) attempt state. Back off exponentially after
  `unverifiable/failed`. Escalate to an incident and approval after K failures.
- **Test.** Supervisor test with a custodian whose verification always fails → at most K
  submissions per hour.

### G4-B21 — Dangerous GUCs in the autonomous allowlist (P1, PLAUSIBLE)
- `ALTER SYSTEM SET statement_timeout = '200ms'` is allowlisted (`validate.go:64`).
  `ValidateGUCValue` has no doc entry for it, so it returns ok. `alter_system_guc` is moderate and
  reloads immediately. The result is a cluster-wide outage for every long query, including
  pg_sage's own rollback. The same applies to `idle_in_transaction_session_timeout`, `lock_timeout`,
  `max_slot_wal_keep_size` (slot invalidation, see B05) and `jit`.
- **Fix.** Remove session-safety GUCs from the executor allowlist. Add executor-side bounds for
  every allowlisted GUC. Only `work_mem`-class knobs should be autonomous; others need approval.
- **Test.** `ValidateExecutorSQL("ALTER SYSTEM SET statement_timeout=100")` → error.

### G4-B22 — SET/RESET allowlisted → pool session leak (P1, PLAUSIBLE)
- `allowedPrefixes` contains `"SET "` and `"RESET "` with no secondary check. An LLM advisor
  recommendation such as `SET search_path = public, pg_catalog;` passes `ValidateConfigSQL` and is
  stored as `recommended_sql`. "Take action" → `ExecuteManual` → `ExecInTransaction`, which
  commits a non-LOCAL `SET` that persists on that pooled superuser connection. Unqualified
  function calls in later pg_sage queries on that connection then resolve to `public.*`, which
  enables privilege escalation by any user who can create functions in public. `SET ROLE` and
  `SET session_replication_role` leak in the same way.
- **Fix.** Delete `SET `/`RESET ` from the allowlist (no generator needs them). Defense in depth:
  run `DISCARD ALL` (or `RESET ALL`) in `resetTopLevelSession`, and after any tx that ran
  arbitrary executor SQL.
- **Test.** `ValidateExecutorSQL("SET search_path=x")` → error.

### G4-B23 — Unquoted identifiers in drop SQL (P1, PLAUSIBLE; cross-ref analyzer group)
- `fmt.Sprintf("DROP INDEX CONCURRENTLY %s.%s;", idx.SchemaName, idx.IndexRelName)`. If an
  unused index is named `"Idx_Orders"` and a used `idx_orders` exists, the used one is dropped.
  Names with spaces or dots produce errors or the wrong target.
- **Fix.** `pgx.Identifier{schema, name}.Sanitize()` everywhere SQL is built from catalog names.
- **Test.** Analyzer test with a mixed-case index name asserts the quoted output.

### G4-B24..B39 (P2/P3) — condensed
- **B24 windows.** Unify on `policy.ParseWindow`. Accept legacy presets there. Add a `timezone`
  policy field (the default container TZ is UTC, so `weekdays 01:00-05:00` is evening peak in
  the Americas). Test: `ParseWindow("0 2 * * *").Contains(02:30)` is true.
- **B25 orphaned monitors.** On startup, resume `monitoring` rows whose
  `executed_at + window` has passed. Mark `interrupted` rows for re-check. Test: restart
  simulation.
- **B26 decision growth.** Record only the final decision per candidate per cycle (or dedupe by
  identity + verdict + policy version within the cycle). Add retention for non-execute decisions.
- **B27 self-audit noise.** Have `ExecuteManual` record an "operator_approved" decision. Scope the
  audit to rows since the last audit watermark and to outcomes that should have verification.
- **B28 verify heuristics.** Treat write-baseline 0 as "insufficient" rather than regression.
  `perQueryRegression` should return "unverifiable" (not "ok") when there is no data.
- **B29 DDL concurrency.** One process-wide DDL semaphore per database shared by RunCycle,
  ExecuteManual, custodians and rollbacks.
- **B30 hidden drops.** Log each blocker drop as its own `action_log` row. Only drop invalid
  indexes whose name matches the approved CREATE (or pg_sage-owned ones).
- **B31 lock_timeout.** Pass `WithLockTimeout(cfg.Safety.LockTimeout())` in every exec path.
  Better, make it mandatory in `ExecInTransaction/ExecConcurrently` (no zero default).
- **B32 re-check.** Re-evaluate the gate after `Admit()` and immediately before
  `executeFinding` in `SubmitVerifiedIndexProposal`.
- **B33 request ctx.** Detach with `context.WithoutCancel` plus a DDL timeout for approved
  execution, and return 202 with the action id.
- **B34 managed config.** Either implement adapters or suppress WAL-bound proposals when
  `isManagedProvider && managedConfig == nil`. Emit one incident, not a row per tick.
- **B35 flaky test.** Inject `emergencyStopFn` in unit-style tests, or give each package its own
  schema/database; do not share `sage.config` across parallel packages.
- **B36 race.** Take `config.RLockForHotReload()` in `policySnapshot`, `TrustLevel` and every
  `e.cfg.*` read on hot paths, or hold an immutable snapshot pointer (wave-0 "immutable snapshot").
- **B37 misleading logs.** Delete the startup warning. Delete the rollout scheduler until a
  factory exists (see D01).
- **B38 XID rate.** Use `txid_current()` / `pg_current_xact_id()` deltas sampled per tick (costs
  one XID; alternatively `pg_control_checkpoint().next_xid` delta), and `mxid` via
  `pg_control_checkpoint().next_multixact_id`.
- **B39 lease leak.** Hijack and close the conn on unlock failure (the pattern already exists in
  `session_scope.go:discardPooledSession`).

---

## B. Dead / unwired / half-built

| ID | file:line | What | Verdict | Why |
|---|---|---|---|---|
| G4-D01 | `rollout/{engine,postgres,runtime}.go`, `cmd/.../rollout_runtime.go:13-37`, `main.go:1115` | Fleet rollout engine, run store, `NewRuntime`. `fleetRolloutFactoryState.factory` is never assigned, so the scheduler logs "runtime is deferred" every 6 h | **DELETE** scheduler stub now. Park the engine (TEST-ONLY) until an `Applier` exists | No production `Applier`/`ReVerifier`. Before it can be wired, the engine needs per-instance gate/e-stop checks, a canary soak time, rollback of canaries on aggregate regression, and halting on the *first* canary regression, not the average of N |
| G4-D02 | `schema/ddl_agent_features.go:63` | `sage.rollout_run` table: created, never written | DELETE (with D01) | No writer in prod |
| G4-D03 | `executor/rollback.go:17-31` | `RollbackMonitor`, `NewRollbackMonitor` | DELETE | Superseded by `MonitorAndRollback`. Auto-rollback *is* running via that function (executor.go:899, manual.go:104) |
| G4-D04 | `executor/executor.go:1211` | `Executor.logAction` | DELETE | Thin wrapper; callers use `logActionWithDecision` |
| G4-D05 | `custodian/freeze/scanner.go:16-66` | Multi-database freeze `Scanner` | **WIRE** (detection only) | `PostgresFreezeCustodian` scans only the connected DB. Wraparound of an unmonitored DB in the same cluster (`pg_database.datfrozenxid`) shuts down the whole cluster, and nothing watches it |
| G4-D06 | `autonomy/freeze_postgres.go:148-172` | `scanRow` | DELETE | Superseded by `scanResponseRow` |
| G4-D07 | `custodian/wal/classifier.go:63` | `wal.DefaultPolicy` | DELETE (or use it for defaults in `NewPostgresWALCustodian`) | Duplicate constants in `wal_postgres.go:42-50` |
| G4-D08 | `autonomy/supervisor.go:61-75` | `Supervisor.TriggerSchemaGuard` | DELETE | Superseded by async `RequestSchemaGuard` (post-DDL hook) |
| G4-D09 | `policy/document.go:295-308,360` | `ValidateContract`, `knownRisk` | WIRE into `ContractForActionType` / gate validation | Would have caught B01 at startup |
| G4-D10 | `policy/lease.go:121` | `LeaseKey` | DELETE | `advisoryObjectKey` is the real key |
| G4-D11 | `clone/snapshot_provider.go`, `cmd/.../mcp_migration_runtime.go:22-26` | `SnapshotProvider`. Config accepts `clone.provider: snapshot` but the factory always errors | DELETE `snapshot` from config validation (or WIRE a real client) | Config key that silently does nothing |
| G4-D12 | `executor/executor.go:122-126`, `managed_config_adapter.go` | `WithManagedConfigAdapter`: no production adapter | WIRE (RDS param group / Cloud SQL flags) or suppress proposals (B34) | Custodian GUC changes always fail on managed providers |
| G4-D13 | `policy/gate.go:154-177`, `document.go:75-82` | `Usage`, `RefusalSet`, `LockDurationCeilingMS`, `SerializeMode` | WIRE | B17 |
| G4-D14 | `config.go:197-201` (`trust.ramp_start`, `tier3_*`, `maintenance_window`) | Config keys not consumed by the live gate (only by UI readiness) | WIRE into gate or DELETE | B02/B16 |
| G4-D15 | `executor/action_policy.go`, `approval_readiness.go` | Legacy `EvaluateActionPolicy` live path (`policyGate == nil`) is unreachable in prod; used only for UI readiness and metadata | DELETE after readiness moves to the gate | Two policy engines = divergence (B16) |
| G4-D16 | `action_contract.go:723,787,820,687` | `promote_role_work_mem`, `create_statistics`, `prepare_parameterized_query`, `prepare_query_rewrite` contracts | DELETE or extend validator | `ALTER ROLE`/`CREATE STATISTICS` are rejected by `ValidateExecutorSQL`; manual "take action" on the work_mem finding always fails |
| G4-D17 | `autonomy/schema_postgres.go:254-259`, `autonomy_runtime.go:22` | `RouteCloneRehearsal`: router lacks `RouteStructuralRehearsal`, so type-tightening remediations return nil silently | WIRE (via the MCP migration runtime) or record "parked: no rehearsal runtime" | Silent no-op looks like success in the ledger |
| G4-D18 | `executor/retained_cleanup.go:16-19,147-155` | `reviewed_cleanup_required` marker | WIRE (surface in cases/approval queue) | No reader, so superseded indexes live forever |
| G4-D19 | `executor/executor.go:586-594` | "DDL concurrency limit reached" branch | DELETE with B29 fix | Unreachable: RunCycle is serial |
| G4-D20 | `executor/validate.go:26-28,77-79` | `SET `, `RESET `, `SELECT pg_reload_conf()` allowlist entries | DELETE | No generator; B22 attack surface |

---

## C. Feature improvements (ranked Impact × Effort; H/M/L)

**Standing policy gate (policy/, executor/standing_policy.go)**
1. G4-I01 (H×M) — Make the gate the only authority. Move ramp, tier flags, configured window,
   provider support (`hardBlockReason.providerSupported` is legacy-only today) and the
   cancel/terminate rule into it. Delete the legacy engine (D15).
2. G4-I02 (H×L) — Record one decision per candidate per cycle, with `evidence.cycle_id`. Link
   the executed decision to the action and mark superseded ones `superseded`.
3. G4-I03 (H×M) — Contract-derived change class, typed guardrails, `ValidateContract` at init
   (panic in tests), and a "policy explain" API that returns the exact reason chain the UI shows.
4. G4-I04 (M×L) — Policy-document timezone and a human-readable "next window opens at…" in API
   responses.

**SQL validation (executor/validate.go)**
5. G4-I05 (H×M) — AST-based validation (`pg_query_go`): single statement, exact node type, one
   ALTER TABLE subcommand, reloption allowlist with bounds, and `CONCURRENTLY` required for index
   DDL. Classification (`actionTypeForProposalSQL`, `categorizeAction`, `NeedsConcurrently` with
   `Contains`) should come from the same AST so that validation and classification cannot
   disagree (B07).
6. G4-I06 (H×L) — Executor-side GUC value bounds, shared with `advisor/docground`, so both
   layers use one table.

**Execution paths (executeFinding / ExecuteManual / custodians)**
7. G4-I07 (H×M) — One `Executor.Apply(ctx, ActionIntent)` used by RunCycle, manual, custodian,
   verified-index and retention. The steps are: authorize → lease → re-authorize →
   semaphore → exec with mandatory timeouts → log → verify. Today there are five subtly different
   pipelines, which is the root cause of B04/B11/B19/B29/B31/B32.
8. G4-I08 (M×L) — Backend signals always go through evidence-matched `signalMatchingBackend`
   (B03).
9. G4-I09 (M×M) — Run approved DDL asynchronously (202 + action id + SSE/poll) instead of inside
   the HTTP request (B33).

**Verify-and-revert (verify/, rollback.go) — F1 status: partial**
Autonomous CREATE INDEX uses the durable per-query verify engine. Other autonomous actions use
per-query regression only when the finding carries `queryid(s)`. The manual/approved path always
uses the coarse global cache-hit heuristic (`snapshotBeforeState(ctx, nil)`, manual.go:41).
10. G4-I10 (H×M) — Route every reversible action (manual, custodian, GUC) through the durable
    verify engine (`sage.verification` watches survive restarts), and retire `MonitorAndRollback`
    along with its in-memory goroutines (fixes B12/B25).
11. G4-I11 (M×L) — Bind verification and rollback to object identity (OID), not names (B08/B09).
12. G4-I12 (M×L) — When a revert is withheld by policy, queue it as an approval item instead of
    writing `rollback_skipped` and moving on.

**Custodians (autonomy/, custodian/)**
13. G4-I13 (H×M) — Durable attempt state and backoff (B20). Wire the HA gate (B14). Add cluster
    `datfrozenxid` coverage (D05).
14. G4-I14 (M×L) — WAL: headroom-based bound, registry-aware, approval for active/registered
    slots, and configurable `RetainedBytesLimit` (B05).

**Rollout (rollout/)**
15. G4-I15 (L×L now) — Delete the stub. If fleet rollout is on the roadmap, spec it as "verified
    prior → per-instance gate → canary with soak → halt on first regression → auto-revert canaries".

**Tests**
16. G4-I16 (H×M) — A *production-wiring* matrix test: build executors exactly as `main.go` does
    (with `EnableStandingPolicy`) and assert the wave-0 table (trust × mode × risk). All current
    matrix tests exercise the legacy path, which production never uses. That is how B01/B02 went
    unnoticed. Executor unit coverage is 47.5% and autonomy is 33.2%, below the 70% floor.

---

## D. Questions the user isn't asking

1. **Is the policy document or the config the source of truth for autonomy?** Today it is the
   policy document for execution and the config for the UI, and they disagree. Which one do
   operators edit, and where is the policy document editable (API? only bootstrap)?
2. **Should `unattended` really default to window `always` with every change class allowed?** In
   practice that means "autonomous = 24x7 index drops and GUC changes on day 0".
3. **Why do the authority rows (emergency stop, policy doc, ledger) live in the *monitored
   target's* `sage` schema?** Wave-0 says the meta DB is canonical and a target DB is "not an
   alternative authority source". Any app role with write access to `sage.config`/`sage.policy` on
   the target can clear emergency stop or rewrite policy.
4. **Should pg_sage ever cancel or terminate a backend without a human?** If yes, what is the
   allowlist (application_name, role, backend_type), and is `pg_dump` protected?
5. **Is retention (row deletion) a feature pg_sage should do at all**, or should it generate
   partition-drop or batch-delete scripts for the app team? A deletion bug means data loss, not a
   perf regression.
6. **What is the blast radius of an LLM prompt injection?** Table names, reloptions and query
   text flow into prompts, and LLM SQL flows into the executor. Is there a threat model and a
   red-team test corpus?
7. **What happens when the sidecar restarts mid-verification?** The durable engine resumes;
   `MonitorAndRollback` does not. Which one is the product?
8. **Is MCP retired (wave-0) or shipping?** `mcp_runtime.go` still wires production intents
   through the gate. Does multi-step migration apply re-authorize per step? (Not verified; this
   is the migration group's scope.)
9. **Who reviews `sage.decision` growth in the customer's database**, and is the ledger meant to
   be append-only forever (with implications for compliance and disk budgets)?

## E. Verification notes

- Ran (sidecar/): `go build ./...` clean. `go vet` on executor, autonomy, policy, rollout,
  verify, custodian/..., clone and cmd/pg_sage_sidecar clean.
- `go test -count=1 -cover` (unit only):

  | Package | Result | Coverage | Passes | Skips (DB-gated, `requireDB`) |
  |---|---|---|---|---|
  | executor | ok | 47.5% | 515 | 94 |
  | autonomy | ok | 33.2% | 19 | 6 |
  | policy | ok | 56.9% | 66 | 15 |
  | verify | ok | 78.0% | 58 | 2 |
  | rollout | ok | 70.0% | 25 | 1 |
  | custodian/freeze | ok | 94.4% | — | — |
  | custodian/wal | ok | 100% | — | — |
  | clone | ok | 80.0% | — | — |

  Below threshold: executor, autonomy and policy.
- Not run: integration/e2e (brief constraint), `-race` (would need DB for the executor paths
  involved). B19 is confirmed by reading only.
- All P0/P1 "CONFIRMED" items were traced by reading from source (analyzer/advisor/custodian) →
  gate → exec call. "PLAUSIBLE" items depend on LLM output shape (B07 reach, B08, B21, B22) or
  runtime naming collisions (B23).
- Could not verify: whether any deployed policy document differs from the bootstrap profiles
  (affects B15 reach); the exact root cause of the `ExecuteManual_ConcurrentlyPath` flake (B35 is
  a hypothesis based on shared `emergency_stop` toggling across packages); MCP migration
  per-step authorization.
- Stale doc claims corrected: reverse_spec 04 §6.1 says the rollback goroutine does not re-check
  emergency stop. It now does (`rollback.go:98`), but it still skips executor-enabled/trust in
  the manual path (B11). reverse_spec §2 ramp thresholds are not enforced in production (B02).
