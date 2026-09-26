# Fixes — API / auth / MCP / explain / web (2026-09-26)

Branch `fix/2026-09-26-api-web`, worktree `C:/Users/jmass/pgsr-fix-api-web`, base `9a3cac7`.
Process: Phase 1 failing tests committed first (`e94c63d` P0/P1, `6dc58fe` P2,
`3451ee7` notify events), each confirmed failing for the stated reason; Phase 2 fixes one
logical change per commit. Security items: impact + fix only.

## Status

| ID | Status | Commit | Test(s) | Notes |
|---|---|---|---|---|
| G6-B01 (P0) | FIXED | 5489570 | `TestExplain_MultiStatementBodyCannotMutate` (integration, probe table unchanged), `TestValidateExplainQuery_*`, `TestExplain_RejectsMultiStatementBeforeDB` | Impact: operator could leave the READ ONLY tx via `SELECT $1; COMMIT; ...`. Fix: single-statement allowlist (SELECT/WITH/VALUES/TABLE) with a literal/comment/dollar-quote-aware scanner; PREPARE via `PgConn.ExecParams` (extended protocol); EXPLAIN via `QueryExecModeExec`. The review's suggested `conn.Exec(ctx, sql, pgx.QueryExecModeExec)` would NOT work: pgx forces simple protocol when no bind args remain. Also fixes the explain half of G1-B29 (DEALLOCATE after ROLLBACK; `TestExplain_FailedExecuteDoesNotLeakPreparedStatement`). |
| G1-B22 | FIXED | b2f9903 | `TestExplainHandler_UnknownDatabaseReturns404` | |
| G9-B01 / G6-B05 (P0) | FIXED | e6dfc0f, ac3c85a | `TestActionsLedger_RecordKindDistinguishesQueueAndLog`, `TestRollbackHandler_RejectsQueuedRecordKind`, `TestFleetRollbackHandler_RejectsQueuedRecordKind`, `Actions.ledger.test.jsx` | Rows carry `record_kind` + `ledger_key`; UI shows rollback only for `executed` (fails closed if absent); server rejects any non-`executed` record_kind before the executor. Absent record_kind still accepted for existing API clients. |
| G9-B14 | FIXED | e6dfc0f, ac3c85a | `expands exactly one row when queue and log ids collide` | |
| G6-B02 / SURF-01 | FIXED | 2246998 | `TestMountedMCP*` (real mounted router + real MCP runtime/server), `TestMutatingTools*`, `TestPostgresAccessPersistsPrincipalAsProposalActor` | Impact: viewer could propose policy / trigger MCP writes. Fix: API binds session user as `mcp.Principal` (401 without user); mutating tools require operator/admin (-32001) before any backend call; actor `mcp:user:<id>` persisted. stdio binds a local `stdio` operator. |
| G6-B04 / SURF-02 | FIXED | 53391e8 | `oidc_identity_test.go`, `oidc_link_test.go` (unverified, subject change, issuer change, password-account link refusal, legacy link, concurrent first login) | Impact: unverified email equal to an admin's logged in as that admin. Fix: require `sub` + `email_verified=true`; match on issuer+subject; never auto-link to a password account or another identity (403, admin must link). Additive migration (cross-area, schema). **Gap:** there is no admin "link identity" UI/endpoint yet, so existing password users cannot switch to SSO. |
| G6-B11 | FIXED | 53391e8 | `TestFetchOIDCIdentity_DoesNotLogEmail` | |
| G9-B02 | FIXED | 56bb41e | `TokenBudgetBanner.test.jsx` | e2e fixtures + `token-budget.spec.ts` fixed to the real shape (e2e not run per brief). |
| G9-B22 | FIXED | 56bb41e | `hides the reset control from non-admins` | |
| G9-B03 | FIXED | 2642f96 | `SettingsPage restart > sends Content-Type application/json` | |
| G9-B04 | FIXED | ac3c85a | `labels queued proposals as awaiting approval`, `never offers rollback on a queued proposal` | Executed-row risk is still the SQL-prefix estimate (`deriveDisplayActionRisk`); tooltip no longer claims auto-run. |
| SURF-12 / G6-B10 | FIXED | e6dfc0f, ac3c85a | `TestActionLogVerificationStatus_UsesDurableVerification`, `TestActionsLedger_VerificationStatusFromDurableRecord` | applied / pending / verified / inconclusive / failed / reverted from latest `sage.verification`. |
| SURF-09 / G6-B09 | FIXED (API) | c8fbdda | `TestSourceQueryHintFromMap_PreservesInt64QueryID`, `TestQueryHintsHandler_SerializesQueryIDAsString` | Case evidence `detail.queryid` (built in `internal/cases`) is still a JSON number; Case `source_ids` is already a string. |
| G6-B06 / SURF-16 | FIXED | 0b15046 | `login_limiter_test.go` (capacity, 50-way concurrency => exactly 5, normalisation) | |
| SURF-10 | FIXED | 2a2a393, 9fc0006, b6108f1 | `CasesPage.actions.test.jsx` | Suppress / unsuppress / resolve in Cases (role-gated, per-case database). Deleted Incidents/Forecasts/SchemaHealth/QueryHints/DatabaseSettings pages. **Kept `Findings.jsx`**: manual execute from a finding is not ported. |
| SURF-11 | FIXED | 8849889, 2a2a393 | `TestSortCasesGlobally`, `does not render fabricated impact or urgency scores` | Scores removed (not computed). Finding observation time still set at projection (`internal/cases`, not owned). |
| SURF-13 | FIXED | 70d7456 | `TestShadowReportHonoursPolicyDecisionAndExpiry` | Cross-area (`internal/cases/shadow.go`). |
| SURF-14 / G9-B20 | FIXED (removed link) | c674c6d | `ValuePage.test.jsx` (updated) | No ledger route exists; id shown as text. Incident-credit producer is out of scope. |
| SURF-18 | FIXED | 419dacf | `TestWriteValueMetricsEmitsVerifiedToilAndIncidentSeries` (updated), `TestWriteValueMetricsReportsQueryFailure` | Renamed to gauge `pg_sage_toil_minutes_saved`; added `pg_sage_value_metrics_up`. `specs/agent-native-autonomy-build-spec.md` still names the old series. |
| SURF-03 | DEFERRED | — | — | Not small: per-pool aggregation would still fail because `internal/value/postgres.go` scans NULL `d.name` into a string and executors write `database_id` NULL. Needs value + executor changes and a ledger-topology decision. |
| G9-B05 | FIXED | 7359c24 | `useAPI.test.jsx` | Data keyed by resource (URL minus from/to). Also fixes G9-B18. |
| G9-B06 | FIXED (UI) | 2a2a393 | `does not show a zero-time expiry` | `CaseAction.ExpiresAt` non-pointer in `internal/cases` unchanged. |
| G9-B07 | FIXED | 2a2a393 | as SURF-11 | |
| G9-B08 | FIXED | 282c95f | `Dashboard.findings.test.jsx` | |
| G9-B09 | FIXED | 9fc0006 | `resolveSelectedDB` tests | |
| G9-B10 | FIXED (UI) | 74fa75d | `DatabaseForm tags` test | Backend "absent = keep" needs `wire.go`/store (not owned). |
| G9-B11 | FIXED | 7b80ec7 | `does not offer the unused Tier 3 High Risk toggle` | Config key remains in `config_meta.json` (generated). |
| G9-B12 | FIXED | 7b80ec7 | `TrustBadge copy` tests | |
| G9-B13 | PARTIAL | 7b80ec7 | `labels the emergency stop scope` | Scope label added. Operator-visible header stop control and per-DB stop state/Resume gating DEFERRED (needs state plumbing + UX decision). |
| G9-B15 | FIXED | 4d9f18a, 45d69ee | `TestFleetPendingActions_ReportsPerDatabaseErrors`, `Actions.pendingErrors.test.jsx` | |
| G9-B16 | FIXED | 8849889, 2a2a393 | `shows which database each case belongs to` | |
| G9-B21 | FIXED | ec3f62a | `auth expiry on mutations` tests | Global fetch interceptor. |
| G9-B25 | FIXED | 9e7064b | `ConfigDiff secret masking` | |
| G9-B27 | FIXED | 4d9f18a | `TestPendingActionsHandler_RejectsMalformedDatabase` | |
| G9-B28 | FIXED | e620528, 9e8c5ca | `RulesTab.test.jsx` | Also incident_* events and per-event default min_severity mirroring `notify.DefaultMinSeverity` (lead request). |
| G9-B30 | FIXED | 7b80ec7 | `does not show Clean for a disconnected database` | |
| G6-D01 | DONE | 4461e45 | — | Moved to `router_helpers_test.go`. |
| G6-D02/D03/D04/D06 | DONE | 4461e45 | — | Live last-admin tests kept via test-only SQL helpers. |
| explain stripToJSON copies | DONE | 2acc58a | — | |

## Cross-area edits
- `internal/schema/bootstrap.go` (53391e8): additive `sage.users.oauth_issuer`, `oauth_subject`
  and partial unique index `idx_users_oauth_identity`.
- `internal/cases/shadow.go` (70d7456): SURF-13 counting rule.
- `cmd/pg_sage_sidecar/value_metrics.go` is owned; the metric rename affects dashboards.
- No notify/executor changes. G7-B05/G7-B09 are OWNED BY RUNTIME (not touched).

## Test Results

**Commands** (from `sidecar/`, `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`):
`go build ./...` (exit 0), `go vet ./...` (exit 0),
`go test -count=1 -cover -v <pkgs>` and the same with `-tags=integration`, where
`<pkgs>` = `./internal/api ./internal/auth ./internal/mcp ./internal/explain ./internal/cases ./internal/schema ./cmd/pg_sage_sidecar`.
Web: `npm ci`, then `npm run lint` (0 problems), `npm run test -- --run`, and `npm run build`.

**Total:**
- Go unit: 1565 passed, 0 failed, 0 skipped.
- Go integration tag: 1628 passed, 0 failed, 0 skipped.
- Vitest: 26 files, 130 passed, 0 failed, 0 skipped.

**Coverage (unit / integration tag):**
| Package | Unit | Integration |
|---|---|---|
| internal/api | 72.4% | 72.9% |
| internal/auth | 81.0% | 81.0% |
| internal/mcp | 77.8% | 77.8% |
| internal/explain | 87.6% | 94.4% |
| internal/cases | 88.2% | 88.2% |
| internal/schema | 82.4% | 82.4% |
| cmd/pg_sage_sidecar | 43.9% | 43.9% |

Baseline coverage for comparison: api 40.1% (no DB), auth 47.9%, mcp 58.4%.

### Skipped Tests (must be zero or justified)
None. `grep` for SKIP/TODO/PENDING in both Go runs returned 0.

### Failures
None.

### Coverage Gaps (packages below threshold)
- `cmd/pg_sage_sidecar`: 43.9%. This is the process wiring/main package, and it was already
  43.5% at baseline. Only `value_metrics.go` in it is owned here; that file is covered by
  `TestWriteValueMetrics*`. Raising the package needs runtime-wiring tests, which belong to
  the runtime area.
- All other touched packages meet the 70% threshold.

### Bugs Found This Session (beyond the review)
1. [BUG] `explain.go`: the review's suggested fix (`conn.Exec(ctx, sql, pgx.QueryExecModeExec)`)
   would still use the simple protocol. pgx forces simple protocol when no bind args remain
   (`conn.go:516`). The fix uses `PgConn().ExecParams` instead.
2. [BUG] `explain.go`: a failed EXPLAIN EXECUTE leaked `_sage_explain` on the pooled
   connection (G1-B29, explain half). Fixed.
3. [BUG] The old explain DDL denylist allowed INSERT/UPDATE/DELETE bodies; existing tests
   asserted this. Now rejected.
4. [TEST] Four existing tests encoded reviewed bugs and were updated, with reasons in the
   commit messages:
   - `TestPhase2_FindOrCreateOAuthUser_FindsPasswordUser` (SURF-02)
   - `TestCaseActionFromActionLogIncludesOutcome` (SURF-12)
   - the value-metrics counter header (SURF-18)
   - the ValuePage `#/ledger` link (SURF-14)

### Manual Checks Remaining
- CHECK-M1: MANUAL. Playwright e2e was not run, per the brief. The `token-budget.spec.ts` and
  `fixtures.ts` shapes were updated to the real payload.
- CHECK-M2: MANUAL. Browser check of the Cases suppress/resolve flow against a live sidecar.

## Deferred / follow-ups
- SURF-03: see table. Needs `internal/value` and executor changes (`database_id` stamping).
- G9-B13: operator-level header emergency stop with per-DB state.
- OIDC: admin endpoint/UI to explicitly link an existing account to an issuer+subject.
- Port manual-execute-from-finding into Cases, then delete `Findings.jsx`.
- `internal/cases`: make `CaseAction.ExpiresAt` a pointer, keep original finding timestamps,
  and stringify the queryid in evidence detail.
- Dashboards: rename `pg_sage_toil_minutes_saved_total` to `pg_sage_toil_minutes_saved`.
  `specs/agent-native-autonomy-build-spec.md` still names the old series.

## Process notes
- Two coordinator messages were misrouted (they pointed at `pgsr-fix-notify-migration`). Per
  the lead's correction, they were ignored. No edits were made in that worktree; its status
  was clean when checked.
- The shared session scratchpad is used by several agents. One verification run briefly
  wrote `build.txt`, `vet.txt` and `unit.txt` at the scratchpad root before being moved to
  `scratchpad/apiweb/`. Another agent's files with those names may have been overwritten.
