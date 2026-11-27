# Foundation Readiness Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make actionable recommendations policy-native by separating risk tier from action level, and make advisor degradation visible as findings.

**Architecture:** Keep the change narrow. Add pure helper functions where possible, wire them through the analyzer/advisor, and test behavior without requiring a real database. Avoid broad handler splits or unrelated roadmap work in this slice.

**Tech Stack:** Go, pgx, existing analyzer/advisor/optimizer packages, `go test -count=1`.

---

## File Structure

- Modify: `sidecar/internal/optimizer/types.go`
  - Add risk-tier constants and a pure mapping helper.
- Test: `sidecar/internal/optimizer/risk_test.go`
  - Unit coverage for risk normalization.
- Modify: `sidecar/internal/analyzer/analyzer.go`
  - Use optimizer risk helper when converting recommendations to findings.
- Test: `sidecar/internal/analyzer/optimizer_mapping_test.go`
  - Pure test for recommendation-to-finding behavior.
- Modify: `sidecar/internal/advisor/advisor.go`
  - Emit degraded-mode finding when advisor is enabled but LLM is disabled.
- Test: `sidecar/internal/advisor/advisor_test.go`
  - Extend tests for degraded-mode behavior.
- Modify: `tasks/todo.md`
  - Track this implementation.

---

### Task 1: Risk-Tier Normalization

**Files:**
- Modify: `sidecar/internal/optimizer/types.go`
- Create: `sidecar/internal/optimizer/risk_test.go`

- [ ] Step 1: Write failing tests for `RiskTierForRecommendation`.
- [ ] Step 2: Run `go test -count=1 ./internal/optimizer -run TestRiskTier`.
- [ ] Step 3: Add risk constants and mapping helper.
- [ ] Step 4: Re-run the targeted optimizer tests.

### Task 2: Analyzer Mapping

**Files:**
- Modify: `sidecar/internal/analyzer/analyzer.go`
- Create: `sidecar/internal/analyzer/optimizer_mapping_test.go`

- [ ] Step 1: Write failing test for recommendation-to-finding conversion.
- [ ] Step 2: Extract conversion helper if needed.
- [ ] Step 3: Use optimizer risk helper for `Finding.ActionRisk`.
- [ ] Step 4: Re-run targeted analyzer tests.

### Task 3: Advisor Degraded Finding

**Files:**
- Modify: `sidecar/internal/advisor/advisor.go`
- Modify: `sidecar/internal/advisor/advisor_test.go`

- [ ] Step 1: Update/add failing advisor tests for LLM-disabled degradation.
- [ ] Step 2: Add `advisorDegradedFinding` helper.
- [ ] Step 3: Return degraded finding only when advisor is enabled and LLM is disabled.
- [ ] Step 4: Re-run targeted advisor tests.

### Task 4: Verification

**Files:**
- Read: `AGENTS.md`, `tasks/todo.md`

- [ ] Step 1: Run targeted tests:
  - `go test -count=1 ./internal/optimizer -run TestRiskTier`
  - `go test -count=1 ./internal/analyzer -run TestOptimizerRecommendationToFinding`
  - `go test -count=1 ./internal/advisor -run 'TestAdvisor_Analyze_(Disabled|LLMDisabled)'`
- [ ] Step 2: Run package tests:
  - `go test -count=1 ./internal/optimizer ./internal/analyzer ./internal/advisor`
- [ ] Step 3: Run coverage for touched packages:
  - `go test -cover -count=1 ./internal/optimizer ./internal/analyzer ./internal/advisor`
- [ ] Step 4: Grep output for `SKIP`, `TODO`, and `PENDING`.
- [ ] Step 5: Record results using the repo test reporting format.

