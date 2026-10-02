# Sage SRE M5 part 1: approved actions and ChatOps approval

Branch `claude/sre-m5-actions` (based on the M3 branch, `e44bd6b`). Spec: AI-SRE-SPEC §4 R1.1,
§7.2, §7.3, §9, §12; Codex CHECK list (`reviews/2026-09-26/codex/ai-sre-spec.md`).

## What was built

An investigation that concludes that **one active backend** is the root of a lock-blocking or
connection-pressure incident now proposes exactly one action: an evidence-matched
`pg_cancel_backend` of that backend. The proposal is a durable item linked to the
investigation. It never executes by itself. A human approves it in the existing approval
queue (browser, Cases panel, Slack or Telegram button), the executor runs it through
`Executor.Apply` and `policy.Gate`, and recovery is then verified and recorded on the
investigation timeline.

Flow: concluded investigation → `Propose` (derived from cited evidence and a fresh target
sample) → `RequestExecution` (exactly one `sage.action_queue` item, anchored on an open
`sre_action` finding with no runnable SQL) → human approval (single use, `decided_by`) →
`Executor.RunApprovedAction` → `ActionService.RunApproved` (claim once, resample, compare the
whole identity, refuse with a reason or hand one cancel to `Executor.CancelBackend`) →
recovery sampling → verdict on the timeline, the queue item and `action_log` verification.

### Files

| Area | Files |
|---|---|
| Executor | `internal/executor/backend_cancel.go` (identity, evidence age, `CancelBackend` through `Apply` with re-authorization, identity recheck inside the signalling SQL, `PreviewBackendCancel`, `RecordRecoveryVerdict`), `repair_contract.go` (repair contract: preconditions, scope, lock impact, timeouts, reversibility, rollback trigger, inverse, post-conditions, blast-radius budget, never-do list, residual risk), `approved_runner.go` (`RunApprovedAction`, `SetApprovedActionRunner`), `executor.go` and `action_contract.go` (small edits) |
| Policy | `internal/policy/gate_operator.go`, `types.go`, `refusal.go`: `mitigation_only` rollback class; an operator-approved mitigation-only `cancel_backend` is not bound by maintenance windows |
| Probes | `internal/sre/probes/catalog_actions.go`, `decode_actions.go`: `signal_target` and `recovery_sample` in a separate `ActionRegistry()` (never in the investigation catalog) |
| Action service | `internal/sre/action/` (new package): `derive.go`, `target.go`, `proposal.go`, `queue.go` (`PGApprovalQueue`), `request.go`, `run.go`, `recovery.go`, `tick.go`, `store.go`, `service.go`, `outcomes.go`, `types.go`; `internal/sre/action_hooks.go` (store lock and helpers it needs) |
| ChatOps | `internal/chatops/` (new): `verify.go` (Slack v0 HMAC with tolerance, Telegram secret token), `parse.go`, `store.go` (identity mapping, replay nonces) |
| Notify | `internal/notify/approval_chatops.go` (approval event, Slack buttons, Slack reply), `telegram.go` (Telegram sender and reply), `slack.go` and `secrets.go` (small edits); `internal/store/notification_chatops_validation.go` |
| Schema | `internal/schema/sre_m5_actions_migration.go` (`sage.sre_action_proposals`, `sage.chatops_identities`, `sage.chatops_replay`, action event types), one registration line in `bootstrap.go` |
| Config | `internal/config/sre_actions.go`, `sre.go`; regenerated `web/src/generated/config_meta.json`, `docs/generated/config-lifecycles.md` |
| API | `internal/api/sre_action_handlers.go` (proposal routes), `approve_core.go` (one approve-and-run core for the browser and ChatOps), `chatops_handlers.go`, `chatops_decide.go`, `chatops_identity_handlers.go`; small edits to `action_handlers.go`, `router.go`, `sre_handlers.go`, `notification_handlers.go` |
| MCP | `internal/mcp/sre_action_tools.go` (`sre_propose_action`, `sre_request_execution`); small edits to `server.go`, `principal.go`, `production_backend.go` |
| Wiring | `cmd/pg_sage_sidecar/database_runtime_sre_actions.go`, `mcp_sre_actions.go`; small edits to `database_runtime.go`, `mcp_runtime.go`, `main.go`; `internal/fleet/types.go` (`DatabaseInstance.Actions`) |
| Retention | `internal/retention/cleanup.go`: exemptions for the three new tables |
| Web | `web/src/pages/cases/ActionProposal.jsx`, `InvestigationPanel.jsx`; rebuilt `internal/api/dist` |
| Docs | `docs/configuration.md` ("Sage SRE approved actions"), `CHANGELOG.md` |

### Interface for M7 (earned autonomy)

`action.ActionClasses()` lists the action classes with their reversibility and
maximum level (`cancel_backend`, mitigation only, `L2`). `Outcomes(ctx, since, limit)` returns
executed, refused, failed and uncertain runs with recovery state and attribution. M7 can read
both without touching the proposal tables.

## Product decisions (and why)

1. **Cancel only, never terminate.** R1.1 and the Codex contract allow no automatic
   termination, and termination is irreversible. Cancel is `mitigation_only`: it ends one
   statement, and the application may retry. The never-do list in the repair contract says so.
2. **Idle-in-transaction roots are never proposed** (CHECK-02). A cancel does not end an idle
   transaction, and proposing it would misrepresent the fix. The proposal is recorded as
   "not proposed" with that reason. Prepared transactions and short hot-row contention are
   refused the same way.
3. **Always L2 in M5.** Every execution needs a human approval of that exact queue item.
   `sre.actions.request_approval` only queues the item and notifies chat; it never runs
   anything. Automatic execution is M7.
4. **Trust level is respected.** Under the default `observation` trust, a proposal is still
   created, with the policy verdict shown. Requesting approval returns `policy_blocked`. If the
   policy withdraws the action after the request, the approval is refused and the proposal
   ends `refused/policy_withheld` (found by mutation testing, see Bugs).
5. **Maintenance windows do not delay an approved mitigation-only cancel.** It is incident
   mitigation, not maintenance. Every other gate still applies: emergency stop, trust,
   execution mode, change classes, replica role and the lock ceiling. The exemption is limited
   to `cancel_backend` + `mitigation_only` + operator-approved, and a test checks each limit.
6. **Identity is wider than the spec's minimum.** The spec asks for pid, backend_start,
   database, user and query hash. The identity also includes `query_start` and `query_id`, so
   the session's next statement is never cancelled (a live test checks this). Evidence must be
   ≤ 5 s old at execution (configurable down only). The identity is checked once in Go just
   before the signal, and again in the signalling SQL statement itself.
7. **Protection by backend type, not role attribute.** Replication sessions are excluded by
   `backend_type`. Superusers are *not* protected by default, because the blocking session is
   often a superuser (migrations). pg_sage's own sessions and dump/backup tools are always
   protected. Operators can add `protected_roles` and `protected_applications`.
8. **Approval TTL is 15 min** (1-60). A stale approval cannot run, because an expired item
   ends the proposal.
9. **The `action_log` outcome `success` means the signal was delivered.** Verification closes
   with the recovery verdict (recovered → verified/success, not recovered → failed,
   otherwise inconclusive/unverifiable). Value credit is given only on recovery. Traffic that
   disappeared is never certified (CHECK-22).
10. **Refusals are attributed.** If the target finished or changed before the signal, nothing
    is signalled. The investigation records why and starts an external-attribution recovery
    watch (CHECK-23). A run abandoned mid-flight (claimed, never recorded) becomes `uncertain`
    with attribution `unknown` and is never retried.
11. **ChatOps identity is explicit.** A chat user can decide only through an admin-created
    mapping `(provider, team_id, external_user_id) → pg_sage user`. The pg_sage user's
    *current* role decides (a demoted user is refused). Callbacks carry only the decision and
    the proposal id. The decider always comes from the provider envelope.
12. **Callbacks sit outside session auth but are signed.** They are mounted on the root mux
    (`POST /api/v1/chatops/{slack|telegram}/{channel}`), authenticated by the provider
    signature, scoped to the channel's workspace (`team_id`) or numeric chat, and processed
    once (replay nonces kept 24 h). A test checks that they are the only routes without a
    session.
13. **Browser and chat share one approve-and-run core.** This rules out a ChatOps-only bypass
    of readiness or policy.
14. **`proposals: false` disables only automatic proposals.** An operator can still propose
    explicitly. Proposing never queues or executes anything.

## Spec CHECKs covered

| CHECK | Covered by |
|---|---|
| CHECK-02 | `TestDeriveCancelRefusesIneligibleRoots`, `TestActionProposeRecordsIneligibleRoots` (idle-in-transaction is "not proposed" with the reason; the web shows it) |
| CHECK-16 | `TestActionHandoffBlockedWhileMetadataIsDegraded` (no request or run while metadata durability is degraded) |
| CHECK-17 | `TestCancelBackendEmergencyStopStopsTheSignal` (an e-stop between authorization and signal stops it); `TestApprovedMitigationKeepsEveryOtherGate` |
| CHECK-18 | Stale approval: `TestActionTickSyncsDenialAndExpiry`. Changed policy: `TestCancelBackendWithheldByPolicyNeverSignals`, `TestSREActionAPI_ChangedPolicyBlocksTheApproval`, `TestActionTickRefusesABlockedApprovalItem`. Changed target: `TestCancelBackendRefusesChangedIdentity`, `TestActionRunApprovedRefusesAChangedOrMissingTarget`. Replica role: `TestCancelBackendGateRequestShape`, `TestApprovedMitigationKeepsEveryOtherGate` |
| CHECK-19 | `TestSignalTargetIsEmptyForAReusedPID`, `TestCancelBackendSQLRechecksTheWholeIdentity`, `TestLiveApprovedCancelRefusesTheSessionsNextQuery`; the residual pid race is documented in the contract's `residual_risk` and shown in the UI |
| CHECK-20 | `TestActionRunApprovedRunsOnce`, `TestPGApprovalQueueConcurrentEnqueueCreatesOneItem`, `TestMarkSeenConcurrentDeliveriesProcessOnce`, replayed Slack/Telegram deliveries in `TestChatOps*` |
| CHECK-22 | `TestRecoveryIgnoresStaleAndUnusableSamples`, `TestRecoveryNeverCertifiesWhenTrafficDisappeared`, `TestRecoveryWaitsForEnoughSamples`, `TestActionTickVerifiesRecovery` |
| CHECK-23 | Refused runs start an external-attribution recovery watch (`TestActionRunApprovedRefusesAChangedOrMissingTarget`); `TestActionTickMarksAbandonedExecutionUncertain` (attribution unknown, never retried) |
| CHECK-39 | `TestSREActionTools_*` (role per tool, typed arguments, never execute), `TestComposedSRE_M5_MCPRequestExecutionQueuesOnceAndNeverExecutes` (real mounted router, real sessions, production MCP backend: exactly one approval item, the blocker keeps running, nothing in `action_log`) |

End to end on real PostgreSQL: `TestLiveApprovedCancelRecoversTheChain` runs a real blocking
chain, approves, cancels the exact backend and verifies recovery.

## Test Results

**Command:** `go test -cover -count=1 -v -p 4 ./...` (Docker `golang:1.25`, repo root mounted, PG17
`pgsage-ag1`, `GEMINI_API_KEY` unset), at `4a3d284` (code-complete head).
**Total:** 9043 passed, 2 failed, 12 skipped (top-level tests and subtests); 69 packages ok,
1 failed (`internal/sre`, see Failures).

**Matrix (touched packages: chatops, notify, store, api, mcp, executor, policy, config,
schema, sre, sre/action, sre/probes, fleet, retention, cmd/pg_sage_sidecar):** PG14 and PG18,
all ok except a lease-timing test in `internal/sre` on each (PG14
`TestStore_PendingFindsQueuedAndOrphanedWork`, then `TestModelTurn_SlowCallKeepsTheLease` on
rerun; PG18 `TestStore_PendingFindsQueuedAndOrphanedWork`, rerun ok). `internal/sre/action`
was ok on every run.
**Race detector (PG17):** `go test -race -count=1` on chatops, notify, mcp, executor, policy,
sre/action, sre/probes, api, retention, store, fleet, config, schema and
`-run 'SREAction|M5|Parity' ./cmd/pg_sage_sidecar`: all ok, no data races. `internal/sre`
under `-race` hit the same lease-timing flake (`TestReserveModel_CrashAfterReservationKeepsTheHold`).
**Lint:** `golangci-lint run ./...`: 0 issues. **Web:** `npm test`: 205 passed (40 files);
`npm run build`: ok, dist committed.
**Mutation testing:** 14 mutants, all killed (see Post-test audit).

**Coverage (touched packages, final PG17 run; `internal/sre` from its passing rerun):**

| Package | Coverage |
|---|---|
| internal/sre/action (new) | 82.4% |
| internal/chatops (new) | 93.3% |
| internal/notify | 90.4% |
| internal/policy | 89.1% |
| internal/sre/probes | 90.0% |
| internal/sre | 86.6% |
| internal/config | 87.7% |
| internal/executor | 82.4% |
| internal/fleet | 82.9% |
| internal/schema | 81.6% |
| internal/mcp | 80.9% |
| internal/api | 75.5% |
| internal/store | 74.5% |
| internal/retention | 97.6% |
| cmd/pg_sage_sidecar | 71.9% |

All touched packages meet their coverage thresholds (business logic ≥ 70%).

### Skipped Tests (must be zero or justified)
All 12 skips are env-gated live tests in untouched packages: AWS RDS, Cloud SQL and Lakebase
provisioning and the AgentDB live gauntlet (`PG_SAGE_LIVE_*` unset), the live Azure parameter
tests (`PG_SAGE_LIVE_AZURE`), the real-provider LLM tests (no API key), a Windows-only path
test, and the RCA child-process fixture (runs only as a child process). No new test skips.

### Failures
- internal/sre: `TestStore_StaleWorkerCannotCommit`, `TestModelProbe_ResumedRunWithTurnsUsedFallsBack`
  — FAIL in the full run, PASS 3/3 on rerun; the package then passed (`ok`, 86.6%). Both are
  lease-expiry tests with 0.3-1 s leases, in code this branch does not touch. Several other
  agents were running test suites on the same Docker host during the run. The same
  `TestStore_StaleWorkerCannotCommit` failed 2/3 times on the **base commit `e44bd6b`** under
  the same load, so it is pre-existing and load-dependent, not caused by M5.
- An earlier full run with default parallelism (`-p` = CPU count) on the loaded host also hit
  fixture `DROP DATABASE` timeouts in many packages. All of them passed on a sequential rerun.
  That run also found the one real regression (retention coverage, fixed in `71fc348`).

### Coverage Gaps (packages below threshold)
None. All packages meet coverage thresholds (lowest touched: cmd/pg_sage_sidecar 71.9%,
store 74.5%, api 75.5%).

### Manual Checks Remaining
- CHECK-M1: MANUAL — Slack interactivity against a real Slack app (request URL, signing
  secret, buttons, response_url reply). Covered by signed-request tests only.
- CHECK-M2: MANUAL — Telegram `setWebhook` with `secret_token` against a real bot.
- CHECK-M3: MANUAL — Cases panel visual check in a browser (component tests only).


## Bugs found this session

1. [BUG] `executor` categorized `SELECT pg_cancel_backend(...)` as `ddl` in `action_log`; it is
   now `cancel_backend` (found by `TestCancelBackendSignalsExactIdentityAndRecordsIt`).
2. [BUG] The first identity SQL excluded every role with `rolreplication`, which includes
   the superuser, so the most common blocker could never be cancelled. Replication sessions
   are now excluded by `backend_type` (test corrected with an explanation in `c22438c`).
3. [BUG] Import cycle: the action service needs the executor, whose import chain reaches
   package `sre`. The service moved to `internal/sre/action`, and the tests moved with it
   (`11098b3`).
4. [BUG] `retention` coverage guard: the three new timestamped tables had no purge rule or
   exemption (`71fc348`).
5. [BUG] A proposal whose approval item the approval flow *blocked* (policy withdrawn between
   request and approval) stayed `requested` forever. It now ends `refused/policy_withheld`
   with the reason (`4a3d284`). Found by mutation testing: skipping the readiness check left
   every API test green, because the test canceller stands in for the executor's own gate.
6. [TEST BUG] `notify` test capture reused one map across requests (`json.Unmarshal` merges),
   so a key leaked from the previous request (`4165fd1`).
7. [TEST BUG] The config boundary test used `recovery_deadline_minutes: 1`, which contradicts
   the cross-field rule (3 × 40 s needs 2 minutes) (`724151b`).

## Post-test audit

- **Inputs not tested:** Slack payloads with several actions or an unknown type are malformed
  (tested). Slack's legacy `token` field is ignored by design (signature only). Telegram
  `@channel` chat ids cannot be matched against callbacks, so such a channel refuses
  decisions (by design, documented). There is no test for that refusal path.
- **Assertions that could pass while broken:** this was checked by mutation testing (below).
  The one surviving mutant (readiness check skipped) led to a new test and bug fix 5.
- **Fakes that hide failures:** API tests use a recording canceller in place of
  `Executor.CancelBackend`. The real executor path is covered in `internal/executor`
  (DB tests against real backends) and end to end in `TestLiveApprovedCancelRecoversTheChain`
  and the composed CHECK-39 test. Telegram and Slack HTTP calls use `httptest` servers. The
  real APIs are not called (no credentials in CI).
- **Mutation testing** (14 mutants, each applied in a separate worktree, the targeted tests
  run): identity SQL without `backend_start`; doubled Slack tolerance; window exemption
  without the rollback-class check; idle-in-transaction treated as eligible; workspace and
  chat scope check removed; replay check removed; "traffic disappeared" certified as
  recovered; refusal skipped on a changed target; MCP action tools removed from the
  operator-only set; approval readiness skipped; approved runner bypassed; durability handoff
  check removed; Slack HMAC comparison skipped; ChatOps role check removed. **All 14 killed**
  (the readiness mutant after the new test).

## What is left / for the coordinator

- **Event-type CHECK union.** The migration rebuilds `sre_events_event_type_m3` as the union
  of every event-type check present plus the 9 action types. Other milestone branches that
  add event types the same way must be merged in an order where each one runs the union. All
  of them are idempotent, but the coordinator should re-run bootstrap tests after merging.
- **Telegram retries non-2xx webhook responses.** Refused deliveries (403/409) are redelivered
  and refused again (replay or the same reason). There is no double effect, only noise.
  Answering 200 with an error body for Telegram would stop the retries; this was left as is
  because the tests specify HTTP status codes per refusal.
- **Slack's 3 s acknowledgement window.** The approve callback runs the recheck and the cancel
  synchronously (typically well under 1 s). On a slow database, Slack may show a timeout to
  the user even though the decision was applied. The follow-up reply on `response_url` still
  reports the outcome.
- **Replies use the strict target policy.** Chat replies only go to `hooks.slack.com` and
  `api.telegram.org`. On-prem relays (`AllowPrivate`) are not used for replies.
- **The web channel editor does not offer the Telegram type yet.** Telegram channels and
  identity mappings are created through the API (documented). No UI exists for identity
  mapping either.
- **No rate limit on the callback routes.** They are signature-checked before any database
  write except the channel lookup.
- Not in scope: SLO/burn-rate (`claude/sre-m5-slo`), autonomy levels (M7).
