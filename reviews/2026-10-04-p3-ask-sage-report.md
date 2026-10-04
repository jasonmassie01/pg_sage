# P3: Ask Sage (roadmap phase 3)

Branch `claude/p3-ask-sage`, stacked on `claude/w3b-investigator` (#116, the tool-calling
investigator and `internal/agentloop`), which sits on master v1.10.0. Neither base moved
during the work. Agent report, 2026-10-04.

## What was built

Ask Sage answers a question about one monitored database from evidence it reads, in the UI
(a chat panel per database), over REST and through the MCP tool `ask_sage`. It runs on the
investigator's bounded tool loop (`internal/agentloop`) with the investigator's grounding
rule (`sre.GroundClaim`); there is no second loop.

| Owner decision | Where | What |
|---|---|---|
| 1. Same loop, read-only tools | `internal/ask/session.go`, `tools_findings.go`, `tools_actions.go`, `tools_catalog.go`, `tools_sources.go`, `concepts.go` | 14 read tools: `list_findings`, `get_finding`, `list_approvals`, `list_actions`, `get_action` (verification outcome: predicted vs observed), `list_facts`, `list_incidents`, `list_investigations`, `get_investigation`, `trust_ledger`, `describe_table`, `top_queries`, `explain_config`, `explain_concept`. Every result is citable evidence with a typed id (`finding:42`, `action:7`, `trust:orders`, `table:public.orders`, ...), the SHA-256 of the exact text shown and a label. A missing object is a citable `not_found`. Strict argument decoding (unknown fields refused), parameterized SQL, bounded rows. |
| 2. Grounded answers | `internal/ask/answer.go`, `run.go` | Final tool `answer` = `claims[]` (text + aliases) and `not_observed[]`. The loop's citation filter drops uncited, unknown-alias and ungrounded claims (every number must be in the cited evidence); `compose` keeps only claims whose ids this run read, only for a run that concluded. "Not observed" notes are kept only when short and without numbers. The answer lists statements with citations (API path + digest), what could not be verified, what was dropped and why, and actions taken. Status: `answered`, `not_observed`, `budget_exhausted`, `llm_unavailable`, `incomplete`. |
| 3. Actions only via existing paths | `internal/ask/tools_write.go`, `internal/executor/ask_proposal.go`, `cmd/pg_sage_sidecar/ask_wiring.go` | Two writes, only for callers who may propose, once per question: `open_investigation` (sre.Service.Start, operator trigger, actor `ask:<actor>`) and `propose_action` (queue one of pg_sage's own open findings). `executor.ProposeFindingForApproval` requires a typed action contract and, for reversible actions, the rollback SQL; asks the standing gate with **Explain** (no decision recorded, no approval flags); refuses blocks an approval cannot lift; records the predicted effect; queues a pending `action_queue` item (24 h) with an approval card. It never executes, even when the gate would. The `ask` package imports no executor, policy, API, MCP or store package (a structural test enforces it). Database text, findings, the question and the history are fenced with `llm.UntrustedData`. |
| 4. Own LLM budget | `internal/ask/budget.go`, `sage.ask_budget_day` | No `sage.tuning_budget_day` exists on this base, so an equivalent table was added: one row per (UTC day, actor) and the database total under actor `*`, both locked (database first) in one transaction per reservation. Each model call reserves the loop's conservative estimate before it is sent and settles to the reported usage. Separate from `llm.token_budget_daily`. |
| 5. Surfaces | `internal/api/ask_routes.go`, `internal/mcp/ask_tools.go`, `web/src/pages/AskPage.jsx` + `pages/ask/*`, `lib/ask.js` | REST: `POST /api/v1/databases/{db}/ask`, `GET .../ask/conversations`, `GET .../ask/conversations/{id}`, `GET .../ask/budget` (session auth; viewers ask, operators/admins may propose). MCP `ask_sage` (read scope; proposals follow the propose scope; annotated not read-only). UI `#/ask`: composer, conversations, numbered citations linking to the evidence's page with the short digest, could-not-verify, dropped (collapsed), actions linking to the Actions page — no approve control anywhere. Conversations in `sage.ask_conversations` / `sage.ask_messages`, purged after `ask.retention_days`. |
| 6. No live LLM in tests | `internal/ask/fake_llm_test.go` and every DB test | Fake OpenAI-compatible httptest server with scripted transcripts: injection via table comment, finding title and query text (fake and real `pg_stat_statements`), requests to run DDL and to approve, hallucinated citations, ungrounded numbers, budget exhaustion before and mid-run, provider without tool calling (JSON fallback), 429, timeout, malformed/empty replies, the caller's deadline and cancellation. |

Other files: `internal/config/ask.go` (`ask.*`, restart lifecycle), `internal/config/field_docs.go`
(`DescribeField` for `explain_config`, never values that are secret or may carry a
credential), `internal/schema/ask_migration.go` (one registration line in `bootstrap.go`),
`internal/retention/rules.go` (two rules) and `exemptions.go` (answers cascade),
`internal/sre/ground_export.go`, one line each in `api/router.go`, `mcp/registry.go`,
`mcp/server.go`, `mcp/production_backend.go`, `cmd/.../database_runtime.go`, `wire.go`,
`mcp_runtime.go`, `config.go` (field, default, validate). Docs: `docs/ask-sage.md` (mkdocs
nav), a section in `docs/configuration.md`, the regenerated MCP tool reference in
`docs/mcp.md`, `docs/generated/config-lifecycles.md`, `web/src/generated/config_meta.json`.
CHANGELOG bullet under `## Unreleased`; everything from `## v1.10.0` down is byte-identical to
origin/master (checked after LF normalization).

## Product calls

1. **Storage is per monitored database** (its sage schema, like facts and findings), so
   bootstrap, retention and fleet isolation work unchanged. The per-user budget is
   therefore per (database, user); a fleet-wide per-user cap would need the control DB.
2. **Only verified statements are shown.** Free prose is never displayed; dropped claims are
   listed as "not verified" with their reason (transparency, as in investigator
   transcripts). Notes may not carry numbers.
3. **Proposals are pg_sage's own findings only** (the finding's SQL, rollback and predicted
   effect), never SQL from the conversation; a typed contract is required, and a
   reversible action needs its rollback SQL.
4. **The gate is asked with Explain, and only blocks an approval cannot lift refuse.** The
   end-to-end test through the real gate found that refusing every `blocked` verdict
   refused an ordinary index fix on a database still on its trust ramp. Blocks of pg_sage's
   own initiative that a person's approval lifts (trust ramp, approval required, budgets,
   rate limits, refusal set, earned autonomy, windows, DDL conflict) still queue; emergency
   stop, replica, binding fact, change class, observe-only and unknown reasons refuse. An
   `execute` verdict is still only queued.
5. **Writes are offered only to callers who may propose** (operator/admin; MCP propose
   scope, agent tokens included) and run at most once per question. Viewers and read-only
   tokens never see them. Unbound MCP callers are treated as read-only agents.
6. **Budget accounting:** reserve before each call, settle to reported usage; calls the
   client refused before provider I/O (429, disabled, cooldown) cost nothing; a timeout or
   an unreported usage is charged the estimate. Defaults: 300k tokens/day per database,
   100k per user, 40k per question.
7. **Bounds fit the API:** 6 model calls, 10 tool calls, 25 s wall (inside the API's 30 s
   deadline), ending 1.5 s before the caller's deadline. An answer cut short by the deadline
   is stored as `incomplete`; a cancelled caller stores nothing.
8. **Conversations are private** per actor (another user's is "not found"); MCP
   conversations belong to the token's actor. At most 50 questions per conversation; the last
   4 turns go to the model as fenced, non-citable history.
9. **On by default** (LLM features are the product); without an LLM the answer is
   `llm_unavailable`.
10. **explain_config** never returns a value that is secret-tagged or whose key names a
    credential (`password`, `key`, `url`, `dsn`, `endpoint`, `..._token`, `meta_db`), nor
    structs/maps. `describe_table` refuses system catalogs.
11. **UI (sub-agent, tests first):** Ctrl/Cmd+Enter sends; input capped at 2000; citations
    link to existing pages (no deep links exist yet); the server's rendered text is never
    shown (structured fields only).

## Bugs found this session

1. [BUG] `executor.ProposeFindingForApproval` refused every blocked verdict, including
   blocks an approval lifts (found by the post-audit end-to-end test through the real gate:
   `trust_ramp_not_satisfied`). Fixed; `TestProposeFindingForApproval_QueuesWhatAnApprovalCanLift`.
2. [BUG] `get_action` read `action_log.approved_by` as text; it is an INT user id. Fixed.
3. [BUG] `ask_messages` had no purge rule or exemption (retention coverage test). Exempted:
   it cascades with its conversation.
4. [BUG, tests, explained in commits] the native LLM client refuses a reply calling
   undeclared tools as a whole (malformed), so the loop's `forbidden_tool` counter never
   moves on that path; the transcripts now cover both refusal paths (native and JSON). The
   mid-run budget test's arithmetic left room for the next call.
5. [DOC] `gen_config_meta` caps doc tags at 200 characters (CHECK-T16); doc tags shortened.

## Mutation testing (citation filter, action restrictions, budget)

31 mutants, **31 killed** (2 first written with syntax errors, corrected and rerun).

| Area | Mutants | Killed by |
|---|---|---|
| Citation filter / answer | unknown ids kept (C1), claims kept without a final (C2), notes with numbers kept (C3), grounding off (C4), answered without statements (C5), dropped text rendered (C6), note length cap off (C7) | `TestCompose_*`, `TestAsk_HallucinatedAndUngroundedClaimsAreDropped`, `TestAsk_NotObservedIsAValidAnswer` |
| Action restrictions | propose/open tools offered without the right (A1, A2), once-per-question off (A3), unknown fields accepted (A4), gate block ignored (A5), rollback rule off (A6), typed-contract rule off (A7), pending dedupe off (A8), non-open findings proposable (A9), MCP propose always (A10), REST viewer may propose (A11), blocked reported as failed (A12), system catalogs describable (A13), credential keys shown (A14), every block liftable (A15), observe-only queued (A16) | `TestGuard_*`, `TestAdversarial_*`, `TestProposal_*`, `TestInvestigation_*`, `TestProposeFindingForApproval_*`, `TestAskTool*`, `TestAskRoutes_*`, `TestTool_DescribeTable`, `TestDescribeField_NeverReturnsCredentials` |
| Budget | database cap off (B1), user cap off by one (B2), no row lock (B3), settle no-op (B4), no reservation (B5), day not UTC-today (B6), usage not charged (B7), `*` accepted as an actor (B8) | `TestBudget_*`, `TestAsk_Budget*`, `TestAsk_GroundedAnswerCitesTheEvidenceItRead` |

Web (sub-agent): 5/5 UI mutants killed (unknown kinds linked, `javascript:` kinds linked,
an Approve button, `dangerouslySetInnerHTML`, `conversation_id` dropped).

## Test Results

**Command:** `go test -p 2 -count=1 -cover -timeout 1800s -v ./...` (Docker `golang:1.25`,
`--cpus=2`, label `owner=asksage`, own PG17 `pgsage-ag3` :55473, repo root mounted)
**Total:** 13,498 passed, 5 failed, 22 skipped (99 packages). One failure was mine and is
fixed (retention coverage, below); the other four are load timeouts in untouched tests that
pass alone.

**Coverage (touched packages, PG17):**

| Package | Coverage |
|---|---|
| internal/ask (new) | 87.8% |
| internal/config | 91.5% |
| internal/schema | 84.5% |
| internal/retention | 87.3% |
| internal/mcp | 84.8% |
| internal/api | 79.5% |
| internal/store | 76.1% |
| internal/sre | 87.7% |
| internal/executor | 87.9% (rerun alone after the load failure) |
| cmd/pg_sage_sidecar | 80.1% (rerun alone after the load failures) |

**Other runs:**
- e2e (`-tags=e2e`, PG17): ok (277 s).
- Perf gate (`-tags perfgate`, `PG_SAGE_PERF_SCALE=small`): `TestPerfGate` ok (196 s).
- PG14 (:55414) and PG18 (:55418), `-race`, touched packages (ask, config, schema, retention, api, mcp, store;
  executor `ProposeFinding*`; cmd `Ask*`): all ok on both, same coverage as PG17.
- golangci-lint: 0 issues.
- vitest: 82 files, 566 passed, 0 failed, 0 skipped; eslint clean; `dist` rebuilt;
  `node_modules` removed.
- gitleaks over the branch diff: no leaks found.

### Skipped Tests (must be zero or justified)
None in touched packages. The 22 skips are the pre-existing environment-gated ones: live
cloud provisioning and AgentDB gauntlets (6), Azure live parameters (2), live LLM providers
(3: `TestChatLive_RealProvider`, `TestChatWithToolsLive_RealProvider`,
`TestTier2Live_RealGemini`), container failover/restart fixtures (3), PgBouncer fixtures
(3), a Windows-only path test, the RCA child-process fixture, the plan fixture generator,
`TestPGIncidentBench` (opt-in) and `TestLiveModelArm` (opt-in; no live LLM calls allowed).

### Failures
- `internal/retention` `TestRetentionRules_CoverEveryTimeSeriesTable`: FAIL (mine:
  `ask_messages` unclassified). Fixed in `ccd0a46c`; the package passes (87.3%).
- `internal/executor` `TestOutcome_DropIsNotDecidedBeforeBusinessCycle`: FAIL, "seed
  query_store: timeout: context deadline exceeded" under full-suite load. Passes alone.
- `cmd/pg_sage_sidecar` `TestFleetReloadAddsDatabaseWithFullRuntime` and
  `TestMetaLifecycleFailedReplacementPreservesOld`: FAIL, "dial error: timeout" /
  "retention foreign keys: timeout" under load. Both pass alone.
- `sre-bench` `TestReplayCorpus`: FAIL, "safety snapshot: context deadline exceeded" while
  the web tests ran in parallel. Passes alone (112 s).

### Coverage Gaps (packages below threshold)
All touched packages meet the thresholds (lowest: store 76.1%, api 79.5%).

### Manual Checks Remaining
- CHECK-ASK-UI: MANUAL. Ask a real question in the browser against a live model (covered by
  vitest and the API tests with a fake model; no live LLM calls were allowed).
- CHECK-ASK-MCP: MANUAL. `ask_sage` from a real Claude Code session with an agent token.

## Post-test audit

- **Untested inputs found and added:** a question and an earlier answer note that try to
  close their `<data>` block (`TestAudit_QuestionAndHistoryCannotLeaveTheirDataBlocks`).
  Still untested: a provider returning parallel tool calls for both writes in one reply
  (each write is guarded by its own once-per-question flag, which `TestProposal_*` and
  `TestInvestigation_*` exercise one call at a time).
- **Assertions that could pass when broken:** the budget-separation test originally asserted
  `errors.Is(x, x)`; it was rewritten before the first run to assert the LLM client's own
  counter stayed at zero.
- **Fakes hiding real failures:** the `ask` tests use a recording Proposer and Starter. Added
  `cmd/.../ask_integration_test.go`: a scripted model proposes a finding through the
  production adapter, the real executor and the real standing gate; the item is pending,
  the index does not exist and nothing is logged as executed. That test found bug 1. The
  investigation adapter is covered with a fake `sre.Service` surface; `sre.Service.Start`
  itself is covered by the SRE suite.

## Open questions

1. Should Ask Sage proposals carry an explicit origin in `sage.action_queue` (today: the
   approval card title "Ask Sage proposal: ..." and the queue's normal fields)?
2. A fleet-wide per-user budget (needs the control database) in addition to per database?
3. Streaming answers (SSE) for a better feel; today it is request/response within 25 s.
4. Should Ask Sage also propose facts (`propose_fact`)? Not included: the owner listed cases
   and proposals.
5. A live-model evaluation set for Ask Sage (answer precision, citation accuracy) like the
   investigator's bench arm.
