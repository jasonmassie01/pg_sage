# Fixes — LLM area (group 03 + codex C05/C06/C11/C12 + G2-B19)

Branch `fix/2026-09-26-llm`, base `9a3cac7`. Owned packages: `internal/llm`, `advisor`,
`optimizer`, `tuner`, `briefing`, `sanitize`, `vectorlab`, `analyzer/optimizer_mapping.go`.
Process: failing tests committed first per package (`test(...)` commits), then fixes.

## Status

| ID | status | commit | test name(s) | notes |
|---|---|---|---|---|
| G3-B01 (P0) | FIXED | 9ebc9c0 | `TestValidateGUCValue_BaseUnits`, `TestValidateGUCValue_EveryDocumentedGUCCovered`, `TestValidateConfigSQL_UnitlessBytesRejected`, `TestParseLLMFindings_DropsUnitlessBytesWorkMem` | A value with no unit is read in the GUC's PG base unit (work_mem/maintenance_work_mem kB; shared_buffers/effective_cache_size/wal_buffers 8kB pages; max_wal_size MB). Unit suffixes are case-sensitive (`B kB MB GB TB`, time `us ms s min h d`), and whitespace before the unit is allowed. `-1` sentinels are accepted. The table test covers every `gucDocs` entry, including the "256MB in bytes" case (`work_mem = 268435456` is rejected). ALTER TABLE reloptions are now validated too. |
| G3-B02 | OWNED BY RUNTIME | — | — | Moved to the runtime agent. No `llm.Manager` API changes were made here. |
| G3-B03 | OWNED BY RUNTIME | — | — | Moved to the runtime agent. No `fleet.FleetBudget` changes. |
| G3-B04 | FIXED | 04990d3 | `TestShouldRun_FiresOncePerScheduledTimeWithCoarseTicks` (605 s and 30 s ticks, 7 days → 7 runs), `TestShouldRun_CatchUpFiresOnce`, `TestShouldRun_UsesPersistedLastRun` (DB) | Fires when a cron minute falls between the last run and now. The last run is read once from `max(sage.briefings.generated_at)`. For a fresh install the worker's start time is the anchor. |
| G3-B05 | FIXED | 5e13675 | `TestAnalyzeTable_DropDDLTargetsCreatedIndex`, `TestAnalyzeTable_DropDDLSynthesizedWhenMissing`, `TestAnalyzeTable_RejectsUnboundDDL` | The DDL is parsed into an `IndexSpec`. It must be a named CREATE INDEX on the analyzed table. `DropDDL` is always built as `DROP INDEX CONCURRENTLY IF EXISTS "<tc.schema>"."<created name>"`; the LLM's `drop_ddl` is never used. |
| G3-B06 | FIXED | 5e13675 | `TestAnalyzeTable_HypoPGRejectionDropsRec`, `TestAnalyzeTable_HypoPGInconclusiveIsNeutral`, `TestScoreConfidence_WithoutHypoPGNeedsEvidence` | HypoPG verdict is now tri-state. If HypoPG measured the index and the gain is below `hypopg_min_improvement_pct` (including 0 or negative), the rec is rejected. Unavailable, error or nothing measurable counts as neutral. Weights rebalanced (HypoPG .10, selectivity .15). Without HypoPG, 0.5 is reachable only with write stats plus pg_stats evidence. |
| G3-B07 | FIXED (owned sites) | 94c3dc1, 9ebc9c0, 87d0506, 67d2109, 04990d3 | `TestUntrustedData_*`, `TestAnalyzeVacuum_PromptDelimitsUntrustedData`, `TestFormatPrompt_RedactsAndDelimits`, `TestFormatTunerPrompt_RedactsAndDelimits`, `TestEnhanceWithLLM_DelimitsDataAndUnwrapsText` | New `llm.UntrustedData`, `llm.UntrustedDataRule` and `llm.SanitizePromptSQL` are the single helper set. All four owned prompt sites delimit DB-derived text and carry the system rule. Tuner: query text, plan JSON and index predicates are sanitized. Optimizer: plan summaries and index predicates are sanitized, and MCV values are replaced by their frequencies. Sites owned by other areas are listed under Cross-area. |
| G3-B08 | FIXED (library) | 9ebc9c0 | `TestHostMemoryGuard_SharedBuffers` | `shared_buffers` SQL is stripped (the finding stays as advisory/info) unless host RAM is supplied via `Advisor.WithHostMemoryBytes` and the value is ≤ 40% of it. Nothing sets host RAM today, so all shared_buffers recommendations are advisory-only (fail closed). |
| G3-B09 | FIXED | 9dbd5fc, 4a42644, 1f7e422 | `TestParseJSON_TruncatedArrayWithInnerArraySalvagesElement`, `_LeadingProseBracketIgnored`, `_SingleObjectForArrayShapeIsWrapped`, `_ObjectWrappingArrayIsUnwrapped`, `_EmptyObjectForArrayIsEmpty`, `TestRepairTruncatedJSON_DepthAware` | The parser now decodes candidate positions one by one instead of slicing first-open to last-close. An object answer to an array prompt is wrapped, or unwrapped when it is a single-field wrapper; `{}` means empty. Truncation repair tracks nesting depth. |
| G3-B10 | FIXED | 9dbd5fc, c73dd91, 67d2109 | `TestParseJSON_EmptyResponseIsError`, `TestChat_EmptyContentReturnsErrEmptyResponse`, `TestTryLLMPrescribe_EmptyResponseNotSuppressed` (DB) | New `llm.ErrEmptyResponse`. Tokens are still charged, the breaker is not tripped, and the tuner records no suppression for it. |
| G3-B11 | FIXED | c73dd91, 04990d3 | `TestChat_JSONModeOnlyForJSONPrompts`, `TestEnhanceWithLLM_DelimitsDataAndUnwrapsText` | `response_format` is sent only when the code-owned system prompt asks for JSON. The briefing output goes through `UnwrapText`. |
| G3-B12 / C06 | FIXED | 5e13675 | `TestOpenRecommendations_SkipLLMAndReEmit`, `TestOpenRecommendations_ExactTableMatch` (DB) | Open findings are matched by exact identity (`category='missing_index'`, `ident = t` or `left(ident, len+1) = t\|\|'\|'`), with no LIKE wildcards. A table with open candidates makes zero LLM calls. Its still-valid, not-acted-on candidates are re-emitted so `ResolveCleared` does not flap them. |
| G3-B13 / C05 / G2-B19 | FIXED | 5e13675 | `TestRecommendation_FindingIdentity`, `TestOptimizerFinding_IdentityPerIndexDefinition`, `_IdentityStableAcrossCosmeticChanges`, `TestOptimizerFindingTable_LegacyIdentity`, `TestVerifiedActionForFinding_OptimizerIdentityUsesTable` | Identity is `schema.table\|<normalized index definition>` with the fixed category `missing_index`; the LLM's label is kept in `detail.index_category`. Table-scoped resolution comes from the re-emit above. Still open: the `UpsertFindings` UPDATE branch does not refresh `rollback_sql` (analysis-owned; see Cross-area). |
| G3-B14 | OWNED BY RUNTIME | — | — | |
| G3-B15 | FIXED (library) | 5e13675, 67d2109 | `TestAnalyzeTable_SameFallbackNotRetried`, `TestLLMPrescribe_SameFallbackNotRetried` | When the fallback client is the primary, the optimizer and tuner no longer retry on it. Passing the optimizer client into the standalone `NewManager` is main.go wiring (Cross-area). |
| G3-B16 | FIXED | 67d2109 | `TestConvertPrescriptions_SetAllowlistAndClamp` | `Set()` is allowed only for work_mem, plan_cache_mode and max_parallel_workers_per_gather. work_mem units are normalized (bare number = kB) and clamped to `work_mem_max_mb`. |
| G3-B18 | PARTIAL | 9ebc9c0 | `TestParseLLMFindings_DropsEmptySQLForConfigCategories` | SQL-less "no changes needed" rows are no longer persisted as vacuum/wal/memory/connection findings, and those rows were what stalled the gate. DEFERRED: per-object dedup and resolve-on-reevaluate. The fix is coupled to C07 (durable candidate queue across analyzer and executor), which needs an analyzer lifecycle change. |
| G3-B19 | FIXED | c73dd91 | `TestChat_ExternalBudgetSmallerThanMaxTokensAdmitsCall`, `TestChat_ExternalBudgetTooSmallForPromptRejects` | The external (per-DB) reservation is now `min(max_tokens, prompt/4 + 1024)`, reconciled to actual usage. Fleet allocations smaller than max_tokens + 16384 can now admit calls. |
| G3-B20 | FIXED (bonus) | 5e13675 | `TestAnalyzeTable_RejectsUnboundDDL` | DDL on another table and UNIQUE indexes are rejected. `rec.Table` comes from the table context, not the LLM. |
| G3-B21 | FIXED | 9ebc9c0 | `TestAnalyzeBloat_DeadTuplesAreNotBloat` | Dead tuples are labeled as dead tuples. The prompt sends them to plain VACUUM and never proposes VACUUM FULL or pg_repack from the dead-tuple ratio alone. |
| G3-B22 | FIXED | c73dd91, 22a342b | `TestThrottle_EvictsExpiredEntries` | Expired entries are evicted from `lastCalls`. The `llm.cooldown_seconds` doc now describes what it really is (per-prompt dedup plus breaker cooldown). |
| G3-B23 | FIXED | 5e13675 | `TestWriteRateKnown`, `TestScoreConfidence_WithoutHypoPGNeedsEvidence` | `TableContext.WriteRateKnown` is true only when the table had activity. |
| G3-B24 | FIXED | 5e13675 | `TestRiskTier_NonCreateIgnoresSelfRating` | Any non-CREATE DDL is `high_risk`. The dead ActionLevel mapping and the LLM's self-rating are removed; before this, an LLM DROP self-rated "safe" passed as safe. |
| G3-B25 | PARTIAL | c73dd91, 4a42644 | `TestBudgetDay_IsUTCAndYearAware`, `TestChat_MissingUsageEstimatesTokens`, `TestIsThinkingModel_Reasoners` | Fixed: UTC budget day (days since epoch), chars/4 estimate when usage is missing, detection of deepseek-r1, reasoner, QwQ and o-series models. DEFERRED: persisted usage ledger (I-LLM-04 needs a schema/migration change). |
| G3-B26 | FIXED | 9ebc9c0 | `TestTruncateAdvisorPrompt_UTF8AndBlockSafe`, `TestVacuumSystemPrompt_NoProseAnswerRule` | Truncation is UTF-8-safe and cuts at a blank-line (per-object) boundary. The vacuum rule that asked for a prose answer is gone. |
| G3-B28 | NOT MINE | — | — | `schema/lint/llm_jsonb.go` belongs to the analysis area. Switch it to `llm.ParseJSON(raw, llm.JSONArray, &m)`. |
| C11 | FIXED (tuner side) | 67d2109 | `TestHintRemovalFindings_RetiredAndBrokenOnly` (DB) | Each Tune cycle compares `hint_plan.hints` with `sage.query_hints`. For an installed hint whose sage rows are all retired or broken, it emits a `query_hint_retirement` finding with `RecommendedSQL = BuildDeleteSQL(qid)`. The executor already classifies this as `retire_query_hint`. User-installed hints (no sage row) are left alone. |
| C12 | FIXED | 67d2109, 22a342b, bcf7f7f | `TestStartRevalidationLoop_RespectsVerifyAfterApply`; config drift and tripwire tests pass | `verify_after_apply=false` now disables the loop. The three cost-comparison keys stay in the config struct and are documented as "Reserved; no effect", because removing them would break existing YAML under `KnownFields(true)`. They were dropped from `config.example.yaml`. `config_meta.json` was regenerated and `api/dist` rebuilt. |
| D01/D02/D03 | DELETED | 5107a05 | — | stripToJSON/stripMarkdownFences copies in advisor, optimizer and tuner. |
| D08 | DELETED | 9ebc9c0 (port), 5107a05 | ported limits covered by `TestValidateGUCValue_BaseUnits` | ValidateConfigRecommendation, parseNumericValue and dangerousLimits. |
| D09 | DELETED (partial) | 5107a05 | — | Removed modelCache.get/set, fetchOpenAIModels and doModelRequest. Kept `InvalidateModelCache` because api package tests use it as an isolation seam. |
| D10 | units FIXED, wiring DEFERRED | 162c440 | `TestComputeQuerySavings_Milliseconds` | The 100× error is fixed. Note that `q.Calls` is cumulative, not per day, so fix that when this gets wired. |
| D11 / D13 | DELETED | 5107a05 | — | decay.go and DetectBloatedIndexes. |
| D12 / D14 | DEFERRED | — | — | Wiring detection.go and IsBRINCandidate into the prompt is out of scope. |
| D22 | FIXED | 04990d3 | — | Dropped the category and recommended_sql columns from the briefing query; they were never rendered. |

## Cross-area edits (made in this branch; small)
- `internal/analyzer/analyzer.go` `openIndexRecommendationTables`: takes the table part before `|` of the new optimizer identity (one `strings.Cut`).
- `internal/executor/index_verification_runtime.go` `verifiedActionForFinding`: reads the table via `analyzer.OptimizerFindingTable(f)` instead of `ObjectIdentifier`, with a regression test. Without this, verification would measure a table named `public.t|btree(...)`.
- `internal/config/config.go` doc tags (3 revalidation keys, `llm.cooldown_seconds`), `config.example.yaml`, `web/src/generated/config_meta.json` (via `cmd/gen_config_meta`), `internal/api/dist` rebuild (`npm ci && npm run build`). If the web agent also rebuilds dist, rebuild once after merging.

## Cross-area wiring still needed (not done here)
- **G3-B15** `cmd/pg_sage_sidecar/main.go:547` (and meta-db/fleet-global): build the optimizer client before the manager and call `llm.NewManager(llmClient, optClient, fallback)`.
- **G3-B08**: add a host-RAM config key (e.g. `advisor.host_memory_bytes`; 7-place config work) and call `adv.WithHostMemoryBytes(cfg.Advisor.HostMemoryBytes)`. Until then shared_buffers stays advisory.
- **C11**: confirm the executor's policy admits `query_hint_retirement` findings (DELETE FROM hint_plan.hints → `retire_query_hint`, base tier safe) so removal actually runs.
- **G3-B05 defense in depth** (executor): in `verifiedActionForFinding`, reject when `extractIndexName(RollbackSQL) != extractIndexName(RecommendedSQL)`.
- **G3-B13 / C04** (analysis): the `UpsertFindings` UPDATE branch should also set `rollback_sql`.
- **G3-B07 sites owned by other areas**: wrap DB text with `llm.UntrustedData` / `llm.SanitizePromptSQL` and add `llm.UntrustedDataRule` to the system prompt in `rca/tier2.go` (drop the log DETAIL and redact message/query), `schema/lint/llm_jsonb.go`, `explain/explain_llm.go`, `analyzer/plan_narrative.go` and `executor/justify.go`. Also delete their unreachable `stripToJSON*` copies (D04–D06).
- **Behavior change for other LLM callers**: `llm.ParseJSON("")` now returns `ErrEmptyResponse`. `Chat` returns it for blank content. `response_format` is only sent for system prompts that mention JSON. All dependent package tests pass (rca, explain, migration, schema/lint, api, analyzer, executor).

## Notes
- **Live cloud test run by accident.** `GEMINI_API_KEY` is set in this shell. The first full unit run executed `rca/TestTier2Live_RealGemini` against the real Gemini API, which the brief forbids. It FAILED ("expected Tier 2 (llm) incident", 21 s; the baseline passed in 9 s). I did not re-run it. The rca mocked end-to-end tests (fenced, truncated, budget, concurrency) all pass. Cause not established: provider variance or a change in the shared parse/Chat path are both possible. Please re-run it deliberately with the key if live tests are allowed. Every later run used `env -u GEMINI_API_KEY`.
- `tuner/tuner.go` (1,050 lines) and several advisor/optimizer functions were already over the size limits. New logic went into new files (`hint_removal.go`, `hint_set.go`, `indexspec.go`, `ddlscan.go`, `openrecs.go`, `llmcall.go`, `gucunits.go`, `schedule.go`, `untrusted.go`). Functions this series touched were trimmed to ≤ 50 lines, except `tryLLMPrescribe` and `parseLLMFindings`, which were already over the limit and grew by a few lines.
- Existing tests changed because they asserted the bugs; each change is explained in its commit message: empty response parsed as nil, a single object treated as an error, `{}` for an array, SQL-less config rows kept as findings, exact-minute briefing semantics, the old confidence arithmetic, and the ActionLevel/self-rated risk pass-through.

## Test Results

**Command:** `SAGE_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:55499/postgres?sslmode=disable go test -count=1 -cover -v ./internal/{llm,advisor,optimizer,tuner,briefing,sanitize,vectorlab,analyzer,executor,config,rca,explain,migration,schema/lint,api} ./cmd/gen_config_meta`, then with `-tags=integration` for llm, advisor, optimizer, tuner, briefing, analyzer, executor, config and rca (`env -u GEMINI_API_KEY`). Also `go build ./...`, `go vet ./...` (clean), `npm ci && npm run lint && npm run test -- --run && npm run build` in `sidecar/web`.

**Total (unit + DB):** 4,542 passed, 1 failed (`TestTier2Live_RealGemini`, live cloud, see Notes), 0 skipped.
**Total (integration tag):** 3,048 passed, 0 failed, 1 skipped.
**Web:** eslint clean; vitest 19 files / 89 tests passed; build OK (existing chunk-size warning only).

**Coverage:**
| package | coverage |
|---|---|
| internal/llm | 89.1% |
| internal/advisor | 80.0% |
| internal/optimizer | 80.4% (was 69.5%) |
| internal/tuner | 83.8% (was 61.0%) |
| internal/briefing | 95.2% |
| internal/sanitize | 71.4% |
| internal/vectorlab | 93.2% |
| internal/analyzer | 84.3% |
| internal/executor | 75.8% |
| internal/config | 84.7% |
| internal/rca | 97.9% (integration run) |
| internal/explain | 82.0% |
| internal/migration | 70.1% |
| internal/schema/lint | 71.3% |
| internal/api | 71.9% |
| cmd/gen_config_meta | 86.4% |

All packages meet coverage thresholds (business logic ≥ 70%, utilities ≥ 50%).

### Skipped Tests (must be zero or justified)
- internal/rca: TestTier2Live_RealGemini — SKIPPED in the integration run because `GEMINI_API_KEY` was unset on purpose (the brief forbids live cloud tests).

### Failures (if any)
- internal/rca: TestTier2Live_RealGemini — FAIL in the first unit run. It ran against live Gemini because `GEMINI_API_KEY` was set in the environment. Not re-run; see Notes.

### Coverage Gaps (packages below threshold)
- None.

### Bugs Found This Session
1. [BUG] advisor/docground.go — unitless memory GUCs read as bytes (P0 G3-B01); 256GB work_mem accepted.
2. [BUG] optimizer/risk.go — an LLM-authored DROP INDEX self-rated "safe" came out as `safe` risk (worse than G3-B24 described).
3. [BUG] optimizer.go — the fixed C06 dedup alone would have caused flapping: skipped tables are not re-emitted and `ResolveCleared` then resolves them. Fixed by re-emitting still-valid open candidates.
4. [BUG] executor/index_verification_runtime.go — verification took the table from `ObjectIdentifier`, so it would break under any per-index identity. Fixed with a regression test.
5. [BUG] briefing — the 30 s debounce let one schedule slot fire twice; an existing test asserted that.
6. [BUG] llm/stripjson.go — `{}` answered to an array prompt in json_object mode became one zero-valued element once objects are wrapped. Now it parses as empty.

### Manual Checks Remaining
- CHECK-M1: MANUAL — re-run `rca/TestTier2Live_RealGemini` deliberately with a key if live tests are permitted.
- CHECK-M2: MANUAL — confirm in a live pg_hint_plan install that the executor applies `query_hint_retirement` removals and that the hint stops applying (C11 acceptance).
