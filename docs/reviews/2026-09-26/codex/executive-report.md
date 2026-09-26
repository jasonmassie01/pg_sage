# pg_sage: full feature audit, improvement roadmap, and AI SRE specification

**Date:** 2026-09-26. **Baseline:** v1.5.0, master,
`b396595059d2b1b312a22231fdbfd1d2cfef9530`.
The canonical checkout was clean and matched remote HEAD. Review and tests used an
independent snapshot. Original source and running services were preserved.

## Decision

**Do not expand autonomous production authority yet.** The product has valuable
working capabilities, but the audit found destructive retention defects, authorization
gaps, broken lifecycles, misleading verification claims and unfinished production paths.
This is a completed audit and design deliverable, not a claim that pg_sage is repaired
or bug-free.

The most important finding is reproduced data loss: retention selected one expired row
and deleted two rows across partitions, including a future-dated row. Independent review
confirmed this path bypasses the central executor gate and can continue after application
emergency stop when retention preconditions are met. Disposable fixture changes were
rolled back; no customer data was involved.

**Recommended new feature: Sage Incident Investigator.** Extend existing RCA and Cases
into a persistent investigation that gathers fresh evidence, tests competing hypotheses,
explains uncertainty, proposes supported actions through existing authority, and verifies
recovery independently. Start with read-only investigations for blocking, connection
pressure and WAL/replication retention. Add approved interventions after repairing the
relevant safety and lifecycle defects.

## Contents and coverage

This consolidated file includes the enriched brief, all three audit reports, independent
cross-review, research, SRE specification, implementation contracts, exact verification
report and review limitations. Companion files remain available for focused reading.
Source citations refer to the pinned snapshot, not a moving branch.

Feature matrices cover collection and individual rules, all six advisors, optimization,
hints, actions/verification, RCA/logs, schema/migrations, policies/custodians, fleet,
AgentDB/providers, API/auth/UI, value/shadow, alerts/metrics, deployment and vector lab.
An entry with no additional confirmed defect is not exhaustive proof of correctness.

## Highest-priority findings

| Priority | Finding | Evidence and next action |
|---|---|---|
| P0 | Retention deletes noneligible rows across partitions | R01, executed SQL reproduction; exact physical identity and strict batch bounds |
| P1 | Retention bypasses stop and central policy | R02/R03, independently traced; one authorized execution path and bound evidence |
| P1 | HTTP MCP write tools omit caller-role checks | SURF-01; trusted principal and viewer-denial tests on the real mounted router |
| P1 | OIDC links existing users by unverified email | SURF-02; issuer-dependent takeover risk; issuer/subject identity and deliberate linking |
| P1 | Suppression resurrects findings; new SQL retains old inverse | C02/C04, failing regression probes; stable identity and atomic forward/inverse updates |
| P1 | Rollback accepts missing evidence or finishes before revert | C14/C15; separate measurement verdict from intervention completion |
| P1 | Real cache percentages are treated as fractions | C01, actual collector SQL probe; canonical units across all consumers |
| P1 | Incident restart/manual resolution is not durable | R04; one state owner, version checks and restart recovery |
| P1 | Fleet omits internal retention cleanup | C08; unified per-database runtime construction |
| P2 | Value loses actual activity; success/dry run become verified | SURF-03/06/12/14; correct storage topology, identity and proof states |

Other substantiated gaps include stranded advisor work, optimizer identity collisions,
installed hints surviving retirement, descending-sequence blindness, false query-volume
growth, corrupted query IDs, unavailable provider readiness despite existing dependencies,
discarded Terraform content, partial AgentDB monitoring, lost alert delivery, dead UI
paths, unwired forecasting/adaptive monitoring/fleet rollout and empty-workload rehearsal.
Detailed entries supply triggers, evidence, limitations and acceptance tests.

## Repair and investment order

| Wave | Scope | Completion evidence | Value measure |
|---|---|---|---|
| 0: authority/data safety | R01-R03, SURF-01/02/17, C04/C14/C15 | Stop/role/partition/crash fixtures prevent forbidden writes; exact actor/payload; restart-safe reverts | Zero wrong-target writes or lost reversals |
| 1: identity/truth | C01-C07, C17/C18, R04/R05/R10, SURF-03/09/11/12/13 | Stable IDs, canonical units, preserved observation times, explicit unknown state | Fewer stale/duplicate cases and false verified outcomes |
| 2: complete runtime paths | C08-C13, R06-R09, SURF-04/07/08/14/15 | Real-binary parity across runtime modes; working result or visible unsupported disposition | Promised workflows completing end to end |
| 3: improve existing features | Full matrices below; SURF-05/06/10/16/18/20 | Workload-aware advice, credential refresh, restore proof, complete Cases controls, reliable alerts | Operator task time, precision and delivery success |
| 4: SRE R1 | Three read-only incident families, durable evidence and Cases | Prerequisite repairs, replay benchmark, budget/permission/freshness tests | Useful packets and measured operator minutes saved |
| 5: SRE R1.1/R2 | Approved cancel handoff, SLI/change context, vector proof, rehearsal | Per-action admission and independently verified recovery | Recovery without harmful intervention; cohort quality |

These are dependency waves, not a promise to ship everything in one release. Delete dead
facades with no current purpose. Complete partial features only when the outcome remains
valuable. Distinguish unsupported, unavailable, unconfigured and degraded states.

## Verification interpretation

The appendix records all runs, including initial fixture failures and corrected results.
Baseline suites are broad and mostly green; focused boundary probes still reproduce bugs.
Package coverage is not proof of a complete workflow.

**The sidecar entry-point package is at 43.5% statement coverage**, below the 70% business
threshold. Corrected unit/integration runs meet thresholds for internal business packages.
This entry-point gap is an unresolved release gate. Audit probes are not a replacement
coverage run, and production remediation is not claimed complete.

Build, vet and Go lint pass. Frontend lint/build pass with a bundle-size warning.
Component tests: 89 passed. Mocked browser tests: 54 passed. These do not prove real
backend/provider commissioning. Dependency audit reports three moderate package entries
for one Vitest development-tool advisory, with actual scope explained in the runtime audit.

No live cloud provisioning or real LLM-quality validation occurred. Credential-dependent
tests are explicitly skipped. No complete legacy C-extension matrix, PostgreSQL 14-18
cross-version fleet, long soak or provider failover was exercised. Those remain gates.

## Research judgment

Research uses 31 linked primary sources: PostgreSQL behavior, operator issue reports,
providers, competitors and vector papers. Generic AI RCA is crowded and partly present
already. The opportunity is Postgres-specific evidence, bounded investigation, honest
abstention and proof of recovery under comparable workload. Vendor scope is distinguished
from independently demonstrated capability; social and market-validation gaps are listed.

The spec includes incident walkthroughs, eight scoped data entities, APIs/UX, worker and
lease recovery, probe/model budgets, permission boundaries, replay evaluation, 35 acceptance
checks and rollout. Targets are proposed, not measured. Investigation and recovery have
separate time budgets. The implementation appendix makes the first slice concrete.

## Review method and limits

Three parallel review/research tracks were combined with coordinating runtime review,
disposable-database probes and independent cross-review of destructive paths.
The pinned Gemini review was attempted but not executed: automatic approval review
rejected external upload of authentication source because that sharing was not authorized.
No alternate upload was attempted; local independent review continued.

Original checkout unchanged. Reports and audit-only failing regression probes are the
deliverables. Product fixes and the SRE feature have not been implemented.


