# Runtime, safety, evidence, and lifecycle audit

Source: pg_sage `master`, `b396595059d2b1b312a22231fdbfd1d2cfef9530`, 2026-09-26.
All source references are relative to the isolated `audit-repo` snapshot. Original
checkout and live services were not changed. P0 means possible data loss; P1 means
release-blocking safety/security or important broken workflow; P2 means correctness
or incomplete capability. A static confirmation establishes the source path, not
that the user's live installation has exercised it.

## R01 — P0: retention can delete live rows in other partitions

**Confirmed by executable SQL reproduction.** `sidecar/internal/autonomy/retention_postgres.go`
`deleteBatch` selects `ctid` from the parent, then deletes using only equality on
`ctid`. The detector explicitly includes partitioned parents (`schema_postgres.go`,
`unboundedAppendSQL`, `relkind IN ('r','p')`). Physical row locations repeat across
partitions. One expired row in partition A and one future row in B both had `(0,1)`;
with batch limit 1 the production-equivalent query returned **DELETE 2**, including
the future row. All fixture changes were rolled back.

Evidence: [SQL reproduction](evidence/retention-partition-repro.sql),
[captured result](evidence/retention-partition-repro.txt).
[PostgreSQL system-column documentation](https://www.postgresql.org/docs/current/ddl-system-columns.html)
confirms `ctid` is a location within a table and `tableoid` distinguishes partitions.

**Fix:** bind victims and deletion to both `tableoid` and `ctid`, or process one
validated leaf relation at a time; keep a cutoff predicate at the deletion boundary.
Test partitioned/inherited tables, duplicate physical locations, concurrent updates,
row movement, RLS, and a strict total batch bound. Acceptance: no noneligible row is
ever deleted; total deleted rows never exceeds the authorized batch size.

## R02 — P1: retention deletes bypass the central execution policy gate

**Confirmed source path.** `cmd/pg_sage_sidecar/autonomy_runtime.go` starts a schema
custodian alongside the executor. `autonomy/schema_postgres.go:28-39` enables retention
planning and injects a `postgresRetentionEnforcer` directly. Its `Apply` method checks
a prior dry run and whether the standing document lists the retention class. It does
not call `policy.Gate.Authorize` or the executor. It then calls `pool.Exec` directly.
Consequently the execution path does not inspect emergency stop, executor enabled,
observation/manual mode, approval-required classes, maintenance windows, current
blast-radius usage, or a mutation lease. Those checks exist in `policy/gate.go` and
are bypassed here. An authorized retention class plus a prior dry run is the enabling
condition; defaults alone do not establish that every deployment will delete rows.

**Fix:** retention must submit a typed destructive action through one authoritative
gate and execution mechanism. Check stop state immediately before each batch, use
bounded query/lock timeouts and leases, and record durable intent before mutation.
Accept only if stop, observation, disabled executor, required approval, closed window,
exceeded budget, replica, and stale policy all produce zero deleted rows.

## R03 — P1: retention guesses the meaning of time and accepts stale dry runs

**Confirmed contract gap.** `schema_postgres.go` picks the first date/time attribute
using a preference for `created_at`, then `occurred_at`, then `updated_at`. The
`schemaguard.TableContract` includes a duration but no explicit retention column.
`requireDurableDryRun` accepts any historical dry run for schema/table, without binding
it to relation identity, policy version, time column, duration, or candidate population.
A changed contract can delete a different population using unrelated old dry-run evidence.

**Fix:** explicit owner-selected column, relation OID plus generation, contract hash,
policy version, candidate count/sample, expiry, and row limit. Changing any semantic
input invalidates the dry-run evidence. No production deletion on inferred semantics.

## R04 — P1: incident state is not durable across restart or manual resolution

**Confirmed source path.** `rca/rca.go:127-136` initializes an empty in-memory incident
list. There is no production read/hydration path for unresolved persisted incidents.
After restart, existing rows are never visited by auto-resolution and a continuing
condition receives a new UUID. Conversely the API resolver (`api/handlers_v09.go:419-436`)
sets only the database row, while `PersistIncidents` upserts its old in-memory
`ResolvedAt` value (`rca.go:370-407`). The next persistence cycle can reopen a manual
resolution. The supplied resolution reason is discarded.

**Fix:** one incident repository/state transition owner, startup recovery, versioned
compare-and-swap updates, explicit reoccurrence semantics, and persisted actor/reason.
Acceptance: restart preserves incident identity; manual resolve survives subsequent
collection; a genuinely recurring incident is linked and distinguished from reopening.

## R05 — P2: RCA database identity and explanation provenance are incomplete

`Incident.DatabaseName` is persisted but normal deterministic/LLM constructors never
populate it. The adapter lacks a database identity field. API aggregation patches
display context from the pool, but stored evidence and self-action matching lack a
strict incident identity (empty names are treated as matching). Do not infer a proven
cross-database exploit: normal per-database pools constrain many calls.

Tier 2 uses a fixed confidence of 0.6 (`rca/tier2.go`) and attaches chain descriptions
to signal IDs by array position; chain evidence text is empty. This is an explanation
quality limitation, not a calibrated probability. The new SRE specification must
require explicit evidence references, competing explanations, and disproof.

**Improve:** attach stable database ID and observation epoch at construction; persist
evidence hashes, timestamps, version, confidence rationale, and model/prompt provenance.
Require every causal statement to reference an actual observation. Missing evidence
must downgrade or reject the explanation rather than fill it with positional labels.

## R06 — P1: fleet rollout is implemented as components but never activated

`cmd/pg_sage_sidecar/rollout_runtime.go:13-24` declares a runtime factory and returns
`ErrRuntimeDeferred` when nil. No production assignment to the factory exists. The
periodic scheduler starts, but all non-test paths remain deferred. `rollout.NewRuntime`
has tests and useful policy/cohort code but no assembled production constructor.

**Fix:** either visibly label the capability unavailable or wire concrete prior
evidence, cohort, policy, run store, actuator, verification, and rollback dependencies.
Acceptance: an actual binary run against three disposable databases selects one
canary, verifies it, halts on regression, survives restart, and never touches a
noncohort database. Unit tests of the rollout engine are not proof of this feature.

## R07 — P1/P2: schema structural remediation ends in a successful no-op

`autonomy/schema_postgres.go:254-259` returns nil when a structural rehearsal router is
missing or SQL is empty. `executorProposalRouter` implements normal and verified-index
routing only; no production implementation of `RouteStructuralRehearsal` exists. The
schema custodian counts this nil result as routed and records a decision. Several
invariant enum/classifier branches also have no production detector in the schema
custodian (the runtime scans only FK indexes, append retention, and text pathologies).
Some similar findings exist in the separate schema lint engine; that does not complete
this specific custodian lifecycle.

**Fix:** return a typed unavailable/recommend-only result that reaches Cases, with a
reason and next action. Wire a supported rehearsal adapter only after proof requirements
are met. Acceptance: every emitted invariant has a visible terminal disposition;
unavailable dependencies cannot increment a successful routing metric.

## R08 — P1: clone rehearsal can claim promotion without workload proof

`migration/runtime/postgres_rehearsal.go:48-69` measures command duration and database
size but never populates `AffectedQueries`. `migration/rehearsal/orchestrator.go` checks
regression only by iterating `AffectedQueries`; an empty slice passes and returns
`promote_expand`. Command wall time is labeled `MaxLockDuration` although it is not a
measurement of held-lock duration. There is no meaningful workload-regression test on
this production path. Policy still applies afterward, so this is an invalid evidence
gate rather than proof of unconditional execution.

The same orchestrator ignores clone-destruction errors, allowing apparent success
with a leaked resource. `mcp_migration_runtime.go:22-27` additionally provides a managed
snapshot factory that always returns unavailable; the DLE path is the concrete path.

**Fix:** require named affected workload, paired pre/post observations, representative
parameters, plan and latency evidence, freshness, coverage and variance checks. Empty
or unrepresentative workload => inconclusive, never promote. Record cleanup failure
durably and reconcile resource teardown. Report DLE versus managed snapshot readiness
honestly. Test interrupted cleanup and partial migration steps.

## R09 — P2: FK-index detector accepts unusable coverage

The `missingFKIndexSQL` subquery in `autonomy/schema_postgres.go` checks whether FK
attribute numbers are contained anywhere in `idx.indkey`. It does not require usable
leading key columns, exclude INCLUDE-only attributes, or reason about a partial-index
predicate. Thus an FK can be labeled covered by an index that cannot serve the relevant
lookup. This is a false-negative performance/safety diagnosis, not constraint loss.

**Fix:** use actual key attributes (`indnkeyatts`), accepted access method and operator
class, leading-prefix compatibility, and a predicate proven applicable to the workload.
Acceptance: INCLUDE-only and noncovering partial indexes cannot hide a missing FK index;
test composite FK order, multicolumn indexes, partitions, invalid indexes and expressions.

## R10 — P2: query evidence needs reset identity and workload comparability

`querystore/querystore.go` correctly computes differences rather than lifetime means
and rejects decreasing endpoint counters. However it records only query ID and totals,
without server/statistics reset epoch, user identity, top-level identity, or schema/plan
version. A reset followed by enough new calls can exceed the old endpoint and look
monotonic. A query fingerprint alone does not prove comparable before/after workload.

**Improve:** capture statistics epoch and grouping identity; use normalized interval
deltas; require comparable cohorts and minimum effective samples; invalidate evidence
on reset, failover, schema drift, or changed query shape. PostgreSQL documents reset
and snapshot semantics in its [statistics reference](https://www.postgresql.org/docs/current/monitoring-stats.html).
Treat absent or reset data as unknown, never zero load or verified improvement.

## Feature dispositions and improvements for this review slice

| Feature group | Runtime disposition | Next improvement / acceptance |
|---|---|---|
| RCA deterministic trees | Wired; incident lifecycle defective | Durable state owner; recovery/resolution tests R04 |
| RCA LLM and self-action correlation | Wired; identity/provenance incomplete | Explicit cited hypotheses; no fixed score presented as probability |
| Log ingestion, parser, fanout | Wired to RCA and migration observers | Durable cursor and observable loss/freshness; rotation/truncation/restart replay |
| On-demand EXPLAIN / auto_explain support | Wired; on-demand path captures estimated plans | Clearly label estimated versus actual plans; distinguish session activation from application coverage |
| Query-store history | Wired; fleet pruning gap in core report | Reset-aware interval identities; bounded retention and drift invalidation |
| Schema lint | Wired deterministic/optional LLM runner | Show severity, owner, scope and remediation evidence; measure false positives |
| Schema guard / table contracts | Partial; R01-R03/R07/R09 | Safety gate first, explicit contracts and visible blocked dispositions |
| Freeze custodian | Wired through typed proposals | Deadline uncertainty, owner exemptions, freshness and hard-stop regression suite |
| WAL/slot custodian | Wired; auto-drop deliberately disabled | Owner heartbeat and retained-WAL evidence; opt-in/manual drop, never guess abandoned |
| Migration detection and PR-ready scripts | Wired; backend produces scripts/metadata | Versioned preflight identity; CI artifact must match reviewed SQL digest |
| Online migration / clone rehearsal | DLE path exists; proof incomplete R08 | Real workload replay, durable step journal, explicit expand/contract ownership |
| Managed snapshot clone | Adapter interface only in live construction | Provider-specific readiness and tested cost/TTL cleanup; mark unavailable until built |
| Decision ledger / self-audit | Wired worker and repository | Capture every mutation before execution; repair retention bypass; link readable evidence from UI |
| Standing policy / leases | Implemented central gate; incomplete path coverage | Prove all writers call the gate; one authoritative database generation and lease |
| Fleet cohort rollout | Components and periodic loop; factory unwired | Compose production runtime with canary, verification and recovery R06 |
| Legacy C extension | Frozen reference, not Go runtime | Archive/package boundary and docs; do not sell `sage.diagnose()` as sidecar functionality |

## Maintainability and release hygiene

The production entry point is about 2,840 lines, far above the 500-line project limit;
several initialization functions also exceed the 50-line limit. Duplicated standalone,
static-fleet, metadata-managed, and AgentDB registration paths explain feature parity
drift. Refactor only after pinning lifecycle behavior with integration tests: one
database runtime constructor with explicit optional capabilities is the useful seam.
Do not refactor blindly while changing action semantics.

The roadmap still describes shipped alerting/plan history as future and contains old
test/coverage claims; the original ignored CLAUDE notes specify older Go versions and
feature counts. `sidecar/go.mod` requires Go 1.25.0. Generate a capability manifest from
runtime registration and use it to drive docs, readiness endpoints, and contract tests.

Frontend build/lint pass but the generated JS bundle is about 874 KB before gzip and
produces a size warning. Split advanced feature pages and large chart code; measure
first usable Cases/Value rendering on a constrained connection before choosing targets.

Dependency audit reports three moderate package entries for the same Vitest mocker
advisory, not three distinct production-server exploits. It concerns dev-server file
read behavior under stated reachability/plugin preconditions. Upgrade supported Vitest
and matching coverage packages, then rerun frontend checks; no blanket `audit fix --force`.
[Primary advisory](https://github.com/advisories/GHSA-82fw-gwwq-j7x9).

## Verification boundary

This audit does not certify zero bugs. Confirmed paths and one destructive-query
reproduction are documented above; complete suite counts, skips and coverage are in
the verification report. No customer data was used, no cloud resources were provisioned,
and no product remediation was silently applied as part of the review.

