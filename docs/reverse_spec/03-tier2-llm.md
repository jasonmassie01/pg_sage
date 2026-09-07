# Tier 2 — LLM-Enhanced Features (Reverse-Engineered Spec)

This documents every LLM-driven capability that exists in the pg_sage sidecar
today, with exact contracts. All citations are `file:line` relative to
`sidecar/`. Tier 2 is **optional**: every feature degrades to deterministic
behavior (or no-op) when the LLM is disabled or its circuit breaker is open.

Common pattern across all features:

- All LLM calls go through `llm.Client.Chat(ctx, system, user, maxTokens)`.
- All JSON responses are parsed via `llm.ParseJSON(raw, shape, &out)`, which
  strips fences/prose and repairs truncated arrays.
- LLM output never reaches the executor directly. It is converted to an
  `analyzer.Finding` (or an `rca.Incident`), persisted, and only then gated by
  the Tier 3 trust/risk system. The LLM picks rationale/SQL; risk tier and
  trust gating are decided by deterministic code.

---

## 1. LLM Provider Abstraction (`internal/llm`)

### 1.1 Client (`internal/llm/client.go`)

OpenAI-compatible chat client. One struct `Client` per logical model.

- **Endpoint shaping** (`client.go:148-154`): trims trailing
  `/chat/completions` / `/chat`, then re-appends `/chat/completions`. Prevents
  double-path. Provider is whatever the OpenAI-compatible endpoint is (Gemini
  via OpenAI-compat, OpenAI, Groq, Ollama, etc).
- **Request shape** (`client.go:36-54`): `model`, `messages` (system+user),
  `max_tokens`, optional `response_format: {type: "json_object"}` when
  `cfg.JSONMode` is true (`client.go:139-141`).
- **Auth**: `Authorization: Bearer <APIKey>` (`client.go:161`).
- **Enabled gate** (`client.go:81-83`): `cfg.Enabled && Endpoint != "" && APIKey != ""`.

**Circuit breaker** (`client.go:85-99, 243-258`):
- Opens after **3 consecutive failures** (`recordFailure`, threshold at
  `client.go:247`).
- Cooldown = `cfg.CooldownSeconds`; auto-closes when `time.Since(opened) > cooldown`
  (`client.go:92-97`). Success resets failure count to 0.

**Token budget** (`client.go:110-120, 306-335`):
- Daily counter `tokensUsedToday` (atomic), reset on calendar-day change
  (`YearDay()` comparison, `client.go:111-115`).
- If `cfg.TokenBudgetDaily > 0` and used >= budget → returns error
  `"daily token budget exhausted (used/budget)"` (`client.go:116-120`). Callers
  detect this substring (`isBudgetExhausted` / `isBudgetError`) to stop early.
- `ResetBudget()` / `Manager.ResetBudgets()` zero the counter on demand.

**Thinking-model handling** (`client.go:122-129`, `models.go:6-13`):
- `isThinkingModel` matches `gemini-2.5`, `gemini-2.0-flash-thinking`, `o1`, `o3`.
- For those models, `maxTokens += 16384` (reasoning tokens consume the output
  budget). Default `maxTokens` when `<= 0` is `16384`.

**Retries** (`doWithRetry`, `client.go:207-241`):
- Exponential backoff delays `[1s, 4s, 16s]` (4 attempts total).
- Retries only on HTTP **429** and **503**; other non-200 returns immediately as
  an error. Request body is recreated each retry. Respects `ctx` cancellation.

**Timeouts**: HTTP client timeout = `cfg.TimeoutSeconds` (`client.go:72-74`).

**Response handling** (`client.go:170-204`):
- Response body capped at **1 MB** (`io.LimitReader`).
- On `finish_reason == "length" | "max_tokens"`, content is run through
  `RepairTruncatedJSON` before returning (`client.go:197-202`).
- Returns `(content, totalTokens, err)`.

### 1.2 Manager (`internal/llm/manager.go`)

Routes by purpose and exposes budget status.

- `ForPurpose(purpose)` (`manager.go:26-34`): returns `Optimizer` client for
  `"index_optimization"` / `"query_tuning"` **if non-nil**, else `General`.
- `ChatForPurpose` (`manager.go:37-48`): calls the purpose client; on error,
  if it was a non-general client and `fallback` is true, retries on `General`.
- `TokenStatus()` (`manager.go:62-71`): per-client `ClientStatus`
  (model, enabled, tokens used, budget, exhausted, circuit open, reset
  timestamp = next local midnight). Surfaced via the API/dashboard.

**DEAD / UNWIRED ROUTING:** In `cmd/pg_sage_sidecar/main.go:390` and `:833`,
the Manager is always built as `NewManager(llmClient, nil, false)` — optimizer
client `nil`, fallback `false`. The advisor calls
`ChatForPurpose(ctx, "advisor", ...)` (e.g. `vacuum.go:142`), but `"advisor"`
is **not** a case in the `ForPurpose` switch, so every advisor call routes to
`General` and the per-purpose routing + fallback path are effectively unused in
production. (The optimizer builds its own clients directly, not via the
Manager — see §3.)

### 1.3 Optimizer client factory (`client.go:263-304`)

`NewOptimizerClient(parent, opt, logFn)` builds an independent client that
inherits endpoint/key/model/timeout/budget/cooldown from the general
`LLMConfig` when the optimizer-specific fields are empty. Lets reasoning spend
be tracked separately.

### 1.4 JSON parsing & fence stripping (`internal/llm/stripjson.go`)

- `JSONShape` = `JSONObject | JSONArray | JSONAuto` (`stripjson.go:13-23`).
- `StripJSON(s, shape)` (`stripjson.go:37-57`): `stripFences` then extract first
  open delimiter → last matching close. `stripFences` (`stripjson.go:120-138`)
  handles ` ```json ` / ` ``` ` Gemini/OpenAI wrappers.
- **`ParseJSON(raw, shape, out)`** (`stripjson.go:70-100`) — the unified parser
  used everywhere:
  - empty / `[]` / `{}` short-circuit to zero value, return nil (callers treat
    "nothing recommended" as empty).
  - First `json.Unmarshal`; on failure retries with `RepairTruncatedJSON`;
    error reports both attempts + first 200 chars of response.

### 1.5 Truncated-JSON repair (`internal/llm/repair.go`)

`RepairTruncatedJSON` (`repair.go:24-76`): for an array cut mid-object (thinking
model exhausted budget), finds the last complete top-level `}` (string/escape
aware) and appends `]`. No-op if both `[` and `]` already present.

### 1.6 Pre-LLM sanitization (`internal/llm/sanitize.go`)

`SanitizeForLLM(text)` (`sanitize.go:178-180`) = `RedactSQLLiterals(StripSQLComments(text))`:
- `StripSQLComments`: removes `/* */` (nested) and `-- ` comments; preserves
  string literals (defeats prompt injection hidden in comments).
- `RedactSQLLiterals`: `'literal'` → `'?'`, `$tag$..$tag$` → `$?$`, E-strings
  handled (prevents PII/secret leakage). Used by the optimizer and rewrite
  advisor before query text is put in a prompt (`prompt.go:115`, `rewrite.go:137`).

### 1.7 Model listing (`internal/llm/models.go`)

`ListModels(ctx, endpoint, apiKey)` with 1-hour cache. Dispatches to Gemini
ListModels (`generativelanguage.googleapis.com`) or OpenAI `/models`. Used by
the Settings/config UI to populate model dropdowns. Not part of an analysis
loop.

---

## 2. Daily Health Briefings (`internal/briefing`)

**Prompt job** (`briefing.go:343-348`): system =
*"You are pg_sage, a PostgreSQL DBA agent. Generate a concise health briefing
from the structured data provided. Use markdown… Prioritize critical findings.
Keep it under 2000 words."* User message = the deterministically-built
structured briefing text.

**Inputs** (`Generate`, `briefing.go:167-200`):
- Open findings (top 50 by severity then occurrence_count), `gatherFindings`
  (`:202-234`).
- System overview JSON: db size, connections, active, cache hit ratio, uptime
  hours, `gatherSystem` (`:236-253`).
- Recent actions (last 24h) from `sage.action_log`, `gatherRecentActions`
  (`:255-270`).

**Flow**: always build the deterministic markdown briefing
(`buildStructured`, `:272-341`). If `llm != nil && IsEnabled() && !IsCircuitOpen()`
(`:188`), enhance with the LLM; on LLM error, fall back to the structured text.
`maxTokens` = `cfg.LLM.ContextBudgetTokens`.

**Output**: markdown string. Stored in `sage.briefings` (`storeBriefing`,
`:351-361`) with `llm_used` and `token_count`. **Dispatched** to configured
channels — `stdout` and/or `slack` webhook (`Dispatch`, `:367-399`).

**Cadence** (`:115-156`): cron schedule `cfg.Briefing.Schedule` parsed into
bitmask (`parseCron`, full 5-field cron with ranges/steps). `ShouldRun(now)`
matches schedule + 30s debounce. Invoked from the analyzer-cadence loop in
`main.go:718`.

**Gating**: `briefing.enabled` + LLM enabled. Read-only feature; never touches
the executor.

---

## 3. LLM Index Recommendations (`internal/optimizer`)

The single source of truth for `missing_index` recommendations (schema lint
deliberately does not propose indexes — see package doc `optimizer.go:1-8`).

**Prompt job** (`prompt.go:14-54`): system prompt = "PostgreSQL index
optimization expert"; 13 hard rules (CONCURRENTLY only, partial/INCLUDE/GIN/
GiST guidance, write-heavy >70% needs >30% improvement, composite column-order
rule, work_mem-not-index and matview-not-index escape hatches). Demands a bare
JSON array.

**Inputs** (`FormatPrompt`, `prompt.go:57-166`): per-table context — columns,
`pg_stats` (n_distinct, correlation, MCVs), existing indexes + scan counts,
sanitized query list (calls/mean/total ms), execution-plan summaries, and
specialized workload hints (JSON/JSONB, vector/pgvector, PostGIS — `prompt.go:168-231`),
BRIN candidates (|correlation|>0.8), join patterns. Prompt capped at 16384 chars
→ `FormatPromptTruncated` keeps top-3 queries (`prompt.go:233-261`).

**Output schema** (`prompt.go:38-51`): JSON array of `{table, ddl, drop_ddl,
rationale, severity, index_type, category, affected_queries,
estimated_improvement_pct}`. Parsed via `llm.ParseJSON(..., JSONArray, ...)`
(`prompt.go:264-270`).

**Pipeline** (`Analyze` `:84-177`, `analyzeTable` `:179-228`):
1. Cold-start gate: wait for `cfg.MinSnapshots` (`:92-101`).
2. Build table contexts, sort by total query time, cap at `maxTablesPerCycle`=10.
3. Skip a table if circuit-open (per-table `CircuitBreaker`) or it has open
   index findings (`hasOpenIndexFindings`, `:381-403`).
4. Call LLM (primary client, then `fallbackClient` on error — `:186-199`).
5. **Validate** each rec (`validator.Validate`); reject invalid (counts toward
   `Rejections`).
6. **HypoPG enrichment** (`enrichWithHypoPG`, `:230-254`): if `pg_hypopg`
   available, create hypothetical index, measure improvement %, set
   `Validated`, `EstimatedImprovementPct`, `CostEstimate.EstimatedSizeBytes`;
   demote to `severity=info` if not accepted.
7. **Confidence scoring** (`scoreConfidence`, `:256-347`).

**Confidence scoring** (`confidence.go`): weighted sum of six normalized
signals — QueryVolume (0.25), PlanClarity (0.25), WriteRateKnown (0.15),
HypoPGValidated (0.15), Selectivity (0.10), TableCallVolume (0.10), clamped
0–1. `ActionLevel(conf)` (`confidence.go:43-51`): `>=0.7` safe, `>=0.4`
moderate, else high_risk. PlanClarity is 1.0 with EXPLAIN plans, **0.5 with
query text only** — so the optimizer can still reach the advisory threshold
without HypoPG.

**Advisory threshold (0.5):** `DefaultOptConfidenceThreshold = 0.5`
(`config/defaults.go:75`), config field
`cfg.LLM.Optimizer.ConfidenceThreshold` (`config/config.go:232`).
**⚠ DEAD/UNWIRED:** a repo-wide search shows **zero non-test, non-config
consumers** of `ConfidenceThreshold`. Every accepted recommendation is mapped
to a finding regardless of score (`analyzer.go:296-303`). The intended
"suppress below 0.5" gate is not enforced anywhere in code today.

**How output reaches executor** (`optimizer_mapping.go`): each `Recommendation`
→ `Finding` (category from rec, `RecommendedSQL = rec.DDL`, `RollbackSQL =
rec.DropDDL`, detail carries confidence_score/action_level/hypopg_validated/
plan_source). `ActionRisk = RiskTierForRecommendation(rec)` (`risk.go:13-30`:
uses rec.ActionRisk, else maps ActionLevel; unknown → high_risk). The Tier 3
executor then gates by trust level + risk tier.

**Gating**: `cfg.LLM.Optimizer.Enabled && llmClient.IsEnabled()`
(`main.go:997-998`); built with optional `WithAutoExplain()` plan source.

---

## 4. Config Tuning Advisor (`internal/advisor`)

Six sub-advisors, each an independent LLM call producing config/remediation
findings. Orchestrated by `Advisor.Analyze` (`advisor.go:69-218`).

**Sub-advisors** (each has its own system prompt + context builder):
| Sub-advisor | File | Recommends |
|---|---|---|
| Vacuum | `vacuum.go` | Per-table `ALTER TABLE … SET` autovacuum overrides / global `ALTER SYSTEM` |
| WAL | `wal.go` | WAL/checkpoint GUC tuning |
| Connections | `connection.go` | `max_connections`, pooling guidance |
| Memory | `memory.go` | `work_mem`, `shared_buffers`, `effective_cache_size` |
| Query rewrites | `rewrite.go` | SQL rewrite suggestions (advisory only) |
| Bloat | `bloat.go` | Bloat remediation (VACUUM FULL / repack guidance) |

**Example prompt (vacuum, `vacuum.go:15-36`)**: "PostgreSQL vacuum tuning
expert"; 9 rules (only behind-tables, show the math, never scale_factor 0,
skip <1000 rows or <5% dead ratio). Output array of
`{object_identifier, severity, rationale, recommended_sql, current_settings,
recommended_settings}`.

**Inputs**: latest + previous `collector.Snapshot` (config data, per-table
stats, write rates computed from snapshot delta). Prompts capped at 16384 chars.
Each sub-advisor is called with `mgr.ChatForPurpose(ctx, "advisor", system,
prompt, 4096)` → routes to General (see §1.2 note).

**Output → finding** (`parseLLMFindings`, `prompt.go:37-81`): `llm.ParseJSON(..,
JSONArray, ..)`; builds `analyzer.Finding{category, severity, ObjectType:
"configuration", RecommendedSQL, ActionRisk}`. **Risk derivation**
(`deriveActionRisk`, `prompt.go:87-97`): `ALTER SYSTEM` / `DROP INDEX` →
moderate, else safe.

**Cloud transform** (`validate.go:104-181`, applied at `advisor.go:202`): on
managed services (rds/aurora/cloud-sql/alloydb/azure) rewrites `ALTER SYSTEM SET`
→ `ALTER DATABASE <db> SET`; restart-requiring or platform-restricted GUCs are
stripped of executable SQL and downgraded to `info` with a console note.
`ValidateConfigRecommendation` enforces safe value ranges (`validate.go:55-87`).

**Rewrite advisor is always advisory** (`rewrite.go:31, 178-184`): severity
forced to `info`, `RecommendedSQL` and `ActionRisk` cleared — never
auto-executed. CREATE INDEX rewrites are filtered out (belong to the optimizer).

**Gating** (`advisor.go:70-78`):
- `cfg.Advisor.Enabled` required.
- If advisor enabled but LLM disabled → emits a single `advisor_degraded`
  warning finding (`advisorDegradedFinding`, `:220-236`) so a silent no-finding
  cycle isn't mistaken for health.
- Per-sub-advisor `*Enabled` flags + `hasOpenFindings(category)` dedup gate
  (`:255-273`) to avoid redundant LLM calls.
- Interval gate `ShouldRun()` (`:62-66`).
- Budget-exhausted errors counted; remaining sub-advisors still attempted.

---

## 5. Per-Query Tuning / pg_hint_plan (`internal/tuner`)

**Prompt job** (`prompt.go:14-37`): "PostgreSQL query tuning expert specializing
in pg_hint_plan directives"; 12 rules (only valid hint tokens, choose join
strategy by table size/selectivity, IndexScan only if index exists, no
`enable_*` GUC toggles, work_mem from spill / active_backends, confidence
0–1, optional query rewrite). Demands a bare JSON array.

**Inputs** (`FormatTunerPrompt`, `prompt.go:40-69`): the slow-query candidate
(queryid, text, mean exec/plan time, calls, temp blks), detected plan symptoms,
specialized workload hints, execution plan JSON (capped 4000 chars), table
details + indexes + col stats, system context (work_mem, shared_buffers,
max_parallel_workers_per_gather, active_backends), and the deterministic
fallback hint. Capped at 14000 chars → `truncatePrompt`.

**Output schema** (`llm_prescriber.go:14-21`): array of
`{hint_directive, rationale, confidence, suggested_rewrite, rewrite_rationale}`.
Parsed via `llm.ParseJSON(.., JSONArray, ..)`.

**validateHintSyntax** (`llm_prescriber.go:96-168`): rejects the prescription
unless every directive starts with an allowed pg_hint_plan token (`Set(`,
`HashJoin(`, `IndexScan(`, … 16 tokens) **and** matches no dangerous SQL
(`dangerousPatterns` regex blocks `;`, `--`, DROP/DELETE/ALTER/…). This is the
hard guard against the LLM emitting injection or arbitrary SQL.

**Flow** (`tuner.go`):
1. `Tune` fetches slow candidates from `pg_stat_statements` (`candidateSQL`
   `:284-299`), filters self-monitoring queries, skips recently-tuned and
   **deferred tables** (tables with pending index recs — `:133-139, 203-219`).
2. `processCandidate` (`:340-379`) gathers plan symptoms; computes deterministic
   fallback hint; tries LLM (`tryLLMPrescribe`).
3. `tryLLMPrescribe` (`:439-493`): skips LLM for a single symptom without plan
   data (token saving); checks LLM-suppression dedup (fingerprint persisted in
   `sage.findings`, category `query_tuning`, status `suppressed`); calls
   `llmPrescribe` (primary→fallback). On error, records suppression and falls
   back to deterministic hints.
4. Empty/duplicate prescriptions are suppressed with a cooldown
   (`filterRepeatedLLMPrescriptions`, `:495-521`).

**Output → executor**: `buildFinding` (`:780-820`) emits
`Finding{category: "query_tuning", ActionRisk: "safe"}`. `RecommendedSQL` is set
only when `hintPlan.Available && HintTableReady` — `BuildInsertSQL` produces a
dollar-quoted (`$sageqh$…`) INSERT into `hint_plan.hints` (`:874-886`),
`RollbackSQL` a matching DELETE. Also upserts `sage.query_hints` for the
dashboard query-hints page (`:824-858`), including any `suggested_rewrite`.

**Gating**: `cfg.Tuner.Enabled`; LLM path only when `cfg.Tuner.LLMEnabled &&
llmMgr != nil` (`main.go:503-516`, `WithLLM`). Stale-stats symptoms bypass the
hint flow entirely and become `stale_statistics` ANALYZE findings
(`extractStaleStats`, `:385-437`).

---

## 6. Explain Narrative + On-Demand Plan Capture

### 6.1 EXPLAIN narrative (`internal/explain`)

**Prompt job** (`explain_llm.go:11-23`): "PostgreSQL query performance analyst";
given EXPLAIN(ANALYZE) JSON + SQL, return `{summary, slow_because[],
recommendations[]}`.

**Flow** (`explain.go:103-165`): validate (reject DDL, `query_id` lookup not
implemented — `:112-116`), cache lookup (`sage.explain_results`, keyed by FNV
hash + db, with TTL), run `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` (or plain
EXPLAIN for parameterized queries via PREPARE/EXECUTE) in a **read-only
rolled-back transaction** with `statement_timeout`. Build deterministic node
breakdown, then `enhanceWithLLM` (`explain_llm.go:28-56`) overlays the LLM
summary/slow_because/recommendations (`llm.ParseJSON(.., JSONAuto, ..)`).
Graceful: LLM failure leaves deterministic values intact.

**Output → user**: HTTP only. `POST /api/v1/explain` (`router.go:291-292`,
handler `handlers_v09.go:212-261`). Gated by `cfg.Explain.Enabled` (503 if off).
This is an **on-demand, user-triggered** narrative — not part of the autonomous
loop and not connected to the executor.

### 6.2 Auto-explain plan capture (`internal/autoexplain`) — **no LLM**

`Collector.Collect` (`collector.go:73-116`) finds slow queries without recent
cached plans and runs `EXPLAIN (FORMAT JSON)` (no ANALYZE) in a read-only
rolled-back tx, storing into `sage.explain_cache` (source `auto_explain`).
Purely deterministic plan capture; it **feeds** the optimizer/tuner prompts
(those read `sage.explain_cache`) but makes no LLM call itself. Listed here
because it is the plan-source backbone for the LLM features.

---

## 7. RCA Tier 2 — LLM Root Cause (`internal/rca/tier2.go`)

**Prompt job** (`tier2.go:97-109`): "PostgreSQL root cause analysis engine";
given N signals that fired together but match no deterministic Tier-1 pattern,
correlate them and return a single JSON object `{root_cause, severity
(warning|critical), causal_chain (arrow notation), recommended_sql[],
action_risk (low|medium|high)}`.

**Inputs** (`buildTier2UserPrompt`, `:112-124`): the **uncovered** signals
(those not consumed by any Tier-1 incident — `findUncoveredSignals` `:53-70`),
each with ID/severity/fired_at/metrics JSON.

**Gating** (`runTier2Correlation`, `:31-49`):
- `llmClient != nil && IsEnabled()`.
- Only fires when `len(uncovered) >= LLMCorrelationThreshold` (default **3**).
- 30s timeout; 2048 max tokens.

**Output** (`parseTier2Response`/`buildTier2Incident`, `:128-178`):
`llm.ParseJSON(.., JSONObject, ..)`. Empty `root_cause` → error (dropped).
Builds an `Incident{Source: "llm", Confidence: 0.6}`, severity/risk clamped to
valid values, causal chain parsed from `A -> B -> C`. The incident then flows
through the normal RCA→cases pipeline (not directly to the executor). Wired via
`rcaEng.WithLLM(llmClient)` when LLM enabled (`main.go:543-544`).

---

## 8. Interactive Diagnose / ReAct Loop — **DOES NOT EXIST**

The CLAUDE.md architecture notes mention "interactive diagnose (ReAct loop)",
but **no agentic/iterative LLM loop exists in the code today**. The
`diagnose_*` symbols (`internal/cases/incident_projector.go`,
`internal/cases/vacuum_autopilot.go`, `internal/executor/action_contract.go`)
are **deterministic, hand-written read-only SQL probes** (e.g.
`diagnose_lock_blockers`, `diagnose_runaway_query`, `diagnose_vacuum_pressure`)
emitted as executor action types — not LLM calls. There is no multi-turn
tool-using/ReAct agent anywhere in `internal/` or `cmd/`. **This Tier-2 feature
is unimplemented.**

---

## 9. Additional LLM Features Beyond the Brief (found in code)

These are real LLM call-sites that are wired and worth noting:

- **Migration DDL safety fallback** (`internal/migration/llm_fallback.go:37`):
  for DDL no deterministic rule matched, asks the LLM to classify
  `{lock_level, requires_rewrite, risk_score, safe_alternative, explanation,
  estimated_duration_seconds}`; returns nil if `risk_score <= 0.3`. Wired via
  `migration.NewAdvisor(..., migLLM)` (`main.go:667-668`), 30s timeout.
- **Migration script generation** (`internal/migration/llm_scripts.go:59`):
  `ScriptGenerator.Generate` produces a tailored migration SQL script; falls
  back to the deterministic `SafeAlternative` if LLM unavailable. Invoked from
  `advisor.go:124`.
- **JSONB lint enrichment** (`internal/schema/lint/llm_jsonb.go:116`):
  `LLMJsonbAnalyzer.Enhance` confirms which JSONB columns appear in JOIN/WHERE
  via slow-query analysis, enriching `lint_jsonb_in_joins` findings. Wired via
  `lintRunner.SetLLMClient(llmClient)` (`main.go:653-654`).
- **Agent-DB blueprint generation** (`internal/api/agent_db_blueprint_handlers.go:118`):
  translates a natural-language deployment intent into a strict blueprint JSON
  object. HTTP-triggered, uses `mgr.General`.

---

## 10. Complete List of Distinct LLM Call-Sites (today)

Every place that invokes `Client.Chat` (directly or via Manager):

1. `internal/briefing/briefing.go:348` — daily health briefing.
2. `internal/optimizer/optimizer.go:186` (+ `:193` fallback) — index recs.
3. `internal/advisor/vacuum.go:142` — vacuum tuning.
4. `internal/advisor/wal.go:107` — WAL tuning.
5. `internal/advisor/connection.go:96` — connection tuning.
6. `internal/advisor/memory.go:139` — memory tuning.
7. `internal/advisor/rewrite.go:160` — query-rewrite advisor (advisory only).
8. `internal/advisor/bloat.go:135` — bloat remediation.
9. `internal/tuner/llm_prescriber.go:34` (+ `:38` fallback) — pg_hint_plan prescriptions.
10. `internal/explain/explain_llm.go:45` — on-demand EXPLAIN narrative.
11. `internal/rca/tier2.go:82` — LLM root-cause correlation.
12. `internal/migration/llm_fallback.go:37` — DDL risk classification.
13. `internal/migration/llm_scripts.go:59` — migration script generation.
14. `internal/schema/lint/llm_jsonb.go:116` — JSONB-in-joins enrichment.
15. `internal/api/agent_db_blueprint_handlers.go:118` — deployment blueprint JSON.

(#3–8 route through `Manager.ChatForPurpose("advisor", …)` which, given the
`nil`/`false` Manager wiring, always uses the General client.)

---

## 11. Dead / Unwired Findings Summary

- **`OptimizerLLMConfig.ConfidenceThreshold` (0.5)** — configured + defaulted,
  but has **zero consumers**. The advisory-threshold gate it implies is not
  enforced; all accepted recommendations become findings.
- **Manager per-purpose routing + fallback** — `NewManager(general, nil, false)`
  in both standalone and fleet wiring means `ForPurpose` always returns
  `General` and the fallback branch never runs. The advisor's `"advisor"`
  purpose string has no switch case anyway.
- **Interactive diagnose / ReAct loop** — claimed in architecture docs,
  not implemented (see §8).
- **Vestigial fence-strippers** — `stripToJSON`, `stripMarkdownFences`,
  `stripToJSONObject` exist in optimizer/advisor/tuner/explain/rca but the
  production path uses `llm.ParseJSON`; these thin wrappers are retained mostly
  for tests / belt-and-suspenders and are largely redundant with
  `llm.StripJSON`.
