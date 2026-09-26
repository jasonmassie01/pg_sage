# pg_sage pre-remediation durability and historical-state validation

Date: 2026-09-26. Frozen code: `b396595059d2b1b312a22231fdbfd1d2cfef9530`.
Audit clone: `core-probe-repo`. This work validates faults and plans state repair;
it does not fix product code, repair historical rows, or touch live databases.
The coordinating audit owns the clean full suite, retention/stop gates and real
sidecar fleet validation. This report owns narrowly selected durability probes.

## Outcome

The prior bugs survive executable validation. Required index reverts disappear
from recovery after either a failed database connection or a process exit at the
persisted-verdict boundary. RCA restart creates duplicate incident identities
and leaves old incidents active after the workload clears. A concurrent stale
engine flush reverses a committed manual resolution. The retained cache-unit,
suppression and mismatched inverse regressions still fail. Concurrent recovery
also finalizes one durable watch twice: there is no durable single-worker claim.

Across 15 distinct top-level regression/control tests executed on Windows,
3 passed and 12 failed, with zero skipped. Helper child-process output is not
counted as separate feature passes. The Linux race run repeats two concurrency
cases and is reported separately below. Failures are intentionally preserved;
no implementation or expected behavior was changed to make them pass.

The diagnostic SQL executed successfully against seeded disposable fixtures in
a read-only transaction, both with and without optional AgentDB tables. This
validates syntax and selected result cohorts, not the health of any live data.

## Isolation and process

- Disposable PostgreSQL: `pgsage-validate-core-20260926`, image
  `pgvector/pgvector:pg17`, bound only to `127.0.0.1:55442`.
- The existing package TestMain creates a separate validated fixture database
  per package/process and deletes it afterward. Test credentials contain no
  production secrets. No configuration/runtime DSN was discovered or reused.
- New tests are staged in the audit clone only. Phase-one patches and UTC stage
  markers precede their separate test runs. Original C01/C02/C04 probes remain.
- Production verification store, observation queries, lifecycle finalization,
  top-level index DDL and RCA persistence run against PostgreSQL. Authorization
  is a stateless allow-gate test double so these probes isolate durability;
  they do not establish policy, trust, replica or HTTP authorization safety.
- Query evidence is a controlled fixture: 100 calls at 1 ms before and 100 calls
  at 3 ms after. The tests prove recovery of the resulting regression verdict,
  not causal attribution of real-world latency to an index.
- Concurrency is deterministic: a scheduling barrier follows real ListDue reads;
  a PostgreSQL row lock orders manual resolution against RCA persistence.
- No production file was modified. The only clone changes are seven test files.

## Executable findings

| Check | Result | Observed state and meaning |
|---|---|---|
| CHECK-CORE-P01 ordinary real revert | PASS | Index absent, verification `revert/completed`, action `rolled_back`. The real DDL and fixture work. |
| CHECK-CORE-P02 fresh pool/engine after verdict-before-effect boundary | FAIL | Index present, verification `revert/completed`, action `pending`; fresh ResumeDue does nothing. |
| CHECK-CORE-P03 connection lost during real DROP, then recovery | FAIL | Test waits for exact DROP backend in a PostgreSQL lock wait and terminates it. Error includes SQLSTATE 57P01. Index remains, action `rollback_failed`, verification terminal. Fresh recovery never retries. |
| CHECK-CORE-P04 two concurrent recovery workers | FAIL | Both workers read one pending watch and call real Revert; observed finalization count is 2, expected 1. Idempotent DROP ends with index absent in this fixture. |
| CHECK-CORE-P05 RCA in-process dedup and clearing control | PASS | Two hot cycles produce one row; healthy cycles resolve it. The restart failure is not a failed detector or malformed fixture. |
| CHECK-CORE-P06 RCA reconstructed-engine recurrence | FAIL | Same signal becomes two active incident rows. |
| CHECK-CORE-P07 RCA reconstructed-engine healthy recovery | FAIL | Eight healthy cycles leave old persisted incident active. |
| CHECK-CORE-P08 concurrent manual resolution / stale flush | FAIL | Manual transaction commits resolved_at; previously captured in-memory incident flush then overwrites it with NULL. |
| CHECK-CORE-P09 C02 suppression | FAIL | Re-observation produces one open plus one suppressed row, expected no open row. |
| CHECK-CORE-P10 C04 forward/inverse pair | FAIL | Updated CREATE index_b retains DROP index_a. |
| CHECK-CORE-P11 C01 actual collector SQL units | FAIL | SQL returns 80 for 800/1000 hits; fractional consumer contract requires 0.8. |
| CHECK-CORE-P12 historical-state SQL | PASS | Read-only execution succeeds, optional-table branches work, seeded suppression/inverse/lost-revert/duplicate-incident cohorts appear. |
| CHECK-CORE-P13 RCA actual process restart recurrence | FAIL | Two distinct OS processes each run production NewEngine/Analyze/Persist; second leaves two active identities. |
| CHECK-CORE-P14 RCA actual process restart clearing | FAIL | A new OS process runs eight healthy cycles after the first process exits; persisted incident remains active. |
| CHECK-CORE-P15 verifier actual process exit after verdict commit | FAIL | Child exits code 77 immediately after engine.Watch commits revert. Parent recovery leaves index present and action pending. |

P15 is an abrupt, controlled process exit at the exact application boundary,
not a server power-failure test. P13/P14 use new test processes invoking the same
constructors as startup, not the full sidecar binary. The coordinating audit's
binary checks provide separate runtime evidence.

### C15: durable decision is confused with completed effect

`sidecar/internal/verify/engine.go:55-70` persists completion before lifecycle
finalization. `verify/postgres.go:232` reloads only `pending/extended` rows.
`executor/verified_index_lifecycle.go:113-138` runs the effect later;
`executor/index_verification_runtime.go:111-130` can record rollback failure
without making the watch eligible again. The real connection fault and process
exit prove this ordering loses remediation. P1 remains justified: an index the
verifier requires removing can remain installed indefinitely.

Required repair contract: persist a retryable effect intent separate from the
measurement verdict; claim it durably; re-authorize against fresh runtime and
object identity; perform/reconcile idempotently; then record effect completion.
A withheld action must remain durably blocked/reviewable. Reconciliation must
not turn an index name that was later reused into permission to drop a new index.

### New concurrency evidence: no durable claim on due watches

`verify/postgres.go:223-243` reads due rows without a claim/lease or conditional
transition. `verify/engine.go:34-50` evaluates each read. Two workers can both
finalize one action. P2 robustness issue with an explicit overlapping-worker
precondition; the fixture proves duplicate finalization, not data loss. Single
worker scheduling can reduce exposure but does not provide durable ownership.
A Go race detector cannot detect this database-level logical race.

### RCA restart and manual-resolution persistence

`sidecar/internal/rca/rca.go:119` initializes an empty incident set. Actual
standalone/fleet startup constructs the same engine (`cmd/pg_sage_sidecar/main.go:702`
and `:1497`; `metadb.go:449`), with no incident reload. The process tests prove
both restart consequences. `rca/rca.go:380` replaces resolved_at on upsert with
stale in-memory state; `api/handlers_v09.go:426` performs the manual resolution.

Required repair contract: restore active identity and counters at startup;
persist transitions with a version/CAS or event log; let explicit human
resolution survive stale writes; define whether renewed evidence reopens the
same case or creates a linked occurrence. Healthy cycles after restart must
resolve a previously tracked incident only with fresh, successfully collected
evidence. Long gaps must not be silently counted as healthy cycles.

## Historical-state diagnostics and repair disposition

Artifact: `evidence/preflight-state-diagnostics.sql` (psql script).
Run against the designated deployment before its repair, including each monitored database and any distinct meta
DB using a read-only role. It uses `REPEATABLE READ READ ONLY`, 30-second statement
and 2-second lock timeouts; optional missing tables are explicit. Schema-version
mismatches fail loudly. It emits IDs, timestamps, hashes and narrow object names,
not SQL bodies, connection_info, secret references, provider outputs or payloads.
No live-state result is asserted in this report.

| Diagnostic cohort | What it can establish | Repair disposition before writes |
|---|---|---|
| DIAG-02 suppression + open row | Candidate resurrection while indefinite/unexpired suppression exists. | Verify DB/object/rule identity and suppression intent; preserve suppressed record and audit history; make suppression authoritative. Do not delete rows as a shortcut. |
| DIAG-03 stale open findings | Age and category distribution only. | Re-run the relevant detector using fresh complete observations. Age alone cannot prove resolution or permit execution. |
| DIAG-04/05 inverse review | Missing inverse and narrow simple CREATE/DROP mismatch candidates. Regex is not a SQL parser; qualification differences can be false positives. | Quarantine affected executable proposals/approvals through a reviewed future change. Reconstruct correct forward/inverse from immutable evidence and actual object identity; otherwise regenerate and reapprove. No blanket replacement from current finding SQL. |
| DIAG-06 queue vs finding revisions | A queue row differs from the latest mutable finding. A legitimate older immutable approval can also differ. | Determine approved revision and hashes. Bind approval to an immutable proposal; never silently upgrade its SQL or inverse. |
| DIAG-07 terminal revert without effect proof | High-priority lost/withheld/failed revert cohort. | Read current catalog, original OID/fingerprint if recorded, decision, inverse and policy. Separate effect already happened, still needed, unsafe/stale identity and explicitly withheld cases. Do not blindly mark all terminal rows pending or rerun their inverse. |
| DIAG-08/09 duplicate/link/due anomalies | Duplicate watch linkage, dangling correlation, overdue watches and contradictory state. | Preserve every evidence row; choose a canonical action/watch linkage after reconciliation. Introduce an idempotent migration with per-row preconditions and audit events; do not delete duplicate evidence blindly. |
| DIAG-10 absent attribution/proof | Pending no-watch actions, credited rows without identity or successful completed proof. | Resolve topology/identity from reliable deployment records. Unknown remains unknown; revoke/recompute unsupported credit only with a reviewed accounting migration. Never guess database_id from a similarly named DB. |
| DIAG-11/12 incident duplicates/staleness | Identity candidates using source, sorted signals and first affected object; stale active rows. | Validate complete affected-object sets and DB identity before merging. Preserve occurrence/history links, select canonical cases, and re-evaluate conditions. A manual resolution erased to NULL cannot be recovered from this table alone; use external logs/PITR if available. |
| DIAG-13 cache units | Distribution with values above 1 and sample time range. | Normalize only cohorts with known producer/version provenance. Do not divide every value by 100: legacy fractions and percent values near 0/1 are ambiguous. Recompute derived alerts where possible. |
| DIAG-14/15 provisioning/cleanup claims | In-flight/stale claims, unfinished attempts and incomplete registration candidates. | Reconcile provider operation ID/idempotency key and actual resource status read-only before any replay. An expired claim or missing local receipt does not prove the provider operation failed. Do not automatically free claims, reprovision or destroy resources. |
| DIAG-16 local claimed schema absent | Same-host catalog mismatch, potentially a meta-DB false positive. | Run on the actual resource host and verify tenant ownership; do not recreate or drop based solely on this query. |
| DIAG-17/18 live authorization/receipt ambiguity | Consumed authorization without receipt, mismatched plan/idempotency correlation. | Treat completion as unknown until provider readback and logs reconcile it. Preserve consumed authorization; no blind retry of a possibly completed non-idempotent operation. |

Before any later repair: retain a consistent evidence export/backup, define
rollback and conflict handling for the migration, dry-run cohort counts, review
representative rows, and make changed-row predicates explicit. This is a repair
plan only; no repair SQL is supplied or executed.

## Test Results

**Command:** `go test -cover -count=1 -timeout=90s -run '^(TestPreflight|TestAuditSuppressedFinding|TestAuditUpdatedForward|TestAuditCollectorCache)' -v ./internal/executor ./internal/rca ./internal/analyzer ./internal/collector`

**Total:** corrected main run: 2 passed, 9 failed, 0 skipped (11 top-level tests).
**Coverage:** executor 9.3%; RCA 34.3%; analyzer 1.9%; collector 0.0%.
This selected regression run is not whole-package or repository coverage.

Additional separately staged runs:

| Evidence log / named pattern | Passed | Failed | Skipped | Coverage |
|---|---:|---:|---:|---|
| `preflight-core-attempt1.log` / main pattern | 2 | 9 | 0 | executor 8.2%, RCA 34.3%, analyzer 1.9%, collector 0.0% |
| `preflight-core-attempt2.log` / corrected main pattern | 2 | 9 | 0 | executor 9.3%, RCA 34.3%, analyzer 1.9%, collector 0.0% |
| `preflight-core-diagnostics-attempt1.log` / `^TestPreflightStateDiagnostics` | 1 | 0 | 0 | executor 0.1% |
| `preflight-core-process-attempt1.log` / `^TestPreflightRCAProcessRestart` | 0 | 2 | 0 | parent RCA 0.0%; child hot 27.3%, healthy 19.5%, not merged |
| `preflight-core-crash-process-attempt1.log` / `^TestPreflightVerifierProcessCrash` | 0 | 1 | 0 | parent executor 1.5%; abrupt child emits no coverage |

All commands above use `-cover -count=1 -timeout=90s -v`; package selection and
pattern differ as shown. JSON-free text logs, exit-code files and test patches
are retained. Exit code 1 means regression assertions failed; it is not a pass.

### Skipped Tests (must be zero or justified)

Zero selected tests skipped. Logs were checked for SKIP/TODO/PENDING markers.
The text `outcome=pending` is observed action state, not a pending/skipped test.
Child helper PASS entries mean successful setup/execution only; their parent
assertions failed. The clean full suite's separately documented skips belong to
the coordinating baseline, not these selected tests.

### Failures (if any)

The 12 distinct failing behavior checks are listed in the numbered matrix.
Attempt 1 additionally had **two invalid fixture results**, not two demonstrated
product defects: pool-close cleanup ran before table cleanup after the restart
probe, leaving the fixture table behind. Only cleanup was corrected to open an
independent disposable-DB connection. Assertions were unchanged. Attempt 1 and
its pre-correction patch remain available; attempt 2 actually exercises both
connection loss and concurrent recovery and records their independent failures.

### Coverage Gaps (packages below threshold)

- Executor: selected runs 0.1%-9.3%, below 70%. These tests cover durable failure
  boundaries, not the complete action catalog, config application, trust/mode
  policy, manual execution, rollout, retention, host load or backend signals.
- RCA: main run 34.3%, below 70%. No LLM provider, log ingestion, all decision
  trees, multiple DB wiring, escalation boundaries or full signal taxonomy here.
- Analyzer: 1.9%, below 70%. Only suppression and complete proposal pair updates;
  rule categories, LLM generation, resolution sweeps and refresh scheduling omitted.
- Collector: 0.0%, below 70%. The test executes the real SQL constant through
  PostgreSQL; Go statement coverage does not count SQL string evaluation.
  Collection orchestration, timeouts, provider adaptation and paging are omitted.
- Subprocess coverage is explicitly separate, not added arithmetically to parent
  percentages. No claim that any package meets coverage thresholds is made here.
  The coordinator's full clean run supplies broad coverage and remaining gaps.

### Bugs Found This Session

1. Confirmed C15 with actual database disconnect and separate process exit.
2. Confirmed RCA restart identity loss and inability to clear persisted incidents.
3. Confirmed stale concurrent RCA persistence overwrites manual resolution.
4. Added executable evidence of duplicate finalization by concurrent due-watch readers.
5. Reconfirmed C01, C02 and C04 without implementation changes.
6. Fixed one audit-fixture cleanup ordering error; this was not a product fix.

### Post-test audit

- Assertions check actual index existence, persisted outcome/verdict/completion,
  incident row count/state, matched SQL pair and produced metric value. Empty
  success-return stubs would fail the happy-path controls and state assertions.
- Backend termination is verified against the exact waiting DROP in the current
  disposable DB; a failure to reach that point is a fixture failure, not evidence.
- Manual-resolution interleaving waits for the actual row-lock contention before
  commit. It is a stale-state race even with properly synchronized Go memory.
- True process tests were added after identifying the first tests reconstructed
  objects in one process. Original tests remain, providing both levels of proof.
- The allow-gate hides real policy denials by design; policy-stop/retry combinations
  remain separate coordinator coverage. No inference of an authorization bypass
  is drawn from these durability tests.
- Missing-baseline, cancellation during observation, rollback permission denial,
  index-name reuse, failure after successful DDL before outcome update, and
  provider mutation crash boundaries are not all injected here. They remain
  explicit acceptance cases for remediation and are not claimed passing.
- Diagnostic SQL has positive seeded-result assertions and missing-table coverage.
  Its simple SQL-name heuristic cannot validate quoted identifiers, transaction
  semantics, schema equivalence, or provider resource ownership.

### Manual Checks Remaining

- CHECK-CORE-M01: MANUAL — inspect the designated live historical cohorts before any
  repair. No authoritative live deployment was selected for this isolated pass.
- CHECK-CORE-M02: MANUAL — reconcile ambiguous live provider outcomes by read-only
  provider identity/status evidence before deciding whether repair/retry is safe.
- CHECK-CORE-M03: MANUAL — obtain absent original proposal/object identity from
  retained logs/backups; some overwritten historical intent may be unrecoverable.

These manual items are future remediation prerequisites, not passes. This
pre-remediation validation is a failing behavior run and does not certify safety.

### Linux race run

**Command:** `go test -race -cover -count=1 -timeout=120s -run '^TestPreflight(ConcurrentRecovery|RCAManualResolution)' -v ./internal/executor ./internal/rca`

Environment: disposable `golang:1.25` container with the audit clone mounted
read-only; connection restricted to the designated disposable PostgreSQL.
**Total:** 0 passed, 2 failed, 0 skipped.
**Coverage:** executor 8.3%; RCA 27.8%, both below the 70% business-logic floor.
**Failures:** duplicate watch finalization (2, expected 1) and stale persistence
erasing manual incident resolution. No Go `WARNING: DATA RACE` was reported.
These are logical races across durable state, not demonstrated unsafe Go memory
access. This repeats two existing checks; it does not add to the 15 unique-test
count. Package scope and coverage gaps remain as documented above.

Evidence: `evidence/preflight-core-race-attempt1.log`, with exit-code companion.
After execution, new test lines were wrapped to the repository's 100-character
limit, with no assertions or behavior changed; the final staged test patch is
`evidence/preflight-core-final-tests.patch`. All new test functions are at most
50 lines and all new test files at most 500 lines. Original pre-run and failed
fixture patches remain preserved for comparison.

## Evidence handoff and cleanup

The source clone remains at b396595 with only the seven staged audit test files
listed in `preflight-core-final-tests.patch`; no unstaged clone changes remain.
`preflight-core-artifact-hashes.txt` records SHA-256 fingerprints for the final
patch and SQL. `preflight-core-marker-scan.txt` records zero SKIP/TODO/PENDING
markers and zero Go race warnings across the captured logs.

For a future isolated replay, provide `SAGE_TEST_DATABASE_URL` for the disposable
server. The diagnostic test additionally needs absolute paths in
`PREFLIGHT_DIAGNOSTIC_SQL` and `PREFLIGHT_DIAGNOSTIC_LOG`, plus the deliberately
fixed disposable container name `pgsage-validate-core-20260926`. The helper-only
`TestRCAChildProcessFixture` and `TestVerifierCrashChildFixture` must be invoked
through their parent tests; they reject execution without the explicit parent
fixture identity. Run the documented named patterns, not an unreviewed wholesale
copy into the canonical suite.

Both owned validation containers are now absent. Only
`pgsage-validate-core-20260926` and its anonymous volume were explicitly removed;
the owned Linux race container used `--rm`. Other agents' containers and the
user's PostgreSQL were not stopped or modified. Cleanup evidence is retained in
`preflight-core-container-cleanup.txt`.

