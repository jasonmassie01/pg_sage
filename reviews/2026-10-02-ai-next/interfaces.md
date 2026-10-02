# Interfaces: how humans and agents use pg_sage (AI-next review, 2026-10-02)

Scope: `sidecar/web` (React UI), `sidecar/internal/api` (REST, auth, roles), `sidecar/internal/mcp`,
`internal/chatops`, `internal/notify`, `internal/briefing`, `docs/`. Master @ v1.8.1. Line numbers
are from that tree. "Inferred" marks claims not verified line by line.

## 1. Inventory

| Feature | What it does today | Where | LLM? | Maturity | Evidence |
|---|---|---|---|---|---|
| Web UI shell | ~25 routes, ValuePage is the default route; "Advanced" holds the DBA pages | `web/src/App.jsx:159-234`, `components/Layout.jsx:20-50` | No | solid | 47 test files / 249 tests (fast-trust report) |
| Cases | Ranked incidents + findings; SRE investigation panel (timeline, evidence, model narrative, similar incidents, runbook result, action proposal) | `pages/CasesPage.jsx`, `pages/cases/*`, `api/cases_handlers.go`, `cases_ranking.go` | Investigation narrative/ranking from SRE model loop (`internal/sre/model_*.go`); case ranking itself is deterministic | solid | CasesPage.*.test.jsx, cases_handlers_test.go |
| Actions | Pending / executed / ledger / recommendations / admission tabs; approve, reject, rollback | `pages/Actions.jsx` (798 lines), `api/approve_core.go`, `action_handlers.go` | No | solid | Actions.*.test.jsx (6 files) |
| Earned autonomy | Family x class table (cap, granted, effective, unmet next checks), pending promotions, downgrade, fast-elevation badge | `pages/AutonomyPage.jsx`, `api/autonomy_handlers.go`, `autonomy_ops_handlers.go`, `internal/earned` | No | prototype (UX) / solid (engine) | earned DB tests, AutonomyPage.test.jsx |
| Trust level | observation/advisory/autonomous + ramps, emergency stop | Settings, `TrustBadge.jsx`, `EmergencyStopControl.jsx`, `policy/gate.go` | No | production | policy matrix tests (36,288 combos) |
| Value | Verified DBA-hours saved | `pages/ValuePage.jsx`, `api/value_handlers.go` | No | solid | ValuePage.test.jsx |
| Shadow mode, Runbooks, SLOs, Alert log, Notifications, Users, Profile, Agent DBs | Admin/ops pages | `pages/*.jsx` | Runbook compile uses LLM tool-calling (`internal/sre/runbook/compile.go:161`) | mixed | per-page tests |
| Command palette | Keyboard navigation between pages only | `components/CommandPalette.jsx` | No | solid | - |
| Live events | SSE stream of counts | `hooks/useLiveEvents.jsx`, `api/events.go` | No | solid | events_test.go |
| REST API | ~208 endpoints; roles admin/operator/viewer; cookie sessions; OAuth (google/github/oidc) | `api/router.go`, `auth_middleware.go`, `oauth_handlers.go` | `llm_handlers.go` is config/budget only, no user-facing ask endpoint | production | api coverage 76.1% |
| MCP server | Hand-written JSON-RPC; stdio (default) or `POST /api/v1/mcp`; 25 tools, no resources/prompts | `internal/mcp/server.go:40-61,160-195`, `*_tools.go` | Indirect (runbook compile, investigations) | prototype | server_test.go, mcp_authz_test.go |
| ChatOps | Signed Slack/Telegram button callbacks approve/deny SRE cancel proposals; identity mapping | `internal/chatops`, `api/chatops_handlers.go`, `chatops_decide.go`, `notify/approval_chatops.go` | No | solid (narrow) | chatops_*_test.go |
| Notify | Rules → Slack/Telegram/email/PagerDuty; redaction; SSRF guard | `internal/notify/dispatcher.go`, `slack.go`, `events.go` | No; fixed templates | solid | notify tests |
| Briefing | Daily per-db summary, optional LLM, to stdout or a Slack webhook | `internal/briefing/briefing.go:362-395`, `schedule.go` | Optional | prototype | briefing_test.go |
| Docs | Walkthroughs (linux/windows/fleet), try-it-out, configuration | `docs/*.md` | - | stale in places | - |

## 2. What is weak

### 2.1 The promotion workflow cannot be completed from the UI (root cause of "could not promote")
- L2 needs (a) a PGIncidentBench report ≤ 30 days old, (b) shadow reviews: first review older than
  the window, ≥ N reviewed packets, ≥ 95% accepted, (c) no safety violation (`docs/configuration.md`
  "Promotion"; thresholds in `internal/earned`).
- Shadow reviews are recorded only by `POST /api/v1/sre/autonomy/reviews`
  (`api/autonomy_handlers.go:57`, handler `autonomy_ops_handlers.go:19-45`). Nothing in `web/src`
  calls it (grep for `reviews`, `verdict` finds no caller), and there is no MCP tool for it.
  `InvestigationPanel.jsx:280-295` offers only pin/stop/resume. A user literally cannot produce the
  evidence the page says it needs.
- Bench upload (`POST .../bench-results`, `autonomy_handlers.go:59`) and "evaluate now"
  (`:51`) also have no UI. The page shows only `Bench report: none ingested`
  (`AutonomyPage.jsx:108`).
- `PendingProposals` returns `null` when nothing is pending (`AutonomyPage.jsx:140`); the only
  guidance is `NextChecks` (`:225-237`), a list like `shadow_reviewed: 0 (needs 3)` with no action.
- `POST /evaluate` returns `{created: []}` with no reason when nothing is proposable (coordinator-
  verified).
- A separate investigation "outcome" endpoint exists but does not feed shadow stats, so an operator
  who confirms a diagnosis elsewhere gets no credit (coordinator-verified).
- Two trust systems never reference each other: `trust.level` + ramps (Settings, TrustBadge) gate
  the index/config actions that did the real work on lifeos; earned L0–L3 (`AutonomyPage`) gates
  only SRE incident families (`lock_blocking`, `connection_pressure`, ...). The user tried to
  "promote" in the second system; the autonomy they already had came from the first. `sre.*` keys
  are not in Settings.

### 2.2 Approval UX lacks the "why"
- The pending-action payload and the Actions pending tab carry no finding title, evidence or LLM
  rationale (coordinator-verified, `api/action_pending_helpers.go`, `pages/Actions.jsx`).
- The approve toast says "executed" even when execution was deferred to a window
  (coordinator-verified).
- Cases: approval is three levels deep (InvestigationPanel → ActionProposal → Request → Approve,
  `pages/cases/ActionProposal.jsx`); finding-type cases have no approve at all; no case shows
  before/after metrics, only status strings.
- `pages/Findings.jsx` (799 lines, has "Take Action") is not imported anywhere: dead. The Dashboard
  "+N new" badge reads a localStorage key only Findings.jsx wrote, so it never changes.
  `components/SparklineChart.jsx` is also unused.

### 2.3 Not AI-native
- No conversational surface: no chat UI, no `/ask` endpoint; `CommandPalette.jsx` is navigation only.
  The LLM tool-calling loop exists (`internal/llm/tools.go:103`, used by `internal/sre/budget.go:97`
  with a 25-probe catalog, `internal/sre/probes/registry.go:139-150`) but only incident triggers can
  start it.
- Cases ranking and messages are deterministic templates; the model's narrative lives only inside
  investigations.
- SSE carries counts only; Cases, Autonomy, SLOs and Value do not subscribe (coordinator-verified).
- First run: default route is ValuePage, which is empty on a new install; `OnboardingWelcome` is
  rendered only on `/advanced` (`Dashboard.jsx:168,403`). `EmptyState` supports CTA props but no
  caller passes them. `/notifications`, `/users`, `/alerts`, `/database` have no nav link.

### 2.4 MCP is not usable by a coding agent today
- Protocol: only `initialize`, `tools/list`, `tools/call` (`mcp/server.go:49-60`); `ping` and
  notifications get `-32601`; results are `{"structuredContent": ...}` with no `content[]`
  (`server.go:134`); tool failures are JSON-RPC errors, not `isError` results. Strict clients
  (Claude Code, Cursor) will mis-handle these (inferred from the MCP spec, not tested here).
- Auth: HTTP transport needs a 24 h browser session cookie; there is no API token for agents.
  stdio runs as an unauthenticated operator principal. Bearer tokens exist only for agent-db.
- `apply_migration` advertises `{ddl, intent, constraints}` but the executor needs
  `table/sql/cycle`, so a schema-following client always fails; the planner supports only
  `ADD UNIQUE` / `SET NOT NULL`; HTTP timeout 30 s vs a 10-minute migration (coordinator-verified).
- Most tools take no `database_id`, so they fail or are ambiguous in fleet mode.
- `ensure_fk_indexes` builds a `LIKE` pattern without escaping `_`/`%` (coordinator-verified).
- Missing for the "fix it at the source" loop: top queries + plans, EXPLAIN / HypoPG what-if,
  query → app source mapping (no sqlcommenter or `application_name` attribution anywhere in
  `internal/`), DDL/migration lint (the classifier in `internal/migration/classifier*.go` and
  `risk*.go` is not exposed), and "this index is app-owned, don't drop" (dogfood: 8 re-drops of
  `idx_thesis_allocation_run`; v1.8.1 now infers `app_managed_index`, but an agent cannot declare it).
- No resources (findings, schema, top-N queries) and no prompts.
- README, try-it-out and walkthroughs never mention MCP or how to configure Claude Code/Cursor.
- The briefing prints to stdout at 06:00 (`briefing.go:369`), which shares stdout with the default
  stdio MCP transport (`config.example.yaml:219-221`) and would corrupt the JSON-RPC stream
  (inferred from code, not reproduced).

### 2.5 Notifications and ChatOps
- `ApprovalNeededEvent(title, sql, database, risk)` (`notify/events.go:51-60`) has no proposal id,
  evidence, rationale or UI link, so index/config approvals cannot get chat buttons; only SRE
  `pg_cancel_backend` proposals can.
- No dedup or rate limit in the dispatcher; the legacy `internal/alerting` is a second, separate path.
- No "verified / rolled back / recovered" follow-up event, so the loop is never closed in chat.
- SQL is sent verbatim; text is fixed templates.
- Telegram `webhook_secret` is optional but required for callbacks; `@channel` chat ids validate but
  can never approve; no UI to set up interactive Slack or Telegram.
- Briefing: Slack raw markdown, bypasses the notify SSRF guard (`briefing.go:384` posts directly),
  not visible in UI or API.

### 2.6 Docs and API hygiene
- All walkthroughs describe an "Overview" landing page that does not exist; no doc on promotion or
  MCP setup; mkdocs nav omits several docs; demo `config-live.yaml` references an unset password var.
- `oauth.provider` doc mentions okta but discovery rejects it; `GET /config` is open to viewers while
  `/config/global` is admin (coordinator-verified).

## 3. Iterate

| # | Change | Value | Effort | Risk | Guardrail |
|---|---|---|---|---|---|
| I1 | "Review this diagnosis" (Accept / Reject + note) on every concluded investigation, wired to `/sre/autonomy/reviews`; also in ChatOps and as MCP `sre_review_investigation` (human principals only) | Unblocks promotion | S | Rubber-stamping | Reviewer must open the evidence tab first; reviews by `mcp:`/system actors rejected like approvals |
| I2 | Autonomy page "Path to next level": per unmet check, a sentence and a button (Review 2 packets, Upload bench / point `bench_results_path`, Evaluate now). `/evaluate` returns `{created, blocked:[{family,class,check,observed,required,how}]}` | Promotion becomes self-explaining | S | None | Read-only explanation |
| I3 | One "Trust" page that shows both systems: trust level + ramps (what pg_sage may do to indexes/config now) and earned L0–L3 per incident family, with "what would change if I raise this" | Removes the main confusion | M | Users raise trust blindly | Show a preview of actions that would have run in the last 7 days |
| I4 | Approval card: title, evidence (scans, size, last use, dependent queries), model rationale, reversibility, rollback SQL, expected effect; toast says queued/deferred/executed truthfully | One-click approve with evidence | M | Bloated payload | Evidence redacted via existing `sanitize` |
| I5 | Notification events carry `queue_id`, rationale and a UI deep link; chat buttons for every approval; dedup per (event, object) per window; follow-up "verified / regressed / rolled back" message on the same thread | ChatOps becomes a full approval loop | M | Approval from chat on wrong item | Same signed callback, single-use approval, identity mapping, re-check evidence at approve |
| I6 | MCP conformance: `ping`, notifications, `content[]` + `isError`, `resources/list`; fix `apply_migration` schema; add `database` arg to every tool; escape LIKE | Works in Claude Code/Cursor | S–M | None | Existing principal checks |
| I7 | Scoped API tokens for MCP-over-HTTP (operator/viewer, per database, expiry), mint from Profile page | Agents can connect without a cookie | M | Token leak | Sealed at rest, hash-only storage, audit every call with token id |
| I8 | Delete `Findings.jsx`, `SparklineChart.jsx`, the dead badge; route `/` to a "Today" page (I/E1); give `EmptyState` CTAs; add nav links | Less confusion | S | None | - |
| I9 | Briefing through `notify` (SSRF guard, Slack blocks), stored and shown in UI/API; never to stdout when MCP stdio is on | Briefing seen, stdio safe | S | - | Existing redaction |
| I10 | Docs: one "Earning autonomy" page; MCP setup for Claude Code / Cursor (`claude mcp add ...`); fix "Overview" references | Shorter time-to-value | S | - | Doc tests that load snippets (pattern already exists for fast-elevation) |

## 4. Expand

- **E1 "Ask Sage" (conversational DBA).** A chat panel (and `/api/v1/ask`, ChatOps `@sage`, MCP
  `ask`) that starts an investigation-style session from a user question, reusing the SRE model
  session and the probe catalog, plus read tools over findings, ledger, value and query store.
  Answers cite evidence ids like investigations already do. It can propose actions, never apply them.
- **E2 Agent-facing developer tools in MCP:** `top_queries` (normalized SQL, calls, time, plan
  hash, regressions), `explain_query` (EXPLAIN without ANALYZE by default; ANALYZE only on a clone),
  `whatif_index` (HypoPG when installed, otherwise planner cost estimate flagged as such),
  `lint_migration` (expose `internal/migration` classifier + risk score: lock level, rewrite,
  estimated duration on this table size), `declare_index_ownership` (app-owned / pg_sage-managed /
  ignore, with the source repo path), `explain_finding` (rationale + evidence for one finding).
- **E3 Query → source attribution.** Parse sqlcommenter comments (`/*file='..',route='..'*/`) and
  `application_name` from `pg_stat_statements` / `pg_stat_activity` samples; store per query id.
  Lets the agent open the right file and lets findings say "OrderRepo.find_by_customer, line 88".
- **E4 Fix at the source (PR mode).** For findings whose real fix is in the app (missing index the
  app should own, N+1, `SELECT *` on wide rows, leaked `test_*` schemas, an index the app recreates)
  pg_sage emits a "source fix packet": the migration file in the app's migration tool format,
  the query rewrite, expected effect, and verification query. A coding agent applies it as a PR via
  MCP; pg_sage watches the deploy (schema change feed already exists in
  `internal/sre/changefeed`) and verifies the outcome in its ledger. pg_sage never writes to the repo
  itself.
- **E5 Migration review in CI.** `pg_sage review-migration <file>` / GitHub check: runs `lint_migration`
  against live stats and a clone rehearsal, comments lock level, duration, and conflicts with
  pg_sage's own planned actions (e.g. "this migration recreates an index pg_sage dropped").
- **E6 Approval inbox.** One queue across actions, SRE proposals, promotions and shadow reviews,
  sorted by risk × age, keyboard-driven (a / r / e), bulk-approve for the same rule and evidence
  pattern.
- **E7 Weekly AI report** from the outcome ledger: done, verified, waiting on you, learned.

## 5. AI-first redesign

**What the model decides.** For each signal or user question, the model picks probes, forms a
hypothesis, drafts the explanation and the proposed action (in the DB or as a source-fix packet),
and chooses the channel and urgency of the message. It also writes the "path to promotion"
explanation and chooses which past investigations most need human review (active learning:
low-confidence and high-impact first).

**What it does not decide.** Execution, trust level, promotions and irreversible actions stay with
`policy.Gate` / `Executor.Apply` and an admin. The interface layer adds no new execution path: chat,
MCP and UI all end in the existing approval queue (`approve_core.go`).

**Tools it needs** (one registry, exposed both to the internal model loop and to MCP clients):
read tools: `top_queries`, `explain_query`, `whatif_index`, `get_finding`, `get_ledger`,
`get_trust_state`, `query_source`, the 25 SRE probes; write-intent tools (all queue, none execute):
`propose_action`, `draft_source_fix`, `request_review`. Same principal and role checks for the model
as for an external agent, so an internal model never has more power than an operator token.

**Validation.** Every model claim must cite evidence ids that exist (the investigation model already
does claim checking, `internal/sre/claims.go`, `model_verify.go`); answers with uncited numbers are
rejected and regenerated once, then shown as "unverified". Proposed SQL goes through `sqlast` and the
policy gate in dry-run before it is shown. Source-fix packets are checked by running the migration on
a clone (`internal/clone`, `internal/migration/rehearsal`) and comparing EXPLAIN before/after.

**Evaluation.**
- Replay: record real sessions (questions, findings, approvals) from dogfood databases; replay
  against new prompts/models and score citation precision, action match with what the human
  approved, and message length.
- Shadow: Ask Sage and generated approval cards run in shadow beside templates for 2 weeks; measure
  time-to-approve and reject rate.
- Outcome ledger: every approved proposal links to its verification (already in `internal/ledger`);
  the metric for this area is "approvals with a verified good outcome per human minute".
- PGIncidentBench gains an "explain" arm: given an incident packet, is the explanation correct and
  cited.

**Learning from outcomes.** Rejections with a reason ("app owns this index", "busy hours") become
standing intents (policy entries or ownership records), not prompt text. Accepted reviews raise the
family's shadow score; repeated rejections of one rule lower its ranking and are surfaced as
"I stopped proposing X because you rejected it 3 times".

**Cost control.** Ask Sage and card generation use the cheap model by default (gpt-6-luna works on
lifeos), the larger model only on escalation; per-database token budget already exists
(`TokenBudgetBanner.jsx`, `llm_handlers.go`); cache explanations by (finding id, evidence hash); no
LLM call for notifications whose evidence did not change; templates remain the fallback when the
budget is exhausted.

## 6. Top 5

1. **Make promotion doable and self-explaining (I1 + I2).** Today the only way to create shadow
   reviews is curl, and the UI shows nothing when nothing is proposable. Add Accept/Reject on every
   concluded investigation (UI, chat button, MCP for human principals), a "path to next level"
   panel with one action per unmet check, and a reasoned `/evaluate` response. Small effort; it is
   the direct fix for the user's failed promotion, and without it earned autonomy cannot advance on
   any install.

2. **One Trust page and one approval inbox (I3 + I4 + E6).** Merge trust level/ramps and earned
   L0–L3 into one view that says what pg_sage may do now and what raising each control would have
   done last week. Every approval card shows the evidence, model rationale, reversibility and
   rollback. This turns "approve" from a guess into a 5-second decision, which is the throughput
   limit on earning trust.

3. **Make MCP a real developer interface (I6 + I7 + E2).** Fix conformance and the broken
   `apply_migration` schema, add scoped API tokens, add `top_queries`, `explain_query`,
   `whatif_index`, `lint_migration`, `declare_index_ownership`, and document `claude mcp add`. This
   lets Claude Code or Cursor fix slow queries and schema in the app repo, where the fix belongs,
   instead of pg_sage fighting the app's migrations (the 8× re-drop on lifeos).

4. **Source-fix packets + query attribution (E3 + E4 + E5).** sqlcommenter/application_name
   attribution and a packet format the coding agent turns into a PR, verified after deploy by the
   existing change feed and ledger; the same classifier as a CI migration check. This expands
   pg_sage from "DBA that changes the database" to "DBA that changes the code that uses it" without
   giving it any write access to repos.

5. **Ask Sage + closed-loop notifications (E1 + I5).** Reuse the SRE model session and probe
   catalog for user questions in the UI, Slack and MCP, with evidence-cited answers; make every
   notification carry the why, a deep link and approve buttons, and post the verification result on
   the same thread. First-run "wow": connect a database, and within 10 minutes the Today page (and
   Slack) says "I found 19 unused indexes costing 4.1 GB; here is the evidence; approve all, or let
   me do them after the 1-hour ramp", followed later by "dropped, verified, no regression".
