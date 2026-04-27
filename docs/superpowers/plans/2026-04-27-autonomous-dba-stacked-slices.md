# Autonomous DBA Stacked Slices Implementation Plan

> Branch: `codex/autonomous-dba-second-slice`
> Goal: keep stacking the autonomous DBA foundation on one branch until the
> product has a durable Observe -> Diagnose -> Decide -> Act -> Verify ->
> Remember loop.

## Build Boundary

Implement these slices on top of the existing first and second slices:

- Durable action lifecycle metadata over `sage.action_queue` and
  `sage.action_log`.
- Case timelines that combine proposed, approved, blocked, expired, executed,
  failed, rolled-back, and verified action states.
- Deterministic expiry, cooldown, and circuit-breaker checks to prevent stale
  approval and repeated failed action loops.
- Shadow-mode value proof from policy decisions and durable action states.
- UI exposure on Cases, Actions, and Shadow Mode without creating another
  generic observability page.

Out of scope for this stack:

- Direct high-risk DDL execution.
- Deleting or migrating user data out of existing tables.
- Autonomous account creation, credentials, or cloud-permission changes.

## Files To Touch

- `sidecar/internal/schema/bootstrap.go`
  - Add idempotent lifecycle columns and indexes.
- `sidecar/internal/store/action_store.go`
  - Persist and read lifecycle metadata for queued actions.
- `sidecar/internal/store/action_lifecycle.go`
  - Pure lifecycle/circuit-breaker helpers.
- `sidecar/internal/store/action_lifecycle_test.go`
  - Red/green coverage for expiry, cooldown, attempts, and shadow toil.
- `sidecar/internal/cases/case.go`
  - Add timeline/event fields to the case read model.
- `sidecar/internal/api/cases_handlers.go`
  - Attach action timelines to projected cases.
- `sidecar/internal/api/action_handlers.go`
  - Return lifecycle metadata in pending/action responses.
- `sidecar/web/src/pages/CasesPage.jsx`
  - Show timeline, expiration, cooldown, and blocked reasons.
- `sidecar/web/src/pages/Actions.jsx`
  - Show lifecycle metadata in approval queue and executed action detail.
- `sidecar/web/src/pages/ShadowModePage.jsx`
  - Show avoided toil and blocked automation reasons more explicitly.
- `sidecar/web/e2e/fixtures.ts`
  - Add lifecycle fixtures.

## Checklist

- [ ] Write failing Go tests for pure action lifecycle decisions.
- [ ] Implement pure lifecycle helpers.
- [ ] Write failing Go tests for queued action metadata mapping.
- [ ] Add schema lifecycle columns with idempotent migrations.
- [ ] Extend queued action store read/write paths.
- [ ] Write failing API tests for case action timelines.
- [ ] Attach timelines and lifecycle metadata to cases and actions APIs.
- [ ] Write failing React/e2e tests for lifecycle UI.
- [ ] Implement Cases/Actions/Shadow UI lifecycle surfaces.
- [ ] Run targeted Go and frontend tests.
- [ ] Run full serialized Go coverage, lint, vitest, build, and targeted e2e.
- [ ] Commit and push stacked slice.
