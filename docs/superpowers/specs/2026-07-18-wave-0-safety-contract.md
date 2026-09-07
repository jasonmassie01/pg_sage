# Wave 0 Safety Contract

**Status:** Accepted for Wave 1 implementation
**Date:** 2026-07-18

This document fixes the safety semantics that Wave 1 must enforce. It is the
decision source for current remediation work; it does not claim that later-wave
operational changes are already implemented.

## Execution authority

`execution_mode` and `trust_level` are independent controls. Authority is the
intersection of both controls, never the more permissive of the two.

| Trust level | `manual` | `approval` | `auto` |
| --- | --- | --- | --- |
| `observation` | Record cases only | Record cases only | Record cases only |
| `advisory` | No background queueing or execution | Queue supported actions | Execute safe eligible actions; queue higher-risk supported actions |
| `autonomous` | No background queueing or execution | Queue supported actions | Execute safe and moderate eligible actions; queue high-risk supported actions |

Additional rules:

- `manual` is a hard disable for background execution and background approval
  queueing. A manual API invocation remains an explicit, separately authorized
  operation; it is not an implicit promotion to `auto`.
- Automatic execution occurs only when `execution_mode: auto` is configured.
- Approval mode determines whether a supported action can be queued. It must not
  first pass the narrower automatic-execution eligibility test.
- `executor_enabled: false` blocks every database mutation path, including
  explicit manual execution.
- Emergency stop blocks every mutation path and is rechecked, together with the
  current authorization snapshot, immediately before each action.
- Policy decisions use typed action semantics such as action kind, reversibility,
  transactional constraints, and risk. Finding severity alone is not execution
  authority.
- Per-database executor and trust overrides are authoritative for that database.

## Canonical control state

When meta-database mode is enabled, the meta database is the canonical store for
authorization, execution control, approvals, and audit state. A monitored target
database is not an alternative authority source. Missing, stale, or unavailable
canonical control state fails closed for mutations.

## AgentDB policy and lifecycle

AgentDB mutation authority is the intersection of:

1. runtime execution authority;
2. global hard ceilings;
3. provider-specific capabilities and narrower provider ceilings; and
4. authorization for the exact requested operation and resource.

Providers may narrow global policy but cannot widen it. Missing policy,
unsupported operations, unknown providers, and stale authorization fail closed.
Lease renewal wins over expiry cleanup when they race. Expiry reconciliation must
claim work atomically, revalidate before provider mutation, and permit at most one
provider mutation across concurrent reconcilers.

## AgentDB monitoring

The target monitoring model is tiered and adaptive: inexpensive health signals
run most often, diagnostic collection escalates when evidence warrants it, and
costly analysis backs off for stable databases. Monitoring cadence does not grant
mutation authority; executor policy remains an orthogonal gate.

## Product surface

MCP is retired. Current configuration, examples, and roadmap language must not
advertise MCP as an active interface. Historical changelog entries may retain
accurate history.

## Configuration lifecycle

Each running component observes an immutable configuration snapshot. A reload
constructs and validates a new snapshot before publication. Fields are classified
as one of:

- **Live:** safe to swap atomically for subsequent work.
- **Reconfigure:** requires an explicit component teardown/rebuild boundary.
- **Restart:** rejected during hot reload with an actionable restart requirement.

In-flight work finishes against the snapshot with which it started. Configuration
objects are not mutated piecemeal beneath running goroutines.

## Wave 1 acceptance gates

- Tests demonstrate the execution matrix and the absence of manual-to-auto
  promotion.
- API presentation and live execution share one typed action policy.
- Every action is authorized against current state immediately before mutation,
  without data races.
- AgentDB expiry handling is renewal-safe and single-winner under concurrency.
- PostgreSQL advisory locks are acquired and released on the same pinned session.
- Focused tests, race tests, build, vet, and the full uncached coverage suite pass,
  with skips and coverage gaps reported explicitly.
