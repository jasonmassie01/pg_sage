# Roadmap 1.5: approval cards with the why (2026-10-03)

Branch `claude/w1b-approval-cards`, based on `origin/master` (e9feff49, v1.8.4).

Goal (ROADMAP Phase 1.5, interfaces review 2.2 / 2.5 / I4 / I5): every action that waits for
approval is one card with the "why", approvable in one click in the UI, Slack and Telegram,
with a follow-up that carries the verification verdict.

## What was built

### Card model (`internal/approvalcard`)

One card per queue item, assembled from the queue row, its finding, the recommendation
revision it is pinned to, the policy-gate decision that queued it, an earlier operator
rejection of the same SQL, the operator's snooze and the action contract
(`executor.ContractForQueuedAction`).

| Card field | Source |
|---|---|
| title, targets | revision title, else finding title; finding `detail.table` and object identifier |
| evidence (kind, label, value, ref) | the finding (`finding:<id>`), every scalar number in its detail (max 12, internal keys such as DDL and model text excluded), affected queries with their query ids (`queryid:<n>`), plan source, revision evidence (`recommendation:<id>@<rev>`), the gate decision (`decision:<id>`) |
| rationale | `llm_rationale` / `rationale` / `narrative` / `rewrite_rationale` (source `llm`, with `confidence_score`), else the rule's recommendation (source `rule`) |
| predicted effect | HypoPG / model `estimated_improvement_pct`, `estimated_size_bytes`, affected queries and ids, what-if verdict and reason, method (`hypopg` or `llm_estimate`); typed `Forecast` slot (metric, baseline, predicted, interval, confidence) left nil for the predicted-effect work (roadmap 1.3) |
| SQL, rollback | exact `proposed_sql`, `rollback_sql`, the contract's rollback class and a note when there is no rollback SQL |
| risk | tier (decision, else queue row, else contract), blast radius (objects, build size, affected queries), the lock the action type takes, guardrails (queue row + decision + contract), contract post-checks |
| why it needs you | stable codes: `approval_required` (producer marker, e.g. standby index usage unknown), `guc_restart`, `guc_not_allowlisted`, `what_if_unverified`, `gate:<reason>` (the linked decision, e.g. `gate:trust_ramp_not_satisfied`), `operator_rejected_before` (with date and reason), `trust_level` (observation / advisory), fallback `queued_for_approval`; `finding_missing` when the finding is gone |
| expiry, snooze | queue `expires_at`; `snoozed_until` and reason while running |
| card hash | SHA-256 over queue id, finding id, action type, risk, proposed SQL, rollback SQL and the revision content hash |

Files: `card.go` (types), `assemble.go`, `evidence.go`, `why.go`, `risk.go`, `text.go`
(chat text, summary, content hash), `loader.go` (DB reads), `outcome.go` (verdict read),
`followup.go`, `notifier.go` (event enrichment, snooze re-send, token issuer).

### API (`internal/api`)

- `GET /api/v1/approvals?database=` lists the cards of every pending item (per-database
  read failures under `errors`, as the pending endpoint does).
- `GET /api/v1/approvals/{id}?database=` returns `{card, eligible, defer_reason}`.
- `POST /api/v1/approvals/{id}/approve` `{card_hash}` (required): refused with `409
  content_changed` when the item changed since the card was shown, `409 not_pending`,
  `409 not_eligible` (lifecycle or policy, with the reason); otherwise the existing
  `approveAndRun` path.
- `POST .../reject` `{reason}` (required) and `POST .../snooze` `{hours 1-168, reason}`.
- `?database=` may be omitted when exactly one database is monitored.
- All are operator/admin only. Files: `approval_card_handlers.go`, `router.go` (one line),
  `approve_core.go` (`approveAndRunExpecting`).

### Chat (`internal/chatops`, `internal/notify`, `internal/api/chatops_card_decide.go`)

- The executor's approval requests (`queueFinding` and the earned-L2 handoff) now name their
  queue item (`notify.QueuedApprovalEvent`) and link the gate decision to it
  (`sage.decision.queue_id`).
- Each executor's dispatcher is wrapped in an `approvalcard.Notifier` that turns the request
  into a card (body = card text, `CardRef` with queue id, card hash, title, summary, expiry).
- The shared control-pool dispatcher mints one card token per interactive channel
  (`approvalcard.TokenIssuer` -> `chatops.CardStore`), revoked if the send fails. Slack shows
  Approve (with confirmation) / Reject / Snooze 4h; Telegram an inline keyboard
  (`sage:ca|cr|cs:<token>`, 30 bytes).
- Callbacks reuse the SRE ChatOps route and security, then for a card token: card exists ->
  issued to this channel -> unused -> unexpired -> queue item content hash unchanged ->
  consume (atomic) -> decide through `approveAndRunExpecting` / `Reject` / `Snooze` as the
  mapped user.
- Follow-up: a 30 s loop (`startApprovalCardLoop`, started with the auth-pool services) posts
  each card's verdict once to every chat it went to (Telegram as a reply to the decision's
  message), and re-sends cards whose snooze ended.

### Executor and safety (`internal/executor`)

- An operator's running snooze of exactly this content keeps the change behind approval like
  a rejection (#91): `operatorHold` (gate evidence) and `classifyQueuedApprovals` (the change
  lease check before an authorized change runs) both refuse to take it over.
- Rejections from any channel are ordinary `rejected` queue rows with the reason, so the
  existing #91 logic keeps them behind approval.

### UI (`sidecar/web`)

- `pages/actions/ApprovalCard.jsx`: the card (why, evidence, rationale, predicted effect, SQL,
  rollback, risk, expiry, snooze badge) with Approve (sends the card hash) / Reject (reason
  required) / Snooze (1, 4, 24, 72 h + reason); truthful result toasts.
- `pages/actions/ApprovalCards.jsx`: the card view of the Pending Approval tab and the card
  in a table row's detail.
- `pages/Actions.jsx`: the pending tab leads with cards ("Table view" / "Card view" toggle);
  without the cards API it shows the table as before.

### Schema

`internal/schema/approval_cards_migration.go` (registered once in `bootstrap.go`):
`sage.approval_card_deliveries` and the `snoozed_until`, `snoozed_by`, `snooze_reason`
columns of `sage.action_queue`, with partial indexes for the follow-up queue and the snooze
scan. Idempotent; schema version not bumped.

## Screenshots (described)

Rendered with the real `PendingCardsView` / `ApprovalCard` components (Vite dev server, a
throw-away harness with a lifeos-like index card, dark theme; the harness is not committed):

1. **Pending Approval tab, card view.** A one-line explanation ("Each card says why the
   action needs you, the evidence behind it, the exact SQL and how to undo it ...") with a
   small "Table view" button on the right; below it one card per waiting action.
2. **The card.** Header "Index recommendation for public.events" in bold; on the right, muted,
   "lifeos · queue item 41 · Expires in 22h". Under it a yellow-bordered box **Why it needs
   you** with three bullets: "HypoPG what-if is unverified: the improvement is not verified
   (hypopg not installed)", "Policy gate: approval required (moderate risk)", "Trust level is
   advisory: pg_sage proposes changes and a person approves them". Then **Evidence**:
   "Finding #18569: Index recommendation for public.events | warning | index_optimization",
   "seq scan: 182330", "mean exec time ms: 412.7", "Query 4242: SELECT * FROM events WHERE
   user_id = $1 ...", "Policy decision #326942: queue_approval (moderate risk): approval
   required", each followed by its muted reference (`finding:18569`, `queryid:4242`,
   `decision:326942`). **Model rationale (confidence 82%)** with the model's sentence.
   **Predicted effect**: "38.5% faster on 2 queries · about 12.0 MB · HypoPG what-if:
   unverified (estimate, not measured)". **SQL** and **Rollback (reversible)** in the
   existing highlighted SQL blocks with copy buttons. **Risk and blast radius**: "Risk:
   moderate" (yellow), "Blast radius: 1 object: public.events; about 12.0 MB to build; 2
   affected queries", "Lock: SHARE UPDATE EXCLUSIVE on the table: reads and writes continue
   while the index builds", "Guardrails: maintenance_window, approval_required". Bottom row:
   green **Approve**, red **Reject**, outlined **Snooze**.
3. **Reject pressed.** A text area ("Why not? (kept with the action; pg_sage will not run it
   unasked)") and a dimmed "Reject with reason" button that enables once a reason is typed.
   Snooze opens a 1h / 4h / 24h / 72h select, an optional reason and "Snooze 4h".
4. **Table view / action detail.** The existing pending table with a "Card view" button;
   expanding a row shows the same card above the existing SQL and script details.
5. **Slack.** The existing header and body blocks, the body now being the card text (why,
   evidence, rationale, predicted effect, SQL, rollback, blast radius, lock, guardrails),
   then an actions block: Approve (primary, with the confirmation "pg_sage rechecks policy and
   the exact SQL, then runs it as you."), Reject (danger), Snooze 4h. After verification, a
   new message "Verified: <title>" with "Result: ..." and "Predicted: ...".
6. **Telegram.** The plain-text card with an inline keyboard Approve | Reject | Snooze 4h;
   the follow-up arrives as a reply to the message whose button was pressed.

## Product decisions

- **Every queued action gets a card, also in chat.** The card is built from what pg_sage
  already knows (finding, revision, gate decision, contract); no model call at card time (no
  extra LLM cost, no invented numbers). A model's rationale is labelled "Model rationale", a
  rule's text "Rationale".
- **A chat approval is a human approval under the mapped user's name.** It needs a chat user
  linked to a pg_sage operator or admin (existing `sage.chatops_identities`) and goes through
  the same `approveAndRun` path as the UI (readiness, policy re-authorization, change lease,
  finding/SQL match). No new execution path, no autonomy granted.
- **Buttons only where they can work.** Slack needs `interactive=true` and a signing secret;
  Telegram needs a `webhook_secret`. Other channels get the card text only.
- **SRE cancel proposals keep their existing buttons.** They are already signed, mapped and
  single-use through their proposal; they are not re-carded, to avoid two button sets on one
  message. Every other action type now has buttons.
- **Snooze is "not now", not "no".** A snoozed item stays pending and approvable; autonomy may
  not take it over while the snooze runs (same rule as a rejection, #91); when it ends the
  card is sent again. Chat snooze is 4 h; the UI offers 1 / 4 / 24 / 72 h (API 1-168 h).
- **Chat Reject records a fixed reason** ("rejected in slack by <email>"): buttons cannot take
  free text without a modal. The UI requires a typed reason. Both feed the #91 rule.
- **Follow-ups go to the same chat, not a Slack thread** (incoming webhooks cannot thread);
  Telegram replies to the decision's message when the decision was made there. Follow-ups are
  posted for verified, rolled back, regressed, failed, unverifiable, and for items closed
  without running (rejected, expired, superseded, blocked), so a chat card is always closed.
- **A card token is consumed before the decision runs.** If the approval is then refused
  (outside the window, finding resolved) the reply says why and the card is spent; the
  operator decides in the UI or on the next card. A user who may not decide does not spend it.
- **Approve toasts are truthful:** "Approved and executed; verification: monitoring", or
  "Approved, but the action did not run: <error>", or the refusal reason (interfaces 2.2).

## Security notes

- **Callback authentication is unchanged and runs first:** Slack v0 HMAC over the raw body
  with a 5-minute timestamp tolerance, Telegram's per-bot secret token (constant-time
  compare), workspace / chat scoping, the per-delivery replay ledger (`sage.chatops_replay`),
  and the mapped user's current role (operator or admin).
- **Card tokens:** 128 random bits (`crypto/rand`), base64url; only the SHA-256 is stored, so
  a table dump cannot be replayed. A token is bound to one channel (presented on another
  channel's endpoint: `403 wrong_channel`), one database and queue item, and one content hash;
  it expires at the earlier of 24 h and the queue item's expiry, and is consumed by one
  conditional `UPDATE` (exactly one of N concurrent presses wins). Unknown: `404
  unknown_card`; used: `409 card_used`; expired: `410 card_expired`. Tokens are never logged.
- **Content binding:** the card hash covers queue id, finding id, action type, risk, proposed
  and rollback SQL and the revision content hash. It is checked before the token is spent
  (UI: the `card_hash` the browser saw; chat: the hash recorded when the card was sent), and
  the approval `UPDATE` itself also requires `proposed_sql` to equal what the card showed, so
  a change between check and approval is refused as well.
- **No chat payload is trusted:** buttons carry only the decision and the opaque token; who
  decided comes from the signed envelope and the identity mapping.
- **Latent race fixed on the shared approval path:** a losing concurrent approval could write
  its readiness outcome (`blocked` / `resolved_ephemeral`) over the winner's row while the
  winner was executing. The outcome is now written only for pending rows.
- **Unchanged:** chat users are linked by an admin (`/api/v1/chatops/identities`); no
  self-service linking. SQL is sent to chat verbatim, as approval events did before.

## Test Results

Tests were written first and committed (`9229970f`) before any implementation; the fixes to
the tests themselves are in `7e767bee` and `fix(retention)` with the reasons.

**Command:** `go test -p 2 -count=1 -cover -timeout 1500s ./...` (Docker `golang:1.25`,
`--cpus=2`, PG17 `pgsage-ag7` :55477)
**Total:** 87 packages ok, 3 failed in the first full run (all three explained below; each
re-run green); 81 new Go test functions + 15 new vitest cases.

| Package (touched) | Coverage |
|---|---|
| internal/approvalcard (new) | 88.7% |
| internal/chatops | 90.9% |
| internal/notify | 91.0% |
| internal/executor | 86.1% |
| internal/retention | 86.9% |
| internal/schema | 83.4% |
| cmd/pg_sage_sidecar | 81.2% |
| internal/api | 78.9% |
| internal/store | 76.1% |

All touched packages meet the 70% business-logic threshold.

Other runs:
- `-race` on approvalcard, chatops, notify, store, retention, api, executor, schema: all ok,
  no data race.
- PG14 (:55414) and PG18 (:55418), touched packages: all ok except two executor lock-ceiling
  timing tests (`TestApplyLockCeilingCapsCustodian` on PG14,
  `TestApplyLockCeilingCapsCycleAnalyze` on PG18: "lock wait took 2.05 s"), which measure
  lock-wait durations on the shared matrix servers; both pass when re-run alone on each
  server and do not touch approval code.
- e2e: `go test -tags=e2e -count=1 -timeout 900s ./e2e/`: ok, 20 passed, 13 skipped.
- Performance gate (small scale): `TestPerfGate` PASS (164 s).
- Lint: `golangci-lint run ./...` (v2, Windows): 0 issues.
- Web: `npm test` 54 files / 290 tests passed (15 new); `eslint` clean on changed files;
  `npm run build` rebuilt `internal/api/dist` (committed).

### Skipped Tests (must be zero or justified)
- e2e: 13 tests (`TestLLM*`, `TestTunerLLM_*`, `TestOptimizerMultiQueryConsolidation`)
  skipped: they need a live LLM (`PG_SAGE_LIVE_LLM=1` and an API key), off by the rules.
- None of the new tests skip (verified with `-v` against PG17).

### Failures (first full run; all fixed or re-run green)
- internal/retention: `TestRetentionRules_CoverEveryTimeSeriesTable`: the new table had no
  purge rule. Fixed (retention rule + test).
- internal/api: `TestActionsLedger_VerificationStatusFromDurableRecord`: its cleanup
  `DELETE FROM sage.decision` hit change leases left by the new card tests' real
  executions. Fixed in the card fixture's cleanup. (One isolated re-run also showed
  `TestActionsPageSQL_UsesIndexes` choosing another plan; it passed in the full run and in
  the next re-run: planner statistics of the shared test database.)
- internal/sre/probes: `TestSequenceRunway_SlicesCoverTheCatalogOnce`: a probe deadline under
  load; passes on re-run; unrelated to this change.

### Coverage Gaps
- All packages meet coverage thresholds (lowest touched: internal/store 76.1%).

### Manual Checks
- CHECK-UI: MANUAL, done: the card, the reject form and the narrow layout were inspected in
  a browser (see "Screenshots").
- CHECK-CHAT-LIVE: MANUAL, not done: a real Slack app and Telegram bot were not used (no
  live network calls by design); payloads are verified against fake servers.

## Bugs found

1. [BUG, fixed] `api/approve_core.go` `approvalBlocked`: a losing concurrent approval wrote
   its readiness outcome (`blocked` / `resolved_ephemeral`) over a row another approver had
   just approved and was executing (the guard only excluded `executed`). Outcomes are now
   written only for pending rows. Found while designing the concurrent-press tests.
2. [BUG, fixed in this branch before merge] the new `sage.approval_card_deliveries` had no
   retention rule; caught by the retention package's table guard in the full suite.
3. [TEST, fixed] three logical errors in the first version of my own tests (SQL parameter
   typing, a helper name clash, a `;` query the Go parser drops) and a fixture that left
   change leases referencing decisions other tests delete; explained in the commits.
4. Interfaces review 2.5 "`ApprovalNeededEvent` has no proposal id": the executor's approval
   events now carry the queue item, and the gate decision is linked to it
   (`sage.decision.queue_id` existed but was never written).

## Post-test audit

**Mutation testing** (each mutation applied on purpose, targeted tests run, then reverted):

| Mutation | Caught by |
|---|---|
| card token consumable twice (`used_at IS NULL` dropped from `Consume`) | `TestCardConsumeOnceThenReplay`, `TestCardConsumeConcurrentExactlyOnce` |
| chat callback skips the content-hash check | `TestChatCardContentChangedAfterSend` |
| chat callback skips the channel binding | `TestChatCardTokenBoundToItsChannel` |
| rollback SQL left out of the card hash | `TestContentHashIsStableAndSensitive`, `TestApprovalCardApproveRefusesChangedContent` |
| `classifyQueuedApprovals` ignores snoozes | `TestClassifySnoozedPendingBlocksAutonomy` (the DB-level `TestStaleApprovalSnoozedStaysBehindApproval` still passes because `operatorHold` holds the change first: two independent guards) |
| follow-up posted but never marked | `TestFollowupsPostTheVerdictOnce`, `TestFollowupsRetryOnSendFailureAndCloseOrphans` |
| Telegram channel without webhook secret treated as interactive | `TestCardInteractive` |

**Inputs not tested / known gaps.**
- Slack's real `response_url` and Telegram's real Bot API are faked (no live network calls,
  by design); the fake servers record the exact payloads, and the payload shapes follow the
  existing, already-tested SRE buttons.
- The follow-up worker's 500-card batch boundary (starvation with more than 500 open cards)
  is not tested; documented under "What is left".
- `RenotifySnoozed` with a dispatcher error part-way through a batch is not tested (the
  snooze is already cleared for the remaining items; they stay approvable in the UI).
- The cmd wiring (`approval_cards_runtime.go`) is exercised by compilation and by the
  approvalcard / api tests of the parts it wires, not by a dedicated runtime test.

**Assertions that would pass if the feature were broken:** none found. Every API test checks
the queue row status, `decided_by`, the action_log count (exactly one execution) and the
card record; every refusal test checks both the error code and that nothing ran and the card
was not consumed.

**Fakes that could hide failures:** the notify tests use an in-memory rule store; the chat
end-to-end test scopes the rule store to its one channel (so no other test's rule in the
shared database can fire) but uses the real Postgres card store, real Slack signatures, the real
executor (`Executor.Apply`, standing policy, change lease) and a real `ANALYZE` on
Postgres, with only the Slack HTTP endpoint faked.

## What is left

- Slack threaded follow-ups need a bot token and `chat.postMessage` with `thread_ts`.
- A typed reject reason from chat (Slack modal / Telegram force-reply).
- A deep link from the chat card to the UI needs a configured public URL (none exists).
- SRE cancel proposals could move to card tokens once their messages are cards.
- The `Forecast` slot is filled by the predicted-effect work (roadmap 1.3); the API already
  serves it when a producer writes it, the UI does not render it yet.
- The follow-up worker reads the oldest 500 open cards per cycle; with more than 500
  undecided cards the newest wait until older ones close (closed after 14 days at most).
- An MCP tool to list and decide cards (human principals only), and a UI page to link chat
  identities (today: the admin API only).
