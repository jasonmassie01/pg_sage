# pg_sage V2 Test Report

**Date:** 2026-03-22
**Version:** 0.5.0
**Environments:** Docker (postgres:17-bookworm) + Cloud SQL (satty-db)
**Sidecar:** Go 1.23, pgx/v5
**Extension:** C, PG17, libcurl linked
**LLM:** Gemini 2.5 Flash via OpenAI-compatible endpoint

---

## Executive Summary

| Area | Tests | Pass | Fail | Notes |
|------|-------|------|------|-------|
| Sidecar unit tests | 55 | 55 | 0 | 6 packages, all green |
| Extension (9 phases) | 9 | 6 | 3 | 3 known bugs confirmed |
| Tier 3 executor | 7 | 5 | 2 | 4 new bugs found |
| **Total** | **71** | **66** | **5** | **9 bugs total (2 critical, 7 medium)** |

---

## Part 1: Sidecar Go Unit Tests

### Test Run Summary

```
ok  github.com/pg-sage/sidecar               0.457s   (11 tests)
ok  github.com/pg-sage/sidecar/internal/analyzer   0.676s   (5 tests)
ok  github.com/pg-sage/sidecar/internal/collector  0.669s   (11 tests) ← NEW
ok  github.com/pg-sage/sidecar/internal/config     0.580s   (9 tests)  ← NEW
ok  github.com/pg-sage/sidecar/internal/executor   0.676s   (4 tests)  ← NEW
ok  github.com/pg-sage/sidecar/internal/llm       24.932s   (15 tests, 4 NEW)
```

**55 tests across 6 packages — ALL PASS.**

### Package Coverage Detail

#### `sidecar` (root) — 11 tests (existing, v1)

MCP, SSE, and Prometheus integration tests against live PostgreSQL.

| Test | What it verifies |
|------|------------------|
| TestResourcesList | MCP resource listing |
| TestResourcesReadHealth | Health JSON via MCP |
| TestResourcesReadFindings | Findings JSON via MCP |
| TestResourcesReadSnapshots | Snapshot data via MCP |
| TestResourcesReadConfig | Config table via MCP |
| TestResourcesReadStatus | Status JSON via MCP |
| TestToolsList | MCP tool listing |
| TestToolsCallBriefing | Briefing generation |
| TestSSEEndpoint | SSE event stream |
| TestSSEReconnect | SSE reconnection |
| TestPrometheusMetrics | Prometheus /metrics endpoint |

#### `internal/analyzer` — 5 tests (existing, v1)

| Test | What it verifies |
|------|------------------|
| TestTableBloatMinRows | Bloat skips tables below threshold |
| TestHighPlanTime | Plan time detection |
| TestResetDetection | pg_stat_statements reset handling |
| TestRealRegression | Query regression detection |
| TestSlowQueries | Slow query threshold |

#### `internal/collector` — 11 tests, 17 subtests (NEW)

| Test | Subtests | What it verifies |
|------|----------|------------------|
| TestQueryStatsSQL_SelectsCorrectVariant | 5 | Correct SQL variant for WAL/plan_time combos; all include `queryid IS NOT NULL` |
| TestSystemStatsSQL_PG17Checkpointer | — | PG17 uses `pg_stat_checkpointer` not `pg_stat_bgwriter` |
| TestSystemStatsSQL_PG14Baseline | — | PG14 uses `pg_stat_bgwriter` not `pg_stat_checkpointer` |
| TestTableStatsSQL_SchemaExclusion | — | Filters sage, pg_catalog, information_schema |
| TestIndexStatsSQL_SchemaExclusion | — | Same schema exclusion |
| TestForeignKeysSQL_SchemaExclusion | — | Same schema exclusion |
| TestPartitionSQL_SchemaExclusion | — | Same schema exclusion |
| TestCircuitBreaker_SkipOnHighLoad | — | Fresh breaker not dormant |
| TestCircuitBreaker_DormantRecovery | — | 3 consecutive successes exit dormant |
| TestSnapshotCategories_PersistMap | — | JSON includes all 11 category fields |
| TestCoalesceInSQL | — | COALESCE present in all nullable-column queries |

#### `internal/config` — 9 tests (NEW)

| Test | What it verifies |
|------|------------------|
| TestConfigDefaults | All defaults match spec (mode=extension, port=5432, etc.) |
| TestConfigPrecedence_CLIOverEnv | CLI `--pg-host` overrides `SAGE_PG_HOST` env |
| TestConfigPrecedence_DatabaseURL | `SAGE_DATABASE_URL` overrides individual pg fields |
| TestConfigValidation_InvalidTrustLevel | Rejects `trust.level: "invalid"` |
| TestConfigValidation_ZeroCollectorInterval | Rejects `collector.interval_seconds: 0` |
| TestConfigValidation_InvalidMode | Rejects `--mode=bogus` |
| TestDSN_BuildsLibpq | Builds libpq string; returns DatabaseURL when set |
| TestApplyHotReload | Hot-reloads collector interval, reports change |
| TestApplyHotReload_PostgresNotChanged | Postgres.Host NOT hot-reloadable |

#### `internal/executor` — 4 tests, 27 subtests (NEW)

| Test | Subtests | What it verifies |
|------|----------|------------------|
| TestShouldExecute_AllCombinations | 12 | Full trust gate matrix: level x risk x ramp x window x emergency x replica |
| TestNeedsConcurrently | 5 | Detects CONCURRENTLY keyword (case-insensitive) |
| TestCategorizeAction | 8 | SQL → action_type mapping (create_index, vacuum, etc.) |
| TestNilIfEmpty | 2 | Empty string → nil, non-empty → pointer |

**Key trust gate validations:**
- `observation` + any risk → false
- `advisory` + any risk → false
- `autonomous` + safe + ramp<8d → false
- `autonomous` + safe + ramp>8d + Tier3Safe=true → **true**
- `autonomous` + moderate + ramp>31d + window + Tier3Moderate=true → **true**
- `autonomous` + high_risk → always false
- `autonomous` + emergencyStop=true → false
- `autonomous` + isReplica=true → false

#### `internal/llm` — 15 tests (11 existing + 4 NEW)

**Existing (v1):**

| Test | Duration | What it verifies |
|------|----------|------------------|
| TestChat_Success | 0s | Happy path: content + token parsing |
| TestChat_BudgetExhausted | 0s | Daily token budget enforcement |
| TestChat_CircuitBreaker | 0s | 3 failures → circuit open |
| TestChat_EmptyChoices | 0s | Empty choices array handled |
| TestChat_ServerError | ~21s | Exponential backoff (1s+4s+16s) |
| TestOptimizer_AnalyzeSuccess | 0s | 1 recommendation + token count |
| TestOptimizer_SkipWriteHeavy | 0s | Write ratio >70% → skip |
| TestOptimizer_SkipOverIndexed | 0s | >=10 indexes → skip |
| TestParseRecommendations_WithFences | 0s | Strips markdown fences |
| TestParseRecommendations_EmptyArray | 0s | [] parses clean |
| TestValidateRecommendation_NoConcurrently | 0s | Rejects missing CONCURRENTLY |

**New (v2):**

| Test | Duration | What it verifies |
|------|----------|------------------|
| TestChat_Timeout | ~3s | Mock hangs, client respects HTTP timeout |
| TestChat_GarbageResponse | 0s | Non-JSON 200 → unmarshal error, no crash |
| TestChat_Non200 | 0s | 401 response → error with status code |
| TestChat_LargeResponse | 0s | >1MB body safely truncated by LimitReader |

---

## Part 2: Extension Docker Tests (9 Phases)

**Container:** `pg_sage_test` | PG17 | trust_level=autonomous
**Test data:** 10K customers, 1K products, 100K orders, duplicate indexes, exhausted sequence

### Phase Results

| Phase | Test | Verdict | Details |
|-------|------|---------|---------|
| 1 | Extension Health Check | **PASS** | 3 workers running, circuits closed, v0.5.0 |
| 2 | Snapshot Categories | **FAIL** | 5 categories (indexes, sage_health, sequences, system, tables). **"queries" missing** |
| 3 | Findings Summary | **PASS** | 13 findings across 8 categories (4 critical, 7 warning, 2 info) |
| 4 | Auto-explain Cache | **PASS** | 96 plans captured in `sage.explain_cache` with valid JSONB |
| 5 | Trust Level & Executor | **PASS** | Autonomous mode, 1 duplicate index auto-dropped |
| 6 | GUC Count | **PASS** | Exactly 44 GUCs registered |
| 7 | Advisory Lock | **PASS** | Key 483722657 held with ExclusiveLock |
| 8 | API Key Security | **FAIL** | `sage.llm_api_key` visible to non-superuser via SHOW |
| 9 | Empty trust_level | **FAIL** | `ALTER SYSTEM SET sage.trust_level = ''` accepted |

### Phase 1: Health Check — PASS

```json
{
  "enabled": true, "version": "0.5.0",
  "workers": {"analyzer": true, "briefing": true, "collector": true},
  "connections": {"max": 100, "total": 9, "active": 1},
  "circuit_state": "closed",
  "llm_circuit_state": "closed",
  "cache_hit_ratio_pct": 97.82
}
```

### Phase 2: Snapshot Categories — FAIL

```
 category    | count
-------------+------
 indexes     |    51
 sage_health |    51
 sequences   |    51
 system      |    51
 tables      |    51
```

**"queries" category absent** despite pg_stat_statements having 300+ rows.
Root cause unknown — SPI context may be silently swallowing errors in the collector
background worker when querying pg_stat_statements.

### Phase 3: Findings — PASS

13 findings detected:

| Category | Severity | Title |
|----------|----------|-------|
| config | info | max_connections exceeds peak usage |
| config | warning | shared_buffers below 25% RAM |
| config | warning | random_page_cost at HDD default (4.0) |
| config | critical | Cache hit ratio 0.0% (below 99%) |
| duplicate_index | critical | idx_orders_duplicate2 matches idx_orders_duplicate1 |
| index_write_penalty | warning | idx_orders_unused (100K mutations vs 1 scan) |
| index_write_penalty | warning | idx_test_unused_lineitem (110K mutations vs 1 scan) |
| security_missing_rls | warning | customers has sensitive columns, no RLS |
| sequence_exhaustion | critical | test_exhausted_seq at 93.1% (integer) |
| slow_query | critical | 11,916 ms mean |
| unused_index | warning | orders_pkey (zero scans in 30 days) |
| vacuum_bloat_dead_tuples | warning | 20.0% dead tuples on orders |
| vacuum_bloat_dead_tuples | warning | 11.8% dead tuples on sage.config |

### Phase 4: Auto-explain — PASS

96 plans captured via GENERIC_PLAN. Schema uses `plan_json` (jsonb), not `plan_text`.
Sample plans include JIT optimization info, nested Plan nodes with costs and row estimates.

### Phase 5: Trust Level & Executor — PASS

- Trust level: autonomous
- 1 action executed: `DROP INDEX public.idx_orders_duplicate2` → outcome=success
- Rollback SQL captured: `CREATE INDEX idx_orders_duplicate2 ON public.orders USING btree (customer_id)`
- Before/after state recorded (idx_scan, mean_query_latency_ms)

### Phase 6: GUC Audit — PASS

44 GUCs total: 2 postmaster, 42 sighup. All match spec.

### Phase 7: Advisory Lock — PASS

Lock key `483722657` held with ExclusiveLock, granted=true.
**Note:** Sidecar uses `hashtext('pg_sage')=710190109` — key mismatch (BUG-1).

### Phase 8: API Key Security — FAIL (BUG-2)

Non-superuser `test_viewer` with `pg_monitor` membership can read:
```
SHOW sage.llm_api_key → <REDACTED>
```

### Phase 9: Empty trust_level — FAIL (BUG-4)

```sql
ALTER SYSTEM SET sage.trust_level = '';  -- SUCCEEDS (should be rejected)
```

---

## Part 3: Tier 3 Executor Deep Test

**Setup:** trust_ramp_start backdated 32 days (required adding `sage.set_trust_ramp_start()` C function).

### Results

| Test | Verdict | Details |
|------|---------|---------|
| SAFE action execution | **PASS** | Dropped duplicate index, logged to action_log |
| MODERATE action gating | **PASS** | Blocked by maintenance window |
| HIGH risk gating | **PASS** | Always skipped (sequence_exhaustion, RLS) |
| Emergency stop/resume | **PASS** | Both toggle correctly |
| CONCURRENTLY stripping | **PASS** | New fix: strips keyword before SPI execution |
| PK index flagged as unused | **FAIL** | orders_pkey recommended for DROP (BUG-7) |
| SPI error handling | **FAIL** | No PG_TRY/PG_CATCH — failed DROP aborts SPI (BUG-6) |

### Tier 3 Action Log

```
id | action_type      | outcome | sql_executed                               | executed_at
 1 | duplicate_index  | success | DROP INDEX public.idx_orders_duplicate2;    | 2026-03-22 23:40:58
```

- Rollback SQL: `CREATE INDEX idx_orders_duplicate2 ON public.orders USING btree (customer_id);`
- Before state: `idx_scan=0, mean_query_latency_ms=2.38`
- After state: `regression_pct=-100%, mean_query_latency_ms=0.0`

### Risk Gating Log Evidence

```
executor: skipping HIGH risk action for finding 8 (sequence_exhaustion)
executor: skipping MODERATE action for finding 3 — not in maintenance window
executor: skipping HIGH risk action for finding 5 (security_missing_rls)
executor: executing SAFE action for finding 7 (duplicate_index)
executor: rewrote SQL (stripped CONCURRENTLY): DROP INDEX public.idx_orders_duplicate2;
executor: successfully executed SAFE action for finding 7
```

### PK Index Bug (BUG-7)

After the duplicate index was dropped, the executor attempted:
```
DROP INDEX CONCURRENTLY public.orders_pkey;
```
This fails because `orders_pkey` is owned by the PRIMARY KEY constraint. The SPI ERROR
aborts the transaction context, preventing failure logging. The finding is never marked
`acted_on_at`, causing infinite retry every cycle.

---

## Part 4: Code Changes Made During Testing

### `src/action_executor.c` — CONCURRENTLY stripping

Added logic to strip `CONCURRENTLY` keyword from DDL before SPI execution.
SPI runs inside a transaction where CONCURRENTLY is illegal.

```
Log: "rewrote SQL (stripped CONCURRENTLY): DROP INDEX public.idx_orders_duplicate2;"
```

### `src/pg_sage.c` — `sage.set_trust_ramp_start(timestamptz)`

Added test helper function to override `sage_state->trust_ramp_start` in shared memory.
Enables Tier 3 testing without waiting 8+ real days.

### `sql/pg_sage--0.5.0.sql` — SQL declaration

```sql
CREATE FUNCTION sage.set_trust_ramp_start(timestamptz) RETURNS boolean ...
```

### New Test Files

| File | Tests | Package |
|------|-------|---------|
| `sidecar/internal/collector/collector_test.go` | 11 (17 subtests) | collector |
| `sidecar/internal/executor/executor_test.go` | 4 (27 subtests) | executor |
| `sidecar/internal/config/config_test.go` | 9 | config |
| `sidecar/internal/llm/client_extra_test.go` | 4 | llm |

---

## Bug Tracker

### CRITICAL

| # | Bug | Component | Impact | Fix |
|---|-----|-----------|--------|-----|
| BUG-1 | Advisory lock key mismatch | ha.c / bootstrap.go | Extension (483722657) and sidecar (710190109) can run simultaneously | Align both to `hashtext('pg_sage')` |
| BUG-2 | API key visible to non-superusers | guc.c | Any LOGIN user can `SHOW sage.llm_api_key` | Use PGC_SUSET or GUC_SUPERUSER_ONLY (PG15+) |

### MEDIUM

| # | Bug | Component | Impact | Fix |
|---|-----|-----------|--------|-----|
| BUG-3 | "queries" snapshots never collected | collector.c | Slow query detection is blind | Debug SPI context in collector background worker |
| BUG-4 | trust_level accepts empty string | guc.c check_hook | Undefined executor behavior | Add `strlen(newval) == 0` check |
| BUG-5 | trust_ramp_start not persisted | pg_sage.c / ha.c | Resets on restart, untestable | Read/write from sage.config table |
| BUG-6 | No PG_TRY/PG_CATCH in executor SPI | action_executor.c | Failed DDL aborts SPI, no failure logging | Wrap SPI_execute in error handler |
| BUG-7 | PK indexes flagged as unused | analyzer.c | Executor tries to DROP primary keys | Add `indisprimary` exclusion to unused_index rule |
| BUG-8 | Failed actions retry infinitely | action_executor.c | Finding never marked acted_on on failure | Set acted_on_at even on failure, or add failure cooldown |
| BUG-9 | sage.config dead tuple bloat | analyzer.c | Flags sage schema table for action | Exclude sage schema from vacuum_bloat findings |

### LOW / NOTES

| # | Issue | Details |
|---|-------|---------|
| NOTE-1 | trust_level is case-sensitive | `'OBSERVATION'` rejected; PG convention is case-insensitive |
| NOTE-2 | sage.briefing() truncation | Truncates at ~150 chars (known from v1) |
| NOTE-3 | GENERIC_PLAN fails on complex $N params | INSERT with $12+ params → syntax error |
| NOTE-4 | 15 compiler warnings | 6 from pg_sage code, 9 from PG elog.h headers |

---

## Coverage Summary

### Sidecar Package Coverage

| Package | v1 Tests | v2 Tests | Total | Status |
|---------|----------|----------|-------|--------|
| `sidecar` (MCP/SSE/Prom) | 11 | 0 | 11 | Tested |
| `internal/analyzer` | 5 | 0 | 5 | Tested |
| `internal/llm` | 11 | 4 | 15 | Tested |
| `internal/collector` | 0 | 11 | 11 | **NEW** |
| `internal/config` | 0 | 9 | 9 | **NEW** |
| `internal/executor` | 0 | 4 | 4 | **NEW** |
| `internal/schema` | 0 | 0 | 0 | Needs testcontainers |
| `internal/startup` | 0 | 0 | 0 | Needs testcontainers |
| `internal/briefing` | 0 | 0 | 0 | Needs live DB |
| `internal/ha` | 0 | 0 | 0 | Needs live DB |
| `internal/retention` | 0 | 0 | 0 | Needs live DB |
| Integration (e2e) | 0 | 0 | 0 | Not implemented |
| **Total** | **27** | **28** | **55** | **6/12 packages** |

### Extension Test Coverage

| Area | v1 | v2 | Status |
|------|----|----|--------|
| Build/load | PASS | PASS | Stable |
| Collector (5 categories) | PASS | PASS | Stable |
| Collector ("queries") | FAIL | FAIL | **BUG-3 open** |
| Analyzer (findings) | PASS | PASS | Stable (13 findings) |
| Auto-explain | PASS | PASS | Stable (96 plans) |
| LLM integration | PASS | PASS | Stable |
| Tier 3 execution (SAFE) | N/A | **PASS** | **NEW — action executed** |
| Tier 3 risk gating | PASS | PASS | Stable |
| Emergency stop | PASS | PASS | Stable |
| Advisory lock | PASS | PASS | Key confirmed (mismatch with sidecar) |
| GUC audit (44) | PASS | PASS | Stable |
| API key security | FAIL | FAIL | **BUG-2 open** |
| Empty trust_level | FAIL | FAIL | **BUG-4 open** |
| PK index exclusion | N/A | FAIL | **BUG-7 new** |
| SPI error handling | N/A | FAIL | **BUG-6 new** |

---

## Recommended Fix Priority

### Iteration 1 (Security + Data Integrity)

1. **BUG-2**: API key visibility — change GUC to PGC_SUSET or add show_hook masking
2. **BUG-1**: Advisory lock alignment — change extension to use `hashtext('pg_sage')`
3. **BUG-7**: PK index exclusion — add `AND NOT ix.indisprimary` to unused_index query

### Iteration 2 (Executor Reliability)

4. **BUG-6**: PG_TRY/PG_CATCH in executor SPI — prevent ERROR from aborting action logging
5. **BUG-8**: Failed action retry loop — set acted_on_at on failure or add cooldown
6. **BUG-4**: Empty trust_level — add length check in check_hook

### Iteration 3 (Collector + Persistence)

7. **BUG-3**: "queries" snapshot — debug collector SPI context for pg_stat_statements
8. **BUG-5**: trust_ramp_start persistence — read from sage.config table at init
9. **BUG-9**: sage.config bloat finding — exclude sage schema from vacuum analysis

---

## Test Artifacts

| File | Description |
|------|-------------|
| `V2_TEST_REPORT.md` | This report |
| `EXT_TEST_REPORT.md` | v1 extension test report |
| `TEST_REPORT.md` | v1 sidecar cloud test report |
| `tasks/ext_test_v2_output.txt` | Extension phase test raw output |
| `tasks/tier3_output.txt` | Tier 3 execution test raw output |
| `tasks/coexistence_output.txt` | Advisory lock coexistence test |
| `tasks/llm_test_output.txt` | LLM integration test output |
| `tasks/guc_audit_output.txt` | GUC audit raw output |
| `tasks/diagnostics_output.txt` | Diagnostics 1-5 output |
| `tasks/autoexplain_output.txt` | Auto-explain test output |

---

## Infrastructure State

- **Docker:** `pg_sage_test` container running (PG17 + extension, autonomous mode)
- **Cloud SQL:** `satty-db` instance RUNNING (activation-policy=ALWAYS)
- **Gemini:** API key confirmed working with `gemini-2.5-flash`
- **Port 5432:** Mapped to Docker container (conflicts with local PG if started)
