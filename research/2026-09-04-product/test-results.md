## Test Results

**Command:** `go test -json -cover -coverprofile=../research/2026-09-04-product/vectorlab-final-coverage.out -count=1 ./internal/vectorlab`
**Total:** 46 passed, 0 failed, 0 skipped (test/subtest events)
**Coverage:** internal/vectorlab 93.2%.

### Skipped Tests (must be zero or justified)
- None. SKIP/TODO/PENDING audit found none in package runs.

### Failures (if any)
- Initial package run: 37 passed, 0 failed/skipped, 77.0% coverage.
- Second attempt failed before tests: PowerShell split the unquoted coverage path.
  Quoting the complete argument resolved it; no product defect or test skip.
- Follow-up run: 43 passed, 0 failed/skipped, 91.3% coverage.
- Native Windows race invocation could not start because CGO/compiler unavailable.
  Linux Docker race run: 43 passed, 0 failed/skipped, 91.3%, no DATA RACE.
- Final Windows run above: 46 passed, 0 failed/skipped, 93.2%.
- Final Linux race rerun: 46 passed, 0 failed/skipped, 93.2%, no DATA RACE.

### Coverage Gaps (packages below threshold)
- All tested business packages meet coverage thresholds: vectorlab 93.2%.
- Remaining branches chiefly concern pgx metadata/result transport errors and rare
  initialization failures; actual connection/permission/timeout failures were exercised.
- Whole-repository coverage is owned by the repair lane, not implied by this report.

### Bugs Found This Session
1. Post-test audit found unique identity was only assumed. Added catalog validation
   so identities outside the sampled top-k cannot falsify set-based recall.
2. Added ordinary-table and vector-column validation to reject views/partition roots
   and unsupported representations instead of silently accepting a broader contract.
3. Added hard manifest read size check; a limited decoder could otherwise accept a
   truncated whitespace tail and fail to reject an oversized input.
4. Strengthened a logically flawed read-only test: checking a write after a failed
   prepare could observe an aborted transaction rather than read-only enforcement.
   It now attempts a write in an active read-only transaction and asserts SQLSTATE 25006.

### Manual Checks Remaining
- None for the CLI slice. No GUI feature was introduced.
- CHECK-V01: PASS — real filtered HNSW plan and exact-reference evidence.
- CHECK-V02: PASS — strict JSON, resource bounds, malformed values, nonfinite vectors.
- CHECK-V03: PASS — empty/tied/underfilled/no-index/low-recall samples fail closed.
- CHECK-V04: PASS — hostile identifiers quoted; vectors and filter values bound.
- CHECK-V05: PASS — missing extension/table, permission denial, cancellation and lock timeout.
- CHECK-V06: PASS — RLS respected; independent concurrent sessions and no connection leak.
- CHECK-V07: PASS — actual built sidecar command produced demo-evidence.json.
- CHECK-V08: PASS — uncached 93.2% coverage, Linux race run, go vet and build.

### Demonstration evidence
- PostgreSQL 16.14, pgvector 0.8.2, 10,000 synthetic rows and three tenant cohorts.
- ef_search=10/off: worst recall 0.2, p95 1.9618 ms, rejected for recall and underfill.
- ef_search=100/strict_order: recall 1.0, p95 2.0008 ms, selected.
- ef_search=200/strict_order: recall 1.0, p95 7.5679 ms, qualified but slower.
- Exact baseline p95 3.4876 ms. These timings are illustrative single-machine data.
- Synthetic demo schema removed; catalog check returned zero remaining schemas.
- AST size audit: all newly introduced functions <=50 lines. New files <=500 lines;
  line-width check passed after formatting. Generated binary is not source for staging.

## Load-admission safety regression and pipeline contract repairs

**Command:** `go test -json -cover -count=1 ./internal/verify ./internal/vectorlab`
**Total:** 106 passed, 0 failed, 0 skipped.
**Coverage:** verify 85.9%; vectorlab 93.2%.

### Skipped Tests (must be zero or justified)
- None. Names/logs containing Pending describe verification state, not skipped tests.

### Failures (if any)
- No failures in this run. The preexisting catalog test had asserted an active
  connection ratio was CPU/I/O utilization; that assertion encoded the defect.
  It now asserts typed unavailable telemetry and zero fabricated metrics.

### Coverage Gaps (packages below threshold)
- All tested business packages meet thresholds: verify 85.9%, vectorlab 93.2%.

### Bugs Found This Session
- Host CPU, data I/O and log I/O were all fabricated from connection count.
  Native catalog-only automatic index admission now fails closed.
- NaN, infinity, negative and >100% load samples could bypass ceiling comparisons;
  the gate now rejects every malformed metric explicitly.

### Manual Checks Remaining
- None for deterministic gate tests; a real host/provider telemetry adapter is
  a missing runtime capability, explicitly documented, not an unchecked test.

**Command:** `go test -json -cover -count=1 ./internal/executor`
**Total:** 543 passed, 0 failed, 0 skipped.
**Coverage:** executor 74.6%.

### Skipped Tests (must be zero or justified)
- None.
### Failures (if any)
- None.
### Coverage Gaps (packages below threshold)
- All tested business packages meet thresholds: executor 74.6%.
### Bugs Found This Session
- No additional executor defect in this regression run.
### Manual Checks Remaining
- None for this package run.

**Command:** `go test -json -cover -count=1 -tags=e2e -run=TestPipelineCoverage_SQLShapes ./e2e`
**Total:** 21 passed, 0 failed, 0 skipped.
**Coverage:** e2e has no production statements; imported business-package coverage
  is reported in the package runs above, not inferred from this test-only package.

### Skipped Tests (must be zero or justified)
- None. Log references to pending rollback state are intentional lifecycle state.
### Failures (if any)
- None after repairing stale contracts.
### Coverage Gaps (packages below threshold)
- e2e is test-only with no production statements; thresholds do not apply.
### Bugs Found This Session
- Old B01-B05 fixtures expected automatic CREATE with no measured load evidence.
  Each now proves auto withholding, then uses supported ExecuteManual for real
  reviewed B-tree/INCLUDE/GIN/HNSW/partial-index SQL effects. No authority weakened.
- Old B13 expected unapproved backend cancellation. It now proves withholding,
  captures PID/query-start/query-ID/app/query evidence, supplies explicit approval,
  and proves the exact victim receives cancellation SQLSTATE 57014.
### Manual Checks Remaining
- None for SQL-shape scenarios. Full-binary A02 now checks detected FK findings and
  evidenced withholding; its complete run is recorded in the final audit report.

## Test Results — durable schema refusal and binary verification

**Command:** `go test -json -cover -count=1 ./internal/schemaguard`
**Total:** 36 passed, 0 failed, 0 skipped.
**Coverage:** schemaguard 87.9%.

### Skipped Tests (must be zero or justified)
- None; no SKIP/TODO/PENDING test outcomes.
### Failures (if any)
- Final run has none. Staged regression run had 34 passed, 2 failed, 0 skipped:
  missing durable route-failure recording and a malformed fixture missing its
  required schema/table. The fixture was corrected to reach the intended path.
### Coverage Gaps (packages below threshold)
- All tested business packages meet thresholds: schemaguard 87.9%.
### Bugs Found This Session
- Schema guard recorded its decision only after routing succeeded. Policy could
  record `execute/authorized`, then verification refused the proposal, leaving no
  durable unsuccessful outcome. Route failures now also record a parked decision
  with the same target, proposed SQL, and route. The reason does not imply no
  partial work occurred. Both routing and ledger errors remain distinguishable.
### Manual Checks Remaining
- None for this package.

**Command:** `go test -json -cover -count=1 -tags=e2e -run=TestPipelineCoverage_FullBinary ./e2e`
**Total:** 1 passed, 0 failed, 0 skipped; all nine CHECK-A01 through CHECK-A09 passed.
**Coverage:** e2e has no production statements; business package coverage is above.

### Skipped Tests (must be zero or justified)
- None.
### Failures (if any)
- Final repaired run has none. Earlier A02 failed because there was no durable
  withheld decision; live inspection confirmed only authorization rows existed.
### Coverage Gaps (packages below threshold)
- e2e is test-only with no production statements; thresholds do not apply.
### Bugs Found This Session
- The production audit gap above was fixed instead of weakening A02's assertion.
  The final binary detected the FK finding, created no FK index, and persisted two
  withholding decisions. Other checks proved vacuum, analyze, index cleanup,
  autovacuum tuning, and zero mutation for advisory-only findings.
### Manual Checks Remaining
- None for these binary scenarios.

## Test Results — command runtime business coverage

**Commands:** `go test -race -json -cover -count=1 ./cmd/pg_sage_sidecar` plus
instrumented `go test -json -count=1 -tags=e2e -run='^(TestSmoke|TestFleet)$' ./e2e`.
The subprocess build receives `GOFLAGS='-cover -covermode=atomic'`; startup and
command test binaries write Go counters into `tasks/product-main-release-coverage`.
`go tool covdata percent` reports actual runtime coverage across those executions.

**Total:** command 102 passed, 0 failed, 0 skipped; startup 38 passed, 0 failed,
0 skipped. Linux race detector found no remaining race in the final command run.
**Coverage:** command unit/integration run alone 41.0%; combined current-source
runtime coverage 70.3% (1,393 of 1,981 statements). Command authority wiring is
classified as business logic, not relabeled as a utility to reduce its threshold.

### Skipped Tests (must be zero or justified)
- None in these runs. The metadata fixture requires the explicit disposable test
  DSN; it was provided and every integration scenario executed.
### Failures (if any)
- None in final runs. Early fixture errors were corrected: stored trust/mode and
  ledger risk enums must use their real schema values; bootstrap must reuse an
  existing admin; deleted store reads return `pgx.ErrNoRows`; logwatch needs an
  actual log file; enabled LLM constructors require a configured endpoint.
- One shell invocation split an unquoted `-test.gocoverdir` argument; it ran no
  tests and was replaced with a correctly quoted invocation.
### Coverage Gaps (packages below threshold)
- All business packages in this lane meet thresholds using measured runtime
  coverage: vectorlab 93.2%, verify 85.9%, schemaguard 87.9%, executor 74.6%,
  command runtime 70.3%. Command-only tests alone remain 41.0%; the documented
  startup scenarios are necessary to reproduce the combined threshold.
### Bugs Found This Session
- Managed database configuration omitted its stored execution mode even while
  the executor retained it. It now carries the same resolved mode into instance
  configuration, capability reporting, and runtime consumers. A real approval
  database exposed the mismatch before the fix.
- The metadata reconnect goroutine reread global shutdown context. The race
  detector caught a lifecycle cleanup race; it now receives its context when
  launched so its ownership remains stable.
### Post-Test Audit
- Added real create/replace/delete, failed create cleanup, failed replacement
  preservation, encrypted credential restart, stopped/healthy reconnect handling,
  wrong-database health checks, agent collector lifetime, LLM feature flags and
  isolated budgets, exact MCP ledger filtering, proposal-without-ratification,
  absent-authority refusals, logwatch startup/shutdown, notification route wiring,
  malformed configuration reload, and supported vector CLI entrypoint scenarios.
- No model or notification requests were sent. LLM/notification wiring tests use
  an unreachable local endpoint and only construct clients/routes.
### Manual Checks Remaining
- None for these scenarios. Parent audit owns complete application/browser results.
