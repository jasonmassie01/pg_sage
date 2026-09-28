# D5 — Owner-declared retention column for `retention_delete`

**Current state:** pg_sage infers the age column by name (`created_at`, else `occurred_at`); the owner declares only `append_only` + interval, and the dry run is bound to the column *name*, not its identity.
**Recommendation:** Option A — add `sage.table_contract.retention_column`, require it for any retention delete, do NOT backfill; legacy contracts park until re-declared. Bind dry runs to relation OID + attnum + contract version.
**Door type:** the schema change is a two-way door (nullable column, additive); the action it governs is a one-way door (`RollbackClass: "not_reversible"`, `executor/retention_authorization.go:64`). Backfilling an inferred value (Option B) turns a guess into a "declaration" and is effectively one-way.

All paths relative to `sidecar/internal/` unless noted.

## 1. How the column is chosen and used today

**Selection (the only place):** `autonomy/schema_postgres.go:349-366` (`unboundedAppendSQL`). For each
`sage.table_contract` row with `append_only AND retention_interval IS NOT NULL` (`:364`) on relkind
`r` or `p` (`:365`), a LATERAL picks one attribute of type `timestamp|timestamptz|date` (`:359`) whose
name is `created_at` or `occurred_at` (`:360`), preferring `created_at` (`:361`). No match yields `''`
via `COALESCE` (`:350`). Earlier heuristic (fallback to *any* date column, e.g. `birth_date`) was
removed in 4cd9317 (`reviews/2026-09-26/fixes-executor.md:44`; test
`autonomy/retention_safety_test.go:256`).
The name is copied into `Invariant.RetentionColumn` (`autonomy/schema_postgres.go:102`;
type at `schemaguard/types.go:52-57`). `TableContract` has no column field (`schemaguard/types.go:58-63`)
and `tableContractSQL` reads no column (`autonomy/schema_postgres.go:338-341`).

**Uses of the inferred name:**
| Use | Location |
|---|---|
| Precondition (non-empty) | `autonomy/retention_postgres.go:60-61` |
| Candidate count `WHERE col < $1` | `autonomy/retention_postgres.go:124-125` |
| Dry-run binding (`retention_column=$3`) | `autonomy/retention_postgres.go:141-151` |
| Policy-gate intent + evidence | `autonomy/retention_postgres.go:89-94`, `cmd/pg_sage_sidecar/autonomy_runtime.go:70-77`, `executor/retention_authorization.go:35-38` |
| Delete SQL (select, ORDER BY, recheck) | `autonomy/retention_postgres.go:193-194`, `:209-216` |
| Audit row (`sage.retention_run.retention_column`) | `autonomy/retention_postgres.go:237-250`; DDL `schema/ddl_agent_features.go:47-61` |
| Contract audit fields | `executor/retention_authorization.go:66` |

**How contracts are declared:** only via the MCP tool `declare_table_contract`
(`mcp/server.go:157-160`: `table, append_only, retention{object}, expected_pk, exemptions`),
classified `retention`/`RiskSafe` (`mcp/intent_adapters.go:57-58`), trusted internal control
(`policy/gate.go:193`), executed by `mcp/production_intent_executor.go:83-110`, which accepts
`retention` as a string or `{"interval": ...}` only (`:313-328`) and upserts via
`mcp/postgres_access.go:26-37`. The declaration struct has no column
(`mcp/intent_execution_types.go:11-21`). No REST handler or UI declares contracts (grep of
`internal/api` and `web/src` for `append_only|declare_table_contract` finds nothing); no config
key either. Table DDL: `schema/ddl_agent_features.go:13-28`.

## 2. Failure scenarios and existing guards

**Guards today:** dry run must match schema, table, column *name* and window (±5 min) and be
24 h–7 d old (`autonomy/retention_postgres.go:22-25`, `:141-151`); a name or window change forces a
fresh dry run (`:158-163`; test `retention_safety_test.go:181`); policy consent to
`ChangeRetention` (`:102-119`); gate authorization per batch (`:83-99`); batch ≤1000 rows, keyed by
`(tableoid, ctid)` with cutoff recheck (`:209-222`); delete + audit in one tx (`:176-207`).

**What can still delete the wrong rows:**
1. **Wrong semantics, right name.** Owner's retention clock is `ingested_at`/`closed_at`/
   `event_time`, but the table also has `created_at` copied from a source system (years old).
   Heuristic picks `created_at` (`schema_postgres.go:361`); every freshly ingested row is "older
   than 90 d". The dry run records a candidate count (`retention_postgres.go:69`) but nothing
   compares it to anything or requires a human to look: "review" is elapsed time only
   (`:141-157`). After 24 h, deletion proceeds batch by batch.
2. **Ingest vs event time.** `created_at` (ingest) and `occurred_at` (event) both exist; owner
   wanted event-time retention. `created_at` wins by fixed preference (`:361`). Backfilled
   historical events survive far past the contract; late-arriving events are kept — or, reversed
   preference, deleted on arrival. Neither is owner-chosen.
3. **Rename swap after the dry run.** `RENAME created_at TO ingested_at; RENAME source_ts TO
   created_at` inside the 7-day window. Binding is by name (`retention_postgres.go:144`), so the aged
   dry run still authorizes deletion against a different column. No attnum/type/candidate-drift
   check exists.
4. **Drop and recreate same table name.** Binding is `schema_name, table_name` text
   (`:144`), not relation OID; a rebuilt table (e.g. `pg_repack`-style swap, restore) inherits the
   old table's reviewed dry run.
5. **Column appears later.** A table with only `occurred_at` gains `created_at` (migration);
   inference silently switches. The guard forces a new dry run (`:158-163`), but only a 24 h delay
   with no ack — the semantics change is never surfaced to a human.
6. **Contract re-declared.** `append_only`, `expected_pk` or owner may change via upsert
   (`mcp/postgres_access.go:30-34`); the dry run binds window but not contract identity/version
   (`updated_at` is not in the binding, `retention_postgres.go:141-147`).
7. **Partitioned tables.** Relkind `p` is included (`schema_postgres.go:365`); the heuristic column
   need not be the partition key, so a DELETE scans all partitions and never enables partition
   drop. Rows are correctly scoped by `(tableoid, ctid)` (`retention_postgres.go:210-215`), so this
   is a cost/semantics mismatch, not a cross-partition bug.
8. **`timestamp` (no tz) columns.** Cutoff is a Go `time.Time` bound as `$1`
   (`retention_postgres.go:124-127`, `:195`); comparison against `timestamp without time zone`
   depends on session `TimeZone`. Hours of skew, not days — minor but undeclared.
9. **No column → whole scan aborts.** Planner does not check the column (`schemaguard/planner.go:78-95`),
   so an empty column still routes; `Apply` errors (`retention_postgres.go:60-61`), the custodian
   records a park and *returns the error* (`schemaguard/custodian.go:106-109`, `:119-134`), which
   stops `Scan` for all later invariants (`custodian.go:73-74`). Fails closed for deletes, but one
   un-inferrable contract starves other schema remediations.
10. **Contract lookup ignores `database_id`** (`schema_postgres.go:338-341`, `:349-366`) and
    `UNIQUE (database_id, schema_name, table_name)` does not dedupe NULL `database_id`
    (`ddl_agent_features.go:26`), so repeated declarations with NULL id create multiple rows; the
    detector can emit one invariant per row. Adjacent, not column-specific.

## 3. Options

**A. Explicit, required column; legacy contracts park (recommended).** Add nullable
`retention_column`; no backfill. Detector uses only the declared column; NULL → park with reason
"retention column not declared" (plus the heuristic's suggestion as text only). Upgrade: existing
contracts stop deleting until the owner re-declares; any in-flight dry run is invalidated because
the binding now includes contract version. Cost: one re-declaration per retention contract
(count with `SELECT count(*) FROM sage.table_contract WHERE append_only AND retention_interval IS
NOT NULL`). Fully reversible; worst case is "deletes pause", which is safe.

**B. Backfill from the heuristic on upgrade.** Migration writes the inferred name into
`retention_column` with `declared_by='migration:inferred'`. Zero disruption, but it certifies
exactly the guess this decision exists to remove (scenarios 1-2) and, once rows are deleted under
it, cannot be undone. Only acceptable with a provenance flag that still parks deletes — at which
point it is A with a pre-filled suggestion.

**C. Keep inference, harden guards.** Bind dry run to OID/attnum/type, add candidate-drift and
operator ack. Fixes 3-6 but not 1-2 (semantics are unknowable from the catalog).

**Recommendation: A**, plus the identity binding from C (it is needed regardless). Pre-1.x
installed base makes the re-declaration cost small; the failure mode of A is a pause, of B/C a
silent wrong delete.

## 4. Implementation sketch

**Schema** (`schema/bootstrap.go:362` `migrationStatements`, pattern at `:627-648`):
`ALTER TABLE sage.table_contract ADD COLUMN IF NOT EXISTS retention_column text;`
`ALTER TABLE sage.retention_run ADD COLUMN IF NOT EXISTS relation_oid oid, ADD COLUMN IF NOT EXISTS
column_attnum smallint, ADD COLUMN IF NOT EXISTS column_type text, ADD COLUMN IF NOT EXISTS
contract_id bigint, ADD COLUMN IF NOT EXISTS contract_updated_at timestamptz;` Mirror in
`ddl_agent_features.go:13-61` for fresh installs. No CHECK constraint (legacy rows must load); enforce
at declaration.

**MCP** (`mcp/server.go:157-160`, `production_intent_executor.go:83-110`, `:313-328`,
`postgres_access.go:26-37`, `intent_execution_types.go:11-21`): `retention` becomes
`{"interval": "...", "column": "..."}`; interval without column is rejected; declaration validates
the column exists, is not dropped, and is `timestamptz|timestamp|date`; warn (not reject) if the
table is partitioned and the column is not the partition key. Upsert writes `retention_column`.
Bare-string `retention` is rejected when it would enable deletion.

**Detector/planner** (`schema_postgres.go:349-366`, `:185-207`; `schemaguard/types.go:58-63`;
`planner.go:78-95`): join `pg_attribute` on `attname = tc.retention_column` and emit OID, attnum,
type, contract id and `updated_at`; add `RetentionColumn` to `TableContract`; `planRetention` parks
(no route) when the column is undeclared, missing, dropped or non-temporal — this also fixes
scenario 9.

**Enforcer** (`retention_postgres.go`): dry-run match on `relation_oid, column_attnum,
column_type, contract_id, contract_updated_at` + window (`:141-151`) and record them (`:237-250`);
candidate-drift guard (current vs dry-run candidates beyond a bound → new dry run); in `deleteBatch`
take `LOCK TABLE ... IN ROW EXCLUSIVE MODE` then re-verify attnum/type in the same tx before the
DELETE (`:176-207`), so a concurrent rename/ALTER cannot slip between check and delete.

**UI/REST:** none exists; out of scope. The park reason must land in the decision ledger
(`schemaguard/custodian.go:112`) so `get_ledger` shows why deletes stopped.

## 5. Tests to write first (all must fail on today's code)

1. `mcp`: `TestDeclareTableContractRejectsIntervalWithoutColumn`.
2. `mcp`: `TestDeclareTableContractRejectsMissingOrNonTemporalColumn` (text column, dropped column).
3. `autonomy` (real PG): `TestRetentionUsesDeclaredColumnNotCreatedAt` — table with old
   `created_at`, fresh `ingested_at`, contract declares `ingested_at`; after aged dry run + apply, zero
   rows deleted.
4. `autonomy`: `TestLegacyContractWithoutColumnParksAndDeletesNothing` — pre-migration row
   (`retention_column` NULL) on a table with `created_at`; decision `park`, rows intact, no
   `retention_run` row.
5. `schemaguard`: `TestScanContinuesAfterUndeclaredRetentionColumn` — undeclared contract followed by
   a missing-FK invariant; FK invariant is still routed.
6. `autonomy`: `TestRetentionDryRunInvalidatedByColumnRenameSwap` — aged dry run, swap renames,
   apply → `ErrRetentionDryRunPending`, rows intact.
7. `autonomy`: `TestRetentionDryRunInvalidatedByTableRecreate` — drop/recreate same name.
8. `autonomy`: `TestRetentionDryRunInvalidatedByContractRedeclare` — bump `updated_at`.
9. `autonomy`: `TestRetentionCandidateDriftRequiresNewDryRun` — insert 10x old rows after dry run.
10. `autonomy`: `TestRetentionDeleteRechecksColumnIdentityInTx` — ALTER blocked/ detected between
    authorize and delete.
11. `schema`: `TestMigrationAddsRetentionColumnWithoutBackfill` — bootstrap over old DDL is
    idempotent and leaves existing rows NULL.
12. Keep passing: `retention_safety_test.go:91,111,181,209,256`, `preflight_retention_test.go:78,99,120,159`,
    `cmd/pg_sage_sidecar/preflight_retention_test.go:26` (update fixtures to declare the column; the
    fixture change is not a test-logic change).
