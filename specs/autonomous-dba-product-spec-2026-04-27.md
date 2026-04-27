# pg_sage Product Spec: Autonomous DBA Agent

> **Status**: Draft
> **Date**: 2026-04-27
> **Author**: Codex, synthesized from local feature audit, competitor research,
> support-forum research, and feature fight reviews
> **Theme**: Turn pg_sage from an observability/advisor surface into an
> autonomous DBA that diagnoses, acts, verifies, and remembers.

---

## 1. Thesis

pg_sage should not compete as another PostgreSQL dashboard.

The product should be an autonomous DBA agent. It should observe database state,
diagnose root causes, choose safe actions, execute within explicit trust
boundaries, verify outcomes, roll back when needed, and retain a durable audit
trail of what happened.

The winning loop is:

```text
Observe -> Diagnose -> Decide -> Act -> Verify -> Remember
```

Any feature that does not participate in this loop must either become evidence
for the loop, move to an admin/debug surface, or be removed from the primary
product experience.

---

## 2. Inputs

This spec synthesizes:

- `tasks/agent-local-feature-research.md`
- `tasks/agent-competitor-dba-research.md`
- `tasks/agent-forum-dba-painpoints.md`
- `tasks/agent-ui-feature-fight.md`
- `tasks/agent-backend-feature-fight.md`
- `research/ROADMAP_SYNTHESIS.md`
- `research/user_pain_points.md`
- `research/llm_automation_opportunities.md`
- Existing specs:
  - `specs/v0.9-root-cause-prevention.md`
  - `specs/v0.10-schema-intelligence.md`

The research and audits agree on the main product lesson: pg_sage already has
a real backend autonomy spine, but the visible product still looks too much
like a conventional monitoring UI.

---

## 3. Terminology

This spec uses the following terms consistently:

| Term | Meaning |
|---|---|
| Agent | pg_sage's autonomous DBA control loop, including deterministic analyzers, LLM reasoning, policy checks, executor, and verification. |
| Case | The durable unit of DBA work. A case may originate from a finding, RCA incident, schema lint result, query hint, forecast, log signal, or direct operator request. |
| Action | A typed, policy-gated operation that may change database state, pg_sage state, or team workflow state. |
| Evidence | Structured facts used to justify a case or action: metrics, plans, catalog rows, lock graphs, log entries, provider capability checks, or prior action outcomes. |
| Identity Key | The stable deduplication key for a case. It must survive repeated detections and, where possible, object renames or query ID churn. |
| Outcome | The verified result of an action. It is not just "SQL executed"; it records whether the intended effect was observed. |
| Playbook | An ordered set of action candidates for resolving an incident or recurring DBA task. |
| Verification | A programmatic post-action check that determines whether the action achieved its intended effect. |
| Rollback | A defined reversal or mitigation path. Some actions have SQL rollback, some have compensating actions, and some only have stop/monitor procedures. |
| Shadow Mode | A non-executing policy posture where pg_sage records what it would have done, why, and what value it believes was missed by not acting. |
| Why Now | The urgency driver for a case. This is separate from the root-cause hypothesis; it explains why the case needs attention now rather than later. |

The UI may call cases "Cases" or "DBA Cases". The backend should use `case`
terminology to avoid overloading the existing `finding` model.

---

## 4. Draft Decisions

These decisions reduce ambiguity for the first implementation wave. They can be
changed later by updating this spec, but implementation should not treat them as
open-ended questions.

1. **Incidents become cases.** There should not be a separate top-level
   Incidents page in the target UI. Incidents are high-urgency cases with
   incident-specific evidence and playbooks.
2. **`ANALYZE` is the reference typed action.** The first typed action contract
   should be `analyze_table` because it is low risk, already partially
   implemented, and exercises the full loop: evidence, action, cooldown,
   dedicated execution path, and post-verification.
3. **DDL starts with preflight and PR/CI output before direct high-risk
   execution.** Direct execution may come later for safe DDL classes, but the
   first migration-safety release should prevent unsafe changes and produce
   reviewed scripts/checklists before it executes DDL itself.
4. **Legacy config gets one compatibility window.** Deprecated config keys may
   be read for one minor release with warnings, then removed from docs and
   product UI.
5. **A top-level page must own a product job.** Admin plumbing moves under
   Settings. Diagnostic artifacts move under Cases or Fleet detail. A top-level
   page exists only if it supports a recurring operator job directly.
6. **Shadow proof is part of onboarding.** Before users grant `auto_safe`,
   pg_sage should show what it would have safely resolved in observation mode,
   including avoided toil and blocked automation reasons.

---

## 5. Goals

1. Make pg_sage feel like an operator, not a metrics console.
2. Collapse fragmented findings, incidents, forecasts, schema lint, and query
   hints into one DBA case lifecycle.
3. Promote the action system into the center of the product: proposed,
   approved, executed, failed, rolled back, verified.
4. Convert advisory-only modules into typed remediation workflows wherever
   safe.
5. Preserve and strengthen trust, emergency stop, policy, approval, and audit
   controls.
6. Remove or demote top-level surfaces that only show charts, counters, raw
   data, or delivery plumbing.
7. Use the LLM as a reasoning, synthesis, and planning layer over deterministic
   evidence, not as an ungrounded advice generator.

## 6. Non-Goals

- No generic observability expansion.
- No new top-level page unless it maps directly to the DBA loop.
- No autonomous semantic SQL rewriting as an early default behavior.
- No automatic materialized view creation in the primary roadmap.
- No major-version upgrade or failover automation in this spec.
- No cost dashboard until pg_sage can attribute cost changes to actions.
- No N+1 detection from `pg_stat_statements` alone.
- No MCP/tooling integration as the core product story.
- No additional daily briefing product lane unless it is generated from the
  action ledger.

---

## 7. Product Principles

### 7.1 Action Or Evidence

Every primary feature must be either:

- an action the agent can take,
- evidence for an action,
- verification of an action,
- safety/policy control for an action,
- or an audit record of an action.

Anything else is secondary.

### 7.2 Closed Loop Beats Recommendation

A recommendation is incomplete until pg_sage can answer:

1. Why does this matter?
2. What should be done?
3. What is the risk?
4. What guardrails will be used?
5. How will success be measured?
6. What rollback or stop condition exists?
7. What happened after the action?

### 7.3 Trust Is A Product Surface

Trust level, execution mode, maintenance windows, blast-radius limits, LLM
budget state, emergency stop, and approval requirements must be visible and
auditable. These are not implementation details; they are the contract that
lets users give an autonomous agent production access.

### 7.4 Deterministic First, LLM Enhanced

The agent should rely on deterministic collectors, analyzers, lock graphs,
catalog reads, EXPLAIN plans, log parsing, and provider capability checks.

The LLM should synthesize evidence, rank tradeoffs, generate explanations,
draft remediation plans, and produce structured action candidates that still
pass deterministic validation.

### 7.5 Fewer Surfaces, Stronger Workflows

The current UI should contract into a small number of high-leverage operator
surfaces. Repeated table-first pages make the product feel like observability.
The new shape should be cockpit-first and case/action-first.

---

## 8. Risk And Policy Model

Typed actions must share a common risk vocabulary. The risk tier determines
default approval, scheduling, concurrency, and rollback expectations.

| Risk Tier | Meaning | Default Policy |
|---|---|---|
| read_only | Reads data or metadata only. No state change. | Always allowed if user can view the database. |
| safe | Low-risk maintenance or pg_sage state change with bounded impact and clear verification. | May run autonomously at `auto_safe` or higher. |
| moderate | Changes database behavior, cancels workload, or creates/removes objects with operational impact. | Requires approval unless policy explicitly allows incident response. |
| high | May block workload, lose performance safety margin, require restart, delete data, or alter application-visible schema. | Requires explicit approval and maintenance-window enforcement. |
| prohibited | Outside pg_sage's autonomy boundary. | Never executed automatically. |

Risk tier has two stages:

1. The action family declares a **base risk tier**.
2. Runtime context may escalate risk, but must not de-escalate it.

Escalation inputs include database role, table size, workload, replication
state, provider limitations, time window, previous failures, unknown backup
state, and configured protected patterns.

Policy templates:

| Policy | Behavior |
|---|---|
| observe_only | Produce cases and evidence only. No action candidates execute. |
| approval_required | Generate action candidates but require approval for all state changes. |
| auto_safe | Execute safe actions automatically; queue moderate/high actions. |
| incident_responder | Execute safe actions and selected moderate incident actions within playbook-specific limits, usually constrained to maintenance windows unless the playbook is time-critical. |
| full_autonomous | Execute safe and approved moderate families according to configured risk controls. High-risk still requires explicit approval. |

Policy precedence is:

```text
action family eligibility
-> runtime risk escalation
-> global policy template
-> per-database policy override
-> protected pattern veto
-> emergency stop veto
```

A high-risk action with unknown backup/PITR status is blocked by default unless
the policy explicitly waives the backup-state check for that database and action
family.

Maintenance-window defaults:

- read-only actions can run any time;
- safe actions can run any time unless the database policy restricts them;
- moderate actions require a maintenance window unless an incident playbook and
  policy explicitly allow immediate execution;
- high-risk actions require approval and maintenance-window enforcement;
- prohibited actions never run.

---

## 9. Current Feature Verdicts

### 9.1 Keep And Enhance

| Feature | Verdict | Required Direction |
|---|---|---|
| Executor | KEEP | Expand typed actions, prechecks, post-checks, rollback contracts, and per-action SLOs. |
| Action queue and action log | KEEP | Promote to agent timeline and audit ledger. |
| Trust levels, ramp, maintenance windows | KEEP | Make semantics explicit and policy-driven. Advisory mode must never surprise users. |
| Findings / Recommendations | KEEP | Convert into unified DBA cases with evidence, action candidates, risk, confidence, state, and outcome. |
| RCA and logwatch | ENHANCE | Promote incidents into remediation playbooks, not narratives. |
| Lock-chain and runaway query handling | KEEP | Make first-class incident automation. |
| Query tuner and hint revalidation | ENHANCE | Add before/after measurement, experiments, retirement, parameterized-query handling, `CREATE STATISTICS`, and non-hint remedies. |
| Stale-stats ANALYZE | KEEP | Treat as the model low-risk autonomous action. |
| LLM index optimizer | ENHANCE | Add real post-create benefit measurement, rollout throttles, duplicate cleanup, and rollback playbooks. |
| Fleet manager | KEEP | Make fleet readiness, isolation, trust posture, and blast-radius control first-class. |
| Settings / config / policy | KEEP | Reorganize around autonomy policy, safety, LLM budgets, notifications, and access. |
| Collector and auto-explain | KEEP | Keep boring and reliable; prioritize evidence quality over chart breadth. |
| Auth, RBAC, users | KEEP AS ADMIN | Required for production, but move user management under Settings / Access. |

### 9.2 Merge Or Demote

| Feature | Verdict | Required Direction |
|---|---|---|
| Schema Health | MERGE | Become schema cases inside unified Findings/Cases. |
| Query Hints / Performance | MERGE | Become query remediation cases and action history. |
| Forecasts | MERGE | Surface only when a forecast requires action or changes priority. |
| Incidents page | MERGE | Incidents become high-urgency cases with incident-mode detail and executable playbooks. |
| Alerts page | MERGE | Move under Settings / Notifications as delivery history. |
| Notifications page | MERGE | Move under Settings; channels and rules are plumbing. |
| Users page | MERGE | Move under Settings / Access. |
| Fleet health chart | MERGE | Use as supporting evidence in Overview or Fleet, not as a primary dashboard artifact. |
| Database tiles | MERGE | Move to Fleet; Overview should only show DBs needing attention. |
| Prometheus/metrics | MERGE | Keep for operating pg_sage; avoid building more user-facing monitoring panels. |
| Natural-language EXPLAIN | MERGE | Use inside case evidence and action explanations, not as a flagship standalone page. |
| Advisor package | MERGE | Keep strong detectors, but route output into typed cases/actions/RCA. |

### 9.3 Remove Or Freeze

| Feature | Verdict | Required Direction |
|---|---|---|
| Dashboard stat cards | REMOVE | Replace counts with agent status, decisions needed, and recent completed work. |
| Raw database snapshot page | REMOVE | Replace with single-DB agent drilldown. |
| Raw/internal JSON toggles in product UI | REMOVE FROM PRODUCT | Keep behind debug/dev mode only. |
| Standalone daily briefing | REMOVE FROM CORE | Generate summaries from action ledger only; suppress the briefing on healthy days rather than manufacturing content. |
| Frozen C extension roadmap scope | FREEZE / DEPRECATE | Sidecar is the autonomous product. Keep only if unique plan capture is required. Publish a sidecar migration note before removing extension docs or packaging. |
| Legacy `llm.index_optimizer` config | REMOVE AFTER COMPAT WINDOW | Migrate to current optimizer config. |
| Low-signal educational lint | REMOVE FROM MAIN FLOW | Keep in docs or debug reports unless it leads to action. |

---

## 10. New Product Shape

The primary UI should contract to five main surfaces plus Login.

### 10.1 Overview

Purpose: answer "What is the agent doing, and do I need to intervene?"

Must show:

- agent state: healthy, degraded, stopped, LLM-budget-limited, waiting for approval;
- top decisions needed, capped at 5 with overflow into Cases;
- top autonomous work completed since last visit, capped at 5 with overflow into Actions;
- blocked work and why it is blocked, capped at 5 with overflow into Cases/Fleet;
- highest-risk active incidents/cases, capped at 5 with overflow into Cases;
- databases whose trust/readiness posture prevents automation, capped at 5 with overflow into Fleet;
- explicit "nothing needs you" state when appropriate.

Must remove:

- generic stat-card grid as the primary hero;
- chart-first fleet health presentation;
- raw recent findings sorted only by recency.

### 10.2 Cases

Purpose: unified DBA work queue.

Cases replace fragmented top-level views for:

- existing findings/recommendations;
- RCA incidents;
- schema lint;
- query hints;
- forecasts;
- advisory output;
- log-derived operational signals.

Each case must include:

- title;
- affected database/object/query;
- category and subsystem;
- severity and business/operational impact;
- current lifecycle state;
- evidence;
- hypothesis/root cause;
- why now;
- action candidates;
- risk tier;
- confidence;
- required approval, if any;
- verification plan;
- action history;
- outcome or unresolved blocker.

Default sort should be impact and decision urgency, not recency.

#### Case Merge Rules

Existing entities should map into cases as follows:

| Existing Entity | Case Mapping |
|---|---|
| Finding | One case per distinct unresolved problem identity. Repeated detections update evidence and urgency. |
| RCA incident | One high-urgency case with incident subtype, root-cause hypothesis, and playbook. |
| Schema lint result | Case only if it has meaningful operational risk or a remediation path. Educational lint remains docs/debug. |
| Query hint | Case if the hint/rewrite/index/statistics action is still active or needs verification/retirement. |
| Forecast | Case only when lead time and severity require a decision or action. Otherwise remains supporting evidence. |
| Alert delivery failure | Case only when delivery failure blocks critical agent operation. Otherwise settings log. |

Cases should deduplicate by stable identity: database, subsystem, affected
object/query, normalized rule/action family, and active lifecycle state.

### 10.3 Actions

Purpose: immutable agent audit ledger and approval center.

Actions should be one chronological feed with filters for:

- proposed;
- pending approval;
- approved;
- executing;
- monitoring;
- succeeded;
- failed;
- rolled back;
- rejected;
- expired.

Each action must include:

- originating case;
- action type;
- proposed SQL/config/script;
- policy/trust decision;
- prechecks;
- guardrails;
- executor logs;
- post-checks;
- measured outcome;
- rollback plan and rollback status;
- actor: autonomous agent, user approval, or manual trigger.

This should become the product's proof layer.

#### Minimum Action Contract

Every action family must define these fields before it can be surfaced as
executable:

```text
action_type
risk_tier
provider_support
required_permissions
prechecks
guardrails
execution_plan
success_criteria
post_checks
rollback_or_mitigation
cooldown
audit_fields
```

An action without post-checks is a draft recommendation, not an executable
action.

### 10.4 Fleet

Purpose: manage targets and blast radius.

Must include:

- database onboarding/import/test;
- per-database trust posture;
- provider/platform capability detection;
- readiness checks;
- current blockers to autonomy;
- per-database action concurrency and cooldown limits;
- fleet-wide blast-radius controls;
- single-database drilldown focused on cases/actions/evidence.

Fleet should not become a generic database monitoring dashboard.

### 10.5 Settings

Purpose: define autonomy policy.

Settings should be organized around:

- Autonomy Policy:
  - observe only;
  - approval for all actions;
  - auto safe actions;
  - incident responder;
  - full autonomous.
- Trust and Safety:
  - trust level;
  - maintenance windows;
  - emergency stop;
  - rollback thresholds;
  - protected app/user patterns;
  - action concurrency.
- LLM and Budgets:
  - provider;
  - model;
  - budget;
  - degraded-mode behavior.
- Notifications:
  - channels;
  - routing;
  - delivery log.
- Access:
  - users;
  - roles;
  - approval permissions.
- Advanced:
  - raw config fields;
  - debug-only options.

### 10.6 Onboarding And Shadow Proof

Purpose: earn trust before autonomy is enabled.

Onboarding must guide a team through:

1. connect database;
2. verify permissions and provider capabilities;
3. run deterministic collection;
4. enter Shadow Mode;
5. review cases pg_sage would have acted on;
6. review blocked reasons and missing permissions;
7. inspect sample action contracts and guardrails;
8. choose an autonomy policy.

Shadow Mode report must show:

- cases detected over the selected window;
- safe actions pg_sage would have executed under `auto_safe`;
- cases that still require approval;
- actions blocked by permissions, provider limits, maintenance windows, or
  missing evidence;
- estimated DBA toil avoided;
- estimated database risk reduced;
- false-positive or expired-action count.

This is the adoption bridge for protective database teams. It proves the
deterministic guardrails before the user grants execution rights.

---

## 11. Core Data Model Direction

### 11.1 Case

A case is the durable unit of DBA work.

Required fields:

```text
case_id
identity_key
database_name
subsystem
category
title
summary
severity
impact_score
urgency_score
state
affected_objects
affected_queries_optional
evidence[]
hypothesis
root_cause
why_now
action_candidates[]
risk_tier
confidence
approval_required
blocked_reason
verification_plan
outcome
created_at
updated_at
resolved_at
```

`impact_score` and `urgency_score` use a 0-100 scale.

- `impact_score` is produced by the case projection layer from severity,
  affected workload, object criticality, estimated resource waste, and user or
  policy hints where available.
- `urgency_score` is produced by the case projection layer from impact,
  time-to-failure, incident status, evidence freshness, business-hour policy,
  and whether a safe action is available now.
- `affected_queries` is required only for query-derived cases. DDL, fleet,
  schema, backup, and operational incidents may leave it empty.

Case identity rules:

- query cases use normalized query fingerprint, not volatile `query_id` alone;
- object cases use a composite identity: stable catalog identity when available,
  qualified name, schema, object kind, column/index signature, and relevant
  source rule family;
- cases must survive object renames when the collector can observe OID history;
- cases must tolerate OID churn from `VACUUM FULL`, `pg_repack`, table rebuilds,
  and similar operations by matching structural signatures and recent action
  history before creating a new case;
- state changes must not create a new identity unless the root problem changed.

Ephemeral resolution rules:

- if the evidence for a case disappears before action, the case moves to
  `resolved_ephemeral` or `monitoring` rather than staying pending forever;
- pending actions for ephemeral cases expire and require revalidation before
  execution;
- flapping cases reopen the existing case identity and increase urgency only
  after crossing configured frequency or duration thresholds;
- cases resolved ephemerally still count toward shadow-mode value reporting when
  the agent had a valid action candidate it was not allowed to execute.

Case confidence and action confidence are related but not identical. Case
confidence describes confidence in the root-cause/hypothesis. Action confidence
describes confidence that a specific candidate will improve the case. A case may
have high root-cause confidence and still have low action confidence if the safe
fix is unclear.

Case states:

```text
open
investigating
action_proposed
waiting_for_approval
scheduled
executing
monitoring
verified
resolved
suppressed
blocked
failed
resolved_ephemeral
```

### 11.2 Action

An action is a proposed or executed state change.

Required fields:

```text
action_id
case_id
database_name
action_type
risk_tier
trust_decision
approval_state
proposed_statement_or_config
rollback_statement_or_plan
prechecks[]
guardrails[]
execution_attempts[]
post_checks[]
measurements_before
measurements_after
outcome
failure_reason
actor
expires_at
created_at
approved_at
executed_at
verified_at
rolled_back_at
```

Action types should be typed contracts, not arbitrary SQL buckets.

Proposed actions must expire. `expires_at` is required for any action awaiting
approval or scheduled execution. Expired actions cannot execute until the case
evidence and action contract are revalidated.

Initial action families:

- `analyze_table`
- `create_index_concurrently`
- `drop_unused_index`
- `reindex_concurrently`
- `vacuum_table`
- `set_table_autovacuum`
- `set_database_parameter`
- `create_statistics`
- `cancel_backend`
- `terminate_backend`
- `retire_hint`
- `install_hint`
- `ddl_preflight`
- `ddl_retry_with_guardrails`
- `resolve_replication_slot`
- `checkpoint_or_wal_remediation`

Worked reference action:

```text
action_type: analyze_table
base_risk_tier: safe
provider_support: PostgreSQL-compatible targets where ANALYZE is permitted
required_permissions: table ownership or ANALYZE privilege where applicable
prechecks:
  - table exists and is not excluded by policy
  - no emergency stop active for database
  - table size and recent ANALYZE cooldown within configured limits
  - stale-stat evidence exists, such as row estimate drift or high planner error
guardrails:
  - dedicated connection
  - statement_timeout
  - per-table cooldown
  - fleet/database action semaphore
  - safe-action concurrency limit across the fleet and per cluster
execution_plan:
  - ANALYZE qualified_table
success_criteria:
  - pg_stat_user_tables.last_analyze advances or analyze_count increases
  - stale-stat finding no longer fires on next analyzer cycle
post_checks:
  - verify last_analyze/analyze_count change
  - refresh affected query plan evidence where available
  - compare planner row-estimate error before/after for tracked queries
rollback_or_mitigation:
  - no SQL rollback; mitigation is stop further ANALYZE actions, reopen case if
    plan regression appears, and route to query-tuning playbook
cooldown:
  - no repeated ANALYZE on same table inside configured cooldown window
audit_fields:
  - table, prior last_analyze, new last_analyze, prior estimate error,
    post-action estimate error, triggering case_id
```

Rollback classifications:

| Rollback Class | Meaning | UI Requirement |
|---|---|---|
| reversible | Direct rollback SQL or config reversal exists. | Show rollback statement and conditions. |
| compensating | No direct rollback, but a safe compensating action exists. | Show mitigation sequence and residual risk. |
| forward_fix_only | Reversal may lose data or require application work. | Show point-of-no-return warning and require explicit approval. |
| no_rollback_needed | Read-only or state-refresh action where rollback does not apply. | Show why rollback is not applicable. |

Moderate/high DDL actions that drop data, rewrite data, or alter application
contracts are `forward_fix_only` unless a verified restoration path exists.

### 11.3 Backward Compatibility

The initial implementation may keep existing `findings`, `actions`,
`incidents`, `query_hints`, and `forecasts` tables as storage sources. The Case
API can be a projection layer at first.

Compatibility requirements:

- existing API endpoints should continue working during the transition;
- new Case APIs should link back to source records;
- UI navigation should migrate before destructive storage changes;
- retention must not delete audit/action history needed for safety;
- old finding IDs used in action approval flows must remain resolvable until
  action flows use case IDs directly.

Cutover requirements:

- maintain a `source_record_id -> case_id` resolution layer for findings,
  incidents, hints, forecasts, and schema lint records;
- preserve suppression state when source records become cases;
- re-bind pending approvals to case IDs without changing their approval state;
- keep a dual-read window where old source endpoints and new Case endpoints
  return consistent lifecycle state;
- provide a rollback path that restores old navigation/API behavior without
  losing approvals or action audit records created during the cutover;
- test emergency-stop behavior before, during, and after the projection cutover.

---

## 12. Research-Derived Feature Priorities

### 12.1 P0: Closed-Loop Action Ledger

Why:

- Competitors increasingly provide executable recommendations.
- Azure SQL's strongest reference point is verification and reversion.
- pg_sage already has an executor and action log; this should become the
  central product artifact.

Scope:

- unify pending and executed actions into one timeline;
- record prechecks, guardrails, post-checks, measurements, and rollback;
- expose action outcome to the originating case;
- make action history first-class in Overview and Cases.

### 12.2 P0: Unified Cases

Why:

- Current product fragments work into findings, schema health, query hints,
  forecasts, incidents, alerts, and action tables.
- DBAs think in incidents and tasks, not pages.

Scope:

- map existing findings into cases;
- map RCA incidents into cases;
- map schema lint into cases;
- map query hints into cases;
- map forecasts into cases only when actionable;
- retain subsystem filters for density.

### 12.3 P1: DDL / Migration Safety Operator

Why:

- Forum research repeatedly shows production freezes from blocked DDL and lock
  priority chains.
- Existing v0.10 schema intelligence work is valuable but too advisory-oriented.

Scope:

Minimum next-wave scope:

- non-executing DDL preflight;
- lock-level and rewrite analysis;
- live lock/activity/table-size/replica-lag preflight;
- safe script generation;
- `lock_timeout` and `statement_timeout` guardrails;
- case/action integration;
- approval gates for high-risk DDL.

Deferred expansion:

- parser-grade DDL classification;
- retry/schedule/cancel logic;
- direct execution of safe DDL classes.

### 12.4 P1: Incident Playbook Automation

Why:

- Real DBA work during incidents is procedural: identify blockers, cancel or
  terminate carefully, run maintenance, verify, and prevent recurrence.
- RCA without action is still an observability product.

Minimum next-wave scope:

- model incidents as cases with playbook slots;
- implement one lock/idle-in-transaction playbook end to end;
- execute only safe actions automatically and queue moderate actions unless
  `incident_responder` explicitly allows them.

Candidate playbooks:

- blocked DDL / lock pileup;
- idle-in-transaction blocker;
- runaway query;
- connection exhaustion;
- WAL growth / replication slot disk risk;
- replica lag / standby conflict;
- autovacuum blocked or falling behind;
- stale stats / planner misestimate;
- sequence exhaustion.

### 12.5 P1: Vacuum / Bloat / Freeze Autopilot

Why:

- Vacuum and bloat are among the most common PostgreSQL pain points.
- Existing advisor/analyzer coverage should become table-specific remediation.

Scope:

- per-table autovacuum tuning recommendations;
- per-table action candidates;
- freeze horizon and XID runway;
- bloat trend and remediation plan;
- blocked vacuum detection;
- verification after `VACUUM`, `ANALYZE`, `REINDEX`, or table parameter change.

### 12.6 P1: Query Tuning Beyond Hints

Why:

- Query tuning is one of the clearest autonomous DBA jobs.
- Hints are useful but can become their own technical debt.

Scope:

- before/after measurement;
- hint retirement;
- `CREATE STATISTICS` recommendations;
- stale-stats ANALYZE;
- index recommendations;
- parameterized-query support;
- non-hint remedies;
- experiment tracking.

### 12.7 P1: Provider Capability Matrix

Why:

- Autonomous action differs across self-hosted PostgreSQL, RDS, Aurora, Cloud
  SQL, AlloyDB, Azure, Kubernetes, and provider-managed environments.

Scope:

- provider detection;
- action capability checks;
- parameter setting strategy;
- extension availability;
- log access strategy;
- permissions limitations;
- backup/PITR visibility;
- user-facing explanation when autonomy is blocked.

### 12.8 P1: PR / CI Mode

Why:

- Tembo-style PR generation is a useful bridge for teams not ready for direct
  autonomous DDL or schema changes.
- For many protective data teams, PR/script generation is the first acceptable
  action output for schema work.

Scope:

- generate migration scripts;
- include expected impact;
- include rollback SQL;
- include verification SQL;
- attach risk labels;
- link PR/change to the originating case.

PR/CI mode is an action output, not a separate top-level product surface. A case
may offer multiple outputs for the same action candidate:

```text
execute_now
queue_for_approval
schedule_for_maintenance_window
generate_pr_or_script
```

---

## 13. First Release Boundary

The first implementation wave should not attempt every playbook. It should make
the product shape real and prove one complete autonomous loop.

### 13.1 Must Ship

1. Case projection API and Cases UI for existing findings, RCA incidents,
   schema lint, query hints, and forecasts.
2. Overview rebuilt around agent state, decisions needed, blocked autonomy,
   and recent actions.
3. Actions timeline combining pending and executed actions.
4. `analyze_table` typed action contract with prechecks, guardrails,
   execution, post-checks, cooldown, and audit.
5. Settings policy templates for `observe_only`, `approval_required`, and
   `auto_safe`.
6. Minimal provider-support stub for `analyze_table`: self-hosted PostgreSQL,
   RDS/Aurora, Cloud SQL, AlloyDB, and unknown-compatible targets are allowed
   only when permissions checks pass; unsupported or unknown privilege state
   blocks execution.
7. Shadow-mode value report for the last 7 days: cases found, actions pg_sage
   would have taken under `auto_safe`, blocked reasons, estimated DBA toil
   avoided, and zero-risk preview of proposed automation.
8. Navigation contraction to Overview, Cases, Actions, Fleet, and Settings.
9. Notifications, Users, Alerts, Schema Health, Query Hints, Forecasts, and
   Incidents removed from top-level navigation only after their destination
   sub-surfaces exist under Settings, Cases, or Fleet.

### 13.2 May Ship If Low Risk

1. DDL preflight as non-executing case/action candidates.
2. Query hint retirement surfaced as typed actions.
3. `retire_hint` typed action as the second action family after
   `analyze_table`.
4. Generate PR/script output for DDL and schema cases.
5. Case detail evidence tabs for plan/log/schema data.
6. Provider capability warnings in Fleet.

### 13.3 Must Not Ship In First Wave

1. Direct execution of high-risk DDL.
2. Automatic materialized view creation.
3. Broad semantic SQL rewrite execution.
4. Major-version upgrade automation.
5. New dashboard/chart surfaces unrelated to cases or actions.

---

## 14. Backend Architecture Direction

The current backend spine is good:

```text
collector -> analyzer / tuner / RCA / optimizer -> executor -> action log
```

Required evolution:

1. Emit cases instead of isolated findings/incidents/hints/forecasts.
2. Emit typed action candidates instead of unstructured recommended SQL.
3. Make every action candidate pass deterministic validation.
4. Store precheck and post-check definitions with the action.
5. Execute only through the trust/policy engine.
6. Verify outcomes with measurements tied to the originating case.
7. Attribute regressions to prior pg_sage actions when possible.
8. Retain action audit data longer than chart/snapshot data.

### 14.1 Typed Action Contract

Each action family must define:

- risk tier;
- required permissions;
- provider support;
- prechecks;
- guardrails;
- execution method;
- retry behavior;
- post-checks;
- success metric;
- rollback plan;
- cooldown rules;
- telemetry emitted.

No high-value module should emit arbitrary SQL directly to product UI without a
typed action wrapper.

### 14.2 LLM Contract

LLM output should be structured and validated.

LLM may:

- summarize evidence;
- rank likely root causes;
- draft action candidates;
- explain risk;
- compare alternatives;
- generate safe migration scripts for review;
- write user-facing explanations.

LLM must not:

- bypass deterministic validation;
- directly authorize execution;
- invent database state;
- hide uncertainty;
- create actions without evidence references.

Every LLM-produced action candidate must include structured uncertainty:

```text
confidence: 0.0-1.0
confidence_reason
missing_evidence[]
assumptions[]
```

Candidates missing these fields are rejected before action validation.

LLM confidence is not the execution confidence. The action confidence shown to
users is a hybrid score produced by the deterministic validator from:

- LLM confidence and missing-evidence flags;
- action-family historical success rate;
- deterministic evidence strength;
- provider support;
- rollback class;
- blast-radius estimate;
- freshness of evidence;
- prior failures or rollbacks for the same case identity/action family.

The LLM confidence may inform ranking, but it must not by itself authorize
execution.

Diagnostic loop prevention:

- a case may not receive the same action candidate more than once after that
  candidate fails verification unless new evidence appears;
- repeated failed candidates trigger an action-family cooldown and escalate the
  case to human review;
- LLM re-diagnosis has a per-case attempt limit and token budget;
- rollback of an autonomous action suppresses equivalent autonomous proposals
  until the case has materially new evidence or a user explicitly requests
  re-evaluation.

### 14.3 Memory And Feedback Contract

"Remember" means action outcomes improve future case ranking and action
selection. It does not require a new user-facing memory dashboard.

Persist as feedback signal:

- action family;
- database and provider class;
- pre-action evidence summary;
- chosen guardrails;
- verification result;
- rollback or failure reason;
- time-to-effect;
- whether the case reopened;
- whether the action was user-approved, autonomous, rejected, or overridden.

Consumers:

- case projection uses prior outcomes to adjust impact and urgency;
- action validators use prior failures for cooldown/escalation;
- RCA uses prior pg_sage actions to detect self-caused regressions;
- LLM prompts may include summarized prior outcomes for the same action family,
  database, or object class.

Memory must not silently lower risk tier. Prior successful actions can increase
confidence, but runtime policy and protected-pattern checks still decide
execution.

### 14.4 Degraded Mode Contract

When LLM budget, provider, or model availability is degraded, pg_sage must keep
the deterministic DBA loop running.

Degraded behavior:

- deterministic collectors, analyzer rules, executor policy checks, and already
  validated typed actions continue;
- new LLM-only action candidates pause;
- cases continue to be created from deterministic evidence;
- case explanations are marked as deterministic-only;
- shadow-mode value reports separate deterministic value from LLM-enhanced
  value;
- the Overview and token budget banner must say which abilities are paused and
  what work is delayed.

---

## 15. UI Architecture Direction

### 15.1 Navigation

Target top-level nav:

```text
Overview
Cases
Actions
Fleet
Settings
```

Optional secondary/admin locations:

```text
Settings / Notifications
Settings / Access
Settings / Advanced
Fleet / Database Detail
Case Detail / Evidence
Case Detail / Query Plan
Case Detail / Schema Rule
```

### 15.2 Page Remapping

| Current Page | Target |
|---|---|
| Dashboard | Rebuild as Overview |
| Recommendations | Cases |
| Actions | Actions |
| Incidents | Cases with incident-mode detail |
| Forecasts | Cases evidence/action candidates |
| Performance / Query Hints | Cases and Actions |
| Schema Health | Cases |
| Alerts | Settings / Notifications / Delivery Log |
| Notifications | Settings / Notifications |
| Users | Settings / Access |
| Databases | Fleet |
| Database snapshot | Fleet / Database Detail, action-centered |
| Settings | Settings, reorganized around autonomy policy |

### 15.3 UI Acceptance Rules

- No page should be a table of facts without a next action, evidence role, or
  audit role.
- No top-level nav item should exist only for configuration plumbing.
- Every primary row in Cases should answer "what should happen next?"
- Every primary row in Actions should answer "what did/will pg_sage do, and how
  do we know it worked?"
- Debug information must not be the default product language.

---

## 16. Safety And Governance

Autonomy requires clear brakes.

Required controls:

- global emergency stop;
- per-database emergency stop;
- policy templates;
- approval requirements by risk/action/provider/database;
- protected users, application names, schemas, and tables;
- maintenance windows;
- fleet concurrency budgets;
- per-object cooldowns;
- LLM budget/degraded-mode rules;
- rollback thresholds;
- audit immutability;
- retention policy for action/audit history.

Emergency stop semantics:

- global emergency stop blocks new action dispatch for every database;
- per-database emergency stop blocks new action dispatch only for that database;
- pg_sage continues collecting evidence and creating/updating cases while
  stopped;
- running actions are not automatically cancelled by emergency stop unless that
  action family declares a safe cancellation path;
- cancelling a running backend, DDL, or maintenance operation is itself a typed
  action with its own risk tier and approval rules;
- resume requires explicit acknowledgment of cases created or escalated during
  the stop window.

Audit retention floor:

- action, approval, rejection, rollback, and emergency-stop records must be kept
  for at least 365 days by default;
- operators may configure longer retention;
- operators must not configure action/audit retention below the floor without an
  explicit advanced override and warning.

Before any high-risk action, pg_sage should also know:

- whether the database is primary or replica;
- whether replication is healthy;
- whether backups/PITR are available when provider data permits;
- whether the required permissions exist;
- whether provider restrictions make the action unsafe or impossible.

---

## 17. Migration Plan

### Phase 1: Product Surface Contraction

1. Add Case domain model over existing findings/incidents/hints/forecasts.
2. Rebuild Overview around agent state and decisions needed.
3. Rename Recommendations to Cases.
4. Merge Schema Health, Query Hints, Forecasts, and Incidents into Cases.
5. Move Notifications, Alerts, and Users under Settings.
6. Move Databases under Fleet.
7. Remove dashboard stat-card hero and raw snapshot product surface.

### Phase 2: Typed Action Framework

1. Define action contracts for existing safe actions.
2. Wrap recommended SQL in typed action families.
3. Add deterministic validators per action type.
4. Enforce provider capability checks.
5. Make low-risk actions the reference implementation.

### Phase 3: Action Ledger Upgrade

1. Merge pending and executed action views into one timeline.
2. Add precheck, guardrail, post-check, measurement, and rollback fields.
3. Link every action to a case.
4. Add action outcome summaries back onto case detail.
5. Add action timeline digest to Overview.

### Phase 4: DBA Playbooks

1. Implement DDL safety operator.
2. Implement lock/connection incident commander.
3. Implement WAL/replication safety playbooks.
4. Implement vacuum/bloat/freeze autopilot.
5. Implement query tuning beyond hints.

### Phase 5: Team Workflow Integrations

1. Add PR/CI mode for schema and migration changes.
2. Add provider-specific action adapters.
3. Add optional daily/weekly summaries generated from action ledger.

---

## 18. Acceptance Criteria

### 18.1 Case Acceptance

A case is valid only if it has:

- source record or source event;
- database scope;
- stable identity key;
- evidence;
- severity;
- state;
- hypothesis or root-cause placeholder;
- why-now field, even when the answer is "not urgent";
- next step:
  - action candidate;
  - awaiting evidence;
  - intentionally informational;
  - suppressed;
  - blocked with reason.

### 18.2 Action Acceptance

An executable action is valid only if it has:

- typed action family;
- risk tier;
- policy decision;
- required permissions checked;
- provider support checked;
- prechecks;
- guardrails;
- success criteria;
- post-checks;
- audit record;
- rollback or mitigation path.
- `expires_at` when approval or scheduled execution is required.

### 18.3 UI Acceptance

The target UI is acceptable when:

- the top-level nav has no more than Overview, Cases, Actions, Fleet,
  Settings, and account/logout controls;
- a user can land on Overview and identify the highest-priority decision in
  under 30 seconds;
- every top-level Case row exposes state, impact, next step, and whether the
  agent can act;
- every top-level Action row exposes status, risk, actor, and verification
  result;
- admin plumbing is not a primary product route;
- raw debug fields are hidden unless debug mode is enabled.

The 30-second Overview check may be verified manually during product QA until a
formal usability test exists.

### 18.4 Safety Acceptance

The first release cannot claim autonomous DBA behavior unless:

- no high-risk action can execute without explicit approval;
- no executable action lacks post-checks;
- no executable action lacks rollback or mitigation text;
- no provider-unsupported action is shown as executable;
- no emergency-stop state permits new action dispatch;
- no autonomous action loops indefinitely after failed verification;
- no expired action can execute without revalidation;
- autonomous actions requiring rollback stay below 5% over the evaluation
  window, or the affected action family is removed from autonomous eligibility.

---

## 19. Success Metrics

Product metrics:

- percentage of cases with a proposed action;
- percentage of cases resolved by pg_sage without manual DBA work;
- percentage of autonomous actions verified successful;
- rollback rate by action family;
- mean time from detection to proposed action;
- mean time from approval to verified outcome;
- number of dashboard-only surfaces removed or merged;
- percentage of users landing on Overview and understanding next action.

Safety metrics:

- high-risk actions executed without required approval: must be zero;
- unverified actions older than policy window: must be zero;
- failed actions without rollback or blocked reason: must be zero;
- cases with missing evidence links: must be zero for action-producing cases;
- provider-unsupported actions proposed as executable: must be zero.

---

## 20. Remaining Open Questions

These are intentionally not blockers for the first wave:

1. Should the public UI say "Cases" or "DBA Cases"?
2. Which provider should get the first dedicated capability adapter after
   self-hosted PostgreSQL?
3. Which team workflow integration should come first: GitHub PRs, Slack
   approvals, or PagerDuty incident updates?

---

## 21. Explicit Design Decisions

1. The product should optimize for fewer, stronger surfaces.
2. Findings are not enough; the durable unit is a case.
3. Actions are not implementation details; they are the proof of autonomy.
4. Incidents, schema health, query hints, and forecasts are not separate product
   pillars unless they become action workflows.
5. LLM reasoning is valuable only when grounded in deterministic evidence and
   constrained by typed action validation.
6. A feature that cannot produce action, evidence, verification, safety control,
   or audit history does not fight its way into the primary UI.
