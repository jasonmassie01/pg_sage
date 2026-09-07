# Vector Evidence Lab: first slice of a database contract engine

Date: 2026-09-04. Status: implementation contract. Owner: product research lane.

## User outcome

An operator can ask: "For these tenant-filtered searches, which existing HNSW
query settings preserve at least 95% recall and keep client-observed p95 below
50 ms?" pg_sage runs a bounded experiment and returns evidence or a specific
reason no recommendation qualifies. The shipped sidecar binary exposes
`pg_sage vector-lab --manifest workload.json`, with its connection only in
`SAGE_VECTORLAB_DATABASE_URL`. No scheduler, deployment, DDL or persistent tuning.

This completes the local measurement slice previously proposed in the April
vector roadmap. It does not claim novel ANN algorithms or completed autonomous
vector tuning. The larger innovation is a database contract whose evidence
expires when workload/schema changes; that integration follows only after a
reliable measurement substrate exists.

## Manifest and limits

Strict JSON, reject unknown fields and trailing documents. Required explicit
schema/table, unique non-null scalar identity column, vector column, distance
(`l2`, `cosine`, `inner_product`), k (1..100), queries (3..200), repeats (1..20),
variants (1..16), minimum recall (0,1], max p95 milliseconds (0,60000], statement
timeout milliseconds (1..30000), total timeout milliseconds (1..600000).
At most 10000 measured queries. Vectors must share finite dimensions (1..2000);
cosine vectors must be nonzero. IDs and identifiers are bounded and reject NUL.
Each query has an opaque label, vector and optional equality-filter values.
The filter-column list is shared; values are passed as text parameters inferred
by PostgreSQL from column equality. Null filter values are not supported.

Each variant has unique name, `ef_search` 1..1000 and iterative scan `off` or
`strict_order`. Explicit budgets avoid zero-as-unlimited defaults. pgvector
must be >=0.8.0, with extension schema discovered from pg_catalog.

## Measurement protocol

1. Validate before connecting. One bounded repeatable-read READ ONLY transaction
   owns every baseline and candidate. Transaction-local statement/lock/idle
   timeouts and total context deadline prevent indefinite snapshot retention.
2. Generate SQL from quoted identifiers, whitelisted operators, and bind values.
   No arbitrary SQL, views, function calls, global GUCs, cloud APIs or LLM calls.
3. For each query, fetch exact k+1 results with distance-plus-zero ordering and
   index/index-only scans disabled. Exclude null vectors. Reject duplicate/null
   IDs and nonfinite distances. A tie across k/k+1 makes the sample ambiguous;
   no ANN recommendation can be certified from it. Empty truth is not perfect
   recall. A corpus with fewer than k matches is measured against its true size.
4. Measure exact and each candidate repeatedly; rotate candidate order across
   query/repeat to reduce ordering bias. The exact query runs first to establish
   truth and warms data, so results are warm-cache observations, not cold-load
   guarantees. Candidates use locally bounded HNSW settings and custom plans.
5. Inspect each candidate EXPLAIN JSON and require a valid HNSW index on the
   requested relation in the plan. A sequential or B-tree fallback is recorded,
   never described as validated HNSW tuning. Do not force HNSW artificially.
6. Compute set-based recall, underfill and p95 using nearest-rank percentile.
   Acceptance requires every measured sample to meet recall, zero underfill,
   unambiguous truth, actual HNSW plans and candidate p95 <= budget. Choose
   lowest observed p95 among passing candidates, deterministic name tie-break.
7. Always roll back and release connection, including cancellation/errors.

## Evidence contract

Versioned JSON reports contain manifest SHA-256, start time, Postgres/pgvector
versions, snapshot identifier, workload counts, exact latency, candidate
settings, per-query worst recall, underfill count, plan index names, p95 and
rejection reasons. No row IDs, vectors, filter values, connection strings or
raw plan JSON are emitted. Errors expose a bounded operation label and SQLSTATE
without raw server detail. A recommendation remains advisory and explicitly
has `auto_apply=false`; this evidence cannot bypass existing `policy.Gate` or
pretend the latency-only `verify.Engine` validates vector recall. Exit 0 means
qualified result, 2 means complete experiment without qualifying result, 1
means invalid input or failed experiment.

The report is ready for Cases/ledger attachment by consumers, but durable Case,
MCP and ongoing verification integration are not shipped in this first slice.
Future contract binding requires schema fingerprint, evidence expiry, workload
identity and recall verification support before policy-gated promotion.

## Acceptance tests

- CHECK-V01: valid filtered workload produces actual HNSW plan evidence and a
  qualifying candidate whose recall/latency meet declared budgets.
- CHECK-V02: invalid/unknown/trailing JSON, missing budgets, finite/dimension
  errors, malformed identifiers, repeated names, resource overages are rejected.
- CHECK-V03: duplicate IDs, null IDs, empty truth, boundary ties, underfill,
  insufficient recall and no HNSW index cannot produce a recommendation.
- CHECK-V04: exact and candidate filters and vectors remain parameterized;
  hostile identifier/value text cannot introduce another statement.
- CHECK-V05: statement timeout, cancellation, unavailable extension, missing
  table/column and SELECT permission denial return distinct actionable failure
  classes and do not leak input or credentials.
- CHECK-V06: real PostgreSQL enforces read-only transaction and local GUCs
  disappear after rollback; concurrent experiments use independent snapshots.
- CHECK-V07: actual sidecar entrypoint emits parsable report; report does not
  contain vectors/filter values/row IDs/DSN; nonqualifying run exits 2.
- CHECK-V08: uncached coverage >=70% for business logic; race test covers shared
  pool execution. Test source is staged before first execution.

## Operational limits

Use a disposable clone or explicitly designated read-only target. Read-only
exact search still consumes I/O, CPU and snapshot lifetime. This tool has time,
query and connection bounds, not a dollar meter. It preserves the connected
role's visibility/RLS; it cannot prove all application roles or tenant security.
No claim about production p95 under concurrency, answer quality, filtered
selectivity distribution, recall confidence intervals, index rebuild cost,
halfvec, quantization, DiskANN, hybrid reranking, or autonomous deployment.
