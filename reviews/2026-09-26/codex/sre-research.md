# pg_sage AI SRE research

Research date: 2026-09-26. Decision artifact; proposed functionality is not implemented.
Companion: `ai-sre-spec.md`. The main audit owns the full defect and test inventory.

## Executive Recommendation

Build **Sage Incident Investigator**: an evidence-driven investigation and recovery workflow
inside Cases. It should collect bounded diagnostic evidence, compare competing explanations,
show the next discriminating check, hand an approved typed action to the existing executor,
and verify whether the incident actually improved. The product promise is fewer operator
minutes spent proving what happened and what to do next, measured against today's pg_sage.

This is an extension of existing RCA, not a new name for it. Current source already contains
decision trees, LLM correlation, log ingestion, incident lifecycle, and self-action correlation.
The useful new unit is a durable investigation with evidence provenance, explicit uncertainty,
customer-impact context, bounded tool use, and a recoverable verification contract.

Source baseline: clean `master`, commit `b396595059d2b1b312a22231fdbfd1d2cfef9530`,
tag `v1.5.0`; remote HEAD matched during the parent audit. Analysis used the independent
`audit-repo` clone in this workspace. No production mutation or cloud commissioning occurred.

| Rank | Recommendation | Impact | Feasibility | Safety risk | Differentiation |
| --- | --- | --- | --- | --- | --- |
| 1 | Repair identity, hydration, and workflow wiring before adding authority | High | High | Reduces risk | Foundational |
| 2 | Ship read-only evidence investigations for blockers, connection pressure, WAL | High | High | Low if budgeted | Medium-high |
| 3 | Prove recovery with freshness-aware, customer-impact verification | High | Medium | Medium | High |
| 4 | Attach Vector Lab proof to Cases with identity, cohort, and expiry | High for RAG users | High | Low | High |
| 5 | Rehearse incident and migration hypotheses in disposable agent DBs | Medium-high | Medium | Medium | High |

These rankings are product judgments, not measured market share or demonstrated ROI.
Start with a focused developer/platform-team buyer managing several PostgreSQL databases.
Use successful investigations and verified operator-time savings as adoption signals.
Do not optimize for the number of AI messages, automatically closed incidents, or SQL actions.

## Current-State Assessment

The following is grounded in the inspected source snapshot, not only roadmap prose.

| Workflow | Existing implementation | Increment worth building |
| --- | --- | --- |
| Detect/correlate | `internal/rca/rca.go`, `signals.go`, `trees.go`, `log_trees.go` | Stable evidence IDs and versioned investigation state |
| LLM explanation | `internal/rca/tier2.go` correlates uncovered signals | Multiple hypotheses, disproof, validated references, bounded next checks |
| Self-induced failure | `internal/rca/self_action.go` links actions and signal families | Object-specific evidence and pre/post causal tests |
| Cases/actions | `internal/cases/case.go`, `incident_projector.go` | Investigation panel and reusable evidence packet |
| Execute | Existing typed actions, policy, trust, approval, emergency stop | Reuse these contracts; no second mutation engine |
| Verify | `internal/verify/types.go` measures query/write latency and load | Incident-specific predicates, service SLIs, confounders |
| Provider telemetry | `internal/providerobs/` and runtime wiring | Explicit freshness/completeness and capability reasons |
| Vector proof | `internal/vectorlab/`, September 4 product guide | Attach existing experiment evidence to a case |
| Agent databases | `internal/agentdb/`, deployment guide | Bounded rehearsal tied to an investigation |

Tier 2 currently assigns confidence `0.6` to LLM incidents. It converts an arrow-delimited
causal-chain string into links and assigns signal IDs by position; those links do not carry
validated evidence for each claim. This is insufficient to call the number a calibrated
probability or to treat a narrative as proof. The new workflow should retain useful hypotheses
while clearly labeling what was observed, inferred, contradicted, or not measured.

The parent audit identified prerequisites: RCA restart hydration is absent; incident database
identity is not bound through the engine; the structural rehearsal router lacks a production
implementation; the fleet rollout factory is not assigned in production; managed snapshot
creation defaults to unavailable. Treat those as repair gates, not available SRE infrastructure.
Verify fixes through executable runtime paths before depending on them.

Repository documents can be stale: `META.json` describes the C extension and version `0.5.0`;
`docs/architecture.md` identifies the Go sidecar as the product and the C extension as frozen.
Earlier RCA specifications remain labeled draft even where source exists. Current code and
runtime tests must decide support. The proposal does not revive the C-extension agent path.

## Evidence Map

Primary sources were retrieved on 2026-09-26 unless a publication date is stated. Product
documentation verifies advertised behavior, not independent efficacy. Issue reports establish
concrete pain and reproducible questions; they do not establish prevalence or current bugs.

| ID | Source | Type and supported claim | Relevance | Confidence |
| --- | --- | --- | --- | --- |
| S01 | [PostgreSQL statistics](https://www.postgresql.org/docs/current/monitoring-stats.html) | Official: cumulative counters and observation semantics need careful interpretation | Normalize intervals and reset epochs | High |
| S02 | [pg_stat_statements](https://www.postgresql.org/docs/current/pgstatstatements.html) | Official: planning/execution statistics update on successful operations; identifiers have stability limits | SQL stats cannot substitute for customer error-rate SLI | High |
| S03 | [Routine vacuuming](https://www.postgresql.org/docs/current/routine-vacuuming.html) | Official: vacuum and freezing preserve space reuse and XID safety | Diagnose horizons before maintenance advice | High |
| S04 | [Replication settings](https://www.postgresql.org/docs/current/runtime-config-replication.html) | Official: slot retention can be unlimited; retention caps can remove needed WAL | WAL mitigation needs replication-loss analysis | High |
| S05 | [Explicit locking](https://www.postgresql.org/docs/current/explicit-locking.html) | Official: operations acquire incompatible locks and can block one another | Build a blocker graph and distinguish holder from waiter | High |
| S06 | [Administration functions](https://www.postgresql.org/docs/current/functions-admin.html) | Official: cancellation and termination are distinct functions with permissions | Preserve exact action semantics and target checks | High |
| S07 | [Google SRE alerting](https://sre.google/workbook/alerting-on-slos/) | Primary SRE guidance: multi-window burn alerts balance precision and response time | Customer-impact priority, not symptom-count priority | High |
| S08 | [PgBouncer issue 1284](https://github.com/pgbouncer/pgbouncer/issues/1284) | Operator report: pool/server disconnect incident and timeout interaction | Include pool evidence and timeout ordering | Medium; one case |
| S09 | [PgBouncer configuration](https://www.pgbouncer.org/config.html) | Official: queue, pool, and timeout controls have different scopes | Avoid treating max_connections as the only lever | High |
| S10 | [Supabase pooling](https://supabase.com/docs/guides/database/connecting-to-postgres/pooling-and-limits) | Provider: client/backend limits and independent poolers differ | Explain managed-service headroom and pool topology | High |
| S11 | [pganalyze Index Advisor](https://pganalyze.com/docs/index-advisor/getting-started) | Vendor: whole-workload recommendations account for read/write tradeoffs | Index suggestions alone are already competitive territory | High for documented scope |
| S12 | [pganalyze docs](https://pganalyze.com/docs) | Vendor: VACUUM Advisor and alerting are offered | Generic vacuum dashboards are not a new category | High for documented scope |
| S13 | [AWS DevOps Guru for RDS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/devops-guru-for-rds.html) | Provider: telemetry-based reactive/proactive insights and investigation suggestions | Cross-signal diagnostics already exist | High for documented scope |
| S14 | [Cloud SQL Index Advisor](https://docs.cloud.google.com/sql/docs/postgres/use-index-advisor) | Provider: recommendations include estimated impact/storage; entitlement requirements apply | Capability gating must include edition and telemetry availability | High |
| S15 | [Azure automatic tuning](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql) | Provider: apply, verify improvement, and revert harmful tuning | Verification is expected product behavior | High |
| S16 | [Oracle automatic indexing](https://docs.oracle.com/en/database/oracle/oracle-database/26/admin/managing-indexes.html) | Provider: automatic index management follows workload changes | Consider workload lifecycle, not one-off suggestions | High |
| S17 | [Datadog investigations](https://docs.datadoghq.com/bits_ai/bits_investigation/investigate_issues/) | Vendor: iterative hypothesis checks, telemetry tools, inconclusive outcomes | A chat summary is below the competitive bar | High for documented scope |
| S18 | [Datadog configuration](https://docs.datadoghq.com/bits_ai/bits_investigation/configure/) | Vendor: investigation rate limits, RBAC, tool-call audit | Investigation cost and permissions need first-class controls | High |
| S19 | [incident.io AI SRE](https://incident.io/solution/ai-sre) | Vendor: telemetry/change/history correlation; reviewed PR is described change path | Human review remains a credible product model | High for stated boundary |
| S20 | [Rootly AI SRE](https://rootly.com/ai-sre) | Vendor: incident/code/telemetry context and hypothesis-based investigation | Compete on Postgres depth and proof, not generic RCA | High for advertised scope |
| S21 | [PagerDuty engineering](https://www.pagerduty.com/eng/inside-pagerdutys-sre-agent-how-we-built-deep-incident-investigation/) | Vendor engineering, June 2026: incident context and investigation system design | Evaluation and context quality are core engineering work | Medium-high |
| S22 | [pgvector README](https://github.com/pgvector/pgvector) | Maintainer: ANN/filter/build/query tradeoffs and iterative scans | Version-aware candidate generation | High |
| S23 | [pgvector issue 244](https://github.com/pgvector/pgvector/issues/244) | Historical report: dead tuples and ANN recall loss | Include maintenance/churn in vector incidents | Medium; not current defect proof |
| S24 | [pgvector issue 776](https://github.com/pgvector/pgvector/issues/776) | Reporter corrected initial assumptions about iterative scans and subqueries | Inspect actual plans; do not repeat issue title as fact | Medium-high |
| S25 | [pgvector issue 785](https://github.com/pgvector/pgvector/issues/785) | Report: filtered ordering/limits create difficult performance tradeoffs | Query-shape/cohort tests over blanket settings | Medium |
| S26 | [pgvectorscale README](https://github.com/timescale/pgvectorscale) | Maintainer: StreamingDiskANN, label filters, post-filtering, query controls | Add a separate adapter after pgvector evidence attachment | High |
| S27 | [Filtered ANN study](https://arxiv.org/abs/2602.11443) | Research, Feb 2026: planner/filter strategies produce workload-dependent outcomes | Benchmark exact and ANN candidates under filters | Medium; workload-specific |
| S28 | [RCheck study](https://arxiv.org/abs/2608.25185) | Research, Aug 2026: search-quality differences across groups warrant evaluation | Report cohort-level recall, not only global mean | Medium; not independently reproduced |
| S29 | [Neon schema-only branches](https://neon.com/blog/instant-branches-schema-only-or-with-data-the-choice-is-yours) | Provider: branching can separate schema from production data | Synthetic rehearsal can minimize sensitive-data transfer | High for concept; verify entitlement |
| S30 | [Neon masked environments](https://neon.com/blog/environments-masked-production-data) | Provider: masking workflows complement branches | A branch is not automatically anonymized | High |
| S31 | [BranchBench](https://arxiv.org/abs/2604.17180) | Research, Apr 2026: branch operations and data operations have different costs | Measure rehearsal setup and fidelity separately | Medium; benchmark-specific |

## Community Pain

**Slow queries and plan changes.** Operators need the relevant plan, parameter distribution,
schema/statistics change, and workload shift together. An average from a cumulative counter
can hide the incident interval. Derive interval deltas only inside a stable statistics epoch;
carry query/database/user identity and plan provenance. Separate expensive, frequent queries
from low-volume tail regressions. S01 and S02 establish why aggregate SQL statistics alone
cannot prove end-user availability or p95 latency. Product hypothesis: evidence collection
and before/after comparison save more time than another generic tuning explanation.

**Index selection and write overhead.** Whole-workload tradeoffs are established in pganalyze
and cloud advisors, not an unexplored opportunity. Improve pg_sage by retiring stale advice,
recording counterfactual plan assumptions, revalidating after schema changes, and proving
read benefit alongside write/storage cost. If a rollout or verification path is unavailable,
show that concretely rather than converting an estimate into a success badge. [S11–S16]

**Vacuum, bloat, XID and replication slots.** A vacuum symptom does not establish that more
vacuum is the appropriate immediate action. Identify old transactions, prepared transactions,
replication horizons, progress and provider limits. For WAL, distinguish production rate,
retained bytes, archiver delay, and consumer lag. Changing retention or deleting a slot can
trade disk pressure for lost replication continuity. The new value is an owner-aware recovery
plan with evidence and escalation, not an autonomous slot-drop shortcut. [S03, S04]

**Locks and migrations.** Show the root blocking backend, waiters, lock mode, object identity,
transaction age, application owner, and recent migration. Check whether the supposed blocker
still exists before offering intervention. A cancellation that returns success is not proof
that an idle transaction released its locks. Teach the UI to distinguish an observed wait
chain, a suspected triggering deployment, and a verified recovery. [S05, S06]

**Connection storms and poolers.** Backend saturation, a client queue, connection churn, and
an application pool leak are different phenomena. The PgBouncer issue is useful precisely
because its timeout interaction is not captured by one database connection percentage.
Supabase documents distinct client/backend limits and independent pooler ceilings. Collect
topology and bounded pool metrics when available; otherwise state the missing evidence.
Increasing `max_connections` must not become the default response. [S08–S10]

**Managed Postgres.** Permission, log access, edition, provider, database version, and telemetry
freshness determine what is possible. A denied log query is a capability limitation, not proof
of no errors. The first release should succeed with catalog observations while making stronger
claims only when independent provider or application evidence is present. [S10, S13, S14]

**Vector/RAG quality incidents.** Returning fewer results and returning less useful results are
different. Filter shape, dead tuples, version, ordering, and search budgets influence what
the operator sees. Issue 776 is particularly instructive: the author revised the premise of
the original report. A trustworthy assistant preserves this correction, investigates the plan,
and supplies a reproducible experiment instead of treating the issue title as settled law.
These are sampled support cases, not a quantitative claim about all users. [S23–S25]

## Competitive Landscape

All entries below describe public documentation reviewed this run. None is an independent
production comparison. Pricing is treated as an access/deployment signal, not a dollar quote;
commercial plans, usage charges and entitlement should be verified before procurement.

| Product | Audience and workflow | Automation/evidence/safety | Access signal and pg_sage design choice |
| --- | --- | --- | --- |
| pganalyze | Postgres specialists; workload and vacuum analysis | Workload-aware index advice and visibility into recommendation reasoning | Commercial monitoring product; win through verified incident work in the local sidecar, not more missing-index suggestions [S11, S12] |
| AWS DevOps Guru for RDS | RDS operators; anomalous load and proactive risks | Correlated telemetry, findings and corrective guidance; docs advise testing changes | AWS managed integration; offer consistent incident artifacts across providers [S13] |
| Cloud SQL Index Advisor | Cloud SQL query optimization | Estimated improvement/storage and SQL; Enterprise Plus/Query Insights prerequisites | Edition-dependent access; model capability evidence explicitly [S14] |
| Azure SQL automatic tuning | Azure SQL/Fabric database users | Continuous tuning with performance verification and reversal | Managed engine feature; adopt the proof loop as the bar, not novelty [S15] |
| Oracle automatic indexing | Oracle workload administration | Automatic index lifecycle responds to workload | Engine-specific capability; make Postgres lifecycle evidence inspectable [S16] |
| Datadog Bits Investigation | Teams with observability/service context | Tool-based hypothesis refinement and an inconclusive outcome; RBAC/rate limits/audit | Commercial platform and credits; preserve useful operation with local PG evidence [S17, S18] |
| incident.io Investigations | Incident response teams | Correlates telemetry, changes and history; described system change is a reviewed PR | Commercial incident platform; export a compact PG evidence packet [S19] |
| Rootly AI SRE | On-call/incident platform users | Parallel hypothesis checks and probable causes from service/change context | Commercial platform; differentiate with exact Postgres action constraints [S20] |
| PagerDuty SRE Agent | Incident-response and on-call users | Engineering material describes a dedicated investigation/context system | Commercial platform; integrate through existing alerting when authorized [S21] |

The defensible position is **PostgreSQL-specific evidence and recovery contracts with a
portable deployment boundary**. This is a design choice, not a claim that competitors lack
all these features. A small team should not attempt to outbuild the integrations, service
catalogs, paging systems and organization-wide telemetry of the larger incident platforms.

Useful optional output is a signed/versioned incident packet consumable by an existing
incident platform: scope, evidence, freshness, alternatives, proposed action, approval and
verification. Sending that packet needs the configured operator policy; drafting it does not.

## Vector Search Opportunities

The current pgvector README retrieved this run uses a `v0.8.6` installation example. Treat
that as documentation context, not the version on a user's server. Read actual extension,
server and index versions before constructing a candidate. Existing pg_sage Vector Lab
requires pgvector at least 0.8.0 and explicitly excludes several advanced shapes.

Maintainer guidance: HNSW exposes `m` and `ef_construction` at build time, `ef_search` at
query time; higher search/build effort trades resource use for recall. Filtering can reduce
returned rows; iterative scans add bounded work. Filter indexes, partial indexes and
partitioning serve different selectivities. `halfvec` and quantization change representation
and need their own validation. Prefer bounded transaction-local settings before considering
rebuilds. This source-derived summary is deliberately limited; the experiment below is our
proposed design, not a guarantee from the extension. [S22]

Proposed evaluation contract:

1. Persist query cohort, tenant/filter selectivity, index/schema/embedding identity, extension
   version, distance, k, settings, row-identity type and snapshot. Never silently reuse an old
   exact baseline after a corpus change.
2. Compute exact top-k truth on the same authorized rows, RLS context and consistent snapshot.
   Prefer an approved clone for heavy scans. Verify the baseline plan is actually exact.
   If exact truth exceeds its budget, report unmeasured; do not label ANN output ground truth.
3. Record tie handling, zero/NULL-vector exclusions, underfill and the denominator
   `min(k, eligible_ground_truth_count)`. Empty eligible sets are not 100% recall successes.
4. Measure recall@k and underfill per cohort/query, latency distribution, timeout fraction,
   I/O, memory, index size, build duration, WAL and insert/update cost. Separate warm and cold
   experiments and repeat order randomly. A faster mean must not hide a bad minority cohort.
5. Compare exact-filter-first against existing HNSW candidates; later add IVFFlat lists/probes,
   half precision with full-precision reranking, and compatible DiskANN configurations.
6. Recommendation requires latency AND recall AND resource constraints for every required
   cohort. Low sample count means provisional evidence. Freeze evaluation queries before
   tuning and hold out an independent set to avoid fitting the benchmark.
7. Persist a case attachment with a content hash and explicit expiry, never raw vectors or
   sensitive filter values. An embedding-model, dimensionality, corpus or index change
   invalidates the certificate and schedules a bounded recheck.

pgvectorscale deserves a separate adapter: its README describes StreamingDiskANN,
label-based filtering and arbitrary WHERE post-filtering, plus `query_search_list_size` and
`query_rescore`. Label and arbitrary-filter paths are not interchangeable; settings can be
transaction-local. Rebuild and compression choices need new fidelity/resource tests. Do not
copy vendor benchmark multipliers into a cross-provider recommendation. [S26]

Recent research strengthens the need for measurement, not a universal tuning recipe.
The filtered-ANN study reports workload-specific planner/filter tradeoffs; RCheck examines
quality differences across groups. Neither was independently reproduced here. The actionable
inference is to evaluate query shape and minority cohorts, with exact truth and held-out
queries, before applying advice. [S27, S28]

**Smallest ship:** attach the already implemented Vector Lab JSON to Cases and the decision
ledger with workload/schema identity and expiration. Next add exact-search as an eligible
winner and representative cohort sampling. DiskANN, halfvec, partition roots, hybrid fusion
and reranking should each be separate measured releases. Distinguish ANN recall from
application relevance, grounded-answer quality, permission correctness and freshness.

## Agent-Created Databases

pg_sage already provisions local schemas/databases and has gated cloud workflows, leases,
budgets, ownership, backups and cleanup. The opportunity is a **rehearsal attached to an
incident or change**: generate a minimum schema, constraints, synthetic seeds and a bounded
workload; reproduce a hypothesized failure; compare a proposed intervention; destroy the lab;
attach both successful and failed evidence. This connects existing modules to an operator job.

Schema-only branches and masking illustrate useful infrastructure, but a branch is not
inherently sanitized. Provider support and entitlements require target-side verification.
BranchBench reports tradeoffs between branching and data operation costs; that is a reason
to measure lab startup and runtime independently, not declare one platform superior. [S29–S31]

Require separate lab credentials without production write access; explicit target allowlists;
egress restrictions; owner/run IDs; pre-reserved compute/storage/query budgets; TTL and
independent reaper; teardown receipts; and a durable audit. Budget estimates must include
provisioning, idle compute, storage, snapshots, I/O and cleanup failures. Unknown price means
no automatic provisioning. Never send production rows to an LLM to make seed data.

Separate fidelity levels: (A) schema-only synthetic functional proof, (B) masked statistical
reproduction, (C) isolated production-shaped replay. A successful A proves semantics, not
production p95 or disk headroom. Preserve skew, hot keys, concurrent transactions and failure
timing where relevant. Application side effects, external services and client retry logic may
still be absent. Surface those gaps next to the result.

Generated migrations must pass existing DDL safety and policy. Generated workloads have a
positive operation allowlist; SQL comments and database text cannot expand authority. Lab
failure is a result worth keeping. Production promotion always revalidates schema identity,
provider readiness and approvals. The current missing rehearsal/router/rollout wiring is a
hard implementation prerequisite, so cloud rehearsals are excluded from the first release.

## Backlog

Scoring: 1–5 ordinal judgments. Pain/differentiation/feasibility/evidence higher is better;
safety risk higher is worse. Scores organize discussion rather than imply measured precision.

| Item | Pain | Differentiation | Feasibility | Risk | Evidence | First slice |
| --- | --- | --- | --- | --- | --- | --- |
| Evidence investigator | 5 | 4 | 4 | 2 | 5 | Three catalog-based incident families, read-only |
| Recovery verifier | 5 | 5 | 3 | 3 | 5 | Blocker/queue recovery with missing-data states |
| Vector evidence attachment | 4 | 4 | 5 | 1 | 5 | Import existing report and invalidate stale identity |
| Application SLO linkage | 4 | 3 | 3 | 2 | 5 | One read-only Prometheus-compatible connector |
| Change attribution | 4 | 4 | 3 | 2 | 4 | Signed change event plus object/window overlap |
| Agent DB rehearsal | 4 | 5 | 2 | 3 | 4 | Local synthetic lab; no production data |
| DiskANN/quantized evaluation | 3 | 4 | 2 | 2 | 4 | Adapter behind explicit capability probe |
| Incident learning/replay | 4 | 4 | 4 | 2 | 4 | Redacted incident bundles and withheld test set |

For the investigator, verify known blocker graphs, stale snapshots, malformed model output,
wrong-tenant evidence, missing privileges and investigation time budgets. Success metric:
operator time to a usable evidence packet and adjudicated cause accuracy, not confidence text.

For recovery, inject apparent success with continuing symptoms and traffic disappearing.
Success metric: independently verified recoveries divided by attempted interventions,
with inconclusive, failed and harmful outcomes reported separately.

For vector attachment, test mismatched schema/corpus/role, boundary ties, empty truth,
corrupted manifests and permission leakage. Success metric: qualified recommendations
reproduced by a second run and quality regressions caught before rollout.

For SLO/change linkage, test stale counters, counter resets, duplicate/out-of-order events,
low traffic, clock skew and deployment correlation without causation. Success metric:
useful-priority precision and reduced duplicate notifications without missed incidents.

For labs, inject provisioning timeout, cost uncertainty, failed cleanup and skew mismatch.
Success metric: reproducible experiments inside budget, verified teardown, and explicit
fidelity; not the number of databases created. All thresholds in the spec are design targets.

## Questions Not Asked

1. Who owns the page and budget: application developer, DBA, platform engineer, or managed service?
2. Which three recurring incidents consume the most actual operator time today?
3. Is the acceptable first win diagnosis quality, faster recovery, or verified toil reduction?
4. Which customer-facing service and SLI map to each database, and who maintains that mapping?
5. What data can leave the host: query fingerprints, normalized SQL, plans, logs or none?
6. Which evidence must remain available when the monitored database is completely unavailable?
7. How should missing logs/host metrics affect diagnosis, authority and user-visible confidence?
8. Who may approve intervention, and what action classes require a second approver?
9. What is the tolerated blast radius when cancellation cannot be undone?
10. Which provider/edition/version combinations will be continuously commissioned in CI?
11. How will actual operator time saved be sampled without rewarding premature closure?
12. Who owns vector relevance ground truth, tenant fairness, and embedding/corpus versioning?
13. Can lab data be exported or cloned, and who funds resources left after a failed teardown?
14. How are incorrect incident conclusions corrected and removed from future retrieved examples?
15. Should pg_sage be the incident workspace or a specialist that feeds an existing platform?

## Research Gaps

No authenticated X/LinkedIn scan was performed. No claim about absent social discussion is
made. Search surfaced public forum material, but technical conclusions here rely on primary
docs, maintainer issues, papers and inspected code. This is not an exhaustive community census.
PostgreSQL mailing-list and Stack Overflow searches did not yield sufficiently focused,
verified material for this report; they remain follow-up research, not implied coverage.

No competitor trial, pricing quote, independent vendor benchmark, customer interview, or
production incident replay was executed. Market willingness to pay and comparative diagnosis
quality remain unproven. Papers were inspected at abstract/documentation level and need
artifact reproduction before engineering decisions depend on numeric benchmark claims.

The next evidence to collect is five to ten redacted real incidents from intended users,
their current investigation steps and time, data boundaries, and actual provider permissions.
Pair these with deliberately misleading replay cases. The feature should earn broader
authority only after it can abstain correctly, survive missing telemetry, and prove recovery.
