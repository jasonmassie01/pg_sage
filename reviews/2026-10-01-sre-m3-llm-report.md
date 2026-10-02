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

**Reasoning allowance (coordinator decision, second pass).** Thinking models (Gemini
2.5+/3, OpenAI o-series, DeepSeek R1, QwQ) get an explicit reasoning budget that is separate
from the unchanged 4k answer ceiling:
- `llm.ToolOptions.ReasoningTokens` replaces the hard-coded 16384 reserve: `max_tokens` is the
  answer cap plus the allowance. 0 keeps today's request for every existing caller, and `Chat`
  is untouched. Non-thinking models never get a reserve.
- `sre.Limits.MaxReasoningTokens` has the ceiling `CeilingReasoningTokens = 16384` per
  investigation and is validated in [1, 16384].
- A thinking-model turn reserves input 8000, answer 2000 and reasoning 8192 durably (new
  `reasoning_reserved` / `reasoning_used` ledger columns) and requests `max_tokens` 10192.
- The per-investigation and daily checks count reasoning. Unknown outcomes hold it in full,
  and settled rows hold what the provider reported. The split uses the provider's
  prompt/completion/`reasoning_tokens` breakdown, or total above prompt + completion for
  Gemini.
- An overrun is recorded as reported, flagged `ErrUsageExceeded`, and refuses any later turn
  that would exceed 16384.
- AI-SRE-SPEC §11 records the allowance in one sentence.

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
| Reasoning allowance (llm) | `sidecar/internal/llm/tools.go`, `tools_wire.go`, `repair.go` |
| Reasoning budget (sre) | `sidecar/internal/sre/limits.go`, `store.go`, `budget.go`, `postgres_budget.go`, `model_turn.go` |
| Schema M3 (event types, reasoning columns) | `sidecar/internal/schema/sre_m3_migration.go`, `bootstrap.go` |
| Spec | `reviews/2026-09-26/AI-SRE-SPEC.md` §11 |
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
**Total (final run, after the reasoning allowance):** 8798 passed, 1 failed, 12 skipped
(top-level tests and subtests), 67 packages ok. The one failure is the same pre-existing analyzer
flake in an untouched package (see Failures). The first run, before the reasoning work, had
8747 passed, 1 failed (the same flake) and 12 skipped.

**Matrix and race detector:** `./internal/sre/... ./internal/llm/ ./internal/schema/` on PG14,
15, 16 and 18: 587 passed, 0 failed, 1 skipped on each (the env-gated live LLM test). The first
PG16 run had one timeout flake (see Failures), and two re-runs passed.
`go test -race -count=1 ./internal/sre/... ./internal/llm/ ./internal/schema/` (PG17): ok.
`-race -run SRE ./cmd/pg_sage_sidecar/`: ok.
**Lint:** `golangci-lint run ./...`: 0 issues. **gofmt:** clean on every changed file.

**Coverage (touched packages, final PG17 run):**

| Package | Coverage |
|---|---|
| internal/sre | 88.0% |
| internal/llm | 90.5% |
| internal/sre/causal | 95.7% |
| internal/sre/probes | 89.5% |
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
- internal/sre `TestStore_OperatorVersionPreconditions` (an existing M1 test) failed once after
  30.03 s on PG16. That run had sre, llm and schema running in parallel, and 30 s matches
  `schema.bootstrapLockTimeout`: the schema package's destructive tests hold the
  cross-package bootstrap lock. Two re-runs of the same command and two sre-only runs on PG16
  passed. This is lock contention between test packages, not a product failure.

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
3. **[BUG, fixed in the second pass]** Reasoning models (Gemini 2.5+/3, OpenAI o-series,
   DeepSeek R1) never got a model turn. `llm.normalizedMaxTokens` added 16384 to
   `max_tokens`, more than the turn's reservation, so every turn was refused before dispatch
   with `budget_exhausted`. A first-pass test pinned the limitation. That test was replaced in
   the tests-first commit `56fa38c`, and the separate reasoning allowance fixes the bug
   (`0531131`, `d045bd3`, `d3c501d`).
4. **[BUG, test logic] Reasoning daily tests.** Both new daily-allocation tests made the
   *deployment* allocation tight. Every test in the package shares one deployment and its UTC
   day, so other tests had already used it up (139546 held). The first reservation was refused,
   and the investigator case would have passed for the wrong reason. The tests now make the
   database allocation (a fresh scope) the tight one (`a6be04d`, with the reason in the
   message).

Every test passed on its first run, which CLAUDE.md treats as suspicious. So I mutated the
implementation (27 mutants) and checked which ones the suite caught. Eight survived. Seven of them (verifier
claim re-grounding, the evaluate key, a final turn with no budget, scope checks on model
evidence, client disabled mid-call, redaction at storage, fencing of evidence) now each have a test that
fails against them (`model_gaps_db_test.go`, commit `f0e8f5c`), plus a heartbeat test
(`model_keepalive_db_test.go`). One mutant is equivalent: removing the run-cancelled check in
`call` ends the same way, because the next store call fails on the cancelled context.

The reasoning code was mutated the same way: 13 mutants (the allowance in `toolMaxTokens` and
its thinking-model gate, the session's thinking-model switch both ways, per-investigation
reasoning cap, daily total, overrun flag, per-call budget limit, usage split, passing the
allowance to the client, the limit's upper bound, the early request cap, and settled-usage
charging). 12 were killed. The one survivor is equivalent: dropping the early
`req.Reasoning > MaxReasoningTokens` check in `validateTokens` still gets the same
`ErrBudgetExhausted` from `checkBudgets`. The check stays because it mirrors the input/output
pre-checks and fails before the advisory lock and transaction.

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
   - `cooldown` and `invalid_request` classification. Usage over the reservation with a reply
     is now covered by `TestReasoningModel_OverrunRecordedAndRefusesSecondTurn`.
   - Homoglyph node ids. They are rejected as `unknown_node` by construction, but no test
     sends one.
   - Real reasoning-usage shapes. The split assumes OpenAI's
     `completion_tokens_details.reasoning_tokens`, or Gemini's total above prompt +
     completion. It is unverified against live Gemini 2.5/3. An o-series reply without the
     details field attributes reasoning to the answer and is flagged as an overrun. Not tested.
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

## PGIncidentBench: the LLM-on arm (third pass)

After `claude/sage-sre-m3-bench` was merged, the bench's `causal-graph+llm` arm was wired to
the model turn:
- **Model per arm.** `Env.investigate` takes the arm's model, which becomes
  `CoordinatorDeps.Model` (the `sre.llm.enabled` path). The causal-graph arm passes nil.
- **Fake mode (default, CI).** Each run gets a fresh in-process OpenAI-compatible fake,
  seeded by the scenario id. Its reviews are valid but against the graph: it ranks the
  last open hypothesis first, asks for a probe whenever one is offered, and narrates claims
  citing real evidence ids, one quoting a grounded number. On a deterministic 4 in 10 of
  its calls it sends fenced JSON, an unknown node, an ungrounded number or HTTP 429.
- **Live mode.** `PG_SAGE_BENCH_LLM_URL/MODEL/KEY` build a real `llm.Client`. Its log is
  silent, and the key never reaches the log or the report.
- **Per-run counts.** Each run records model turns, accepted reviews, `model_rejected` and
  `model_disagreed` (from the event chain). The JSON has them per run and per cell, and
  the Markdown has a "Model turn" table.
- **Gates.**
  - `M3-LLM-PARITY` (fake mode, per family): Safe Pass and top-1 at most 0 points under
    `causal-graph`.
  - `M3-LLM-ROOT` (every mode): no root that `causal-graph` concluded on the same scenario
    and repeat is changed or dropped.

  Both fail the bench. For the fake model, the LLM arm's §12 gates are `not_evaluated`
  with the reason; in live mode they are evaluated.

Files: `sidecar/sre-bench/{fakemodel,fakeprompt,llmclient,llmgates,report_model}.go`, and
changes to `arms.go`, `harness.go`, `env.go`, `scenario.go`, `metrics.go`, `report.go`,
`report_md.go` and `README.md`.

### Results: PostgreSQL 16, 1 repeat, fake adversarial model

Per family (k/n). Two scenarios were skipped because the server has no fixture for them:
`lock-prepared-holder` (max_prepared_transactions = 0) and `wal-archiver-failure`
(archive_mode off). They are excluded for every arm.

| family | arm | Safe Pass | top-1 | abstain (insufficient) | decoy false dx | probes/run | packet p95 |
|---|---|---|---|---|---|---|---|
| connection_pressure | causal-graph | 8/8 | 5/5 | 3/3 | 0/2 | 4.0 | 5426 ms |
| connection_pressure | causal-graph+llm (fake) | 8/8 | 5/5 | 3/3 | 0/2 | 4.4 | 6182 ms |
| connection_pressure | always-escalate | 8/8 | 0/5 | 3/3 | 0/2 | 0 | n/a |
| connection_pressure | rules-only | 6/8 | 5/5 | 1/3 | 2/2 | 4.0 | 5426 ms |
| lock_blocking | causal-graph | 9/9 | 6/6 | 3/3 | 0/2 | 4.0 | 690 ms |
| lock_blocking | causal-graph+llm (fake) | 9/9 | 6/6 | 3/3 | 0/2 | 4.2 | 828 ms |
| lock_blocking | always-escalate | 9/9 | 0/6 | 3/3 | 0/2 | 0 | n/a |
| lock_blocking | rules-only | 8/9 | 5/6 | 3/3 | 0/2 | 4.0 | 690 ms |
| plan_regression | causal-graph | 5/5 | 3/3 | 2/2 | 0/1 | 2.0 | 29 ms |
| plan_regression | causal-graph+llm (fake) | 5/5 | 3/3 | 2/2 | 0/1 | 2.4 | 67 ms |
| plan_regression | always-escalate | 5/5 | 0/3 | 2/2 | 0/1 | 0 | n/a |
| plan_regression | rules-only | 4/5 | 3/3 | 1/2 | 1/1 | 2.0 | 29 ms |
| wal_retention | causal-graph | 7/7 | 5/5 | 2/2 | 0/1 | 7.0 | 5297 ms |
| wal_retention | causal-graph+llm (fake) | 7/7 | 5/5 | 2/2 | 0/1 | 7.3 | 5386 ms |
| wal_retention | always-escalate | 7/7 | 0/5 | 2/2 | 0/1 | 0 | n/a |
| wal_retention | rules-only | 6/7 | 5/5 | 1/2 | 1/1 | 7.0 | 5297 ms |
| **all** | causal-graph | **29/29** | **19/19** | 10/10 | 0/6 | 4.4 | 5297 ms |
| **all** | causal-graph+llm (fake) | **29/29** | **19/19** | 10/10 | 0/6 | 4.7 | 5386 ms |
| **all** | always-escalate | 29/29 | 0/19 | 10/10 | 0/6 | 0 | n/a |
| **all** | rules-only | 24/29 | 18/19 | 6/10 | 4/6 | 4.4 | 5297 ms |

Model turn of the LLM arm:

| family | runs | model turns | accepted reviews | model_rejected | model_disagreed |
|---|---|---|---|---|---|
| connection_pressure | 8 | 14 | 5 | 1 | 2 |
| lock_blocking | 9 | 13 | 4 | 3 | 2 |
| plan_regression | 5 | 8 | 5 | 0 | 0 |
| wal_retention | 7 | 11 | 3 | 1 | 3 |
| all | 29 | 46 | 17 | 5 | 7 |

What this shows:
- **Parity and roots hold.** `M3-LLM-PARITY` and `M3-LLM-ROOT` pass in every family, with 0
  of 19 conclusive roots changed. The adversarial model never moved Safe Pass or top-1, and
  it never named a root on a decoy or benign run, even though it asked for a probe on every
  inconclusive one. Probes/run rose by 0.2 to 0.4, and the packet p95 stayed well under the
  2-minute gate.
- **Disagreements.** 7 disagreements were recorded where the graph was conclusive with
  more than one open hypothesis. The graph won each time.
- **Fallbacks.** 5 replies fell back to the deterministic result: 429s, and repairs that
  failed after a rejected first reply.
- **Accepted reviews.** 17 reviews were accepted: inconclusive graphs, which accept any
  order, and conclusive graphs with a single open hypothesis, where the reversed order is
  the same.
- **Run-to-run consistency** is n/a because there was 1 repeat.

### Runtime and the CI budget

With both live arms, the bench took **327 s** alone on PG16 and **322 s** inside the full
PG17 suite (with -cover). Before this change, with one live arm, it took 122 s in the PG17 suite. CI runs the
bench inside `go test ./...` in three steps:
- unit, `-race`, 900 s;
- integration, 600 s per test binary;
- the PG14/15/16/18 matrix, `-race`, 1200 s.

At `SAGE_BENCH_REPEATS=1` (CI's default), the 600 s integration step has about 45% headroom,
with no live model in CI. It would not fit two repeats (about 650 s), or a live model with
slow replies (up to 2 turns × 50 s per investigation). **Proposed fix, if the budget
tightens:** run the bench in its own CI step, for example
`go test -run TestPGIncidentBench -timeout 1200s ./sre-bench/`, and skip it in the other
steps with `-skip TestPGIncidentBench`. That keeps every scenario; no scenario is cut.

### Analyzer flake (`TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding`)

The root cause is test isolation through the cluster-wide `pg_stat_statements` hash table.
The sibling test's `DROP EXTENSION` only affects the analyzer's own database, so it is not
the cause. The test ran `SELECT pg_sleep(0.02)` once and expected the first snapshot to
show it, but two things can remove that entry:
- **Eviction.** Other packages fill the 5000-entry table. During a full PG17 run,
  `pg_stat_statements_info.dealloc` reached 29: about 5% of the entries evicted 29 times. A
  once-run statement is among the lowest-usage entries.
- **Unscoped resets.** `pg_stat_statements_reset()` in `internal/collector` and in
  `test/hint_verify` clears every database's entries.

Fix (commit `898d2bb`):
- The analyzer test re-runs the workload until a snapshot captures it.
- The avoidable cluster-wide resets are scoped to their own database. The collector's
  epoch-checking reset must stay cluster-wide.

Synthetic reset loops did not reproduce the failure on demand. The fix is validated by a
clean full PG17 run in which both mechanisms were active (stats_reset moved, dealloc 29).

### Test results (third pass)

- **Full suite (PG17, repo root mounted):** 8862 passed, **0 failed**, 12 skipped (the same
  env-gated or Windows-only tests). sre-bench 89.5% coverage (64.6% without the full bench
  run), analyzer 85.6%, collector 85.5%, sre 88.1%, llm 90.5%.
- **Bench (PG16, both live arms, 1 repeat):** pass, 327 s.
- **Bench unit and DB tests with `-race` (PG16, full bench excluded):** 77 passed, 0
  skipped.
- **Lint:** 0 issues.
- **Two-phase:** tests in `3a37587` before the implementation (`05cf44e`, `e44bd6b`). Two
  existing arm tests that asserted the LLM arm was "not wired" were rewritten, because that
  behavior is what the change replaces; the commit says so.
- **Post-test audit (bench):**
  - The fake's prompt parser depends on the investigator's prompt format (section headers,
    `E<n> [probe status]`, `- id: {}` menu lines). A format change would make the fake
    rank nothing, so every review would be rejected and the arm would fall back to
    deterministic. Parity would still pass while the model path went unexercised.
    `TestFakeModel_ConclusiveGraphWinsAgainstTheRealInvestigator` and
    `TestFakeModel_ProbeRunsOnAnInconclusiveGraph` use the real prompt, so they would
    catch that.
  - `M3-LLM-ROOT` pairs runs by scenario and repeat across separate injections. A fault
    that manifests differently between the two injections could fail it without any model
    effect. It did not happen in this run.

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
- A live `PG_SAGE_LIVE_LLM` check of the reasoning-usage breakdown on Gemini 2.5/3 (see the
  post-test audit).
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
