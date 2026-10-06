# Plan: keep pg_sage's history outside the monitored database (v2.3.0)

Date: 2026-10-06. Branch `claude/v23-history-outside`. Design input:
`reviews/2026-10-04-p3-observer-report.md` section 4.

## Scope decision

Ship an opt-in `history.store: meta` (default `monitored`, so nothing changes for
existing installs) that moves pg_sage's **telemetry history** to the metadata
database in meta-db mode:

| table | moves in meta mode | why |
|---|---|---|
| `sage.snapshots` | yes | the bulk of pg_sage's bytes (lifeos: 9.3 GB before deltas) |
| `sage.query_store` | yes | the second largest, written every cycle |
| `sage.findings`, `sage.action_log`, `action_outcome`, `verification`, `decision`, `recommendation*`, `incident_avoided` | **no (deferred)** | one foreign-key graph (findings.action_log_id -> action_log <- outcome, verification, decision, recommendation, incident_avoided); about 55 files read findings; moving part of the graph is impossible across databases and moving all of it is its own release |
| `explain_cache`, `size_history`, `health_history`, `briefings`, `alert_log`, `explain_results`, ... | no (deferred) | small; explain_cache is joined with the monitored catalog by the optimizer |

This is the "coherent subset" the brief allows: the two tables that make pg_sage
heavy move together, every reader of them is rewired, and the rest is documented
as staying. No mode silently loses or mixes data:

- every history row in the meta database carries `database_id` (the meta-db
  record id, never reused, unlike a name);
- every history statement is written with a scope marker and fails to parse if it
  is ever sent without the store binding it (a static test checks every SQL
  literal that names the two tables carries the marker);
- a runtime refuses to start when its history would be split between the two
  places (mode changed without migration).

## Design

### Config

`history.store: monitored | meta` (new `internal/config/history.go`), default
`monitored`. `meta` requires `meta_db` (validation error otherwise). Restart-bound,
YAML only (not an API override). Key class **operator_preference**: it declares
deployment topology (where pg_sage keeps its own data), never widens what pg_sage
may do, and is never derived. Its safety (no hidden history) is enforced by the
startup refusal, not by the class; the endpoint it uses (`meta_db`) is already
safety_critical.

### Schema (`internal/schema/history_store_migration.go`)

`BootstrapHistoryStore(ctx, metaPool)` runs only when `history.store: meta`, after
the normal meta bootstrap, under the bootstrap advisory lock and its own timeout:
- `database_id integer` added to `sage.snapshots` and `sage.query_store`
  (catalog-checked first, like the delta migration; propagates to partitions;
  `partition.Convert` keeps it via `LIKE`);
- indexes `(database_id, category, collected_at DESC)` on snapshots and
  `(database_id, queryid, captured_at DESC)`, `(database_id, captured_at DESC)` on
  query_store;
- `sage.history_store_databases` (database_id PK, database_name, db_bytes,
  registered_at, updated_at): which databases keep history here and their last
  known size (the snapshot cap's input);
- `EnsureHistoryMigrationTables`: `sage.history_migration` (progress, per
  database/table/direction, in the destination) and `sage.history_migration_ids`
  (source snapshot id -> destination id, for delta bases).

Monitored databases get no schema change. Foreign keys: neither moved table has
any; snapshot delta bases (`base_id`) stay within the store (ids are the store's).

### Storage interface (`internal/histstore`)

- `Store`: where one monitored database's history lives: a DB handle plus a scope
  (`database_id`, or none). `Bind(sql, args)` rewrites markers:
  `{db:ALIAS}` -> `true` (monitored) or `ALIAS.database_id = $n` (meta);
  `{dbcol}`/`{dbval}` -> the insert column/value. Query/QueryRow/Exec/Begin bind.
- Registry: `Register(monitoredPool, store)` / `Resolve(db)`. Every reader already
  holds the monitored pool; resolving its store from that pool means no reader can
  be wired to the wrong store (the observer report's main risk: "a partial move
  would silently feed empty history to the ones missed"). Unregistered = monitored.
- Writers/readers rewired (each through `Resolve`): snapstore writer, collector
  partitions, querystore recorder + evidence, verify (query_store intervals,
  snapshot write measurements), analyzer regression history, optimizer cold start,
  forecaster (system aggregates + day picks), onboarding install kind, API latest +
  history, SRE SLO latency proxy, SRE plan_regressions probe (runs on the store).
- Reads that joined the monitored database are split into two queries joined in Go:
  - query_store insert stamped `plan_hash` from `sage.explain_cache` (monitored):
    meta mode reads the latest fingerprints from the monitored DB, then inserts
    into the store with them;
  - onboarding's install kind (sage.config on the monitored DB + newest snapshot);
  - verify's observation source: catalog reads (pg_index) on the monitored pool,
    history reads on the store.
  Tests run each on the same fixture in both modes and require identical results.

### Retention, cap, footprint, self budget

- Per-database cleaners skip the two history rules when the store is the meta DB.
- One store cleaner per process (meta mode) runs the history rules on the meta
  DB: day partitions dropped by the global `retention.snapshots_days` /
  `query_store_days` (retention windows are process-wide, so a day is expired for
  every database at once).
- Snapshot cap in meta mode: `sum over databases in history_store_databases of
  max(snapshots_max_pct% of that database's size, 256 MB)` (the
  `retention.minSnapshotCapBytes` constant; PR #131 is not merged). The cleaner
  refreshes the sizes of the databases this process monitors; an offline database
  keeps its last known size.
- `sage_footprint` keeps measuring the monitored database's sage schema (that is
  the guard's subject); in meta mode history is no longer written there, so it is
  excluded by construction; the finding says where history lives and that rows
  left behind are removed by `history migrate --cleanup`.
- `self_budget.storage_mb` in meta mode = monitored sage schema + this database's
  share of the store (store history size x this database's row share, cached 15
  minutes).

### Migration path (`pg_sage history migrate`)

- `--to meta` (default) copies the monitored sage history into the store,
  `--to monitored` copies it back. Source is read-only; snapshots copied in id
  order with delta bases remapped through `history_migration_ids`; query_store in
  (captured_at, id) order; each batch commits with its progress row (idempotent,
  resumable, safe to re-run after new rows).
- `--cleanup` (explicit) removes the source rows once the copy is complete.
- Startup refusal: meta mode refuses a database whose own sage history has rows
  newer than the recorded migration mark; monitored mode (with a meta DB) refuses a
  database the store still holds history for, unless copied back. Messages name the
  exact command.

### Performance gate

The gate runs in both modes: the meta variant seeds the store with this
database's history plus two other databases' (so scoping must use its indexes)
and measures statements and table scans on the monitored and the meta database.

### Docs and release

`docs/configuration.md` (history.store, placement table), `docs/architecture.md`,
deployment notes (meta-db, migrate command), remove "not supported yet";
CHANGELOG `## Unreleased` bullet; key class; regenerate config metadata.

## Steps (checkable)

- [ ] 1. Plan (this file)
- [ ] 2. Phase 1 tests written and committed (histstore bind/registry/static scan,
      schema migration, config, copier, startup check, readers identical in both
      modes, retention store cleaner + cap, footprint/self budget, CLI)
- [ ] 3. Config + key class + config meta
- [ ] 4. Schema migration
- [ ] 5. histstore package
- [ ] 6. Rewire writers/readers (+ split reads)
- [ ] 7. Retention store cleaner and cap; footprint; self budget
- [ ] 8. Migration command and startup refusal; runtime wiring
- [ ] 9. Perf gate meta variant
- [ ] 10. Docs, CHANGELOG
- [ ] 11. Build, vet, lint, -race touched packages (PG17, DB tests PG14), e2e,
      perf gate both modes, secret scan, push, PR

## Deferred (documented)

- Findings, action log and the verification/decision/recommendation graph in the
  meta DB (one FK graph; needs its own release).
- explain_cache, size_history, health_history, briefings in the store.
- `history.store` for standalone/YAML fleet via a separate `history_dsn`.
