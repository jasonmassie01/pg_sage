# pg_sage C Extension — Self-Managed Test Report

**Date:** 2026-03-22
**Version:** 0.5.0
**Environment:** Docker (postgres:17-bookworm), PG17, libcurl linked
**Build:** Zero errors, 15 compiler warnings (see Appendix A)

---

## Executive Summary

| Area | Result | Notes |
|------|--------|-------|
| Build | PASS (warnings) | 15 warnings — 6 from pg_sage code, 9 from PG headers |
| Extension load | PASS | Workers start, advisory lock acquired |
| Collector | PARTIAL | 5/6 snapshot categories active, **"queries" missing** |
| Analyzer | PASS | 9 findings across 5 categories |
| Auto-explain | PASS | 54 plans captured via GENERIC_PLAN, sage.explain() works |
| LLM integration | PASS | libcurl linked, 9 GUCs, Gemini configured, explain narration works |
| Tier 3 executor | PARTIAL | Executor runs, gating logic correct, **cannot backdate trust_ramp** |
| Advisory lock | FAIL | **Key mismatch between extension and sidecar** |
| GUC audit | PARTIAL | 44 GUCs, check_hook **accepts empty string** (bug) |
| API key security | FAIL | **sage.llm_api_key visible to non-superusers** |

**Critical bugs: 2** | **Medium bugs: 3** | **Low/Notes: 4**

---

## Phase-by-Phase Results

### Phase 0: Infrastructure

- PG17 container built and started successfully
- Extension loaded via `shared_preload_libraries`
- 3 background workers confirmed: collector, analyzer, briefing
- Test data: 10K customers, 1K products, 100K orders, duplicate indexes, exhausted sequence
- pg_stat_statements loaded with 1096 entries

### Diagnostics (1-5)

| # | Diagnostic | Result |
|---|-----------|--------|
| 1 | Sage schema self-analysis | **PASS** — zero sage.* objects in findings (excluding sage_health) |
| 2 | Snapshot categories | **PARTIAL** — 5 categories (indexes, sage_health, sequences, system, tables). **"queries" category absent** despite pg_stat_statements having 304 matching rows |
| 3 | Slow query detection | **N/A** — no query snapshots means no slow queries detected |
| 4 | Schema design findings | **PASS** — zero schema_design findings (no noise issue in this workload) |
| 5 | LLM feature existence | **PASS** — 9 LLM GUCs registered, disabled by default |

### Phase 4b: Auto-Explain

| Test | Result |
|------|--------|
| `sage.autoexplain_enabled` GUC exists | PASS |
| `sage.autoexplain_min_duration_ms` GUC exists | PASS |
| `sage.autoexplain_capture_window` GUC exists | PASS (bonus — not in spec) |
| `sage.autoexplain_sample_rate` GUC exists | PASS (bonus — not in spec) |
| Plans captured in `sage.explain_cache` | PASS — **54 plans** captured via GENERIC_PLAN |
| Plans contain real EXPLAIN output (nodes, costs) | PASS — JSON format with Plan nodes |
| `sage.explain(queryid)` returns LLM-narrated explanation | PASS — Gemini generates Markdown analysis |
| pg_stat_statements coexistence | PASS — 1096 entries, no hook conflict |
| Parameterized query handling | PARTIAL — GENERIC_PLAN works for most, fails on complex INSERT with $12+ params |
| Default sample_rate = 0.1 | NOTE — captures are probabilistic; set to 1.0 for deterministic testing |

### Phase 7b: Tier 3 Execution

| Test | Result |
|------|--------|
| `sage.trust_level = 'autonomous'` accepted | PASS |
| Executor finds actionable findings | PASS — finds 5 actionable findings per cycle |
| Risk classification correct | PASS — SAFE/MODERATE/HIGH correctly classified |
| SAFE gate (trust >= ADVISORY, day >= 8) | PASS — correctly skipped (day=0) |
| MODERATE gate (trust >= AUTONOMOUS, day >= 31) | PASS — correctly skipped (day=0) |
| HIGH gate (never executed) | PASS — sequence_exhaustion and RLS correctly blocked |
| Emergency stop | PASS — `sage.emergency_stop()` and `sage.resume()` work |
| Actions in action_log | **NO ACTIONS** — trust_day=0, all gated |

**Root cause for no actions:** `sage_state->trust_ramp_start` is set in shared memory at
`_PG_init()` to `GetCurrentTimestamp()`. It is never read from the `sage.config` table.
There is no way to backdate it without modifying C code or waiting 8+ days.

**Recommendation:** Add a `sage.trust_ramp_override_days` GUC (PGC_SUSET) that, when set,
overrides the computed trust_day. This enables testing and allows operators to fast-track
trust after verified observation periods.

### Phase 12: LLM Integration

| Test | Result |
|------|--------|
| LLM GUCs exist (9 total) | PASS |
| libcurl linked to pg_sage.so | PASS |
| LLM C source code exists (src/llm.c) | PASS |
| Configure Gemini via ALTER SYSTEM | PASS |
| LLM circuit_state = closed | PASS |
| sage.explain() uses LLM narration | **PASS** — Gemini generates Markdown explain analysis |
| LLM-enhanced findings (auto-annotated) | NOT APPLICABLE — LLM is on-demand, not auto-injection |
| LLM features: briefing, explain, diagnostic, shell | PASS — all 4 registered |
| API key visible to non-superuser | **FAIL — SECURITY ISSUE** |

**LLM Features confirmed working:**
- `sage.explain(queryid)` — calls Gemini to narrate EXPLAIN plans
- `sage.briefing()` — generates LLM health summaries (known truncation bug from prior test)
- `sage.diagnose()` — ReAct diagnostic loop via LLM (confirmed in prior test)

### Phase 13: Extension + Sidecar Coexistence

| Test | Result |
|------|--------|
| Extension holds advisory lock | PASS — ExclusiveLock on key 483722657 |
| Sidecar blocked by extension lock | **FAIL — CRITICAL BUG** |
| Schema compatibility | PASS — all 7 tables identical |

**Root cause:** Advisory lock key mismatch:
- Extension uses hardcoded: `483722657`
- Sidecar uses: `hashtext('pg_sage')` = `710190109`
- They are different keys — both can run simultaneously without mutual exclusion

### Phase 14: GUC Audit

**Total GUCs: 44** (2 postmaster, 42 sighup)

| Category | Count |
|----------|-------|
| Core (enabled, database, trust) | 3 |
| Collector | 3 |
| Analyzer/thresholds | 7 |
| Auto-explain | 4 |
| LLM | 9 |
| Executor/trust/rollback | 5 |
| Retention | 4 |
| Briefing/channels | 4 |
| Cloud/cost | 3 |
| Misc (max_schema_size, react_max_steps) | 2 |

**trust_level check_hook:**

| Input | Expected | Actual |
|-------|----------|--------|
| `'invalid'` | REJECT | REJECT — PASS |
| `''` (empty) | REJECT | **ACCEPT — BUG** |
| `'observation'` | ACCEPT | ACCEPT — PASS |
| `'advisory'` | ACCEPT | ACCEPT — PASS |
| `'autonomous'` | ACCEPT | ACCEPT — PASS |
| `'OBSERVATION'` | ACCEPT (convention) | **REJECT — case-sensitive** |

**Missing spec GUCs:**
- `sage.toast_bloat_min_rows` — NOT IMPLEMENTED
- `sage.schema_design_min_rows` — NOT IMPLEMENTED
- `sage.schema_design_min_columns` — NOT IMPLEMENTED

---

## Bugs Found

### CRITICAL

| # | Bug | Component | Details |
|---|-----|-----------|---------|
| BUG-1 | Advisory lock key mismatch | ha.c / sidecar bootstrap.go | Extension uses 483722657, sidecar uses hashtext('pg_sage')=710190109. Both can run simultaneously. Fix: align to same key. |
| BUG-2 | API key visible to non-superusers | guc.c (sage.llm_api_key) | Any user with LOGIN can `SHOW sage.llm_api_key` and see the plaintext key. Must use PGC_SUSET context or GUC_SUPERUSER_ONLY flag. |

### MEDIUM

| # | Bug | Component | Details |
|---|-----|-----------|---------|
| BUG-3 | "queries" snapshot category not collected | collector.c | pg_stat_statements has 304+ matching rows but no "queries" snapshots appear. PG_CATCH may be silently swallowing errors, or there's a timing/permissions issue in the background worker SPI context. |
| BUG-4 | trust_level check_hook accepts empty string | guc.c | `ALTER SYSTEM SET sage.trust_level = ''` succeeds. Empty string should be rejected. |
| BUG-5 | trust_ramp_start not readable from config table | pg_sage.c / ha.c | `sage_state->trust_ramp_start` is set once at _PG_init() and never updated from sage.config. Cannot be backdated for testing or operations. |

### LOW / NOTES

| # | Issue | Details |
|---|-------|---------|
| NOTE-1 | trust_level is case-sensitive | `'OBSERVATION'` rejected; PG convention is case-insensitive for enum-like GUCs |
| NOTE-2 | sage.briefing() output truncation | Known from prior test — truncates at ~150 chars |
| NOTE-3 | EXPLAIN GENERIC_PLAN fails on complex $N params | `INSERT ... VALUES ($1..$12)` causes syntax error — expected limitation |
| NOTE-4 | 15 compiler warnings | 6 from pg_sage code (unused vars, const qualifiers), 9 from PG elog.h headers (shadow warnings in nested PG_TRY) |

---

## Compiler Warnings (Appendix A)

**pg_sage code (6 warnings):**
- `src/pg_sage.c:388,408,428` — const qualifier discarded (3x)
- `src/circuit_breaker.c:159` — unused variable `ts_size`
- `src/ha.c:108` — unused variable `safe`
- `src/llm.c:125` — mixed declarations and code (ISO C90)
- `src/llm.c:225` — unused variable `today`
- `src/context.c:112` — unused variable `val`
- `src/briefing.c:341,1159` — unused variables `section`, `findings_text`
- `src/briefing.c:112` — unused function `spi_getval_alloc`
- `src/tier2_extra.c:75` — unused variable `cost_per_cpu_hour`

**PG header warnings (9 from elog.h):**
- Shadow warnings from nested PG_TRY/PG_CATCH macros — expected when nesting error handlers

---

## Definition of Done Checklist

### Bug fixes
- [x] Schema exclusion: zero sage.* objects in findings (except sage_health)
- [ ] SPI error handling: NOT TESTED (would need rapid DROP/CREATE cycling)
- [x] trust_level check_hook: rejects 'invalid', accepts valid values
- [ ] trust_level check_hook: **accepts empty string** (BUG-4)
- [ ] trust_level case sensitivity: **case-sensitive, not PG convention** (NOTE-1)

### Auto-explain
- [x] `sage.autoexplain_enabled = on` → slow queries captured
- [x] Captured plans contain real EXPLAIN output (JSON with Plan nodes, costs)
- [x] `sage.explain(queryid)` returns LLM-narrated content
- [x] Auto-explain doesn't break pg_stat_statements

### Tier 3 execution
- [x] Executor runs and finds actionable findings
- [x] Risk classification correct (SAFE/MODERATE/HIGH)
- [x] Trust gating logic correct
- [ ] **No actual actions executed** (trust_day=0, cannot backdate)
- [x] Emergency stop/resume works

### LLM integration
- [x] LLM exists in C extension with libcurl
- [x] 9 GUCs registered and configurable
- [x] Gemini endpoint works (explain narration confirmed)
- [x] Circuit breaker state tracked (closed = healthy)
- [ ] **API key visible to non-superusers** (BUG-2)
- [ ] curl_global_init() location not verified (would need code audit)

### Extension + sidecar coexistence
- [ ] **Advisory lock key mismatch** (BUG-1)
- [x] Schema compatible (all 7 tables identical)
- [ ] Mutual exclusion NOT working

### GUC audit
- [x] 44 GUCs registered with correct types and defaults
- [ ] toast_bloat_min_rows NOT IMPLEMENTED
- [ ] schema_design_min_rows/min_columns NOT IMPLEMENTED

### Build
- [x] Builds on PG17
- [ ] 15 compiler warnings (6 fixable in pg_sage code)
- [ ] Cross-version testing (PG14-16) NOT PERFORMED (single Dockerfile)

---

## Recommendations

1. **Fix BUG-1 immediately** — advisory lock mismatch allows dual operation, potential data corruption
2. **Fix BUG-2 before any production use** — API key leak is a security vulnerability
3. **Investigate BUG-3** — missing "queries" snapshots means slow query detection is blind
4. **Add trust_ramp_override_days GUC** — enables testing without waiting 8+ days
5. **Fix compiler warnings** — unused variables are easy wins, const qualifiers prevent future bugs
6. **Add multi-version Dockerfile** — need PG14-16 test coverage
7. **Noise reduction GUCs** (toast_bloat_min_rows, schema_design_min_*) — implement per spec
