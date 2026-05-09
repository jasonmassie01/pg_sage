# Roadmap Slices Implementation Plan

Date: 2026-04-30

## Goal

Turn the pg_sage product research into small, testable implementation slices
that fit the autonomous DBA mission: observe, diagnose, decide, act, verify, and
remember.

## Slices

- Foundation readiness gate: separate action risk from recommendation level and
  make degraded advisory mode visible.
- JSON/JSONB Workload Advisor: add LLM prompt hints for containment, key
  existence, scalar extraction, JSONPath, and sort/group patterns.
- Vector Workload Advisor: identify ANN, filtered ANN, and missing LIMIT vector
  query shapes and feed those hints into the optimizer prompt.
- Agent Database Guard: identify agent-created or agent-driven database traffic
  that lacks stable attribution before pg_sage makes autonomous decisions.

## Testing Strategy

- Unit tests for every classifier and pure planner.
- Prompt tests for LLM-driven optimizer context changes.
- Touched-package coverage after slice implementation.
- Full sidecar Go coverage, Go e2e, build, and Playwright runs before final
  completion reporting.
