# Foundation Readiness Gate Spec

Date: 2026-04-30

## Goal

Make pg_sage safe enough for the next vector, JSON, extension, and
agent-native work by ensuring actionable recommendations carry enforceable risk
metadata and degraded advisory capabilities surface as findings instead of
silent no-ops.

## Mission Fit

This is not a new product surface. It strengthens the existing autonomous DBA
spine:

Observe -> Diagnose -> Decide -> Act -> Verify -> Remember

The feature exists to keep later autonomy honest. It should make pg_sage more
boring, explicit, and inspectable before it starts doing expensive work like
vector index tuning or high-risk DDL review.

## Current Problems

1. Optimizer recommendations have `ActionLevel` values such as `autonomous`,
   `advisory`, and `informational`, but analyzer findings put that value into
   `ActionRisk`. Executor trust gates expect `safe`, `moderate`, or
   `high_risk`, so the same recommendation can be visible but not policy-native.
2. The configuration advisor returns `(nil, nil)` when the advisor or LLM layer
   is disabled. Operators cannot distinguish "nothing to do" from "the advisor
   did not run."
3. Extension/capability-dependent advice is handled in scattered places. The
   optimizer validates `pg_trgm` and PostGIS for some index recommendations,
   and extension drift exists, but capability failures are not yet a unified
   first-class finding.

## Scope

### In This Slice

- Normalize optimizer action risk before findings reach executor trust gates.
- Preserve the existing `action_level` detail field for UI and explanation.
- Emit explicit degraded-mode findings when the config advisor is enabled but
  LLM is disabled or unavailable.
- Add tests that prove degraded-mode findings are specific and actionable.
- Add tests that prove optimizer findings cannot carry `advisory` or
  `informational` as executable risk tiers.
- Document the next capability-preflight work for extensions and vector/JSON
  advisors.

### Out Of Scope

- Full vector inventory or HNSW tuning.
- Full JSON/JSONB advisor.
- Full deploy-request system.
- Large API handler split.
- Deleting legacy `src/` C extension files.
- Autonomous HNSW rebuilds or shadow-index swaps.

## Design

### Risk Taxonomy

Action risk is the executor policy vocabulary:

- `safe`
- `moderate`
- `high_risk`

Action level is the recommendation confidence/exposure vocabulary:

- `autonomous`
- `advisory`
- `informational`

The two must not be stored in the same field. Optimizer index creation is a
moderate-risk action by default because it creates DDL, can consume disk and
I/O, and may need a maintenance window even with `CONCURRENTLY`.

Mapping for this slice:

| Source | Finding Detail | Finding ActionRisk |
|---|---|---|
| optimizer `autonomous` | `action_level=autonomous` | `moderate` |
| optimizer `advisory` | `action_level=advisory` | `moderate` |
| optimizer `informational` with SQL | `action_level=informational` | `moderate` |
| optimizer empty SQL | no executable action | empty risk |

The important invariant is that `ActionRisk` is always an executor risk tier,
never a UI confidence/action-level label.

### Degraded-Mode Findings

When the advisor is enabled but cannot run because LLM is disabled, emit an
`advisor_degraded` finding:

- Severity: `warning`
- Object type: `advisor`
- Object identifier: `llm`
- Recommendation: enable/configure the LLM provider or disable advisor features
  intentionally.
- No recommended SQL.
- Action risk: empty/read-only.

When advisor or LLM calls fail during a run, return findings that identify the
failed sub-advisor and reason where possible. Token budget exhaustion may remain
aggregated, but should still be visible as a finding.

## Testing Requirements

- Unit tests for risk normalization:
  - `advisory` maps to `moderate`
  - `autonomous` maps to `moderate`
  - `informational` maps to `moderate` when SQL exists
  - existing safe/moderate/high_risk values pass through
  - unknown values fail closed to `high_risk`
- Unit tests for analyzer optimizer mapping:
  - optimizer recommendation details retain `action_level`
  - finding `ActionRisk` is `moderate`
  - finding never stores `advisory` as risk
- Unit tests for advisor degradation:
  - advisor disabled remains quiet
  - advisor enabled + LLM disabled emits one `advisor_degraded` finding
  - finding has no executable SQL and no action risk

Integration/e2e/UI tests are not required for this slice because no API or UI
surface changes are introduced. Later deploy-request and capability dashboards
will require those tests.

