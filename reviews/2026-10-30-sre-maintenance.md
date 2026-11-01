# Operator-requested SRE statistics maintenance

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

The final frozen-source native Lakebase package passed on 2026-10-30 with Go's race detector: 79 test/subtest records, zero failures, two explicit opt-in skips in 435.223 seconds. The bridge connects a retained investigation observation to an explicit operator request, pending human approval, policy-gated execution, structural verification, and a separately requested exact-object rollback. This verifies a maintenance lifecycle; it does **not** establish column correlation, workload improvement, automatic root-cause remediation, or incident recovery.

## v2.5.0-dev.5 retention correction

The new maintenance table is explicitly owned by SRE retention and cascades
with its investigation. Finalized maintenance ages from the latest maintenance
change, using the separate evidence and timeline windows. Execution, rollback,
uncertain outcomes and failed rollbacks keep their investigation and evidence.
Expired requests eventually age out. An operator who needs finalized history
or rollback identity beyond the timeline window must pin the investigation.

The generic cleaner keeps the exact maintenance queue item, finding, action,
verification and policy decision while that ledger exists. The queue uses its
immutable proposal UUID identity, including the separately committed enqueue
window before the proposal saves its queue ID. If execution commits before the
bridge saves its action ID, a narrow unresolved-state check preserves the
matching finding, forward SQL, inverse and audit record. This preserves crash
evidence; it does not implement automatic reconciliation or crash recovery.
Finalized, aged investigations still cascade and release ordinary dependencies.
Existing credited-action retention remains unchanged.

Retention prefilters held proposals before its bounded batch limit, locks the
investigation, and checks maintenance again on a fresh READ COMMITTED snapshot.
This covers a child-only maintenance update committed between the original
snapshot and the parent lock. Two additive expression indexes support exact
finding and queue-identity lookups. The existing generic-plan gate now includes
6,000 distinct proposal objects with at least **8,274 stored bytes each**; a
separate 6,000-case check verifies the SRE full-scope proposal index.

Maintenance requires the coordination and target pools to have the same
host/port/database binding. Separate pools and roles for the same binding are
accepted. Split control/target databases omit the maintenance bridge; existing
observations and backend-cancel wiring remain available. Distributed retention
holds are not implemented.

Actual-source local validation passed **859 test/subtest records, zero failures
and no skips** across the complete `internal/sre`, `internal/sre/action`,
`internal/retention` and `internal/schema` packages with `-p=2 -race -cover
-tags=integration,e2e`. Package times were **80.647s, 19.060s, 49.209s and
29.667s**, respectively. The ten owned source hashes matched before/after;
stderr was empty. Evidence:
`.local-test-results/gap-dev5-retention-full-owned-summary.json` and the untouched
JSON event stream. Independent review approved the correction. The native
package also compiled with no test selection (`-run '^$'`); live dev.5 acceptance
is recorded separately after the fresh whole-package run.

The new tests exercise actual approved execution, SRE evidence deletion with a
tombstone, generic cleanup, a working typed rollback, terminal timeline cascade
and eventual audit cleanup. They also cover the post-commit/pre-save record,
exact queue identity before save, refusal to retain unrelated actions, scoped
UUID collisions, each maintenance state, stale snapshots, held-batch starvation,
and retention while real execution and rollback wait for the table lock.

The initial dev.5 whole native attempt exposed an existing-install upgrade
failure: the deployed prototype's named state check has nine states and omits
`uncertain`. `CREATE TABLE IF NOT EXISTS` preserved that older check. The actual
maintenance execution completed, but the controlled uncertainty update failed
with PostgreSQL `23514`, so that attempt does **not** prove typed rollback or the
new native retention lifecycle. Read-only catalog evidence is preserved in
`.local-test-results/gap-dev5-native-final-state-constraint-catalog.json`; the
whole red invocation remains unchanged.

The correction replaces only the owned named state check with the canonical ten
supported states, validates existing rows in the same transaction, and compares
the complete `pg_get_constraintdef` expression before its idempotent fast path.
Unknown states remain outside the canonical constraint; retention holds them
fail-closed if a later schema ever introduces one. The local schema regressions
passed with race detection and coverage: the exact old nine-state check preserves
an existing proposal, wrong `NOT IN` / extra `OR true` / extra `AND false` checks
are repaired, an unvalidated check is validated, repeated Bootstrap preserves
the canonical constraint OID, all ten supported states are accepted, unsupported
states are rejected, and a preexisting unsupported row fails validation without
changing that row or the previous check. Evidence:
`.local-test-results/gap-dev5-upgrade-schema-v2.json` (seven test/subtest passes,
zero failures/skips; package also passed). Independent review approved the code.

The focused production upgrade then passed against native PostgreSQL **18.6**
in **52.85s** (package **54.485s**, stderr empty):
`.local-test-results/gap-dev5-upgrade-native-maintenance.json` and its summary.
The test used the same isolated branch/database, the selected operator’s OAuth identity and verified TLS;
it exercised the actual production Bootstrap, recent-proposal and explicitly
fixture-injected uncertainty holds, actual typed rollback, terminal owned
cascade and the chain-head tombstone. It did not run the database-wide generic
cleaner or simulate an actual provider crash/recovery. Native identifiers:

| Focused upgrade proof | Observed value |
| --- | --- |
| Investigation | `1168e428-8341-4062-99e1-2245515099f3` |
| Actual `stat_tables` evidence | `0788f06e-7283-4a5e-8fb7-0bd15a411a58` |
| Maintenance proposal | `f2a1485b-e813-4fad-a084-8d9051ec1b80` |
| Queue / action / execute decision | **6 / 74 / 170** |
| Created statistics OID | **270352**, built then verified absent after exact rollback |
| Final tombstone chain head | `a990ed327db6992a0c076299b5bad63e4386fc362570b78e9e72c438e6a9b4b8` |

A read-only catalog afterward confirmed the canonical check includes `uncertain`,
is validated, and has OID **270343**; both added expression indexes are valid
and ready. Evidence:
`.local-test-results/gap-dev5-upgrade-native-state-constraint-catalog.json`,
recorded **2026-10-30 18:02:09 UTC**. This targeted run occurred while the
independent elapsed-clock correction was being edited, so the whole-repository
source digests differ. It is targeted upgrade/lifecycle evidence, **not** final
frozen-source acceptance. The earlier red whole invocation remains red; a new
whole run is required after the joint source freeze.

## Final dev.5 frozen-source native acceptance

After the canonical constraint migration and elapsed-clock correction, a fresh
single whole native invocation passed **79 test/subtest records, zero failures
and two explicit opt-in skips** in **435.223 seconds** (wall **440.727s**),
**2026-10-30 18:03:58–18:11:21 UTC**. This is a new invocation, not a composite
of the earlier red run and focused proof. Go/module sources were unchanged,
SHA256 `3df43110e9f5de27cf43b8793fd874b48258b326f937acafe16b52265e8d5f35`;
stderr was empty. Evidence prefix:
`.local-test-results/gap-dev5-native-post-upgrade` (`.json`, `-summary.json`,
`-verification.json` and five separate reports). The raw stream is 219,474 bytes,
SHA256 `d2a1b16894f69b83942de12a35716030bf537703a2d76383df4b28ae64b778d3`.

The maintenance root passed again in this frozen whole run, including recent
proposal and explicitly injected uncertainty retention holds, actual typed
rollback, exact statistics-object absence, terminal owned investigation cascade
and a verified chain-head tombstone. It used no database-wide generic cleaner
and establishes no actual provider crash or automatic recovery:

| Frozen whole-run maintenance proof | Observed value |
| --- | --- |
| Investigation | `c035f7e4-789e-46b0-803d-6553e9547a46` |
| Actual `stat_tables` evidence | `92083506-bc42-4738-99c9-e35b8c93aec7` |
| Maintenance proposal | `c60da49f-c327-4ec6-a5fc-81951f8adda1` |
| Queue / action / execute decision | **7 / 75 / 174** |
| Created statistics OID | **270375**, built then verified absent after exact rollback |
| Tombstone chain head | `7e724f481a1758a4adbab2aec680e4e43261535e0b723813325d8d92c4996b05` |

The subsequent read-only catalog at **18:12:05 UTC** confirmed PostgreSQL
**18.6**, canonical validated ten-state check and both valid/ready expression
indexes. The constraint OID stayed **270343** across the fresh whole-run
Bootstrap, verifying the native idempotent fast path. Evidence:
`.local-test-results/gap-dev5-native-post-upgrade-state-constraint-catalog.json`.
The earlier focused proof is retained above; the original old-constraint red
whole invocation remains unchanged and is not counted as a pass.

## Historical dev.4 native evidence

**Final fixed-source full-package acceptance:** the same maintenance root passed
again in **51.20 seconds** inside the fresh complete native invocation after
the historical query-filter fix. That package passed **79 test/subtest records,
zero failures and two intentional unrelated opt-in skips in 453.537 seconds**.
Before/after Go/module source SHA256 matched
`0878423b465ddcf7e4671fbcd8e3e09918bafd76af43a9d5b8c17f29f750526c`.
The exact endpoint/database, the selected operator’s OAuth identity and verified TLS remained
unchanged; no grants or App configuration changes were used.

| Final-source identifier | Observed value |
| --- | --- |
| Investigation | `ba4a3721-4e27-4fe2-b3a8-8c11389aad5c` |
| Actual `stat_tables` v3 evidence | `29e423de-6ab6-4711-ae7a-290b8aad9266` |
| Maintenance proposal | `90dfea13-9097-45cb-8e84-56771fbcc11c` |
| Approval queue / action / execute decision | **4 / 56 / 128** |
| Created statistics OID | **253967**, built and then verified absent after exact rollback |

The final root again verified approval/replay/manual-bypass refusal, durable
policy linkage, structural creation/verification, exact-object removal and
investigation hash chain. Its owned table cleanup passed. ANALYZE statistics
remain unrestored, with no workload-benefit, incident-recovery or App-principal
execution claim. Evidence is the untouched
`.local-test-results/gap-dev4-native-history-filter.json` plus its summary and
verification artifacts; the native root emitted the identifiers above.
The earlier independent native proof remains preserved below.

- Test: `TestLakebaseSREStatisticsMaintenance`, 49.94 seconds; package 51.571 seconds, `-race`.
- Raw evidence: `.local-test-results/gap-dev4-native-maintenance-v3.jsonl`.
- Scope: profile `<test-workspace-profile>`; `projects/<project>/branches/<branch>/endpoints/<endpoint>`; database `pg_sage_test`; verified TLS and the existing native OAuth identity. Unique fixture table in `sage_live`, dropped during cleanup. No added privileges, passwords, customer writes, or deployed App setting changes.
- Investigation: `bcd7cc68-e083-4d35-a0e9-882fd8194462`.
- Actual bounded `stat_tables` v3 evidence, including physical database/table OIDs: `db6c7a38-e0c4-4221-9015-9e3f1dcac702`.
- Maintenance proposal: `f74927a7-25fd-44b7-8ddd-ae810b6b381a`; approval queue item `2`; action log `46`; standing-policy execute decision `102`.
- Created statistics object OID `237651` was built and then removed by the exact guarded rollback. The investigation event hash chain verified. Replayed approval and generic manual bypass were refused.
- Performance verification was finalized as `unverifiable` with no value or autonomy credit. The proposal and queue separately record the successful **structural** built-object check. The test supplied an explicit fixture correlation hypothesis and a durable operator review conclusion; it did not ask a model to diagnose or choose the change.

## Safety and limits

`create_statistics` has its own typed proposal ledger, `sage.sre_maintenance_proposals`. The existing cancel-only ledger, automatic proposal loop, and action class are preserved. The bridge has no automatic maintenance proposal, approval request, execution, notification, or rollback path. Root API wiring requires an operator/admin to propose and request approval, uses the existing approval queue, and requires an admin for explicit rollback.

The request must cite fresh, retained, same-investigation, hash-verified `stat_tables` evidence that names the exact schema-qualified table and physical database/table OIDs. Legacy name-only evidence is refused; a same-name replacement table cannot reuse prior observations. The operator supplies the columns, supported statistics kinds, and rationale. Table counters establish observation scope; they do not prove that extended statistics will help. The immutable investigation read-policy snapshot must match the current trusted policy, permit `stat_tables`, match the target database and hashed host/port/database binding, and not be bound to a single query. Target discovery and the executor must use the same monitored pool. Target discovery has a five-second context budget, at most eight column reads, and no table-data read. Missing/inaccessible targets produce a sanitized invalid-request result; catalog outages produce a sanitized unavailable result.

Maintenance evidence freshness is a fixed default and hard maximum of **15 minutes**, independent of the cancel action's five-second default. Approval expiry is the earlier of the configured approval TTL (at most 15 minutes) and the evidence expiry. Evidence and live durable approval identity are checked again before claiming execution. A reserved `sre_maintenance:` queue identity cannot fall back to the generic SQL runner when the typed bridge is absent.

Before CREATE and ANALYZE, one transaction takes a bounded `SHARE UPDATE EXCLUSIVE` table lock and rechecks database OID, table OID, owner OID, and each selected column's attnum, type OID, type modifier, and collation OID. CREATE STATISTICS and ANALYZE execute atomically; failure leaves neither partial object creation nor partial ANALYZE effects from that transaction. The built-object check and created statistics OID capture occur in that same transaction. Existing operator trust, emergency stop, executor enablement, standing policy, provider support, lock timeout, statement timeout, and target lease checks remain in force.

Rollback reauthorizes the exact stored inverse before waiting and again under the table lock, then verifies that the statistics object's OID still matches the created object. A replacement object with the same name is preserved. **Rollback removes only the newly created statistics object and cannot restore the table's prior ANALYZE statistics.** Structural verification does not claim a performance benefit or incident recovery. There is no automatic rollback monitor for this bridge.

## App principal boundary

A read-only catalog snapshot at 2026-10-30 15:49:05 UTC (`.local-test-results/gap-dev4-app-statistics-ownership.json`) confirmed that deployed App principal `<app-principal-id>` has the `CAN_CONNECT_AND_CREATE` resource, owns its dedicated `pg_sage` database and `sage` schema, and owns all 135 `sage` base/partitioned tables. It has USAGE/CREATE on `public`, and has no superuser, CREATEROLE, or CREATEDB attribute. This snapshot used the selected operator’s own OAuth database credential for read-only catalogs; it did not retrieve an App secret or execute as the App principal.

**App-principal maintenance execution remains unproven.** The native mutation test uses the selected operator’s native OAuth role on an owned fixture. Schema/database ownership and resource attachment do not prove ownership of a monitored workload table; the bridge deliberately excludes the `sage` metadata schema. CREATE STATISTICS must have the required table ownership and schema permissions. No broad ownership changes or grants were made to obtain this proof. The deployed App remains in observation mode.

## Local validation

The complete `internal/sre/action` package passed in the task-owned, per-package local database fixture (15.923 seconds). The complete `internal/executor` package passed (159.742 seconds). The final focused maintenance lifecycle/refusal suite passed with `-race` after physical OID matching and target-error sanitization (7.486 seconds; `.local-test-results/gap-dev4-local-maintenance-race.log`). The final schema-CREATE/table-ownership permission clauses and complete positive lifecycle passed an additional focused race run (5.463 seconds; `.local-test-results/gap-dev4-maintenance-contract-final.log`). The earlier focused native v3 behavioral run preceded that permission-text correction; the final full-package native rerun above exercises the current frozen source.

The focused suite covers proposal/request idempotency, no mutation while pending, actual durable approval through execution/structural verification/rollback, approval and rollback replay refusal, generic manual and rollback bypass refusal, wrong digest, foreign evidence, unobserved table, legacy name-only evidence, table recreation before proposal, stale/purged evidence, pinned policy mismatch, unapproved/wrong-user/expired approval, missing typed runner, recreated table, changed column type, emergency stop, observation trust, missing standing policy, and preserving a replacement statistics object during rollback.

Relevant implementation: `sidecar/internal/sre/action/maintenance.go`, `maintenance_run.go`, `maintenance_db_test.go`; `sidecar/internal/executor/sre_statistics.go`; `sidecar/internal/schema/sre_maintenance_migration.go`; `sidecar/tests/lakebaselive/maintenance_test.go`. Root-owned API/runtime wiring is covered separately by API lifecycle tests and the final repository validation pass.
