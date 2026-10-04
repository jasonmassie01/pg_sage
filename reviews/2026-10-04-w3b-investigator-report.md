# W3-B report: tool-calling investigator (roadmap 2.1)

2026-10-04 · branch `claude/w3b-investigator` (from `release/v1.10.0`, merged again after the
v1.10.0 cut) · stacked on #112.

The model now plans its own read-only probes after the causal graph has scored the
hypotheses. It concludes with cited evidence and gets authority only through #111's measured
per-family rule.

## Design

| Owner decision | Where | What |
|---|---|---|
| 1. Tool calling over the probe catalog | `internal/agentloop/` (new, reusable), `internal/sre/investigator*.go`, `probes/catalog_stats.go`, `investigator_explain.go`, `explain/read_statement.go` | **agentloop** is a provider-neutral bounded loop: native tool calls through `llm.Client.ChatWithTools`, with automatic fallback to a JSON action protocol (`{"tool","args","plan"}`, parsed with `StripJSON`, so ```json fences work) when the provider refuses tools before any native success.<br>The SRE investigator gives it six tools: `run_probe` (catalog probes with typed, checked args), `read_stat_view` (`database`, `tables`, `statements`, added as catalog probes in family `stat_views`), `explain_statement`, `graph_state`, `confirmed_facts` and `submit_conclusion`.<br>`explain_statement` is broad plan only. It reads the statement text by queryid inside a read-only transaction, then `PREPARE` and `EXPLAIN (FORMAT JSON) EXECUTE` with `plan_cache_mode=force_generic_plan` and NULL arguments, then `DEALLOCATE` after rollback. It is never `ANALYZE` and returns node shapes only, never query text.<br>No tool mutates anything. Remediation stays a typed proposal through `policy.Gate`; the investigator has no path to it. |
| 2. Bounded loop | `agentloop/loop.go`, `actions.go`, `citations.go`, `investigator_model.go`, `postgres_investigator.go` | Steps, tool calls (3× steps), probe cost (the plan's probes, capped by the investigation's remaining 12-probe ceiling, i.e. the probe limiter), wall clock (plan, capped by the lease segment) and tokens.<br>On the last step, the last call, or when tokens cannot pay for two more calls, only `submit_conclusion` is offered and any other call is refused.<br>Each model call is reserved before it is sent, in `sage.sre_budget_reservations` under caller kind `sre_investigator`. The reservation covers the per-investigation cap plus the database and deployment daily allocations, is exactly the loop's conservative estimate, and is settled with reported usage. No migration was needed.<br>Every executed read is committed as `sre_evidence` (sha256 of the canonical payload). The transcript records the digest, cost and elapsed time per step.<br>The citation filter drops claims that are uncited, cite unknown aliases, are ungrounded (a number not in the cited evidence), empty, too long, duplicate or over the limit. Drops are counted by reason, never repaired. |
| 3. Outcomes and authority | `investigator_outcome.go`, `investigator_events.go`, `investigator_record.go`, `replay_export.go` | `agree`, `conclude` (inconclusive graph), `contest` (conclusive graph) and `unmodeled` (cause plus mechanism), or `inconclusive`. Any outcome without a surviving cited claim is downgraded to inconclusive.<br>`conclude` and `contest` are adopted only with #111's `RootGrant` for the family of the named node, and only for an open hypothesis; otherwise they stay advisory (L1). `unmodeled` is always advisory. Self-reported `confidence` is parsed and ignored (there is a test).<br>Contests write `model_disagreed` and `Summary.ModelContest`, are tagged `model_contested` in the replay-case export, and carry `ReplayCaseExport.ModelContest` for #111's contested → replay pipeline. |
| 4. Plans as data | `investigator_plan.go`, `causal/triage.go`, `plan.go` | `InvestigatorPlan` values:<br>• broad: 10 steps, 6 probes, 90 s, 64k tokens, plus EXPLAIN; used for `operator` and `slo_burn` triggers.<br>• narrow: 5 steps, 3 probes, 45 s, 32k tokens; used for every detector and incident trigger.<br>Validated against ceilings (12 steps, 64k tokens). An operator start now runs the broad triage (`causal.DiagnoseOperator`) instead of failing for want of a plan. |
| 5. Bench arm | `sre-bench/investigatorarm.go`, `fakeinvestigator.go`, `llmgates.go`, `replay_gates.go`, `report.go`, `replay_grade.go`, `harness.go`, `README.md` | `causal-graph+investigator` is a live arm in `DefaultConfig`, gated like the LLM-on arm (parity, root, claim-refs; §12 gates not evaluated for a fake). It is measured by the model lift: override precision on contests, and inconclusive lift with an unmodeled cause counted as a wrong pick.<br>In CI the fake is stateless and plays one scripted transcript per scenario: diligent, contrarian, unmodeled, tool_spam, forbidden_tool, injection_follower, malformed_call, hallucinated_ids, never_concludes, rate_limited. Every final also states confidence 0.99.<br>`TestReplayCorpus` now replays all 73 cases through three arms. All gates pass; under the fake, override precision is 0/3 and inconclusive lift is −2, as designed. |
| 6. Surfaces | `web/src/pages/cases/InvestigatorTranscript.jsx`, `internal/api/sre_transcript_handlers.go`, `internal/mcp/sre_transcript_tools.go`, `investigator_transcript.go` | The investigation view shows the plan and budget, the model's stated plan (as text), each step with its target, result (`ok, 1 row`), digest and evidence link, `cited` markers, the outcome with `data-authority`, refused calls, dropped claims and the stop reason.<br>`GET /api/v1/databases/{db}/investigations/{id}/transcript` and MCP `sre_get_transcript` (read scope) return `pg_sage.sre.investigator_transcript.v1`, redacted with #111's export redactor (HMAC tokens, default deny). `keep_identifiers=true` needs the operator or admin role (API) or the approve scope (MCP, never an agent). |

Config: `sre.llm.mode` is `investigator` by default, or `review` (the M3 single turn).
It is restart-only and YAML-only. Docs:

- `docs/configuration.md`
- `docs/sage-sre-permissions-and-data-flow.md` (what investigator mode sends to the LLM)
- `docs/mcp.md` (generated tool reference)
- `sidecar/sre-bench/README.md`

The CHANGELOG bullet is under a new `## Unreleased` above `## v1.10.0`. Everything from
`## v1.9.0` down is byte-identical to origin/master (checked after LF normalization; git
stores LF).

## Bugs found this session

1. [BUG] `internal/sre/investigator.go` `finish`: the investigator re-diagnosed on all
   evidence, so a model that re-read `lock_graph` after the chain cleared (tool spam, a loop
   that never concludes) **dropped the graph's conclusive root**. Found by the bench arm's
   adversarial replay. Fixed: a conclusive prior stands, and the reads can only conclude an
   inconclusive graph. Regression test: `TestInvestigator_ReadsCannotDropAConclusiveRoot`
   (both sub-cases fail without the fix).
2. [BUG] `sre-bench/investigator_arm_test.go`: the `_arm` suffix is a GOARCH build constraint,
   so the arm's tests never compiled on amd64. Renamed to `investigatorarm*.go`.
3. [BUG] `internal/mcp/sre_transcript_tools.go` after the merge: MCP v2 removed `canMutate`.
   Keeping identifiers is now gated on the approve scope.
4. [BUG] `internal/store` consistency: `sre.llm.mode` was not classified (YAML-only, restart
   lifecycle, like `sre.llm.enabled`).
5. EXPLAIN of `$n` statements:
   - `EXPLAIN` alone needs arguments;
   - binding zero parameters fails;
   - binding NULLs constant-folds the plan.

   Fixed with `PREPARE` plus `force_generic_plan`.
6. The token estimate did not match the durable reservation, which let calls be refused at
   the store. Fixed: the loop reserves exactly its conservative estimate, plus the "tight"
   final-only rule.

## Mutation testing (citation filter, budget stops, authority rule)

21 mutants were tried. 17 were killed by the first suite; the 4 survivors got tests and are
now killed: **21/21 killed**.

| Mutant | Killed by |
|---|---|
| C1 uncited claim kept · C2 unknown alias skipped · C3 grounding skipped · C6 over-long claim kept | `TestFilterClaims_DropsEachKindOfBadClaimWithItsReason`, `TestRun_HallucinatedEvidenceIsDroppedAndCounted`, `TestRun_ClaimsMayCiteEvidenceFromToolCalls` |
| C4 claim limit off by one · C5 duplicates kept | `TestFilterClaims_DuplicatesAndOverflowAreDropped` |
| B1 final-only ignored on the call path *(survived; new test)* | `TestBudget_LastStepRefusesToolsItDidNotOffer` |
| B2 call budget ignored | `TestBudget_ToolSpamIsCappedByCalls` |
| B3 cost budget off by one | `TestBudget_CostCapsChargedToolsButNotFreeOnes` |
| B4 one step too many | `TestBudget_ALoopThatNeverConcludesStopsAtMaxSteps`, `TestClient_ToolCallLoopNeverConcludingStops` |
| B5 wall clock ignored | `TestBudget_WallClockStopsBeforeTheNextCall` |
| B6 token stop removed | `TestBudget_TokenCapStopsBeforeAnOversizedCall`, `…BoundaryIsInclusive` |
| B7 tight token budget ignored *(survived; new test)* | `TestBudget_TightTokensOfferOnlyTheFinal` |
| B8 duplicate calls run | `TestBudget_DuplicateCallReusesTheFirstResult` |
| A1 contest adopted without grant · A4 every family granted | `TestInvestigator_ContestStaysAdvisoryWithoutAuthority` (+ conclude test) |
| A2 conclusion adopted without grant | `TestInvestigator_ConcludesAnInconclusiveGraphOnlyWithAuthority` |
| A3 contest of a non-open node recorded *(survived; new test)* | `TestApplyAuthority_ContestOfANonOpenNodeIsNoDisagreement` |
| A5 reads move a conclusive root | `TestInvestigator_ReadsCannotDropAConclusiveRoot` |
| A6 adopted unmodeled passes its shape check *(survived; new test)* | `TestModelConclusion_AdoptedUnmodeledFailsItsShape` |
| A7 advisory root stored as root | `TestModelConclusion_ValidateRefusesWhatDoesNotMatch` |

## Test Results

**Command:** `go test -p 2 -count=1 -cover ./...` (PG17, `pgsage-ag6` :55476, golang:1.25,
`--cpus=2`)
**Total (merged tree, after the v1.10.0 merge):** 10348 passed, 2 failed, 22 skipped. The
2 failures are timing flakes in untouched tests and pass 3/3 in isolation (below). The run
before the merge had 99 packages ok; its only failure, the unclassified `sre.llm.mode` key,
was fixed.
**Coverage (touched packages):**

| Package | Coverage |
|---|---|
| `internal/agentloop` | 94.3% |
| `internal/sre` | 87.7% |
| `internal/sre/causal` | 94.3% |
| `internal/sre/probes` | 93.5% |
| `internal/explain` | 93.5% |
| `internal/config` | 91.3% |
| `internal/mcp` | 84.8% |
| `internal/api` | 79.4% |
| `internal/store` | 76.1% |
| `cmd/pg_sage_sidecar` | 80.0% |
| `sre-bench/replay` | 95.1% |
| `sre-bench` | 66.6% (harness, excluded as documented) |

**Other runs:**

- e2e (`-tags=e2e`, PG17): ok (200 s).
- Perf gate (`-tags perfgate`, `PG_SAGE_PERF_SCALE=small`): `TestPerfGate` PASS (166 s).
- PG14 (:55414) and PG18 (:55418), touched packages (agentloop, `sre/...`, mcp, config,
  explain, sre-bench): all ok.
- `-race`, touched packages: all ok except a load flake (below).
- golangci-lint: 0 issues.
- vitest: 452/452 on the merged tree. eslint is clean. `dist` rebuilt.
- gitleaks over the branch diff: no leaks.

### Skipped Tests (must be zero or justified)

There are 22 skips in the full suite. The 16 outside the touched packages are opt-in live
or cloud tests and fixtures, all untouched:

- agentdb: 6 live provisioning tests.
- azure: 2 live tests.
- ha: container failover.
- llm: 2 live provider tests.
- logwatch: a Windows-only path.
- rca: a child-process fixture and live Gemini.
- tuner: a fixture generator.

In the touched packages:

- `sre/causal` `TestContainer_RestartBetweenSamplesInvalidatesComparisons`,
  `…PromotionBetweenSamples…`: SKIPPED. They need `SAGE_TEST_RESTARTABLE_SUPERUSER_URL` (a
  disposable server to restart). Untouched.
- `sre/pooler` `TestPgBouncer_*` (3): SKIPPED. They need a PgBouncer. Untouched.
- `sre-bench` `TestPGIncidentBench`: SKIPPED (opt-in `SAGE_BENCH_RUN=1`, its own CI step).
  The replay corpus with the new arm runs in `TestReplayCorpus`.
- `sre-bench` `TestLiveModelArm`: SKIPPED (opt-in; live LLM calls are forbidden here).

### Failures

Both are in untouched tests, under `-p 2` full-suite load on the shared Docker VM. Both pass
3/3 when rerun alone.

- `cmd/pg_sage_sidecar` `TestFleetReloadConcurrentRetriesConverge`: FAIL. `db "b": ping:
  context deadline exceeded`.
- `internal/sre/probes` `TestRunner_SidecarWideLimit`: FAIL. A 500 ms probe deadline was
  exceeded.

### Coverage Gaps (packages below threshold)

None among the touched packages, other than the harness exclusion above. `internal/store`
is at 76.1% and `internal/mcp` at 84.8%, both above 70%.

### Flakes seen under load (not touched by this branch; pass in isolation)

- `sre/probes` `TestRunner_OneProbePerDatabase` under `-race` with parallel packages: a
  `pg_sleep(0.25)` hit its statement timeout. 5/5 pass in isolation.
- `cmd/pg_sage_sidecar` `TestMainChar_StandaloneStartsEveryComponentAndRestartsUnderSupervisor`
  and `TestEpisodeIncidents_StoreUnavailable`: "create extra test database: connect
  designated test server" timed out at 30 s under load. Both pass alone and in the plain
  full run.

### Manual Checks Remaining

- CHECK-UI: MANUAL. Look at a real investigator transcript in the Cases panel with a live
  model (no live LLM calls were allowed in this session).

## Post-test audit

- **Inputs not tested:** a provider that supports tools but returns parallel calls with
  identical ids. The loop keys duplicates on name + canonical args, not ids, so this is
  covered by design. EXPLAIN of a statement whose `pg_stat_statements` text was truncated
  (`track_activity_query_size`) fails the PREPARE and is reported as `unknown_statement` /
  error; there is no dedicated test.
- **Assertions that would pass with a broken feature:** none found. The adversarial DB tests
  assert runner calls, transcript counts, events and the stored root, not just `err == nil`.
- **Test doubles hiding failure modes:**
  - The fake OpenAI server stands in for providers. Its reply shapes are exercised for
    malformed JSON, fences, empty replies, 429, timeouts and endless loops.
  - The SRE tests use the scripted probe runner. The real runner path is covered by
    `investigator_explain_db_test.go`, the stat-view probe DB tests and the replay corpus.
- **Added after the audit:** the four mutant-killing tests and the conclusive-root regression
  test.

## Product calls (confirmed by the owner, 2026-10-04)

1. **The investigator is the default model mode.** `sre.llm.mode: review` keeps the cheaper
   M3 turn.
2. **An unmodeled cause is always advisory.** No family can earn authority over a cause the
   graph cannot represent.
3. **Authority is the named node's family.** Adoption also requires that node to be an open
   hypothesis; a ruled-out or foreign node is "not an open hypothesis", even with a grant.
4. **The model's reads never move a conclusive root**, though they may conclude an
   inconclusive graph (the M3 probe rule, generalized).
5. **EXPLAIN is never ANALYZE.** It uses the generic plan, so `$n` statements have a plan on
   every version, and returns node shapes only.
6. **The pg_stat views are catalog probes** (family `stat_views`), so the probe ceiling,
   limiter, capability checks and replay corpus apply unchanged.
7. **Contests are tagged in the replay export but are not gold.** They still need an
   operator verdict before they enter the corpus.
8. **Transcripts are readable by viewers with hashed identifiers.** Keeping identifiers is
   operator or admin (API) or the approve scope (MCP); agents never get them.
9. **An operator start ("investigate now") runs the broad triage** instead of failing.

## Open questions (answered by the owner, 2026-10-04) and follow-up

1. **Live-model run: include the investigator arm inside the existing caps. Done, on
   alternating nights.** The caps (400 requests, 2.5M tokens, 45 min, $2) cover one model
   arm's replay of the 73-case corpus, not two. The investigator makes up to 5 or 10 calls a
   case against the review arm's 2, so 73 × 5 already uses most of the request cap. Each run
   therefore measures one model arm, named by `SAGE_BENCH_LIVE_MODEL_ARM`.
   - The workflow's `arm` step picks it. A `v*` tag always measures the review arm: the
     release's model-root authority reads its held-out lift, and a tag build has only one
     run.
   - Scheduled and manual runs alternate by UTC day of the year: even days review, odd days
     investigator.
   - The caps are unchanged. The ledger keeps reading the newest report with a review-arm
     record, so an investigator night never displaces the review arm's measurement.
   - Contract tests run the step's script for both parities, leading-zero days and tags.
2. **Narrow plan: plan-only EXPLAIN for plan-regression triggers. Done.** Plans gain
   `TriggerTools`. The narrow plan offers `explain_statement` only for `plan_regression`,
   with an unchanged budget, and trigger tools are validated like the plan's own.
3. **Ask Sage gets its own budget.** Nothing to do in this PR.

Follow-up verification: the touched packages on PG17, the small perf gate, actionlint (0),
golangci-lint (0) and gitleaks are all green.

## CI fix: pg_stat views at scale (PR #116 CI run 37224261119)

`stat_statements` timed out (57014) on CI's server, which has ~50000 pg_stat_statements
entries. It read the view with its text and matched pg_sage's self-exclusion regex against
every entry.

**Fix.** v2 reads `pg_stat_statements(showtext => false)`, ranks on the counters with
`LIMIT $1`, and marks pg_sage-role statements with `own_role`. pg_sage's own statements can
no longer be told apart by text. `stat_tables` v2 limits before naming relations.

**Timing.** PG17, 45598 entries, 127 MB of text, best of 3:

| Query | Time |
|---|---|
| v1 | 1066–1166 ms |
| Text loading alone | 237–260 ms |
| v2 | 56–69 ms (EXPLAIN ANALYZE: 56–64 ms) |
| `stat_tables` over 3000 tables | 8 ms |

**Tests.** New scale tests fill 3000 long distinct statements and 1500 tables, and require
both probes to stay under half their timeout. They failed before the fix and pass after it.

**Not changed: `temp_spill_statements`.** This existing M6 probe reads text the same way, to
keep pg_sage's statements out (a v1.8.3 rule pinned by
`TestTempSpillProbeLeavesOutPgSageStatements`). It takes 300–370 ms at that size against its
500 ms timeout. It passes on CI's current pgss, but it is a scaling risk worth its own change.

**MCP log line.** `REST=403 MCP=200` in `TestPreflightSurfaceMCP*` is expected. MCP returns a
refused tool call as an `isError` result over HTTP 200 (code -32001, scope required), and no
rows are persisted. The tests pass, so this is not an auth regression.

