# pg_sage: from autonomous actions to evidence-backed database contracts

Prepared 2026-09-04 for jmass, product builder and operator. Scope: product intent,
community demand, Postgres and adjacent competitors, vector workloads, agent-created
databases, and a concrete implementation. Research used repository evidence and
public primary sources; forum reports are labeled anecdotes, not market surveys.

## Executive recommendation

Build the **database contract engine**: declare an outcome, measure whether it holds,
experiment within a budget, apply through standing policy, and revoke the claim when
the evidence expires. Examples: "retrieval recall is at least 95% for every tenant
cohort," "this migration preserves the selected business invariants," and "this
backup was restored and checked within the last seven days."

This is a product hypothesis, not a demonstrated market moat. Its first implemented
slice is **Vector Evidence Lab**, reached through the shipped sidecar's `vector-lab`
command. It measures filtered HNSW candidates against exact ground truth and refuses
to recommend an unmeasured or failing configuration. It gives an operator and an
agent a reproducible decision artifact without changing production configuration.

The strategic discontinuity would be moving from proposed SQL to a maintained
contract about database behavior. The credible near-term advantage is trustworthy
evidence and interoperability, not a claim to have invented tuning or verification.

Ranked priorities, using judgment scores 1..5 (higher is better except risk):

| Opportunity | Pain | Differentiation | Feasibility | Risk | Evidence | First slice |
|---|---:|---:|---:|---:|---:|---|
| Harden existing action/verify loop | 5 | 3 | 5 | 3 | 5 | Runtime audit and real failure tests, parallel repair lane |
| Vector quality contract | 4 | 4 | 4 | 2 | 5 | Read-only filtered recall/latency experiment, built here |
| Verified restore contract | 5 | 4 | 3 | 4 | 3 | Disposable restore with application invariant checks |
| Workload-aware change tournament | 4 | 4 | 3 | 4 | 4 | Compare index/statistics/query alternatives on one clone |
| Agent database lifecycle contract | 4 | 4 | 3 | 4 | 3 | Owner, TTL, spend limit, teardown evidence and drift policy |

## Current-state assessment

The checkout began at branch `fix/v1.4.0-lint`, HEAD `2447ef2`. A read-only fetch
found `origin/master` at merge commit `7c238f3`. No pull or branch switch was made:
the working tree had user changes in `local_monitor_config.yaml` and an untracked
monitor launcher. Parallel remediation is editing the same checkout in reserved
paths; this report describes the inspected starting state plus this lane's change.

The April product spec states Observe → Diagnose → Decide → Act → Verify → Remember.
July's build spec prescribes standing policy, verification coupled to apply, clones,
MCP intent tools, custodians and value accounting. This is already a substantial
autonomous DBA architecture. Adding another generic dashboard or an LLM that emits
DDL would move away from that intent.

| Workflow | Code evidence inspected | Assessment |
|---|---|---|
| Collect/analyze | collector, analyzer, forecaster packages and runtime orchestration | Implemented; repair lane owns current correctness validation |
| Advise | optimizer/advisor/tuner prompts and tests | Implemented LLM and deterministic pathways; optimizer accepts HNSW-shaped DDL |
| Authorize/execute | policy, executor, autonomy packages; July contract | Existing typed authority boundary; new lab does not mutate through it |
| Verify/rollback | `verify/evaluate.go`, PostgreSQL tests | Actual per-query latency/write-impact decisions; explicitly rejects unsupported criteria |
| Explain/evidence/value | cases, ledger, value, MCP | Existing evidence and intent surfaces; new vector report is advisory JSON |
| Clone/rehearse | `clone.Provider`, DLE/snapshot adapters, migration packages | Implemented adapters; provider availability must be checked per deployment |
| Fleet/UI/security | fleet, API router, auth, React routes | Present; old endpoint/page counts in CLAUDE.md are stale |
| Vector quality | no recall benchmark package in starting tree | Missing; implemented here as bounded CLI experiment |

The July research's universal claim that no Postgres tool closes the loop is too
broad to repeat. Lantern has HNSW autotuning; PostgresAI provides a clone substrate.
The July statement that MCP was retired is also historical: current `internal/mcp`
implements intent handling. `CHANGELOG.md` is more current than parts of CLAUDE.md
and roadmap.md. Presence of a package is not proof of every product guarantee.

The audit also found an actual safety defect: the load source labeled the ratio
of active connections to max_connections as CPU and both I/O percentages. That
ratio cannot establish resource headroom. The repair now returns an explicit
unavailable-telemetry error from the native catalog adapter and rejects malformed
percentages at the gate. Automatic index admission is therefore withheld on this
adapter; reviewed manual actions still function. A real host/provider telemetry
adapter is an outstanding capability, not silently replaced by a false proxy.

## Evidence map and community pain

**Silent retrieval quality loss.** A developer reports about 50,000 vectors,
HNSW plus user filtering, and searches returning nothing; another reports trouble
at 35,000 rows. This is an unanswered support report and could involve SQL shape,
RLS or other causes. It establishes a concrete debugging pain, not a proven single
root cause. [Stack Overflow, January 2024 with December follow-up](https://stackoverflow.com/questions/77812490/with-supabase-i-am-doing-a-pg-vector-search-and-when-i-have-too-many-documents)

**Vacuum appears to run while storage keeps growing.** An RDS operator describes a
much smaller restored database and continuing production growth despite recent
autovacuum timestamps. Replies propose several causes, including orphaned large
objects and cleanup blockers. The product implication is a causal investigation
and verified outcome, not "last_autovacuum exists, therefore healthy."
[Firsthand Reddit report](https://www.reddit.com/r/PostgreSQL/comments/1dpbl95/something_peculiar_table_keeps_growing_on_disk/)

**Replication retention can exhaust disk.** A migration operator reports WAL slots
filling storage and the difficulty of introducing a cap without losing replica
continuity. This is a high-severity anecdote. Official documentation confirms the
tradeoff: slots can retain WAL, while caps can make a lagging standby unable to
continue. "Delete old slots" is therefore an unsafe generic fix.
[Reddit migration report](https://www.reddit.com/r/PostgreSQL/comments/y2si1q/wal_slots_filled_storage_during_replication/),
[PostgreSQL replication documentation](https://www.postgresql.org/docs/current/warm-standby.html)

**Index choice is a workload portfolio problem.** A good index can improve reads
while adding write cost. pganalyze explicitly models candidate combinations and
write overhead. pg_sage should compare net workload effects and preserve evidence
for rejected choices, rather than count created indexes as value.
[pganalyze Index Advisor documentation](https://pganalyze.com/docs/index-advisor/getting-started)

The themes map to distinct outcome tests: response quality, space reuse or growth,
replication continuity and net workload cost. A common health score cannot certify
all four. Forums are discovery signals; no willingness-to-pay or prevalence
estimate is inferred from these examples.

## Competitive landscape

Deployment/pricing signals below are qualitative. Exact paid tiers and quotes were
not necessary for the implementation decision and have not been verified.

| Product/ecosystem | Audience/workflow | Automation and evidence | Deployment signal | Implication for pg_sage |
|---|---|---|---|---|
| pganalyze | Postgres engineers optimizing workload indexes | Models alternative indexes and write overhead; operator reviews insights | Commercial service and collector | Compete on safe outcome execution and evidence portability, not another missing-index list |
| PostgresAI DBLab | Developers/DBAs testing full-sized data | Fast thin clones, migration checks, reset/destroy lifecycle | Apache 2.0 engine; paid platform/SE/EE | Integrate clone substrate; do not rebuild storage snapshots |
| AlloyDB Index Advisor | Managed Postgres customers | Tracks workload and recommends indexes | Provider-integrated | Provider capability awareness and portable verification matter |
| AWS DevOps Guru for RDS | AWS operators finding performance anomalies | Reactive/proactive insights and corrective recommendations | AWS service | Explain the action and measured result beyond another anomaly alert |
| Azure SQL automatic tuning | Managed relational operations | Continuous performance checks and corrective behavior; validated index lifecycle in research | Managed SQL ecosystem | Verification is established prior art and a minimum quality bar |
| Oracle Automatic Indexing | Oracle workload tuning | Candidate evaluation/invisible-index testing and workload-driven management | Oracle ecosystem | Learn safe experimentation; Postgres lacks identical invisible-index semantics |
| Lantern | Vector application developers | CLI autotunes HNSW with recall/latency experiment results | Open source CLI/extension ecosystem | Generic HNSW autotune is not novel; focus on scoped contracts and explicit evidence limits |
| pgvector/pgvectorscale | Postgres vector workloads | HNSW/IVFFlat and StreamingDiskANN have different controls | Extensions plus hosted availability | Implement adapters and prove quality; never translate knobs blindly |

Sources: [pganalyze](https://pganalyze.com/docs/index-advisor/getting-started),
[DBLab](https://postgres.ai/docs/database-lab),
[AlloyDB](https://docs.cloud.google.com/alloydb/docs/index-advisor-overview),
[AWS RDS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/devops-guru-for-rds.html),
[Azure tuning](https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql),
[Microsoft's index lifecycle research](https://www.microsoft.com/en-us/research/wp-content/uploads/2019/02/autoindexing_azuredb.pdf),
[Oracle optimizer white paper](https://www.oracle.com/technetwork/database/bi-datawarehousing/twp-optimizer-with-oracledb-19c-5324206.pdf),
[Lantern autotune](https://github.com/lanterndata/lantern_extras#Index-Autotune),
[pgvectorscale](https://github.com/timescale/pgvectorscale).

## Vector search opportunities

The retrieved pgvector README installs version 0.8.6. HNSW construction controls
`m` and `ef_construction` trade memory/build and insertion cost against quality;
`ef_search` trades query work against recall. Iterative scans arrived in 0.8.0
and can mitigate filtered underfill, subject to scan/memory bounds. Selective
filters may favor exact search with filter indexes; partial indexes and partitioning
are other workload-specific options. `halfvec` and binary quantization reduce
representation/index costs but need quality measurement and, often, reranking.
Use transaction-local query controls before considering an index rebuild.
[pgvector primary documentation](https://github.com/pgvector/pgvector)

StreamingDiskANN is a separate pgvectorscale access method with its own search
controls, compression and filtering behavior. Its published vendor benchmarks
are not predictions for LifeOS. A future adapter must measure the same workload
contract, rather than reuse HNSW assumptions.
[pgvectorscale primary documentation](https://github.com/timescale/pgvectorscale)

ACORN explores predicate-aware vector graph search; newer filtered-ANN research
compares multiple systems and reports filter/plan-dependent quality and latency.
These papers support benchmarking by filter cohort, not importing headline
speedups. Their datasets, implementations and hardware differ from this tool.
[ACORN, SIGMOD 2024 research](https://arxiv.org/abs/2403.04871),
[Filtered ANN systems analysis, 2026 preprint](https://arxiv.org/abs/2602.11443)

The implemented experiment design uses the same repeatable-read snapshot for
exact and candidate runs, a stable unique identity, k+1 boundary-tie detection,
per-query worst recall, underfill counts, observed HNSW plan evidence and p95.
No recommendation is possible for empty or ambiguous ground truth. This prevents
an aggregate mean from concealing a failed tenant sample. A short selected
workload remains selected evidence, not a statistical guarantee for all traffic.

Before production promotion, add representative filter cohorts and held-out
queries, concurrent load trials, evidence expiry, schema/index fingerprints,
parameter provenance, recall uncertainty and actual application-role semantics.
RAG answer correctness is a further application evaluation: nearest-neighbor
recall is not answer faithfulness or task success. Hybrid search/reranking needs
its own contract and judged relevance dataset.

## Agent-created databases and step-function opportunities

Agents create schema, seed data, migrations, vector corpora, workloads and eval
sets. The hard question is who owns cost, authority and correctness after the
agent leaves. Existing copy-on-write branching can provide cheap initial forks,
but modifications still consume resources; branching is not a spend guarantee.
[Neon storage engineering documentation](https://github.com/neondatabase/neon/blob/main/docs/synthetic-size.md)

Three hypotheses extend the current product coherently:

1. **Database outcome contracts.** An agent asks for an outcome with explicit
   latency/quality/cost/invariant limits. pg_sage returns a scoped evidence
   certificate, not a promise. The certificate expires on schema/workload drift;
   failed revalidation parks changes or triggers the already-authorized fallback.
   New capability: verification is an ongoing resource the agent can reason about.
2. **Counterfactual change tournaments.** On the same disposable clone compare
   index, extended statistics, query rewrite, exact vector search and ANN tuning.
   Select a Pareto frontier, record why alternatives lost, then canary the selected
   change through existing policy. New capability: optimize the whole intervention,
   not just a knob inside the first guessed solution. This is not causal certainty:
   cloned hardware, workload coverage and cache state remain explicit confounders.
3. **Recoverability as an executable dependency.** A migration or destructive
   maintenance policy requires a recent restore-and-invariant artifact. Restore
   failure revokes authority before an incident. New capability: backup confidence
   becomes a tested prerequisite rather than a green timestamp. Provider costs,
   encryption keys, role grants and restore targets require their own adapters.

For disposable labs require target allowlists, separate credentials, fixed time
and query budgets, owner/TTL, cancellation, teardown verification, audit records,
and an external-cost ceiling enforced by the provider adapter. Production data
must stay within authorized boundaries; a clone containing PII is still sensitive.
Synthetic seeds are labeled synthetic and cannot certify production plans or
tenant quality. This first implementation creates no cloud resource and sends no
data to a model or third party.

## Backlog and success measures

| Slice | Verification | Safety limit | Success measure |
|---|---|---|---|
| Vector Evidence Lab, built | Real pgvector, exact/ANN, permission, RLS, no-index, timeout, CLI and race tests | Advisory only; bounded read-only transaction | Correctly rejects bad evidence; no pooled state leak |
| Cases/ledger contract attachment | Versioned schema + stale artifact rejection + authorization tests | Evidence is never authority | Every recommendation points to a reproducible experiment |
| Workload/schema expiry | Change fixture invalidates prior certificate | No cached approval after drift | Zero stale certificates used for promotion |
| Clone tournament | Compare fixed candidate set with controlled replay and budget failure tests | No production DDL or cloud work without configured authority | Lower workload cost with no protected cohort regression |
| Restore contract | Real restore, expected schema/count/domain invariants, teardown after failure | Separate destination; provider spend/TTL bounds | Verified recovery-time distribution, not theoretical RTO |

## Questions not asked

1. Is the first buyer an overloaded DBA, an application developer, or an agent platform?
2. Which outcome would that buyer pay to maintain rather than merely inspect?
3. What is an acceptable false-success rate for autonomous verification?
4. Who declares a representative workload and owns excluded tenants?
5. Does a database contract attach to an application release, schema, role, or all three?
6. When a contract fails at 02:00, what standing authority actually exists?
7. Can a safe fallback hurt another workload outside the sampled query set?
8. How long may a read-only experiment retain a snapshot on a busy primary?
9. Which provider restrictions prevent rollback or a real restore test?
10. What data can leave the database, and who can read experiment artifacts?
11. Who pays for clones that survive a crashed controller?
12. What measured toil or incident outcome justifies the value counter?
13. Can a second agent independently reproduce the evidence without trusting the first?
14. What customer result would disconfirm the contract-engine thesis?

## Research gaps and stopping rationale

No customer interviews, paid-tier price audit, X research, or universal competitor
exhaustiveness claim. Neon documentation's initial browser open failed; its public
engineering repository supplied primary branching evidence. The Stack Overflow
case is unresolved. Current papers may be preprints, and reproducibility against
their exact datasets was outside this first implementation. Managed-provider
vector availability is deployment-specific; the built slice is exercised on
PostgreSQL 16 with pgvector, not certified across every advertised sidecar target.

Discovery covered local specs/current code, firsthand forums, Postgres-native
tools, managed providers, two other RDBMS ecosystems, vector prior art and clone
engineering. Follow-up explicitly disconfirmed uniqueness claims and resolved
which capability was missing in code. More broad searches were unlikely to change
the first implementation decision; the next useful evidence is real workload
experimentation and buyer interviews, not another list of links.
