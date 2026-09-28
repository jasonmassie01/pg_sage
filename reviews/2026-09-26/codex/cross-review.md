# Independent cross-review of runtime findings R01-R04

Snapshot: `b396595059d2b1b312a22231fdbfd1d2cfef9530`. Reviewed 2026-09-26.
Source-only review by the surface-audit agent. No full tests or live mutations.

## R01: Confirmed; no counterevidence found

The query in `sidecar/internal/autonomy/retention_postgres.go:116-119` selects
only ctid and deletes using only ctid equality. The detector explicitly includes
partitioned parents. The coordinator's transactional reproduction establishes the
cross-partition effect. This also applies to inherited relations with overlapping
physical tuple locations. LIMIT bounds selected tuple locations, not deleted
rows; FOR UPDATE SKIP LOCKED cannot make ctid globally unique.

P0 is justified under the report's definition of possible data loss. Preserve the
retention contract/policy enabling conditions; do not imply default deletion.

## R02: Confirmed; emergency stop does not cancel this worker

The full production chain is:

1. `cmd/pg_sage_sidecar/main.go:756` starts standalone autonomy. Fleet starts it
   at `main.go:1542` even after SetExecutorEnabled at line 1541. The autonomy
   constructor does not consume the flag.
2. `cmd/pg_sage_sidecar/autonomy_runtime.go:61-98` constructs the schema custodian.
3. `autonomy/schema_postgres.go:28-37` sets AllowRetentionApply=true and installs
   postgresRetentionEnforcer directly.
4. `autonomy/supervisor.go:143-177` runs schema work on ticks and DDL triggers.
   runSchemaGuard checks context and a concurrency semaphore, not stop state.
5. `schemaguard/custodian.go:91-116` plans and routes. Its policy checks contract,
   exemption and prior history, not executor mode, trust, replica or stop state.
6. `autonomy/schema_postgres.go:241-246` routes retention directly to Apply;
   `retention_postgres.go:40-47` checks old dry-run existence and the allowed
   retention class, then executes DELETE directly.

Crucial counterevidence check: `sidecar/internal/fleet/manager.go:239-293`
intentionally leaves monitoring goroutines running on emergency stop. It persists
the stop flag and changes inst.Stopped but does not invoke Cancel. Retention reads
neither state. An enabled retention contract with dry-run history and permitted
retention class can still delete rows after the API emergency-stop operation.
Process shutdown/instance removal can cancel context, but are separate controls.

Precision: a true PostgreSQL hot standby rejects writes itself. Missing application
replica gating does not imply bypass of PostgreSQL read-only enforcement.

## R03: Confirmed; distinguish dry-run evidence from human approval

The detector selects a timestamp/date column by name/order; TableContract contains
a duration but no explicit owner-selected column. requireDurableDryRun accepts a
historical row for schema/table only. History also counts decisions by target text
and invariant kind, with no contract hash, relation identity or expiry.

No upstream semantic/version check was found. Drop/recreate under the same table
name or changing duration can retain eligibility from old evidence. Replace
"unrelated old approval" in R03 with "unrelated old dry-run evidence": this path
does not require human approval of that dry run in the first place. A scheduled
first dry run creates its own history and a later cycle becomes Apply if standing
policy permits retention, without separate operator approval after seeing count.

## R04: Confirmed; manual resolution overwrite is a production chain

`rca/rca.go:125` starts with an empty incident slice; no hydration/manual-resolve
callback was found. `cmd/pg_sage_sidecar/rca_adapter.go:31-35` only forwards persist.
The API updates only the database resolved_at (`api/handlers_v09.go:419-436`) and
discards reason. The RCA upsert sets resolved_at=EXCLUDED.resolved_at and iterates
all tracked incidents. A still-active memory incident therefore overwrites a
manually resolved row with NULL on the next persistence cycle. A continuing signal
keeps it active; no unusual race beyond the next normal persist is needed.

If memory independently auto-resolves first, that cycle does not reopen the row;
this does not negate the demonstrated path. After restart, old unresolved rows
are absent from memory, cannot be auto-resolved or deduplicated there, and later
detections can create new UUIDs while old rows remain unresolved.

## Recommendation

R01 and R02 form a destructive-path release blocker. Keep retention mutation
disabled until central policy, leases, exact target identity and evidence binding
are mandatory. R03 and R04 also remain substantiated. No severity downgrade is
recommended, with the enabling-condition and PostgreSQL-read-only qualifications.
