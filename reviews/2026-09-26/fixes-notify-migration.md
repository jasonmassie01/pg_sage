# Fixes — notifications/alerting and schema-migration safety (group 07)

Branch `fix/2026-09-26-notify-migration`, worktree `C:/Users/jmass/pgsr-fix-notify-migration`.
Process: failing regression tests were committed first for every batch
(552e478, 43efd97, f899791, 8f2745d). The fixes followed in separate commits.
Paths below are relative to `sidecar/`.

## Status

| ID | Status | Commit | Test(s) | Notes |
|---|---|---|---|---|
| G7-B01 (P0) | FIXED | fbfe6d6 | `TestLLMFallback_NeverSendsCredentialDDL`, `TestLLMFallback_RedactsLiteralsInTableDDL`, `TestLLMFallback_SkipsMultiStatementBatches`, `TestFindingFromIncident_RedactsLiterals`, `TestScriptGenerator_RedactsLiterals` | `Advisor.Analyze` now sanitizes every statement before doing anything else: it strips comments and replaces all literals (`'…'`, `E'…'`, `$tag$…$tag$`) with `'***'`. The LLM fallback runs only for a single table/index/maintenance statement with no credential markers (`PASSWORD`, `USER MAPPING`, `SUBSCRIPTION`, `CONNECTION`, `CONNINFO`, `SERVER`, `SECRET`). Anything else is not sent (fails closed). The script-generator prompt and catalog schema are redacted too, and persisted findings store only sanitized SQL. |
| G7-B02 | FIXED | 2b94609 | `TestIntegration_Detector_SeesBlockedDDL`, `TestIntegration_RiskAssessor_CountsActiveQueries`, `TestIntegration_RiskAssessor_EscapesTableRegex` | `\b` → `\y` in both SQL regexes. Table names are escaped for ARE, and leading comments are allowed. The integration tests hold a real `ACCESS EXCLUSIVE` lock on the fixture DB. |
| G7-B03 | FIXED | 821248b | `TestIntegration_Advisor_ReportsEveryRuleOnLargeTable`, `TestIntegration_Advisor_SmallTableBelowThreshold`, `TestComputeRiskScore_IntrinsicFloor`, `TestMaintenanceRules_ExtractTarget` | Score = `base + (1-base)·escalation`. The rule's intrinsic hazard is the floor. `ddl_row_threshold` (default 10000, previously unused) caps known-small idle tables at 0.3. The boundary is kept: idle `CREATE INDEX` scores exactly 0.3 and produces no incident. VACUUM FULL, CLUSTER, REFRESH and SET TABLESPACE are marked as rewrites, and maintenance rules now extract their target table. |
| G7-B04 | FIXED | fbfe6d6 | `TestIsVolatileDefault_VolatileFunctions`, `TestIntegration_VolatileAllowlistMatchesCatalog` | Removed `gen_random_uuid`, `uuid_generate_v4` and `clock_timestamp` from the allowlist. Two tests asserted the bug and were inverted; the reason is in the commit message. A test now cross-checks the allowlist against `pg_proc`. |
| G7-B05 | FIXED (library) | 301e958 | `TestDispatcher_ReadsRulesFromInjectedStore` | New `notify.RuleStore`, `NewDispatcherWithStore` and `NewPoolStore`. main.go wiring still needed (see cross-area). |
| G7-B06 | FIXED | 301e958, 46e9042 | `TestRuleCanFire_FixedEventSeverities`, `TestCreateRule_RejectsUnreachableSeverity`, `TestDefaultRuleSeverity_MatchesEvent` | Unreachable rules are rejected with 400. The API default is now the event's own severity, and the UI default is `info`. Four store fixtures created unreachable rules and were fixed. |
| G7-B08 | FIXED | 301e958 | `TestEmailSender_STARTTLSSubmission`, `TestEmailSender_RefusesServerWithoutSTARTTLS`, `TestEmailSender_ImplicitTLS` | STARTTLS on submission ports, failing closed if the server doesn't offer it. Implicit TLS on port 465 or with `smtp_tls=implicit`. Tests use a fake SMTP server. |
| G7-B09 | FIXED (library) | a444ca0 | `TestPayloads_CarryDatabaseName` | `ManagerConfig.DatabaseName` is stamped on Slack, PagerDuty and webhook payloads. Fleet construction in main.go is cross-area. |
| G7-B10 | FIXED | 301e958, 46e9042, a444ca0 | `TestSlackSend_ErrorDoesNotLeakWebhookPath`, `TestDispatcher_LogsRedactedErrors`, `TestRedactURLs_InFreeText`, `TestTestChannel_LogsTestEventAndRedacts`, `TestSenders_ErrorsDoNotLeakURLs` | `*url.Error` is rebuilt with scheme and host only. `notification_log` errors are redacted on write and again on read (covers old rows). |
| G7-B11, B12 | DEFERRED | — | — | Owned by the collection agent. |
| G7-B13 | FIXED | 2b94609 | `TestIntegration_ResolveStaleMigrationFindings` | `ResolveStaleFindings` resolves findings not re-detected for 24h. It runs inside the detector loops at most every 10 minutes. |
| G7-B14 | FIXED (notify library + alerting) | 301e958, a444ca0 | `TestPagerDuty_DedupKeyPerFindingAndResolve`, `TestPagerDuty_ResolveUsesSameDedupKey`, `TestEvaluate_SendsResolveForAlertedFinding` | notify: the dedup key includes a subject hash, and `ResolvedEvent` sends `resolve`. The analyzer emitting `ResolvedEvent` is cross-area. alerting sends one resolve for each previously alerted finding. |
| G7-B15 | FIXED | 301e958, a444ca0 | `TestSlackPayload_TruncatesAndEscapes`, `TestSlackPayload_LimitsAndEscaping` | Rune-safe truncation to 150 chars (header) and 3000 chars (section). |
| G7-B16 | FIXED | 301e958 | `TestEmailSender_StalledServerTimesOut` | Dial timeout plus a conversation deadline taken from ctx (30s default). |
| G7-B17 | FIXED | 2b94609 | `TestIntegration_Detector_IgnoresOtherDatabases` | Filters on `datname = current_database()` and on `pg_locks.database`. The test creates a sibling database on the fixture server. |
| G7-B18 / SURF-15 | FIXED | a444ca0 | `TestEvaluate_WatermarkCapturedBeforeQuery`, `TestEvaluate_RetriesFailedDelivery` | The watermark is the DB clock captured before the query. Findings where every channel failed are re-read from `alert_log` (bounded to 24h). Failed rows are written before sent rows, so a partial delivery is not re-sent. |
| G7-B19 | FIXED | a444ca0 | `TestThrottle_CriticalBypassesQuietHours`, `TestEvaluate_QuietHoursDeferNotDrop` | Critical alerts bypass quiet hours. Other alerts are recorded once as `deferred` and delivered after quiet hours end. |
| G7-B20 | FIXED (library) | 301e958, 46e9042 | `TestSealOpenSecrets`, `TestChannelSecrets_EncryptedAtRestAndLazilyMigrated` | AES-GCM via `internal/crypto`. Existing plaintext rows are migrated lazily when read. Not active until the key is wired (see cross-area). |
| G7-B21 | FIXED | 301e958, 46e9042 | `TestTargetPolicy_ValidateURL`, `TestSlackSender_DefaultPolicyBlocksLoopbackAtDial`, `TestCreateChannel_RejectsInternalTargets`, `TestTestChannel_LogsTestEventAndRedacts` | Static check plus a dial-time check (also blocks DNS rebinding and redirects). Link-local and metadata addresses are always denied. Loopback, RFC 1918 and CGNAT need `AllowPrivate`. The test endpoint now returns a generic error. |
| G7-B22 / R08 | FIXED (gate + persistence); workload capture DEFERRED | 6cc213b | `TestOrchestratorEmptyWorkloadIsInconclusive`, `TestOrchestratorRecordsCloneCleanupFailure`, `TestApplyRecordsCleanupFailureAndMeasurement`, `TestPostgresRecorderPersistsMeasurement` | Empty `AffectedQueries` now means `recommend_only` / `inconclusive_no_workload`. A clone-cleanup failure blocks promotion and is recorded. The measurement is persisted. **Consequence: `apply_migration` never promotes** until the runner captures paired before/after workload. |
| G7-B23 | FIXED (reporting); continuation worker DEFERRED | 6cc213b | `TestApplyReportsPendingContract` | Reason is `contract_pending`, and `Result.PendingContractSQL` carries the contract steps. MCP should surface both (cross-area). |
| G7-B24 | FIXED | 245818f | `TestPlannerRejectsMultiClauseAlter`, `TestPlannerRejectsTableMismatch`, `TestPlannerAllowsCommaInsideUniqueColumns` | |
| G7-B25 | FIXED | 6cc213b, 63d0df9 | `TestOrchestratorClassifiesErrors`, `TestRehearsalStepFailureIsNotCloneUnavailable` | Step failures now report `rehearsal_failed:<SQLSTATE>`. |
| G7-B26 | FIXED (cheap scope) | fbfe6d6 | `TestClassifier_FalseNegatives`, `TestClassifier_KeyUsingIndexIsSafe`, `TestClassifier_LockTimeoutInBatchSuppressesAnnotation` | Covers optional `COLUMN`, unnamed CHECK/FK, quoted and case-folded identifiers, the new rule `ddl_add_key_builds_index`, serial/IDENTITY/STORED columns, leading comments and batches. `pg_query_go` is DEFERRED. |
| G7-B27 | FIXED | fbfe6d6, 2b94609 | `TestLLMFallback_SkipsProvablySafeDDL`, `TestLLMFallback_ClampsRiskAndRendersVersion`, `TestIntegration_Detector_IgnoresOwnBackends` | Covers the safe-form skip, the risk clamp, the `16.4` version string and excluding pg_sage's own backends. |
| G7-B29 | FIXED | 46e9042 | `TestUpdateRuleHandler_MissingEnabledIsRejected` | |
| G7-B30 | FIXED | 301e958 | `TestFormatEmailMessage_NoHeaderInjection` | Also adds Date and Message-ID headers and Q-encodes the subject. |
| G7-B31 | FIXED | 301e958, a444ca0 | Slack payload tests (see B15) | |
| G7-B32 | FIXED | a444ca0 | `TestSlack_NoRetryOnPermanent4xx`, `TestNew_WarnsOnInvalidTimezone`, `TestThrottle_QuietHoursHonourMinutes`, `TestThrottle_EvictsExpiredEntries` | The throttle is also seeded from `alert_log` on restart. |
| G7-B33 | FIXED | 821248b | `TestIntegration_Advisor_LockTimeoutIsAnnotation` | Now an annotation on the top ACCESS EXCLUSIVE incident. |
| G7-B34 | FIXED | fbfe6d6 | `TestIndexSafeAlternative_IsFormatted` | Falls back to prose when the statement had a redacted literal. |
| G7-B35 | FIXED (varchar/text scope) | fbfe6d6, 821248b | `TestRuleText_VolatileDefault`, `TestIntegration_AlterType_BinaryCoercible` | numeric typmod widening is not recognized. |
| G7-B37 | FIXED | 46e9042 | `TestTestChannel_LogsTestEventAndRedacts` | Test sends are logged as event `test`. The dead `sender` variable and the `sendTestDirect` wrapper (D15) are removed. |
| G7-D04, D07 | DELETED | 79fd020 | — | `stripToJSONObject` and the `looksLikeSQL` no-op, with their tests. |
| G7-D05, D06 | DEFERRED | — | — | Removing them means rewriting about 10 existing tests. Low value. |

Not assigned, so not touched: G7-B07 (analyzer critical storm), B28 (LLM `safe_alternative` can still reach
`recommended_sql`), B36, B38.

## Cross-area edits

None. No files outside the owned set were changed. **These wiring changes are needed** for the
library-side fixes to take effect at runtime:

1. **G7-B05** — `cmd/pg_sage_sidecar/main.go` (about line 1554, `initFleetMultiDB`): build each fleet dispatcher with
   `notify.NewDispatcherWithStore(notify.NewPoolStore(controlPool, encKey), logStructuredWrapper)`.
   `controlPool` is `metaState.Pool` in meta mode, or the API auth pool in YAML fleet mode. Standalone mode is unchanged.
2. **G7-B20** — two changes that must land together:
   - `api/router.go registerNotificationRoutes`: `store.NewNotificationStore(pool, d).WithSecretKey(key)`, plus `NewPoolStore(pool, key)` for its test dispatcher.
   - Every runtime dispatcher: `NewPoolStore(..., key)` with the same key.

   If only one side is wired, sealed channels fail with an explicit "encrypted but no key" error. They do not fail silently. In standalone mode there is no key yet.
3. **G7-B21** — add a config key (for example `notifications.allow_private_targets`). It should feed
   `store.WithTargetPolicy`, `notify.NewSlackSenderWithPolicy` / `NewEmailSenderWithPolicy` in
   `registerNotifySenders`, and the router's `newDefaultDispatcher`.
   **Behavior change:** without it, existing channels that point at internal SMTP relays or RFC 1918 webhooks are refused.
4. **G7-B09** — `initFleetMultiDB`: start one `alerting.Manager` per database with `DatabaseName`, or at minimum `logWarn` when `alerting.enabled` in fleet mode.
5. **G7-B14 (notify)** — the analyzer should dispatch `notify.ResolvedEvent(...)` when a critical finding resolves.
6. **G7-B23/B25** — `internal/mcp` `MigrationOutcome` should expose `Result.Reason` and `PendingContractSQL`.
7. **G7-B06** — optional: `schema/notifications.go` still has DB default `'warning'`. The API always sends a value, so it is harmless. Existing unreachable rules in deployed DBs are not migrated.

Also noted: the coordinator sent a mid-task message asking me to "resume in `C:/Users/jmass/pgsr-fix-api-web`" and to take
SURF-03 (`api/router.go`). That is a different worktree and area (G1), and my brief limits me to this worktree. I treated it
as misrouted and did not act on it. The lead should re-send it to the api-web agent.

## Consolidation plan (notify as the single stack; not done in this pass)

1. Port the throttle into `notify`: `Decide`, minute-precision quiet hours, critical bypass, `alert_log` seeding.
   Also port the retry classification (`alerting/httpsend.go`), the generic webhook sender, and the resolve lifecycle.
2. Add a durable outbox to `notification_log` (`pending`/`error`/`sent`, attempts, next_attempt_at) with a bounded worker,
   so `Dispatch` never blocks the analyzer or executor (G7-I02). This generalizes the SURF-15 contract implemented here.
3. Emit finding lifecycle events from the analyzer: first occurrence or escalation sends `finding_critical`, resolution sends
   `ResolvedEvent`. Apply a per-(event, database, object) cooldown. This replaces the `alerting.Manager` poller and fixes G7-B07.
4. Use one control-plane dispatcher for the fleet, and give `notification_rules` a nullable `database` column.
5. Seed DB channels and rules from the YAML `alerting:` block on first boot and log a deprecation. After one release,
   delete `internal/alerting` (D11) and point the `/alerts` UI at the notification log.

## Test Results

**Command:** `go test -count=1 -cover -v ./internal/migration/... ./internal/notify/ ./internal/alerting/ ./internal/store/ ./internal/api/ ./cmd/pg_sage_sidecar/`, run once plain and once with `-tags=integration`
(`SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable`).
Also ran `go build ./...` and `go vet ./...` (clean).
Web: `npm ci && npm run lint && npm run test -- --run && npm run build` (lint 0, 19 files / 89 tests passed, build ok; `internal/api/dist` rebuilt in f82ed35).

**Total:**
- Unit: 1913 passed, 0 failed, 0 skipped.
- Integration tag: 1989 passed, 0 failed, 0 skipped.

**Coverage:**

| Package | Coverage |
|---|---|
| migration | 78.0% |
| migration/plan | 89.7% |
| migration/rehearsal | 94.4% |
| migration/runtime | 81.4% |
| notify | 90.1% |
| alerting | 90.3% |
| store | 75.6% (76.1% integration) |
| api | 71.8% (72.3% integration) |
| cmd/pg_sage_sidecar | 43.7% |

### Skipped Tests (must be zero or justified)
- None. Grepped for `--- SKIP`: 0 in both runs.

### Failures
- None.

### Coverage Gaps (packages below threshold)
- `cmd/pg_sage_sidecar`: 43.7%. This is pre-existing (large `main.go` wiring). I only touched `mcp_migration_runtime.go`, which is covered by `TestRehearsalStepFailureIsNotCloneUnavailable`.
- All owned business-logic packages meet the 70% threshold. Before this pass, notify was 88.1%, alerting 82.9% and migration 70.1%.

### Bugs Found This Session (beyond the review)
1. [BUG] `migration/llm_scripts.go`: the LLM script generator sent raw DDL plus catalog-derived defaults and CHECK bodies (with literals) to the LLM. This path was not mentioned in G7-B01. Now redacted.
2. [BUG] `migration/risk_queries.go`: `reltuples = -1` (a never-analyzed table) was treated as a real row count. Stats are now marked unknown.
3. [BUG] `migration` classifier: unquoted identifiers were not case-folded, so `ALTER TABLE Orders` looked up relname `Orders` and table stats never loaded.
4. [BUG] `migration/rehearsal`: a clone `Destroy` error was swallowed while the result still read "passed". Now recorded as `CleanupError` (R08).
5. [TEST] My own `TestNew_WarnsOnInvalidTimezone` checked the format string instead of the formatted message. Fixed; the reason is in commit a444ca0.
6. Existing tests that encoded bugs were updated, with the reason stated in each commit message:
   - volatile-default allowlist (B04)
   - multiplicative risk formula expectations (B03)
   - PagerDuty dedup key equality (B14)
   - implicit-TLS-only email error text (B08)
   - four fixtures creating unreachable rules (B06)
   - slack httptest cases now opt into `AllowPrivate` (B21)

### Code-quality notes
- Every new or changed production function is ≤ 50 lines and every line ≤ 100 chars.
- `api/notification_handlers.go: updateChannelHandler` (63 lines) was already over the limit and was not touched.
- Owned test files that already exceeded 500 lines were left as they were.

### Manual Checks Remaining
- MANUAL: after the cross-area wiring above lands, confirm in fleet/meta mode that a UI-created rule fires for a non-primary database.
- MANUAL: confirm STARTTLS delivery against a real provider (Gmail/SES on port 587).
