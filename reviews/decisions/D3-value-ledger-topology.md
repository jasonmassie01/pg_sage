# D3 (SURF-03) — Value ledger topology across fleets

**Current state:** the ledger is written into each *monitored* database's `sage.action_log`, but every reader (API, MCP, Prometheus) reads one "control" pool, so fleet shows only the primary DB and meta-db shows zero.
**Recommendation:** Option A: the ledger stays with the change (per-target `action_log`), and all readers fan out across fleet pools with the instance name as the label. Also exempt credited rows from retention.
**Door type:** two-way. Reads only, with no change to stored data semantics. The retention exemption keeps more data and deletes none.

Worktree HEAD `f99a302`. All paths are relative to `sidecar/`.

## 1. What the code does today

**Storage (schema).**
- `sage.action_log` gains `database_id bigint` (nullable, no FK), `toil_minutes_saved` and `toil_model_version` (`internal/schema/ddl_agent_value.go:41-44`).
- `sage.incident_avoided` is the second ledger table (`ddl_agent_value.go:46-65`).
- `sage.value_rollup` is a view (`ddl_agent_value.go:67-114`). No Go code reads it. The only references are in `internal/schema`.
- Every bootstrap creates `sage.databases` (`internal/schema/bootstrap.go:50`). Target DBs therefore have their own, usually empty, registry.

**Writers.**
- Action rows are inserted with `database_id = e.databaseIDValue()` (`internal/executor/executor.go:1177-1185`, `manual.go:403-412`). The value comes from `e.databaseID`, which is set only by `EnableStandingPolicyWithStore` (`executor/standing_policy.go:41`, `operator_decision.go:89-90`).
- Credit is stamped via `value.CreditVerifiedAction` on the **executor's pool**:
  - after a retained index verdict (`executor/index_verification_runtime.go:87-89`)
  - after post-action checks pass (`executor/rollback_eval.go:249-251`)
- Stamping is an idempotent `UPDATE ... COALESCE` that requires a completed successful verification (`internal/value/postgres.go:66-83`).
- Retraction on rollback is done by `ZeroCreditOnRevert` on the executor pool (`executor/rollback.go:120`, `manual.go:212`, `index_verification_runtime.go:158`, `value/postgres.go:91-109`).
- Pending "potential" value comes from `sage.action_queue`, written by `store.NewActionStore(<target pool>)` (`internal/store/action_store.go:88-102`).
- `RecordIncident` (`value/postgres.go:111-127`) has **no production caller**; the only callers are tests in `internal/value`. `incident_avoided` is therefore always empty. This is a separate gap and is out of scope here.

**Readers.**
- REST: `GET /api/v1/value` is built on the single `pool` given to the router (`internal/api/router.go:157-158`). That pool is `authPool` (`cmd/pg_sage_sidecar/wire.go:57-65,144-145`). The handler takes `?database=<name>&since&until` (`api/value_handlers.go:41-60`).
- Query: `readRealized` LEFT JOINs `sage.databases` *in the same pool* and filters `d.name=$1` (`value/postgres.go:132-139`). It now COALESCEs a NULL name to `''` (fix 09fff77, `null_database_test.go:9`).
- MCP: `fleetMCPAccess.GetValue` calls `adapter(nil)` (`cmd/pg_sage_sidecar/mcp_runtime.go:129-135`). That resolves to the single instance, or else to `fallback` = the global `pool` (`mcp_runtime.go:24,218-225`, `mcpInstance` `:228-243`).
- Prometheus: `writeValueMetrics` is called only `if pool != nil` (`cmd/pg_sage_sidecar/main.go:2248-2250`). It queries the global `pool` (`value_metrics.go:43-49,66-68`) and labels `database=COALESCE(d.name,'')`.
- Dashboard: `web/src/pages/ValuePage.jsx:16-19` calls `/api/v1/value[?database=<selector name>]`.
- Retention: `action_log` rows are purged after `actions_days`, default 365 (`internal/retention/cleanup.go:84`, `config/defaults.go:94`). The only protected rows are those referenced by `incident_avoided` (`cleanup.go:54-55`), which is never written. Credited rows are therefore purged, and "all time" silently decays.

## 2. Behavior per mode

| Mode | Global `pool` | Executor writes ledger to | `database_id` stamped | API/MCP/metrics read | Result |
|---|---|---|---|---|---|
| Standalone | monitored DB (`main.go:185-193`) | same DB (`main.go:754`) | NULL (`EnableStandingPolicy(...,nil)` `main.go:763`) | same DB | **Correct totals.** Labelled `''`. `?database=<name>` returns 0 because `d.name=$1` never matches a NULL id (`value/postgres.go:137`). |
| YAML fleet | **nil**. Only set for meta (`main.go:179`) or non-fleet (`main.go:185`). | each target pool (`main.go:1532-1534`) | NULL (`main.go:1547-1548` passes nil) | API: `PoolForDatabase("all")` = primary/first pool (`wire.go:60-61`, `fleet/manager.go:267-288`). MCP: errors for >1 DB (fallback nil). Metrics: **none** (`main.go:2248`). | **Lost.** Only the primary DB's value is shown, and it is labelled `''`. Per-DB filter returns 0. Other DBs are invisible. |
| Meta-db | meta pool (`main.go:179`) | each target pool (`metadb.go:686-688`) | meta `sage.databases.id` (`metadb.go:694-698`) | meta pool (`wire.go:58-59`, `main.go:2248`) | **Lost.** Only two `INSERT INTO sage.action_log` statements exist, both executor-side on target pools (`executor.go:1177`, `manual.go:403`). The meta ledger is therefore empty and Value shows 0. Target rows carry an id from the *meta* registry, and the target's own `sage.databases` cannot resolve it (label `''`). |

**Double-count risks under any fan-out.**
- Two fleet entries pointing at the same physical database.
- A meta DB that is also registered as a target.

The only existing dedupe is by pool/instance name (`api/helpers.go:133-162`).

**Other mismatches.**
- `database_id` has three meanings: NULL, meta id, and nothing in YAML fleet. The column is only meaningful to a meta-pool join, which never sees these rows.
- The Cases API already fans out over `poolsForDatabaseSelection` (`api/cases_handlers.go:19-43`, `api/helpers.go:133-162`). Value is the outlier (SURF-03, `codex/surface-cross-review.md:68-100`).

## 3. Options

**A. Ledger stays per-target; readers fan out (recommended).**
- Keep `action_log` and `verification` in the target, next to the evidence they credit. `StampCredit` already requires a same-database verification join (`value/postgres.go:72-74`).
- `value.Service` takes `[]namedPool`. It reads each pool without joining `sage.databases` and labels rows with the fleet instance name.
- API, MCP and metrics all use the `poolsForDatabaseSelection` pattern.
- Dedupe pools by physical identity: `system_identifier` + datname where readable, else host:port/dbname.
- Pros: no data migration; attribution is correct in every mode; rollback retraction keeps working unchanged.
- Cons:
  - Value from a database that has been *removed* from the fleet disappears from totals.
  - An unreachable DB makes the total partial. Mark it `partial: true` instead of failing.
  - Scrape cost grows with N DBs.
- **Two-way door.**

**B. Central ledger in the control DB.**
- The executor also writes credit rows to the meta pool.
- Cons:
  - YAML fleet has no stable control DB (`wire.go:81-82,127-129`).
  - Every writer (`index_verification_runtime.go:88`, `rollback_eval.go:250`, the three `ZeroCreditOnRevert` sites) needs a second pool and cross-DB atomicity. Verification lives in the target, so credit and retraction can diverge.
  - It moves stored data semantics and needs a backfill.
- **One-way door.**

**C. Hybrid projection.**
- The target stays the source of truth. An append-only `sage.value_credit` projection is kept in the meta DB, keyed `(database_id, action_log_id)` and populated by an outbox/reconciler. It survives target removal.
- Justified only if value must outlive fleet membership (billing or compliance). The cross-review found no such requirement (`codex/surface-cross-review.md:103-107`).
- **One-way door** (new stored semantics). It is additive on top of A.

**Recommendation.**
- Adopt A now.
- Reserve C for a later billing requirement. A does not foreclose it.
- As part of A, add `OR (action_log.outcome='success' AND action_log.toil_minutes_saved IS NOT NULL)` protection to `keepActionLog` (`retention/cleanup.go:54-55`). Without it, all-time value is not durable. This keeps data and deletes none.

## 4. Implementation sketch (Option A)

1. `internal/value/postgres.go`
   - Add a `FleetRepository{pools []namedSource}`. For each pool, run the existing realized/potential queries without the `sage.databases` join, with the label injected by the caller. Merge the snapshots.
   - Filter by instance name *before* querying (select the pool), not with `d.name=$1`.
   - Record per-pool errors into `Report.Partial` and `Report.Unavailable []string`. `internal/value/types.go` needs these fields.
2. `internal/api/router.go:157-158`: build the service from `poolsForDatabaseSelection(mgr, filter.Database)` when `mgr != nil`, else from the standalone pool. Reject unknown names like Cases does (`rejectUnknownDatabase`).
3. `cmd/pg_sage_sidecar/value_metrics.go` and `main.go:2248`
   - Iterate `fleetMgr.Instances()`. Use `database=<instance name>`.
   - Emit `pg_sage_value_metrics_up{database=...}` per pool so a partial scrape is visible.
4. `cmd/pg_sage_sidecar/mcp_runtime.go:129-135`: aggregate across instances instead of `adapter(nil)`.
5. `internal/retention/cleanup.go:54-55`: protect credited rows.
6. `database_id`: leave the column; no migration.
   - Document it as "meta registry id when known; not used for reads".
   - Drop the unused `sage.value_rollup` view in a later additive migration only if you want to. Deleting the view is not required.
7. Dedupe: compute a physical identity per instance at connect time, and skip duplicates in `poolsForDatabaseSelection` for value reads. Store it on `fleet.DatabaseInstance`.

## 5. Test plan (write first; each fails on HEAD)

- **T1 `TestValueHandlerAggregatesFleetPools`** (`internal/api`, integration, 2 databases)
  - Setup: credited rows 15 min in `a` and 30 min in `b`.
  - Expect: `GET /api/v1/value` gives `all_time` = 0.75 h and `by_database` = `[{b,0.5},{a,0.25}]`.
  - Expect: `?database=a` gives 0.25 h.
  - Fails today: only the primary pool is read.
- **T2 `TestValueMetaModeReadsTargetLedger`** (`cmd/pg_sage_sidecar`, meta integration)
  - Setup: meta pool plus one target with a credited row.
  - Expect: non-zero value labelled with the registry name. Fails today: the result is 0.
- **T3 `TestValueMetricsFleetModeEmitsPerDatabase`**
  - Setup: YAML fleet, `pool == nil`.
  - Expect: `pg_sage_toil_minutes_saved{database="a",...}` and `{database="b",...}`. Fails today: the series is absent.
- **T4 `TestValueStandaloneDatabaseFilterMatchesUnattributedRows`**
  - Setup: standalone, `?database=<configured name>`.
  - Expect: the credited row is counted. Fails today: the result is 0.
- **T5 `TestRetentionKeepsCreditedActionLog`**
  - Setup: a credited success row older than `actions_days`.
  - Expect: it survives the purge, while an uncredited old row is purged.
- **T6 `TestValueRollbackRetractsInFleetTotal`**
  - Setup: T1, then `ZeroCreditOnRevert` on `b`'s action.
  - Expect: the total drops to 0.25 h.
- **T7 `TestValueFleetDedupesSamePhysicalDatabase`**
  - Setup: two instances with the same DSN target.
  - Expect: minutes are counted once.
- **T8 `TestValuePartialWhenOnePoolDown`**
  - Setup: pool `b` is closed.
  - Expect: HTTP 200, `partial=true`, `unavailable=["b"]`, and `a`'s minutes intact. Not a 500.
- **T9 `TestFleetMCPGetValueAggregates`**
  - Setup: more than one instance.
  - Expect: the aggregate report. Fails today: "MCP target database is unavailable" or the meta fallback.
