# P0: schema guard load (sage.decision flood, per-invariant history scans)

Branch `claude/p0-schemaguard-load` (from master `49a088a9`). Not pushed.

## Problem (measured on lifeos, v1.8.1)

15k tables, 160 leaked `test_*` schemas. The schema guard wrote one `sage.decision` row per
invariant per cycle (41,300 rows/hour) and, per invariant, ran an unindexed
`count(*) FILTER ... WHERE target_objects @> $2` over `sage.decision`, keeping one core at
~85%. Per invariant it also ran one `pg_stat_statements` ILIKE query (FK invariants) and
one `sage.table_contract` query.

## What was built

| Concern | Change | Files |
|---|---|---|
| Change-only ledger | A decision row is written only when the decision for an invariant identity changes (hash of route, disposition, reason and SQL of every member), or when the invariant reappears after a scan without it. Unchanged invariants are still planned and routed every cycle. | `internal/schemaguard/dedupe.go`, `custodian.go`, `types.go` |
| Batched reads | One contract query and one history query per cycle (`Contracts`/`History` take all invariants). `pg_stat_statements` is read once per cycle (scoped to the current database, capped at 10,000 rows) and indexed by qualified name. | `internal/autonomy/schema_sources_postgres.go`, `schema_statements.go`, `schema_detect_postgres.go` |
| Clone families | Families use the analyzer's definition (generated suffix, same table set, at least 5 copies). Idle leftovers are parked once per identity and never planned or routed; live families are remediated per member and recorded once per identity, with every member in `target_objects`. | `internal/schemaguard/families.go`, `internal/autonomy/schema_family_postgres.go` |
| Post-DDL debounce | `analyzer.schema_guard_ddl_debounce_seconds` (default 60; 0 means default; range 0-3600; restart-bound). A burst of requests becomes at most one extra scan per window, and a periodic scan satisfies a pending request. `TriggerSchemaGuard` (explicit, synchronous) is not debounced. | `internal/autonomy/ddl_debounce.go`, `supervisor.go`, `types.go`, `internal/config/schema_guard.go`, `config.go` (field, default, one validate line) |
| Index | `idx_decision_schema_guard_targets`, the coordinator's exact definition: `gin (target_objects) WHERE feature = 'schema_guard'`, created `CONCURRENTLY IF NOT EXISTS`. An existing index is kept (same OID). An INVALID one left by a failed build is dropped concurrently and rebuilt. A build already in progress is left alone. | `internal/schema/schema_guard_index.go`, `cmd/pg_sage_sidecar/autonomy_schema_guard.go`, `autonomy_runtime.go` |
| Retention | `schema_guard` decisions age out on `retention.findings_days`. A row is kept while an `action_log`, `verification`, active lease or `incident_avoided` row refers to it. | `internal/retention/cleanup.go` |
| Generated docs | `docs/generated/config-lifecycles.md` and `web/src/generated/config_meta.json` were regenerated for the new key. | |

Per-cycle reads by the guard are now bounded by a constant: 3 detection queries, at most 1
statement read, 1 shape query, and (only when some family is quiet) 1 more statement read
and 1 session read, then 1 contract and 1 history query. That is at most 9; the test bound
is 12. Writes are O(changes).

## Product decisions (and why)

1. **Audit semantics: rows describe changes.** Each row is the decision that held from its
   `created_at` until the next row for the same `invariant_key`. Recency is not kept as a
   per-row `last_seen`, because updating a row every cycle would bring back O(invariants)
   writes, and an UPDATE costs more than an INSERT. The dedupe state lives in the ledger
   (`evidence.invariant_key`, `evidence.decision_hash`), so a restart does not re-record.
   The only in-memory state is the set of identities seen in the last completed scan,
   which is used to re-record an invariant that disappeared and came back. Before the
   first scan after a restart there is no absence information, and only hashes are
   compared. A route failure is still recorded every time it happens, as before (it is an
   event, and it aborts the scan). The outcomes planned before the failure are now also
   recorded.
2. **Identity** is kind + `schema.table` (or `family:<key>` + table) + subject, where the
   subject is the FK constraint or the column. Before this change, two unindexed FKs on
   one table shared a ledger target and were counted together.
3. **Routing is unchanged** for every invariant that is planned, including every member of
   a live family. Only recording is deduplicated, so the retention dry-run then apply
   sequence, parks, and verified index routes behave as before.
4. **Idle leftover families are not remediated.** Creating FK indexes or rehearsing type
   changes in copies nothing uses is wasted work and noise. They get one parked row per
   identity, with the reason and a "drop the schemas if unused" hint. The exception is
   `unbounded_append`, which comes from an owner's declared contract. That is explicit
   intent, and idleness never overrides it.
5. **Idle needs every signal** (the analyzer's rule): no scan or tuple activity since the
   stats reset, or none within `analyzer.unused_index_window_days` (counters tracked in
   memory); no `pg_stat_statements` text naming a member; and a known set of sessions,
   none of which holds a lock in a member or runs a statement on one. An unknown lookup
   makes the family live, and the lookup error is appended to the recorded
   `family_reason` rather than dropped. After a restart, a family with any activity since
   the stats reset stays live until a full window has passed.
6. **Workload evidence matching changed from ILIKE to qualified-name tokens.**
   `public.orders` no longer matches `public.orders_archive`, `_` is no longer a
   wildcard, and quoted names now match. Statements are scoped to the current database
   (previously every database's statements were matched).
7. **The index is not built in `Bootstrap`.** `CREATE INDEX CONCURRENTLY` waits for every
   older snapshot. That includes another sidecar's session blocked in
   `pg_advisory_lock` on the bootstrap lock, which would deadlock. The build could also
   outlast the 30 s migration budget on a large v1.8.x ledger. So
   `schema.EnsureSchemaGuardIndex` runs in the background at autonomy start (30 min
   budget), serialized by `pg_try_advisory_lock`. On failure it logs a WARN, and the
   sidecar keeps running, because the single history query per cycle works without the
   index. **This deviates from "register migrations in bootstrap.go"** for the reasons
   above. The coordinator may prefer a different home.
8. **Retention window**: schema guard rows are observations (observe_only and parked rows),
   so they follow `findings_days` (default 180) instead of `actions_days` (365).
   `findings_days: 0` disables only this rule, and the general decision rule still applies.

## Spec CHECKs

No AI-SRE-SPEC CHECK maps directly; this is a Phase 0 load fix. It preserves the ledger
invariants the self-audit relies on: decisions behind actions and verifications are never
purged, and nothing is executed outside `Executor.Apply`.

## Test Results

RESULTS_PLACEHOLDER

## Mutation testing

24 mutations of the key logic were applied one at a time; all 24 were killed. M8 (the hash
ignores the reason) survived the first round, and a test was added for it
(`TestChangedReasonAloneIsRecorded`).

MUTATION_TABLE

## Bugs found

1. [BUG, phase-1 fixture] Idle-family fixtures built from tables with primary keys were
   live or idle depending on the statistics flush timing, because index builds count as
   sequential scans. Fixtures now use index-free tables. Explained in commit `1819c05a`.
2. [BUG, fixture] `LOCK TABLE schema.t` is recorded by `pg_stat_statements`, so the "live
   by lock" test passed for a statement, not for the lock. Fixed in the same commit.
3. [FINDING, pre-existing] `relatedQueryIDs` swallowed every error and matched other
   databases' statements and `LIKE` wildcards. Replaced (decision 6).
4. [FINDING, pre-existing] The guard's history lookup had no supporting index (fixed), and
   its decisions lived 365 days (now `findings_days`).

## Post-test audit

POST_AUDIT

## What is left / for the coordinator

- **The leftover classification is on `claude/p0-tuning-correctness` (`380fffcc`), not
  master.** The brief said master had it, but master's analyzer only collapses families.
  `schemaguard.CloneStem` and `GroupFamilies` duplicate the analyzer's stem and shape
  logic, and the idle rules mirror `clone_activity.go`. Once both land, the analyzer
  should call `schemaguard`'s helpers, or a shared `clonefamily` package should hold them.
- **The post-DDL hook only fires for pg_sage's own DDL** (`executor.notifyPostDDL`).
  Application DDL never triggered a guard scan. The lifeos load came from the periodic
  cycle multiplied by per-invariant writes and reads. The debounce is still in place as a
  guard.
- **Routing per cycle is still O(planned invariants).** Every FK apply decision is
  re-submitted to `SubmitVerifiedIndexProposal` each cycle (behaviour unchanged, by the
  brief). On lifeos this may still cost executor work and write its own decision rows
  per route. Worth measuring after deploy.
- **Legacy lifeos rows**: the existing ~73k+ `schema_guard` rows have no `invariant_key`.
  The first cycle after upgrade records one row per identity. The old rows age out
  through retention (180 days), or can be deleted by hand.
- Decision 7 (index outside `Bootstrap`) needs the coordinator's sign-off.
