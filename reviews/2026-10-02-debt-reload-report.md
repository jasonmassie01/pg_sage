# Engineering debt part 2: fleet and meta-db hot reload

Date: 2026-10-02. Branch `claude/debt-reload` (worktree `pg_sage-debt-reload`), from master
`fce3674`. Not pushed. Test database: PG17 `pgsage-ag8` (:55478, repo root mounted); shared
matrix PG14 (:55414) and PG18 (:55418) for the touched packages.

## Problem

Before this branch, YAML fleet mode could not add, remove or change a database without a
restart: the `databases` key was classified `lifecycle_api`, which the controller reported as
"pending restart", and YAML fleet has no managed-database API. `defaults.*` was `restart`.
Meta-db mode already applied changes made through its own API at once, but rows changed
outside that API (another replica, SQL, an enable/disable) were never picked up. Removing a
runtime also cancelled its context first, so an in-flight action (for example an index
build) was interrupted.

## What was built

| File | What |
|---|---|
| `internal/config/database_lifecycle.go` | Per-database field registry (`databases[].*`): `live_policy` for `trust_level`, `execution_mode`, `executor_enabled`, `tags` (applied in place); `reconfigure` for every other field (rebuilds that database's runtime). `ClassifyDatabaseChange` compares gates by effective value, treats nil and empty tags as equal, counts a trust level switching between explicit and inherited as a policy change, and includes private DSN connection options. `FleetDatabasesOwner = "fleet_databases"`. |
| `internal/config/lifecycle.go`, `controller.go`, `lifecycle_reference.go` | `databases` and `defaults.*` are now `reconfigure` with owner `fleet_databases`. `lifecycle_api` is removed: no field uses it any more. Without the owner (standalone, meta-db) the controller still fails closed to `restart`. The generated reference adds a per-database table. |
| `internal/executor/quiesce.go` (+1 field in `executor.go`) | `Executor.Quiesce(ctx)` takes every DDL slot. Every action path (background, operator, approved, custodian, rollback, verification) runs while holding a slot, so Quiesce returns once nothing is executing, and from its first slot on new actions park (`ErrDDLSlotUnavailable`, retried next cycle by the next runtime). On timeout it returns `ErrQuiesceTimeout` with the count still running. `InFlightActions()`. |
| `internal/fleet/teardown_drain.go`, `types.go` | `DatabaseInstance.Quiesce` and `DrainTimeout` (default 60 s). Teardown order is now: drain (bounded), cancel, release slots, wait for workers (Sage SRE investigator, orchestrator, autonomy loops), executor shutdown, pool close. The drain error is joined into the lifecycle error. `LifecycleMutation.UpdateMetadata` swaps an exact generation's config in place. |
| `cmd/.../database_runtime.go` | Runtimes publish `Quiesce: rt.executor.Quiesce`. |
| `cmd/.../fleet_bootstrap.go` | `prepareFleetRuntime` (connect + `buildDatabaseRuntime`, unpublished) used by startup and reload; the control database name is recorded. |
| `cmd/.../fleet_reload_plan.go` | `planFleetReload` (add / remove / rebuild / in place, rename = remove + add) and `validateFleetReloadPlan` (control database, policy values). |
| `cmd/.../fleet_reload.go`, `fleet_reload_commit.go` | The `fleet_databases` owner. Prepare builds every new runtime unpublished. Commit validates every observed generation and publishes the whole plan in one lifecycle reservation (steps with undo). Rollback (another owner failed after ours committed) restores the old generations and stops the candidates. Drain retires removed and replaced generations in parallel and returns removed budget shares. The process `cfg.Databases`/`cfg.Defaults` follow the commit. Registered in `initFleetMultiDB` (one line in `main.go`). |
| `cmd/.../meta_reconcile.go`, `meta_reconcile_apply.go` | `reconcileMetaDatabases`: every 30 s (in the existing reconnect loop, before failed-database retries) it converges the fleet on `sage.databases`. Per database it plans outside the lifecycle reservation, builds a new runtime outside it (connecting can take 15 s), then re-reads the row and the generation inside it and publishes only when both are unchanged. Adds, removals (row deleted or `enabled = false`), rebuilds (name, pool size or connection string, including a rotated password), and in-place trust/execution mode. |
| `cmd/.../metadb.go` | Calls the reconcile pass; `registerFailedInstance` uses the shared `failedStoreInstance`. |
| docs | `docs/configuration.md` (new paragraph), `docs/generated/config-lifecycles.md` (regenerated), `docs/reverse_spec/01-architecture.md`, `CHANGELOG.md`. |

## Product decisions

- **D1 A reload is all-or-nothing.** An invalid file, an invalid per-database trust level or
  execution mode, or a changed running database whose new runtime cannot be built rejects
  the whole reload; every running runtime keeps going and the watcher retries on the next
  save. Never trade a working runtime for a broken one.
- **D2 A new or currently failed database that cannot connect is published as failed**, as
  at startup, so the dashboard shows why. In meta-db mode the reconnect loop retries it.
- **D3 The YAML control database (the first one that started; logins, sessions, standing
  policy and notification rules live there) cannot be removed or reconnected by reload.**
  The API's auth pool and every runtime's control pool point at it; moving it needs a restart
  (error `errFleetControlDatabase`). In-place changes to it are allowed.
- **D4 Trust level, execution mode and the executor switch apply in place**, like the
  existing global `trust.level` live policy. A downgrade takes effect before the next action.
  A raise is logged at WARN. It does not grant unearned autonomy: the standing gate and the
  M7 earned-autonomy limiter still decide, and a live raise does not run the carry-over
  seeding a restart would. So a live raise is more conservative than a restart.
- **D5 Drain bound 60 s.** In-flight actions finish. A longer action (a large index build)
  is cancelled after 60 s rather than holding a removal open. The executor's own cleanup and
  lease release run (`WithoutCancel`), and the cancelled action is recorded as failed. New
  actions during the drain park, and the next generation retries them.
- **D6 Meta-db reconcile polls every 30 s** (the existing reconnect ticker) instead of
  LISTEN/NOTIFY, so there is no new trigger or migration. The managed-database API still
  applies changes at once. `sage.databases` is the policy source of truth, so a trust/mode
  change made there by SQL or another replica is adopted. The settings API writes and applies
  it under the same lifecycle reservation, so the two cannot race.
- **D7 Stale `sage.databases` rows are not deleted** when a YAML database is removed (same as
  startup, which only upserts). Their ids stay stable if the database comes back.

## Spec CHECKs

This branch is reload machinery, not an SRE milestone, so no AI-SRE-SPEC CHECK is
implemented here. It preserves the spec's invariants. Retiring a runtime stops its Sage SRE
investigator (worker group drained; investigation leases are fenced and lapse for the next
generation). Actions still go only through `Executor.Apply` and the standing gate, and a
reload never raises autonomy past the M7 limiter (D4). Reload checks:

```
CHECK-R01: PASS add via reload builds a full runtime (TestFleetReloadAddsDatabaseWithFullRuntime)
CHECK-R02: PASS remove closes pool, backends, budget share and metric labels (TestFleetReloadRemovesDatabaseAndCleansUp)
CHECK-R03: PASS hot fields apply in place, same generation (TestFleetReloadHotSettingsApplyInPlace)
CHECK-R04: PASS restart-class field rebuilds only that database (TestFleetReloadRestartClassChangeRebuildsOnlyThatDatabase)
CHECK-R05: PASS unreachable change rejected, old runtime healthy, desired unchanged (TestFleetReloadUnreachableChangeKeepsRunningRuntime)
CHECK-R06: PASS invalid policy and duplicate names rejected (TestFleetReloadRejectsInvalidDatabasePolicy, ...DuplicateNames)
CHECK-R07: PASS control database protected (TestFleetReloadRefusesToRemoveControlDatabase)
CHECK-R08: PASS in-flight action finishes before removal completes; new actions park (TestFleetReloadRemovalLetsInFlightActionFinish)
CHECK-R09: PASS stuck action bounded by DrainTimeout, ErrQuiesceTimeout surfaced (TestFleetReloadRemovalBoundsAStuckAction)
CHECK-R10: PASS concurrent reloads from one generation: exactly one applies (TestFleetReloadConcurrentAppliesSerialize, ...RetriesConverge)
CHECK-R11: PASS no goroutine/connection/LLM-client leak over add/remove cycles (TestFleetReloadAddRemoveCyclesLeakNothing, TestMetaReconcileCyclesLeakNothing)
CHECK-R12: PASS real fsnotify YAML edit adds a database; unparsable write changes nothing (TestFleetReloadThroughWatchedYAMLFile)
CHECK-R13: PASS rollback after a later owner's commit failure restores the fleet (TestFleetReloadRollsBackWhenALaterOwnerFails)
CHECK-R14: PASS meta: out-of-band add/remove/disable/rename/rebuild/policy (TestMetaReconcile*)
CHECK-R15: PASS meta: concurrent passes and pass vs API update converge without spurious errors
```

## Test Results

**Command (full suite, PG17):** `go test -count=1 -cover ./...` (repo root mounted). The
first run with default parallelism timed out in 11 DB-heavy packages: the shared Docker host
was saturated (`statement_timeout`, ping and `DROP DATABASE` deadlines in packages this branch
does not touch, plus one fixture cleanup in mine). Those 11 packages were rerun with
`go test -count=1 -cover -p 2 -timeout 40m ...`. All passed.
**Total:** 80 packages ok, 0 failed (69 in the first run + the 11 reruns). Touched packages
under `-race -v` on PG17: 1822 passed, 0 failed, 0 skipped.
**Race:** `go test -race -count=1 -cover -v -p 2 ./cmd/pg_sage_sidecar ./internal/config
./internal/fleet ./internal/executor`: ok, no data races.
**PG14 / PG18 (touched packages):** both ok, 1822 passed, 0 skipped (addendum).
**Lint:** `golangci-lint run ./...`: 0 issues.

**Coverage (touched packages):**
- `cmd/pg_sage_sidecar`: 74.5%
- `internal/config`: 89.8%
- `internal/fleet`: 83.7%
- `internal/executor`: 83.3%

### Skipped Tests
None. 0 skips in the touched packages (live DB configured).

### Failures
None after the reduced-parallelism rerun. The first-run timeouts were infrastructure load,
not assertions. Every rerun package passed unchanged.

### Coverage Gaps
All touched packages meet thresholds (70% business logic).

### Bugs Found This Session
1. [BUG, fixed] Meta-db rows changed outside this process's API (another replica, SQL,
   `enabled = false`) were never applied until restart. Fixed by `reconcileMetaDatabases`.
2. [BUG, fixed] Runtime removal cancelled in-flight actions immediately (cancel came before
   the executor drain). Fixed by the quiesce stage.
3. [DESIGN BUG, fixed during development] The first meta reconciler built candidates inside
   the lifecycle reservation. An unreachable host then held the reservation, and with it
   the managed-database API, about 15 s per pass. Building moved outside, and publishing
   re-checks the row and the generation.

## Post-test audit

- **Mutation testing** (each mutation broke code on purpose, ran the targeted tests, then
  restored). All 13 were killed: skip the drain in teardown; Quiesce acquires nothing; Drain
  retires nothing; every change classified hot; control check removed; failed rebuild
  publishes a placeholder over a healthy runtime; meta ignores connection-string changes;
  budget share kept on removal; meta stale-recheck removed; meta skips execution mode; fleet
  skips executor gate; gate compared by pointer, not effective value; Rollback ignores a
  committed plan. The stale-recheck mutation first survived (the lifecycle's own conflict
  check still kept one runtime). The concurrency test was strengthened to assert that losing
  passes report no errors, which kills it.
- **Tests added in phase 2** (strengthening, after implementation):
  `TestFleetReloadRollsBackWhenALaterOwnerFails`, and the no-spurious-error assertion in
  `TestMetaReconcileConcurrentPassesPublishOneRuntime`. No test was weakened.
- **Untested inputs:** a reload racing process shutdown (Prepare checks `ctx.Err()`; not
  exercised). A YAML fleet whose every database failed at boot (no control database: adds are
  allowed but the API auth pool stays nil until restart). Emergency stop inheritance across a
  reload rebuild (covered by fleet's existing `PublishReplacement` tests, not end-to-end here).
- **Fakes:** the in-flight tests drive a real `Executor.Apply` with a `pg_sleep` action and an
  injected authorizer. The gate is bypassed on purpose to make the action deterministic. The
  slot/lease path is real. `failingCommitOwner` fakes only a later owner's commit failure.
- **Known limitation:** a controller commit failure that is not ours returns warnings, not an
  error (existing controller behaviour), so the watcher advances its file baseline.

## What is left / for the coordinator

1. **Shared-file touch points** (small, additive): `main.go` (1 line in `initFleetMultiDB`),
   `metadb.go` (reconcile call; `registerFailedInstance` body), `executor/executor.go` (1 field
   + import), `fleet/types.go` (2 fields, 3 lines in teardown), `config/lifecycle.go`,
   `controller.go` (lifecycle_api removed). `debt-exec` also edits the executor: there will be
   a trivial struct-field merge at most.
2. **`debt-split`** should keep `fleet_reload*.go` and `meta_reconcile*.go` intact; `metadb.go`
   was already over 500 lines before this branch and is now slightly shorter.
3. **Decision needed?** D3 (control database is restart-only) and D5 (60 s drain) are product
   calls I made. Raise the drain bound if large index builds should not be cancelled by a
   removal.
4. Not done: LISTEN/NOTIFY for meta-db (D6); deleting stale `sage.databases` rows for
   removed YAML databases (D7).

## Addendum: cross-version runs (touched packages)

`go test -count=1 -cover -v -p 2 ./cmd/pg_sage_sidecar ./internal/config ./internal/fleet
./internal/executor`:

- PG14 (:55414): all 4 packages ok; 1822 passed, 0 failed, 0 skipped.
- PG18 (:55418): all 4 packages ok; 1822 passed, 0 failed, 0 skipped.

Coverage is identical to PG17.
