# pg_sage Sidecar — Cloud Test Report

**Date:** 2026-03-22
**Environment:** Windows 11 / Git Bash, Go sidecar (local build)
**Cloud SQL Instance:** `satty-db` (34.123.139.43:5432, db=satty, user=satty-user)
**Gemini Model:** `gemini-2.5-flash` (via OpenAI-compat endpoint)
**Sidecar Build:** All packages compile cleanly (`go build ./...`)

---

## Summary

| Suite | Tests | Passed | Failed | Skipped | Duration |
|-------|-------|--------|--------|---------|----------|
| `sidecar` (MCP/SSE/Prometheus) | 11 | 11 | 0 | 0 | ~1.0s |
| `internal/analyzer` (rules engine) | 5 | 5 | 0 | 0 | ~0.5s |
| `internal/llm` (client + index optimizer) | 11 | 11 | 0 | 0 | ~21.8s |
| **Total** | **27** | **27** | **0** | **0** | **~23.3s** |

---

## Sidecar Integration Tests (Cloud SQL)

Ran against live Cloud SQL instance with `SAGE_DATABASE_URL` set.

| # | Test | Duration | Notes |
|---|------|----------|-------|
| 1 | `TestSSEConnection` | 0.21s | SSE handshake over Cloud SQL pool |
| 2 | `TestMCPInitialize` | <0.01s | MCP protocol init |
| 3 | `TestResourcesList` | <0.01s | Lists sage:// resources |
| 4 | `TestToolsList` | <0.01s | Lists MCP tools |
| 5 | `TestPromptsList` | <0.01s | Lists MCP prompts |
| 6 | `TestResourcesReadHealth` | 0.22s | Reads sage://health, validates JSON |
| 7 | `TestResourcesReadFindings` | <0.01s | Reads sage://findings |
| 8 | `TestToolsCallBriefing` | <0.01s | Calls briefing tool via MCP |
| 9 | `TestRateLimiting` | 0.06s | Verifies rate limiter blocks excess requests |
| 10 | `TestInvalidSession` | <0.01s | Rejects invalid session IDs |
| 11 | `TestPrometheusMetrics` | 0.40s | /metrics endpoint returns valid Prometheus output |

---

## Analyzer Rules Unit Tests

Pure unit tests with fixture data, no DB required.

| # | Test | Notes |
|---|------|-------|
| 1 | `TestRuleTableBloat_MinRows` | Skips tiny tables below min_rows threshold |
| 2 | `TestRuleHighPlanTime` | Detects mean_plan_time > mean_exec_time × ratio |
| 3 | `TestRuleQueryRegression_ResetDetection` | Skips regression analysis after pg_stat_statements reset |
| 4 | `TestRuleQueryRegression_RealRegression` | Flags real performance regression from sampled history |
| 5 | `TestRuleSlowQueries` | Flags queries exceeding slow_query_threshold_ms |

---

## LLM Client Unit Tests (Mock Server)

All use `httptest.NewServer` mock — no real Gemini API calls.

| # | Test | Duration | Notes |
|---|------|----------|-------|
| 1 | `TestChat_Success` | <0.01s | Happy path: content + token parsing |
| 2 | `TestChat_BudgetExhausted` | <0.01s | Rejects when daily token budget exceeded |
| 3 | `TestChat_CircuitBreaker` | <0.01s | Opens after 3 failures, blocks subsequent calls |
| 4 | `TestChat_EmptyChoices` | <0.01s | Handles `{"choices":[]}` without crash |
| 5 | `TestChat_ServerError` | 21.0s | Retries on HTTP 500 with exponential backoff (1s+4s+16s) |

---

## LLM Index Optimizer Unit Tests (Mock Server)

| # | Test | Duration | Notes |
|---|------|----------|-------|
| 6 | `TestOptimizer_AnalyzeSuccess` | <0.01s | Returns 1 index recommendation with token count |
| 7 | `TestOptimizer_SkipWriteHeavy` | <0.01s | Rejects tables with write ratio >70% |
| 8 | `TestOptimizer_SkipOverIndexed` | <0.01s | Skips tables with ≥10 existing indexes |
| 9 | `TestParseRecommendations_WithFences` | <0.01s | Strips markdown code fences from LLM JSON |
| 10 | `TestParseRecommendations_EmptyArray` | <0.01s | Parses `[]` without error |
| 11 | `TestValidateRecommendation_NoConcurrently` | <0.01s | Rejects DDL missing CONCURRENTLY keyword |

---

## Gemini API Verification

```
Endpoint:  https://generativelanguage.googleapis.com/v1beta/openai/chat/completions
Model:     gemini-2.5-flash
Auth:      Bearer token (API key)
Status:    ✅ Working — returns valid OpenAI-format response with usage.total_tokens
```

**Delisted models (404):**
- `gemini-2.5-flash-preview` — "not found for API version v1main"
- `gemini-2.0-flash` — "no longer available to new users"

---

## Fixes Applied During Testing

### 1. Pool MaxConns (sidecar_test.go:36)

- **Problem:** `MaxConns=3` caused "connection pool exhausted" errors on 3 tests
  (`TestResourcesReadHealth`, `TestResourcesReadFindings`, `TestToolsCallBriefing`)
- **Root cause:** SSE session goroutine + handler queries competed for 3 connections
  against Cloud SQL (higher latency than localhost Docker)
- **Fix:** Bumped `poolCfg.MaxConns` from 3 → 5

### 2. Gemini Model Name (sidecar/tasks/deploy_config.yaml:38)

- **Problem:** `gemini-2.5-flash-preview` returns HTTP 404
- **Fix:** Updated to `gemini-2.5-flash`

---

## Infrastructure Notes

| Item | Detail |
|------|--------|
| Cloud SQL state at session start | STOPPED (activation policy: NEVER) |
| Started via | `gcloud sql instances patch --activation-policy=ALWAYS` |
| Time to RUNNABLE | ~5 minutes |
| IP allowlist | 75.8.104.202/32 (confirmed current) |
| SSL mode | ALLOW_UNENCRYPTED_AND_ENCRYPTED |
| **Current state** | **RUNNING — set back to NEVER when done to stop billing** |

---

## Packages Without Tests

| Package | Status |
|---------|--------|
| `cmd/pg_sage_sidecar` | Entry point only — no tests needed |
| `internal/briefing` | No test files |
| `internal/collector` | No test files |
| `internal/config` | No test files |
| `internal/executor` | No test files |
| `internal/ha` | No test files |
| `internal/retention` | No test files |
| `internal/schema` | No test files |
| `internal/startup` | No test files |

---

## Not Yet Implemented

The `cloudsql_claude.md` spec describes integration tests with `//go:build integration` tag:
- `TestFullLLMCycleGemini` — full collect→analyze→LLM optimize→execute→findings→MCP→Prometheus
- `TestFullCycleNoLLM` — same without LLM, Tier 1 only

These test files do not exist yet.
