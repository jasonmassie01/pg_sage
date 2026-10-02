# Sage SRE M3 part A: the model turn in the investigator

Branch `claude/sage-sre-m3-llm` (based on master `ca515e5`). Spec: `reviews/2026-09-26/AI-SRE-SPEC.md`
§2, §5, §6, §11, §12 (CHECK-10..12, 41), §13 (M3). Decisions: `~/.claude/tasks/todo-2026-10-01-sre-m3.md`,
amended by the product owner during the work: **the model turn is on by default** whenever an
LLM is configured.

## What was built

After the deterministic diagnosis, the configured LLM reviews it. The causal graph stays the
authority. The model may only:

1. **Rank** the graph's open (not ruled-out) hypotheses by node id. The ranking must be an exact
   permutation: no additions, removals or status changes.
2. **Ask for one next probe** while the graph is inconclusive: a catalog id, typed arguments
   checked by `probes.Registry.CheckArgs`, and a rationale of one line and at most 300
   characters. The probe runs within the 12-probe ceiling and the remaining active time, and
   moves the investigation evaluating → needs_evidence → collecting → evaluating. The
   deterministic diagnosis is then re-run on all stored evidence, and a turn that is left
   reviews the new diagnosis.
3. **Narrate** up to 5 claims citing evidence aliases (E1, E2, ...). `ValidateClaims` checks
   that every number appears in the cited evidence.

Then a **verifier pass** runs. It rechecks the ranking against the final diagnosis, and each
claim against the evidence re-read from the store: the evidence must still be stored, its hash
must still match, and it must still contain every number the claim uses. If the graph is
conclusive and the model ranks another hypothesis first, the **graph wins**. The investigation
records a `model_disagreed` event and keeps nothing the model said. The store itself refuses a
ranking that puts another node above a concluded root.

**Validation and fallback.** Every reply is checked. It is read from the `submit_review` tool
call, or from JSON content that may be bare, fenced or inside prose. It must be 16 KiB or less
and is decoded strictly: unknown fields, nodes, probes and arguments are rejected, and so is
evidence that is out of scope, stale (hash mismatch) or not cited. A rejected reply gets **one
repair turn**. A provider error (for example HTTP 400 on `tools`) makes the repair turn use
JSON-schema prompting without tools. Timeouts, 429s, an exhausted budget and too little time
left fall back immediately. Every fallback concludes with the **deterministic result** and a
`model_rejected` event that gives `reason`, `stage` and `detail`. The model never fails an
investigation. If the store refuses the model output at conclusion, the deterministic
conclusion is stored instead. A disabled or missing LLM is logged **once per process** and
never recorded per investigation.

**Budgets (§11).** The durable reservation allows 2 turns, 16k input and 4k output tokens, split
evenly per turn (8000/2000). Each turn is also limited by the remaining active time (120 s
ceiling, with a 5 s margin for the conclusion) and by a 50 s cap (`ModelTimeout`). The lease
is heartbeated during the call. The daily allocation is `llm.token_budget_daily`.

**Prompt safety (CHECK-10).** The graph's own text (node ids, labels, mechanisms) is trusted.
Database-derived text (subject, facts, probe reasons) is redacted with `RedactText` and fenced
as untrusted data, and the system prompt carries `llm.UntrustedDataRule`. Exactly one tool is
offered. The tool schema includes `next_probe` only when a probe is allowed.

**Surfaces.** `summary.model_ranking` (`label: "model ranking"`, plus a basis that says it is not
a confidence), `summary.narrative` (`label: "model-generated narrative"`, with evidence UUIDs)
and `summary.model_probe` are stored beside the deterministic hypotheses, which the model never
touches. The detail, the export, the API and the MCP read tools, which all render
`Service.Detail`/`Export`, show them together with `model_turns`. The Markdown export adds
"Model turns: N", "Model ranking (model-generated; ...)", "Narrative (model-generated; ...)"
and "Model-proposed probe" after the deterministic sections. Model text is redacted before it
is stored and again when it is read.

**Config.** `sre.llm.enabled` defaults to **true** (YAML only, restart lifecycle). With an LLM
configured, investigations use the model. With none, the sidecar logs one line saying why. Set
`false` to turn the model turn off. Config metadata, the lifecycle reference and
`docs/configuration.md` are updated, and the setting is registered as YAML-only in the store
consistency test.

### Files

| Area | Files |
|---|---|
| Contract (parse, check, reasons) | `sidecar/internal/sre/model_contract.go` |
| Evidence aliases + grounding text | `sidecar/internal/sre/model_evidence.go` |
| Prompt + tool schema | `sidecar/internal/sre/model_prompt.go` |
| Turn orchestration, repair, budgets | `sidecar/internal/sre/model_turn.go`, `model_session.go` |
| Next probe, final review, finish | `sidecar/internal/sre/model_probe.go` |
| Verifier | `sidecar/internal/sre/model_verify.go` |
| Persisted model output + store validation | `sidecar/internal/sre/model_record.go`, `record.go`, `conclusion.go` |
| Once-per-process notices | `sidecar/internal/sre/model_notice.go` |
| `RecordEvent`, `Limits` | `sidecar/internal/sre/postgres_model.go` |
| Coordinator wiring | `sidecar/internal/sre/coordinator.go`, `worker.go`, `plan.go` |
| Export | `sidecar/internal/sre/export.go`, `export_model.go` |
| Probe arg check | `sidecar/internal/sre/probes/check_args.go` |
| Schema M3 (event types) | `sidecar/internal/schema/sre_m3_migration.go`, `bootstrap.go` |
| Config | `sidecar/internal/config/sre.go`, `sidecar/web/src/generated/config_meta.json`, `docs/generated/config-lifecycles.md`, `docs/configuration.md` |
| Runtime wiring | `sidecar/cmd/pg_sage_sidecar/database_runtime_sre.go`, `sre_model_wiring.go` |
| Changelog | `CHANGELOG.md` (Unreleased: Added + upgrade note) |

## Design notes

- **Evidence aliases.** The model cites `E1..En` (store order: step sequence, then time), never
  UUIDs. A model-proposed probe appends evidence at the end, so aliases stay stable across
  turns. A checked review binds each cited alias to its UUID, and the verifier resolves claims
  through that binding, not by position.
- **Grounding text.** Each alias's text is the probe/status line plus the facts the
  deterministic diagnosis bound to that evidence, redacted and capped at 1200 characters.
  Numbers in those facts were computed by code, so a claim can only quote them (§2.2).
- **Newest one-shot observation wins.** One-shot probes (lock graph, prepared xacts, ...)
  diagnose their newest observation. Series probes (connection saturation, slots, WAL,
  archiver) keep every sample. Deterministic plans never repeat one-shot probes, so their
  behaviour is unchanged.
- **Resume safety.** The plan's move to evaluating is now keyed `evaluate-f<fence>`. A run
  resumed after it had needed more evidence (including one under the v1.7 key `evaluate`)
  reaches evaluating again. Before this change it would have stayed `collecting`.
- **Request keys** are `model-f<fence>-<n>`. A resumed run whose turns were already used gets
  `budget_exhausted` from the durable turn cap and never calls the provider.
- **Event schema.** The M2 inline check on `sre_events.event_type` refused new types. M3
  replaces it with `sre_events_event_type_m3`, added NOT VALID and then validated, idempotent.

## Test Results

**Command:** `go test -cover -count=1 -v ./...` (Docker `golang:1.25`, cgo, repo root mounted,
PG17, `GEMINI_API_KEY` unset)
**Total:** 8747 passed, 1 failed, 12 skipped (top-level tests and subtests). The one failure is a
pre-existing flake in an untouched package (see Failures). Two test-only commits came after this
run (the keep-alive test and the PG18 schema-test fix). Their packages were re-run on PG14-18
and pass.

**Matrix and race detector:** `./internal/sre/... ./internal/schema/` on PG14, 15, 16 and 18:
385 passed, 0 failed, 0 skipped on each (after the PG18 test fix below).
`go test -race -count=1 ./internal/sre/... ./internal/schema/` and
`-run SRE ./cmd/pg_sage_sidecar/` (PG17): ok.
**Lint:** `golangci-lint run ./...`: 0 issues. **gofmt:** clean on every changed file.

**Coverage (touched packages):**

| Package | Coverage |
|---|---|
| internal/sre | 87.4% (PG17 full run), 87.7% (PG14-18 after the keep-alive test) |
| internal/sre/causal | 95.7% |
| internal/sre/probes | 90.2% |
| internal/schema | 81.6% |
| internal/config | 87.5% |
| internal/store | 74.2% |
| internal/startup | 92.2% |
| cmd/pg_sage_sidecar | 71.8% |

All touched packages meet their coverage thresholds (business logic ≥ 70%). No touched package
is below threshold.

### Skipped tests (all outside the touched packages and all env-gated or not applicable)
- agentdb `TestAWSRDSLiveProvisioning`, `TestCloudSQLLiveProvisioning`,
  `TestLakebaseLiveProvisioning` and three `TestAgentDBLiveGauntlet*` tests: need
  `PG_SAGE_LIVE_*` cloud credentials.
- azure `TestAzureLiveServerParameter`, `TestAzureLiveRestartBoundParameter`: need
  `PG_SAGE_LIVE_AZURE=1`.
- llm `TestChatWithToolsLive_RealProvider`, rca `TestTier2Live_RealGemini`: need
  `PG_SAGE_LIVE_LLM=1` (no live LLM calls by default).
- logwatch `TestResolveLogDir_AbsoluteWindows`: Windows only.
- `TestRCAChildProcessFixture`: a helper process, run only by its parent test.

### Failures
- internal/analyzer `TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding`: FAIL in the full
  parallel run with `relation "pg_stat_statements" does not exist`. Pre-existing and unrelated:
  the analyzer is untouched, and the package passed twice when re-run alone. A sibling test
  (`preflight_evidence_test.go:102`) drops and re-creates the extension, which races with the
  other tests under load.

### Coverage gaps
None below threshold.

### Bugs found by the tests
1. **[BUG, test logic] PG18 only.** `TestSREMigrationM3_ModelEventTypes` counted
   `sre_events_event_type_not_null`, because PostgreSQL 18 stores NOT NULL as `pg_constraint`
   rows. The test now selects `contype = 'c'`. The migration was correct. This is the only
   test that was changed after it failed, and the commit message says why.
2. **[BUG, found while designing the tests, fixed]** `worker.go`: a run resumed after
   `needs_evidence` would replay the step key `evaluate` (an idempotent no-op), stay
   `collecting`, and fail `Conclude` with an invalid transition until its active time ran out.
   The key is now per claim. `TestCollect_ResumeAfterNeedsEvidenceConcludes` covers it.
3. **[LIMITATION, pinned by a test]** Reasoning models (Gemini 2.5+/3, OpenAI o-series,
   DeepSeek R1) never get a model turn. `llm.normalizedMaxTokens` adds 16384 to `max_tokens`,
   which exceeds the 4k output ceiling (a DB CHECK). Each turn is refused before dispatch with
   `budget_exhausted`. `TestModelTurn_ReasoningModelRefusedBeforeDispatch` pins this. **It
   matters now that the turn is on by default and the house LLM is Gemini.** See "Left for
   later".

Every test passed on its first run, which CLAUDE.md treats as suspicious. So I mutated the
implementation (27 mutants) and checked which ones the suite caught. Eight survived. Seven of them (verifier
claim re-grounding, the evaluate key, a final turn with no budget, scope checks on model
evidence, client disabled mid-call, redaction at storage, fencing of evidence) now each have a test that
fails against them (`model_gaps_db_test.go`, commit `f0e8f5c`), plus a heartbeat test
(`model_keepalive_db_test.go`). One mutant is equivalent: removing the run-cancelled check in
`call` ends the same way, because the next store call fails on the cancelled context.

## Post-test audit

1. **Inputs that would still break this and are untested.**
   - Real provider quirks: OpenAI-compatible servers that return `content` as an array of
     parts, or both content and a tool call (the tool call wins, but this is untested), and
     Gemini rejecting JSON-schema keywords such as `additionalProperties` in tool parameters.
     The 400 falls back to JSON prompting, but that uses both turns, so no review follows a
     probe.
   - The `oversized_prompt` path: no fixture produces a prompt over 24 KB. Twelve connection
     samples still fit.
   - Plan- and WAL-family investigations with the model on. Only the lock and connection
     families are driven end to end.
   - Usage over the reservation (`ErrUsageExceeded` with a reply), `cooldown` and
     `invalid_request` classification.
   - Homoglyph node ids. They are rejected as `unknown_node` by construction, but no test
     sends one.
2. **Behaviour no assertion covers.** The per-turn timeout as `min(ModelTimeout, remaining
   active time)` when the remaining time is the smaller (only the cap side and "no time" are
   tested). The content of the `detail` field in rejection events. The text of the log line
   for the zero daily budget in a real runtime (it is asserted only through
   `sreLimits`).
3. **Assertions that could pass with the feature broken.**
   - `TestModelTurn_ConcurrentWorkersReviewOnce` also passes if the scheduler runs the two
     workers one after the other. It shows that there is one review, not that a race was
     exercised.
   - `TestReviewMessages_FitsTheTurnBudget` checks only size.
   - The cmd integration tests check `ModelRanking != nil`, not its content. The sre tests
     cover the content.
4. **Fakes that hide real failure modes.**
   - The fake model always returns well-formed `usage` and `finish_reason: "stop"`. Real
     providers truncate (`finish_reason: "length"`, cut-off tool arguments) and omit usage,
     which settles as an unknown full hold.
   - The scripted probe runner hides probe latency and real catalog SQL, which other packages
     cover against Postgres.
   - No test runs against a live model. A `PG_SAGE_LIVE_LLM`-gated investigation test is left
     for part B of the integration.

## Product-owner follow-up: LLM-backed features that default to off (`internal/config`, not changed)

- `llm.enabled`: `DefaultLLMEnabled = false` (`defaults.go:57`). This is the master switch, so
  "on whenever an LLM is configured" still means a user must turn it on.
- `rca.narration_enabled`: false. This is the LLM narrative on incident notifications, an
  SRE/RCA feature.
- `llm.index_optimizer.enabled`: `DefaultIdxOptEnabled = false`.
- `llm.optimizer.enabled`: `DefaultOptEnabled = false`.
- `llm.optimizer_llm.enabled`: false. This is the dedicated reasoning tier; it falls back to the
  general client.
- `tuner.llm_enabled`: false (zero value; not set in `DefaultConfig`).
- `advisor.enabled`: false. This is the LLM configuration advisor.
- For reference, these are already on: `explain.enabled` (true), RCA Tier-2 LLM correlation
  (gated only by `rca.llm_correlation_threshold`), and the per-database `llm_enabled` (nil means
  enabled).

## Left for later
- **Reasoning-model support.** This is the top item, because Gemini 2.5+ never gets a turn
  today. Either let `ToolOptions` set the reasoning reserve or disable thinking for the SRE
  turn, or decide (spec owner) whether the 4k output ceiling should exclude reasoning tokens.
- An LLM-on arm in PGIncidentBench, with a fake model in CI and a real one behind
  `PG_SAGE_LIVE_LLM` (integration step).
- Cases panel UI (`web/src/pages/cases/InvestigationPanel.jsx`) for the model ranking,
  narrative and turn count. The data is in the API, but the React panel and the built `dist`
  were not changed.
- §11 `hypotheses[].missing facts` and `conclusion.limitations / next operator step` from the
  model are not in the contract. The narrative covers observed facts and inference.
- The analyzer `pg_stat_statements` flake (pre-existing; see Failures).
- The lesson in `tasks/lessons.md` was written into this worktree by the coordinator. It is
  committed separately so it can be dropped if it belongs elsewhere.
