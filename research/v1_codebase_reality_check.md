# pg_sage v1 Codebase Reality Check
**Date:** 2026-04-28  
**Repo:** C:\Users\jmass\pg_sage  
**Scope:** Feature completeness, tech debt, production-readiness

---

## Executive Summary

pg_sage is a **well-tested, partially-mature backend** with **significant production debt**. The core autonomy spine (Tier 1 rules, Tier 3 executor) is solid. The LLM tier and feature completeness have correctness gaps. Most critically: **the C extension (src/) is dead code** that will confuse future maintainers, the UI trails the backend in feature depth, and several "shipped" features are stubs or have broken assumption chains.

**Test count:** 237 test files (excellent).  
**Production lines (non-test):** 224 Go files across 30+ packages.  
**Lines of code violations:** 11 files exceed the 500-line CLAUDE.md hard limit. Largest: handlers.go (2105 lines).

---

## 1. Feature Reality Matrix

### Rules Engine (Tier 1)
Status: 5/5 - All 20+ deterministic checks implemented and tested
Files: sidecar/internal/analyzer/ (746 lines main)
Issue: Some rules hardcoded thresholds; no config override

### Index Optimizer (LLM)
Status: 3.5/5 - Works but confidence opaque without HypoPG
Files: sidecar/internal/optimizer/
Issue: Confidence threshold hardcoded at 0.5; HypoPG unavailability silent

### Config Advisors (6 LLM advisors)
Status: 3/5 - Silent failures when LLM unavailable
Files: sidecar/internal/advisor/ (vacuum, WAL, connections, memory, query rewrite, bloat)
Issue: Returns (nil, nil) without logging if LLM fails

### Health Briefings
Status: 2/5 - ReAct loop NOT IMPLEMENTED
Files: sidecar/internal/briefing/
Issue: Briefings are one-shot, not interactive. Marketing claim only.

### Trust-Ramped Executor
Status: 3.5/5 - Risk assigned RETROACTIVELY, not prospectively
Files: sidecar/internal/executor/ (930 lines)
CRITICAL: Advisors don't assess safety; executor guesses risk from SQL type

### Fleet Mode
Status: 3.5/5 - Architecture sound; quota enforcement underdocumented
Files: sidecar/internal/fleet/
Issue: Token budget interaction unclear

### Per-Query Tuner
Status: 2.5/5 - Assumes hint_plan extension; no validation
Files: sidecar/internal/tuner/ (1026 lines)
CRITICAL: Silent failure if pg_hint_plan not installed

### Workload Forecaster
Status: 2.5/5 - Predictions directional, not actionable
Files: sidecar/internal/forecaster/
Issue: Linear extrapolation only; no time-series model

### Alerting
Status: 3.5/5 - Slack/PagerDuty work; quiet hours MISSING
Files: sidecar/internal/notify/

### Dashboard + API
Status: 3.5/5 - 32 endpoints (more than claimed 17)
Files: sidecar/internal/api/
Issue: Orphan endpoints (shadow-report, growth-forecast); UI lags backend

### Prometheus Metrics
Status: 4.5/5 - Solid

---

## 2. Vector Search: 0/5 Maturity
- Zero vector-aware code
- Mentioned in research only as v1.x roadmap candidate
- No analyzer rules for vector indexes

---

## 3. Agent Integration: NOT IMPLEMENTED
- LLM provider layer (OpenAI-compatible) works fine
- sidecar does NOT know it's operated by Claude Desktop
- No MCP server in Go sidecar (README claims it; C extension had it)
- No callback mechanism to report findings to MCP client

---

## 4. REST API Orphan Endpoints
- GET /api/v1/shadow-report - registered but UI doesn't call it
- GET /api/v1/forecasts/growth - exists but forecasts page uses generic endpoint
- GET /api/v1/findings/stats - exists; purpose unclear vs. /findings

---

## 5. Tech Debt Hotspots

### Files Over 500-Line CLAUDE.md Hard Limit:
| File | Lines |
|------|-------|
| api/handlers.go | 2105 |
| config/config.go | 1041 |
| tuner/tuner.go | 1026 |
| executor/executor.go | 930 |
| api/action_handlers.go | 918 |

### Packages with Zero Test Coverage:
- ha/ (1 file) - HA logic untested
- smoke/ (1 file) - No unit tests

### Dead Code:
- **src/ directory (19 .c files) - COMPLETELY ABANDONED**
  - C extension from v0.x era
  - No compilation recipe
  - No CREATE EXTENSION in bootstrap
  - No tests
  - src/mcp_helpers.c exists (dead)
  
**Impact:** Future maintainers will assume src/ is part of product

- handlers_new.go (91 lines) - refactoring stub
- handlers_v09.go (600 lines) - v0.9-era endpoints still active but vestigial

### Dependency Creep:
- go.mod includes github.com/stretchr/testify (FORBIDDEN per CLAUDE.md)
- Used in migration/ and analyzer/ tests
- Violates test-suite style guidelines

---

## 6. Specification vs. Code Gaps

From autonomous-dba-product-spec-2026-04-27.md:

**Case unification:** Cases should unify findings, incidents, schema lint, query hints
- Gap: Not all features project to cases; full case semantics incomplete

**Operator feel:** Should "feel like an operator, not a metrics console"
- Gap: UI defaults to findings table, not case work queue; shadow mode visualization missing

**Risk assessment:** Should assess risk PROSPECTIVELY
- Gap: Advisors generate recommendations without safety assessment; executor retroactively assigns risk tier

**Action-centric:** Every feature must be action, evidence, verification, or policy control
- Gap: Forecasts and briefings are read-only; they're secondary features, not primary

---

## 7. Promised But Not Implemented

| Feature | Promised | Status |
|---------|----------|--------|
| ReAct loop for diagnostics | README | Stub only - not interactive |
| Shadow mode UI report | README | Endpoint exists; UI doesn't use it |
| MCP server in sidecar | v0.7 roadmap | NO - C extension had it; Go sidecar doesn't |
| Cost attribution | Roadmap | Not started |
| GitHub Actions DDL review | Roadmap | Not started |
| Historical trend analysis | Roadmap | Not started |
| Query plan diffing | Roadmap | Not started |

---

## 8. Foundation Risks Before v1 - Top 5

**Risk 1: API Handler Monolith (handlers.go 2105 lines)**
- Impact: Hard to test/change handler logic
- Effort: 2-3 days
- Priority: HIGH

**Risk 2: Risk Tier Assigned After Fact**
- Impact: Advisors don't assess safety; executor retroactively guesses risk tier
- A bad recommendation marked "safe" when should be "moderate" or "high"
- Effort: 3-5 days
- Priority: CRITICAL

**Risk 3: Query Hint Extension Not Validated**
- Impact: Silent failure if pg_hint_plan missing
- Effort: 1 day
- Priority: HIGH

**Risk 4: Dead C Code in src/**
- Impact: Confuses ownership; wastes future maintainer time
- Effort: 2 hours (delete or archive)
- Priority: MEDIUM

**Risk 5: LLM Failures Silent in Advisors**
- Impact: If LLM misconfigured, advisors return (nil, nil) without logging
- Operators see no findings; assume system is healthy
- Effort: 1 day
- Priority: HIGH

---

## Production-Readiness Score

Overall: 3.2/5 — **Suitable for supervised testing/early pilots, NOT for unattended production**

Component breakdown:
- Tier 1 Rules: 4.5/5 - solid
- Index Optimizer: 3.5/5 - confidence opaque
- Config Advisors: 3/5 - silent failures
- Health Briefings: 2/5 - stub
- Executor: 3.5/5 - risk assignment flawed
- Fleet Mode: 3.5/5 - unclear quota enforcement
- Query Tuner: 2.5/5 - extension not validated
- Forecaster: 2.5/5 - not actionable
- Alerting: 3.5/5 - quiet hours missing
- Dashboard: 3.5/5 - orphan endpoints
- Prometheus: 4.5/5 - solid

---

## Audit Summary

**Audit Date:** 2026-04-28  
**Scope:** Feature completeness, tech debt, production-readiness  
**Files Analyzed:** 224 production .go files, 237 test files, 30+ packages  
**Total Lines:** 141,529 (internal + cmd/web)  
**Test Coverage:** 237 test files (1:1 ratio; excellent)

