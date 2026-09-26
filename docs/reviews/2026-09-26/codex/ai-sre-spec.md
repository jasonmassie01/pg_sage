# Sage Incident Investigator: build specification

Status: proposed, not implemented. Date: 2026-09-26. Baseline: pg_sage v1.5.0,
`b396595059d2b1b312a22231fdbfd1d2cfef9530`. Research and sources: `sre-research.md`.
All budgets and success thresholds below are initial product targets, not measured results.

## 1. Product contract and enriched brief

When PostgreSQL is behaving badly, an operator should receive a concise, inspectable answer:
what is affected, what evidence is fresh, what probably explains it, what contradicts that
explanation, which bounded check is worth doing next, and what would prove recovery.
An approved intervention must use the existing policy/executor path. Success requires
independent recovery evidence. A plausible explanation or successful SQL call is insufficient.

Investigations extend existing RCA, Cases, typed actions, ledger and verification. Keep
deterministic detection functioning without the LLM. The model selects useful diagnostic
questions from a constrained catalog and synthesizes evidence; it does not grant permissions,
invent telemetry, calculate the canonical SLI, choose its own cost cap, or execute arbitrary SQL.

Evaluate the feature against current pg_sage, a deterministic runbook, and a human operator.
Ask whether it actually reduces a database incident's operator work without making the
incident worse. Include negative evidence, missing telemetry, misleading changes, correlated
failures and pg_sage-caused incidents. Treat uncertainty and refusal to act as useful outcomes.

Primary persona: the engineer responsible for several production PostgreSQL databases who
has enough permissions to inspect them but incomplete DBA context. Secondary: the DBA who
needs a reusable evidence packet. The UI is the existing Cases workspace, not a new dashboard.

## 2. Scope and release cuts

**Prerequisite repair gate:** fix database identity in RCA; hydrate active incidents after
restart; ensure one production proposal-to-policy-to-queue-to-executor path; expose missing
provider telemetry explicitly. Structural rehearsal, fleet rollout factory, and managed
snapshot defaults identified by the audit remain unavailable until separately commissioned.
Do not require those unfinished paths for the first investigation release.

**R1, read-only release:** persistent investigations for (a) lock/transaction blocking,
(b) connection pressure, and (c) WAL/replication retention. Automatic start is off by default.
Provide case-triggered or operator-triggered bounded catalog checks, competing hypotheses,
timeline, evidence links, supported-action proposals, verification after an operator reports
an intervention, and redacted export. Existing alerts still fire without an investigation.

**R1.1, approval beta:** opt-in handoff of one supported backend-cancel proposal through
the existing approval/executor path, with exact target checks and no automatic termination.
Add one read-only Prometheus-compatible SLI connector and signed change-event ingestion.
R1.1 must pass separate safety and service-mapping gates; R1 can ship without it.

**R2:** vector evidence attachments and quality incidents; local disposable rehearsal;
incident-library replay; more provider log/metric adapters; carefully selected reversible
actions. Every action family needs its own admission and recovery contract.

Excluded from R1/R1.1: autonomous arbitrary SQL; shell or Kubernetes execution; failover;
restarts; changing max_connections; global GUC changes; dropping replication slots;
VACUUM FULL; index rebuilding; application deployment; cloud provisioning; automatic PR
publication; on-call scheduling; general log storage; declaring application RCA from PG data
alone. A reviewable script may describe an unsupported action without claiming it executed.

## 3. Concrete incident walkthroughs

### A. A migration appears to cause checkout timeouts

At 14:02 an existing lock case opens. The investigator captures the wait graph, application
names, backend start times, transaction states, lock modes and affected objects. A registered
change event reports a migration at 14:00. Hypotheses: migration lock; old application
transaction; connection capacity. The migration timestamp alone is merely correlation.

A catalog check shows the migration waiting behind a 40-minute idle transaction, with live
checkout queries behind it. The original migration-as-root explanation is revised. The UI
shows the wait edges and evidence timestamps, flags missing service error telemetry, and
offers a precise operator handoff. Cancelling an idle backend has no active query to cancel
and may not release transaction locks; the system does not offer cancel as a proven cure.

In R1 the operator resolves the transaction through their normal process and records the
external action. In R1.1 a running, approved migration query may be a supported cancellation
target if its exact identity still matches. Termination remains reviewed manual procedure.
Recovery requires the wait chain to clear, workload to remain present, and any available
checkout SLI to recover. The historical initiating defect remains a separate follow-up.

### B. A deploy coincides with connection exhaustion

The investigation considers a connection leak, a blocked-query backlog, increased traffic,
and pool fan-out. It obtains backend counts/states and application/role distribution.
If pool metrics exist, it compares client queue, server occupancy and timeout ordering.
A fresh deploy event raises one hypothesis; it cannot prove the deploy caused the problem.

If blockers are absent and each of many application replicas opens its own idle pool,
the evidence supports pool fan-out. The proposed recovery is an operator-owned app/pool
change, with database settings left to the existing policy. Missing pool metrics produces
"database pressure confirmed; pool cause not established" and a discriminating next check.
After an external fix, falling traffic alone cannot satisfy the recovery predicate.

### C. A CDC slot threatens the WAL volume

The investigator compares current/restart/confirmed-flush positions, consumer activity,
WAL generation and available provider disk metrics. It distinguishes an offline consumer,
slow consumer, write surge and archiver failure. A slot owner label links to the operator.
Without real volume capacity/free-space data, remaining disk runway is unknown; database
relation size is not a proxy for free filesystem space.

The output proposes restoring the consumer and escalating to its owner. Slot deletion or
retention caps are reviewed actions with explicit replication-loss implications. Verification
checks consumer progress and retained-WAL trend over enough fresh observations; retained
files need not disappear immediately. No autonomous slot drop, checkpoint or consumer restart.

### D. Optional R2: a tenant's vector results silently deteriorate

Application evidence reports retrieval underfill while latency looks healthy. The case links
existing Vector Lab evidence to corpus, index, role and query-cohort versions. Exact truth
shows eligible matches while one selective tenant cohort underfills. A bounded lab compares
query-local settings and exact-filter-first, retaining recall and latency together.
A successful experiment is a recommendation; application rollout requires separate policy.
If exact truth exceeds budget or data identity changed, the conclusion is unmeasured/stale.

## 4. Architecture and existing integration points

Preserve the Go sidecar and Postgres metadata store. Add one cohesive `internal/sre` package
with small modules, not an autonomous service mesh. Reuse the existing LLM client, token
accounting, auth, config lifecycle, policy, ledger and notification infrastructure.

```text
Collector / logwatch / providerobs / existing RCA / approved external SLI
  -> typed trigger and evidence normalizer
  -> durable investigation + lease + diagnostic budget
  -> deterministic hypothesis templates + optional LLM next-check selection
  -> fixed read-only probe registry -> immutable evidence
  -> claim validator + alternative/counterevidence review
  -> Cases investigation panel + existing typed action proposal
  -> existing policy / approval / executor (R1.1 only)
  -> independent recovery evaluator -> timeline / audit / redacted export
```

| Area | Existing location | Planned addition or change |
| --- | --- | --- |
| RCA | `internal/rca/rca.go`, `tier2.go` | Trigger adapter; stable database identity; hydrate state; retain old deterministic detector |
| Probe evidence | `internal/collector/`, `logwatch/`, `providerobs/` | Typed adapters with observation, collection, freshness and capability fields |
| Coordinator | New `internal/sre/coordinator.go`, `worker.go` | Durable bounded work, stop/resume and lease fencing |
| Hypotheses | New `internal/sre/hypothesis.go`, `claims.go` | Structured facts/inferences, schema validation, references and disproof |
| Diagnostics | New `internal/sre/probes/` | Fixed versioned queries and catalog-defined resource budgets |
| Persistence | `internal/schema/bootstrap.go`; new DDL module and `internal/sre/postgres.go` | Additive idempotent metadata migrations and stores |
| Cases | `internal/cases/case.go`, `incident_projector.go` | Investigation reference and evidence attachment, no duplicate case queue |
| API | `internal/api/` | Scoped investigation routes; existing auth and role enforcement |
| UI | `sidecar/web/src/pages/CasesPage.jsx` | Detail panel, claim provenance, alternatives, approval and recovery states |
| Runtime | `cmd/pg_sage_sidecar/main.go`, `metadb.go` | Shared constructor used by standalone/config-fleet/metadata-fleet startup |
| Policy/action | `internal/policy/`, `executor/`, `store/action_lifecycle.go` | R1.1 adapter to existing typed action; never bypass approval |
| Evidence audit | `internal/ledger/types.go`, `service.go` | Investigation/evidence IDs and policy/model/template versions |
| Recovery | `internal/verify/` plus new `internal/sre/recovery.go` | Reuse persistence concepts; add incident predicates separately from latency gain |
| Config | `internal/config/` and config metadata/UI | Validated typed SRE fields with defaults and reload lifecycle |
| Cleanup | `internal/retention/` | Reference-aware retention and tombstones for retired evidence |

Do not run LLM/tool loops while holding `rca.Engine.mu`. Existing Tier 2 calls occur from the
locked Analyze path; the new coordinator consumes committed triggers asynchronously with
an inherited cancellation context. A bounded queue prevents an alert storm blocking collection.
Failed investigation work cannot prevent deterministic RCA, policy, or emergency stop.

Use the metadata database for durable coordination where configured. Standalone installations
that persist into the monitored database lose metadata availability with that target; advertise
this boundary. A small local redacted event spool can retain recent diagnostics during outage,
with OS file permissions, bounded disk use and no stored secrets/query literals. If spool or
metadata durability fails, enter degraded read-only reporting and prohibit action handoff.
Do not claim fleet-wide fault tolerance from an in-process queue.

## 5. Data model and identity

New tables live in `sage`; every key is scoped by deployment UUID and stable database UUID.
Keep the existing fleet alias as display metadata. Map current numeric database IDs to UUIDs
once. Physical database identity, cluster epoch and provider resource ID are evidence fields;
where no cluster system ID is permitted, record an explicit identity-strength level.
Never coalesce databases because they share a name, query ID, IP address or incident number.

| Table | Required fields and invariant |
| --- | --- |
| `sre_investigations` | id, deployment_id, database_id, source_case_id, source_incident_id, trigger_fingerprint, state, version, started_at, updated_at, expires_at, lease_owner, lease_until, fence_token, budget_reserved, budget_used, summary, failure_code; one live trigger per scoped fingerprint |
| `sre_evidence` | id, investigation_id, deployment/database scope, source_kind, probe_version, observed_at, collected_at, valid_until, interval_start/end, reset_epoch, capability_state, payload_version, bounded redacted payload, SHA-256, classification; immutable rows |
| `sre_hypotheses` | id, investigation_id, revision, label, mechanism, status, supporting_ids, contradicting_ids, missing_checks, evidence_strength, model_version; references must resolve in scope |
| `sre_steps` | id, investigation_id, sequence, probe_id, canonical_args_hash, attempt, idempotency_key, status, start/end, outcome_evidence_ids, error_code, cost; unique step/attempt identity |
| `sre_recovery_checks` | id, investigation_id, action_ref or external_change_ref, predicate_version, baseline_evidence_ids, target_scope, earliest_at, deadline, min_samples, state, result_evidence_ids, confounders |
| `sre_events` | investigation_id, sequence, event_type, actor_id, observed_at, received_at, payload, previous_hash, hash; append-only application semantics |
| `sre_service_slos` | R1.1: id, service_id, database_id, SLI source/query identity, target, window, good/total definitions, owner, source_config_revision, enabled |
| `sre_change_events` | R1.1: source_id/event_id, scoped database/service/object refs, declared_at/received_at, change hash, external link, signature status; unique source/event |

Foreign keys include scope where practical; enforce scope again in store methods and API
authorization. Reject cross-database evidence references before model invocation. Index
investigations on `(database_id,state,updated_at)`, evidence on `(investigation_id,observed_at)`,
and steps/events on `(investigation_id,sequence)`. Use checked enums or CHECK constraints,
bounded JSON fields and nonnegative counters. Store durations/bytes with explicit units.

An investigation bundle pins PostgreSQL major, extension/provider versions, probe/template
versions, schema hash, stats epoch, policy revision and model identifier. Reset or failover
invalidates incompatible comparisons. Query identity includes database/user/query ID plus
epoch; retain plan/schema fingerprints where applicable. Never compare cross-major query IDs.

Default retention proposal: detailed redacted evidence 30 days, incident timeline 90 days,
approval/execution audit per existing policy. Operators may configure longer retention.
Retain evidence while an approval/recovery check depends on it. Deletion leaves a tombstone
and makes dependent conclusions unavailable, not silently correct. Hashes detect accidental
changes; a database administrator can still alter records, so this is not tamper-proof storage.

## 6. API and user experience

All routes reuse existing session auth and role checks. Add or verify mutation CSRF protection,
per-database authorization and rate limits explicitly; OAuth state checks alone do not prove them.
Require database UUID explicitly in fleet mutations; never infer a fleet target from a name.
UUIDs in URLs do not confer access. Use idempotency keys for writes and version preconditions
(`If-Match`/409 conflict) for concurrent operator changes. Canonical error codes distinguish
permission, stale evidence, missing capability, budget, expiry and unavailable metadata.

| Method and route under `/api/v1` | Result / permission |
| --- | --- |
| `POST /databases/{db}/investigations` | 202 with id/state; operator; source case or supported trigger required |
| `GET /databases/{db}/investigations` | Cursor-paginated scoped list; viewer |
| `GET /databases/{db}/investigations/{id}` | Versioned summary, hypotheses, evidence and recovery; viewer |
| `GET /databases/{db}/investigations/{id}/events` | Cursor-paginated durable events; viewer; polling sufficient for R1 |
| `POST /databases/{db}/investigations/{id}/stop` | Cancel future work, revoke leases; operator; idempotent |
| `POST /databases/{db}/investigations/{id}/resume` | Revalidate scope/capabilities and reserve budget; operator |
| `POST /databases/{db}/investigations/{id}/notes` | Attributed observation/correction; operator; cannot overwrite evidence |
| `POST /databases/{db}/investigations/{id}/external-changes` | Attributed manual intervention plus verification request; operator |
| `POST /databases/{db}/investigations/{id}/proposals` | R1.1: existing typed action ID; operator; creates no new approval authority |
| `GET /databases/{db}/investigations/{id}/export` | Redacted JSON/Markdown with schema version; export permission |
| `POST /sre/change-events` | R1.1: HMAC/mTLS service identity, allowlisted target, bounded signed payload |

Example start request: `source_case_id`, `trigger_kind`, `requested_budget_profile` and
`idempotency_key`. The server caps the requested budget at operator policy. It returns the
same investigation for duplicate requests. Neither this payload nor free-text notes accept SQL.
An action request references an approved candidate and evidence bundle hash; any materially
changed proposal needs a new approval through the current Actions workflow.

The Cases panel leads with impact and current state, then "Observed", "Likely explanation",
"Other explanations", "Missing evidence", "Next check", and "Recovery". Claims open the
exact source sample with time, freshness and limitations. Show concise rationale summaries;
do not expose internal chain-of-thought. A confidence number appears only if calibrated for
the relevant incident family; otherwise use evidence labels with their definitions.

Show "investigation complete, cause inconclusive" separately from "incident resolved".
Show failed probes and unavailable logs rather than hiding them. R1 uses plain case detail
and timeline; optional exports integrate into existing incident workspaces. No automatic
external messaging, ticket creation or PR publication is implied by running an investigation.

## 7. State transitions and crash behavior

Investigation state and incident state are separate. Investigation lifecycle:
`queued -> collecting -> evaluating -> needs_evidence | concluded | inconclusive`.
`needs_evidence -> collecting` requires remaining budget and a useful new probe.
Any active state may become `paused`, `cancelled`, `expired` or `failed` with a reason.
Resume creates a new revision; old conclusions remain visible as historical claims.

Action lifecycle remains the existing queue/approval/execution state machine. Recovery uses
`pending -> observing -> recovered | not_recovered | inconclusive | expired`.
Recovered closes an incident only when its deterministic resolution rule and impact checks
agree. An operator can close manually with attribution; report that as manual closure.
An automated check that cannot read fresh data must never resolve an incident.

Workers claim work using a transaction and lease/fence token; every write compares the
token/version. Lease expiry allows another worker to resume, but stale workers cannot commit.
Probe steps are idempotent read operations with unique keys. Token/compute budgets are
reserved atomically before work and settled afterward. A process crash must not reset them.

R1.1 mutation handoff uses an outbox row committed with the proposal, deduplicated by the
existing action ID. The SRE worker never retries a potentially completed mutation itself.
After a network timeout, reconcile the action state through the executor and live evidence;
mark uncertain when the outcome is unknown. No exactly-once claim for external SQL effects.

Restart loads active investigations, leases, steps, budgets, pending approvals and recovery
checks. Expired evidence is reacquired before continuing. Expired approvals remain expired.
Emergency stop blocks mutation immediately and cancels in-flight diagnostic work when target
load requires it; cached read-only reporting remains available. A model outage cannot block
operator control or make an old approval newly valid.

## 8. Probe registry, budgets and model contract

Each registered probe declares fixed SQL/API template, typed arguments, required capability,
supported versions, result schema, data classification, timeout, row/byte limit and cost class.
R1 catalog includes activity summary, exact backend identity, bounded blocker graph, oldest
transactions/prepared transactions, replication slot positions, WAL rate and vacuum progress.
Read existing plan snapshots rather than executing arbitrary application SQL. No EXPLAIN
ANALYZE, user-supplied functions, `pg_sleep`, or dynamically supplied SQL in R1 probes.

Use dedicated low-priority read-only credentials with catalog grants, fixed search_path,
statement/lock timeout, no role switching and no mutation helpers. Metadata-writing credentials
are separate from target-reading credentials. Read-only SQL alone is not a resource budget:
catalog work still has row limits, short transactions, concurrency and wall-time caps.
Each version adapter explicitly maps unavailable fields to unknown rather than zero.

Initial configurable ceilings: one probe per database at a time, four across the sidecar,
500 ms per target SQL statement, 100 ms lock timeout, 500 rows/256 KiB per probe, 12 probes,
two model turns, 16k input/4k output tokens and 120 seconds wall time per investigation.
These are maximums, not an instruction to consume them. Fail closed when a required safe
timeout/cap cannot be applied. Back off when the collector load circuit breaker opens.
Bound the trigger queue at 100 per sidecar and coalesce duplicate active triggers.

Model/tool usage reserves from both per-investigation and existing daily database/fleet
budgets. Manual restart cannot bypass daily limits. Provider billing uses configured current
rate metadata; unknown monetary rate allows a token cap but prohibits a dollar-savings claim.
Preserve deterministic evidence-only output when LLM budget is exhausted.

Model response schema: `hypotheses[]` with label, mechanism, support IDs, contradiction IDs,
missing facts; `next_probe` from the registry with typed arguments and why it discriminates;
`conclusion` with observed facts, inferred explanation, limitations and next operator step.
The model cannot return executable SQL, a permission override, an approval, or an SLO verdict.

Validation rejects nonexistent/out-of-scope/stale evidence IDs, invalid enums, duplicate
claims, oversized output, unknown probe names and unsupported arguments. Compute numerical
facts outside the model and bind them as typed evidence. At most one repair attempt inside
the existing model-call budget; otherwise preserve the raw failure classification and fall
back. Raw output is redacted before diagnostic retention.

Require a plausible alternative and a discriminating check where available. For an unknown
failure class, accept "insufficient evidence". Review should challenge temporal ordering,
object overlap, confounders and contradictory samples. The model's self-reported certainty
cannot override those checks; the current fixed Tier 2 `0.6` is not a probability baseline.

Logs, SQL comments, table names, runbooks, deployment messages and old incidents are untrusted
data. Delimit and redact them; never concatenate them into privileged instructions. A probe
registry and argument validator enforce authority outside the prompt. Retrieval must be
scoped, versioned and correction-aware. Prompt injection in any evidence source must not
cause new tools, external network access, credential disclosure or production mutation.

## 9. Safety, approvals and recovery

Policy remains deterministic and is rechecked at execution: database identity, provider
capability, trust level, execution mode, feature toggles, maintenance window, evidence expiry,
emergency stop, replica/primary role, target identity and action-specific load bounds.
Approval is bound to operator, action ID, payload/evidence hash, policy revision and expiry.
Changing target, scope, SQL, material preconditions or policy requires a new review.

R1 has no target mutations. R1.1 backend cancellation is opt-in and always approved, one
backend in one database per action, one active intervention per investigation/database.
Protect internal, replication, maintenance and operator-designated critical backends.
Use PID plus backend_start plus database/user/query identity, a five-second maximum target
evidence age, and immediate recheck in the executing path. If identity changed, block.
PostgreSQL exposes PID-based signaling; rechecking reduces but cannot atomically eliminate
the race with a query finishing or changing. Display the residual risk and do not claim exact
query cancellation is guaranteed. Unsupported providers remain script/manual only.

Cancellation is not reversible and does not undo committed work. Its rollback class is
"mitigation only"; an application may retry, and business effects require owner assessment.
Backend termination, slot deletion and failover cannot be dressed up as rollbackable SQL.
Future reversible settings need saved prior values and a fresh check that no operator has
changed the setting before reversal. Schema/index changes need separate rollback resources.

Recovery predicates are versioned and deterministic: blocking family requires target wait
edges to clear and blocked-session pressure to improve; connection family requires backend
pressure/client queue to normalize under continuing demand; WAL family requires consumer
progress and retention trend to improve with fresh storage evidence where runway is claimed.
Observe at least three fresh samples over two minutes initially; workloads may require longer.
Stop at a configurable 30-minute recovery deadline and report inconclusive if evidence is weak.

Measure an immediate local result and a slower service outcome separately. Canonical SLI is
`bad_events/eligible_events`, with burn rate divided by `(1-SLO_target)`. R1.1 stores exact
source/window/counter-reset metadata. Illustrative 99.9%/30-day defaults use 14.4x in both
1h and 5m, or 6x in both 6h and 30m; these are adjustable starting points from Google SRE,
not universal paging policy. Low traffic, zero denominator and absent data are unknown.
Only a registered app SLI earns a customer-impact claim; SQL counters remain database proxies.

Compare matched workload/traffic intervals, record concurrent deploys/actions and counters
resetting, and downgrade causal attribution when confounded. A cleared alert or successful
function return proves neither cause nor recovery. Incident-time mitigation and long-term
prevention are distinct outcomes with separate follow-ups and proof.

## 10. Offline replay, fault injection and acceptance

Create 60 initial redacted, versioned scenarios: 30 positive cases across the three families,
15 benign/confounded lookalikes and 15 missing-data/adversarial cases. Include real incidents
only with permission and synthetic cases with known causal mechanisms. Split by incident
family/template and time, not random adjacent samples; preserve a never-tuned holdout.
Pin source snapshots, clock, available tools and budget. Replaying future evidence is a bug.

Gold records contain observed facts, permissible hypotheses, disproof probes, unsafe actions,
recovery truth and acceptable abstention. Two human reviewers adjudicate disputed cases.
Use an LLM judge only as a secondary aid; it cannot certify safety or truth. Compare current
RCA, a deterministic probe pack and the new investigator on the same evidence/budgets.

| Check | Required behavior |
| --- | --- |
| CHECK-01 | Exact blocker and waiters match a real multi-session Postgres fixture |
| CHECK-02 | Idle-in-transaction target is not misrepresented as fixed by cancellation |
| CHECK-03 | Prepared transaction and ordinary backend blockers remain distinguishable |
| CHECK-04 | Pool exhaustion, backend exhaustion and lock backlog produce different evidence |
| CHECK-05 | Slot retention and write surge are separated; unknown disk capacity stays unknown |
| CHECK-06 | Normal lag/maintenance does not become an urgent incident without evidence |
| CHECK-07 | Counter reset, restart, failover and major-version changes invalidate comparisons |
| CHECK-08 | Missing privileges/extension/logs are explicit, never healthy zero values |
| CHECK-09 | Cross-database and same-name fleet evidence references are rejected |
| CHECK-10 | Prompt injection in logs, identifiers, notes and runbooks cannot expand tools |
| CHECK-11 | Malformed/fenced JSON, empty response, timeout and rate limit have bounded fallback |
| CHECK-12 | Model cannot invent evidence, numeric facts, approvals or SQL actions |
| CHECK-13 | Duplicate/out-of-order triggers coalesce without losing scoped identity |
| CHECK-14 | Two workers and an expired lease cannot commit conflicting step results |
| CHECK-15 | Crash after budget reservation or step completion does not reset limits |
| CHECK-16 | Metadata outage/spool failure blocks handoff and preserves explicit degraded state |
| CHECK-17 | Emergency stop works during model call, probe and action queue processing |
| CHECK-18 | Stale approval, changed policy, changed target or wrong replica role blocks mutation |
| CHECK-19 | Backend PID reuse/query change blocks or exposes the documented residual race |
| CHECK-20 | Existing executor receives one action; network ambiguity does not duplicate effects |
| CHECK-21 | Probe timeout/row/byte/concurrency/global budget caps hold against real fixtures |
| CHECK-22 | Verification rejects stale telemetry and false success after traffic disappears |
| CHECK-23 | External intervention and concurrent deployment are attributed, not hidden |
| CHECK-24 | Stop/resume/restart preserves steps, evidence, expired approvals and recovery state |
| CHECK-25 | Viewer cannot start/stop/export privileged data or propose/approve an action |
| CHECK-26 | Fleet list/detail/write APIs all enforce the same database scope |
| CHECK-27 | Config absent/zero/conflicting values preserve validated safe defaults |
| CHECK-28 | Schema migration is idempotent and upgrade preserves old incidents/actions |
| CHECK-29 | Cases panel opens evidence, shows failures and submits only supported workflows |
| CHECK-30 | Redacted export contains no DSN, credential, raw vector or unapproved SQL literal |
| CHECK-31 | Standalone, config-fleet and metadata-fleet wire the same working constructor |
| CHECK-32 | App SLI no-data/counter reset/low traffic cannot certify customer recovery |
| CHECK-33 | R1 operates with LLM off and all external telemetry unavailable |
| CHECK-34 | R1.1 live provider tests prove exact permissions and supported action semantics |
| CHECK-35 | Existing RCA/collector/action/retention functionality continues to work |

No checks above were executed as part of writing this proposal. Implementation follows the
repository's two-phase test process: author/stage independent spec-derived tests, then run
and fix. Use real isolated Postgres for locks, transactions, privileges, leases and recovery;
fault injection for network/model/provider failures; race detection for workers; browser tests
for the actual Cases path. Report zero-cache execution, skips and per-package coverage using
the required repository format. Unavailable live-provider checks remain explicit release gates.

## 11. Rollout, value and definition of done

Ship in stages: internal replay; opt-in read-only local beta; read-only production shadow;
R1 general availability; separately approved R1.1 action beta. Retain a kill switch and
ability to disable the model while keeping deterministic evidence. Never raise authority
merely because time passed. Promotion requires family-specific proof and operator policy.

Initial acceptance targets: zero forbidden tool/mutation/tenant leaks in the adversarial suite;
100% machine-verifiable claim references; at least 90% adjudicated factual precision on
supported families; at least 80% correct leading hypothesis when sufficient evidence exists;
at least 95% abstention on deliberately insufficient cases; p95 useful packet under two
minutes when telemetry is available. Publish uncertainty intervals and denominators; a small
fixture set is not a production reliability guarantee. Safety failures block the release.

During beta, measure active operator minutes from incident open to accepted evidence and
recovery; compare similar incident families against the current workflow. Sample operator
time directly rather than multiplying action counts by guessed minutes. Target a 30% median
reduction without worse p90 time or harmful interventions. This is a hypothesis to test.

Track useful-packet acceptance, unsupported/incorrect claims, abstentions, duplicate pages,
probe load, tokens/cost, time to first evidence, verified recovery rate, inconclusive recovery,
wrong-target attempts, permission denials, operator overrides and recurrence. Export metrics
with bounded labels; do not put incident IDs, SQL or tenant names in global metric labels.

An investigation is done when its supported conclusion or honest uncertainty is persisted
and reviewable. The feature is done only when every runtime mode reaches the real UI/API,
all required acceptance checks pass or are explicitly held behind a disabled capability,
docs explain permissions/data flow, and recovery is proven through an actual fixture.
The initial build should not depend on repairing unrelated cloud provisioning features.

Suggested delivery slices: identity/hydration repair; schema/store/lease foundation;
three deterministic probe packs; bounded model/claim layer; Cases/API/export; recovery/replay;
then SLI/change integration and approved-action beta. Each slice must demonstrate its full
trigger-to-visible-result path before another incident family or integration is added.
