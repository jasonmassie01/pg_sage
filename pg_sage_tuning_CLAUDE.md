# CLAUDE.md — pg_sage Optimizer v2 Tuning & Verification

## Mission

Three targeted fixes to make the optimizer v2 produce actionable recommendations on managed PostgreSQL (Cloud SQL, AlloyDB) where HypoPG is unavailable.

**Current state:** Optimizer runs, LLM calls succeed, but (a) Gemini truncates JSON responses on complex tables, (b) confidence scores land at 0.05–0.15 (informational) because HypoPG weight drags the average down, and (c) Tier 3 executor actions from the live run haven't been verified.

**Target state:** Gemini returns complete JSON for all tables, confidence scores reach advisory (0.5+) for well-supported recommendations without HypoPG, and executor actions are verified on Cloud SQL.

---

## Context

### Current Test Results (from INDEX_OPTIMIZER_V2_REPORT.md)

- 230 tests, 0 failures across 12 packages
- Cloud SQL PG16: 39 findings, 2 LLM recommendations, 49K tokens, 13 executor actions
- AlloyDB PG17: 23 findings, 2 LLM recommendations, 84K tokens, 0 executor actions
- Both platforms: confidence 0.05–0.15, HypoPG unavailable, 1 parse error per platform (orders table)
- Gemini model: `gemini-2.5-flash` via OpenAI-compatible endpoint
- Gemini auth: `Authorization: Bearer $SAGE_GEMINI_API_KEY`
- Endpoint: `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`

### Files That Will Change

| File | Change |
|------|--------|
| `internal/optimizer/prompt.go` | Prompt tuning to prevent thinking/truncation |
| `internal/optimizer/confidence.go` | Weight rebalancing for non-HypoPG scenarios |
| `internal/optimizer/confidence_test.go` | Updated assertions for new weights |
| `internal/optimizer/optimizer.go` | max_tokens configuration passthrough |
| `internal/llm/client.go` | max_tokens in ChatRequest |

### Files That Must NOT Change

Everything else. 230 tests must still pass after these changes.

---

## Fix 1: Gemini Truncation

### Problem

The orders table (500K rows, 8+ columns, 4+ slow queries) generates a large prompt context. Gemini's response gets truncated mid-JSON — the `parseRecommendations` function gets `[{"ddl":"CREATE INDEX CONCURRENTLY...` and fails to parse it. This happens on both Cloud SQL and AlloyDB, specifically on the orders table (the most complex table in the test data).

The report says `max_output_tokens` was removed from `ChatRequest` because Gemini's OpenAI-compatible endpoint rejected it. But `max_tokens` (the OpenAI-standard field) should still work.

### Root Causes (investigate in order)

**A: `max_tokens` not being sent.** When `max_output_tokens` was removed from the ChatRequest struct, `max_tokens` may have been removed too. Check:

```go
// In internal/llm/client.go — find the ChatRequest struct:
type ChatRequest struct {
    Model    string    `json:"model"`
    Messages []Message `json:"messages"`
    // Is max_tokens present? It MUST be.
    MaxTokens int     `json:"max_tokens,omitempty"`
}
```

If `max_tokens` is missing or set to 0, Gemini uses its default (which may be low). Fix: ensure `max_tokens` is set from config. The optimizer config has `max_output_tokens: 4096` — this should map to `max_tokens` in the JSON request body.

**B: Gemini "thinking" bloat.** Gemini 2.5 Flash is a thinking model. It may spend most of its output budget on internal reasoning (chain-of-thought) before producing the JSON, leaving insufficient tokens for the actual response. The report mentions "truncated JSON from Gemini thinking model."

Fix: Add explicit anti-thinking instructions to the system prompt:

```
Do NOT include any thinking, reasoning, or explanation. 
Respond with ONLY the JSON array. No preamble, no markdown fences, no text before or after the array.
Start your response with [ and end with ].
```

**C: Prompt too large for context budget.** The orders table has many queries hitting it. If the context packet for orders exceeds `context_budget_tokens`, the response gets squeezed.

Fix: Count the prompt tokens before sending. If the prompt exceeds 80% of the model's context window, truncate the query list (keep top-N by calls/day, drop the rest). The config has `context_budget_tokens: 4096` — this is the INPUT budget. The output budget (`max_tokens`) is separate.

### Implementation

**Step 1: Verify max_tokens is being sent.**

```bash
# Find the ChatRequest struct:
grep -rn "ChatRequest\|max_tokens\|MaxTokens\|MaxOutputTokens" internal/llm/client.go
```

If `max_tokens` is missing from the struct or not populated before `json.Marshal`, add it:

```go
type ChatRequest struct {
    Model     string    `json:"model"`
    Messages  []Message `json:"messages"`
    MaxTokens int       `json:"max_tokens,omitempty"`
}
```

In the `Chat()` method, populate it from config:

```go
req := ChatRequest{
    Model:     c.model,
    Messages:  messages,
    MaxTokens: c.maxOutputTokens, // from config: optimizer_llm.max_output_tokens (default 4096)
}
```

If the Client struct doesn't have `maxOutputTokens`, add it and wire it through from config.

**Step 2: Add anti-thinking instructions to the optimizer prompt.**

File: `internal/optimizer/prompt.go`

Find the system prompt (the 12-rule prompt from the spec). Add at the TOP, before the rules:

```go
const systemPromptPrefix = `CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning, no explanation, no markdown fences, no text before or after the array. Start your response with [ and end with ].

`
```

Also add at the END of the prompt, after the table context:

```go
const responseDirective = `

RESPOND NOW with ONLY the JSON array. Start with [ immediately.`
```

Verify the existing `stripMarkdownFences` function in `prompt.go` also strips any thinking prefix. It should handle:
- ```json ... ``` fences (already handled)
- Text before the first `[` (may need adding)
- Text after the last `]` (may need adding)

If `stripMarkdownFences` doesn't strip pre-JSON text, enhance it:

```go
func stripToJSON(s string) string {
    // Find the first [ and last ]
    start := strings.Index(s, "[")
    end := strings.LastIndex(s, "]")
    if start >= 0 && end > start {
        return s[start : end+1]
    }
    // Fallback: try existing fence stripping
    return stripMarkdownFences(s)
}
```

**Step 3: Truncate oversized prompts.**

File: `internal/optimizer/prompt.go` — in `FormatPrompt()`:

```go
// After assembling the full prompt, check token estimate:
// Rough estimate: 1 token ≈ 4 chars
estimatedTokens := len(prompt) / 4
maxPromptTokens := config.ContextBudgetTokens
if maxPromptTokens == 0 {
    maxPromptTokens = 4096
}

if estimatedTokens > maxPromptTokens {
    // Truncate: keep top-N queries by calls, drop the rest
    // Re-assemble with fewer queries
    reducedQueries := topNByCalls(table.Queries, maxQueriesForBudget(maxPromptTokens))
    prompt = assemblePrompt(table, reducedQueries, config)
}
```

This is a safety valve. Most tables should fit within budget. Only tables like orders (8+ queries, each with plan summaries) should hit this.

### Tests

Add to `prompt_test.go`:

```go
func TestStripToJSON_ThinkingPrefix(t *testing.T)
    // Input: "Let me think about this...\n\n[{\"ddl\":\"CREATE INDEX...\"}]"
    // Assert: returns "[{\"ddl\":\"CREATE INDEX...\"}]"

func TestStripToJSON_MarkdownFencedJSON(t *testing.T)
    // Input: "```json\n[{\"ddl\":\"...\"}]\n```"
    // Assert: returns "[{\"ddl\":\"...\"}]"

func TestStripToJSON_CleanJSON(t *testing.T)
    // Input: "[{\"ddl\":\"...\"}]"
    // Assert: returns unchanged

func TestStripToJSON_TruncatedJSON(t *testing.T)
    // Input: "[{\"ddl\":\"CREATE INDEX"  (no closing bracket)
    // Assert: returns error or empty (can't recover truncated JSON)
```

### Validation

After fixing, run the sidecar against Cloud SQL with the orders table workload. Check:

```sql
SELECT object_identifier, 
       detail->>'llm_rationale' IS NOT NULL AS has_rationale,
       length(recommended_sql) AS sql_length
FROM sage.findings 
WHERE category = 'index_optimization'
  AND object_identifier LIKE '%orders%';
```

- [ ] orders table has at least 1 finding with `llm_rationale` NOT NULL
- [ ] `recommended_sql` is a complete DDL statement (not truncated)
- [ ] No parse errors in sidecar logs for orders table

---

## Fix 2: Confidence Score Rebalancing

### Problem

Current confidence scores land at 0.05–0.15 on managed services because HypoPG validation carries too much weight and HypoPG is unavailable on Cloud SQL, AlloyDB, Aurora, and RDS. This means EVERY optimizer recommendation is `informational` — the executor ignores them all.

The managed service market (Cloud SQL, AlloyDB, Aurora, RDS, Azure Flex) is the primary market. If the optimizer only works on self-managed PG with HypoPG, it's useless for 80%+ of the target users.

### Current Weights (from `confidence.go`)

Look at the current `ComputeConfidence` function. It likely has something like:

```go
// Current (estimated from 0.05-0.15 output with HypoPG=0):
QueryVolume:      0.15  // weight
PlanClarity:      0.20
WriteRateKnown:   0.15
HypoPGValidated:  0.30  // THIS IS THE PROBLEM — 30% of the score is 0 on managed
SelectivityKnown: 0.10
TableCallVolume:  0.10
```

With HypoPG=0 and other signals partially filled, max possible score is ~0.70 × partial_fill ≈ 0.15. That's why everything is informational.

### New Weights

Rebalance so that well-supported recommendations WITHOUT HypoPG can reach advisory (0.5+):

```go
// New weights (must sum to 1.0):
QueryVolume:      0.25  // high-call queries are high confidence
PlanClarity:      0.25  // plan data is the next best validation after HypoPG
WriteRateKnown:   0.15  // write rate context prevents over-indexing
HypoPGValidated:  0.15  // still valuable but not a gatekeeper
SelectivityKnown: 0.10  // pg_stats data adds nuance
TableCallVolume:  0.10  // how much traffic hits this table overall
```

**Why this works:**

With HypoPG=0 but QueryVolume=1.0, PlanClarity=1.0, WriteRateKnown=1.0:
- Score = 0.25(1.0) + 0.25(1.0) + 0.15(1.0) + 0.15(0.0) + 0.10(1.0) + 0.10(1.0) = 0.85
- ActionLevel = autonomous ✓

With HypoPG=0, QueryVolume=0.6, PlanClarity=0.5 (query-text-only), WriteRateKnown=1.0:
- Score = 0.25(0.6) + 0.25(0.5) + 0.15(1.0) + 0.15(0.0) + 0.10(0.5) + 0.10(0.6) = 0.49
- ActionLevel = informational (just below advisory)

With HypoPG=1.0 and everything else high:
- Score = 0.25(1.0) + 0.25(1.0) + 0.15(1.0) + 0.15(1.0) + 0.10(1.0) + 0.10(1.0) = 1.0
- ActionLevel = autonomous ✓

This means: high-traffic queries with plan data and write rate context can reach autonomous without HypoPG. Low-traffic queries without plan data stay informational. HypoPG bumps any recommendation up by 0.15 — a nice boost but not a hard requirement.

### Implementation

**File:** `internal/optimizer/confidence.go`

Find the weight constants or the computation function. Update the weights:

```go
const (
    weightQueryVolume     = 0.25
    weightPlanClarity     = 0.25
    weightWriteRateKnown  = 0.15
    weightHypoPGValidated = 0.15
    weightSelectivity     = 0.10
    weightTableCallVolume = 0.10
)

func ComputeConfidence(input ConfidenceInput) float64 {
    score := weightQueryVolume*input.QueryVolume +
        weightPlanClarity*input.PlanClarity +
        weightWriteRateKnown*input.WriteRateKnown +
        weightHypoPGValidated*input.HypoPGValidated +
        weightSelectivity*input.SelectivityKnown +
        weightTableCallVolume*input.TableCallVolume

    if score > 1.0 {
        score = 1.0
    }
    return score
}
```

### Signal Value Computation

The input signals also matter. Check how each signal is computed in `optimizer.go` → `scoreConfidence()`. The signals should be normalized 0.0–1.0:

```go
// QueryVolume: based on calls/day for the top query hitting this table
//   >= 500 calls/day → 1.0
//   >= 100 calls/day → 0.7
//   >= 10 calls/day  → 0.4
//   < 10             → 0.1

// PlanClarity:
//   EXPLAIN plan available (GENERIC_PLAN or explain_cache) → 1.0
//   Query text only (PG14 standalone, no explain_cache)    → 0.5
//   No query data at all                                   → 0.0

// WriteRateKnown:
//   2+ snapshots, write rate computed → 1.0
//   1 snapshot (cold start)           → 0.0

// HypoPGValidated:
//   HypoPG confirmed improvement >= threshold → 1.0
//   HypoPG showed no improvement              → 0.2 (still ran, just no gain)
//   HypoPG not available                      → 0.0

// SelectivityKnown:
//   pg_stats data with n_distinct + MCV/MCF → 1.0
//   pg_stats with n_distinct only           → 0.5
//   No pg_stats                             → 0.0

// TableCallVolume: total queries/day hitting this table
//   >= 1000 → 1.0
//   >= 100  → 0.6
//   >= 10   → 0.3
//   < 10    → 0.1
```

If the current `scoreConfidence()` doesn't compute signals this way, update it. The key change: make `QueryVolume` and `PlanClarity` produce high values for the Phase 15 test data (500+ calls, PG16 GENERIC_PLAN available).

### ActionLevel Thresholds

Check the current thresholds in `ActionLevel()`:

```go
func ActionLevel(score float64) string {
    if score >= 0.7 {
        return "autonomous"    // Tier 3 can execute
    }
    if score >= 0.4 {
        return "advisory"      // Show in findings, MCP, briefing
    }
    return "informational"     // Low confidence, info only
}
```

Adjust if needed. The key requirement: Phase 15 test data with 500 calls/day queries on PG16 (GENERIC_PLAN available) + write rate known should produce scores in the 0.5–0.8 range → advisory or autonomous.

### Tests

**File:** `internal/optimizer/confidence_test.go`

The existing 18 tests check signal boundaries and ActionLevel. Update the assertions to match the new weights. The test structure should stay the same — only the expected scores and action levels change.

```go
func TestConfidence_HighConfidence_NoHypoPG(t *testing.T)
    // QueryVolume=1.0, PlanClarity=1.0, WriteRate=1.0, HypoPG=0.0, Selectivity=1.0, TableCalls=1.0
    // Expected: 0.25 + 0.25 + 0.15 + 0.0 + 0.10 + 0.10 = 0.85
    // ActionLevel: autonomous

func TestConfidence_MediumConfidence_NoHypoPG(t *testing.T)
    // QueryVolume=0.7, PlanClarity=0.5, WriteRate=1.0, HypoPG=0.0, Selectivity=0.5, TableCalls=0.6
    // Expected: 0.175 + 0.125 + 0.15 + 0.0 + 0.05 + 0.06 = 0.56
    // ActionLevel: advisory

func TestConfidence_LowConfidence_ColdStart(t *testing.T)
    // QueryVolume=0.4, PlanClarity=0.0, WriteRate=0.0, HypoPG=0.0, Selectivity=0.0, TableCalls=0.3
    // Expected: 0.10 + 0.0 + 0.0 + 0.0 + 0.0 + 0.03 = 0.13
    // ActionLevel: informational

func TestConfidence_WithHypoPG_Boost(t *testing.T)
    // Same as MediumConfidence but HypoPG=1.0
    // Expected: 0.175 + 0.125 + 0.15 + 0.15 + 0.05 + 0.06 = 0.71
    // ActionLevel: autonomous
```

**Critical:** Run ALL 18 existing confidence tests after updating weights. Some assertions WILL need updating. That's expected — the weights changed. But the TEST STRUCTURE (what signals produce what action levels) should be logically consistent.

### Validation

After fixing, run against Cloud SQL. Check:

```sql
SELECT object_identifier,
       detail->>'confidence_score' AS score,
       detail->>'action_level' AS level,
       detail->>'hypopg_validated' AS hypopg
FROM sage.findings
WHERE category = 'index_optimization'
ORDER BY (detail->>'confidence_score')::float DESC;
```

- [ ] At least 1 recommendation with `action_level = 'advisory'` or `'autonomous'`
- [ ] Confidence scores in 0.4–0.85 range for high-traffic tables
- [ ] Tables with low query volume still score low (informational)
- [ ] Prometheus `pg_sage_optimizer_recommendations_total{action_level="advisory"}` > 0

---

## Fix 3: Tier 3 Executor Verification

### What Already Executed (13 Actions from Report)

The report says 13 executor actions ran on Cloud SQL. These are Tier 1 findings, not optimizer v2 recommendations (which were all informational). Verify they completed correctly.

### Verification Queries

Connect to Cloud SQL:

```bash
psql -h 130.211.209.178 -U sage_agent -d sage_test -p 5432
```

**3.1 Check action log:**

```sql
SELECT id, category, object_identifier, recommended_sql, outcome, 
       error_message, executed_at,
       left(before_state::text, 80) AS before,
       left(after_state::text, 80) AS after
FROM sage.action_log
ORDER BY executed_at DESC;
```

- [ ] 13 rows in action_log
- [ ] All with `outcome = 'success'` (no failures)
- [ ] No error messages
- [ ] before_state and after_state populated

**3.2 Verify FK indexes created:**

```sql
-- Missing FK indexes on orders should now be created:
SELECT indexname, indexdef 
FROM pg_indexes 
WHERE tablename = 'orders' 
  AND indexdef LIKE '%customer_id%';
-- Should show a new index (created by executor)

SELECT indexname, indexdef 
FROM pg_indexes 
WHERE tablename = 'orders' 
  AND indexdef LIKE '%product_id%';
-- Should show a new index
```

- [ ] Index on orders(customer_id) exists
- [ ] Index on orders(product_id) exists
- [ ] Both use CONCURRENTLY (check action_log sql)

**3.3 Verify duplicate indexes dropped:**

```sql
-- idx_li_product_dup should be gone:
SELECT indexname FROM pg_indexes WHERE indexname = 'idx_li_product_dup';
-- Should return 0 rows

-- idx_li_order (subset of idx_li_order_product) may be gone:
SELECT indexname FROM pg_indexes WHERE indexname = 'idx_li_order';
-- May return 0 rows
```

- [ ] `idx_li_product_dup` dropped
- [ ] `idx_li_order` dropped (or explain why not)

**3.4 Verify unused indexes dropped:**

```sql
-- Check which unused indexes were dropped:
SELECT indexname FROM pg_indexes 
WHERE tablename IN ('orders', 'line_items', 'customers', 'products', 'order_events')
ORDER BY tablename, indexname;
```

Compare with the original test data indexes. The executor should have dropped:
- `idx_li_discount` (zero scans)
- `idx_orders_status` (low selectivity, zero scans)
- Any other zero-scan indexes

- [ ] At least 2 unused indexes dropped
- [ ] No PK or unique indexes dropped
- [ ] No sage schema objects in action_log

**3.5 Verify no damage:**

```sql
-- All tables still accessible:
SELECT count(*) FROM orders;          -- ~500K
SELECT count(*) FROM line_items;      -- ~1M  
SELECT count(*) FROM customers;       -- ~50K
SELECT count(*) FROM order_events;    -- ~400K

-- No INVALID indexes left behind:
SELECT indexrelid::regclass, indisvalid 
FROM pg_index 
WHERE NOT indisvalid;
-- Must return 0 rows

-- FKs still enforced:
SELECT conname, conrelid::regclass, confrelid::regclass
FROM pg_constraint 
WHERE contype = 'f' AND conrelid::regclass::text IN ('orders', 'line_items');
-- All FK constraints still present
```

- [ ] Row counts unchanged
- [ ] Zero INVALID indexes
- [ ] FK constraints intact

**3.6 Verify rollback data captured:**

```sql
SELECT id, before_state, after_state, rollback_sql
FROM sage.action_log
WHERE outcome = 'success'
LIMIT 5;
```

- [ ] `before_state` has snapshot of index/table state before action
- [ ] `after_state` has snapshot after action
- [ ] `rollback_sql` has the reverse DDL (CREATE for drops, DROP for creates)

### Post-Confidence-Fix Verification

After Fix 2 is deployed, run the sidecar for 1-2 cycles and verify optimizer recommendations now reach advisory/autonomous:

```sql
-- New optimizer findings should have higher confidence:
SELECT object_identifier, severity, 
       detail->>'action_level' AS level,
       detail->>'confidence_score' AS score
FROM sage.findings
WHERE detail->>'llm_rationale' IS NOT NULL
ORDER BY (detail->>'confidence_score')::float DESC;
```

- [ ] At least 1 finding with `action_level` IN ('advisory', 'autonomous')
- [ ] Executor picks up advisory+ findings in next cycle
- [ ] New action_log entries from optimizer recommendations

```sql
-- Optimizer-driven actions (post-confidence fix):
SELECT a.id, f.category, f.object_identifier, a.recommended_sql, a.outcome
FROM sage.action_log a
JOIN sage.findings f ON a.finding_id = f.id
WHERE f.category = 'index_optimization'
  OR f.detail->>'llm_rationale' IS NOT NULL
ORDER BY a.executed_at DESC;
```

- [ ] At least 1 optimizer-driven action executed
- [ ] DDL uses CONCURRENTLY
- [ ] outcome = 'success'

---

## Execution Order

1. **Fix 1 (Gemini truncation)** — code changes in `prompt.go` and `client.go`
2. **Run tests** — `go test ./... -count=1` — 230+ PASS
3. **Fix 2 (confidence)** — code changes in `confidence.go`
4. **Run tests** — update confidence_test.go assertions, 230+ PASS
5. **Deploy to Cloud SQL** — start sidecar, wait 2 cycles
6. **Fix 3 (verification)** — run all verification queries
7. **Verify optimizer recommendations reach advisory+** — confirm executor acts on them

---

## Definition of Done

### Fix 1: Gemini Truncation
- [ ] `max_tokens` sent in ChatRequest JSON body
- [ ] Anti-thinking directive in system prompt
- [ ] `stripToJSON()` handles thinking prefix + markdown fences
- [ ] Prompt truncation safety valve for oversized tables
- [ ] orders table produces complete JSON response (no parse errors)
- [ ] Tests pass (existing + 4 new stripToJSON tests)

### Fix 2: Confidence Rebalancing
- [ ] Weights updated: QueryVolume=0.25, PlanClarity=0.25, WriteRate=0.15, HypoPG=0.15, Selectivity=0.10, TableCalls=0.10
- [ ] Signal computation normalized 0.0–1.0 with clear thresholds
- [ ] ActionLevel thresholds: ≥0.7 autonomous, ≥0.4 advisory, <0.4 informational
- [ ] Phase 15 high-traffic queries produce scores in 0.5–0.85 range
- [ ] All confidence tests pass (updated assertions)
- [ ] 230+ total tests pass

### Fix 3: Executor Verification
- [ ] 13 action_log entries verified (all success)
- [ ] FK indexes on orders(customer_id) and orders(product_id) exist
- [ ] Duplicate indexes dropped (idx_li_product_dup)
- [ ] Unused indexes dropped (idx_li_discount, idx_orders_status)
- [ ] Zero INVALID indexes
- [ ] FK constraints intact
- [ ] Row counts unchanged
- [ ] Rollback SQL captured for all actions

### Post-Fix Integration
- [ ] Optimizer recommendations reach advisory+ on Cloud SQL
- [ ] Executor acts on optimizer-driven findings
- [ ] At least 1 optimizer-created index on Cloud SQL
- [ ] Prometheus shows `action_level="advisory"` or `"autonomous"` counts > 0

### Cost Control
- [ ] AlloyDB instance stopped/deleted after verification (saves ~$200/mo)
